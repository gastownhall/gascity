package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func boolPtr(v bool) *bool { return &v }

type routedSleepProvider struct {
	runtime.Provider
	capabilities runtime.ProviderCapabilities
	sleep        runtime.SessionSleepCapability
}

func (p routedSleepProvider) Capabilities() runtime.ProviderCapabilities {
	return p.capabilities
}

func (p routedSleepProvider) SleepCapability(string) runtime.SessionSleepCapability {
	return p.sleep
}

func startedSessionNames(sp *runtime.Fake) map[string]bool {
	started := make(map[string]bool)
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			started[call.Name] = true
		}
	}
	return started
}

func TestResolveSessionSleepPolicyPrecedence(t *testing.T) {
	cfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Rigs: []config.Rig{{
			Name: "rig-a",
			SessionSleep: config.SessionSleepConfig{
				InteractiveResume: "5m",
			},
		}},
		Agents: []config.Agent{{
			Name:                 "worker",
			Dir:                  "rig-a",
			SleepAfterIdle:       "10s",
			SleepAfterIdleSource: "agent",
		}},
	}

	session := makeBead("b1", map[string]string{
		"template":     "rig-a/worker",
		"session_name": "worker",
	})

	policy := resolveSessionSleepPolicy(session, cfg, runtime.NewFake())
	if policy.Class != config.SessionSleepInteractiveResume {
		t.Fatalf("Class = %q, want %q", policy.Class, config.SessionSleepInteractiveResume)
	}
	if policy.Requested != "10s" || policy.Effective != "10s" {
		t.Fatalf("requested/effective = %q/%q, want 10s/10s", policy.Requested, policy.Effective)
	}
	if policy.Source != "agent" {
		t.Fatalf("Source = %q, want agent", policy.Source)
	}
}

func TestWakeReasonsInteractiveResumeGraceWindow(t *testing.T) {
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	session := makeBead("b1", map[string]string{
		"template":            "worker",
		"session_name":        "worker",
		"started_config_hash": "started",
		"detached_at":         now.Add(-30 * time.Second).Format(time.RFC3339),
	})

	reasons := wakeReasons(session, cfg, runtime.NewFake(), nil, nil, nil, &clock.Fake{Time: now})
	if !containsWakeReason(reasons, WakeKeepWarm) {
		t.Fatalf("expected WakeKeepWarm during keep-warm window, got %v", reasons)
	}

	expired := &clock.Fake{Time: now.Add(31 * time.Second)}
	reasons = wakeReasons(session, cfg, runtime.NewFake(), nil, nil, nil, expired)
	if containsWakeReason(reasons, WakeKeepWarm) {
		t.Fatalf("did not expect WakeKeepWarm after keep-warm expiry, got %v", reasons)
	}
}

func TestWakeReasonsNonInteractiveImmediateUsesHardWakeReasons(t *testing.T) {
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			NonInteractive: "0s",
		},
		Agents: []config.Agent{{
			Name:   "worker",
			Attach: boolPtr(false),
		}},
	}
	session := makeBead("b1", map[string]string{
		"template":            "worker",
		"session_name":        "worker",
		"started_config_hash": "started",
	})

	reasons := wakeReasons(session, cfg, runtime.NewFake(), nil, nil, nil, &clock.Fake{Time: now})
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons without hard wake triggers, got %v", reasons)
	}

	// Demand via poolDesired → WakeConfig (replaces WakeWork).
	reasons = wakeReasons(session, cfg, runtime.NewFake(), map[string]int{"worker": 1}, nil, nil, &clock.Fake{Time: now})
	if len(reasons) != 1 || reasons[0] != WakeConfig {
		t.Fatalf("expected [WakeConfig], got %v", reasons)
	}

	sp := runtime.NewFake()
	sp.SetPendingInteraction("worker", &runtime.PendingInteraction{RequestID: "req-1"})
	reasons = wakeReasons(session, cfg, sp, nil, nil, nil, &clock.Fake{Time: now})
	if len(reasons) != 1 || reasons[0] != WakePending {
		t.Fatalf("expected [WakePending], got %v", reasons)
	}
}

func TestWakeReasons_DependencyOnlyFloorDoesNotGetWakeConfig(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "db", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), Attach: boolPtr(false)},
			{Name: "api", Attach: boolPtr(false), DependsOn: []string{"db"}},
		},
	}
	session := makeBead("b1", map[string]string{
		"template":            "db",
		"session_name":        "db-1",
		"pool_slot":           "1",
		"dependency_only":     "true",
		"started_config_hash": "started",
	})

	reasons := wakeReasons(session, cfg, runtime.NewFake(), map[string]int{"db": 1}, nil, nil, &clock.Fake{Time: time.Now().UTC()})
	if containsWakeReason(reasons, WakeConfig) {
		t.Fatalf("dependency-only slot should not get WakeConfig, got %v", reasons)
	}
}

func TestReconcileDetachedAtUsesRoutedSleepCapability(t *testing.T) {
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker",
		},
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	base := runtime.NewFake()
	provider := routedSleepProvider{
		Provider:     base,
		capabilities: runtime.ProviderCapabilities{},
		sleep:        runtime.SessionSleepCapabilityFull,
	}
	policy := resolveSessionSleepPolicy(session, cfg, provider)
	if policy.Capability != runtime.SessionSleepCapabilityFull {
		t.Fatalf("policy capability = %q, want %q", policy.Capability, runtime.SessionSleepCapabilityFull)
	}

	reconcileDetachedAt(&session, store, policy, true, provider, &clock.Fake{Time: now})

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata["detached_at"] == "" {
		t.Fatal("detached_at was not recorded for routed full-capability session")
	}
}

func TestReconcileSessionBeads_StartsIdleDrainAfterGrace(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"last_woke_at": ts,
		"detached_at":  ts,
	})
	session.Metadata["last_woke_at"] = ts
	session.Metadata["detached_at"] = ts
	env.sp.WaitForIdleErrors["worker"] = nil

	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames, env.cfg, env.sp,
		env.store, nil, nil, nil, env.dt, poolDesired, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)
	waitForIdleProbeReady(t, env.dt, session.ID)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames, env.cfg, env.sp,
		env.store, nil, nil, nil, env.dt, poolDesired, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected idle drain to start")
	}
	if ds.reason != "idle" {
		t.Fatalf("drain reason = %q, want idle", ds.reason)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata["config_wake_suppressed"] != "true" {
		t.Fatalf("config_wake_suppressed = %q, want true", got.Metadata["config_wake_suppressed"])
	}
	foundProbe := false
	for _, call := range env.sp.Calls {
		if call.Method == "WaitForIdle" && call.Name == "worker" {
			foundProbe = true
			break
		}
	}
	if !foundProbe {
		t.Fatal("expected WaitForIdle probe before idle drain")
	}
}

func TestReconcileSessionBeads_WaitHoldBypassesIdleProbe(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"last_woke_at": ts,
		"detached_at":  ts,
		"sleep_intent": "wait-hold",
	})
	session.Metadata["last_woke_at"] = ts
	session.Metadata["detached_at"] = ts
	session.Metadata["sleep_intent"] = "wait-hold"

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected wait-hold drain to start immediately")
	}
	if ds.reason != "wait-hold" {
		t.Fatalf("drain reason = %q, want wait-hold", ds.reason)
	}
	for _, call := range env.sp.Calls {
		if call.Method == "WaitForIdle" && call.Name == "worker" {
			t.Fatal("did not expect idle probe for explicit wait-hold sleep")
		}
	}
}

func TestReconcileSessionBeads_IdleLatchedSessionDoesNotWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	policy := resolveSessionSleepPolicy(session, env.cfg, env.sp)
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"sleep_reason":             "idle",
		"sleep_policy_fingerprint": policy.Fingerprint,
		"slept_at":                 ts,
	})
	session.Metadata["sleep_reason"] = "idle"
	session.Metadata["sleep_policy_fingerprint"] = policy.Fingerprint
	session.Metadata["slept_at"] = ts

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	if starts := startedSessionNames(env.sp); len(starts) != 0 {
		t.Fatalf("unexpected starts: %v", starts)
	}
}

func TestReconcileSessionBeads_ConfigChangeDoesNotWakeIdleLatchedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	oldCfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.cfg = oldCfg
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	oldPolicy := resolveSessionSleepPolicy(session, oldCfg, env.sp)
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"sleep_reason":             "idle",
		"sleep_policy_fingerprint": oldPolicy.Fingerprint,
		"slept_at":                 ts,
	})
	session.Metadata["sleep_reason"] = "idle"
	session.Metadata["sleep_policy_fingerprint"] = oldPolicy.Fingerprint
	session.Metadata["slept_at"] = ts

	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	if starts := startedSessionNames(env.sp); len(starts) != 0 {
		t.Fatalf("did not expect worker to start, got %v", starts)
	}
}

func TestReconcileSessionBeads_ConfigChangeDoesNotRetryIdleLatchedSingletonWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.sp = runtime.NewFailFake()
	oldCfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.cfg = oldCfg
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	oldPolicy := resolveSessionSleepPolicy(session, oldCfg, env.sp)
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":                    "asleep",
		"sleep_reason":             "idle",
		"sleep_policy_fingerprint": oldPolicy.Fingerprint,
		"slept_at":                 ts,
	})
	session.Metadata["state"] = "asleep"
	session.Metadata["sleep_reason"] = "idle"
	session.Metadata["sleep_policy_fingerprint"] = oldPolicy.Fingerprint
	session.Metadata["slept_at"] = ts

	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	env.reconcile([]beads.Bead{session})
	env.reconcile([]beads.Bead{session})

	startCalls := 0
	for _, call := range env.sp.Calls {
		if call.Method == "Start" && call.Name == "worker" {
			startCalls++
		}
	}
	if startCalls != 0 {
		t.Fatalf("start calls = %d, want 0", startCalls)
	}
}

func TestReconcileSessionBeads_ConfigChangeCancelsPendingIdleDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	oldCfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.cfg = oldCfg
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	oldPolicy := resolveSessionSleepPolicy(session, oldCfg, env.sp)
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":                    "active",
		"sleep_intent":             "idle-stop-pending",
		"sleep_policy_fingerprint": oldPolicy.Fingerprint,
		"last_woke_at":             ts,
		"detached_at":              ts,
	})
	session.Metadata["state"] = "active"
	session.Metadata["sleep_intent"] = "idle-stop-pending"
	session.Metadata["sleep_policy_fingerprint"] = oldPolicy.Fingerprint
	session.Metadata["last_woke_at"] = ts
	session.Metadata["detached_at"] = ts
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Time.Add(-30 * time.Second),
		deadline:   env.clk.Time.Add(2 * time.Minute),
		reason:     "idle",
		generation: 1,
	})

	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	if env.dt.get(session.ID) != nil {
		t.Fatal("idle drain should be canceled after config change")
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("worker should remain running after config change")
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata["sleep_intent"] != "" {
		t.Fatalf("sleep_intent = %q, want empty", got.Metadata["sleep_intent"])
	}
}

func TestReconcileSessionBeads_IdleTimeoutLeavesImmediateSleepPolicyAsleep(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name:           "worker",
			Attach:         boolPtr(false),
			IdleTimeout:    "5m",
			SleepAfterIdle: "0s",
		}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	lastWoke := env.clk.Time.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":        "active",
		"last_woke_at": lastWoke,
	})
	session.Metadata["state"] = "active"
	session.Metadata["last_woke_at"] = lastWoke

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	got := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil,
		env.dt,
		map[string]int{},
		nil,
		"",
		it,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	startCalls := 0
	for _, call := range env.sp.Calls {
		if call.Method == "Start" && call.Name == "worker" {
			startCalls++
		}
	}
	if startCalls != 1 {
		t.Fatalf("did not expect worker to restart, calls=%v", env.sp.Calls)
	}
}

func TestReconcileSessionBeads_IdleTimeoutDoesNotRetryWithoutExplicitWakeReason(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name:           "worker",
			Attach:         boolPtr(false),
			IdleTimeout:    "5m",
			SleepAfterIdle: "0s",
		}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	lastWoke := env.clk.Time.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":        "active",
		"last_woke_at": lastWoke,
	})
	session.Metadata["state"] = "active"
	session.Metadata["last_woke_at"] = lastWoke
	env.sp.StartErrors["worker"] = errors.New("boom")

	it := newFakeIdleTracker()
	it.idle["worker"] = true
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	got := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil,
		env.dt,
		map[string]int{},
		nil,
		"",
		it,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if got != 0 {
		t.Fatalf("planned wakes after failed restart = %d, want 0", got)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should stay stopped after failed restart")
	}
	failed, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get after failed restart: %v", err)
	}
	if failed.Metadata["sleep_reason"] != "idle-timeout" {
		t.Fatalf("sleep_reason after failed restart = %q, want idle-timeout", failed.Metadata["sleep_reason"])
	}
	if failed.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at after failed restart = %q, want empty", failed.Metadata["last_woke_at"])
	}

	delete(env.sp.StartErrors, "worker")
	got = reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{failed},
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil,
		env.dt,
		map[string]int{},
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if got != 0 {
		t.Fatalf("planned wakes after retry = %d, want 0", got)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should stay asleep without an explicit wake reason")
	}
	retried, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get after retry: %v", err)
	}
	if retried.Metadata["sleep_reason"] != "idle-timeout" {
		t.Fatalf("sleep_reason after retry = %q, want idle-timeout", retried.Metadata["sleep_reason"])
	}
}

func TestReconcileSessionBeads_RecoversPendingIdleSleep(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	policy := resolveSessionSleepPolicy(session, env.cfg, env.sp)
	lastWoke := env.clk.Time.Add(-10 * time.Second).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":                    "active",
		"sleep_intent":             "idle-stop-pending",
		"sleep_policy_fingerprint": policy.Fingerprint,
		"last_woke_at":             lastWoke,
	})
	session.Metadata["state"] = "active"
	session.Metadata["sleep_intent"] = "idle-stop-pending"
	session.Metadata["sleep_policy_fingerprint"] = policy.Fingerprint
	session.Metadata["last_woke_at"] = lastWoke

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
	if got.Metadata["sleep_intent"] != "" {
		t.Fatalf("sleep_intent = %q, want empty", got.Metadata["sleep_intent"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestRecoverPendingIdleSleep_PreservesPreDrainFingerprint(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		Title: "worker",
		Metadata: map[string]string{
			"session_name":             "worker",
			"state":                    "active",
			"sleep_intent":             "idle-stop-pending",
			"sleep_policy_fingerprint": "old-fingerprint",
			"last_woke_at":             clk.Time.Add(-10 * time.Second).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !recoverPendingIdleSleep(&session, store, false, clk) {
		t.Fatal("expected pending idle sleep to recover")
	}
	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata["sleep_policy_fingerprint"] != "old-fingerprint" {
		t.Fatalf("sleep_policy_fingerprint = %q, want preserved pre-drain value", got.Metadata["sleep_policy_fingerprint"])
	}
}

func TestReconcileSessionBeads_DoesNotRecoverPendingIdleSleepWhileZombieStillRunning(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			NonInteractive: "0s",
		},
		Agents: []config.Agent{{
			Name:   "worker",
			Attach: boolPtr(false),
		}},
	}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.Hints.ProcessNames = []string{"claude"}
	env.desiredState["worker"] = tp
	env.sp.Zombies["worker"] = true

	session := env.createSessionBead("worker", "worker")
	lastWoke := env.clk.Time.Add(-10 * time.Second).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":        "active",
		"sleep_intent": "idle-stop-pending",
		"last_woke_at": lastWoke,
	})
	session.Metadata["state"] = "active"
	session.Metadata["sleep_intent"] = "idle-stop-pending"
	session.Metadata["last_woke_at"] = lastWoke

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata["sleep_intent"] != "idle-stop-pending" {
		t.Fatalf("sleep_intent = %q, want idle-stop-pending while runtime still exists", got.Metadata["sleep_intent"])
	}
	if got.Metadata["sleep_reason"] != "" {
		t.Fatalf("sleep_reason = %q, want unchanged empty value", got.Metadata["sleep_reason"])
	}
}

func TestReconcileSessionBeads_IdleStopPendingRestartsDrainAsIdle(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			NonInteractive: "0s",
		},
		Agents: []config.Agent{{
			Name:   "worker",
			Attach: boolPtr(false),
		}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	lastWoke := env.clk.Time.Add(-10 * time.Second).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"state":        "active",
		"sleep_intent": "idle-stop-pending",
		"last_woke_at": lastWoke,
	})
	session.Metadata["state"] = "active"
	session.Metadata["sleep_intent"] = "idle-stop-pending"
	session.Metadata["last_woke_at"] = lastWoke

	if got := env.reconcile([]beads.Bead{session}); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected idle-stop-pending session to enter a drain after restart")
	}
	if ds.reason != "idle" {
		t.Fatalf("drain reason = %q, want idle", ds.reason)
	}
}

func TestReconcileSessionBeads_ClearsIdleProbeForMissingSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.dt.startIdleProbe("missing")
	if _, ok := env.dt.idleProbe("missing"); !ok {
		t.Fatal("expected probe state for missing session before reconcile")
	}

	if got := env.reconcile(nil); got != 0 {
		t.Fatalf("planned wakes = %d, want 0", got)
	}
	if _, ok := env.dt.idleProbe("missing"); ok {
		t.Fatal("expected missing-session idle probe to be cleared")
	}
}

func TestReconcileSessionBeads_AsleepSingletonsDoNotWakeViaScaleCheck(t *testing.T) {
	// Asleep sessions are never restarted by scaleCheck/poolDesired alone.
	// They wake only via direct assignment to their alias. The reconciler
	// creates fresh sessions to fill demand instead of reusing asleep ones.
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			NonInteractive: "0s",
		},
		Agents: []config.Agent{
			{Name: "db", Attach: boolPtr(false)},
			{Name: "api", Attach: boolPtr(false), DependsOn: []string{"db"}},
		},
	}
	env.addDesired("db", "db", false)
	env.addDesired("api", "api", false)
	dbSession := env.createSessionBead("db", "db")
	apiSession := env.createSessionBead("api", "api")
	cfgNames := configuredSessionNames(env.cfg, "", env.store)

	got := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{dbSession, apiSession},
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil,
		env.dt,
		map[string]int{"api": 1, "db": 1},
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if got != 0 {
		t.Fatalf("planned wakes = %d, want 0 (asleep sessions stay asleep)", got)
	}
}

func TestComputeWakeEvaluations_KeepWarmDoesNotPropagateDependencies(t *testing.T) {
	cfg := &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{
			{Name: "db"},
			{Name: "api", DependsOn: []string{"db"}},
		},
	}
	now := time.Now().UTC()
	sessions := []beads.Bead{
		makeBead("db-bead", map[string]string{
			"template":     "db",
			"session_name": "db",
		}),
		makeBead("api-bead", map[string]string{
			"template":     "api",
			"session_name": "api",
			"detached_at":  now.Add(-30 * time.Second).Format(time.RFC3339),
		}),
	}
	evals := computeWakeEvaluations(sessions, cfg, runtime.NewFake(), nil, nil, nil, &clock.Fake{Time: now})
	dbEval := evals["db-bead"]
	if containsWakeReason(dbEval.Reasons, WakeDependency) {
		t.Fatalf("db reasons = %v, did not want WakeDependency from keep-warm wake", dbEval.Reasons)
	}
	apiEval := evals["api-bead"]
	if !containsWakeReason(apiEval.Reasons, WakeKeepWarm) {
		t.Fatalf("api reasons = %v, want WakeKeepWarm", apiEval.Reasons)
	}
}

func TestSelectIdleProbeTargets_RotatesAcrossTicks(t *testing.T) {
	mkTarget := func(id string) wakeTarget {
		return wakeTarget{
			session: &beads.Bead{
				ID: id,
				Metadata: map[string]string{
					"session_name": id,
				},
			},
			alive: true,
		}
	}
	wakeTargets := []wakeTarget{
		mkTarget("one"),
		mkTarget("two"),
		mkTarget("three"),
		mkTarget("four"),
	}
	policy := resolvedSessionSleepPolicy{
		Class:      config.SessionSleepInteractiveResume,
		Effective:  "60s",
		Capability: runtime.SessionSleepCapabilityFull,
	}
	wakeEvals := map[string]wakeEvaluation{
		"one":   {Policy: policy, ConfigSuppressed: true},
		"two":   {Policy: policy, ConfigSuppressed: true},
		"three": {Policy: policy, ConfigSuppressed: true},
		"four":  {Policy: policy, ConfigSuppressed: true},
	}
	dt := newDrainTracker()

	first := selectIdleProbeTargets(wakeTargets, wakeEvals, dt)
	if len(first) != 3 {
		t.Fatalf("first selection = %d targets, want 3", len(first))
	}
	if first["four"] {
		t.Fatalf("first selection unexpectedly included fourth target: %v", first)
	}

	second := selectIdleProbeTargets(wakeTargets, wakeEvals, dt)
	if !second["four"] {
		t.Fatalf("second selection should rotate in fourth target, got %v", second)
	}
}

func TestSelectIdleProbeTargets_SkipsExplicitSleepIntent(t *testing.T) {
	dt := newDrainTracker()
	policy := resolvedSessionSleepPolicy{
		Class:      config.SessionSleepInteractiveResume,
		Effective:  "60s",
		Capability: runtime.SessionSleepCapabilityFull,
	}
	wakeTargets := []wakeTarget{{
		session: &beads.Bead{
			ID: "wait-hold",
			Metadata: map[string]string{
				"session_name": "worker",
				"sleep_intent": "wait-hold",
			},
		},
		alive: true,
	}}
	wakeEvals := map[string]wakeEvaluation{
		"wait-hold": {Policy: policy, ConfigSuppressed: true},
	}

	targets := selectIdleProbeTargets(wakeTargets, wakeEvals, dt)
	if len(targets) != 0 {
		t.Fatalf("selectIdleProbeTargets returned %v, want no probe targets", targets)
	}
}

func waitForIdleProbeReady(t *testing.T, dt *drainTracker, beadID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if probe, ok := dt.idleProbe(beadID); ok && probe.ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("idle probe for %s did not complete", beadID)
}
