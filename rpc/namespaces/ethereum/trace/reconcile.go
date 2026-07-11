package trace

// Consensus reconciliation for trace_debankBlock.
//
// The block file is assembled from a replay of historical transactions under
// the CURRENT binary's execution semantics. Ethermint chains evolve by
// swapping binaries, so gas accounting drifts across eras and a replay of an
// old near-gas-limit tx can flip its outcome (OOG <-> success) relative to
// what consensus actually recorded (observed on cronosmainnet_25-1 for
// pre-v1.2 blocks: 4 status-flip txs around heights 1.28M-13.18M, all within
// 0.7-6.4% of their gas limit).
//
// The node itself stores the consensus truth: per-tx status/gasUsed/logs are
// recoverable from the CometBFT block results (the eth_getBlockReceipts
// source). This file reconciles every replayed tx against that truth:
//
//   - tx status / gas_used      <- consensus (always; no-op when replay agrees)
//   - trace bucket + root error <- consensus status (frame internals stay
//     replay-derived: consensus keeps no call traces, best-effort by nature)
//   - events                    <- consensus logs; parent linkage inherited
//     from the paired replay event (equal count -> positional, else content
//     match), otherwise attached to the root trace at the next free position
//   - events of a consensus-reverted tx -> error_events (idx=0), mirroring
//     the producer's convention for failed txs
//
// Replay==consensus (the overwhelmingly common case) leaves everything
// untouched, so this is always-on rather than gated by height.

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/evmos/ethermint/debank/util"
	rpctypes "github.com/evmos/ethermint/rpc/types"
)

// consensusReceipt is the stored consensus truth for one transaction.
type consensusReceipt struct {
	Status  bool
	GasUsed uint64
	Logs    []*ethtypes.Log
}

// reconcileSummary reports what reconciliation changed for one tx.
type reconcileSummary struct {
	StatusFlipped bool
	EventsRebuilt bool
	DroppedEvents int
	RootAttached  int
}

func (s *reconcileSummary) changed() bool {
	return s.StatusFlipped || s.EventsRebuilt
}

// loadConsensus builds the per-tx consensus map for a block from the node's
// own receipt path (CometBFT block results). Failing to load it fails the
// whole trace: emitting unreconciled data would silently reintroduce the
// replay-divergence class this exists to fix.
func (api *API) loadConsensus(blockHeight rpctypes.BlockNumber) (map[string]*consensusReceipt, error) {
	rcpts, err := api.backend.GetBlockReceipts(blockHeight)
	if err != nil {
		return nil, fmt.Errorf("load consensus receipts for block %d: %w", blockHeight, err)
	}
	cons := make(map[string]*consensusReceipt, len(rcpts))
	for _, rc := range rcpts {
		var key string
		switch h := rc["transactionHash"].(type) {
		case interface{ Hex() string }: // common.Hash, what buildReceiptDirect stores today
			key = strings.ToLower(h.Hex())
		case string: // tolerate a receipt builder that stores the hex string directly
			key = strings.ToLower(h)
		default:
			return nil, fmt.Errorf("receipt transactionHash has unexpected type %T", rc["transactionHash"])
		}
		status, ok := rc["status"].(hexutil.Uint)
		if !ok {
			return nil, fmt.Errorf("receipt status has unexpected type %T", rc["status"])
		}
		gasUsed, ok := rc["gasUsed"].(hexutil.Uint64)
		if !ok {
			return nil, fmt.Errorf("receipt gasUsed has unexpected type %T", rc["gasUsed"])
		}
		// logs is fail-closed like the fields above: this reconciliation path is
		// mandatory, so a type mismatch (nil logs) would silently rebuild every
		// consensus-success tx to zero events instead of loudly aborting.
		logs, ok := rc["logs"].([]*ethtypes.Log)
		if !ok {
			return nil, fmt.Errorf("receipt logs has unexpected type %T", rc["logs"])
		}
		cons[key] = &consensusReceipt{
			Status:  status == hexutil.Uint(ethtypes.ReceiptStatusSuccessful),
			GasUsed: uint64(gasUsed),
			Logs:    logs,
		}
	}
	return cons, nil
}

// reconcileTx aligns one tx's replay output (tr holds only this tx's frames
// and events) with its consensus receipt, in place.
func reconcileTx(tr *dtypes.TraceResult, c *consensusReceipt, gasLimit uint64) reconcileSummary {
	var sum reconcileSummary
	if c == nil {
		return sum
	}

	// Locate the tx-level root frame (TraceAddress == []). Mutate it by
	// index in its current bucket BEFORE any bucket move: moves copy trace
	// structs, so a pointer taken here would go stale afterwards.
	bucket, rootIdx, replaySuccess := findRoot(tr)
	if rootIdx < 0 {
		return sum
	}
	root := &bucket[rootIdx]

	// tx-level gas: consensus value on the root trace (the tx entry is built
	// from it via rootGasAndStatus).
	root.GasUsed = new(big.Int).SetUint64(c.GasUsed)

	if c.Status != replaySuccess {
		sum.StatusFlipped = true
		if c.Status {
			root.Error = "" // only the root: child frames keep their genuine errors
		} else if c.GasUsed >= gasLimit {
			root.Error = "out of gas"
		} else {
			root.Error = "execution reverted"
		}
	}
	rootID := root.ID

	// Move buckets after root mutations (append copies the structs).
	if c.Status != replaySuccess {
		if c.Status {
			tr.Traces = append(tr.Traces, tr.ErrorTraces...)
			tr.ErrorTraces = nil
		} else {
			tr.ErrorTraces = append(tr.Traces, tr.ErrorTraces...)
			tr.Traces = nil
		}
	}

	if !c.Status {
		// A consensus-reverted tx contributes no receipt logs. Whatever the
		// replay emitted goes to error_events (idx=0), the producer's
		// convention for failed txs.
		if len(tr.Events) > 0 {
			sum.EventsRebuilt = true
			for i := range tr.Events {
				tr.Events[i].LogIndex = 0
			}
			tr.ErrorEvents = append(tr.ErrorEvents, tr.Events...)
			tr.Events = nil
		}
		return sum
	}

	// Consensus success: events must equal the consensus logs. The replay
	// source is Events normally, or ErrorEvents when the replay had reverted.
	source := tr.Events
	fromErrorBucket := false
	if sum.StatusFlipped {
		source = tr.ErrorEvents
		fromErrorBucket = true
	}
	if !eventsMatch(source, c.Logs) {
		sum.EventsRebuilt = true
		rebuilt, dropped, attached := rebuildEvents(source, c.Logs, rootID, tr)
		sum.DroppedEvents = dropped
		sum.RootAttached = attached
		tr.Events = rebuilt
		if fromErrorBucket {
			tr.ErrorEvents = nil
		}
	} else if fromErrorBucket {
		tr.Events = source
		tr.ErrorEvents = nil
	}
	return sum
}

// findRoot locates the tx-level root frame. Returns the bucket slice it lives
// in, its index (or -1), and whether that bucket is the success one.
func findRoot(tr *dtypes.TraceResult) ([]dtypes.Trace, int, bool) {
	for i := range tr.Traces {
		if len(tr.Traces[i].TraceAddress) == 0 {
			return tr.Traces, i, true
		}
	}
	for i := range tr.ErrorTraces {
		if len(tr.ErrorTraces[i].TraceAddress) == 0 {
			return tr.ErrorTraces, i, false
		}
	}
	return nil, -1, false
}

// logSig is the comparable content of a consensus log; eventSig the same for
// a replay event. A zero-topic log yields an empty selector and no topics,
// so both sides collapse to the same signature.
func logSig(l *ethtypes.Log) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(l.Address.Hex()))
	for _, t := range l.Topics {
		b.WriteString("|")
		b.WriteString(strings.ToLower(t.Hex()))
	}
	b.WriteString("#")
	b.WriteString(hexutil.Bytes(l.Data).String())
	return b.String()
}

func eventSig(e *dtypes.Event) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(e.Address))
	if e.Selector != "" {
		b.WriteString("|")
		b.WriteString(strings.ToLower(e.Selector))
	}
	for _, t := range e.Topics {
		b.WriteString("|")
		b.WriteString(strings.ToLower(t))
	}
	b.WriteString("#")
	b.WriteString(e.Data.String())
	return b.String()
}

func eventsMatch(events []dtypes.Event, logs []*ethtypes.Log) bool {
	if len(events) != len(logs) {
		return false
	}
	for i := range events {
		if eventSig(&events[i]) != logSig(logs[i]) {
			return false
		}
	}
	return true
}

// rebuildEvents produces one event per consensus log, in log order. Parent
// linkage (parent_trace_id/pos/id) is inherited from a paired replay event
// when possible; unpaired logs are attached to the root trace at the next
// free position. Returns (events, dropped replay events, root-attached logs).
func rebuildEvents(source []dtypes.Event, logs []*ethtypes.Log, rootID string, tr *dtypes.TraceResult) ([]dtypes.Event, int, int) {
	matched := make([]bool, len(source))

	// positions already taken under the root: child traces plus every source
	// event that may be inherited (reserving a dropped event's slot is
	// harmless, colliding with an inherited one is not).
	usedRootPos := make(map[int64]bool)
	for _, bucket := range [][]dtypes.Trace{tr.Traces, tr.ErrorTraces} {
		for i := range bucket {
			if bucket[i].ParentTraceID == rootID {
				usedRootPos[bucket[i].PosInParentTrace] = true
			}
		}
	}
	for i := range source {
		if source[i].ParentTraceID == rootID {
			usedRootPos[source[i].Position] = true
		}
	}
	nextRootPos := func() int64 {
		var p int64
		for ; usedRootPos[p]; p++ {
		}
		usedRootPos[p] = true
		return p
	}

	// Pair each consensus log to the replay event that emitted it, then take
	// content from consensus. Two passes, so an equal event count can no longer
	// staple a log onto the wrong frame (the old positional bug where a reordered
	// replay made source[j] not the emitter of logs[j]):
	//   1. exact content signature — the true emitter when the replay reproduced
	//      the log; this alone fixes pure reordering within the tx.
	//   2. only when the replay and consensus counts match, pair the leftovers in
	//      order: an equal count means the divergence is content-only (e.g. a
	//      gaslimit-seeded tokenId), so a leftover log is the same emit as the
	//      leftover event and inherits its frame.
	// When the counts differ the sets themselves diverge, so a log matched by
	// neither pass has no replay emitter and attaches to the root below.
	srcFor := make([]int, len(logs))
	for j := range srcFor {
		srcFor[j] = -1
	}
	for j, lg := range logs {
		want := logSig(lg)
		for i := range source {
			if !matched[i] && eventSig(&source[i]) == want {
				srcFor[j], matched[i] = i, true
				break
			}
		}
	}
	if len(source) == len(logs) {
		si := 0
		for j := range logs {
			if srcFor[j] >= 0 {
				continue
			}
			for si < len(source) && matched[si] {
				si++
			}
			if si >= len(source) {
				break
			}
			srcFor[j], matched[si] = si, true
		}
	}

	attached := 0
	out := make([]dtypes.Event, 0, len(logs))
	for j, lg := range logs {
		var ev dtypes.Event
		if src := srcFor[j]; src >= 0 {
			ev = source[src] // inherit ID/ParentTraceID/Position from the emitter
		} else {
			pos := nextRootPos()
			ev = dtypes.Event{
				ID:            util.ToHash([]string{rootID, strconv.FormatInt(pos, 10)}),
				ParentTraceID: rootID,
				Position:      pos,
			}
			attached++
		}
		// content always from the consensus log
		ev.Address = strings.ToLower(lg.Address.Hex())
		ev.Selector = ""
		ev.Topics = nil
		if len(lg.Topics) > 0 {
			topics := make([]string, len(lg.Topics))
			for i, t := range lg.Topics {
				topics[i] = t.Hex()
			}
			ev.Selector = topics[0]
			ev.Topics = topics[1:]
		}
		ev.Data = lg.Data
		out = append(out, ev)
	}

	dropped := 0
	for i := range matched {
		if !matched[i] {
			dropped++
		}
	}
	return out, dropped, attached
}
