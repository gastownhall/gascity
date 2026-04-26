package graphroute

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/session"
)

func TestIsCompiledGraphWorkflow(t *testing.T) {
	t.Run("nil recipe", func(t *testing.T) {
		if IsCompiledGraphWorkflow(nil) {
			t.Error("expected false for nil recipe")
		}
	})
	t.Run("empty steps", func(t *testing.T) {
		if IsCompiledGraphWorkflow(&formula.Recipe{}) {
			t.Error("expected false for empty steps")
		}
	})
	t.Run("graph workflow", func(t *testing.T) {
		r := &formula.Recipe{
			Steps: []formula.RecipeStep{{
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			}},
		}
		if !IsCompiledGraphWorkflow(r) {
			t.Error("expected true for graph.v2 workflow")
		}
	})
}

func TestIsControlDispatcherKind(t *testing.T) {
	for _, kind := range []string{"check", "fanout", "retry-eval", "scope-check", "workflow-finalize", "retry", "ralph"} {
		if !IsControlDispatcherKind(kind) {
			t.Errorf("expected true for %q", kind)
		}
	}
	if IsControlDispatcherKind("task") {
		t.Error("expected false for task")
	}
}

func TestIsWorkflowTopologyKind(t *testing.T) {
	for _, kind := range []string{"workflow", "scope", "spec"} {
		if !IsWorkflowTopologyKind(kind) {
			t.Errorf("expected true for %q", kind)
		}
	}
	for _, kind := range []string{"", "task", "check", "fanout", "ralph"} {
		if IsWorkflowTopologyKind(kind) {
			t.Errorf("expected false for %q", kind)
		}
	}
}

func TestGraphRouteRigContext(t *testing.T) {
	if got := GraphRouteRigContext("myrig/worker"); got != "myrig" {
		t.Errorf("got %q, want myrig", got)
	}
	if got := GraphRouteRigContext("mayor"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestGraphWorkflowRouteVars(t *testing.T) {
	dflt := "default-val"
	r := &formula.Recipe{
		Vars: map[string]*formula.VarDef{
			"base": {Default: &dflt},
		},
	}
	got := GraphWorkflowRouteVars(r, map[string]string{"override": "yes"})
	if got["base"] != "default-val" {
		t.Errorf("base = %q, want default-val", got["base"])
	}
	if got["override"] != "yes" {
		t.Errorf("override = %q, want yes", got["override"])
	}
}

func intPtr(v int) *int { return &v }

func TestApplyGraphRouting_LegacyStampsRoutedTo(t *testing.T) {
	// Legacy [[steps]] recipes (no graph.v2 root) must still stamp
	// gc.routed_to on every non-root step so Agent.EffectiveWorkQuery
	// tier-3 and pool scale_check can see the work. Regression for #796.
	r := &formula.Recipe{
		Name: "mol-legacy",
		Steps: []formula.RecipeStep{
			{ID: "mol-legacy", IsRoot: true, Metadata: map[string]string{}},
			{ID: "mol-legacy.step1", Metadata: map[string]string{}},
			{ID: "mol-legacy.step2", Metadata: map[string]string{}},
		},
	}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	err := ApplyGraphRouting(r, &a, "worker", nil, "", "", "", "", nil, "city", &config.City{}, Deps{})
	if err != nil {
		t.Fatalf("unexpected error for legacy recipe: %v", err)
	}
	if got := r.Steps[0].Metadata["gc.routed_to"]; got != "" {
		t.Errorf("root gc.routed_to = %q, want empty (root carries routing via InstantiateSlingFormula, not graphroute)", got)
	}
	for i := 1; i < len(r.Steps); i++ {
		if got := r.Steps[i].Metadata["gc.routed_to"]; got != "worker" {
			t.Errorf("step %d (%s) gc.routed_to = %q, want worker", i, r.Steps[i].ID, got)
		}
	}
}

func TestApplyGraphRouting_LegacyNilAgent(t *testing.T) {
	// Legacy path stamps gc.routed_to from the routedTo argument without
	// needing agent resolution — the order-dispatch caller passes a=nil.
	r := &formula.Recipe{
		Name: "mol-legacy",
		Steps: []formula.RecipeStep{
			{ID: "mol-legacy", IsRoot: true, Metadata: map[string]string{}},
			{ID: "mol-legacy.step1", Metadata: map[string]string{}},
		},
	}
	err := ApplyGraphRouting(r, nil, "worker", nil, "", "", "", "", nil, "city", &config.City{}, Deps{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := r.Steps[1].Metadata["gc.routed_to"]; got != "worker" {
		t.Errorf("step gc.routed_to = %q, want worker", got)
	}
}

func TestApplyGraphRouting_LegacyAttachmentKeepsRoutingOnSourceBeadOnly(t *testing.T) {
	r := &formula.Recipe{
		Name: "mol-legacy",
		Steps: []formula.RecipeStep{
			{ID: "mol-legacy", IsRoot: true, Metadata: map[string]string{}},
			{ID: "mol-legacy.step1", Metadata: map[string]string{}},
		},
	}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	err := ApplyGraphRouting(r, &a, "worker", nil, "source-1", "", "", "", nil, "city", &config.City{}, Deps{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := r.Steps[1].Metadata["gc.routed_to"]; ok {
		t.Errorf("attached legacy step gc.routed_to was stamped; attached demand should stay on the routed source bead")
	}
}

func TestApplyGraphRouting_LegacyNilCfg(t *testing.T) {
	// ApplyGraphRouting is a no-op whenever cfg is nil.
	r := &formula.Recipe{
		Steps: []formula.RecipeStep{
			{IsRoot: true, Metadata: map[string]string{}},
			{ID: "step1", Metadata: map[string]string{}},
		},
	}
	err := ApplyGraphRouting(r, nil, "worker", nil, "", "", "", "", nil, "city", nil, Deps{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := r.Steps[1].Metadata["gc.routed_to"]; ok {
		t.Error("expected no routing when cfg is nil")
	}
}

func TestApplyGraphRouting_LegacyOverwritesExistingRouting(t *testing.T) {
	// Legacy stamping mirrors graph.v2 AssignGraphStepRoute: unconditional
	// overwrite on every non-topology step.
	r := &formula.Recipe{
		Name: "mol-legacy",
		Steps: []formula.RecipeStep{
			{IsRoot: true, Metadata: map[string]string{}},
			{ID: "step1", Metadata: map[string]string{"gc.routed_to": "stale-agent"}},
		},
	}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	err := ApplyGraphRouting(r, &a, "worker", nil, "", "", "", "", nil, "city", &config.City{}, Deps{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := r.Steps[1].Metadata["gc.routed_to"]; got != "worker" {
		t.Errorf("expected overwrite to worker, got %q", got)
	}
}

func TestApplyGraphRouting_LegacySkipsWorkflowKinds(t *testing.T) {
	// Defensive: if a legacy recipe somehow contains a step with
	// gc.kind=workflow/scope/spec, skip routing on it — those are
	// workflow-topology kinds and legacy formulas don't activate the
	// workflow machinery.
	r := &formula.Recipe{
		Name: "mol-legacy",
		Steps: []formula.RecipeStep{
			{IsRoot: true, Metadata: map[string]string{}},
			{ID: "scope", Metadata: map[string]string{"gc.kind": "scope"}},
			{ID: "work", Metadata: map[string]string{}},
		},
	}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	err := ApplyGraphRouting(r, &a, "worker", nil, "", "", "", "", nil, "city", &config.City{}, Deps{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := r.Steps[1].Metadata["gc.routed_to"]; ok {
		t.Error("expected scope-kind step to be skipped")
	}
	if got := r.Steps[2].Metadata["gc.routed_to"]; got != "worker" {
		t.Errorf("work step gc.routed_to = %q, want worker", got)
	}
}

type testAgentResolver struct{}

func (testAgentResolver) ResolveAgent(cfg *config.City, name, _ string) (config.Agent, bool) {
	for _, a := range cfg.Agents {
		if a.QualifiedName() == name || a.Name == name {
			return a, true
		}
	}
	return config.Agent{}, false
}

func TestDecorateGraphWorkflowRecipe_SetsRootMetadata(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "mayor", MaxActiveSessions: intPtr(1)},
		{Name: "control-dispatcher", MaxActiveSessions: intPtr(1)},
	}}
	r := &formula.Recipe{
		Name: "wf-test",
		Steps: []formula.RecipeStep{
			{ID: "wf-test.root", IsRoot: true, Metadata: map[string]string{
				"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
			}},
			{ID: "wf-test.work", Metadata: map[string]string{}},
		},
	}
	deps := Deps{Resolver: testAgentResolver{}}
	err := DecorateGraphWorkflowRecipe(r, nil, "src-1", "city", "test-city", "city:test", "mayor", "test--mayor", nil, "test-city", cfg, deps)
	if err != nil {
		t.Fatalf("DecorateGraphWorkflowRecipe: %v", err)
	}
	root := r.Steps[0]
	if root.Metadata["gc.run_target"] != "mayor" {
		t.Errorf("root gc.run_target = %q, want mayor", root.Metadata["gc.run_target"])
	}
	if root.Metadata["gc.source_bead_id"] != "src-1" {
		t.Errorf("root gc.source_bead_id = %q, want src-1", root.Metadata["gc.source_bead_id"])
	}
	if root.Metadata["gc.scope_kind"] != "city" {
		t.Errorf("root gc.scope_kind = %q, want city", root.Metadata["gc.scope_kind"])
	}
	// Work step should have gc.routed_to set.
	work := r.Steps[1]
	if work.Metadata["gc.routed_to"] != "mayor" {
		t.Errorf("work gc.routed_to = %q, want mayor", work.Metadata["gc.routed_to"])
	}
}

func TestDecorateGraphWorkflowRecipe_NilRecipe(t *testing.T) {
	err := DecorateGraphWorkflowRecipe(nil, nil, "", "", "", "", "", "", nil, "", nil, Deps{})
	if err == nil {
		t.Error("expected error for nil recipe")
	}
}

func TestResolveGraphStepBinding_CycleDetection(t *testing.T) {
	// Step A has kind "check" with dep on B, B has kind "check" with dep on A.
	// This creates a routing cycle.
	stepA := &formula.RecipeStep{ID: "A", Metadata: map[string]string{"gc.kind": "check"}}
	stepB := &formula.RecipeStep{ID: "B", Metadata: map[string]string{"gc.kind": "check"}}
	stepByID := map[string]*formula.RecipeStep{"A": stepA, "B": stepB}
	depsByStep := map[string][]string{"A": {"B"}, "B": {"A"}}
	cache := make(map[string]GraphRouteBinding)
	resolving := make(map[string]bool)
	fallback := GraphRouteBinding{QualifiedName: "default"}

	_, err := ResolveGraphStepBinding("A", stepByID, nil, depsByStep, cache, resolving, fallback, "", nil, "", nil, Deps{})
	if err == nil {
		t.Error("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want cycle mention", err.Error())
	}
}

func TestResolveGraphStepBinding_AssigneeTemplateTargetRejected(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		},
	}
	stepByID := map[string]*formula.RecipeStep{
		"demo.work": {
			ID:       "demo.work",
			Title:    "Work",
			Assignee: "worker",
		},
	}
	cache := make(map[string]GraphRouteBinding)
	resolving := make(map[string]bool)

	_, err := ResolveGraphStepBinding("demo.work", stepByID, nil, nil, cache, resolving, GraphRouteBinding{}, "frontend", beads.NewMemStore(), cfg.Workspace.Name, cfg, Deps{Resolver: testAgentResolver{}})
	if err == nil {
		t.Fatal("ResolveGraphStepBinding unexpectedly succeeded for template assignee")
	}
	if !strings.Contains(err.Error(), "use gc.run_target for config routing") {
		t.Fatalf("ResolveGraphStepBinding error = %q, want gc.run_target guidance", err)
	}
}

func TestResolveGraphStepBinding_AssigneeConcreteSessionBeatsTemplateCollision(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		},
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-frontend-worker",
			"alias":        "frontend/worker-live",
			"template":     "frontend/worker",
			"state":        "active",
		},
	}}, nil)
	stepByID := map[string]*formula.RecipeStep{
		"demo.work": {
			ID:       "demo.work",
			Title:    "Work",
			Assignee: "worker",
		},
	}
	cache := make(map[string]GraphRouteBinding)
	resolving := make(map[string]bool)

	binding, err := ResolveGraphStepBinding("demo.work", stepByID, nil, nil, cache, resolving, GraphRouteBinding{}, "frontend", store, cfg.Workspace.Name, cfg, Deps{Resolver: testAgentResolver{}})
	if err != nil {
		t.Fatalf("ResolveGraphStepBinding: %v", err)
	}
	if binding.DirectSessionID != "worker" {
		t.Fatalf("DirectSessionID = %q, want worker", binding.DirectSessionID)
	}
}

func TestControlDispatcherBinding_NilConfig(t *testing.T) {
	_, err := ControlDispatcherBinding(nil, "city", nil, "", Deps{})
	if err == nil {
		t.Error("expected error for nil config")
	}
}

func TestControlDispatcherBinding_NilResolver(t *testing.T) {
	cfg := &config.City{}
	_, err := ControlDispatcherBinding(nil, "city", cfg, "", Deps{})
	if err == nil {
		t.Error("expected error for nil resolver")
	}
}

func TestControlDispatcherBinding_ConfiguredDispatcherUsesConcreteSessionName(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{
		Name: "control-dispatcher",
		Dir:  "gascity",
	}}}

	binding, err := ControlDispatcherBinding(nil, "test-city", cfg, "gascity", Deps{Resolver: testAgentResolver{}})
	if err != nil {
		t.Fatalf("ControlDispatcherBinding: %v", err)
	}
	if binding.QualifiedName != "gascity/control-dispatcher" {
		t.Fatalf("QualifiedName = %q, want gascity/control-dispatcher", binding.QualifiedName)
	}
	if binding.SessionName != "gascity--control-dispatcher" {
		t.Fatalf("SessionName = %q, want gascity--control-dispatcher", binding.SessionName)
	}
	if binding.MetadataOnly {
		t.Fatalf("MetadataOnly = true, want false")
	}
}

func TestAssignGraphStepRoute_ControlBindingUsesDirectAssigneeWithoutRoutedTo(t *testing.T) {
	step := &formula.RecipeStep{
		Metadata: map[string]string{
			"gc.routed_to": "stale-control-route",
		},
	}
	execution := GraphRouteBinding{
		QualifiedName: "gascity/claude",
		MetadataOnly:  true,
	}
	control := GraphRouteBinding{
		QualifiedName: "gascity/control-dispatcher",
		SessionName:   "gascity--control-dispatcher",
	}

	AssignGraphStepRoute(step, execution, &control)

	if step.Assignee != "gascity--control-dispatcher" {
		t.Fatalf("control assignee = %q, want gascity--control-dispatcher", step.Assignee)
	}
	if got := step.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("control gc.routed_to = %q, want empty direct assignee", got)
	}
	if got := step.Metadata[GraphExecutionRouteMetaKey]; got != "gascity/claude" {
		t.Fatalf("control execution route = %q, want gascity/claude", got)
	}
}

func TestWorkflowExecutionRoute(t *testing.T) {
	b := beads.Bead{Metadata: map[string]string{"gc.routed_to": "myrig/worker"}}
	if got := WorkflowExecutionRoute(b); got != "myrig/worker" {
		t.Errorf("got %q, want myrig/worker", got)
	}
}

func TestWorkflowExecutionRouteFromMeta_PrefersExecutionKey(t *testing.T) {
	meta := map[string]string{
		GraphExecutionRouteMetaKey: "executor",
		"gc.routed_to":             "control",
	}
	if got := WorkflowExecutionRouteFromMeta(meta); got != "executor" {
		t.Errorf("got %q, want executor (execution key takes precedence)", got)
	}
}
