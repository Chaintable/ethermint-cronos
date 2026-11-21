package replay

import (
	"bytes"
	"context"
	"fmt"

	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
	"github.com/tendermint/tendermint/rpc/client"
	tm "github.com/tendermint/tendermint/types"
)

type BlockResult struct {
	*tmstate.ABCIResponses
	Block  *tm.Block
	Parent *tm.Header
}

// ArchiveReader keeps a three-block window and the last authenticated
// validator set for a single sequential replay worker.
type ArchiveReader struct {
	source        client.Client
	blocks        [3]*tm.Block
	validators    []*tm.Validator
	validatorHash []byte
}

func NewArchiveReader(source client.Client) *ArchiveReader { return &ArchiveReader{source: source} }

func (a *ArchiveReader) block(ctx context.Context, height int64) (*tm.Block, error) {
	slot := height % int64(len(a.blocks))
	if b := a.blocks[slot]; b != nil && b.Height == height {
		return b, nil
	}
	result, err := a.source.Block(ctx, &height)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Block == nil || result.Block.Height != height || result.Block.ChainID != "cronosmainnet_25-1" {
		return nil, fmt.Errorf("unexpected archive block at %d", height)
	}
	a.blocks[slot] = result.Block
	return result.Block, nil
}

// FromArchive executes one block in isolation from other archive reads.
func FromArchive(ctx context.Context, source client.Client, app abci.Application, height int64) (*BlockResult, error) {
	return NewArchiveReader(source).Replay(ctx, app, height)
}

// Replay uses the original Cosmos block, evidence and commit votes. Execute
// authenticates the parent validator set, the ABCI results and the post-state.
func (a *ArchiveReader) Replay(ctx context.Context, app abci.Application, height int64) (*BlockResult, error) {
	if height < 1 {
		return nil, fmt.Errorf("invalid replay height %d", height)
	}
	block, err := a.block(ctx, height)
	if err != nil {
		return nil, err
	}
	next, err := a.block(ctx, height+1)
	if err != nil {
		return nil, err
	}
	var parent *tm.Header
	var vals []*tm.Validator
	if height > 1 {
		prev, err := a.block(ctx, height-1)
		if err != nil {
			return nil, err
		}
		parent = &prev.Header
		if block.LastCommit == nil {
			return nil, fmt.Errorf("missing last commit at %d", height)
		}
		total := len(block.LastCommit.Signatures)
		if len(a.validators) == total && bytes.Equal(parent.ValidatorsHash, a.validatorHash) {
			vals = a.validators
		}
		perPage := 100
		for page := 1; len(vals) < total; page++ {
			v, err := a.source.Validators(ctx, &parent.Height, &page, &perPage)
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
	if parent != nil {
		a.validators = vals
		a.validatorHash = append(a.validatorHash[:0], parent.ValidatorsHash...)
	}
	return &BlockResult{res, block, parent}, nil
}
