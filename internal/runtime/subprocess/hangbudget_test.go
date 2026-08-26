package subprocess

import (
	"github.com/gastownhall/gascity/internal/testutil"
)

// hangBudget is the wall-clock ceiling for every pure hang-detector wait in
// this package's tests: the legacy-socket command loop in
// TestLegacySocketRemainsVisibleWhenPrivateFallbackIsMissing, the
// interrupt-command wait in
// TestInterruptPreservesBestEffortForNormalSocketProtocolError (both in
// subprocess_test.go), and the liveness-polling ceiling in TestSeamsInterrupt
// (seams_test.go).
//
// It is a HANG DETECTOR, not a latency assertion. None of the waits above
// depend on how long they took — the real assertions happen after the wait
// returns (or, for TestSeamsInterrupt's poll loop, the moment the polled
// condition becomes true) — so raising this number does not slow a passing
// run; it only changes how long a genuinely wedged test takes to report. See
// TESTING.md "Floors, ceilings, and inputs".
//
// Derived from testutil.ExecRaceTimeout (the floor for a wait that races a
// real subprocess or, here, a Unix-socket control connection to one) using
// the same 6x multiplier as cmd/gc's hangBudget (cmd/gc/hangbudget_test.go,
// ga-h51wa1) and internal/session's execHangBudget
// (internal/session/hangbudget_test.go), since this package has no
// dedicated load-contention measurement of its own yet. If a site here
// starts flaking under fleet load, re-derive from fresh measurement rather
// than assuming this number still holds.
//
// sendSocketCommand's timeout argument in requirePrivateFallbackRejected
// (subprocess_test.go) is deliberately NOT covered by hangBudget: that
// testutil.ExecRaceTimeout is a real net.DialTimeout/SetDeadline parameter
// fed into the production code under test (sendSocketCommandAt), not a
// test-owned wait — TESTING.md's "timeout fed into code under test" case,
// which is never migrated.
const hangBudget = 6 * testutil.ExecRaceTimeout
