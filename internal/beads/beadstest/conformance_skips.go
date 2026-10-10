package beadstest

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testpolicy/waiverclock"
)

// ConformanceSkip is a governed opt-out from a conformance subtest. Every skip
// MUST name a tracking bead and carry an expiry, so an opt-out is loud at
// definition (a committed entry), loud over time (it warns ahead of and past
// its expiry, then fails every lane once the waiverclock grace runs out), and
// impossible to add silently. This is the anti-rot mechanism
// that keeps a known defect from quietly laundering a real regression behind a
// green suite.
type ConformanceSkip struct {
	// Subtest is the exact t.Run name this skip applies to.
	Subtest string
	// Reason explains why a conforming Store cannot pass the subtest today and
	// names the replacement proof that retires the skip (TESTING.md).
	Reason string
	// BeadID is the REQUIRED tracking bead/issue (e.g. "ga-1234").
	BeadID string
	// Expiry is the REQUIRED last day the skip is valid. waiverclock decides
	// what a lapse costs: it warns from WarnAhead before, warns through Grace
	// after, then fails every lane. Keep it close (<= maxSkipHorizon out): an
	// opt-out is a temporary escalation, not a permanent exemption.
	Expiry time.Time
}

// maxSkipHorizon bounds how far in the future a skip's expiry may be set, so an
// opt-out cannot be parked indefinitely by pushing the date out years.
const maxSkipHorizon = 90 * 24 * time.Hour

// ledgeredSkips is the committed registry of every allowed conformance opt-out.
// Adding a skip requires an entry here.
var ledgeredSkips = []ConformanceSkip{
	{
		Subtest: readyParitySubtest,
		Reason: "MemStore and FileStore Ready block on a missing or foreign blocker and on a closed " +
			"blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a " +
			"ready projection, and return insertion order with Limit applied mid-scan instead of the " +
			"canonical (priority, created_at, id) order. Replacement proof: MemStore implements " +
			"enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance " +
			"and TestFileStoreReadyParityConformance run this suite unwaived",
		BeadID: "ga-gmf8r",
		Expiry: time.Date(2026, time.December, 15, 0, 0, 0, 0, time.UTC),
	},
}

// lookupSkip returns the ledger entry governing a subtest, or nil if none.
func lookupSkip(subtest string) *ConformanceSkip {
	for i := range ledgeredSkips {
		if ledgeredSkips[i].Subtest == subtest {
			return &ledgeredSkips[i]
		}
	}
	return nil
}

// skipClock reports what a skip's expiry costs at now under mode, through the
// one fleet clock every dated test-policy waiver uses (TESTING.md "Waiver
// expiry clocks").
func skipClock(s ConformanceSkip, now time.Time, mode waiverclock.Mode) waiverclock.Report {
	return waiverclock.Check([]waiverclock.Expiry{{
		Label:   "conformance skip " + s.Subtest,
		Owner:   s.BeadID,
		Expires: s.Expiry,
	}}, now, mode)
}

// requireLedgeredSkip skips the named subtest only when a ledger entry governs
// it and the waiver clock tolerates its expiry; otherwise it fails the test
// loudly. Callers invoke this in place of a bare t.Skip so no opt-out can
// bypass the ledger.
func requireLedgeredSkip(t *testing.T, subtest string) {
	t.Helper()
	s := lookupSkip(subtest)
	if s == nil {
		t.Fatalf("conformance opt-out for %q is not in the skip ledger; add a ConformanceSkip "+
			"(with a tracking bead and an expiry) to conformance_skips.go before skipping", subtest)
	}
	mode, err := waiverclock.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	clock := skipClock(*s, time.Now(), mode)
	for _, fatal := range clock.Fatal {
		t.Fatal(fatal)
	}
	for _, warning := range clock.Warnings {
		t.Log(warning)
	}
	t.Skipf("skipping %s (bead %s, expires %s): %s", subtest, s.BeadID, s.Expiry.Format("2006-01-02"), s.Reason)
}
