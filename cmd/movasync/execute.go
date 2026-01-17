package main

import (
	"fmt"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/protocol"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/state"
	"github.com/erigontech/erigon/execution/tracing"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/vm"
)

func ExecuteBlock(
	chainConfig *chain.Config, vmConfig *vm.Config,
	blockHashFunc func(n uint64) (common.Hash, error),
	engine rules.Engine, block *types.Block,
	stateReader state.StateReader, stateWriter state.StateWriter,
	chainReader rules.ChainReader, getTracer func(txIndex int, txHash common.Hash) (*tracing.Hooks, error),
	logger log.Logger,
) (executeBlockErr error) {
	ibs := state.New(stateReader)
	ibs.SetHooks(vmConfig.Tracer)
	header := block.Header()

	gasUsed := new(uint64)
	usedBlobGas := new(uint64)
	gp := new(protocol.GasPool)
	gp.AddGas(block.GasLimit()).AddBlobGas(chainConfig.GetMaxBlobGasPerBlock(block.Time()))

	if vmConfig.Tracer != nil && vmConfig.Tracer.OnBlockStart != nil {
		td := chainReader.GetTd(block.ParentHash(), block.NumberU64()-1)
		vmConfig.Tracer.OnBlockStart(tracing.BlockEvent{
			Block:     block,
			TD:        td,
			Finalized: chainReader.CurrentFinalizedHeader(),
			Safe:      chainReader.CurrentSafeHeader(),
		})
	}

	if vmConfig.Tracer != nil && vmConfig.Tracer.OnBlockEnd != nil {
		defer func() {
			vmConfig.Tracer.OnBlockEnd(executeBlockErr)
		}()
	}

	if err := protocol.InitializeBlockExecution(engine, chainReader, block.Header(), chainConfig, ibs, stateWriter, logger, vmConfig.Tracer); err != nil {
		return err
	}

	var rejectedTxs []*protocol.RejectedTx
	includedTxs := make(types.Transactions, 0, block.Transactions().Len())
	receipts := make(types.Receipts, 0, block.Transactions().Len())
	blockNum := block.NumberU64()

	for i, txn := range block.Transactions() {
		ibs.SetTxContext(blockNum, i)
		writeTrace := false
		if vmConfig.Tracer == nil && getTracer != nil {
			tracer, err := getTracer(i, txn.Hash())
			if err != nil {
				return fmt.Errorf("could not obtain tracer: %w", err)
			}
			vmConfig.Tracer = tracer
			writeTrace = true
		}
		receipt, _, err := protocol.ApplyTransaction(chainConfig, blockHashFunc, engine, nil, gp, ibs, stateWriter, header, txn, gasUsed, usedBlobGas, *vmConfig)
		if writeTrace && vmConfig.Tracer != nil && vmConfig.Tracer.Flush != nil {
			vmConfig.Tracer.Flush(txn)
			vmConfig.Tracer = nil
		}

		if err != nil {
			if !vmConfig.StatelessExec {
				return fmt.Errorf("could not apply txn %d from block %d [%v]: %w", i, block.NumberU64(), txn.Hash().Hex(), err)
			}
			rejectedTxs = append(rejectedTxs, &protocol.RejectedTx{i, err.Error()})
		} else {
			includedTxs = append(includedTxs, txn)
			if !vmConfig.NoReceipts {
				receipts = append(receipts, receipt)
			}
		}
	}

	receiptSha := types.DeriveSha(receipts)
	logger.Info("receipt root calculated", "root", receiptSha.Hex(), "expected", block.ReceiptHash().Hex())

	var bloom types.Bloom
	if !vmConfig.NoReceipts {
		bloom = types.CreateBloom(receipts)
		if !vmConfig.StatelessExec && bloom != header.Bloom {
			return fmt.Errorf("bloom computed by execution: %x, in header: %x", bloom, header.Bloom)
		}
	}
	var newBlock *types.Block
	var err error
	if !vmConfig.ReadOnly {
		txs := block.Transactions()
		newBlock, _, err = protocol.FinalizeBlockExecution(engine, stateReader, block.Header(), txs, block.Uncles(), stateWriter, chainConfig, ibs, receipts, block.Withdrawals(), chainReader, true, logger, vmConfig.Tracer)
		if err != nil {
			return err
		}
	}
	blockLogs := ibs.Logs()
	logger.Info("block logs calculated", "count", len(blockLogs))
	newRoot := newBlock.Root()
	logger.Info("new block root calculated", "root", newRoot.Hex(), "expected", block.Root().Hex())

	return nil
}
