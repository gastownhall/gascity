package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/testutil"
)

// The setup deadline bounds the command, not the wait for an in-flight
// process-env projection (mirrors the tmux adapter's runSetupCommand).
func TestRunSetupCommandDeadlineStartsAfterAProcessEnvProjection(t *testing.T) {
	const budget = time.Second
	p := &Provider{setupTimeout: budget}
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
	go func() { done <- p.runSetupCommand(context.Background(), "true", map[string]string{}) }()
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

// A pre_start failure is scrubbed against the environment the command
// inherited, not against this process's environment when it fails (mirrors
// the tmux adapter's runSetupCommand).
func TestRunSetupCommandFailureScrubsTheEnvTheCommandInherited(t *testing.T) {
	const inherited = "inherited-NOT-A-REAL-CREDENTIAL-scrub-set"
	t.Setenv("BEADS_DOLT_PASSWORD", inherited)
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	p := &Provider{setupTimeout: testutil.ExecRaceTimeout}
	done := make(chan error, 1)
	go func() {
		done <- p.runSetupCommand(context.Background(),
			`echo "inherited=$BEADS_DOLT_PASSWORD"; read -r _ < "$GC_TEST_SETUP_GATE"; exit 3`,
			map[string]string{"GC_TEST_SETUP_GATE": gate})
	}()
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err != nil {
			opened <- nil
			return
		}
		opened <- f
	}()
	var w *os.File
	select {
	case w = <-opened:
		if w == nil {
			t.Fatal("opening the setup command gate failed")
		}
	case err := <-done:
		t.Fatalf("runSetupCommand returned before its command reached the gate: %v", err)
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("the setup command never reached the gate")
	}

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
	_ = w.Close()
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
		t.Errorf("pre_start failure carries the inherited credential a projection withheld when it was scrubbed: %v", err)
	}
}
