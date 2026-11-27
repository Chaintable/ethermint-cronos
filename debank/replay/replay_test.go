package replay

import (
	"bytes"
	"errors"
	"testing"
	"time"

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

func TestVerifyLostArchivedBeginBlock(t *testing.T) {
	want, got := responses(), responses()
	got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
	want.BeginBlock.Events = nil
	require.NoError(t, Verify(want, got))
	want.BeginBlock.Events = []abci.Event{{Type: "mint"}}
	require.ErrorContains(t, Verify(want, got), "begin block")
}

func TestVerifyRepeatedConversionEvents(t *testing.T) {
	ev := func(typ, value string) abci.Event {
		return abci.Event{Type: typ, Attributes: []abci.EventAttribute{{Key: []byte("amount"), Value: []byte(value)}}}
	}
	conversion := []abci.Event{ev("coin_spent", "1ibc/x"), ev("coin_received", "1ibc/x"), ev("coinbase", "10basecro")}
	tx := func(runs ...[]abci.Event) []abci.Event {
		events := []abci.Event{ev("recv_packet", "p"), ev("fungible_token_packet", "1")}
		for _, r := range runs {
			events = append(events, r...)
		}
		return append(events, ev("write_acknowledgement", "ack"))
	}
	verify := func(archived []abci.Event) error {
		want, got := responses(), responses()
		got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
		want.DeliverTxs[0].Events, got.DeliverTxs[0].Events = archived, tx(conversion)
		return Verify(want, got)
	}
	require.NoError(t, verify(tx(conversion, conversion)))

	altered := append([]abci.Event{}, conversion...)
	altered[2] = ev("coinbase", "20basecro")
	require.ErrorContains(t, verify(tx(conversion, altered)), "event count")
	require.ErrorContains(t, verify(tx(conversion, conversion, conversion)), "event count")
	noAck := append(tx(conversion, conversion)[:8:8], ev("message", "m"))
	require.ErrorContains(t, verify(noAck), "event count")
}

// A refund on an error acknowledgement (H10439843) or a timeout has no
// write_acknowledgement; the run ends at the next message or the last event.
func TestVerifyRepeatedRefundConversionEvents(t *testing.T) {
	ev := func(typ, key, value string) abci.Event {
		return abci.Event{Type: typ, Attributes: []abci.EventAttribute{{Key: []byte(key), Value: []byte(value)}}}
	}
	refund := []abci.Event{ev("coin_spent", "amount", "1ibc/x"), ev("coin_received", "amount", "1ibc/x"), ev("coinbase", "amount", "10basecro")}
	next := ev("message", "action", "/cosmos.bank.v1beta1.MsgSend")
	tx := func(tail []abci.Event, runs ...[]abci.Event) []abci.Event {
		events := []abci.Event{ev("acknowledge_packet", "packet_sequence", "1"), ev("fungible_token_packet", "acknowledgement", "error"), ev("fungible_token_packet", "error", "e")}
		for _, r := range runs {
			events = append(events, r...)
		}
		return append(events, tail...)
	}
	verify := func(archived, replayed []abci.Event) error {
		want, got := responses(), responses()
		got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
		want.DeliverTxs[0].Events, got.DeliverTxs[0].Events = archived, replayed
		return Verify(want, got)
	}
	require.NoError(t, verify(tx(nil, refund, refund), tx(nil, refund)))
	require.NoError(t, verify(tx([]abci.Event{next}, refund, refund), tx([]abci.Event{next}, refund)))

	altered := append([]abci.Event{}, refund...)
	altered[2] = ev("coinbase", "amount", "20basecro")
	require.ErrorContains(t, verify(tx(nil, refund, altered), tx(nil, refund)), "event count")
	require.ErrorContains(t, verify(tx(nil, refund, refund), tx(nil, refund, refund, refund)), "event count")
}

// H12736432: MsgTimeout refunds archived each conversion twice before timeout_packet.
func TestVerifyRepeatedTimeoutConversionEvents(t *testing.T) {
	ev := func(typ, key, value string) abci.Event {
		return abci.Event{Type: typ, Attributes: []abci.EventAttribute{{Key: []byte(key), Value: []byte(value)}}}
	}
	conv := []abci.Event{ev("coin_spent", "amount", "1ibc/x"), ev("coin_received", "amount", "1ibc/x"), ev("transfer", "amount", "1ibc/x"), ev("message", "sender", "a")}
	tx := func(runs ...[]abci.Event) []abci.Event {
		events := []abci.Event{ev("message", "action", "/ibc.core.channel.v1.MsgTimeout"), ev("timeout", "module", "transfer")}
		for _, r := range runs {
			events = append(events, r...)
		}
		return append(events, ev("timeout_packet", "packet_sequence", "1"), ev("message", "module", "ibc_channel"))
	}
	verify := func(archived, replayed []abci.Event) error {
		want, got := responses(), responses()
		got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
		want.DeliverTxs[0].Events, got.DeliverTxs[0].Events = archived, replayed
		return Verify(want, got)
	}
	require.NoError(t, verify(tx(conv, conv), tx(conv)))
	altered := append([]abci.Event{}, conv...)
	altered[0] = ev("coin_spent", "amount", "2ibc/x")
	require.ErrorContains(t, verify(tx(conv, altered), tx(conv)), "event count")
}

func TestVerifyWithdrawRewardsWithoutDelegator(t *testing.T) {
	withdraw := func(kv ...string) abci.Event {
		e := abci.Event{Type: "withdraw_rewards"}
		for i := 0; i < len(kv); i += 2 {
			e.Attributes = append(e.Attributes, abci.EventAttribute{Key: []byte(kv[i]), Value: []byte(kv[i+1])})
		}
		return e
	}
	replayed := withdraw("amount", "0stake", "validator", "v", "delegator", "d", "authz_msg_index", "0")
	verify := func(archived, got abci.Event) error {
		want, res := responses(), responses()
		res.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
		want.DeliverTxs[0].Events, res.DeliverTxs[0].Events = []abci.Event{archived}, []abci.Event{got}
		return Verify(want, res)
	}
	require.NoError(t, verify(withdraw("amount", "0stake", "validator", "v", "authz_msg_index", "0"), replayed))

	require.Error(t, verify(withdraw("amount", "1stake", "validator", "v", "authz_msg_index", "0"), replayed))
	require.Error(t, verify(withdraw("amount", "0stake", "validator", "v"), replayed))
	require.Error(t, verify(withdraw("amount", "0stake", "validator", "v", "delegator", "x", "authz_msg_index", "0"), replayed))
	other, archived := replayed, withdraw("amount", "0stake", "validator", "v", "authz_msg_index", "0")
	other.Type, archived.Type = "withdraw_commission", "withdraw_commission"
	require.Error(t, verify(archived, other))
}

func TestVerifyFailedAckEvents(t *testing.T) {
	ev := func(typ, key, value string) abci.Event {
		return abci.Event{Type: typ, Attributes: []abci.EventAttribute{{Key: []byte(key), Value: []byte(value)}}}
	}
	failed := ev("fungible_token_packet", "success", "false")
	tx := func(ack string, callback ...abci.Event) []abci.Event {
		events := []abci.Event{ev("recv_packet", "packet_sequence", "1"), ev("message", "module", "ibc_channel")}
		events = append(events, callback...)
		return append(events, ev("write_acknowledgement", "packet_ack", ack), ev("message", "module", "ibc_channel"))
	}
	verify := func(archived, replayed []abci.Event) error {
		want, got := responses(), responses()
		got.EndBlock.ValidatorUpdates = want.EndBlock.ValidatorUpdates
		want.DeliverTxs[0].Events, got.DeliverTxs[0].Events = archived, replayed
		return Verify(want, got)
	}
	errAck := `{"error":"ABCI code: 1: error handling packet"}`
	require.NoError(t, verify(tx(errAck, failed), tx(errAck)))
	require.NoError(t, verify(tx(errAck, ev("coin_received", "amount", "1"), failed), tx(errAck)))

	require.ErrorContains(t, verify(tx(`{"result":"AQ=="}`, failed), tx(`{"result":"AQ=="}`)), "event count")
	require.ErrorContains(t, verify(tx(errAck, failed), tx(errAck, ev("fungible_token_packet", "success", "true"))), "event")
	require.ErrorContains(t, verify(tx(errAck), tx(errAck, failed)), "event count")
}
