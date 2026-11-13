package events

import "github.com/gastownhall/gascity/internal/testutil"

// hangBudget is the wall-clock ceiling for hang-detector waits in this
// package's tests: a watcher read (drainWatcher) or a rotation's
// compression-done signal.
//
// It is a HANG DETECTOR, not a latency assertion. Each wait returns the
// instant its event arrives, so raising this value does not slow a passing
// run and lowering it does not make the suite stricter — it only changes how
// long a genuinely wedged watcher takes to report.
//
// Rotation fsyncs twice (ForceRotate syncs the fresh active log; the
// compression goroutine behind RotationResult.Done syncs the archive). On a
// host under heavy write load a single fsync can stall for seconds, so a
// watcher's context must be cancel-only when a rotation sits between its
// reads: a shared deadline charges that setup I/O against the read's budget
// and fails a watcher that never got to run (ga-i4jg3a; the eventstest
// conformance suite made the same fix in #4733).
//
// testutil.GoroutineRaceTimeout is a FLOOR, not a target (TESTING.md "Floors,
// ceilings, and inputs"). Mirrors cmd/gc/hangbudget_test.go and
// internal/events/eventstest (hangBudget = 6 * testutil.GoroutineRaceTimeout).
const hangBudget = 6 * testutil.GoroutineRaceTimeout
