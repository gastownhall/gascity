package workertest

import "github.com/gastownhall/gascity/internal/testutil"

// hangBudget is the ceiling on post-hoc elapsed-time bounds in this package
// (checked after the subprocess exits) that absorb a real subprocess's own
// OS-scheduling and file-I/O overhead rather than in-process application
// logic. Two bounds in phase2_fake_worker_test.go use it:
// fakeStartupPostControlOverhead, which bounds the gap between the
// standalone fake worker's control_observed and state_transition event
// timestamps (ga-e4bhca), and fakeInteractionSignalBound, which bounds a
// whole interaction run from exec to exit (ga-4pxoo4r).
//
// It is a HANG DETECTOR, not a latency assertion. No test in this package
// waits on it to prove the system is fast: the real assertions (state
// content, event ordering, event identity) never depend on how long the
// run took. Raising this number does not make a passing run slower, and
// lowering it does not make the suite stricter — it only changes how late
// the bounded transition or run may finish before the result reports Fail.
// A fake worker that never exits is caught by the `go test -timeout`, not
// by this bound.
//
// DO NOT tune hangBudget to make a failing test pass. A test that needs to
// assert a real latency bound must keep its own explicit deadline plus a
// comment saying which bound it asserts.
//
// Matches the cmd/gc/hangbudget_test.go reference implementation
// (hangBudget = 6 * testutil.GoroutineRaceTimeout), keeping one source of
// truth for the multiplier across packages: the GoroutineRaceTimeout base
// deliberately matches cmd/gc. TESTING.md's "Test deadline rule" makes
// GoroutineRaceTimeout / ExecRaceTimeout the MINIMUM safe deadline for a
// timer racing a goroutine or subprocess start ("must be >= 10s"); this
// package's fake-worker runs are a real subprocess (os/exec), so
// ExecRaceTimeout (currently equal to GoroutineRaceTimeout) is the floor
// TestPhase2StartupOutcomeBoundStaysAHangDetector and
// TestPhase2InteractionSignalBoundStaysAHangDetector check.
const hangBudget = 6 * testutil.GoroutineRaceTimeout
