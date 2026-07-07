package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQueryTraceRequestBlockMaxGasRoundTrip guards the hand-added block_max_gas
// field on the trace requests: marshal -> unmarshal must preserve it (and the
// neighbouring fields) so the backend->keeper gRPC hop carries the value.
func TestQueryTraceRequestBlockMaxGasRoundTrip(t *testing.T) {
	tb := &QueryTraceBlockRequest{BlockNumber: 123, ChainId: 25, BlockMaxGas: 40_000_000}
	b, err := tb.Marshal()
	require.NoError(t, err)
	var gotB QueryTraceBlockRequest
	require.NoError(t, gotB.Unmarshal(b))
	require.Equal(t, int64(40_000_000), gotB.BlockMaxGas)
	require.Equal(t, int64(25), gotB.ChainId)
	require.Equal(t, int64(123), gotB.BlockNumber)

	tt := &QueryTraceTxRequest{BlockNumber: 7, ChainId: 25, BlockMaxGas: 60_000_000}
	b2, err := tt.Marshal()
	require.NoError(t, err)
	var gotT QueryTraceTxRequest
	require.NoError(t, gotT.Unmarshal(b2))
	require.Equal(t, int64(60_000_000), gotT.BlockMaxGas)
	require.Equal(t, int64(25), gotT.ChainId)
	require.Equal(t, int64(7), gotT.BlockNumber)

	// zero stays zero (proto3 omits it)
	var zero QueryTraceBlockRequest
	zb, err := (&QueryTraceBlockRequest{BlockNumber: 1}).Marshal()
	require.NoError(t, err)
	require.NoError(t, zero.Unmarshal(zb))
	require.Zero(t, zero.BlockMaxGas)
}
