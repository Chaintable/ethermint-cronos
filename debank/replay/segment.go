package replay

import (
	"context"
	"fmt"
)

// Segment identifies one ordered run of heights on an isolated application.
type Segment struct {
	ID    string `json:"id"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

func (s Segment) Validate(height int64) error {
	if s.ID == "" || len(s.ID) > 128 {
		return fmt.Errorf("invalid replay segment ID length")
	}
	if s.Start < 1 || s.End < s.Start || s.End-s.Start >= 4096 || height < s.Start || height > s.End {
		return fmt.Errorf("invalid replay segment [%d,%d] for height %d", s.Start, s.End, height)
	}
	return nil
}

// SegmentExecutor retains speculative state until the caller confirms the
// completed block. The caller must invoke finish exactly once on success.
type SegmentExecutor interface {
	ReplaySegmentBlock(ctx context.Context, req Request, segment Segment) (result *Result, finish func(verified bool) error, err error)
	DropReplaySegment(ctx context.Context, segment Segment, height int64) error
}
