package tracer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	evmtypes "github.com/tharsis/ethermint/x/evm/types"
	"time"

	"github.com/holiman/uint256"
	dtypes "github.com/tharsis/ethermint/debank/types"
	"github.com/tharsis/ethermint/debank/util"
)

const Name = "ExecutionTracer"

// callFrame mirrors the cosmos-evm (TAC) debank tracer call frame verbatim;
// these fields are part of the DeBank wire contract.
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

// ExecutionTracer records calls and StateDB writes during one canonical Ethereum message.
type ExecutionTracer struct {
	// The old keeper retains successful VM logs in its ABCI response even
	// when a later Cosmos hook discards the VM cache.
	PreRollbackEvents []dtypes.Event
	// RootOutput is captured before call-frame error formatting.
	RootOutput []byte
	evmtypes.NoOpTracer
	callstack []callFrame

	entered int

	hash common.Hash

	traces      []dtypes.Trace
	logs        []dtypes.Event
	errorTraces []dtypes.Trace
	errorLogs   []dtypes.Event

	// discovery sets (raw addr/slot); committed values re-read at OnTxEnd.
	touchedStorage  map[common.Address]map[common.Hash]common.Hash // addr -> slot -> pre-tx value (first prev)
	touchedAccounts map[common.Address]struct{}                    // accounts with a nonce/code change

	// committed state diff, built by commitStateDiff at OnTxEnd.
	storageChanges  map[common.Address]struct{} // contracts with a committed storage change
	DeletedAccounts map[common.Hash]struct{}
	NewAccounts     map[common.Hash]*dtypes.NewAccount
	StorageDiff     map[common.Hash]map[common.Hash][]byte
	NewCodes        map[common.Hash][]byte

	committed bool // commitStateDiff ran (OnTxEnd)
	done      bool // finalize ran (GetResult)
}

// NewExecutionTracer creates a tracer bound to a single transaction.
func NewExecutionTracer(hash common.Hash) *ExecutionTracer {
	return &ExecutionTracer{
		hash:            hash,
		traces:          make([]dtypes.Trace, 0),
		logs:            make([]dtypes.Event, 0),
		errorTraces:     make([]dtypes.Trace, 0),
		errorLogs:       make([]dtypes.Event, 0),
		touchedStorage:  make(map[common.Address]map[common.Hash]common.Hash),
		touchedAccounts: make(map[common.Address]struct{}),
		storageChanges:  make(map[common.Address]struct{}),
		DeletedAccounts: make(map[common.Hash]struct{}),
		NewAccounts:     make(map[common.Hash]*dtypes.NewAccount),
		StorageDiff:     make(map[common.Hash]map[common.Hash][]byte),
		NewCodes:        make(map[common.Hash][]byte),
	}
}

func (t *ExecutionTracer) CaptureStart(env *vm.EVM, from, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	op := vm.CALL
	if create {
		op = vm.CREATE
	}
	t.CaptureEnter(op, from, to, input, gas, value)
}
func (t *ExecutionTracer) CaptureEnd(output []byte, gasUsed uint64, _ time.Duration, err error) {
	t.RootOutput = common.CopyBytes(output)
	t.captureEnd(output, gasUsed, err, err != nil)
}
func (t *ExecutionTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	t.entered++
	if value != nil {
		value = new(big.Int).Set(value)
	}
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

func (t *ExecutionTracer) CaptureExit(output []byte, gasUsed uint64, err error) {

	size := len(t.callstack)
	if size <= 1 {
		return
	}
	// Pop call.
	call := t.callstack[size-1]
	t.callstack = t.callstack[:size-1]
	size -= 1

	call.GasUsed = gasUsed
	call.processOutput(output, err, err != nil)

	call.PosInParentTrace = len(t.callstack[size-1].Calls) + len(t.callstack[size-1].Logs)
	t.callstack[size-1].Calls = append(t.callstack[size-1].Calls, call)
}

func (t *ExecutionTracer) captureEnd(output []byte, gasUsed uint64, err error, reverted bool) {
	if len(t.callstack) != 1 {
		return
	}
	t.callstack[0].GasUsed = gasUsed
	t.callstack[0].processOutput(output, err, reverted)
}

func (t *ExecutionTracer) OnLog(log *ethtypes.Log) {
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

// --- contract state-diff channel: On*Change hooks DISCOVER touched
// slots/accounts; committed values are re-read in commitStateDiff at OnTxEnd ---

func (t *ExecutionTracer) OnStorageChange(addr common.Address, slot, prev common.Hash) {
	slots := t.touchedStorage[addr]
	if slots == nil {
		slots = make(map[common.Hash]common.Hash)
		t.touchedStorage[addr] = slots
	}
	if _, seen := slots[slot]; !seen {
		slots[slot] = prev // pre-tx value, to drop slots with no net committed change
	}
	// mark the executing frame (callstack top) as having changed storage
	if n := len(t.callstack); n > 0 {
		t.callstack[n-1].SelfStorageChange = true
		t.callstack[n-1].StorageChange = true
	}
}

func (t *ExecutionTracer) OnAccountChange(addr common.Address) { t.touchedAccounts[addr] = struct{}{} }

// commitStateDiff builds the committed state diff from the post-execution StateDB
// (reverts already applied), so reverted writes/creates are excluded. Storage is
// emitted only when the committed value differs from the pre-tx value; an account
// that no longer exists becomes a DeletedAccount, otherwise its committed
// nonce/code (Balance left 0, filled by the bank channel) becomes a NewAccount.
func (t *ExecutionTracer) commitStateDiff(stateDB vm.StateDB) {
	if t.committed {
		return
	}
	t.committed = true

	for addr, slots := range t.touchedStorage {
		addrhash := crypto.Keccak256Hash(addr.Bytes())
		for slot, pre := range slots {
			committed := stateDB.GetState(addr, slot)
			if committed == pre {
				continue // reverted or no net change this tx
			}
			if t.StorageDiff[addrhash] == nil {
				t.StorageDiff[addrhash] = make(map[common.Hash][]byte)
			}
			t.StorageDiff[addrhash][crypto.Keccak256Hash(slot.Bytes())] = committed.Bytes()
			t.storageChanges[addr] = struct{}{}
		}
	}

	for addr := range t.touchedAccounts {
		addrhash := crypto.Keccak256Hash(addr.Bytes())
		if !stateDB.Exist(addr) {
			t.DeletedAccounts[addrhash] = struct{}{}
			continue
		}
		code := stateDB.GetCode(addr)
		codeHash := stateDB.GetCodeHash(addr)
		t.NewAccounts[addrhash] = &dtypes.NewAccount{
			Address:  addrhash,
			Balance:  new(uint256.Int),
			Nonce:    stateDB.GetNonce(addr),
			CodeHash: codeHash,
		}
		if len(code) > 0 {
			t.NewCodes[codeHash] = code
		}
	}
}

// --- DeBank semantics (verbatim from cosmos-evm tracer; hook-agnostic) ---

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

func (t *ExecutionTracer) ToTrace(f *callFrame, traceAddress []int64) dtypes.Trace {
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

func (t *ExecutionTracer) addTraceAndLog(cf *callFrame, traceAddress []int64) {
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

func (t *ExecutionTracer) ToStorageDiff() dtypes.TransactionStateDiff {
	stateDiff := dtypes.TransactionStateDiff{}
	for hash := range t.DeletedAccounts {
		stateDiff.DeletedAccounts = append(stateDiff.DeletedAccounts, hash)
	}
	for _, account := range t.NewAccounts {
		stateDiff.NewAccounts = append(stateDiff.NewAccounts, *account)
	}
	for account, storage := range t.StorageDiff {
		values := make([]dtypes.IndexValuePair, 0, len(storage))
		for index, v := range storage {
			value := new(uint256.Int)
			if len(v) > 0 {
				value = new(uint256.Int).SetBytes(v)
			}
			values = append(values, dtypes.IndexValuePair{
				Index: index,
				Value: value,
			})
		}
		stateDiff.StorageDiff = append(stateDiff.StorageDiff, dtypes.AccountStorageDiff{
			Address: account,
			Values:  values,
		})
	}
	for hash, code := range t.NewCodes {
		stateDiff.NewCodes = append(stateDiff.NewCodes, dtypes.NewCode{
			CodeHash: hash,
			Code:     code,
		})
	}
	return stateDiff
}

func (t *ExecutionTracer) getStorageAddresses() []string {
	res := make([]string, 0, len(t.storageChanges))
	for address := range t.storageChanges {
		res = append(res, strings.ToLower(address.Hex()))
	}
	return res
}

// finalize runs the DeBank post-tx assembly (cosmos-evm CaptureTxEnd): propagate
// failure/storage flags, assign trace ids, and flatten the call tree into the
// traces/logs (and their error variants). The per-tx Transaction is NOT built
// here — Cronos's standard hooks cannot supply nonce/gasPrice/status, so it is
// assembled in the RPC layer from the real tx.
func (t *ExecutionTracer) finalize() {
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

// vmEvents excludes subcalls reverted inside the VM. OnLog records the
// historical block log index before the keeper rolls back its cache.
func vmEvents(f *callFrame, events []dtypes.Event) []dtypes.Event {
	if f.failed() {
		return events
	}
	events = append(events, f.Logs...)
	for i := range f.Calls {
		events = vmEvents(&f.Calls[i], events)
	}
	return events
}

func (t *ExecutionTracer) Result(stateDB vm.StateDB, vmError string) (json.RawMessage, error) {
	if vmError != "" && len(t.callstack) == 1 && !t.callstack[0].failed() {
		t.PreRollbackEvents = vmEvents(&t.callstack[0], nil)
		sort.SliceStable(t.PreRollbackEvents, func(i, j int) bool {
			return t.PreRollbackEvents[i].LogIndex < t.PreRollbackEvents[j].LogIndex
		})
		t.callstack[0].Error = vmError
	} else {
		t.commitStateDiff(stateDB)
	}
	t.finalize()
	if t.entered == 0 || len(t.callstack) != 1 || t.entered != len(t.traces)+len(t.errorTraces) {
		return nil, fmt.Errorf("incomplete call tree: %d entries, %d open frames, %d output frames", t.entered, len(t.callstack), len(t.traces)+len(t.errorTraces))
	}
	result := &dtypes.TraceResult{
		Traces:           t.traces,
		Events:           t.logs,
		ErrorTraces:      t.errorTraces,
		ErrorEvents:      t.errorLogs,
		StateDiff:        t.ToStorageDiff(),
		StorageContracts: t.getStorageAddresses(),
	}
	return json.Marshal(result)
}
