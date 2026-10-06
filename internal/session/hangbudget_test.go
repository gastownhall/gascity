package session

import (
	"github.com/gastownhall/gascity/internal/testutil"
)

// goroutineHangBudget is the wall-clock ceiling for test-side waits in this
// package that race an in-process goroutine (a select against a channel the
// code under test is expected to deliver on).
//
// It is a HANG DETECTOR, not a latency assertion. No test in this package
// waits on it to prove the system is fast: the real assertions happen after
// the wait returns. DO NOT tune it to make a failing test pass — see
// TESTING.md "Floors, ceilings, and inputs".
//
// Derived from testutil.GoroutineRaceTimeout (the floor) using the same 6x
// multiplier as cmd/gc's hangBudget (cmd/gc/hangbudget_test.go, ga-h51wa1),
// since this package has no dedicated load-contention measurement of its own
// yet. If a site here starts flaking under fleet load the way cmd/gc's did,
// re-derive from fresh measurement rather than assuming this number still
// holds.
const goroutineHangBudget = 6 * testutil.GoroutineRaceTimeout

// execHangBudget is the wall-clock ceiling for test-side waits in this
// package that race a spawned child process (an exec boundary), such as
// polling for a file a child writes.
//
// Same HANG DETECTOR caveat as goroutineHangBudget above: nothing here
// asserts speed, and the wait returns the instant its condition is met, so
// raising this number does not slow a passing run.
//
// Derived from testutil.ExecRaceTimeout (the floor) using the same 6x
// multiplier as cmd/gc's hangBudget, for the same reason: no
// internal/session-specific contention measurement exists yet. See
// ga-ap7lpd for measured exec-boundary latency under contention in a sibling
// package (storebinding/sqlite) if this budget needs re-deriving.
const execHangBudget = 6 * testutil.ExecRaceTimeout
