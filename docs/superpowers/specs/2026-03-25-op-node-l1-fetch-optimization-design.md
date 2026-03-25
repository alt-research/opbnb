# op-node L1 Fetch Optimization Design

**Date**: 2026-03-25
**Context**: BSC L1 at 0.45s blocks, L2 at 0.25s, op-node L1 RPC latency ~200ms. After restart, node fails to catch up due to sequential per-block receipt fetching.

---

## Problem

After op-node restart, two sequential phases are slow:

1. **`FindL2Heads` walk-back**: Traverses L2 blocks from unsafe head back to finalized, fetching L1 block refs one-by-one (~200ms each). With a gap of thousands to tens of thousands of L2 blocks, this takes minutes to hours.

2. **`AdvanceL1Block` catch-up**: For every L1 block, fetches full receipts (~200ms) to scan for `ConfigUpdate` events from `L1SystemConfigAddress`. These events are extremely rare, so 99%+ of receipt fetches return no useful data.

At 200ms latency and 450ms BSC block time, the node barely keeps up in steady state and cannot recover after restart.

---

## Solution Overview

Three complementary optimizations, in priority order:

### Opt-1: `eth_getLogs` Range Query for System Config (Highest Impact)

**Root cause**: `AdvanceL1Block` calls `FetchReceipts` for every L1 block, but only needs logs from one contract (`L1SystemConfigAddress`) with one topic (`ConfigUpdateEventABIHash`).

**Change**:
- Add `FetchSystemConfigLogs(ctx, fromBlock, toBlock uint64) ([]*types.Log, error)` to `EthClient`, using `eth_getLogs` with address + topic filter.
- Add `UpdateSystemConfigWithL1Logs(sysCfg, logs, cfg, blockNumber, l1Time)` that applies only the logs relevant to a given block number (same logic as `UpdateSystemConfigWithL1Receipts`).
- In `L1Traversal`, maintain a log range cache: when `AdvanceL1Block` needs block N and the cache is empty or exhausted, fetch logs for `[N, N+rangeSize)` in one RPC call, store in a `map[uint64][]*types.Log`.
- `AdvanceL1Block` reads from the cache instead of calling `FetchReceipts`.
- The cache is invalidated in `L1Traversal.Reset()` — the natural hook already called on every pipeline reset (including reorg recovery). This ensures stale logs from reorged blocks are never applied.
- Add CLI flag `--l1-syscfg-log-range` (default: 1000, configurable for nodes with stricter limits).

**Impact**: 1000 L1 blocks → 1 RPC call instead of 1000. Catch-up speed increases ~1000x for the receipt-fetch bottleneck.

**Trade-off**: Skips receipt trie root validation for system config purposes. Acceptable when `trustRPC=true` (already bypasses this validation) or when the L1 node is trusted infrastructure.

### Opt-2: `FindL2Heads` L1 Block Ref Batch Pre-fetch

**Root cause**: The walk-back loop fetches L1 block refs sequentially. Each `L1BlockRefByNumber` or `L1BlockRefByHash` call is ~200ms. Tens of thousands of steps = hours.

**Change**:
- Before the walk-back loop starts, determine the L1 origin range: `[finalized.L1Origin.Number, unsafe.L1Origin.Number]`.
- Batch-fetch all L1 block refs in that range using `BatchCallContext` (batches of 20). Each batch fetches full block headers (to get both number and hash), then populates the existing hash-keyed `l1BlockRefsCache`. This is required because the walk-back loop's fast path calls `L1BlockRefByHash` which checks the hash-keyed cache.
- The walk-back loop then hits the cache for every L1 block ref lookup — near-zero latency per step.
- Add CLI flag `--l1-walkback-prefetch-batch` (default: 20).

**Impact**: Walk-back L1 fetch latency drops from O(N × 200ms) to O(N/20 × 200ms) = 20x faster. For 10k L1 blocks: ~33 minutes → ~2 minutes.

**Trade-off**: Upfront cost of batch-fetching the full L1 range before walk-back begins. For very large ranges (>50k blocks), this could be slow itself — mitigated by the batch size and the fact that `BatchCallContext` amortizes RTT.

### Opt-3: GoOrUpdatePreFetchReceipts Concurrency Tuning

**Root cause**: Pre-fetch goroutine uses `maxConcurrentRequests/2 = 10` concurrent goroutines (default `MaxConcurrentRequests=20`, halved at line 177 of `l1_client.go`). With Opt-1 applied, receipt fetches for system config are eliminated, but the pre-fetch goroutine still fetches block headers and blob/calldata receipts.

**Change**:
- Add CLI flag `--l1-prefetch-concurrency` (default: 20). This sets `MaxConcurrentRequests`, making effective pre-fetch concurrency 10→20.
- No change to the header-fetch pattern inside the goroutine (batching headers before receipts would serialize the header phase and is not a net win).

**Impact**: Moderate — 2x throughput improvement for the pre-fetch phase in steady state.

---

## Immediate No-Code Fix

Enable `--l2.skip-sync-start-check` (or `OP_NODE_L2_SKIP_SYNC_START_CHECK=true`).

This causes `FindL2Heads` to stop walk-back as soon as it finds the first L2 block with a canonical L1 origin, jumping directly to safe head. Reduces walk-back from tens of thousands of steps to a handful. Safe to use when the local execution engine's L2 data is trusted.

---

## Files to Modify

| File | Change |
|------|--------|
| `op-node/flags/flags.go` | Add `L1SyscfgLogRange`, `L1WalkbackPrefetchBatch`, `L1PrefetchConcurrency` flags |
| `op-node/service.go` | Wire new flags into `OpNode` config; pass `L1TraversalConfig` through `NewDerivationPipeline` → `NewL1Traversal` |
| `op-node/rollup/derive/l1_traversal.go` | Accept `L1TraversalConfig`; maintain log range cache; invalidate on `Reset()`; use logs in `AdvanceL1Block` |
| `op-node/rollup/derive/system_config.go` | Add `UpdateSystemConfigWithL1Logs` |
| `op-node/rollup/derive/pipeline.go` | Thread `L1TraversalConfig` parameter through `NewDerivationPipeline` to `NewL1Traversal` |
| `op-node/rollup/sync/start.go` | Add `BatchPrefetchL1BlockRefs` helper; call before walk-back loop |
| `op-service/sources/eth_client.go` | Add `FetchSystemConfigLogs` using `CallContext` with raw `eth_getLogs` params |
| `op-service/sources/l1_client.go` | Expose `FetchSystemConfigLogs`; wire `--l1-prefetch-concurrency` into `MaxConcurrentRequests` |

---

## Interface Changes

```go
// New method on EthClient — uses CallContext with raw JSON params (consistent with
// existing EthClient patterns; does NOT use ethclient.FilterLogs).
// Calls eth_getLogs with address + topic filter over [fromBlock, toBlock].
FetchSystemConfigLogs(ctx context.Context, fromBlock, toBlock uint64, addr common.Address, topic common.Hash) ([]*types.Log, error)

// New function in derive/system_config.go.
// Applies only logs whose BlockNumber == blockNumber.
// NOTE: eth_getLogs returns logs only from successful transactions on BSC (the node
// does not include logs from reverted txs in its log index). The receipt-status check
// in UpdateSystemConfigWithL1Receipts is therefore redundant in practice. This is an
// accepted behavioral difference; a ConfigUpdate event from a failed tx is not possible
// given the SystemConfig contract's design.
UpdateSystemConfigWithL1Logs(sysCfg *eth.SystemConfig, logs []*types.Log, cfg *rollup.Config, blockNumber uint64, l1Time uint64) error

// L1Traversal config — passed as a new parameter to NewL1Traversal and threaded from
// CLI flag → service.go → NewDerivationPipeline → NewL1Traversal.
// Does NOT extend rollup.Config (that struct is consensus-critical; runtime tuning
// params should not live there).
type L1TraversalConfig struct {
    SyscfgLogRange uint64 // default 1000; 0 disables the optimization (falls back to per-block receipt fetch)
}
```

---

## Config / CLI Summary

| Flag | Default | Description |
|------|---------|-------------|
| `--l2.skip-sync-start-check` | false | Skip walk-back sanity check (immediate fix, no code change) |
| `--l1-syscfg-log-range` | 1000 | Blocks per `eth_getLogs` batch for system config |
| `--l1-walkback-prefetch-batch` | 20 | Batch size for L1 block ref pre-fetch during walk-back |
| `--l1-prefetch-concurrency` | 20 | Max concurrent goroutines in receipt pre-fetch loop |
