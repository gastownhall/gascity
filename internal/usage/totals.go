package usage

// Totals is the canonical accumulation of a stream of Facts — the ONE fold
// every consumer shares so surfaces cannot drift: `gc costs` groups it per
// run and the supervisor's /usage endpoint windows it per period, but both
// roll facts through Add.
//
// Cost semantics: a priced fact's CostUSDEstimate accumulates regardless of
// Kind (compute facts carry no cost today, but a runtime that prices
// wall-time must not be silently dropped). An Unpriced fact increments
// Unpriced and never contributes cost, even if a non-conformant record
// carries one — "unpriced" means not measured, and a not-measured number is
// not addable.
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

// Add folds one fact into the totals.
func (t *Totals) Add(f Fact) {
	switch f.Kind {
	case KindModel:
		t.Invocations++
		t.InputTokens += f.InputTokens
		t.OutputTokens += f.OutputTokens
		t.CacheReadTokens += f.CacheReadTokens
		t.CacheCreationTokens += f.CacheCreationTokens
	case KindCompute:
		t.ComputeFacts++
		t.WallSeconds += f.WallSeconds
	}
	if f.Unpriced {
		t.Unpriced++
	} else {
		t.CostUSDEstimate += f.CostUSDEstimate
	}
}
