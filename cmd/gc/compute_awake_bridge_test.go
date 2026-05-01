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

// TestBuildAwakeInputFromReconciler_NamedAlwaysPostChurnRewakes pins issue
// #1493: a mode=always named session that hit checkChurn below the
// quarantine threshold must be re-woken on the next reconciler tick.
//
// The metadata shape below is the exact post-churn snapshot reported on the
// issue: state=asleep, sleep_reason="" (recordChurn does not set
// sleep_reason below defaultMaxChurnCycles), state_reason="creation_complete"
// (carried over from the prior wake, untouched by checkChurn/recordChurn),
// last_woke_at="" (cleared by checkChurn:644 to make the trigger
// edge-triggered), wake_attempts=0, churn_count=1.
//
// The test drives the full bead → AwakeInput → ComputeAwakeSet path so any
// regression in the lifecycle projection, the bridge, or ComputeAwakeSet
// fails it. Without the fix, the session sits asleep indefinitely despite
// mode=always and only `gc session pin` unsticks it.
func TestBuildAwakeInputFromReconciler_NamedAlwaysPostChurnRewakes(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "refinery"}},
		NamedSessions: []config.NamedSession{
			{Name: "refinery", Template: "refinery", Mode: "always"},
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
			"session_name":               "refinery",
			"template":                   "refinery",
			"configured_named_identity":  "refinery",
			"configured_named_mode":      "always",
		},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		[]beads.Bead{postChurnBead},
		nil, nil, nil, nil, nil,
		runtime.NewFake(),
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	bead := input.SessionBeads[0]
	if bead.NamedIdentity != "refinery" {
		t.Errorf("projected NamedIdentity = %q, want refinery (configured_named_identity should survive churn)", bead.NamedIdentity)
	}
	if bead.State != "asleep" {
		t.Errorf("projected State = %q, want asleep", bead.State)
	}

	decisions := ComputeAwakeSet(input)
	got, ok := decisions["refinery"]
	if !ok {
		t.Fatal("decision for 'refinery' missing from awake set")
	}
	if !got.ShouldWake {
		t.Fatalf("post-churn named-always session should wake; got decision = %+v", got)
	}
	if got.Reason != "named-always" {
		t.Errorf("wake reason = %q, want named-always", got.Reason)
	}
}
