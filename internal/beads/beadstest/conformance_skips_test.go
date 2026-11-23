package beadstest

import (
	"strings"
	"testing"
	"time"
)

// TestLedgeredSkipsAreWellFormed is the structural guard that keeps every
// conformance opt-out honest: each entry must name its subtest, a reason, a
// bead, and an expiry. It is deliberately date-free so its cached result cannot
// go stale; whether each expiry is acceptable today is judged by
// internal/testpolicy/waiverexpiry, the one never-cached date check.
func TestLedgeredSkipsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range ledgeredSkips {
		if s.Subtest == "" || s.Reason == "" || s.BeadID == "" || s.Expiry.IsZero() {
			t.Errorf("incomplete ledger entry (Subtest, Reason, BeadID, Expiry all required): %+v", s)
		}
		if seen[s.Subtest] {
			t.Errorf("ledger entry %q is duplicated", s.Subtest)
		}
		seen[s.Subtest] = true
	}
}

// TestUnledgeredSubtestHasNoSkip proves the lookup that backs requireLedgeredSkip
// returns nil for any subtest not in the ledger — the condition that makes an
// unledgered opt-out hard-fail instead of silently skipping.
func TestUnledgeredSubtestHasNoSkip(t *testing.T) {
	if got := lookupSkip("NoSuchSubtest"); got != nil {
		t.Fatalf("lookupSkip returned %+v for an unledgered subtest; want nil", got)
	}
	// Every ledger entry must be findable by its own Subtest name.
	for _, s := range ledgeredSkips {
		if lookupSkip(s.Subtest) == nil {
			t.Errorf("ledger entry %q is not findable via lookupSkip", s.Subtest)
		}
	}
}

// TestRequireLedgeredSkipIsDateFree pins that a ledgered skip is honored no
// matter its date. requireLedgeredSkip runs inside every store's conformance
// suite (internal/beads and its consumers), so a clock read there would make
// all of those cached results go stale on the calendar. The expiry is enforced
// once, by internal/testpolicy/waiverexpiry.
func TestRequireLedgeredSkipIsDateFree(t *testing.T) {
	saved := ledgeredSkips
	t.Cleanup(func() { ledgeredSkips = saved })
	ledgeredSkips = []ConformanceSkip{{
		Subtest: "LongLapsed",
		Reason:  "example",
		BeadID:  "ga-example",
		Expiry:  time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
	}}

	var skipped bool
	passed := t.Run("LongLapsed", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		requireLedgeredSkip(t, "LongLapsed")
	})
	if !passed || !skipped {
		t.Fatalf("requireLedgeredSkip on a lapsed entry: passed=%t skipped=%t, want a plain skip", passed, skipped)
	}
}

// TestSkipExpiriesHandsEverySkipToTheClock checks the data the date check
// reads: one expiry per ledgered skip, naming the subtest and its bead, and
// bounded by maxSkipHorizon so an opt-out cannot be parked indefinitely.
func TestSkipExpiriesHandsEverySkipToTheClock(t *testing.T) {
	got := SkipExpiries()
	if len(got) != len(ledgeredSkips) {
		t.Fatalf("SkipExpiries() = %d, want one per ledgered skip (%d)", len(got), len(ledgeredSkips))
	}
	for i, expiry := range got {
		skip := ledgeredSkips[i]
		if expiry.Owner != skip.BeadID || !expiry.Expires.Equal(skip.Expiry) || expiry.Horizon != maxSkipHorizon {
			t.Errorf("SkipExpiries()[%d] = %+v, want owner %s, expiry %s, horizon %s",
				i, expiry, skip.BeadID, skip.Expiry.Format("2006-01-02"), maxSkipHorizon)
		}
		if !strings.Contains(expiry.Label, skip.Subtest) {
			t.Errorf("SkipExpiries()[%d].Label = %q, want the subtest %q named", i, expiry.Label, skip.Subtest)
		}
	}
}
