package tracer

import (
	"fmt"
	"sort"

	dtypes "github.com/tharsis/ethermint/debank/types"
)

// RevertTrace rewrites the trace of a message whose root returned as the
// tracer reports it when the root fails with vmError: every frame and log is
// an error, in the tracer's order, and no storage change is kept.
func RevertTrace(tr *dtypes.TraceResult, vmError string) error {
	frames := append(append([]dtypes.Trace(nil), tr.Traces...), tr.ErrorTraces...)
	children := map[string][]dtypes.Trace{}
	var roots []dtypes.Trace
	for _, f := range frames {
		if len(f.TraceAddress) == 0 {
			roots = append(roots, f)
		} else {
			children[f.ParentTraceID] = append(children[f.ParentTraceID], f)
		}
	}
	if len(roots) != 1 || roots[0].Error != "" {
		return fmt.Errorf("expected one returned root, found %d", len(roots))
	}
	logs := map[string][]dtypes.Event{}
	for _, e := range append(append([]dtypes.Event(nil), tr.Events...), tr.ErrorEvents...) {
		logs[e.ParentTraceID] = append(logs[e.ParentTraceID], e)
	}
	for _, c := range children {
		sort.Slice(c, func(i, j int) bool { return c[i].PosInParentTrace < c[j].PosInParentTrace })
	}
	for _, l := range logs {
		sort.Slice(l, func(i, j int) bool { return l[i].Position < l[j].Position })
	}

	root := roots[0]
	root.Error = vmError
	traces := []dtypes.Trace{root}
	events := []dtypes.Event{}
	// Same order as addTraceAndLog: logs in execution order, and a frame's
	// children after the descendants of each child.
	var visit func(id string)
	visit = func(id string) {
		kids, own := children[id], logs[id]
		for k, l := 0, 0; k < len(kids) || l < len(own); {
			if k >= len(kids) || (l < len(own) && own[l].Position < kids[k].PosInParentTrace) {
				e := own[l]
				e.LogIndex = 0
				events = append(events, e)
				l++
			} else {
				visit(kids[k].ID)
				k++
			}
		}
		for _, c := range kids {
			if c.Error == "" {
				c.Error = "parent call failed"
			}
			traces = append(traces, c)
		}
	}
	visit(root.ID)
	if len(traces) != len(frames) || len(events) != len(tr.Events)+len(tr.ErrorEvents) {
		return fmt.Errorf("trace tree has %d of %d frames and %d of %d logs", len(traces), len(frames), len(events), len(tr.Events)+len(tr.ErrorEvents))
	}
	tr.Traces, tr.Events = []dtypes.Trace{}, []dtypes.Event{}
	tr.ErrorTraces, tr.ErrorEvents = traces, events
	tr.StorageContracts = []string{}
	return nil
}
