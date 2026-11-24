package formula

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

var compileShapeGoldens = map[string]string{
	"top_level_check": `loop|gc.check_mode=exec,gc.check_path=scripts/pass.sh,gc.check_timeout=,gc.control_epoch=1,gc.kind=ralph,gc.max_attempts=2,gc.step_id=loop|needs=loop.iteration.1|deps=
loop.iteration.1|gc.attempt=1,gc.control_for=loop,gc.iteration=1,gc.kind=scope,gc.ralph_step_id=loop,gc.scope_name=loop,gc.scope_role=body,gc.step_id=loop,gc.step_ref=loop.iteration.1|needs=loop.iteration.1.m1,loop.iteration.1.m2|deps=
loop.iteration.1.m1|gc.attempt=1,gc.iteration=1,gc.on_fail=abort_scope,gc.ralph_step_id=loop,gc.scope_ref=loop.iteration.1,gc.scope_role=member,gc.step_id=m1,gc.step_ref=loop.iteration.1.m1|needs=|deps=
loop.iteration.1.m1-scope-check|gc.attempt=1,gc.control_for=loop.iteration.1.m1,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=loop,gc.scope_ref=loop.iteration.1,gc.scope_role=control,gc.step_id=m1|needs=loop.iteration.1.m1|deps=
loop.iteration.1.m2|gc.attempt=1,gc.iteration=1,gc.on_fail=abort_scope,gc.ralph_step_id=loop,gc.scope_ref=loop.iteration.1,gc.scope_role=member,gc.step_id=m2,gc.step_ref=loop.iteration.1.m2|needs=loop.iteration.1.m1-scope-check|deps=
loop.iteration.1.m2-scope-check|gc.attempt=1,gc.control_for=loop.iteration.1.m2,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=loop,gc.scope_ref=loop.iteration.1,gc.scope_role=control,gc.step_id=m2|needs=loop.iteration.1.m2|deps=
loop.spec|gc.kind=spec,gc.spec_for=loop,gc.spec_for_ref=loop|needs=|deps=
workflow-finalize|gc.kind=workflow-finalize|needs=loop,loop.iteration.1.m2-scope-check|deps=
`,
	"childless_check_in_check_body": `outer|gc.check_mode=exec,gc.check_path=scripts/pass.sh,gc.check_timeout=,gc.control_epoch=1,gc.kind=ralph,gc.max_attempts=2,gc.step_id=outer|needs=outer.iteration.1|deps=
outer.iteration.1|gc.attempt=1,gc.control_for=outer,gc.iteration=1,gc.kind=scope,gc.ralph_step_id=outer,gc.scope_name=outer,gc.scope_role=body,gc.step_id=outer,gc.step_ref=outer.iteration.1|needs=outer.iteration.1.inner,outer.iteration.1.inner.iteration.1,outer.iteration.1.o1|deps=
outer.iteration.1.inner|gc.attempt=1,gc.check_mode=exec,gc.check_path=scripts/pass.sh,gc.check_timeout=,gc.control_epoch=1,gc.iteration=1,gc.kind=ralph,gc.max_attempts=2,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=member,gc.step_id=inner,gc.step_ref=outer.iteration.1.inner|needs=outer.iteration.1.inner.iteration.1-scope-check|deps=
outer.iteration.1.inner-scope-check|gc.attempt=1,gc.control_for=outer.iteration.1.inner,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=control,gc.step_id=inner|needs=outer.iteration.1.inner|deps=
outer.iteration.1.inner.iteration.1|gc.attempt=1,gc.control_for=outer.iteration.1.inner,gc.iteration=1,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=member,gc.step_id=inner,gc.step_ref=outer.iteration.1.inner.iteration.1|needs=|deps=
outer.iteration.1.inner.iteration.1-scope-check|gc.attempt=1,gc.control_for=outer.iteration.1.inner.iteration.1,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=control,gc.step_id=inner|needs=outer.iteration.1.inner.iteration.1|deps=
outer.iteration.1.inner.spec|gc.kind=spec,gc.spec_for=inner,gc.spec_for_ref=outer.iteration.1.inner|needs=|deps=
outer.iteration.1.o1|gc.attempt=1,gc.iteration=1,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=member,gc.step_id=o1,gc.step_ref=outer.iteration.1.o1|needs=outer.iteration.1.inner-scope-check|deps=
outer.iteration.1.o1-scope-check|gc.attempt=1,gc.control_for=outer.iteration.1.o1,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=control,gc.step_id=o1|needs=outer.iteration.1.o1|deps=
outer.spec|gc.kind=spec,gc.spec_for=outer,gc.spec_for_ref=outer|needs=|deps=
workflow-finalize|gc.kind=workflow-finalize|needs=outer,outer.iteration.1.o1-scope-check|deps=
`,
	"retry_in_check_body": `outer|gc.check_mode=exec,gc.check_path=scripts/pass.sh,gc.check_timeout=,gc.control_epoch=1,gc.kind=ralph,gc.max_attempts=2,gc.step_id=outer|needs=outer.iteration.1|deps=
outer.iteration.1|gc.attempt=1,gc.control_for=outer,gc.iteration=1,gc.kind=scope,gc.ralph_step_id=outer,gc.scope_name=outer,gc.scope_role=body,gc.step_id=outer,gc.step_ref=outer.iteration.1|needs=outer.iteration.1.r1,outer.iteration.1.r1.attempt.1|deps=
outer.iteration.1.r1|gc.attempt=1,gc.control_epoch=1,gc.iteration=1,gc.kind=retry,gc.max_attempts=2,gc.on_exhausted=hard_fail,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=member,gc.step_id=r1,gc.step_ref=outer.iteration.1.r1|needs=outer.iteration.1.r1.attempt.1-scope-check|deps=
outer.iteration.1.r1-scope-check|gc.attempt=1,gc.control_for=outer.iteration.1.r1,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=control,gc.step_id=r1|needs=outer.iteration.1.r1|deps=
outer.iteration.1.r1.attempt.1|gc.attempt=1,gc.control_for=outer.iteration.1.r1,gc.iteration=1,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.retry_attempt=1,gc.scope_ref=outer.iteration.1,gc.scope_role=member,gc.step_id=r1,gc.step_ref=outer.iteration.1.r1.attempt.1|needs=|deps=
outer.iteration.1.r1.attempt.1-scope-check|gc.attempt=1,gc.control_for=outer.iteration.1.r1.attempt.1,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=control,gc.step_id=r1|needs=outer.iteration.1.r1.attempt.1|deps=
outer.iteration.1.r1.spec|gc.kind=spec,gc.spec_for=r1,gc.spec_for_ref=outer.iteration.1.r1|needs=|deps=
outer.spec|gc.kind=spec,gc.spec_for=outer,gc.spec_for_ref=outer|needs=|deps=
workflow-finalize|gc.kind=workflow-finalize|needs=outer,outer.iteration.1.r1-scope-check|deps=
`,
	"authored_scope_in_check_body": `outer|gc.check_mode=exec,gc.check_path=scripts/pass.sh,gc.check_timeout=,gc.control_epoch=1,gc.kind=ralph,gc.max_attempts=2,gc.step_id=outer|needs=outer.iteration.1|deps=
outer.iteration.1|gc.attempt=1,gc.control_for=outer,gc.iteration=1,gc.kind=scope,gc.ralph_step_id=outer,gc.scope_name=outer,gc.scope_role=body,gc.step_id=outer,gc.step_ref=outer.iteration.1|needs=outer.iteration.1.body|deps=
outer.iteration.1.body|gc.attempt=1,gc.iteration=1,gc.kind=scope,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_name=worktree,gc.scope_ref=outer.iteration.1,gc.scope_role=body,gc.step_id=body,gc.step_ref=outer.iteration.1.body|needs=|deps=
outer.iteration.1.s1|gc.attempt=1,gc.iteration=1,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=member,gc.step_id=s1,gc.step_ref=outer.iteration.1.s1|needs=|deps=
outer.iteration.1.s1-scope-check|gc.attempt=1,gc.control_for=outer.iteration.1.s1,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=outer,gc.scope_ref=outer.iteration.1,gc.scope_role=control,gc.step_id=s1|needs=outer.iteration.1.s1|deps=
outer.spec|gc.kind=spec,gc.spec_for=outer,gc.spec_for_ref=outer|needs=|deps=
workflow-finalize|gc.kind=workflow-finalize|needs=outer,outer.iteration.1.body,outer.iteration.1.s1-scope-check|deps=
`,
	"check_in_authored_scope": `body|gc.kind=scope,gc.scope_name=worktree,gc.scope_role=body|needs=|deps=
loop|gc.check_mode=exec,gc.check_path=scripts/pass.sh,gc.check_timeout=,gc.control_epoch=1,gc.kind=ralph,gc.max_attempts=2,gc.on_fail=abort_scope,gc.scope_ref=body,gc.scope_role=member,gc.step_id=loop|needs=loop.iteration.1|deps=
loop-scope-check|gc.control_for=loop,gc.kind=scope-check,gc.on_fail=abort_scope,gc.scope_ref=body,gc.scope_role=control,gc.step_id=loop|needs=loop|deps=
loop.iteration.1|gc.attempt=1,gc.control_for=loop,gc.iteration=1,gc.kind=scope,gc.ralph_step_id=loop,gc.scope_name=loop,gc.scope_role=body,gc.step_id=loop,gc.step_ref=loop.iteration.1|needs=loop.iteration.1.m1|deps=
loop.iteration.1.m1|gc.attempt=1,gc.iteration=1,gc.on_fail=abort_scope,gc.ralph_step_id=loop,gc.scope_ref=loop.iteration.1,gc.scope_role=member,gc.step_id=m1,gc.step_ref=loop.iteration.1.m1|needs=|deps=
loop.iteration.1.m1-scope-check|gc.attempt=1,gc.control_for=loop.iteration.1.m1,gc.kind=scope-check,gc.on_fail=abort_scope,gc.ralph_step_id=loop,gc.scope_ref=loop.iteration.1,gc.scope_role=control,gc.step_id=m1|needs=loop.iteration.1.m1|deps=
loop.spec|gc.kind=spec,gc.spec_for=loop,gc.spec_for_ref=loop|needs=|deps=
workflow-finalize|gc.kind=workflow-finalize|needs=body,loop,loop-scope-check,loop.iteration.1.m1-scope-check|deps=
`,
}

// TestCompileUnaffectedShapesArePinned pins the compiled output of every shape
// WITHOUT a check-with-children nested in a check body (issue #9412 AC-2):
// the nested-loop identity fix must not change any other shape's metadata or
// edges. The goldens are the compile at the fix's base commit; the guard
// passes there and must keep passing after the change.
func TestCompileUnaffectedShapesArePinned(t *testing.T) {
	for _, shape := range []struct {
		name  string
		steps []*Step
	}{
		{name: "top_level_check", steps: topLevelCheckSteps()},
		{name: "childless_check_in_check_body", steps: childlessCheckInBodySteps()},
		{name: "retry_in_check_body", steps: retryInBodySteps()},
		{name: "authored_scope_in_check_body", steps: authoredScopeInBodySteps()},
		{name: "check_in_authored_scope", steps: checkInAuthoredScopeSteps()},
	} {
		t.Run(shape.name, func(t *testing.T) {
			got := dumpCompiledFormula(t, shape.steps)
			want := compileShapeGoldens[shape.name]
			if want == "" {
				t.Logf("golden dump for shape %q:\n%s", shape.name, got)
				t.Fatalf("missing golden for shape %q", shape.name)
			}
			if got != want {
				t.Errorf("compiled shape changed:\n--- want (base compile)\n%s\n--- got\n%s", want, got)
			}
		})
	}
}

// topLevelCheckSteps: one check loop with children, at the workflow top level.
func topLevelCheckSteps() []*Step {
	return []*Step{
		{
			ID:    "loop",
			Title: "Loop",
			Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{
				{ID: "m1", Title: "Member one"},
				{ID: "m2", Title: "Member two", Needs: []string{"m1"}},
			},
		},
	}
}

// childlessCheckInBodySteps: a check WITHOUT children nested in a check body.
func childlessCheckInBodySteps() []*Step {
	return []*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{
				{ID: "inner", Title: "Inner check", Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}}},
				{ID: "o1", Title: "Outer work", Needs: []string{"inner"}},
			},
		},
	}
}

// retryInBodySteps: a retry-managed step directly in a check body.
func retryInBodySteps() []*Step {
	return []*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{
				{ID: "r1", Title: "Retry work", Retry: &RetrySpec{MaxAttempts: 2}},
			},
		},
	}
}

// authoredScopeInBodySteps: an authored plain scope inside a check body, with
// one member declaring membership via its step-local scope ref.
func authoredScopeInBodySteps() []*Step {
	return []*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{
				{
					ID:       "body",
					Title:    "Work scope",
					Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope, beadmeta.ScopeNameMetadataKey: "worktree", beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleBody},
					Children: []*Step{
						{ID: "s1", Title: "Scope member", Metadata: map[string]string{beadmeta.ScopeRefMetadataKey: "body", beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleMember}},
					},
				},
			},
		},
	}
}

// checkInAuthoredScopeSteps: a check loop (with children) inside an authored
// plain scope at the workflow top level.
func checkInAuthoredScopeSteps() []*Step {
	return []*Step{
		{
			ID:       "body",
			Title:    "Work scope",
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope, beadmeta.ScopeNameMetadataKey: "worktree", beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleBody},
			Children: []*Step{
				{
					ID:    "loop",
					Title: "Loop",
					Metadata: map[string]string{
						beadmeta.ScopeRefMetadataKey:  "body",
						beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleMember,
						beadmeta.OnFailMetadataKey:    "abort_scope",
					},
					Ralph:    &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
					Children: []*Step{{ID: "m1", Title: "Member one"}},
				},
			},
		},
	}
}

// dumpCompiledFormula compiles steps the way the v2 compiler does and returns
// a canonical dump of every step: ID, sorted metadata, sorted needs, sorted
// depends_on. The dump is the whole observable compile for AC-2's purposes.
func dumpCompiledFormula(t *testing.T, steps []*Step) string {
	t.Helper()
	expanded, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}
	expanded, err = ApplyRalph(expanded)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	f := &Formula{Steps: expanded}
	ApplyGraphControls(f)

	all := collectGraphSteps(f.Steps)
	byID := make(map[string]*Step, len(all))
	for _, s := range all {
		byID[s.ID] = s
	}
	ids := make([]string, 0, len(all))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	for _, id := range ids {
		s := byID[id]
		keys := make([]string, 0, len(s.Metadata))
		for k := range s.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		meta := make([]string, 0, len(keys))
		for _, k := range keys {
			meta = append(meta, k+"="+s.Metadata[k])
		}
		fmt.Fprintf(&b, "%s|%s|needs=%s|deps=%s\n", id, strings.Join(meta, ","), strings.Join(sortedCopy(s.Needs), ","), strings.Join(sortedCopy(s.DependsOn), ","))
	}
	return b.String()
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
