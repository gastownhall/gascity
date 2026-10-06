package credentialprovider

import "github.com/gastownhall/gascity/internal/testutil"

// hangBudget is the single wall-clock ceiling shared by every test-side wait in
// this package, covering both in-process goroutine waits (cache_test.go) and
// subprocess-lifecycle waits (credentialprovider_process_{unix,windows}_test.go).
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
