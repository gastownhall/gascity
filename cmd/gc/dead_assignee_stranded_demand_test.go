package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildDesiredStateDeadAssigneeCountsAsPoolDemand(t *testing.T) {
	store := beads.NewMemStore()
	cfg := deadAssigneeDemandConfig(1)
	template := cfg.Agents[0].QualifiedName()
	closed := deadAssigneeSessionBead("session-dead", "worker-dead", template, "closed")
	createdClosed, err := store.Create(closed)
	if err != nil {
		t.Fatalf("create closed session bead: %v", err)
	}
	// MemStore.Create always creates beads open (in production, closing a
	// session happens through the runtime lifecycle, not the create call);
	// Close is how a test puts one into the closed state Create can't set.
	if err := store.Close(createdClosed.ID); err != nil {
		t.Fatalf("close session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		ID:       "ga-dead-assignee",
		Title:    "ready work held by a closed worker",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatalf("create assigned routed work: %v", err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
	)

	if got := result.ScaleCheckCounts[template]; got != 1 {
		t.Fatalf("ScaleCheckCounts[%q] = %d, want 1 for ready work assigned to a confirmed-dead session", template, got)
	}
	if len(result.State) != 1 {
		t.Fatalf("desired session count = %d, want 1 replacement for confirmed-dead assignee; state=%v", len(result.State), result.State)
	}
}

func TestBuildDesiredStateDoesNotTreatOpenAssigneesAsDeadDemand(t *testing.T) {
	for _, state := range []string{"active", "idle", "asleep", ""} {
		t.Run("state_"+state, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := deadAssigneeDemandConfig(2)
			template := cfg.Agents[0].QualifiedName()
			session := deadAssigneeSessionBead("session-open", "worker-open", template, "open")
			session.Metadata["state"] = state
			if _, err := store.Create(session); err != nil {
				t.Fatalf("create open session bead: %v", err)
			}
			if _, err := store.Create(beads.Bead{
				ID:       "ga-open-assignee",
				Title:    "ready work held by an open worker",
				Type:     "task",
				Status:   "open",
				Assignee: "worker-open",
				Metadata: map[string]string{"gc.routed_to": template},
			}); err != nil {
				t.Fatalf("create assigned routed work: %v", err)
			}

			result := buildDesiredStateWithSessionBeads(
				"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
				store, nil, newSessionBeadSnapshot([]beads.Bead{session}), nil, io.Discard,
			)

			if got := result.ScaleCheckCounts[template]; got != 0 {
				t.Fatalf("ScaleCheckCounts[%q] = %d, want 0: open session state %q is not confirmed-dead fallback demand", template, got, state)
			}
			requests := PoolDesiredCounts(ComputePoolDesiredStates(cfg, result.AssignedWorkBeads, sessionInfosFromBeads([]beads.Bead{session}), result.ScaleCheckCounts))
			if got := requests[template]; got != 1 {
				t.Fatalf("pool desired for open session state %q = %d, want 1 resume demand for the existing session", state, got)
			}
		})
	}
}

func TestBuildDesiredStateDoesNotTreatUncertainAssigneeAsDeadDemand(t *testing.T) {
	store := beads.NewMemStore()
	cfg := deadAssigneeDemandConfig(1)
	template := cfg.Agents[0].QualifiedName()
	if _, err := store.Create(beads.Bead{
		ID:       "ga-unknown-assignee",
		Title:    "ready work held by an unknown assignee",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-never-seen",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatalf("create assigned routed work: %v", err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
	)

	if got := result.ScaleCheckCounts[template]; got != 0 {
		t.Fatalf("ScaleCheckCounts[%q] = %d, want 0 for unresolvable/uncertain assignee", template, got)
	}
	if len(result.State) != 0 {
		t.Fatalf("desired session count = %d, want 0 for unresolvable/uncertain assignee; state=%v", len(result.State), result.State)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandSkipsAmbiguousDeadAssignee(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	sessions := []beads.Bead{
		deadAssigneeSessionBead("session-dead-a", "shared-dead", "worker", "closed"),
		deadAssigneeSessionBead("session-dead-b", "shared-dead", "worker", "closed"),
	}
	work := []beads.Bead{{
		ID:       "ga-ambiguous-dead-assignee",
		Type:     "task",
		Status:   "open",
		Assignee: "shared-dead",
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", sessionInfosFromBeads(sessions), work, []string{""}, nil)

	if len(got) != 0 {
		t.Fatalf("filtered work = %#v, want empty because ambiguous dead-session identity is uncertain, not confirmed-dead demand", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandUsesClosedSessionTemplateFallback(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "primary"}, {Name: "fallback"}}}
	closed := deadAssigneeSessionBead("session-dead", "fallback-dead", "fallback", "closed")
	work := []beads.Bead{{
		ID:       "ga-assignee-only",
		Type:     "task",
		Status:   "open",
		Assignee: "fallback-dead",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", sessionInfosFromBeads([]beads.Bead{closed}), work, []string{""}, nil)

	if len(got) != 1 || got[0].ID != "ga-assignee-only" {
		t.Fatalf("filtered work = %#v, want assignee-only work mapped through the confirmed-dead session template", got)
	}
}

func TestDeadAssigneeDemandMapsAssigneeTemplateBeforeClosedSessionTemplate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}, {Name: "fallback"}}}
	closed := deadAssigneeSessionBead("session-dead", "worker", "fallback", "closed")
	work := []beads.Bead{{
		ID:       "ga-assignee-template-wins",
		Type:     "task",
		Status:   "open",
		Assignee: "worker",
	}}

	states := ComputePoolDesiredStates(cfg, filterAssignedWorkBeadsForPoolDemand(cfg, "", sessionInfosFromBeads([]beads.Bead{closed}), work, []string{""}, nil), sessionInfosFromBeads([]beads.Bead{closed}), nil)
	counts := PoolDesiredCounts(states)

	if got := counts["worker"]; got != 1 {
		t.Fatalf("worker pool desired = %d, want 1 because assignee identity maps to configured template before closed-session template fallback", got)
	}
	if got := counts["fallback"]; got != 0 {
		t.Fatalf("fallback pool desired = %d, want 0 because assignee-template mapping must outrank closed-session template fallback", got)
	}
}

func TestDeadAssigneeDemandPreservesRouteTemplatePrecedence(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "preferred"}, {Name: "fallback"}}}
	closed := deadAssigneeSessionBead("session-dead", "fallback-dead", "fallback", "closed")
	work := []beads.Bead{{
		ID:       "ga-route-wins",
		Type:     "task",
		Status:   "open",
		Assignee: "fallback-dead",
		Metadata: map[string]string{"gc.routed_to": "preferred"},
	}}

	states := ComputePoolDesiredStates(cfg, filterAssignedWorkBeadsForPoolDemand(cfg, "", sessionInfosFromBeads([]beads.Bead{closed}), work, []string{""}, nil), sessionInfosFromBeads([]beads.Bead{closed}), nil)
	counts := PoolDesiredCounts(states)

	if got := counts["preferred"]; got != 1 {
		t.Fatalf("preferred pool desired = %d, want 1 because gc.routed_to outranks the dead session's template", got)
	}
	if got := counts["fallback"]; got != 0 {
		t.Fatalf("fallback pool desired = %d, want 0 because gc.routed_to must win over closed-session template fallback", got)
	}
}

func TestDeadAssigneeDemandHonorsReadyExcludeTypesAndBlockingDependencies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, store *beads.MemStore, template string)
	}{
		{
			name: "graph step excluded by readyExcludeTypes",
			setup: func(t *testing.T, store *beads.MemStore, template string) {
				t.Helper()
				if _, err := store.Create(beads.Bead{
					ID:       "ga-graph-step",
					Title:    "graph.v2 drain step",
					Type:     "step",
					Status:   "open",
					Assignee: "worker-dead",
					Metadata: map[string]string{
						"gc.routed_to":          template,
						"gc.kind":               "workflow",
						"gc.formula_contract":   "graph.v2",
						"gc.workflow_member_id": "drain-unit-member",
					},
				}); err != nil {
					t.Fatalf("create graph step: %v", err)
				}
			},
		},
		{
			name: "blocking dependency excludes ready task",
			setup: func(t *testing.T, store *beads.MemStore, template string) {
				t.Helper()
				blocker, err := store.Create(beads.Bead{ID: "ga-blocker", Title: "blocker", Type: "task", Status: "open"})
				if err != nil {
					t.Fatalf("create blocker: %v", err)
				}
				blocked, err := store.Create(beads.Bead{
					ID:       "ga-blocked",
					Title:    "blocked routed work",
					Type:     "task",
					Status:   "open",
					Assignee: "worker-dead",
					Metadata: map[string]string{"gc.routed_to": template},
				})
				if err != nil {
					t.Fatalf("create blocked work: %v", err)
				}
				if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
					t.Fatalf("add blocking dependency: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := deadAssigneeDemandConfig(1)
			template := cfg.Agents[0].QualifiedName()
			createdClosed, err := store.Create(deadAssigneeSessionBead("session-dead", "worker-dead", template, "closed"))
			if err != nil {
				t.Fatalf("create closed session bead: %v", err)
			}
			if err := store.Close(createdClosed.ID); err != nil {
				t.Fatalf("close session bead: %v", err)
			}
			tc.setup(t, store, template)

			result := buildDesiredStateWithSessionBeads(
				"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
				store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
			)

			if got := result.ScaleCheckCounts[template]; got != 0 {
				t.Fatalf("ScaleCheckCounts[%q] = %d, want 0 for non-actionable dead-assignee work", template, got)
			}
			if len(result.State) != 0 {
				t.Fatalf("desired session count = %d, want 0 for non-actionable dead-assignee work; state=%v", len(result.State), result.State)
			}
		})
	}
}

func deadAssigneeDemandConfig(maxActive int) *config.City {
	minActive := 0
	return &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxActive,
			MinActiveSessions: &minActive,
			Provider:          "mock",
			StartCommand:      "true",
		}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
}

func deadAssigneeSessionBead(id, sessionName, template, status string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  template,
		Type:   sessionBeadType,
		Status: status,
		Labels: []string{sessionBeadLabel, "agent:" + template},
		Metadata: map[string]string{
			"session_name":         sessionName,
			"template":             template,
			"agent_name":           template,
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
		},
	}
}
