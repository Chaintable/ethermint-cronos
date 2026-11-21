package trace

import (
	"context"
	"fmt"
	"strings"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"

	"github.com/cometbft/cometbft/rpc/client"

	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/evmos/ethermint/debank/replay"
)

func (api *API) replayRequest(ctx context.Context, block *tmtypes.Block) (replay.Request, []byte, error) {
	var req replay.Request
	c, ok := api.clientCtx.Client.(client.SignClient)
	if !ok {
		return req, nil, fmt.Errorf("client lacks historical consensus queries")
	}
	return replay.ArchiveRequest(ctx, c, block)
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
