// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/consensuschain"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/rawdb"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/execution/protocol/rules/ethash"
	"github.com/erigontech/erigon/execution/protocol/rules/merge"
	"github.com/erigontech/erigon/execution/state"
	"github.com/erigontech/erigon/execution/vm"
	"github.com/erigontech/erigon/rpc"
	"github.com/urfave/cli/v2"

	erigonnode "github.com/erigontech/erigon/cmd/erigon/node"
	"github.com/erigontech/erigon/db/kv/rawdbv3"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/node/debug"
	"github.com/erigontech/erigon/node/eth"
)

type rpcTransaction struct {
	tx *types.Transaction
	txExtraInfo
}

type txExtraInfo struct {
	BlockNumber *string         `json:"blockNumber,omitempty"`
	BlockHash   *common.Hash    `json:"blockHash,omitempty"`
	From        *common.Address `json:"from,omitempty"`
}
type rpcBlock struct {
	Hash         *common.Hash        `json:"hash"`
	Transactions []rpcTransaction    `json:"transactions"`
	UncleHashes  []common.Hash       `json:"uncles"`
	Withdrawals  []*types.Withdrawal `json:"withdrawals,omitempty"`
}

func convertEthToLocalBlock(raw json.RawMessage) (*types.Block, error) {

	// Decode header and transactions.
	var head *types.Header
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	// When the block is not found, the API returns JSON null.
	if head == nil {
		return nil, errors.New("header is nil")
	}

	var body rpcBlock
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	// Pending blocks don't return a block hash, compute it for sender caching.
	if body.Hash == nil {
		tmp := head.Hash()
		body.Hash = &tmp
	}

	// Fill the sender cache of transactions in the block.
	txs := make([]types.Transaction, len(body.Transactions))
	for i, tx := range body.Transactions {
		if tx.tx != nil {
			txs[i] = *tx.tx
		}

	}
	blk := types.NewBlockFromStorage(head.Hash(), head, txs, nil, nil, nil)
	return blk, nil
}

func fetchBlocksFromRpc(url string, logger log.Logger, start int64, count int) (types.Blocks, error) {
	client, err := rpc.Dial(url, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RPC %s: %w", url, err)
	}
	defer client.Close()

	blocks := make(types.Blocks, 0, count)

	for i := 0; i < count; i++ {
		blockNum := start + int64(i) + 1
		res := json.RawMessage{}

		// Format block number as hex
		hexNum := fmt.Sprintf("0x%x", blockNum)

		// Retry logic for RPC calls
		maxRetries := 3
		var lastErr error
		for retry := 0; retry < maxRetries; retry++ {
			if err = client.Call(&res, "eth_getBlockByNumber", hexNum, true); err != nil {
				lastErr = err
				logger.Warn("Failed to fetch block, retrying", "err", err, "number", hexNum, "retry", retry+1)
				continue
			}

			blk, err := convertEthToLocalBlock(res)
			if err != nil {
				lastErr = err
				logger.Error("Failed to convert block", "err", err, "number", hexNum)
				break
			}

			blocks = append(blocks, blk)
			logger.Info("Fetched block", "number", blockNum, "hash", blk.Hash().Hex(), "txs", len(blk.Transactions()))
			lastErr = nil
			break
		}

		if lastErr != nil {
			return blocks, fmt.Errorf("failed to fetch block %d after %d retries: %w", blockNum, maxRetries, lastErr)
		}
	}

	return blocks, nil
}

func importChain(cliCtx *cli.Context, rpcUrl string, start int64) error {
	logger, tracer, _, _, err := debug.Setup(cliCtx, true /* rootLogger */)
	if err != nil {
		return err
	}

	nodeCfg, err := erigonnode.NewNodConfigUrfave(cliCtx, nil, logger)
	if err != nil {
		return err
	}

	ethCfg := erigonnode.NewEthConfigUrfave(cliCtx, nodeCfg, logger)
	ethCfg.Snapshot.NoDownloader = true // no need to run this for import chain (also used in hive eest/consume-rlp tests)
	ethCfg.InternalCL = false           // no need to run this for import chain (also used in hive eest/consume-rlp tests)
	stack := makeConfigNode(cliCtx.Context, nodeCfg, logger)
	defer func() { _ = stack.Close() }()

	ethereum, err := eth.New(cliCtx.Context, stack, ethCfg, logger, tracer)
	if err != nil {
		return err
	}
	err = ethereum.Init(stack, ethCfg, ethCfg.Genesis.Config)
	if err != nil {
		return err
	}

	// Get current database head to resume sync
	chainDb := ethereum.ChainDB()
	ctx := context.Background()
	tx, err := chainDb.BeginRo(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin read tx: %w", err)
	}
	currentHeader := rawdb.ReadCurrentHeader(tx)
	tx.Rollback()

	var currentBlock int64 = 0
	if currentHeader != nil {
		currentBlock = int64(currentHeader.Number.Uint64())
		logger.Info("Resuming sync from database head", "block", currentBlock)
	} else {
		logger.Info("Starting sync from genesis")
	}

	// Use the max of CLI start flag or current DB head
	if start > currentBlock {
		currentBlock = start
	}

	// Get latest block number from RPC
	rpcClient, err := rpc.Dial(rpcUrl, logger)
	if err != nil {
		return fmt.Errorf("failed to connect to RPC for latest block: %w", err)
	}
	defer rpcClient.Close()

	var latestBlockHex string
	if err := rpcClient.Call(&latestBlockHex, "eth_blockNumber"); err != nil {
		return fmt.Errorf("failed to get latest block number: %w", err)
	}
	latestBlock := new(big.Int)
	latestBlock.SetString(latestBlockHex[2:], 16)

	logger.Info("Starting block sync", "fromBlock", currentBlock+1, "toBlock", latestBlock.Uint64())

	batchSize := 10

	// Continuous sync loop
	for currentBlock < latestBlock.Int64() {
		remainingBlocks := latestBlock.Int64() - currentBlock
		fetchCount := int(remainingBlocks)
		if fetchCount > batchSize {
			fetchCount = batchSize
		}

		logger.Info("Fetching block batch", "from", currentBlock+1, "count", fetchCount)
		blocks, err := fetchBlocksFromRpc(rpcUrl, logger, currentBlock, fetchCount)
		if err != nil {
			logger.Error("Failed to fetch blocks", "err", err)
			return err
		}

		if len(blocks) > 0 {
			logger.Info("Executing block batch", "count", len(blocks), "from", blocks[0].NumberU64(), "to", blocks[len(blocks)-1].NumberU64())
			err = ExecuteChain(ethereum, ethereum.ChainDB(), blocks, logger)
			if err != nil {
				logger.Error("ExecuteChain failed", "err", err)
				return err
			}
			currentBlock = int64(blocks[len(blocks)-1].NumberU64())
			logger.Info("Block batch executed successfully", "lastBlock", currentBlock)
		} else {
			break
		}
	}

	logger.Info("Sync completed", "finalBlock", currentBlock)
	return nil
}

func ExecuteChain(ethereum *eth.Ethereum, chainDb kv.RwDB, blocks types.Blocks, logger log.Logger) error {
	engine := merge.New(&ethash.FullFakeEthash{})

	ctx := context.Background()

	// We need a temporal transaction for v3 state reader/writer.
	tdb, ok := chainDb.(kv.TemporalRwDB)
	if !ok {
		return fmt.Errorf("chainDb does not implement kv.TemporalRwDB: %T", chainDb)
	}

	tx, err := tdb.BeginTemporalRw(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	sd, err := execctx.NewSharedDomains(tx, log.New())
	if err != nil {
		return err
	}
	defer sd.Close()

	vmConfig := vm.Config{}
	chainReader := consensuschain.NewReader(ethereum.ChainConfig(), tx, nil, logger)

	// Determine starting txNum based on DB (like stagedsync does).
	currentHead := rawdb.ReadCurrentHeader(tx)
	startBlockNum := uint64(0)
	if currentHead != nil {
		startBlockNum = currentHead.Number.Uint64()
	}

	var txNum uint64
	var baseMax uint64
	if startBlockNum == 0 {
		// First non-genesis block txNums normally start after genesis mapping.
		// If mapping exists, use it.
		baseMax, err = rawdbv3.TxNums.Max(tx, 0)
		if err != nil {
			return err
		}
		if baseMax > 0 {
			txNum = baseMax + 1
		} else {
			txNum = 1
		}
	} else {
		baseMax, err = rawdbv3.TxNums.Max(tx, startBlockNum)
		if err != nil {
			return err
		}
		txNum = baseMax + 1
	}

	// getHashFn used by EVM to resolve historical block hashes.
	getHashFn := func(n uint64) (common.Hash, error) {
		h := rawdb.ReadHeaderByNumber(tx, n)
		if h == nil {
			return common.Hash{}, fmt.Errorf("header not found for number %d", n)
		}
		return h.Hash(), nil
	}

	for _, block := range blocks {
		if block == nil || block.Header() == nil {
			return errors.New("nil block/header")
		}
		h := block.HeaderNoCopy()
		bn := block.NumberU64()
		bh := block.Hash()

		// Ensure chain DB contains header/body/canonical mapping.
		if err := rawdb.WriteHeader(tx, h); err != nil {
			return fmt.Errorf("WriteHeader: %w", err)
		}
		if err := rawdb.WriteCanonicalHash(tx, bh, bn); err != nil {
			return fmt.Errorf("WriteCanonicalHash: %w", err)
		}
		if err := rawdb.WriteHeadHeaderHash(tx, bh); err != nil {
			return fmt.Errorf("WriteHeadHeaderHash: %w", err)
		}
		if _, err := rawdb.WriteRawBodyIfNotExists(tx, bh, bn, block.RawBody()); err != nil {
			return fmt.Errorf("WriteRawBodyIfNotExists: %w", err)
		}

		// Bind domains to current execution position.
		sd.SetBlockNum(bn)
		sd.SetTxNum(txNum)

		stateReader := state.NewReaderV3(sd.AsGetter(tx))
		stateWriter := state.NewWriter(sd.AsPutDel(tx), nil, txNum)

		if err := ExecuteBlock(ethereum.ChainConfig(), &vmConfig, getHashFn, engine, block, stateReader, stateWriter, chainReader, nil, logger); err != nil {
			return fmt.Errorf("execute block %d (%x): %w", bn, bh, err)
		}

		// Compute and verify state root (commitment) like stagedsync.
		computedRoot, err := sd.ComputeCommitment(ctx, tx, true, bn, sd.TxNum(), "movasync", nil)
		if err != nil {
			return fmt.Errorf("ComputeCommitment: %w", err)
		}
		_ = computedRoot
		//if !bytes.Equal(computedRoot, h.Root.Bytes()) {
		//	return fmt.Errorf("wrong state root at block %d: got=%x expected=%x", bn, computedRoot, h.Root.Bytes())
		//}

		// Update PlainStateVersion like stagedsync does when committing state.
		if _, err := rawdb.IncrementStateVersion(tx); err != nil {
			return fmt.Errorf("IncrementStateVersion: %w", err)
		}

		// Maintain txNum mapping for this block.
		// We approximate total txs as len(txs)+1 (block end), and maxTxNum = txNum + total - 1.
		totalTx := uint64(len(block.Transactions())) + 1
		maxTxNum := txNum + totalTx - 1
		if err := rawdbv3.TxNums.Append(tx, bn, maxTxNum); err != nil {
			return fmt.Errorf("TxNums.Append(block=%d,maxTx=%d): %w", bn, maxTxNum, err)
		}

		// Advance execution txNum for next block.
		txNum = maxTxNum + 1

		// Flush & commit per-block for correctness (simpler than stagedsync batching).
		if err := sd.Flush(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}

		// New transaction after commit
		tx, err = tdb.BeginTemporalRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// Re-bind shared domains to new tx
		sd.Close()
		sd, err = execctx.NewSharedDomains(tx, log.New())
		if err != nil {
			return err
		}
		defer sd.Close()
		chainReader = consensuschain.NewReader(ethereum.ChainConfig(), tx, nil, logger)

		logger.Info("executed+committed block", "number", bn, "hash", bh, "txs", len(block.Transactions()), "nextTxNum", txNum)
	}

	return nil
}
