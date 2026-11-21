package trace

import (
	"encoding/json"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	eth "github.com/ethereum/go-ethereum/core/types"
	"github.com/evmos/ethermint/debank/replay"
	wire "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evm "github.com/evmos/ethermint/x/evm/types"
	"github.com/stretchr/testify/require"
)

type fixedGasTxConfig struct {
	client.TxConfig
	tx sdk.Tx
}

func (c fixedGasTxConfig) TxDecoder() sdk.TxDecoder {
	return func([]byte) (sdk.Tx, error) { return c.tx, nil }
}

func TestBlockGasFailureTrace(t *testing.T) {
	tx := eth.NewTx(&eth.LegacyTx{Gas: 54137})
	var msg evm.MsgEthereumTx
	msg.FromEthereumTx(tx)
	api := &API{clientCtx: client.Context{TxConfig: fixedGasTxConfig{tx: &msg}}}
	block := &ctypes.ResultBlock{Block: &tm.Block{Data: tm.Data{Txs: tm.Txs{{1}}}}}
	result := &abci.ExecTxResult{Code: 11, Log: rpctypes.ExceedBlockGasLimitError, GasUsed: 950977623}
	results := &ctypes.ResultBlockResults{TxsResults: []*abci.ExecTxResult{result}}
	r := &replay.Result{Finalize: &abci.ResponseFinalizeBlock{TxResults: results.TxsResults}}
	for _, captured := range []bool{false, true} {
		receipts, err := loadConsensus(block, results, api.clientCtx.TxConfig.TxDecoder())
		require.NoError(t, err)
		r.Transactions = nil
		if captured {
			raw, err := json.Marshal(wire.TraceResult{
				Traces: []wire.Trace{{TxID: tx.Hash().Hex()}}, Events: []wire.Event{{ID: "attempt"}},
			})
			require.NoError(t, err)
			r.Transactions = []replay.Transaction{{Hash: tx.Hash(), Trace: raw, GasUsed: 21000}}
		}
		included, err := api.canonicalObservations(block, results, r, receipts)
		require.NoError(t, err)
		require.Len(t, included, 1)
		rc := receipts[tx.Hash().Hex()]
		require.False(t, rc.Status)
		require.Equal(t, tx.Gas(), rc.GasUsed)
		tr, err := included[0].receiptTrace(rc)
		require.NoError(t, err)
		require.Empty(t, tr.Events)
		require.Empty(t, tr.Traces)
		if captured {
			require.Len(t, tr.ErrorTraces, 1)
			require.Equal(t, result.Log, tr.ErrorTraces[0].Error)
			require.Len(t, tr.ErrorEvents, 1)
		} else {
			require.Empty(t, tr.ErrorTraces)
			require.Empty(t, tr.ErrorEvents)
		}
	}
	result.Log = "no block gas left to run tx: out of gas"
	r.Transactions = nil
	included, err := api.canonicalObservations(block, results, r, nil)
	require.NoError(t, err)
	require.Empty(t, included)
}
