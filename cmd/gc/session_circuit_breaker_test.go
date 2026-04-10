package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// breakerAt is a tiny helper that returns a breaker with explicit config
// for tests so we can use fake clocks freely.
func breakerAt(window time.Duration, maxRestarts int) *sessionCircuitBreaker {
	return newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      window,
		MaxRestarts: maxRestarts,
	})
}

func TestSessionCircuitBreaker_TrippingAndStaying(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	type step struct {
		kind     string // "restart" or "progress" or "isopen"
		offset   time.Duration
		wantOpen bool
	}
	tests := []struct {
		name    string
		window  time.Duration
		maxRest int
		steps   []step
	}{
		{
			name:    "5 restarts in 30m with no progress trips breaker",
			window:  30 * time.Minute,
			maxRest: 5,
			steps: []step{
				{"restart", 0, false},
				{"restart", 1 * time.Minute, false},
				{"restart", 2 * time.Minute, false},
				{"restart", 3 * time.Minute, false},
				{"restart", 4 * time.Minute, false},
				// Sixth restart exceeds max=5 -> CIRCUIT_OPEN.
				{"restart", 5 * time.Minute, true},
				{"isopen", 6 * time.Minute, true},
			},
		},
		{
			name:    "progress inside window keeps breaker CLOSED",
			window:  30 * time.Minute,
			maxRest: 5,
			steps: []step{
				{"restart", 0, false},
				{"restart", 1 * time.Minute, false},
				{"progress", 2 * time.Minute, false},
				{"restart", 3 * time.Minute, false},
				{"restart", 4 * time.Minute, false},
				{"restart", 5 * time.Minute, false},
				{"restart", 6 * time.Minute, false},
				{"isopen", 7 * time.Minute, false},
			},
		},
		{
			name:    "restarts spread beyond window never trip",
			window:  30 * time.Minute,
			maxRest: 5,
			steps: []step{
				{"restart", 0, false},
				{"restart", 10 * time.Minute, false},
				{"restart", 20 * time.Minute, false},
				{"restart", 31 * time.Minute, false}, // oldest trimmed
				{"restart", 42 * time.Minute, false}, // oldest trimmed
				{"restart", 53 * time.Minute, false}, // oldest trimmed
				{"isopen", 60 * time.Minute, false},
			},
		},
		{
			name:    "stale progress (outside window) does not save us",
			window:  30 * time.Minute,
			maxRest: 5,
			steps: []step{
				{"progress", 0, false},               // recorded, then becomes stale
				{"restart", 45 * time.Minute, false}, // progress is now 45m old, outside 30m
				{"restart", 46 * time.Minute, false},
				{"restart", 47 * time.Minute, false},
				{"restart", 48 * time.Minute, false},
				{"restart", 49 * time.Minute, false},
				{"restart", 50 * time.Minute, true}, // trip
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cb := breakerAt(tc.window, tc.maxRest)
			const id = "gastown/mayor"
			for i, s := range tc.steps {
				at := t0.Add(s.offset)
				switch s.kind {
				case "restart":
					got := cb.RecordRestart(id, at) == circuitOpen
					if got != s.wantOpen {
						t.Fatalf("step %d restart: wantOpen=%v got=%v", i, s.wantOpen, got)
					}
				case "progress":
					cb.RecordProgress(id, at)
				case "isopen":
					got := cb.IsOpen(id, at)
					if got != s.wantOpen {
						t.Fatalf("step %d isopen: wantOpen=%v got=%v", i, s.wantOpen, got)
					}
				default:
					t.Fatalf("unknown step kind %q", s.kind)
				}
			}
		})
	}
}

func TestSessionCircuitBreaker_AutoResetAfterSilence(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 5,
		// ResetAfter defaults to 2 * Window = 60 minutes.
	})
	const id = "gastown/mayor"

	// Trip the breaker with 6 rapid restarts.
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}
	if !cb.IsOpen(id, t0.Add(6*time.Minute)) {
		t.Fatalf("precondition: breaker should be open after 6 restarts")
	}

	// 59 minutes of silence: still OPEN.
	if !cb.IsOpen(id, t0.Add(5*time.Minute+59*time.Minute)) {
		t.Fatalf("breaker should stay OPEN until 2 x window of silence")
	}

	// 60 minutes since last restart (last restart was at t0+5m, so probe at t0+65m):
	// silence interval == 60m == 2 * window, breaker auto-resets to CLOSED.
	if cb.IsOpen(id, t0.Add(5*time.Minute+60*time.Minute)) {
		t.Fatalf("breaker should auto-reset to CLOSED after 60m of silence")
	}

	// After reset, new restarts accumulate fresh — so we can't trip with just 1.
	if got := cb.RecordRestart(id, t0.Add(5*time.Minute+61*time.Minute)); got == circuitOpen {
		t.Fatalf("post-reset: single restart should not re-open breaker, got %v", got)
	}
}

func TestSessionCircuitBreaker_ManualReset(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "gastown/mayor"
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}
	if !cb.IsOpen(id, t0.Add(6*time.Minute)) {
		t.Fatalf("precondition: should be OPEN")
	}
	// Manual reset (the hook a future `gc session reset` CLI would call).
	cb.Reset(id)
	if cb.IsOpen(id, t0.Add(6*time.Minute)) {
		t.Fatalf("after Reset, breaker should be CLOSED")
	}
}

func TestSessionCircuitBreaker_LogOpenOnce(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "gastown/mayor"
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}
	var buf bytes.Buffer
	cb.LogOpenOnce(id, &buf)
	first := buf.String()
	if !strings.Contains(first, "CIRCUIT_OPEN") {
		t.Fatalf("expected CIRCUIT_OPEN message, got %q", first)
	}
	if !strings.Contains(first, "gc session reset") {
		t.Fatalf("expected reset instructions in log, got %q", first)
	}
	if !strings.Contains(first, id) {
		t.Fatalf("expected identity in log, got %q", first)
	}
	// Second call is a no-op.
	cb.LogOpenOnce(id, &buf)
	if buf.String() != first {
		t.Fatalf("LogOpenOnce should only log once per OPEN incident, got repeat: %q", buf.String())
	}
}

func TestSessionCircuitBreaker_Snapshot(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	cb.RecordRestart("gastown/mayor", t0)
	cb.RecordRestart("gastown/refinery", t0.Add(1*time.Minute))
	snap := cb.Snapshot(t0.Add(2 * time.Minute))
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Identity != "gastown/mayor" || snap[1].Identity != "gastown/refinery" {
		t.Fatalf("snapshot not sorted: %+v", snap)
	}
	for _, s := range snap {
		if s.State != "CIRCUIT_CLOSED" {
			t.Fatalf("expected CLOSED, got %s for %s", s.State, s.Identity)
		}
	}
}

func TestSessionCircuitBreaker_ObserveProgressSignature(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "gastown/mayor"
	// First observation seeds the signature — no progress event.
	cb.ObserveProgressSignature(id, "sig-1", t0)
	// Trip the breaker: 6 restarts with no progress.
	for i := 0; i < 5; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}
	// Same signature -> no progress recorded.
	cb.ObserveProgressSignature(id, "sig-1", t0.Add(5*time.Minute+30*time.Second))
	if got := cb.RecordRestart(id, t0.Add(5*time.Minute+40*time.Second)); got != circuitOpen {
		t.Fatalf("expected circuitOpen on 6th restart with no progress, got %v", got)
	}
}

func TestComputeNamedSessionProgressSignatures(t *testing.T) {
	sessionBeads := []beads.Bead{
		{
			ID: "sb-1",
			Metadata: map[string]string{
				"session_name":               "mayor",
				namedSessionIdentityMetadata: "gastown/mayor",
			},
		},
		{
			ID: "sb-2",
			Metadata: map[string]string{
				"session_name": "worker-1",
				// not a named session — no identity
			},
		},
	}
	work := []beads.Bead{
		{ID: "wb-1", Assignee: "gastown/mayor", Status: "open"},
		{ID: "wb-2", Assignee: "mayor", Status: "in_progress"},
		{ID: "wb-3", Assignee: "worker-1", Status: "open"}, // ignored: not named
	}
	got := computeNamedSessionProgressSignatures(sessionBeads, work)
	if _, ok := got["gastown/mayor"]; !ok {
		t.Fatalf("expected signature for mayor, got keys=%v", got)
	}
	if _, ok := got["worker-1"]; ok {
		t.Fatalf("worker-1 is not a named session, should not be in signatures")
	}

	// Changing a work bead's status should change the signature.
	work2 := []beads.Bead{
		{ID: "wb-1", Assignee: "gastown/mayor", Status: "closed"},
		{ID: "wb-2", Assignee: "mayor", Status: "in_progress"},
	}
	got2 := computeNamedSessionProgressSignatures(sessionBeads, work2)
	if got["gastown/mayor"] == got2["gastown/mayor"] {
		t.Fatalf("signature should change when assignee bead status changes")
	}
}

// TestReconciler_CircuitOpenBlocksSpawn verifies that a named session with
// an OPEN breaker is NOT added to startCandidates and is NOT spawned.
func TestReconciler_CircuitOpenBlocksSpawn(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}

	// Inject a breaker with aggressive thresholds and pre-trip it for the mayor.
	cb := breakerAt(30*time.Minute, 5)
	const identity = "gastown/mayor"
	base := env.clk.Now().UTC()
	for i := 0; i < 6; i++ {
		cb.RecordRestart(identity, base.Add(-time.Duration(6-i)*time.Minute))
	}
	if !cb.IsOpen(identity, base) {
		t.Fatalf("precondition: breaker should be OPEN")
	}
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

	// Register the mayor as desired (and NOT running).
	env.addDesired("mayor", "mayor", false)

	// Create a session bead tagged as the named-session canonical bead.
	b, err := env.store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "mayor",
			"agent_name":                 "mayor",
			"template":                   "mayor",
			"state":                      "creating",
			"live_hash":                  runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	// Run the reconciler. With the breaker OPEN the mayor must not be started.
	_ = env.reconcile([]beads.Bead{b})

	if env.sp.IsRunning("mayor") {
		t.Fatalf("mayor should NOT be running: circuit breaker is OPEN")
	}
	if !strings.Contains(env.stderr.String(), "CIRCUIT_OPEN") {
		t.Fatalf("expected CIRCUIT_OPEN log in stderr, got: %q", env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "gc session reset") {
		t.Fatalf("expected reset instructions in stderr, got: %q", env.stderr.String())
	}
}

// TestReconciler_CircuitClosedAllowsSpawn is the control case: without any
// prior restart history the breaker is CLOSED and the reconciler spawns the
// named session normally.
func TestReconciler_CircuitClosedAllowsSpawn(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}

	cb := breakerAt(30*time.Minute, 5)
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

	env.addDesired("mayor", "mayor", false)

	b, err := env.store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "mayor",
			"agent_name":                 "mayor",
			"template":                   "mayor",
			"state":                      "creating",
			"live_hash":                  runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "gastown/mayor",
		},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	_ = env.reconcile([]beads.Bead{b})

	if strings.Contains(env.stderr.String(), "CIRCUIT_OPEN") {
		t.Fatalf("did not expect CIRCUIT_OPEN log, got: %q", env.stderr.String())
	}
	// Breaker should now have exactly one restart recorded.
	snap := cb.Snapshot(env.clk.Now().UTC())
	var found bool
	for _, s := range snap {
		if s.Identity == "gastown/mayor" {
			found = true
			if s.RestartCount != 1 {
				t.Fatalf("expected 1 recorded restart, got %d", s.RestartCount)
			}
		}
	}
	if !found {
		t.Fatalf("expected mayor in snapshot, got %+v", snap)
	}
}
