package replay

import (
	"strconv"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
)

func TestHistoricalAuthzWithdrawals(t *testing.T) {
	reward := &distrtypes.MsgWithdrawDelegatorReward{DelegatorAddress: "delegator", ValidatorAddress: "validator"}
	commission := &distrtypes.MsgWithdrawValidatorCommission{ValidatorAddress: "validator"}
	exec := authz.NewMsgExec(sdk.AccAddress("grantee"), []sdk.Msg{reward, commission})
	ev := func(kind string, kv ...string) abci.Event {
		event := sdk.NewEvent(kind)
		for i := 0; i < len(kv); i += 2 {
			event.Attributes = append(event.Attributes, sdk.NewAttribute(kv[i], kv[i+1]).ToKVPair())
		}
		return abci.Event(event)
	}
	makeEvents := func(indexed bool, zero string) []abci.Event {
		events := []abci.Event{
			ev("withdraw_rewards", "amount", zero, "validator", reward.ValidatorAddress),
			ev("message", "module", "distribution", "sender", reward.DelegatorAddress),
			ev("coin_spent", "spender", "moduleAcct", "amount", "325basecro"),
			ev("coin_received", "receiver", "withdrawAddr", "amount", "325basecro"),
			ev("transfer", "recipient", "withdrawAddr", "sender", "moduleAcct", "amount", "325basecro"),
			ev("message", "sender", "moduleAcct"),
			ev("withdraw_commission", "amount", "325basecro"),
			ev("message", "module", "distribution", "sender", commission.ValidatorAddress),
		}
		if indexed {
			for i := range events {
				idx := 0
				if i >= 2 {
					idx = 1
				}
				events[i].Attributes = append(events[i].Attributes, abci.EventAttribute{Key: []byte(authzMsgIndex), Value: []byte(strconv.Itoa(idx))})
			}
		}
		return events
	}
	for _, indexed := range []bool{false, true} {
		require.True(t, matchHistoricalAuthz(&exec, makeEvents(indexed, ""), makeEvents(true, "0stake")))
	}
	// The archive also contains MsgExec with only a commission withdrawal.
	onlyCommission := authz.NewMsgExec(sdk.AccAddress("grantee"), []sdk.Msg{commission})
	wCommission, gCommission := makeEvents(false, "")[2:], makeEvents(true, "")[2:]
	for i, ev := range gCommission {
		gCommission[i].Attributes[len(ev.Attributes)-1].Value = []byte("0")
	}
	require.True(t, matchHistoricalAuthz(&onlyCommission, wCommission, gCommission))
	errCases := map[string]func([]abci.Event, []abci.Event){
		"wrong child index": func(_, g []abci.Event) { g[2].Attributes[2].Value = []byte("0") },
		"wrong archived index": func(w, _ []abci.Event) {
			w[2].Attributes = append(w[2].Attributes, abci.EventAttribute{Key: []byte(authzMsgIndex), Value: []byte("0")})
		},
		"partial worker index":     func(_, g []abci.Event) { g[1].Attributes = g[1].Attributes[:2] },
		"duplicate worker index":   func(_, g []abci.Event) { g[0].Attributes = append(g[0].Attributes, g[0].Attributes[2]) },
		"wrong worker sender":      func(_, g []abci.Event) { g[1].Attributes[1].Value = []byte("stranger") },
		"wrong archive sender":     func(w, _ []abci.Event) { w[7].Attributes[1].Value = []byte("stranger") },
		"changed bank transfer":    func(_, g []abci.Event) { g[4].Attributes[2].Value = []byte("326basecro") },
		"changed commission":       func(w, _ []abci.Event) { w[6].Attributes[0].Value = []byte("0basecro") },
		"nonzero reward":           func(_, g []abci.Event) { g[0].Attributes[0].Value = []byte("1stake") },
		"extra archived attribute": func(w, _ []abci.Event) { w[0].Attributes = append(w[0].Attributes, w[0].Attributes[0]) },
	}
	for name, mutate := range errCases {
		t.Run(name, func(t *testing.T) {
			w, g := makeEvents(false, ""), makeEvents(true, "0stake")
			mutate(w, g)
			require.False(t, matchHistoricalAuthz(&exec, w, g))
		})
	}
	w, g := makeEvents(false, ""), makeEvents(true, "0stake")
	reversed := authz.NewMsgExec(sdk.AccAddress("grantee"), []sdk.Msg{commission, reward})
	require.False(t, matchHistoricalAuthz(&reversed, w, g))
	require.False(t, matchHistoricalAuthz(&exec, w[:7], g[:7]))
	require.False(t, matchHistoricalAuthz(&exec, append(w, w[7]), append(g, g[7])))
}
