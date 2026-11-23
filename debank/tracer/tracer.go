package tracer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/evmos/ethermint/debank/util"
)

// callFrame is one EVM call frame of the DeBank trace.
type callFrame struct {
	Type         vm.OpCode       `json:"-"`
	From         common.Address  `json:"from"`
	Gas          uint64          `json:"gas"`
	GasUsed      uint64          `json:"gasUsed"`
	To           *common.Address `json:"to,omitempty" rlp:"optional"`
	Input        []byte          `json:"input" rlp:"optional"`
	Output       []byte          `json:"output,omitempty" rlp:"optional"`
	Error        string          `json:"error,omitempty" rlp:"optional"`
	RevertReason string          `json:"revertReason,omitempty"`
	ParentFailed bool
	Calls        []callFrame    `json:"calls,omitempty" rlp:"optional"`
	Logs         []dtypes.Event `json:"logs,omitempty" rlp:"optional"`

	PosInParentTrace  int    `json:"pos_in_parent_trace"`
	ParentTraceID     string `json:"parent_trace_id"`
	TraceID           string `json:"trace_id"`
	StorageChange     bool   `json:"storageChange"`
	SelfStorageChange bool   `json:"self_storage_change"`

	// Placed at end on purpose. The RLP will be decoded to 0 instead of
	// nil if there are non-empty elements after in the struct.
	Value *big.Int `json:"value,omitempty" rlp:"optional"`
}

func (f callFrame) failed() bool {
	return len(f.Error) > 0
}

func (f *callFrame) processOutput(output []byte, err error, reverted bool) {
	output = common.CopyBytes(output)
	// Clear error if tx wasn't reverted. This happened
	// for pre-homestead contract storage OOG.
	if err != nil && !reverted {
		err = nil
	}
	if err == nil {
		f.Output = output
		return
	}
	f.Error = err.Error()
	if f.Type == vm.CREATE || f.Type == vm.CREATE2 {
		f.To = nil
	}
	if !errors.Is(err, vm.ErrExecutionReverted) || len(output) == 0 {
		return
	}
	f.Output = output
	if len(output) < 4 {
		return
	}
	if unpacked, err := abi.UnpackRevert(output); err == nil {
		f.RevertReason = unpacked
	}
}

// debankTracer collects the DeBank call tree, logs and storage-changing
// contracts of one signed EVM message from core/tracing hooks. Native CRO
// balances and the block state diff come from the committed IAVL stores.
//
// OnStorageChange fires at execution time and a revert does not undo it, so the
// hook only records touched slots; committed values are re-read from the StateDB
// once the message finishes.
type debankTracer struct {
	hash      common.Hash
	callstack []callFrame
	entered   int
	stateDB   tracing.StateDB

	traces      []dtypes.Trace
	logs        []dtypes.Event
	errorTraces []dtypes.Trace
	errorLogs   []dtypes.Event

	touchedStorage map[common.Address]map[common.Hash]common.Hash // addr -> slot -> pre-tx value
	storageChanges map[common.Address]struct{}                    // contracts with a committed storage change

	committed bool
	done      bool
}

// NewReplayTracer traces one signed message. The returned function finalizes
// the result after Cosmos post-processing; a non-empty vmError marks a root
// that the EVM completed but post-processing reverted.
func NewReplayTracer(hash common.Hash) (*tracing.Hooks, func(string) (json.RawMessage, error)) {
	t := &debankTracer{
		hash:           hash,
		traces:         make([]dtypes.Trace, 0),
		logs:           make([]dtypes.Event, 0),
		errorTraces:    make([]dtypes.Trace, 0),
		errorLogs:      make([]dtypes.Event, 0),
		touchedStorage: make(map[common.Address]map[common.Hash]common.Hash),
		storageChanges: make(map[common.Address]struct{}),
	}
	hooks := &tracing.Hooks{
		OnTxStart:       t.OnTxStart,
		OnTxEnd:         t.OnTxEnd,
		OnEnter:         t.OnEnter,
		OnExit:          t.OnExit,
		OnLog:           t.OnLog,
		OnStorageChange: t.OnStorageChange,
	}
	return hooks, func(vmError string) (json.RawMessage, error) {
		if vmError != "" && len(t.callstack) == 1 && t.callstack[0].Error == "" {
			t.callstack[0].Error = vmError
			t.committed = true
			clear(t.storageChanges)
		}
		return t.result()
	}
}

// --- call tree channel (modeled on go-ethereum eth/tracers/native/call.go) ---

func (t *debankTracer) OnTxStart(env *tracing.VMContext, _ *ethtypes.Transaction, _ common.Address) {
	if env != nil {
		t.stateDB = env.StateDB
	}
}

func (t *debankTracer) OnEnter(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	t.entered++
	toCopy := to
	call := callFrame{
		Type:  vm.OpCode(typ),
		From:  from,
		To:    &toCopy,
		Input: common.CopyBytes(input),
		Gas:   gas,
		Value: value,
	}
	t.callstack = append(t.callstack, call)
}

func (t *debankTracer) OnExit(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
	if depth == 0 {
		t.captureEnd(output, gasUsed, err, reverted)
		return
	}
	size := len(t.callstack)
	if size <= 1 {
		return
	}
	// Pop call.
	call := t.callstack[size-1]
	t.callstack = t.callstack[:size-1]
	size -= 1

	call.GasUsed = gasUsed
	call.processOutput(output, err, reverted)

	call.PosInParentTrace = len(t.callstack[size-1].Calls) + len(t.callstack[size-1].Logs)
	t.callstack[size-1].Calls = append(t.callstack[size-1].Calls, call)
}

func (t *debankTracer) captureEnd(output []byte, gasUsed uint64, err error, reverted bool) {
	if len(t.callstack) != 1 {
		return
	}
	t.callstack[0].GasUsed = gasUsed
	t.callstack[0].processOutput(output, err, reverted)
}

func (t *debankTracer) OnTxEnd(_ *ethtypes.Receipt, _ error) {
	t.commitStorageChanges()
}

func (t *debankTracer) OnLog(log *ethtypes.Log) {
	topics := make([]string, len(log.Topics))
	for i, topic := range log.Topics {
		topics[i] = topic.Hex()
	}
	var selector string
	var remainingTopics []string
	if len(topics) > 0 {
		selector = topics[0]
		remainingTopics = topics[1:]
	}
	// Defense-in-depth: a log without an enclosing call frame (empty callstack)
	// would index callstack[-1] and panic. Should not happen once OnEnter fires
	// for the root frame, but guard so a malformed/edge trace degrades gracefully
	// instead of taking down the whole trace_debankBlock query.
	if len(t.callstack) == 0 {
		return
	}
	top := &t.callstack[len(t.callstack)-1]
	l := dtypes.Event{
		Address:  strings.ToLower(log.Address.Hex()),
		Selector: selector,
		Topics:   remainingTopics,
		Data:     log.Data,
		Position: int64(len(top.Calls) + len(top.Logs)),
		LogIndex: int64(log.Index),
	}
	top.Logs = append(top.Logs, l)
}

func (t *debankTracer) OnStorageChange(addr common.Address, slot common.Hash, prev common.Hash, _ common.Hash) {
	slots := t.touchedStorage[addr]
	if slots == nil {
		slots = make(map[common.Hash]common.Hash)
		t.touchedStorage[addr] = slots
	}
	if _, seen := slots[slot]; !seen {
		slots[slot] = prev
	}
	if n := len(t.callstack); n > 0 {
		t.callstack[n-1].SelfStorageChange = true
		t.callstack[n-1].StorageChange = true
	}
}

// commitStorageChanges keeps contracts whose committed storage differs from the
// pre-message value, dropping writes undone by a revert.
func (t *debankTracer) commitStorageChanges() {
	if t.committed || t.stateDB == nil {
		return
	}
	t.committed = true
	for addr, slots := range t.touchedStorage {
		for slot, pre := range slots {
			if t.stateDB.GetState(addr, slot) != pre {
				t.storageChanges[addr] = struct{}{}
				break
			}
		}
	}
}

func setParentFailed(cf *callFrame, parentFailed bool) {
	failed := cf.failed() || parentFailed
	for i := range cf.Calls {
		cf.Calls[i].ParentFailed = failed
		setParentFailed(&cf.Calls[i], failed)
	}
}

func setStorageChange(cf *callFrame) {
	subCallStorageChange := false
	for i := range cf.Calls {
		setStorageChange(&cf.Calls[i])
		if cf.Calls[i].StorageChange && !cf.Calls[i].failed() {
			subCallStorageChange = true
		}
	}
	if subCallStorageChange {
		cf.StorageChange = true
	}
}

func childTraceAddress(a []int64, i int64) []int64 {
	child := make([]int64, 0, len(a)+1)
	child = append(child, a...)
	child = append(child, i)
	return child
}

func (t *debankTracer) ToTrace(f *callFrame, traceAddress []int64) dtypes.Trace {
	CallCreateType := ""
	CallType := ""
	switch f.Type {
	case vm.CREATE, vm.CREATE2:
		CallCreateType = strings.ToLower(vm.CREATE.String())
	case vm.SELFDESTRUCT:
		CallCreateType = "suicide"
	case vm.CALL, vm.STATICCALL, vm.CALLCODE, vm.DELEGATECALL:
		CallCreateType = strings.ToLower(vm.CALL.String())
		CallType = strings.ToLower(f.Type.String())
	default:
		CallCreateType = "empty"
	}
	to := common.Address{}
	if f.To != nil {
		to = *f.To
	}
	value := big.NewInt(0)
	if f.Value != nil {
		value = f.Value
	}
	err := ""
	if f.failed() {
		err = f.Error
		if f.RevertReason != "" {
			err = fmt.Sprintf("%s: %s", f.Error, f.RevertReason)
		}
	} else if f.ParentFailed {
		err = "parent call failed"
	}
	return dtypes.Trace{
		ID:                f.TraceID,
		From:              strings.ToLower(f.From.Hex()),
		Gas:               big.NewInt(int64(f.Gas)),
		Input:             f.Input,
		To:                strings.ToLower(to.Hex()),
		Value:             (*hexutil.Big)(value),
		GasUsed:           big.NewInt(int64(f.GasUsed)),
		Output:            f.Output,
		CallCreateType:    CallCreateType,
		CallType:          CallType,
		TxID:              t.hash.Hex(),
		ParentTraceID:     f.ParentTraceID,
		PosInParentTrace:  int64(f.PosInParentTrace),
		SelfStorageChange: f.SelfStorageChange,
		StorageChange:     f.StorageChange,
		Subtraces:         int64(len(f.Calls)),
		TraceAddress:      traceAddress,
		Error:             err,
	}
}

func (t *debankTracer) addTraceAndLog(cf *callFrame, traceAddress []int64) {
	// Emit this frame's own logs and recurse into child calls in EVM execution
	// order, so t.logs accumulates in canonical log-index order (a parent log
	// emitted before a subcall must come before the subcall's logs). cf.Logs and
	// cf.Calls are each already in ascending insertion order; a log's Position and
	// a child's PosInParentTrace are both the (calls+logs) count at insertion time,
	// sharing one coordinate and unique within the frame, so a two-pointer merge by
	// ascending pos reproduces execution order. (A naive "recurse all children then
	// append own logs" walk is post-order and misorders logs vs receipt logIndex.)
	li, ci := 0, 0
	for li < len(cf.Logs) || ci < len(cf.Calls) {
		takeLog := ci >= len(cf.Calls) ||
			(li < len(cf.Logs) && cf.Logs[li].Position < int64(cf.Calls[ci].PosInParentTrace))
		if takeLog {
			cf.Logs[li].ParentTraceID = cf.TraceID
			cf.Logs[li].ID = util.ToHash([]string{cf.Logs[li].ParentTraceID, fmt.Sprintf("%d", cf.Logs[li].Position)})
			if cf.failed() || cf.ParentFailed {
				cf.Logs[li].LogIndex = 0
				t.errorLogs = append(t.errorLogs, cf.Logs[li])
			} else {
				t.logs = append(t.logs, cf.Logs[li])
			}
			li++
		} else {
			cf.Calls[ci].ParentTraceID = cf.TraceID
			cf.Calls[ci].TraceID = util.ToHash([]string{t.hash.Hex(), cf.TraceID, fmt.Sprintf("%d", cf.Calls[ci].PosInParentTrace)})
			t.addTraceAndLog(&cf.Calls[ci], childTraceAddress(traceAddress, int64(ci)))
			ci++
		}
	}
	for i := range cf.Calls {
		if cf.Calls[i].failed() || cf.Calls[i].ParentFailed {
			t.errorTraces = append(t.errorTraces, t.ToTrace(&cf.Calls[i], childTraceAddress(traceAddress, int64(i))))
		} else {
			t.traces = append(t.traces, t.ToTrace(&cf.Calls[i], childTraceAddress(traceAddress, int64(i))))
		}
	}
}

func (t *debankTracer) getStorageAddresses() []string {
	res := make([]string, 0, len(t.storageChanges))
	for address := range t.storageChanges {
		res = append(res, strings.ToLower(address.Hex()))
	}
	return res
}

// finalize propagates failure and storage flags, assigns trace ids and flattens
// the call tree into committed and error traces and logs.
func (t *debankTracer) finalize() {
	if t.done {
		return
	}
	t.done = true
	if len(t.callstack) < 1 {
		return
	}
	setParentFailed(&t.callstack[0], false)
	setStorageChange(&t.callstack[0])
	top := &t.callstack[0]
	top.TraceID = util.ToHash([]string{t.hash.Hex(), "", "0"})
	if top.failed() {
		t.errorTraces = append(t.errorTraces, t.ToTrace(top, []int64{}))
	} else {
		t.traces = append(t.traces, t.ToTrace(top, []int64{}))
	}
	t.addTraceAndLog(top, []int64{})
}

func (t *debankTracer) result() (json.RawMessage, error) {
	t.commitStorageChanges()
	t.finalize()
	if t.entered == 0 || len(t.callstack) != 1 || t.entered != len(t.traces)+len(t.errorTraces) {
		return nil, fmt.Errorf("incomplete call tree: %d entries, %d open frames, %d output frames", t.entered, len(t.callstack), len(t.traces)+len(t.errorTraces))
	}
	return json.Marshal(&dtypes.TraceResult{
		Traces:           t.traces,
		Events:           t.logs,
		ErrorTraces:      t.errorTraces,
		ErrorEvents:      t.errorLogs,
		StorageContracts: t.getStorageAddresses(),
	})
}
