package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestComputePoolDesiredStates_UnresolvedRigRefusesFloorAndNewDemand(t *testing.T) {
	workspaceMax := 10
	agent := poolAgent("worker", "/tmp/missing-rig", nil, 2)
	agent.Scope = "rig"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Rigs:      []config.Rig{{Name: "known", Path: "/tmp/known"}},
		Agents:    []config.Agent{agent},
	}
	template := agent.QualifiedName()
	trace := newPoolDesiredStateTestTrace(template)
	var logs bytes.Buffer
	oldLogOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldLogOutput) })

	result := ComputePoolDesiredStatesTraced(cfg, nil, nil, map[string]int{template: 3}, trace)
	counts := poolDesiredRequestCounts(result)

	if counts[template] != 0 {
		t.Fatalf("request counts = %#v, want %s=0: unresolved rig must receive neither its floor nor new demand", counts, template)
	}
	rec := poolTraceDecision(t, trace, TraceSitePoolNewDemandCap)
	if got, ok := rec.Fields["rig_resolution_error"].(bool); !ok || !got {
		t.Fatalf("rig_resolution_error = %#v, want true; record=%#v", rec.Fields["rig_resolution_error"], rec)
	}
	if !strings.Contains(logs.String(), "rig_resolution_error") || !strings.Contains(logs.String(), template) {
		t.Fatalf("supervisor log = %q, want rig_resolution_error and template %q", logs.String(), template)
	}
}

// A refused floor must not hold workspace capacity either: the unresolved agent
// gets nothing, so its minimum cannot shrink the headroom other pools compete for.
func TestComputePoolDesiredStates_UnresolvedRigFloorReservesNoWorkspaceCapacity(t *testing.T) {
	workspaceMax := 2
	unresolved := poolAgent("orphan", "/tmp/missing-rig", nil, 2)
	unresolved.Scope = "rig"
	other := poolAgent("worker", "", nil, 0)
	other.Scope = "city"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{unresolved, other},
	}

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{other.QualifiedName(): 2})
	counts := poolDesiredRequestCounts(result)

	if counts[unresolved.QualifiedName()] != 0 || counts[other.QualifiedName()] != 2 {
		t.Fatalf("request counts = %#v, want %s=0 and %s=2: a refused floor must reserve no workspace capacity",
			counts, unresolved.QualifiedName(), other.QualifiedName())
	}
}

func TestComputePoolDesiredStates_UnresolvedRigPreservesExistingSession(t *testing.T) {
	agent := poolAgent("worker", "/tmp/missing-rig", nil, 1)
	agent.Scope = "rig"
	cfg := &config.City{Agents: []config.Agent{agent}}
	template := agent.QualifiedName()
	work := []beads.Bead{workBead("work-1", template, "session-1", "in_progress", 1)}
	sessions := []beads.Bead{sessionBead("session-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), map[string]int{template: 2})

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("result = %#v, want only the existing session retained", result)
	}
	request := result[0].Requests[0]
	if request.Tier != "resume" || request.SessionBeadID != "session-1" {
		t.Fatalf("retained request = %#v, want resume for session-1", request)
	}
}

func TestComputePoolDesiredStates_ExistingSessionsPrecedeCompetingFloor(t *testing.T) {
	workspaceMax := 2
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents: []config.Agent{
			poolAgent("a", "", nil, 0),
			poolAgent("b", "", nil, 2),
		},
	}
	work := []beads.Bead{
		workBead("work-1", "a", "session-1", "in_progress", 1),
		workBead("work-2", "a", "session-2", "in_progress", 1),
	}
	sessions := []beads.Bead{
		sessionBead("session-1", "open"),
		sessionBead("session-2", "open"),
	}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)
	counts := poolDesiredRequestCounts(result)

	if counts["a"] != 2 || counts["b"] != 0 {
		t.Fatalf("request counts = %#v, want a=2 and b=0: floors must not displace existing sessions", counts)
	}
}

func TestComputePoolDesiredStates_RigCappedExistingSessionPrecedesCompetingFloor(t *testing.T) {
	rigMax := 1
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "r", Path: "/tmp/r", MaxActiveSessions: &rigMax}},
		Agents: []config.Agent{
			poolAgent("b", "r", nil, 1),
			poolAgent("a", "r", nil, 0),
		},
	}
	work := []beads.Bead{workBead("work-1", "r/a", "session-1", "in_progress", 1)}
	sessions := []beads.Bead{sessionBead("session-1", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)
	counts := poolDesiredRequestCounts(result)

	if counts["r/a"] != 1 || counts["r/b"] != 0 {
		t.Fatalf("request counts = %#v, want r/a=1 and r/b=0: a rig floor must not displace an existing session", counts)
	}
	if ids := poolDesiredSessionIDs(result, "r/a"); len(ids) != 1 || ids[0] != "session-1" {
		t.Fatalf("r/a session IDs = %#v, want [session-1]", ids)
	}
}

func TestComputePoolDesiredStates_InFlightSessionsPrecedeCompetingFloor(t *testing.T) {
	now := time.Date(2026, 9, 23, 21, 0, 0, 0, time.UTC)
	for _, floorName := range []string{"aaa", "zzz"} {
		t.Run(floorName, func(t *testing.T) {
			workspaceMax := 2
			cfg := &config.City{
				Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
				Agents: []config.Agent{
					poolAgent("claude", "", nil, 0),
					poolAgent(floorName, "", nil, 2),
				},
			}
			sessions := []beads.Bead{
				pendingPoolSessionBeadAt("session-1", now.Add(-30*time.Second)),
				pendingPoolSessionBeadAt("session-2", now.Add(-20*time.Second)),
			}

			result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 0}, now)
			counts := poolDesiredRequestCounts(result)

			if counts["claude"] != 2 || counts[floorName] != 0 {
				t.Fatalf("request counts = %#v, want claude=2 and %s=0: a floor must not displace in-flight sessions", counts, floorName)
			}
			ids := poolDesiredSessionIDs(result, "claude")
			if len(ids) != 2 || ids[0] != "session-1" || ids[1] != "session-2" {
				t.Fatalf("claude session IDs = %#v, want [session-1 session-2]", ids)
			}
		})
	}
}

func TestComputePoolDesiredStates_ProtectedSessionsPrecedeCompetingFloor(t *testing.T) {
	now := time.Date(2026, 9, 23, 21, 0, 0, 0, time.UTC)
	for _, floorName := range []string{"aaa", "zzz"} {
		t.Run(floorName, func(t *testing.T) {
			workspaceMax := 2
			cfg := &config.City{
				Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
				Agents: []config.Agent{
					poolAgent("claude", "", nil, 0),
					poolAgent(floorName, "", nil, 2),
				},
			}
			sessions := []beads.Bead{
				protectedPoolSessionBeadAt("session-1", now.Add(-30*time.Second)),
				protectedPoolSessionBeadAt("session-2", now.Add(-20*time.Second)),
			}

			result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), nil, now)
			counts := poolDesiredRequestCounts(result)

			if counts["claude"] != 2 || counts[floorName] != 0 {
				t.Fatalf("request counts = %#v, want claude=2 and %s=0: a floor must not displace protected sessions", counts, floorName)
			}
		})
	}
}

func TestComputePoolDesiredStates_UnresolvedRigPreservesInFlightSession(t *testing.T) {
	agent := poolAgent("claude", "", nil, 0)
	agent.Scope = "rig"
	cfg := &config.City{Agents: []config.Agent{agent}}
	sessions := []beads.Bead{pendingPoolSessionBead("session-1")}

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 1})

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("result = %#v, want only the in-flight session retained", result)
	}
	request := result[0].Requests[0]
	if request.SessionBeadID != "session-1" {
		t.Fatalf("retained request = %#v, want session-1", request)
	}
}

// The unresolved-rig carve-out preserves concrete sessions, but it must not
// charge the shared pre-pass usage for concrete capacity applyNestedCaps will
// reject anyway: that pass can only reject candidates, never mint them, so an
// over-charge permanently costs every later template its new-demand headroom.
func TestComputePoolDesiredStates_UnresolvedRigConcreteRespectsAgentCap(t *testing.T) {
	now := time.Date(2026, 9, 23, 21, 0, 0, 0, time.UTC)
	workspaceMax := 4
	unresolved := poolAgent("claude", "", intPtr(1), 0)
	unresolved.Scope = "rig"
	other := poolAgent("worker", "", nil, 0)
	other.Scope = "city"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{unresolved, other},
	}
	// Two protected sessions against max_active_sessions=1: applyNestedCaps
	// admits exactly one of them, so only one may be charged here.
	sessions := []beads.Bead{
		protectedPoolSessionBeadAt("session-1", now.Add(-30*time.Second)),
		protectedPoolSessionBeadAt("session-2", now.Add(-20*time.Second)),
	}

	result := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"worker": 3}, now)
	counts := poolDesiredRequestCounts(result)

	if counts["claude"] != 1 || counts["worker"] != 3 {
		t.Fatalf("request counts = %#v, want claude=1 and worker=3: an over-provisioned unresolved-rig template must not strand workspace headroom", counts)
	}
}

func TestComputePoolDesiredStates_CityScopedAgentNeedsNoRigCap(t *testing.T) {
	workspaceMax := 10
	agent := poolAgent("worker", "/tmp/not-a-rig", nil, 1)
	agent.Scope = "city"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents:    []config.Agent{agent},
	}
	template := agent.QualifiedName()

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{template: 2})
	counts := poolDesiredRequestCounts(result)

	if counts[template] != 2 {
		t.Fatalf("request counts = %#v, want %s=2: city-scoped agent must not require a rig", counts, template)
	}
}

func TestComputePoolDesiredStates_ResolvedRigStillHonorsRigCap(t *testing.T) {
	workspaceMax := 10
	rigMax := 1
	agent := poolAgent("worker", "/tmp/rig", nil, 2)
	agent.Scope = "rig"
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Rigs:      []config.Rig{{Name: "rig", Path: "/tmp/rig", MaxActiveSessions: &rigMax}},
		Agents:    []config.Agent{agent},
	}
	template := agent.QualifiedName()

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{template: 3})
	counts := poolDesiredRequestCounts(result)

	if counts[template] != 1 {
		t.Fatalf("request counts = %#v, want %s=1 from resolved rig cap", counts, template)
	}
}

func TestComputePoolDesiredStates_ReservesWorkspaceCapacityForFloors(t *testing.T) {
	tests := []struct {
		name   string
		agents []config.Agent
	}{
		{
			name: "demand agent first",
			agents: []config.Agent{
				poolAgent("a", "", nil, 0),
				poolAgent("b", "", nil, 2),
			},
		},
		{
			name: "floor agent first",
			agents: []config.Agent{
				poolAgent("b", "", nil, 2),
				poolAgent("a", "", nil, 0),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspaceMax := 4
			cfg := &config.City{
				Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
				Agents:    tt.agents,
			}
			work := []beads.Bead{
				workBead("a-1", "a", "", "open", 1),
				workBead("a-2", "a", "", "open", 1),
				workBead("a-3", "a", "", "open", 1),
				workBead("a-4", "a", "", "open", 1),
			}

			result := ComputePoolDesiredStates(cfg, work, nil, map[string]int{"a": 4})
			counts := poolDesiredRequestCounts(result)

			if counts["a"] != 2 || counts["b"] != 2 {
				t.Fatalf("request counts = %#v, want a=2 b=2", counts)
			}
		})
	}
}

func TestComputePoolDesiredStates_DemandForFloorTemplateUsesReservation(t *testing.T) {
	workspaceMax := 4
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents: []config.Agent{
			poolAgent("a", "", nil, 0),
			poolAgent("b", "", nil, 2),
		},
	}
	work := []beads.Bead{
		workBead("a-1", "a", "", "open", 1),
		workBead("a-2", "a", "", "open", 1),
		workBead("a-3", "a", "", "open", 1),
		workBead("a-4", "a", "", "open", 1),
		workBead("b-1", "b", "", "open", 2),
		workBead("b-2", "b", "", "open", 2),
		workBead("b-3", "b", "", "open", 2),
	}

	result := ComputePoolDesiredStatesWithDemandTraced(
		cfg,
		work,
		nil,
		map[string]int{"a": 4, "b": 3},
		map[string]scaleCheckDemand{
			"a": {Count: 4, WorkBeadIDs: []string{"a-1", "a-2", "a-3", "a-4"}},
			"b": {Count: 3, WorkBeadIDs: []string{"b-1", "b-2", "b-3"}},
		},
		nil,
	)
	counts := poolDesiredRequestCounts(result)
	total := counts["a"] + counts["b"]

	if counts["b"] < 2 {
		t.Fatalf("request counts = %#v, want b>=2", counts)
	}
	if total > workspaceMax {
		t.Fatalf("total requests = %d, want <= %d; counts=%#v", total, workspaceMax, counts)
	}
	for _, state := range result {
		if state.Template != "b" {
			continue
		}
		for _, request := range state.Requests[:2] {
			if request.WorkBeadID == "" || !request.FloorGuarantee {
				t.Fatalf("floor request = %#v, want preserved demand bead marked as floor guarantee", request)
			}
		}
	}
}

func TestApplyNestedCaps_OvercommittedFloorsUseLexicalTemplateOrder(t *testing.T) {
	workspaceMax := 3
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
		Agents: []config.Agent{
			poolAgent("zeta", "", nil, 2),
			poolAgent("alpha", "", nil, 2),
		},
	}
	trace := newPoolDesiredStateTestTrace("alpha", "zeta")

	result := applyNestedCaps(cfg, nil, nil, trace)
	counts := poolDesiredRequestCounts(result)

	if counts["alpha"] != 2 || counts["zeta"] != 1 {
		t.Fatalf("request counts = %#v, want lexical floor allocation alpha=2 zeta=1", counts)
	}
	rec := poolTraceDecision(t, trace, TraceSitePoolWorkspaceCap)
	if rec.Template != "zeta" {
		t.Fatalf("rejected floor template = %q, want zeta; record=%#v", rec.Template, rec)
	}
	if got, ok := rec.Fields["floor_template"].(string); !ok || got != "zeta" {
		t.Fatalf("floor_template = %#v, want zeta; record=%#v", rec.Fields["floor_template"], rec)
	}
}

// TestApplyNestedCaps_NoFloorsPreservesDemandPriority pins both precedence
// regimes applyNestedCaps implements when no floor is configured: BeadPriority
// orders competing *new* demand, while a concrete request (resume-like tier or
// one carrying a SessionBeadID) is admitted in phase 1 and so outranks new
// demand of any priority.
func TestApplyNestedCaps_NoFloorsPreservesDemandPriority(t *testing.T) {
	t.Run("new demand competes by priority", func(t *testing.T) {
		workspaceMax := 3
		cfg := &config.City{
			Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
			Agents: []config.Agent{
				poolAgent("a", "", nil, 0),
				poolAgent("b", "", nil, 0),
			},
		}
		requests := []SessionRequest{
			{Template: "b", Tier: "new", BeadPriority: 3, WorkBeadID: "b-1"},
			{Template: "a", Tier: "new", BeadPriority: 4, WorkBeadID: "a-1"},
			{Template: "b", Tier: "new", BeadPriority: 3, WorkBeadID: "b-2"},
			{Template: "a", Tier: "new", BeadPriority: 4, WorkBeadID: "a-2"},
		}

		result := applyNestedCaps(cfg, requests, nil, nil)
		counts := poolDesiredRequestCounts(result)

		if counts["a"] != 2 || counts["b"] != 1 {
			t.Fatalf("request counts = %#v, want priority allocation a=2 b=1", counts)
		}
	})

	// The concrete-first phase outranks BeadPriority across tiers, which the
	// rows above cannot observe: they are all "new" tier with no SessionBeadID,
	// so every one of them skips phase 1. Before the concrete-first phase
	// existed the single priority walk gave this slot to b.
	t.Run("concrete outranks higher-priority new demand", func(t *testing.T) {
		workspaceMax := 1
		cfg := &config.City{
			Workspace: config.Workspace{MaxActiveSessions: &workspaceMax},
			Agents: []config.Agent{
				poolAgent("a", "", nil, 0),
				poolAgent("b", "", nil, 0),
			},
		}
		requests := []SessionRequest{
			{Template: "a", Tier: "resume", BeadPriority: 1, SessionBeadID: "session-1"},
			{Template: "b", Tier: "new", BeadPriority: 9, WorkBeadID: "b-1"},
		}

		result := applyNestedCaps(cfg, requests, nil, nil)
		counts := poolDesiredRequestCounts(result)

		if counts["a"] != 1 || counts["b"] != 0 {
			t.Fatalf("request counts = %#v, want a=1 and b=0: a live session keeps the last slot against higher-priority new demand", counts)
		}
		if ids := poolDesiredSessionIDs(result, "a"); len(ids) != 1 || ids[0] != "session-1" {
			t.Fatalf("template a session ids = %#v, want [session-1]", ids)
		}
	})
}

func TestApplyNestedCaps_PathShapedAgentDirHonorsRigCap(t *testing.T) {
	rigMax := 1
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "rig", Path: "/tmp/rig", MaxActiveSessions: &rigMax}},
		Agents: []config.Agent{
			poolAgent("worker", "/tmp/rig", nil, 0),
		},
	}
	template := cfg.Agents[0].QualifiedName()
	requests := []SessionRequest{
		{Template: template, Tier: "new", WorkBeadID: "work-1"},
		{Template: template, Tier: "new", WorkBeadID: "work-2"},
	}

	result := applyNestedCaps(cfg, requests, nil, nil)
	counts := poolDesiredRequestCounts(result)

	if counts[template] != 1 {
		t.Fatalf("request counts = %#v, want %s=1 from rig cap", counts, template)
	}
}

func poolDesiredRequestCounts(states []PoolDesiredState) map[string]int {
	counts := make(map[string]int, len(states))
	for _, state := range states {
		counts[state.Template] = len(state.Requests)
	}
	return counts
}

func poolDesiredSessionIDs(states []PoolDesiredState, template string) []string {
	var ids []string
	for _, state := range states {
		if state.Template != template {
			continue
		}
		for _, request := range state.Requests {
			ids = append(ids, request.SessionBeadID)
		}
	}
	return ids
}
