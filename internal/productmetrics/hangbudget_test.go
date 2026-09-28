//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

// hangBudget is the single wall-clock ceiling shared by every test-side wait in
// this package, covering both in-process goroutine waits (control_unix_test.go,
// spawn_unix_test.go, uploader_unix_test.go) and subprocess-lifecycle waits
// (marker_protocol_unix_test.go, spool_unix_test.go, storage_unix_test.go).
//
// It is a HANG DETECTOR, not a latency assertion. No test in this package waits
// on it to prove the system is fast: the real assertions always come after the
// wait returns. Raising this number does not make a passing run slower, and
// lowering it does not make the suite stricter — it only changes how long a
// genuinely wedged test takes to report.
//
// DO NOT tune hangBudget to make a failing test pass. A test that needs to
// assert a latency bound must keep its own explicit deadline plus a comment
// saying which bound it asserts, or be written as a benchmark. A test that
// needs to assert a negative ("nothing arrives within X") must likewise keep
// its own short deadline — hangBudget is the wrong tool there and would add
// nearly a minute of dead wait.
//
// RELATIONSHIP TO testutil.GoroutineRaceTimeout / testutil.ExecRaceTimeout —
// these are not competing constants, which is why hangBudget is derived from
// GoroutineRaceTimeout rather than declared independently, matching the
// cmd/gc/hangbudget_test.go reference implementation and keeping one source of
// truth. TESTING.md's "Test deadline rule" makes GoroutineRaceTimeout /
// ExecRaceTimeout the MINIMUM safe deadline for a timer racing a goroutine or
// subprocess start ("must be >= 10s"). hangBudget is the point at which this
// package declares a wedge, which is a ceiling, not a floor. Use the testutil
// floor directly when a test needs a deadline that satisfies the rule; use
// hangBudget when the wait is purely a hang detector and no assertion depends
// on how long it took.
const hangBudget = 6 * testutil.GoroutineRaceTimeout

// TestHangBudgetStaysAHangDetector guards the constant against exactly the
// change that would quietly reintroduce flake risk: shrinking it to "make the
// suite faster". Shrinking it cannot make a passing suite faster — every wait
// hangBudget bounds returns on its own condition — it can only convert load
// spikes back into red builds.
//
// Three floors are asserted. The first two are TESTING.md's "Test deadline
// rule" applied to both base constants this package replaced call sites of.
// They are equal today (10s each), but are checked independently in case
// they diverge later — this package genuinely uses both, unlike a package
// that only ever raced goroutines or only ever raced subprocesses.
//
// The third is this migration's own evidence: the largest deadline any site
// in this package replaced was spool_unix_test.go's 4*testutil.ExecRaceTimeout
// (40s, bounding TestSpoolDeepPurgeConvergesUnderLowFileDescriptorLimit and
// TestSpoolNestedPurgeConvergesAtMinimumDirectoryBudget's re-exec helpers).
// Those same call sites now use 4*hangBudget, not a bare hangBudget, so the
// check below is expressed against that same multiplied shape — checking
// bare hangBudget against 2x this 40s deadline would demand 80s of a budget
// no such site actually needs, since the x4 multiplier applies identically
// on both sides of the conversion at every call site in this package.
func TestHangBudgetStaysAHangDetector(t *testing.T) {
	t.Parallel()

	if hangBudget < testutil.GoroutineRaceTimeout {
		t.Fatalf("hangBudget = %s, want >= testutil.GoroutineRaceTimeout (%s); "+
			"TESTING.md's test deadline rule is a floor this package may not go below",
			hangBudget, testutil.GoroutineRaceTimeout)
	}
	if hangBudget < testutil.ExecRaceTimeout {
		t.Fatalf("hangBudget = %s, want >= testutil.ExecRaceTimeout (%s); "+
			"TESTING.md's test deadline rule is a floor this package may not go below",
			hangBudget, testutil.ExecRaceTimeout)
	}

	const largestReplacedDeadline = 4 * testutil.ExecRaceTimeout
	if 4*hangBudget < 2*largestReplacedDeadline {
		t.Fatalf("4*hangBudget = %s, want >= %s (twice the largest deadline this package replaced, "+
			"at the same x4 multiplier its call sites actually use); "+
			"it is a hang detector, not a latency assertion — do not tune it to make a test pass",
			4*hangBudget, 2*largestReplacedDeadline)
	}
}
