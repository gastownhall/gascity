package session

import (
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"testing"
)

// noCityRefusals records, under go test, every ErrRuntimeLeaseNoCity refusal
// no test said it expected (ExpectNoCityRefusalsForTest), so a test whose
// start or stop never ran because it had no city fails the run
// (FailOnNoCityRefusals) instead of passing on a path production never takes.
var noCityRefusals struct {
	sync.Mutex
	expected int
	msgs     []string
}

// RefuseWithoutCity is the refusal of a start or stop of what with a city path
// that is not absolute: ErrRuntimeLeaseNoCity, recorded under go test.
func RefuseWithoutCity(cityPath, what string) error {
	err := fmt.Errorf("%w (%q): %s", ErrRuntimeLeaseNoCity, cityPath, what)
	if testing.Testing() {
		noCityRefusals.Lock()
		if noCityRefusals.expected == 0 {
			noCityRefusals.msgs = append(noCityRefusals.msgs, fmt.Sprintf("%v\n%s", err, debug.Stack()))
		}
		noCityRefusals.Unlock()
	}
	return err
}

// ExpectNoCityRefusalsForTest marks t as a test of the refusal itself: the
// refusals made while it runs are not reported. The record is process-wide, so
// t and its parents must not be parallel; t.Setenv enforces that.
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

// FailOnNoCityRefusals is a TestMain epilogue: given m.Run's code, it reports
// every unexpected refusal to w and returns a failing code if there was any.
// Every test package that starts or stops runtimes runs it (cmd/gc,
// internal/api, internal/session, internal/worker, internal/worker/workertest).
func FailOnNoCityRefusals(code int, w io.Writer) int {
	noCityRefusals.Lock()
	defer noCityRefusals.Unlock()
	for _, msg := range noCityRefusals.msgs {
		fmt.Fprintf(w, "no-city guard: a test started or stopped a runtime without a city path; give it one (t.TempDir()), or, in a test of the refusal, call session.ExpectNoCityRefusalsForTest: %s\n", msg) //nolint:errcheck // best-effort report before exit
	}
	if len(noCityRefusals.msgs) > 0 && code == 0 {
		return 1
	}
	return code
}
