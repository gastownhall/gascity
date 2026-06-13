package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestBuildAwakeInputFromReconcilerUsesLifecycleProjectionForCompatibilityStates(t *testing.T) {
	now := time.Now().UTC()
	input := buildAwakeInputFromReconciler(
		&config.City{},
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":        "stopped",
				"session_name": "s-worker",
				"template":     "worker",
			},
		}},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if got := input.SessionBeads[0].State; got != "asleep" {
		t.Fatalf("State = %q, want asleep-compatible projection for stopped", got)
	}
}

// TestBuildAwakeInputFromReconcilerCanonicalizesLegacyBoundTemplate pins the
// bridge-side identity normalization for adopted legacy-bound session beads.
// A bead persisted under a removed binding ("gascity-packs/gc.implementation-worker")
// must enter the awake engine keyed by the current unbound agent's canonical
// template, so explicit wake, suspension gates, and scale/min-active
// accounting all see the adopted session. Without normalization the raw
// stored template misses every agentsByName lookup and the explicit wake
// request lingers unhonored.
func TestBuildAwakeInputFromReconcilerCanonicalizesLegacyBoundTemplate(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "implementation-worker", Dir: "gascity-packs"}},
	}
	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":        "stopped",
				"session_name": "gc__implementation-worker-mc-1",
				"template":     "gascity-packs/gc.implementation-worker",
				"wake_request": "explicit",
			},
		}},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if got := input.SessionBeads[0].Template; got != "gascity-packs/implementation-worker" {
		t.Fatalf("Template = %q, want canonical current template", got)
	}
	if !input.SessionBeads[0].ExplicitWake {
		t.Fatal("ExplicitWake = false, want true for wake_request=explicit")
	}

	decisions := ComputeAwakeSet(input)
	got := decisions["gc__implementation-worker-mc-1"]
	if !got.ShouldWake || got.Reason != "explicit-wake" {
		t.Fatalf("decision = %+v, want explicit-wake for adopted legacy-bound bead", got)
	}
}

// TestBuildAwakeInputFromReconcilerKeepsUnresolvableTemplateRaw guards the
// conservative half of the bridge normalization: a stored template that does
// not resolve to any configured agent must pass through unchanged rather
// than being rewritten or dropped.
func TestBuildAwakeInputFromReconcilerKeepsUnresolvableTemplateRaw(t *testing.T) {
	now := time.Now().UTC()
	input := buildAwakeInputFromReconciler(
		&config.City{Agents: []config.Agent{{Name: "other", Dir: "rig"}}},
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":        "stopped",
				"session_name": "s-orphan",
				"template":     "removed-rig/gone-worker",
			},
		}},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if got := input.SessionBeads[0].Template; got != "removed-rig/gone-worker" {
		t.Fatalf("Template = %q, want raw stored template preserved", got)
	}
}

func TestBuildAwakeInputFromReconcilerCarriesResetPendingMetadata(t *testing.T) {
	now := time.Now().UTC()
	input := buildAwakeInputFromReconciler(
		&config.City{},
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":                      "stopped",
				"session_name":               "s-reset-target",
				"template":                   "build-agent",
				"restart_requested":          "true",
				"continuation_reset_pending": "true",
				session.ResetCommittedAtKey:  now.Format(time.RFC3339),
			},
		}},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	got := input.SessionBeads[0]
	if !got.RestartRequested {
		t.Fatalf("RestartRequested = false, want true")
	}
	if !got.ContinuationResetPending {
		t.Fatalf("ContinuationResetPending = false, want true")
	}
}

func TestBuildAwakeInputFromReconcilerPopulatesPendingInteractions(t *testing.T) {
	now := time.Now().UTC()
	sp := runtime.NewFake()
	sp.SetPendingInteraction("s-worker", &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "question",
		Prompt:    "approve?",
	})
	session := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "s-worker",
			"template":     "worker",
		},
	}

	input := buildAwakeInputFromReconciler(
		&config.City{Agents: []config.Agent{{Name: "worker"}}},
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{session},
		nil,
		nil,
		nil,
		nil,
		nil,
		[]wakeTarget{{session: &session, alive: true}},
		sp,
		now,
	)

	if !input.PendingSessions["s-worker"] {
		t.Fatalf("PendingSessions[s-worker] = false, want true")
	}
	decisions := ComputeAwakeSet(input)
	got := decisions["s-worker"]
	if !got.ShouldWake || got.Reason != "pending" {
		t.Fatalf("decision = %+v, want pending wake", got)
	}
}

func TestAwakeSetToWakeEvalsPreservesDecisionReason(t *testing.T) {
	evals := awakeSetToWakeEvals(
		map[string]AwakeDecision{
			"s-worker": {ShouldWake: true, Reason: "assigned-work"},
		},
		[]AwakeSessionBead{{
			ID:          "mc-session-1",
			SessionName: "s-worker",
		}},
	)

	got := evals["mc-session-1"]
	if got.Reason != "assigned-work" {
		t.Fatalf("Reason = %q, want assigned-work", got.Reason)
	}
	if !containsWakeReason(got.Reasons, WakeWork) {
		t.Fatalf("Reasons = %v, want WakeWork", got.Reasons)
	}
}

func TestAwakeSetToWakeEvalsMapsMinActiveToWakeConfig(t *testing.T) {
	evals := awakeSetToWakeEvals(
		map[string]AwakeDecision{
			"s-worker": {ShouldWake: true, Reason: "min-active"},
		},
		[]AwakeSessionBead{{
			ID:          "mc-session-1",
			SessionName: "s-worker",
		}},
	)

	got := evals["mc-session-1"]
	if got.Reason != "min-active" {
		t.Fatalf("Reason = %q, want min-active", got.Reason)
	}
	if !containsWakeReason(got.Reasons, WakeConfig) {
		t.Fatalf("Reasons = %v, want WakeConfig", got.Reasons)
	}
}

func TestBuildAwakeInputFromReconcilerCarriesNamedSessionDemand(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{
			{Name: "primary", Template: "worker", Mode: "on_demand"},
		},
	}
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":                     "asleep",
			"session_name":              "primary",
			"template":                  "worker",
			"configured_named_identity": "primary",
			"configured_named_mode":     "on_demand",
		},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{sessionBead},
		map[string]int{"worker": 1},
		map[string]bool{"primary": true},
		nil,
		nil,
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	if !input.NamedSessionDemand["primary"] {
		t.Fatalf("NamedSessionDemand[primary] = false, want true")
	}
	decisions := ComputeAwakeSet(input)
	got := decisions["primary"]
	if !got.ShouldWake || got.Reason != "named-demand" {
		t.Fatalf("decision = %+v, want named-demand wake", got)
	}
}

func TestBuildAwakeInputFromReconciler_RigNamedWorkQueryDemandWakesCanonicalSession(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-test",
		Agents: []config.Agent{
			{Name: "worker", Scope: "rig", WorkQuery: "echo 1"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "refinery", Template: "worker", Mode: "on_demand", Scope: "rig", Dir: "rig-a"},
		},
	}
	identity := "rig-a/refinery"
	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"configured_named_session":  "true",
			"state":                     "asleep",
			"session_name":              runtimeName,
			"template":                  "rig-a/worker",
			"configured_named_identity": identity,
			"configured_named_mode":     "on_demand",
		},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{sessionBead},
		nil,
		nil,
		map[string]bool{"rig-a/worker": true},
		nil,
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	decisions := ComputeAwakeSet(input)
	got, ok := decisions[runtimeName]
	if !ok {
		t.Fatal("decision for rig named session missing from awake set")
	}
	if !got.ShouldWake {
		t.Fatalf("decision = %+v, want wake", got)
	}
	if got.Reason != "work-query" {
		t.Fatalf("Reason = %q, want work-query", got.Reason)
	}
}

// TestBuildAwakeInputFromReconcilerNamedAlwaysPostChurnRewakes pins the
// contract for a mode=always named session that was put to sleep after churn:
// if named-session metadata survives, the next awake-set pass must re-wake it.
func TestBuildAwakeInputFromReconcilerNamedAlwaysPostChurnRewakes(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{
			{Name: "worker", Template: "worker", Mode: "always"},
		},
	}
	postChurnBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":                      "asleep",
			"sleep_reason":               "",
			"state_reason":               "creation_complete",
			"last_woke_at":               "",
			"wake_attempts":              "0",
			"churn_count":                "1",
			"session_key":                "",
			"continuation_reset_pending": "",
			"pending_create_claim":       "",
			"pin_awake":                  "",
			"session_name":               "worker",
			"template":                   "worker",
			"configured_named_identity":  "worker",
			"configured_named_mode":      "always",
		},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{postChurnBead},
		nil, nil, nil, nil, nil, nil,
		runtime.NewFake(),
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	bead := input.SessionBeads[0]
	if bead.NamedIdentity != "worker" {
		t.Errorf("projected NamedIdentity = %q, want worker (configured_named_identity should survive churn)", bead.NamedIdentity)
	}
	if bead.State != "asleep" {
		t.Errorf("projected State = %q, want asleep", bead.State)
	}

	decisions := ComputeAwakeSet(input)
	got, ok := decisions["worker"]
	if !ok {
		t.Fatal("decision for 'worker' missing from awake set")
	}
	if !got.ShouldWake {
		t.Fatalf("post-churn named-always session should wake; got decision = %+v", got)
	}
	if got.Reason != "named-always" {
		t.Errorf("wake reason = %q, want named-always", got.Reason)
	}
}
