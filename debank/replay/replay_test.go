package replay

import (
	"bytes"
	"errors"
	"testing"
	"time"

	_ "github.com/cosmos/cosmos-sdk/x/authz"
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
}

func TestVerifyTypedEventAttributeOrder(t *testing.T) {
	attr := func(k, v string) abci.EventAttribute { return abci.EventAttribute{Key: []byte(k), Value: []byte(v)} }
	event := func(typ string, attrs ...abci.EventAttribute) []abci.Event {
		return []abci.Event{{Type: typ, Attributes: attrs}}
	}
	a, b, c := attr("granter", `"x"`), attr("grantee", `"y"`), attr("msg_type_url", `"z"`)
	grant := "cosmos.authz.v1beta1.EventGrant"

	want, got := responses(), responses()
	got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
	want.DeliverTxs[0].Events = event(grant, a, b, c)
	got.DeliverTxs[0].Events = event(grant, c, a, b)
	require.NoError(t, Verify(want, got), "typed event attribute order is not compared")

	got.DeliverTxs[0].Events = event(grant, c, a, attr("grantee", `"w"`))
	require.ErrorContains(t, Verify(want, got), "tx[0]: event[0]")

	want.DeliverTxs[0].Events = event("message", a, b)
	got.DeliverTxs[0].Events = event("message", b, a)
	require.ErrorContains(t, Verify(want, got), "tx[0]: event[0]", "legacy event attribute order is compared")
}
