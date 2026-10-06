package dashboardbff

import "github.com/gastownhall/gascity/internal/testutil"

// hangBudget is the wall-clock ceiling for every pure hang-detector wait in
// this package's tests: a select that waits for a goroutine-signal channel
// (readyCh, doneCh, a started/done marker, ...) and fails the test if that
// signal never arrives.
//
// It is a HANG DETECTOR, not a latency assertion. No test in this package
// waits on it to prove the system is fast — the real assertions always come
// after the wait returns, and nothing waits the budget out on a passing run
// because the channel fires the instant its condition is met. Raising this
// number does not make the suite slower; lowering it does not make it
// stricter. It only changes how long a genuinely wedged test takes to report,
// so a hang fails at the line that wedged instead of taking the whole package
// down via Go's -timeout backstop.
//
// Set to exactly testutil.GoroutineRaceTimeout (1x, no multiplier applied):
// unlike cmd/gc's hangBudget (6x, justified there by measured CPU-starvation
// over-runs recorded against ga-h51wa1), this package has no over-run
// evidence of its own. A test that needs more headroom because of measured
// over-runs should earn it with its own evidence-backed comment, not inherit
// a blanket multiplier from another package.
//
// DO NOT tune hangBudget to make a failing test pass. A test asserting a
// latency bound must keep its own explicit deadline plus a comment naming
// the bound. A test asserting a negative ("nothing arrives within X") must
// likewise keep its own short deadline — hangBudget is the wrong tool there
// and would add unnecessary dead wait time to a passing run.
const hangBudget = testutil.GoroutineRaceTimeout
