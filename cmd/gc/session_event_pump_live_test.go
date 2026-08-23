package main

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
)

// TestSessionEventPumpLiveHerdr proves the event→poke chain against a real
// herdr binary: the pump subscribes through the provider's stream, and an
// agent's natural process exit lands a reconcile poke without any polling.
// Skipped when herdr is unavailable or in -short mode.
func TestSessionEventPumpLiveHerdr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}

	const session = "gctest-pump-live"
	p := herdr.New(session, t.TempDir(), t.TempDir(), 0, 0)
	_ = p.TeardownServer() // clear any leftover server from a crashed prior run
	t.Cleanup(func() { _ = p.TeardownServer() })
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pokeCh := make(chan struct{}, 1)
	pump := newSessionEventPump(ctx, pokeCh, &bytes.Buffer{}, "live")
	// Park resync pokes outside the test window so the only poke observed
	// below is the attributed process-exit one.
	pump.resyncDelay = time.Minute
	pump.restart(p)
	if !pump.streaming() {
		t.Fatal("pump not streaming against live herdr")
	}

	// A short-lived agent: its natural exit is the death we detect.
	cfg := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 2"}
	if err := p.Start(ctx, "evt-pump-live", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("evt-pump-live") })

	// Drain startup noise (leading resync, resubscribe cycles for the new
	// agent pane) until the pokes go quiet, then the process exit must poke.
	quietUntil := time.Now().Add(time.Second)
	for time.Now().Before(quietUntil) {
		select {
		case <-pokeCh:
			quietUntil = time.Now().Add(time.Second)
		case <-time.After(100 * time.Millisecond):
		}
	}

	start := time.Now()
	select {
	case <-pokeCh:
		t.Logf("process exit → reconcile poke in %v", time.Since(start).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("no reconcile poke after the agent process exited")
	}
}
