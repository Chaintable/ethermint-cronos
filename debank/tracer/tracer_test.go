package tracer

import (
	"encoding/json"
	"math/big"
	"testing"

	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/holiman/uint256"
)

// mockStateDB implements tracing.StateDB to provide the committed post-tx state
// that the tracer re-reads in commitStateDiff.
type mockStateDB struct {
	state map[common.Address]map[common.Hash]common.Hash
	nonce map[common.Address]uint64
	code  map[common.Address][]byte
	exist map[common.Address]bool
}

func (m *mockStateDB) GetState(a common.Address, s common.Hash) common.Hash { return m.state[a][s] }
func (m *mockStateDB) GetNonce(a common.Address) uint64                     { return m.nonce[a] }
func (m *mockStateDB) GetCode(a common.Address) []byte                      { return m.code[a] }
func (m *mockStateDB) GetCodeHash(a common.Address) common.Hash {
	if c := m.code[a]; len(c) > 0 {
		return crypto.Keccak256Hash(c)
	}
	return ethtypes.EmptyCodeHash
}
func (m *mockStateDB) Exist(a common.Address) bool                               { return m.exist[a] }
func (m *mockStateDB) GetBalance(common.Address) *uint256.Int                    { return uint256.NewInt(0) }
func (m *mockStateDB) GetTransientState(common.Address, common.Hash) common.Hash { return common.Hash{} }
func (m *mockStateDB) GetRefund() uint64                                         { return 0 }

func newTestTracer(t *testing.T, txHash common.Hash) *tracers.Tracer {
	t.Helper()
	tr, err := newDebankTracer(&tracers.Context{TxHash: txHash, TxIndex: 0}, nil, nil)
	if err != nil {
		t.Fatalf("newDebankTracer: %v", err)
	}
	return tr
}

// TestCallTree drives a root call with one nested subcall and asserts the
// DeBank call-tree semantics (ids, pos_in_parent_trace, subtraces, gas).
func TestCallTree(t *testing.T) {
	txHash := common.HexToHash("0xaa")
	from := common.HexToAddress("0x1")
	to := common.HexToAddress("0x2")
	to2 := common.HexToAddress("0x3")
	tr := newTestTracer(t, txHash)
	h := tr.Hooks

	h.OnTxStart(nil, ethtypes.NewTx(&ethtypes.LegacyTx{Gas: 100000}), from)
	h.OnEnter(0, byte(vm.CALL), from, to, []byte{0x01}, 100000, big.NewInt(0))
	h.OnEnter(1, byte(vm.CALL), to, to2, []byte{0x02}, 50000, big.NewInt(5))
	h.OnExit(1, []byte{0xbb}, 21000, nil, false)
	h.OnExit(0, []byte{0xcc}, 50000, nil, false)
	h.OnTxEnd(&ethtypes.Receipt{GasUsed: 60000}, nil)

	raw, err := tr.GetResult()
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	var res dtypes.TraceResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(res.Traces) != 2 {
		t.Fatalf("want 2 traces, got %d", len(res.Traces))
	}
	if len(res.ErrorTraces) != 0 {
		t.Fatalf("want 0 error traces, got %d", len(res.ErrorTraces))
	}
	root, child := res.Traces[0], res.Traces[1]
	if root.Subtraces != 1 {
		t.Errorf("root.Subtraces = %d, want 1", root.Subtraces)
	}
	if root.GasUsed.Int64() != 60000 {
		t.Errorf("root.GasUsed = %d, want 60000 (receipt overrides EVM gas)", root.GasUsed.Int64())
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
	txHash := common.HexToHash("0xab")
	from := common.HexToAddress("0x1")
	to := common.HexToAddress("0x2")
	tr := newTestTracer(t, txHash)
	h := tr.Hooks

	h.OnTxStart(nil, ethtypes.NewTx(&ethtypes.LegacyTx{Gas: 100000}), from)
	h.OnEnter(0, byte(vm.CALL), from, to, nil, 100000, big.NewInt(0))
	h.OnEnter(1, byte(vm.CALL), to, to, nil, 50000, big.NewInt(0))
	h.OnExit(1, nil, 21000, vm.ErrExecutionReverted, true)
	h.OnExit(0, nil, 50000, nil, false)
	h.OnTxEnd(&ethtypes.Receipt{GasUsed: 60000}, nil)

	raw, _ := tr.GetResult()
	var res dtypes.TraceResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Traces) != 1 || len(res.ErrorTraces) != 1 {
		t.Fatalf("want 1 trace + 1 error trace, got %d + %d", len(res.Traces), len(res.ErrorTraces))
	}
}

// TestStateDiff asserts the contract state-diff channel and that no native
// balance leaks through the EVM hooks (Balance stays 0; bank channel fills it).
func TestStateDiff(t *testing.T) {
	txHash := common.HexToHash("0xac")
	from := common.HexToAddress("0x1")
	storeAddr := common.HexToAddress("0x10")
	codeAddr := common.HexToAddress("0x20")
	nonceAddr := common.HexToAddress("0x30")
	deadAddr := common.HexToAddress("0x40")
	slot := common.HexToHash("0x07")
	val := common.HexToHash("0x2a") // 42
	code := []byte{0x60, 0x00}
	codeHash := crypto.Keccak256Hash(code)

	// committed post-tx state the tracer re-reads in commitStateDiff
	sdb := &mockStateDB{
		state: map[common.Address]map[common.Hash]common.Hash{storeAddr: {slot: val}},
		nonce: map[common.Address]uint64{nonceAddr: 5},
		code:  map[common.Address][]byte{codeAddr: code},
		exist: map[common.Address]bool{storeAddr: true, codeAddr: true, nonceAddr: true, deadAddr: false},
	}

	tr := newTestTracer(t, txHash)
	h := tr.Hooks
	h.OnTxStart(&tracing.VMContext{StateDB: sdb}, ethtypes.NewTx(&ethtypes.LegacyTx{Gas: 100000}), from)
	h.OnEnter(0, byte(vm.CALL), from, storeAddr, nil, 100000, big.NewInt(0))
	h.OnStorageChange(storeAddr, slot, common.Hash{}, val) // pre=0, committed=42 -> emitted
	h.OnCodeChange(codeAddr, common.Hash{}, nil, codeHash, code)
	h.OnNonceChangeV2(nonceAddr, 0, 5, 0)
	h.OnCodeChange(deadAddr, codeHash, code, ethtypes.EmptyCodeHash, nil) // exist=false -> deleted
	h.OnExit(0, nil, 50000, nil, false)
	h.OnTxEnd(&ethtypes.Receipt{GasUsed: 60000}, nil)

	raw, _ := tr.GetResult()
	var res dtypes.TraceResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sd := res.StateDiff

	// storage diff: keccak(addr) -> keccak(slot) -> val
	if len(sd.StorageDiff) != 1 {
		t.Fatalf("want 1 storage-diff account, got %d", len(sd.StorageDiff))
	}
	if sd.StorageDiff[0].Address != crypto.Keccak256Hash(storeAddr.Bytes()) {
		t.Errorf("storage addr hash mismatch")
	}
	if len(sd.StorageDiff[0].Values) != 1 ||
		sd.StorageDiff[0].Values[0].Index != crypto.Keccak256Hash(slot.Bytes()) ||
		sd.StorageDiff[0].Values[0].Value.Uint64() != 42 {
		t.Errorf("storage value mismatch: %+v", sd.StorageDiff[0].Values)
	}

	// new code
	if len(sd.NewCodes) != 1 || sd.NewCodes[0].CodeHash != codeHash {
		t.Errorf("new code mismatch: %+v", sd.NewCodes)
	}

	// deleted account
	if len(sd.DeletedAccounts) != 1 || sd.DeletedAccounts[0] != crypto.Keccak256Hash(deadAddr.Bytes()) {
		t.Errorf("deleted account mismatch: %+v", sd.DeletedAccounts)
	}

	// new accounts: code change + nonce change -> 2 entries, all balance 0
	if len(sd.NewAccounts) != 2 {
		t.Fatalf("want 2 new accounts (code+nonce), got %d", len(sd.NewAccounts))
	}
	var sawNonce bool
	for _, a := range sd.NewAccounts {
		if a.Balance == nil || a.Balance.Sign() != 0 {
			t.Errorf("native balance must not leak via EVM hooks: addr=%s balance=%v", a.Address, a.Balance)
		}
		if a.Address == crypto.Keccak256Hash(nonceAddr.Bytes()) {
			sawNonce = true
			if a.Nonce != 5 {
				t.Errorf("nonce = %d, want 5", a.Nonce)
			}
		}
	}
	if !sawNonce {
		t.Errorf("nonce account not found in new accounts")
	}

	// storage_contracts includes the contract whose storage changed
	if len(res.StorageContracts) != 1 {
		t.Errorf("want 1 storage contract, got %v", res.StorageContracts)
	}
}

// TestRevertedStateDropped asserts that writes/creates rolled back by a revert do
// not leak into the state diff: the committed StateDB shows the slot back at its
// pre-tx value and the created account as non-existent.
func TestRevertedStateDropped(t *testing.T) {
	txHash := common.HexToHash("0xad")
	from := common.HexToAddress("0x1")
	addr := common.HexToAddress("0x10")
	created := common.HexToAddress("0x50")
	slot := common.HexToHash("0x07")

	// committed: slot reverted to its pre-tx value (0); created contract not committed
	sdb := &mockStateDB{
		state: map[common.Address]map[common.Hash]common.Hash{addr: {slot: common.Hash{}}},
		nonce: map[common.Address]uint64{created: 1},
		exist: map[common.Address]bool{addr: true, created: false},
	}

	tr := newTestTracer(t, txHash)
	h := tr.Hooks
	h.OnTxStart(&tracing.VMContext{StateDB: sdb}, ethtypes.NewTx(&ethtypes.LegacyTx{Gas: 100000}), from)
	h.OnEnter(0, byte(vm.CALL), from, addr, nil, 100000, big.NewInt(0))
	h.OnStorageChange(addr, slot, common.Hash{}, common.HexToHash("0x5")) // wrote 5, then reverted
	h.OnNonceChangeV2(created, 0, 1, 0)                                    // created a contract, then reverted
	h.OnExit(0, nil, 50000, nil, false)
	h.OnTxEnd(&ethtypes.Receipt{GasUsed: 60000}, nil)

	raw, _ := tr.GetResult()
	var res dtypes.TraceResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sd := res.StateDiff

	if len(sd.StorageDiff) != 0 {
		t.Errorf("reverted storage write must be dropped (committed == pre), got %+v", sd.StorageDiff)
	}
	for _, a := range sd.NewAccounts {
		if a.Address == crypto.Keccak256Hash(created.Bytes()) {
			t.Errorf("reverted-create account must not be a NewAccount")
		}
	}
}
