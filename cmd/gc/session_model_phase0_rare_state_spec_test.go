package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type capabilityOverrideProvider struct {
	runtime.Provider
	caps     runtime.ProviderCapabilities
	sleepCap runtime.SessionSleepCapability
}

func (p capabilityOverrideProvider) Capabilities() runtime.ProviderCapabilities {
	return p.caps
}

func (p capabilityOverrideProvider) SleepCapability(string) runtime.SessionSleepCapability {
	return p.sleepCap
}

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Resume vs fresh rematerialization
// - Reconciler Contract / duplicate canonical repair
// - Config Drift and Restart
// - Close/Wake Race Semantics
// - Status and Diagnostics / degraded health rule

func TestPhase0ConfigDrift_ActiveNamedSessionDefersWhenAttached(t *testing.T) {
	// When a named session is attached (actively in use), config-drift
	// should be deferred -- not immediately restarted. See #119.
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "new-cmd",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "new-cmd",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}

	oldRuntime := runtime.Config{Command: "old-cmd"}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	env.sp.SetAttached(sessionName, true)

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                "old-provider-conversation",
		"started_config_hash":        runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})

	env.reconcile([]beads.Bead{session})

	// Session should still be running -- config-drift deferred while attached.
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("attached named session was stopped during config-drift; want deferred")
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	// State should NOT be "creating" — config-drift was deferred.
	if got.Metadata["state"] == "creating" {
		t.Fatal("state = creating; config-drift should have been deferred for attached session")
	}
	// started_config_hash should NOT have been cleared.
	if got.Metadata["started_config_hash"] == "" {
		t.Fatal("started_config_hash was cleared during deferred config-drift; want preserved")
	}
}

func TestPhase0ConfigDrift_IdleNamedSessionRestartsInPlaceWithoutCapVacancy(t *testing.T) {
	// When a named session is idle (detached, no recent activity),
	// config-drift should proceed with restart-in-place.
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "new-cmd",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "new-cmd",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}

	oldRuntime := runtime.Config{Command: "old-cmd"}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	// NOT attached, no recent activity -- session is idle.

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                "old-provider-conversation",
		"started_config_hash":        runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})

	env.reconcile([]beads.Bead{session})

	all, err := env.store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want same single bead during restart; beads=%v", len(all), all)
	}
	if all[0].ID != session.ID {
		t.Fatalf("restart used bead %q, want in-place restart on %q", all[0].ID, session.ID)
	}
	if all[0].Status != "open" {
		t.Fatalf("status = %q, want open while live restart is in progress", all[0].Status)
	}
	if got := all[0].Metadata["state"]; got != "active" {
		t.Fatalf("state = %q, want active after same-tick config-drift restart", got)
	}
	if got := all[0].Metadata["started_config_hash"]; got == "" || got == runtime.CoreFingerprint(oldRuntime) {
		t.Fatalf("started_config_hash = %q, want non-empty fresh config hash", got)
	}
	if got := all[0].Metadata["continuation_reset_pending"]; got != "" {
		t.Fatalf("continuation_reset_pending = %q, want cleared after same-tick wake", got)
	}
}

func TestPhase0ConfigDrift_NamedSessionBoundsRecentActivityDeferral(t *testing.T) {
	// Recent activity is a headless-use signal, but it must not let a live
	// process loop hide one fixed config-drift episode forever.
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "new-cmd",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "new-cmd",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}

	oldRuntime := runtime.Config{Command: "old-cmd"}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	// NOT attached, but has recent activity (30 seconds ago).
	env.sp.SetActivity(sessionName, env.clk.Now().Add(-30*time.Second))

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"started_config_hash":        runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})

	env.reconcile([]beads.Bead{session})

	// Session should still be running -- deferred due to recent activity.
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("named session with recent activity was stopped during config-drift; want deferred")
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] == "creating" {
		t.Fatal("state = creating; config-drift should have been deferred for session with recent activity")
	}
	if got.Metadata[namedSessionConfigDriftDeferredAtMetadata] == "" {
		t.Fatal("recent-activity config-drift deferral timestamp was not recorded")
	}

	env.clk.Time = env.clk.Now().Add(namedSessionRecentActivityConfigDriftDeferralLimit + time.Second)
	env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Second))
	env.reconcile([]beads.Bead{got})

	got, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after deferral limit: %v", session.ID, err)
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active after bounded recent-activity restart", got.Metadata["state"])
	}
	if got.Metadata[namedSessionConfigDriftDeferredAtMetadata] != "" {
		t.Fatalf("deferred timestamp = %q, want cleared after restart", got.Metadata[namedSessionConfigDriftDeferredAtMetadata])
	}
}

func TestPhase0ConfigDrift_NamedSessionDrainsWhenStaleActivity(t *testing.T) {
	// When a named session has stale activity (beyond threshold) and
	// is not attached, config-drift should proceed.
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "new-cmd",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "new-cmd",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}

	oldRuntime := runtime.Config{Command: "old-cmd"}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	// NOT attached, activity is 5 minutes old (beyond 2-minute threshold).
	env.sp.SetActivity(sessionName, env.clk.Now().Add(-5*time.Minute))

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"started_config_hash":        runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})

	env.reconcile([]beads.Bead{session})

	// Session should have been restarted in-place.
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active after stale-activity config-drift restart", got.Metadata["state"])
	}
}

func TestNamedSessionActivelyInUse(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	name := "test-session"
	_ = sp.Start(context.Background(), name, runtime.Config{Command: "test"})

	session := beads.Bead{
		Metadata: map[string]string{
			"session_name": name,
		},
	}

	// No attachment, no activity -> not active.
	if namedSessionActivelyInUse(session, sp, name, clk) {
		t.Error("expected not active with no attachment and no activity")
	}

	// Attached -> active.
	sp.SetAttached(name, true)
	if !namedSessionActivelyInUse(session, sp, name, clk) {
		t.Error("expected active when attached")
	}
	sp.SetAttached(name, false)

	// Recent activity -> active.
	sp.SetActivity(name, clk.Now().Add(-30*time.Second))
	if !namedSessionActivelyInUse(session, sp, name, clk) {
		t.Error("expected active with recent activity (30s ago)")
	}

	// Stale activity -> not active.
	sp.SetActivity(name, clk.Now().Add(-5*time.Minute))
	if namedSessionActivelyInUse(session, sp, name, clk) {
		t.Error("expected not active with stale activity (5m ago)")
	}

	// Unknown provider activity is conservative: an alive named session is
	// treated as active because config-drift cannot prove it is idle.
	unknownActivity := capabilityOverrideProvider{Provider: sp}
	if !namedSessionActivelyInUse(session, unknownActivity, name, clk) {
		t.Error("expected active when provider cannot report activity")
	}

	// Routed providers can have conservative global capabilities while the
	// specific session backend still reports enough idle data. Honor the
	// routed sleep capability rather than the global capability intersection.
	routedActivity := capabilityOverrideProvider{
		Provider: sp,
		sleepCap: runtime.SessionSleepCapabilityFull,
	}
	sp.SetActivity(name, clk.Now().Add(-5*time.Minute))
	if namedSessionActivelyInUse(session, routedActivity, name, clk) {
		t.Error("expected not active when routed backend reports stale activity")
	}

	timedOnlyUnknownActivity := capabilityOverrideProvider{
		Provider: sp,
		sleepCap: runtime.SessionSleepCapabilityTimedOnly,
	}
	if !namedSessionActivelyInUse(session, timedOnlyUnknownActivity, name, clk) {
		t.Error("expected active when timed-only backend cannot report activity")
	}

	// Nil provider -> not active.
	if namedSessionActivelyInUse(session, nil, name, clk) {
		t.Error("expected not active with nil provider")
	}

	// Empty name -> not active.
	if namedSessionActivelyInUse(session, sp, "", clk) {
		t.Error("expected not active with empty name")
	}
}

func TestShouldDeferNamedSessionConfigDriftBoundsUnknownActivity(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	name := "test-session"
	if err := sp.Start(context.Background(), name, runtime.Config{Command: "test"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	provider := capabilityOverrideProvider{Provider: sp}
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title: name,
		Type:  sessionBeadType,
		Metadata: map[string]string{
			"session_name": name,
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	reason, deferDrift, err := shouldDeferNamedSessionConfigDrift(session, store, provider, name, clk, "drift-1")
	if err != nil {
		t.Fatalf("shouldDeferNamedSessionConfigDrift: %v", err)
	}
	if !deferDrift {
		t.Fatal("expected initial unknown-activity config drift to defer")
	}
	if reason != "activity_unknown" {
		t.Fatalf("reason = %q, want activity_unknown", reason)
	}
	session, err = store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got := session.Metadata[namedSessionConfigDriftDeferredAtMetadata]; got == "" {
		t.Fatal("config drift deferred timestamp was not recorded")
	}
	if got := session.Metadata[namedSessionConfigDriftDeferredKeyMetadata]; got != "drift-1" {
		t.Fatalf("config drift deferred key = %q, want drift-1", got)
	}

	clk.Time = clk.Now().Add(namedSessionActivityThreshold + time.Second)
	_, deferDrift, err = shouldDeferNamedSessionConfigDrift(session, store, provider, name, clk, "drift-1")
	if err != nil {
		t.Fatalf("shouldDeferNamedSessionConfigDrift after threshold: %v", err)
	}
	if deferDrift {
		t.Fatal("expected unknown-activity config drift to stop deferring after threshold")
	}

	reason, deferDrift, err = shouldDeferNamedSessionConfigDrift(session, store, provider, name, clk, "drift-2")
	if err != nil {
		t.Fatalf("shouldDeferNamedSessionConfigDrift new drift: %v", err)
	}
	if !deferDrift {
		t.Fatal("expected new config drift episode to get a fresh unknown-activity deferral")
	}
	if reason != "activity_unknown" {
		t.Fatalf("new drift reason = %q, want activity_unknown", reason)
	}

	session, err = store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead after new drift: %v", err)
	}
	if err := clearNamedSessionConfigDriftDeferral(session, store); err != nil {
		t.Fatalf("clearNamedSessionConfigDriftDeferral: %v", err)
	}
	session, err = store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead after clear: %v", err)
	}
	if got := session.Metadata[namedSessionConfigDriftDeferredAtMetadata]; got != "" {
		t.Fatalf("deferred timestamp after clear = %q, want empty", got)
	}
	if got := session.Metadata[namedSessionConfigDriftDeferredKeyMetadata]; got != "" {
		t.Fatalf("deferred key after clear = %q, want empty", got)
	}
}

func TestShouldDeferNamedSessionConfigDriftDoesNotDeferWhenMarkerWriteFails(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	name := "test-session"
	if err := sp.Start(context.Background(), name, runtime.Config{Command: "test"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	provider := capabilityOverrideProvider{Provider: sp}
	session := beads.Bead{
		ID:    "missing",
		Title: name,
		Type:  sessionBeadType,
		Metadata: map[string]string{
			"session_name": name,
		},
	}

	_, deferDrift, err := shouldDeferNamedSessionConfigDrift(session, beads.NewMemStore(), provider, name, clk, "drift-1")
	if err == nil {
		t.Fatal("expected marker write error")
	}
	if deferDrift {
		t.Fatal("expected config drift not to defer when marker write fails")
	}
}

func TestPoolSessionConfigDriftNotAffectedByActiveGuard(t *testing.T) {
	// Pool (non-named) sessions should still defer on config-drift
	// via existing guards -- the new guard only applies to named sessions.
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", StartCommand: "new-cmd"}},
	}
	env.addRunningWorkerDesiredWithNewConfig()

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	startedHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": startedHash,
	})
	// Attach the session -- pool sessions should still defer via existing guard.
	env.sp.SetAttached("worker", true)
	if !env.sp.IsRunning("worker") {
		t.Fatal("pool session test setup did not start the runtime")
	}

	env.reconcile([]beads.Bead{session})

	// Pool session with attachment defers via the existing IsAttached guard,
	// not our new named-session guard. Verify the session is NOT drained
	// (existing behavior for attached pool sessions).
	ds := env.dt.get(session.ID)
	if ds != nil {
		t.Fatalf("attached pool session should defer config-drift via existing guard, got drain: %+v", ds)
	}
}

func TestPhase0ConfigDrift_AsleepNamedSessionRepairsInPlaceWithoutWaking(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "new-cmd",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "on_demand",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	oldRuntime := runtime.Config{Command: "old-cmd"}
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"session_key":                "old-provider-conversation",
		"started_config_hash":        runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
		"pending_create_claim":       "true",
	})

	env.reconcile([]beads.Bead{session})

	all, err := env.store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want same single non-live bead during drift repair; beads=%v", len(all), all)
	}
	got := all[0]
	if got.ID != session.ID {
		t.Fatalf("drift repair used bead %q, want in-place repair on %q", got.ID, session.ID)
	}
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q started during asleep config-drift repair; want no wake", sessionName)
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep after non-live drift repair", got.Metadata["state"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared so next wake uses fresh config", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true for unified restart handoff", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] == "old-provider-conversation" {
		t.Fatalf("session_key still points at old provider conversation after config-drift repair")
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after asleep config-drift repair", got.Metadata["pending_create_claim"])
	}
}

func TestPhase0CanonicalRepair_DuplicateOpenNamedBeadsRetiresLosersNonTerminally(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "true",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}

	older := env.createSessionBead("worker-older", "worker")
	env.setSessionMetadata(&older, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"alias":                      "worker",
		"generation":                 "1",
		"continuity_eligible":        "true",
	})
	newer := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&newer, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"alias":                      "worker",
		"generation":                 "2",
		"continuity_eligible":        "true",
	})
	assignedOpen, err := env.store.Create(beads.Bead{
		Title:    "open work owned by duplicate",
		Type:     "task",
		Assignee: older.ID,
	})
	if err != nil {
		t.Fatalf("Create(assigned open work): %v", err)
	}
	assignedInProgress, err := env.store.Create(beads.Bead{
		Title:    "in-progress work owned by duplicate",
		Type:     "task",
		Assignee: older.ID,
	})
	if err != nil {
		t.Fatalf("Create(assigned in-progress work): %v", err)
	}
	inProgressStatus := "in_progress"
	if err := env.store.Update(assignedInProgress.ID, beads.UpdateOpts{Status: &inProgressStatus}); err != nil {
		t.Fatalf("Update(%s, in_progress): %v", assignedInProgress.ID, err)
	}

	env.reconcile([]beads.Bead{older, newer})

	all, err := env.store.ListByLabel(sessionBeadLabel, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(session, IncludeClosed): %v", err)
	}

	var canonicalIDs []string
	var retiredLoserIDs []string
	for _, b := range all {
		if b.Metadata[namedSessionIdentityMetadata] != "worker" {
			continue
		}
		if b.Status == "closed" {
			t.Fatalf("duplicate canonical repair closed loser %s; want non-terminal retirement", b.ID)
		}
		if b.Status == "open" && !phase0RetiredCanonicalState(b.Metadata["state"]) && b.Metadata["continuity_eligible"] != "false" {
			canonicalIDs = append(canonicalIDs, b.ID)
		}
		if b.Status == "open" && phase0RetiredCanonicalState(b.Metadata["state"]) && b.Metadata["continuity_eligible"] == "false" {
			retiredLoserIDs = append(retiredLoserIDs, b.ID)
		}
	}

	if len(canonicalIDs) != 1 || canonicalIDs[0] != newer.ID {
		t.Fatalf("canonical winners = %v, want exactly newest generation bead %s", canonicalIDs, newer.ID)
	}
	if len(retiredLoserIDs) != 1 || retiredLoserIDs[0] != older.ID {
		t.Fatalf("retired losers = %v, want exactly older generation bead %s retired non-terminally", retiredLoserIDs, older.ID)
	}
	for _, id := range []string{assignedOpen.ID, assignedInProgress.ID} {
		got, err := env.store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Assignee != newer.ID {
			t.Fatalf("work bead %s assignee = %q, want reassigned to canonical winner %s", id, got.Assignee, newer.ID)
		}
	}
}

func TestPhase0StatusText_DegradesAlwaysNamedIdentityBlockedByCitySuspend(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{
			Name:      "test-city",
			Suspended: true,
		},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	out := phase0StatusTextForConfig(t, cfg)
	if !strings.Contains(out, "degraded") || !strings.Contains(out, "worker") || !strings.Contains(out, "blocked") {
		t.Fatalf("status output should mark city-suspended mode=always named identity as degraded:\n%s", out)
	}
}

func TestPhase0StatusText_DegradesAlwaysNamedIdentityBlockedByAgentSuspend(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
			Suspended:    true,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}

	out := phase0StatusTextForConfig(t, cfg)
	if !strings.Contains(out, "degraded") || !strings.Contains(out, "worker") || !strings.Contains(out, "blocked") {
		t.Fatalf("status output should mark agent-suspended mode=always named identity as degraded:\n%s", out)
	}
}

func TestPhase0StatusText_DegradesAlwaysNamedIdentityBlockedByRigSuspend(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name:      "demo",
			Path:      t.TempDir(),
			Suspended: true,
		}},
		Agents: []config.Agent{{
			Name:         "worker",
			Dir:          "demo",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "demo",
			Mode:     "always",
		}},
	}

	out := phase0StatusTextForConfig(t, cfg)
	if !strings.Contains(out, "degraded") || !strings.Contains(out, "demo/worker") || !strings.Contains(out, "blocked") {
		t.Fatalf("status output should mark rig-suspended mode=always named identity as degraded:\n%s", out)
	}
}

func phase0StatusTextForConfig(t *testing.T, cfg *config.City) string {
	t.Helper()
	sp := runtime.NewFake()
	dops := newDrainOps(sp)

	var stdout strings.Builder
	if code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &strings.Builder{}); code != 0 {
		t.Fatalf("doCityStatus() = %d, want 0", code)
	}
	return strings.ToLower(stdout.String())
}

func phase0RetiredCanonicalState(state string) bool {
	switch strings.TrimSpace(state) {
	case "drained", "archived", "orphaned", "suspended":
		return true
	default:
		return false
	}
}
