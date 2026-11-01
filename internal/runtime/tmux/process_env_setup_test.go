package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// The setup deadline bounds the command, not the wait for an in-flight
// process-env projection: a native store open can hold the projection past a
// pre_start budget, and the command must still get its whole budget once the
// projection restores.
func TestRunSetupCommandDeadlineStartsAfterAProcessEnvProjection(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}
	const budget = time.Second
	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if projected {
			projected = false
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(restore)

	done := make(chan error, 1)
	go func() { done <- ops.runSetupCommand(context.Background(), "true", map[string]string{}, budget) }()
	// The window is the assertion: the command cannot run while the
	// projection stands, and it stands past the command's budget.
	select {
	case err := <-done:
		restore()
		t.Fatalf("runSetupCommand returned (err=%v) while a process-env projection was in flight", err)
	case <-time.After(2 * budget):
	}
	restore()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runSetupCommand after the projection restored: %v", err)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("runSetupCommand did not return after the projection restored")
	}
}

// A setup command's failure output is scrubbed against the environment the
// command inherited, not against this process's environment when it fails. A
// native store open withholds ambient BEADS_* variables process-wide for its
// whole window, so a scrub set read inside that window lacks a credential the
// command still holds and echoed, and the value would land in a durable error.
func TestRunSetupCommandFailureScrubsTheEnvTheCommandInherited(t *testing.T) {
	const inherited = "inherited-NOT-A-REAL-CREDENTIAL-scrub-set"
	t.Setenv("BEADS_DOLT_PASSWORD", inherited)
	gate := newSetupCommandGate(t)
	ops := &tmuxStartOps{tm: &Tmux{}}
	done := make(chan error, 1)
	go func() {
		done <- ops.runSetupCommand(context.Background(),
			`echo "inherited=$BEADS_DOLT_PASSWORD"; read -r _ < "$GC_TEST_SETUP_GATE"; exit 3`,
			map[string]string{"GC_TEST_SETUP_GATE": gate}, testutil.ExecRaceTimeout)
	}()
	release := openSetupCommandGate(t, gate, done)

	// The command runs with its environment fixed. A projection now withholds
	// the variable, as a native store open does, and the command fails inside
	// that window.
	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if projected {
			projected = false
			_ = os.Setenv("BEADS_DOLT_PASSWORD", inherited)
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(restore)
	if err := os.Unsetenv("BEADS_DOLT_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	release()
	var err error
	select {
	case err = <-done:
	case <-time.After(testutil.ExecRaceTimeout):
		restore()
		t.Fatal("runSetupCommand did not return after its command was released")
	}
	restore()
	if err == nil {
		t.Fatal("runSetupCommand succeeded; want the command's exit-3 failure")
	}
	if !strings.Contains(err.Error(), "inherited=") {
		t.Fatalf("the command's output never reached the error, so this proves nothing: %v", err)
	}
	if strings.Contains(err.Error(), inherited) {
		t.Errorf("setup failure carries the inherited credential a projection withheld when it was scrubbed: %v", err)
	}
}

// newSetupCommandGate makes a FIFO a setup command blocks reading until the
// test releases it.
func newSetupCommandGate(t *testing.T) string {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return gate
}

// openSetupCommandGate opens gate for writing, which returns once the command
// has opened it for reading, so the command is running and its environment is
// fixed. The returned release closes the write end, ending the command's read.
func openSetupCommandGate(t *testing.T, gate string, done <-chan error) (release func()) {
	t.Helper()
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err != nil {
			opened <- nil
			return
		}
		opened <- f
	}()
	select {
	case f := <-opened:
		if f == nil {
			t.Fatal("opening the setup command gate failed")
		}
		return func() { _ = f.Close() }
	case err := <-done:
		t.Fatalf("runSetupCommand returned before its command reached the gate: %v", err)
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("the setup command never reached the gate")
	}
	return nil
}

// The process snapshot is a one-shot ps whose output this process parses and
// discards: it reads no BEADS_* or BD_* variable and cannot outlive the call,
// so building it must not wait out an in-flight process-env projection. A
// native store open holds the projection for its whole window, and the
// snapshot runs inside the state cache's refresh singleflight, where every
// IsRunning, ProcessAlive and SessionAttached caller on a stale cache waits
// with it.
func TestProcessSnapshotCommandDoesNotWaitOutAProcessEnvProjection(t *testing.T) {
	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if projected {
			projected = false
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(restore)
	built := make(chan []string, 1)
	go func() { built <- processSnapshotCommand(context.Background(), processSnapshotPSArgs()).Env }()
	select {
	case env := <-built:
		restore()
		if env == nil {
			t.Fatal("ps inherits the environment whole, COLUMNS included")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		restore()
		t.Fatal("building the process snapshot command waited out a process-env projection")
	}
}

// A dead session's pane is scrubbed against the inherited environment as it
// stands when the pane is captured, read through execenv.Environ, not as it
// stood when the start's ops were built. Ops built while a projection withheld
// an ambient credential (a start that began inside a native store open) must
// still scrub it from a capture taken after the open ended, and building them
// must not wait the projection out: a start that succeeds never captures.
func TestCapturePaneScrubsTheAmbientEnvironmentAProjectionHid(t *testing.T) {
	const ambient = "ambient-NOT-A-REAL-CREDENTIAL-pane-capture"
	t.Setenv("BEADS_DOLT_PASSWORD", ambient)
	tm := NewTmux()
	// The agent-pane lookup, then the capture.
	tm.exec = &fakeExecutor{outs: []string{"pane\t@1.%0", "+ curl -u gc:" + ambient + "\ncurl: (22) 401\n"}}

	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if projected {
			projected = false
			_ = os.Setenv("BEADS_DOLT_PASSWORD", ambient)
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(restore)
	if err := os.Unsetenv("BEADS_DOLT_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	built := make(chan *tmuxStartOps, 1)
	go func() { built <- newTmuxStartOps(tm, "", 0, gcruntime.Config{}, true) }()
	var ops *tmuxStartOps
	select {
	case ops = <-built:
	case <-time.After(testutil.GoroutineRaceTimeout):
		restore()
		t.Fatal("building start ops waited out a process-env projection")
	}
	restore()

	pane, err := ops.capturePane("gc-test-crash", startupPaneCaptureLines)
	if err != nil {
		t.Fatalf("capturePane: %v", err)
	}
	if !strings.Contains(pane, "curl: (22) 401") {
		t.Fatalf("the capture lost its diagnostic, so this proves nothing: %q", pane)
	}
	if strings.Contains(pane, ambient) {
		t.Errorf("the pane capture carries the ambient credential a projection hid when the start's ops were built: %q", pane)
	}
}
