package tracer

import (
	"testing"

	"github.com/ethereum/go-ethereum/eth/tracers"
)

// TestRegistered confirms init() registered the tracer under its name so the
// keeper can select it via TraceConfig{Tracer: "debankTracer"}.
func TestRegistered(t *testing.T) {
	tr, err := tracers.DefaultDirectory.New(Name, &tracers.Context{}, nil, nil)
	if err != nil {
		t.Fatalf("DefaultDirectory.New(%q): %v", Name, err)
	}
	if tr == nil || tr.Hooks == nil {
		t.Fatalf("tracer or hooks nil")
	}
	if tr.Hooks.OnBalanceChange != nil {
		t.Errorf("OnBalanceChange must NOT be registered (native balance goes through the bank channel)")
	}
	if tr.Hooks.OnStorageChange == nil || tr.Hooks.OnEnter == nil {
		t.Errorf("expected OnStorageChange and OnEnter hooks to be set")
	}
}
