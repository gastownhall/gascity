package formula

import "testing"

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

func TestApplyGraphControls_TallyInjected(t *testing.T) {
	t.Parallel()

	f := &Formula{
		Steps: []*Step{
			{
				ID:    "ask",
				Title: "Ask voters",
				OnComplete: &OnCompleteSpec{
					ForEach: "output.voters",
					Bond:    "mol-voter",
				},
				Tally: &TallySpec{
					VoteField: "answer",
					Mode:      "majority",
				},
			},
			{
				ID:    "summarize",
				Title: "Summarize",
				Needs: []string{"ask"},
			},
		},
	}

	ApplyGraphControls(f)

	steps := collectGraphSteps(f.Steps)

	fanout := findGraphStepByID(steps, "ask-fanout")
	if fanout == nil {
		t.Fatal("missing ask-fanout control")
	}
	if got := fanout.Metadata["gc.kind"]; got != "fanout" {
		t.Errorf("ask-fanout gc.kind = %q, want fanout", got)
	}

	tally := findGraphStepByID(steps, "ask-tally")
	if tally == nil {
		t.Fatal("missing ask-tally control step")
	}
	if got := tally.Metadata["gc.kind"]; got != "tally" {
		t.Errorf("ask-tally gc.kind = %q, want tally", got)
	}
	if got := tally.Metadata["gc.control_for"]; got != "ask" {
		t.Errorf("ask-tally gc.control_for = %q, want ask", got)
	}
	if got := tally.Metadata["gc.tally_mode"]; got != "majority" {
		t.Errorf("ask-tally gc.tally_mode = %q, want majority", got)
	}
	if got := tally.Metadata["gc.vote_field"]; got != "answer" {
		t.Errorf("ask-tally gc.vote_field = %q, want answer", got)
	}
	if !containsString(tally.Needs, "ask-fanout") {
		t.Errorf("ask-tally.Needs = %v, want ask-fanout", tally.Needs)
	}

	// Downstream step should be rewritten to wait for ask-tally, not ask.
	summarize := findGraphStepByID(steps, "summarize")
	if summarize == nil {
		t.Fatal("missing summarize step")
	}
	if containsString(summarize.Needs, "ask") {
		t.Error("summarize.Needs still contains ask — should have been rewritten to ask-tally")
	}
	if !containsString(summarize.Needs, "ask-tally") {
		t.Errorf("summarize.Needs = %v, want ask-tally", summarize.Needs)
	}
}

func TestApplyGraphControls_TallyDefaultMode(t *testing.T) {
	t.Parallel()

	f := &Formula{
		Steps: []*Step{
			{
				ID:    "vote",
				Title: "Vote",
				OnComplete: &OnCompleteSpec{
					ForEach: "output.items",
					Bond:    "mol-voter",
				},
				Tally: &TallySpec{},
			},
		},
	}

	ApplyGraphControls(f)

	steps := collectGraphSteps(f.Steps)
	tally := findGraphStepByID(steps, "vote-tally")
	if tally == nil {
		t.Fatal("missing vote-tally")
	}
	if got := tally.Metadata["gc.tally_mode"]; got != "majority" {
		t.Errorf("gc.tally_mode = %q, want majority (default)", got)
	}
}

func TestApplyGraphControls_NoTallyWhenFieldAbsent(t *testing.T) {
	t.Parallel()

	f := &Formula{
		Steps: []*Step{
			{
				ID:    "ask",
				Title: "Ask",
				OnComplete: &OnCompleteSpec{
					ForEach: "output.items",
					Bond:    "mol-voter",
				},
				// No Tally field.
			},
		},
	}

	ApplyGraphControls(f)

	steps := collectGraphSteps(f.Steps)
	if tally := findGraphStepByID(steps, "ask-tally"); tally != nil {
		t.Errorf("unexpected ask-tally control when Tally is nil: %+v", tally)
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
