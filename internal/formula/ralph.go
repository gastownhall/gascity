package formula

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
	// Runtime control metadata keeps legacy ralph keys so existing controller
	// and dispatch paths remain stable while the public formula surface uses
	// the canonical "check" spelling.
	controlMeta := map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindRalph,
		beadmeta.StepIDMetadataKey:       step.ID,
		beadmeta.MaxAttemptsMetadataKey:  strconv.Itoa(step.Ralph.MaxAttempts),
		beadmeta.CheckModeMetadataKey:    step.Ralph.Check.Mode,
		beadmeta.CheckPathMetadataKey:    step.Ralph.Check.Path,
		beadmeta.CheckTimeoutMetadataKey: step.Ralph.Check.Timeout,
		beadmeta.ControlEpochMetadataKey: "1",
	}
	if step.Timeout != "" {
		controlMeta[beadmeta.StepTimeoutMetadataKey] = step.Timeout
	}
	control.Metadata = withMetadata(control.Metadata, controlMeta)
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
	// These runtime keys are internal control-bead metadata, not user-facing
	// formula syntax, so they intentionally retain legacy ralph naming.
	iteration.Metadata = withMetadata(iteration.Metadata, map[string]string{
		beadmeta.AttemptMetadataKey:     strconv.Itoa(attempt),
		beadmeta.StepIDMetadataKey:      step.ID,
		beadmeta.RalphStepIDMetadataKey: step.ID,
		beadmeta.StepRefMetadataKey:     iterationID,
		// gc.control_for is the durable lineage pointer to the ralph control
		// (step.ID here, which the control carries as gc.step_id).
		beadmeta.ControlForMetadataKey: step.ID,
	})
	delete(iteration.Metadata, beadmeta.ScopeRefMetadataKey)
	delete(iteration.Metadata, beadmeta.ScopeRoleMetadataKey)
	delete(iteration.Metadata, beadmeta.OnFailMetadataKey)
	if step.OnComplete != nil {
		iteration.Metadata[beadmeta.OutputJSONRequiredMetadataKey] = "true"
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
		beadmeta.KindMetadataKey:        beadmeta.KindScope,
		beadmeta.ScopeRoleMetadataKey:   beadmeta.ScopeRoleBody,
		beadmeta.ScopeNameMetadataKey:   step.ID,
		beadmeta.StepIDMetadataKey:      step.ID,
		beadmeta.RalphStepIDMetadataKey: step.ID,
		beadmeta.AttemptMetadataKey:     strconv.Itoa(attempt),
		beadmeta.StepRefMetadataKey:     iterationID,
		// gc.control_for on the scope root only (body children hang off it via
		// gc.scope_ref and are not attempt roots — they must not be stamped).
		beadmeta.ControlForMetadataKey: step.ID,
	})
	if step.OnComplete != nil {
		iteration.Metadata[beadmeta.OutputJSONRequiredMetadataKey] = "true"
	}
	delete(iteration.Metadata, beadmeta.ScopeRefMetadataKey)
	delete(iteration.Metadata, beadmeta.OnFailMetadataKey)

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
			childStepID := node.Metadata[beadmeta.StepIDMetadataKey]
			if childStepID == "" {
				childStepID = node.ID
			}
			childMeta := map[string]string{
				beadmeta.ScopeRefMetadataKey:    iterationID,
				beadmeta.OnFailMetadataKey:      metadataDefault(node.Metadata, beadmeta.OnFailMetadataKey, "abort_scope"),
				beadmeta.ScopeRoleMetadataKey:   metadataDefault(node.Metadata, beadmeta.ScopeRoleMetadataKey, beadmeta.ScopeRoleMember),
				beadmeta.StepIDMetadataKey:      childStepID,
				beadmeta.RalphStepIDMetadataKey: owner.ID,
				beadmeta.AttemptMetadataKey:     strconv.Itoa(attempt),
				beadmeta.StepRefMetadataKey:     clone.ID,
			}
			// A nested control's attempt/iteration root carries gc.control_for as
			// the bare inner-control step id (stamped by expandRetry/expandRalph
			// before this body was namespaced). Rewrite it to the namespaced
			// control ref (iterationID-prefixed, matching the cloned inner
			// control's gc.step_ref above) so findLatestAttempt scopes it to THIS
			// outer iteration's inner control instead of matching every sibling
			// outer iteration through the shared bare step id. This mirrors the
			// runtime buildNestedControlSeed stamp for outer iterations 2+ (both
			// yield the inner control's namespaced ref); the bare value only
			// remains on top-level attempt roots, where the step id is unique per
			// workflow root so no cross-iteration collision exists (S38).
			if cf := strings.TrimSpace(node.Metadata[beadmeta.ControlForMetadataKey]); cf != "" {
				childMeta[beadmeta.ControlForMetadataKey] = iterationID + "." + cf
			}
			clone.Metadata = withMetadata(clone.Metadata, childMeta)
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
		// Control/structural kinds are never worker-executed, so they cannot
		// honor gc.output_json_required. KindRalph is additionally exempt
		// here: a nested ralph control's output contract is owned by its own
		// OnComplete, not by the enclosing body.
		if kind := step.Metadata[beadmeta.KindMetadataKey]; beadmeta.IsScopeCheckExemptKind(kind) || kind == beadmeta.KindRalph {
			continue
		}
		if step.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
			continue
		}
		if _, ok := referenced[step.ID]; ok {
			continue
		}
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		}
		step.Metadata[beadmeta.OutputJSONRequiredMetadataKey] = "true"
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
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]string)
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
	out := make([]string, 0, len(slice))
	out = append(out, slice...)
	out = append(out, item)
	return out
}
