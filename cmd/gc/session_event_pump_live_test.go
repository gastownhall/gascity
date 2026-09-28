package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// TestSessionEventPumpLiveHerdr proves the event→poke chain against a real
// herdr binary: the pump subscribes through the provider's stream, and an
// agent's natural process exit lands a reconcile poke without any polling.
// Opt-in: see herdrtest.RequireLive.
func TestSessionEventPumpLiveHerdr(t *testing.T) {
	herdrtest.RequireLive(t)

	// Unique per run: herdr persists session state across server restarts, so a
	// fixed name inherits a prior run's leftovers.
	session := fmt.Sprintf("gctest-pump-live-%d", time.Now().UnixNano())
	p := herdr.New(session, t.TempDir(), t.TempDir(), 0, 0)
	_ = p.TeardownServer() // clear any leftover server from a crashed prior run
	t.Cleanup(func() { _ = p.TeardownServer() })
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The agent blocks on a flag file rather than a fixed sleep, so the exit
	// happens when this test asks for it. A timed exit races the quiet-drain
	// below, which would consume the very poke the assertion then waits for,
	// and the failure reads as "the pump never poked".
	const agentName = "evt-pump-live"
	work := t.TempDir()
	exitFlag := filepath.Join(work, "exit-now")
	cfg := runtime.Config{
		WorkDir: work,
		Command: fmt.Sprintf("/bin/sh -c 'while [ ! -e %s ]; do sleep 0.2; done'", exitFlag),
	}
	if err := p.Start(ctx, agentName, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(agentName) })

	// Register the pane as an agent under the gc session name BEFORE the pump
	// subscribes. The stream builds its pane-to-session map from herdr's agent
	// registry, and herdr 0.8.0 registers nothing for a raw-command pane, so
	// without this every frame for this pane arrives unattributed and the pump
	// drops it by design.
	herdrtest.ReportAgent(t, session, agentName, "working", func() string { return herdrLivePaneID(p, agentName) })

	pokeCh := make(chan struct{}, 1)
	pump := newSessionEventPump(ctx, pokeCh, &bytes.Buffer{}, "live")
	// Park resync pokes outside the test window so the only poke observed
	// below is the attributed process-exit one.
	pump.resyncDelay = time.Minute
	pump.restart(p)
	if !pump.streaming() {
		t.Fatal("pump not streaming against live herdr")
	}

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

	if err := os.WriteFile(exitFlag, nil, 0o600); err != nil {
		t.Fatalf("signaling the agent to exit: %v", err)
	}
	start := time.Now()
	select {
	case <-pokeCh:
		t.Logf("process exit → reconcile poke in %v", time.Since(start).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("no reconcile poke after the agent process exited")
	}
}

// herdrLivePaneID resolves the pane herdr bound to a gc session by reading the
// sidecar binding the provider writes at Start. It deliberately does NOT ask
// `herdr agent list`: from herdr 0.8.0 that registry holds only panes with a
// registered agent, and these fixtures start a raw command rather than an agent
// kind, so the pane has no registration until this file creates one. Resolving
// the pane through the registry is therefore circular -- it needs the
// registration it exists to make possible. "GC_HERDR_PANE_ID" is
// internal/runtime/herdr's metaBoundPane, the same value the provider's own
// lookups read. Returns "" until the binding lands, so callers can poll.
func herdrLivePaneID(p *herdr.Provider, agentName string) string {
	pane, err := p.GetMeta(agentName, "GC_HERDR_PANE_ID")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(pane)
}
