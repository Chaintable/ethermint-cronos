package tracer

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	eth "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	wire "github.com/evmos/ethermint/debank/types"
)

func TestReplayPostProcessingFailure(t *testing.T) {
	hooks, finish := NewReplayTracer(common.HexToHash("0x1234"))
	a := common.HexToAddress("0x01")
	b := common.HexToAddress("0x02")
	hooks.OnEnter(0, byte(vm.CALL), a, b, nil, 100000, big.NewInt(1))
	hooks.OnEnter(1, byte(vm.CALL), b, a, nil, 30000, big.NewInt(0))
	hooks.OnLog(&eth.Log{Address: a, Index: 3})
	hooks.OnExit(1, nil, 12000, nil, false)
	hooks.OnExit(0, nil, 33000, nil, false)
	raw, err := finish("failed to execute post processing")
	if err != nil {
		t.Fatal(err)
	}
	var r wire.TraceResult
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Traces) != 0 || len(r.ErrorTraces) != 2 || r.ErrorTraces[0].Error != "failed to execute post processing" || r.ErrorTraces[1].Error != "parent call failed" {
		t.Fatalf("unexpected traces %+v / %+v", r.Traces, r.ErrorTraces)
	}
	if len(r.Events) != 0 || len(r.ErrorEvents) != 1 || r.ErrorEvents[0].ParentTraceID != r.ErrorTraces[1].ID || r.ErrorEvents[0].LogIndex != 0 {
		t.Fatalf("misclassified log %+v / %+v", r.Events, r.ErrorEvents)
	}
}
