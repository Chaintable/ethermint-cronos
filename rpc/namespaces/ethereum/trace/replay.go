package trace

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/rpc/client"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/state"
	tmtypes "github.com/cometbft/cometbft/types"

	"github.com/evmos/ethermint/debank/replay"
)

func (api *API) signClient() (client.SignClient, error) {
	c, ok := api.clientCtx.Client.(client.SignClient)
	if !ok {
		return nil, fmt.Errorf("client lacks historical consensus queries")
	}
	return c, nil
}

// parentHeader returns header H-1 after checking that block H references it.
func (api *API) parentHeader(ctx context.Context, block *tmtypes.Block) (*tmtypes.Header, error) {
	c, err := api.signClient()
	if err != nil {
		return nil, err
	}
	parent := block.Height - 1
	header, err := c.Header(ctx, &parent)
	if err != nil {
		return nil, fmt.Errorf("parent header: %w", err)
	}
	if header == nil || header.Header == nil || header.Header.Height != parent || !bytes.Equal(header.Header.Hash(), block.LastBlockID.Hash) {
		return nil, fmt.Errorf("parent header hash mismatch")
	}
	return header.Header, nil
}

func (api *API) replayRequest(ctx context.Context, block *tmtypes.Block) (replay.Request, []byte, error) {
	var req replay.Request
	header, err := api.parentHeader(ctx, block)
	if err != nil {
		return req, nil, err
	}
	vals, err := api.parentValidators(ctx, block, header)
	if err != nil {
		return req, nil, err
	}
	set := &tmtypes.ValidatorSet{Validators: vals}
	req = replay.Request{ChainID: block.ChainID, ParentHash: block.AppHash, Request: &abci.RequestFinalizeBlock{
		Hash: block.Hash(), NextValidatorsHash: block.NextValidatorsHash, ProposerAddress: block.ProposerAddress,
		Height: block.Height, Time: block.Time, DecidedLastCommit: state.BuildLastCommitInfo(block, set, 1),
		Misbehavior: block.Evidence.Evidence.ToABCI(), Txs: block.Txs.ToSliceOfBytes(),
	}}
	return req, header.AppHash, nil
}

// validatorCache keeps the last verified validator set; sets rarely change
// between consecutive blocks.
type validatorCache struct {
	mu   sync.Mutex
	hash []byte
	vals []*tmtypes.Validator
}

// parentValidators returns the H-1 validator set whose hash the parent header commits to.
func (api *API) parentValidators(ctx context.Context, block *tmtypes.Block, header *tmtypes.Header) ([]*tmtypes.Validator, error) {
	api.validators.mu.Lock()
	cached, hash := api.validators.vals, api.validators.hash
	api.validators.mu.Unlock()
	if cached != nil && bytes.Equal(hash, header.ValidatorsHash) && len(cached) == block.LastCommit.Size() {
		return cached, nil
	}
	c, err := api.signClient()
	if err != nil {
		return nil, err
	}
	parent := block.Height - 1
	var vals []*tmtypes.Validator
	perPage := 100
	for page := 1; len(vals) < block.LastCommit.Size(); page++ {
		r, err := c.Validators(ctx, &parent, &page, &perPage)
		if err != nil {
			return nil, fmt.Errorf("validators at %d: %w", parent, err)
		}
		if r == nil || r.BlockHeight != parent || r.Total != block.LastCommit.Size() || len(r.Validators) == 0 || len(vals)+len(r.Validators) > r.Total {
			return nil, fmt.Errorf("incomplete validators at %d page %d", parent, page)
		}
		for _, v := range r.Validators {
			if v == nil || v.PubKey == nil {
				return nil, fmt.Errorf("missing validator at %d index %d", parent, len(vals))
			}
			vals = append(vals, v)
		}
	}
	set := &tmtypes.ValidatorSet{Validators: vals}
	if !bytes.Equal(set.Hash(), header.ValidatorsHash) {
		return nil, fmt.Errorf("parent validator hash mismatch")
	}
	api.validators.mu.Lock()
	api.validators.hash, api.validators.vals = header.ValidatorsHash, vals
	api.validators.mu.Unlock()
	return vals, nil
}

// canonicalObservations matches each observed message to its original Cosmos
// position, and checks the charged gas and status against the receipt path.
// A failed Cosmos transaction discards every message it executed.
func canonicalObservations(messages []committedEVMMessage, results *ctypes.ResultBlockResults, replayed *replay.Result) ([]replay.Transaction, error) {
	observed := make([]replay.Transaction, 0, len(messages))
	for _, obs := range replayed.Transactions {
		if obs.CosmosIndex < 0 || obs.CosmosIndex >= len(results.TxsResults) {
			return nil, fmt.Errorf("invalid observed Cosmos index %d", obs.CosmosIndex)
		}
		if results.TxsResults[obs.CosmosIndex].Code != 0 {
			continue
		}
		if len(observed) == len(messages) {
			return nil, fmt.Errorf("unexpected observation at tx[%d]/msg[%d]", obs.CosmosIndex, obs.MessageIndex)
		}
		msg := messages[len(observed)]
		if obs.CosmosIndex != msg.cosmosIndex || obs.MessageIndex != msg.messageIndex || obs.Hash != msg.tx.Hash() {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: mismatched observation for %s", msg.cosmosIndex, msg.messageIndex, msg.tx.Hash())
		}
		if obs.Error != "" || len(obs.Trace) == 0 {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: invalid observation: %s", msg.cosmosIndex, msg.messageIndex, obs.Error)
		}
		if obs.GasUsed != msg.receipt.GasUsed || obs.Failed == msg.receipt.Status {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: gas or status differs from receipt", msg.cosmosIndex, msg.messageIndex)
		}
		observed = append(observed, obs)
	}
	if len(observed) != len(messages) {
		msg := messages[len(observed)]
		return nil, fmt.Errorf("tx[%d]/msg[%d]: missing observation", msg.cosmosIndex, msg.messageIndex)
	}
	return observed, nil
}
