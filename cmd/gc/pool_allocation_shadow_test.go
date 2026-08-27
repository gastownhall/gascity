package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPoolAllocationShadowPolicyClassifiesPoolShapes(t *testing.T) {
	base := func() (*config.City, *config.Agent) {
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "rig"}},
			Agents: []config.Agent{{
				Name: "worker",
				Dir:  "rig",
			}},
		}
		return cfg, &cfg.Agents[0]
	}

	tests := []struct {
		name   string
		mutate func(*config.City, *config.Agent) map[string]struct{}
		want   poolAllocationShadowReason
	}{
		{name: "default multi-session pool", want: poolAllocationShadowEligible},
		{name: "custom scale check", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.ScaleCheck = "printf 1"
			return nil
		}, want: poolAllocationShadowCustomScaleCheck},
		{name: "dependency-bearing template", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.DependsOn = []string{"database"}
			return nil
		}, want: poolAllocationShadowDependencies},
		{name: "named session binding", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			return map[string]struct{}{agent.QualifiedName(): {}}
		}, want: poolAllocationShadowNamedSession},
		{name: "minimum floor", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			minimum := 1
			agent.MinActiveSessions = &minimum
			return nil
		}, want: poolAllocationShadowMinFloor},
		{name: "workspace cap", mutate: func(cfg *config.City, _ *config.Agent) map[string]struct{} {
			maximum := 10
			cfg.Workspace.MaxActiveSessions = &maximum
			return nil
		}, want: poolAllocationShadowWorkspaceCap},
		{name: "rig cap", mutate: func(cfg *config.City, _ *config.Agent) map[string]struct{} {
			maximum := 10
			cfg.Rigs[0].MaxActiveSessions = &maximum
			return nil
		}, want: poolAllocationShadowRigCap},
		{name: "namepool", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.Namepool = "names.txt"
			return nil
		}, want: poolAllocationShadowNamepool},
		{name: "loaded namepool", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.NamepoolNames = []string{"one"}
			return nil
		}, want: poolAllocationShadowNamepool},
		{name: "canonical singleton", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			maximum := 1
			agent.MaxActiveSessions = &maximum
			return nil
		}, want: poolAllocationShadowEligibleAgentCap},
		{name: "bounded agent cap", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			maximum := 2
			agent.MaxActiveSessions = &maximum
			return nil
		}, want: poolAllocationShadowEligibleAgentCap},
		{name: "disabled", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			maximum := 0
			agent.MaxActiveSessions = &maximum
			return nil
		}, want: poolAllocationShadowDisabled},
		{name: "suspended", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.Suspended = true
			return nil
		}, want: poolAllocationShadowSuspended},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, agent := base()
			var namedTemplates map[string]struct{}
			if test.mutate != nil {
				namedTemplates = test.mutate(cfg, agent)
			}
			policy := newPoolAllocationShadowPolicy(cfg, agent, namedTemplates)
			if policy.reason != test.want {
				t.Fatalf("policy reason = %q, want %q", policy.reason, test.want)
			}
			wantSupported := test.want == poolAllocationShadowEligible || test.want == poolAllocationShadowEligibleAgentCap
			if got := policy.supported(); got != wantSupported {
				t.Fatalf("policy supported = %t for reason %q", got, policy.reason)
			}
		})
	}
}

// TestPoolAllocationShadowPolicyCapacityIsTheOnlyCapSpelling holds the
// biconditional the uniform predicate contract's capacity clause rests on
// (poolAllocationShadowPolicy's type doc): under supported(), reason ==
// EligibleAgentCap if and only if maxActiveSessions >= 0, and reason ==
// Eligible if and only if maxActiveSessions == -1.
//
// That is what makes `policy.maxActiveSessions == 1` behavior-identical to the
// `reason == EligibleAgentCap && maxActiveSessions == 1` conjunction the
// pool-family sites used to carry, and it is a property of the CONSTRUCTOR --
// maxActiveSessions is assigned on exactly one branch -- not of the sites. If a
// later reason ever carries a cap of its own, or the Eligible branch ever
// stops meaning "unlimited", this fails here rather than silently widening
// every capacity clause in the fleet.
func TestPoolAllocationShadowPolicyCapacityIsTheOnlyCapSpelling(t *testing.T) {
	newCap := func(n int) *int { return &n }
	for _, test := range []struct {
		name    string
		mutate  func(*config.City, *config.Agent) map[string]struct{}
		wantCap int
	}{
		{name: "unlimited pool", wantCap: -1},
		{name: "explicit unlimited", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.MaxActiveSessions = newCap(-1)
			return nil
		}, wantCap: -1},
		{name: "canonical singleton", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.MaxActiveSessions = newCap(1)
			return nil
		}, wantCap: 1},
		{name: "bounded agent cap", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.MaxActiveSessions = newCap(4)
			return nil
		}, wantCap: 4},
		{name: "ineligible carries no cap", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.MaxActiveSessions = newCap(4)
			agent.DependsOn = []string{"database"}
			return nil
		}, wantCap: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs:      []config.Rig{{Name: "rig"}},
				Agents:    []config.Agent{{Name: "worker", Dir: "rig"}},
			}
			var namedTemplates map[string]struct{}
			if test.mutate != nil {
				namedTemplates = test.mutate(cfg, &cfg.Agents[0])
			}
			policy := newPoolAllocationShadowPolicy(cfg, &cfg.Agents[0], namedTemplates)
			if policy.maxActiveSessions != test.wantCap {
				t.Fatalf("maxActiveSessions = %d, want %d (policy %+v)", policy.maxActiveSessions, test.wantCap, policy)
			}
			if !policy.supported() {
				if policy.maxActiveSessions != -1 {
					t.Fatalf("ineligible policy %+v carries a cap; capacity clauses would read it", policy)
				}
				return
			}
			if hasCap := policy.maxActiveSessions >= 0; hasCap != (policy.reason == poolAllocationShadowEligibleAgentCap) {
				t.Fatalf("policy %+v breaks the capacity biconditional: a cap is present = %t but reason = %q", policy, hasCap, policy.reason)
			}
			if policy.reason == poolAllocationShadowEligible && policy.maxActiveSessions != -1 {
				t.Fatalf("policy %+v: reason Eligible must mean unlimited", policy)
			}
		})
	}
}

func TestPoolAllocationShadowPolicyRejectsUnreachableSourceStore(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: "rig"}},
		Agents:    []config.Agent{{Name: "worker", Dir: "rig"}},
	}
	policy := newPoolAllocationShadowPolicy(cfg, &cfg.Agents[0], nil)

	if got := policy.forSourceStore(cfg, &cfg.Agents[0], "/city", "rig:rig"); !got.supported() {
		t.Fatalf("reachable source policy = %+v, want supported", got)
	}
	if got := policy.forSourceStore(cfg, &cfg.Agents[0], "/city", "city:test-city"); got.reason != poolAllocationShadowSourceStore {
		t.Fatalf("unreachable source policy = %+v, want %q", got, poolAllocationShadowSourceStore)
	}
}

func TestDecideRoutedWorkPoolAllocationShadow(t *testing.T) {
	supported := poolAllocationShadowPolicy{reason: poolAllocationShadowEligible}
	baseContribution := readyRoutedWorkDemandContribution{
		WorkID:              "ga-ready",
		PoolTarget:          "rig/worker",
		ContributionPresent: true,
		AllocationPolicy:    supported,
	}
	baseMembership := poolMembershipObservation{certified: true, revision: 7}

	tests := []struct {
		name         string
		contribution readyRoutedWorkDemandContribution
		membership   poolMembershipObservation
		wantAction   poolAllocationShadowAction
		wantReason   poolAllocationShadowReason
		wantStarts   int
	}{
		{
			name:         "certified cold pool starts one",
			contribution: baseContribution,
			membership:   baseMembership,
			wantAction:   poolAllocationShadowStartOne,
			wantReason:   poolAllocationShadowColdFromZero,
			wantStarts:   1,
		},
		{
			name: "unsupported demand remains legacy owned",
			contribution: func() readyRoutedWorkDemandContribution {
				value := baseContribution
				value.ContributionPresent = false
				value.AllocationPolicy.reason = poolAllocationShadowCustomScaleCheck
				return value
			}(),
			membership: baseMembership,
			wantAction: poolAllocationShadowLegacy,
			wantReason: poolAllocationShadowCustomScaleCheck,
		},
		{
			name:         "uncertified membership remains legacy owned",
			contribution: baseContribution,
			membership: poolMembershipObservation{
				revision: 8,
				reason:   poolMembershipUncertifiedSnapshotGap,
			},
			wantAction: poolAllocationShadowLegacy,
			wantReason: poolAllocationShadowMembershipUncertified,
		},
		{
			name:         "asleep reusable member remains legacy owned",
			contribution: baseContribution,
			membership:   poolMembershipObservation{members: 1, certified: true, revision: 9},
			wantAction:   poolAllocationShadowLegacy,
			wantReason:   poolAllocationShadowNonemptyPool,
		},
		{
			name:         "occupied member remains legacy owned",
			contribution: baseContribution,
			membership:   poolMembershipObservation{members: 1, occupied: 1, nextFreeSlot: 2, certified: true, revision: 10},
			wantAction:   poolAllocationShadowStartOne,
			wantReason:   poolAllocationShadowOccupiedGrowth,
			wantStarts:   1,
		},
		{
			name: "cold canonical singleton starts one",
			contribution: func() readyRoutedWorkDemandContribution {
				value := baseContribution
				value.AllocationPolicy = poolAllocationShadowPolicy{
					reason:            poolAllocationShadowEligibleAgentCap,
					maxActiveSessions: 1,
				}
				return value
			}(),
			membership: baseMembership,
			wantAction: poolAllocationShadowStartOne,
			wantReason: poolAllocationShadowColdFromZero,
			wantStarts: 1,
		},
		{
			name: "occupied canonical singleton remains legacy owned",
			contribution: func() readyRoutedWorkDemandContribution {
				value := baseContribution
				value.AllocationPolicy = poolAllocationShadowPolicy{
					reason:            poolAllocationShadowEligibleAgentCap,
					maxActiveSessions: 1,
				}
				return value
			}(),
			membership: poolMembershipObservation{members: 1, occupied: 1, nextFreeSlot: 1, certified: true, revision: 10},
			wantAction: poolAllocationShadowLegacy,
			wantReason: poolAllocationShadowAgentCap,
		},
		{
			name: "bounded pool below cap starts one",
			contribution: func() readyRoutedWorkDemandContribution {
				value := baseContribution
				value.AllocationPolicy = poolAllocationShadowPolicy{
					reason:            poolAllocationShadowEligibleAgentCap,
					maxActiveSessions: 2,
				}
				return value
			}(),
			membership: poolMembershipObservation{members: 1, occupied: 1, nextFreeSlot: 2, certified: true, revision: 10},
			wantAction: poolAllocationShadowStartOne,
			wantReason: poolAllocationShadowOccupiedGrowth,
			wantStarts: 1,
		},
		{
			name: "bounded pool at cap remains legacy owned",
			contribution: func() readyRoutedWorkDemandContribution {
				value := baseContribution
				value.AllocationPolicy = poolAllocationShadowPolicy{
					reason:            poolAllocationShadowEligibleAgentCap,
					maxActiveSessions: 2,
				}
				return value
			}(),
			membership: poolMembershipObservation{members: 2, occupied: 2, nextFreeSlot: 3, certified: true, revision: 10},
			wantAction: poolAllocationShadowLegacy,
			wantReason: poolAllocationShadowAgentCap,
		},
		{
			name:         "mixed occupied and asleep members remain legacy owned",
			contribution: baseContribution,
			membership:   poolMembershipObservation{members: 2, occupied: 1, nextFreeSlot: 3, certified: true, revision: 10},
			wantAction:   poolAllocationShadowLegacy,
			wantReason:   poolAllocationShadowNonemptyPool,
		},
		{
			name:         "occupied members without a certified free slot remain legacy owned",
			contribution: baseContribution,
			membership:   poolMembershipObservation{members: 1, occupied: 1, certified: true, revision: 10},
			wantAction:   poolAllocationShadowLegacy,
			wantReason:   poolAllocationShadowInvalidMembership,
		},
		{
			name:         "impossible membership fails closed",
			contribution: baseContribution,
			membership:   poolMembershipObservation{occupied: 1, certified: true, revision: 11},
			wantAction:   poolAllocationShadowLegacy,
			wantReason:   poolAllocationShadowInvalidMembership,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideRoutedWorkPoolAllocationShadow(test.contribution, test.membership)
			if got.action != test.wantAction || got.reason != test.wantReason || got.startCount != test.wantStarts {
				t.Fatalf("decision = %+v, want action=%q reason=%q starts=%d", got, test.wantAction, test.wantReason, test.wantStarts)
			}
			if got.workID != test.contribution.WorkID || got.poolTarget != test.contribution.PoolTarget || got.membershipRevision != test.membership.revision {
				t.Fatalf("decision provenance = %+v, want contribution and membership revision retained", got)
			}
		})
	}
}

func TestColdPoolAllocationShadowMatchesLegacyMinimumAction(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	policy := newPoolAllocationShadowPolicy(cfg, &cfg.Agents[0], nil)
	decision := decideRoutedWorkPoolAllocationShadow(readyRoutedWorkDemandContribution{
		WorkID:              "ga-ready",
		PoolTarget:          "worker",
		ContributionPresent: true,
		AllocationPolicy:    policy,
	}, poolMembershipObservation{certified: true})

	legacy := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{"worker": 1})
	if len(legacy) != 1 || len(legacy[0].Requests) != decision.startCount || decision.action != poolAllocationShadowStartOne {
		t.Fatalf("legacy=%+v shadow=%+v, want the same one cold start", legacy, decision)
	}
}

func BenchmarkRoutedWorkPoolAllocationShadowFleetSize(b *testing.B) {
	for _, size := range []int{1, 1_000, 10_000} {
		b.Run(fmt.Sprintf("fleet-%d", size), func(b *testing.B) {
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs:      []config.Rig{{Name: "rig", Path: "rig"}},
				Agents:    make([]config.Agent, size),
			}
			for i := range cfg.Agents {
				cfg.Agents[i] = config.Agent{Name: fmt.Sprintf("unrelated-%d", i)}
			}
			cfg.Agents[size-1] = config.Agent{Name: "worker", Dir: "rig"}
			target := &cfg.Agents[size-1]
			membership := poolMembershipObservation{certified: true, revision: 7}
			var decision poolAllocationShadowDecision

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				policy := newPoolAllocationShadowPolicy(cfg, target, nil).
					forSourceStore(cfg, target, "/city", "rig:rig")
				decision = decideRoutedWorkPoolAllocationShadow(readyRoutedWorkDemandContribution{
					WorkID:              "ga-ready",
					PoolTarget:          "rig/worker",
					ContributionPresent: policy.contributionPresent,
					AllocationPolicy:    policy,
				}, membership)
			}
			if decision.action != poolAllocationShadowStartOne {
				b.Fatalf("decision = %+v, want start one", decision)
			}
		})
	}
}
