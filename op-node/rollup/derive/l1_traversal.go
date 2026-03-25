package derive

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// L1 Traversal fetches the next L1 block and exposes it through the progress API

type L1BlockRefByNumberFetcher interface {
	L1BlockRefByNumber(context.Context, uint64) (eth.L1BlockRef, error)
	FetchReceipts(ctx context.Context, blockHash common.Hash) (eth.BlockInfo, types.Receipts, error)
	PreFetchReceipts(ctx context.Context, blockHash common.Hash) (bool, error)
	FetchSystemConfigLogs(ctx context.Context, fromBlock, toBlock uint64, addr common.Address, topic common.Hash) ([]*types.Log, error)
}

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

var _ ResettableStage = (*L1Traversal)(nil)

func NewL1Traversal(log log.Logger, cfg *rollup.Config, l1Blocks L1BlockRefByNumberFetcher, tCfg L1TraversalConfig) *L1Traversal {
	return &L1Traversal{
		log:            log,
		l1Blocks:       l1Blocks,
		cfg:            cfg,
		tCfg:           tCfg,
		syscfgLogCache: make(map[uint64][]*types.Log),
	}
}

func (l1t *L1Traversal) Origin() eth.L1BlockRef {
	return l1t.block
}

// NextL1Block returns the next block. It does not advance, but it can only be
// called once before returning io.EOF
func (l1t *L1Traversal) NextL1Block(_ context.Context) (eth.L1BlockRef, error) {
	if !l1t.done {
		l1t.done = true
		return l1t.block, nil
	} else {
		return eth.L1BlockRef{}, io.EOF
	}
}

// AdvanceL1Block advances the internal state of L1 Traversal
func (l1t *L1Traversal) AdvanceL1Block(ctx context.Context) error {
	origin := l1t.block
	nextL1Origin, err := l1t.l1Blocks.L1BlockRefByNumber(ctx, origin.Number+1)
	if errors.Is(err, ethereum.NotFound) {
		l1t.log.Debug("can't find next L1 block info (yet)", "number", origin.Number+1, "origin", origin)
		return io.EOF
	} else if err != nil {
		return NewTemporaryError(fmt.Errorf("failed to find L1 block info by number, at origin %s next %d: %w", origin, origin.Number+1, err))
	}
	if l1t.block.Hash != nextL1Origin.ParentHash {
		return NewResetError(fmt.Errorf("detected L1 reorg from %s to %s with conflicting parent %s", l1t.block, nextL1Origin, nextL1Origin.ParentID()))
	}

	// Parse L1 receipts / logs of the given block and update the L1 system configuration
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
		// TODO: may need to pass l1origin milli-timestamp later if IsEcotone() use the milli-timestamp
		if err := UpdateSystemConfigWithL1Logs(&l1t.sysCfg, blockLogs, l1t.cfg, nextL1Origin.Number, nextL1Origin.Time); err != nil {
			return NewCriticalError(fmt.Errorf("failed to update L1 sysCfg with logs from block %s: %w", nextL1Origin, err))
		}
	} else {
		_, receipts, err := l1t.l1Blocks.FetchReceipts(ctx, nextL1Origin.Hash)
		if err != nil {
			return NewTemporaryError(fmt.Errorf("failed to fetch receipts of L1 block %s (parent: %s) for L1 sysCfg update: %w", nextL1Origin, origin, err))
		}
		// TODO: may need to pass l1origin milli-timestamp later if IsEcotone() use the milli-timestamp
		if err := UpdateSystemConfigWithL1Receipts(&l1t.sysCfg, receipts, l1t.cfg, nextL1Origin.Time); err != nil {
			// the sysCfg changes should always be formatted correctly.
			return NewCriticalError(fmt.Errorf("failed to update L1 sysCfg with receipts from block %s: %w", nextL1Origin, err))
		}
	}

	l1t.block = nextL1Origin
	l1t.done = false
	return nil
}

// Reset sets the internal L1 block to the supplied base.
func (l1t *L1Traversal) Reset(ctx context.Context, base eth.L1BlockRef, cfg eth.SystemConfig) error {
	l1t.block = base
	l1t.done = false
	l1t.sysCfg = cfg
	l1t.syscfgLogCache = make(map[uint64][]*types.Log)
	l1t.nextLogFetch = 0
	l1t.log.Info("completed reset of derivation pipeline", "origin", base)
	return io.EOF
}

func (l1c *L1Traversal) SystemConfig() eth.SystemConfig {
	return l1c.sysCfg
}
