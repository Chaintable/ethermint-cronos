package trace

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
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

// An included message may have exhausted block gas before entering the EVM.
type includedMessage struct {
	replay.Transaction
	blockGasError string
}

// canonicalObservations matches each observed message to its original Cosmos
// position, and checks the charged gas and status against the receipt path.
func (api *API) canonicalObservations(block *ctypes.ResultBlock, results *ctypes.ResultBlockResults, replayed *replay.Result, receipts map[string]*consensusReceipt) ([]includedMessage, error) {
	var selected []replay.Transaction
	for _, obs := range replayed.Transactions {
		if obs.CosmosIndex < 0 || obs.CosmosIndex >= len(results.TxsResults) {
			return nil, fmt.Errorf("invalid observed Cosmos index %d", obs.CosmosIndex)
		}
		if rpctypes.TxSuccessOrExceedsBlockGasLimit(results.TxsResults[obs.CosmosIndex]) {
			selected = append(selected, obs)
		}
	}
	var included []includedMessage
	i := 0
	for txIndex, raw := range block.Block.Txs {
		result := replayed.Finalize.TxResults[txIndex]
		archived := results.TxsResults[txIndex]
		if rpctypes.TxSuccessOrExceedsBlockGasLimit(archived) != rpctypes.TxSuccessOrExceedsBlockGasLimit(result) {
			return nil, fmt.Errorf("tx[%d]: replay and archive disagree on EVM inclusion", txIndex)
		}
		if !rpctypes.TxSuccessOrExceedsBlockGasLimit(result) {
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
			hash := eth.AsTransaction().Hash()
			key := strings.ToLower(hash.Hex())
			entry := includedMessage{Transaction: replay.Transaction{CosmosIndex: txIndex, MessageIndex: msgIndex, Hash: hash}}
			matched := i < len(selected) && selected[i].CosmosIndex == txIndex && selected[i].MessageIndex == msgIndex
			if matched {
				entry.Transaction = selected[i]
				i++
				if entry.Hash != hash || entry.Error != "" || len(entry.Trace) == 0 {
					return nil, fmt.Errorf("tx[%d]/msg[%d]: invalid observation for %s: %s", txIndex, msgIndex, hash, entry.Error)
				}
			}
			rc := receipts[key]
			if result.Code != 0 {
				entry.blockGasError = archived.Log
				if rc == nil || rc.Status || rc.GasUsed != eth.GetGas() || len(rc.Logs) != 0 {
					return nil, fmt.Errorf("tx[%d]/msg[%d]: invalid block gas failure receipt", txIndex, msgIndex)
				}
			} else if !matched || rc == nil || entry.GasUsed != rc.GasUsed || entry.Failed == rc.Status {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: missing observation or receipt mismatch", txIndex, msgIndex)
			}
			included = append(included, entry)
		}
	}
	if i != len(selected) {
		return nil, fmt.Errorf("unexpected observation at EVM index %d", i)
	}
	return included, nil
}
