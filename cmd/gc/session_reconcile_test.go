package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// testStore wraps a bead slice for SetMetadata tracking in tests.
type testStore struct {
	beads.Store
	metadata map[string]map[string]string // id -> key -> value
}

func newTestStore() *testStore {
	return &testStore{metadata: make(map[string]map[string]string)}
}

func (s *testStore) SetMetadata(id, key, value string) error {
	if s.metadata[id] == nil {
		s.metadata[id] = make(map[string]string)
	}
	s.metadata[id][key] = value
	return nil
}

func (s *testStore) SetMetadataBatch(id string, kvs map[string]string) error {
	for k, v := range kvs {
		if err := s.SetMetadata(id, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *testStore) Ping() error {
	return nil
}

func (s *testStore) Get(id string) (beads.Bead, error) {
	return beads.Bead{ID: id}, nil
}

func makeBead(id string, meta map[string]string) beads.Bead {
	if meta == nil {
		meta = make(map[string]string)
	}
	return beads.Bead{
		ID:       id,
		Status:   "open",
		Metadata: meta,
	}
}

func TestWakeReasons_SingletonTemplateDoesNotWakeFromConfigAlone(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
	})

	reasons := wakeReasons(session, cfg, nil, nil, nil, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("expected no reasons, got %v", reasons)
	}
}

func TestWakeReasons_NoConfig(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "other"},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
	})

	reasons := wakeReasons(session, cfg, nil, nil, nil, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("expected no reasons, got %v", reasons)
	}
}

func TestWakeReasons_HeldUntil(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
		},
	}

	// Hold until future — suppresses all reasons.
	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
		"held_until":   now.Add(1 * time.Hour).Format(time.RFC3339),
	})

	reasons := wakeReasons(session, cfg, nil, nil, nil, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("held session should have no reasons, got %v", reasons)
	}
}

func TestWakeReasons_HoldExpiredDoesNotRestoreSingletonConfigWake(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
		},
	}

	// Hold expired — should produce reasons.
	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
		"held_until":   now.Add(-1 * time.Hour).Format(time.RFC3339),
	})

	reasons := wakeReasons(session, cfg, nil, nil, nil, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("expired hold should not restore singleton config wake, got %v", reasons)
	}
}

func TestWakeReasons_Quarantined(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":          "worker",
		"session_name":      "test-worker",
		"quarantined_until": now.Add(5 * time.Minute).Format(time.RFC3339),
	})

	reasons := wakeReasons(session, cfg, nil, nil, nil, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("quarantined session should have no reasons, got %v", reasons)
	}
}

func TestWakeReasons_PoolWithinDesired(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Pool: &config.PoolConfig{Min: 1, Max: 5}},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker-1",
		"pool_slot":    "1",
	})

	poolDesired := map[string]int{"worker": 3}

	reasons := wakeReasons(session, cfg, nil, poolDesired, nil, nil, clk)
	if len(reasons) != 1 || reasons[0] != WakeConfig {
		t.Errorf("pool slot within desired should wake, got %v", reasons)
	}
}

func TestWakeReasons_PoolExceedsDesired(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Pool: &config.PoolConfig{Min: 1, Max: 5}},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker-4",
		"pool_slot":    "4",
	})

	poolDesired := map[string]int{"worker": 3}

	reasons := wakeReasons(session, cfg, nil, poolDesired, nil, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("pool slot exceeding desired should not wake, got %v", reasons)
	}
}

func TestWakeReasons_DrainedSleepPoolSessionDoesNotGetWakeConfig(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Pool: &config.PoolConfig{Min: 1, Max: 5}},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker-1",
		"pool_slot":    "1",
		"state":        "asleep",
		"sleep_reason": "drained",
	})

	reasons := wakeReasons(session, cfg, nil, map[string]int{"worker": 3}, nil, nil, clk)
	for _, reason := range reasons {
		if reason == WakeConfig {
			t.Fatalf("drained sleep session should not get WakeConfig, got %v", reasons)
		}
	}
}

func TestWakeReasons_Attached(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{} // no agents — so no WakeConfig

	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "test-worker", runtime.Config{})
	sp.SetAttached("test-worker", true)

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
	})

	reasons := wakeReasons(session, cfg, sp, nil, nil, nil, clk)
	if len(reasons) != 1 || reasons[0] != WakeAttached {
		t.Errorf("attached session should get WakeAttached, got %v", reasons)
	}
}

func TestWakeReasons_WorkSet(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{} // no agents — so no WakeConfig

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
	})

	// Work exists for this template.
	workSet := map[string]bool{"worker": true}

	reasons := wakeReasons(session, cfg, nil, nil, workSet, nil, clk)
	if len(reasons) != 1 || reasons[0] != WakeWork {
		t.Errorf("session with work should get WakeWork, got %v", reasons)
	}
}

func TestWakeReasons_WorkSetEmpty(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{} // no agents

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
	})

	// No work for this template.
	workSet := map[string]bool{"other": true}

	reasons := wakeReasons(session, cfg, nil, nil, workSet, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("session without work should have no reasons, got %v", reasons)
	}
}

func TestWakeReasons_WorkSetHeldSuppressed(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
		"held_until":   now.Add(1 * time.Hour).Format(time.RFC3339),
	})

	workSet := map[string]bool{"worker": true}

	reasons := wakeReasons(session, cfg, nil, nil, workSet, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("held session should have no reasons even with work, got %v", reasons)
	}
}

func TestWakeReasons_WaitHoldSuppressesConfigAndAttached(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}

	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "test-worker", runtime.Config{})
	sp.SetAttached("test-worker", true)

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
		"wait_hold":    "true",
	})

	reasons := wakeReasons(session, cfg, sp, nil, nil, nil, clk)
	if len(reasons) != 0 {
		t.Errorf("wait-hold should suppress config/attached wake reasons, got %v", reasons)
	}
}

func TestWakeReasons_WaitHoldPreservesWaitOnly(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{}
	workSet := map[string]bool{"worker": true}
	readyWaitSet := map[string]bool{"b1": true}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
		"wait_hold":    "true",
	})

	reasons := wakeReasons(session, cfg, nil, nil, workSet, readyWaitSet, clk)
	if len(reasons) != 1 || reasons[0] != WakeWait {
		t.Errorf("wait-hold should preserve wait only, got %v", reasons)
	}
}

func TestWakeReasons_WorkSetPoolSlotGated(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "pooled", Pool: &config.PoolConfig{Min: 1, Max: 3}},
		},
	}

	poolDesired := map[string]int{"pooled": 2}
	workSet := map[string]bool{"pooled": true}

	// Slot 1 (within desired) — should get WakeWork.
	s1 := makeBead("b1", map[string]string{
		"template":     "pooled",
		"session_name": "test-pooled-1",
		"pool_slot":    "1",
	})
	reasons := wakeReasons(s1, cfg, nil, poolDesired, workSet, nil, clk)
	hasWork := false
	for _, r := range reasons {
		if r == WakeWork {
			hasWork = true
		}
	}
	if !hasWork {
		t.Errorf("pool slot 1 (within desired=2) should get WakeWork, got %v", reasons)
	}

	// Slot 3 (exceeds desired) — should NOT get WakeWork.
	s3 := makeBead("b3", map[string]string{
		"template":     "pooled",
		"session_name": "test-pooled-3",
		"pool_slot":    "3",
	})
	reasons = wakeReasons(s3, cfg, nil, poolDesired, workSet, nil, clk)
	for _, r := range reasons {
		if r == WakeWork {
			t.Errorf("pool slot 3 (exceeds desired=2) should NOT get WakeWork, got %v", reasons)
			break
		}
	}
}

func TestWakeReasons_DependencyOnlyPoolSlotDoesNotWakeOnWork(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "pooled", Pool: &config.PoolConfig{Min: 0, Max: 3}},
		},
	}

	reasons := wakeReasons(makeBead("b1", map[string]string{
		"template":        "pooled",
		"session_name":    "test-pooled-1",
		"pool_slot":       "1",
		"dependency_only": "true",
	}), cfg, nil, map[string]int{"pooled": 1}, map[string]bool{"pooled": true}, nil, clk)

	for _, r := range reasons {
		if r == WakeWork {
			t.Fatalf("dependency-only pool slot should not get WakeWork, got %v", reasons)
		}
	}
}

func TestWakeReasons_ManualPoolSessionDoesNotGetWakeConfigAtZeroScale(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "pooled", Pool: &config.PoolConfig{Min: 0, Max: 3}},
		},
	}

	reasons := wakeReasons(makeBead("b1", map[string]string{
		"template":       "pooled",
		"session_name":   "manual-pooled",
		"manual_session": "true",
	}), cfg, nil, map[string]int{"pooled": 0}, nil, nil, clk)

	for _, r := range reasons {
		if r == WakeConfig {
			t.Fatalf("manual pool session should not get WakeConfig at zero scale, got %v", reasons)
		}
	}
}

func TestWakeReasons_UsesLegacyAgentLabelTemplate(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Pool: &config.PoolConfig{Min: 1, Max: 3}},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "custom-worker-1",
		"pool_slot":    "1",
	})
	session.Labels = []string{sessionBeadLabel, "agent:frontend/worker-1"}

	poolDesired := map[string]int{"frontend/worker": 1}
	workSet := map[string]bool{"frontend/worker": true}

	reasons := wakeReasons(session, cfg, nil, poolDesired, workSet, nil, clk)
	if len(reasons) != 2 || reasons[0] != WakeConfig || reasons[1] != WakeWork {
		t.Fatalf("wakeReasons(legacy labeled pool worker) = %v, want [WakeConfig WakeWork]", reasons)
	}
}

func TestComputeWorkSet_RunsWorkQuery(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker"},
			{Name: "idle", WorkQuery: "bd ready --assignee=idle"},
		},
	}

	runner := func(command, _ string) (string, error) {
		if strings.Contains(command, "GC_SESSION_NAME='worker'") || strings.Contains(command, "GC_SESSION_NAME=worker") {
			return `[{"id":"BL-42"}]`, nil
		}
		return "", nil // empty = no work for idle's custom query
	}

	work := computeWorkSet(cfg, runner, "test-city", "/tmp", nil, nil)
	if !work["worker"] {
		t.Error("expected worker to have work")
	}
	if work["idle"] {
		t.Error("expected idle to have no work")
	}
}

func TestComputeWorkSet_ResolvesRigDir(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", Pool: &config.PoolConfig{Min: 0, Max: 3}},
		},
	}

	runner := func(_ string, dir string) (string, error) {
		// The dir must be the resolved absolute path, not the relative "myrig".
		if dir == rigDir {
			return "MC-1\n", nil
		}
		return "", fmt.Errorf("unexpected dir %q, want %q", dir, rigDir)
	}

	work := computeWorkSet(cfg, runner, "test-city", cityDir, nil, nil)
	if !work["myrig/polecat"] {
		t.Error("expected myrig/polecat to have work when dir is resolved")
	}
}

func TestComputeWorkSet_UsesConfiguredRigRoot(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "external-rig")

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "myrig", Path: rigDir}},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", Pool: &config.PoolConfig{Min: 0, Max: 3}},
		},
	}

	runner := func(_ string, dir string) (string, error) {
		if dir == rigDir {
			return "MC-1\n", nil
		}
		return "", fmt.Errorf("unexpected dir %q, want %q", dir, rigDir)
	}

	work := computeWorkSet(cfg, runner, "test-city", cityDir, nil, nil)
	if !work["myrig/polecat"] {
		t.Error("expected myrig/polecat to have work when rig root is configured externally")
	}
}

func TestComputeWorkSet_NilRunner(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	work := computeWorkSet(cfg, nil, "test-city", "/tmp", nil, nil)
	if work != nil {
		t.Errorf("expected nil, got %v", work)
	}
}

func TestComputeWorkSet_CommandError(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}

	runner := func(_, _ string) (string, error) {
		return "", fmt.Errorf("connection refused")
	}

	work := computeWorkSet(cfg, runner, "test-city", "/tmp", nil, nil)
	if work["worker"] {
		t.Error("command error should not produce work")
	}
}

func TestComputeWorkSet_IgnoresNoReadyMessage(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}

	runner := func(_, _ string) (string, error) {
		return "✨ No ready work found (all issues have blocking dependencies)\n", nil
	}

	work := computeWorkSet(cfg, runner, "test-city", "/tmp", nil, nil)
	if work["worker"] {
		t.Error("no-ready message should not produce work")
	}
}

func TestHealExpiredTimers_ClearsExpiredHold(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"held_until":   now.Add(-1 * time.Hour).Format(time.RFC3339),
		"sleep_reason": "user-hold",
	})

	healExpiredTimers(&session, store, clk)

	if session.Metadata["held_until"] != "" {
		t.Error("expected held_until to be cleared")
	}
	if session.Metadata["sleep_reason"] != "" {
		t.Error("expected sleep_reason to be cleared")
	}
}

func TestHealExpiredTimers_KeepsActiveHold(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	future := now.Add(1 * time.Hour).Format(time.RFC3339)
	session := makeBead("b1", map[string]string{
		"held_until":   future,
		"sleep_reason": "user-hold",
	})

	healExpiredTimers(&session, store, clk)

	if session.Metadata["held_until"] != future {
		t.Error("active hold should not be cleared")
	}
}

func TestHealExpiredTimers_ClearsExpiredQuarantine(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"quarantined_until": now.Add(-1 * time.Minute).Format(time.RFC3339),
		"wake_attempts":     "5",
		"sleep_reason":      "quarantine",
	})

	healExpiredTimers(&session, store, clk)

	if session.Metadata["quarantined_until"] != "" {
		t.Error("expected quarantined_until to be cleared")
	}
	if session.Metadata["wake_attempts"] != "0" {
		t.Errorf("expected wake_attempts to be 0, got %q", session.Metadata["wake_attempts"])
	}
	if session.Metadata["sleep_reason"] != "" {
		t.Error("expected sleep_reason to be cleared")
	}
}

func TestCheckStability_AliveReturnsFalse(t *testing.T) {
	clk := &clock.Fake{Time: time.Now()}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at": clk.Now().Add(-10 * time.Second).Format(time.RFC3339),
	})

	if checkStability(&session, nil, true, dt, store, clk) {
		t.Error("alive session should not report stability failure")
	}
}

func TestCheckStability_RapidExit(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at":  now.Add(-10 * time.Second).Format(time.RFC3339),
		"wake_attempts": "0",
	})

	if !checkStability(&session, nil, false, dt, store, clk) {
		t.Error("rapid exit should report stability failure")
	}

	// wake_attempts should be incremented.
	if session.Metadata["wake_attempts"] != "1" {
		t.Errorf("wake_attempts = %q, want 1", session.Metadata["wake_attempts"])
	}

	// last_woke_at should be cleared (edge-triggered).
	if session.Metadata["last_woke_at"] != "" {
		t.Error("last_woke_at should be cleared after rapid exit detection")
	}
}

func TestCheckStability_DrainingNotCounted(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()
	dt.set("b1", &drainState{reason: "idle"})

	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-10 * time.Second).Format(time.RFC3339),
	})

	if checkStability(&session, nil, false, dt, store, clk) {
		t.Error("draining session death should not count as stability failure")
	}
}

func TestCheckStability_StableSession(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	// Woke long ago — past stability threshold.
	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-2 * time.Minute).Format(time.RFC3339),
	})

	if checkStability(&session, nil, false, dt, store, clk) {
		t.Error("session that lived past threshold should not be stability failure")
	}
}

func TestCheckStability_SubprocessProviderSkipsCrashCounting(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "subprocess"},
	}

	session := makeBead("b1", map[string]string{
		"last_woke_at":  now.Add(-10 * time.Second).Format(time.RFC3339),
		"wake_attempts": "0",
	})

	if checkStability(&session, cfg, false, dt, store, clk) {
		t.Fatal("subprocess rapid exit should not be counted as a crash")
	}
	if got := session.Metadata["wake_attempts"]; got != "0" {
		t.Fatalf("wake_attempts = %q, want 0", got)
	}
	if got := session.Metadata["last_woke_at"]; got == "" {
		t.Fatal("last_woke_at should be preserved when no crash is recorded")
	}
}

func TestRecordWakeFailure_Quarantine(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"wake_attempts": "4", // one below threshold
	})

	recordWakeFailure(&session, store, clk)

	if session.Metadata["wake_attempts"] != "5" {
		t.Errorf("wake_attempts = %q, want 5", session.Metadata["wake_attempts"])
	}
	if session.Metadata["quarantined_until"] == "" {
		t.Error("expected quarantine to be set at max attempts")
	}
	if session.Metadata["sleep_reason"] != "quarantine" {
		t.Errorf("sleep_reason = %q, want quarantine", session.Metadata["sleep_reason"])
	}
}

func TestRecordWakeFailure_BelowThreshold(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"wake_attempts": "1",
	})

	recordWakeFailure(&session, store, clk)

	if session.Metadata["wake_attempts"] != "2" {
		t.Errorf("wake_attempts = %q, want 2", session.Metadata["wake_attempts"])
	}
	if session.Metadata["quarantined_until"] != "" {
		t.Error("should not quarantine below threshold")
	}
}

func TestClearWakeFailures(t *testing.T) {
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"wake_attempts":     "5",
		"quarantined_until": "2026-03-08T12:00:00Z",
	})

	clearWakeFailures(&session, store)

	if session.Metadata["wake_attempts"] != "0" {
		t.Errorf("wake_attempts = %q, want 0", session.Metadata["wake_attempts"])
	}
	if session.Metadata["quarantined_until"] != "" {
		t.Error("quarantined_until should be cleared")
	}
}

func TestStableLongEnough(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	tests := []struct {
		name     string
		lastWoke string
		want     bool
	}{
		{"no last_woke_at", "", false},
		{"recent wake", now.Add(-10 * time.Second).Format(time.RFC3339), false},
		{"exactly at threshold", now.Add(-stabilityThreshold).Format(time.RFC3339), true},
		{"past threshold", now.Add(-2 * time.Minute).Format(time.RFC3339), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := makeBead("b1", map[string]string{
				"last_woke_at": tt.lastWoke,
			})
			got := stableLongEnough(session, clk)
			if got != tt.want {
				t.Errorf("stableLongEnough = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSessionIsQuarantined(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	tests := []struct {
		name string
		qVal string
		want bool
	}{
		{"not set", "", false},
		{"future", now.Add(5 * time.Minute).Format(time.RFC3339), true},
		{"past", now.Add(-5 * time.Minute).Format(time.RFC3339), false},
		{"invalid", "not-a-time", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := makeBead("b1", map[string]string{
				"quarantined_until": tt.qVal,
			})
			got := sessionIsQuarantined(session, clk)
			if got != tt.want {
				t.Errorf("sessionIsQuarantined = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsPoolExcess(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Pool: &config.PoolConfig{Min: 1, Max: 5}},
			{Name: "singleton"},
		},
	}
	poolDesired := map[string]int{"worker": 3}

	tests := []struct {
		name     string
		template string
		slot     string
		want     bool
	}{
		{"within desired", "worker", "2", false},
		{"at desired", "worker", "3", false},
		{"exceeds desired", "worker", "4", true},
		{"singleton", "singleton", "", false},
		{"unknown template", "missing", "1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := makeBead("b1", map[string]string{
				"template":  tt.template,
				"pool_slot": tt.slot,
			})
			got := isPoolExcess(session, cfg, poolDesired)
			if got != tt.want {
				t.Errorf("isPoolExcess = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHealState(t *testing.T) {
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"state": "asleep",
	})

	healState(&session, true, store)
	if session.Metadata["state"] != "awake" {
		t.Errorf("state = %q, want awake", session.Metadata["state"])
	}

	healState(&session, false, store)
	if session.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep", session.Metadata["state"])
	}

	// No-op when already correct.
	prevCalls := len(store.metadata["b1"])
	healState(&session, false, store)
	if len(store.metadata["b1"]) != prevCalls {
		t.Error("healState should not write when state unchanged")
	}
}

func TestTopoOrder_NoDeps(t *testing.T) {
	sessions := []beads.Bead{
		makeBead("b1", map[string]string{"template": "a"}),
		makeBead("b2", map[string]string{"template": "b"}),
	}

	result := topoOrder(sessions, nil)
	if len(result) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(result))
	}
}

func TestTopoOrder_WithDeps(t *testing.T) {
	sessions := []beads.Bead{
		makeBead("b1", map[string]string{"template": "frontend"}),
		makeBead("b2", map[string]string{"template": "api"}),
		makeBead("b3", map[string]string{"template": "database"}),
	}

	deps := map[string][]string{
		"frontend": {"api"},
		"api":      {"database"},
	}

	result := topoOrder(sessions, deps)
	if len(result) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(result))
	}

	// database should come before api, api before frontend.
	idx := make(map[string]int)
	for i, s := range result {
		idx[s.Metadata["template"]] = i
	}
	if idx["database"] > idx["api"] {
		t.Errorf("database (idx %d) should come before api (idx %d)", idx["database"], idx["api"])
	}
	if idx["api"] > idx["frontend"] {
		t.Errorf("api (idx %d) should come before frontend (idx %d)", idx["api"], idx["frontend"])
	}
}

func TestTopoOrder_CycleFallback(t *testing.T) {
	sessions := []beads.Bead{
		makeBead("b1", map[string]string{"template": "a"}),
		makeBead("b2", map[string]string{"template": "b"}),
	}

	deps := map[string][]string{
		"a": {"b"},
		"b": {"a"},
	}

	result := topoOrder(sessions, deps)
	// Should return original order (fallback on cycle).
	if len(result) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(result))
	}
	if result[0].ID != "b1" || result[1].ID != "b2" {
		t.Error("cycle fallback should return original order")
	}
}

func TestReverseBeads(t *testing.T) {
	beadSlice := []beads.Bead{
		makeBead("b1", nil),
		makeBead("b2", nil),
		makeBead("b3", nil),
	}

	reversed := reverseBeads(beadSlice)
	if reversed[0].ID != "b3" || reversed[1].ID != "b2" || reversed[2].ID != "b1" {
		t.Errorf("expected reversed order, got %s %s %s",
			reversed[0].ID, reversed[1].ID, reversed[2].ID)
	}

	// Original unchanged.
	if beadSlice[0].ID != "b1" {
		t.Error("original should not be modified")
	}
}

func TestSessionWakeAttempts(t *testing.T) {
	tests := []struct {
		val  string
		want int
	}{
		{"", 0},
		{"0", 0},
		{"3", 3},
		{"invalid", 0},
	}
	for _, tt := range tests {
		session := makeBead("b1", map[string]string{"wake_attempts": tt.val})
		got := sessionWakeAttempts(session)
		if got != tt.want {
			t.Errorf("sessionWakeAttempts(%q) = %d, want %d", tt.val, got, tt.want)
		}
	}
}

func TestFindAgentByTemplate(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
			{Name: "mayor"},
		},
	}

	if a := findAgentByTemplate(cfg, "worker"); a == nil || a.Name != "worker" {
		t.Error("expected to find worker")
	}
	if a := findAgentByTemplate(cfg, "missing"); a != nil {
		t.Error("expected nil for missing template")
	}
	if a := findAgentByTemplate(nil, "worker"); a != nil {
		t.Error("expected nil for nil config")
	}
	if a := findAgentByTemplate(cfg, ""); a != nil {
		t.Error("expected nil for empty template")
	}
}

// --- isKnownState tests (Phase 0b: forward compatibility) ---

func TestIsKnownState_KnownStates(t *testing.T) {
	known := []string{
		"active", "asleep", "awake", "stopped", "suspended",
		"orphaned", "closed", "quarantined", "creating", "",
	}
	for _, state := range known {
		session := makeBead("b1", map[string]string{"state": state})
		if !isKnownState(session) {
			t.Errorf("state %q should be known", state)
		}
	}
}

func TestIsKnownState_UnknownStates(t *testing.T) {
	unknown := []string{"draining", "archived", "future-state"}
	for _, state := range unknown {
		session := makeBead("b1", map[string]string{"state": state})
		if isKnownState(session) {
			t.Errorf("state %q should be unknown", state)
		}
	}
}

func TestForwardCompatibility_UnknownState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)

	// Create a session bead with a future state that the current reconciler
	// doesn't understand.
	session := env.createSessionBead("worker", "worker")
	_ = env.store.SetMetadata(session.ID, "state", "draining")
	session.Metadata["state"] = "draining"

	// Should not panic, should skip the unknown-state bead.
	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Errorf("expected 0 woken for unknown state, got %d", woken)
	}

	// The warning should appear in stderr.
	if !strings.Contains(env.stderr.String(), "unknown state") {
		t.Errorf("expected warning about unknown state in stderr, got: %s", env.stderr.String())
	}
}
