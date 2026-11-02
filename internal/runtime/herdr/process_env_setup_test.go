package herdr

import (
	"context"
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
