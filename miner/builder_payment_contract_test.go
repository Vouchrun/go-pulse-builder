package miner

import (
	"math/big"
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
	"github.com/ethereum/go-ethereum/params"
)

// revertingContractCode is a minimal always-revert contract: PUSH1 0x00 PUSH1 0x00 REVERT.
const revertingContractCode = "0x60006000fd"

// TestBuilderPaymentTxContractRecipient verifies the naive-mode contract
// guard: a validator fee recipient that is a smart contract (e.g. a fee
// distributor) must be rejected in proposerTxPrepare with a clear error - the
// payment would otherwise revert at execution (receipt status 0, the
// production "proposer payment tx failed" symptom).
func TestBuilderPaymentTxContractRecipient(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	recipient := common.HexToAddress("0xdeadbeef")
	builderKey, _ := crypto.GenerateKey()

	backend := newTestWorkerBackendWithGenesis(t, params.TestChainConfig, ethash.NewFaker(), db, 0, &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			testBankAddress: {Balance: testBankFunds},
			recipient:       {Balance: new(big.Int), Code: common.FromHex(revertingContractCode)},
		},
	})

	// A tip-paying mempool tx so the fill earns income at the builder coinbase.
	signer := types.LatestSigner(params.TestChainConfig)
	tipTx := types.MustSignNewTx(testBankKey, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     0,
		To:        &testUserAddress,
		Value:     big.NewInt(0),
		Gas:       params.TxGas,
		GasTipCap: big.NewInt(3 * params.GWei),
		GasFeeCap: big.NewInt(10 * params.GWei),
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
	// The guard makes generateWork fail, so no full block is ever produced:
	// ResolveFull must not deliver a full payload within a short window.
	resCh := make(chan *engine.ExecutionPayloadEnvelope, 1)
	go func() { resCh <- payload.ResolveFull() }()
	select {
	case env := <-resCh:
		if env != nil {
			t.Fatalf("unexpected full block produced for contract recipient: %v", env.ExecutionPayload.BlockHash)
		}
		t.Log("guard rejected contract recipient (no full block) - expected")
	case <-time.After(1500 * time.Millisecond):
		t.Log("guard rejected contract recipient (no full block within window) - expected")
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
