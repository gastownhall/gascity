package formula

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// technicalHealthPatrolFormula is the declared graph of the live
// mol-technical-health-patrol formula (prose trimmed), the shape that produced
// four of the measured deadlocked pairs for ga-a6zy9. The scope body declares
// needs on its own member, which is also the canonical scope example in
// docs/reference/specs/formula-spec-v2.md section 3.5.
const technicalHealthPatrolFormula = `
formula = "mol-technical-health-patrol"
version = 1
contract = "graph.v2"
description = "Technical health patrol"

[[steps]]
id = "body"
title = "Capped technical-health patrol"
needs = ["inspect-in-flight"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "technical-health-patrol", "gc.scope_role" = "body" }

[[steps]]
id = "inspect-in-flight"
title = "Inspect one capped changed/in-flight set"
needs = []
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 2
on_exhausted = "hard_fail"
`

// scopedWorkWithDownstreamFormula pairs a body that needs its own member with a
// downstream step that needs the same member. The body's reference must survive
// unrewritten and the downstream step's must be rewritten to the scope-check:
// only the self-closing edge is exempt.
const scopedWorkWithDownstreamFormula = `
formula = "mol-scope-downstream"
version = 1
contract = "graph.v2"
description = "Scope with a downstream consumer"

[[steps]]
id = "body"
title = "Scope body"
needs = ["member"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "work", "gc.scope_role" = "body" }

[[steps]]
id = "member"
title = "Scoped member"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[[steps]]
id = "downstream"
title = "Downstream consumer"
needs = ["member"]
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member" }

[[steps]]
id = "cleanup"
title = "Tear down"
needs = ["body"]
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }
`

func compileFormulaSource(t *testing.T, name, source string) *Recipe {
	t.Helper()
	enableV2ForTest(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(source), 0o600); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	recipe, err := CompileWithoutRuntimeVarValidation(context.Background(), name, []string{dir}, map[string]string{})
	if err != nil {
		t.Fatalf("compiling %s: %v", name, err)
	}
	return recipe
}

func blockersOf(recipe *Recipe, stepID string) []string {
	var out []string
	for _, dep := range recipe.Deps {
		if dep.StepID == stepID && isReadinessBlockingDepType(dep.Type) {
			out = append(out, dep.DependsOnID)
		}
	}
	return out
}

func TestCompileScopeBodyIsNeverBlockedByItsOwnScopeCheck(t *testing.T) {
	t.Parallel()

	recipe := compileFormulaSource(t, "mol-technical-health-patrol", technicalHealthPatrolFormula)

	const (
		body        = "mol-technical-health-patrol.body"
		member      = "mol-technical-health-patrol.inspect-in-flight"
		scopeCheck  = "mol-technical-health-patrol.inspect-in-flight-scope-check"
		finalizeSte = "mol-technical-health-patrol.workflow-finalize"
	)

	blockers := blockersOf(recipe, body)
	for _, blocker := range blockers {
		if blocker == scopeCheck {
			t.Fatalf("scope body %s is blocked by %s, the control that closes it", body, scopeCheck)
		}
	}
	if !containsString(blockers, member) {
		t.Fatalf("scope body blockers = %v, want the raw member %s", blockers, member)
	}

	// Ordering is preserved without the self-edge: the scope-check still
	// waits on its member, and the finalizer still waits on the body.
	if !containsString(blockersOf(recipe, scopeCheck), member) {
		t.Fatalf("scope-check blockers = %v, want %s", blockersOf(recipe, scopeCheck), member)
	}
	if !containsString(blockersOf(recipe, finalizeSte), body) {
		t.Fatalf("workflow-finalize blockers = %v, want the scope body %s", blockersOf(recipe, finalizeSte), body)
	}
}

func TestCompileRewritesDownstreamRefsButNotTheScopeBodys(t *testing.T) {
	t.Parallel()

	recipe := compileFormulaSource(t, "mol-scope-downstream", scopedWorkWithDownstreamFormula)

	const (
		body       = "mol-scope-downstream.body"
		member     = "mol-scope-downstream.member"
		scopeCheck = "mol-scope-downstream.member-scope-check"
		downstream = "mol-scope-downstream.downstream"
	)

	// The legitimate rewrite is preserved: a downstream consumer waits on
	// scope convergence, not on the raw member close.
	if !containsString(blockersOf(recipe, downstream), scopeCheck) {
		t.Fatalf("downstream blockers = %v, want %s", blockersOf(recipe, downstream), scopeCheck)
	}
	if containsString(blockersOf(recipe, downstream), member) {
		t.Fatalf("downstream blockers = %v, want the rewritten scope-check, not the raw member", blockersOf(recipe, downstream))
	}

	// The self-closing rewrite is not.
	if containsString(blockersOf(recipe, body), scopeCheck) {
		t.Fatalf("scope body blockers = %v, must not include the control that closes it", blockersOf(recipe, body))
	}
	if !containsString(blockersOf(recipe, body), member) {
		t.Fatalf("scope body blockers = %v, want the raw member %s", blockersOf(recipe, body), member)
	}
}

func TestCompileWorkflowRootIsNeverBlockedByItsOwnFinalizer(t *testing.T) {
	t.Parallel()

	recipe := compileFormulaSource(t, "mol-technical-health-patrol", technicalHealthPatrolFormula)

	const (
		root     = "mol-technical-health-patrol"
		finalize = "mol-technical-health-patrol.workflow-finalize"
	)

	var edgeType string
	found := false
	for _, dep := range recipe.Deps {
		if dep.StepID == root && dep.DependsOnID == finalize {
			edgeType = dep.Type
			found = true
		}
	}
	if !found {
		t.Fatalf("workflow root %s lost its edge to %s; the root must still reach its finalizer", root, finalize)
	}
	if isReadinessBlockingDepType(edgeType) {
		t.Fatalf("root -> finalizer edge type = %q, which blocks readiness; the finalizer closes the root", edgeType)
	}
	if edgeType != "tracks" {
		t.Fatalf("root -> finalizer edge type = %q, want tracks", edgeType)
	}
}

func TestFindSelfClosingControlEdgesDetectsBothInstances(t *testing.T) {
	t.Parallel()

	steps := []RecipeStep{
		{ID: "root", Metadata: map[string]string{"gc.kind": "workflow"}},
		{ID: "root.body", Metadata: map[string]string{"gc.kind": "scope", "gc.scope_role": "body"}},
		{ID: "root.member", Metadata: map[string]string{"gc.scope_ref": "body", "gc.scope_role": "member"}},
		{ID: "root.member-scope-check", Metadata: map[string]string{"gc.kind": "scope-check", "gc.scope_ref": "body", "gc.scope_role": "control"}},
		{ID: "root.workflow-finalize", Metadata: map[string]string{"gc.kind": "workflow-finalize"}},
	}

	tests := []struct {
		name string
		deps []RecipeDep
		want string
	}{
		{
			name: "scope body blocked by its scope-check",
			deps: []RecipeDep{{StepID: "root.body", DependsOnID: "root.member-scope-check", Type: "blocks"}},
			want: "root.body is blocked by root.member-scope-check (blocks), which closes it",
		},
		{
			name: "workflow root blocked by its finalizer",
			deps: []RecipeDep{{StepID: "root", DependsOnID: "root.workflow-finalize", Type: "blocks"}},
			want: "root is blocked by root.workflow-finalize (blocks), which closes it",
		},
		{
			name: "empty dep type defaults to blocks",
			deps: []RecipeDep{{StepID: "root.body", DependsOnID: "root.member-scope-check"}},
			want: "root.body is blocked by root.member-scope-check (), which closes it",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			found := FindSelfClosingControlEdges(steps, tc.deps)
			if len(found) != 1 {
				t.Fatalf("FindSelfClosingControlEdges = %v, want exactly one edge", found)
			}
			if got := found[0].String(); got != tc.want {
				t.Fatalf("edge = %q, want %q", got, tc.want)
			}
			err := ValidateNoSelfClosingControlEdges("demo", steps, tc.deps)
			if err == nil {
				t.Fatal("ValidateNoSelfClosingControlEdges returned nil for a self-closing edge")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestFindSelfClosingControlEdgesAllowsLegitimateEdges(t *testing.T) {
	t.Parallel()

	steps := []RecipeStep{
		{ID: "root", Metadata: map[string]string{"gc.kind": "workflow"}},
		{ID: "root.body", Metadata: map[string]string{"gc.kind": "scope", "gc.scope_role": "body"}},
		{ID: "root.other-body", Metadata: map[string]string{"gc.kind": "scope", "gc.scope_role": "body"}},
		{ID: "root.member", Metadata: map[string]string{"gc.scope_ref": "body", "gc.scope_role": "member"}},
		{ID: "root.member-scope-check", Metadata: map[string]string{"gc.kind": "scope-check", "gc.scope_ref": "body", "gc.scope_role": "control"}},
		{ID: "root.member-fanout", Metadata: map[string]string{"gc.kind": "fanout", "gc.scope_ref": "body", "gc.scope_role": "control"}},
		{ID: "root.downstream", Metadata: map[string]string{}},
		{ID: "root.workflow-finalize", Metadata: map[string]string{"gc.kind": "workflow-finalize"}},
	}

	deps := []RecipeDep{
		// Downstream work waiting on scope convergence: the whole point of
		// the rewrite.
		{StepID: "root.downstream", DependsOnID: "root.member-scope-check", Type: "blocks"},
		// A different scope's body: this scope-check does not close it.
		{StepID: "root.other-body", DependsOnID: "root.member-scope-check", Type: "blocks"},
		// A control kind that closes only itself.
		{StepID: "root.body", DependsOnID: "root.member-fanout", Type: "blocks"},
		// The finalizer waiting on its sinks, and the body on its member.
		{StepID: "root.workflow-finalize", DependsOnID: "root.body", Type: "blocks"},
		{StepID: "root.body", DependsOnID: "root.member", Type: "blocks"},
		// Informational edges are never readiness obligations.
		{StepID: "root", DependsOnID: "root.workflow-finalize", Type: "tracks"},
		{StepID: "root.body", DependsOnID: "root.member-scope-check", Type: "tracks"},
	}

	if found := FindSelfClosingControlEdges(steps, deps); len(found) != 0 {
		t.Fatalf("FindSelfClosingControlEdges = %v, want none", found)
	}
	if err := ValidateNoSelfClosingControlEdges("demo", steps, deps); err != nil {
		t.Fatalf("ValidateNoSelfClosingControlEdges = %v, want nil", err)
	}
}

// TestBundledFormulaCorpusHasNoSelfClosingControlEdges compiles every formula
// the SDK ships and pins the invariant across all of them, so a new bundled
// formula cannot reintroduce the deadlock.
func TestBundledFormulaCorpusHasNoSelfClosingControlEdges(t *testing.T) {
	t.Parallel()
	enableV2ForTest(t)

	dirs := []string{
		filepath.Join("..", "bootstrap", "packs", "core", "formulas"),
		filepath.Join("..", "..", "cmd", "gc", "testdata", "formulas"),
	}

	compiled := 0
	for _, dir := range dirs {
		paths, err := filepath.Glob(filepath.Join(dir, "*.toml"))
		if err != nil {
			t.Fatalf("globbing %s: %v", dir, err)
		}
		if len(paths) == 0 {
			t.Fatalf("no formulas found under %s", dir)
		}
		for _, path := range paths {
			name := strings.TrimSuffix(filepath.Base(path), ".toml")
			recipe, err := CompileWithoutRuntimeVarValidation(context.Background(), name, []string{dir}, map[string]string{})
			if err != nil {
				t.Fatalf("compiling %s: %v", name, err)
			}
			compiled++
			if found := FindSelfClosingControlEdges(recipe.Steps, recipe.Deps); len(found) != 0 {
				t.Errorf("formula %s compiles to self-closing control edges: %v", name, found)
			}
		}
	}
	if compiled == 0 {
		t.Fatal("compiled no formulas; the corpus sweep proved nothing")
	}
}

func TestRewriteRecipeDepsToControlsSkipsOnlyTheSelfClosingEdge(t *testing.T) {
	t.Parallel()

	nodes := []RecipeStep{
		{ID: "mol.body", Metadata: map[string]string{"gc.kind": "scope", "gc.scope_role": "body"}},
		{ID: "mol.member", Metadata: map[string]string{"gc.scope_ref": "mol.body", "gc.scope_role": "member"}},
		{ID: "mol.downstream", Metadata: map[string]string{}},
	}
	controls := []RecipeStep{
		{ID: "mol.member-scope-check", Metadata: map[string]string{"gc.kind": "scope-check", "gc.scope_ref": "mol.body", "gc.scope_role": "control"}},
	}
	deps := []RecipeDep{
		{StepID: "mol.body", DependsOnID: "mol.member", Type: "blocks"},
		{StepID: "mol.downstream", DependsOnID: "mol.member", Type: "blocks"},
	}

	RewriteRecipeDepsToControls(deps, nodes, controls, map[string]string{"mol.member": "mol.member-scope-check"})

	if deps[0].DependsOnID != "mol.member" {
		t.Fatalf("scope body dep rewritten to %q; it must keep naming the raw member", deps[0].DependsOnID)
	}
	if deps[1].DependsOnID != "mol.member-scope-check" {
		t.Fatalf("downstream dep = %q, want the rewritten scope-check", deps[1].DependsOnID)
	}
}

// TestApplyFragmentRecipeGraphControlsKeepsTheScopeBodyUnblocked covers the
// dynamic-fragment minting site: a fragment carrying its own scope body must
// not come back with the body waiting on a scope-check that closes it.
func TestApplyFragmentRecipeGraphControlsKeepsTheScopeBodyUnblocked(t *testing.T) {
	t.Parallel()

	fragment := &FragmentRecipe{
		Name: "expansion-scoped",
		Steps: []RecipeStep{
			{
				ID:    "expansion-scoped.body",
				Title: "Scope body",
				Metadata: map[string]string{
					"gc.kind":       "scope",
					"gc.scope_role": "body",
				},
			},
			{
				ID:    "expansion-scoped.review",
				Title: "Review",
				Metadata: map[string]string{
					"gc.scope_ref":  "expansion-scoped.body",
					"gc.scope_role": "member",
				},
			},
			{
				ID:    "expansion-scoped.submit",
				Title: "Submit",
				Metadata: map[string]string{
					"gc.scope_ref":  "expansion-scoped.body",
					"gc.scope_role": "member",
				},
			},
		},
		Deps: []RecipeDep{
			{StepID: "expansion-scoped.body", DependsOnID: "expansion-scoped.review", Type: "blocks"},
			{StepID: "expansion-scoped.submit", DependsOnID: "expansion-scoped.review", Type: "blocks"},
		},
	}

	ApplyFragmentRecipeGraphControls(fragment)

	if found := FindSelfClosingControlEdges(fragment.Steps, fragment.Deps); len(found) != 0 {
		t.Fatalf("fragment controls minted self-closing edges: %v", found)
	}

	var sawBodyOnRawMember, sawRewrittenSubmit bool
	for _, dep := range fragment.Deps {
		switch {
		case dep.StepID == "expansion-scoped.body" && dep.DependsOnID == "expansion-scoped.review":
			sawBodyOnRawMember = true
		case dep.StepID == "expansion-scoped.submit" && dep.DependsOnID == "expansion-scoped.review-scope-check":
			sawRewrittenSubmit = true
		}
	}
	if !sawBodyOnRawMember {
		t.Fatalf("scope body lost its edge to the raw member; deps = %+v", fragment.Deps)
	}
	if !sawRewrittenSubmit {
		t.Fatalf("downstream submit dep was not rewritten to the scope-check; deps = %+v", fragment.Deps)
	}
}
