package session

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"testing"
)

// noCityRefusals records, under go test, every ErrRuntimeLeaseNoCity refusal
// no test said it expected (ExpectNoCityRefusalsForTest), so a test whose
// start or stop never ran because it had no city fails the run
// (FailOnNoCityRefusals) instead of passing on a path production never takes.
// armed is set once FailOnNoCityRefusals runs the tests: without it no
// epilogue will report a refusal, so the refusal reports itself and exits.
var noCityRefusals struct {
	sync.Mutex
	armed    bool
	expected int
	msgs     []string
}

// noCityReport is the gate's report of one unexpected refusal.
const noCityReport = "no-city guard: a test started or stopped a runtime without a city path; give it one (t.TempDir()), or, in a test of the refusal, call session.ExpectNoCityRefusalsForTest: %s\n"

// RefuseWithoutCity is the refusal of a start or stop of what with a city path
// that is not absolute: ErrRuntimeLeaseNoCity, recorded under go test. In a
// test binary that does not run FailOnNoCityRefusals, an unexpected refusal
// prints the report and exits 1 at once.
func RefuseWithoutCity(cityPath, what string) error {
	err := fmt.Errorf("%w (%q): %s", ErrRuntimeLeaseNoCity, cityPath, what)
	if !testing.Testing() {
		return err
	}
	noCityRefusals.Lock()
	report := noCityRefusals.expected == 0
	armed := noCityRefusals.armed
	if report && armed {
		noCityRefusals.msgs = append(noCityRefusals.msgs, fmt.Sprintf("%v\n%s", err, debug.Stack()))
	}
	noCityRefusals.Unlock()
	if report && !armed {
		fmt.Fprintf(os.Stderr, noCityReport, fmt.Sprintf("%v\n%s", err, debug.Stack())) //nolint:errcheck // best-effort report before exit
		os.Exit(1)
	}
	return err
}

// ExpectNoCityRefusalsForTest marks t as a test of the refusal itself: no
// refusal made while it runs is reported. The record is process-wide, so t and
// its parents must not be parallel; t.Setenv enforces that. A goroutine an
// earlier test left running can still refuse inside t's window, unreported.
func ExpectNoCityRefusalsForTest(t testing.TB) {
	t.Helper()
	t.Setenv("GC_TEST_EXPECTS_NO_CITY_REFUSALS", t.Name()) // the parallel check; nothing reads it
	noCityRefusals.Lock()
	noCityRefusals.expected++
	noCityRefusals.Unlock()
	t.Cleanup(func() {
		noCityRefusals.Lock()
		noCityRefusals.expected--
		noCityRefusals.Unlock()
	})
}

// FailOnNoCityRefusals is a TestMain body: it runs run (m.Run, and any other
// epilogue under it) with the gate armed, then reports every unexpected
// refusal to w and returns run's code, or a failing one if there was any. A
// test package needs it only for the report: one that does not run it still
// fails at its first unexpected refusal (RefuseWithoutCity).
func FailOnNoCityRefusals(run func() int, w io.Writer) int {
	noCityRefusals.Lock()
	noCityRefusals.armed = true
	noCityRefusals.Unlock()
	code := run()
	noCityRefusals.Lock()
	defer noCityRefusals.Unlock()
	for _, msg := range noCityRefusals.msgs {
		fmt.Fprintf(w, noCityReport, msg) //nolint:errcheck // best-effort report before exit
	}
	if len(noCityRefusals.msgs) > 0 && code == 0 {
		return 1
	}
	return code
}
