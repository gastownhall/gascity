package usage

import (
	"math"
)

// Totals is the canonical accumulation of usage facts shared by the CLI and
// HTTP telemetry surfaces.
type Totals struct {
	Invocations         int
	ComputeFacts        int
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	WallSeconds         float64
	CostUSDEstimate     float64
	Unpriced            int
}

// Add folds one fact into the totals. Unpriced facts retain their token volume
// but never contribute a cost estimate: unknown price is not zero price.
func (t *Totals) Add(f Fact) {
	switch f.Kind {
	case KindModel:
		t.Invocations = saturatingAdd(t.Invocations, 1)
		t.InputTokens = saturatingAdd(t.InputTokens, f.InputTokens)
		t.OutputTokens = saturatingAdd(t.OutputTokens, f.OutputTokens)
		t.CacheReadTokens = saturatingAdd(t.CacheReadTokens, f.CacheReadTokens)
		t.CacheCreationTokens = saturatingAdd(t.CacheCreationTokens, f.CacheCreationTokens)
	case KindCompute:
		t.ComputeFacts = saturatingAdd(t.ComputeFacts, 1)
		if f.WallSeconds >= 0 && !math.IsNaN(f.WallSeconds) && !math.IsInf(f.WallSeconds, 0) {
			t.WallSeconds = saturatingFloatAdd(t.WallSeconds, f.WallSeconds)
		}
	}
	if f.Unpriced {
		t.Unpriced = saturatingAdd(t.Unpriced, 1)
	} else if f.CostUSDEstimate >= 0 && !math.IsNaN(f.CostUSDEstimate) && !math.IsInf(f.CostUSDEstimate, 0) {
		t.CostUSDEstimate = saturatingFloatAdd(t.CostUSDEstimate, f.CostUSDEstimate)
	}
}

func saturatingAdd(current, delta int) int {
	if delta <= 0 {
		return current
	}
	if current > math.MaxInt-delta {
		return math.MaxInt
	}
	return current + delta
}

func saturatingFloatAdd(current, delta float64) float64 {
	if current >= math.MaxFloat64-delta {
		return math.MaxFloat64
	}
	return current + delta
}
