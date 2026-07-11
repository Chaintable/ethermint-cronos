package keeper

import (
	"testing"

	"github.com/evmos/ethermint/x/evm/types"
)

func TestScaleGasSat(t *testing.T) {
	cases := []struct{ original, factor, want uint64 }{
		{1000, 2, 2000},
		{1000, 4, 4000},
		{guidanceGasCap, 2, guidanceGasCap},       // at cap -> saturates
		{guidanceGasCap/2 + 1, 2, guidanceGasCap}, // just over cap/2 -> saturates
		{^uint64(0), 4, guidanceGasCap},           // would overflow uint64 -> saturates, no wrap
	}
	for _, c := range cases {
		if got := scaleGasSat(c.original, c.factor); got != c.want {
			t.Errorf("scaleGasSat(%d,%d)=%d want %d", c.original, c.factor, got, c.want)
		}
	}
}

func TestParseGuidance(t *testing.T) {
	// absent/empty config -> no guidance, no error
	if g, err := parseGuidance(nil); g != nil || err != nil {
		t.Fatalf("nil config: got %v, %v", g, err)
	}
	if g, err := parseGuidance(&types.TraceConfig{}); g != nil || err != nil {
		t.Fatalf("empty config: got %v, %v", g, err)
	}

	// valid envelope -> parsed
	valid := `{"debankConsensusGuidance":[{"failed":true,"logs":[]},` +
		`{"failed":false,"logs":[{"address":"0xabc","topics":["0x1"],"data":"0x"}]}]}`
	g, err := parseGuidance(&types.TraceConfig{TracerJsonConfig: valid})
	if err != nil {
		t.Fatalf("valid config errored: %v", err)
	}
	if len(g) != 2 || !g[0].Failed || g[1].Failed || len(g[1].Logs) != 1 {
		t.Fatalf("valid config parsed wrong: %+v", g)
	}

	// valid JSON without the guidance key (a legit non-guidance tracer config) -> nil, no error
	if g, err := parseGuidance(&types.TraceConfig{TracerJsonConfig: `{"withLog":true}`}); g != nil || err != nil {
		t.Fatalf("non-guidance config: got %v, %v", g, err)
	}

	// non-empty but unparseable -> error, not a silent nil fallback
	if _, err := parseGuidance(&types.TraceConfig{TracerJsonConfig: `{not json`}); err == nil {
		t.Fatal("invalid JSON should error, got nil")
	}
}
