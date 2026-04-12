package formula

import (
	"fmt"
	"strconv"
)

// ApplyRalph expands inline Ralph steps into control + iteration beads.
//
// A Ralph step:
//   - keeps its original step ID as the control bead (gc.kind=ralph)
//   - emits a first iteration: <step>.iteration.1
//
// The control bead blocks on the iteration. When the iteration closes, the
// controller re-activates the control bead to run the check script and
// optionally spawn the next iteration via molecule.Attach.
//
// Downstream steps continue to depend on the original logical step ID.
func ApplyRalph(steps []*Step) ([]*Step, error) {
	result := make([]*Step, 0, len(steps))

	for _, step := range steps {
		if step.Ralph == nil {
			clone := cloneStep(step)
			if len(step.Children) > 0 {
				children, err := ApplyRalph(step.Children)
				if err != nil {
					return nil, err
				}
				clone.Children = children
			}
			result = append(result, clone)
			continue
		}

		expanded, err := expandRalph(step)
		if err != nil {
			return nil, err
		}
		result = append(result, expanded...)
	}

	return result, nil
}

func expandRalph(step *Step) ([]*Step, error) {
	if step.Ralph == nil {
		return nil, fmt.Errorf("expanding ralph: step %q missing ralph spec", step.ID)
	}
	if step.Ralph.Check == nil {
		return nil, fmt.Errorf("expanding ralph %q: missing check spec", step.ID)
	}

	attempt := 1
	iterationID := fmt.Sprintf("%s.iteration.%d", step.ID, attempt)

	specStep, err := newSourceSpecStep(step)
	if err != nil {
		return nil, err
	}

	// Control bead — orchestrates ralph iterations.
	control := cloneStep(step)
	control.Ralph = nil
	control.Children = nil
	control.Metadata = withMetadata(control.Metadata, map[string]string{
		"gc.kind":          "ralph",
		"gc.step_id":       step.ID,
		"gc.max_attempts":  strconv.Itoa(step.Ralph.MaxAttempts),
		"gc.check_mode":    step.Ralph.Check.Mode,
		"gc.check_path":    step.Ralph.Check.Path,
		"gc.check_timeout": step.Ralph.Check.Timeout,
		"gc.control_epoch": "1",
	})
	control.Needs = appendUniqueCopy(control.Needs, iterationID)
	control.WaitsFor = ""

	if len(step.Children) > 0 {
		return expandNestedRalph(step, control, specStep, iterationID, attempt)
	}

	// Simple ralph (no children) — iteration is a single work bead.
	iteration := cloneStep(step)
	iteration.ID = iterationID
	iteration.Ralph = nil
	iteration.OnComplete = nil
	iteration.Children = nil
	iteration.Metadata = withMetadata(iteration.Metadata, map[string]string{
		"gc.attempt":       strconv.Itoa(attempt),
		"gc.step_id":       step.ID,
		"gc.ralph_step_id": step.ID,
		"gc.step_ref":      iterationID,
	})
	delete(iteration.Metadata, "gc.scope_ref")
	delete(iteration.Metadata, "gc.scope_role")
	delete(iteration.Metadata, "gc.on_fail")
	if step.OnComplete != nil {
		iteration.Metadata["gc.output_json_required"] = "true"
	}
	iteration.SourceLocation = fmt.Sprintf("%s.ralph.iteration.%d", step.SourceLocation, attempt)

	return []*Step{control, specStep, iteration}, nil
}

func expandNestedRalph(step, control, specStep *Step, iterationID string, attempt int) ([]*Step, error) {
	bodySteps, err := ApplyRalph(step.Children)
	if err != nil {
		return nil, err
	}
	bodyIDs := collectRalphBodyStepIDs(bodySteps)
	flattenedBody, topLevelBodyIDs := namespaceRalphBodySteps(bodySteps, iterationID, step, attempt, bodyIDs)
	if step.OnComplete != nil {
		markRalphBodyOutputSinks(flattenedBody)
	}

	// Iteration scope bead — wraps the children for this attempt.
	iteration := cloneStep(step)
	iteration.ID = iterationID
	iteration.Ralph = nil
	iteration.OnComplete = nil
	iteration.Children = nil
	iteration.DependsOn = nil
	iteration.Needs = append([]string{}, topLevelBodyIDs...)
	iteration.WaitsFor = ""
	iteration.SourceLocation = fmt.Sprintf("%s.ralph.iteration.%d", step.SourceLocation, attempt)
	iteration.Metadata = withMetadata(step.Metadata, map[string]string{
		"gc.kind":          "scope",
		"gc.scope_role":    "body",
		"gc.scope_name":    step.ID,
		"gc.step_id":       step.ID,
		"gc.ralph_step_id": step.ID,
		"gc.attempt":       strconv.Itoa(attempt),
		"gc.step_ref":      iterationID,
	})
	if step.OnComplete != nil {
		iteration.Metadata["gc.output_json_required"] = "true"
	}
	delete(iteration.Metadata, "gc.scope_ref")
	delete(iteration.Metadata, "gc.on_fail")

	out := []*Step{control, specStep, iteration}
	out = append(out, flattenedBody...)
	return out, nil
}

func collectRalphBodyStepIDs(steps []*Step) map[string]bool {
	ids := make(map[string]bool)
	var collect func([]*Step)
	collect = func(nodes []*Step) {
		for _, node := range nodes {
			ids[node.ID] = true
			if len(node.Children) > 0 {
				collect(node.Children)
			}
		}
	}
	collect(steps)
	return ids
}

func namespaceRalphBodySteps(steps []*Step, iterationID string, owner *Step, attempt int, bodyIDs map[string]bool) ([]*Step, []string) {
	var out []*Step
	var topLevel []string
	var walk func([]*Step, bool)
	walk = func(nodes []*Step, top bool) {
		for _, node := range nodes {
			if isSourceSpecStep(node) {
				out = append(out, namespaceSourceSpecStep(node, iterationID))
				continue
			}
			clone := cloneStep(node)
			clone.ID = iterationID + "." + node.ID
			clone.Children = nil
			clone.Ralph = nil
			clone.DependsOn = rewriteRalphBodyDependencies(node.DependsOn, iterationID, bodyIDs)
			clone.Needs = rewriteRalphBodyDependencies(node.Needs, iterationID, bodyIDs)
			clone.SourceLocation = fmt.Sprintf("%s.ralph.attempt.%d", node.SourceLocation, attempt)
			// Preserve the child's own step_id (set by expandRetry/expandRalph)
			// so that find_canonical_control can distinguish nested controls.
			// Fall back to the ralph owner's ID for plain (non-control) children.
			childStepID := node.Metadata["gc.step_id"]
			if childStepID == "" {
				childStepID = node.ID
			}
			clone.Metadata = withMetadata(clone.Metadata, map[string]string{
				"gc.scope_ref":     iterationID,
				"gc.on_fail":       metadataDefault(node.Metadata, "gc.on_fail", "abort_scope"),
				"gc.scope_role":    metadataDefault(node.Metadata, "gc.scope_role", "member"),
				"gc.step_id":       childStepID,
				"gc.ralph_step_id": owner.ID,
				"gc.attempt":       strconv.Itoa(attempt),
				"gc.step_ref":      clone.ID,
			})
			if top {
				topLevel = append(topLevel, clone.ID)
				clone.DependsOn = append(clone.DependsOn, owner.DependsOn...)
				clone.Needs = append(clone.Needs, owner.Needs...)
			}
			out = append(out, clone)
			if len(node.Children) > 0 {
				walk(node.Children, false)
			}
		}
	}
	walk(steps, true)
	return out, topLevel
}

func markRalphBodyOutputSinks(steps []*Step) {
	if len(steps) == 0 {
		return
	}

	byID := make(map[string]*Step, len(steps))
	referenced := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		if step == nil {
			continue
		}
		byID[step.ID] = step
	}
	for _, step := range steps {
		if step == nil {
			continue
		}
		for _, dep := range step.DependsOn {
			if _, ok := byID[dep]; ok {
				referenced[dep] = struct{}{}
			}
		}
		for _, need := range step.Needs {
			if _, ok := byID[need]; ok {
				referenced[need] = struct{}{}
			}
		}
	}
	for _, step := range steps {
		if step == nil {
			continue
		}
		switch step.Metadata["gc.kind"] {
		case "scope", "scope-check", "workflow-finalize", "fanout", "check", "ralph", "spec":
			continue
		}
		if step.Metadata["gc.scope_role"] == "teardown" {
			continue
		}
		if _, ok := referenced[step.ID]; ok {
			continue
		}
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		}
		step.Metadata["gc.output_json_required"] = "true"
	}
}

func rewriteRalphBodyDependencies(deps []string, iterationID string, bodyIDs map[string]bool) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, len(deps))
	for i, dep := range deps {
		if bodyIDs[dep] {
			out[i] = iterationID + "." + dep
			continue
		}
		out[i] = dep
	}
	return out
}

func metadataDefault(meta map[string]string, key, def string) string {
	if meta != nil {
		if value := meta[key]; value != "" {
			return value
		}
	}
	return def
}

func withMetadata(base map[string]string, extra map[string]string) map[string]string {
	size := len(base) + len(extra)
	if size == 0 {
		return nil
	}
	out := make(map[string]string, size)
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func appendUniqueCopy(slice []string, item string) []string {
	if item == "" {
		if len(slice) == 0 {
			return nil
		}
		out := make([]string, len(slice))
		copy(out, slice)
		return out
	}
	for _, s := range slice {
		if s == item {
			out := make([]string, len(slice))
			copy(out, slice)
			return out
		}
	}
	out := make([]string, 0, len(slice)+1)
	out = append(out, slice...)
	out = append(out, item)
	return out
}
