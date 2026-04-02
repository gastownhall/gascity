package formula

import "testing"

func TestApplyGraphControlsRecursesIntoNestedChildren(t *testing.T) {
	t.Parallel()

	f := &Formula{
		Version: 2,
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
		Version: 2,
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
		Version: 2,
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
