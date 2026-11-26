package replay

import (
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	abci "github.com/tendermint/tendermint/abci/types"
)

// SDK #12673 appended the index of each dispatched message to all of its
// events. Recover the message boundaries from the signed MsgExec and the
// distribution handlers, independently of the indices reported by the worker.
func matchHistoricalAuthz(exec *authz.MsgExec, want, got []abci.Event) bool {
	if len(want) == 0 || len(want) != len(got) {
		return false
	}
	msgs, err := exec.GetMessages()
	if err != nil || len(msgs) == 0 {
		return false
	}
	archiveIndexed := hasAuthzIndex(want[0])
	for i, msg := range msgs {
		size := withdrawalEventCount(msg, got)
		if size == 0 {
			return false
		}
		index := strconv.Itoa(i)
		oldEvents := unindexDispatch(want[:size], index, archiveIndexed)
		newEvents := unindexDispatch(got[:size], index, true)
		if oldEvents == nil || newEvents == nil {
			return false
		}
		if compareEvents(oldEvents, newEvents) != nil && !matchZeroReward(msg, oldEvents, newEvents) {
			return false
		}
		want, got = want[size:], got[size:]
	}
	return len(got) == 0
}

// Successful withdrawals emit optional bank events, then the withdrawal and
// the distribution module's sender event. An authz child has no action event.
func withdrawalEventCount(msg sdk.Msg, events []abci.Event) int {
	if len(events) < 2 {
		return 0
	}
	var eventType, sender, validator string
	switch m := msg.(type) {
	case *distrtypes.MsgWithdrawDelegatorReward:
		eventType, sender, validator = distrtypes.EventTypeWithdrawRewards, m.DelegatorAddress, m.ValidatorAddress
	case *distrtypes.MsgWithdrawValidatorCommission:
		eventType, sender = distrtypes.EventTypeWithdrawCommission, m.ValidatorAddress
	default:
		return 0
	}
	bank := 0
	if events[0].Type == banktypes.EventTypeCoinSpent {
		if len(events) < 6 || events[1].Type != banktypes.EventTypeCoinReceived ||
			events[2].Type != banktypes.EventTypeTransfer || events[3].Type != sdk.EventTypeMessage {
			return 0
		}
		bank = 4
	}
	withdrawal := events[bank]
	if validator != "" && (len(withdrawal.Attributes) != 3 ||
		string(withdrawal.Attributes[1].Key) != distrtypes.AttributeKeyValidator || string(withdrawal.Attributes[1].Value) != validator) {
		return 0
	}
	end := events[bank+1]
	if withdrawal.Type != eventType || end.Type != sdk.EventTypeMessage || len(end.Attributes) != 3 ||
		string(end.Attributes[0].Key) != sdk.AttributeKeyModule || string(end.Attributes[0].Value) != distrtypes.ModuleName ||
		string(end.Attributes[1].Key) != sdk.AttributeKeySender || string(end.Attributes[1].Value) != sender {
		return 0
	}
	return bank + 2
}

const authzMsgIndex = "authz_msg_index"

func hasAuthzIndex(event abci.Event) bool {
	attrs := event.Attributes
	return len(attrs) > 0 && string(attrs[len(attrs)-1].Key) == authzMsgIndex
}

func unindexDispatch(events []abci.Event, index string, indexed bool) []abci.Event {
	out := append([]abci.Event(nil), events...)
	for i, event := range out {
		attrs := event.Attributes
		if indexed {
			if !hasAuthzIndex(event) || string(attrs[len(attrs)-1].Value) != index {
				return nil
			}
			attrs = attrs[:len(attrs)-1]
		}
		for _, attr := range attrs {
			if string(attr.Key) == authzMsgIndex {
				return nil
			}
		}
		out[i].Attributes = attrs
	}
	return out
}
