package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bytedance/sonic"
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

// The worker request crosses the process boundary as sonic JSON.
func TestRequestJSON(t *testing.T) {
	at := time.Unix(1700000000, 123).UTC()
	req := Request{ChainID: "cronosmainnet_25-1", ParentHash: bytes.Repeat([]byte{9}, 32), Request: &abci.RequestFinalizeBlock{
		Txs: [][]byte{{1, 2}}, Hash: bytes.Repeat([]byte{3}, 32), Height: 101485001, Time: at,
		ProposerAddress:    bytes.Repeat([]byte{4}, 20),
		NextValidatorsHash: bytes.Repeat([]byte{5}, 32),
		DecidedLastCommit:  abci.CommitInfo{Round: 1, Votes: []abci.VoteInfo{{Validator: abci.Validator{Address: bytes.Repeat([]byte{6}, 20), Power: 10}, BlockIdFlag: 2}}},
		Misbehavior:        []abci.Misbehavior{{Type: abci.MisbehaviorType_DUPLICATE_VOTE, Validator: abci.Validator{Power: 1}, Height: 3, Time: at, TotalVotingPower: 11}},
	}}
	data, err := sonic.ConfigStd.Marshal(req)
	require.NoError(t, err)
	std, err := json.Marshal(req)
	require.NoError(t, err)
	require.Equal(t, string(std), string(data))
	var got Request
	require.NoError(t, sonic.ConfigStd.Unmarshal(data, &got))
	require.Equal(t, req, got)
}

func TestReplayErrorFrame(t *testing.T) {
	var wire bytes.Buffer
	require.NoError(t, WriteError(&wire, errors.New("parent commit mismatch")))
	_, err := ReadResult(&wire)
	require.EqualError(t, err, "parent commit mismatch")
}
