package herdr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderLive drives the herdr Provider against a real herdr binary in an
// isolated session. Opt-in live tier: see requireLiveHerdr.
func TestProviderLive(t *testing.T) {
	requireLiveHerdr(t)

	p := New("gctest-live", t.TempDir(), t.TempDir(), 0, 0)
	_ = p.Stop("smoke") // clear any leftover from a crashed prior run
	t.Cleanup(func() { _ = p.Stop("smoke"); _ = p.TeardownServer() })

	ctx := context.Background()
	cfg := runtime.Config{
		WorkDir: t.TempDir(),
		Command: `for i in $(seq 1 60); do echo "tick $i"; sleep 1; done`,
		Env:     map[string]string{"GC_SESSION_ID": "gctest-live-session"},
	}
	if err := p.Start(ctx, "smoke", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start must persist GC_SESSION_ID to the meta sidecar (tmux parity):
	// ProcessAlive's session-scoped tree-walk widening has nothing to read
	// without it.
	if v, err := p.GetMeta("smoke", "GC_SESSION_ID"); err != nil || v != "gctest-live-session" {
		t.Errorf("GetMeta(GC_SESSION_ID) = %q, %v; want %q, nil", v, err, "gctest-live-session")
	}

	if !p.IsRunning("smoke") {
		t.Error("IsRunning = false after Start")
	}
	if names, err := p.ListRunning("smo"); err != nil || len(names) != 1 || names[0] != "smoke" {
		t.Errorf("ListRunning(smo) = %v, %v; want [smoke]", names, err)
	}

	// Peek the current screen ("visible") — wait for output to render.
	var got string
	for i := 0; i < 20; i++ {
		time.Sleep(300 * time.Millisecond)
		got, _ = p.Peek("smoke", 10)
		if strings.Contains(got, "tick") {
			break
		}
	}
	if !strings.Contains(got, "tick") {
		t.Errorf("Peek did not capture screen output; got %q", got)
	}

	// ProcessAlive: nil → true; matching name → true; bogus → false.
	if !p.ProcessAlive("smoke", nil) {
		t.Error("ProcessAlive(nil) = false")
	}
	if !p.ProcessAlive("smoke", []string{"sleep", "sh", "bash"}) {
		t.Error("ProcessAlive([sleep/sh/bash]) = false")
	}
	if p.ProcessAlive("smoke", []string{"definitely-not-a-real-process"}) {
		t.Error("ProcessAlive([bogus]) = true")
	}

	// Metadata sidecar roundtrip.
	if err := p.SetMeta("smoke", "drain", "1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if v, err := p.GetMeta("smoke", "drain"); err != nil || v != "1" {
		t.Errorf("GetMeta(drain) = %q, %v; want 1", v, err)
	}
	if v, err := p.GetMeta("smoke", "absent"); err != nil || v != "" {
		t.Errorf("GetMeta(absent) = %q, %v; want empty,nil", v, err)
	}

	// Stop → no longer running.
	if err := p.Stop("smoke"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i := 0; i < 10 && p.IsRunning("smoke"); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if p.IsRunning("smoke") {
		t.Error("IsRunning = true after Stop")
	}
}

// TestProviderLiveStartsOverRestoredStaleWorkspace covers the leftover-state
// path behind the workspace_not_found gate failures (ga-19onv3): herdr saves a
// session's workspaces when its server goes down and restores them on the next
// server start under the same name, so a Start can find a workspace whose only
// tab is the stale one it is about to recycle. herdr closes a workspace with its
// last tab, so the Start must recreate the workspace rather than fail.
//
// The session starts a bare shell (empty Command) on purpose: typing into a
// fresh zsh pane is flaky under account HOMEs that have no .zshrc.
func TestProviderLiveStartsOverRestoredStaleWorkspace(t *testing.T) {
	requireLiveHerdr(t)

	metaDir, cityRoot := t.TempDir(), t.TempDir()
	cfg := runtime.Config{WorkDir: t.TempDir()}
	ctx := context.Background()

	first := New("gctest-restore", metaDir, cityRoot, 0, 0)
	t.Cleanup(func() { _ = first.TeardownServer() })
	if err := first.Start(ctx, "smoke", cfg); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	// Shutting the server down saves the workspace holding smoke's only tab.
	if err := first.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer: %v", err)
	}

	// A fresh metaDir: no pane binding survived, so IsRunning is false and Start
	// proceeds to placement instead of adopting the restored pane.
	second := New("gctest-restore", t.TempDir(), cityRoot, 0, 0)
	t.Cleanup(func() { _ = second.Stop("smoke"); _ = second.TeardownServer() })
	if err := second.Start(ctx, "smoke", cfg); err != nil {
		t.Fatalf("Start over the restored workspace: %v", err)
	}
	if !second.IsRunning("smoke") {
		t.Error("IsRunning = false after Start over the restored workspace")
	}
}
