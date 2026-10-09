package session

import (
	"testing"
	"time"
)

// TestHolds: the hold kernel reads each operator intent once, keeps an
// unparseable timer as Unknown (not held), and ignores legacy's own
// intents. Kills a bit read from the wrong key and Unknown counted as held.
func TestHolds(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		meta        map[string]string
		in, unknown Hold
	}{
		"none":          {meta: map[string]string{"sleep_intent": "idle-stop-pending"}},
		"user":          {meta: map[string]string{"sleep_intent": "user-hold"}, in: HoldUser},
		"timer":         {meta: map[string]string{"held_until": "2099-01-01T00:00:00Z"}, in: HoldTimer},
		"expired timer": {meta: map[string]string{"held_until": "2000-01-01T00:00:00Z"}},
		"quarantine":    {meta: map[string]string{"quarantined_until": "2099-01-01T00:00:00Z"}, in: HoldQuarantine},
		"wait":          {meta: map[string]string{"wait_hold": "true"}, in: HoldWait},
		"suspended":     {meta: map[string]string{"state": "suspended"}, in: HoldSuspended},
		"unparseable":   {meta: map[string]string{"held_until": "soon", "quarantined_until": "x"}, unknown: HoldTimer | HoldQuarantine},
	} {
		if got := Holds(tc.meta, now); got.In != tc.in || got.Unknown != tc.unknown || got.OperatorHeld() != (tc.in != 0) {
			t.Errorf("%s: Holds = %+v, want in %b unknown %b", name, got, tc.in, tc.unknown)
		}
	}
}
