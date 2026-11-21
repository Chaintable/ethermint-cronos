package tracer

import (
	"github.com/ethereum/go-ethereum/eth/tracers"
)

// Register the debank tracer in the global tracer directory so the keeper can
// select it by name via tracers.DefaultDirectory.New (grpc_query.go) when a
// TraceConfig{Tracer: "debankTracer"} is supplied. No keeper patch is needed.
func init() {
	tracers.DefaultDirectory.Register(Name, newDebankTracer, false)
}
