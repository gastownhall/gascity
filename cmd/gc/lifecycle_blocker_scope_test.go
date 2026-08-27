package main

import (
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func blockerScopeRows(now time.Time) []struct {
	name string
	info sessionpkg.Info
	// wantFull is what the unnarrowed ladder reports; wantTimed is what the
	// narrowed forms report. They differ on exactly one value.
	wantFull  string
	wantTimed string
} {
	future := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Hour).Format(time.RFC3339)
	return []struct {
		name      string
		info      sessionpkg.Info
		wantFull  string
		wantTimed string
	}{
		{name: "clear", info: sessionpkg.Info{}},
		{name: "hold", info: sessionpkg.Info{HeldUntil: future}, wantFull: "user_hold", wantTimed: "user_hold"},
		{name: "quarantine", info: sessionpkg.Info{QuarantinedUntil: future}, wantFull: "quarantine", wantTimed: "quarantine"},
		{name: "expired hold", info: sessionpkg.Info{HeldUntil: past}},
		{name: "pinned", info: sessionpkg.Info{PinAwake: "true"}, wantFull: "pinned"},
		{name: "pinned with padding", info: sessionpkg.Info{PinAwake: " true "}, wantFull: "pinned"},
		{
			// The timed blockers are checked first, so a row carrying both
			// reports the timed one and the narrowing loses nothing.
			name: "pinned and held", info: sessionpkg.Info{PinAwake: "true", HeldUntil: future},
			wantFull: "user_hold", wantTimed: "user_hold",
		},
	}
}

// TestWakeAndMaxAgeBlockerScopesAgree pins the claim wakeBlockerInfo's doc
// comment makes: it is the SAME narrowing main applies for the age timer, kept
// under its own name because the reason differs. Two names for one restriction
// is only safe while they cannot drift apart, and this is what stops them.
func TestWakeAndMaxAgeBlockerScopesAgree(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range blockerScopeRows(now) {
		t.Run(test.name, func(t *testing.T) {
			if got := lifecycleTimerBlockerInfo(test.info, now); got != test.wantFull {
				t.Fatalf("lifecycleTimerBlockerInfo = %q, want %q", got, test.wantFull)
			}
			wake := wakeBlockerInfo(test.info, now)
			if wake != test.wantTimed {
				t.Fatalf("wakeBlockerInfo = %q, want %q", wake, test.wantTimed)
			}
			if age := maxSessionAgeBlockerInfo(test.info, now); age != wake {
				t.Fatalf("maxSessionAgeBlockerInfo = %q but wakeBlockerInfo = %q; the two narrowings have drifted", age, wake)
			}
		})
	}
}

// TestDeadlineTimerBlockerScopeSplitsByTimer pins the one asymmetry the whole
// bug came from: the same row, the same instant, two different answers
// depending on which timer is asking.
func TestDeadlineTimerBlockerScopeSplitsByTimer(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range blockerScopeRows(now) {
		t.Run(test.name, func(t *testing.T) {
			if got := deadlineTimerBlockerInfo(test.info, now, false); got != test.wantFull {
				t.Fatalf("idle-timer blocker = %q, want the full ladder's %q", got, test.wantFull)
			}
			if got := deadlineTimerBlockerInfo(test.info, now, true); got != test.wantTimed {
				t.Fatalf("age-timer blocker = %q, want the narrowed %q", got, test.wantTimed)
			}
		})
	}
}
