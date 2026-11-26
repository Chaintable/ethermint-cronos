package bankdiff

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
)

func coinEvent(typ, key string, addr sdk.AccAddress, amount string) abci.Event {
	return abci.Event{Type: typ, Attributes: []abci.EventAttribute{
		{Key: []byte(key), Value: []byte(addr.String())},
		{Key: []byte("amount"), Value: []byte(amount)},
	}}
}

func TestCoinTouchedAddresses(t *testing.T) {
	a := sdk.AccAddress(common.HexToAddress("0x01").Bytes())
	b := sdk.AccAddress(common.HexToAddress("0x02").Bytes())
	c := sdk.AccAddress(common.HexToAddress("0x03").Bytes())
	d := sdk.AccAddress(common.HexToAddress("0x04").Bytes())
	long := sdk.AccAddress(make([]byte, 32))
	res := &tmstate.ABCIResponses{
		BeginBlock: &abci.ResponseBeginBlock{Events: []abci.Event{coinEvent("coin_received", "receiver", a, "5basecro")}},
		DeliverTxs: []*abci.ResponseDeliverTx{{Events: []abci.Event{
			coinEvent("coin_spent", "spender", b, "1basecro,2ibc/x"),
			coinEvent("coin_spent", "spender", c, "2ibc/x"),
			coinEvent("coin_received", "receiver", long, "1basecro"),
		}}},
		EndBlock: &abci.ResponseEndBlock{Events: []abci.Event{coinEvent("coin_received", "receiver", d, "3basecro")}},
	}
	got := CoinTouchedAddresses(res, "basecro")
	require.Equal(t, map[common.Address]struct{}{
		common.HexToAddress("0x01"): {}, common.HexToAddress("0x02"): {}, common.HexToAddress("0x04"): {},
	}, got)
}
