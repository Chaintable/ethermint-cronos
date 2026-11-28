package replay

import (
	"bytes"
	"errors"
	tm "github.com/cometbft/cometbft/types"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cryptoenc "github.com/cometbft/cometbft/crypto/encoding"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"
)

func responses() *tmstate.ABCIResponses {
	pk, err := cryptoenc.PubKeyToProto(ed25519.GenPrivKey().PubKey())
	if err != nil {
		panic(err)
	}
	ev := func() []abci.Event {
		return []abci.Event{{Type: "transfer", Attributes: []abci.EventAttribute{{Key: "amount", Value: "1basecro", Index: true}}}}
	}
	return &tmstate.ABCIResponses{
		BeginBlock: &abci.ResponseBeginBlock{Events: ev()},
		DeliverTxs: []*abci.ResponseDeliverTx{{Code: 0, Data: []byte{1}, GasWanted: 10, GasUsed: 9, Events: ev()}},
		EndBlock:   &abci.ResponseEndBlock{ValidatorUpdates: []abci.ValidatorUpdate{{PubKey: pk, Power: 7}}},
	}
}

func TestWireRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := Request{ChainID: "cronosmainnet_25-1", ParentHash: []byte{9}, BeginBlock: &abci.RequestBeginBlock{
		Hash:   []byte{1},
		Header: tmproto.Header{Height: 6346, Time: time.Unix(1636000000, 5).UTC()},
	}, Txs: [][]byte{{2}}}
	require.NoError(t, WriteRequest(&buf, req))
	got, err := ReadRequest(&buf)
	require.NoError(t, err)
	require.Equal(t, req, got)

	res := Result{Responses: responses(), AppHash: []byte{3}, Transactions: []Transaction{{CosmosIndex: 1, Trace: []byte(`{"a":1}`)}}}
	require.NoError(t, WriteResult(&buf, res))
	back, err := ReadResult(&buf)
	require.NoError(t, err)
	require.NoError(t, Verify(res.Responses, back.Responses))
	require.Equal(t, res.AppHash, back.AppHash)
	require.JSONEq(t, `{"a":1}`, string(back.Transactions[0].Trace))

	require.NoError(t, WriteError(&buf, errors.New("boom")))
	_, err = ReadResult(&buf)
	require.EqualError(t, err, "boom")
}

func TestVerify(t *testing.T) {
	want := responses()
	got := responses()
	got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
	got.BeginBlock.Events[0].Attributes[0].Index = false
	require.NoError(t, Verify(want, got), "Index is not compared")

	got.DeliverTxs[0].Data = []byte{2}
	require.ErrorContains(t, Verify(want, got), "tx[0] result data")
	got = responses()
	got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
	got.DeliverTxs[0].Events[0].Attributes[0].Value = "2basecro"
	require.ErrorContains(t, Verify(want, got), "tx[0]: event[0]")
	got = responses()
	require.ErrorContains(t, Verify(want, got), "validator updates")

	// The block gas failure decides whether a transaction is listed, though
	// Log is not otherwise compared.
	exceeded := "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 9: out of gas"
	want.DeliverTxs[0].Code, want.DeliverTxs[0].Codespace, want.DeliverTxs[0].Log = 11, "sdk", "out of gas"
	got = responses()
	got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
	got.DeliverTxs[0].Code, got.DeliverTxs[0].Codespace, got.DeliverTxs[0].Data, got.DeliverTxs[0].Log = 11, "sdk", want.DeliverTxs[0].Data, exceeded
	require.ErrorContains(t, Verify(want, got), "tx[0] outcome")
	want.DeliverTxs[0].Log = exceeded + " (archived)"
	require.NoError(t, Verify(want, got))
}

func TestVerifyUncommittedFields(t *testing.T) {
	tests := map[string]func(*tmstate.ABCIResponses){
		"attribute order":         func(r *tmstate.ABCIResponses) { a := r.BeginBlock.Events[0].Attributes; a[0], a[1] = a[1], a[0] },
		"duplicate event":         func(r *tmstate.ABCIResponses) { r.EndBlock.Events = append(r.EndBlock.Events, r.BeginBlock.Events[0]) },
		"codespace":               func(r *tmstate.ABCIResponses) { r.DeliverTxs[0].Codespace = "bank" },
		"validator power":         func(r *tmstate.ABCIResponses) { r.EndBlock.ValidatorUpdates[0].Power++ },
		"consensus max bytes":     func(r *tmstate.ABCIResponses) { r.EndBlock.ConsensusParamUpdates.Block.MaxBytes++ },
		"absent consensus update": func(r *tmstate.ABCIResponses) { r.EndBlock.ConsensusParamUpdates = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			want := responses()
			want.BeginBlock.Events[0].Attributes = append(want.BeginBlock.Events[0].Attributes, abci.EventAttribute{Key: "recipient", Value: "alice"})
			want.EndBlock.ConsensusParamUpdates = &tmproto.ConsensusParams{Block: &tmproto.BlockParams{MaxGas: 60000000, MaxBytes: 1000}}
			raw, err := want.Marshal()
			require.NoError(t, err)
			var got tmstate.ABCIResponses
			require.NoError(t, got.Unmarshal(raw))
			mutate(&got)
			require.Equal(t, tm.NewResults(want.DeliverTxs).Hash(), tm.NewResults(got.DeliverTxs).Hash())
			require.Error(t, Verify(want, &got))
		})
	}
}
