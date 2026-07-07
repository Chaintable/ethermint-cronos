package keeper

// Consensus-guided block tracing (DeBank).
//
// Replaying an old-era block under the current binary's gas semantics can
// diverge from what consensus recorded (see the emitter-side reconciliation
// in rpc/namespaces/ethereum/trace). Divergence has two consequences inside
// a block replay:
//
//   - a tx whose outcome flips loses/pollutes state for every later tx in
//     the same block (a consensus-reverted tx that "succeeds" in replay
//     leaks writes; a consensus-successful tx that OOGs in replay loses its
//     writes and truncates its call tree)
//   - a consensus-successful tx that OOGs in replay yields a truncated trace
//
// When the caller supplies per-tx consensus guidance (status + logs) through
// TraceConfig.TracerJsonConfig, TraceBlock replays the block consensus-guided:
//
//   - consensus-failed txs are traced WITHOUT committing state (their writes
//     never persisted historically), and the block log index does not advance
//   - consensus-successful txs must reproduce the consensus logs; if the
//     plain replay fails to, the tx is retried on a rising gas-limit ladder
//     and the first attempt whose (success, logs) matches consensus is
//     committed. If no rung matches, the highest-gas attempt is committed so
//     the call tree is at least complete (never truncated); the emitter's
//     reconciliation then anchors status/gas/events to consensus.
//
// Guidance only affects the query/trace path; consensus execution never
// passes through here.

import (
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"

	"github.com/evmos/ethermint/x/evm/types"
)

// guidanceGasCap is the top rung of the retry ladder: far above any real
// historical consumption, so a consensus-successful tx can never OOG here.
const guidanceGasCap = 100_000_000

// scaleGasSat returns original*factor saturated at guidanceGasCap. Real gas
// limits never approach the cap, but the bare multiply is unbounded and would
// overflow uint64 for a pathological limit; saturating keeps the ladder valid.
func scaleGasSat(original, factor uint64) uint64 {
	if original > guidanceGasCap/factor {
		return guidanceGasCap
	}
	return original * factor
}

type GuidanceLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

// TxGuidance is the consensus truth for one tx, in block tx order.
type TxGuidance struct {
	Failed bool          `json:"failed"`
	Logs   []GuidanceLog `json:"logs"`
}

type guidanceEnvelope struct {
	Guidance []*TxGuidance `json:"debankConsensusGuidance"`
}

// parseGuidance extracts consensus guidance from TracerJsonConfig. The field
// doubles as the (ignored) json config of the debank tracer, so an envelope
// key namespaces it. An absent/empty config disables guidance; a non-empty
// config that will not parse is a broken emitter->keeper contract and returns
// an error rather than silently degrading to an unreconciled replay.
func parseGuidance(tc *types.TraceConfig) ([]*TxGuidance, error) {
	if tc == nil || tc.TracerJsonConfig == "" {
		return nil, nil
	}
	var env guidanceEnvelope
	if err := json.Unmarshal([]byte(tc.TracerJsonConfig), &env); err != nil {
		return nil, fmt.Errorf("parse consensus guidance envelope: %w", err)
	}
	return env.Guidance, nil
}

// guidanceAt returns the guidance entry for tx index i, nil when absent.
func guidanceAt(g []*TxGuidance, i int) *TxGuidance {
	if i < 0 || i >= len(g) {
		return nil
	}
	return g[i]
}

func logsMatchGuidance(logs []*types.Log, want []GuidanceLog) bool {
	if len(logs) != len(want) {
		return false
	}
	for i, l := range logs {
		w := want[i]
		if !strings.EqualFold(l.Address, w.Address) || len(l.Topics) != len(w.Topics) {
			return false
		}
		for j := range l.Topics {
			if !strings.EqualFold(l.Topics[j], w.Topics[j]) {
				return false
			}
		}
		if !strings.EqualFold(hexutil.Encode(l.Data), normalizeHexData(w.Data)) {
			return false
		}
	}
	return true
}

func normalizeHexData(s string) string {
	if s == "" {
		return "0x"
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return "0x" + s
	}
	return s
}

// guidedTrace traces one message under consensus guidance. Returns the trace
// result and the next block log index.
func (k *Keeper) guidedTrace(
	ctx sdk.Context,
	cfg *EVMConfig,
	msg *core.Message,
	traceConfig *types.TraceConfig,
	g *TxGuidance,
) (*interface{}, uint, error) {
	if g.Failed {
		// Consensus rolled this tx back: trace it dry (no state commit) so
		// later txs see the state history really left behind. Its logs never
		// existed, so the block log index does not advance.
		tr, _, _, err := k.prepareTrace(ctx, cfg, msg, traceConfig, false)
		return tr, cfg.TxConfig.LogIndex, err
	}

	original := msg.GasLimit
	ladder := []uint64{original, scaleGasSat(original, 2), scaleGasSat(original, 4), guidanceGasCap}
	chosen := uint64(0)        // lowest rung whose logs reproduce consensus (best)
	firstComplete := uint64(0) // lowest rung that executed without failing (faithful fallback)
	for _, gl := range ladder {
		if gl < original {
			continue
		}
		msg.GasLimit = gl
		_, _, res, err := k.prepareTrace(ctx, cfg, msg, traceConfig, false)
		if err != nil {
			continue
		}
		if res.Failed() {
			continue
		}
		if firstComplete == 0 {
			firstComplete = gl
		}
		if logsMatchGuidance(res.Logs, g.Logs) {
			chosen = gl
			break
		}
	}
	if chosen == 0 {
		if firstComplete != 0 {
			// No rung reproduced the consensus logs, but the tx completes at
			// firstComplete. Commit the LOWEST completing gas, not the cap: it
			// is closest to the historical gas budget, so a gas-sensitive
			// contract keeps the call tree (and internal native transfers) it
			// actually took on-chain, instead of the divergent branch a huge gas
			// budget can trigger (a forwarder that stops forwarding, etc.). Only
			// an original-gas OOG escalates, and no further than needed.
			// Events/status are anchored to consensus by reconciliation.
			chosen = firstComplete
			k.Logger(ctx).Info("debank trace guidance: no gas rung reproduced consensus logs; committing lowest completing gas",
				"tx", cfg.TxConfig.TxHash.Hex(), "gas_limit", original, "committed", chosen)
		} else {
			// The tx fails at every rung up to the cap: under the current binary
			// there is no faithful tree. Commit the cap for a complete-as-possible
			// tree, but flag the tx — its internal-transfer tree is reduced
			// confidence and should be handled/backfilled separately.
			chosen = guidanceGasCap
			if original > chosen {
				chosen = original
			}
			k.Logger(ctx).Warn("debank trace guidance: tx unreproducible at every gas rung, committing reduced-confidence tree",
				"tx", cfg.TxConfig.TxHash.Hex(), "gas_limit", original)
		}
	}
	msg.GasLimit = chosen
	tr, _, _, err := k.prepareTrace(ctx, cfg, msg, traceConfig, true)
	// Advance the block log index by the consensus log count: that is what
	// the reconciled block file will contain for this tx.
	return tr, cfg.TxConfig.LogIndex + uint(len(g.Logs)), err
}
