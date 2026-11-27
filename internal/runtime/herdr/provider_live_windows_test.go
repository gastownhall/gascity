//go:build windows

package herdr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderLive's command, the output it puts on screen, and the process
// names it runs under, for this platform's pane shell. The output is computed
// ("tick-42" from 6*7), so text the pane merely echoes cannot supply it: a
// typed line PowerShell failed to run once passed this check by being echoed.
const (
	liveTickCommand = `"tick-$(6*7)"; & ping.exe -n 60 127.0.0.1 | Out-Null`
	liveTickOutput  = "tick-42"
)

var liveProcessNames = []string{"ping"}

// liveSwapCommand is TestProviderLiveOccupantSwapKeepsLiveness's command. Windows
// has no exec, so there is no occupant swap: the command runs as the pane
// shell's child for its whole life. What the test still pins is its point:
// liveness with no registered herdr name, which a raw session never has, a
// refused re-Start, and a Stop that tears the pane down.
const liveSwapCommand = `& ping.exe -n 120 127.0.0.1 | Out-Null`

// TestRawLaunchEndsWithItsCommandLive pins the Windows raw launch against a
// real herdr. The typed statement runs the command in a child interpreter whose
// output and errors reach the pane as text; Start returns once that interpreter
// has started rather than after rawLaunchWait; and the pane closes when the
// command exits, as a tmux session ends with its command. Opt-in live tier: see
// requireLiveHerdr.
func TestRawLaunchEndsWithItsCommandLive(t *testing.T) {
	requireLiveHerdr(t)

	p := New("gctest-raw", t.TempDir(), t.TempDir(), 0, 0)
	_ = p.Stop("raw") // clear any leftover from a crashed prior run
	t.Cleanup(func() { _ = p.Stop("raw"); _ = p.TeardownServer() })

	ctx := context.Background()
	cfg := runtime.Config{
		WorkDir: t.TempDir(),
		// Computed text on both streams, so neither an echoed line nor the typed
		// statement can supply it.
		Command: `Write-Error ('raw-error-' + 6*7); "raw-out-$(6*7)"; Start-Sleep -Seconds 6; exit 3`,
	}
	started := time.Now()
	if err := p.Start(ctx, "raw", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if took := time.Since(started); took >= rawLaunchWait {
		t.Errorf("Start took %s; the launched interpreter was never seen, only waited out", took)
	}

	var screen string
	for i := 0; i < 20; i++ {
		time.Sleep(300 * time.Millisecond)
		screen, _ = p.Peek("raw", 30)
		if strings.Contains(screen, "raw-out-42") && strings.Contains(screen, "raw-error-42") {
			break
		}
	}
	if !strings.Contains(screen, "raw-out-42") || !strings.Contains(screen, "raw-error-42") {
		t.Errorf("the pane does not show the command's output and error; screen %q", screen)
	}
	if strings.Contains(screen, "CLIXML") {
		t.Errorf("the pane shows serialized error records: %q", screen)
	}

	deadline := time.Now().Add(20 * time.Second)
	for p.IsRunning("raw") && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	if p.IsRunning("raw") {
		t.Error("the session outlived its command; the pane should close when the command exits")
	}
}
