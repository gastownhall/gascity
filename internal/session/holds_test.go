package session

import (
	"testing"
	"time"
)

// TestHoldVerdict: CONTRACT v5.9 D8 7(a)'s one hold predicate over each hold
// shape, its runtime running or not. A hold blocks a consume unless the row
// is not suspended and its runtime runs; an unparseable timer blocks (O6);
// legacy's own intents hold nothing. Kills a shape read from the wrong key and
// a verdict that ignores the runtime or the suspended state.
func TestHoldVerdict(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		meta             map[string]string
		running, stopped bool // the verdict with the runtime running, and stopped
	}{
		"none":                 {map[string]string{"sleep_intent": "idle-stop-pending"}, false, false},
		"user":                 {map[string]string{"sleep_intent": "user-hold"}, false, true},
		"user, suspended":      {map[string]string{"sleep_intent": "user-hold", "state": "suspended"}, true, true},
		"timer":                {map[string]string{"held_until": "2099-01-01T00:00:00Z"}, false, true},
		"expired timer":        {map[string]string{"held_until": "2000-01-01T00:00:00Z"}, false, false},
		"quarantine":           {map[string]string{"quarantined_until": "2099-01-01T00:00:00Z"}, false, true},
		"wait":                 {map[string]string{"wait_hold": "true"}, false, true},
		"suspended":            {map[string]string{"state": "suspended"}, true, true},
		"unparseable":          {map[string]string{"held_until": "soon", "quarantined_until": "x"}, false, true},
		"unparseable and wait": {map[string]string{"held_until": "soon", "wait_hold": "true"}, false, true},
	} {
		if got := HoldVerdict(tc.meta, true, now); got != tc.running {
			t.Errorf("%s, running: HoldVerdict = %v, want %v", name, got, tc.running)
		}
		if got := HoldVerdict(tc.meta, false, now); got != tc.stopped {
			t.Errorf("%s, stopped: HoldVerdict = %v, want %v", name, got, tc.stopped)
		}
	}
}
