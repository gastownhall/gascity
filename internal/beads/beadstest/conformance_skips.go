package beadstest

import (
	"testing"
	"time"
)

// ConformanceSkip is a governed opt-out from a conformance subtest. Every skip
// MUST name a tracking bead and carry an expiry, so an opt-out is loud at
// definition (a committed entry), loud over time (it expires and then
// hard-fails), and impossible to add silently. This is the anti-rot mechanism
// that keeps a known defect from quietly laundering a real regression behind a
// green suite.
type ConformanceSkip struct {
	// Subtest is the exact t.Run name this skip applies to.
	Subtest string
	// Reason explains why a conforming Store cannot pass the subtest today.
	Reason string
	// BeadID is the REQUIRED tracking bead/issue (e.g. "ga-1234").
	BeadID string
	// Expiry is the REQUIRED date past which the skip hard-fails. Keep it close
	// (<= maxSkipHorizon out): an opt-out is a temporary escalation, not a
	// permanent exemption.
	Expiry time.Time
}

// maxSkipHorizon bounds how far in the future a skip's expiry may be set, so an
// opt-out cannot be parked indefinitely by pushing the date out years.
const maxSkipHorizon = 90 * 24 * time.Hour

// ledgeredSkips is the committed registry of every allowed conformance opt-out.
// Adding a skip requires an entry here; there are none today.
var ledgeredSkips = []ConformanceSkip{}

// lookupSkip returns the ledger entry governing a subtest, or nil if none.
func lookupSkip(subtest string) *ConformanceSkip {
	for i := range ledgeredSkips {
		if ledgeredSkips[i].Subtest == subtest {
			return &ledgeredSkips[i]
		}
	}
	return nil
}

// requireLedgeredSkip skips the named subtest only when a valid, unexpired
// ledger entry governs it; otherwise it fails the test loudly. Callers invoke
// this in place of a bare t.Skip so no opt-out can bypass the ledger.
func requireLedgeredSkip(t *testing.T, subtest string) {
	t.Helper()
	s := lookupSkip(subtest)
	if s == nil {
		t.Fatalf("conformance opt-out for %q is not in the skip ledger; add a ConformanceSkip "+
			"(with a tracking bead and an expiry) to conformance_skips.go before skipping", subtest)
	}
	if time.Now().After(s.Expiry) {
		t.Fatalf("conformance skip for %q expired %s (bead %s); fix the underlying defect or renew the ledger entry",
			subtest, s.Expiry.Format("2006-01-02"), s.BeadID)
	}
	t.Skipf("skipping %s (bead %s, expires %s): %s", subtest, s.BeadID, s.Expiry.Format("2006-01-02"), s.Reason)
}
