package trace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/evmos/ethermint/debank/replay"
	rpctypes "github.com/evmos/ethermint/rpc/types"
)

// DebankBlockSegment traces one explicit height in a series of calls sharing
// the same segment ID. The response is identical to trace_debankBlock.
func (api *API) DebankBlockSegment(ctx context.Context, id rpctypes.BlockNumberOrHash, segment replay.Segment) (json.RawMessage, error) {
	if id.BlockNumber == nil || id.BlockHash != nil || *id.BlockNumber <= 0 {
		return nil, fmt.Errorf("segment replay requires an explicit block height")
	}
	height := id.BlockNumber.Int64()
	if err := segment.Validate(height); err != nil {
		return nil, err
	}
	executor, ok := api.executor.(replay.SegmentExecutor)
	if !ok {
		return nil, fmt.Errorf("segment replay executor unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var finish func(bool) error
	// A failed verification, encoding error or panic must discard the working App.
	defer func() {
		if finish != nil {
			_ = finish(false)
		}
	}()
	run := func(ctx context.Context, req replay.Request) (*replay.Result, error) {
		var res *replay.Result
		var err error
		res, finish, err = executor.ReplaySegmentBlock(ctx, req, segment)
		if err == nil && finish == nil {
			return nil, fmt.Errorf("segment replay missing acknowledgement for block %d", height)
		}
		return res, err
	}
	output, err := api.block(ctx, id, false, run)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeOutput(output)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if finish == nil {
		// The no-EVM fast path did not advance the retained Cosmos state.
		err = executor.DropReplaySegment(ctx, segment, height)
	} else {
		ack := finish
		finish = nil
		err = ack(true)
	}
	if err != nil {
		return nil, fmt.Errorf("finish replay segment at block %d: %w", height, err)
	}
	return encoded, nil
}
