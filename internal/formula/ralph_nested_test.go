package formula

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// nestedCheck returns a check-with-children step whose check always uses the
// same exec script; the script never runs at compile time.
func nestedCheck(id string, children ...*Step) *Step {
	return &Step{
		ID:       id,
		Title:    id,
		Type:     "task",
		Ralph:    &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
		Children: children,
	}
}

func nestedPlain(id string, needs ...string) *Step {
	return &Step{ID: id, Title: id, Type: "task", Needs: needs}
}

// compileNestedSteps runs the graph compile stages that shape loop membership:
// retries, then checks, then the minted graph controls and workflow-finalize.
func compileNestedSteps(t *testing.T, steps []*Step) []*Step {
	t.Helper()
	retried, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries: %v", err)
	}
	expanded, err := ApplyRalph(retried)
	if err != nil {
		t.Fatalf("ApplyRalph: %v", err)
	}
	f := &Formula{Steps: expanded}
	ApplyGraphControls(f)
	return collectGraphSteps(f.Steps)
}

func mustNestedStep(t *testing.T, steps []*Step, id string) *Step {
	t.Helper()
	step := findGraphStepByID(steps, id)
	if step == nil {
		ids := make([]string, 0, len(steps))
		for _, s := range steps {
			ids = append(ids, s.ID)
		}
		t.Fatalf("missing compiled step %q; have %v", id, ids)
	}
	return step
}

// assertNestedMeta checks each key of want on step; an empty want value means
// the key must be absent.
func assertNestedMeta(t *testing.T, step *Step, want map[string]string) {
	t.Helper()
	for key, value := range want {
		got, present := step.Metadata[key]
		if value == "" {
			if present {
				t.Errorf("%s %s = %q, want absent", step.ID, key, got)
			}
			continue
		}
		if got != value {
			t.Errorf("%s %s = %q, want %q", step.ID, key, got, value)
		}
	}
}

// TestApplyRalph_NestedCheckWithChildrenKeepsInnerLoopIdentity pins issue
// #9412: a check-with-children nested in another check's body keeps its own
// iteration scope and members. Only the inner control joins the enclosing
// iteration, as dispatch.buildAttemptRecipe builds it for outer iterations 2+.
func TestApplyRalph_NestedCheckWithChildrenKeepsInnerLoopIdentity(t *testing.T) {
	t.Parallel()

	const (
		outerIter = "outer.iteration.1"
		innerCtl  = outerIter + ".inner"
		innerIter = innerCtl + ".iteration.1"
	)
	innerMember := func(scopeRef, ralphStepID string) map[string]string {
		return map[string]string{
			beadmeta.ScopeRefMetadataKey:           scopeRef,
			beadmeta.RalphStepIDMetadataKey:        ralphStepID,
			beadmeta.ScopeRoleMetadataKey:          beadmeta.ScopeRoleMember,
			beadmeta.OnFailMetadataKey:             "abort_scope",
			beadmeta.AttemptMetadataKey:            "1",
			beadmeta.IterationMetadataKey:          "1",
			beadmeta.StepIDMetadataKey:             "i1",
			beadmeta.StepRefMetadataKey:            scopeRef + ".i1",
			beadmeta.KindMetadataKey:               "",
			beadmeta.ControlForMetadataKey:         "",
			beadmeta.ScopeNameMetadataKey:          "",
			beadmeta.OutputJSONRequiredMetadataKey: "",
		}
	}
	innerScope := func(stepID, controlFor string) map[string]string {
		return map[string]string{
			beadmeta.KindMetadataKey:        beadmeta.KindScope,
			beadmeta.ScopeRoleMetadataKey:   beadmeta.ScopeRoleBody,
			beadmeta.ScopeNameMetadataKey:   stepID,
			beadmeta.StepIDMetadataKey:      stepID,
			beadmeta.RalphStepIDMetadataKey: stepID,
			beadmeta.ControlForMetadataKey:  controlFor,
			beadmeta.AttemptMetadataKey:     "1",
			beadmeta.IterationMetadataKey:   "1",
			beadmeta.ScopeRefMetadataKey:    "",
			beadmeta.OnFailMetadataKey:      "",
		}
	}
	innerControl := func(stepID, scopeRef, ralphStepID string) map[string]string {
		return map[string]string{
			beadmeta.KindMetadataKey:        beadmeta.KindRalph,
			beadmeta.StepIDMetadataKey:      stepID,
			beadmeta.ScopeRefMetadataKey:    scopeRef,
			beadmeta.ScopeRoleMetadataKey:   beadmeta.ScopeRoleMember,
			beadmeta.OnFailMetadataKey:      "abort_scope",
			beadmeta.RalphStepIDMetadataKey: ralphStepID,
		}
	}

	for _, tc := range []struct {
		name  string
		steps []*Step
		check func(t *testing.T, steps []*Step)
	}{
		{
			name: "two_level",
			steps: []*Step{nestedCheck("outer",
				nestedCheck("inner", nestedPlain("i1")),
				nestedPlain("o1", "inner"),
			)},
			check: func(t *testing.T, steps []*Step) {
				assertNestedMeta(t, mustNestedStep(t, steps, innerIter+".i1"), innerMember(innerIter, "inner"))
				assertNestedMeta(t, mustNestedStep(t, steps, innerIter+".i1-scope-check"), map[string]string{
					beadmeta.KindMetadataKey:        beadmeta.KindScopeCheck,
					beadmeta.ScopeRefMetadataKey:    innerIter,
					beadmeta.RalphStepIDMetadataKey: "inner",
				})
				scope := mustNestedStep(t, steps, innerIter)
				assertNestedMeta(t, scope, innerScope("inner", innerCtl))
				if !containsString(scope.Needs, innerIter+".i1") || containsString(scope.Needs, innerIter+".i1-scope-check") {
					t.Errorf("inner iteration scope needs = %v, want the raw member %s.i1, not its scope-check", scope.Needs, innerIter)
				}
				assertNestedMeta(t, mustNestedStep(t, steps, innerCtl), innerControl("inner", outerIter, "outer"))
				o1 := mustNestedStep(t, steps, outerIter+".o1")
				if !containsString(o1.Needs, innerCtl+"-scope-check") {
					t.Errorf("o1 needs = %v, want the inner control's scope-check %s-scope-check", o1.Needs, innerCtl)
				}
			},
		},
		{
			name: "o1_parallel",
			steps: []*Step{nestedCheck("outer",
				nestedCheck("inner", nestedPlain("i1")),
				nestedPlain("o1"),
			)},
			check: func(t *testing.T, steps []*Step) {
				assertNestedMeta(t, mustNestedStep(t, steps, innerIter+".i1"), innerMember(innerIter, "inner"))
				assertNestedMeta(t, mustNestedStep(t, steps, innerIter), innerScope("inner", innerCtl))
				assertNestedMeta(t, mustNestedStep(t, steps, innerCtl), innerControl("inner", outerIter, "outer"))
			},
		},
		{
			name: "three_level",
			steps: []*Step{nestedCheck("outer",
				nestedCheck("mid", nestedCheck("inner", nestedPlain("i1"))),
			)},
			check: func(t *testing.T, steps []*Step) {
				const (
					midCtl     = outerIter + ".mid"
					midIter    = midCtl + ".iteration.1"
					deepCtl    = midIter + ".inner"
					deepIter   = deepCtl + ".iteration.1"
					deepMember = deepIter + ".i1"
				)
				assertNestedMeta(t, mustNestedStep(t, steps, deepMember), innerMember(deepIter, "inner"))
				assertNestedMeta(t, mustNestedStep(t, steps, deepMember+"-scope-check"), map[string]string{
					beadmeta.ScopeRefMetadataKey:    deepIter,
					beadmeta.RalphStepIDMetadataKey: "inner",
				})
				assertNestedMeta(t, mustNestedStep(t, steps, deepIter), innerScope("inner", deepCtl))
				assertNestedMeta(t, mustNestedStep(t, steps, deepCtl), innerControl("inner", midIter, "mid"))
				assertNestedMeta(t, mustNestedStep(t, steps, midIter), innerScope("mid", midCtl))
				assertNestedMeta(t, mustNestedStep(t, steps, midCtl), innerControl("mid", outerIter, "outer"))
			},
		},
		{
			name: "retry_in_inner_body",
			steps: []*Step{nestedCheck("outer",
				nestedCheck("inner", &Step{ID: "r", Title: "r", Type: "task", Retry: &RetrySpec{MaxAttempts: 2}}),
				nestedPlain("o1", "inner"),
			)},
			check: func(t *testing.T, steps []*Step) {
				assertNestedMeta(t, mustNestedStep(t, steps, innerIter+".r"), map[string]string{
					beadmeta.KindMetadataKey:        beadmeta.KindRetry,
					beadmeta.ScopeRefMetadataKey:    innerIter,
					beadmeta.RalphStepIDMetadataKey: "inner",
					beadmeta.StepIDMetadataKey:      "r",
				})
				assertNestedMeta(t, mustNestedStep(t, steps, innerIter+".r.attempt.1"), map[string]string{
					beadmeta.ScopeRefMetadataKey:     innerIter,
					beadmeta.RalphStepIDMetadataKey:  "inner",
					beadmeta.ControlForMetadataKey:   innerIter + ".r",
					beadmeta.RetryAttemptMetadataKey: "1",
				})
				assertNestedMeta(t, mustNestedStep(t, steps, innerIter), innerScope("inner", innerCtl))
				assertNestedMeta(t, mustNestedStep(t, steps, innerCtl), innerControl("inner", outerIter, "outer"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.check(t, compileNestedSteps(t, tc.steps))
		})
	}
}

// renderCompiledSteps is a stable dump of every compiled step's identity,
// edges and metadata, one line per step in compile order.
func renderCompiledSteps(steps []*Step) string {
	var b strings.Builder
	for _, step := range steps {
		fmt.Fprintf(&b, "%s needs=%v depends_on=%v", step.ID, step.Needs, step.DependsOn)
		keys := make([]string, 0, len(step.Metadata))
		for key := range step.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, " %s=%s", key, step.Metadata[key])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TestApplyRalph_ShapesWithoutNestedCheckBodyCompileUnchanged guards the
// #9412 fix's blast radius: a formula with no check-with-children nested in a
// check body compiles to exactly the steps, edges and metadata it compiled to
// before the fix.
func TestApplyRalph_ShapesWithoutNestedCheckBodyCompileUnchanged(t *testing.T) {
	t.Parallel()

	plainScopeMember := func(step *Step) *Step {
		step.Metadata = map[string]string{
			beadmeta.ScopeRefMetadataKey:  "body",
			beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleMember,
		}
		return step
	}
	plainScopeBody := func(needs ...string) *Step {
		step := nestedPlain("body", needs...)
		step.Metadata = map[string]string{
			beadmeta.KindMetadataKey:      beadmeta.KindScope,
			beadmeta.ScopeNameMetadataKey: "worktree",
			beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleBody,
		}
		return step
	}
	childless := func(id string) *Step {
		step := nestedCheck(id)
		step.Children = nil
		return step
	}

	for _, tc := range []struct {
		name  string
		steps []*Step
		want  string
	}{
		{
			name:  "top_level_check",
			steps: []*Step{nestedCheck("loop", nestedPlain("m1"), nestedPlain("m2", "m1")), nestedPlain("after", "loop")},
			want: `
loop needs=[loop.iteration.1] depends_on=[] gc.check_mode=exec gc.check_path=scripts/pass.sh gc.check_timeout= gc.control_epoch=1 gc.kind=ralph gc.max_attempts=2 gc.step_id=loop
loop.spec needs=[] depends_on=[] gc.kind=spec gc.spec_for=loop gc.spec_for_ref=loop
loop.iteration.1.m1 needs=[] depends_on=[] gc.attempt=1 gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=loop gc.scope_ref=loop.iteration.1 gc.scope_role=member gc.step_id=m1 gc.step_ref=loop.iteration.1.m1
after needs=[loop] depends_on=[]
loop.iteration.1.m1-scope-check needs=[loop.iteration.1.m1] depends_on=[] gc.attempt=1 gc.control_for=loop.iteration.1.m1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=loop gc.scope_ref=loop.iteration.1 gc.scope_role=control gc.step_id=m1
loop.iteration.1.m2 needs=[loop.iteration.1.m1-scope-check] depends_on=[] gc.attempt=1 gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=loop gc.scope_ref=loop.iteration.1 gc.scope_role=member gc.step_id=m2 gc.step_ref=loop.iteration.1.m2
loop.iteration.1 needs=[loop.iteration.1.m1 loop.iteration.1.m2] depends_on=[] gc.attempt=1 gc.control_for=loop gc.iteration=1 gc.kind=scope gc.ralph_step_id=loop gc.scope_name=loop gc.scope_role=body gc.step_id=loop gc.step_ref=loop.iteration.1
loop.iteration.1.m2-scope-check needs=[loop.iteration.1.m2] depends_on=[] gc.attempt=1 gc.control_for=loop.iteration.1.m2 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=loop gc.scope_ref=loop.iteration.1 gc.scope_role=control gc.step_id=m2
workflow-finalize needs=[loop after loop.iteration.1.m2-scope-check] depends_on=[] gc.kind=workflow-finalize
`,
		},
		{
			name:  "childless_check_in_check_body",
			steps: []*Step{nestedCheck("outer", childless("inner"), nestedPlain("o1", "inner"))},
			want: `
outer needs=[outer.iteration.1] depends_on=[] gc.check_mode=exec gc.check_path=scripts/pass.sh gc.check_timeout= gc.control_epoch=1 gc.kind=ralph gc.max_attempts=2 gc.step_id=outer
outer.spec needs=[] depends_on=[] gc.kind=spec gc.spec_for=outer gc.spec_for_ref=outer
outer.iteration.1.inner.spec needs=[] depends_on=[] gc.kind=spec gc.spec_for=inner gc.spec_for_ref=outer.iteration.1.inner
outer.iteration.1.inner.iteration.1 needs=[] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.inner gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=inner gc.step_ref=outer.iteration.1.inner.iteration.1
outer.iteration.1.inner.iteration.1-scope-check needs=[outer.iteration.1.inner.iteration.1] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.inner.iteration.1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=inner
outer.iteration.1.inner needs=[outer.iteration.1.inner.iteration.1-scope-check] depends_on=[] gc.attempt=1 gc.check_mode=exec gc.check_path=scripts/pass.sh gc.check_timeout= gc.control_epoch=1 gc.iteration=1 gc.kind=ralph gc.max_attempts=2 gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=inner gc.step_ref=outer.iteration.1.inner
outer.iteration.1.inner-scope-check needs=[outer.iteration.1.inner] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.inner gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=inner
outer.iteration.1.o1 needs=[outer.iteration.1.inner-scope-check] depends_on=[] gc.attempt=1 gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=o1 gc.step_ref=outer.iteration.1.o1
outer.iteration.1 needs=[outer.iteration.1.inner outer.iteration.1.inner.iteration.1 outer.iteration.1.o1] depends_on=[] gc.attempt=1 gc.control_for=outer gc.iteration=1 gc.kind=scope gc.ralph_step_id=outer gc.scope_name=outer gc.scope_role=body gc.step_id=outer gc.step_ref=outer.iteration.1
outer.iteration.1.o1-scope-check needs=[outer.iteration.1.o1] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.o1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=o1
workflow-finalize needs=[outer outer.iteration.1.o1-scope-check] depends_on=[] gc.kind=workflow-finalize
`,
		},
		{
			name: "retry_in_check_body",
			steps: []*Step{nestedCheck("outer",
				&Step{ID: "r", Title: "r", Type: "task", Retry: &RetrySpec{MaxAttempts: 2}},
				nestedPlain("o1", "r"),
			)},
			want: `
outer needs=[outer.iteration.1] depends_on=[] gc.check_mode=exec gc.check_path=scripts/pass.sh gc.check_timeout= gc.control_epoch=1 gc.kind=ralph gc.max_attempts=2 gc.step_id=outer
outer.spec needs=[] depends_on=[] gc.kind=spec gc.spec_for=outer gc.spec_for_ref=outer
outer.iteration.1.r.spec needs=[] depends_on=[] gc.kind=spec gc.spec_for=r gc.spec_for_ref=outer.iteration.1.r
outer.iteration.1.r.attempt.1 needs=[] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.r gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=outer gc.retry_attempt=1 gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=r gc.step_ref=outer.iteration.1.r.attempt.1
outer.iteration.1.r.attempt.1-scope-check needs=[outer.iteration.1.r.attempt.1] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.r.attempt.1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=r
outer.iteration.1.r needs=[outer.iteration.1.r.attempt.1-scope-check] depends_on=[] gc.attempt=1 gc.control_epoch=1 gc.iteration=1 gc.kind=retry gc.max_attempts=2 gc.on_exhausted=hard_fail gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=r gc.step_ref=outer.iteration.1.r
outer.iteration.1.r-scope-check needs=[outer.iteration.1.r] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.r gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=r
outer.iteration.1.o1 needs=[outer.iteration.1.r-scope-check] depends_on=[] gc.attempt=1 gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=o1 gc.step_ref=outer.iteration.1.o1
outer.iteration.1 needs=[outer.iteration.1.r outer.iteration.1.r.attempt.1 outer.iteration.1.o1] depends_on=[] gc.attempt=1 gc.control_for=outer gc.iteration=1 gc.kind=scope gc.ralph_step_id=outer gc.scope_name=outer gc.scope_role=body gc.step_id=outer gc.step_ref=outer.iteration.1
outer.iteration.1.o1-scope-check needs=[outer.iteration.1.o1] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.o1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=o1
workflow-finalize needs=[outer outer.iteration.1.o1-scope-check] depends_on=[] gc.kind=workflow-finalize
`,
		},
		{
			name: "plain_scope_in_check_body",
			steps: []*Step{nestedCheck("outer",
				plainScopeMember(nestedPlain("w1")),
				plainScopeBody("w1"),
				nestedPlain("o1", "body"),
			)},
			want: `
outer needs=[outer.iteration.1] depends_on=[] gc.check_mode=exec gc.check_path=scripts/pass.sh gc.check_timeout= gc.control_epoch=1 gc.kind=ralph gc.max_attempts=2 gc.step_id=outer
outer.spec needs=[] depends_on=[] gc.kind=spec gc.spec_for=outer gc.spec_for_ref=outer
outer.iteration.1.w1 needs=[] depends_on=[] gc.attempt=1 gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=w1 gc.step_ref=outer.iteration.1.w1
outer.iteration.1.w1-scope-check needs=[outer.iteration.1.w1] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.w1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=w1
outer.iteration.1.body needs=[outer.iteration.1.w1-scope-check] depends_on=[] gc.attempt=1 gc.iteration=1 gc.kind=scope gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_name=worktree gc.scope_ref=outer.iteration.1 gc.scope_role=body gc.step_id=body gc.step_ref=outer.iteration.1.body
outer.iteration.1.o1 needs=[outer.iteration.1.body] depends_on=[] gc.attempt=1 gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=member gc.step_id=o1 gc.step_ref=outer.iteration.1.o1
outer.iteration.1 needs=[outer.iteration.1.w1 outer.iteration.1.body outer.iteration.1.o1] depends_on=[] gc.attempt=1 gc.control_for=outer gc.iteration=1 gc.kind=scope gc.ralph_step_id=outer gc.scope_name=outer gc.scope_role=body gc.step_id=outer gc.step_ref=outer.iteration.1
outer.iteration.1.o1-scope-check needs=[outer.iteration.1.o1] depends_on=[] gc.attempt=1 gc.control_for=outer.iteration.1.o1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=outer gc.scope_ref=outer.iteration.1 gc.scope_role=control gc.step_id=o1
workflow-finalize needs=[outer outer.iteration.1.body outer.iteration.1.o1-scope-check] depends_on=[] gc.kind=workflow-finalize
`,
		},
		{
			name: "check_in_plain_scope",
			steps: []*Step{
				plainScopeMember(nestedCheck("loop", nestedPlain("m1"))),
				plainScopeBody("loop"),
			},
			want: `
loop needs=[loop.iteration.1] depends_on=[] gc.check_mode=exec gc.check_path=scripts/pass.sh gc.check_timeout= gc.control_epoch=1 gc.kind=ralph gc.max_attempts=2 gc.scope_ref=body gc.scope_role=member gc.step_id=loop
loop.spec needs=[] depends_on=[] gc.kind=spec gc.spec_for=loop gc.spec_for_ref=loop
loop.iteration.1.m1 needs=[] depends_on=[] gc.attempt=1 gc.iteration=1 gc.on_fail=abort_scope gc.ralph_step_id=loop gc.scope_ref=loop.iteration.1 gc.scope_role=member gc.step_id=m1 gc.step_ref=loop.iteration.1.m1
loop.iteration.1 needs=[loop.iteration.1.m1] depends_on=[] gc.attempt=1 gc.control_for=loop gc.iteration=1 gc.kind=scope gc.ralph_step_id=loop gc.scope_name=loop gc.scope_role=body gc.step_id=loop gc.step_ref=loop.iteration.1
body needs=[loop] depends_on=[] gc.kind=scope gc.scope_name=worktree gc.scope_role=body
loop-scope-check needs=[loop] depends_on=[] gc.control_for=loop gc.kind=scope-check gc.scope_ref=body gc.scope_role=control gc.step_id=loop
loop.iteration.1.m1-scope-check needs=[loop.iteration.1.m1] depends_on=[] gc.attempt=1 gc.control_for=loop.iteration.1.m1 gc.kind=scope-check gc.on_fail=abort_scope gc.ralph_step_id=loop gc.scope_ref=loop.iteration.1 gc.scope_role=control gc.step_id=m1
workflow-finalize needs=[loop body loop-scope-check loop.iteration.1.m1-scope-check] depends_on=[] gc.kind=workflow-finalize
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := strings.TrimPrefix(tc.want, "\n")
			if got := renderCompiledSteps(compileNestedSteps(t, tc.steps)); got != want {
				t.Errorf("compiled steps changed:\n--- got\n%s--- want\n%s", got, want)
			}
		})
	}
}
