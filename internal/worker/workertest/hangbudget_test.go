package workertest

import "github.com/gastownhall/gascity/internal/testutil"

// hangBudget is the wall-clock ceiling for test-side waits in this package
// that race a real subprocess's own OS-scheduling and file-I/O overhead
// rather than in-process application logic — currently
// fakeStartupPostControlOverhead in phase2_fake_worker_test.go, which bounds
// the gap between the standalone fake worker's control_observed and
// state_transition events (ga-e4bhca).
//
// It is a HANG DETECTOR, not a latency assertion. No test in this package
// waits on it to prove the system is fast: the real assertions (state
// content, event ordering, event identity) always run first and
// independently of how long the wait took. Raising this number does not
// make a passing run slower, and lowering it does not make the suite
// stricter — it only changes how long a genuinely wedged fake-worker
// subprocess takes to report as failed.
//
// DO NOT tune hangBudget to make a failing test pass. A test that needs to
// assert a real latency bound must keep its own explicit deadline plus a
// comment saying which bound it asserts.
//
// Matches the cmd/gc/hangbudget_test.go reference implementation
// (hangBudget = 6 * testutil.GoroutineRaceTimeout), keeping one source of
// truth for the multiplier across packages. TESTING.md's "Test deadline
// rule" makes GoroutineRaceTimeout / ExecRaceTimeout the MINIMUM safe
// deadline for a timer racing a goroutine or subprocess start ("must be >=
// 10s"); this package's fake-worker runs are a real subprocess
// (os/exec), so ExecRaceTimeout is the applicable floor.
const hangBudget = 6 * testutil.GoroutineRaceTimeout
