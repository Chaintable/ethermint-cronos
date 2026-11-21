package replay

import (
	"context"
	"fmt"

	abci "github.com/cometbft/cometbft/abci/types"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	"github.com/cometbft/cometbft/rpc/client"
	tm "github.com/cometbft/cometbft/types"
)

type BlockResult struct {
	*tmstate.ABCIResponses
	Block  *tm.Block
	Parent *tm.Header
}

// FromArchive uses the original Cosmos block, including evidence and commit
// signatures. The next block authenticates both the results and the post-state.
func FromArchive(ctx context.Context, source client.Client, app abci.Application, height int64) (*BlockResult, error) {
	at := func(h int64) (*tm.Block, error) {
		r, err := source.Block(ctx, &h)
		if err != nil {
			return nil, err
		}
		if r == nil || r.Block == nil || r.Block.Height != h || r.Block.ChainID != "cronosmainnet_25-1" {
			return nil, fmt.Errorf("unexpected archive block at %d", h)
		}
		return r.Block, nil
	}
	block, err := at(height)
	if err != nil {
		return nil, err
	}
	next, err := at(height + 1)
	if err != nil {
		return nil, err
	}
	var parent *tm.Header
	var vals []*tm.Validator
	if height > 1 {
		prev, err := at(height - 1)
		if err != nil {
			return nil, err
		}
		parent = &prev.Header
		if block.LastCommit == nil {
			return nil, fmt.Errorf("missing last commit at %d", height)
		}
		total := len(block.LastCommit.Signatures)
		perPage := 100
		for page := 1; len(vals) < total; page++ {
			v, err := source.Validators(ctx, &parent.Height, &page, &perPage)
			if err != nil {
				return nil, err
			}
			if v == nil || v.BlockHeight != parent.Height || v.Total != total || len(v.Validators) == 0 || len(vals)+len(v.Validators) > total {
				return nil, fmt.Errorf("incomplete validators at %d page %d", parent.Height, page)
			}
			vals = append(vals, v.Validators...)
		}
	}
	res, err := Execute(app, block, parent, &next.Header, vals)
	if err != nil {
		return nil, err
	}
	return &BlockResult{res, block, parent}, nil
}
