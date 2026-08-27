package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

const (
	poolTemplate      = "fixture/build"
	poolSessionName   = "fixture--build-slot-a"
	poolSessionBeadID = "mc-pool-a"
)

func TestAwakeSetReadyPoolTemplateAssignmentWakesEligibleMember(t *testing.T) {
	input := poolTemplateAwakeInput(t, AwakeWorkBead{
		ID:       "work-ready",
		Assignee: poolTemplate,
		Status:   "open",
		Ready:    true,
	})

	got := ComputeAwakeSet(input)
	assertAwake(t, got, poolSessionName)
	assertReason(t, got, poolSessionName, "assigned-work")
	decision := got[poolSessionName]
	if !decision.HasAssignedWork {
		t.Fatal("HasAssignedWork = false, want true for ready work serviceable by the pool")
	}
	if decision.AssignedWorkBeadID != "work-ready" {
		t.Fatalf("AssignedWorkBeadID = %q, want work-ready", decision.AssignedWorkBeadID)
	}
}

func TestAwakeSetPoolTemplateAssignmentRequiresReadyDemand(t *testing.T) {
	tests := []struct {
		name string
		work AwakeWorkBead
	}{
		{
			name: "blocked in progress",
			work: AwakeWorkBead{ID: "work-blocked", Assignee: poolTemplate, Status: "in_progress", Blocked: true},
		},
		{
			name: "dependency gated open",
			work: AwakeWorkBead{ID: "work-dependency", Assignee: poolTemplate, Status: "open", Ready: false},
		},
		{
			name: "deferred open",
			work: AwakeWorkBead{ID: "work-deferred", Assignee: poolTemplate, Status: "open", Ready: false},
		},
		{
			name: "terminal",
			work: AwakeWorkBead{ID: "work-terminal", Assignee: poolTemplate, Status: "closed", Ready: true},
		},
		{
			name: "otherwise not ready",
			work: AwakeWorkBead{ID: "work-not-ready", Assignee: poolTemplate, Status: "open", Ready: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeAwakeSet(poolTemplateAwakeInput(t, tt.work))
			assertAsleep(t, got, poolSessionName)
			if got[poolSessionName].HasAssignedWork {
				t.Fatal("HasAssignedWork = true, want false without ready demand")
			}
		})
	}
}

func TestAwakeSetPoolInProgressOwnershipRemainsConcrete(t *testing.T) {
	t.Run("template is serviceability not ownership", func(t *testing.T) {
		got := ComputeAwakeSet(poolTemplateAwakeInput(t, AwakeWorkBead{
			ID:       "work-template-owned",
			Assignee: poolTemplate,
			Status:   "in_progress",
		}))
		assertAsleep(t, got, poolSessionName)
	})

	t.Run("concrete session name owns claim", func(t *testing.T) {
		got := ComputeAwakeSet(poolTemplateAwakeInput(t, AwakeWorkBead{
			ID:       "work-concrete-owner",
			Assignee: poolSessionName,
			Status:   "in_progress",
		}))
		assertAwake(t, got, poolSessionName)
		assertReason(t, got, poolSessionName, "assigned-work")
	})
}

func TestAwakeSetPoolTemplateServiceabilityRequiresConfiguredMembership(t *testing.T) {
	tests := []struct {
		name               string
		independentlyAwake bool
		session            func(t *testing.T) AwakeSessionBead
	}{
		{
			name: "ordinary non-pool session",
			session: func(t *testing.T) AwakeSessionBead {
				t.Helper()
				return poolAwakeSession()
			},
		},
		{
			name: "configured named session",
			session: func(t *testing.T) AwakeSessionBead {
				t.Helper()
				bead := poolAwakeSession()
				bead.SessionName = "fixture--review"
				bead.NamedIdentity = "fixture/review"
				bead.ConfiguredNamedSession = true
				return bead
			},
		},
		{
			name:               "manual session",
			independentlyAwake: true,
			session: func(t *testing.T) AwakeSessionBead {
				t.Helper()
				bead := poolAwakeSession()
				bead.SessionName = "fixture--manual"
				bead.ManualSession = true
				return bead
			},
		},
		{
			name: "numeric suffix lookalike",
			session: func(t *testing.T) AwakeSessionBead {
				t.Helper()
				bead := poolAwakeSession()
				bead.SessionName = "fixture--build-7"
				return bead
			},
		},
		{
			name: "member of another pool",
			session: func(t *testing.T) AwakeSessionBead {
				t.Helper()
				return AwakeSessionBead{
					ID:          "mc-other-pool",
					SessionName: "fixture--verify-slot-a",
					Template:    "fixture/verify",
					State:       "asleep",
					PoolManaged: true,
				}
			},
		},
		{
			name: "drained pool member",
			session: func(t *testing.T) AwakeSessionBead {
				t.Helper()
				bead := poolManagedAwakeSession()
				bead.SessionName = "fixture--build-slot-drained"
				bead.Drained = true
				return bead
			},
		},
		{
			name: "dependency-only pool member",
			session: func(t *testing.T) AwakeSessionBead {
				t.Helper()
				bead := poolManagedAwakeSession()
				bead.SessionName = "fixture--build-slot-depfloor"
				bead.DependencyOnly = true
				return bead
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bead := tt.session(t)
			got := ComputeAwakeSet(AwakeInput{
				Agents: []AwakeAgent{
					{QualifiedName: poolTemplate},
					{QualifiedName: "fixture/verify"},
				},
				SessionBeads: []AwakeSessionBead{bead},
				WorkBeads: []AwakeWorkBead{{
					ID:       "work-ready",
					Assignee: poolTemplate,
					Status:   "open",
					Ready:    true,
				}},
				Now: now,
			})
			decision := got[bead.SessionName]
			if decision.HasAssignedWork || decision.Reason == "assigned-work" {
				t.Fatalf("decision = %+v, want no match for another session's pool-template assignment", decision)
			}
			if tt.independentlyAwake {
				assertReason(t, got, bead.SessionName, "manual")
				return
			}
			assertAsleep(t, got, bead.SessionName)
		})
	}
}

func TestBuildAwakeInputFromReconcilerCarriesConfiguredPoolMembership(t *testing.T) {
	clk := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:     poolSessionBeadID,
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"state":          "stopped",
			"session_name":   poolSessionName,
			"template":       poolTemplate,
			"pool_managed":   "true",
			"session_origin": "pool",
		},
	})
	if !info.PoolManaged {
		t.Fatal("session.Info PoolManaged = false, test fixture did not project pool_managed metadata")
	}

	input := buildAwakeInputFromReconciler(
		&config.City{Agents: []config.Agent{{Name: "build", Dir: "fixture"}}},
		"",
		[]session.Info{info},
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
		clk,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if !input.SessionBeads[0].PoolManaged {
		t.Fatal("AwakeSessionBead pool membership = false, want typed session.Info membership preserved")
	}
}

func poolTemplateAwakeInput(t *testing.T, work AwakeWorkBead) AwakeInput {
	t.Helper()
	return AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: poolTemplate}},
		SessionBeads: []AwakeSessionBead{poolManagedAwakeSession()},
		WorkBeads:    []AwakeWorkBead{work},
		Now:          now,
	}
}

func poolAwakeSession() AwakeSessionBead {
	return AwakeSessionBead{
		ID:          poolSessionBeadID,
		SessionName: poolSessionName,
		Template:    poolTemplate,
		State:       "asleep",
	}
}

// poolManagedAwakeSession returns the pool fixture with configured pool
// membership set, as the reconciler bridge projects it from session.Info.
func poolManagedAwakeSession() AwakeSessionBead {
	bead := poolAwakeSession()
	bead.PoolManaged = true
	return bead
}
