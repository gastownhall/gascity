package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The "phantom olivia record" wedge (maintainer city, 2026-09-22).
//
// The record was NOT phantom: its bead existed in the session-class store the
// whole time. What it had was this shape — a configured on_demand named session
// backing a canonical-singleton template (max_active_sessions = 1), under an
// INTERACTIVE session_sleep policy, asleep with sleep_reason=idle and a
// sleep_policy_fingerprint equal to the current policy (the "idle latch").
//
//   - The asleep holder owns the canonical alias, so canonicalSingletonAliasHeld
//     suppresses the pool standby: the record satisfies the template's capacity.
//   - Routed (gc.routed_to, unassigned) work produces decision.Reason
//     "routed-demand" for the holder, and `gc session wake` produces a durable
//     wake_request=explicit. Both put the holder in the awake set.
//   - The reconciler's sleep-suppression pass then cancels the wake:
//     configWakeSuppressedInfo is true (idle latch), and
//     wakeDemandOverridesSleepSuppression only overrode it for assigned work,
//     min-active, or routed/pool demand on a NON-interactive policy. The start is
//     dropped and config_wake_suppressed=true is persisted — every tick, forever.
//
// Nothing else can serve the work (the standby is suppressed by the very record
// that refuses to wake), so the template is wedged until an operator closes the
// record. These tests pin that routed demand and an explicit wake both start the
// idle-latched holder, while plain config/pool demand still honors the latch.

type idleLatchedNamedHolderResult struct {
	woken        int
	running      bool
	starts       []string
	postSessions []beads.Bead
	routed       map[string]bool
	holder       beads.Bead
}

func reconcileIdleLatchedInteractiveNamedHolder(t *testing.T, routedWork bool, holderMeta map[string]string) idleLatchedNamedHolderResult {
	t.Helper()
	const identity = "olivia"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "5m",
			InteractiveFresh:  "5m",
		},
		Agents: []config.Agent{{
			Name:              identity,
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{Template: identity, Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)

	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 9, 22, 21, 52, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	if routedWork {
		if _, err := store.Create(beads.Bead{
			Title:    "Publish or reconcile canonical PR",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": identity},
		}); err != nil {
			t.Fatalf("Create(work): %v", err)
		}
	}
	holder, err := store.Create(beads.Bead{
		Title:  identity,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      identity,
			"template":                   identity,
			"state":                      "asleep",
			"generation":                 "2246",
			"instance_token":             "canonical-token",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	// Idle latch: slept idle under the policy that is still in force.
	holderInfo, err := sessionFrontDoor(store).Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(holder): %v", err)
	}
	policy := resolveSessionSleepPolicyInfo(holderInfo, cfg, sp)
	if !policy.enabled() || policy.Class == config.SessionSleepNonInteractive {
		t.Fatalf("precondition: holder policy must be an enabled interactive class, got enabled=%v class=%q", policy.enabled(), policy.Class)
	}
	latch := map[string]string{
		"sleep_reason":             string(sessionpkg.SleepReasonIdle),
		"sleep_policy_fingerprint": policy.Fingerprint,
		"slept_at":                 clk.Now().Add(-22 * time.Hour).UTC().Format(time.RFC3339),
	}
	for k, v := range holderMeta {
		latch[k] = v
	}
	if err := store.SetMetadataBatch(holder.ID, latch); err != nil {
		t.Fatalf("SetMetadataBatch(latch): %v", err)
	}
	latchedInfo, err := sessionFrontDoor(store).Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(latched holder): %v", err)
	}
	if !configWakeSuppressedInfo(latchedInfo, policy, sp, clk) {
		t.Fatal("precondition: the holder must be idle-latched (configWakeSuppressedInfo = true)")
	}

	var stdout, stderr bytes.Buffer
	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)
	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, &stderr, true)
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessionInfosFromBeads(sessions), dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

	snap := newSessionBeadSnapshot(sessions)
	woken := reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath, snap.OpenForReconcile(), snap, dsResult.State, cfgNames, cfg, sp,
		store, nil, dsResult.AssignedWorkBeads, nil, nil, newDrainTracker(), nil, poolDesired,
		dsResult.NamedSessionDemand, dsResult.NamedSessionRoutedDemand, dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	var starts []string
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Start" {
			starts = append(starts, call.Name)
		}
	}
	postSessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads (post-reconcile): %v", err)
	}
	gotHolder, err := store.Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(holder post-reconcile): %v", err)
	}
	return idleLatchedNamedHolderResult{
		woken:        woken,
		running:      sp.IsRunning(sessionName),
		starts:       starts,
		postSessions: postSessions,
		routed:       dsResult.NamedSessionRoutedDemand,
		holder:       gotHolder,
	}
}

func assertIdleLatchedHolderWoke(t *testing.T, res idleLatchedNamedHolderResult) {
	t.Helper()
	if res.woken != 1 || !res.running {
		t.Fatalf("idle-latched interactive named holder did not wake (woken=%d running=%v starts=%v config_wake_suppressed=%q): "+
			"the asleep holder owns the canonical alias, so the pool standby is suppressed and nothing else can serve the work",
			res.woken, res.running, res.starts, res.holder.Metadata["config_wake_suppressed"])
	}
	if len(res.starts) != 1 {
		t.Fatalf("starts = %v, want exactly the named holder (no standby)", res.starts)
	}
	if len(res.postSessions) != 1 {
		t.Fatalf("session beads after reconcile = %d, want 1: the holder is reused, never replaced by a standby", len(res.postSessions))
	}
	if res.holder.Metadata["config_wake_suppressed"] == "true" {
		t.Fatal("config_wake_suppressed persisted true on a holder the reconciler just woke")
	}
}

func TestReconcileSessionBeads_IdleLatchedInteractiveNamedHolderWakesOnRoutedDemand(t *testing.T) {
	res := reconcileIdleLatchedInteractiveNamedHolder(t, true, nil)
	if !res.routed["olivia"] {
		t.Fatalf("precondition: NamedSessionRoutedDemand[olivia] = false, want true")
	}
	assertIdleLatchedHolderWoke(t, res)
}

func TestReconcileSessionBeads_IdleLatchedInteractiveNamedHolderHonorsExplicitWake(t *testing.T) {
	res := reconcileIdleLatchedInteractiveNamedHolder(t, false, map[string]string{
		"wake_request":      string(sessionpkg.WakeCauseExplicit),
		"wake_requested_at": "2026-09-22T21:52:00Z",
	})
	assertIdleLatchedHolderWoke(t, res)
}

// The idle latch itself stays intact: with no routed work and no explicit wake,
// the interactive holder remains asleep.
func TestReconcileSessionBeads_IdleLatchedInteractiveNamedHolderStaysAsleepWithoutDemand(t *testing.T) {
	res := reconcileIdleLatchedInteractiveNamedHolder(t, false, nil)
	if res.woken != 0 || res.running || len(res.starts) != 0 {
		t.Fatalf("idle-latched holder woke with no demand: woken=%d running=%v starts=%v", res.woken, res.running, res.starts)
	}
}
