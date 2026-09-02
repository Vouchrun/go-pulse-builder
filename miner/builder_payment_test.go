package miner

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// TestBuilderPaymentTx verifies the flashbots proposer-payment flow on the
// 1.15 miner: with a BuilderTxSigningKey set, the built block's coinbase is
// the builder's address and its LAST transaction is a successful EIP-1559
// value transfer to the payload-attributes fee recipient, whose value equals
// the tips the builder earned during the fill minus the payment tx's own base
// fee. The bid value reported through the block hook must equal that payment.
func TestBuilderPaymentTx(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	recipient := common.HexToAddress("0xdeadbeef") // "validator" fee recipient

	builderKey, _ := crypto.GenerateKey()
	builderAddr := crypto.PubkeyToAddress(builderKey.PublicKey)

	backend := newTestWorkerBackend(t, params.TestChainConfig, ethash.NewFaker(), db, 0)

	// A mempool tx with a positive tip so the builder's coinbase earns income
	// during the fill: tip 3 GWei, 21000 gas -> tips = 63 GWei.
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

	// ResolveFull may block forever if the full block never builds (e.g. the
	// payment tx fails), so guard it with a timeout.
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
	// payment = tips the builder earned from the mempool fill (mempool txs'
	// effective tips) - the payment tx's own base-fee cost.
	tips := new(big.Int)
	for i, tx := range txs[:len(txs)-1] {
		tip, _ := tx.EffectiveGasTip(baseFee)
		tips.Add(tips, new(big.Int).Mul(tip, new(big.Int).SetUint64(txs[i].Gas())))
	}
	expectedPayment := new(big.Int).Sub(tips, new(big.Int).Mul(baseFee, new(big.Int).SetUint64(lastTx.Gas())))
	if lastTx.Value().Cmp(expectedPayment) != 0 {
		t.Fatalf("payment value = %v, want %v (tips %v, baseFee %v)", lastTx.Value(), expectedPayment, tips, baseFee)
	}
	if gotValue.Cmp(expectedPayment) != 0 {
		t.Fatalf("bid value from hook = %v, want payment %v", gotValue, expectedPayment)
	}
}

// TestBuildPayloadNoPaymentTx verifies that without a builder signing key the
// block coinbase is the payload-attributes fee recipient and no payment tx is
// added (validation-node behavior is unchanged).
func TestBuildPayloadNoPaymentTx(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	recipient := common.HexToAddress("0xdeadbeef")
	w, b := newTestWorker(t, params.TestChainConfig, ethash.NewFaker(), db, 0)

	args := &BuildPayloadArgs{
		Parent:       b.chain.CurrentBlock().Hash(),
		Timestamp:    uint64(time.Now().Unix()),
		Random:       common.Hash{},
		FeeRecipient: recipient,
	}
	payload, err := w.BuildPayload(args, false)
	if err != nil {
		t.Fatalf("Failed to build payload: %v", err)
	}
	env := payload.ResolveFull()
	if env.ExecutionPayload.FeeRecipient != recipient {
		t.Fatalf("block coinbase = %s, want fee recipient %s", env.ExecutionPayload.FeeRecipient, recipient)
	}
	if len(env.ExecutionPayload.Transactions) != len(pendingTxs) {
		t.Fatalf("unexpected tx set: %d, want %d", len(env.ExecutionPayload.Transactions), len(pendingTxs))
	}
}
