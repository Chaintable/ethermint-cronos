package trace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/evmos/ethermint/debank/replay"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
)

type segmentTestExecutor struct {
	countingExecutor
	finished []bool
	dropped  []int64
	ackErr   error
}

func (e *segmentTestExecutor) ReplaySegmentBlock(_ context.Context, _ replay.Request, _ replay.Segment) (*replay.Result, func(bool) error, error) {
	return &replay.Result{}, func(ok bool) error {
		e.finished = append(e.finished, ok)
		return e.ackErr
	}, nil
}

func (e *segmentTestExecutor) DropReplaySegment(_ context.Context, _ replay.Segment, height int64) error {
	e.dropped = append(e.dropped, height)
	return nil
}

// The working App advances only after the whole block passed every check.
func TestDebankBlockSegmentConfirmation(t *testing.T) {
	height := rpctypes.BlockNumber(10)
	id := rpctypes.BlockNumberOrHash{BlockNumber: &height}
	segment := replay.Segment{ID: "a", Start: 10, End: 12}
	output := &dtypes.DebankOutPut{BlockFile: &dtypes.BlockFile{}}
	for _, name := range []string{"verified", "failed-after-replay", "canceled", "panicked", "ack-failed", "no-evm"} {
		t.Run(name, func(t *testing.T) {
			executor := &segmentTestExecutor{}
			api := &API{executor: executor}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			raw := func(ctx context.Context, _ rpctypes.BlockNumberOrHash, run replayBlockFunc) (*dtypes.DebankOutPut, error) {
				if name == "no-evm" {
					return output, nil
				}
				if _, err := run(ctx, replay.Request{}); err != nil {
					return nil, err
				}
				switch name {
				case "failed-after-replay":
					return nil, errors.New("next header AppHash mismatch")
				case "canceled":
					cancel()
				case "panicked":
					panic("injected")
				case "ack-failed":
					executor.ackErr = errors.New("worker disconnected")
				}
				return output, nil
			}
			if name == "panicked" {
				require.Panics(t, func() { _, _ = api.debankBlockSegment(ctx, id, segment, raw) })
				require.Equal(t, []bool{false}, executor.finished)
				return
			}
			result, err := api.debankBlockSegment(ctx, id, segment, raw)
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
