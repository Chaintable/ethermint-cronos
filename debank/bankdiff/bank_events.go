package bankdiff

import (
	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"
)

// CoinTouchedAddresses scans a block's ABCI results for coin_spent / coin_received
// events that move the EVM denom (native CRO) and returns the EVM-address set
// whose native balance changed in block N.
//
// Cronos is bank-backed, so native CRO movements (gas/fee, plain transfers,
// x/cronos CRC20 convert, IBC, precompile bank ops) never reach the EVM tracer.
// These addresses — many of which are module accounts not present in any tx
// from/to — are discovered here, then looked up with GetBalance(addr, N) in the
// pipeline to obtain the authoritative post-N balance.
//
// Only coin_spent/coin_received are used (not transfer/coinbase/mint/burn): they
// are emitted on every leg of every native movement, so the union is exact with
// no double counting. Movements are filtered by evmDenom so non-CRO transfers
// (e.g. IBC tokens) don't add spurious accounts. Non-20-byte cosmos addresses
// (e.g. some ADR-028 derived accounts) are skipped — they have no EVM form.
func CoinTouchedAddresses(blockRes *coretypes.ResultBlockResults, evmDenom string) map[common.Address]struct{} {
	out := make(map[common.Address]struct{})
	scanEvents(out, blockRes.FinalizeBlockEvents, evmDenom)
	for _, txr := range blockRes.TxsResults {
		scanEvents(out, txr.Events, evmDenom)
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
			switch attr.Key {
			case addrKey:
				bech32 = attr.Value
			case sdk.AttributeKeyAmount:
				amount = attr.Value
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

// movesDenom reports whether the coins string (sdk.Coins.String()) contains a
// positive amount of denom.
func movesDenom(amount, denom string) bool {
	coins, err := sdk.ParseCoinsNormalized(amount)
	if err != nil {
		return false
	}
	return coins.AmountOf(denom).IsPositive()
}
