package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
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

func TestWakeReasons_DemandExistsSessionWakes(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker-4",
	})

	// With demand > 0, all sessions for the template are eligible to wake.
	poolDesired := map[string]int{"worker": 3}
	reasons := wakeReasons(session, cfg, nil, poolDesired, nil, nil, clk)
	if !containsWakeReason(reasons, WakeConfig) {
		t.Errorf("session should wake when demand exists, got %v", reasons)
	}

	// With demand = 0, no sessions wake.
	poolDesired = map[string]int{"worker": 0}
	reasons = wakeReasons(session, cfg, nil, poolDesired, nil, nil, clk)
	if containsWakeReason(reasons, WakeConfig) {
		t.Errorf("session should not wake when demand is 0, got %v", reasons)
	}
}

func TestWakeReasons_StaleCreatingWithoutPendingClaimDoesNotWakeCreate(t *testing.T) {
	now := time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "worker-b1",
		"state":        "creating",
	})
	session.CreatedAt = now.Add(-2 * time.Minute)

	reasons := wakeReasons(session, &config.City{}, nil, nil, nil, nil, clk)
	if containsWakeReason(reasons, WakeCreate) {
		t.Fatalf("stale creating session should not wake for create, got %v", reasons)
	}
}

func TestWakeReasons_FreshCreatingWithoutPendingClaimStillWakesCreate(t *testing.T) {
	now := time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "worker-b1",
		"state":        "creating",
	})
	session.CreatedAt = now.Add(-30 * time.Second)

	reasons := wakeReasons(session, &config.City{}, nil, nil, nil, nil, clk)
	if !containsWakeReason(reasons, WakeCreate) {
		t.Fatalf("fresh creating session should wake for create, got %v", reasons)
	}
}

func TestWakeReasons_PendingCreateClaimKeepsWakeCreateAfterCreatingGoesStale(t *testing.T) {
	now := time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	session := makeBead("b1", map[string]string{
		"template":             "worker",
		"session_name":         "worker-b1",
		"state":                "creating",
		"pending_create_claim": "true",
	})
	session.CreatedAt = now.Add(-2 * time.Minute)

	reasons := wakeReasons(session, &config.City{}, nil, nil, nil, nil, clk)
	if !containsWakeReason(reasons, WakeCreate) {
		t.Fatalf("session with pending_create_claim should wake for create even when stale, got %v", reasons)
	}
}

func TestWakeReasons_DrainedSleepPoolSessionDoesNotGetWakeConfig(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
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

func TestWakeReasons_DemandWakesSession(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
		"pool_slot":    "1",
	})

	// Demand exists: poolDesired=1 → session within desired → WakeConfig.
	poolDesired := map[string]int{"worker": 1}
	reasons := wakeReasons(session, cfg, nil, poolDesired, nil, nil, clk)
	if len(reasons) != 1 || reasons[0] != WakeConfig {
		t.Errorf("session with demand should get WakeConfig, got %v", reasons)
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

func TestWakeReasons_WorkSetEmitsWakeWork(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
	})

	// workSet includes the template — should produce WakeWork.
	workSet := map[string]bool{"worker": true}
	reasons := wakeReasons(session, cfg, nil, nil, workSet, nil, clk)
	if !containsWakeReason(reasons, WakeWork) {
		t.Errorf("session with work should get WakeWork, got %v", reasons)
	}
}

func TestWakeReasons_WakeWorkSuppressedByWaitHold(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "test-worker",
		"wait_hold":    "true",
	})

	workSet := map[string]bool{"worker": true}
	reasons := wakeReasons(session, cfg, nil, nil, workSet, nil, clk)
	if containsWakeReason(reasons, WakeWork) {
		t.Errorf("wait-hold should suppress WakeWork, got %v", reasons)
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
			{Name: "pooled", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		},
	}

	poolDesired := map[string]int{"pooled": 2}
	workSet := map[string]bool{"pooled": true}

	// With demand > 0, any session for this template gets WakeConfig.
	s1 := makeBead("b1", map[string]string{
		"template":     "pooled",
		"session_name": "test-pooled-1",
	})
	reasons := wakeReasons(s1, cfg, nil, poolDesired, workSet, nil, clk)
	if !containsWakeReason(reasons, WakeConfig) {
		t.Errorf("session should get WakeConfig when demand exists, got %v", reasons)
	}

	// With demand = 0, no sessions get WakeConfig.
	poolDesiredZero := map[string]int{"pooled": 0}
	reasons = wakeReasons(s1, cfg, nil, poolDesiredZero, workSet, nil, clk)
	if containsWakeReason(reasons, WakeConfig) {
		t.Errorf("session should NOT get WakeConfig when demand is 0, got %v", reasons)
	}
}

func TestWakeReasons_DependencyOnlyPoolSlotDoesNotWakeOnWork(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "pooled", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
	}

	reasons := wakeReasons(makeBead("b1", map[string]string{
		"template":        "pooled",
		"session_name":    "test-pooled-1",
		"pool_slot":       "1",
		"dependency_only": "true",
	}), cfg, nil, map[string]int{"pooled": 1}, map[string]bool{"pooled": true}, nil, clk)

	for _, r := range reasons {
		if r == WakeConfig {
			t.Fatalf("dependency-only pool slot should not get WakeConfig, got %v", reasons)
		}
	}
}

func TestWakeReasons_ManualPoolSessionGetsWakeConfigOnImplicitAgent(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "pooled", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
	}

	reasons := wakeReasons(makeBead("b1", map[string]string{
		"template":       "pooled",
		"session_name":   "manual-pooled",
		"manual_session": "true",
	}), cfg, nil, map[string]int{"pooled": 0}, nil, nil, clk)

	// Manual sessions on multi-session (implicit) agents are config-eligible
	// and should get WakeConfig so they survive the reconciler.
	foundWakeConfig := false
	for _, r := range reasons {
		if r == WakeConfig {
			foundWakeConfig = true
			break
		}
	}
	if !foundWakeConfig {
		t.Fatalf("manual pool session should get WakeConfig, got %v", reasons)
	}
}

func TestWakeReasons_SessionOriginManualPoolSessionGetsWakeConfigOnImplicitAgent(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "pooled", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
	}

	reasons := wakeReasons(makeBead("b1", map[string]string{
		"template":       "pooled",
		"session_name":   "manual-pooled",
		"session_origin": "manual",
	}), cfg, nil, map[string]int{"pooled": 0}, nil, nil, clk)

	foundWakeConfig := false
	for _, r := range reasons {
		if r == WakeConfig {
			foundWakeConfig = true
			break
		}
	}
	if !foundWakeConfig {
		t.Fatalf("manual session_origin pool session should get WakeConfig, got %v", reasons)
	}
}

func TestWakeReasons_ManualFixedTemplateSessionGetsWakeConfig(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}

	reasons := wakeReasons(makeBead("b1", map[string]string{
		"template":       "worker",
		"session_name":   "manual-worker",
		"session_origin": "manual",
	}), cfg, nil, map[string]int{"worker": 0}, nil, nil, clk)

	if !containsWakeReason(reasons, WakeConfig) {
		t.Fatalf("manual fixed-template session should get WakeConfig, got %v", reasons)
	}
}

func TestWakeReasons_UsesLegacyAgentLabelTemplate(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		},
	}

	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "custom-worker-1",
		"pool_slot":    "1",
	})
	session.Labels = []string{sessionBeadLabel, "agent:frontend/worker-1"}

	poolDesired := map[string]int{"frontend/worker": 1}

	reasons := wakeReasons(session, cfg, nil, poolDesired, nil, nil, clk)
	if len(reasons) != 1 || reasons[0] != WakeConfig {
		t.Fatalf("wakeReasons(legacy labeled pool worker) = %v, want [WakeConfig]", reasons)
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
		if strings.Contains(command, "gc.routed_to=worker") {
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
			{Name: "polecat", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
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
			{Name: "polecat", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
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

func TestRecordWakeFailure_ClearsStartedConfigHash(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"session_key":         "old-key",
		"started_config_hash": "abc123",
	})

	recordWakeFailure(&session, store, clk)

	if session.Metadata["session_key"] != "" {
		t.Errorf("session_key = %q, want empty", session.Metadata["session_key"])
	}
	if session.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash = %q, want empty (so next wake uses --session-id, not --resume)", session.Metadata["started_config_hash"])
	}
}

func TestRecordWakeFailure_ClearsStartedConfigHashWhenSessionKeyAlreadyEmpty(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"started_config_hash": "abc123",
	})

	recordWakeFailure(&session, store, clk)

	if session.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash = %q, want empty", session.Metadata["started_config_hash"])
	}
	if session.Metadata["continuation_reset_pending"] != "true" {
		t.Errorf("continuation_reset_pending = %q, want true", session.Metadata["continuation_reset_pending"])
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

func TestCapWakeConfigByDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(10)},
		},
	}
	poolDesired := map[string]int{"worker": 2}

	// 5 asleep sessions, all get WakeConfig from evaluateWakeReasons.
	// But desired is 2, so only 2 should keep WakeConfig.
	sessions := make([]beads.Bead, 5)
	for i := range sessions {
		sessions[i] = makeBead(fmt.Sprintf("s%d", i), map[string]string{
			"template":     "worker",
			"session_name": fmt.Sprintf("worker-%d", i),
			"state":        "asleep",
		})
	}

	evals := computeWakeEvaluations(sessions, cfg, nil, poolDesired, nil, nil, &clock.Fake{Time: time.Now()})

	wakeCount := 0
	for _, eval := range evals {
		if containsWakeReason(eval.Reasons, WakeConfig) {
			wakeCount++
		}
	}
	if wakeCount != 2 {
		t.Errorf("WakeConfig count = %d, want 2 (poolDesired)", wakeCount)
	}
}

func TestCapWakeConfigByDemand_ActiveCountsAgainstBudget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(10)},
		},
	}
	poolDesired := map[string]int{"worker": 3}

	// 1 active (creating), 4 asleep. Desired is 3.
	// Active counts against budget: 3 - 1 = 2 asleep should wake.
	sessions := []beads.Bead{
		makeBead("s0", map[string]string{
			"template": "worker", "session_name": "worker-0", "state": "creating",
		}),
		makeBead("s1", map[string]string{
			"template": "worker", "session_name": "worker-1", "state": "asleep",
		}),
		makeBead("s2", map[string]string{
			"template": "worker", "session_name": "worker-2", "state": "asleep",
		}),
		makeBead("s3", map[string]string{
			"template": "worker", "session_name": "worker-3", "state": "asleep",
		}),
		makeBead("s4", map[string]string{
			"template": "worker", "session_name": "worker-4", "state": "asleep",
		}),
	}

	evals := computeWakeEvaluations(sessions, cfg, nil, poolDesired, nil, nil, &clock.Fake{Time: time.Now()})

	asleepWakes := 0
	for _, s := range sessions {
		if s.Metadata["state"] == "asleep" && containsWakeReason(evals[s.ID].Reasons, WakeConfig) {
			asleepWakes++
		}
	}
	if asleepWakes != 2 {
		t.Errorf("asleep sessions with WakeConfig = %d, want 2 (desired 3 minus 1 active)", asleepWakes)
	}
}

func TestIsPoolExcess(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
			{Name: "singleton"},
		},
	}
	poolDesired := map[string]int{"worker": 3}

	tests := []struct {
		name     string
		template string
		want     bool
	}{
		{"demand exists", "worker", false},
		{"no demand", "singleton", false},
		{"unknown template", "missing", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := makeBead("b1", map[string]string{
				"template": tt.template,
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
	clk := &clock.Fake{Time: time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)}

	session := makeBead("b1", map[string]string{
		"state": "asleep",
	})

	healState(&session, true, store, clk)
	if session.Metadata["state"] != "awake" {
		t.Errorf("state = %q, want awake", session.Metadata["state"])
	}

	healState(&session, false, store, clk)
	if session.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep", session.Metadata["state"])
	}

	// No-op when already correct.
	prevCalls := len(store.metadata["b1"])
	healState(&session, false, store, clk)
	if len(store.metadata["b1"]) != prevCalls {
		t.Error("healState should not write when state unchanged")
	}
}

func TestHealState_DeadActiveHealsToAsleep(t *testing.T) {
	store := newTestStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)}

	session := makeBead("b1", map[string]string{
		"state": "active",
	})

	healState(&session, false, store, clk)
	if session.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", session.Metadata["state"])
	}
}

func TestHealState_PreservesCreatingWhileStartRequested(t *testing.T) {
	store := newTestStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)}

	session := makeBead("b1", map[string]string{
		"state":                "creating",
		"pending_create_claim": "true",
	})

	healState(&session, false, store, clk)
	if session.Metadata["state"] != "creating" {
		t.Fatalf("state = %q, want creating", session.Metadata["state"])
	}
}

func TestHealState_PreservesFreshCreatingWithoutPendingClaim(t *testing.T) {
	store := newTestStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)}

	session := makeBead("b1", map[string]string{
		"state": "creating",
	})
	session.CreatedAt = clk.Now().Add(-30 * time.Second)

	healState(&session, false, store, clk)
	if session.Metadata["state"] != "creating" {
		t.Fatalf("state = %q, want creating", session.Metadata["state"])
	}
}

func TestHealState_StaleCreatingWithoutPendingClaimHealsToAsleep(t *testing.T) {
	store := newTestStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 29, 4, 0, 0, 0, time.UTC)}

	session := makeBead("b1", map[string]string{
		"state": "creating",
	})
	session.CreatedAt = clk.Now().Add(-2 * time.Minute)

	healState(&session, false, store, clk)
	if session.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", session.Metadata["state"])
	}
}

func TestHealStatePatchProjectsRuntimeLiveness(t *testing.T) {
	now := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	tests := []struct {
		name    string
		alive   bool
		session beads.Bead
		want    map[string]string
	}{
		{
			name:  "alive runtime writes awake advisory state",
			alive: true,
			session: makeBead("b1", map[string]string{
				"state": "asleep",
			}),
			want: map[string]string{"state": "awake"},
		},
		{
			name:  "drained compatibility state becomes asleep with drained reason",
			alive: false,
			session: makeBead("b1", map[string]string{
				"state": "drained",
			}),
			want: map[string]string{
				"state":        "asleep",
				"sleep_reason": "drained",
			},
		},
		{
			name:  "fresh creating stays creating without write",
			alive: false,
			session: func() beads.Bead {
				b := makeBead("b1", map[string]string{"state": "creating"})
				b.CreatedAt = now.Add(-30 * time.Second)
				return b
			}(),
			want: nil,
		},
		{
			name:  "dead blank legacy state heals to asleep",
			alive: false,
			session: makeBead("b1", map[string]string{
				"state": "",
			}),
			want: map[string]string{"state": "asleep"},
		},
		{
			name:  "dead blank legacy state with create claim heals to creating",
			alive: false,
			session: makeBead("b1", map[string]string{
				"state":                "",
				"pending_create_claim": "true",
			}),
			want: map[string]string{"state": "creating"},
		},
		{
			name:  "stale creating heals to asleep and resets stale resume identity",
			alive: false,
			session: func() beads.Bead {
				b := makeBead("b1", map[string]string{
					"state":               "creating",
					"session_key":         "old-key",
					"started_config_hash": "old-hash",
				})
				b.CreatedAt = now.Add(-2 * time.Minute)
				return b
			}(),
			want: map[string]string{
				"state":                      "asleep",
				"session_key":                "",
				"started_config_hash":        "",
				"continuation_reset_pending": "true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := healStatePatch(tt.session, tt.alive, clk)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("healStatePatch = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHealStatePatchNilClockKeepsCreatingFresh(t *testing.T) {
	session := makeBead("b1", map[string]string{
		"state": "creating",
	})
	session.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	if got := healStatePatch(session, false, nil); got != nil {
		t.Fatalf("healStatePatch with nil clock = %#v, want nil patch for fresh-compatible creating", got)
	}
}

func TestHealState_ClearsStaleResumeMetadata(t *testing.T) {
	tests := []struct {
		name                   string
		prevState              string
		sleepReason            string
		wakeMode               string
		sessionKey             string
		startedConfigHash      string
		wantKeyCleared         bool
		wantStartedHashCleared bool
	}{
		{
			name:                   "active with no drain reason — resume metadata cleared",
			prevState:              "active",
			sleepReason:            "",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         true,
			wantStartedHashCleared: true,
		},
		{
			name:                   "awake with no drain reason — resume metadata cleared",
			prevState:              "awake",
			sleepReason:            "",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         true,
			wantStartedHashCleared: true,
		},
		{
			name:                   "creating with no drain reason — resume metadata cleared",
			prevState:              "creating",
			sleepReason:            "",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         true,
			wantStartedHashCleared: true,
		},
		{
			name:                   "idle drain — resume metadata preserved",
			prevState:              "active",
			sleepReason:            "idle",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "idle-timeout drain — resume metadata preserved",
			prevState:              "active",
			sleepReason:            "idle-timeout",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "no-wake-reason drain — resume metadata preserved",
			prevState:              "active",
			sleepReason:            "no-wake-reason",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "config-drift drain — resume metadata preserved",
			prevState:              "active",
			sleepReason:            "config-drift",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "user-hold drain — resume metadata preserved",
			prevState:              "active",
			sleepReason:            "user-hold",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "wait-hold drain — resume metadata preserved",
			prevState:              "active",
			sleepReason:            "wait-hold",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "drained — resume metadata preserved",
			prevState:              "active",
			sleepReason:            "drained",
			wakeMode:               "resume",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "drained with wake_mode=fresh — resume metadata preserved (identity cleared at drain-ack/completeDrain)",
			prevState:              "active",
			sleepReason:            "drained",
			wakeMode:               "fresh",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "asleep prev state — resume metadata preserved (not in active set)",
			prevState:              "asleep",
			sleepReason:            "",
			sessionKey:             "abc-123",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: false,
		},
		{
			name:                   "no session key — clear stale started hash",
			prevState:              "active",
			sleepReason:            "",
			sessionKey:             "",
			startedConfigHash:      "hash-before",
			wantKeyCleared:         false,
			wantStartedHashCleared: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore()
			clk := &clock.Fake{Time: time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)}
			session := makeBead("b1", map[string]string{
				"state":               tt.prevState,
				"sleep_reason":        tt.sleepReason,
				"wake_mode":           tt.wakeMode,
				"session_key":         tt.sessionKey,
				"started_config_hash": tt.startedConfigHash,
			})
			healState(&session, false, store, clk)
			keyAfter := session.Metadata["session_key"]
			startedHashAfter := session.Metadata["started_config_hash"]
			if tt.wantKeyCleared && keyAfter != "" {
				t.Errorf("session_key should be cleared, got %q", keyAfter)
			}
			if !tt.wantKeyCleared && keyAfter != tt.sessionKey {
				t.Errorf("session_key should be preserved as %q, got %q", tt.sessionKey, keyAfter)
			}
			if tt.wantStartedHashCleared && startedHashAfter != "" {
				t.Errorf("started_config_hash should be cleared, got %q", startedHashAfter)
			}
			if !tt.wantStartedHashCleared && startedHashAfter != tt.startedConfigHash {
				t.Errorf("started_config_hash should be preserved as %q, got %q", tt.startedConfigHash, startedHashAfter)
			}
			if tt.wantKeyCleared || tt.wantStartedHashCleared {
				if session.Metadata["continuation_reset_pending"] != "true" {
					t.Error("continuation_reset_pending should be set when resume metadata is cleared")
				}
			}
		})
	}
}

func TestCheckStability_RapidExitAfterHealStateKeepsStartedConfigHashCleared(t *testing.T) {
	now := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"state":               "active",
		"session_key":         "old-key",
		"started_config_hash": "hash-before",
		"last_woke_at":        now.Add(-5 * time.Second).UTC().Format(time.RFC3339),
	})

	healState(&session, false, store, clk)
	if session.Metadata["session_key"] != "" {
		t.Fatalf("healState session_key = %q, want empty", session.Metadata["session_key"])
	}
	if session.Metadata["started_config_hash"] != "" {
		t.Fatalf("healState started_config_hash = %q, want empty", session.Metadata["started_config_hash"])
	}
	if !checkStability(&session, nil, false, nil, store, clk) {
		t.Fatal("checkStability should record the rapid exit")
	}
	if session.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q after recordWakeFailure, want empty", session.Metadata["started_config_hash"])
	}
	if session.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", session.Metadata["continuation_reset_pending"])
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
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
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

// --- Churn detection tests (ga-cy4: context exhaustion circuit breaker) ---

func TestCheckChurn_AliveReturnsFalse(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-90 * time.Second).Format(time.RFC3339),
	})

	if checkChurn(&session, nil, true, dt, store, clk) {
		t.Error("alive session should not trigger churn")
	}
}

func TestCheckChurn_NonProductiveDeath(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	// Woke 90 seconds ago — past stabilityThreshold (30s) but before
	// churnProductivityThreshold (5min). This is the churn band.
	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-90 * time.Second).Format(time.RFC3339),
		"churn_count":  "0",
	})

	if !checkChurn(&session, nil, false, dt, store, clk) {
		t.Error("non-productive death should trigger churn")
	}
	if session.Metadata["churn_count"] != "1" {
		t.Errorf("churn_count = %q, want 1", session.Metadata["churn_count"])
	}
	// Edge-triggered: last_woke_at should be cleared.
	if session.Metadata["last_woke_at"] != "" {
		t.Error("last_woke_at should be cleared after churn detection")
	}
}

func TestCheckChurn_RapidExitIgnored(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	// Died within stabilityThreshold — handled by checkStability, not churn.
	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-10 * time.Second).Format(time.RFC3339),
	})

	if checkChurn(&session, nil, false, dt, store, clk) {
		t.Error("rapid exit should not trigger churn (handled by checkStability)")
	}
}

func TestCheckChurn_ProductiveSessionIgnored(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	// Ran for 10 minutes — productive, not churn.
	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-10 * time.Minute).Format(time.RFC3339),
	})

	if checkChurn(&session, nil, false, dt, store, clk) {
		t.Error("productive session death should not trigger churn")
	}
}

func TestCheckChurn_DeadProductiveSessionClearsChurnCount(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	// Session ran for 10 minutes (past churnProductivityThreshold) but is now
	// dead. Pre-existing churn_count=2 must be cleared so it doesn't carry
	// over and cause premature quarantine on the next incarnation.
	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-10 * time.Minute).Format(time.RFC3339),
		"churn_count":  "2",
	})

	if checkChurn(&session, nil, false, dt, store, clk) {
		t.Error("dead productive session should not trigger churn")
	}
	if session.Metadata["churn_count"] != "0" {
		t.Errorf("churn_count = %q, want 0 (should be cleared for productive session)", session.Metadata["churn_count"])
	}
}

func TestCheckChurn_ClearedLastWokeAtSkipsChurn(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()

	// When the restart handler clears last_woke_at, checkChurn should
	// skip the session (no timestamp to measure against). This is how
	// intentional restarts avoid false churn counts.
	session := makeBead("b1", map[string]string{
		"last_woke_at": "",
		"churn_count":  "2",
	})

	if checkChurn(&session, nil, false, dt, store, clk) {
		t.Error("session with cleared last_woke_at should not trigger churn")
	}
	if session.Metadata["churn_count"] != "2" {
		t.Errorf("churn_count = %q, want 2 (should not have changed)", session.Metadata["churn_count"])
	}
}

func TestCheckChurn_DrainingNotCounted(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()
	dt.set("b1", &drainState{reason: "idle"})

	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-90 * time.Second).Format(time.RFC3339),
	})

	if checkChurn(&session, nil, false, dt, store, clk) {
		t.Error("draining session death should not count as churn")
	}
}

func TestCheckChurn_SubprocessProviderSkipped(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()
	dt := newDrainTracker()
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "subprocess"},
	}

	session := makeBead("b1", map[string]string{
		"last_woke_at": now.Add(-90 * time.Second).Format(time.RFC3339),
	})

	if checkChurn(&session, cfg, false, dt, store, clk) {
		t.Error("subprocess sessions should not trigger churn")
	}
}

func TestRecordChurn_Quarantine(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"churn_count": "2", // one below threshold (defaultMaxChurnCycles=3)
	})

	recordChurn(&session, store, clk)

	if session.Metadata["churn_count"] != "3" {
		t.Errorf("churn_count = %q, want 3", session.Metadata["churn_count"])
	}
	if session.Metadata["quarantined_until"] == "" {
		t.Error("expected quarantine to be set at max churn cycles")
	}
	if session.Metadata["sleep_reason"] != "context-churn" {
		t.Errorf("sleep_reason = %q, want context-churn", session.Metadata["sleep_reason"])
	}
}

func TestRecordChurn_BelowThreshold(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"churn_count": "0",
	})

	recordChurn(&session, store, clk)

	if session.Metadata["churn_count"] != "1" {
		t.Errorf("churn_count = %q, want 1", session.Metadata["churn_count"])
	}
	if session.Metadata["quarantined_until"] != "" {
		t.Error("should not quarantine below threshold")
	}
}

func TestRecordChurn_ClearsSessionKey(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"churn_count": "0",
		"session_key": "old-key-123",
	})

	recordChurn(&session, store, clk)

	if session.Metadata["session_key"] != "" {
		t.Error("session_key should be cleared on churn")
	}
	if session.Metadata["continuation_reset_pending"] != "true" {
		t.Error("continuation_reset_pending should be set on churn")
	}
}

func TestClearChurn(t *testing.T) {
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"churn_count": "2",
	})

	clearChurn(&session, store)

	if session.Metadata["churn_count"] != "0" {
		t.Errorf("churn_count = %q, want 0", session.Metadata["churn_count"])
	}
}

func TestClearChurn_NoopWhenZero(t *testing.T) {
	store := newTestStore()

	session := makeBead("b1", map[string]string{
		"churn_count": "0",
	})

	clearChurn(&session, store)

	// Should not have written to store (no-op).
	if _, ok := store.metadata["b1"]; ok {
		t.Error("clearChurn should be a no-op when churn_count is already 0")
	}
}

func TestProductiveLongEnough(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	tests := []struct {
		name    string
		wokeAgo time.Duration
		want    bool
	}{
		{"just started", 30 * time.Second, false},
		{"under threshold", 4 * time.Minute, false},
		{"at threshold", 5 * time.Minute, true},
		{"well past threshold", 30 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := makeBead("b1", map[string]string{
				"last_woke_at": now.Add(-tt.wokeAgo).Format(time.RFC3339),
			})
			if got := productiveLongEnough(session, clk); got != tt.want {
				t.Errorf("productiveLongEnough(%v ago) = %v, want %v", tt.wokeAgo, got, tt.want)
			}
		})
	}
}

func TestProductiveLongEnough_NoLastWokeAt(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	session := makeBead("b1", map[string]string{})
	if productiveLongEnough(session, clk) {
		t.Error("should return false when last_woke_at is empty")
	}
}

func TestHealExpiredTimers_ClearsChurnOnQuarantineExpiry(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := newTestStore()

	// Quarantine expired 1 minute ago. Has churn_count from context-churn.
	session := makeBead("b1", map[string]string{
		"quarantined_until": now.Add(-1 * time.Minute).Format(time.RFC3339),
		"wake_attempts":     "5",
		"churn_count":       "3",
		"sleep_reason":      "context-churn",
	})

	healExpiredTimers(&session, store, clk)

	if session.Metadata["quarantined_until"] != "" {
		t.Error("quarantined_until should be cleared")
	}
	if session.Metadata["wake_attempts"] != "0" {
		t.Errorf("wake_attempts = %q, want 0", session.Metadata["wake_attempts"])
	}
	if session.Metadata["churn_count"] != "0" {
		t.Errorf("churn_count = %q, want 0", session.Metadata["churn_count"])
	}
	if session.Metadata["sleep_reason"] != "" {
		t.Errorf("sleep_reason = %q, want empty", session.Metadata["sleep_reason"])
	}
}
