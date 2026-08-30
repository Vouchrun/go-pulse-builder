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
