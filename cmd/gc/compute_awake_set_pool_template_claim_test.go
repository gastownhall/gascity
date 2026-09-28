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

// poolManagedAwakeSessionMembers returns a multi-member pool fixture: three
// distinct configured members of the same template, as the reconciler bridge
// projects a pool with three live slots.
func poolManagedAwakeSessionMembers() []AwakeSessionBead {
	return []AwakeSessionBead{
		{ID: "mc-pool-a", SessionName: "fixture--build-slot-a", Template: poolTemplate, State: "asleep", PoolManaged: true},
		{ID: "mc-pool-b", SessionName: "fixture--build-slot-b", Template: poolTemplate, State: "asleep", PoolManaged: true},
		{ID: "mc-pool-c", SessionName: "fixture--build-slot-c", Template: poolTemplate, State: "asleep", PoolManaged: true},
	}
}

// TestAwakeSetPoolTemplateAssignmentWakesAllEligibleMembers pins the fan-out
// shape: a bare template assignment is serviceability, not ownership, so every
// eligible member of that pool is a candidate claimer and wakes. Unlike the
// WorkSet pass, which deliberately wakes exactly one session, the assigned-work
// pass applies no cap — the bead's own claim is what resolves the race.
func TestAwakeSetPoolTemplateAssignmentWakesAllEligibleMembers(t *testing.T) {
	members := poolManagedAwakeSessionMembers()
	got := ComputeAwakeSet(AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: poolTemplate}},
		SessionBeads: members,
		WorkBeads: []AwakeWorkBead{{
			ID:       "work-ready",
			Assignee: poolTemplate,
			Status:   "open",
			Ready:    true,
		}},
		Now: now,
	})

	for _, member := range members {
		t.Run(member.SessionName, func(t *testing.T) {
			assertAwake(t, got, member.SessionName)
			assertReason(t, got, member.SessionName, "assigned-work")
			decision := got[member.SessionName]
			if !decision.HasAssignedWork {
				t.Error("HasAssignedWork = false, want true for work serviceable by this member")
			}
			if decision.AssignedWorkBeadID != "work-ready" {
				t.Errorf("AssignedWorkBeadID = %q, want work-ready", decision.AssignedWorkBeadID)
			}
		})
	}
}

// TestAwakeSetPoolTemplateAssignmentFillsScaleSlotPerMember documents the
// countAssignedScaleSlots interaction. That counter asks sessionHasAssignedWork
// per member, so a single template-assigned ready bead now reports a filled
// slot for every member of the pool. The members reach the same awake state
// through the assigned-work pass either way, so the observable contract is that
// scale accounting never downgrades a serviceable member to scaled:demand.
func TestAwakeSetPoolTemplateAssignmentFillsScaleSlotPerMember(t *testing.T) {
	members := poolManagedAwakeSessionMembers()
	got := ComputeAwakeSet(AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: poolTemplate}},
		SessionBeads: members,
		WorkBeads: []AwakeWorkBead{{
			ID:       "work-ready",
			Assignee: poolTemplate,
			Status:   "open",
			Ready:    true,
		}},
		ScaleCheckCounts: map[string]int{poolTemplate: 2},
		Now:              now,
	})

	for _, member := range members {
		t.Run(member.SessionName, func(t *testing.T) {
			assertAwake(t, got, member.SessionName)
			assertReason(t, got, member.SessionName, "assigned-work")
		})
	}
	for sessionName, decision := range got {
		if decision.Reason == "scaled:demand" {
			t.Errorf("session %q reason = scaled:demand, want serviceable members to wake as assigned-work", sessionName)
		}
	}
}

// TestAwakeSetPoolTemplateClaimCollapsesFanOutToHolder pins the post-claim
// state. While the work is open the bare template wakes every eligible member
// (the fan-out above), but once one member claims the bead the assignee is that
// member's concrete identity and the status is in_progress — so the
// serviceability branch no longer applies and only the holder sees assigned
// work. The members that lost the claim race must report no assigned work
// rather than staying awake on a bead they do not own.
func TestAwakeSetPoolTemplateClaimCollapsesFanOutToHolder(t *testing.T) {
	members := poolManagedAwakeSessionMembers()
	const holder = "fixture--build-slot-a"
	got := ComputeAwakeSet(AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: poolTemplate}},
		SessionBeads: members,
		WorkBeads: []AwakeWorkBead{{
			ID:       "work-claimed",
			Assignee: holder,
			Status:   "in_progress",
		}},
		Now: now,
	})

	assertAwake(t, got, holder)
	assertReason(t, got, holder, "assigned-work")
	if decision := got[holder]; !decision.HasAssignedWork {
		t.Error("holder HasAssignedWork = false, want true for the claimed bead")
	}

	for _, member := range members {
		if member.SessionName == holder {
			continue
		}
		t.Run(member.SessionName, func(t *testing.T) {
			decision := got[member.SessionName]
			if decision.HasAssignedWork {
				t.Errorf("HasAssignedWork = true, want false after %q claimed the bead", holder)
			}
			if decision.AssignedWorkBeadID != "" {
				t.Errorf("AssignedWorkBeadID = %q, want empty for a member that lost the claim", decision.AssignedWorkBeadID)
			}
			assertAsleep(t, got, member.SessionName)
		})
	}
}
