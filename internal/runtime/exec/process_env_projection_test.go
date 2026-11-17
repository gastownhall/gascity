package exec //nolint:revive // internal package, always imported with alias

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
const processEnvProbe = "GC_EXEC_ENV_PROJECTION_PROBE"

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

// Kills a city op that reads gc's environment bare, so it runs inside an
// in-flight process-env projection (the native beads store open) and passes
// its child the projection, or that waits the projection out and spends the
// op's budget. The op starts at once and its child carries the ambient value.
// tmux's and herdr's runSetupCommand read it through execenv for the same
// reason.
func TestCityOpRunsAtOnceDuringAProcessEnvProjection(t *testing.T) {
	const budget = time.Second
	script := writeScript(t, t.TempDir(), `echo "$GC_EXEC_CITY_NAME $`+processEnvProbe+`"`)
	p := NewProviderForCity(script, "alpha", "/cities/alpha")
	end := holdProcessEnvProjection(t)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := p.runWithTimeout(budget, nil, "get-meta", "s", "k")
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.out != "alpha ambient" || r.err != nil {
			t.Fatalf("op during the projection = %q, %v; want %q, nil", r.out, r.err, "alpha ambient")
		}
	case <-time.After(testutil.ExecRaceTimeout):
		end()
		t.Fatal("op did not start while a process-env projection stood")
	}
	end()
}
