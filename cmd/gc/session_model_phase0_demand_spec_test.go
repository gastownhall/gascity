package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - scale_check as the only controller-side generic demand signal
// - named-session wake semantics
// - assigned-work continuity driven by assignee, not gc.routed_to

func TestPhase0NamedOnDemand_DoesNotWakeFromTemplateWorkSet(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		WorkSet:       map[string]bool{"hello-world/refinery": true},
		Now:           now,
	})

	assertAsleep(t, result, "hello-world--refinery")
}

func TestPhase0NamedOnDemand_WakesFromAssignedBeadID(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		WorkBeads:     []AwakeWorkBead{{ID: "hw-1", Assignee: "mc-1", Status: "open"}},
		Now:           now,
	})

	assertAwake(t, result, "hello-world--refinery")
}

func TestPhase0NamedOnDemand_DoesNotWakeFromTemplateAssigneeToken(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		WorkBeads:     []AwakeWorkBead{{ID: "hw-1", Assignee: "hello-world/refinery", Status: "open"}},
		Now:           now,
	})

	assertAsleep(t, result, "hello-world--refinery")
}

func TestPhase0PoolDesiredStates_AssigneeOnlyKeepsConcreteSessionAlive(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)},
	}
	work := []beads.Bead{{
		ID:       "w1",
		Status:   "open",
		Assignee: "sess-1",
	}}
	sessions := []beads.Bead{sessionBead("sess-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1 resume request", len(result[0].Requests))
	}
	if got := result[0].Requests[0].Tier; got != "resume" {
		t.Fatalf("tier = %q, want resume", got)
	}
	if got := result[0].Requests[0].SessionBeadID; got != "sess-1" {
		t.Fatalf("session_bead_id = %q, want sess-1", got)
	}
}
