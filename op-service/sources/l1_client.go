package sources

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/sources/caching"
)

const sequencerConfDepth = 15

type L1ClientConfig struct {
	EthClientConfig

	L1BlockRefsCacheSize int
}

func L1ClientDefaultConfig(config *rollup.Config, trustRPC bool, kind RPCProviderKind) *L1ClientConfig {
	// Cache 3/2 worth of sequencing window of receipts and txs
	span := int(config.SeqWindowSize) * 3 / 2
	fullSpan := span
	if span > 1000 { // sanity cap. If a large sequencing window is configured, do not make the cache too large
		span = 1000
	}
	return &L1ClientConfig{
		EthClientConfig: EthClientConfig{
			// receipts and transactions are cached per block
			ReceiptsCacheSize:     span,
			TransactionsCacheSize: span,
			HeadersCacheSize:      span,
			PayloadsCacheSize:     span,
			MaxRequestsPerBatch:   20, // TODO: tune batch param
			MaxConcurrentRequests: 40,
			TrustRPC:              trustRPC,
			MustBePostMerge:       false,
			RPCProviderKind:       kind,
			MethodResetDuration:   time.Minute,
		},
		// Not bounded by span, to cover find-sync-start range fully for speedy recovery after errors.
		L1BlockRefsCacheSize: fullSpan,
	}
}

// L1Client provides typed bindings to retrieve L1 data from an RPC source,
// with optimized batch requests, cached results, and flag to not trust the RPC
// (i.e. to verify all returned contents against corresponding block hashes).
type L1Client struct {
	*EthClient

	// cache L1BlockRef by hash
	// common.Hash -> eth.L1BlockRef
	l1BlockRefsCache *caching.LRUCache[common.Hash, eth.L1BlockRef]

	//ensure pre-fetch receipts only once
	preFetchReceiptsOnce      sync.Once
	isPreFetchReceiptsRunning atomic.Bool
	//start block for pre-fetch receipts
	preFetchReceiptsStartBlockChan chan uint64
	preFetchReceiptsClosedChan     chan struct{}
	// highest block number ever sent to preFetchReceiptsStartBlockChan;
	// prevents engine_queue's lower-numbered calls from pulling the prefetcher backwards.
	preFetchHighWaterMark atomic.Uint64

	// blockRefPrefetchChan triggers async batch prefetch of L1 block headers.
	// Sending a block number N causes the goroutine to batch-fetch headers for [N, N+batchSize).
	blockRefPrefetchChan chan uint64
	// blockRefByNumberCache caches L1BlockRef by block number for catch-up acceleration.
	// Entries are invalidated on reorg (detected by AdvanceL1Block's parent-hash check).
	// uint64 -> eth.L1BlockRef
	blockRefByNumberCache sync.Map

	//max concurrent requests
	maxConcurrentRequests int
	//done chan
	done chan struct{}
}

// NewL1Client wraps a RPC with bindings to fetch L1 data, while logging errors, tracking metrics (optional), and caching.
func NewL1Client(client client.RPC, log log.Logger, metrics caching.Metrics, config *L1ClientConfig) (*L1Client, error) {
	ethClient, err := NewEthClient(client, log, metrics, &config.EthClientConfig, true)
	if err != nil {
		return nil, err
	}

	l1Client := &L1Client{
		EthClient:                      ethClient,
		l1BlockRefsCache:               caching.NewLRUCache[common.Hash, eth.L1BlockRef](metrics, "blockrefs", config.L1BlockRefsCacheSize),
		preFetchReceiptsOnce:           sync.Once{},
		preFetchReceiptsStartBlockChan: make(chan uint64, 1),
		preFetchReceiptsClosedChan:     make(chan struct{}),
		blockRefPrefetchChan:           make(chan uint64, 1),
		maxConcurrentRequests:          config.MaxConcurrentRequests,
		done:                           make(chan struct{}),
	}
	l1Client.startBlockRefPrefetcher(context.Background())
	return l1Client, nil
}

// L1BlockRefByLabel returns the [eth.L1BlockRef] for the given block label.
// Notice, we cannot cache a block reference by label because labels are not guaranteed to be unique.
func (s *L1Client) L1BlockRefByLabel(ctx context.Context, label eth.BlockLabel) (eth.L1BlockRef, error) {
	info, err := s.BSCInfoByLabel(ctx, label)
	if label == eth.Finalized && err != nil && strings.Contains(err.Error(), "eth_getFinalizedHeader does not exist") {
		// op-e2e not support bsc as L1 currently, so fallback to not use bsc specific method eth_getFinalizedBlock
		info, err = s.InfoByLabel(ctx, label)
	}
	if err != nil {
		// Both geth and erigon like to serve non-standard errors for the safe and finalized heads, correct that.
		// This happens when the chain just started and nothing is marked as safe/finalized yet.
		if strings.Contains(err.Error(), "block not found") || strings.Contains(err.Error(), "Unknown block") {
			err = ethereum.NotFound
		}
		return eth.L1BlockRef{}, fmt.Errorf("failed to fetch head header: %w", err)
	}
	ref := eth.InfoToL1BlockRef(info)
	s.l1BlockRefsCache.Add(ref.Hash, ref)
	return ref, nil
}

// L1BlockRefByNumber returns an [eth.L1BlockRef] for the given block number.
// Notice, we cannot cache a block reference by number because L1 re-orgs can invalidate the cached block reference.
// However, during catch-up we pre-populate blockRefByNumberCache via batch RPC; hits avoid individual round-trips.
func (s *L1Client) L1BlockRefByNumber(ctx context.Context, num uint64) (eth.L1BlockRef, error) {
	if v, ok := s.blockRefByNumberCache.Load(num); ok {
		ref := v.(eth.L1BlockRef)
		return ref, nil
	}
	info, err := s.InfoByNumber(ctx, num)
	if err != nil {
		return eth.L1BlockRef{}, fmt.Errorf("failed to fetch header by num %d: %w", num, err)
	}
	ref := eth.InfoToL1BlockRef(info)
	s.l1BlockRefsCache.Add(ref.Hash, ref)
	return ref, nil
}

// TriggerBlockRefPrefetch signals the batch prefetcher to fetch headers starting at nextNum.
// Should only be called from bq's AdvanceL1Block to avoid polluting the prefetch position
// with sequencer or initialization calls.
func (s *L1Client) TriggerBlockRefPrefetch(nextNum uint64) {
	select {
	case s.blockRefPrefetchChan <- nextNum:
	default:
	}
}

// InvalidateBlockRefByNumberCache removes entries >= fromNumber from blockRefByNumberCache.
// Must be called on L1 reorg to prevent stale number-keyed entries from being served.
func (s *L1Client) InvalidateBlockRefByNumberCache(fromNumber uint64) {
	s.log.Info("invalidating blockRefByNumber cache on reorg", "fromNumber", fromNumber)
	s.blockRefByNumberCache.Range(func(k, _ any) bool {
		if k.(uint64) >= fromNumber {
			s.blockRefByNumberCache.Delete(k)
		}
		return true
	})
}

// startBlockRefPrefetcher runs a goroutine that batch-fetches L1 full blocks
// into blockRefByNumberCache, headersCache, and transactionsCache whenever
// triggered by blockRefPrefetchChan. Fetching full blocks (with txs) avoids
// the per-block eth_getBlockByHash RPC call in InfoAndTxsByHash during derivation.
func (s *L1Client) startBlockRefPrefetcher(ctx context.Context) {
	const batchSize = 20
	go func() {
		for {
			select {
			case <-s.done:
				return
			case startNum := <-s.blockRefPrefetchChan:
				// Skip only if both blockRef and txs are already cached.
				// blockRefByNumberCache may be populated by the receipts prefetcher,
				// but transactionsCache (LRU) may have evicted the entry by the time
				// derivation reaches this block. Re-fetch if txs are missing.
				if v, ok := s.blockRefByNumberCache.Load(startNum); ok {
					ref := v.(eth.L1BlockRef)
					if _, txOk := s.transactionsCache.Get(ref.Hash); txOk {
						continue
					}
				}
				blocks := make([]*RPCBlock, batchSize)
				elems := make([]rpc.BatchElem, batchSize)
				for i := 0; i < batchSize; i++ {
					elems[i] = rpc.BatchElem{
						Method: "eth_getBlockByNumber",
						Args:   []interface{}{hexutil.EncodeUint64(startNum + uint64(i)), true},
						Result: &blocks[i],
					}
				}
				if err := s.client.BatchCallContext(ctx, elems); err != nil {
					s.log.Warn("blockref batch prefetch failed", "start", startNum, "err", err)
					continue
				}
				cached := 0
				for i, elem := range elems {
					if elem.Error != nil || blocks[i] == nil {
						continue
					}
					info, txs, err := blocks[i].Info(s.trustRPC, s.mustBePostMerge)
					if err != nil {
						continue
					}
					ref := eth.InfoToL1BlockRef(info)
					s.blockRefByNumberCache.Store(ref.Number, ref)
					s.l1BlockRefsCache.Add(ref.Hash, ref)
					s.headersCache.Add(ref.Hash, info)
					s.transactionsCache.Add(ref.Hash, txs)
					cached++
				}
				s.log.Info("blockref batch prefetch done", "start", startNum, "cached", cached)
			}
		}
	}()
}

// L1BlockRefByHash returns the [eth.L1BlockRef] for the given block hash.
// We cache the block reference by hash as it is safe to assume collision will not occur.
func (s *L1Client) L1BlockRefByHash(ctx context.Context, hash common.Hash) (eth.L1BlockRef, error) {
	if v, ok := s.l1BlockRefsCache.Get(hash); ok {
		return v, nil
	}
	info, err := s.InfoByHash(ctx, hash)
	if err != nil {
		return eth.L1BlockRef{}, fmt.Errorf("failed to fetch header by hash %v: %w", hash, err)
	}
	ref := eth.InfoToL1BlockRef(info)
	s.l1BlockRefsCache.Add(ref.Hash, ref)
	return ref, nil
}

func (s *L1Client) GoOrUpdatePreFetchReceipts(ctx context.Context, l1Start uint64) error {
	// Only advance the prefetcher; never let a lower-numbered caller (e.g. engine_queue
	// passing bq's origin) pull it backwards and clear the cache.
	for {
		old := s.preFetchHighWaterMark.Load()
		if l1Start <= old {
			// Already at or ahead of this request — nothing to do.
			s.preFetchReceiptsOnce.Do(func() {}) // ensure goroutine starts on first call
			return nil
		}
		if s.preFetchHighWaterMark.CompareAndSwap(old, l1Start) {
			break
		}
	}
	// Non-blocking: keep the highest requested start block in the channel.
	// Drain any stale value first, then send the new (higher) value.
	select {
	case <-s.preFetchReceiptsStartBlockChan:
		// discard stale lower value
	default:
	}
	s.preFetchReceiptsStartBlockChan <- l1Start
	s.preFetchReceiptsOnce.Do(func() {
		s.log.Info("pre-fetching receipts start", "startBlock", l1Start)
		s.isPreFetchReceiptsRunning.Store(true)
		go func() {
			var currentL1Block uint64
			var parentHash common.Hash
			for {
				select {
				case <-s.done:
					s.log.Info("pre-fetching receipts done")
					s.preFetchReceiptsClosedChan <- struct{}{}
					return
				case newStart := <-s.preFetchReceiptsStartBlockChan:
					if newStart < currentL1Block {
						// Backward jump: reorg or reset — clear cache and restart.
						s.log.Info("pre-fetching receipts reset backwards", "from", currentL1Block, "to", newStart)
						s.recProvider.GetReceiptsCache().RemoveAll()
						parentHash = common.Hash{}
						currentL1Block = newStart
					} else if newStart > currentL1Block {
						// Forward jump: advance without clearing cache.
						s.log.Info("pre-fetching receipts jumped forward", "from", currentL1Block, "to", newStart)
						currentL1Block = newStart
					}
				default:
					blockRef, err := s.L1BlockRefByLabel(ctx, eth.Unsafe)
					if err != nil {
						s.log.Debug("failed to fetch latest block ref", "err", err)
						time.Sleep(3 * time.Second)
						continue
					}

					if currentL1Block > blockRef.Number {
						s.log.Debug("current block height exceeds the latest block height of l1, will wait for a while.", "currentL1Block", currentL1Block, "l1Latest", blockRef.Number)
						time.Sleep(3 * time.Second)
						continue
					}

					// Sliding-window prefetch: keep maxConcurrentRequests goroutines running
					// at all times. Each goroutine signals completion immediately after fetching
					// one block, so the next window starts without waiting for the slowest block
					// (eliminates head-of-line blocking from the old wg.Wait batch approach).
					maxConcurrent := s.maxConcurrentRequests
					windowEnd := currentL1Block + uint64(maxConcurrent)
					if windowEnd > blockRef.Number+1 {
						windowEnd = blockRef.Number + 1
					}
					taskCount := int(windowEnd - currentL1Block)
					if taskCount <= 0 {
						time.Sleep(100 * time.Millisecond)
						continue
					}

					type fetchResult struct {
						ref eth.L1BlockRef
					}
					resultCh := make(chan fetchResult, taskCount)
					oldestFetchBlockNumber := currentL1Block

					for i := 0; i < taskCount; i++ {
						go func(blockNumber uint64) {
							for {
								select {
								case <-s.done:
									resultCh <- fetchResult{}
									return
								default:
									// Fetch full block (with txs) so transactionsCache is populated,
									// avoiding a second eth_getBlockByHash RPC in fetchReceiptsInner.
									info, txs, err := s.InfoAndTxsByNumber(ctx, blockNumber)
									if err != nil {
										s.log.Debug("failed to fetch block info+txs", "err", err, "blockNumber", blockNumber)
										time.Sleep(1 * time.Second)
										continue
									}
									s.headersCache.Add(info.Hash(), info)
									s.transactionsCache.Add(info.Hash(), txs)
									blockInfo := eth.InfoToL1BlockRef(info)
									s.blockRefByNumberCache.Store(blockInfo.Number, blockInfo)
									s.l1BlockRefsCache.Add(blockInfo.Hash, blockInfo)
									pair, ok := s.recProvider.GetReceiptsCache().Get(blockNumber, false)
									if ok && pair.blockHash == blockInfo.Hash {
										resultCh <- fetchResult{ref: blockInfo}
										return
									}
									isSuccess, err := s.PreFetchReceipts(ctx, blockInfo.Hash)
									if err != nil {
										s.log.Warn("failed to pre-fetch receipts", "err", err)
										time.Sleep(1 * time.Second)
										continue
									}
									if !isSuccess {
										s.log.Debug("receipts cache full, retrying", "blockNumber", blockNumber)
										time.Sleep(1 * time.Second)
										continue
									}
									s.log.Debug("pre-fetching receipts done", "block", blockInfo.Number, "hash", blockInfo.Hash)
									resultCh <- fetchResult{ref: blockInfo}
									return
								}
							}
						}(currentL1Block)
						currentL1Block++
					}

					// Collect results and check for reorg.
					var latestBlockHash common.Hash
					latestBlockNumber := uint64(0)
					var oldestBlockParentHash common.Hash
					for i := 0; i < taskCount; i++ {
						r := <-resultCh
						if r.ref.Number == 0 {
							continue // done signal from closed goroutine
						}
						if r.ref.Number > latestBlockNumber {
							latestBlockHash = r.ref.Hash
							latestBlockNumber = r.ref.Number
						}
						if r.ref.Number == oldestFetchBlockNumber {
							oldestBlockParentHash = r.ref.ParentHash
						}
					}

					if parentHash != (common.Hash{}) && oldestBlockParentHash != (common.Hash{}) && oldestBlockParentHash != parentHash && currentL1Block >= sequencerConfDepth+uint64(taskCount) {
						currentL1Block = currentL1Block - sequencerConfDepth - uint64(taskCount)
						s.log.Warn("pre-fetching receipts found l1 reOrg, return to an earlier block height for re-prefetching", "recordParentHash", parentHash, "unsafeParentHash", oldestBlockParentHash, "number", oldestFetchBlockNumber, "backToNumber", currentL1Block)
						parentHash = common.Hash{}
						continue
					}
					parentHash = latestBlockHash
				}
			}
		}()
	})
	return nil
}

func (s *L1Client) ClearReceiptsCacheBefore(blockNumber uint64) {
	s.recProvider.GetReceiptsCache().RemoveLessThan(blockNumber)
}

func (s *L1Client) Close() {
	close(s.done)
	if s.isPreFetchReceiptsRunning.Load() {
		<-s.preFetchReceiptsClosedChan
	}
	s.EthClient.Close()
}
