package formula

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// mergeSteps replaces an overridden step wholesale, so a scope member's
// gc.scope_ref vanishes from an override unless the child redeclares it. Three
// structural checks guard scope membership across `extends`, and Resolve runs
// one of them: a gc.scope_ref must name a scope body. The override-regression
// and uncovered-sink checks are pinned by direct tests on constructed steps and
// are not wired into Resolve, because enforcing them would reject an extender
// written before its base declared membership (enabling them is ga-prk8k5).
//
// Every table case that goes through Resolve extends scopedBaseFormula, a
// minimal scoped formula whose only scope body is "body".

const scopedBaseFormula = `
formula = "scoped-base"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "setup"
title = "Set up"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[[steps]]
id = "work"
title = "Do the work"
needs = ["setup"]
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[[steps]]
id = "body"
title = "Scope body"
needs = ["setup", "work"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "main", "gc.scope_role" = "body" }
`

const memberMetadata = `{ "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }`

// templatedMember is a child whose member picks its scope with a variable.
const templatedMember = `
[vars.scope]
description = "Scope the published step joins"
default = "body"

[[steps]]
id = "publish"
title = "Publish"
needs = ["work"]
metadata = { "gc.scope_ref" = "{{scope}}", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }
`

type scopeMembershipCase struct {
	name  string
	child string   // steps appended under the child's header
	want  []string // substrings the error must contain; empty means Resolve must succeed
}

func resolveScopeFixture(t *testing.T, name string, searchPaths ...string) (*Formula, error) {
	t.Helper()
	p := NewParser(searchPaths...)
	f, err := p.LoadByName(name)
	if err != nil {
		t.Fatalf("LoadByName(%q): %v", name, err)
	}
	return p.Resolve(f)
}

func requireScopeResolve(t *testing.T, err error, want []string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Fatalf("Resolve failed, want success: %v", err)
		}
		return
	}
	for _, substr := range want {
		requireErrorContains(t, err, substr)
	}
}

// runScopedBaseCases resolves each case's child formula against scopedBaseFormula.
func runScopedBaseCases(t *testing.T, cases []scopeMembershipCase) {
	t.Helper()
	enableV2ForTest(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeNamedFormula(t, dir, "scoped-base", scopedBaseFormula)
			writeNamedFormula(t, dir, "child", "\nformula = \"child\"\nextends = [\"scoped-base\"]\n"+tc.child)

			_, err := resolveScopeFixture(t, "child", dir)
			requireScopeResolve(t, err, tc.want)
		})
	}
}

func TestResolveScopeMembershipAcrossExtends(t *testing.T) {
	runScopedBaseCases(t, []scopeMembershipCase{
		{
			name: "override_repointed_at_a_scope_the_child_declares_passes",
			child: `
[[steps]]
id = "other"
title = "Second scope"
metadata = { "gc.kind" = "scope", "gc.scope_name" = "other", "gc.scope_role" = "body" }

[[steps]]
id = "setup"
title = "Set up under the second scope"
metadata = { "gc.scope_ref" = "other", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }
`,
		},

		// Dangling scope_ref: the body a member names must exist in the merged formula.
		{
			name: "override_repointed_at_a_missing_scope_is_rejected",
			child: `
[[steps]]
id = "setup"
title = "Set up under nothing"
metadata = { "gc.scope_ref" = "missing", "gc.scope_role" = "setup" }
`,
			want: []string{`formula "child"`, `step "setup"`, `"missing"`, `declares no scope body`},
		},
		{
			name: "scope_ref_naming_a_step_that_is_not_a_scope_body_is_rejected",
			child: `
[[steps]]
id = "work"
title = "Work under a plain step"
needs = ["setup"]
metadata = { "gc.scope_ref" = "setup", "gc.scope_role" = "member" }
`,
			want: []string{`formula "child"`, `step "work"`, `is not a scope body`},
		},

		// Cook substitutes metadata values after Resolve, so a placeholder cannot
		// be judged here: the formula below cooks today with scope = "body".
		{
			name:  "templated_scope_ref_is_left_for_cook_to_substitute",
			child: templatedMember,
		},
		{
			name: "literal_missing_ref_is_still_rejected_beside_a_templated_one",
			child: templatedMember + `
[[steps]]
id = "audit"
title = "Audit"
needs = ["work"]
metadata = { "gc.scope_ref" = "missing", "gc.scope_role" = "member" }
`,
			want: []string{`step "audit"`, `"missing"`},
		},

		// Reporting: every violation, not only the first.
		{
			name: "every_violation_is_reported_not_just_the_first",
			child: `
[[steps]]
id = "setup"
title = "Set up under nothing"
metadata = { "gc.scope_ref" = "missing", "gc.scope_role" = "setup" }

[[steps]]
id = "publish"
title = "Publish under nothing"
needs = ["work"]
metadata = { "gc.scope_ref" = "absent", "gc.scope_role" = "member" }
`,
			want: []string{`step "setup"`, `"missing"`, `step "publish"`, `"absent"`},
		},
	})
}

// Resolve runs only the dangling-reference check. A formula the other two
// checks would reject still resolves, so an extender written before its base
// declared scope membership keeps cooking. Enabling them is ga-prk8k5, which
// replaces these cases with rejections.
func TestResolveScopeMembershipEnforcesOnlyDanglingRefs(t *testing.T) {
	runScopedBaseCases(t, []scopeMembershipCase{
		{
			name: "override_dropping_inherited_scope_ref_is_accepted",
			child: `
[[steps]]
id = "setup"
title = "Set up differently"
`,
		},
		{
			name: "unmarked_sink_downstream_of_a_member_is_accepted",
			child: `
[[steps]]
id = "publish"
title = "Publish"
needs = ["work"]
`,
		},
	})
}

// A base whose scope body is missing or named differently is the cross-repo
// skew case: the extender's own file never changed, so nothing but this check
// tells its author before the first run.
func TestResolveScopeMembershipRejectsExtendingABaseWithoutTheBody(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "unbodied-base", `
formula = "unbodied-base"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "work"
title = "Do the work"
metadata = `+memberMetadata+`
`)
	writeNamedFormula(t, dir, "extender", `
formula = "extender"
extends = ["unbodied-base"]

[[steps]]
id = "extra"
title = "Extra"
`)

	_, err := resolveScopeFixture(t, "extender", dir)
	requireErrorContains(t, err, `formula "extender"`)
	requireErrorContains(t, err, `step "work"`)
	requireErrorContains(t, err, `declares no scope body named "body"`)
}

// A formula that declares no scope must resolve exactly as it did before the
// checks existed, however it overrides and extends.
func TestResolveScopeMembershipIsInertWithoutScopes(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "plain-base", `
formula = "plain-base"

[[steps]]
id = "a"
title = "A"

[[steps]]
id = "b"
title = "B"
needs = ["a"]
`)
	writeNamedFormula(t, dir, "plain-child", `
formula = "plain-child"
extends = ["plain-base"]

[[steps]]
id = "a"
title = "A, overridden"

[[steps]]
id = "c"
title = "C, a new terminal step"
needs = ["b"]
`)

	if _, err := resolveScopeFixture(t, "plain-child", dir); err != nil {
		t.Fatalf("Resolve failed for a formula with no scope: %v", err)
	}
}

// The checks guard a merge; a formula with no `extends` has nothing merged
// under it, so it is left alone.
func TestResolveScopeMembershipDoesNotCheckFormulasWithoutExtends(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "standalone", `
formula = "standalone"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "work"
title = "Do the work"
metadata = `+memberMetadata+`
`)

	if _, err := resolveScopeFixture(t, "standalone", dir); err != nil {
		t.Fatalf("Resolve failed for a standalone formula, want the checks skipped: %v", err)
	}
}

// mol-scoped-work is the canonical scoped formula shipped in the core bundle;
// extending it is the real-world shape these checks protect.
func TestResolveScopeMembershipAgainstBundledScopedWork(t *testing.T) {
	enableV2ForTest(t)
	core := filepath.Join("..", "bootstrap", "packs", "core", "formulas")

	cases := []scopeMembershipCase{
		{
			name: "extension_adding_a_member_passes",
			child: `
[[steps]]
id = "announce"
title = "Announce"
needs = ["submit"]
metadata = ` + memberMetadata + `
`,
		},
		{
			name: "extension_overriding_a_member_and_redeclaring_it_passes",
			child: `
[[steps]]
id = "implement"
title = "Implement differently"
needs = ["preflight-tests"]
metadata = ` + memberMetadata + `
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeNamedFormula(t, dir, "scoped-work-child", "\nformula = \"scoped-work-child\"\nextends = [\"mol-scoped-work\"]\n"+tc.child)

			_, err := resolveScopeFixture(t, "scoped-work-child", core, dir)
			requireScopeResolve(t, err, tc.want)
		})
	}
}

// The checks below take constructed steps rather than loaded formulas, so each
// stays pinned whether or not Resolve calls it.

func scopeStep(id string, metadata map[string]string, needs ...string) *Step {
	return &Step{ID: id, Title: id, Metadata: metadata, Needs: needs}
}

// memberOf is the metadata of a step that joins the named scope.
func memberOf(scope string) map[string]string {
	return map[string]string{
		beadmeta.ScopeRefMetadataKey:  scope,
		beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleMember,
	}
}

// scopedParent mirrors scopedBaseFormula: setup and work join the scope body.
func scopedParent() []*Step {
	return []*Step{
		scopeStep("setup", memberOf("body")),
		scopeStep("work", memberOf("body"), "setup"),
		scopeStep("body", map[string]string{
			beadmeta.KindMetadataKey:      beadmeta.KindScope,
			beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleBody,
		}, "setup", "work"),
	}
}

// requireScopeProblems wants exactly len(want) problems, each containing one of
// the want substrings.
func requireScopeProblems(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d:\n  - %s", len(got), len(want), strings.Join(got, "\n  - "))
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.Contains(g, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no problem contains %q; got:\n  - %s", w, strings.Join(got, "\n  - "))
		}
	}
}

func TestDroppedScopeRefs(t *testing.T) {
	cases := []struct {
		name   string
		parent []*Step // scopedParent() when nil
		child  []*Step
		want   []string
	}{
		{
			name:  "override_dropping_inherited_scope_ref_is_reported",
			child: []*Step{scopeStep("setup", nil)},
			want:  []string{`step "setup" overrides an inherited scope member and drops its gc.scope_ref "body"`},
		},
		{
			name:  "override_redeclaring_scope_ref_passes",
			child: []*Step{scopeStep("setup", memberOf("body"))},
		},
		{
			name:  "override_repointed_at_another_scope_passes",
			child: []*Step{scopeStep("setup", memberOf("other"))},
		},
		{
			name:  "override_with_explicit_empty_scope_ref_opts_out",
			child: []*Step{scopeStep("setup", map[string]string{beadmeta.ScopeRefMetadataKey: ""})},
		},
		{
			name:  "untouched_inherited_member_passes",
			child: []*Step{scopeStep("extra", nil)},
		},
		{
			name:   "override_of_a_step_that_was_never_a_member_passes",
			parent: append(scopedParent(), scopeStep("notify", nil)),
			child:  []*Step{scopeStep("notify", nil)},
		},
		{
			name:   "override_of_a_step_that_opted_out_passes",
			parent: append(scopedParent(), scopeStep("audit", map[string]string{beadmeta.ScopeRefMetadataKey: ""})),
			child:  []*Step{scopeStep("audit", nil)},
		},
		{
			name:  "every_dropped_member_is_reported",
			child: []*Step{scopeStep("setup", nil), scopeStep("work", nil, "setup")},
			want:  []string{`step "setup" overrides`, `step "work" overrides`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := tc.parent
			if parent == nil {
				parent = scopedParent()
			}
			got := droppedScopeRefs(mergeSteps(parent, tc.child), inheritedScopeRefs(parent))
			requireScopeProblems(t, got, tc.want)
		})
	}
}

func TestDanglingScopeRefs(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want []string
	}{
		{name: "ref_naming_the_scope_body_passes", ref: "body"},
		{
			name: "ref_naming_no_scope_body_is_reported",
			ref:  "missing",
			want: []string{`step "publish" has gc.scope_ref "missing"`},
		},
		{name: "empty_ref_is_the_explicit_opt_out", ref: ""},
		{name: "placeholder_ref_is_left_for_cook_to_substitute", ref: "{{scope}}"},
		{name: "placeholder_inside_a_longer_ref_is_left_for_cook_to_substitute", ref: "scope-{{suffix}}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := append(scopedParent(), scopeStep("publish", memberOf(tc.ref), "work"))
			requireScopeProblems(t, danglingScopeRefs(steps), tc.want)
		})
	}
}

func TestUncoveredSinks(t *testing.T) {
	const publishEscapesBody = `step "publish" is a sink downstream of scope "body"`

	cases := []struct {
		name  string
		extra []*Step // appended to scopedParent()
		want  []string
	}{
		{name: "scoped_formula_alone_has_only_its_scope_body_as_a_sink"},
		{
			name:  "sink_downstream_of_a_member_is_reported",
			extra: []*Step{scopeStep("publish", nil, "work")},
			want:  []string{publishEscapesBody},
		},
		{
			name:  "sink_hanging_off_the_body_is_reported",
			extra: []*Step{scopeStep("after-body", nil, "body")},
			want:  []string{`step "after-body" is a sink downstream of scope "body"`},
		},
		{
			name: "sink_several_hops_below_a_member_is_reported",
			extra: []*Step{
				scopeStep("first", nil, "work"),
				scopeStep("second", nil, "first"),
				scopeStep("publish", nil, "second"),
			},
			want: []string{publishEscapesBody},
		},
		{
			name: "sink_several_hops_below_the_body_is_reported",
			extra: []*Step{
				scopeStep("first", nil, "body"),
				scopeStep("publish", nil, "first"),
			},
			want: []string{publishEscapesBody},
		},
		{
			name:  "sink_depending_through_depends_on_is_reported",
			extra: []*Step{{ID: "publish", Title: "publish", DependsOn: []string{"work"}}},
			want:  []string{publishEscapesBody},
		},
		{
			name:  "sink_declaring_membership_passes",
			extra: []*Step{scopeStep("publish", memberOf("body"), "work")},
		},
		{
			name:  "sink_with_explicit_empty_scope_ref_opts_out",
			extra: []*Step{scopeStep("publish", map[string]string{beadmeta.ScopeRefMetadataKey: ""}, "work")},
		},
		{
			name:  "sink_independent_of_the_scope_passes",
			extra: []*Step{scopeStep("notify", nil)},
		},
		{
			name: "teardown_after_the_body_passes",
			extra: []*Step{scopeStep("cleanup", map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindCleanup,
				beadmeta.ScopeRefMetadataKey:  "body",
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
			}, "body")},
		},
		{
			name: "unmarked_step_between_members_is_not_a_sink_and_passes",
			extra: []*Step{
				scopeStep("mid", nil, "work"),
				scopeStep("publish", memberOf("body"), "mid"),
			},
		},
		{
			name: "every_uncovered_sink_is_reported",
			extra: []*Step{
				scopeStep("publish", nil, "work"),
				scopeStep("after-body", nil, "body"),
			},
			want: []string{publishEscapesBody, `step "after-body" is a sink downstream of scope "body"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			top := append(scopedParent(), tc.extra...)
			requireScopeProblems(t, uncoveredSinks(top, collectGraphSteps(top)), tc.want)
		})
	}
}
