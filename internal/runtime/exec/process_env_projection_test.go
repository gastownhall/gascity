package exec //nolint:revive // internal package, always imported with alias

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/testutil"
)

// Kills a city op that reads gc's environment bare, so it runs inside an
// in-flight process-env projection (the native beads store open), or reads
// it after arming its deadline, so waiting out the projection spends the
// op's budget. tmux's and herdr's runSetupCommand read it first for the
// same reason.
func TestCityOpDeadlineStartsAfterAProcessEnvProjection(t *testing.T) {
	const budget = time.Second
	script := writeScript(t, t.TempDir(), `echo "$GC_EXEC_CITY_NAME"`)
	p := NewProviderForCity(script, "alpha", "/cities/alpha")
	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if projected {
			projected = false
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(restore)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := p.runWithTimeout(budget, nil, "get-meta", "s", "k")
		done <- result{out, err}
	}()
	// The window is the assertion: the op cannot run while the projection
	// stands, and it stands past the op's budget.
	select {
	case r := <-done:
		restore()
		t.Fatalf("op returned (%q, %v) while a process-env projection was in flight", r.out, r.err)
	case <-time.After(2 * budget):
	}
	restore()
	select {
	case r := <-done:
		if r.out != "alpha" || r.err != nil {
			t.Fatalf("op after the projection restored = %q, %v; want alpha, nil", r.out, r.err)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("op did not return after the projection restored")
	}
}
