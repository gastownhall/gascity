package doctor

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// TestReproOrphanedPackScriptKilledOnAbandon is the regression test for
// gastownhall/gascity#8504: a pack [[doctor]] script check that hangs past
// --check-timeout must have its underlying process killed, not just reported
// as abandoned.
//
// Before the fix, PackScriptCheck.Run used a plain exec.Command blocking on
// CombinedOutput, which never watched ctx.Done at all: boundedRun gave up
// waiting on the goroutine and returned a "timed out and was abandoned"
// result, but the script process itself (and anything it had forked, like
// the backgrounded sleep below) kept running, re-parented to init, forever.
//
// The script backgrounds a `sleep 30`, records its PID, then waits on it —
// mirroring the established process-group-kill regression pattern in
// internal/beads/bdstore_exec_internal_test.go
// (TestKillCommandTreeKillsProcessGroup), which guards the equivalent fix for
// bd subprocesses. After Doctor.Run abandons the check at a short
// CheckTimeout, this test polls for the recorded PID to actually disappear.
// On the pre-fix code this assertion fails: the sleep keeps running
// indefinitely because nothing ever signals it.
func TestReproOrphanedPackScriptKilledOnAbandon(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "hang.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
sleep 30 &
echo "$!" > "$GC_PACK_DIR/child.pid"
wait
`), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// 2s gives the shell comfortable headroom to fork the backgrounded sleep
	// and write its pid before the timeout fires — a tighter bound (tried
	// 50ms and 500ms here) flakes under load, since this races process
	// startup (fork+exec of sh, then fork+exec of sleep) against the
	// timeout rather than the thing actually under test. macOS also pays a
	// validation tax on first-exec of a freshly written script file (see
	// the same tradeoff noted on TestKillCommandTreeKillsProcessGroup in
	// internal/beads/bdstore_exec_internal_test.go). It's still well under
	// the 30s sleep, so the regression (an un-killed process) is caught.
	d := &Doctor{CheckTimeout: 2 * time.Second}
	d.Register(&PackScriptCheck{
		CheckName: "repro:hang",
		Script:    script,
		PackDir:   dir,
		PackName:  "repro",
	})

	var buf bytes.Buffer
	report := d.Run(&CheckContext{CityPath: dir}, &buf, false)

	if len(report.Results) != 1 || !report.Results[0].TimedOut {
		t.Fatalf("report.Results = %+v, want one TimedOut result", report.Results)
	}

	childPID := waitForNonEmptyPIDFile(t, pidFile, 5*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return // the orphaned script's child process is gone: fix confirmed.
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(childPID, syscall.SIGKILL)
	t.Fatalf("pid %d (child of an abandoned pack script) survived after Doctor.Run returned", childPID)
}

// waitForNonEmptyPIDFile polls path until it holds a parseable PID, returning
// it. Fails the test if timeout elapses first.
func waitForNonEmptyPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
				pid, parseErr := strconv.Atoi(trimmed)
				if parseErr != nil {
					t.Fatalf("parse pid file %s: %v", path, parseErr)
				}
				return pid
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read pid file %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}
