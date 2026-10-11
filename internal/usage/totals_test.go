package usage

import (
	"math"
	"testing"
)

func TestTotalsAddSaturatesIntegerCounters(t *testing.T) {
	totals := Totals{InputTokens: math.MaxInt - 2}
	totals.Add(Fact{Kind: KindModel, InputTokens: 10})
	if totals.InputTokens != math.MaxInt {
		t.Fatalf("InputTokens = %d, want saturated MaxInt", totals.InputTokens)
	}
}

func TestTotalsAddSaturatesFiniteFloatCounters(t *testing.T) {
	totals := Totals{
		WallSeconds:     math.MaxFloat64,
		CostUSDEstimate: math.MaxFloat64,
	}
	totals.Add(Fact{
		Kind:            KindCompute,
		WallSeconds:     math.MaxFloat64,
		CostUSDEstimate: math.MaxFloat64,
	})
	if totals.WallSeconds != math.MaxFloat64 {
		t.Fatalf("WallSeconds = %v, want saturated MaxFloat64", totals.WallSeconds)
	}
	if totals.CostUSDEstimate != math.MaxFloat64 {
		t.Fatalf("CostUSDEstimate = %v, want saturated MaxFloat64", totals.CostUSDEstimate)
	}
}
