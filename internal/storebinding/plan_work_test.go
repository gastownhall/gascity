package storebinding

import (
	"errors"
	"testing"
)

func TestWorkTopologyPlanPinsScopesOrdersAndSuspension(t *testing.T) {
	plan, err := newWorkTopologyPlan(planWorkPins())
	if err != nil {
		t.Fatalf("newWorkTopologyPlan: %v", err)
	}

	if !plan.Present() || !plan.Recorded() {
		t.Fatalf("plan present=%v recorded=%v, want a recorded pinned topology", plan.Present(), plan.Recorded())
	}
	if plan.HQ().Scope != HQScope() || plan.HQ().ConfigRank != 0 {
		t.Fatalf("HQ pin = %#v", plan.HQ())
	}
	config := plan.RigsInConfigOrder()
	if len(config) != 2 || config[0].Scope != RigScope("beta") || config[1].Scope != RigScope("alpha") {
		t.Fatalf("config order = %#v, want the configured rig order", config)
	}
	if config[0].ConfigRank != 1 || config[1].ConfigRank != 2 {
		t.Fatalf("config ranks = %d,%d, want configuration position", config[0].ConfigRank, config[1].ConfigRank)
	}
	lexical := plan.RigsInLexicalOrder()
	if lexical[0].Scope != RigScope("alpha") || lexical[1].Scope != RigScope("beta") {
		t.Fatalf("lexical order = %#v", lexical)
	}
	all := plan.All()
	if len(all) != 3 || all[0].Scope != HQScope() || all[1].Scope != RigScope("alpha") || all[2].Scope != RigScope("beta") {
		t.Fatalf("All() = %#v, want HQ then lexical rigs", all)
	}
	// Every configured rig is represented, suspended ones included, carrying
	// their suspension flag so consumers keep their own inclusion rules.
	suspended, err := plan.ForScope(RigScope("beta"))
	if err != nil || !suspended.Suspended {
		t.Fatalf("ForScope(beta) = %#v, %v; want the suspended rig retained", suspended, err)
	}
	if _, err := plan.ForScope(RigScope("missing")); !errors.Is(err, ErrWorkScopeNotFound) {
		t.Fatalf("ForScope(missing) error = %v, want the typed not-found error", err)
	}
}

func TestWorkTopologyPlanGroupsUnifiedScopesOncePerPhysicalIdentity(t *testing.T) {
	scoped, err := newWorkTopologyPlan(planWorkPins())
	if err != nil {
		t.Fatalf("newWorkTopologyPlan: %v", err)
	}
	if len(scoped.Physical()) != 3 {
		t.Fatalf("scoped physical groups = %d, want one per distinct workspace", len(scoped.Physical()))
	}

	unified, err := newWorkTopologyPlan(planUnifiedWorkPins())
	if err != nil {
		t.Fatalf("newWorkTopologyPlan(unified): %v", err)
	}
	groups := unified.Physical()
	if len(groups) != 1 {
		t.Fatalf("unified physical groups = %d, want one shared workspace", len(groups))
	}
	if len(groups[0].Scopes) != 3 {
		t.Fatalf("unified group scopes = %#v, want every semantic scope retained", groups[0].Scopes)
	}
	if groups[0].Scopes[0] != HQScope() || groups[0].Scopes[1] != RigScope("beta") {
		t.Fatalf("unified group order = %#v, want HQ then config order", groups[0].Scopes)
	}

	participants, err := unified.Participants()
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(participants) != 1 || len(participants[0].Members) != 3 {
		t.Fatalf("unified participants = %#v, want one physical workspace with three members", participants)
	}
	if participants[0].PhysicalIdentity != PhysicalIdentity("city-physical") {
		t.Fatalf("participant identity = %q", participants[0].PhysicalIdentity)
	}
	for _, member := range participants[0].Members {
		if member.ConfigContext != unified.ConfigContext() {
			t.Fatalf("member %s config context = %q, want the pinned context", member.Scope, member.ConfigContext)
		}
	}

	// A component identity shared by two openers must not collapse: the whole
	// identity triple is the deduplication key.
	pins := planUnifiedWorkPins()
	pins.Rigs[0].OpenerID = "other-opener"
	distinct, err := newWorkTopologyPlan(pins)
	if err != nil {
		t.Fatalf("newWorkTopologyPlan(distinct opener): %v", err)
	}
	if len(distinct.Physical()) != 2 {
		t.Fatalf("physical groups = %d, want distinct openers to stay distinct", len(distinct.Physical()))
	}
}

// TestWorkTopologyPlanSelectsTheSameScopesAsTheShippedTopology proves the
// pinned prefix rule is the shipped rule, not a second implementation of it.
func TestWorkTopologyPlanSelectsTheSameScopesAsTheShippedTopology(t *testing.T) {
	pins := planWorkPins()
	plan, err := newWorkTopologyPlan(pins)
	if err != nil {
		t.Fatalf("newWorkTopologyPlan: %v", err)
	}
	topology := planWorkTopology(t, pins)

	for _, id := range []string{"hq-1", "alpha-42", "beta-7"} {
		planScope, matched, err := plan.PrefixScopeForID(id)
		if err != nil || !matched {
			t.Fatalf("PrefixScopeForID(%q) = %v, %v, %v", id, planScope, matched, err)
		}
		liveScope, err := topology.ScopeForID(id)
		if err != nil {
			t.Fatalf("ScopeForID(%q): %v", id, err)
		}
		if planScope != liveScope {
			t.Fatalf("plan selected %v for %q while the topology selected %v", planScope, id, liveScope)
		}
	}
	// An unprefixed ID needs the residence probe, which requires live stores the
	// plan deliberately does not hold.
	if scope, matched, err := plan.PrefixScopeForID("unknown-1"); matched || err != nil {
		t.Fatalf("PrefixScopeForID(unknown-1) = %v, %v, %v; want no pinned match", scope, matched, err)
	}
}

func TestWorkTopologyPlanRejectsAliasedIncompleteAndDriftedPins(t *testing.T) {
	valid := planWorkPins()
	cases := []struct {
		name   string
		mutate func(*WorkPinInputs)
		want   error
	}{
		{
			name:   "missing HQ scope",
			mutate: func(in *WorkPinInputs) { in.HQ.Scope = RigScope("hq") },
			want:   ErrInvalidWorkPin,
		},
		{
			name:   "second HQ scope",
			mutate: func(in *WorkPinInputs) { in.Rigs[0].Scope = HQScope() },
			want:   ErrInvalidWorkPin,
		},
		{
			name:   "duplicate rig scope",
			mutate: func(in *WorkPinInputs) { in.Rigs[0].Scope = in.Rigs[1].Scope },
			want:   ErrInvalidWorkPin,
		},
		{
			name:   "duplicate prefix",
			mutate: func(in *WorkPinInputs) { in.Rigs[0].Prefix = in.HQ.Prefix },
			want:   ErrInvalidWorkPin,
		},
		{
			name:   "nested prefix cannot select one scope",
			mutate: func(in *WorkPinInputs) { in.Rigs[0].Prefix = in.HQ.Prefix + "-inner" },
			want:   ErrInvalidWorkPin,
		},
		{
			name:   "missing physical identity",
			mutate: func(in *WorkPinInputs) { in.Rigs[0].PhysicalID = "" },
			want:   ErrInvalidWorkPin,
		},
		{
			name:   "missing opener",
			mutate: func(in *WorkPinInputs) { in.HQ.OpenerID = "" },
			want:   ErrInvalidWorkPin,
		},
		{
			name:   "non-canonical config context",
			mutate: func(in *WorkPinInputs) { in.ConfigContext = "work-config" },
			want:   ErrInvalidWorkPin,
		},
		{
			name: "observed identity drift",
			mutate: func(in *WorkPinInputs) {
				observed := append([]WorkScopePin{in.HQ}, in.Rigs...)
				observed[2].PhysicalID = "relocated-physical"
				in.Observed = observed
			},
			want: ErrWorkPinDrift,
		},
		{
			name: "observed scope census drift",
			mutate: func(in *WorkPinInputs) {
				in.Observed = []WorkScopePin{in.HQ}
			},
			want: ErrWorkPinDrift,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pins := valid
			pins.Rigs = append([]WorkScopePin(nil), valid.Rigs...)
			testCase.mutate(&pins)
			plan, err := newWorkTopologyPlan(pins)
			if plan.Present() {
				t.Fatalf("newWorkTopologyPlan returned a partial plan: %#v", plan)
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("newWorkTopologyPlan error = %v, want %v", err, testCase.want)
			}
		})
	}
}

// TestWorkTopologyPlanAcceptsAgreeingObservedMetadata pins the other half of
// drift detection: agreeing observations must not be rejected, or the drift
// test would pass for the wrong reason.
func TestWorkTopologyPlanAcceptsAgreeingObservedMetadata(t *testing.T) {
	pins := planWorkPins()
	pins.Observed = append([]WorkScopePin{pins.HQ}, pins.Rigs...)
	if _, err := newWorkTopologyPlan(pins); err != nil {
		t.Fatalf("newWorkTopologyPlan with agreeing observations: %v", err)
	}
}

func TestWorkTopologyPlanFactCheckBlocksOpenedTopologyDrift(t *testing.T) {
	pins := planWorkPins()
	plan, err := newWorkTopologyPlan(pins)
	if err != nil {
		t.Fatalf("newWorkTopologyPlan: %v", err)
	}
	if err := plan.checkTopology(planWorkTopology(t, pins), true); err != nil {
		t.Fatalf("matching topology rejected: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*WorkPinInputs)
		identity bool
	}{
		{name: "prefix", mutate: func(in *WorkPinInputs) { in.Rigs[0].Prefix = "renamed" }, identity: true},
		{name: "suspension", mutate: func(in *WorkPinInputs) { in.Rigs[0].Suspended = !in.Rigs[0].Suspended }, identity: true},
		{name: "physical identity", mutate: func(in *WorkPinInputs) { in.HQ.PhysicalID = "relocated" }, identity: true},
		{name: "opener", mutate: func(in *WorkPinInputs) { in.HQ.OpenerID = "other" }, identity: true},
		{name: "scope census", mutate: func(in *WorkPinInputs) { in.Rigs = in.Rigs[:1] }, identity: true},
		{name: "config order", mutate: func(in *WorkPinInputs) { in.Rigs[0], in.Rigs[1] = in.Rigs[1], in.Rigs[0] }, identity: true},
		{name: "prefix under a moved work class", mutate: func(in *WorkPinInputs) { in.Rigs[0].Prefix = "renamed" }, identity: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			drifted := planWorkPins()
			drifted.Rigs = append([]WorkScopePin(nil), planWorkPins().Rigs...)
			testCase.mutate(&drifted)
			if err := plan.checkTopology(planWorkTopology(t, drifted), testCase.identity); !errors.Is(err, ErrWorkTopologyDrift) {
				t.Fatalf("checkTopology drift error = %v, want %v", err, ErrWorkTopologyDrift)
			}
		})
	}

	// A moved Work class is served by another provider's identities while every
	// semantic fact survives, so identity comparison is scoped to the reserved
	// binding.
	moved := planWorkPins()
	moved.HQ.OpenerID = "task-beads"
	moved.HQ.PhysicalID = "postgres-hq"
	moved.Rigs = append([]WorkScopePin(nil), planWorkPins().Rigs...)
	for index := range moved.Rigs {
		moved.Rigs[index].OpenerID = "task-beads"
		moved.Rigs[index].PhysicalID = "postgres-" + moved.Rigs[index].Prefix
	}
	movedTopology := planWorkTopology(t, moved)
	if err := plan.checkTopology(movedTopology, false); err != nil {
		t.Fatalf("moved Work topology rejected on semantic facts: %v", err)
	}
	if err := plan.checkTopology(movedTopology, true); !errors.Is(err, ErrWorkTopologyDrift) {
		t.Fatal("reserved-binding fact check accepted foreign pinned identities")
	}
}

func TestWorkTopologyPlanAccessorsReturnDetachedValues(t *testing.T) {
	plan, err := newWorkTopologyPlan(planUnifiedWorkPins())
	if err != nil {
		t.Fatalf("newWorkTopologyPlan: %v", err)
	}
	groups := plan.Physical()
	groups[0].Scopes[0] = RigScope("tampered")
	if plan.Physical()[0].Scopes[0] == RigScope("tampered") {
		t.Fatal("Physical returned the plan's own groups")
	}
	rigs := plan.RigsInConfigOrder()
	rigs[0].Prefix = "tampered"
	if plan.RigsInConfigOrder()[0].Prefix == "tampered" {
		t.Fatal("RigsInConfigOrder returned the plan's own pins")
	}
}
