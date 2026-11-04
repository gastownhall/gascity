package formula

import (
	"context"
	"testing"
)

// A formula-level [[scope]] table declares scope membership once, by step id.
// mergeSteps replaces an overridden step wholesale, so membership written on a
// step's own metadata is lost whenever an extender overrides that step. The
// declaration is resolved against the FINAL step list, after extends, so an
// override or an added terminal step still lands in the scope.
//
// Every fixture below extends scopeDeclBase, whose [[scope]] covers setup,
// implement and review as members, body as the scope body and cleanup as
// teardown.

const scopeDeclBase = `
formula = "decl-base"

[requires]
formula_compiler = ">=2.0.0"

[[scope]]
name = "worktree"
body = "body"
members = ["setup", "implement", "review"]
teardown = ["cleanup"]
on_fail = "abort_scope"

[[steps]]
id = "setup"
title = "Set up"

[[steps]]
id = "implement"
title = "Implement"
needs = ["setup"]

[[steps]]
id = "review"
title = "Review"
needs = ["implement"]

[[steps]]
id = "body"
title = "Scope body"
needs = ["setup", "implement", "review"]

[[steps]]
id = "cleanup"
title = "Tear down"
needs = ["body"]
metadata = { "gc.kind" = "cleanup" }
`

const scopeDeclChildHeader = `
formula = "decl-child"
extends = ["decl-base"]
`

// scopeKeys are the metadata keys a declaration owns.
var scopeKeys = []string{"gc.kind", "gc.scope_name", "gc.scope_ref", "gc.scope_role", "gc.on_fail"}

// declMember is the metadata a declared member of the "body" scope carries.
func declMember() map[string]string {
	return map[string]string{"gc.scope_ref": "body", "gc.scope_role": "member", "gc.on_fail": "abort_scope"}
}

func resolveScopeDecl(t *testing.T, child string) (*Formula, error) {
	t.Helper()
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "decl-base", scopeDeclBase)
	writeNamedFormula(t, dir, "decl-child", scopeDeclChildHeader+child)
	return resolveScopeFixture(t, "decl-child", dir)
}

func requireScopeMetadata(t *testing.T, f *Formula, want map[string]map[string]string) {
	t.Helper()
	byID := make(map[string]*Step, len(f.Steps))
	for _, step := range f.Steps {
		byID[step.ID] = step
	}
	for id, expected := range want {
		step := byID[id]
		if step == nil {
			t.Fatalf("step %q missing from the resolved formula", id)
		}
		for _, key := range scopeKeys {
			if got := step.Metadata[key]; got != expected[key] {
				t.Errorf("step %q %s = %q, want %q", id, key, got, expected[key])
			}
		}
	}
}

func TestResolveScopeDeclStampsMembersBodyAndTeardown(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "decl-base", scopeDeclBase)
	f, err := resolveScopeFixture(t, "decl-base", dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireScopeMetadata(t, f, map[string]map[string]string{
		"setup":     declMember(),
		"implement": declMember(),
		"review":    declMember(),
		"body":      {"gc.kind": "scope", "gc.scope_name": "worktree", "gc.scope_role": "body"},
		"cleanup":   {"gc.kind": "cleanup", "gc.scope_ref": "body", "gc.scope_role": "teardown"},
	})
}

func TestResolveScopeDeclSurvivesAnOverrideAcrossExtends(t *testing.T) {
	// The child overrides implement wholesale, with no scope metadata, and adds a
	// terminal step. It declares no [[scope]], so it inherits the parent's
	// declaration unchanged: the override stays a member, the new step does not
	// join (it was never listed).
	f, err := resolveScopeDecl(t, `
[[steps]]
id = "implement"
title = "Implement, the variant way"
needs = ["setup"]

[[steps]]
id = "publish"
title = "Publish"
needs = ["review"]
`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireScopeMetadata(t, f, map[string]map[string]string{
		"setup":     declMember(),
		"implement": declMember(),
		"review":    declMember(),
		"body":      {"gc.kind": "scope", "gc.scope_name": "worktree", "gc.scope_role": "body"},
		"publish":   {},
	})
}

func TestResolveScopeDeclChildDeclarationReplacesTheParentsByName(t *testing.T) {
	// The child redeclares "worktree" with a different member list that adds its
	// terminal step and drops review. Replacement is whole: review is not a
	// member any more, and there is no merge of the two lists.
	f, err := resolveScopeDecl(t, `
[[scope]]
name = "worktree"
body = "body"
members = ["setup", "implement", "publish"]
on_fail = "abort_scope"

[[steps]]
id = "implement"
title = "Implement, the variant way"
needs = ["setup"]

[[steps]]
id = "publish"
title = "Publish"
needs = ["review"]
`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireScopeMetadata(t, f, map[string]map[string]string{
		"setup":     declMember(),
		"implement": declMember(),
		"publish":   declMember(),
		"review":    {},
		"body":      {"gc.kind": "scope", "gc.scope_name": "worktree", "gc.scope_role": "body"},
		// The parent's teardown list went with the parent's declaration.
		"cleanup": {"gc.kind": "cleanup"},
	})
}

func TestResolveScopeDeclOnlyTheNamedScopeIsReplaced(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "two-base", `
formula = "two-base"

[requires]
formula_compiler = ">=2.0.0"

[[scope]]
name = "alpha"
body = "alpha-body"
members = ["a1"]

[[scope]]
name = "beta"
body = "beta-body"
members = ["b1"]

[[steps]]
id = "a1"
title = "A1"

[[steps]]
id = "alpha-body"
title = "Alpha body"
needs = ["a1"]

[[steps]]
id = "b1"
title = "B1"

[[steps]]
id = "beta-body"
title = "Beta body"
needs = ["b1"]

[[steps]]
id = "b2"
title = "B2"
`)
	writeNamedFormula(t, dir, "two-child", `
formula = "two-child"
extends = ["two-base"]

[[scope]]
name = "beta"
body = "beta-body"
members = ["b1", "b2"]
`)
	f, err := resolveScopeFixture(t, "two-child", dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireScopeMetadata(t, f, map[string]map[string]string{
		"a1":         {"gc.scope_ref": "alpha-body", "gc.scope_role": "member"},
		"alpha-body": {"gc.kind": "scope", "gc.scope_name": "alpha", "gc.scope_role": "body"},
		"b1":         {"gc.scope_ref": "beta-body", "gc.scope_role": "member"},
		"b2":         {"gc.scope_ref": "beta-body", "gc.scope_role": "member"},
		"beta-body":  {"gc.kind": "scope", "gc.scope_name": "beta", "gc.scope_role": "body"},
	})
}

func TestResolveScopeDeclDoesNotLeakIntoAParentOrItsCache(t *testing.T) {
	// One parser resolves the child, whose declaration drops review, and then
	// the parent itself. The parser caches the parent's steps by pointer, so a
	// stamp written in place would survive into the second resolve.
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "decl-base", scopeDeclBase)
	writeNamedFormula(t, dir, "decl-child", scopeDeclChildHeader+`
[[scope]]
name = "worktree"
body = "body"
members = ["setup"]
`)
	p := NewParser(dir)
	child, err := p.LoadByName("decl-child")
	if err != nil {
		t.Fatal(err)
	}
	resolvedChild, err := p.Resolve(child)
	if err != nil {
		t.Fatalf("Resolve child: %v", err)
	}
	requireScopeMetadata(t, resolvedChild, map[string]map[string]string{
		// The child's declaration sets no on_fail, so setup carries none.
		"setup":     {"gc.scope_ref": "body", "gc.scope_role": "member"},
		"implement": {},
		"review":    {},
	})

	base, err := p.LoadByName("decl-base")
	if err != nil {
		t.Fatal(err)
	}
	resolvedBase, err := p.Resolve(base)
	if err != nil {
		t.Fatalf("Resolve base: %v", err)
	}
	requireScopeMetadata(t, resolvedBase, map[string]map[string]string{
		"setup":     declMember(),
		"implement": declMember(),
		"review":    declMember(),
	})
}

func TestResolveScopeDeclWithoutADeclarationChangesNothing(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "scoped-base", scopedBaseFormula)
	f, err := resolveScopeFixture(t, "scoped-base", dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	requireScopeMetadata(t, f, map[string]map[string]string{
		"setup": {"gc.scope_ref": "body", "gc.scope_role": "setup", "gc.on_fail": "abort_scope"},
		"work":  declMember(),
		"body":  {"gc.kind": "scope", "gc.scope_name": "main", "gc.scope_role": "body"},
	})
}

func TestResolveScopeDeclRejectsABadDeclaration(t *testing.T) {
	cases := []struct {
		name  string
		child string
		want  []string
	}{
		{
			name: "typo_in_a_member_id",
			child: `
[[scope]]
name = "worktree"
body = "body"
members = ["setup", "implment"]
`,
			want: []string{`formula "decl-child"`, `scope "worktree"`, `"implment"`, "no step"},
		},
		{
			name: "typo_in_the_body_id",
			child: `
[[scope]]
name = "worktree"
body = "bodyy"
members = ["setup"]
`,
			want: []string{`scope "worktree"`, `"bodyy"`, "no step"},
		},
		{
			name: "typo_in_a_teardown_id",
			child: `
[[scope]]
name = "worktree"
body = "body"
members = ["setup"]
teardown = ["clean-up"]
`,
			want: []string{`scope "worktree"`, `"clean-up"`, "no step"},
		},
		{
			name: "every_missing_id_is_reported_at_once",
			child: `
[[scope]]
name = "worktree"
body = "body"
members = ["nope-one", "nope-two"]
`,
			want: []string{`"nope-one"`, `"nope-two"`},
		},
		{
			name: "body_listed_as_a_member",
			child: `
[[scope]]
name = "worktree"
body = "body"
members = ["setup", "body"]
`,
			want: []string{`scope "worktree"`, `"body"`, "body"},
		},
		{
			name: "step_in_two_scopes",
			child: `
[[scope]]
name = "worktree"
body = "body"
members = ["setup"]

[[scope]]
name = "second"
body = "second-body"
members = ["setup"]

[[steps]]
id = "second-body"
title = "Second body"
needs = ["setup"]
`,
			want: []string{`"setup"`, `"worktree"`, `"second"`},
		},
		{
			name: "no_members",
			child: `
[[scope]]
name = "worktree"
body = "body"
`,
			want: []string{`scope "worktree"`, "members"},
		},
		{
			name: "no_body",
			child: `
[[scope]]
name = "worktree"
members = ["setup"]
`,
			want: []string{`scope "worktree"`, "body"},
		},
		{
			name: "unknown_on_fail",
			child: `
[[scope]]
name = "worktree"
body = "body"
members = ["setup"]
on_fail = "explode"
`,
			want: []string{`scope "worktree"`, `"explode"`, "abort_scope"},
		},
		{
			name: "a_hand_placed_opt_out_conflicts_with_the_declaration",
			child: `
[[steps]]
id = "implement"
title = "Implement outside the scope"
needs = ["setup"]
metadata = { "gc.scope_ref" = "" }
`,
			want: []string{`step "implement"`, "gc.scope_ref", `scope "worktree"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveScopeDecl(t, tc.child)
			if err == nil {
				t.Fatal("Resolve succeeded, want a scope declaration error")
			}
			for _, substr := range tc.want {
				requireErrorContains(t, err, substr)
			}
		})
	}
}

func TestResolveScopeDeclRejectsATypoWithNoExtends(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "solo", `
formula = "solo"

[requires]
formula_compiler = ">=2.0.0"

[[scope]]
name = "main"
body = "body"
members = ["workk"]

[[steps]]
id = "work"
title = "Work"

[[steps]]
id = "body"
title = "Body"
needs = ["work"]
`)
	_, err := resolveScopeFixture(t, "solo", dir)
	if err == nil {
		t.Fatal("Resolve succeeded, want an error for the typo'd member id")
	}
	requireErrorContains(t, err, `"workk"`)
}

func TestCompileScopeDeclStampsBeforeGraphControls(t *testing.T) {
	// The stamp must land before ApplyGraphControls, which reads gc.scope_ref off
	// the final steps to mint each member's scope-check. An override that dropped
	// its membership would get no scope-check at all.
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "decl-base", scopeDeclBase)
	writeNamedFormula(t, dir, "decl-child", scopeDeclChildHeader+`
[[steps]]
id = "implement"
title = "Implement, the variant way"
needs = ["setup"]
`)
	recipe, err := Compile(context.Background(), "decl-child", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, id := range []string{"setup", "implement", "review"} {
		check := recipe.StepByID("decl-child." + id + "-scope-check")
		if check == nil {
			t.Fatalf("%s-scope-check missing: the member was not in the scope when graph controls ran", id)
		}
		if got := check.Metadata["gc.scope_ref"]; got != "body" {
			t.Errorf("%s-scope-check gc.scope_ref = %q, want body", id, got)
		}
	}
	if cleanup := recipe.StepByID("decl-child.cleanup-scope-check"); cleanup != nil {
		t.Error("teardown step minted a scope-check; teardown is exempt")
	}
}
