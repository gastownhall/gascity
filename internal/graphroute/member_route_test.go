package graphroute

import (
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// TestIsExpandedWorkflowRoot pins the shape member routing responds to: the
// same predicate the demand-side guard uses to drop an expanded root.
func TestIsExpandedWorkflowRoot(t *testing.T) {
	expanded := map[string]string{
		beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
		beadmeta.WorkflowExpandedMetadataKey: "true",
	}
	if !IsExpandedWorkflowRoot(expanded) {
		t.Error("expanded workflow root not recognized")
	}
	for name, meta := range map[string]map[string]string{
		"root-only workflow": {beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
		"plain task":         {},
		"scope latch":        {beadmeta.KindMetadataKey: beadmeta.KindScope},
		"nil":                nil,
	} {
		if IsExpandedWorkflowRoot(meta) {
			t.Errorf("%s: recognized as an expanded workflow root, want false", name)
		}
	}
}

func TestUnroutedMemberPatch(t *testing.T) {
	member := func(meta map[string]string) beads.Bead {
		return beads.Bead{ID: "m-1", Title: "step", Type: "task", Status: "open", Metadata: meta}
	}
	poolBinding := GraphRouteBinding{QualifiedName: "worker", MetadataOnly: true}

	t.Run("pool step gets only the route", func(t *testing.T) {
		patch, err := UnroutedMemberPatch(member(map[string]string{"gc.root_bead_id": "r-1"}), poolBinding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		want := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
		if !equalPatch(patch, want) {
			t.Fatalf("patch = %v, want %v", patch, want)
		}
	})

	t.Run("single-session target also stamps the session name", func(t *testing.T) {
		binding := GraphRouteBinding{QualifiedName: "mayor", SessionName: "mayor-session"}
		patch, err := UnroutedMemberPatch(member(map[string]string{}), binding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		want := map[string]string{
			beadmeta.RoutedToMetadataKey:    "mayor",
			beadmeta.SessionNameMetadataKey: "mayor-session",
		}
		if !equalPatch(patch, want) {
			t.Fatalf("patch = %v, want %v", patch, want)
		}
	})

	t.Run("formula-declared continuation group is carried with affinity", func(t *testing.T) {
		patch, err := UnroutedMemberPatch(member(map[string]string{
			beadmeta.ContinuationGroupMetadataKey: "pancakes-slot",
		}), poolBinding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		want := map[string]string{
			beadmeta.RoutedToMetadataKey:          "worker",
			beadmeta.ContinuationGroupMetadataKey: "pancakes-slot",
			beadmeta.SessionAffinityMetadataKey:   "require",
		}
		if !equalPatch(patch, want) {
			t.Fatalf("patch = %v, want %v", patch, want)
		}
	})

	t.Run("drain continuation group is router bookkeeping, never an opt-in", func(t *testing.T) {
		for _, independent := range []bool{false, true} {
			binding := poolBinding
			binding.IndependentSteps = independent
			patch, err := UnroutedMemberPatch(member(map[string]string{
				beadmeta.ContinuationGroupMetadataKey: "drain:cv-9",
			}), binding)
			if err != nil {
				t.Fatalf("IndependentSteps=%v drain group: %v", independent, err)
			}
			want := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
			if !equalPatch(patch, want) {
				t.Fatalf("IndependentSteps=%v patch = %v, want %v", independent, patch, want)
			}
		}
	})

	t.Run("one-shot target honors a formula-declared group like a launch does", func(t *testing.T) {
		// Launch decoration captures the authored group into the binding
		// before stamping, so the group wins even for a one-shot pool target
		// (graphroute.go); member routing must stamp the same pair.
		binding := poolBinding
		binding.IndependentSteps = true
		patch, err := UnroutedMemberPatch(member(map[string]string{
			beadmeta.ContinuationGroupMetadataKey: "pancakes-slot",
		}), binding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		want := map[string]string{
			beadmeta.RoutedToMetadataKey:          "worker",
			beadmeta.ContinuationGroupMetadataKey: "pancakes-slot",
			beadmeta.SessionAffinityMetadataKey:   "require",
		}
		if !equalPatch(patch, want) {
			t.Fatalf("patch = %v, want %v", patch, want)
		}
	})

	t.Run("control member gains the execution route, never the target as its own", func(t *testing.T) {
		// Cook stamps a control's own route (the control dispatcher) and
		// leaves gc.execution_routed_to empty; a launch fills the execution
		// route with the sling target. Member routing must fill exactly that.
		control := beads.Bead{
			ID: "ctl-1", Title: "gate the run", Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindRalph,
				beadmeta.RoutedToMetadataKey: "core.control-dispatcher",
			},
		}
		patch, err := UnroutedMemberPatch(control, poolBinding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		want := map[string]string{beadmeta.ExecutionRoutedToMetadataKey: "worker"}
		if !equalPatch(patch, want) {
			t.Fatalf("patch = %v, want %v", patch, want)
		}
	})

	t.Run("rig-scoped target carries the execution rig context", func(t *testing.T) {
		control := beads.Bead{
			ID: "ctl-2", Title: "gate the run", Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindWorkflowFinalize,
				beadmeta.RoutedToMetadataKey: "core.control-dispatcher",
			},
		}
		binding := GraphRouteBinding{QualifiedName: "acme/worker", MetadataOnly: true}
		patch, err := UnroutedMemberPatch(control, binding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		want := map[string]string{
			beadmeta.ExecutionRoutedToMetadataKey:   "acme/worker",
			beadmeta.ExecutionRigContextMetadataKey: "acme",
		}
		if !equalPatch(patch, want) {
			t.Fatalf("patch = %v, want %v", patch, want)
		}
	})

	t.Run("control with an execution route already set is left alone", func(t *testing.T) {
		control := beads.Bead{
			ID: "ctl-3", Title: "gate the run", Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:              beadmeta.KindRalph,
				beadmeta.RoutedToMetadataKey:          "core.control-dispatcher",
				beadmeta.ExecutionRoutedToMetadataKey: "other-worker",
			},
		}
		patch, err := UnroutedMemberPatch(control, poolBinding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		if patch != nil {
			t.Fatalf("patch = %v, want none: the control already has its execution route", patch)
		}
	})

	t.Run("members member routing never touches", func(t *testing.T) {
		cases := map[string]beads.Bead{
			"already routed":    member(map[string]string{beadmeta.RoutedToMetadataKey: "other-agent"}),
			"closed":            {ID: "m-2", Status: "closed", Metadata: map[string]string{}},
			"assigned":          {ID: "m-3", Status: "open", Assignee: "someone", Metadata: map[string]string{}},
			"session-bound":     member(map[string]string{beadmeta.SessionIDMetadataKey: "gc-sess-1"}),
			"workflow topology": member(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}),
			"scope latch":       member(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope}),
			"formula spec":      member(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindSpec}),
			"cross-store gate":  crossStoreGate("g-1"),
		}
		for name, b := range cases {
			patch, err := UnroutedMemberPatch(b, poolBinding)
			if err != nil {
				t.Fatalf("%s: UnroutedMemberPatch: %v", name, err)
			}
			if patch != nil {
				t.Errorf("%s: patch = %v, want none", name, patch)
			}
		}
	})

	t.Run("formula gate step is routed like any other step", func(t *testing.T) {
		// A formula [steps.gate] bead is type gate with no cross-store label,
		// and a launch routes it; only the drain's cross-store gate is skipped.
		gate := beads.Bead{ID: "g-2", Title: "wait for approval", Type: "gate", Status: "open", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "r-1",
		}}
		patch, err := UnroutedMemberPatch(gate, poolBinding)
		if err != nil {
			t.Fatalf("UnroutedMemberPatch: %v", err)
		}
		want := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
		if !equalPatch(patch, want) {
			t.Fatalf("patch = %v, want %v", patch, want)
		}
	})
}

// crossStoreGate is the gate bead a drain control mints for a blocker held in
// another store (dispatch.NewCrossStoreGate): it carries the run's
// gc.root_bead_id, so it is a member of the run, but no launch routes it.
func crossStoreGate(id string) beads.Bead {
	return beads.Bead{
		ID: id, Title: "Waits for other-1, held in another store", Type: "gate", Status: "open",
		Labels: []string{beadmeta.CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          "r-1",
			beadmeta.CrossStoreGateOwnerMetadataKey: "ctl-1",
			beadmeta.CrossStoreBlockerMetadataKey:   "other-1",
		},
	}
}

// TestMemberRouteEligible pins the members member routing considers at all.
// An eligible member may still take no patch (it already carries a route);
// eligibility is what lets the sling report it as left on another agent.
func TestMemberRouteEligible(t *testing.T) {
	open := func(meta map[string]string) beads.Bead {
		return beads.Bead{ID: "m-1", Title: "step", Type: "task", Status: "open", Metadata: meta}
	}
	for name, tc := range map[string]struct {
		bead beads.Bead
		want bool
	}{
		"unrouted step":      {open(map[string]string{beadmeta.RootBeadIDMetadataKey: "r-1"}), true},
		"step routed away":   {open(map[string]string{beadmeta.RoutedToMetadataKey: "other-agent"}), true},
		"control":            {open(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRalph}), true},
		"formula gate step":  {beads.Bead{ID: "g-2", Type: "gate", Status: "open", Metadata: map[string]string{}}, true},
		"closed":             {beads.Bead{ID: "m-2", Status: "closed", Metadata: map[string]string{}}, false},
		"assigned":           {beads.Bead{ID: "m-3", Status: "open", Assignee: "someone", Metadata: map[string]string{}}, false},
		"session-bound":      {open(map[string]string{beadmeta.SessionIDMetadataKey: "gc-sess-1"}), false},
		"workflow topology":  {open(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}), false},
		"scope latch":        {open(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope}), false},
		"cross-store gate":   {crossStoreGate("g-1"), false},
		"nil metadata, open": {beads.Bead{ID: "m-4", Status: "open"}, true},
	} {
		if got := MemberRouteEligible(tc.bead); got != tc.want {
			t.Errorf("%s: MemberRouteEligible = %v, want %v", name, got, tc.want)
		}
	}
}

func equalPatch(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestApplyGraphRoutingUsesSlingTargetBinding pins that extracting the target
// binding into SlingTargetBinding changed no ApplyGraphRouting behavior: pool
// targets stay metadata-only, single-session targets resolve the session name.
func TestApplyGraphRoutingUsesSlingTargetBinding(t *testing.T) {
	newRecipe := func() *formula.Recipe {
		return &formula.Recipe{
			Name: "pancakes",
			Steps: []formula.RecipeStep{
				{ID: "pancakes", IsRoot: true, Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				}},
				{ID: "pancakes.mix", Metadata: map[string]string{}},
			},
		}
	}
	store := beadstest.Sessions(beads.NewMemStore())
	cfg := &config.City{Agents: []config.Agent{
		{Name: config.ControlDispatcherAgentName, MaxActiveSessions: intPtr(1)},
	}}

	t.Run("pool target stamps routed_to with no session name", func(t *testing.T) {
		recipe := newRecipe()
		a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
		if err := ApplyGraphRouting(recipe, &a, "worker", nil, "", "", "", "", store, "test-city", cfg, Deps{Resolver: testAgentResolver{}}); err != nil {
			t.Fatalf("ApplyGraphRouting: %v", err)
		}
		step := recipe.Steps[1]
		if got := step.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
			t.Fatalf("gc.routed_to = %q, want worker", got)
		}
		if got := step.Metadata[beadmeta.SessionNameMetadataKey]; got != "" {
			t.Fatalf("gc.session_name = %q, want empty for a pool target", got)
		}
	})

	t.Run("single-session target resolves the session name", func(t *testing.T) {
		recipe := newRecipe()
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
		if err := ApplyGraphRouting(recipe, &a, "mayor", nil, "", "", "", "", store, "test-city", cfg, Deps{Resolver: testAgentResolver{}}); err != nil {
			t.Fatalf("ApplyGraphRouting: %v", err)
		}
		step := recipe.Steps[1]
		if got := step.Metadata[beadmeta.RoutedToMetadataKey]; got != "mayor" {
			t.Fatalf("gc.routed_to = %q, want mayor", got)
		}
		want := agent.SessionNameFor("test-city", "mayor", "")
		if got := step.Metadata[beadmeta.SessionNameMetadataKey]; got != want {
			t.Fatalf("gc.session_name = %q, want %q", got, want)
		}
	})

	t.Run("SlingTargetBinding matches the agent shape", func(t *testing.T) {
		pool := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
		binding, err := SlingTargetBinding(pool, "worker", store, "test-city", cfg)
		if err != nil {
			t.Fatalf("SlingTargetBinding: %v", err)
		}
		if !binding.MetadataOnly || binding.QualifiedName != "worker" || binding.SessionName != "" {
			t.Fatalf("pool binding = %+v, want metadata-only worker", binding)
		}
		single := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
		binding, err = SlingTargetBinding(single, "mayor", store, "test-city", cfg)
		if err != nil {
			t.Fatalf("SlingTargetBinding: %v", err)
		}
		if binding.MetadataOnly || binding.SessionName == "" {
			t.Fatalf("single-session binding = %+v, want a resolved session name", binding)
		}
	})
}
