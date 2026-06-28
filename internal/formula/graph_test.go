package formula

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestApplyGraphControlsRecursesIntoNestedChildren(t *testing.T) {
	t.Parallel()

	f := &Formula{
		Steps: []*Step{
			{
				ID:    "parent",
				Title: "Parent",
				Children: []*Step{
					{
						ID:    "survey",
						Title: "Survey",
						OnComplete: &OnCompleteSpec{
							ForEach: "output.items",
							Bond:    "review-fragment",
						},
					},
					{
						ID:       "member",
						Title:    "Member",
						Metadata: map[string]string{"gc.scope_ref": "body", "gc.scope_role": "member"},
					},
				},
			},
		},
	}

	ApplyGraphControls(f)

	steps := collectGraphSteps(f.Steps)
	fanout := findGraphStepByID(steps, "survey-fanout")
	if fanout == nil {
		t.Fatal("missing nested survey-fanout control")
	}
	survey := findGraphStepByID(steps, "survey")
	if survey == nil {
		t.Fatal("missing nested survey step")
	}
	if got := survey.Metadata["gc.output_json_required"]; got != "true" {
		t.Fatalf("survey gc.output_json_required = %q, want true", got)
	}
	if got := fanout.Metadata["gc.kind"]; got != "fanout" {
		t.Fatalf("survey-fanout gc.kind = %q, want fanout", got)
	}
	if got := fanout.Metadata["gc.control_for"]; got != "survey" {
		t.Fatalf("survey-fanout gc.control_for = %q, want survey", got)
	}

	scopeCheck := findGraphStepByID(steps, "member-scope-check")
	if scopeCheck == nil {
		t.Fatal("missing nested member-scope-check control")
	}
	if got := scopeCheck.Metadata["gc.kind"]; got != "scope-check" {
		t.Fatalf("member-scope-check gc.kind = %q, want scope-check", got)
	}
	if got := scopeCheck.Metadata["gc.control_for"]; got != "member" {
		t.Fatalf("member-scope-check gc.control_for = %q, want member", got)
	}

	finalizer := findGraphStepByID(steps, "workflow-finalize")
	if finalizer == nil {
		t.Fatal("missing workflow-finalize")
	}
	if !containsString(finalizer.Needs, "survey-fanout") {
		t.Fatalf("workflow-finalize needs = %v, want nested fanout sink", finalizer.Needs)
	}
	if !containsString(finalizer.Needs, "member-scope-check") {
		t.Fatalf("workflow-finalize needs = %v, want nested scope-check sink", finalizer.Needs)
	}
}

func TestApplyGraphControlsRalphOnCompleteOnlyControlsLogicalStep(t *testing.T) {
	t.Parallel()

	f := &Formula{
		Steps: []*Step{
			{
				ID:    "review-loop",
				Title: "Review loop",
				OnComplete: &OnCompleteSpec{
					ForEach: "output.items",
					Bond:    "review-fragment",
				},
				Ralph: &RalphSpec{
					MaxAttempts: 3,
					Check: &RalphCheckSpec{
						Mode: "exec",
						Path: ".gascity/checks/review.sh",
					},
				},
				Children: []*Step{
					{ID: "review", Title: "Review", Type: "task"},
					{ID: "synthesize", Title: "Synthesize", Type: "task", Needs: []string{"review"}},
				},
			},
		},
	}

	expanded, err := ApplyRalph(f.Steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	f.Steps = expanded

	ApplyGraphControls(f)

	steps := collectGraphSteps(f.Steps)
	logical := findGraphStepByID(steps, "review-loop")
	if logical == nil {
		t.Fatal("missing review-loop logical step")
	}
	if got := logical.Metadata["gc.output_json_required"]; got != "true" {
		t.Fatalf("review-loop gc.output_json_required = %q, want true", got)
	}

	logicalFanout := findGraphStepByID(steps, "review-loop-fanout")
	if logicalFanout == nil {
		t.Fatal("missing logical fanout control")
	}
	if got := logicalFanout.Metadata["gc.control_for"]; got != "review-loop" {
		t.Fatalf("logical fanout gc.control_for = %q, want review-loop", got)
	}

	if run := findGraphStepByID(steps, "review-loop.iteration.1"); run == nil {
		t.Fatal("missing review-loop.iteration.1")
	} else {
		if run.OnComplete != nil {
			t.Fatal("review-loop.iteration.1 should not retain OnComplete")
		}
		if got := run.Metadata["gc.output_json_required"]; got != "true" {
			t.Fatalf("review-loop.iteration.1 gc.output_json_required = %q, want true", got)
		}
	}

	if runFanout := findGraphStepByID(steps, "review-loop.iteration.1-fanout"); runFanout != nil {
		t.Fatalf("unexpected run-level fanout control: %+v", runFanout)
	}

	sink := findGraphStepByID(steps, "review-loop.iteration.1.synthesize")
	if sink == nil {
		t.Fatal("missing nested sink step")
	}
	if got := sink.Metadata["gc.output_json_required"]; got != "true" {
		t.Fatalf("review-loop.iteration.1.synthesize gc.output_json_required = %q, want true", got)
	}

	nonSink := findGraphStepByID(steps, "review-loop.iteration.1.review")
	if nonSink == nil {
		t.Fatal("missing nested non-sink step")
	}
	if got := nonSink.Metadata["gc.output_json_required"]; got != "" {
		t.Fatalf("review-loop.iteration.1.review gc.output_json_required = %q, want empty", got)
	}
}

func TestApplyGraphControlsSimpleRalphInsideScopeDoesNotCreateRunScopeCheck(t *testing.T) {
	t.Parallel()

	f := &Formula{
		Steps: []*Step{
			{
				ID:    "review-loop",
				Title: "Review loop",
				Metadata: map[string]string{
					"gc.scope_ref":  "body",
					"gc.scope_role": "member",
					"gc.on_fail":    "abort_scope",
				},
				Ralph: &RalphSpec{
					MaxAttempts: 2,
					Check: &RalphCheckSpec{
						Mode: "exec",
						Path: ".gascity/checks/review.sh",
					},
				},
			},
		},
	}

	expanded, err := ApplyRalph(f.Steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	f.Steps = expanded

	ApplyGraphControls(f)

	steps := collectGraphSteps(f.Steps)
	run := findGraphStepByID(steps, "review-loop.iteration.1")
	if run == nil {
		t.Fatal("missing review-loop.iteration.1")
	}
	if got := run.Metadata["gc.scope_ref"]; got != "" {
		t.Fatalf("review-loop.iteration.1 gc.scope_ref = %q, want empty", got)
	}
	if got := run.Metadata["gc.scope_role"]; got != "" {
		t.Fatalf("review-loop.iteration.1 gc.scope_role = %q, want empty", got)
	}
	if got := run.Metadata["gc.on_fail"]; got != "" {
		t.Fatalf("review-loop.iteration.1 gc.on_fail = %q, want empty", got)
	}
	if scopeCheck := findGraphStepByID(steps, "review-loop.iteration.1-scope-check"); scopeCheck != nil {
		t.Fatalf("unexpected run scope-check control: %+v", scopeCheck)
	}
}

// TestNeedsScopeCheckTracksBeadmetaExemptKinds keeps the compile-path
// scope-check predicate in lockstep with beadmeta.ScopeCheckExemptKinds: every
// exempt kind is skipped, every non-exempt kind with a scope_ref still gets a
// paired scope-check, and the teardown-role guard is kind-independent.
func TestNeedsScopeCheckTracksBeadmetaExemptKinds(t *testing.T) {
	t.Parallel()

	for _, kind := range beadmeta.ScopeCheckExemptKinds {
		step := &Step{
			ID: "subject",
			Metadata: map[string]string{
				beadmeta.ScopeRefMetadataKey: "body",
				beadmeta.KindMetadataKey:     kind,
			},
		}
		if needsScopeCheck(step) {
			t.Errorf("needsScopeCheck(kind=%q) = true, want false (exempt kind)", kind)
		}
	}

	for _, kind := range []string{"", beadmeta.KindTask, beadmeta.KindRetry, beadmeta.KindRalph, beadmeta.KindCleanup} {
		step := &Step{
			ID: "subject",
			Metadata: map[string]string{
				beadmeta.ScopeRefMetadataKey: "body",
				beadmeta.KindMetadataKey:     kind,
			},
		}
		if !needsScopeCheck(step) {
			t.Errorf("needsScopeCheck(kind=%q) = false, want true (non-exempt kind)", kind)
		}
	}

	teardown := &Step{
		ID: "subject",
		Metadata: map[string]string{
			beadmeta.ScopeRefMetadataKey:  "body",
			beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
		},
	}
	if needsScopeCheck(teardown) {
		t.Error("needsScopeCheck(teardown role) = true, want false")
	}
	if needsScopeCheck(nil) {
		t.Error("needsScopeCheck(nil) = true, want false")
	}
	if needsScopeCheck(&Step{ID: "no-scope"}) {
		t.Error("needsScopeCheck(no scope_ref) = true, want false")
	}
}

func findGraphStepByID(steps []*Step, id string) *Step {
	for _, step := range steps {
		if step != nil && step.ID == id {
			return step
		}
	}
	return nil
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestApplyGraphControls_FanoutControlScopeRoleIsControl pins that a minted
// fanout control for a scope member is classified as scope-role control, not
// member: control infrastructure must not inherit the host step's member role,
// or its metadata/output participates in scope finalization as if it were work
// (mirrors the explicit ScopeRoleControl stamp on minted scope-checks).
func TestApplyGraphControls_FanoutControlScopeRoleIsControl(t *testing.T) {
	f := &Formula{
		Steps: []*Step{{
			ID:    "work",
			Title: "Work",
			OnComplete: &OnCompleteSpec{
				ForEach: "output.members",
				Bond:    "review-member",
			},
			Metadata: map[string]string{
				beadmeta.ScopeRefMetadataKey:  "scope-1",
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleMember,
			},
		}},
	}
	applyGraphControls(f, false)
	var control *Step
	for _, s := range f.Steps {
		if s.ID == "work-fanout" {
			control = s
		}
	}
	if control == nil {
		t.Fatal("missing minted fanout control work-fanout")
	}
	if got := control.Metadata[beadmeta.ScopeRefMetadataKey]; got != "scope-1" {
		t.Fatalf("fanout control gc.scope_ref = %q, want scope-1", got)
	}
	if got := control.Metadata[beadmeta.ScopeRoleMetadataKey]; got != beadmeta.ScopeRoleControl {
		t.Fatalf("fanout control gc.scope_role = %q, want %q", got, beadmeta.ScopeRoleControl)
	}
}
