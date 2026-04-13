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
		// Invariant violation: control bead should not be ready if attempt is open.
		return ControlResult{}, fmt.Errorf("%s: latest attempt %s is %s, not closed (invariant violation)", bead.ID, attempt.ID, attempt.Status)
	}

	attemptNum, _ := strconv.Atoi(attempt.Metadata["gc.attempt"])
	result := classifyRetryAttempt(attempt)

	// Record decision in attempt log.
	if err := appendAttemptLog(store, bead.ID, attemptNum, result.Outcome, result.Reason); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}

	switch result.Outcome {
	case "pass":
		if outputJSON := attempt.Metadata["gc.output_json"]; outputJSON != "" {
			if err := store.SetMetadata(bead.ID, "gc.output_json", outputJSON); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating output: %w", bead.ID, err)
			}
		}
		if err := propagateRetrySubjectMetadata(store, bead.ID, attempt); err != nil {
			return ControlResult{}, fmt.Errorf("%s: propagating metadata: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "pass"}, nil

	case "hard":
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.failed_attempt":    strconv.Itoa(attemptNum),
			"gc.failure_class":     "hard",
			"gc.failure_reason":    result.Reason,
			"gc.final_disposition": "hard_fail",
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking hard fail: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, "fail"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "hard-fail"}, nil

	case "transient":
		if attemptNum >= maxAttempts {
			return handleRetryExhaustion(store, bead.ID, attemptNum, result.Reason, onExhausted)
		}

		// Spawn next attempt.
		nextAttempt := attemptNum + 1
		if err := spawnNextAttempt(context.Background(), store, bead, nextAttempt, opts); err != nil {
			// Controller-internal failure → close with hard error.
			_ = store.SetMetadataBatch(bead.ID, map[string]string{
				"gc.controller_error":  err.Error(),
				"gc.final_disposition": "controller_error",
			})
			_ = setOutcomeAndClose(store, bead.ID, "fail")
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
		return ControlResult{}, fmt.Errorf("%s: latest iteration %s is %s, not closed (invariant violation)", bead.ID, iteration.ID, iteration.Status)
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

	if err := appendAttemptLog(store, bead.ID, iterationNum, checkResult.Outcome, checkResult.Stderr); err != nil {
		return ControlResult{}, fmt.Errorf("%s: recording attempt log: %w", bead.ID, err)
	}

	if checkResult.Outcome == convergence.GatePass {
		if outputJSON := iteration.Metadata["gc.output_json"]; outputJSON != "" {
			if err := store.SetMetadata(bead.ID, "gc.output_json", outputJSON); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating output: %w", bead.ID, err)
			}
		}
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "pass"}, nil
	}

	if iterationNum >= maxAttempts {
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.outcome":        "fail",
			"gc.failed_attempt": strconv.Itoa(iterationNum),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking exhausted: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, "fail"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing exhausted: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "fail"}, nil
	}

	// Spawn next iteration.
	nextIteration := iterationNum + 1
	if err := spawnNextAttempt(context.Background(), store, bead, nextIteration, opts); err != nil {
		_ = store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.controller_error":  err.Error(),
			"gc.final_disposition": "controller_error",
		})
		_ = setOutcomeAndClose(store, bead.ID, "fail")
		return ControlResult{}, fmt.Errorf("%s: spawning iteration %d: %w", bead.ID, nextIteration, err)
	}

	return ControlResult{Processed: true, Action: "retry", Created: 1}, nil
}

func handleRetryExhaustion(store beads.Store, beadID string, attemptNum int, reason, onExhausted string) (ControlResult, error) {
	if onExhausted == "soft_fail" {
		if err := store.SetMetadataBatch(beadID, map[string]string{
			"gc.failed_attempt":    strconv.Itoa(attemptNum),
			"gc.failure_class":     "transient",
			"gc.failure_reason":    reason,
			"gc.final_disposition": "soft_fail",
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking soft-fail: %w", beadID, err)
		}
		if err := setOutcomeAndClose(store, beadID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing soft-failed: %w", beadID, err)
		}
		return ControlResult{Processed: true, Action: "soft-fail"}, nil
	}

	if err := store.SetMetadataBatch(beadID, map[string]string{
		"gc.failed_attempt":    strconv.Itoa(attemptNum),
		"gc.failure_class":     "transient",
		"gc.failure_reason":    reason,
		"gc.final_disposition": "hard_fail",
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: marking exhausted: %w", beadID, err)
	}
	if err := setOutcomeAndClose(store, beadID, "fail"); err != nil {
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
	executionRoute := control.Metadata["gc.execution_routed_to"]
	routeCfg := loadAttemptRouteConfig(opts.CityPath)
	for i := range recipe.Steps {
		if recipe.Steps[i].Metadata["gc.kind"] == "spec" {
			continue
		}
		target := strings.TrimSpace(recipe.Steps[i].Metadata["gc.routed_to"])
		if target == "" {
			target = strings.TrimSpace(recipe.Steps[i].Assignee)
		}
		if target == "" {
			target = executionRoute
		}
		if target == "" {
			continue
		}
		applyAttemptStepRoute(&recipe.Steps[i], target, routeCfg)
	}

	epoch := 0
	if raw := control.Metadata["gc.control_epoch"]; raw != "" {
		epoch, _ = strconv.Atoi(raw)
	}

	_, err := molecule.Attach(ctx, store, recipe, control.ID, molecule.AttachOptions{
		IdempotencyKey: fmt.Sprintf("%s:attempt:%d", control.ID, attemptNum),
		ExpectedEpoch:  epoch,
	})
	return err
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

	return recipe
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

func applyAttemptStepRoute(step *formula.RecipeStep, target string, cfg *config.City) {
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	if binding, ok := resolveAttemptRouteBinding(target, cfg); ok {
		step.Metadata["gc.routed_to"] = binding.qualifiedName
		step.Metadata["gc.execution_routed_to"] = binding.qualifiedName
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

type attemptRouteBinding struct {
	qualifiedName string
	metadataOnly  bool
	sessionName   string
}

func resolveAttemptRouteBinding(target string, cfg *config.City) (attemptRouteBinding, bool) {
	if cfg == nil || strings.TrimSpace(target) == "" {
		return attemptRouteBinding{}, false
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

	if named := config.FindNamedSession(cfg, target); named != nil {
		return attemptRouteBinding{
			qualifiedName: named.QualifiedName(),
			sessionName:   config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, named.QualifiedName()),
		}, true
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
	if agentCfg == nil {
		return false
	}
	maxSess := agentCfg.EffectiveMaxActiveSessions()
	return maxSess == nil || *maxSess != 1
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

	controlRef := control.Metadata["gc.step_ref"]
	if controlRef == "" {
		controlRef = control.ID
	}

	var latest beads.Bead
	latestAttempt := 0

	controlKind := control.Metadata["gc.kind"]
	for _, b := range all {
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

	return latest, nil
}

// appendAttemptLog records a retry/ralph decision to the control bead's
// gc.attempt_log metadata.
func appendAttemptLog(store beads.Store, controlID string, attempt int, outcome, reason string) error {
	control, err := store.Get(controlID)
	if err != nil {
		return err
	}

	var log []map[string]string
	if raw := control.Metadata["gc.attempt_log"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &log)
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
		return err
	}

	return store.SetMetadata(controlID, "gc.attempt_log", string(logJSON))
}

// Note: listByWorkflowRoot, setOutcomeAndClose, propagateRetrySubjectMetadata,
// classifyRetryAttempt, retryPreservedAssignee, and runRalphCheck are defined
// in runtime.go, retry.go, and ralph.go respectively.
