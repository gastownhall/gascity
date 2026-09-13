package beadstest

import (
	"testing"
	"time"
)

// TestLedgeredSkipsAreValidAndUnexpired is the guard that keeps every
// conformance opt-out honest: each entry must name a bead, carry an expiry that
// has not passed, and not be parked further than maxSkipHorizon out. A skip that
// outlives its fix turns red here, forcing the defect to be fixed or the
// escalation to be renewed.
func TestLedgeredSkipsAreValidAndUnexpired(t *testing.T) {
	now := time.Now()
	for _, s := range ledgeredSkips {
		if s.Subtest == "" || s.Reason == "" || s.BeadID == "" || s.Expiry.IsZero() {
			t.Errorf("incomplete ledger entry (Subtest, Reason, BeadID, Expiry all required): %+v", s)
			continue
		}
		if now.After(s.Expiry) {
			t.Errorf("ledger skip %q expired %s (bead %s) — fix the defect or renew the entry",
				s.Subtest, s.Expiry.Format("2006-01-02"), s.BeadID)
		}
		if s.Expiry.After(now.Add(maxSkipHorizon)) {
			t.Errorf("ledger skip %q expiry %s is more than 90 days out; opt-outs must be short-lived",
				s.Subtest, s.Expiry.Format("2006-01-02"))
		}
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
