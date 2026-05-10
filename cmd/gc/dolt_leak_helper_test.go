package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// testReporter is the subset of *testing.T methods that
// requireNoLeakedDoltAfterWith and snapshotDoltProcessPIDsWith touch.
// Splitting these out lets unit tests pass a recording stand-in
// (recordingTB) instead of a real *testing.T, so the helper's reports
// can be inspected without failing the outer test.
type testReporter interface {
	Helper()
	Cleanup(fn func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// recordingTB is a testReporter that records Errorf/Fatalf calls and
// queues Cleanup callbacks for explicit invocation. It does NOT call
// runtime.Goexit on Fatalf — the call is captured so the test can
// assert on the message instead of terminating.
type recordingTB struct {
	cleanups []func()
	errors   []string
	fatals   []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Cleanup(fn func()) {
	r.cleanups = append(r.cleanups, fn)
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingTB) failed() bool {
	return len(r.errors) > 0 || len(r.fatals) > 0
}

// runCleanups invokes registered cleanups in LIFO order to mirror the
// ordering that *testing.T.Cleanup guarantees.
func (r *recordingTB) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

func doltTestProc(pid int, args ...string) DoltProcInfo {
	configPath := filepath.Join(
		"/tmp",
		"TestDoltLeakHelper",
		fmt.Sprintf("%d", pid),
		".gc",
		"runtime",
		"dolt.yaml",
	)
	argv := append([]string{"dolt", "sql-server", "--config=" + configPath}, args...)
	return DoltProcInfo{PID: pid, Argv: argv}
}

// scriptedDoltEnumerator returns a stub func() ([]DoltProcInfo, error)
// that yields successive snapshots from the given slice on each call.
// After all snapshots are exhausted further calls fail the outer test
// — a wrong call count is a test bug, not a behavior we want to assert.
func scriptedDoltEnumerator(t *testing.T, snapshots ...[]DoltProcInfo) func() ([]DoltProcInfo, error) {
	t.Helper()
	var idx int
	return func() ([]DoltProcInfo, error) {
		if idx >= len(snapshots) {
			t.Fatalf("scriptedDoltEnumerator: enumerator called %d times, only %d snapshots scripted", idx+1, len(snapshots))
			return nil, nil
		}
		out := snapshots[idx]
		idx++
		return out, nil
	}
}

// TestRequireNoLeakedDoltAfter_NoChangeNoError pins that when the
// pre-registration and cleanup snapshots are identical (both empty),
// no error is reported. This is the dominant happy path — most tests
// don't spawn any dolt and shouldn't see false-positive leak reports.
func TestRequireNoLeakedDoltAfter_NoChangeNoError(t *testing.T) {
	enumerate := scriptedDoltEnumerator(t, nil, nil)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if inner.failed() {
		t.Fatalf("unexpected reports: errors=%v fatals=%v", inner.errors, inner.fatals)
	}
}

// TestRequireNoLeakedDoltAfter_NewPIDReportedWithArgv pins the core
// behavior: a PID present at cleanup but absent at registration is
// reported via Errorf, and the message embeds both the PID and the
// argv string so operators can trace the spawn site from the test
// log. This is the regression that originally motivated the helper
// (3.3 GiB OOM from un-reaped dolt children — see ga-de27g).
func TestRequireNoLeakedDoltAfter_NewPIDReportedWithArgv(t *testing.T) {
	leaked := DoltProcInfo{
		PID:  99999,
		Argv: []string{"dolt", "sql-server", "--config=/tmp/Test123/.gc/runtime/dolt.yaml"},
	}
	enumerate := scriptedDoltEnumerator(t,
		nil,                    // initial: no procs
		[]DoltProcInfo{leaked}, // cleanup: one new proc
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if !inner.failed() {
		t.Fatalf("expected leak Errorf; nothing recorded")
	}
	if len(inner.errors) != 1 {
		t.Fatalf("expected exactly 1 Errorf, got %d: %v", len(inner.errors), inner.errors)
	}
	msg := inner.errors[0]
	if !strings.Contains(msg, "99999") {
		t.Errorf("error message missing leaked PID 99999; got %q", msg)
	}
	for _, arg := range leaked.Argv {
		if !strings.Contains(msg, arg) {
			t.Errorf("error message missing argv token %q; got %q", arg, msg)
		}
	}
}

// TestRequireNoLeakedDoltAfter_PreExistingPIDsNotReported pins the
// diff math when pre-existing dolt processes are running on the host:
// PIDs present at registration MUST NOT be reported as leaks at
// cleanup, even though they appear in the cleanup snapshot. Without
// this subtraction the helper would false-positive on every host
// running an unrelated dolt server.
func TestRequireNoLeakedDoltAfter_PreExistingPIDsNotReported(t *testing.T) {
	preexisting := doltTestProc(1000)
	enumerate := scriptedDoltEnumerator(t,
		[]DoltProcInfo{preexisting}, // initial
		[]DoltProcInfo{preexisting}, // cleanup: same set, no leak
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if inner.failed() {
		t.Fatalf("pre-existing PID reported as leaked: errors=%v fatals=%v",
			inner.errors, inner.fatals)
	}
}

// TestRequireNoLeakedDoltAfter_OnlyNewPIDsInDiff pins that when the
// cleanup snapshot contains BOTH a pre-existing PID and a new PID,
// only the new one appears in the error message. This proves the diff
// is computed (cleanup minus initial), not re-reported in full.
func TestRequireNoLeakedDoltAfter_OnlyNewPIDsInDiff(t *testing.T) {
	preexisting := doltTestProc(1000)
	leaked := doltTestProc(9999, "--leaked")
	enumerate := scriptedDoltEnumerator(t,
		[]DoltProcInfo{preexisting},
		[]DoltProcInfo{preexisting, leaked},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if !inner.failed() {
		t.Fatalf("expected leak Errorf for PID 9999; nothing recorded")
	}
	msg := strings.Join(inner.errors, "\n")
	if !strings.Contains(msg, "9999") {
		t.Errorf("error missing leaked PID 9999; got %q", msg)
	}
	if strings.Contains(msg, "1000") {
		t.Errorf("error must not include pre-existing PID 1000; got %q", msg)
	}
}

// TestRequireNoLeakedDoltAfter_MultipleLeaksReportedSorted pins two
// guarantees needed for stable test logs across runs:
//
//  1. Multiple leaked PIDs are aggregated into a single Errorf call
//     (operators get one report per test, not N).
//  2. PIDs are listed in ascending numerical order regardless of how
//     the enumerator returns them.
func TestRequireNoLeakedDoltAfter_MultipleLeaksReportedSorted(t *testing.T) {
	leakedHi := doltTestProc(50002, "--port=3308")
	leakedLo := doltTestProc(50001, "--port=3307")
	enumerate := scriptedDoltEnumerator(t,
		nil,
		// Order in slice deliberately unsorted to verify the helper sorts.
		[]DoltProcInfo{leakedHi, leakedLo},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if !inner.failed() {
		t.Fatalf("expected leak Errorf for two leaked PIDs; nothing recorded")
	}
	if len(inner.errors) != 1 {
		t.Fatalf("multiple leaks must be aggregated into one Errorf, got %d: %v",
			len(inner.errors), inner.errors)
	}
	msg := inner.errors[0]
	iLo := strings.Index(msg, "50001")
	iHi := strings.Index(msg, "50002")
	if iLo == -1 {
		t.Errorf("error missing PID 50001; got %q", msg)
	}
	if iHi == -1 {
		t.Errorf("error missing PID 50002; got %q", msg)
	}
	if iLo != -1 && iHi != -1 && iLo > iHi {
		t.Errorf("PIDs not in ascending order; got %q", msg)
	}
}

// TestRequireNoLeakedDoltAfter_NewNonTestPIDIgnored pins that the leak helper
// ignores unrelated dolt servers whose config path is outside the test-temp
// allowlist. City or pack runtimes can start their own managed dolt process
// while this test package is running; those are not leaks from the test under
// inspection.
func TestRequireNoLeakedDoltAfter_NewNonTestPIDIgnored(t *testing.T) {
	unrelated := DoltProcInfo{
		PID: 2041535,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			"/data/projects/maintainer-city/.gc/runtime/packs/dolt/dolt-config.yaml",
		},
	}
	enumerate := scriptedDoltEnumerator(t,
		nil,
		[]DoltProcInfo{unrelated},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if inner.failed() {
		t.Fatalf("unrelated dolt server reported as leaked: errors=%v fatals=%v",
			inner.errors, inner.fatals)
	}
}

func TestRequireNoLeakedDoltAfterWithFilterIgnoresUnownedTempPID(t *testing.T) {
	ownedRoot := filepath.Join("/tmp", "TestDoltLeakHelper", "owned-city")
	unownedRoot := filepath.Join("/tmp", "TestDoltLeakHelper", "other-city")
	owned := DoltProcInfo{
		PID: 1001,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(ownedRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	unowned := DoltProcInfo{
		PID: 1002,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(unownedRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	enumerate := scriptedDoltEnumerator(t,
		nil,
		[]DoltProcInfo{owned, unowned},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWithFilter(inner, enumerate, func(configPath string) bool {
		return samePath(configPath, ownedRoot) || strings.HasPrefix(configPath, ownedRoot+string(filepath.Separator))
	})
	inner.runCleanups()

	if !inner.failed() {
		t.Fatalf("expected scoped leak Errorf for owned PID; nothing recorded")
	}
	msg := strings.Join(inner.errors, "\n")
	if !strings.Contains(msg, "1001") {
		t.Fatalf("error missing owned leaked PID 1001; got %q", msg)
	}
	if strings.Contains(msg, "1002") {
		t.Fatalf("error included unowned leaked PID 1002; got %q", msg)
	}
}

// TestSnapshotDoltProcessPIDs_EnumeratorErrorIsFatal pins that a
// discovery error is reported via Fatalf so test runs surface
// enumeration failures directly rather than silently treating them
// as "no procs". A swallowed error here would mask real leaks.
func TestSnapshotDoltProcessPIDs_EnumeratorErrorIsFatal(t *testing.T) {
	boom := errors.New("synthetic enumeration failure")
	enumerate := func() ([]DoltProcInfo, error) {
		return nil, boom
	}
	inner := &recordingTB{}
	snapshotDoltProcessPIDsWith(inner, enumerate)
	if !inner.failed() {
		t.Fatalf("expected Fatalf when enumerator errors; nothing recorded")
	}
	if len(inner.fatals) == 0 {
		t.Fatalf("expected Fatalf, got Errorf only: %v", inner.errors)
	}
	if !strings.Contains(inner.fatals[0], boom.Error()) {
		t.Errorf("Fatalf message missing original error %q; got %q",
			boom.Error(), inner.fatals[0])
	}
}
