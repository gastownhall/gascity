package beadstest

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testpolicy/waiverclock"
)

// ConformanceSkip is a governed opt-out from a conformance subtest. Every skip
// MUST name a tracking bead and carry an expiry, so an opt-out is loud at
// definition (a committed entry), loud over time (it warns ahead of and past
// its expiry, then fails every lane once the waiverclock grace runs out, via
// internal/testpolicy/waiverexpiry), and
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
	{
		Subtest: "ParentIDNamesARowThisStoreDoesNotHave",
		Reason:  "BdStore passes ParentID to bd as --parent, which bd resolves unconditionally, so a parent bd cannot see fails the create instead of being kept as a weak reference",
		BeadID:  "ga-6mfvtl",
		Expiry:  time.Date(2026, time.December, 15, 0, 0, 0, 0, time.UTC),
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

// SkipExpiries returns one dated expiry per ledgered skip, for the fleet waiver
// clock to judge against today (TESTING.md "Waiver expiry clocks"). The date is
// enforced there, in one never-cached check, rather than inside every store's
// conformance run.
func SkipExpiries() []waiverclock.Expiry {
	expiries := make([]waiverclock.Expiry, 0, len(ledgeredSkips))
	for _, s := range ledgeredSkips {
		expiries = append(expiries, waiverclock.Expiry{
			Label:   "conformance skip " + s.Subtest,
			Owner:   s.BeadID,
			Expires: s.Expiry,
			Horizon: maxSkipHorizon,
		})
	}
	return expiries
}

// requireLedgeredSkip skips the named subtest only when a ledger entry governs
// it; otherwise it fails the test loudly. Callers invoke this in place of a
// bare t.Skip so no opt-out can bypass the ledger. It never reads the clock:
// it runs inside every store's conformance suite, whose cached results must not
// depend on the date. SkipExpiries carries the expiry to the one date check.
func requireLedgeredSkip(t *testing.T, subtest string) {
	t.Helper()
	s := lookupSkip(subtest)
	if s == nil {
		t.Fatalf("conformance opt-out for %q is not in the skip ledger; add a ConformanceSkip "+
			"(with a tracking bead and an expiry) to conformance_skips.go before skipping", subtest)
	}
	t.Skipf("skipping %s (bead %s, expires %s): %s", subtest, s.BeadID, s.Expiry.Format("2006-01-02"), s.Reason)
}
