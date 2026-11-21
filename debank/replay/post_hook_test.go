package replay

import (
	"errors"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/tharsis/ethermint/debank/tracer"
	"github.com/tharsis/ethermint/x/evm/keeper"
	evm "github.com/tharsis/ethermint/x/evm/types"
)

// The raw mainnet ABCI response retains logs after its Cosmos hook failed.
// Model a nested VM revert reusing a log index and a successful child call.
func TestHistoricalPostProcessingReceipt(t *testing.T) {
	b, err := os.ReadFile("testdata/post-hook-5542610.pb")
	if err != nil {
		t.Fatal(err)
	}
	var receipt evm.MsgEthereumTxResponse
	if err = receipt.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Logs) != 2 || receipt.VmError != evm.ErrPostTxProcessing.Error() {
		t.Fatal("unexpected mainnet fixture")
	}
	h := common.HexToHash(receipt.Hash)
	tr := tracer.NewExecutionTracer(h)
	a := common.HexToAddress("0x01")
	tr.CaptureEnter(vm.CALL, a, a, nil, 900000, nil)
	tr.CaptureEnter(vm.CALL, a, a, nil, 60000, nil)
	tr.OnLog(receipt.Logs[0].ToEthereum())
	tr.CaptureExit(nil, 100, errors.New("nested failure"))
	tr.CaptureEnter(vm.CALL, a, a, nil, 60000, nil)
	tr.OnLog(receipt.Logs[0].ToEthereum())
	tr.CaptureExit(nil, 100, nil)
	for _, l := range receipt.Logs[1:] {
		tr.OnLog(l.ToEthereum())
	}
	tr.CaptureEnd(receipt.Ret, receipt.GasUsed, 0, nil)
	raw, err := tr.Result(nil, receipt.VmError)
	if err != nil {
		t.Fatal(err)
	}
	obs := keeper.ReplayTransaction{Hash: h, GasUsed: receipt.GasUsed, RootOutput: tr.RootOutput, VmError: receipt.VmError, Trace: raw, PreRollbackEvents: tr.PreRollbackEvents}
	out, err := checkMessage(obs, &receipt)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Traces) != 0 || len(out.Events) != 0 || len(out.ErrorTraces) != 3 || len(out.ErrorEvents) != len(receipt.Logs)+1 || out.ErrorTraces[0].Error != receipt.VmError {
		t.Fatalf("incorrect rollback: %d live frames, %d live logs, %d failed frames, %d failed logs", len(out.Traces), len(out.Events), len(out.ErrorTraces), len(out.ErrorEvents))
	}
	receipt.Logs[0].Index++
	if _, err := checkMessage(obs, &receipt); err == nil {
		t.Fatal("accepted a changed receipt log index")
	}
}
