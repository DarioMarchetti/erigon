package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"sort"

	"github.com/erigontech/erigon/cmd/utils"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/config3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/rawdbv3"
	"github.com/erigontech/erigon/db/kv/temporal"
	"github.com/erigontech/erigon/db/rawdb"
	"github.com/erigontech/erigon/db/state"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/state/genesiswrite"
	"github.com/erigontech/erigon/execution/tracing/tracers"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/execution/types/accounts"
	"github.com/erigontech/erigon/node"
	"github.com/erigontech/erigon/node/debug"
	"github.com/erigontech/erigon/node/nodecfg"
	"github.com/erigontech/erigon/rpc"
	"github.com/holiman/uint256"
	"github.com/urfave/cli/v2"
)

var (
	genesisFlag = cli.StringFlag{
		Name:  "genesis",
		Usage: "Path to genesis file to use for the network. If not specified, the default genesis for the network will be used.",
		Value: "",
	}
	rpcFlag = cli.StringFlag{
		Name:  "rpc",
		Usage: "RPC URL to fetch blocks from",
		Value: "https://ethereum-rpc.publicnode.com",
	}
	blockFlag = cli.Uint64Flag{
		Name:  "block",
		Usage: "Block number to start replay",
		Value: 1,
	}
	// Basic HTTP-RPC flags (subset of Erigon/rpcdaemon flags)
	httpEnabledFlag = cli.BoolFlag{
		Name:  "http.enabled",
		Usage: "Enable HTTP JSON-RPC server bound to movasync DB",
		Value: true,
	}
	httpAddrFlag = cli.StringFlag{
		Name:  "http.addr",
		Usage: "HTTP server listening interface",
		Value: nodecfg.DefaultHTTPHost,
	}
	httpPortFlag = cli.IntFlag{
		Name:  "http.port",
		Usage: "HTTP server listening port",
		Value: nodecfg.DefaultHTTPPort,
	}
	httpAPIFlag = cli.StringFlag{
		Name:  "http.api",
		Usage: "Comma separated list of APIs to enable: eth,erigon,web3,net,debug,trace,txpool,db",
		Value: "eth,erigon",
	}
	app = cli.App{
		Name:  "movasync",
		Usage: "Replays a block range from a network on a local Erigon instance and optionally exposes HTTP JSON-RPC",
		Flags: []cli.Flag{
			&genesisFlag,
			&rpcFlag,
			&blockFlag,
			&utils.DataDirFlag,
			&utils.DbPageSizeFlag,
			&utils.DbSizeLimitFlag,
			&utils.ChainFlag,
			&utils.ErigonDBStepSizeFlag,
			&utils.ErigonDBStepsInFrozenFileFlag,
			&httpEnabledFlag,
			&httpAddrFlag,
			&httpPortFlag,
			&httpAPIFlag,
		},
		Action: func(c *cli.Context) error {
			return run(c)
		},
	}
)

type mainAccountInfo struct {
	EthAddress common.Address
	Balance    *big.Int
	Nonce      int
}

func readMovaGenesis(genesis string) (*types.Genesis, error) {
	data, err := os.ReadFile(genesis)
	if err != nil {
		return nil, err
	}
	var mg movaGenesis
	if err := json.Unmarshal(data, &mg); err != nil {
		return nil, err
	}
	chainId, _ := new(big.Int).SetString(mg.ChainId, 10)

	ethGenesis := types.Genesis{
		Config: &chain.Config{
			ChainName:             "mova",
			ChainID:               chainId,
			Rules:                 chain.EtHashRules,
			HomesteadBlock:        big.NewInt(0),
			DAOForkBlock:          big.NewInt(0),
			TangerineWhistleBlock: big.NewInt(0),
			SpuriousDragonBlock:   big.NewInt(0),
			ByzantiumBlock:        big.NewInt(0),
			ConstantinopleBlock:   big.NewInt(0),
			PetersburgBlock:       big.NewInt(0),
			IstanbulBlock:         big.NewInt(0),
		},
		Nonce:      0,
		Timestamp:  uint64(mg.GenesisTime.Unix()),
		ExtraData:  nil,
		GasLimit:   30000000,
		Difficulty: big.NewInt(0),
		Mixhash:    common.Hash{},
		Coinbase:   common.HexToAddress("0x"),
		Alloc:      make(types.GenesisAlloc),
		Number:     0,
		GasUsed:    0,
		ParentHash: common.Hash{},
	}
	var allAccount = make(map[string]*mainAccountInfo)

	for _, account := range mg.AppState.Auth.Accounts {

		addr := common.HexToAddress(account.Value.EthAddress)
		var balance = new(big.Int)
		for _, coin := range account.Value.Coins {
			if coin.Denom == mg.AppState.Evm.Params.EvmDenom {
				balance.SetString(coin.Amount, 10)
				break
			}
		}
		allAccount[account.Value.Address] = &mainAccountInfo{
			EthAddress: addr,
			Balance:    balance,
			Nonce:      account.Value.Sequence,
		}
	}
	for _, deposit := range mg.AppState.Genutil.Gentxs {
		depositor := deposit.Value.Msg[0].Value.DelegatorAddress
		ma, ok := allAccount[depositor]
		if !ok {
			continue
		}
		depBalance, _ := new(big.Int).SetString(deposit.Value.Msg[0].Value.Value.Amount, 10)
		ma.Balance = new(big.Int).Sub(ma.Balance, depBalance)
	}
	for _, ma := range allAccount {
		ethGenesis.Alloc[ma.EthAddress] = types.GenesisAccount{
			Balance: ma.Balance,
			Nonce:   uint64(ma.Nonce),
		}
		log.Info("genesis account", "address", ma.EthAddress.Hex(), "balance", ma.Balance.Text(16), "nonce", ma.Nonce)
	}
	return &ethGenesis, nil
}

func run(c *cli.Context) error {
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlInfo, log.StdoutHandler))
	dataDir := c.String("datadir")
	rpcUrl := c.String("rpc")
	blockNum := c.Uint64("block")
	genesis := c.String("genesis")

	log.Info("dump param", "datadir", dataDir, "rpc", rpcUrl, "block", blockNum, "genesis", genesis)

	ethGenesis, err := readMovaGenesis(genesis)
	if err != nil {
		return fmt.Errorf("failed to read genesis: %w", err)
	}
	if err := initGenesis(c, ethGenesis); err != nil {
		return fmt.Errorf("failed to init genesis: %w", err)
	}
	log.Info("genesis loaded", "chainId", ethGenesis.Config.ChainID.String(), "alloc_accounts", len(ethGenesis.Alloc))

	// Start lightweight HTTP JSON-RPC server bound to movasync node DB so we can
	// query while importChain is running. This uses the node's internal
	// rpcstack, not a separate rpcdaemon process.
	if c.Bool(httpEnabledFlag.Name) {
		if err := startMovasyncHTTP(c); err != nil {
			log.Warn("Failed to start HTTP-RPC server", "err", err)
		} else {
			log.Info("HTTP-RPC server started",
				"addr", c.String(httpAddrFlag.Name),
				"port", c.Int(httpPortFlag.Name),
				"api", c.String(httpAPIFlag.Name))
		}
	}

	if err := importChain(c, rpcUrl, int64(blockNum-1)); err != nil {
		return err
	}

	return nil
}

// startMovasyncHTTP configures and starts a minimal HTTP JSON-RPC stack
// bound to the movasync process. Right now it only exposes meta "rpc" API,
// which is enough for basic health checks; extending with full eth backend
// would require wiring movasync through eth.Ethereum like cmd/erigon.
func startMovasyncHTTP(c *cli.Context) error {
	logger := log.New()

	// Build a bare Server instance; movasync does not yet register chain
	// backends as RPC services, so only the default "rpc" module is exposed.
	// Batch/streaming configuration can be refined later or exposed as flags.
	srv := rpc.NewServer(2 /* batchConcurrency */, false /* traceRequests */, false /* debugSingleRequest */, false /* disableStreaming */, logger, 0)

	httpAddr := c.String(httpAddrFlag.Name)
	httpPort := c.Int(httpPortFlag.Name)

	http.Handle("/", srv)

	go func() {
		if err := http.ListenAndServe(fmt.Sprintf("%s:%d", httpAddr, httpPort), nil); err != nil {
			logger.Error("HTTP-RPC server exited", "err", err)
		}
	}()

	logger.Info("movasync HTTP-RPC listening", "addr", httpAddr, "port", httpPort)
	return nil
}

// initGenesis will initialise the given JSON format genesis file and writes it as
// the zero'd block (i.e. genesis) or will fail hard if it can't succeed.
// This custom implementation properly stores the genesis state to the database.
func initGenesis(cliCtx *cli.Context, gen *types.Genesis) error {
	var logger log.Logger
	var tracer *tracers.Tracer
	var err error
	if logger, tracer, _, _, err = debug.Setup(cliCtx, true /* rootLogger */); err != nil {
		return err
	}

	stack, err := MakeNodeWithDefaultConfig(cliCtx, logger)
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()

	chaindb, err := node.OpenDatabase(cliCtx.Context, stack.Config(), dbcfg.ChainDB, "", false, logger)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer chaindb.Close()

	if tracer != nil {
		if tracer.Hooks != nil && tracer.Hooks.OnBlockchainInit != nil {
			tracer.Hooks.OnBlockchainInit(gen.Config)
		}
	}

	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))

	// Call our custom genesis commit that stores state
	_, block, err := commitGenesisBlockWithState(cliCtx, chaindb, gen, dirs, logger)
	if err != nil {
		return fmt.Errorf("commit genesis block: %w", err)
	}
	logger.Info("Successfully wrote genesis state", "hash", block.Hash(), "root", block.Root())

	// Log genesis summary for verification
	totalBalance := new(big.Int)
	for _, alloc := range gen.Alloc {
		if alloc.Balance != nil {
			totalBalance.Add(totalBalance, alloc.Balance)
		}
	}
	logger.Info("Genesis summary",
		"totalAccounts", len(gen.Alloc),
		"totalBalance", totalBalance.String(),
		"stateRoot", block.Root().Hex())

	return nil
}

// commitGenesisBlockWithState writes the genesis block and properly persists the state to the database.
// Unlike the standard genesiswrite.CommitGenesisBlock which uses NoopWriter,
// this implementation actually stores account balances and state to the domains.
func commitGenesisBlockWithState(cliCtx *cli.Context, db kv.RwDB, genesis *types.Genesis, dirs datadir.Dirs, logger log.Logger) (*chain.Config, *types.Block, error) {
	ctx := context.Background()

	// Ensure snapshots dir exists; GetStateIndicesSalt may need it.
	if err := os.MkdirAll(dirs.Snap, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create snapshots dir %s: %w", dirs.Snap, err)
	}

	// ErigonDB geometry must never be 0, otherwise state history/ii will panic with
	// `assert: empty stepSize`.
	stepSize := cliCtx.Uint64(utils.ErigonDBStepSizeFlag.Name)
	stepsInFrozen := cliCtx.Uint64(utils.ErigonDBStepsInFrozenFileFlag.Name)
	if stepSize == 0 {
		logger.Warn("Invalid step size override (0). Falling back to default", "default", config3.DefaultStepSize)
		stepSize = config3.DefaultStepSize
	}
	if stepsInFrozen == 0 {
		logger.Warn("Invalid steps-in-frozen-file override (0). Falling back to default", "default", config3.DefaultStepsInFrozenFile)
		stepsInFrozen = config3.DefaultStepsInFrozenFile
	}
	if stepSize == 0 || stepsInFrozen == 0 {
		return nil, nil, fmt.Errorf("invalid ErigonDB geometry: stepSize=%d stepsInFrozenFile=%d", stepSize, stepsInFrozen)
	}

	// Create aggregator for state management
	logger.Info("Opening aggregator for state management...", "stepSize", stepSize, "stepsInFrozenFile", stepsInFrozen)
	agg, err := state.New(dirs).
		Logger(logger).
		// movasync is often running on a fresh datadir; generate salt if missing.
		GenSaltIfNeed(true).
		StepSize(stepSize).
		StepsInFrozenFile(stepsInFrozen).
		Open(ctx, db)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open aggregator: %w", err)
	}
	defer agg.Close()

	// Re-open folder after aggregator creation.
	if err := agg.OpenFolder(); err != nil {
		return nil, nil, fmt.Errorf("failed to open aggregator folder: %w", err)
	}

	// Wrap database with temporal support
	tdb, err := temporal.New(db, agg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temporal db: %w", err)
	}
	defer tdb.Close()

	txTemporal, err := tdb.BeginTemporalRw(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin temporal tx: %w", err)
	}
	defer txTemporal.Rollback()

	// First, check if genesis already exists
	storedHash, storedErr := rawdb.ReadCanonicalHash(txTemporal, 0)
	if storedErr != nil {
		return nil, nil, storedErr
	}

	if (storedHash != common.Hash{}) {
		// Genesis already exists
		logger.Info("Genesis block already exists", "hash", storedHash.Hex())
		block := rawdb.ReadBlock(txTemporal, storedHash, 0)
		if block == nil {
			return nil, nil, fmt.Errorf("genesis block not found: %s", storedHash.Hex())
		}
		return genesis.Config, block, nil
	}

	// Create the genesis block header
	head, withdrawals := genesiswrite.GenesisWithoutStateToBlock(genesis)

	logger.Info("Writing genesis state with persistent storage", "accounts", len(genesis.Alloc))

	// Create SharedDomains for state management
	sd, err := execctx.NewSharedDomains(txTemporal, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("create shared domains: %w", err)
	}
	defer sd.Close()

	blockNum := uint64(0)
	txNum := uint64(1)

	sd.SetBlockNum(blockNum)
	sd.SetTxNum(txNum)

	// Sort addresses for deterministic processing
	addrs := make([]common.Address, 0, len(genesis.Alloc))
	for addr := range genesis.Alloc {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i][:], addrs[j][:]) < 0
	})

	// Write each account to the domains
	logger.Info("Writing genesis accounts to domains...")
	for _, addr := range addrs {
		account := genesis.Alloc[addr]

		balance, overflow := uint256.FromBig(account.Balance)
		if overflow {
			return nil, nil, fmt.Errorf("balance overflow for account %s", addr.Hex())
		}

		// Create account entry
		acc := accounts.Account{
			Nonce:   account.Nonce,
			Balance: *balance,
		}

		// Encode and store account
		encodedAccount := accounts.SerialiseV3(&acc)

		err = sd.DomainPut(kv.AccountsDomain, txTemporal, addr[:], encodedAccount, txNum, nil, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to put account %s: %w", addr.Hex(), err)
		}

		// Store code if present
		if len(account.Code) > 0 {
			err = sd.DomainPut(kv.CodeDomain, txTemporal, addr[:], account.Code, txNum, nil, 0)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to put code for %s: %w", addr.Hex(), err)
			}
		}

		// Store storage if present
		if len(account.Storage) > 0 {
			for key, value := range account.Storage {
				composite := append(addr[:], key[:]...)
				err = sd.DomainPut(kv.StorageDomain, txTemporal, composite, value.Bytes(), txNum, nil, 0)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to put storage for %s: %w", addr.Hex(), err)
				}
			}
		}
	}

	logger.Info("Computing genesis state commitment...")

	// Compute state root
	rh, err := sd.ComputeCommitment(ctx, txTemporal, true, blockNum, txNum, "genesis", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("compute commitment: %w", err)
	}
	head.Root = common.BytesToHash(rh)

	// Flush domains to persist state
	logger.Info("Flushing genesis state to database...")
	err = sd.Flush(ctx, txTemporal)
	if err != nil {
		return nil, nil, fmt.Errorf("flush domains: %w", err)
	}

	// Create the genesis block
	block := types.NewBlock(head, nil, nil, nil, withdrawals)

	// Write genesis block metadata
	config := genesis.Config
	if config == nil {
		config = chain.AllProtocolChanges
	}

	if err := rawdb.WriteBlock(txTemporal, block); err != nil {
		return nil, nil, fmt.Errorf("write block: %w", err)
	}
	if err := rawdb.WriteTd(txTemporal, block.Hash(), block.NumberU64(), genesis.Difficulty); err != nil {
		return nil, nil, fmt.Errorf("write td: %w", err)
	}
	if err := rawdbv3.TxNums.Append(txTemporal, 0, uint64(block.Transactions().Len()+1)); err != nil {
		return nil, nil, fmt.Errorf("write txnums: %w", err)
	}
	if err := rawdb.WriteCanonicalHash(txTemporal, block.Hash(), block.NumberU64()); err != nil {
		return nil, nil, fmt.Errorf("write canonical hash: %w", err)
	}
	rawdb.WriteHeadBlockHash(txTemporal, block.Hash())
	if err := rawdb.WriteHeadHeaderHash(txTemporal, block.Hash()); err != nil {
		return nil, nil, fmt.Errorf("write head header: %w", err)
	}
	if err := rawdb.WriteChainConfig(txTemporal, block.Hash(), config); err != nil {
		return nil, nil, fmt.Errorf("write chain config: %w", err)
	}

	// Commit transaction
	logger.Info("Committing genesis block to database...")
	err = txTemporal.Commit()
	if err != nil {
		return nil, nil, fmt.Errorf("commit tx: %w", err)
	}

	logger.Info("Genesis block committed successfully",
		"hash", block.Hash().Hex(),
		"root", block.Root().Hex(),
		"accounts", len(genesis.Alloc))

	return config, block, nil
}

// verifyGenesisAccounts verifies all genesis account balances and nonces by reading from the database
func verifyGenesisAccounts(cliCtx *cli.Context, gen *types.Genesis) error {
	logger := log.New()

	logger.Info("Starting genesis account verification", "totalAccounts", len(gen.Alloc))

	// Open database for reading
	stack, err := MakeNodeWithDefaultConfig(cliCtx, logger)
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()

	chaindb, err := node.OpenDatabase(cliCtx.Context, stack.Config(), dbcfg.ChainDB, "", false, logger)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer chaindb.Close()

	ctx := context.Background()

	// Try to read from PlainState table first (most reliable for genesis)
	tx, err := chaindb.BeginRo(ctx)
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer tx.Rollback()

	// Sort addresses for deterministic output
	addrs := make([]common.Address, 0, len(gen.Alloc))
	for addr := range gen.Alloc {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i][:], addrs[j][:]) < 0
	})

	verified := 0
	failed := 0
	totalBalance := new(big.Int)

	for _, addr := range addrs {
		expected := gen.Alloc[addr]

		// Try reading from PlainState
		acc, err := readPlainStateAccount(tx, addr)
		if err != nil {
			logger.Error("Failed to read account", "address", addr.Hex(), "error", err)
			failed++
			continue
		}

		if acc == nil {
			logger.Warn("Genesis account not found in database",
				"address", addr.Hex(),
				"expectedBalance", expected.Balance,
				"expectedNonce", expected.Nonce)
			failed++
			continue
		}

		// Verify nonce
		if acc.Nonce != expected.Nonce {
			logger.Error("Genesis nonce mismatch",
				"address", addr.Hex(),
				"expected", expected.Nonce,
				"got", acc.Nonce)
			failed++
			continue
		}

		// Verify balance
		gotBal := acc.Balance.ToBig()
		if expected.Balance != nil && gotBal.Cmp(expected.Balance) != 0 {
			logger.Error("Genesis balance mismatch",
				"address", addr.Hex(),
				"expected", expected.Balance.String(),
				"got", gotBal.String())
			failed++
			continue
		}

		verified++
		totalBalance.Add(totalBalance, gotBal)

		// Log details for first few and last few accounts
		if verified <= 5 || verified > len(addrs)-5 {
			logger.Info("Genesis account verified",
				"address", addr.Hex(),
				"balance", gotBal.String(),
				"nonce", acc.Nonce)
		}
	}

	if failed > 0 {
		return fmt.Errorf("genesis verification failed: %d accounts verified, %d failed", verified, failed)
	}

	logger.Info("Genesis account verification complete",
		"totalAccounts", verified,
		"totalBalance", totalBalance.String(),
		"allVerified", true)

	return nil
}

func readPlainStateAccount(tx kv.Tx, addr common.Address) (*accounts.Account, error) {
	// PlainState key is 20-byte address.
	enc, err := tx.GetOne(kv.PlainState, addr[:])
	if err != nil {
		return nil, err
	}
	if len(enc) == 0 {
		return nil, nil
	}

	var out accounts.Account
	if err := out.DecodeForStorage(enc); err != nil {
		return nil, err
	}
	return &out, nil
}

func main() {
	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
