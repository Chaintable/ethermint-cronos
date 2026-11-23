package tracer

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"

	dtypes "github.com/evmos/ethermint/debank/types"
)

// mockStateDB provides the committed post-message storage the tracer re-reads.
type mockStateDB struct {
	state map[common.Address]map[common.Hash]common.Hash
}

func (m *mockStateDB) GetState(a common.Address, s common.Hash) common.Hash { return m.state[a][s] }
func (m *mockStateDB) GetNonce(common.Address) uint64                       { return 0 }
func (m *mockStateDB) GetCode(common.Address) []byte                        { return nil }
func (m *mockStateDB) GetCodeHash(common.Address) common.Hash               { return ethtypes.EmptyCodeHash }
func (m *mockStateDB) Exist(common.Address) bool                            { return true }
func (m *mockStateDB) GetBalance(common.Address) *uint256.Int               { return uint256.NewInt(0) }
func (m *mockStateDB) GetTransientState(common.Address, common.Hash) common.Hash {
	return common.Hash{}
}
func (m *mockStateDB) GetRefund() uint64 { return 0 }

// traceMessage drives one message through the replay tracer and decodes the result.
func traceMessage(t *testing.T, hash common.Hash, sdb tracing.StateDB, drive func(h *tracing.Hooks)) dtypes.TraceResult {
	t.Helper()
	h, finish := NewReplayTracer(hash)
	h.OnTxStart(&tracing.VMContext{StateDB: sdb}, ethtypes.NewTx(&ethtypes.LegacyTx{Gas: 100000}), common.Address{})
	drive(h)
	h.OnTxEnd(&ethtypes.Receipt{}, nil)
	raw, err := finish("")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	var res dtypes.TraceResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return res
}

// TestEventEmissionOrder asserts logs come out in EVM emission order: a parent
// frame emits log_a, calls a child that emits log_b, then emits log_c.
func TestEventEmissionOrder(t *testing.T) {
	from := common.HexToAddress("0x1")
	root := common.HexToAddress("0x2")
	child := common.HexToAddress("0x3")
	addrA := common.HexToAddress("0xaaa")
	addrB := common.HexToAddress("0xbbb")
	addrC := common.HexToAddress("0xccc")
	mkLog := func(a common.Address, idx uint) *ethtypes.Log {
		return &ethtypes.Log{Address: a, Topics: []common.Hash{common.HexToHash("0x1")}, Data: []byte{0x01}, Index: idx}
	}

	res := traceMessage(t, common.HexToHash("0xee"), nil, func(h *tracing.Hooks) {
		h.OnEnter(0, byte(vm.CALL), from, root, nil, 100000, big.NewInt(0))
		h.OnLog(mkLog(addrA, 0))
		h.OnEnter(1, byte(vm.CALL), root, child, nil, 50000, big.NewInt(0))
		h.OnLog(mkLog(addrB, 1))
		h.OnExit(1, nil, 21000, nil, false)
		h.OnLog(mkLog(addrC, 2))
		h.OnExit(0, nil, 50000, nil, false)
	})
	if len(res.Events) != 3 {
		t.Fatalf("want 3 events, got %d", len(res.Events))
	}
	for i, w := range []common.Address{addrA, addrB, addrC} {
		if res.Events[i].Address != strings.ToLower(w.Hex()) {
			t.Errorf("event[%d].contract_id = %s, want %s", i, res.Events[i].Address, strings.ToLower(w.Hex()))
		}
		if res.Events[i].ID == "" || res.Events[i].ParentTraceID == "" {
			t.Errorf("event[%d] id/parent_trace_id must be non-empty", i)
		}
	}
}

// TestCallTree asserts DeBank call-tree ids, positions and frame gas. The root
// keeps the gas the EVM entered and used, as the pipeline call tracer does.
func TestCallTree(t *testing.T) {
	from := common.HexToAddress("0x1")
	to := common.HexToAddress("0x2")
	to2 := common.HexToAddress("0x3")
	res := traceMessage(t, common.HexToHash("0xaa"), nil, func(h *tracing.Hooks) {
		h.OnEnter(0, byte(vm.CALL), from, to, []byte{0x01}, 79000, big.NewInt(0))
		h.OnEnter(1, byte(vm.CALL), to, to2, []byte{0x02}, 50000, big.NewInt(5))
		h.OnExit(1, []byte{0xbb}, 21000, nil, false)
		h.OnExit(0, []byte{0xcc}, 30000, nil, false)
	})
	if len(res.Traces) != 2 || len(res.ErrorTraces) != 0 {
		t.Fatalf("want 2 traces + 0 error traces, got %d + %d", len(res.Traces), len(res.ErrorTraces))
	}
	root, child := res.Traces[0], res.Traces[1]
	if root.Subtraces != 1 {
		t.Errorf("root.Subtraces = %d, want 1", root.Subtraces)
	}
	if root.Gas.Int64() != 79000 || root.GasUsed.Int64() != 30000 {
		t.Errorf("root gas/gasUsed = %d/%d, want 79000/30000", root.Gas.Int64(), root.GasUsed.Int64())
	}
	if root.ID == "" || child.ID == "" {
		t.Errorf("trace ids must be non-empty: root=%q child=%q", root.ID, child.ID)
	}
	if child.ParentTraceID != root.ID {
		t.Errorf("child.ParentTraceID = %q, want root.ID %q", child.ParentTraceID, root.ID)
	}
	if child.PosInParentTrace != 0 {
		t.Errorf("child.PosInParentTrace = %d, want 0", child.PosInParentTrace)
	}
}

// TestRevertedSubcall asserts a reverted subcall lands in error_traces.
func TestRevertedSubcall(t *testing.T) {
	from := common.HexToAddress("0x1")
	to := common.HexToAddress("0x2")
	res := traceMessage(t, common.HexToHash("0xab"), nil, func(h *tracing.Hooks) {
		h.OnEnter(0, byte(vm.CALL), from, to, nil, 100000, big.NewInt(0))
		h.OnEnter(1, byte(vm.CALL), to, to, nil, 50000, big.NewInt(0))
		h.OnExit(1, nil, 21000, vm.ErrExecutionReverted, true)
		h.OnExit(0, nil, 50000, nil, false)
	})
	if len(res.Traces) != 1 || len(res.ErrorTraces) != 1 {
		t.Fatalf("want 1 trace + 1 error trace, got %d + %d", len(res.Traces), len(res.ErrorTraces))
	}
}

// TestStorageContracts keeps only contracts whose committed storage changed.
func TestStorageContracts(t *testing.T) {
	from := common.HexToAddress("0x1")
	changed := common.HexToAddress("0x10")
	reverted := common.HexToAddress("0x20")
	slot := common.HexToHash("0x07")
	sdb := &mockStateDB{state: map[common.Address]map[common.Hash]common.Hash{
		changed:  {slot: common.HexToHash("0x2a")},
		reverted: {slot: common.Hash{}},
	}}
	res := traceMessage(t, common.HexToHash("0xac"), sdb, func(h *tracing.Hooks) {
		h.OnEnter(0, byte(vm.CALL), from, changed, nil, 100000, big.NewInt(0))
		h.OnStorageChange(changed, slot, common.Hash{}, common.HexToHash("0x2a"))
		h.OnStorageChange(reverted, slot, common.Hash{}, common.HexToHash("0x5"))
		h.OnExit(0, nil, 50000, nil, false)
	})
	if len(res.StorageContracts) != 1 || res.StorageContracts[0] != strings.ToLower(changed.Hex()) {
		t.Errorf("storage_contracts = %v, want [%s]", res.StorageContracts, strings.ToLower(changed.Hex()))
	}
}

func TestFailedParentRoutesWholeSubtreeToErrorTraces(t *testing.T) {
	from := common.HexToAddress("0x1")
	rootAddr := common.HexToAddress("0x2")
	failedAddr := common.HexToAddress("0x3")
	successfulDescendantAddr := common.HexToAddress("0x4")
	failedDescendantAddr := common.HexToAddress("0x5")

	res := traceMessage(t, common.HexToHash("0xae"), nil, func(h *tracing.Hooks) {
		h.OnEnter(0, byte(vm.CALL), from, rootAddr, nil, 100000, big.NewInt(0))
		h.OnEnter(1, byte(vm.CALL), rootAddr, failedAddr, nil, 80000, big.NewInt(0))
		h.OnEnter(2, byte(vm.CALL), failedAddr, successfulDescendantAddr, nil, 40000, big.NewInt(0))
		h.OnLog(&ethtypes.Log{Address: successfulDescendantAddr, Index: 7})
		h.OnExit(2, nil, 10000, nil, false)
		h.OnEnter(2, byte(vm.CALL), failedAddr, failedDescendantAddr, nil, 40000, big.NewInt(0))
		h.OnExit(2, nil, 40000, vm.ErrOutOfGas, true)
		h.OnExit(1, nil, 70000, vm.ErrExecutionReverted, true)
		h.OnExit(0, nil, 90000, nil, false)
	})
	if len(res.Traces) != 1 || len(res.ErrorTraces) != 3 {
		t.Fatalf("want 1 trace + 3 error traces, got %d + %d", len(res.Traces), len(res.ErrorTraces))
	}
	findTrace := func(address common.Address) dtypes.Trace {
		want := strings.ToLower(address.Hex())
		for _, trace := range res.ErrorTraces {
			if trace.To == want {
				return trace
			}
		}
		t.Fatalf("error trace to %s not found", want)
		return dtypes.Trace{}
	}
	if got := findTrace(failedAddr).Error; got != vm.ErrExecutionReverted.Error() {
		t.Errorf("failed call error = %q", got)
	}
	if got := findTrace(successfulDescendantAddr).Error; got != "parent call failed" {
		t.Errorf("successful descendant error = %q", got)
	}
	if got := findTrace(failedDescendantAddr).Error; got != vm.ErrOutOfGas.Error() {
		t.Errorf("failed descendant error = %q", got)
	}
	if len(res.Events) != 0 || len(res.ErrorEvents) != 1 {
		t.Errorf("events/error_events = %d/%d, want 0/1", len(res.Events), len(res.ErrorEvents))
	}
}
