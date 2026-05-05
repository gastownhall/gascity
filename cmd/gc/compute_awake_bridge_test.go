package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildAwakeInputFromReconcilerUsesLifecycleProjectionForCompatibilityStates(t *testing.T) {
	now := time.Now().UTC()
	input := buildAwakeInputFromReconciler(
		&config.City{},
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
