package tmux

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/testutil"
)

// processEnvProbe is the variable a held projection window changes.
const processEnvProbe = "GC_TMUX_ENV_PROJECTION_PROBE"

// holdProcessEnvProjection enters an execenv projection window, as a native
// beads store open does, and projects a different processEnvProbe value than
// the ambient one a child must inherit. The returned end is idempotent and also
// runs at cleanup.
func holdProcessEnvProjection(t *testing.T) (end func()) {
	t.Helper()
	t.Setenv(processEnvProbe, "ambient")
	exit, err := execenv.EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatalf("EnterProjectionWindow: %v", err)
	}
	if err := os.Setenv(processEnvProbe, "projected"); err != nil {
		exit()
		t.Fatalf("projecting the probe: %v", err)
	}
	var once sync.Once
	end = func() {
		once.Do(func() {
			_ = os.Setenv(processEnvProbe, "ambient")
			exit()
		})
	}
	t.Cleanup(end)
	return end
}

// The setup command starts at once during an in-flight process-env projection:
// a native store open can hold the projection past a pre_start budget, and the
// command neither waits it out nor spends its budget on it. Its child carries
// the ambient value, not the projection; the command exits non-zero when it
// sees anything else.
func TestRunSetupCommandRunsAtOnceDuringAProcessEnvProjection(t *testing.T) {
	ops := &tmuxStartOps{tm: &Tmux{}}
	const budget = time.Second
	end := holdProcessEnvProjection(t)

	done := make(chan error, 1)
	go func() {
		done <- ops.runSetupCommand(context.Background(), `test "$`+processEnvProbe+`" = ambient`, map[string]string{}, budget)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runSetupCommand during the projection: %v", err)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		end()
		t.Fatal("runSetupCommand did not start while a process-env projection stood")
	}
	end()
}
