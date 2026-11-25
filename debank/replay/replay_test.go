package replay

import (
	"bytes"
	"errors"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
	cryptoenc "github.com/tendermint/tendermint/crypto/encoding"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
)

func responses() *tmstate.ABCIResponses {
	pk, err := cryptoenc.PubKeyToProto(ed25519.GenPrivKey().PubKey())
	if err != nil {
		panic(err)
	}
	ev := func() []abci.Event {
		return []abci.Event{{Type: "transfer", Attributes: []abci.EventAttribute{{Key: []byte("amount"), Value: []byte("1basecro"), Index: true}}}}
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
	got.DeliverTxs[0].Events[0].Attributes[0].Value = []byte("2basecro")
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

type signedFee struct {
	sdk.FeeTx
	payer, granter sdk.AccAddress
}

func (t signedFee) GetFee() sdk.Coins          { return sdk.NewCoins(sdk.NewInt64Coin("basecro", 123)) }
func (t signedFee) FeePayer() sdk.AccAddress   { return t.payer }
func (t signedFee) FeeGranter() sdk.AccAddress { return t.granter }

func TestHistoricalFeeEvent(t *testing.T) {
	payer, granter := sdk.AccAddress(bytes.Repeat([]byte{1}, 20)), sdk.AccAddress(bytes.Repeat([]byte{2}, 20))
	event := func(kv ...string) abci.Event {
		ev := abci.Event{Type: "tx"}
		for i := 0; i < len(kv); i += 2 {
			ev.Attributes = append(ev.Attributes, abci.EventAttribute{Key: []byte(kv[i]), Value: []byte(kv[i+1])})
		}
		return ev
	}
	legacy := event("fee", "123basecro")
	full := event("fee", "123basecro", "fee_payer", granter.String())
	for _, tc := range []struct {
		name      string
		want, got abci.Event
		grant     sdk.AccAddress
		valid     bool
	}{
		{"fee grant", legacy, full, granter, true},
		{"ordinary payer", legacy, event("fee", "123basecro", "fee_payer", payer.String()), nil, true},
		{"wrong payer", legacy, full, nil, false},
		{"wrong amount", event("fee", "124basecro"), full, granter, false},
		{"wrong signed fee", event("fee", "124basecro"), event("fee", "124basecro", "fee_payer", granter.String()), granter, false},
		{"unknown extra", legacy, event("fee", "123basecro", "fee_payer", granter.String(), "unknown", "x"), granter, false},
		{"modern disagreement", event("fee", "123basecro", "fee_payer", payer.String()), full, granter, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := responses(), responses()
			b.EndBlock = a.EndBlock
			a.DeliverTxs[0].Events = []abci.Event{tc.want}
			b.DeliverTxs[0].Events = []abci.Event{tc.got}
			decode := func([]byte) (sdk.Tx, error) { return signedFee{payer: payer, granter: tc.grant}, nil }
			err := VerifyWithTxs(a, b, [][]byte{{1}}, decode)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, tc.got, b.DeliverTxs[0].Events[0])
		})
	}
}
