package api

import "github.com/gastownhall/gascity/internal/testutil"

// hangBudget is the single wall-clock ceiling shared by every test-side wait in
// this package that is a pure hang detector, including waits that bound an
// entire streaming subtest's context (e.g. an SSE handler goroutine driven via
// context.WithTimeout) as well as single-event polls
// (waitForRecorderSubstring, channel receives guarded by time.After).
//
// It is a HANG DETECTOR, not a latency assertion. No test in this package
// waits on it to prove the system is fast: the real assertions always come
// after the wait returns, or on the non-timeout branch of a select. Raising
// this number does not make a passing run slower, and lowering it does not
// make the suite stricter — it only changes how long a genuinely wedged test
// takes to report. Streaming subtests that use hangBudget as a context
// deadline additionally call cancel() explicitly once their assertions are
// done; the deadline is a last-resort backstop for an unanticipated hang, not
// the mechanism that ends a passing run. The production handler underneath
// (streamSessionTranscriptHistoryStructured and peers) does not distinguish
// context.DeadlineExceeded from context.Canceled — both take the same
// `case <-ctx.Done(): return` branch — so the exact magnitude of this budget
// has no bearing on behavior under test.
//
// DO NOT tune hangBudget to make a failing test pass. A test that needs to
// assert a latency bound must keep its own explicit deadline plus a comment
// saying which bound it asserts, or be written as a benchmark. A test that
// needs to assert a negative ("nothing arrives within X") must likewise keep
// its own short deadline — hangBudget is the wrong tool there and would add
// nearly a minute of dead wait.
//
// RELATIONSHIP TO testutil.GoroutineRaceTimeout — these are not competing
// constants, which is why hangBudget is derived from GoroutineRaceTimeout
// rather than declared independently, matching the cmd/gc/hangbudget_test.go
// reference implementation (and internal/credentialprovider/hangbudget_test.go)
// and keeping one source of truth across the migration program. TESTING.md's
// "Test deadline rule" makes GoroutineRaceTimeout the MINIMUM safe deadline
// for a timer racing a goroutine ("must be >= 10s"). hangBudget is the point
// at which this package declares a wedge, which is a ceiling, not a floor.
// Use GoroutineRaceTimeout directly when a test needs a deadline that
// satisfies the rule; use hangBudget when the wait is purely a hang detector
// and no assertion depends on how long it took.
const hangBudget = 6 * testutil.GoroutineRaceTimeout
