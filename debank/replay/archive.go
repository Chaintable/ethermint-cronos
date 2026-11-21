package replay

import (
	"bytes"
	"context"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/rpc/client"
	"github.com/cometbft/cometbft/state"
	tmtypes "github.com/cometbft/cometbft/types"
)

// ArchiveRequest authenticates the parent validator set and reconstructs the
// complete native FinalizeBlock input, including evidence and commit votes.
func ArchiveRequest(ctx context.Context, c client.SignClient, block *tmtypes.Block) (Request, []byte, error) {
	var req Request
	if err := block.ValidateBasic(); err != nil {
		return req, nil, err
	}
	if block.Height < 2 || block.LastCommit == nil {
		return req, nil, fmt.Errorf("missing parent commit at %d", block.Height)
	}
	parent := block.Height - 1
	header, err := c.Header(ctx, &parent)
	if err != nil {
		return req, nil, fmt.Errorf("parent header: %w", err)
	}
	if header == nil || header.Header == nil || !bytes.Equal(header.Header.Hash(), block.LastBlockID.Hash) {
		return req, nil, fmt.Errorf("parent header hash mismatch")
	}
	var vals []*tmtypes.Validator
	perPage := 100
	for page := 1; len(vals) < block.LastCommit.Size(); page++ {
		r, err := c.Validators(ctx, &parent, &page, &perPage)
		if err != nil {
			return req, nil, fmt.Errorf("validators at %d: %w", parent, err)
		}
		if r == nil || r.BlockHeight != parent || r.Total != block.LastCommit.Size() || len(r.Validators) == 0 || len(vals)+len(r.Validators) > r.Total {
			return req, nil, fmt.Errorf("incomplete validators at %d page %d", parent, page)
		}
		for _, v := range r.Validators {
			if v == nil || v.PubKey == nil {
				return req, nil, fmt.Errorf("missing validator at %d index %d", parent, len(vals))
			}
			vals = append(vals, v)
		}
	}
	set := &tmtypes.ValidatorSet{Validators: vals}
	if !bytes.Equal(set.Hash(), header.Header.ValidatorsHash) {
		return req, nil, fmt.Errorf("parent validator hash mismatch")
	}
	req = Request{ChainID: block.ChainID, ParentHash: block.AppHash, Request: &abci.RequestFinalizeBlock{
		Hash: block.Hash(), NextValidatorsHash: block.NextValidatorsHash, ProposerAddress: block.ProposerAddress,
		Height: block.Height, Time: block.Time, DecidedLastCommit: state.BuildLastCommitInfo(block, set, 1),
		Misbehavior: block.Evidence.Evidence.ToABCI(), Txs: block.Txs.ToSliceOfBytes(),
	}}
	return req, header.Header.AppHash, nil
}
