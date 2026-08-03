package herdr

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderLiveClaudeKindPath drives the herdr ≥0.7.5 kind-launch path
// against a real herdr AND a real claude binary: Start places a shell pane
// and has herdr launch + detect claude in it (native claude-detection), the
// agent is registered under the session name, liveness holds across checks
// (with a re-issued Start refusing), and Stop tears the pane down. Skipped
// when herdr or claude is unavailable or in -short mode.
func TestProviderLiveClaudeKindPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr+claude test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}

	p := New("gctest-kind", t.TempDir(), t.TempDir(), 0, 0)
	_ = p.Stop("kindsmoke")
	t.Cleanup(func() { _ = p.Stop("kindsmoke"); _ = p.TeardownServer() })

	ctx := context.Background()
	cfg := runtime.Config{
		WorkDir: t.TempDir(),
		Command: "claude",
		Env:     map[string]string{"GC_SESSION_ID": "gctest-kind-session"},
	}
	if err := p.Start(ctx, "kindsmoke", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// herdr registered the agent under the session name (kind path).
	if _, ok, err := p.c.getAgent(ctx, "kindsmoke"); err != nil || !ok {
		t.Fatalf("agent get kindsmoke = ok=%v, %v; want registered", ok, err)
	}
	if mode, _ := p.GetMeta("kindsmoke", metaBoundMode); mode != bindModeAgent {
		t.Errorf("bound mode = %q; want %q", mode, bindModeAgent)
	}
	if pane, _ := p.GetMeta("kindsmoke", metaBoundPane); pane == "" {
		t.Error("bound pane empty after kind Start")
	}

	if !p.IsRunning("kindsmoke") {
		t.Error("IsRunning = false after kind Start")
	}
	if live := p.ObserveLiveness("kindsmoke", nil); !live.Running || !live.Alive {
		t.Errorf("ObserveLiveness = %+v; want Running=true Alive=true", live)
	}
	if err := p.Start(ctx, "kindsmoke", cfg); !errors.Is(err, runtime.ErrSessionExists) {
		t.Errorf("re-issued Start = %v; want ErrSessionExists", err)
	}

	if err := p.Stop("kindsmoke"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i := 0; i < 15 && p.IsRunning("kindsmoke"); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if p.IsRunning("kindsmoke") {
		t.Error("IsRunning = true after Stop")
	}
}
