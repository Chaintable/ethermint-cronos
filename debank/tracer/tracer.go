package tracer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/vm"
	dtypes "github.com/tharsis/ethermint/debank/types"
	"github.com/tharsis/ethermint/debank/util"
)

// callFrame is one EVM call frame of the DeBank trace.
type callFrame struct {
	Type         vm.OpCode
	From         common.Address
	Gas          uint64
	GasUsed      uint64
	To           *common.Address
	Input        []byte
	Output       []byte
	Error        string
	RevertReason string
	ParentFailed bool
	Calls        []callFrame
	Logs         []dtypes.Event

	PosInParentTrace int
	ParentTraceID    string
	// static frames and their children cannot write state or emit logs.
	static            bool
	TraceID           string
	StorageChange     bool
	SelfStorageChange bool

	Value *big.Int
}

func (f callFrame) failed() bool {
	return len(f.Error) > 0
}

func (f *callFrame) processOutput(output []byte, err error) {
	output = common.CopyBytes(output)
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
// contracts of one signed EVM message. The pinned VM has no log or storage
// hooks; LOG and SSTORE are read in CaptureState, which the interpreter calls
// before the opcode's own write protection check, so they are skipped in a
// static frame where they fail.
type debankTracer struct {
	hash      common.Hash
	callstack []callFrame
	entered   int
	stateDB   vm.StateDB

	traces      []dtypes.Trace
	logs        []dtypes.Event
	errorTraces []dtypes.Trace
	errorLogs   []dtypes.Event

	touchedStorage map[common.Address]map[common.Hash]common.Hash // addr -> slot -> pre-message value
	storageChanges map[common.Address]struct{}

	committed bool
	done      bool
}

var _ vm.EVMLogger = (*debankTracer)(nil)

// Result is the finished trace of one message. HookLogs holds the logs the EVM
// committed when Cosmos post-processing then reverted the message; the receipt
// still carries them.
type Result struct {
	Trace    json.RawMessage
	HookLogs []dtypes.Event
}

// NewReplayTracer traces one signed message. The returned function finalizes
// the result after Cosmos post-processing; a non-empty vmError on a root that
// the EVM completed marks a post-processing revert.
func NewReplayTracer(hash common.Hash) (vm.EVMLogger, func(vmError string) (Result, error)) {
	t := &debankTracer{
		hash:           hash,
		traces:         make([]dtypes.Trace, 0),
		logs:           make([]dtypes.Event, 0),
		errorTraces:    make([]dtypes.Trace, 0),
		errorLogs:      make([]dtypes.Event, 0),
		touchedStorage: make(map[common.Address]map[common.Hash]common.Hash),
		storageChanges: make(map[common.Address]struct{}),
	}
	return t, func(vmError string) (Result, error) {
		var res Result
		if vmError != "" && len(t.callstack) == 1 && t.callstack[0].Error == "" {
			res.HookLogs = committedLogs(&t.callstack[0])
			t.callstack[0].Error = vmError
			t.committed = true
			t.storageChanges = map[common.Address]struct{}{}
		}
		trace, err := t.result()
		res.Trace = trace
		return res, err
	}
}

func (t *debankTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	t.stateDB = env.StateDB
	typ := vm.CALL
	if create {
		typ = vm.CREATE
	}
	t.CaptureEnter(typ, from, to, input, gas, value)
}

func (t *debankTracer) CaptureEnd(output []byte, gasUsed uint64, _ time.Duration, err error) {
	if len(t.callstack) != 1 {
		return
	}
	t.callstack[0].GasUsed = gasUsed
	t.callstack[0].processOutput(output, err)
	// The root frame has returned and been reverted if it failed.
	t.commitStorageChanges()
}

func (t *debankTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	t.entered++
	static := typ == vm.STATICCALL
	if n := len(t.callstack); n > 0 && t.callstack[n-1].static {
		static = true
	}
	toCopy := to
	var v *big.Int
	if value != nil {
		v = new(big.Int).Set(value)
	}
	t.callstack = append(t.callstack, callFrame{
		Type:   typ,
		From:   from,
		To:     &toCopy,
		Input:  common.CopyBytes(input),
		Gas:    gas,
		Value:  v,
		static: static,
	})
}

func (t *debankTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	size := len(t.callstack)
	if size <= 1 {
		return
	}
	call := t.callstack[size-1]
	t.callstack = t.callstack[:size-1]
	size--

	call.GasUsed = gasUsed
	call.processOutput(output, err)

	parent := &t.callstack[size-1]
	call.PosInParentTrace = len(parent.Calls) + len(parent.Logs)
	parent.Calls = append(parent.Calls, call)
}

func (t *debankTracer) CaptureState(_ uint64, op vm.OpCode, _, _ uint64, scope *vm.ScopeContext, _ []byte, _ int, err error) {
	if err != nil || len(t.callstack) == 0 || t.callstack[len(t.callstack)-1].static {
		return
	}
	switch {
	case op == vm.SSTORE:
		addr := scope.Contract.Address()
		slot := common.Hash(scope.Stack.Back(0).Bytes32())
		slots := t.touchedStorage[addr]
		if slots == nil {
			slots = make(map[common.Hash]common.Hash)
			t.touchedStorage[addr] = slots
		}
		if _, seen := slots[slot]; !seen {
			slots[slot] = t.stateDB.GetState(addr, slot)
		}
		top := &t.callstack[len(t.callstack)-1]
		top.SelfStorageChange = true
		top.StorageChange = true
	case op >= vm.LOG0 && op <= vm.LOG4:
		stack := scope.Stack
		offset, size := stack.Back(0), stack.Back(1)
		n := int(op - vm.LOG0)
		var selector string
		topics := make([]string, 0, n)
		for i := 0; i < n; i++ {
			topic := common.Hash(stack.Back(2 + i).Bytes32()).Hex()
			if i == 0 {
				selector = topic
			} else {
				topics = append(topics, topic)
			}
		}
		top := &t.callstack[len(t.callstack)-1]
		top.Logs = append(top.Logs, dtypes.Event{
			Address:  strings.ToLower(scope.Contract.Address().Hex()),
			Selector: selector,
			Topics:   topics,
			Data:     scope.Memory.GetCopy(int64(offset.Uint64()), int64(size.Uint64())),
			Position: int64(len(top.Calls) + len(top.Logs)),
		})
	}
}

func (t *debankTracer) CaptureFault(uint64, vm.OpCode, uint64, uint64, *vm.ScopeContext, int, error) {
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

// committedLogs returns, in execution order, the logs of frames that did not
// fail in the EVM.
func committedLogs(cf *callFrame) []dtypes.Event {
	if cf.failed() {
		return nil
	}
	var out []dtypes.Event
	li, ci := 0, 0
	for li < len(cf.Logs) || ci < len(cf.Calls) {
		if ci >= len(cf.Calls) || (li < len(cf.Logs) && cf.Logs[li].Position < int64(cf.Calls[ci].PosInParentTrace)) {
			out = append(out, cf.Logs[li])
			li++
		} else {
			out = append(out, committedLogs(&cf.Calls[ci])...)
			ci++
		}
	}
	return out
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
	return append(child, i)
}

func (t *debankTracer) toTrace(f *callFrame, traceAddress []int64) dtypes.Trace {
	callCreateType := ""
	callType := ""
	switch f.Type {
	case vm.CREATE, vm.CREATE2:
		callCreateType = strings.ToLower(vm.CREATE.String())
	case vm.SELFDESTRUCT:
		callCreateType = "suicide"
	case vm.CALL, vm.STATICCALL, vm.CALLCODE, vm.DELEGATECALL:
		callCreateType = strings.ToLower(vm.CALL.String())
		callType = strings.ToLower(f.Type.String())
	default:
		callCreateType = "empty"
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
		Gas:               new(big.Int).SetUint64(f.Gas),
		Input:             f.Input,
		To:                strings.ToLower(to.Hex()),
		Value:             (*hexutil.Big)(value),
		GasUsed:           new(big.Int).SetUint64(f.GasUsed),
		Output:            f.Output,
		CallCreateType:    callCreateType,
		CallType:          callType,
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

// addTraceAndLog emits a frame's logs and child calls in execution order: a
// log's Position and a child's PosInParentTrace share one counter per frame.
func (t *debankTracer) addTraceAndLog(cf *callFrame, traceAddress []int64) {
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
			t.errorTraces = append(t.errorTraces, t.toTrace(&cf.Calls[i], childTraceAddress(traceAddress, int64(i))))
		} else {
			t.traces = append(t.traces, t.toTrace(&cf.Calls[i], childTraceAddress(traceAddress, int64(i))))
		}
	}
}

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
		t.errorTraces = append(t.errorTraces, t.toTrace(top, []int64{}))
	} else {
		t.traces = append(t.traces, t.toTrace(top, []int64{}))
	}
	t.addTraceAndLog(top, []int64{})
}

func (t *debankTracer) result() (json.RawMessage, error) {
	t.commitStorageChanges()
	t.finalize()
	if t.entered == 0 || len(t.callstack) != 1 || t.entered != len(t.traces)+len(t.errorTraces) {
		return nil, fmt.Errorf("incomplete call tree: %d entries, %d open frames, %d output frames", t.entered, len(t.callstack), len(t.traces)+len(t.errorTraces))
	}
	storage := make([]string, 0, len(t.storageChanges))
	for address := range t.storageChanges {
		storage = append(storage, strings.ToLower(address.Hex()))
	}
	return sonic.ConfigStd.Marshal(&dtypes.TraceResult{
		Traces:           t.traces,
		Events:           t.logs,
		ErrorTraces:      t.errorTraces,
		ErrorEvents:      t.errorLogs,
		StorageContracts: storage,
	})
}
