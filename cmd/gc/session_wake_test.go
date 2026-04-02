package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type countingWakeMetadataStore struct {
	*beads.MemStore
	singleCalls int
	batchCalls  int
}

func (s *countingWakeMetadataStore) SetMetadata(id, key, value string) error {
	s.singleCalls++
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *countingWakeMetadataStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.batchCalls++
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func TestPreWakeCommit(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "test-session",
		Metadata: map[string]string{
			"session_name": "test-worker",
			"template":     "worker",
			"generation":   "2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	newGen, token, err := preWakeCommit(&b, store, clk)
	if err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}

	if newGen != 3 {
		t.Errorf("newGen = %d, want 3", newGen)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}

	// Verify persisted in store.
	got, _ := store.Get(b.ID)
	if got.Metadata["generation"] != "3" {
		t.Errorf("stored generation = %q, want 3", got.Metadata["generation"])
	}
	if got.Metadata["instance_token"] != token {
		t.Errorf("stored token mismatch")
	}
	if got.Metadata["last_woke_at"] == "" {
		t.Error("expected last_woke_at to be set")
	}
	if got.Metadata["sleep_reason"] != "" {
		t.Error("expected sleep_reason to be cleared")
	}
	if got.Metadata["continuation_epoch"] != "1" {
		t.Errorf("stored continuation_epoch = %q, want 1", got.Metadata["continuation_epoch"])
	}
}

func TestPreWakeCommitUsesSingleBatchMetadataWrite(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := &countingWakeMetadataStore{MemStore: beads.NewMemStore()}

	b, err := store.Create(beads.Bead{
		Title: "test-session",
		Metadata: map[string]string{
			"session_name": "test-worker",
			"template":     "worker",
			"generation":   "2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := preWakeCommit(&b, store, clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	if store.batchCalls != 1 {
		t.Fatalf("batchCalls = %d, want 1", store.batchCalls)
	}
	if store.singleCalls != 0 {
		t.Fatalf("singleCalls = %d, want 0", store.singleCalls)
	}
}

func TestPreWakeCommit_InvalidName(t *testing.T) {
	clk := &clock.Fake{Time: time.Now()}
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title: "bad-session",
		Metadata: map[string]string{
			"session_name": "../bad",
			"template":     "worker",
		},
	})

	_, _, err := preWakeCommit(&b, store, clk)
	if err == nil {
		t.Error("expected error for invalid session_name")
	}
}

func TestPreWakeCommit_BumpsContinuationEpochForFreshWake(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "fresh-session",
		Metadata: map[string]string{
			"session_name":       "fresh-worker",
			"template":           "worker",
			"generation":         "2",
			"continuation_epoch": "3",
			"wake_mode":          "fresh",
			"last_woke_at":       now.Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := preWakeCommit(&b, store, clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata["continuation_epoch"] != "4" {
		t.Fatalf("continuation_epoch = %q, want 4", got.Metadata["continuation_epoch"])
	}
}

func TestPreWakeCommit_BumpsContinuationEpochForPendingReset(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "reset-session",
		Metadata: map[string]string{
			"session_name":               "reset-worker",
			"template":                   "worker",
			"generation":                 "2",
			"continuation_epoch":         "5",
			"continuation_reset_pending": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := preWakeCommit(&b, store, clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata["continuation_epoch"] != "6" {
		t.Fatalf("continuation_epoch = %q, want 6", got.Metadata["continuation_epoch"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
}

func TestValidateWorkDir(t *testing.T) {
	// Valid: use temp dir.
	dir := t.TempDir()
	if err := validateWorkDir(dir); err != nil {
		t.Errorf("valid dir: %v", err)
	}

	// Invalid: non-existent.
	if err := validateWorkDir("/nonexistent/path/xyz"); err == nil {
		t.Error("expected error for nonexistent path")
	}

	// Invalid: file, not directory.
	f := dir + "/file.txt"
	if err := writeTestFile(f); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkDir(f); err == nil {
		t.Error("expected error for file path")
	}
}

func writeTestFile(path string) error {
	return os.WriteFile(path, []byte("test"), 0o644)
}

func TestVerifiedStop_MatchingToken(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})
	_ = sp.SetMeta("test-session", "GC_INSTANCE_TOKEN", "abc123")

	session := makeBead("b1", map[string]string{
		"session_name":   "test-session",
		"instance_token": "abc123",
	})

	err := verifiedStop(session, sp)
	if err != nil {
		t.Errorf("verifiedStop with matching token: %v", err)
	}
	if sp.IsRunning("test-session") {
		t.Error("expected session to be stopped")
	}
}

func TestVerifiedStop_MismatchedToken(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})
	_ = sp.SetMeta("test-session", "GC_INSTANCE_TOKEN", "old-token")

	session := makeBead("b1", map[string]string{
		"session_name":   "test-session",
		"instance_token": "new-token",
	})

	err := verifiedStop(session, sp)
	if err == nil {
		t.Error("expected error for mismatched token")
	}
	if !sp.IsRunning("test-session") {
		t.Error("session should NOT be stopped on token mismatch")
	}
}

func TestVerifiedStop_NoToken(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	session := makeBead("b1", map[string]string{
		"session_name":   "test-session",
		"instance_token": "",
	})

	err := verifiedStop(session, sp)
	if err != nil {
		t.Errorf("verifiedStop with no token: %v", err)
	}
}

func TestVerifiedInterrupt_MismatchedToken(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})
	_ = sp.SetMeta("test-session", "GC_INSTANCE_TOKEN", "old-token")

	session := makeBead("b1", map[string]string{
		"session_name":   "test-session",
		"instance_token": "new-token",
	})

	err := verifiedInterrupt(session, sp)
	if err == nil {
		t.Error("expected error for mismatched token")
	}
}

func TestBeginSessionDrain(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	session := makeBead("b1", map[string]string{
		"session_name": "test-session",
		"generation":   "5",
	})

	beginSessionDrain(session, sp, dt, "idle", clk, 30*time.Second)

	ds := dt.get("b1")
	if ds == nil {
		t.Fatal("expected drain state")
	}
	if ds.reason != "idle" {
		t.Errorf("reason = %q, want idle", ds.reason)
	}
	if ds.generation != 5 {
		t.Errorf("generation = %d, want 5", ds.generation)
	}
	if ds.deadline != now.Add(30*time.Second) {
		t.Errorf("deadline = %v, want %v", ds.deadline, now.Add(30*time.Second))
	}
}

func TestBeginSessionDrain_AlreadyDraining(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	session := makeBead("b1", map[string]string{
		"session_name": "test-session",
		"generation":   "5",
	})

	beginSessionDrain(session, sp, dt, "idle", clk, 30*time.Second)
	beginSessionDrain(session, sp, dt, "config-drift", clk, 60*time.Second)

	// Second drain should not overwrite first.
	ds := dt.get("b1")
	if ds.reason != "idle" {
		t.Errorf("reason = %q, want idle (should not be overwritten)", ds.reason)
	}
}

func TestCancelSessionDrain(t *testing.T) {
	dt := newDrainTracker()
	dt.set("b1", &drainState{
		reason:     "idle",
		generation: 5,
	})

	session := makeBead("b1", map[string]string{
		"generation": "5",
	})

	if !cancelSessionDrain(session, dt) {
		t.Error("expected cancel to succeed")
	}
	if dt.get("b1") != nil {
		t.Error("drain should be removed after cancel")
	}
}

func TestCancelSessionDrain_GenerationMismatch(t *testing.T) {
	dt := newDrainTracker()
	dt.set("b1", &drainState{
		reason:     "idle",
		generation: 5,
	})

	session := makeBead("b1", map[string]string{
		"generation": "6", // re-woken
	})

	if cancelSessionDrain(session, dt) {
		t.Error("cancel should fail when generation doesn't match")
	}
}

func TestAdvanceSessionDrains_ProcessExited(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// No session running (process exited).
	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
			"state":        "active",
			"pool_slot":    "1",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "idle",
		generation: 3,
	})

	cfg := &config.City{}

	advanceSessionDrains(dt, sp, store, func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}, cfg, map[string]int{}, nil, nil, clk)

	// Drain should be cleaned up.
	if dt.get(b.ID) != nil {
		t.Error("drain should be removed after process exit")
	}

	// Metadata should be updated.
	got, _ := store.Get(b.ID)
	if got.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Errorf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
}

func TestAdvanceSessionDrains_Timeout(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is still running.
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})

	// Drain deadline already passed.
	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-60 * time.Second),
		deadline:   now.Add(-10 * time.Second),
		reason:     "pool-excess",
		generation: 3,
	})

	cfg := &config.City{}

	advanceSessionDrains(dt, sp, store, func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}, cfg, map[string]int{}, nil, nil, clk)

	// Should have force-stopped.
	if sp.IsRunning("test-session") {
		t.Error("session should be force-stopped after deadline")
	}
	if dt.get(b.ID) != nil {
		t.Error("drain should be removed after timeout")
	}
}

func TestAdvanceSessionDrains_WakeReasonsReappear(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is still running.
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "idle", // NOT config-drift, so cancelable
		generation: 3,
	})

	// A desired pool slot still has WakeConfig, which should cancel the drain.
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}}}

	advanceSessionDrains(dt, sp, store, func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}, cfg, map[string]int{"worker": 1}, nil, nil, clk)

	// Drain should be canceled — wake reasons reappeared.
	if dt.get(b.ID) != nil {
		t.Error("drain should be canceled when wake reasons reappear")
	}
	// Session should still be running.
	if !sp.IsRunning("test-session") {
		t.Error("session should still be running after drain cancel")
	}
}

func TestAdvanceSessionDrains_ConfigDriftNotCancelable(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is still running.
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "config-drift", // NOT cancelable
		generation: 3,
	})

	// Config has the worker agent — but config-drift drains are not canceled.
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
		},
	}

	advanceSessionDrains(dt, sp, store, func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}, cfg, map[string]int{}, nil, nil, clk)

	// Drain should NOT be canceled — config-drift is non-cancelable.
	// Session is alive and deadline not reached, so drain continues.
	if dt.get(b.ID) == nil {
		t.Error("config-drift drain should not be canceled by wake reasons")
	}
}

func TestAdvanceSessionDrains_TimeoutTokenMismatch(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is running with a different token (re-woken by different incarnation).
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})
	_ = sp.SetMeta("test-session", "GC_INSTANCE_TOKEN", "new-token")

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name":   "test-session",
			"template":       "worker",
			"generation":     "3",
			"instance_token": "old-token", // stale token
		},
	})

	// Drain deadline already passed.
	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-60 * time.Second),
		deadline:   now.Add(-10 * time.Second),
		reason:     "pool-excess",
		generation: 3,
	})

	cfg := &config.City{}

	advanceSessionDrains(dt, sp, store, func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}, cfg, map[string]int{}, nil, nil, clk)

	// Drain should be canceled (stale token), session still running.
	if dt.get(b.ID) != nil {
		t.Error("drain should be removed after token mismatch")
	}
	if !sp.IsRunning("test-session") {
		t.Error("session should still be running after token mismatch")
	}
	// Metadata should NOT be updated to asleep.
	got, _ := store.Get(b.ID)
	if got.Metadata["state"] == "asleep" {
		t.Error("state should not be asleep after failed stop")
	}
}

func TestCompleteDrain_ClearsLastWokeAt(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"last_woke_at": now.Add(-10 * time.Second).UTC().Format(time.RFC3339),
		},
	})

	ds := &drainState{reason: "idle"}
	completeDrain(&b, store, ds, clk)

	got, _ := store.Get(b.ID)
	if got.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at should be cleared after drain, got %q", got.Metadata["last_woke_at"])
	}
	if got.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Errorf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
}

func TestAdvanceSessionDrains_CancelsForReadyWait(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "wait-hold",
		generation: 3,
	})

	advanceSessionDrains(dt, sp, store, func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}, &config.City{}, map[string]int{}, nil, map[string]bool{b.ID: true}, clk)

	if dt.get(b.ID) != nil {
		t.Fatal("drain should be canceled when a wait becomes ready mid-drain")
	}
	if !sp.IsRunning("test-session") {
		t.Fatal("session should remain running after wait-based drain cancellation")
	}
}

func TestAdvanceSessionDrains_ClearsIdleProbeOnCompletion(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	b, err := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "config-drift",
		generation: 3,
	})
	if probe := dt.startIdleProbe(b.ID); probe == nil {
		t.Fatal("expected idle probe to start")
	}

	advanceSessionDrains(dt, sp, store, func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}, &config.City{}, map[string]int{}, nil, nil, clk)

	if dt.get(b.ID) != nil {
		t.Fatal("drain should be removed after completion")
	}
	if _, ok := dt.idleProbe(b.ID); ok {
		t.Fatal("idle probe should be cleared when the drain completes")
	}
}

func TestDrainTracker_FinishIdleProbeIgnoresStaleProbe(t *testing.T) {
	dt := newDrainTracker()
	first := dt.startIdleProbe("bead-1")
	if first == nil {
		t.Fatal("expected first idle probe to start")
	}
	dt.clearIdleProbe("bead-1")

	second := dt.startIdleProbe("bead-1")
	if second == nil {
		t.Fatal("expected replacement idle probe to start")
	}

	dt.finishIdleProbe("bead-1", first, true, time.Now().UTC())
	probe, ok := dt.idleProbe("bead-1")
	if !ok {
		t.Fatal("expected current probe to remain registered")
	}
	if probe.ready {
		t.Fatal("stale probe completion should not mark the replacement probe ready")
	}

	dt.finishIdleProbe("bead-1", second, true, time.Now().UTC())
	probe, ok = dt.idleProbe("bead-1")
	if !ok || !probe.ready || !probe.success {
		t.Fatalf("replacement probe should complete successfully, got ok=%v probe=%+v", ok, probe)
	}
}

func TestNeedsConfigRestart(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "claude --new-command"},
		},
	}

	buildFn := func(a *config.Agent) runtime.Config {
		return runtime.Config{Command: a.StartCommand}
	}

	// Hash matches — no restart needed.
	currentHash := runtime.CoreFingerprint(buildFn(&cfg.Agents[0]))
	session := makeBead("b1", map[string]string{
		"template":    "worker",
		"config_hash": currentHash,
	})
	if needsConfigRestart(session, cfg, buildFn) {
		t.Error("should not need restart when hashes match")
	}

	// Hash differs — restart needed.
	session.Metadata["config_hash"] = "old-hash"
	if !needsConfigRestart(session, cfg, buildFn) {
		t.Error("should need restart when hashes differ")
	}

	// No hash stored — can't detect drift.
	session.Metadata["config_hash"] = ""
	if needsConfigRestart(session, cfg, buildFn) {
		t.Error("should not need restart when no hash stored")
	}
}

func TestNeedsConfigRestart_UsesLegacyAgentLabelTemplate(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", StartCommand: "claude --new-command"},
		},
	}

	buildFn := func(a *config.Agent) runtime.Config {
		return runtime.Config{Command: a.StartCommand}
	}

	currentHash := runtime.CoreFingerprint(buildFn(&cfg.Agents[0]))
	session := makeBead("b1", map[string]string{
		"template":    "worker",
		"config_hash": currentHash,
	})
	session.Labels = []string{sessionBeadLabel, "agent:frontend/worker"}

	if needsConfigRestart(session, cfg, buildFn) {
		t.Fatal("legacy labeled session should not restart when hashes match")
	}

	session.Metadata["config_hash"] = "old-hash"
	if !needsConfigRestart(session, cfg, buildFn) {
		t.Fatal("legacy labeled session should detect config drift")
	}
}
