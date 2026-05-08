package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
)

// processRetryControl handles a retry control bead when it becomes ready
// (its blocking dep on the latest attempt has resolved).
func processRetryControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	maxAttempts, err := strconv.Atoi(bead.Metadata["gc.max_attempts"])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata["gc.max_attempts"])
	}
	onExhausted := bead.Metadata["gc.on_exhausted"]
	if onExhausted == "" {
		onExhausted = "hard_fail"
	}

	// Find the most recent attempt.
	attempt, err := findLatestAttempt(store, bead)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: finding latest attempt: %w", bead.ID, err)
	}
	if attempt.ID == "" {
		return ControlResult{}, fmt.Errorf("%s: no attempt found", bead.ID)
	}
	if attempt.Status != "closed" {
		if err := ensureBlockingDependency(store, bead.ID, attempt.ID); err != nil {
			return ControlResult{}, fmt.Errorf("%s: blocking on pending attempt %s: %w", bead.ID, attempt.ID, err)
		}
		return ControlResult{}, ErrControlPending
	}

	attemptNum, _ := strconv.Atoi(attempt.Metadata["gc.attempt"])
	result := classifyRetryAttempt(attempt)
	attemptLog, err := appendAttemptLogValue(bead.Metadata["gc.attempt_log"], attemptNum, result.Outcome, result.Reason)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}

	switch result.Outcome {
	case "pass":
		closeMetadata := map[string]string{
			"gc.attempt_log": attemptLog,
			"gc.outcome":     "pass",
		}
		if outputJSON := attempt.Metadata["gc.output_json"]; outputJSON != "" {
			closeMetadata["gc.output_json"] = outputJSON
		}
		copyNonGCMetadata(closeMetadata, attempt.Metadata)
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMember(store, bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "pass", Skipped: scopeResult.Skipped}, nil

	case "hard":
		if err := updateMetadataAndClose(store, bead.ID, map[string]string{
			"gc.attempt_log":       attemptLog,
			"gc.outcome":           "fail",
			"gc.failed_attempt":    strconv.Itoa(attemptNum),
			"gc.failure_class":     "hard",
			"gc.failure_reason":    result.Reason,
			"gc.final_disposition": "hard_fail",
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMember(store, bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "hard-fail", Skipped: scopeResult.Skipped}, nil

	case "transient":
		if attemptNum >= maxAttempts {
			exhaustedResult, err := handleRetryExhaustion(store, bead.ID, attemptNum, result.Reason, onExhausted, attemptLog)
			if err != nil {
				return ControlResult{}, err
			}
			scopeResult, err := reconcileClosedScopeMember(store, bead.ID)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
			}
			exhaustedResult.Skipped += scopeResult.Skipped
			return exhaustedResult, nil
		}

		// Spawn next attempt.
		if err := store.SetMetadata(bead.ID, "gc.attempt_log", attemptLog); err != nil {
			return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
		}
		nextAttempt := attemptNum + 1
		if err := spawnNextAttempt(context.Background(), store, bead, nextAttempt, opts); err != nil {
			// Controller-internal failure → close with hard error.
			_ = store.SetMetadataBatch(bead.ID, map[string]string{
				"gc.controller_error":  err.Error(),
				"gc.final_disposition": "controller_error",
			})
			_ = setOutcomeAndClose(store, bead.ID, "fail")
			// Reconcile any enclosing scope so a controller_error terminal
			// closure does not leave the scope body stalled.
			_, _ = reconcileClosedScopeMember(store, bead.ID)
			return ControlResult{}, fmt.Errorf("%s: spawning attempt %d: %w", bead.ID, nextAttempt, err)
		}

		return ControlResult{Processed: true, Action: "retry", Created: 1}, nil

	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported outcome %q", bead.ID, result.Outcome)
	}
}

// processRalphControl handles a ralph control bead when it becomes ready.
func processRalphControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	maxAttempts, err := strconv.Atoi(bead.Metadata["gc.max_attempts"])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata["gc.max_attempts"])
	}

	// Find the most recent iteration.
	iteration, err := findLatestAttempt(store, bead)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: finding latest iteration: %w", bead.ID, err)
	}
	if iteration.ID == "" {
		return ControlResult{}, fmt.Errorf("%s: no iteration found", bead.ID)
	}
	if iteration.Status != "closed" {
		if err := ensureBlockingDependency(store, bead.ID, iteration.ID); err != nil {
			return ControlResult{}, fmt.Errorf("%s: blocking on pending iteration %s: %w", bead.ID, iteration.ID, err)
		}
		return ControlResult{}, ErrControlPending
	}

	iterationNum, _ := strconv.Atoi(iteration.Metadata["gc.attempt"])

	// Propagate non-gc metadata from the iteration to the ralph control
	// BEFORE running the check. This makes the iteration's output (e.g.,
	// review.verdict) visible on the ralph bead for check scripts that
	// read $GC_BEAD_ID metadata.
	if err := propagateRetrySubjectMetadata(store, bead.ID, iteration); err != nil {
		return ControlResult{}, fmt.Errorf("%s: propagating iteration metadata: %w", bead.ID, err)
	}
	// Reload the bead after metadata propagation so the check sees updated values.
	bead, err = store.Get(bead.ID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: reloading after propagation: %w", bead.ID, err)
	}

	// Run check script. The control bead carries the check config (gc.check_path etc),
	// and the iteration is the subject whose output is being checked.
	checkResult, err := runRalphCheck(store, bead, iteration, iterationNum, opts)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: running check: %w", bead.ID, err)
	}

	attemptLog, err := appendAttemptLogValue(bead.Metadata["gc.attempt_log"], iterationNum, checkResult.Outcome, checkResult.Stderr)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}

	if checkResult.Outcome == convergence.GatePass {
		closeMetadata := map[string]string{
			"gc.attempt_log": attemptLog,
			"gc.outcome":     "pass",
		}
		if outputJSON := iteration.Metadata["gc.output_json"]; outputJSON != "" {
			closeMetadata["gc.output_json"] = outputJSON
		}
		if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMember(store, bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "pass", Skipped: scopeResult.Skipped}, nil
	}

	if iterationNum >= maxAttempts {
		if err := updateMetadataAndClose(store, bead.ID, map[string]string{
			"gc.attempt_log":    attemptLog,
			"gc.outcome":        "fail",
			"gc.failed_attempt": strconv.Itoa(iterationNum),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing exhausted: %w", bead.ID, err)
		}
		scopeResult, err := reconcileClosedScopeMember(store, bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reconciling enclosing scope: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "fail", Skipped: scopeResult.Skipped}, nil
	}

	// Spawn next iteration.
	if err := store.SetMetadata(bead.ID, "gc.attempt_log", attemptLog); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}
	nextIteration := iterationNum + 1
	if err := spawnNextAttempt(context.Background(), store, bead, nextIteration, opts); err != nil {
		_ = store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.controller_error":  err.Error(),
			"gc.final_disposition": "controller_error",
		})
		_ = setOutcomeAndClose(store, bead.ID, "fail")
		// Reconcile any enclosing scope so a controller_error terminal
		// closure does not leave the scope body stalled.
		_, _ = reconcileClosedScopeMember(store, bead.ID)
		return ControlResult{}, fmt.Errorf("%s: spawning iteration %d: %w", bead.ID, nextIteration, err)
	}

	return ControlResult{Processed: true, Action: "retry", Created: 1}, nil
}

func ensureBlockingDependency(store beads.Store, issueID, dependsOnID string) error {
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.DependsOnID == dependsOnID && dep.Type == "blocks" {
			return nil
		}
	}
	return store.DepAdd(issueID, dependsOnID, "blocks")
}

func handleRetryExhaustion(store beads.Store, beadID string, attemptNum int, reason, onExhausted, attemptLog string) (ControlResult, error) {
	if onExhausted == "soft_fail" {
		if err := updateMetadataAndClose(store, beadID, map[string]string{
			"gc.attempt_log":       attemptLog,
			"gc.outcome":           "pass",
			"gc.failed_attempt":    strconv.Itoa(attemptNum),
			"gc.failure_class":     "transient",
			"gc.failure_reason":    reason,
			"gc.final_disposition": "soft_fail",
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing soft-failed: %w", beadID, err)
		}
		return ControlResult{Processed: true, Action: "soft-fail"}, nil
	}

	if err := updateMetadataAndClose(store, beadID, map[string]string{
		"gc.attempt_log":       attemptLog,
		"gc.outcome":           "fail",
		"gc.failed_attempt":    strconv.Itoa(attemptNum),
		"gc.failure_class":     "transient",
		"gc.failure_reason":    reason,
		"gc.final_disposition": "hard_fail",
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: closing exhausted: %w", beadID, err)
	}
	return ControlResult{Processed: true, Action: "fail"}, nil
}

// spawnNextAttempt deserializes the frozen step spec, builds an attempt recipe,
// and calls molecule.Attach to graft it onto the control bead.
func spawnNextAttempt(ctx context.Context, store beads.Store, control beads.Bead, attemptNum int, opts ProcessOptions) error {
	specJSON := control.Metadata["gc.source_step_spec"]
	if specJSON == "" {
		// New path: look up the spec bead.
		spec, err := findSpecBead(store, control)
		if err != nil {
			return fmt.Errorf("control bead %s: finding spec bead: %w", control.ID, err)
		}
		specJSON = spec.Description
	}

	var step formula.Step
	if err := json.Unmarshal([]byte(specJSON), &step); err != nil {
		return fmt.Errorf("deserializing step spec: %w", err)
	}

	recipe := buildAttemptRecipe(&step, control, attemptNum)

	// Attach bypasses graph compile routing, so spawned attempts need their
	// execution lane restored manually. Prefer each step's explicit target when
	// available, and only inherit the parent execution lane as a fallback.
	executionRoute := strings.TrimSpace(control.Metadata["gc.execution_routed_to"])
	routeCfg := loadAttemptRouteConfig(opts.CityPath)
	for i := range recipe.Steps {
		if recipe.Steps[i].Metadata["gc.kind"] == "spec" {
			continue
		}
		target := strings.TrimSpace(recipe.Steps[i].Metadata["gc.run_target"])
		if target == "" {
			target = strings.TrimSpace(recipe.Steps[i].Metadata["gc.routed_to"])
		}
		if target == "" {
			target = strings.TrimSpace(recipe.Steps[i].Assignee)
		}
		if target == "" {
			target = executionRoute
		} else {
			target = qualifyAttemptTargetWithSourceRoute(target, executionRoute, routeCfg)
		}
		if isAttemptControlKind(recipe.Steps[i].Metadata["gc.kind"]) {
			applyAttemptControlStepRoute(&recipe.Steps[i], target, routeCfg, store)
			continue
		}
		if target == "" {
			continue
		}
		applyAttemptStepRoute(&recipe.Steps[i], target, routeCfg, store)
	}

	epoch := 0
	if raw := control.Metadata["gc.control_epoch"]; raw != "" {
		epoch, _ = strconv.Atoi(raw)
	}

	result, err := molecule.Attach(ctx, store, recipe, control.ID, molecule.AttachOptions{
		IdempotencyKey: fmt.Sprintf("%s:attempt:%d", control.ID, attemptNum),
		ExpectedEpoch:  epoch,
	})
	if err != nil {
		return err
	}
	if err := closeAttachedSpecBeads(store, recipe, result.IDMapping); err != nil {
		return err
	}
	return nil
}

func qualifyAttemptTargetWithSourceRoute(target, sourceRoute string, cfg *config.City) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, "/") || cfg == nil {
		return target
	}
	sourceRoute = strings.TrimSpace(sourceRoute)
	slash := strings.IndexByte(sourceRoute, '/')
	if slash <= 0 {
		return target
	}
	candidate := sourceRoute[:slash] + "/" + target
	if config.FindAgent(cfg, candidate) != nil || config.FindNamedSession(cfg, candidate) != nil {
		return candidate
	}
	return target
}

// buildAttemptRecipe constructs a minimal formula.Recipe for one attempt
// from the frozen step spec.
func buildAttemptRecipe(step *formula.Step, control beads.Bead, attemptNum int) *formula.Recipe {
	// stepID is the bare logical ID for metadata grouping.
	stepID := control.Metadata["gc.step_id"]
	if stepID == "" {
		stepID = control.ID
	}
	// stepRef is the fully namespaced ref (e.g., mol-demo-v2.self-review)
	// so Attach-created beads match the same namespace as compiler-created ones.
	stepRef := control.Metadata["gc.step_ref"]
	if stepRef == "" {
		stepRef = stepID
	}

	var attemptPrefix string
	if step.Ralph != nil {
		attemptPrefix = fmt.Sprintf("%s.iteration.%d", stepRef, attemptNum)
	} else {
		attemptPrefix = fmt.Sprintf("%s.attempt.%d", stepRef, attemptNum)
	}

	// Root step for the attempt sub-DAG.
	// For ralph iterations with children, the root is a scope bead.
	// For simple retries, it's the work bead itself (no wrapper).
	rootKind := "task"
	if step.Ralph != nil && len(step.Children) > 0 {
		rootKind = "scope"
	}
	rootMeta := map[string]string{
		"gc.kind":     rootKind,
		"gc.attempt":  strconv.Itoa(attemptNum),
		"gc.step_id":  stepID,
		"gc.step_ref": attemptPrefix,
	}
	// Ralph iterations need scope metadata for grouping.
	if rootKind == "scope" {
		rootMeta["gc.scope_role"] = "body"
		rootMeta["gc.scope_name"] = stepID
		rootMeta["gc.ralph_step_id"] = stepID
	}
	rootStep := formula.RecipeStep{
		ID:       attemptPrefix,
		Title:    step.Title,
		Type:     step.Type,
		IsRoot:   true,
		Labels:   append([]string{}, step.Labels...),
		Assignee: step.Assignee,
		Metadata: rootMeta,
	}
	if step.Type == "" {
		rootStep.Type = "task"
	}

	recipe := &formula.Recipe{
		Name:  attemptPrefix,
		Steps: []formula.RecipeStep{rootStep},
	}

	// For steps with children (scoped ralph), add children as sub-steps.
	// Children may have retry/ralph config — propagate their metadata
	// so the beads get the correct gc.kind for logical grouping.
	if len(step.Children) > 0 {
		// Collect top-level child IDs so the scope bead blocks on them.
		var topChildIDs []string
		for _, child := range step.Children {
			topChildIDs = append(topChildIDs, attemptPrefix+"."+child.ID)
		}
		// Wire scope → children: scope closes when all children close.
		for _, cid := range topChildIDs {
			recipe.Deps = append(recipe.Deps, formula.RecipeDep{
				StepID:      attemptPrefix,
				DependsOnID: cid,
				Type:        "blocks",
			})
		}

		for _, child := range step.Children {
			childID := attemptPrefix + "." + child.ID
			childMeta := map[string]string{
				"gc.attempt":       strconv.Itoa(attemptNum),
				"gc.step_ref":      childID,
				"gc.step_id":       child.ID,
				"gc.scope_ref":     attemptPrefix,
				"gc.ralph_step_id": stepID,
				"gc.scope_role":    "member",
				"gc.on_fail":       "abort_scope",
			}
			// Copy formula-defined metadata from the child step.
			for k, v := range child.Metadata {
				if _, exists := childMeta[k]; !exists {
					childMeta[k] = v
				}
			}
			// Derive gc.kind and control metadata from retry/ralph config.
			if child.Retry != nil {
				childMeta["gc.kind"] = "retry"
				childMeta["gc.max_attempts"] = strconv.Itoa(child.Retry.MaxAttempts)
				childMeta["gc.control_epoch"] = "1"
				if child.Retry.OnExhausted != "" {
					childMeta["gc.on_exhausted"] = child.Retry.OnExhausted
				} else {
					childMeta["gc.on_exhausted"] = "hard_fail"
				}
				// Emit a spec bead for the nested retry so it can spawn
				// its own attempts without oversized metadata.
				if step := newSpecRecipeStep(childID, child); step != nil {
					recipe.Steps = append(recipe.Steps, *step)
				}
			}
			if child.Ralph != nil {
				childMeta["gc.kind"] = "ralph"
				childMeta["gc.max_attempts"] = strconv.Itoa(child.Ralph.MaxAttempts)
				childMeta["gc.control_epoch"] = "1"
				if child.Ralph.Check != nil {
					childMeta["gc.check_mode"] = child.Ralph.Check.Mode
					childMeta["gc.check_path"] = child.Ralph.Check.Path
					childMeta["gc.check_timeout"] = child.Ralph.Check.Timeout
					if child.Timeout != "" {
						childMeta["gc.step_timeout"] = child.Timeout
					}
				}
				if step := newSpecRecipeStep(childID, child); step != nil {
					recipe.Steps = append(recipe.Steps, *step)
				}
			}
			childStep := formula.RecipeStep{
				ID:          childID,
				Title:       child.Title,
				Description: child.Description,
				Type:        child.Type,
				Labels:      append([]string{}, child.Labels...),
				Assignee:    child.Assignee,
				Metadata:    childMeta,
			}
			if childStep.Type == "" {
				childStep.Type = "task"
			}
			recipe.Steps = append(recipe.Steps, childStep)
			// No parent-child dep to the iteration scope — it creates a
			// deadlock (scope waits for children, children wait for scope).
			// Children are associated with the iteration via gc.scope_ref
			// metadata, and their execution order comes from blocks deps.

			// Wire inter-child deps.
			for _, need := range child.Needs {
				needID := attemptPrefix + "." + need
				recipe.Deps = append(recipe.Deps, formula.RecipeDep{
					StepID:      childID,
					DependsOnID: needID,
					Type:        "blocks",
				})
			}
		}
	}

	applyAttemptRecipeScopeChecks(recipe)

	return recipe
}

func applyAttemptRecipeScopeChecks(recipe *formula.Recipe) {
	if recipe == nil || len(recipe.Steps) == 0 {
		return
	}

	existingStepIDs := make(map[string]struct{}, len(recipe.Steps))
	replacements := make(map[string]string)
	controls := make([]formula.RecipeStep, 0)
	controlDeps := make([]formula.RecipeDep, 0)
	for _, step := range recipe.Steps {
		existingStepIDs[step.ID] = struct{}{}
	}

	for _, step := range recipe.Steps {
		if !attemptRecipeStepNeedsScopeCheck(step) {
			continue
		}
		controlID := step.ID + "-scope-check"
		if _, exists := existingStepIDs[controlID]; exists {
			continue
		}

		replacements[step.ID] = controlID
		meta := map[string]string{
			"gc.kind":        "scope-check",
			"gc.scope_ref":   step.Metadata["gc.scope_ref"],
			"gc.scope_role":  "control",
			"gc.control_for": step.ID,
		}
		for _, key := range []string{"gc.step_id", "gc.ralph_step_id", "gc.attempt", "gc.on_fail"} {
			if value := step.Metadata[key]; value != "" {
				meta[key] = value
			}
		}
		controls = append(controls, formula.RecipeStep{
			ID:       controlID,
			Title:    "Finalize scope for " + step.Title,
			Type:     "task",
			Metadata: meta,
		})
		controlDeps = append(controlDeps, formula.RecipeDep{
			StepID:      controlID,
			DependsOnID: step.ID,
			Type:        "blocks",
		})
	}

	if len(controls) == 0 {
		return
	}

	for i := range recipe.Deps {
		if replacement, ok := replacements[recipe.Deps[i].DependsOnID]; ok {
			recipe.Deps[i].DependsOnID = replacement
		}
	}
	recipe.Steps = append(recipe.Steps, controls...)
	recipe.Deps = append(recipe.Deps, controlDeps...)
}

func attemptRecipeStepNeedsScopeCheck(step formula.RecipeStep) bool {
	if step.Metadata["gc.scope_ref"] == "" {
		return false
	}
	if step.Metadata["gc.scope_role"] == "teardown" {
		return false
	}
	switch step.Metadata["gc.kind"] {
	case "scope", "scope-check", "workflow-finalize", "fanout", "check", "spec":
		return false
	default:
		return true
	}
}

func loadAttemptRouteConfig(cityPath string) *config.City {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil
	}
	return cfg
}

func applyAttemptStepRoute(step *formula.RecipeStep, target string, cfg *config.City, store beads.Store) {
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	if binding, ok := resolveAttemptRouteBinding(target, cfg, store); ok {
		if binding.directSessionID != "" {
			delete(step.Metadata, "gc.routed_to")
			delete(step.Metadata, "gc.execution_routed_to")
			step.Labels = removeAttemptPoolLabels(step.Labels)
			step.Assignee = binding.directSessionID
			return
		}
		if binding.qualifiedName != "" {
			step.Metadata["gc.routed_to"] = binding.qualifiedName
			step.Metadata["gc.execution_routed_to"] = binding.qualifiedName
		} else {
			delete(step.Metadata, "gc.routed_to")
			delete(step.Metadata, "gc.execution_routed_to")
		}
		step.Labels = removeAttemptPoolLabels(step.Labels)
		if binding.metadataOnly {
			step.Assignee = ""
			return
		}
		step.Assignee = binding.sessionName
		return
	}

	// Target not found in config — route via metadata only and clear assignee
	// to avoid stale routing. Work discovery relies on gc.routed_to (tier 3).
	step.Metadata["gc.routed_to"] = target
	step.Metadata["gc.execution_routed_to"] = target
	step.Labels = removeAttemptPoolLabels(step.Labels)
	step.Assignee = ""
}

func applyAttemptControlStepRoute(step *formula.RecipeStep, executionTarget string, cfg *config.City, store beads.Store) {
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	resolvedExecutionTarget := strings.TrimSpace(executionTarget)
	if binding, ok := resolveAttemptRouteBinding(executionTarget, cfg, store); ok {
		switch {
		case binding.qualifiedName != "":
			resolvedExecutionTarget = binding.qualifiedName
			step.Metadata["gc.execution_routed_to"] = binding.qualifiedName
		case executionTarget != "":
			// Direct session delivery still executes via the named/session target,
			// but control beads themselves must remain on control-dispatcher.
			step.Metadata["gc.execution_routed_to"] = executionTarget
		default:
			delete(step.Metadata, "gc.execution_routed_to")
		}
	} else if executionTarget != "" {
		step.Metadata["gc.execution_routed_to"] = executionTarget
	} else {
		delete(step.Metadata, "gc.execution_routed_to")
	}
	step.Labels = removeAttemptPoolLabels(step.Labels)

	controlTarget := controlDispatcherTargetForExecutionTarget(resolvedExecutionTarget)
	if assignee, ok := resolveAttemptControlAssignee(controlTarget, cfg, store); ok {
		delete(step.Metadata, "gc.routed_to")
		step.Assignee = assignee
		return
	}

	step.Metadata["gc.routed_to"] = controlTarget
	step.Assignee = ""
}

func controlDispatcherTargetForExecutionTarget(executionTarget string) string {
	executionTarget = strings.TrimSpace(executionTarget)
	if slash := strings.IndexByte(executionTarget, '/'); slash > 0 {
		return executionTarget[:slash] + "/" + config.ControlDispatcherAgentName
	}
	return config.ControlDispatcherAgentName
}

func resolveAttemptControlAssignee(target string, cfg *config.City, store beads.Store) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if binding, ok := resolveAttemptRouteBinding(target, cfg, store); ok {
		if binding.directSessionID != "" {
			return binding.directSessionID, true
		}
		if binding.sessionName != "" {
			return binding.sessionName, true
		}
	}
	if cfg != nil {
		if named := config.FindNamedSession(cfg, target); named != nil {
			if spec, ok := session.FindNamedSessionSpec(cfg, cfg.EffectiveCityName(), named.QualifiedName()); ok && spec.SessionName != "" {
				return spec.SessionName, true
			}
		}
		if agentCfg := config.FindAgent(cfg, target); agentCfg != nil {
			if sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, agentCfg.QualifiedName()); sessionName != "" {
				return sessionName, true
			}
		}
	}
	return "", false
}

func isAttemptControlKind(kind string) bool {
	switch kind {
	case "check", "fanout", "retry-eval", "scope-check", "workflow-finalize", "retry", "ralph":
		return true
	default:
		return false
	}
}

type attemptRouteBinding struct {
	qualifiedName   string
	metadataOnly    bool
	sessionName     string
	directSessionID string
}

func resolveAttemptRouteBinding(target string, cfg *config.City, store beads.Store) (attemptRouteBinding, bool) {
	if strings.TrimSpace(target) == "" {
		return attemptRouteBinding{}, false
	}
	if cfg != nil {
		if named := config.FindNamedSession(cfg, target); named != nil {
			if spec, ok := session.FindNamedSessionSpec(cfg, cfg.EffectiveCityName(), named.QualifiedName()); ok {
				if store != nil {
					if candidates, err := session.NamedSessionResolutionCandidates(store, spec); err == nil {
						if bead, found := session.FindCanonicalNamedSessionBead(candidates, spec); found {
							return attemptRouteBinding{directSessionID: bead.ID}, true
						}
					}
				}
				if spec.SessionName != "" {
					return attemptRouteBinding{sessionName: spec.SessionName}, true
				}
			}
			return attemptRouteBinding{
				qualifiedName: named.QualifiedName(),
				metadataOnly:  true,
			}, true
		}

		if agentCfg := config.FindAgent(cfg, target); agentCfg != nil {
			binding := attemptRouteBinding{qualifiedName: agentCfg.QualifiedName()}
			if isAttemptMultiSessionTarget(agentCfg.QualifiedName(), cfg) {
				binding.metadataOnly = true
				return binding, true
			}
			binding.sessionName = config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, agentCfg.QualifiedName())
			return binding, true
		}
	}
	if store != nil {
		if id, err := session.ResolveSessionID(store, target); err == nil {
			return attemptRouteBinding{directSessionID: id}, true
		}
	}

	return attemptRouteBinding{}, false
}

func routedAttemptTarget(bead beads.Bead) string {
	if bead.Metadata == nil {
		return ""
	}
	if target := strings.TrimSpace(bead.Metadata["gc.execution_routed_to"]); target != "" {
		return target
	}
	return strings.TrimSpace(bead.Metadata["gc.routed_to"])
}

func isAttemptMultiSessionTarget(target string, cfg *config.City) bool {
	if cfg == nil || strings.TrimSpace(target) == "" {
		return false
	}
	agentCfg := config.FindAgent(cfg, target)
	return agentCfg != nil && agentCfg.SupportsInstanceExpansion()
}

func beadUsesMetadataPoolRoute(bead beads.Bead, cityPath string) bool {
	return beadUsesMetadataPoolRouteWithConfig(bead, loadAttemptRouteConfig(cityPath))
}

func beadUsesMetadataPoolRouteWithConfig(bead beads.Bead, cfg *config.City) bool {
	if isAttemptMultiSessionTarget(routedAttemptTarget(bead), cfg) {
		return true
	}
	// Legacy fallback: check pool labels on the bead. This function is always
	// called on the previous attempt's bead (which retains its original labels),
	// not on the newly cloned bead (which has pool labels stripped).
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "pool:") {
			return true
		}
	}
	return false
}

func removeAttemptPoolLabels(labels []string) []string {
	if len(labels) == 0 {
		return labels
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.HasPrefix(label, "pool:") {
			continue
		}
		out = append(out, label)
	}
	return out
}

// findSpecBead locates the spec bead for a control (retry/ralph) bead.
// The spec bead has gc.kind=spec and gc.spec_for matching the control's
// step ID, under the same workflow root.
func findSpecBead(store beads.Store, control beads.Bead) (beads.Bead, error) {
	rootID := control.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return beads.Bead{}, fmt.Errorf("missing gc.root_bead_id")
	}
	stepID := control.Metadata["gc.step_id"]
	if stepID == "" {
		stepID = control.Metadata["gc.step_ref"]
	}
	if stepID == "" {
		return beads.Bead{}, fmt.Errorf("missing gc.step_id")
	}
	stepRef := control.Metadata["gc.step_ref"]

	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return beads.Bead{}, err
	}
	for _, b := range all {
		if b.Metadata["gc.kind"] != "spec" {
			continue
		}
		if stepRef != "" && b.Metadata["gc.spec_for_ref"] == stepRef {
			return b, nil
		}
	}
	for _, b := range all {
		if b.Metadata["gc.kind"] == "spec" && b.Metadata["gc.spec_for"] == stepID {
			return b, nil
		}
	}
	return beads.Bead{}, fmt.Errorf("no spec bead found for step %q under root %s", stepID, rootID)
}

// newSpecRecipeStep builds a spec recipe step for a nested retry/ralph child.
// Returns nil if marshaling fails.
func newSpecRecipeStep(childID string, child *formula.Step) *formula.RecipeStep {
	specJSON, err := json.Marshal(child)
	if err != nil {
		return nil
	}
	return &formula.RecipeStep{
		ID:          childID + ".spec",
		Title:       "Step spec for " + child.Title,
		Type:        "spec",
		Description: string(specJSON),
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.spec_for":     child.ID,
			"gc.spec_for_ref": childID,
		},
	}
}

func closeAttachedSpecBeads(store beads.Store, recipe *formula.Recipe, idMapping map[string]string) error {
	if recipe == nil || len(recipe.Steps) == 0 || len(idMapping) == 0 {
		return nil
	}
	for _, step := range recipe.Steps {
		if step.Metadata["gc.kind"] != "spec" {
			continue
		}
		beadID := idMapping[step.ID]
		if beadID == "" {
			continue
		}
		if err := setOutcomeAndClose(store, beadID, "pass"); err != nil {
			return fmt.Errorf("closing spec bead %s: %w", beadID, err)
		}
	}
	return nil
}

// findLatestAttempt finds the most recent attempt/iteration child of a control bead.
// Matches by gc.step_ref pattern: the attempt's step_ref ends with
// .attempt.N or .iteration.N where the prefix matches the control's step_ref.
func findLatestAttempt(store beads.Store, control beads.Bead) (beads.Bead, error) {
	rootID := control.Metadata["gc.root_bead_id"]
	if rootID == "" {
		rootID = control.ID
	}

	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return beads.Bead{}, err
	}

	latest := latestAttemptFromCandidates(control, all)
	if latest.ID != "" {
		return latest, nil
	}

	deps, err := store.DepList(control.ID, "down")
	if err != nil {
		return beads.Bead{}, err
	}
	candidates := make([]beads.Bead, 0, len(deps))
	for _, dep := range deps {
		candidate, err := store.Get(dep.DependsOnID)
		if err != nil {
			return beads.Bead{}, err
		}
		candidates = append(candidates, candidate)
	}
	return latestAttemptFromCandidates(control, candidates), nil
}

func latestAttemptFromCandidates(control beads.Bead, candidates []beads.Bead) beads.Bead {
	controlRef := control.Metadata["gc.step_ref"]
	if controlRef == "" {
		controlRef = control.ID
	}

	var latest beads.Bead
	latestAttempt := 0
	controlKind := control.Metadata["gc.kind"]
	for _, b := range candidates {
		// Skip beads that are control infrastructure, not actual work.
		// For ralph controls, scope beads ARE the iterations — don't skip them.
		kind := b.Metadata["gc.kind"]
		switch kind {
		case "scope-check", "workflow-finalize", "fanout", "check", "retry-eval", "retry", "ralph", "workflow":
			continue
		case "scope":
			if controlKind != "ralph" {
				continue
			}
		}

		ref := b.Metadata["gc.step_ref"]
		if ref == "" {
			continue
		}

		// Match: attempt ref starts with the control's ref + ".attempt." or ".iteration."
		isAttempt := strings.HasPrefix(ref, controlRef+".attempt.") ||
			strings.HasPrefix(ref, controlRef+".iteration.")
		// Also match by step_id (ralph parent ID).
		stepID := control.Metadata["gc.step_id"]
		if !isAttempt && stepID != "" {
			isAttempt = strings.HasPrefix(ref, stepID+".attempt.") ||
				strings.HasPrefix(ref, stepID+".iteration.")
		}
		// Also match short refs from nested retries inside ralphs where the
		// step_ref is the bare child ID + ".attempt.N" (not fully namespaced).
		// Try progressively shorter suffixes of the control's step_ref.
		if !isAttempt {
			// First: extract after ".iteration.N." for compose.expand children
			// whose short refs include multi-segment IDs (e.g., "review-pipeline.review-codex").
			for _, marker := range []string{".iteration.", ".attempt."} {
				if idx := strings.LastIndex(controlRef, marker); idx >= 0 {
					rest := controlRef[idx+len(marker):]
					if dotIdx := strings.IndexByte(rest, '.'); dotIdx >= 0 {
						childRef := rest[dotIdx+1:]
						if childRef != "" {
							isAttempt = strings.HasPrefix(ref, childRef+".attempt.") ||
								strings.HasPrefix(ref, childRef+".iteration.")
						}
					}
				}
				if isAttempt {
					break
				}
			}
		}
		// Fallback: last dot segment (handles single-segment child IDs).
		if !isAttempt {
			if lastDot := strings.LastIndex(controlRef, "."); lastDot >= 0 {
				shortRef := controlRef[lastDot+1:]
				isAttempt = strings.HasPrefix(ref, shortRef+".attempt.") ||
					strings.HasPrefix(ref, shortRef+".iteration.")
			}
		}
		if !isAttempt {
			continue
		}

		attemptNum, _ := strconv.Atoi(b.Metadata["gc.attempt"])
		if attemptNum > latestAttempt {
			latestAttempt = attemptNum
			latest = b
		}
	}
	return latest
}

// appendAttemptLog records a retry/ralph decision to the control bead's
// gc.attempt_log metadata.
func appendAttemptLog(store beads.Store, controlID string, attempt int, outcome, reason string) error {
	control, err := store.Get(controlID)
	if err != nil {
		return err
	}
	logJSON, err := appendAttemptLogValue(control.Metadata["gc.attempt_log"], attempt, outcome, reason)
	if err != nil {
		return err
	}
	return store.SetMetadata(controlID, "gc.attempt_log", logJSON)
}

func appendAttemptLogValue(existing string, attempt int, outcome, reason string) (string, error) {
	var log []map[string]string
	if existing != "" {
		_ = json.Unmarshal([]byte(existing), &log)
	}

	entry := map[string]string{
		"attempt": strconv.Itoa(attempt),
		"outcome": outcome,
	}
	if reason != "" {
		entry["reason"] = reason
	}

	var action string
	switch outcome {
	case "pass":
		action = "close"
	case "hard":
		action = "hard-fail"
	case "transient":
		action = "retry"
	default:
		action = outcome
	}
	entry["action"] = action

	log = append(log, entry)
	logJSON, err := json.Marshal(log)
	if err != nil {
		return "", err
	}

	return string(logJSON), nil
}

func copyNonGCMetadata(dst, src map[string]string) {
	for key, value := range src {
		if key == "" || strings.HasPrefix(key, "gc.") {
			continue
		}
		dst[key] = value
	}
}

func updateMetadataAndClose(store beads.Store, beadID string, metadata map[string]string) error {
	status := "closed"
	return store.Update(beadID, beads.UpdateOpts{
		Status:   &status,
		Metadata: metadata,
	})
}

// Note: listByWorkflowRoot, setOutcomeAndClose, propagateRetrySubjectMetadata,
// classifyRetryAttempt, retryPreservedAssignee, and runRalphCheck are defined
// in runtime.go, retry.go, and ralph.go respectively.
