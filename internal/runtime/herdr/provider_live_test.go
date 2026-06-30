package herdr

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderLive drives the herdr Provider against a real herdr binary in an
// isolated session. Skipped when herdr is unavailable or in -short mode.
func TestProviderLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}

	p := New("gctest-live", t.TempDir(), t.TempDir())
	_ = p.Stop("smoke") // clear any leftover from a crashed prior run
	t.Cleanup(func() { _ = p.Stop("smoke"); _ = p.TeardownServer() })

	ctx := context.Background()
	cfg := runtime.Config{
		WorkDir: t.TempDir(),
		Command: `for i in $(seq 1 60); do echo "tick $i"; sleep 1; done`,
	}
	if err := p.Start(ctx, "smoke", cfg); err != nil {
		t.Fatalf("Start: %v", err)
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
