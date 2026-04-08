package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

var now = time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)

func assertAwake(t *testing.T, result map[string]AwakeDecision, sessionName string) {
	t.Helper()
	d, ok := result[sessionName]
	if !ok {
		t.Errorf("session %q not in result", sessionName)
		return
	}
	if !d.ShouldWake {
		t.Errorf("session %q should be awake but isn't (reason: %s)", sessionName, d.Reason)
	}
}

func assertAsleep(t *testing.T, result map[string]AwakeDecision, sessionName string) {
	t.Helper()
	d, ok := result[sessionName]
	if !ok {
		return // not in result = not awake = correct
	}
	if d.ShouldWake {
		t.Errorf("session %q should be asleep but is awake (reason: %s)", sessionName, d.Reason)
	}
}

func assertReason(t *testing.T, result map[string]AwakeDecision, sessionName, wantReason string) {
	t.Helper()
	d, ok := result[sessionName]
	if !ok {
		t.Errorf("session %q not in result", sessionName)
		return
	}
	if d.Reason != wantReason {
		t.Errorf("session %q reason = %q, want %q", sessionName, d.Reason, wantReason)
	}
}

// ---------------------------------------------------------------------------
// Named session (always)
// ---------------------------------------------------------------------------

func TestNamedAlways_AsleepWakes(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep", NamedIdentity: "deacon"}},
		Now:           now,
	})
	assertAwake(t, result, "deacon")
}

func TestNamedAlways_ActiveStaysAwake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:          []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions:   []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:    []AwakeSessionBead{{ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "active", NamedIdentity: "deacon"}},
		RunningSessions: map[string]bool{"deacon": true},
		Now:             now,
	})
	assertAwake(t, result, "deacon")
}

func TestNamedAlways_NoBead(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{},
		Now:           now,
	})
	if len(result) != 0 {
		t.Errorf("expected empty result (no beads), got %d", len(result))
	}
}

func TestNamedAlways_Quarantined(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep",
			NamedIdentity: "deacon", QuarantinedUntil: now.Add(5 * time.Minute),
		}},
		Now: now,
	})
	assertAsleep(t, result, "deacon")
}

func TestNamedAlways_TemplateRemoved(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{},
		NamedSessions: []AwakeNamedSession{},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep", NamedIdentity: "deacon"}},
		Now:           now,
	})
	assertAsleep(t, result, "deacon")
}

func TestNamedAlways_AgentSuspended(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon", Suspended: true}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep", NamedIdentity: "deacon"}},
		Now:           now,
	})
	assertAsleep(t, result, "deacon")
}

func TestNamedAlways_AgentSuspended_NoBead(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon", Suspended: true}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{},
		Now:           now,
	})
	if len(result) != 0 {
		t.Errorf("expected empty result for suspended agent with no bead, got %d", len(result))
	}
}

func TestNamedAlways_AgentNotSuspended(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon", Suspended: false}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep", NamedIdentity: "deacon"}},
		Now:           now,
	})
	assertAwake(t, result, "deacon")
	assertReason(t, result, "deacon", "named-always")
}

// ---------------------------------------------------------------------------
// Named session (on_demand)
// ---------------------------------------------------------------------------

func TestNamedOnDemand_NoWork(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		Now:           now,
	})
	assertAsleep(t, result, "hello-world--refinery")
}

func TestNamedOnDemand_AssigneeMatches(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		WorkBeads:     []AwakeWorkBead{{ID: "hw-1", Assignee: "hello-world/refinery", Status: "open"}},
		Now:           now,
	})
	assertAwake(t, result, "hello-world--refinery")
}

func TestNamedOnDemand_PendingCreateWakesWithoutDemand(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{{
			ID:            "mc-1",
			SessionName:   "hello-world--refinery",
			Template:      "hello-world/refinery",
			State:         "stopped",
			PendingCreate: true,
			NamedIdentity: "hello-world/refinery",
		}},
		Now: now,
	})
	assertAwake(t, result, "hello-world--refinery")
	assertReason(t, result, "hello-world--refinery", "pending-create")
}

func TestNamedOnDemand_WorkDone_StaysAwakeUntilIdle(t *testing.T) {
	// On-demand session with work done: still running, no demand.
	// Stays awake via on-demand:running override — drains only after
	// idle timeout (default 5 min).
	result := ComputeAwakeSet(AwakeInput{
		Agents:          []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions:   []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:    []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "active", NamedIdentity: "hello-world/refinery"}},
		RunningSessions: map[string]bool{"hello-world--refinery": true},
		Now:             now,
	})
	assertAwake(t, result, "hello-world--refinery")
	if d := result["hello-world--refinery"]; d.Reason != "on-demand:running" {
		t.Errorf("reason = %q, want %q", d.Reason, "on-demand:running")
	}
}

func TestNamedOnDemand_WorkDone_DrainsAfterDefaultIdle(t *testing.T) {
	// Same scenario but idle for 6 min. Default 5 min timeout drains it.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery",
			State: "active", NamedIdentity: "hello-world/refinery", IdleSince: now.Add(-6 * time.Minute),
		}},
		RunningSessions: map[string]bool{"hello-world--refinery": true},
		Now:             now,
	})
	assertAsleep(t, result, "hello-world--refinery")
}

func TestNamedOnDemand_Attached_StaysAwake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:           []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions:    []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:     []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "active", NamedIdentity: "hello-world/refinery"}},
		RunningSessions:  map[string]bool{"hello-world--refinery": true},
		AttachedSessions: map[string]bool{"hello-world--refinery": true},
		Now:              now,
	})
	assertAwake(t, result, "hello-world--refinery")
}

func TestNamedOnDemand_ScaleCheckIrrelevant(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:           []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions:    []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:     []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		ScaleCheckCounts: map[string]int{"hello-world/refinery": 1},
		Now:              now,
	})
	assertAsleep(t, result, "hello-world--refinery")
}

func TestNamedOnDemand_AgentSuspended_WithWork(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery", Suspended: true}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		WorkBeads:     []AwakeWorkBead{{ID: "hw-1", Assignee: "hello-world/refinery", Status: "open"}},
		Now:           now,
	})
	assertAsleep(t, result, "hello-world--refinery")
}

// ---------------------------------------------------------------------------
// Agent template (scaled)
// ---------------------------------------------------------------------------

func TestScaled_NoDemand_NoBeads(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:           []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		Now:              now,
	})
	if len(result) != 0 {
		t.Errorf("expected no decisions, got %d", len(result))
	}
}

func TestScaled_Demand1_NoBeads(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:           []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	if len(result) != 0 {
		t.Errorf("expected no decisions (no beads yet), got %d", len(result))
	}
}

func TestScaled_Demand2_OneActive(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
			{ID: "mc-2", SessionName: "polecat-mc-2", Template: "hello-world/polecat", State: "asleep"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 2},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-1")
	assertAsleep(t, result, "polecat-mc-2") // asleep ephemerals not reused
}

func TestScaled_Demand1_TwoActive(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
			{ID: "mc-2", SessionName: "polecat-mc-2", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		RunningSessions:  map[string]bool{"polecat-mc-1": true, "polecat-mc-2": true},
		Now:              now,
	})
	awake := 0
	for _, d := range result {
		if d.ShouldWake {
			awake++
		}
	}
	if awake != 1 {
		t.Errorf("expected 1 awake (capped), got %d", awake)
	}
}

func TestScaled_Demand0_OneActive(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestScaled_CreatingBead(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "creating"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-1")
}

func TestScaled_AsleepEphemeral_NotReused(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-old", SessionName: "polecat-mc-old", Template: "hello-world/polecat", State: "asleep"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-old")
}

func TestScaled_MultipleCapped(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
			{ID: "mc-2", SessionName: "polecat-mc-2", Template: "hello-world/polecat", State: "active"},
			{ID: "mc-3", SessionName: "polecat-mc-3", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 2},
		RunningSessions:  map[string]bool{"polecat-mc-1": true, "polecat-mc-2": true, "polecat-mc-3": true},
		Now:              now,
	})
	awake := 0
	for _, d := range result {
		if d.ShouldWake {
			awake++
		}
	}
	if awake != 2 {
		t.Errorf("expected 2 awake (capped by scaleCheck), got %d", awake)
	}
}

// ---------------------------------------------------------------------------
// Manual session
// ---------------------------------------------------------------------------

func TestManual_ImplicitAgent(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "creating", ManualSession: true}},
		Now:          now,
	})
	assertAwake(t, result, "s-mc-1")
}

func TestManual_ExplicitAgent(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "s-mc-1", Template: "hello-world/polecat", State: "creating", ManualSession: true}},
		Now:          now,
	})
	assertAwake(t, result, "s-mc-1")
}

func TestManual_NoDemand_StaysAwake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:           []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads:     []AwakeSessionBead{{ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "active", ManualSession: true}},
		ScaleCheckCounts: map[string]int{"gascity/claude": 0},
		RunningSessions:  map[string]bool{"s-mc-1": true},
		Now:              now,
	})
	assertAwake(t, result, "s-mc-1")
}

func TestManual_Closed(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "closed", ManualSession: true}},
		Now:          now,
	})
	assertAsleep(t, result, "s-mc-1")
}

func TestManual_PendingInteraction(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:          []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads:    []AwakeSessionBead{{ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "active", ManualSession: true}},
		RunningSessions: map[string]bool{"s-mc-1": true},
		PendingSessions: map[string]bool{"s-mc-1": true},
		Now:             now,
	})
	assertAwake(t, result, "s-mc-1")
}

// ---------------------------------------------------------------------------
// Drained beads
// ---------------------------------------------------------------------------

func TestDrained_NotWokenByDemand(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "asleep", Drained: true},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestDrained_WokenByAttach(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "asleep", Drained: true},
		},
		AttachedSessions: map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-1")
}

func TestDrained_WokenByPending(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "asleep", Drained: true},
		},
		PendingSessions: map[string]bool{"polecat-mc-1": true},
		Now:             now,
	})
	assertAwake(t, result, "polecat-mc-1")
}

func TestDrained_ManualNotWoken(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "asleep", ManualSession: true, Drained: true}},
		Now:          now,
	})
	assertAsleep(t, result, "s-mc-1")
}

// ---------------------------------------------------------------------------
// Hold
// ---------------------------------------------------------------------------

func TestHeld_SuppressesEverything(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "active",
			NamedIdentity: "deacon", HeldUntil: now.Add(10 * time.Minute),
		}},
		RunningSessions:  map[string]bool{"deacon": true},
		AttachedSessions: map[string]bool{"deacon": true},
		Now:              now,
	})
	assertAsleep(t, result, "deacon")
	assertReason(t, result, "deacon", "held")
}

func TestHeld_Expired_Wakes(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep",
			NamedIdentity: "deacon", HeldUntil: now.Add(-1 * time.Minute),
		}},
		Now: now,
	})
	assertAwake(t, result, "deacon")
}

// ---------------------------------------------------------------------------
// Wait hold + ready wait
// ---------------------------------------------------------------------------

func TestWaitHold_SuppressesAttachAndPending(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "asleep",
			ManualSession: true, WaitHold: true,
		}},
		AttachedSessions: map[string]bool{"s-mc-1": true},
		PendingSessions:  map[string]bool{"s-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "s-mc-1")
}

func TestWaitHold_SuppressesNamedAlwaysDemand(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep",
			NamedIdentity: "deacon", WaitHold: true,
		}},
		Now: now,
	})
	assertAsleep(t, result, "deacon")
}

func TestWaitHold_SuppressesAssignedWorkDemand(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-p1", SessionName: "polecat-mc-p1", Template: "hello-world/polecat", State: "asleep",
			WaitHold: true,
		}},
		WorkBeads: []AwakeWorkBead{{
			ID: "hw-1", Assignee: "mc-p1", Status: "in_progress",
		}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-p1")
}

func TestWaitHold_PendingCreateStillWakes(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "creating",
			PendingCreate: true, WaitHold: true,
		}},
		Now: now,
	})
	assertAwake(t, result, "polecat-mc-1")
	assertReason(t, result, "polecat-mc-1", "pending-create")
}

func TestWaitHold_PendingCreateStillWakesWithManualDemand(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "creating",
			PendingCreate: true, ManualSession: true, WaitHold: true,
		}},
		Now: now,
	})
	assertAwake(t, result, "s-mc-1")
	assertReason(t, result, "s-mc-1", "manual")
}

func TestReadyWait_Wakes(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "asleep",
			WaitHold: true,
		}},
		ReadyWaitSet: map[string]bool{"mc-1": true},
		Now:          now,
	})
	assertAwake(t, result, "s-mc-1")
	assertReason(t, result, "s-mc-1", "wait-ready")
}

func TestReadyWait_NotReady_StaysAsleep(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{{
			ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "asleep",
			WaitHold: true,
		}},
		ReadyWaitSet: map[string]bool{}, // not ready
		Now:          now,
	})
	assertAsleep(t, result, "s-mc-1")
}

// ---------------------------------------------------------------------------
// Dependency only
// ---------------------------------------------------------------------------

func TestDependencyOnly_NotWokenByDemand(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "asleep", DependencyOnly: true},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

// ---------------------------------------------------------------------------
// Dependencies
// ---------------------------------------------------------------------------

func TestDependency_DepRunning(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{
			{QualifiedName: "hello-world/witness"},
			{QualifiedName: "hello-world/polecat", DependsOn: []string{"hello-world/witness"}},
		},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-w", SessionName: "hello-world--witness", Template: "hello-world/witness", State: "active"},
			{ID: "mc-p", SessionName: "polecat-mc-p", Template: "hello-world/polecat", State: "creating"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		RunningSessions:  map[string]bool{"hello-world--witness": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-p")
}

func TestDependency_DepNotRunning_StillDesired(t *testing.T) {
	// Dependency ordering is handled by the reconciler's wave-based
	// executePlannedStarts, not ComputeAwakeSet. A session whose
	// dependency isn't running yet should still be marked ShouldWake
	// so it reaches the start candidate list.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{
			{QualifiedName: "hello-world/witness"},
			{QualifiedName: "hello-world/polecat", DependsOn: []string{"hello-world/witness"}},
		},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-w", SessionName: "hello-world--witness", Template: "hello-world/witness", State: "asleep"},
			{ID: "mc-p", SessionName: "polecat-mc-p", Template: "hello-world/polecat", State: "creating"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-p")
}

// ---------------------------------------------------------------------------
// Idle sleep
// ---------------------------------------------------------------------------

func TestIdleSleep_ManualSession_Sleeps(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "active",
				ManualSession: true, IdleSince: now.Add(-20 * time.Minute),
			},
		},
		RunningSessions: map[string]bool{"s-mc-1": true},
		ChatIdleTimeout: 15 * time.Minute,
		Now:             now,
	})
	assertAsleep(t, result, "s-mc-1")
}

func TestIdleSleep_ManualSession_NotLongEnough(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "active",
				ManualSession: true, IdleSince: now.Add(-5 * time.Minute),
			},
		},
		RunningSessions: map[string]bool{"s-mc-1": true},
		ChatIdleTimeout: 15 * time.Minute,
		Now:             now,
	})
	assertAwake(t, result, "s-mc-1")
}

func TestIdleSleep_ManualSession_Attached_NeverSleeps(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "active",
				ManualSession: true, IdleSince: now.Add(-1 * time.Hour),
			},
		},
		RunningSessions:  map[string]bool{"s-mc-1": true},
		AttachedSessions: map[string]bool{"s-mc-1": true},
		ChatIdleTimeout:  15 * time.Minute,
		Now:              now,
	})
	assertAwake(t, result, "s-mc-1")
}

func TestIdleSleep_Disabled_NeverSleeps(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "active",
				ManualSession: true, IdleSince: now.Add(-24 * time.Hour),
			},
		},
		RunningSessions: map[string]bool{"s-mc-1": true},
		ChatIdleTimeout: 0,
		Now:             now,
	})
	assertAwake(t, result, "s-mc-1")
}

func TestIdleSleep_AgentSleepAfterIdle(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat", SleepAfterIdle: 2 * time.Hour}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active",
				IdleSince: now.Add(-3 * time.Hour),
			},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestIdleSleep_AgentNotIdleEnough(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat", SleepAfterIdle: 2 * time.Hour}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active",
				IdleSince: now.Add(-30 * time.Minute),
			},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-1")
}

func TestIdleSleep_OnDemandNamed(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery", SleepAfterIdle: 30 * time.Minute}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "active",
				NamedIdentity: "hello-world/refinery", IdleSince: now.Add(-1 * time.Hour),
			},
		},
		WorkBeads:       []AwakeWorkBead{{ID: "hw-1", Assignee: "hello-world/refinery", Status: "open"}},
		RunningSessions: map[string]bool{"hello-world--refinery": true},
		Now:             now,
	})
	assertAsleep(t, result, "hello-world--refinery")
}

// ---------------------------------------------------------------------------
// Bug regressions
// ---------------------------------------------------------------------------

func TestRegression_PoolManagedCreatingBead(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "creating"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-1")
}

func TestRegression_ManualSessionNotDrained(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "gascity/claude"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "s-mc-1", Template: "gascity/claude", State: "active", ManualSession: true},
		},
		ScaleCheckCounts: map[string]int{"gascity/claude": 0},
		RunningSessions:  map[string]bool{"s-mc-1": true},
		Now:              now,
	})
	assertAwake(t, result, "s-mc-1")
}

func TestRegression_OnDemandRefineryAssignee(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/refinery"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/refinery", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/refinery", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		WorkBeads:     []AwakeWorkBead{{ID: "hw-1", Assignee: "hello-world/refinery", Status: "open"}},
		Now:           now,
	})
	assertAwake(t, result, "hello-world--refinery")
}

func TestRegression_PolecatWithInProgressWork_StaysAwake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-p1", SessionName: "polecat-mc-p1", Template: "hello-world/polecat", State: "active"},
		},
		WorkBeads:        []AwakeWorkBead{{ID: "hw-1", Assignee: "mc-p1", Status: "in_progress"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{"polecat-mc-p1": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-p1")
}

func TestRegression_SessionWithOpenWorkByBeadID_StaysAwake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-p1", SessionName: "polecat-mc-p1", Template: "hello-world/polecat", State: "active"},
		},
		WorkBeads:        []AwakeWorkBead{{ID: "hw-1", Assignee: "mc-p1", Status: "open"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{"polecat-mc-p1": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-p1")
}

func TestRegression_SessionWithWorkByAlias_StaysAwake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-p1", SessionName: "polecat-mc-p1", Template: "hello-world/polecat", State: "active"},
		},
		WorkBeads:        []AwakeWorkBead{{ID: "hw-1", Assignee: "hello-world/polecat", Status: "in_progress"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{"polecat-mc-p1": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-p1")
}

// ---------------------------------------------------------------------------
// Asleep ephemeral with assigned work (e2e regression)
// ---------------------------------------------------------------------------

func TestRegression_AsleepEphemeralWithAssignedWork_WakesViaAssignedWork(t *testing.T) {
	// An asleep polecat that has in_progress work assigned to its bead ID
	// must wake via the assigned-work path, even though scaleCheck alone
	// would not wake it. This is the production path after a city restart:
	// the polecat claimed work, went to asleep, resume tier puts it in
	// desired, and ComputeAwakeSet must mark it ShouldWake=true.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-sctve", SessionName: "polecat-mc-sctve", Template: "hello-world/polecat", State: "asleep"},
		},
		WorkBeads:        []AwakeWorkBead{{ID: "hw-8lb", Assignee: "mc-sctve", Status: "in_progress"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-sctve")
	if result["polecat-mc-sctve"].Reason != "assigned-work" {
		t.Errorf("reason = %q, want assigned-work", result["polecat-mc-sctve"].Reason)
	}
}

// ---------------------------------------------------------------------------
// WorkSet — work_query demand signal (defense-in-depth alongside ScaleCheck)
// ---------------------------------------------------------------------------

func TestWorkSet_WakesOneSession_WhenScaleCheckZero(t *testing.T) {
	// work_query sees work but scale_check hasn't caught up (count=0).
	// WorkSet should wake exactly one active session.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
			{ID: "mc-2", SessionName: "polecat-mc-2", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		WorkSet:          map[string]bool{"hello-world/polecat": true},
		RunningSessions:  map[string]bool{"polecat-mc-1": true, "polecat-mc-2": true},
		Now:              now,
	})
	awake := 0
	for _, d := range result {
		if d.ShouldWake {
			awake++
		}
	}
	if awake != 1 {
		t.Errorf("WorkSet should wake exactly 1 session, got %d", awake)
	}
}

func TestWorkSet_ReasonIsWorkQuery(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		WorkSet:          map[string]bool{"hello-world/polecat": true},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAwake(t, result, "polecat-mc-1")
	assertReason(t, result, "polecat-mc-1", "work-query")
}

func TestWorkSet_NoOpWhenScaleCheckCovers(t *testing.T) {
	// When ScaleCheckCounts already covers the template, WorkSet shouldn't
	// add extra sessions — ScaleCheck is the authoritative count.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
			{ID: "mc-2", SessionName: "polecat-mc-2", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		WorkSet:          map[string]bool{"hello-world/polecat": true},
		RunningSessions:  map[string]bool{"polecat-mc-1": true, "polecat-mc-2": true},
		Now:              now,
	})
	awake := 0
	for _, d := range result {
		if d.ShouldWake {
			awake++
		}
	}
	if awake != 1 {
		t.Errorf("ScaleCheck=1 should cap to 1, WorkSet should not add more, got %d awake", awake)
	}
	// The awake session should have reason "scaled:demand", not "work-query"
	assertReason(t, result, "polecat-mc-1", "scaled:demand")
}

func TestWorkSet_SkipsDependencyOnly(t *testing.T) {
	// dependency_only sessions should NOT be woken by WorkSet.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active", DependencyOnly: true},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		WorkSet:          map[string]bool{"hello-world/polecat": true},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestWorkSet_SkipsDrained(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active", Drained: true},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		WorkSet:          map[string]bool{"hello-world/polecat": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestWorkSet_SkipsSuspendedAgent(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat", Suspended: true}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		WorkSet:          map[string]bool{"hello-world/polecat": true},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestWorkSet_WakesNamedSessionFromTemplateKey(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/worker"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/worker", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "hello-world--refinery", Template: "hello-world/worker", State: "asleep", NamedIdentity: "hello-world/refinery"}},
		WorkSet:       map[string]bool{"hello-world/worker": true},
		Now:           now,
	})
	assertAwake(t, result, "hello-world--refinery")
	assertReason(t, result, "hello-world--refinery", "named-on-demand:work-query")
}

func TestWorkSet_WakesRigScopedNamedSessionFromQualifiedTemplateKey(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "rig-a/worker"}},
		NamedSessions: []AwakeNamedSession{{Identity: "rig-a/refinery", Template: "rig-a/worker", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "gc-test--rig-a--refinery", Template: "rig-a/worker", State: "asleep", NamedIdentity: "rig-a/refinery"}},
		WorkSet:       map[string]bool{"rig-a/worker": true},
		Now:           now,
	})
	assertAwake(t, result, "gc-test--rig-a--refinery")
	assertReason(t, result, "gc-test--rig-a--refinery", "named-on-demand:work-query")
}

func TestWorkSet_SkipsOrdinarySiblingForNamedTemplate(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/worker"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/worker", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-ordinary", SessionName: "worker-pool-1", Template: "hello-world/worker", State: "active"},
			{ID: "mc-named", SessionName: "hello-world--refinery", Template: "hello-world/worker", State: "active", NamedIdentity: "hello-world/refinery"},
		},
		WorkSet:         map[string]bool{"hello-world/worker": true},
		RunningSessions: map[string]bool{"worker-pool-1": true, "hello-world--refinery": true},
		Now:             now,
	})
	assertAsleep(t, result, "worker-pool-1")
	assertAwake(t, result, "hello-world--refinery")
	assertReason(t, result, "hello-world--refinery", "named-on-demand:work-query")
}

func TestScaleCheck_SkipsOrdinarySiblingForNamedTemplate(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "hello-world/worker"}},
		NamedSessions: []AwakeNamedSession{{Identity: "hello-world/refinery", Template: "hello-world/worker", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-ordinary", SessionName: "worker-pool-1", Template: "hello-world/worker", State: "active"},
			{ID: "mc-named", SessionName: "hello-world--refinery", Template: "hello-world/worker", State: "active", NamedIdentity: "hello-world/refinery"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/worker": 1},
		RunningSessions:  map[string]bool{"worker-pool-1": true, "hello-world--refinery": true},
		Now:              now,
	})
	assertAsleep(t, result, "worker-pool-1")
	assertAwake(t, result, "hello-world--refinery")
	assertReason(t, result, "hello-world--refinery", "on-demand:running")
}

func TestScaleCheck_SkipsOrdinarySiblingForRigScopedNamedTemplate(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "rig-a/worker"}},
		NamedSessions: []AwakeNamedSession{{Identity: "rig-a/refinery", Template: "rig-a/worker", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-ordinary", SessionName: "worker-pool-1", Template: "rig-a/worker", State: "active"},
			{ID: "mc-named", SessionName: "gc-test--rig-a--refinery", Template: "rig-a/worker", State: "active", NamedIdentity: "rig-a/refinery"},
		},
		ScaleCheckCounts: map[string]int{"rig-a/worker": 1},
		RunningSessions:  map[string]bool{"worker-pool-1": true, "gc-test--rig-a--refinery": true},
		Now:              now,
	})
	assertAsleep(t, result, "worker-pool-1")
	assertAwake(t, result, "gc-test--rig-a--refinery")
	assertReason(t, result, "gc-test--rig-a--refinery", "on-demand:running")
}

func TestWorkSet_FallsBackToCreating(t *testing.T) {
	// When no active sessions exist, WorkSet should wake a creating one.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "creating"},
		},
		WorkSet: map[string]bool{"hello-world/polecat": true},
		Now:     now,
	})
	assertAwake(t, result, "polecat-mc-1")
	assertReason(t, result, "polecat-mc-1", "work-query")
}

func TestWorkSet_FalseValue_NoEffect(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		WorkSet:          map[string]bool{"hello-world/polecat": false},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestWorkSet_NilMap_NoEffect(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestWorkSet_SuppressedByHeldUntil(t *testing.T) {
	// HeldUntil suppresses all wake reasons including WorkSet
	// (step 5 hold override in ComputeAwakeSet).
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active",
				HeldUntil: now.Add(5 * time.Minute),
			},
		},
		WorkSet:         map[string]bool{"hello-world/polecat": true},
		RunningSessions: map[string]bool{"polecat-mc-1": true},
		Now:             now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}

func TestRegression_AsleepEphemeralWithoutWork_StaysAsleep(t *testing.T) {
	// An asleep polecat WITHOUT assigned work should NOT wake, even with
	// scaleCheck demand. A fresh session should be created instead.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-old", SessionName: "polecat-mc-old", Template: "hello-world/polecat", State: "asleep"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 1},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-old")
}

// --- On-demand running override tests ---

func TestOnDemand_RunningStaysAwake(t *testing.T) {
	// On-demand named session is running but has no demand (scale=0,
	// no assigned work). Should stay awake via "on-demand:running".
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "gascity/quinn"}},
		NamedSessions: []AwakeNamedSession{{Identity: "gascity/quinn", Template: "gascity/quinn", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "gascity--quinn", Template: "gascity/quinn", State: "active", NamedIdentity: "gascity/quinn"},
		},
		RunningSessions: map[string]bool{"gascity--quinn": true},
		Now:             now,
	})
	assertAwake(t, result, "gascity--quinn")
	if d := result["gascity--quinn"]; d.Reason != "on-demand:running" {
		t.Errorf("reason = %q, want %q", d.Reason, "on-demand:running")
	}
}

func TestOnDemand_AsleepNotForced(t *testing.T) {
	// On-demand named session is NOT running. Should stay asleep.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "gascity/quinn"}},
		NamedSessions: []AwakeNamedSession{{Identity: "gascity/quinn", Template: "gascity/quinn", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "gascity--quinn", Template: "gascity/quinn", State: "asleep", NamedIdentity: "gascity/quinn"},
		},
		RunningSessions: map[string]bool{},
		Now:             now,
	})
	assertAsleep(t, result, "gascity--quinn")
}

func TestOnDemand_RunningDrainsAfterIdleTimeout(t *testing.T) {
	// On-demand running but idle past explicit timeout. Idle sleep overrides.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "gascity/quinn", SleepAfterIdle: 5 * time.Minute}},
		NamedSessions: []AwakeNamedSession{{Identity: "gascity/quinn", Template: "gascity/quinn", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "gascity--quinn", Template: "gascity/quinn", State: "active", NamedIdentity: "gascity/quinn",
				IdleSince: now.Add(-6 * time.Minute),
			},
		},
		RunningSessions: map[string]bool{"gascity--quinn": true},
		Now:             now,
	})
	assertAsleep(t, result, "gascity--quinn")
}

func TestOnDemand_DefaultIdleTimeoutDrains(t *testing.T) {
	// No explicit idle_timeout. Default 5min should drain after 6min idle.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "gascity/quinn"}},
		NamedSessions: []AwakeNamedSession{{Identity: "gascity/quinn", Template: "gascity/quinn", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "gascity--quinn", Template: "gascity/quinn", State: "active", NamedIdentity: "gascity/quinn",
				IdleSince: now.Add(-6 * time.Minute),
			},
		},
		RunningSessions: map[string]bool{"gascity--quinn": true},
		Now:             now,
	})
	assertAsleep(t, result, "gascity--quinn")
}

func TestOnDemand_DefaultIdleTimeoutKeepsAlive(t *testing.T) {
	// No explicit idle_timeout. Default 5min, only 2min idle. Stays awake.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "gascity/quinn"}},
		NamedSessions: []AwakeNamedSession{{Identity: "gascity/quinn", Template: "gascity/quinn", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "gascity--quinn", Template: "gascity/quinn", State: "active", NamedIdentity: "gascity/quinn",
				IdleSince: now.Add(-2 * time.Minute),
			},
		},
		RunningSessions: map[string]bool{"gascity--quinn": true},
		Now:             now,
	})
	assertAwake(t, result, "gascity--quinn")
}

func TestOnDemand_RunningNotIdleYet(t *testing.T) {
	// On-demand running, idle 2min, explicit timeout 5min. Stays awake.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "gascity/quinn", SleepAfterIdle: 5 * time.Minute}},
		NamedSessions: []AwakeNamedSession{{Identity: "gascity/quinn", Template: "gascity/quinn", Mode: "on_demand"}},
		SessionBeads: []AwakeSessionBead{
			{
				ID: "mc-1", SessionName: "gascity--quinn", Template: "gascity/quinn", State: "active", NamedIdentity: "gascity/quinn",
				IdleSince: now.Add(-2 * time.Minute),
			},
		},
		RunningSessions: map[string]bool{"gascity--quinn": true},
		Now:             now,
	})
	assertAwake(t, result, "gascity--quinn")
}

func TestAlwaysNamed_NotAffectedByRunningOverride(t *testing.T) {
	// Always-mode uses desired set, not on-demand override.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "mayor"}},
		NamedSessions: []AwakeNamedSession{{Identity: "mayor", Template: "mayor", Mode: "always"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "mayor", Template: "mayor", State: "active", NamedIdentity: "mayor"},
		},
		RunningSessions: map[string]bool{"mayor": true},
		Now:             now,
	})
	assertAwake(t, result, "mayor")
	if d := result["mayor"]; d.Reason != "named-always" {
		t.Errorf("reason = %q, want %q", d.Reason, "named-always")
	}
}

// ---------------------------------------------------------------------------
// Named session suspension (ga-40x)
//
// ComputeAwakeSet sees a pre-collapsed Suspended bool. The rig/agent/city
// distinction is resolved upstream in isAgentEffectivelySuspended (tested in
// cmd_suspend_test.go). Tests here verify the pure-function guard; the
// bridge test below verifies source-specific propagation end-to-end.
// ---------------------------------------------------------------------------

func TestNamedAlways_Suspended_Sleeps(t *testing.T) {
	// Effective suspension (regardless of source: rig, agent, or city) →
	// named-always should NOT wake.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "monorepo/witness", Suspended: true}},
		NamedSessions: []AwakeNamedSession{{Identity: "monorepo/witness", Template: "witness", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "witness", Template: "monorepo/witness", State: "active", NamedIdentity: "monorepo/witness"}},
		Now:           now,
	})
	assertAsleep(t, result, "witness")
}

func TestNamedAlways_CitySuspended_AllSleep(t *testing.T) {
	// Multiple agents all effectively suspended → no named sessions wake.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{
			{QualifiedName: "monorepo/witness", Suspended: true},
			{QualifiedName: "monorepo/refinery", Suspended: true},
		},
		NamedSessions: []AwakeNamedSession{
			{Identity: "monorepo/witness", Template: "witness", Mode: "always"},
			{Identity: "monorepo/refinery", Template: "refinery", Mode: "always"},
		},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "witness", Template: "monorepo/witness", State: "active", NamedIdentity: "monorepo/witness"},
			{ID: "mc-2", SessionName: "refinery", Template: "monorepo/refinery", State: "active", NamedIdentity: "monorepo/refinery"},
		},
		Now: now,
	})
	assertAsleep(t, result, "witness")
	assertAsleep(t, result, "refinery")
}

func TestNamedAlways_NotSuspended_StillWakes(t *testing.T) {
	// Regression guard: not suspended → named-always still wakes.
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "monorepo/witness", Suspended: false}},
		NamedSessions: []AwakeNamedSession{{Identity: "monorepo/witness", Template: "witness", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "witness", Template: "monorepo/witness", State: "active", NamedIdentity: "monorepo/witness"}},
		Now:           now,
	})
	assertAwake(t, result, "witness")
	assertReason(t, result, "witness", "named-always")
}

// TestNamedAlways_SuspensionPropagation verifies the end-to-end path from
// each suspension source (rig, agent, city) through isAgentEffectivelySuspended
// into ComputeAwakeSet. This bridges the unit tests in cmd_suspend_test.go
// with the pure-function tests above.
func TestNamedAlways_SuspensionPropagation(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.City
	}{
		{
			name: "rig_suspended",
			cfg: config.City{
				Workspace: config.Workspace{Name: "test"},
				Agents:    []config.Agent{{Name: "witness", Dir: "myrig"}},
				Rigs:      []config.Rig{{Name: "myrig", Path: "/tmp/myrig", Suspended: true}},
				NamedSessions: []config.NamedSession{
					{Template: "witness", Dir: "myrig", Mode: "always"},
				},
			},
		},
		{
			name: "agent_suspended",
			cfg: config.City{
				Workspace: config.Workspace{Name: "test"},
				Agents:    []config.Agent{{Name: "witness", Suspended: true}},
				NamedSessions: []config.NamedSession{
					{Template: "witness", Mode: "always"},
				},
			},
		},
		{
			name: "city_suspended",
			cfg: config.City{
				Workspace: config.Workspace{Name: "test", Suspended: true},
				Agents:    []config.Agent{{Name: "witness"}},
				NamedSessions: []config.NamedSession{
					{Template: "witness", Mode: "always"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &tt.cfg.Agents[0]
			if !isAgentEffectivelySuspended(&tt.cfg, a) {
				t.Fatalf("expected agent to be effectively suspended")
			}
			qn := a.QualifiedName()
			result := ComputeAwakeSet(AwakeInput{
				Agents:        []AwakeAgent{{QualifiedName: qn, Suspended: true}},
				NamedSessions: []AwakeNamedSession{{Identity: qn, Template: qn, Mode: "always"}},
				SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "witness", Template: qn, State: "active", NamedIdentity: qn}},
				Now:           now,
			})
			assertAsleep(t, result, "witness")
		})
	}
}

func TestScaledPool_NotAffectedByRunningOverride(t *testing.T) {
	// Pool with scale=0 and running session. Override must NOT
	// keep pool sessions alive — scale-down must work.
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-1", SessionName: "polecat-mc-1", Template: "hello-world/polecat", State: "active"},
		},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{"polecat-mc-1": true},
		Now:              now,
	})
	assertAsleep(t, result, "polecat-mc-1")
}
