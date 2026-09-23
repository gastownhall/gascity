package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

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

func TestApplyNestedCaps_NoFloorsPreservesDemandPriority(t *testing.T) {
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
