package trace

import (
	"context"
	"errors"
	"math/big"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/tendermint/tendermint/libs/log"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	sm "github.com/tendermint/tendermint/state"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/tharsis/ethermint/debank/replay"
	dtypes "github.com/tharsis/ethermint/debank/types"
	rpctypes "github.com/tharsis/ethermint/rpc/ethereum/types"
	evmtypes "github.com/tharsis/ethermint/x/evm/types"
)

type segmentTestChain struct {
	resultsChain
	onStoreGet func() error
}

func (c *segmentTestChain) StoreGet(_ int64, store string, _ []byte) ([]byte, error) {
	if c.onStoreGet != nil {
		if err := c.onStoreGet(); err != nil {
			return nil, err
		}
	}
	if store == "params" {
		return []byte(`"basecro"`), nil
	}
	return make([]byte, 20), nil
}

func (c *segmentTestChain) ConsensusParams(int64) (tmproto.ConsensusParams, error) {
	return *tmtypes.DefaultConsensusParams(), nil
}

type segmentTestExecutor struct {
	checkOnlyExecutor
	result   *replay.Result
	finished []bool
	dropped  []int64
	ackErr   error
}

func (e *segmentTestExecutor) ReplaySegmentBlock(_ context.Context, _ replay.Request, _ replay.Segment) (*replay.Result, func(bool) error, error) {
	return e.result, func(ok bool) error {
		e.finished = append(e.finished, ok)
		return e.ackErr
	}, nil
}

func (e *segmentTestExecutor) DropReplaySegment(_ context.Context, _ replay.Segment, height int64) error {
	e.dropped = append(e.dropped, height)
	return nil
}

func segmentTestAPI(t *testing.T) (*API, *segmentTestChain, *segmentTestExecutor) {
	t.Helper()
	validator := tmtypes.NewValidator(ed25519.GenPrivKey().PubKey(), 10)
	set := tmtypes.NewValidatorSet([]*tmtypes.Validator{validator})
	parent := &tmtypes.Header{Height: 9, ChainID: "cronosmainnet_25-1", ValidatorsHash: set.Hash()}
	parentID := tmtypes.BlockID{Hash: parent.Hash(), PartSetHeader: tmtypes.PartSetHeader{Total: 1, Hash: make([]byte, 32)}}
	block := tmtypes.MakeBlock(10, []tmtypes.Tx{{1}}, &tmtypes.Commit{
		Height: 9, BlockID: parentID, Signatures: []tmtypes.CommitSig{tmtypes.NewCommitSigAbsent()},
	}, nil)
	block.ChainID, block.LastBlockID, block.ValidatorsHash, block.ProposerAddress = parent.ChainID, parentID, set.Hash(), validator.Address
	msg := ethTx(0)
	hash := msg.AsTransaction().Hash()
	results := &tmstate.ABCIResponses{
		BeginBlock: &abci.ResponseBeginBlock{}, EndBlock: &abci.ResponseEndBlock{},
		DeliverTxs: []*abci.ResponseDeliverTx{{Data: msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: hash.Hex(), GasUsed: 21000}), GasUsed: 21000}},
	}
	root := make([]byte, 32)
	next := &tmtypes.Header{Height: 11, LastBlockID: tmtypes.BlockID{Hash: block.Hash()}, LastResultsHash: sm.ABCIResponsesResultsHash(results), AppHash: root}
	chain := &segmentTestChain{resultsChain: resultsChain{
		fakeChain: fakeChain{headers: map[int64]*tmtypes.Header{9: parent, 11: next}, vals: map[int64]*tmtypes.ValidatorSet{9: set}},
		block:     block, results: results,
	}}
	executor := &segmentTestExecutor{result: &replay.Result{
		Responses: results, AppHash: root, Transactions: []replay.Transaction{{
			Hash: hash, GasUsed: 21000, Trace: traceJSON(t, dtypes.TraceResult{Traces: []dtypes.Trace{{TxID: hash.Hex()}}}),
		}},
	}}
	api := NewAPI(log.NewNopLogger(), chain, func([]byte) (sdk.Tx, error) { return msg, nil }, big.NewInt(25), executor)
	return api, chain, executor
}

func TestDebankBlockSegmentConfirmation(t *testing.T) {
	height := rpctypes.BlockNumber(10)
	id := rpctypes.BlockNumberOrHash{BlockNumber: &height}
	segment := replay.Segment{ID: "a", Start: 10, End: 12}
	for _, name := range []string{"verified", "bad-root", "assembly-failed", "canceled", "panicked", "ack-failed", "no-evm"} {
		t.Run(name, func(t *testing.T) {
			api, chain, executor := segmentTestAPI(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "bad-root":
				executor.result.AppHash = []byte{1}
			case "assembly-failed":
				chain.onStoreGet = func() error { return errors.New("metadata unavailable") }
			case "canceled":
				chain.onStoreGet = func() error { cancel(); return nil }
			case "panicked":
				chain.onStoreGet = func() error { panic("injected") }
				require.Panics(t, func() { _, _ = api.DebankBlockSegment(ctx, id, segment) })
				require.Equal(t, []bool{false}, executor.finished)
				return
			case "ack-failed":
				executor.ackErr = errors.New("worker disconnected")
			case "no-evm":
				chain.results.DeliverTxs[0].Code = 5
				chain.headers[11].LastResultsHash = sm.ABCIResponsesResultsHash(chain.results)
			}
			result, err := api.DebankBlockSegment(ctx, id, segment)
			switch name {
			case "verified":
				require.NoError(t, err)
				require.Contains(t, string(result), `"block_file"`)
				require.Equal(t, []bool{true}, executor.finished)
			case "no-evm":
				require.NoError(t, err)
				require.Contains(t, string(result), `"block_file"`)
				require.Empty(t, executor.finished)
				require.Equal(t, []int64{10}, executor.dropped)
			case "ack-failed":
				require.ErrorIs(t, err, executor.ackErr)
				require.Nil(t, result)
				require.Equal(t, []bool{true}, executor.finished)
			default:
				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, []bool{false}, executor.finished)
			}
		})
	}
}

func TestDebankBlockSegmentParameters(t *testing.T) {
	api := &API{}
	seg := replay.Segment{ID: "a", Start: 1, End: 4096}
	require.NoError(t, seg.Validate(4096))
	for _, height := range []rpctypes.BlockNumber{rpctypes.EthEarliestBlockNumber, rpctypes.EthLatestBlockNumber, rpctypes.EthPendingBlockNumber, 4097} {
		_, err := api.DebankBlockSegment(context.Background(), rpctypes.BlockNumberOrHash{BlockNumber: &height}, seg)
		require.Error(t, err)
	}
	seg.End++
	require.Error(t, seg.Validate(1))
	seg.End, seg.ID = 1, ""
	require.Error(t, seg.Validate(1))
}
