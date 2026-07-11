package usage

import "testing"

func TestTotalsAddFoldsModelAndComputeFacts(t *testing.T) {
	var tot Totals
	tot.Add(Fact{
		Kind: KindModel, Model: "m",
		InputTokens: 100, OutputTokens: 50,
		CacheReadTokens: 20, CacheCreationTokens: 10,
		CostUSDEstimate: 0.25,
	})
	tot.Add(Fact{Kind: KindCompute, Runtime: "local", WallSeconds: 12.5})

	want := Totals{
		Invocations: 1, ComputeFacts: 1,
		InputTokens: 100, OutputTokens: 50,
		CacheReadTokens: 20, CacheCreationTokens: 10,
		WallSeconds: 12.5, CostUSDEstimate: 0.25,
	}
	if tot != want {
		t.Errorf("Totals = %+v, want %+v", tot, want)
	}
}

func TestTotalsAddUnpricedFactCountsButNeverAddsCost(t *testing.T) {
	var tot Totals
	// A conformant unpriced fact carries zero cost; a NON-conformant one
	// carrying a cost anyway must still not pollute the priced total —
	// "unpriced" means not measured, and a not-measured number is not
	// addable. This is the semantic gc costs always had; the /usage
	// endpoint now shares it.
	tot.Add(Fact{Kind: KindModel, Model: "mystery", InputTokens: 5, Unpriced: true, CostUSDEstimate: 9.99})

	if tot.Unpriced != 1 {
		t.Errorf("Unpriced = %d, want 1", tot.Unpriced)
	}
	if tot.CostUSDEstimate != 0 {
		t.Errorf("CostUSDEstimate = %v, want 0 (unpriced cost must not accumulate)", tot.CostUSDEstimate)
	}
	if tot.InputTokens != 5 || tot.Invocations != 1 {
		t.Errorf("tokens/invocations = %d/%d, want 5/1 (tokens are kept even when unpriced)", tot.InputTokens, tot.Invocations)
	}
}

func TestTotalsAddAcceptsPricedCostFromAnyKind(t *testing.T) {
	var tot Totals
	// Compute facts carry no cost today, but if a runtime ever prices
	// wall-time the canonical fold must not silently drop it.
	tot.Add(Fact{Kind: KindCompute, WallSeconds: 1, CostUSDEstimate: 0.05})
	if tot.CostUSDEstimate != 0.05 {
		t.Errorf("CostUSDEstimate = %v, want 0.05", tot.CostUSDEstimate)
	}
}
