package replay

import (
	"bytes"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cryptoproto "github.com/cometbft/cometbft/proto/tendermint/crypto"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/stretchr/testify/require"
)

func TestReplayResultAndConsensus(t *testing.T) {
	event := abci.Event{Type: "transfer", Attributes: []abci.EventAttribute{{Key: "sender", Value: "a", Index: true}}}
	response := &abci.ResponseFinalizeBlock{
		AppHash: bytes.Repeat([]byte{1}, 32), Events: []abci.Event{event},
		TxResults:        []*abci.ExecTxResult{{GasUsed: 4, Data: []byte{3}}},
		ValidatorUpdates: []abci.ValidatorUpdate{{Power: 7, PubKey: cryptoproto.PublicKey{Sum: &cryptoproto.PublicKey_Ed25519{Ed25519: bytes.Repeat([]byte{2}, 32)}}}},
	}
	var wire bytes.Buffer
	require.NoError(t, WriteResult(&wire, Result{Finalize: response}))
	got, err := ReadResult(&wire)
	require.NoError(t, err)
	require.Equal(t, response, got.Finalize)
	canonical := &ctypes.ResultBlockResults{AppHash: response.AppHash, FinalizeBlockEvents: response.Events, TxsResults: response.TxResults, ValidatorUpdates: response.ValidatorUpdates}
	got.Finalize.Events[0].Attributes[0].Index = false
	require.NoError(t, Verify(canonical, got.Finalize))
	got.Finalize.TxResults[0].GasUsed++
	require.ErrorContains(t, Verify(canonical, got.Finalize), "tx[0] gas")
	got.Finalize.TxResults[0].GasUsed--
	got.Finalize.Events[0].Attributes[0].Value = "other"
	require.ErrorContains(t, Verify(canonical, got.Finalize), "event[0]")
	got.Finalize.Events[0].Attributes[0].Value = "a"
	got.Finalize.AppHash[0]++
	require.ErrorContains(t, Verify(canonical, got.Finalize), "AppHash")
}
