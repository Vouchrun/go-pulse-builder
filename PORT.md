# PORT.md - flashbots block-validation slice onto the official go-pulse EL

This repo is the **official go-pulse client** (`gitlab.com/pulsechaincom/go-pulse`, v3.3.0,
geth 1.15 lineage, go 1.23) with the **flashbots block-validation namespace** ported on top,
for use as the relay's block-validation node. This replaces the abandoned approach (PulseChain
ported onto `flashbots/builder` geth 1.13.14), which hit three era-boundary sync walls.

go-pulse already provides everything PulseChain: the native `--pulsechain` flag, chain config
(chain 369, PrimordialPulseBlock), sacrifice credits, and the sync corrections. We add ONLY the
validation slice.

## What was ported

| File | Source (old fork) | Notes |
|---|---|---|
| `eth/block-validation/api.go` | `builder/eth/block-validation/api.go` | `flashbots` namespace: `ValidateBuilderSubmissionV1/V2` full; V3 returns `unsupported on PulseChain` (Deneb disabled) |
| `eth/block-validation/api_test.go` | `builder/eth/block-validation/api_test.go` | Adapted to 1.15 test infra |
| `core/blockchain.go` `ValidatePayload` | `builder/core/blockchain.go` | Proposer-payment + balance-diff validation |
| `beacon/engine/types.go` `ExecutionPayloadV1ToBlock`/`V2ToBlock` | `builder/beacon/engine/types.go` | attestantio payloads -> blocks |
| `eth/ethconfig/config.go` | - | 3 config fields |
| `cmd/utils/flags.go` | `builder/cmd/utils/flags.go` | 3 flags + wiring + registration |
| `cmd/geth/main.go` | - | flags appended to nodeFlags |
| `go.mod`/`go.sum` | - | attestantio deps added |
| `ops/validation-node/docker-compose.yml`, `docs/validation-node-runbook.md` | `builder/` | Simplified (official client) |

## 1.13 -> 1.15 API adaptations (mechanical, no behavior change)

- `vm.Config.Tracer` is now `*tracing.Hooks`; AccessListTracer exposes `Hooks()`.
- `logger.NewAccessListTracer` is 2-arg (`acl`, `addressesToExclude`) instead of 4-arg.
- `StateProcessor.Process` returns `(*ProcessResult, error)` (Receipts/GasUsed on the struct).
- `BlockValidator.ValidateState` takes `(block, statedb, res *ProcessResult, stateless bool)`.
- `BlockChain.CurrentBlock()` returns `*types.Header` (not `*types.Block`).
- `Engine.VerifyHeader(chain, header, parent)` takes the parent (was fetched internally).
- **ForkChoice/`ReorgNeeded` removed** in 1.15; `ValidatePayload`'s reorg guard replaced with a
  `HasBlock` check (the old guard never fired for fresh builder submissions - GetTd nil).
- `ExecutableDataToBlock` is 4-arg (`data, versionedHashes, beaconRoot, requests`).
- `BlockToExecutableData` is 4-arg (added `requests`).
- `FinalizeAndAssemble` takes a `*types.Body` (Transactions+Withdrawals).
- `core.ApplyTransaction` takes an `*vm.EVM` (built via `NewEVMBlockContext`+`vm.NewEVM`).
- `TxPool.Add(txs, sync)` is 2-arg (was 4-arg).
- `Merger`/`SetEtherbase` removed (no runtime merger; fee recipient from payload attrs).
- `executionPayloadV1/V2ToBlock` adapted to the 1.15 engine package.

## Flags / config

- `--builder.validation_use_balance_diff` -> `cfg.ValidationUseBalanceDiff`
- `--builder.validation_exclude_withdrawals` -> `cfg.ValidationExcludeWithdrawals`
- `--builder.blacklist=<path>` -> `cfg.BlacklistSourceFilePath`
- Registration: `RegisterEthService` (cmd/utils/flags.go) calls
  `blockvalidation.Register(stack, backend, cfg)` after `eth.New`.

## Test results

`go build ./...` **PASSED**. `go test ./eth/block-validation/ -count=1` — **8/8 PASS**:

| Test | Result |
|---|---|
| `TestValidateBuilderSubmissionV1` | PASS |
| `TestValidateBuilderSubmissionV2` | PASS |
| `TestValidateBuilderSubmissionV3` | PASS (unsupported on PulseChain) |
| `TestBlacklistLoad` | PASS |
| `TestValidateBuilderSubmissionV2_CoinbasePaymentUnderflow` | PASS |
| `TestValidateBuilderSubmissionV2_CoinbasePaymentDefault` | PASS |
| `TestValidateBuilderSubmissionV2_Blocklist` | PASS |
| `TestValidateBuilderSubmissionV2_ExcludeWithdrawals` | PASS |

**Miner-difference note (why V1/V2 needed rework):** the stock go-pulse miner does **not** add a
proposer-payment tx to the fee recipient (the old flashbots builder miner did). The ported
V1/V2 tests therefore assemble the block explicitly via `buildBlock` with an EIP-1559 payment
tx (tip 0, fee cap = base fee, signed by the builder key) as the last transaction, and use
`useBalanceDiffProfit=false` so the wrong-value assertion reaches the last-tx payment check
(the balance-diff path is covered by the `CoinbasePayment*`/`ExcludeWithdrawals` tests).
Additionally the test node binds to loopback only (`127.0.0.1:0`, `NoDial`, `MaxPeers: 0`) so
tests never request external network access.

## What remains (server-side)

- Build the image, bring up the stack, and smoke-test `flashbots_validateBuilderSubmissionV2`
  with a real Capella-era PulseChain block payload on val002 (see
  `docs/validation-node-runbook.md`).

---

# Phase 4 - the flashbots BUILDER module (naive mode)

The same EL now also runs in **builder mode** (`--builder`): it builds blocks from its own
mempool on payload attributes, adds a proposer-payment tx, signs the bid, and submits it to
our relay. Naive = **no searcher bundle ingestion yet** (Phase 3 / milestone 2).

## What was ported (dependency order)

| Package | Files | Notes |
|---|---|---|
| `flashbotsextra/` | `database.go`, `database_types.go`, `rpc_block_service.go` | `BlockConsumer` (rpc block client / Nil), `IDatabaseService` (Nil), Postgres `DatabaseService` |
| `core/types` | `builder.go` (BuilderPayloadAttributes), `sbundle.go` (SBundle/UsedSBundle), `bundle.go` (MevBundle/SimulatedBundle/LatestUuidBundle/TimestampedTxHashSet) | pure data types; ingestion machinery deferred |
| `miner/` | `miner.go`, `worker.go`, `payload_building.go` | builder-mode: `BuildPayloadArgs.BlockHook` + proposer-payment tx + bid-value semantics |
| `builder/` | all 12 non-test files + 6 test files | payload building per slot, bid signing, relay/local-relay submission, beacon SSE |
| `cmd/` | `cmd/geth/main.go`, `cmd/geth/config.go`, `cmd/utils/flags.go`, `internal/flags/categories.go` | `--builder.*` flags, `SetBuilderConfig`, `RegisterEthService -> builder.Register` |
| `go.mod` | +`cenkalti/backoff/v4`, `flashbots/go-utils`, `gorilla/mux`, `r3labs/sse`, `jmoiron/sqlx`, `lib/pq` | |

## Builder-mode architecture (adapted to 1.15)

The flashbots worker's coinbase model is kept (compatible with our relay: the relay checks the
bid trace's `proposer_fee_recipient` against the validator registration, and `ValidatePayload`
checks the **last tx** is a successful payment to that recipient with value == bid value):

- `generateWork`: with `BuilderTxSigningKey` set, the block **coinbase = the builder's address**
  (`miner.builderCoinbase()`); the payload-attributes fee recipient (the validator) is captured
  as `validatorCoinbase` before the swap.
- The mempool fill earns tips at the builder coinbase; then `proposerTxPrepare` (records the
  builder balance + reserves `params.TxGas`) / `proposerTxCommit` (pays out `availableFunds =
  balance_after - balance_before`, i.e. the tips earned in this block, minus the payment tx's
  own base fee) append the payment tx: EIP-1559, `GasTipCap=0`, `GasFeeCap=baseFee`, `To=validator
  coinbase`. **EOA recipients only** - the contract-recipient gas-estimation path needs the
  env-diff machinery (milestone 2).
- **Bid value = the payment tx value** (flashbots `checkProposerPayment` semantics), reported
  through the block hook and kept by `Payload.update` as the highest-value block. This matches
  the relay's `ValidatePayload` requirement `paymentTx.Value() == expectedProfit`.
- `Payload.update` fires the `BlockHook` on each newly sealed, higher-value full block
  (`BuildPayloadArgs.BlockHook`, wired via `newPayload`).
- `BuildPayload` stays 2-arg (`args, witness`); the builder's `EthereumService.BuildBlock` calls
  `Miner().BuildPayload(args, false)`. `BuildPayloadArgs` gained `GasLimit` (from the validator's
  registered gas limit via `core.CalcGasLimit`) and `BlockHook`.

## 1.13 -> 1.15 adaptations (Phase 4)

- `engine.BlockToExecutableData(block, value, sidecars, nil)` - 4-arg (added `requests`).
- `engine.ExecutableDataToBlock(data, nil, nil, nil)` in tests - 4-arg.
- `Miner().BuildPayload(args, witness)` - 2-arg (was 1-arg); `Payload` has no `Cancel` (the
  builder's timeout path just returns; the payload's own end-timer stops it).
- Gas pool is lazily created in 1.15 `commitTransactions`; `proposerTxPrepare` now initializes it.
- The 1.15 miner is a single worker (no flashbots `multiWorker`/algo machinery - deferred).

## Flags / config (builder mode)

`--builder` + (all under BUILDER category in `geth --help`): `--builder.secret_key`
(BUILDER_SECRET_KEY, BLS bid-signing key - no default, fail-fast), `--builder.relay_secret_key`
(local relay), `--builder.beacon_endpoints`, `--builder.remote_relay_endpoint` (+
`--builder.secondary_remote_relay_endpoints`), `--builder.local_relay`, `--builder.listen_addr`,
`--builder.dry-run`, `--builder.validator_checks`, `--builder.no_bundle_fetcher`,
`--builder.genesis_fork_version`, `--builder.bellatrix_fork_version`,
`--builder.genesis_validators_root`, `--builder.slots_in_epoch`, `--builder.seconds_in_slot`,
rate-limit + submission-offset flags, `--builder.block_processor_url`, plus the validation
flags (already present from Phase 3/Task 18). The proposer-payment ECDSA key is **env-only**:
`BUILDER_TX_SIGNING_KEY` (flashbots convention; parsed in `miner.New`).

PulseChain values (verified, see mev-boost-relay pulsechain-values.md): genesis_fork_version
`0x00000369`, bellatrix_fork_version `0x0000036b`, genesis_validators_root
`0x3357ba0018a2582aeabe4ae847aa17d50a3a99aaeb66293c01f80a83aecd0c90`, slots_in_epoch 32,
seconds_in_slot 10 (Capella-era bids, Deneb disabled).

## Naive-mode startup (val002, on mev-net)

```bash
geth --pulsechain --datadir=/buildertest/validation-data --syncmode=snap \
  --builder \
  --builder.secret_key=$BUILDER_SECRET_KEY \
  --builder.remote_relay_endpoint=http://relay-api:9062 \
  --builder.beacon_endpoints=http://validation-consensus:5052 \
  --builder.genesis_fork_version=0x00000369 \
  --builder.bellatrix_fork_version=0x0000036b \
  --builder.genesis_validators_root=0x3357ba0018a2582aeabe4ae847aa17d50a3a99aaeb66293c01f80a83aecd0c90 \
  --builder.seconds_in_slot=10 \
  --builder.slots_in_epoch=32
# BUILDER_TX_SIGNING_KEY (ECDSA) must be in the environment for the proposer-payment tx.
```

## Deferred to milestone 2 (searcher bundle ingestion)

- `internal/ethapi` sbundle API + `miner` bundle machinery (algo_*, bundle_cache, env_changes,
  environment_diff, multi_worker, verify_bundles), `flashbotsextra/fetcher.go`, the
  contract-recipient payment gas-estimation path, `--builder.algotype` / price-cutoff flags.

## Test results (Phase 4)

- `go build ./...` **PASSED**.
- `go test ./miner/` **PASSED** incl. new `TestBuilderPaymentTx` (payment tx is last, tip 0,
  fee cap = base fee, value = tips - base-fee*gas; bid value == payment; coinbase = builder)
  and `TestBuildPayloadNoPaymentTx` (validation-mode behavior unchanged).
- `go test ./builder/` **PASSED** (13s; relay/local-relay/resubmit/beacon tests adapted to 1.15).
- `go test ./core/... ./eth/block-validation/ ./beacon/engine/` **PASSED**.
- Pre-existing env failures only: `cmd/geth` `TestAttachWelcome`/`TestConsoleWelcome`
  (interactive console) + `TestVerification` (minisign) - fail identically on the pre-Task-20
  baseline.

