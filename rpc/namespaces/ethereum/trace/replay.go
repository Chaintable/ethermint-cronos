package trace

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/rpc/client"
	"github.com/cometbft/cometbft/state"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/evmos/ethermint/debank/replay"
)

func (api *API) replayRequest(ctx context.Context, block *tmtypes.Block) (replay.Request, []byte, error) {
	var req replay.Request
	c, ok := api.clientCtx.Client.(client.SignClient)
	if !ok {
		return req, nil, fmt.Errorf("client lacks historical consensus queries")
	}
	if err := block.ValidateBasic(); err != nil {
		return req, nil, err
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
	req = replay.Request{ChainID: block.ChainID, ParentHash: block.AppHash, Request: &abci.RequestFinalizeBlock{
		Hash: block.Hash(), NextValidatorsHash: block.NextValidatorsHash, ProposerAddress: block.ProposerAddress,
		Height: block.Height, Time: block.Time, DecidedLastCommit: state.BuildLastCommitInfo(block, set, 1),
		Misbehavior: block.Evidence.Evidence.ToABCI(), Txs: block.Txs.ToSliceOfBytes(),
	}}
	return req, header.Header.AppHash, nil
}

// canonicalObservations matches each observed message to its original Cosmos
// position, and checks the charged gas and status against the receipt path.
// A failed Cosmos transaction discards every message it executed.
func (api *API) canonicalObservations(block *ctypes.ResultBlock, results *ctypes.ResultBlockResults, replayed *replay.Result, receipts map[string]*consensusReceipt) ([]replay.Transaction, error) {
	var selected []replay.Transaction
	for _, obs := range replayed.Transactions {
		if obs.CosmosIndex < 0 || obs.CosmosIndex >= len(results.TxsResults) {
			return nil, fmt.Errorf("invalid observed Cosmos index %d", obs.CosmosIndex)
		}
		if results.TxsResults[obs.CosmosIndex].Code == 0 {
			selected = append(selected, obs)
		}
	}
	var included []replay.Transaction
	i := 0
	for txIndex, raw := range block.Block.Txs {
		if results.TxsResults[txIndex].Code != 0 {
			continue
		}
		tx, err := api.clientCtx.TxConfig.TxDecoder()(raw)
		if err != nil {
			return nil, fmt.Errorf("tx[%d]: %w", txIndex, err)
		}
		for msgIndex, m := range tx.GetMsgs() {
			eth, ok := m.(*evmtypes.MsgEthereumTx)
			if !ok {
				continue
			}
			if i >= len(selected) || selected[i].CosmosIndex != txIndex || selected[i].MessageIndex != msgIndex {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: missing observation", txIndex, msgIndex)
			}
			obs := selected[i]
			i++
			hash := eth.AsTransaction().Hash()
			if obs.Hash != hash || obs.Error != "" || len(obs.Trace) == 0 {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: invalid observation for %s: %s", txIndex, msgIndex, hash, obs.Error)
			}
			rc := receipts[strings.ToLower(hash.Hex())]
			if rc == nil || obs.GasUsed != rc.GasUsed || obs.Failed == rc.Status {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: receipt mismatch for %s", txIndex, msgIndex, hash)
			}
			included = append(included, obs)
		}
	}
	if i != len(selected) {
		return nil, fmt.Errorf("unexpected observation at EVM index %d", i)
	}
	return included, nil
}
