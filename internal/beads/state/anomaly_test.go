package state_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/state"
)

// TestIsAnomalyPartitionsEveryState pins the anomaly partition to the state
// vocabulary. The renderer used to keep its own copy of this set, so adding a
// state silently lost its marker; deriving it here means a new state must be
// classified as anomaly-or-not in the same file that declares it.
func TestIsAnomalyPartitionsEveryState(t *testing.T) {
	wantAnomaly := map[state.EffectiveState]bool{
		state.StateOrphaned:              true,
		state.StateReadyUnrouted:         true,
		state.StateRoutedStalledDispatch: true,
		state.StateUnknown:               true,
	}
	for _, s := range state.DisplayOrder {
		if got := state.IsAnomaly(s); got != wantAnomaly[s] {
			t.Errorf("IsAnomaly(%q) = %v, want %v", s, got, wantAnomaly[s])
		}
	}
	// Every state the renderer can show must be covered by DisplayOrder, so an
	// anomaly can never be omitted from the report.
	if len(state.DisplayOrder) != 16 {
		t.Errorf("DisplayOrder has %d states, want 16", len(state.DisplayOrder))
	}
	// An unknown value is not an anomaly marker by accident.
	if state.IsAnomaly(state.EffectiveState("not-a-state")) {
		t.Error("IsAnomaly reported true for an unrecognized state")
	}
}
