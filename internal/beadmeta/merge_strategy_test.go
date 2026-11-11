package beadmeta

import "testing"

func TestIsKnownMergeStrategy(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		want     bool
	}{
		{"direct", MergeStrategyDirect, true},
		{"mr", MergeStrategyMR, true},
		{"local", MergeStrategyLocal, true},
		{"empty is unset, not a strategy", "", false},
		{"unknown value", "rebase", false},
		{"pr alias is not accepted here", "pr", false},
		{"case sensitive", "MR", false},
		{"untrimmed", " mr ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsKnownMergeStrategy(tt.strategy); got != tt.want {
				t.Errorf("IsKnownMergeStrategy(%q) = %v, want %v", tt.strategy, got, tt.want)
			}
		})
	}
}

// TestKnownMergeStrategiesCoversEveryConstant keeps the slice — which callers
// render into "valid values are ..." error messages — in step with the
// constants. A new strategy constant that never lands in the slice would be
// rejected by every validator that consults it.
func TestKnownMergeStrategiesCoversEveryConstant(t *testing.T) {
	for _, strategy := range []string{MergeStrategyDirect, MergeStrategyMR, MergeStrategyLocal} {
		if !IsKnownMergeStrategy(strategy) {
			t.Errorf("constant %q is missing from KnownMergeStrategies", strategy)
		}
	}
	if len(KnownMergeStrategies) != 3 {
		t.Errorf("KnownMergeStrategies has %d entries, want 3 — add the new constant to this test too", len(KnownMergeStrategies))
	}
}
