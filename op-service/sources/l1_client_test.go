package sources

import (
	"context"
	"testing"
	"time"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/testlog"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestGoOrUpdatePreFetchReceipts(t *testing.T) {
	t.Run("handleReOrg", func(t *testing.T) {
		m := new(mockRPC)
		ctx := context.Background()
		clientLog := testlog.Logger(t, log.LvlDebug)
		latestHead := &RPCHeader{
			ParentHash:      randHash(),
			UncleHash:       common.Hash{},
			Coinbase:        common.Address{},
			Root:            types.EmptyRootHash,
			TxHash:          types.EmptyTxsHash,
			ReceiptHash:     types.EmptyReceiptsHash,
			Bloom:           eth.Bytes256{},
			Difficulty:      hexutil.Big{},
			Number:          100,
			GasLimit:        0,
			GasUsed:         0,
			Time:            0,
			Extra:           nil,
			MixDigest:       common.Hash{},
			Nonce:           types.BlockNonce{},
			BaseFee:         nil,
			WithdrawalsRoot: nil,
			Hash:            randHash(),
		}
		m.On("CallContext", ctx, new(*RPCHeader),
			"eth_getBlockByNumber", []any{"latest", false}).Run(func(args mock.Arguments) {
			*args[1].(**RPCHeader) = latestHead
		}).Return([]error{nil})
		for i := 81; i <= 90; i++ {
			currentHead := &RPCHeader{
				ParentHash:      randHash(),
				UncleHash:       common.Hash{},
				Coinbase:        common.Address{},
				Root:            types.EmptyRootHash,
				TxHash:          types.EmptyTxsHash,
				ReceiptHash:     types.EmptyReceiptsHash,
				Bloom:           eth.Bytes256{},
				Difficulty:      hexutil.Big{},
				Number:          hexutil.Uint64(i),
				GasLimit:        0,
				GasUsed:         0,
				Time:            0,
				Extra:           nil,
				MixDigest:       common.Hash{},
				Nonce:           types.BlockNonce{},
				BaseFee:         nil,
				WithdrawalsRoot: nil,
				Hash:            randHash(),
			}
			currentBlock := &RPCBlock{
				RPCHeader:    *currentHead,
				Transactions: []*types.Transaction{},
			}
			m.On("CallContext", ctx, new(*RPCHeader),
				"eth_getBlockByNumber", []any{numberID(i).Arg(), false}).Once().Run(func(args mock.Arguments) {
				*args[1].(**RPCHeader) = currentHead
			}).Return([]error{nil})
			m.On("CallContext", ctx, new(*RPCBlock),
				"eth_getBlockByHash", []any{currentHead.Hash, true}).Once().Run(func(args mock.Arguments) {
				*args[1].(**RPCBlock) = currentBlock
			}).Return([]error{nil})
		}
		for i := 91; i <= 100; i++ {
			currentHead := &RPCHeader{
				ParentHash:      randHash(),
				UncleHash:       common.Hash{},
				Coinbase:        common.Address{},
				Root:            types.EmptyRootHash,
				TxHash:          types.EmptyTxsHash,
				ReceiptHash:     types.EmptyReceiptsHash,
				Bloom:           eth.Bytes256{},
				Difficulty:      hexutil.Big{},
				Number:          hexutil.Uint64(i),
				GasLimit:        0,
				GasUsed:         0,
				Time:            0,
				Extra:           nil,
				MixDigest:       common.Hash{},
				Nonce:           types.BlockNonce{},
				BaseFee:         nil,
				WithdrawalsRoot: nil,
				Hash:            randHash(),
			}
			m.On("CallContext", ctx, new(*RPCHeader),
				"eth_getBlockByNumber", []any{numberID(i).Arg(), false}).Once().Run(func(args mock.Arguments) {
				*args[1].(**RPCHeader) = currentHead
			}).Return([]error{nil})
			currentBlock := &RPCBlock{
				RPCHeader:    *currentHead,
				Transactions: []*types.Transaction{},
			}
			m.On("CallContext", ctx, new(*RPCBlock),
				"eth_getBlockByHash", []any{currentHead.Hash, true}).Once().Run(func(args mock.Arguments) {
				*args[1].(**RPCBlock) = currentBlock
			}).Return([]error{nil})
		}
		var lastParentHeader common.Hash
		var real100Hash common.Hash
		for i := 76; i <= 100; i++ {
			currentHead := &RPCHeader{
				ParentHash:      lastParentHeader,
				UncleHash:       common.Hash{},
				Coinbase:        common.Address{},
				Root:            types.EmptyRootHash,
				TxHash:          types.EmptyTxsHash,
				ReceiptHash:     types.EmptyReceiptsHash,
				Bloom:           eth.Bytes256{},
				Difficulty:      hexutil.Big{},
				Number:          hexutil.Uint64(i),
				GasLimit:        0,
				GasUsed:         0,
				Time:            0,
				Extra:           nil,
				MixDigest:       common.Hash{},
				Nonce:           types.BlockNonce{},
				BaseFee:         nil,
				WithdrawalsRoot: nil,
				Hash:            randHash(),
			}
			if i == 100 {
				real100Hash = currentHead.Hash
			}
			lastParentHeader = currentHead.Hash
			m.On("CallContext", ctx, new(*RPCHeader),
				"eth_getBlockByNumber", []any{numberID(i).Arg(), false}).Once().Run(func(args mock.Arguments) {
				*args[1].(**RPCHeader) = currentHead
			}).Return([]error{nil})
			currentBlock := &RPCBlock{
				RPCHeader:    *currentHead,
				Transactions: []*types.Transaction{},
			}
			m.On("CallContext", ctx, new(*RPCBlock),
				"eth_getBlockByHash", []any{currentHead.Hash, true}).Once().Run(func(args mock.Arguments) {
				*args[1].(**RPCBlock) = currentBlock
			}).Return([]error{nil})
		}
		s, err := NewL1Client(m, clientLog, nil, L1ClientDefaultConfig(&rollup.Config{SeqWindowSize: 1000}, true, RPCKindBasic))
		require.NoError(t, err)
		err2 := s.GoOrUpdatePreFetchReceipts(ctx, 81)
		require.NoError(t, err2)
		time.Sleep(1 * time.Second)
		pair, ok := s.recProvider.GetReceiptsCache().Get(100, false)
		require.True(t, ok, "100 cache miss")
		require.Equal(t, real100Hash, pair.blockHash, "block 100 hash is different,want:%s,but:%s", real100Hash, pair.blockHash)
		_, ok2 := s.recProvider.GetReceiptsCache().Get(76, false)
		require.True(t, ok2, "76 cache miss")
	})
}

func makeMinimalRPCHeader(number uint64) *RPCHeader {
	return &RPCHeader{
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		UncleHash:   types.EmptyUncleHash,
		Number:      hexutil.Uint64(number),
		Hash:        randHash(),
	}
}

func TestBatchPrefetchL1BlockRefsByNumber_PopulatesCache(t *testing.T) {
	m := new(mockRPC)
	ctx := context.Background()
	lgr := testlog.Logger(t, log.LvlDebug)
	client, err := NewL1Client(m, lgr, nil, L1ClientDefaultConfig(&rollup.Config{SeqWindowSize: 1000}, true, RPCKindBasic))
	require.NoError(t, err)

	h100 := makeMinimalRPCHeader(100)
	h101 := makeMinimalRPCHeader(101)
	h102 := makeMinimalRPCHeader(102)

	m.On("BatchCallContext", ctx, mock.Anything).
		Run(func(args mock.Arguments) {
			elems := args.Get(1).([]rpc.BatchElem)
			headers := []*RPCHeader{h100, h101, h102}
			for i := range elems {
				*elems[i].Result.(**RPCHeader) = headers[i]
			}
		}).
		Return([]error{nil})

	err = client.BatchPrefetchL1BlockRefsByNumber(ctx, 100, 102, 10)
	require.NoError(t, err)

	// All three refs should now be in the cache
	_, ok100 := client.l1BlockRefsCache.Get(h100.Hash)
	_, ok101 := client.l1BlockRefsCache.Get(h101.Hash)
	_, ok102 := client.l1BlockRefsCache.Get(h102.Hash)
	require.True(t, ok100, "block 100 not in cache")
	require.True(t, ok101, "block 101 not in cache")
	require.True(t, ok102, "block 102 not in cache")
	m.AssertExpectations(t)
}

func TestBatchPrefetchL1BlockRefsByNumber_BatchChunking(t *testing.T) {
	m := new(mockRPC)
	ctx := context.Background()
	lgr := testlog.Logger(t, log.LvlDebug)
	client, err := NewL1Client(m, lgr, nil, L1ClientDefaultConfig(&rollup.Config{SeqWindowSize: 1000}, true, RPCKindBasic))
	require.NoError(t, err)

	// 5 blocks with batchSize=2 → 3 BatchCallContext calls (2+2+1)
	headers := make([]*RPCHeader, 5)
	for i := range headers {
		headers[i] = makeMinimalRPCHeader(uint64(100 + i))
	}

	callCount := 0
	m.On("BatchCallContext", ctx, mock.Anything).
		Run(func(args mock.Arguments) {
			elems := args.Get(1).([]rpc.BatchElem)
			offset := callCount * 2
			for i := range elems {
				*elems[i].Result.(**RPCHeader) = headers[offset+i]
			}
			callCount++
		}).
		Return([]error{nil})

	err = client.BatchPrefetchL1BlockRefsByNumber(ctx, 100, 104, 2)
	require.NoError(t, err)
	require.Equal(t, 3, callCount, "expected 3 batch calls for 5 blocks with batchSize=2")
	m.AssertExpectations(t)
}
