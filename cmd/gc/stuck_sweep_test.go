package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// stuckSweepFreshWisps marshals a canned set of in-progress wisp beads
// into the shape emitted by `bd list --json`, so sweepWispFreshness
// can decode them without a real bd subprocess.
func stuckSweepFreshnessRunner(t *testing.T, entries []freshnessEntry) beads.CommandRunner {
	t.Helper()
	out, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal freshness entries: %v", err)
	}
	return func(_, _ string, _ ...string) ([]byte, error) {
		return out, nil
	}
}

// newStuckSweepRuntime builds a minimal CityRuntime wired with a stuck
// tracker, a fake session provider, an in-memory bead store, and a
// recording event recorder. Each call site supplies the runner.
func newStuckSweepRuntime(t *testing.T, daemon config.DaemonConfig, runner beads.CommandRunner, rec events.Recorder) (*CityRuntime, *runtime.Fake, *beads.MemStore) {
	t.Helper()
	tr, err := newStuckTracker(daemon)
	if err != nil {
		t.Fatalf("newStuckTracker: %v", err)
	}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{Daemon: daemon},
		sp:                  sp,
		standaloneCityStore: store,
		stuck:               tr,
		runner:              runner,
		rec:                 rec,
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc start",
	}
	return cr, sp, store
}

// enabledDaemon returns a DaemonConfig with the stuck sweep enabled and
// a single pattern so StuckSweepEnabled() reports true.
func enabledDaemon() config.DaemonConfig {
	peekLines := 50
	return config.DaemonConfig{
		StuckSweep:         true,
		StuckErrorPatterns: []string{`(?i)rate limit`},
		StuckWispThreshold: "10m",
		StuckPeekLines:     &peekLines,
		StuckWarrantLabel:  "pool:dog",
	}
}

// TestStuckSweep_NoAgentsConfigured asserts SDK self-sufficiency (AC16/E21):
// with zero [[agent]] entries the sweep completes cleanly — no panic,
// no warrants — provided ListRunning is empty.
func TestStuckSweep_NoAgentsConfigured(t *testing.T) {
	// ListRunning will be empty since no sessions are Start()-ed.
	runner := stuckSweepFreshnessRunner(t, nil)
	var rec memRecorder
	cr, _, store := newStuckSweepRuntime(t, enabledDaemon(), runner, &rec)
	// No [[agent]] entries in cfg.Agents — zero SDK-role dependency.
	if len(cr.cfg.Agents) != 0 {
		t.Fatalf("precondition: Agents must be empty, got %d", len(cr.cfg.Agents))
	}

	cr.runStuckSweep(context.Background(), time.Now())

	warrants, err := store.ListByLabel("pool:dog", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(warrants) != 0 {
		t.Fatalf("no-agents sweep must file zero warrants, got %d", len(warrants))
	}
	if rec.hasType(events.AgentStuck) {
		t.Fatal("no-agents sweep must emit zero agent.stuck events")
	}
}

// TestStuckSweep_IdempotenceAcrossTicks asserts E11: running the sweep
// twice against the same stuck session produces exactly one warrant.
func TestStuckSweep_IdempotenceAcrossTicks(t *testing.T) {
	now := time.Now()
	wispOpenedAt := now.Add(-30 * time.Minute) // stale beyond 10m threshold
	entries := []freshnessEntry{{
		ID:        "bd-1",
		UpdatedAt: wispOpenedAt.Format(time.RFC3339),
		Metadata:  map[string]string{"session_name": "worker-1"},
	}}
	runner := stuckSweepFreshnessRunner(t, entries)
	var rec memRecorder
	cr, sp, store := newStuckSweepRuntime(t, enabledDaemon(), runner, &rec)

	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("sp.Start: %v", err)
	}
	sp.SetPeekOutput("worker-1", "HTTP 429: rate limit exceeded\n")
	sp.SetActivity("worker-1", now.Add(-20*time.Minute))

	cr.runStuckSweep(context.Background(), now)
	cr.runStuckSweep(context.Background(), now.Add(time.Second))

	warrants, err := store.ListByLabel("pool:dog", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(warrants) != 1 {
		t.Fatalf("expected exactly 1 warrant after two sweeps, got %d", len(warrants))
	}
	if got := warrants[0].Metadata["target"]; got != "worker-1" {
		t.Fatalf("warrant target = %q, want worker-1", got)
	}
}

// TestStuckSweep_HaltGateSuppresses asserts E14: with the halt flag set,
// tick() short-circuits before the sweep runs, so no warrant is filed
// even when a genuinely stuck session is present.
func TestStuckSweep_HaltGateSuppresses(t *testing.T) {
	now := time.Now()
	wispOpenedAt := now.Add(-30 * time.Minute)
	entries := []freshnessEntry{{
		ID:        "bd-1",
		UpdatedAt: wispOpenedAt.Format(time.RFC3339),
		Metadata:  map[string]string{"session_name": "worker-1"},
	}}
	runner := stuckSweepFreshnessRunner(t, entries)
	var rec memRecorder
	cr, sp, store := newStuckSweepRuntime(t, enabledDaemon(), runner, &rec)

	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("sp.Start: %v", err)
	}
	sp.SetPeekOutput("worker-1", "rate limit exceeded")
	sp.SetActivity("worker-1", now.Add(-20*time.Minute))

	// Write the halt file so the in-sweep defense-in-depth halt gate
	// short-circuits. We then call runStuckSweep directly and assert no
	// warrant is filed — exercising the halt gate rather than skipping it.
	if err := writeHaltFile(cr.cityPath); err != nil {
		t.Fatalf("writeHaltFile: %v", err)
	}
	cr.runStuckSweep(context.Background(), now)

	warrants, err := store.ListByLabel("pool:dog", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(warrants) != 0 {
		t.Fatalf("halted tick must file zero warrants, got %d", len(warrants))
	}
	if rec.hasType(events.AgentStuck) {
		t.Fatal("halted tick must emit zero agent.stuck events")
	}
}

// TestStuckSweep_WispQueryFailsOpenSweep asserts E24/AC18: when the
// bd-list runner returns an error, the whole sweep fails open — no
// panic, no warrants filed.
func TestStuckSweep_WispQueryFailsOpenSweep(t *testing.T) {
	var stderr bytes.Buffer
	failingRunner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("bd unavailable")
	}
	var rec memRecorder
	cr, sp, store := newStuckSweepRuntime(t, enabledDaemon(), failingRunner, &rec)
	cr.stderr = &stderr

	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("sp.Start: %v", err)
	}
	sp.SetPeekOutput("worker-1", "rate limit exceeded")
	sp.SetActivity("worker-1", time.Now().Add(-time.Hour))

	// Must not panic.
	cr.runStuckSweep(context.Background(), time.Now())

	warrants, err := store.ListByLabel("pool:dog", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(warrants) != 0 {
		t.Fatalf("fail-open sweep must file zero warrants, got %d", len(warrants))
	}
	if rec.hasType(events.AgentStuck) {
		t.Fatal("fail-open sweep must emit zero agent.stuck events")
	}
	if !strings.Contains(stderr.String(), "stuck sweep:") {
		t.Fatalf("expected warn log on stderr, got %q", stderr.String())
	}
}

// TestStuckSweep_EventOnCreateSuccess asserts AC17: an agent.stuck event
// is emitted exactly once when a warrant bead is successfully created.
func TestStuckSweep_EventOnCreateSuccess(t *testing.T) {
	now := time.Now()
	wispOpenedAt := now.Add(-30 * time.Minute)
	entries := []freshnessEntry{{
		ID:        "bd-1",
		UpdatedAt: wispOpenedAt.Format(time.RFC3339),
		Metadata:  map[string]string{"session_name": "worker-1"},
	}}
	runner := stuckSweepFreshnessRunner(t, entries)
	var rec memRecorder
	cr, sp, store := newStuckSweepRuntime(t, enabledDaemon(), runner, &rec)

	if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("sp.Start: %v", err)
	}
	sp.SetPeekOutput("worker-1", "HTTP 429: rate limit exceeded\n")
	sp.SetActivity("worker-1", now.Add(-20*time.Minute))

	cr.runStuckSweep(context.Background(), now)

	// Exactly one warrant, exactly one agent.stuck event on Create success.
	warrants, err := store.ListByLabel("pool:dog", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(warrants) != 1 {
		t.Fatalf("expected exactly 1 warrant, got %d", len(warrants))
	}
	count := 0
	for _, e := range rec.events {
		if e.Type == events.AgentStuck && e.Subject == "worker-1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 agent.stuck event on Create success, got %d", count)
	}
}
