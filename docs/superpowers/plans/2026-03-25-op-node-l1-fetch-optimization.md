# op-node L1 Fetch Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accelerate op-node restart recovery on BSC L1 by replacing per-block receipt fetching with `eth_getLogs` range queries, batch-prefetching L1 block refs during walk-back, and tuning pre-fetch concurrency.

**Architecture:** Three layered optimizations: (1) `L1Traversal` uses a log-range cache filled by `eth_getLogs` instead of per-block `FetchReceipts`; (2) `FindL2Heads` batch-prefills `l1BlockRefsCache` before the walk-back loop; (3) `--l1.max-concurrency` default raised via flag. Each optimization is independent and can be merged separately.

**Tech Stack:** Go, go-ethereum RPC (`CallContext`, `BatchCallContext`), op-node derivation pipeline, testify, testutils mocks.

**Task ordering note:** Tasks 3 and 4 are tightly coupled — Task 4 adds `FetchSystemConfigLogs` to the `L1BlockRefByNumberFetcher` interface, which breaks compilation until Task 3 adds it to `MockL1Source`. Complete Task 3 before running Task 4's tests.

---

## Task 1: Add `UpdateSystemConfigWithL1Logs` to system_config.go

**Files:**
- Modify: `op-node/rollup/derive/system_config.go`
- Test: `op-node/rollup/derive/system_config_test.go`

This function is the pure-logic core of Opt-1. It takes a slice of `*types.Log` (already filtered to the right address+topic by `eth_getLogs`) and applies only those whose `BlockNumber` matches the target block. No receipt-status check needed — `eth_getLogs` only returns logs from successful txs on BSC.

- [ ] **Step 1: Write the failing test**

Add to `op-node/rollup/derive/system_config_test.go`:

```go
func TestUpdateSystemConfigWithL1Logs_NoLogs(t *testing.T) {
    cfg := &rollup.Config{L1SystemConfigAddress: common.Address{0x42}}
    sysCfg := eth.SystemConfig{}
    err := UpdateSystemConfigWithL1Logs(&sysCfg, nil, cfg, 100, 0)
    require.NoError(t, err)
}

func TestUpdateSystemConfigWithL1Logs_WrongBlock(t *testing.T) {
    cfg := &rollup.Config{L1SystemConfigAddress: common.Address{0x42}}
    sysCfg := eth.SystemConfig{}
    // log is for block 99, not 100 — should be ignored
    lg := &types.Log{BlockNumber: 99, Address: cfg.L1SystemConfigAddress}
    err := UpdateSystemConfigWithL1Logs(&sysCfg, []*types.Log{lg}, cfg, 100, 0)
    require.NoError(t, err)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd /home/king/workspace/blockchain/bnb-chain/opbnb
go test ./op-node/rollup/derive/ -run TestUpdateSystemConfigWithL1Logs -v
```

Expected: compile error — `UpdateSystemConfigWithL1Logs` undefined.

- [ ] **Step 3: Implement `UpdateSystemConfigWithL1Logs`**

Add to `op-node/rollup/derive/system_config.go` after `UpdateSystemConfigWithL1Receipts`:

```go
// UpdateSystemConfigWithL1Logs applies ConfigUpdate events from the given logs
// to sysCfg. Only logs whose BlockNumber equals blockNumber are processed.
// logs should be pre-filtered to L1SystemConfigAddress and ConfigUpdateEventABIHash
// (e.g. via eth_getLogs). Unlike UpdateSystemConfigWithL1Receipts, no receipt-status
// check is performed — eth_getLogs only returns logs from successful transactions.
func UpdateSystemConfigWithL1Logs(sysCfg *eth.SystemConfig, logs []*types.Log, cfg *rollup.Config, blockNumber uint64, l1Time uint64) error {
    var result error
    for j, lg := range logs {
        if lg.BlockNumber != blockNumber {
            continue
        }
        if err := ProcessSystemConfigUpdateLogEvent(sysCfg, lg, cfg, l1Time); err != nil {
            result = multierror.Append(result, fmt.Errorf("malformatted L1 system sysCfg log at block %d, log %d: %w", blockNumber, j, err))
        }
    }
    return result
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./op-node/rollup/derive/ -run TestUpdateSystemConfigWithL1Logs -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add op-node/rollup/derive/system_config.go op-node/rollup/derive/system_config_test.go
git commit -m "feat(derive): add UpdateSystemConfigWithL1Logs for log-range optimization"
```


## Task 2: Add `FetchSystemConfigLogs` to EthClient and L1Client

**Files:**
- Modify: `op-service/sources/eth_client.go`
- Modify: `op-service/sources/l1_client.go`
- Test: `op-service/sources/eth_client_test.go`

`eth_getLogs` is called via `CallContext` with a raw filter struct — consistent with all other RPC calls in `EthClient`. The `RPCHeader` type used internally is defined in `op-service/sources/types.go`.

- [ ] **Step 1: Write the failing test**

Add to `op-service/sources/eth_client_test.go`:

```go
func TestEthClient_FetchSystemConfigLogs(t *testing.T) {
    m := new(mockRPC)
    lgr := testlog.Logger(t, log.LevelError)
    cfg := &EthClientConfig{
        MaxRequestsPerBatch:   10,
        MaxConcurrentRequests: 10,
        ReceiptsCacheSize:     10,
        TransactionsCacheSize: 10,
        HeadersCacheSize:      10,
        PayloadsCacheSize:     10,
        TrustRPC:              true,
    }
    client, err := NewEthClient(m, lgr, nil, cfg, false)
    require.NoError(t, err)

    addr := common.Address{0x42}
    topic := common.Hash{0x01}
    expectedLogs := []*types.Log{{BlockNumber: 100}}

    // CallContext receives (ctx, result, method, args...).
    // The variadic args are passed as []interface{}{filter} to MethodCalled.
    m.On("CallContext", mock.Anything, mock.AnythingOfType("*[]*types.Log"),
        "eth_getLogs", mock.Anything).
        Run(func(args mock.Arguments) {
            result := args.Get(1).(*[]*types.Log)
            *result = expectedLogs
        }).
        Return([]error{nil})

    got, err := client.FetchSystemConfigLogs(context.Background(), 100, 200, addr, topic)
    require.NoError(t, err)
    require.Equal(t, expectedLogs, got)
    m.AssertExpectations(t)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./op-service/sources/ -run TestEthClient_FetchSystemConfigLogs -v
```

Expected: compile error — `FetchSystemConfigLogs` undefined.

- [ ] **Step 3: Implement `FetchSystemConfigLogs` on `EthClient`**

Add to `op-service/sources/eth_client.go` after `FetchReceipts`:

```go
// syscfgLogFilter is the JSON structure for the eth_getLogs filter parameter.
type syscfgLogFilter struct {
    FromBlock string         `json:"fromBlock"`
    ToBlock   string         `json:"toBlock"`
    Address   common.Address `json:"address"`
    Topics    []interface{}  `json:"topics"`
}

// FetchSystemConfigLogs fetches logs from [fromBlock, toBlock] filtered to the
// given contract address and first topic. Uses eth_getLogs via CallContext.
func (s *EthClient) FetchSystemConfigLogs(ctx context.Context, fromBlock, toBlock uint64, addr common.Address, topic common.Hash) ([]*types.Log, error) {
    filter := syscfgLogFilter{
        FromBlock: hexutil.EncodeUint64(fromBlock),
        ToBlock:   hexutil.EncodeUint64(toBlock),
        Address:   addr,
        Topics:    []interface{}{topic},
    }
    var logs []*types.Log
    if err := s.client.CallContext(ctx, &logs, "eth_getLogs", filter); err != nil {
        return nil, fmt.Errorf("eth_getLogs [%d,%d]: %w", fromBlock, toBlock, err)
    }
    return logs, nil
}
```

- [ ] **Step 4: Expose `FetchSystemConfigLogs` on `L1Client`**

Add to `op-service/sources/l1_client.go`:

```go
// FetchSystemConfigLogs fetches ConfigUpdate logs for [fromBlock, toBlock] in one RPC call.
func (s *L1Client) FetchSystemConfigLogs(ctx context.Context, fromBlock, toBlock uint64, addr common.Address, topic common.Hash) ([]*types.Log, error) {
    return s.EthClient.FetchSystemConfigLogs(ctx, fromBlock, toBlock, addr, topic)
}
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./op-service/sources/ -run TestEthClient_FetchSystemConfigLogs -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add op-service/sources/eth_client.go op-service/sources/l1_client.go op-service/sources/eth_client_test.go
git commit -m "feat(sources): add FetchSystemConfigLogs using eth_getLogs range query"
```


## Task 3: Update `MockL1Source` test helper

**Files:**
- Modify: `op-service/testutils/mock_l1.go`

`L1BlockRefByNumberFetcher` (modified in Task 4) will gain `FetchSystemConfigLogs`. Do this first so Task 4's tests compile.

- [ ] **Step 1: Add `FetchSystemConfigLogs` to `MockL1Source`**

In `op-service/testutils/mock_l1.go`, add after the existing methods:

```go
func (m *MockL1Source) FetchSystemConfigLogs(ctx context.Context, fromBlock, toBlock uint64, addr common.Address, topic common.Hash) ([]*types.Log, error) {
    out := m.Mock.MethodCalled("FetchSystemConfigLogs", fromBlock, toBlock, addr, topic)
    return out[0].([]*types.Log), *out[1].(*error)
}

func (m *MockL1Source) ExpectFetchSystemConfigLogs(fromBlock, toBlock uint64, addr common.Address, topic common.Hash, logs []*types.Log, err error) {
    m.Mock.On("FetchSystemConfigLogs", fromBlock, toBlock, addr, topic).Once().Return(logs, &err)
}
```

Add required imports if not already present: `"github.com/ethereum/go-ethereum/common"`, `"github.com/ethereum/go-ethereum/core/types"`.

- [ ] **Step 2: Build to verify no compile errors**

```bash
go build ./op-service/testutils/...
```

Expected: clean build.

- [ ] **Step 3: Commit**

```bash
git add op-service/testutils/mock_l1.go
git commit -m "test: add FetchSystemConfigLogs to MockL1Source"
```


## Task 4: Add `L1TraversalConfig` and log-range cache to `L1Traversal`

**Files:**
- Modify: `op-node/rollup/derive/l1_traversal.go`
- Modify: `op-node/rollup/derive/pipeline.go`
- Modify: `op-node/rollup/driver/driver.go`
- Modify: `op-node/rollup/driver/config.go`
- Test: `op-node/rollup/derive/l1_traversal_test.go`

`L1Traversal` gains a `syscfgLogCache map[uint64][]*types.Log` and `nextLogFetch uint64`. When `AdvanceL1Block` needs block N and `SyscfgLogRange > 0`, it fetches logs for `[N, N+rangeSize-1]` once and caches them. On `Reset()`, the cache is cleared. Existing call sites of `NewL1Traversal` (including in tests) must be updated to pass the new `tCfg` parameter.

- [ ] **Step 1: Extend `L1BlockRefByNumberFetcher` interface**

In `op-node/rollup/derive/l1_traversal.go`, change the interface to:

```go
type L1BlockRefByNumberFetcher interface {
    L1BlockRefByNumber(context.Context, uint64) (eth.L1BlockRef, error)
    FetchReceipts(ctx context.Context, blockHash common.Hash) (eth.BlockInfo, types.Receipts, error)
    PreFetchReceipts(ctx context.Context, blockHash common.Hash) (bool, error)
    FetchSystemConfigLogs(ctx context.Context, fromBlock, toBlock uint64, addr common.Address, topic common.Hash) ([]*types.Log, error)
}
```

- [ ] **Step 2: Add `L1TraversalConfig` and update `L1Traversal` struct**

```go
// L1TraversalConfig holds runtime tuning parameters for L1Traversal.
// Not consensus-critical — do not add to rollup.Config.
type L1TraversalConfig struct {
    // SyscfgLogRange is the number of L1 blocks per eth_getLogs call for system
    // config updates. Default 1000. Set to 0 to fall back to per-block receipt fetch.
    SyscfgLogRange uint64
}

type L1Traversal struct {
    block    eth.L1BlockRef
    done     bool
    l1Blocks L1BlockRefByNumberFetcher
    log      log.Logger
    sysCfg   eth.SystemConfig
    cfg      *rollup.Config
    tCfg     L1TraversalConfig

    syscfgLogCache map[uint64][]*types.Log
    nextLogFetch   uint64
}
```

- [ ] **Step 3: Update `NewL1Traversal`**

```go
func NewL1Traversal(log log.Logger, cfg *rollup.Config, l1Blocks L1BlockRefByNumberFetcher, tCfg L1TraversalConfig) *L1Traversal {
    return &L1Traversal{
        log:            log,
        l1Blocks:       l1Blocks,
        cfg:            cfg,
        tCfg:           tCfg,
        syscfgLogCache: make(map[uint64][]*types.Log),
    }
}
```

- [ ] **Step 4: Update `Reset()` to clear the log cache**

```go
func (l1t *L1Traversal) Reset(ctx context.Context, base eth.L1BlockRef, cfg eth.SystemConfig) error {
    l1t.block = base
    l1t.done = false
    l1t.sysCfg = cfg
    l1t.syscfgLogCache = make(map[uint64][]*types.Log)
    l1t.nextLogFetch = 0
    l1t.log.Info("completed reset of derivation pipeline", "origin", base)
    return io.EOF
}
```

- [ ] **Step 5: Update `AdvanceL1Block()` to use log-range cache**

Replace the `FetchReceipts` + `UpdateSystemConfigWithL1Receipts` block (lines 74–84 of `l1_traversal.go`) with:

```go
if l1t.tCfg.SyscfgLogRange > 0 {
    if nextL1Origin.Number >= l1t.nextLogFetch {
        toBlock := nextL1Origin.Number + l1t.tCfg.SyscfgLogRange - 1
        fetchedLogs, err := l1t.l1Blocks.FetchSystemConfigLogs(ctx, nextL1Origin.Number, toBlock,
            l1t.cfg.L1SystemConfigAddress, ConfigUpdateEventABIHash)
        if err != nil {
            return NewTemporaryError(fmt.Errorf("failed to fetch system config logs [%d,%d]: %w",
                nextL1Origin.Number, toBlock, err))
        }
        for _, lg := range fetchedLogs {
            l1t.syscfgLogCache[lg.BlockNumber] = append(l1t.syscfgLogCache[lg.BlockNumber], lg)
        }
        l1t.nextLogFetch = toBlock + 1
    }
    blockLogs := l1t.syscfgLogCache[nextL1Origin.Number]
    delete(l1t.syscfgLogCache, nextL1Origin.Number)
    if err := UpdateSystemConfigWithL1Logs(&l1t.sysCfg, blockLogs, l1t.cfg, nextL1Origin.Number, nextL1Origin.Time); err != nil {
        return NewCriticalError(fmt.Errorf("failed to update L1 sysCfg with logs from block %s: %w", nextL1Origin, err))
    }
} else {
    _, receipts, err := l1t.l1Blocks.FetchReceipts(ctx, nextL1Origin.Hash)
    if err != nil {
        return NewTemporaryError(fmt.Errorf("failed to fetch receipts of L1 block %s for L1 sysCfg update: %w", nextL1Origin, err))
    }
    if err := UpdateSystemConfigWithL1Receipts(&l1t.sysCfg, receipts, l1t.cfg, nextL1Origin.Time); err != nil {
        return NewCriticalError(fmt.Errorf("failed to update L1 sysCfg with receipts from block %s: %w", nextL1Origin, err))
    }
}
```

- [ ] **Step 6: Update `NewDerivationPipeline` in `pipeline.go`**

Add `tCfg derive.L1TraversalConfig` as the last parameter and pass it to `NewL1Traversal`:

```go
func NewDerivationPipeline(log log.Logger, rollupCfg *rollup.Config, l1Fetcher L1Fetcher, l1Blobs L1BlobsFetcher,
    plasma PlasmaInputFetcher, l2Source L2Source, engine LocalEngineControl, metrics Metrics,
    syncCfg *sync.Config, safeHeadListener SafeHeadListener, finalizer FinalizerHooks,
    attributesHandler AttributesHandler, tCfg L1TraversalConfig) *DerivationPipeline {

    l1Traversal := NewL1Traversal(log, rollupCfg, l1Fetcher, tCfg)
    // ... rest unchanged
```

- [ ] **Step 7: Add `L1TraversalConfig` to `driver.Config` and update call sites**

In `op-node/rollup/driver/config.go`, add:

```go
// L1TraversalConfig holds runtime tuning for the L1 traversal stage.
L1TraversalConfig derive.L1TraversalConfig `json:"l1_traversal_config"`
```

In `op-node/rollup/driver/driver.go`, update the `NewDerivationPipeline` call:

```go
derivationPipeline := derive.NewDerivationPipeline(log, cfg, verifConfDepth, l1Blobs, plasma, l2, engine,
    metrics, syncCfg, safeHeadListener, finalizer, attributesHandler, driverCfg.L1TraversalConfig)
```

In `op-node/service.go`, update `NewDriverConfig` to read the flag:

```go
func NewDriverConfig(ctx *cli.Context) *driver.Config {
    return &driver.Config{
        // ... existing fields ...
        L1TraversalConfig: derive.L1TraversalConfig{
            SyscfgLogRange: ctx.Uint64(flags.L1SyscfgLogRange.Name),
        },
    }
}
```

- [ ] **Step 8: Update existing `NewL1Traversal` calls in test files**

In `op-node/rollup/derive/l1_traversal_test.go`, update all existing calls from:
```go
NewL1Traversal(log, cfg, src)
```
to:
```go
NewL1Traversal(log, cfg, src, L1TraversalConfig{})
```

Search for all occurrences:
```bash
grep -n "NewL1Traversal" /home/king/workspace/blockchain/bnb-chain/opbnb/op-node/rollup/derive/l1_traversal_test.go
```

- [ ] **Step 9: Write new tests for the log-range cache**

Add to `op-node/rollup/derive/l1_traversal_test.go`:

```go
func TestL1TraversalAdvance_LogRangeCache(t *testing.T) {
    rng := rand.New(rand.NewSource(1234))
    a := testutils.RandomBlockRef(rng)
    b := testutils.RandomBlockRef(rng)
    b.Number = a.Number + 1
    b.ParentHash = a.Hash

    l1Cfg := eth.SystemConfig{BatcherAddr: testutils.RandomAddress(rng)}
    sysCfgAddr := testutils.RandomAddress(rng)
    cfg := &rollup.Config{
        Genesis:               rollup.Genesis{SystemConfig: l1Cfg},
        L1SystemConfigAddress: sysCfgAddr,
    }
    tCfg := L1TraversalConfig{SyscfgLogRange: 10}

    l1Src := &testutils.MockL1Source{}
    l1Src.ExpectL1BlockRefByNumber(b.Number, b, nil)
    l1Src.ExpectFetchSystemConfigLogs(b.Number, b.Number+9, sysCfgAddr, ConfigUpdateEventABIHash, nil, nil)

    tr := NewL1Traversal(testlog.Logger(t, log.LevelError), cfg, l1Src, tCfg)
    _ = tr.Reset(context.Background(), a, l1Cfg)
    err := tr.AdvanceL1Block(context.Background())
    require.NoError(t, err)
    l1Src.AssertExpectations(t)
}

func TestL1TraversalReset_ClearsLogCache(t *testing.T) {
    rng := rand.New(rand.NewSource(42))
    a := testutils.RandomBlockRef(rng)
    cfg := &rollup.Config{L1SystemConfigAddress: testutils.RandomAddress(rng)}
    tCfg := L1TraversalConfig{SyscfgLogRange: 10}
    tr := NewL1Traversal(testlog.Logger(t, log.LevelError), cfg, nil, tCfg)
    tr.syscfgLogCache[100] = []*types.Log{{BlockNumber: 100}}
    tr.nextLogFetch = 110
    _ = tr.Reset(context.Background(), a, eth.SystemConfig{})
    require.Empty(t, tr.syscfgLogCache)
    require.Equal(t, uint64(0), tr.nextLogFetch)
}
```

- [ ] **Step 10: Build and run all derive tests**

```bash
go build ./op-node/... ./op-service/...
go test ./op-node/rollup/derive/ -run TestL1Traversal -v
```

Expected: clean build, all PASS.

- [ ] **Step 11: Commit**

```bash
git add op-node/rollup/derive/l1_traversal.go op-node/rollup/derive/pipeline.go \
    op-node/rollup/derive/l1_traversal_test.go op-node/rollup/driver/config.go \
    op-node/rollup/driver/driver.go op-node/service.go
git commit -m "feat(derive): L1Traversal log-range cache replaces per-block receipt fetch"
```


## Task 5: Add CLI flags to `flags.go`

**Files:**
- Modify: `op-node/flags/flags.go`

Add two new flags. Note: Opt-3 (pre-fetch concurrency) reuses the existing `--l1.max-concurrency` flag (`L1RPCMaxConcurrency`) — its default is already 10 but the `L1EndpointConfig.MaxConcurrency` field is set from it in `service.go` line 166. To raise the default, simply change the flag's `Value` from 10 to 20. Do NOT add a separate `--l1.prefetch-concurrency` flag — that would conflict with the existing flag.

- [ ] **Step 1: Add `L1SyscfgLogRange` and `L1WalkbackPrefetchBatch` flags**

After the `SkipSyncStartCheck` flag definition in `op-node/flags/flags.go`, add:

```go
L1SyscfgLogRange = &cli.Uint64Flag{
    Name:    "l1.syscfg-log-range",
    Usage:   "Number of L1 blocks per eth_getLogs batch when scanning for system config updates. Set to 0 to disable and use per-block receipt fetching.",
    EnvVars: prefixEnvVars("L1_SYSCFG_LOG_RANGE"),
    Value:   1000,
}
L1WalkbackPrefetchBatch = &cli.IntFlag{
    Name:    "l1.walkback-prefetch-batch",
    Usage:   "Batch size for L1 block ref pre-fetch during FindL2Heads walk-back.",
    EnvVars: prefixEnvVars("L1_WALKBACK_PREFETCH_BATCH"),
    Value:   20,
}
```

- [ ] **Step 2: Raise `L1RPCMaxConcurrency` default from 10 to 20**

The wiring path is confirmed: `--l1.max-concurrency` flag → `L1EndpointConfig.MaxConcurrency` (service.go line 166) → `rpcCfg.MaxConcurrentRequests` (node/client.go lines 173/191) → `EthClientConfig.MaxConcurrentRequests` → `L1Client.maxConcurrentRequests` (l1_client.go line 91). Raising the flag default therefore does affect the pre-fetch goroutine.

Find the `L1RPCMaxConcurrency` flag definition (line ~166) and change `Value: 10` to `Value: 20`.

- [ ] **Step 3: Add both new flags to the flags slice**

Find where `SkipSyncStartCheck` is added to the flags slice (around line 486) and add `L1SyscfgLogRange` and `L1WalkbackPrefetchBatch` to the same slice.

- [ ] **Step 4: Build to verify**

```bash
go build ./op-node/...
```

Expected: clean build.

- [ ] **Step 5: Commit**

```bash
git add op-node/flags/flags.go
git commit -m "feat(node): add l1.syscfg-log-range and l1.walkback-prefetch-batch flags; raise max-concurrency default to 20"
```


## Task 6: Batch pre-fetch L1 block refs in `FindL2Heads`

**Files:**
- Modify: `op-node/rollup/sync/start.go`
- Modify: `op-node/rollup/sync/config.go`
- Modify: `op-node/service.go`
- Modify: `op-service/sources/l1_client.go`

Before the walk-back loop, batch-fetch all L1 block headers in `[finalized.L1Origin.Number, unsafe.L1Origin.Number]` using `BatchCallContext`, populating `l1BlockRefsCache` (keyed by hash). The walk-back loop's fast path calls `L1BlockRefByHash` which checks this cache — so pre-warming it eliminates the per-step 200ms latency.

`RPCHeader` is defined in `op-service/sources/types.go` and is accessible within the `sources` package. Use it directly in `BatchPrefetchL1BlockRefsByNumber`.

- [ ] **Step 1: Add `WalkbackPrefetchBatch` to `sync.Config`**

In `op-node/rollup/sync/config.go`:

```go
type Config struct {
    SyncMode              Mode   `json:"syncmode"`
    SkipSyncStartCheck    bool   `json:"skip_sync_start_check"`
    ELTriggerGap          int    `json:"el_trigger_gap"`
    // WalkbackPrefetchBatch is the batch size for L1 block ref pre-fetch
    // during FindL2Heads walk-back. Default 20. Set to 0 to disable.
    WalkbackPrefetchBatch int    `json:"walkback_prefetch_batch"`
}
```

- [ ] **Step 2: Read flag in `service.go`**

In `NewSyncConfig` (around line 324 of `op-node/service.go`), update the `sync.Config` construction:

```go
cfg := &sync.Config{
    SyncMode:              mode,
    SkipSyncStartCheck:    ctx.Bool(flags.SkipSyncStartCheck.Name),
    ELTriggerGap:          elTriggerGap,
    WalkbackPrefetchBatch: ctx.Int(flags.L1WalkbackPrefetchBatch.Name),
}
```

- [ ] **Step 3: Add `L1BatchPrefetcher` interface to `start.go`**

Add to `op-node/rollup/sync/start.go`:

```go
// L1BatchPrefetcher is an optional interface. If the L1Chain implementation
// supports it, FindL2Heads will use it to pre-warm the block ref cache before
// the walk-back loop, reducing per-step RPC latency.
type L1BatchPrefetcher interface {
    BatchPrefetchL1BlockRefsByNumber(ctx context.Context, from, to uint64, batchSize int) error
}
```

- [ ] **Step 4: Implement `BatchPrefetchL1BlockRefsByNumber` on `L1Client`**

Add to `op-service/sources/l1_client.go`. Note: `RPCHeader` is in the same package (`sources`), so it is directly accessible. `RPCHeader.Info(trustRPC, mustBePostMerge bool)` returns `(eth.BlockInfo, error)`.

```go
// BatchPrefetchL1BlockRefsByNumber fetches L1 block headers for [from, to] in
// batches of batchSize using BatchCallContext, populating l1BlockRefsCache.
// Used by FindL2Heads to pre-warm the hash-keyed cache before walk-back.
func (s *L1Client) BatchPrefetchL1BlockRefsByNumber(ctx context.Context, from, to uint64, batchSize int) error {
    if batchSize <= 0 {
        batchSize = 20
    }
    for start := from; start <= to; start += uint64(batchSize) {
        end := start + uint64(batchSize) - 1
        if end > to {
            end = to
        }
        count := int(end - start + 1)
        headers := make([]*RPCHeader, count)
        elems := make([]rpc.BatchElem, count)
        for i := 0; i < count; i++ {
            elems[i] = rpc.BatchElem{
                Method: "eth_getBlockByNumber",
                Args:   []interface{}{hexutil.EncodeUint64(start + uint64(i)), false},
                Result: &headers[i],
            }
        }
        if err := s.client.BatchCallContext(ctx, elems); err != nil {
            return fmt.Errorf("batch prefetch L1 block refs [%d,%d]: %w", start, end, err)
        }
        for i, elem := range elems {
            if elem.Error != nil {
                s.log.Warn("batch prefetch L1 block ref error", "number", start+uint64(i), "err", elem.Error)
                continue
            }
            if headers[i] == nil {
                continue
            }
            info, err := headers[i].Info(s.trustRPC, s.mustBePostMerge)
            if err != nil {
                s.log.Warn("batch prefetch L1 block ref info error", "number", start+uint64(i), "err", err)
                continue
            }
            ref := eth.InfoToL1BlockRef(info)
            s.l1BlockRefsCache.Add(ref.Hash, ref)
        }
    }
    return nil
}
```

`s.trustRPC` and `s.mustBePostMerge` are confirmed unexported fields on `EthClient` (eth_client.go lines 112/114), set at construction from `config.TrustRPC` and `config.MustBePostMerge`. Since `L1Client` is in the same `sources` package, these fields are directly accessible. No verification step needed.
```bash
grep -n "trustRPC\|mustBePostMerge" /home/king/workspace/blockchain/bnb-chain/opbnb/op-service/sources/eth_client.go | head -5
```

- [ ] **Step 5: Call batch pre-fetch in `FindL2Heads`**

In `op-node/rollup/sync/start.go`, after `currentHeads` returns and before the walk-back `for` loop:

```go
// Opt-2: batch pre-fetch L1 block refs to warm the hash-keyed cache before walk-back.
// Non-fatal if it fails — the loop will fetch individually.
if bp, ok := l1.(L1BatchPrefetcher); ok && result.Unsafe.L1Origin.Number > result.Finalized.L1Origin.Number {
    batchSize := syncCfg.WalkbackPrefetchBatch
    if batchSize <= 0 {
        batchSize = 20
    }
    lgr.Info("batch pre-fetching L1 block refs for walk-back",
        "from", result.Finalized.L1Origin.Number,
        "to", result.Unsafe.L1Origin.Number,
        "batchSize", batchSize)
    if err := bp.BatchPrefetchL1BlockRefsByNumber(ctx,
        result.Finalized.L1Origin.Number,
        result.Unsafe.L1Origin.Number,
        batchSize); err != nil {
        lgr.Warn("batch pre-fetch L1 block refs failed, continuing without cache", "err", err)
    }
}
```

- [ ] **Step 6: Full build and test**

```bash
go build ./op-node/... ./op-service/...
go test ./op-node/rollup/sync/ ./op-service/sources/ -count=1 2>&1 | grep -E "FAIL|ok"
```

Expected: all packages `ok`, no `FAIL`.

- [ ] **Step 7: Smoke-test the flags**

```bash
go run ./op-node/cmd/main.go --help 2>&1 | grep -E "syscfg-log-range|walkback-prefetch"
```

Expected: both flags appear in help output.

- [ ] **Step 8: Commit**

```bash
git add op-node/rollup/sync/start.go op-node/rollup/sync/config.go \
    op-service/sources/l1_client.go op-node/service.go
git commit -m "feat(sync): batch pre-fetch L1 block refs before FindL2Heads walk-back"
```

---

## Summary of Changes

| Optimization | Key Files | Expected Impact |
|---|---|---|
| Opt-1: eth_getLogs range | `system_config.go`, `l1_traversal.go`, `eth_client.go` | ~1000x fewer RPC calls during catch-up |
| Opt-2: walk-back batch prefetch | `start.go`, `l1_client.go` | ~20x faster FindL2Heads on restart |
| Opt-3: max-concurrency default | `flags.go` | ~2x pre-fetch throughput |
| Immediate (no code) | `--l2.skip-sync-start-check` flag | Walk-back steps: thousands → handful |

