package formula

import (
	"path/filepath"
	"testing"
)

// mergeSteps replaces an overridden step wholesale, so a scope member's
// gc.scope_ref vanishes from an override unless the child redeclares it. These
// tests pin the three structural checks that make scope membership survive
// `extends`: an override may not drop it silently, a sink downstream of a scope
// may not sit outside it, and every gc.scope_ref must name a scope body.
//
// Every table case extends scopedBaseFormula, a minimal scoped formula whose
// only scope body is "body".

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

func TestResolveScopeMembershipAcrossExtends(t *testing.T) {
	enableV2ForTest(t)

	cases := []scopeMembershipCase{
		// Override regression: the child's step replaces the parent's wholesale.
		{
			name: "override_dropping_inherited_scope_ref_is_rejected",
			child: `
[[steps]]
id = "setup"
title = "Set up differently"
`,
			want: []string{`formula "child"`, `step "setup"`, `drops its gc.scope_ref "body"`},
		},
		{
			name: "override_redeclaring_scope_ref_passes",
			child: `
[[steps]]
id = "setup"
title = "Set up differently"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }
`,
		},
		{
			name: "override_with_explicit_empty_scope_ref_opts_out",
			child: `
[[steps]]
id = "setup"
title = "Set up outside the scope"
metadata = { "gc.scope_ref" = "" }
`,
		},
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

		// Uncovered sink: terminal work downstream of a scope must join it.
		{
			name: "new_sink_downstream_of_a_member_without_membership_is_rejected",
			child: `
[[steps]]
id = "publish"
title = "Publish"
needs = ["work"]
`,
			want: []string{`formula "child"`, `step "publish"`, `scope "body"`, `is a sink downstream of scope`},
		},
		{
			name: "new_sink_declaring_membership_passes",
			child: `
[[steps]]
id = "publish"
title = "Publish"
needs = ["work"]
metadata = ` + memberMetadata + `
`,
		},
		{
			name: "new_sink_with_explicit_empty_scope_ref_opts_out",
			child: `
[[steps]]
id = "publish"
title = "Publish outside the scope"
needs = ["work"]
metadata = { "gc.scope_ref" = "" }
`,
		},
		{
			name: "sink_independent_of_the_scope_passes",
			child: `
[[steps]]
id = "notify"
title = "Notify"
`,
		},
		{
			name: "sink_hanging_off_the_scope_body_is_rejected",
			child: `
[[steps]]
id = "after-body"
title = "After the scope"
needs = ["body"]
`,
			want: []string{`step "after-body"`, `scope "body"`, `is a sink downstream of scope`},
		},
		{
			name: "teardown_after_the_body_passes",
			child: `
[[steps]]
id = "cleanup"
title = "Tear down"
needs = ["body"]
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }
`,
		},
		{
			name: "unmarked_step_between_members_is_not_a_sink_and_passes",
			child: `
[[steps]]
id = "mid"
title = "Middle"
needs = ["work"]

[[steps]]
id = "publish"
title = "Publish"
needs = ["mid"]
metadata = ` + memberMetadata + `
`,
		},

		// Reporting: every violation, not only the first.
		{
			name: "every_violation_is_reported_not_just_the_first",
			child: `
[[steps]]
id = "setup"
title = "Set up differently"

[[steps]]
id = "publish"
title = "Publish"
needs = ["work"]
`,
			want: []string{`step "setup"`, `step "publish"`},
		},
	}

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

func TestResolveScopeMembershipHoldsThroughAnInheritanceChain(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "scoped-base", scopedBaseFormula)
	writeNamedFormula(t, dir, "scoped-mid", `
formula = "scoped-mid"
extends = ["scoped-base"]

[[steps]]
id = "report"
title = "Report"
needs = ["work"]
metadata = `+memberMetadata+`
`)
	writeNamedFormula(t, dir, "scoped-leaf", `
formula = "scoped-leaf"
extends = ["scoped-mid"]

[[steps]]
id = "work"
title = "Work differently"
`)

	// The middle formula only added a covered member: fine on its own.
	if _, err := resolveScopeFixture(t, "scoped-mid", dir); err != nil {
		t.Fatalf("scoped-mid Resolve failed, want success: %v", err)
	}
	// The leaf overrides a step the middle formula merely inherited.
	_, err := resolveScopeFixture(t, "scoped-leaf", dir)
	requireErrorContains(t, err, `formula "scoped-leaf"`)
	requireErrorContains(t, err, `step "work"`)
	requireErrorContains(t, err, `drops its gc.scope_ref "body"`)
}

func TestResolveScopeMembershipSeesEveryParent(t *testing.T) {
	enableV2ForTest(t)
	dir := t.TempDir()
	writeNamedFormula(t, dir, "plain-parent", `
formula = "plain-parent"

[[steps]]
id = "plain"
title = "Plain"
`)
	writeNamedFormula(t, dir, "scoped-base", scopedBaseFormula)
	writeNamedFormula(t, dir, "two-parents", `
formula = "two-parents"
extends = ["plain-parent", "scoped-base"]

[[steps]]
id = "work"
title = "Work differently"
needs = ["setup"]
`)

	_, err := resolveScopeFixture(t, "two-parents", dir)
	requireErrorContains(t, err, `formula "two-parents"`)
	requireErrorContains(t, err, `step "work"`)
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
			name: "extension_adding_an_unmarked_terminal_step_is_rejected",
			child: `
[[steps]]
id = "announce"
title = "Announce"
needs = ["submit"]
`,
			want: []string{`step "announce"`, `scope "body"`, `is a sink downstream of scope`},
		},
		{
			name: "extension_overriding_a_member_without_membership_is_rejected",
			child: `
[[steps]]
id = "implement"
title = "Implement differently"
needs = ["preflight-tests"]
`,
			want: []string{`step "implement"`, `drops its gc.scope_ref "body"`},
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
