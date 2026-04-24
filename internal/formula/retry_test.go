package formula

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyRetriesBasic(t *testing.T) {
	steps := []*Step{
		{
			ID:          "review",
			Title:       "Review change",
			Description: "Run review work.",
			Type:        "task",
			Needs:       []string{"setup"},
			Assignee:    "polecat",
			Labels:      []string{"pool:polecat"},
			Metadata: map[string]string{
				"gc.scope_ref":  "body",
				"gc.scope_role": "member",
				"gc.on_fail":    "abort_scope",
				"custom":        "value",
			},
			Retry: &RetrySpec{
				MaxAttempts: 3,
				OnExhausted: "soft_fail",
			},
		},
	}

	got, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (control + spec + attempt)", len(got))
	}

	control := got[0]
	spec := got[1]
	attempt := got[2]

	// Control bead identity and metadata.
	if control.ID != "review" {
		t.Fatalf("control.ID = %q, want review", control.ID)
	}
	if control.Metadata["gc.kind"] != "retry" {
		t.Fatalf("control gc.kind = %q, want retry", control.Metadata["gc.kind"])
	}
	if control.Metadata["gc.max_attempts"] != "3" {
		t.Fatalf("control gc.max_attempts = %q, want 3", control.Metadata["gc.max_attempts"])
	}
	if control.Metadata["gc.on_exhausted"] != "soft_fail" {
		t.Fatalf("control gc.on_exhausted = %q, want soft_fail", control.Metadata["gc.on_exhausted"])
	}
	if control.Metadata["gc.control_epoch"] != "1" {
		t.Fatalf("control gc.control_epoch = %q, want 1", control.Metadata["gc.control_epoch"])
	}

	// Control preserves scope metadata (scope_ref, on_fail).
	if control.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("control gc.scope_ref = %q, want body", control.Metadata["gc.scope_ref"])
	}
	if control.Metadata["gc.on_fail"] != "abort_scope" {
		t.Fatalf("control gc.on_fail = %q, want abort_scope", control.Metadata["gc.on_fail"])
	}

	// Control blocks on the attempt (not an eval bead).
	if len(control.Needs) != 2 || control.Needs[0] != "setup" || control.Needs[1] != "review.attempt.1" {
		t.Fatalf("control.Needs = %v, want [setup review.attempt.1]", control.Needs)
	}

	// Control has no assignee (it's a control node, not work).
	if control.Assignee != "" {
		t.Fatalf("control.Assignee = %q, want empty", control.Assignee)
	}

	if control.Metadata["gc.source_step_spec"] != "" {
		t.Fatalf("control gc.source_step_spec = %q, want empty", control.Metadata["gc.source_step_spec"])
	}
	assertFrozenSpecStep(t, spec, "review", func(frozen Step) {
		if frozen.Retry == nil || frozen.Retry.MaxAttempts != 3 {
			t.Fatalf("frozen step retry spec = %+v, want max_attempts=3", frozen.Retry)
		}
	})

	// Attempt bead identity and metadata.
	if attempt.ID != "review.attempt.1" {
		t.Fatalf("attempt.ID = %q, want review.attempt.1", attempt.ID)
	}
	if attempt.Metadata["gc.attempt"] != "1" {
		t.Fatalf("attempt gc.attempt = %q, want 1", attempt.Metadata["gc.attempt"])
	}
	// Attempt keeps original step's custom metadata.
	if attempt.Metadata["custom"] != "value" {
		t.Fatalf("attempt custom metadata = %q, want value", attempt.Metadata["custom"])
	}
	// Attempt strips scope metadata (scope_ref, scope_role, on_fail).
	if attempt.Metadata["gc.scope_ref"] != "" || attempt.Metadata["gc.scope_role"] != "" || attempt.Metadata["gc.on_fail"] != "" {
		t.Fatalf("attempt scope metadata leaked: scope_ref=%q scope_role=%q on_fail=%q",
			attempt.Metadata["gc.scope_ref"], attempt.Metadata["gc.scope_role"], attempt.Metadata["gc.on_fail"])
	}
	// Attempt has no retry config (not "retry-run" kind).
	if attempt.Metadata["gc.kind"] == "retry-run" {
		t.Fatal("attempt should not have gc.kind=retry-run (v1 pattern)")
	}
	// Attempt must NOT set gc.step_ref at compile time — molecule.Instantiate
	// fills it from step.ID which includes the formula prefix. Setting it here
	// produces short refs (e.g., "review.attempt.1" instead of "mol.review.attempt.1")
	// that break logical grouping in the presentation layer.
	if attempt.Metadata["gc.step_ref"] != "" {
		t.Fatalf("attempt gc.step_ref = %q, want empty (molecule.Instantiate fills it)",
			attempt.Metadata["gc.step_ref"])
	}
}

func TestCompileRetryManagedStepBlocksWorkflowOnLogicalBead(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaContent := `
formula = "retry-demo"
version = 2
contract = "graph.v2"

[[steps]]
id = "review"
title = "Review"
metadata = { "gc.run_target" = "polecat" }
type = "task"

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"
`
	if err := os.WriteFile(filepath.Join(dir, "retry-demo.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "retry-demo", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	var rootID, finalizerID string
	for _, step := range recipe.Steps {
		if step.IsRoot {
			rootID = step.ID
		}
		if step.Metadata["gc.kind"] == "workflow-finalize" {
			finalizerID = step.ID
		}
	}
	if rootID == "" {
		t.Fatal("missing workflow root")
	}
	if finalizerID == "" {
		t.Fatal("missing workflow finalizer")
	}

	var sawControl, sawAttempt bool
	for _, dep := range recipe.Deps {
		if dep.StepID == rootID && dep.Type == "blocks" && dep.DependsOnID != finalizerID {
			t.Fatalf("workflow root should only block on finalizer, saw %s", dep.DependsOnID)
		}
		if dep.StepID != finalizerID || dep.Type != "blocks" {
			continue
		}
		switch dep.DependsOnID {
		case "retry-demo.review":
			sawControl = true
		case "retry-demo.review.attempt.1":
			sawAttempt = true
		}
	}
	if !sawControl {
		t.Fatal("workflow finalizer should block on retry control bead")
	}
	if sawAttempt {
		t.Fatal("workflow finalizer should not block directly on retry attempt bead")
	}
}

func TestApplyRetriesPreservesNonRetrySteps(t *testing.T) {
	steps := []*Step{
		{ID: "setup", Title: "Setup"},
		{ID: "review", Title: "Review", Retry: &RetrySpec{MaxAttempts: 2}},
		{ID: "cleanup", Title: "Cleanup"},
	}

	got, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}
	// setup + (control + spec + attempt) + cleanup = 5
	if len(got) != 5 {
		t.Fatalf("len(got) = %d, want 5", len(got))
	}
	if got[0].ID != "setup" {
		t.Fatalf("got[0].ID = %q, want setup", got[0].ID)
	}
	if got[1].ID != "review" { // control
		t.Fatalf("got[1].ID = %q, want review (control)", got[1].ID)
	}
	if got[2].ID != "review.spec" {
		t.Fatalf("got[2].ID = %q, want review.spec", got[2].ID)
	}
	if got[3].ID != "review.attempt.1" {
		t.Fatalf("got[3].ID = %q, want review.attempt.1", got[3].ID)
	}
	if got[4].ID != "cleanup" {
		t.Fatalf("got[4].ID = %q, want cleanup", got[4].ID)
	}
}

func TestApplyRetriesDefaultOnExhausted(t *testing.T) {
	steps := []*Step{
		{ID: "work", Title: "Work", Retry: &RetrySpec{MaxAttempts: 5}},
	}

	got, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}
	control := got[0]
	if control.Metadata["gc.on_exhausted"] != "hard_fail" {
		t.Fatalf("default on_exhausted = %q, want hard_fail", control.Metadata["gc.on_exhausted"])
	}
}

func TestApplyRetriesFrozenSpecRoundTrips(t *testing.T) {
	original := &Step{
		ID:          "deploy",
		Title:       "Deploy {{service}}",
		Description: "Deploy service to production",
		Type:        "task",
		Assignee:    "deployer",
		Labels:      []string{"pool:deploy", "critical"},
		Metadata:    map[string]string{"env": "prod"},
		Retry:       &RetrySpec{MaxAttempts: 3, OnExhausted: "soft_fail"},
	}

	got, err := ApplyRetries([]*Step{original})
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}

	assertFrozenSpecStep(t, got[1], "deploy", func(frozen Step) {
		if frozen.Title != "Deploy {{service}}" {
			t.Errorf("frozen title = %q, want original", frozen.Title)
		}
		if frozen.Assignee != "deployer" {
			t.Errorf("frozen assignee = %q, want deployer", frozen.Assignee)
		}
		if len(frozen.Labels) != 2 || frozen.Labels[0] != "pool:deploy" {
			t.Errorf("frozen labels = %v, want [pool:deploy critical]", frozen.Labels)
		}
		if frozen.Retry == nil || frozen.Retry.MaxAttempts != 3 {
			t.Errorf("frozen retry = %+v, want max_attempts=3", frozen.Retry)
		}
	})
}
