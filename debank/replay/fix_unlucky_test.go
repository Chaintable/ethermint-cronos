package replay

import (
	"testing"

	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
)

func event(typ string, kv ...string) abci.Event {
	e := abci.Event{Type: typ}
	for i := 0; i < len(kv); i += 2 {
		e.Attributes = append(e.Attributes, abci.EventAttribute{Key: []byte(kv[i]), Value: []byte(kv[i+1])})
	}
	return e
}

// Mainnet H2694973 tx 26 exceeded the block gas limit; cronosd fix-unlucky-tx
// appended an ethereum_tx event to the archived row and changed nothing else.
func exceededRow() *abci.ResponseDeliverTx {
	return &abci.ResponseDeliverTx{
		Code: 11, Codespace: "sdk", GasUsed: 236538,
		Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 236538: out of gas",
		Events: []abci.Event{
			event("coin_spent", "spender", "crc1srqvusdr7kh36er2lmxrue8lqfu8u7mg6ak0d9", "amount", "4001561218712800000basecro"),
			event("tx", "fee", "4001561218712800000basecro"),
		},
	}
}

func block(rows ...*abci.ResponseDeliverTx) *tmstate.ABCIResponses {
	return &tmstate.ABCIResponses{DeliverTxs: rows, BeginBlock: &abci.ResponseBeginBlock{}, EndBlock: &abci.ResponseEndBlock{}}
}

func TestFixUnluckyTxEvents(t *testing.T) {
	native := exceededRow()
	require.True(t, ExceedsBlockGas(native))

	archived := exceededRow()
	archived.Events = append(archived.Events, event("ethereum_tx",
		"ethereumTxHash", "0xe324619b1b490abe704069918485202489174b80d84f8eb201f2b912b891876d", "txIndex", "26"))
	require.NoError(t, Verify(block(archived), block(native)))

	// Any other difference still fails.
	other := exceededRow()
	other.GasUsed++
	require.ErrorContains(t, Verify(block(archived), block(other)), "tx[0] gas")

	extra := exceededRow()
	extra.Events = append(extra.Events, event("message", "sender", "x"))
	require.ErrorContains(t, Verify(block(extra), block(native)), "tx[0]")

	// Only a row that exceeded the block gas limit is excused.
	failed := exceededRow()
	failed.Log = "out of gas in location: WriteFlat; gasWanted: 0"
	patched := *failed
	patched.Events = archived.Events
	require.ErrorContains(t, Verify(block(&patched), block(failed)), "tx[0]")
}
