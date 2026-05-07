package main

import (
	"bytes"
	"context"
	"reflect"
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
			name:    "6th restart inside 30m with no progress trips breaker",
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
			const id = "rig-a/session-a"
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

func TestSessionCircuitBreaker_AutoResetAfterCooldown(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 5,
		// ResetAfter defaults to 2 * Window = 60 minutes.
	})
	const id = "rig-a/session-a"

	// Trip the breaker with 6 rapid restarts.
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}
	if !cb.IsOpen(id, t0.Add(6*time.Minute)) {
		t.Fatalf("precondition: breaker should be open after 6 restarts")
	}

	// 59 minutes of cooldown: still OPEN.
	if !cb.IsOpen(id, t0.Add(5*time.Minute+59*time.Minute)) {
		t.Fatalf("breaker should stay OPEN until 2 x window cooldown")
	}

	// 60 minutes since last restart (last restart was at t0+5m, so probe at t0+65m):
	// cooldown interval == 60m == 2 * window, breaker auto-resets to CLOSED.
	if cb.IsOpen(id, t0.Add(5*time.Minute+60*time.Minute)) {
		t.Fatalf("breaker should auto-reset to CLOSED after 60m cooldown")
	}

	// After reset, new restarts accumulate fresh — so we can't trip with just 1.
	if got := cb.RecordRestart(id, t0.Add(5*time.Minute+61*time.Minute)); got == circuitOpen {
		t.Fatalf("post-reset: single restart should not re-open breaker, got %v", got)
	}
}

func TestSessionCircuitBreaker_AutoResetClearsProgressSignature(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"

	cb.ObserveProgressSignature(id, "assigned-work-before-open", t0)
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}
	if !cb.IsOpen(id, t0.Add(6*time.Minute)) {
		t.Fatalf("precondition: breaker should be open after 6 restarts")
	}

	resetAt := t0.Add(65 * time.Minute)
	if cb.IsOpen(id, resetAt) {
		t.Fatalf("breaker should auto-reset to CLOSED after cooldown")
	}
	cb.ObserveProgressSignature(id, "assigned-work-after-reset", resetAt.Add(time.Minute))
	for i := 0; i < 6; i++ {
		state := cb.RecordRestart(id, resetAt.Add(time.Duration(i+2)*time.Minute))
		if i == 5 && state != circuitOpen {
			t.Fatalf("expected breaker to re-open after reset with no post-reset progress, got %v", state)
		}
	}
}

func TestSessionCircuitBreaker_ManualReset(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"
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
	const id = "rig-a/session-a"
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
	cb.RecordRestart("rig-a/session-a", t0)
	cb.RecordRestart("rig-a/session-b", t0.Add(1*time.Minute))
	snap := cb.Snapshot(t0.Add(2 * time.Minute))
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Identity != "rig-a/session-a" || snap[1].Identity != "rig-a/session-b" {
		t.Fatalf("snapshot not sorted: %+v", snap)
	}
	for _, s := range snap {
		if s.State != "CIRCUIT_CLOSED" {
			t.Fatalf("expected CLOSED, got %s for %s", s.State, s.Identity)
		}
	}
}

func TestSessionCircuitBreaker_SnapshotTrimsExpiredRestartWindow(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	cb.RecordRestart("rig-a/session-a", t0)
	cb.RecordRestart("rig-a/session-a", t0.Add(time.Minute))

	snap := cb.Snapshot(t0.Add(32 * time.Minute))
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if snap[0].RestartCount != 0 {
		t.Fatalf("restart count = %d, want expired entries trimmed", snap[0].RestartCount)
	}
	if !snap[0].WindowStart.IsZero() {
		t.Fatalf("window start = %v, want zero after all entries expire", snap[0].WindowStart)
	}
}

func TestSessionCircuitBreaker_SnapshotPreservesOpenRestartCountAfterWindowExpires(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 5,
		ResetAfter:  3 * time.Hour,
	})
	const id = "rig-a/session-a"
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}

	snap := cb.Snapshot(t0.Add(40 * time.Minute))
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if snap[0].State != "CIRCUIT_OPEN" {
		t.Fatalf("state = %q, want CIRCUIT_OPEN", snap[0].State)
	}
	if snap[0].RestartCount != 0 {
		t.Fatalf("restart count = %d, want expired rolling count", snap[0].RestartCount)
	}
	if snap[0].OpenRestartCount != 6 {
		t.Fatalf("open restart count = %d, want trip-time count", snap[0].OpenRestartCount)
	}
}

func TestSessionCircuitBreaker_RecordRestartDoesNotMutateOpenEntry(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 5,
		ResetAfter:  3 * time.Hour,
	})
	const id = "rig-a/session-a"
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}
	before := cb.Snapshot(t0.Add(6 * time.Minute))[0]

	if got := cb.RecordRestart(id, t0.Add(10*time.Minute)); got != circuitOpen {
		t.Fatalf("RecordRestart while open = %v, want circuitOpen", got)
	}
	after := cb.Snapshot(t0.Add(10 * time.Minute))[0]
	if !after.LastRestart.Equal(before.LastRestart) {
		t.Fatalf("last restart changed from %v to %v while open", before.LastRestart, after.LastRestart)
	}
	if after.RestartCount != before.RestartCount {
		t.Fatalf("restart count changed from %d to %d while open", before.RestartCount, after.RestartCount)
	}
}

func TestSessionCircuitBreaker_ObserveEmptyProgressSignatureDoesNotCreateIdleEntry(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)

	cb.ObserveProgressSignature("rig-a/session-a", "", t0)

	if snap := cb.Snapshot(t0); len(snap) != 0 {
		t.Fatalf("snapshot len = %d, want no idle entry: %+v", len(snap), snap)
	}
}

func TestSessionCircuitBreaker_PruneIdleProgressOnlyEntry(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:     30 * time.Minute,
		ResetAfter: time.Hour,
	})

	cb.ObserveProgressSignature("rig-a/session-a", "assigned-work", t0)
	if snap := cb.Snapshot(t0); len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want one seeded progress entry: %+v", len(snap), snap)
	}

	cb.pruneIdle(t0.Add(time.Hour))
	if snap := cb.Snapshot(t0.Add(time.Hour)); len(snap) != 0 {
		t.Fatalf("snapshot len = %d, want stale progress-only entry pruned: %+v", len(snap), snap)
	}
}

func TestSessionCircuitBreaker_ObserveProgressSignature(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"
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

func TestSessionCircuitBreaker_EmptyToAssignedWorkSignatureCountsAsProgress(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"

	cb.RecordRestart(id, t0)
	cb.ObserveProgressSignature(id, "", t0.Add(time.Second))
	for i := 1; i < 5; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}

	if changed := cb.ObserveProgressSignature(id, "new-assigned-work", t0.Add(25*time.Minute)); !changed {
		t.Fatal("empty-to-non-empty signature transition after an observation should be recorded")
	}
	if got := cb.RecordRestart(id, t0.Add(26*time.Minute)); got != circuitClosed {
		t.Fatalf("newly assigned work should keep breaker closed on threshold restart, got %v", got)
	}
	snap := cb.Snapshot(t0.Add(26 * time.Minute))
	if len(snap) != 1 || !snap[0].LastProgress.Equal(t0.Add(25*time.Minute)) {
		t.Fatalf("last progress = %+v, want transition timestamp", snap)
	}
}

func TestSessionCircuitBreaker_NonEmptyToEmptySignatureCountsAsProgress(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"

	cb.ObserveProgressSignature(id, "assigned-work", t0)
	for i := 0; i < 5; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}

	if changed := cb.ObserveProgressSignature(id, "", t0.Add(25*time.Minute)); !changed {
		t.Fatal("non-empty-to-empty signature transition should be recorded")
	}
	if got := cb.RecordRestart(id, t0.Add(26*time.Minute)); got != circuitClosed {
		t.Fatalf("completed work should keep breaker closed on threshold restart, got %v", got)
	}
}

func TestSessionCircuitBreaker_RestoreFromMetadata(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	openMeta := func() map[string]string {
		return map[string]string{
			sessionCircuitStateMetadata:             circuitOpen.String(),
			sessionCircuitRestartsMetadata:          `["2026-04-01T12:00:00Z","2026-04-01T12:01:00Z"]`,
			sessionCircuitLastRestartMetadata:       t0.Add(time.Minute).Format(time.RFC3339Nano),
			sessionCircuitLastProgressMetadata:      t0.Add(-time.Hour).Format(time.RFC3339Nano),
			sessionCircuitLastObservedMetadata:      t0.Add(2 * time.Minute).Format(time.RFC3339Nano),
			sessionCircuitProgressSignatureMetadata: "assigned-work",
			sessionCircuitOpenedAtMetadata:          t0.Add(2 * time.Minute).Format(time.RFC3339Nano),
			sessionCircuitOpenRestartCountMetadata:  "6",
		}
	}

	tests := []struct {
		name      string
		meta      map[string]string
		now       time.Time
		wantReset bool
		wantSnap  bool
		wantState circuitBreakerStateKind
		wantErr   string
	}{
		{
			name: "empty metadata short-circuits",
			meta: map[string]string{},
			now:  t0,
		},
		{
			name:      "valid open restores",
			meta:      openMeta(),
			now:       t0.Add(3 * time.Minute),
			wantSnap:  true,
			wantState: circuitOpen,
		},
		{
			name:      "stale open auto-resets",
			meta:      openMeta(),
			now:       t0.Add(2 * time.Hour),
			wantReset: true,
			wantSnap:  true,
			wantState: circuitClosed,
		},
		{
			name:    "malformed restart json errors",
			meta:    mapWith(openMeta(), sessionCircuitRestartsMetadata, "not-json"),
			now:     t0,
			wantErr: "parsing session_circuit_restarts",
		},
		{
			name:    "invalid timestamp errors",
			meta:    mapWith(openMeta(), sessionCircuitLastRestartMetadata, "not-time"),
			now:     t0,
			wantErr: "parsing session_circuit_last_restart",
		},
		{
			name:    "invalid open restart count errors",
			meta:    mapWith(openMeta(), sessionCircuitOpenRestartCountMetadata, "NaN"),
			now:     t0,
			wantErr: "parsing session_circuit_open_restart_count",
		},
		{
			name:    "unknown state errors",
			meta:    mapWith(openMeta(), sessionCircuitStateMetadata, "BROKEN"),
			now:     t0,
			wantErr: "unknown state",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cb := breakerAt(30*time.Minute, 5)
			gotReset, err := cb.restoreFromMetadata("rig-a/session-a", tc.meta, tc.now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("restoreFromMetadata error = %v, want containing %q", err, tc.wantErr)
				}
				if snap := cb.Snapshot(tc.now); len(snap) != 0 {
					t.Fatalf("snapshot after failed restore = %+v, want empty", snap)
				}
				return
			}
			if err != nil {
				t.Fatalf("restoreFromMetadata: %v", err)
			}
			if gotReset != tc.wantReset {
				t.Fatalf("auto-reset = %v, want %v", gotReset, tc.wantReset)
			}
			snap := cb.Snapshot(tc.now)
			if !tc.wantSnap {
				if len(snap) != 0 {
					t.Fatalf("snapshot = %+v, want empty", snap)
				}
				return
			}
			if len(snap) != 1 || snap[0].State != tc.wantState.String() {
				t.Fatalf("snapshot = %+v, want one %s entry", snap, tc.wantState)
			}
		})
	}
}

func TestSessionCircuitBreaker_RestoreFromMetadataDuplicateIsNoOp(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"
	cb.RecordRestart(id, t0)

	reset, err := cb.restoreFromMetadata(id, map[string]string{
		sessionCircuitStateMetadata:             circuitOpen.String(),
		sessionCircuitRestartsMetadata:          `["2026-04-01T12:00:00Z"]`,
		sessionCircuitLastRestartMetadata:       t0.Format(time.RFC3339Nano),
		sessionCircuitLastObservedMetadata:      t0.Format(time.RFC3339Nano),
		sessionCircuitProgressSignatureMetadata: "assigned-work",
		sessionCircuitOpenedAtMetadata:          t0.Format(time.RFC3339Nano),
		sessionCircuitOpenRestartCountMetadata:  "6",
	}, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("restoreFromMetadata: %v", err)
	}
	if reset {
		t.Fatal("duplicate restore should not report auto-reset")
	}
	snap := cb.Snapshot(t0.Add(time.Minute))
	if len(snap) != 1 || snap[0].State != circuitClosed.String() || snap[0].RestartCount != 1 {
		t.Fatalf("duplicate restore overwrote existing entry: %+v", snap)
	}
}

func TestSessionCircuitBreaker_MetadataRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	const id = "rig-a/session-a"
	cb := breakerAt(30*time.Minute, 5)
	cb.ObserveProgressSignature(id, "assigned-work", t0)
	for i := 0; i < 6; i++ {
		cb.RecordRestart(id, t0.Add(time.Duration(i)*time.Minute))
	}

	metadata, err := cb.metadata(id, t0.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	restored := breakerAt(30*time.Minute, 5)
	if reset, err := restored.restoreFromMetadata(id, metadata, t0.Add(6*time.Minute)); err != nil || reset {
		t.Fatalf("restoreFromMetadata reset=%v err=%v", reset, err)
	}
	got, err := restored.metadata(id, t0.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("metadata after restore: %v", err)
	}
	if !reflect.DeepEqual(got, metadata) {
		t.Fatalf("metadata round trip mismatch\ngot:  %#v\nwant: %#v", got, metadata)
	}
}

func TestPersistSessionCircuitBreakerMetadataSkipsUnchangedSnapshot(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	store := &metadataCountingStore{Store: beads.NewMemStore()}
	session, err := store.Create(beads.Bead{
		Title:    "session-a",
		Type:     sessionBeadType,
		Metadata: map[string]string{namedSessionIdentityMetadata: "rig-a/session-a"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"
	cb.RecordRestart(id, t0)

	if err := persistSessionCircuitBreakerMetadata(store, &session, cb, id, t0); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	if store.writes != 1 {
		t.Fatalf("metadata writes = %d, want 1", store.writes)
	}
	if err := persistSessionCircuitBreakerMetadata(store, &session, cb, id, t0); err != nil {
		t.Fatalf("second persist: %v", err)
	}
	if store.writes != 1 {
		t.Fatalf("unchanged metadata writes = %d, want still 1", store.writes)
	}

	cb.RecordRestart(id, t0.Add(time.Minute))
	if err := persistSessionCircuitBreakerMetadata(store, &session, cb, id, t0.Add(time.Minute)); err != nil {
		t.Fatalf("changed persist: %v", err)
	}
	if store.writes != 2 {
		t.Fatalf("changed metadata writes = %d, want 2", store.writes)
	}
}

func TestSessionCircuitMetadataHelpersIncludeResetGeneration(t *testing.T) {
	empty := emptySessionCircuitMetadata()
	if _, ok := empty[sessionCircuitResetGenerationMetadata]; !ok {
		t.Fatalf("empty metadata missing %s", sessionCircuitResetGenerationMetadata)
	}

	existing := emptySessionCircuitMetadata()
	next := emptySessionCircuitMetadata()
	existing[sessionCircuitResetGenerationMetadata] = "1"
	next[sessionCircuitResetGenerationMetadata] = "2"
	if sessionCircuitMetadataEqual(existing, next) {
		t.Fatalf("metadata equality ignored %s", sessionCircuitResetGenerationMetadata)
	}
	if hasSessionCircuitMetadata(next) {
		t.Fatalf("generation-only metadata should not restore a breaker entry")
	}
}

func TestPersistSessionCircuitBreakerMetadataWritesAutoResetClosedState(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title: "session-a",
		Type:  sessionBeadType,
		Metadata: map[string]string{
			namedSessionIdentityMetadata: "rig-a/session-a",
			sessionCircuitStateMetadata:  circuitOpen.String(),
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	cb := breakerAt(30*time.Minute, 5)
	const id = "rig-a/session-a"
	reset, err := cb.restoreFromMetadata(id, map[string]string{
		sessionCircuitStateMetadata:            circuitOpen.String(),
		sessionCircuitRestartsMetadata:         `["2026-04-01T12:00:00Z"]`,
		sessionCircuitLastRestartMetadata:      t0.Format(time.RFC3339Nano),
		sessionCircuitOpenedAtMetadata:         t0.Format(time.RFC3339Nano),
		sessionCircuitOpenRestartCountMetadata: "6",
	}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("restoreFromMetadata: %v", err)
	}
	if !reset {
		t.Fatal("restoreFromMetadata reset = false, want true")
	}
	if err := persistSessionCircuitBreakerMetadata(store, &session, cb, id, t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("persist: %v", err)
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("get updated session: %v", err)
	}
	if got := updated.Metadata[sessionCircuitStateMetadata]; got != circuitClosed.String() {
		t.Fatalf("persisted state = %q, want %q", got, circuitClosed.String())
	}
	if got := updated.Metadata[sessionCircuitOpenedAtMetadata]; got != "" {
		t.Fatalf("persisted opened_at = %q, want cleared", got)
	}
}

type metadataCountingStore struct {
	beads.Store
	writes int
}

func (s *metadataCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.writes++
	return s.Store.SetMetadataBatch(id, kvs)
}

func mapWith(in map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	out[key] = value
	return out
}

func TestComputeNamedSessionProgressSignatures(t *testing.T) {
	sessionBeads := []beads.Bead{
		{
			ID: "sb-1",
			Metadata: map[string]string{
				"session_name":               "session-a",
				namedSessionIdentityMetadata: "rig-a/session-a",
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
		{ID: "wb-1", Assignee: "rig-a/session-a", Status: "open"},
		{ID: "wb-2", Assignee: "session-a", Status: "in_progress"},
		{ID: "wb-3", Assignee: "worker-1", Status: "open"}, // ignored: not named
	}
	got := computeNamedSessionProgressSignatures(sessionBeads, work)
	if _, ok := got["rig-a/session-a"]; !ok {
		t.Fatalf("expected signature for session-a, got keys=%v", got)
	}
	if _, ok := got["worker-1"]; ok {
		t.Fatalf("worker-1 is not a named session, should not be in signatures")
	}

	// Changing a work bead's status should change the signature.
	work2 := []beads.Bead{
		{ID: "wb-1", Assignee: "rig-a/session-a", Status: "closed"},
		{ID: "wb-2", Assignee: "session-a", Status: "in_progress"},
	}
	got2 := computeNamedSessionProgressSignatures(sessionBeads, work2)
	if got["rig-a/session-a"] == got2["rig-a/session-a"] {
		t.Fatalf("signature should change when assignee bead status changes")
	}
}

func TestComputeNamedSessionProgressSignaturesSkipsAmbiguousBareKeys(t *testing.T) {
	sessionBeads := []beads.Bead{
		{
			ID: "sb-a",
			Metadata: map[string]string{
				"session_name":               "shared",
				"alias":                      "shared-alias",
				namedSessionIdentityMetadata: "rig-a/session",
			},
		},
		{
			ID: "sb-b",
			Metadata: map[string]string{
				"session_name":               "shared",
				"alias":                      "shared-alias",
				namedSessionIdentityMetadata: "rig-b/session",
			},
		},
	}
	work := []beads.Bead{
		{ID: "wb-name", Assignee: "shared", Status: "open"},
		{ID: "wb-alias", Assignee: "shared-alias", Status: "in_progress"},
	}

	got := computeNamedSessionProgressSignatures(sessionBeads, work)
	if got["rig-a/session"] != "" {
		t.Fatalf("rig-a signature = %q, want empty for ambiguous bare keys", got["rig-a/session"])
	}
	if got["rig-b/session"] != "" {
		t.Fatalf("rig-b signature = %q, want empty for ambiguous bare keys", got["rig-b/session"])
	}

	work = append(work, beads.Bead{ID: "wb-exact", Assignee: "rig-a/session", Status: "closed"})
	got = computeNamedSessionProgressSignatures(sessionBeads, work)
	if got["rig-a/session"] == "" {
		t.Fatal("exact identity assignment should still contribute a signature")
	}
	if got["rig-b/session"] != "" {
		t.Fatalf("rig-b signature = %q, want empty", got["rig-b/session"])
	}
}

func intPtrCircuit(n int) *int { return &n }

func configureAlwaysNamedSession(env *reconcilerTestEnv) {
	env.cfg = &config.City{
		Daemon: config.DaemonConfig{
			SessionCircuitBreaker:            true,
			SessionCircuitBreakerMaxRestarts: intPtrCircuit(5),
			SessionCircuitBreakerWindow:      "30m",
		},
		Agents: []config.Agent{{Name: "template-a", Dir: "rig-a"}},
		NamedSessions: []config.NamedSession{{
			Name:     "session-a",
			Template: "template-a",
			Dir:      "rig-a",
			Mode:     "always",
		}},
	}
}

func configureAlwaysNamedSessionWithoutCircuit(env *reconcilerTestEnv) {
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "template-a", Dir: "rig-a"}},
		NamedSessions: []config.NamedSession{{
			Name:     "session-a",
			Template: "template-a",
			Dir:      "rig-a",
			Mode:     "always",
		}},
	}
}

func createCircuitTestNamedSession(t *testing.T, env *reconcilerTestEnv, state string) beads.Bead {
	t.Helper()
	return createCircuitTestNamedSessionWithIdentity(t, env, "session-a", "template-a", "rig-a/session-a", state)
}

func createCircuitTestNamedSessionWithIdentity(
	t *testing.T,
	env *reconcilerTestEnv,
	name string,
	template string,
	identity string,
	state string,
) beads.Bead {
	t.Helper()
	b, err := env.store.Create(beads.Bead{
		Title:  name,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               name,
			"agent_name":                 name,
			"template":                   template,
			"state":                      state,
			"live_hash":                  runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}
	return b
}

func TestReconciler_CircuitDisabledByDefaultAllowsRepeatedWakeAttempts(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSessionWithoutCircuit(env)
	env.addDesired("session-a", "template-a", false)

	b := createCircuitTestNamedSession(t, env, "asleep")
	for i := 0; i < 6; i++ {
		current, err := env.store.Get(b.ID)
		if err != nil {
			t.Fatalf("get bead attempt %d: %v", i+1, err)
		}
		if woken := env.reconcile([]beads.Bead{current}); woken != 1 {
			t.Fatalf("attempt %d: woken = %d, want 1 with circuit disabled; stderr=%s", i+1, woken, env.stderr.String())
		}
		if err := env.sp.Stop("session-a"); err != nil {
			t.Fatalf("attempt %d: stop session-a: %v", i+1, err)
		}
		env.clk.Advance(6 * time.Minute)
	}
	if strings.Contains(env.stderr.String(), "CIRCUIT_OPEN") {
		t.Fatalf("circuit breaker should be disabled by default, stderr=%q", env.stderr.String())
	}
}

func TestReconciler_CircuitUsesConfiguredDaemonThresholds(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Daemon: config.DaemonConfig{
			SessionCircuitBreaker:            true,
			SessionCircuitBreakerMaxRestarts: intPtrCircuit(2),
			SessionCircuitBreakerWindow:      "30m",
		},
		Agents: []config.Agent{{Name: "template-a", Dir: "rig-a"}},
		NamedSessions: []config.NamedSession{{
			Name:     "session-a",
			Template: "template-a",
			Dir:      "rig-a",
			Mode:     "always",
		}},
	}
	env.addDesired("session-a", "template-a", false)
	b := createCircuitTestNamedSession(t, env, "asleep")

	for i := 0; i < 2; i++ {
		current, err := env.store.Get(b.ID)
		if err != nil {
			t.Fatalf("get bead attempt %d: %v", i+1, err)
		}
		if woken := env.reconcile([]beads.Bead{current}); woken != 1 {
			t.Fatalf("attempt %d: woken = %d, want 1 before configured threshold; stderr=%s", i+1, woken, env.stderr.String())
		}
		if err := env.sp.Stop("session-a"); err != nil {
			t.Fatalf("attempt %d: stop session-a: %v", i+1, err)
		}
		env.clk.Advance(6 * time.Minute)
	}

	current, err := env.store.Get(b.ID)
	if err != nil {
		t.Fatalf("get bead before trip: %v", err)
	}
	if woken := env.reconcile([]beads.Bead{current}); woken != 0 {
		t.Fatalf("third wake = %d, want 0 because configured max_restarts=2", woken)
	}
	if !strings.Contains(env.stderr.String(), "CIRCUIT_OPEN") {
		t.Fatalf("expected CIRCUIT_OPEN log with configured threshold, got %q", env.stderr.String())
	}
}

func TestReconciler_CircuitOpenStatePersistsAcrossControllerRestart(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	env.addDesired("session-a", "template-a", false)

	const identity = "rig-a/session-a"
	b := createCircuitTestNamedSession(t, env, "asleep")
	for i := 0; i < 6; i++ {
		current, err := env.store.Get(b.ID)
		if err != nil {
			t.Fatalf("get bead attempt %d: %v", i+1, err)
		}
		_ = env.reconcile([]beads.Bead{current})
		_ = env.sp.Stop("session-a")
		env.clk.Advance(6 * time.Minute)
	}

	persisted, err := env.store.Get(b.ID)
	if err != nil {
		t.Fatalf("get persisted session: %v", err)
	}
	if got := persisted.Metadata[sessionCircuitStateMetadata]; got != circuitOpen.String() {
		t.Fatalf("persisted circuit state = %q, want %q", got, circuitOpen.String())
	}
	if got := persisted.Metadata[sessionCircuitRestartsMetadata]; got == "" {
		t.Fatal("persisted circuit restart history is empty")
	}

	sessionCircuitBreakerMu.Lock()
	sessionCircuitBreakerSingleton = newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	sessionCircuitBreakerMu.Unlock()

	env.stderr.Reset()
	env.clk.Advance(time.Minute)
	if woken := env.reconcile([]beads.Bead{persisted}); woken != 0 {
		t.Fatalf("woken after singleton reset = %d, want 0 from persisted OPEN state", woken)
	}
	if env.sp.IsRunning("session-a") {
		t.Fatal("session-a should not be running after persisted CIRCUIT_OPEN restore")
	}
	if snap := sessionCircuitBreakerSnapshot(env.clk.Now().UTC()); len(snap) != 1 || snap[0].Identity != identity || snap[0].State != circuitOpen.String() {
		t.Fatalf("restored snapshot = %+v, want one OPEN entry for %s", snap, identity)
	}
}

// TestReconciler_CircuitOpenBlocksSpawn verifies that a named session with
// an OPEN breaker is NOT added to startCandidates and is NOT spawned.
func TestReconciler_CircuitOpenBlocksSpawn(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)

	// Inject a breaker with aggressive thresholds and pre-trip it.
	cb := breakerAt(30*time.Minute, 5)
	const identity = "rig-a/session-a"
	base := env.clk.Now().UTC()
	for i := 0; i < 6; i++ {
		cb.RecordRestart(identity, base.Add(-time.Duration(6-i)*time.Minute))
	}
	if !cb.IsOpen(identity, base) {
		t.Fatalf("precondition: breaker should be OPEN")
	}
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

	// Register the named session as desired (and NOT running).
	env.addDesired("session-a", "template-a", false)

	b := createCircuitTestNamedSession(t, env, "creating")

	// Run the reconciler. With the breaker OPEN the session must not be started.
	_ = env.reconcile([]beads.Bead{b})

	if env.sp.IsRunning("session-a") {
		t.Fatalf("session-a should NOT be running: circuit breaker is OPEN")
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
	configureAlwaysNamedSession(env)

	cb := breakerAt(30*time.Minute, 5)
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

	env.addDesired("session-a", "template-a", false)

	b := createCircuitTestNamedSession(t, env, "creating")

	_ = env.reconcile([]beads.Bead{b})

	if strings.Contains(env.stderr.String(), "CIRCUIT_OPEN") {
		t.Fatalf("did not expect CIRCUIT_OPEN log, got: %q", env.stderr.String())
	}
	// Breaker should now have exactly one restart recorded.
	snap := cb.Snapshot(env.clk.Now().UTC())
	var found bool
	for _, s := range snap {
		if s.Identity == "rig-a/session-a" {
			found = true
			if s.RestartCount != 1 {
				t.Fatalf("expected 1 recorded restart, got %d", s.RestartCount)
			}
		}
	}
	if !found {
		t.Fatalf("expected session-a in snapshot, got %+v", snap)
	}
}

func TestReconciler_CircuitDoesNotRecordRestartForDependencyBlockedNamedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Daemon: config.DaemonConfig{
			SessionCircuitBreaker:            true,
			SessionCircuitBreakerMaxRestarts: intPtrCircuit(5),
			SessionCircuitBreakerWindow:      "30m",
		},
		Agents: []config.Agent{
			{Name: "template-a", DependsOn: []string{"db"}},
			{Name: "db"},
		},
		NamedSessions: []config.NamedSession{{
			Name:     "session-a",
			Template: "template-a",
			Mode:     "always",
		}},
	}
	cb := breakerAt(30*time.Minute, 5)
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()
	env.addDesired("session-a", "template-a", false)
	b := createCircuitTestNamedSession(t, env, "asleep")

	if woken := env.reconcile([]beads.Bead{b}); woken != 0 {
		t.Fatalf("woken = %d, want 0 while dependency is blocked", woken)
	}
	if env.sp.IsRunning("session-a") {
		t.Fatal("session-a should not start while dependency is blocked")
	}
	for _, snap := range cb.Snapshot(env.clk.Now().UTC()) {
		if snap.Identity == "rig-a/session-a" && snap.RestartCount != 0 {
			t.Fatalf("restart count for dependency-blocked session = %d, want 0", snap.RestartCount)
		}
	}
}

func TestReconciler_CircuitDoesNotRecordRestartForWakeBudgetDeferredNamedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	maxWakes := 1
	env.cfg = &config.City{
		Daemon: config.DaemonConfig{
			MaxWakesPerTick:                  &maxWakes,
			SessionCircuitBreaker:            true,
			SessionCircuitBreakerMaxRestarts: intPtrCircuit(5),
			SessionCircuitBreakerWindow:      "30m",
		},
		Agents: []config.Agent{
			{Name: "template-a", Dir: "rig-a"},
			{Name: "template-b", Dir: "rig-a"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "session-a", Template: "template-a", Dir: "rig-a", Mode: "always"},
			{Name: "session-b", Template: "template-b", Dir: "rig-a", Mode: "always"},
		},
	}
	cb := breakerAt(30*time.Minute, 5)
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()
	env.addDesired("session-a", "template-a", false)
	env.addDesired("session-b", "template-b", false)
	sessionA := createCircuitTestNamedSession(t, env, "asleep")
	sessionB := createCircuitTestNamedSessionWithIdentity(t, env, "session-b", "template-b", "rig-a/session-b", "asleep")

	if woken := env.reconcile([]beads.Bead{sessionA, sessionB}); woken != 1 {
		t.Fatalf("woken = %d, want 1 under wake budget", woken)
	}
	if !env.sp.IsRunning("session-a") {
		t.Fatal("session-a should start before the wake budget is exhausted")
	}
	if env.sp.IsRunning("session-b") {
		t.Fatal("session-b should be deferred by wake budget")
	}
	counts := make(map[string]int)
	for _, snap := range cb.Snapshot(env.clk.Now().UTC()) {
		counts[snap.Identity] = snap.RestartCount
	}
	if got := counts["rig-a/session-a"]; got != 1 {
		t.Fatalf("restart count for started session = %d, want 1", got)
	}
	if got := counts["rig-a/session-b"]; got != 0 {
		t.Fatalf("restart count for wake-budget-deferred session = %d, want 0", got)
	}
}

func TestReconciler_CircuitTripsThroughRepeatedWakeAttempts(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	env.addDesired("session-a", "template-a", false)

	cb := breakerAt(30*time.Minute, 5)
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

	const identity = "rig-a/session-a"
	b := createCircuitTestNamedSession(t, env, "asleep")

	for i := 0; i < 5; i++ {
		current, err := env.store.Get(b.ID)
		if err != nil {
			t.Fatalf("get bead attempt %d: %v", i+1, err)
		}
		if woken := env.reconcile([]beads.Bead{current}); woken != 1 {
			t.Fatalf("attempt %d: woken = %d, want 1; stderr=%s", i+1, woken, env.stderr.String())
		}
		if !env.sp.IsRunning("session-a") {
			t.Fatalf("attempt %d: session-a should be running after CLOSED breaker wake", i+1)
		}
		if err := env.sp.Stop("session-a"); err != nil {
			t.Fatalf("attempt %d: stop session-a: %v", i+1, err)
		}
		env.clk.Advance(6 * time.Minute)
	}

	current, err := env.store.Get(b.ID)
	if err != nil {
		t.Fatalf("get bead before trip: %v", err)
	}
	if woken := env.reconcile([]beads.Bead{current}); woken != 0 {
		t.Fatalf("trip attempt: woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("session-a") {
		t.Fatal("session-a should not be running after circuit trips")
	}
	if !strings.Contains(env.stderr.String(), "CIRCUIT_OPEN") {
		t.Fatalf("expected CIRCUIT_OPEN log in stderr, got: %q", env.stderr.String())
	}
	snap := cb.Snapshot(env.clk.Now().UTC())
	if len(snap) != 1 || snap[0].Identity != identity || snap[0].State != "CIRCUIT_OPEN" || snap[0].RestartCount != 6 {
		t.Fatalf("snapshot after trip = %+v, want one OPEN entry with 6 restarts", snap)
	}
}

func TestReconciler_CircuitStaysClosedWhenAssignedWorkStatusProgresses(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	env.addDesired("session-a", "template-a", false)

	cb := breakerAt(30*time.Minute, 5)
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

	const identity = "rig-a/session-a"
	b := createCircuitTestNamedSession(t, env, "asleep")
	statuses := []string{"open", "in_progress", "blocked", "open", "in_progress", "closed"}

	for i, status := range statuses {
		current, err := env.store.Get(b.ID)
		if err != nil {
			t.Fatalf("get bead attempt %d: %v", i+1, err)
		}
		poolDesired := map[string]int{"template-a": 1}
		woken := reconcileSessionBeads(
			context.Background(), []beads.Bead{current}, env.desiredState,
			configuredSessionNames(env.cfg, "", env.store), env.cfg, env.sp,
			env.store, nil,
			[]beads.Bead{{ID: "work-1", Assignee: identity, Status: status}},
			nil, env.dt, poolDesired, false, nil, "", nil, env.clk, env.rec,
			0, 0, &env.stdout, &env.stderr,
		)
		if woken != 1 {
			t.Fatalf("attempt %d (%s): woken = %d, want 1; stderr=%s", i+1, status, woken, env.stderr.String())
		}
		if cb.IsOpen(identity, env.clk.Now().UTC()) {
			t.Fatalf("attempt %d (%s): breaker should stay CLOSED after assigned work progress", i+1, status)
		}
		if !env.sp.IsRunning("session-a") {
			t.Fatalf("attempt %d (%s): session-a should be running after CLOSED breaker wake", i+1, status)
		}
		if err := env.sp.Stop("session-a"); err != nil {
			t.Fatalf("attempt %d (%s): stop session-a: %v", i+1, status, err)
		}
		if err := env.store.SetMetadata(b.ID, "state", "asleep"); err != nil {
			t.Fatalf("attempt %d (%s): set state asleep: %v", i+1, status, err)
		}
		if i < len(statuses)-1 {
			env.clk.Advance(6 * time.Minute)
		}
	}

	snap := cb.Snapshot(env.clk.Now().UTC())
	if len(snap) != 1 || snap[0].Identity != identity || snap[0].State != circuitClosed.String() || snap[0].RestartCount != len(statuses) {
		t.Fatalf("snapshot after progressing work = %+v, want one CLOSED entry with %d restarts", snap, len(statuses))
	}
	if strings.Contains(env.stderr.String(), "CIRCUIT_OPEN") {
		t.Fatalf("did not expect CIRCUIT_OPEN log, got: %q", env.stderr.String())
	}
}
