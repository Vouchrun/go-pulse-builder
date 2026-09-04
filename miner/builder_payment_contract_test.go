package miner

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// nonRevertingReceiveCode is a minimal VFD-like fee-distributor receive: it
// accepts the transfer and does state work (SSTORE(1,1)) so the payment costs
// more than the 21000 EOA base gas. (A receive that is a bare STOP costs
// exactly 21000 - the 9000 value-transfer / 2600 cold-access surcharges apply
// only to CALL instructions, not the transaction's own top-level call.)
const nonRevertingReceiveCode = "0x6001600155"

// revertingContractCode is a minimal always-revert contract: PUSH1 0x00 PUSH1 0x00 REVERT.
const revertingContractCode = "0x60006000fd"

// TestBuilderPaymentTxContractRecipientNonReverting verifies the contract
// fee-recipient path (e.g. VFD): a value transfer to a contract with a
// non-reverting receive() succeeds, is the LAST tx, uses more than the 21000
// EOA base gas (measured by simulation), and its value equals the tips the
// builder earned minus the payment's own base-fee burn.
func TestBuilderPaymentTxContractRecipientNonReverting(t *testing.T) {
	var buf bytes.Buffer
	oldLogger := log.Root()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(&buf, log.LevelInfo, false)))
	defer log.SetDefault(oldLogger)

	db := rawdb.NewMemoryDatabase()
	recipient := common.HexToAddress("0xc0ffee0000000000000000000000000000000001")

	builderKey, _ := crypto.GenerateKey()
	builderAddr := crypto.PubkeyToAddress(builderKey.PublicKey)

	backend := newTestWorkerBackendWithGenesis(t, params.TestChainConfig, ethash.NewFaker(), db, 0, &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			testBankAddress: {Balance: testBankFunds},
			recipient:       {Balance: new(big.Int), Code: common.FromHex(nonRevertingReceiveCode)},
		},
	})

	// A mempool tx with a positive tip so the builder's coinbase earns income
	// during the fill: tip 10 GWei, 21000 gas -> tips = 210 GWei (must exceed
	// the provisional payment cost at the 100000 contract gas cap so the
	// payment simulation runs).
	signer := types.LatestSigner(params.TestChainConfig)
	tipTx := types.MustSignNewTx(testBankKey, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     0,
		To:        &testUserAddress,
		Value:     big.NewInt(0),
		Gas:       params.TxGas,
		GasTipCap: big.NewInt(10 * params.GWei),
		GasFeeCap: big.NewInt(20 * params.GWei),
	})
	backend.txPool.Add([]*types.Transaction{tipTx}, true)

	cfg := testConfig
	cfg.BuilderTxSigningKey = builderKey
	w := New(backend, cfg, ethash.NewFaker())

	var (
		gotBlock *types.Block
		gotValue *big.Int
	)
	args := &BuildPayloadArgs{
		Parent:       backend.chain.CurrentBlock().Hash(),
		Timestamp:    uint64(time.Now().Unix()),
		Random:       common.Hash{},
		FeeRecipient: recipient,
		GasLimit:     params.GenesisGasLimit,
		BlockHook: func(block *types.Block, blockValue *big.Int, _ []*types.BlobTxSidecar, _ time.Time, _, _ []types.SimulatedBundle, _ []types.UsedSBundle) {
			gotBlock = block
			gotValue = new(big.Int).Set(blockValue)
		},
	}
	payload, err := w.BuildPayload(args, false)
	if err != nil {
		t.Fatalf("Failed to build payload: %v", err)
	}

	resCh := make(chan *engine.ExecutionPayloadEnvelope, 1)
	go func() {
		resCh <- payload.ResolveFull()
	}()
	var env *engine.ExecutionPayloadEnvelope
	select {
	case env = <-resCh:
	case <-time.After(12 * time.Second):
		t.Fatal("timed out waiting for full payload")
	}
	if env == nil {
		t.Fatal("got nil payload envelope")
	}
	if env.ExecutionPayload.FeeRecipient != builderAddr {
		t.Fatalf("block coinbase = %s, want builder %s", env.ExecutionPayload.FeeRecipient, builderAddr)
	}
	if gotBlock == nil {
		t.Fatal("block hook was never invoked")
	}

	txs := gotBlock.Transactions()
	if len(txs) < 2 {
		t.Fatalf("expected mempool + payment txs, got %d", len(txs))
	}
	lastTx := txs[len(txs)-1]
	baseFee := gotBlock.BaseFee()
	if lastTx.To() == nil || *lastTx.To() != recipient {
		t.Fatalf("last tx pays %v, want %v", lastTx.To(), recipient)
	}
	if lastTx.Gas() <= params.TxGas {
		t.Logf("--- builder log ---\n%s", buf.String())
		t.Fatalf("contract payment gas = %d, want > %d (EOA base)", lastTx.Gas(), params.TxGas)
	}
	// Relay invariants: tip 0, fee cap = base fee, no calldata.
	if lastTx.GasTipCap().Sign() != 0 {
		t.Fatalf("payment tx tip = %v, want 0", lastTx.GasTipCap())
	}
	if lastTx.GasFeeCap().Cmp(baseFee) != 0 {
		t.Fatalf("payment tx fee cap = %v, want base fee %v", lastTx.GasFeeCap(), baseFee)
	}
	if len(lastTx.Data()) != 0 {
		t.Fatalf("payment tx carries calldata")
	}
	// payment = tips the builder earned from the mempool fill - the payment
	// tx's own base-fee cost at the SIMULATED gas (which is the tx gas).
	tips := new(big.Int)
	for i, tx := range txs[:len(txs)-1] {
		tip, _ := tx.EffectiveGasTip(baseFee)
		tips.Add(tips, new(big.Int).Mul(tip, new(big.Int).SetUint64(txs[i].Gas())))
	}
	expectedPayment := new(big.Int).Sub(tips, new(big.Int).Mul(baseFee, new(big.Int).SetUint64(lastTx.Gas())))
	if lastTx.Value().Cmp(expectedPayment) != 0 {
		t.Fatalf("payment value = %v, want %v (tips %v, baseFee %v, gas %d)", lastTx.Value(), expectedPayment, tips, baseFee, lastTx.Gas())
	}
	if gotValue.Cmp(expectedPayment) != 0 {
		t.Fatalf("bid value from hook = %v, want payment %v", gotValue, expectedPayment)
	}
}

// TestBuilderPaymentTxContractRecipientReverts verifies the failure path for a
// fee recipient whose receive() reverts (or a precompile with empty input):
// the payment simulation fails, no bid is produced (no full block), and the
// logged error carries the diagnostics (recipient, amounts, balances, gas).
func TestBuilderPaymentTxContractRecipientReverts(t *testing.T) {
	var buf bytes.Buffer
	oldLogger := log.Root()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(&buf, log.LevelInfo, false)))
	defer log.SetDefault(oldLogger)

	db := rawdb.NewMemoryDatabase()
	recipient := common.HexToAddress("0xc0ffee0000000000000000000000000000000002")
	builderKey, _ := crypto.GenerateKey()

	backend := newTestWorkerBackendWithGenesis(t, params.TestChainConfig, ethash.NewFaker(), db, 0, &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			testBankAddress: {Balance: testBankFunds},
			recipient:       {Balance: new(big.Int), Code: common.FromHex(revertingContractCode)},
		},
	})

	// A tip-paying mempool tx so the fill earns income at the builder coinbase
	// (tip 10 GWei -> 210 GWei, above the provisional 100000-gas cost so the
	// payment simulation actually executes and hits the reverter).
	signer := types.LatestSigner(params.TestChainConfig)
	tipTx := types.MustSignNewTx(testBankKey, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     0,
		To:        &testUserAddress,
		Value:     big.NewInt(0),
		Gas:       params.TxGas,
		GasTipCap: big.NewInt(10 * params.GWei),
		GasFeeCap: big.NewInt(20 * params.GWei),
	})
	backend.txPool.Add([]*types.Transaction{tipTx}, true)

	cfg := testConfig
	cfg.BuilderTxSigningKey = builderKey
	w := New(backend, cfg, ethash.NewFaker())

	args := &BuildPayloadArgs{
		Parent:       backend.chain.CurrentBlock().Hash(),
		Timestamp:    uint64(time.Now().Unix()),
		Random:       common.Hash{},
		FeeRecipient: recipient,
		GasLimit:     params.GenesisGasLimit,
	}
	payload, err := w.BuildPayload(args, false)
	if err != nil {
		t.Fatalf("Failed to build payload: %v", err)
	}

	// The payment simulation reverts, so generateWork fails and no full block
	// is ever produced: ResolveFull must not deliver a full payload within a
	// short window.
	resCh := make(chan *engine.ExecutionPayloadEnvelope, 1)
	go func() { resCh <- payload.ResolveFull() }()
	select {
	case env := <-resCh:
		if env != nil {
			t.Fatalf("unexpected full block produced for reverting recipient: %v", env.ExecutionPayload.BlockHash)
		}
	case <-time.After(1500 * time.Millisecond):
	}

	// The failure must carry full diagnostics, not a bare "failed" message.
	out := buf.String()
	for _, want := range []string{"proposer payment", "simulation reverted", recipient.Hex()} {
		if !strings.Contains(out, want) {
			t.Fatalf("builder log does not contain %q\n--- log ---\n%s", want, out)
		}
	}
}

func newTestWorkerBackendWithGenesis(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine, db ethdb.Database, n int, gspec *core.Genesis) *testWorkerBackend {
	chain, err := core.NewBlockChain(db, gspec, engine, &core.BlockChainConfig{ArchiveMode: true})
	if err != nil {
		t.Fatalf("core.NewBlockChain failed: %v", err)
	}
	pool := legacypool.New(testTxPoolConfig, chain)
	txpool, _ := txpool.New(testTxPoolConfig.PriceLimit, chain, []txpool.SubPool{pool})
	return &testWorkerBackend{
		db:      db,
		chain:   chain,
		txPool:  txpool,
		genesis: gspec,
	}
}
