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

// canonicalObservations matches each observed message to its original Cosmos
// position, and checks the charged gas and status against the receipt path.
func (api *API) canonicalObservations(block *ctypes.ResultBlock, results *ctypes.ResultBlockResults, replayed *replay.Result, receipts map[string]*consensusReceipt) ([]replay.Transaction, error) {
	var selected []replay.Transaction
	for _, obs := range replayed.Transactions {
		if obs.CosmosIndex < 0 || obs.CosmosIndex >= len(results.TxsResults) {
			return nil, fmt.Errorf("invalid observed Cosmos index %d", obs.CosmosIndex)
		}
		if rpctypes.TxSuccessOrExceedsBlockGasLimit(results.TxsResults[obs.CosmosIndex]) {
			selected = append(selected, obs)
		}
	}
	i := 0
	for txIndex, raw := range block.Block.Txs {
		result := replayed.Finalize.TxResults[txIndex]
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
			if i >= len(selected) || selected[i].CosmosIndex != txIndex || selected[i].MessageIndex != msgIndex || selected[i].Hash != hash {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: missing or incorrect observation for %s", txIndex, msgIndex, hash)
			}
			rc := receipts[strings.ToLower(hash.Hex())]
			if selected[i].Error != "" {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: %s", txIndex, msgIndex, selected[i].Error)
			}
			if rc == nil || selected[i].GasUsed != rc.GasUsed || selected[i].Failed == rc.Status {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: charged gas or status mismatch", txIndex, msgIndex)
			}
			i++
		}
	}
	if i != len(selected) {
		return nil, fmt.Errorf("unexpected observation at EVM index %d", i)
	}
	return selected, nil
}
