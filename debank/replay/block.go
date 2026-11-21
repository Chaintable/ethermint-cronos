// Package replay executes historical blocks against an isolated application.
package replay

import (
	"bytes"
	"fmt"

	abci "github.com/cometbft/cometbft/abci/types"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tm "github.com/cometbft/cometbft/types"
)

// Execute consumes a private app positioned at H-1 (after InitChain for H1).
// Discard the private app on error: BeginBlock and Commit mutate its state.
func Execute(app abci.Application, block *tm.Block, parent, next *tm.Header, validators []*tm.Validator) (*tmstate.ABCIResponses, error) {
	if block == nil || next == nil {
		return nil, fmt.Errorf("missing replay block or successor header")
	}
	if err := block.ValidateBasic(); err != nil {
		return nil, err
	}
	if next.ChainID != block.ChainID || next.Height != block.Height+1 || !bytes.Equal(next.LastBlockID.Hash, block.Hash()) {
		return nil, fmt.Errorf("successor does not commit to block %d", block.Height)
	}
	info := app.Info(abci.RequestInfo{})
	if info.LastBlockHeight != block.Height-1 {
		return nil, fmt.Errorf("application height %d, expected %d", info.LastBlockHeight, block.Height-1)
	}
	commit := abci.CommitInfo{}
	if block.Height > 1 {
		if parent == nil || parent.Height != block.Height-1 || !bytes.Equal(parent.Hash(), block.LastBlockID.Hash) || !bytes.Equal(info.LastBlockAppHash, block.AppHash) {
			return nil, fmt.Errorf("parent identity mismatch at %d", block.Height)
		}
		if len(validators) != len(block.LastCommit.Signatures) {
			return nil, fmt.Errorf("validator count mismatch at %d", parent.Height)
		}
		for _, val := range validators {
			if val == nil || val.PubKey == nil {
				return nil, fmt.Errorf("invalid validator at %d", parent.Height)
			}
		}
		set := &tm.ValidatorSet{Validators: validators}
		if !bytes.Equal(set.Hash(), parent.ValidatorsHash) {
			return nil, fmt.Errorf("validator hash mismatch at %d", parent.Height)
		}
		commit.Round = block.LastCommit.Round
		for i, sig := range block.LastCommit.Signatures {
			val := validators[i]
			if !sig.Absent() && !bytes.Equal(sig.ValidatorAddress, val.Address) {
				return nil, fmt.Errorf("validator order mismatch at index %d", i)
			}
			commit.Votes = append(commit.Votes, abci.VoteInfo{Validator: tm.TM2PB.Validator(val), SignedLastBlock: !sig.Absent()})
		}
	}
	begin := abci.RequestBeginBlock{Hash: block.Hash(), Header: *block.Header.ToProto(), LastCommitInfo: commit}
	for _, evidence := range block.Evidence.Evidence {
		begin.ByzantineValidators = append(begin.ByzantineValidators, evidence.ABCI()...)
	}
	start := app.BeginBlock(begin)
	results := &tmstate.ABCIResponses{BeginBlock: &start}
	for _, tx := range block.Txs {
		result := app.DeliverTx(abci.RequestDeliverTx{Tx: tx})
		results.DeliverTxs = append(results.DeliverTxs, &result)
	}
	end := app.EndBlock(abci.RequestEndBlock{Height: block.Height})
	results.EndBlock = &end
	if !bytes.Equal(tm.NewResults(results.DeliverTxs).Hash(), next.LastResultsHash) {
		return nil, fmt.Errorf("transaction results hash mismatch at %d", block.Height)
	}
	post := app.Commit()
	if !bytes.Equal(post.Data, next.AppHash) {
		return nil, fmt.Errorf("application hash at %d: got %X, want %X", block.Height, post.Data, next.AppHash)
	}
	return results, nil
}
