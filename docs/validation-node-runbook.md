# Validation Node Runbook - PulseChain block-validation node (official go-pulse + flashbots validation slice)

**Audience:** the owner, executing by hand on the voter/relay server.
**Goal:** bring up a **second** execution client as the relay's block-validation node, serving
the `flashbots` RPC namespace, **without touching the production geth** already running on the
same host.
**Source of truth:** this repo's `pulse` branch. The EL is the **official go-pulse client**
(`gitlab.com/pulsechaincom/go-pulse`, v3.3.0) with the flashbots block-validation namespace
(`eth/block-validation`, `core.BlockChain.ValidatePayload`) ported on top. This replaces the
abandoned flashbots/builder geth 1.13.14 fork (see `PORT.md`).

> **Fresh-sync requirement:** this image uses the official go-pulse client and its native
> `--pulsechain` sync. It must be brought up with a **fresh datadir** - it cannot be pointed at
> a datadir produced by the old flashbots/builder fork (the chain config, sync corrections and
> the 1.13.14-era datadir layout are incompatible).

---

## 1. Prerequisites

1. **Image:** build the EL image from this repo (`pulse` branch):
   ```
   docker build -t vouchrun/go-pulse-builder:pulse-<commit> .
   ```
   This is the official go-pulse client with the `flashbots` namespace registered
   (`builder.validation_*` flags + `flashbots_validateBuilderSubmissionV2`).
2. **Datadir:** a fresh bind-mount path (e.g. `/buildertest/validation-data`), 1TB+ NVMe free
   (PulseChain includes Ethereum mainnet state through the PrimordialPulse fork block).
3. **JWT secret** (shared by EL + CL):
   ```
   mkdir -p /buildertest/validation-data
   openssl rand -hex 32 > /buildertest/validation-data/jwt.hex
   ```
4. **Shared docker network** with the relay stack (create once):
   ```
   docker network create mev-net
   ```

> The production geth is **never touched**: separate datadir, separate ports, separate
> container. Rollback is delete-the-container + delete-the-datadir (see 7).

---

## 2. Bring up

The stack is `ops/validation-node/docker-compose.yml` (EL + Lighthouse-Pulse CL):

```
docker compose -f ops/validation-node/docker-compose.yml up -d
```

- EL (`validation-node`): `--pulsechain` (native), snap sync, HTTP RPC on
  `127.0.0.1:18546` with `eth,net,web3,flashbots,debug`, authrpc on 8552 (internal), metrics on
  6060 (host 6061). P2P on 30307 (offset from production 30303).
- CL (`validation-consensus`): Lighthouse-Pulse, beacon API on `127.0.0.1:5053` (production
  uses 5052), checkpoint-sync from `https://checkpoint.pulsechain.com`. No validator attaches
  to this stack.
- Both join the external `mev-net` network so the relay reaches them by container name
  (`validation-node:18546`, `validation-consensus:5052`).

---

## 3. Sync

1. Wait for snap sync to complete:
   ```
   docker logs -f validation-node | grep -E "Syncing|Imported new block|State sync"
   ```
   Target: the state sync finishes and the node reaches the chain head (~10s slots).
2. Confirm the CL is paired and the EL head advances with the beacon head:
   ```
   curl -s http://127.0.0.1:5053/eth/v2/debug/beacon/heads
   curl -s -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' http://127.0.0.1:18546
   ```
   The EL number should track the beacon head (minus ~1 slot).

---

## 4. Verify the flashbots namespace

1. Confirm the `flashbots` namespace is registered:
   ```
   curl -s -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"rpc_modules","params":[]}' http://127.0.0.1:18546
   # expect ..."flashbots":"1.0"...
   ```
2. Smoke-test the live path the relay calls (V2, Capella):
   ```
   curl -s -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"flashbots_validateBuilderSubmissionV2","params":[]}' http://127.0.0.1:18546
   # expect a clean decode/validation error, NOT "method not found"
   ```
   A real Capella block payload is the full smoke test (relay's §6); a malformed/empty payload
   returning a validation error (not `method not found`) proves the namespace serves.
3. Enforcement tuning flags (optional, matching the relay's needs):
   - `--builder.validation_use_balance_diff` - validate proposer payment by fee-recipient
     balance difference.
   - `--builder.validation_exclude_withdrawals` - exclude fee-recipient withdrawals from the
     balance difference.
   - `--builder.blacklist=<path>` - JSON file of blacklisted addresses.

---

## 5. Relay integration

Point the relay's `-blocksim` and `-vouch-registry-rpc` at the validation node over `mev-net`:
- `-blocksim=http://validation-node:18546`
- `-vouch-registry-rpc=http://validation-node:18546`

See `repos/mev-boost-relay/docs/relay-deploy-runbook.md` (§2.1 networking, §5 relay).

---

## 6. Failure drill

1. `docker compose stop validation-node` (kill the validation EL).
2. The relay's validations fail (the relay sees the validation node as unavailable) - builder
   submissions are rejected, validators fall back to local building. No keys or fee-recipient
   changes are involved.
3. Restart: `docker compose start validation-node`; confirm it reconnects to the beacon and
   re-serves `flashbots_validateBuilderSubmissionV2`.

---

## 7. Rollback

- Stop + remove the stack: `docker compose down`.
- Delete the validation datadir (the only data of value is the synced chain; re-sync is a
  fresh-sync requirement anyway): `rm -rf /buildertest/validation-data`.
- Production geth is untouched throughout.

---

## Notes / known limitations

- **Fresh sync only:** do not reuse an old-fork datadir.
- **Deneb disabled:** PulseChain is Capella-era; `flashbots_validateBuilderSubmissionV3+`
  (blob-bearing) return `unsupported on PulseChain` by design.
- The test suite's miner-built-block cases (V1/V2, payment underflow, blocklist) depend on a
  builder-miner adding a proposer-payment tx; the stock go-pulse miner does not, so those
  cases are documented in `PORT.md` (the `buildBlock`-based cases pass).
