package bankdiff

import (
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
)

func addr20(b byte) sdk.AccAddress {
	x := make([]byte, common.AddressLength)
	x[common.AddressLength-1] = b
	return sdk.AccAddress(x)
}

func coinEvent(typ, key, val, amount string) abci.Event {
	return abci.Event{
		Type: typ,
		Attributes: []abci.EventAttribute{
			{Key: key, Value: val},
			{Key: "amount", Value: amount},
		},
	}
}

func TestCoinTouchedAddresses(t *testing.T) {
	const denom = "basecro"
	spent := addr20(0x11)   // coin_spent in finalize events
	recv := addr20(0x22)    // coin_received in a tx
	ibcOnly := addr20(0x33) // moves only a non-CRO denom -> excluded

	blockRes := &coretypes.ResultBlockResults{
		FinalizeBlockEvents: []abci.Event{
			coinEvent("coin_spent", "spender", spent.String(), "1000"+denom),
			// a non-bank event must be ignored
			{Type: "message", Attributes: []abci.EventAttribute{{Key: "action", Value: "x"}}},
		},
		TxsResults: []*abci.ExecTxResult{
			{Events: []abci.Event{
				coinEvent("coin_received", "receiver", recv.String(), "5"+denom),
				coinEvent("coin_spent", "spender", ibcOnly.String(), "7ibc/ABCDEF0123"),
			}},
		},
	}

	got := CoinTouchedAddresses(blockRes, denom)

	wantSpent := common.BytesToAddress(spent.Bytes())
	wantRecv := common.BytesToAddress(recv.Bytes())
	wantExcluded := common.BytesToAddress(ibcOnly.Bytes())

	if _, ok := got[wantSpent]; !ok {
		t.Errorf("missing coin_spent address %s", wantSpent)
	}
	if _, ok := got[wantRecv]; !ok {
		t.Errorf("missing coin_received address %s", wantRecv)
	}
	if _, ok := got[wantExcluded]; ok {
		t.Errorf("address moving only non-CRO denom must be excluded: %s", wantExcluded)
	}
	if len(got) != 2 {
		t.Errorf("want 2 touched addresses, got %d: %v", len(got), got)
	}
}
