package bankdiff

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
)

// CoinTouchedAddresses returns the EVM addresses whose native balance moved in
// the block, from coin_spent / coin_received events of the EVM denom. Native
// movements (fees, bank transfers, IBC) never reach the EVM tracer. Addresses
// that are not 20 bytes have no EVM form and are skipped.
func CoinTouchedAddresses(results *tmstate.ABCIResponses, evmDenom string) map[common.Address]struct{} {
	out := make(map[common.Address]struct{})
	if results.BeginBlock != nil {
		scanEvents(out, results.BeginBlock.Events, evmDenom)
	}
	for _, txr := range results.DeliverTxs {
		scanEvents(out, txr.Events, evmDenom)
	}
	if results.EndBlock != nil {
		scanEvents(out, results.EndBlock.Events, evmDenom)
	}
	return out
}

func scanEvents(out map[common.Address]struct{}, events []abci.Event, evmDenom string) {
	for _, ev := range events {
		var addrKey string
		switch ev.Type {
		case banktypes.EventTypeCoinSpent:
			addrKey = banktypes.AttributeKeySpender
		case banktypes.EventTypeCoinReceived:
			addrKey = banktypes.AttributeKeyReceiver
		default:
			continue
		}
		var bech32, amount string
		for _, attr := range ev.Attributes {
			switch string(attr.Key) {
			case addrKey:
				bech32 = string(attr.Value)
			case sdk.AttributeKeyAmount:
				amount = string(attr.Value)
			}
		}
		if bech32 == "" || !movesDenom(amount, evmDenom) {
			continue
		}
		accAddr, err := sdk.AccAddressFromBech32(bech32)
		if err != nil || len(accAddr.Bytes()) != common.AddressLength {
			continue
		}
		out[common.BytesToAddress(accAddr.Bytes())] = struct{}{}
	}
}

// movesDenom reports whether the coins string contains a positive amount of denom.
func movesDenom(amount, denom string) bool {
	coins, err := sdk.ParseCoinsNormalized(amount)
	if err != nil {
		return false
	}
	return coins.AmountOf(denom).IsPositive()
}
