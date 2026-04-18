package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/session"
)

// ---------------------------------------------------------------------------
// Integration tests: full retry/ralph lifecycle through processRetryControl
// and processRalphControl, including molecule.Attach for spawning attempts.
// ---------------------------------------------------------------------------

// makeRetryControl creates a workflow root + retry control bead with frozen step spec.
func makeRetryControl(t *testing.T, store beads.Store, stepRef string, spec *formula.Step, maxAttempts int) (root, control beads.Bead) {
	t.Helper()
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal step spec: %v", err)
	}
	root = mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control = mustCreate(t, store, beads.Bead{
		Title: spec.Title + " (retry)",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         stepRef,
			"gc.step_id":          spec.ID,
			"gc.max_attempts":     strconv.Itoa(maxAttempts),
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": string(specJSON),
			"gc.control_epoch":    "1",
		},
	})
	return
}

// makeAttemptBead creates and closes an attempt bead with the given outcome metadata.
func makeAttemptBead(t *testing.T, store beads.Store, rootID, stepRef string, attemptNum int, meta map[string]string) beads.Bead {
	t.Helper()
	baseMeta := map[string]string{
		"gc.root_bead_id": rootID,
		"gc.step_ref":     stepRef,
		"gc.attempt":      strconv.Itoa(attemptNum),
	}
	for k, v := range meta {
		baseMeta[k] = v
	}
	b := mustCreate(t, store, beads.Bead{
		Title:    "attempt " + strconv.Itoa(attemptNum),
		Metadata: baseMeta,
	})
	mustClose(t, store, b.ID)
	return b
}

// TestRetryLifecycleTransientThenPass exercises the full lifecycle:
// attempt 1 fails transient → processRetryControl spawns attempt 2 via Attach →
// attempt 2 passes → processRetryControl closes control as pass.
func TestRetryLifecycleTransientThenPass(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	spec := &formula.Step{
		ID:    "review",
		Title: "Review",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 3},
	}
	root, control := makeRetryControl(t, store, "mol-test.review", spec, 3)

	// --- Attempt 1: transient failure ---
	attempt1 := makeAttemptBead(t, store, root.ID, "mol-test.review.attempt.1", 1, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "rate_limited",
	})
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl attempt 1: %v", err)
	}
	if result.Action != "retry" {
		t.Fatalf("attempt 1 action = %q, want retry", result.Action)
	}

	// Control should still be open, epoch advanced.
	controlAfter1 := mustGet(t, store, control.ID)
	if controlAfter1.Status != "open" {
		t.Fatalf("control status after attempt 1 = %q, want open", controlAfter1.Status)
	}
	if controlAfter1.Metadata["gc.control_epoch"] != "2" {
		t.Fatalf("epoch after attempt 1 = %q, want 2", controlAfter1.Metadata["gc.control_epoch"])
	}

	// Attach should have created attempt 2 beads in the store.
	// Find attempt 2 by step_ref pattern.
	attempt2 := findAttemptByRef(t, store, root.ID, "mol-test.review.attempt.2")
	if attempt2.ID == "" {
		t.Fatal("attempt 2 was not created by Attach")
	}
	if attempt2.Metadata["gc.attempt"] != "2" {
		t.Fatalf("attempt 2 gc.attempt = %q, want 2", attempt2.Metadata["gc.attempt"])
	}
	if attempt2.Metadata["gc.root_bead_id"] != root.ID {
		t.Fatalf("attempt 2 gc.root_bead_id = %q, want %q", attempt2.Metadata["gc.root_bead_id"], root.ID)
	}

	// --- Attempt 2: pass ---
	// Close attempt 2 with pass outcome.
	if err := store.SetMetadataBatch(attempt2.ID, map[string]string{
		"gc.outcome":     "pass",
		"gc.output_json": `{"verdict":"approved"}`,
	}); err != nil {
		t.Fatalf("set attempt 2 metadata: %v", err)
	}
	mustClose(t, store, attempt2.ID)

	// Process control again — should see attempt 2 passed.
	result2, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl attempt 2: %v", err)
	}
	if result2.Action != "pass" {
		t.Fatalf("attempt 2 action = %q, want pass", result2.Action)
	}

	// Control should be closed with pass outcome and propagated output.
	controlFinal := mustGet(t, store, control.ID)
	if controlFinal.Status != "closed" {
		t.Fatalf("control final status = %q, want closed", controlFinal.Status)
	}
	if controlFinal.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("control outcome = %q, want pass", controlFinal.Metadata["gc.outcome"])
	}
	if controlFinal.Metadata["gc.output_json"] != `{"verdict":"approved"}` {
		t.Fatalf("control output_json = %q, want propagated", controlFinal.Metadata["gc.output_json"])
	}

	// Attempt log should have 2 entries.
	var log []map[string]string
	if err := json.Unmarshal([]byte(controlFinal.Metadata["gc.attempt_log"]), &log); err != nil {
		t.Fatalf("unmarshal attempt_log: %v", err)
	}
	if len(log) != 2 {
		t.Fatalf("attempt_log entries = %d, want 2", len(log))
	}
	if log[0]["outcome"] != "transient" || log[0]["action"] != "retry" {
		t.Errorf("log[0] = %v, want transient/retry", log[0])
	}
	if log[1]["outcome"] != "pass" || log[1]["action"] != "close" {
		t.Errorf("log[1] = %v, want pass/close", log[1])
	}
}

// TestRetryLifecycleExhaustion exercises: attempt 1 fails → retry →
// attempt 2 fails → retry → attempt 3 fails → exhausted (hard_fail).
func TestRetryLifecycleExhaustion(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	spec := &formula.Step{
		ID:    "deploy",
		Title: "Deploy",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 3},
	}
	root, control := makeRetryControl(t, store, "mol-test.deploy", spec, 3)

	// --- Attempt 1: transient ---
	attempt1 := makeAttemptBead(t, store, root.ID, "mol-test.deploy.attempt.1", 1, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "timeout",
	})
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	result1, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if result1.Action != "retry" {
		t.Fatalf("round 1 action = %q, want retry", result1.Action)
	}

	// --- Attempt 2: transient ---
	attempt2 := findAttemptByRef(t, store, root.ID, "mol-test.deploy.attempt.2")
	if err := store.SetMetadataBatch(attempt2.ID, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "network_error",
	}); err != nil {
		t.Fatalf("set attempt 2 meta: %v", err)
	}
	mustClose(t, store, attempt2.ID)

	result2, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if result2.Action != "retry" {
		t.Fatalf("round 2 action = %q, want retry", result2.Action)
	}

	// --- Attempt 3: transient (exhausted) ---
	attempt3 := findAttemptByRef(t, store, root.ID, "mol-test.deploy.attempt.3")
	if err := store.SetMetadataBatch(attempt3.ID, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "still_broken",
	}); err != nil {
		t.Fatalf("set attempt 3 meta: %v", err)
	}
	mustClose(t, store, attempt3.ID)

	result3, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if result3.Action != "fail" {
		t.Fatalf("round 3 action = %q, want fail (exhausted)", result3.Action)
	}

	// Control should be closed with fail + hard_fail disposition.
	controlFinal := mustGet(t, store, control.ID)
	if controlFinal.Status != "closed" {
		t.Fatalf("control status = %q, want closed", controlFinal.Status)
	}
	if controlFinal.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("control outcome = %q, want fail", controlFinal.Metadata["gc.outcome"])
	}
	if controlFinal.Metadata["gc.final_disposition"] != "hard_fail" {
		t.Fatalf("disposition = %q, want hard_fail", controlFinal.Metadata["gc.final_disposition"])
	}

	// Attempt log should have 3 entries.
	var log []map[string]string
	if err := json.Unmarshal([]byte(controlFinal.Metadata["gc.attempt_log"]), &log); err != nil {
		t.Fatalf("unmarshal attempt_log: %v", err)
	}
	if len(log) != 3 {
		t.Fatalf("attempt_log entries = %d, want 3", len(log))
	}
}

// TestRetryLifecycleEpochAdvancesPerAttempt verifies that each retry
// Attach increments the epoch, preventing stale controllers from acting.
func TestRetryLifecycleEpochAdvancesPerAttempt(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	spec := &formula.Step{
		ID:    "build",
		Title: "Build",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 5},
	}
	root, control := makeRetryControl(t, store, "mol-test.build", spec, 5)

	// Create and fail 3 attempts, checking epoch each time.
	for i := 1; i <= 3; i++ {
		ref := "mol-test.build.attempt." + strconv.Itoa(i)
		if i == 1 {
			// First attempt created manually (compiler would normally do this).
			attempt := makeAttemptBead(t, store, root.ID, ref, i, map[string]string{
				"gc.outcome":        "fail",
				"gc.failure_class":  "transient",
				"gc.failure_reason": "flaky",
			})
			mustDep(t, store, control.ID, attempt.ID, "blocks")
		} else {
			// Subsequent attempts created by Attach — find and fail them.
			attempt := findAttemptByRef(t, store, root.ID, ref)
			if attempt.ID == "" {
				t.Fatalf("attempt %d not found", i)
			}
			if err := store.SetMetadataBatch(attempt.ID, map[string]string{
				"gc.outcome":        "fail",
				"gc.failure_class":  "transient",
				"gc.failure_reason": "flaky",
			}); err != nil {
				t.Fatalf("set attempt %d meta: %v", i, err)
			}
			mustClose(t, store, attempt.ID)
		}

		_, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
		if err != nil {
			t.Fatalf("processRetryControl round %d: %v", i, err)
		}

		expectedEpoch := strconv.Itoa(i + 1)
		actual := mustGet(t, store, control.ID).Metadata["gc.control_epoch"]
		if actual != expectedEpoch {
			t.Fatalf("epoch after round %d = %q, want %q", i, actual, expectedEpoch)
		}
	}
}

// TestBuildAttemptRecipeEnrichesNestedRetryChildren verifies that
// buildAttemptRecipe propagates gc.kind, gc.source_step_spec,
// gc.control_epoch, gc.max_attempts for nested retry children.
func TestBuildAttemptRecipeEnrichesNestedRetryChildren(t *testing.T) {
	t.Parallel()

	step := &formula.Step{
		ID:    "self-review",
		Title: "Self Review",
		Type:  "task",
		Ralph: &formula.RalphSpec{MaxAttempts: 5},
		Children: []*formula.Step{
			{
				ID:    "review-code",
				Title: "Review Code",
				Type:  "task",
				Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "soft_fail"},
			},
			{
				ID:    "apply-fixes",
				Title: "Apply Fixes",
				Type:  "task",
				Needs: []string{"review-code"},
			},
		},
	}

	control := beads.Bead{
		ID: "gc-1",
		Metadata: map[string]string{
			"gc.step_id":  "self-review",
			"gc.step_ref": "mol-demo.self-review",
		},
	}

	recipe := buildAttemptRecipe(step, control, 2)

	// Should have 6 steps: scope root + 2 children + 1 spec bead + 2 scope-checks.
	if len(recipe.Steps) != 6 {
		t.Fatalf("steps = %d, want 6", len(recipe.Steps))
	}

	// Find the review-code child step.
	var reviewStep *formula.RecipeStep
	var specStep *formula.RecipeStep
	var reviewScopeCheck *formula.RecipeStep
	var applyScopeCheck *formula.RecipeStep
	for i := range recipe.Steps {
		if recipe.Steps[i].ID == "mol-demo.self-review.iteration.2.review-code" {
			reviewStep = &recipe.Steps[i]
		}
		if recipe.Steps[i].ID == "mol-demo.self-review.iteration.2.review-code.spec" {
			specStep = &recipe.Steps[i]
		}
		if recipe.Steps[i].ID == "mol-demo.self-review.iteration.2.review-code-scope-check" {
			reviewScopeCheck = &recipe.Steps[i]
		}
		if recipe.Steps[i].ID == "mol-demo.self-review.iteration.2.apply-fixes-scope-check" {
			applyScopeCheck = &recipe.Steps[i]
		}
	}
	if reviewStep == nil {
		t.Fatal("review-code child step not found in recipe")
	}

	// Should have retry-specific metadata.
	if reviewStep.Metadata["gc.kind"] != "retry" {
		t.Errorf("review-code gc.kind = %q, want retry", reviewStep.Metadata["gc.kind"])
	}
	if reviewStep.Metadata["gc.max_attempts"] != "3" {
		t.Errorf("review-code gc.max_attempts = %q, want 3", reviewStep.Metadata["gc.max_attempts"])
	}
	if reviewStep.Metadata["gc.control_epoch"] != "1" {
		t.Errorf("review-code gc.control_epoch = %q, want 1", reviewStep.Metadata["gc.control_epoch"])
	}
	if reviewStep.Metadata["gc.on_exhausted"] != "soft_fail" {
		t.Errorf("review-code gc.on_exhausted = %q, want soft_fail", reviewStep.Metadata["gc.on_exhausted"])
	}

	// Frozen step spec stored as a separate spec bead.
	if specStep == nil {
		t.Fatal("review-code.spec bead not found in recipe")
	}
	if reviewScopeCheck == nil {
		t.Fatal("review-code scope-check bead not found in recipe")
	}
	if applyScopeCheck == nil {
		t.Fatal("apply-fixes scope-check bead not found in recipe")
	}
	if specStep.Metadata["gc.kind"] != "spec" {
		t.Errorf("spec gc.kind = %q, want spec", specStep.Metadata["gc.kind"])
	}
	if reviewScopeCheck.Metadata["gc.kind"] != "scope-check" {
		t.Errorf("review-code scope-check gc.kind = %q, want scope-check", reviewScopeCheck.Metadata["gc.kind"])
	}
	if applyScopeCheck.Metadata["gc.kind"] != "scope-check" {
		t.Errorf("apply-fixes scope-check gc.kind = %q, want scope-check", applyScopeCheck.Metadata["gc.kind"])
	}
	var frozenSpec formula.Step
	if err := json.Unmarshal([]byte(specStep.Description), &frozenSpec); err != nil {
		t.Fatalf("unmarshal frozen spec: %v", err)
	}
	if frozenSpec.ID != "review-code" {
		t.Errorf("frozen spec ID = %q, want review-code", frozenSpec.ID)
	}
	if frozenSpec.Retry == nil || frozenSpec.Retry.MaxAttempts != 3 {
		t.Errorf("frozen spec retry = %+v, want MaxAttempts=3", frozenSpec.Retry)
	}
}

// TestBuildAttemptRecipeEnrichesNestedRalphChildren verifies that
// buildAttemptRecipe propagates gc.kind, gc.check_*, and a spec bead
// for nested ralph children.
func TestBuildAttemptRecipeEnrichesNestedRalphChildren(t *testing.T) {
	t.Parallel()

	step := &formula.Step{
		ID:    "converge",
		Title: "Converge",
		Type:  "task",
		Ralph: &formula.RalphSpec{MaxAttempts: 5},
		Children: []*formula.Step{
			{
				ID:    "inner-converge",
				Title: "Inner Converge",
				Type:  "task",
				Ralph: &formula.RalphSpec{
					MaxAttempts: 3,
					Check: &formula.RalphCheckSpec{
						Mode:    "script",
						Path:    "/tmp/check.sh",
						Timeout: "30s",
					},
				},
			},
		},
	}

	control := beads.Bead{
		ID: "gc-1",
		Metadata: map[string]string{
			"gc.step_id":  "converge",
			"gc.step_ref": "mol-test.converge",
		},
	}

	recipe := buildAttemptRecipe(step, control, 1)

	// Find the inner-converge child.
	var innerStep *formula.RecipeStep
	for i := range recipe.Steps {
		if recipe.Steps[i].ID == "mol-test.converge.iteration.1.inner-converge" {
			innerStep = &recipe.Steps[i]
			break
		}
	}
	if innerStep == nil {
		t.Fatal("inner-converge child step not found")
	}

	if innerStep.Metadata["gc.kind"] != "ralph" {
		t.Errorf("inner gc.kind = %q, want ralph", innerStep.Metadata["gc.kind"])
	}
	if innerStep.Metadata["gc.max_attempts"] != "3" {
		t.Errorf("inner gc.max_attempts = %q, want 3", innerStep.Metadata["gc.max_attempts"])
	}
	if innerStep.Metadata["gc.check_mode"] != "script" {
		t.Errorf("inner gc.check_mode = %q, want script", innerStep.Metadata["gc.check_mode"])
	}
	if innerStep.Metadata["gc.check_path"] != "/tmp/check.sh" {
		t.Errorf("inner gc.check_path = %q, want /tmp/check.sh", innerStep.Metadata["gc.check_path"])
	}
	if innerStep.Metadata["gc.check_timeout"] != "30s" {
		t.Errorf("inner gc.check_timeout = %q, want 30s", innerStep.Metadata["gc.check_timeout"])
	}
	// Frozen step spec stored as a separate spec bead.
	var innerSpecStep *formula.RecipeStep
	for i := range recipe.Steps {
		if recipe.Steps[i].ID == "mol-test.converge.iteration.1.inner-converge.spec" {
			innerSpecStep = &recipe.Steps[i]
			break
		}
	}
	if innerSpecStep == nil {
		t.Error("inner missing spec bead for frozen step spec")
	} else if innerSpecStep.Metadata["gc.kind"] != "spec" {
		t.Errorf("inner spec gc.kind = %q, want spec", innerSpecStep.Metadata["gc.kind"])
	}
}

// TestSpawnNextAttemptPropagatesRoutingMetadata verifies that
// spawnNextAttempt stamps gc.routed_to metadata from gc.execution_routed_to.
func TestSpawnNextAttemptPropagatesRoutingMetadata(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	spec := &formula.Step{
		ID:    "lint",
		Title: "Lint",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 3},
	}
	specJSON, _ := json.Marshal(spec)

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "lint retry",
		Metadata: map[string]string{
			"gc.kind":                "retry",
			"gc.root_bead_id":        root.ID,
			"gc.step_ref":            "mol-test.lint",
			"gc.step_id":             "lint",
			"gc.max_attempts":        "3",
			"gc.source_step_spec":    string(specJSON),
			"gc.control_epoch":       "1",
			"gc.execution_routed_to": "polecat",
		},
	})

	// Create attempt 1 (manually, simulating compiler).
	attempt1 := makeAttemptBead(t, store, root.ID, "mol-test.lint.attempt.1", 1, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "timeout",
	})
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	// Process — should spawn attempt 2 with routing labels.
	_, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}

	// Find attempt 2 and check its labels.
	attempt2 := findAttemptByRef(t, store, root.ID, "mol-test.lint.attempt.2")
	if attempt2.ID == "" {
		t.Fatal("attempt 2 not created")
	}

	// Check routing metadata.
	if attempt2.Metadata["gc.execution_routed_to"] != "polecat" {
		t.Errorf("attempt 2 gc.execution_routed_to = %q, want polecat", attempt2.Metadata["gc.execution_routed_to"])
	}
	if attempt2.Metadata["gc.routed_to"] != "polecat" {
		t.Errorf("attempt 2 gc.routed_to = %q, want polecat", attempt2.Metadata["gc.routed_to"])
	}

	for _, l := range attempt2.Labels {
		if l == "pool:polecat" {
			t.Errorf("attempt 2 labels = %v, should not contain legacy pool label", attempt2.Labels)
		}
	}
}

func TestSpawnNextAttemptPreservesExplicitChildPoolRoutes(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`
[workspace]
name = "maintainer-city"
provider = "claude"

[[agent]]
name = "claude"
dir = "gascity"

[agent.pool]
min = 0
max = -1

[[agent]]
name = "codex"
dir = "gascity"

[agent.pool]
min = 0
max = -1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	store := beads.NewMemStore()
	spec := &formula.Step{
		ID:    "review-loop",
		Title: "Review / fix loop",
		Type:  "task",
		Ralph: &formula.RalphSpec{MaxAttempts: 3},
		Children: []*formula.Step{
			{
				ID:    "review-claude",
				Title: "Code review: Claude",
				Type:  "task",
				Metadata: map[string]string{
					"gc.run_target": "gascity/claude",
				},
				Retry: &formula.RetrySpec{MaxAttempts: 3},
			},
			{
				ID:    "review-codex",
				Title: "Code review: Codex",
				Type:  "task",
				Metadata: map[string]string{
					"gc.run_target": "gascity/codex",
				},
				Retry: &formula.RetrySpec{MaxAttempts: 3},
			},
			{
				ID:    "synthesize",
				Title: "Synthesize findings",
				Type:  "task",
				Needs: []string{"review-claude", "review-codex"},
			},
		},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal step spec: %v", err)
	}

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review-loop",
		Metadata: map[string]string{
			"gc.kind":                "ralph",
			"gc.root_bead_id":        root.ID,
			"gc.step_ref":            "mol-adopt-pr-v2.review-loop",
			"gc.step_id":             "review-loop",
			"gc.source_step_spec":    string(specJSON),
			"gc.control_epoch":       "1",
			"gc.execution_routed_to": "gascity/claude",
		},
	})

	if err := spawnNextAttempt(t.Context(), store, control, 2, ProcessOptions{CityPath: cityPath}); err != nil {
		t.Fatalf("spawnNextAttempt: %v", err)
	}

	scope := findAttemptByRef(t, store, root.ID, "mol-adopt-pr-v2.review-loop.iteration.2")
	if scope.ID == "" {
		t.Fatal("ralph scope iteration not created")
	}
	if scope.Metadata["gc.routed_to"] != "gascity/claude" {
		t.Fatalf("scope gc.routed_to = %q, want gascity/claude", scope.Metadata["gc.routed_to"])
	}
	if containsString(scope.Labels, "pool:gascity/claude") {
		t.Fatalf("scope labels = %v, should not contain legacy pool label", scope.Labels)
	}

	claude := findAttemptByRef(t, store, root.ID, "mol-adopt-pr-v2.review-loop.iteration.2.review-claude")
	if claude.ID == "" {
		t.Fatal("review-claude child not created")
	}
	if claude.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
		t.Fatalf("review-claude gc.routed_to = %q, want %q", claude.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
	}
	if claude.Metadata["gc.execution_routed_to"] != "gascity/claude" {
		t.Fatalf("review-claude gc.execution_routed_to = %q, want gascity/claude", claude.Metadata["gc.execution_routed_to"])
	}
	if containsString(claude.Labels, "pool:gascity/claude") {
		t.Fatalf("review-claude labels = %v, should not contain legacy pool label", claude.Labels)
	}
	if claude.Assignee != "" {
		t.Fatalf("review-claude assignee = %q, want empty metadata-only control route", claude.Assignee)
	}

	codex := findAttemptByRef(t, store, root.ID, "mol-adopt-pr-v2.review-loop.iteration.2.review-codex")
	if codex.ID == "" {
		t.Fatal("review-codex child not created")
	}
	if codex.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
		t.Fatalf("review-codex gc.routed_to = %q, want %q", codex.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
	}
	if codex.Metadata["gc.execution_routed_to"] != "gascity/codex" {
		t.Fatalf("review-codex gc.execution_routed_to = %q, want gascity/codex", codex.Metadata["gc.execution_routed_to"])
	}
	if containsString(codex.Labels, "pool:gascity/codex") {
		t.Fatalf("review-codex labels = %v, should not contain legacy pool label", codex.Labels)
	}
	if containsString(codex.Labels, "pool:gascity/claude") {
		t.Fatalf("review-codex labels = %v, should not contain pool:gascity/claude", codex.Labels)
	}
	if codex.Assignee != "" {
		t.Fatalf("review-codex assignee = %q, want empty metadata-only control route", codex.Assignee)
	}

	synthesize := findAttemptByRef(t, store, root.ID, "mol-adopt-pr-v2.review-loop.iteration.2.synthesize")
	if synthesize.ID == "" {
		t.Fatal("synthesize child not created")
	}
	if synthesize.Metadata["gc.routed_to"] != "gascity/claude" {
		t.Fatalf("synthesize gc.routed_to = %q, want gascity/claude fallback", synthesize.Metadata["gc.routed_to"])
	}
	if containsString(synthesize.Labels, "pool:gascity/claude") {
		t.Fatalf("synthesize labels = %v, should not contain legacy pool label", synthesize.Labels)
	}

	assertSpawnedSpecClosedAndUnrouted(t, store, root.ID, "review-claude")
	assertSpawnedSpecClosedAndUnrouted(t, store, root.ID, "review-codex")

	claudeSpec, err := findSpecBead(store, claude)
	if err != nil {
		t.Fatalf("findSpecBead(review-claude): %v", err)
	}
	if claudeSpec.Status != "closed" {
		t.Fatalf("review-claude spec status = %q, want closed", claudeSpec.Status)
	}

	codexSpec, err := findSpecBead(store, codex)
	if err != nil {
		t.Fatalf("findSpecBead(review-codex): %v", err)
	}
	if codexSpec.Status != "closed" {
		t.Fatalf("review-codex spec status = %q, want closed", codexSpec.Status)
	}
}

func assertSpawnedSpecClosedAndUnrouted(t *testing.T, store beads.Store, rootID, specFor string) {
	t.Helper()
	all, err := store.ListByMetadata(map[string]string{"gc.root_bead_id": rootID}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByMetadata(gc.root_bead_id=%q): %v", rootID, err)
	}
	for _, bead := range all {
		if bead.Metadata["gc.kind"] != "spec" || bead.Metadata["gc.spec_for"] != specFor {
			continue
		}
		if bead.Status != "closed" {
			t.Fatalf("spec %s status = %q, want closed", bead.ID, bead.Status)
		}
		if bead.Assignee != "" {
			t.Fatalf("spec %s assignee = %q, want empty", bead.ID, bead.Assignee)
		}
		for _, key := range []string{"gc.routed_to", "gc.execution_routed_to"} {
			if bead.Metadata[key] != "" {
				t.Fatalf("spec %s metadata %s = %q, want empty; full metadata: %#v", bead.ID, key, bead.Metadata[key], bead.Metadata)
			}
		}
		return
	}
	t.Fatalf("missing spec bead for %q under root %s", specFor, rootID)
}

func TestSpawnNextAttemptRoutesDirectSessionRetryControlViaDispatcher(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	_ = mustCreate(t, store, beads.Bead{
		Title:  "sky",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "sky",
			"session_name": "s-gc-sky",
		},
	})
	spec := &formula.Step{
		ID:    "review-loop",
		Title: "Review / fix loop",
		Type:  "task",
		Ralph: &formula.RalphSpec{MaxAttempts: 3},
		Children: []*formula.Step{{
			ID:       "review-direct",
			Title:    "Code review",
			Type:     "task",
			Assignee: "sky",
			Retry:    &formula.RetrySpec{MaxAttempts: 3},
		}},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal step spec: %v", err)
	}

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review-loop",
		Metadata: map[string]string{
			"gc.kind":             "ralph",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-direct.review-loop",
			"gc.step_id":          "review-loop",
			"gc.source_step_spec": string(specJSON),
			"gc.control_epoch":    "1",
		},
	})

	if err := spawnNextAttempt(t.Context(), store, control, 2, ProcessOptions{}); err != nil {
		t.Fatalf("spawnNextAttempt: %v", err)
	}

	child := findAttemptByRef(t, store, root.ID, "mol-direct.review-loop.iteration.2.review-direct")
	if child.ID == "" {
		t.Fatal("review-direct child not created")
	}
	if got := child.Assignee; got != "" {
		t.Fatalf("review-direct assignee = %q, want empty when control-dispatcher is unresolved", got)
	}
	if got := child.Metadata["gc.routed_to"]; got != config.ControlDispatcherAgentName {
		t.Fatalf("review-direct gc.routed_to = %q, want %q", got, config.ControlDispatcherAgentName)
	}
	if got := child.Metadata["gc.execution_routed_to"]; got != "sky" {
		t.Fatalf("review-direct gc.execution_routed_to = %q, want direct session target preserved", got)
	}
}

func TestResolveAttemptRouteBinding_ConfigTargetBeatsCollidingSessionAlias(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "colliding session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "gascity/claude",
			"session_name": "s-gc-colliding",
		},
	}); err != nil {
		t.Fatalf("create colliding session: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "claude",
			Dir:  "gascity",
		}},
	}

	binding, ok := resolveAttemptRouteBinding("gascity/claude", cfg, store)
	if !ok {
		t.Fatal("resolveAttemptRouteBinding did not resolve config target")
	}
	if binding.directSessionID != "" {
		t.Fatalf("directSessionID = %q, want empty so config route is not hijacked by alias", binding.directSessionID)
	}
	if binding.qualifiedName != "gascity/claude" || !binding.metadataOnly {
		t.Fatalf("binding = %+v, want metadata-only gascity/claude config route", binding)
	}
}

func TestResolveAttemptRouteBinding_NamedSessionTargetUsesCanonicalBeadID(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	named, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "test-city--worker",
			"template":                  "worker",
			"configured_named_session":  "true",
			"configured_named_identity": "worker",
			"configured_named_mode":     "on_demand",
			"state":                     "asleep",
			"continuity_eligible":       "true",
		},
	})
	if err != nil {
		t.Fatalf("create named session: %v", err)
	}
	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxActive,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "on_demand",
		}},
	}

	binding, ok := resolveAttemptRouteBinding("worker", cfg, store)
	if !ok {
		t.Fatal("resolveAttemptRouteBinding did not resolve named target")
	}
	if binding.directSessionID != named.ID {
		t.Fatalf("directSessionID = %q, want canonical named bead ID %q", binding.directSessionID, named.ID)
	}
	if binding.qualifiedName != "" || binding.sessionName != "" {
		t.Fatalf("binding = %+v, want direct named session only", binding)
	}
}

func TestResolveAttemptRouteBinding_NamedSessionTargetWithoutCanonicalBeadUsesMetadataOnly(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxActive,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "on_demand",
		}},
	}

	binding, ok := resolveAttemptRouteBinding("worker", cfg, store)
	if !ok {
		t.Fatal("resolveAttemptRouteBinding did not resolve named target")
	}
	if binding.directSessionID != "" || binding.sessionName != "" {
		t.Fatalf("binding = %+v, want no direct or legacy session-name target without a canonical bead", binding)
	}
	if binding.qualifiedName != "worker" || !binding.metadataOnly {
		t.Fatalf("binding = %+v, want metadata-only worker route", binding)
	}
}

func TestApplyAttemptControlStepRoute_KeepsControlBeadsOnDispatcherForNamedExecutionTarget(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "worker",
			"template":                  "worker",
			"configured_named_session":  "true",
			"configured_named_identity": "worker",
			"configured_named_mode":     "always",
			"state":                     "active",
		},
	}); err != nil {
		t.Fatalf("create named session: %v", err)
	}
	dispatcher, err := store.Create(beads.Bead{
		Title:  "control-dispatcher",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "control-dispatcher",
			"template":                  config.ControlDispatcherAgentName,
			"configured_named_session":  "true",
			"configured_named_identity": config.ControlDispatcherAgentName,
			"configured_named_mode":     "always",
			"state":                     "active",
		},
	})
	if err != nil {
		t.Fatalf("create control-dispatcher session: %v", err)
	}

	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxActive,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}
	config.InjectImplicitAgents(cfg)

	step := &formula.RecipeStep{
		ID:       "review-scope-check",
		Title:    "Finalize scope for review",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "scope-check"},
	}

	applyAttemptControlStepRoute(step, "worker", cfg, store)

	if got := step.Metadata["gc.execution_routed_to"]; got != "worker" {
		t.Fatalf("gc.execution_routed_to = %q, want worker", got)
	}
	if got := step.Metadata["gc.routed_to"]; got != config.ControlDispatcherAgentName {
		t.Fatalf("gc.routed_to = %q, want %q", got, config.ControlDispatcherAgentName)
	}
	if step.Assignee != dispatcher.ID {
		t.Fatalf("assignee = %q, want canonical control-dispatcher bead %q", step.Assignee, dispatcher.ID)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestBuildAttemptRecipeScopeMetadataForRalph verifies that ralph iteration
// root beads get scope metadata (gc.scope_role, gc.scope_name, gc.ralph_step_id).
func TestBuildAttemptRecipeScopeMetadataForRalph(t *testing.T) {
	t.Parallel()

	step := &formula.Step{
		ID:    "self-review",
		Title: "Self Review",
		Type:  "task",
		Ralph: &formula.RalphSpec{MaxAttempts: 5},
		Children: []*formula.Step{
			{ID: "implement", Title: "Implement", Type: "task"},
			{ID: "check", Title: "Check", Type: "task", Needs: []string{"implement"}},
		},
	}

	control := beads.Bead{
		ID: "gc-1",
		Metadata: map[string]string{
			"gc.step_id":  "self-review",
			"gc.step_ref": "mol-demo.self-review",
		},
	}

	recipe := buildAttemptRecipe(step, control, 3)

	rootStep := recipe.Steps[0]
	if rootStep.Metadata["gc.kind"] != "scope" {
		t.Errorf("root gc.kind = %q, want scope", rootStep.Metadata["gc.kind"])
	}
	if rootStep.Metadata["gc.scope_role"] != "body" {
		t.Errorf("root gc.scope_role = %q, want body", rootStep.Metadata["gc.scope_role"])
	}
	if rootStep.Metadata["gc.scope_name"] != "self-review" {
		t.Errorf("root gc.scope_name = %q, want self-review", rootStep.Metadata["gc.scope_name"])
	}
	if rootStep.Metadata["gc.ralph_step_id"] != "self-review" {
		t.Errorf("root gc.ralph_step_id = %q, want self-review", rootStep.Metadata["gc.ralph_step_id"])
	}
}

// TestRetryIdempotencyKeyPreventsDoubleSpawn verifies that processing the
// same control bead twice (e.g., due to a race in the controller) does not
// create duplicate attempt sub-DAGs.
func TestRetryIdempotencyKeyPreventsDoubleSpawn(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	spec := &formula.Step{
		ID:    "build",
		Title: "Build",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 3},
	}
	root, control := makeRetryControl(t, store, "mol-test.build", spec, 3)

	attempt1 := makeAttemptBead(t, store, root.ID, "mol-test.build.attempt.1", 1, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "flaky",
	})
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	// Process once — spawns attempt 2.
	_, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("first process: %v", err)
	}

	allAfterFirst, _ := store.ListOpen()
	countAfterFirst := len(allAfterFirst)

	// Process again with same state — epoch conflict should prevent double spawn.
	// The epoch was already incremented by the first Attach, so a second
	// processRetryControl with the same attempt (attempt 1 still closed, attempt 2
	// still open) will find attempt 2 as the latest and see it's not closed.
	// This verifies the invariant violation guard.
	_, err = processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err == nil {
		t.Fatal("expected error on second process (attempt 2 is open)")
	}

	// No new beads should have been created.
	allAfterSecond, _ := store.ListOpen()
	if len(allAfterSecond) != countAfterFirst {
		t.Errorf("bead count changed: %d → %d (double spawn?)", countAfterFirst, len(allAfterSecond))
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// findAttemptByRef finds a bead with a matching gc.step_ref in the workflow.
func findAttemptByRef(t *testing.T, store beads.Store, _, stepRef string) beads.Bead {
	t.Helper()
	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, b := range all {
		if b.Metadata["gc.step_ref"] == stepRef {
			return b
		}
	}
	return beads.Bead{}
}
