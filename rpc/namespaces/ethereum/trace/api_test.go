package trace

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/evmos/ethermint/rpc/backend"
	rpctypes "github.com/evmos/ethermint/rpc/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/stretchr/testify/require"
)

func TestMergeStorageContractsPreservesAndAddsAddresses(t *testing.T) {
	first := common.HexToAddress("0xaAaAaAaaAaAaAaaAaAAAAAAAAaaaAaAaAaaAaaAa")
	second := common.HexToAddress("0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB")
	got := mergeStorageContracts(
		map[common.Address]struct{}{second: {}},
		[]string{first.Hex(), second.Hex()},
	)
	require.ElementsMatch(t, []string{
		strings.ToLower(first.Hex()), strings.ToLower(second.Hex()),
	}, got)
}

type movedBlockBackend struct {
	backend.EVMBackend
	block *ctypes.ResultBlock
}

func (m movedBlockBackend) BlockNumberFromTendermint(rpctypes.BlockNumberOrHash) (rpctypes.BlockNumber, error) {
	return rpctypes.BlockNumber(m.block.Block.Height), nil
}

func (m movedBlockBackend) TendermintBlockByNumber(rpctypes.BlockNumber) (*ctypes.ResultBlock, error) {
	return m.block, nil
}

func TestHashLookupRejectsChangedBlock(t *testing.T) {
	block := tmtypes.MakeBlock(101485001, nil, nil, nil)
	block.ValidatorsHash = bytes.Repeat([]byte{1}, 32)
	hash := block.Hash()
	other := common.HexToHash("0x22")
	api := API{backend: movedBlockBackend{block: &ctypes.ResultBlock{Block: block, BlockID: tmtypes.BlockID{Hash: hash}}}}
	result, err := api.DebankBlockRaw(context.Background(), rpctypes.BlockNumberOrHash{BlockHash: &other})
	require.Nil(t, result)
	require.ErrorContains(t, err, "block identity mismatch")
	require.ErrorContains(t, err, "101485001")
}

type growingChainBackend struct {
	backend.EVMBackend
	head  atomic.Uint64
	polls atomic.Int32
	grow  int32
}

func (g *growingChainBackend) BlockNumber() (hexutil.Uint64, error) {
	if g.grow > 0 && g.polls.Add(1) == g.grow {
		g.head.Add(1)
	}
	return hexutil.Uint64(g.head.Load()), nil
}

func (g *growingChainBackend) TendermintBlockByNumber(h rpctypes.BlockNumber) (*ctypes.ResultBlock, error) {
	return &ctypes.ResultBlock{Block: tmtypes.MakeBlock(int64(h), nil, nil, nil)}, nil
}

func TestWaitNextBlock(t *testing.T) {
	defer func(timeout, poll time.Duration) { nextBlockTimeout, nextBlockPoll = timeout, poll }(nextBlockTimeout, nextBlockPoll)
	nextBlockPoll = time.Millisecond

	// The request arrives at the chain head and H+1 appears on the third poll.
	chain := &growingChainBackend{grow: 3}
	chain.head.Store(100)
	next, err := (&API{backend: chain}).waitNextBlock(context.Background(), 100)
	require.NoError(t, err)
	require.Equal(t, int64(101), next.Block.Height)

	nextBlockTimeout = 20 * time.Millisecond
	stalled := &growingChainBackend{}
	stalled.head.Store(100)
	_, err = (&API{backend: stalled}).waitNextBlock(context.Background(), 100)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	nextBlockTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (&API{backend: stalled}).waitNextBlock(ctx, 100)
	require.ErrorIs(t, err, context.Canceled)
}
