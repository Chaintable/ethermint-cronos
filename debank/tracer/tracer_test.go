package tracer

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
	dtypes "github.com/tharsis/ethermint/debank/types"
)

var (
	contractB = common.HexToAddress("0xbb")
	contractC = common.HexToAddress("0xcc")
	// SSTORE(0,1), LOG1(0xaa), CALL 0xbb (logs then reverts), CALL 0xcc
	// (SSTORE(1,2), LOG0), STOP.
	rootCode = common.Hex2Bytes("600160005560aa60006000a1" +
		"6000600060006000600060bb61fffff150" +
		"6000600060006000600060cc61fffff150" + "00")
	codeB = common.Hex2Bytes("60006000a060006000fd")
	codeC = common.Hex2Bytes("600260015560006000a000")
	// LOG0, CALL 0xcc, CALL 0xbb, LOG0, STOP.
	contractD = common.HexToAddress("0xdd")
	codeD     = common.Hex2Bytes("60006000a0" + "6000600060006000600060cc61fffff150" +
		"6000600060006000600060bb61fffff150" + "60006000a0" + "00")
	// CALL 0xdd, LOG1(0xaa), CALL 0xcc, STOP.
	nestedCode = common.Hex2Bytes("6000600060006000600060dd61fffff150" + "60aa60006000a1" +
		"6000600060006000600060cc61fffff150" + "00")
)

func run(t *testing.T, vmError string) (dtypes.TraceResult, Result) {
	return runCode(t, rootCode, vmError)
}

func runCode(t *testing.T, code []byte, vmError string) (dtypes.TraceResult, Result) {
	t.Helper()
	db, err := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	require.NoError(t, err)
	db.SetCode(contractB, codeB)
	db.SetCode(contractC, codeC)
	db.SetCode(contractD, codeD)
	tracer, finish := NewReplayTracer(common.HexToHash("0x01"))
	_, _, err = runtime.Execute(code, nil, &runtime.Config{State: db, GasLimit: 1000000, EVMConfig: vm.Config{Debug: true, Tracer: tracer}})
	require.NoError(t, err)
	res, err := finish(vmError)
	require.NoError(t, err)
	var tr dtypes.TraceResult
	require.NoError(t, json.Unmarshal(res.Trace, &tr))
	return tr, res
}

func TestReplayTracerTree(t *testing.T) {
	tr, res := run(t, "")
	require.Empty(t, res.HookLogs)

	require.Len(t, tr.Traces, 2)
	root, c := tr.Traces[0], tr.Traces[1]
	require.Empty(t, root.TraceAddress)
	require.Equal(t, int64(2), root.Subtraces)
	require.True(t, root.SelfStorageChange)
	require.Equal(t, []int64{1}, c.TraceAddress)
	require.Equal(t, strings.ToLower(contractC.Hex()), c.To)
	require.Equal(t, int64(2), c.PosInParentTrace)
	require.True(t, c.SelfStorageChange)

	require.Len(t, tr.ErrorTraces, 1)
	b := tr.ErrorTraces[0]
	require.Equal(t, []int64{0}, b.TraceAddress)
	require.Equal(t, "execution reverted", b.Error)
	require.Equal(t, int64(1), b.PosInParentTrace)

	require.Len(t, tr.Events, 2)
	require.Equal(t, "0x"+strings.Repeat("0", 62)+"aa", tr.Events[0].Selector)
	require.Equal(t, root.ID, tr.Events[0].ParentTraceID)
	require.Equal(t, c.ID, tr.Events[1].ParentTraceID)
	require.Len(t, tr.ErrorEvents, 1)
	require.Equal(t, b.ID, tr.ErrorEvents[0].ParentTraceID)

	require.ElementsMatch(t, []string{strings.ToLower(common.BytesToAddress([]byte("contract")).Hex()), strings.ToLower(contractC.Hex())}, tr.StorageContracts)
}

func TestReplayTracerRootGasIsExecutionGas(t *testing.T) {
	tr, _ := run(t, "")
	root := tr.Traces[0]
	require.Equal(t, int64(1000000), root.Gas.Int64())
	require.Less(t, root.GasUsed.Int64(), int64(1000000))
	require.Greater(t, root.GasUsed.Int64(), int64(20000))
}

func TestReplayTracerPostProcessingRevert(t *testing.T) {
	tr, res := run(t, "failed to execute post processing")
	require.Empty(t, tr.Traces)
	require.Len(t, tr.ErrorTraces, 3)
	require.Empty(t, tr.Events)
	require.Len(t, tr.ErrorEvents, 3)
	require.Empty(t, tr.StorageContracts)
	// The receipt keeps the two logs the EVM committed, not the reverted call's.
	require.Len(t, res.HookLogs, 2)
	require.Equal(t, strings.ToLower(common.BytesToAddress([]byte("contract")).Hex()), res.HookLogs[0].Address)
	require.Equal(t, strings.ToLower(contractC.Hex()), res.HookLogs[1].Address)
}

func TestRevertTraceMatchesRootFailure(t *testing.T) {
	want, _ := run(t, "execution reverted")
	got, _ := run(t, "")
	require.NoError(t, RevertTrace(&got, "execution reverted"))
	requireSameJSON(t, want, got)

	want, _ = runCode(t, nestedCode, "execution reverted")
	got, _ = runCode(t, nestedCode, "")
	require.Len(t, got.Traces, 4)
	require.Len(t, got.ErrorTraces, 1)
	require.Len(t, got.Events, 5)
	require.Len(t, got.ErrorEvents, 1)
	require.NoError(t, RevertTrace(&got, "execution reverted"))
	requireSameJSON(t, want, got)

	require.ErrorContains(t, RevertTrace(&got, "execution reverted"), "returned root")
}

func requireSameJSON(t *testing.T, want, got dtypes.TraceResult) {
	t.Helper()
	w, err := json.Marshal(want)
	require.NoError(t, err)
	g, err := json.Marshal(got)
	require.NoError(t, err)
	require.Equal(t, string(w), string(g))
}

// A top-level CREATE whose address is taken fails before running any code;
// the trace still has its failed root frame.
func TestReplayTracerRootCreateCollision(t *testing.T) {
	db, err := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	require.NoError(t, err)
	origin := common.HexToAddress("0x0a")
	db.SetNonce(crypto.CreateAddress(origin, 0), 1)
	tracer, finish := NewReplayTracer(common.HexToHash("0x02"))
	_, _, leftOver, err := runtime.Create(common.Hex2Bytes("00"), &runtime.Config{Origin: origin, State: db, GasLimit: 100000, EVMConfig: vm.Config{Debug: true, Tracer: tracer}})
	require.ErrorIs(t, err, vm.ErrContractAddressCollision)
	res, err := finish(err.Error())
	require.NoError(t, err)
	require.Empty(t, res.HookLogs)
	var tr dtypes.TraceResult
	require.NoError(t, json.Unmarshal(res.Trace, &tr))
	require.Empty(t, tr.Traces)
	require.Len(t, tr.ErrorTraces, 1)
	root := tr.ErrorTraces[0]
	require.Empty(t, root.TraceAddress)
	require.Equal(t, "create", root.CallCreateType)
	require.Equal(t, vm.ErrContractAddressCollision.Error(), root.Error)
	require.Equal(t, strings.ToLower(common.Address{}.Hex()), root.To)
	require.Equal(t, int64(100000), root.Gas.Int64())
	require.Equal(t, int64(100000-leftOver), root.GasUsed.Int64())
}

// A top-level CALL whose value exceeds the sender's balance (an EVM message
// run by authz MsgExec skips the ante handler) still yields one failed root frame.
func TestReplayTracerRootInsufficientBalance(t *testing.T) {
	db, err := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	require.NoError(t, err)
	tracer, finish := NewReplayTracer(common.HexToHash("0x03"))
	_, _, err = runtime.Call(contractB, nil, &runtime.Config{State: db, GasLimit: 21000, Value: big.NewInt(7), EVMConfig: vm.Config{Debug: true, Tracer: tracer}})
	require.ErrorIs(t, err, vm.ErrInsufficientBalance)
	res, err := finish(vm.ErrInsufficientBalance.Error())
	require.NoError(t, err)
	var tr dtypes.TraceResult
	require.NoError(t, json.Unmarshal(res.Trace, &tr))
	require.Empty(t, tr.Traces)
	require.Len(t, tr.ErrorTraces, 1)
	require.Equal(t, vm.ErrInsufficientBalance.Error(), tr.ErrorTraces[0].Error)
}
