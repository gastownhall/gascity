//go:build integration || dolt_integration

package gastown_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/doltorphan"
)

// TestSweep_ReapsRealDoltDataDirAfterSIGKILL exercises acceptance criterion 3
// of ga-ntbpyb.2 against a real dolt sql-server rather than the synthetic
// fixtures in internal/doltorphan/sweep_test.go: those tests fake lsof, so
// none of them prove the heuristic behaves correctly against a live
// process's actual open files. This spawns a real `dolt sql-server
// --data-dir` (the same config-less shape startDoltServerForMaintenanceTest
// uses, which SIGKILL cannot cleanly shut down since a killed process gets
// no chance to release anything itself), then runs the sweep twice against
// the same deliberately-backdated clock: once while the server is still
// alive, to prove a sweep racing a still-running process — e.g. one
// triggered by a `go test -timeout` firing mid-run — cannot destroy live
// data just because it looks old and marker-tagged; and once after a direct
// SIGKILL, to prove the same directory is reaped once genuinely orphaned.
func TestSweep_ReapsRealDoltDataDirAfterSIGKILL(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skipf("lsof not found: %v", err)
	}

	root := t.TempDir()
	dataDir := filepath.Join(root, "dolt-data")
	dbDir := filepath.Join(dataDir, "sweepdb")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dbDir, err)
	}
	runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")

	pid, port, killAndWait := startSweepDoltServer(t, doltPath, dataDir)
	t.Logf("dolt sql-server pid=%d port=%d dataDir=%s", pid, port, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "sweepdb")

	// Backdate "now" past DefaultMinAge so both sweeps see an old-enough
	// candidate without a real 60-minute wait; the margin comfortably
	// covers however long this test takes to run.
	sweepCfg := doltorphan.SweepConfig{
		Root:  root,
		Clock: &clock.Fake{Time: time.Now().Add(doltorphan.DefaultMinAge + 5*time.Minute)},
	}

	before := doltorphan.Sweep(sweepCfg)
	if len(before.Errors) != 0 {
		t.Fatalf("sweep errored while the dolt sql-server was still alive: %v", before.Errors)
	}
	if len(before.Removed) != 0 {
		t.Fatalf("sweep removed a directory still held open by a live dolt sql-server: %v", before.Removed)
	}
	if before.Skipped != 1 {
		t.Fatalf("sweep.Skipped = %d, want 1 (dataDir held open by the live dolt sql-server)", before.Skipped)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("data dir vanished from a sweep pass that should have skipped it: %v", err)
	}

	if !killAndWait() {
		t.Fatalf("dolt sql-server (pid %d) did not exit within 10s of SIGKILL", pid)
	}

	after := doltorphan.Sweep(sweepCfg)
	if len(after.Errors) != 0 {
		t.Fatalf("sweep errored after the dolt sql-server was SIGKILLed: %v", after.Errors)
	}
	if len(after.Removed) != 1 || after.Removed[0] != dataDir {
		t.Fatalf("sweep.Removed = %v, want [%s] after the dolt sql-server was SIGKILLed", after.Removed, dataDir)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir %s still exists after sweep reported removing it (stat err = %v)", dataDir, err)
	}
}

// startSweepDoltServer spawns a real dolt sql-server serving dataDir on a
// free localhost port, mirroring startDoltServerForMaintenanceTest's
// --data-dir-only shape. Unlike that helper, it hands back a killAndWait
// func the test calls directly — a real SIGKILL is the whole point of this
// test — guarded by sync.Once so the t.Cleanup safety net can call it again
// for free if the test never reaches its own call (e.g. an earlier Fatalf).
func startSweepDoltServer(t *testing.T, doltPath, dataDir string) (pid, port int, killAndWait func() bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port = listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("Create(%s): %v", logPath, err)
	}

	cmd := exec.Command(doltPath, "sql-server",
		"-H", "127.0.0.1",
		"-P", fmt.Sprintf("%d", port),
		"--data-dir", dataDir,
		"--loglevel", "warning",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("Start dolt sql-server: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var once sync.Once
	var exited bool
	killAndWait = func() bool {
		once.Do(func() {
			_ = cmd.Process.Kill()
			select {
			case <-done:
				exited = true
			case <-time.After(10 * time.Second):
				exited = false
			}
			_ = logFile.Close()
		})
		return exited
	}
	t.Cleanup(func() { killAndWait() })
	return cmd.Process.Pid, port, killAndWait
}
