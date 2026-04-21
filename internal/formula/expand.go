// Package formula provides expansion operators for macro-style step transformation.
//
// Expansion operators replace target steps with template-expanded steps.
// Unlike advice operators which insert steps around targets, expansion
// operators completely replace the target with the expansion template.
//
// Two operators are supported:
//   - expand: Apply template to a single target step
//   - map: Apply template to all steps matching a pattern
//
// Templates use {target} and {target.description} placeholders that are
// substituted with the target step's values during expansion.
//
// A maximum expansion depth (default 5) prevents runaway nested expansions.
// This allows massive work generation while providing a safety bound.
package formula

import (
	"fmt"
	"strings"
)

// DefaultMaxExpansionDepth is the maximum depth for recursive template expansion.
// This prevents runaway nested expansions while still allowing substantial work
// generation. The limit applies to template children, not to expansion rules.
const DefaultMaxExpansionDepth = 5

// ApplyExpansions applies all expand and map rules to a formula's steps.
// Returns a new steps slice with expansions applied.
// The original steps slice is not modified.
//
// The parser is used to load referenced expansion formulas by name.
// If parser is nil, no expansions are applied.
func ApplyExpansions(steps []*Step, compose *ComposeRules, parser *Parser) ([]*Step, error) {
	return ApplyExpansionsWithVars(steps, compose, parser, nil)
}

// ApplyExpansionsWithVars applies all expand and map rules to a formula's
// steps, resolving any override values against the provided parent vars before
// merging them into the expansion formula's own defaults.
func ApplyExpansionsWithVars(steps []*Step, compose *ComposeRules, parser *Parser, parentVars map[string]string) ([]*Step, error) {
	if compose == nil || parser == nil {
		return steps, nil
	}

	if len(compose.Expand) == 0 && len(compose.Map) == 0 {
		return steps, nil
	}

	// Build a map of step ID -> step for quick lookup
	stepMap := buildStepMap(steps)

	// Track which steps have been expanded (to avoid double expansion)
	expanded := make(map[string]bool)

	// Apply expand rules first (specific targets)
	result := steps
	for _, rule := range compose.Expand {
		targetStep, ok := stepMap[rule.Target]
		if !ok {
			return nil, fmt.Errorf("expand: target step %q not found", rule.Target)
		}

		if expanded[rule.Target] {
			continue // Already expanded
		}

		expFormula, err := loadResolvedExpansionFormula(parser, rule.With, "expand")
		if err != nil {
			return nil, err
		}

		// Merge formula default vars with rule overrides
		vars := mergeVars(expFormula, resolveOverrideVars(rule.Vars, parentVars))

		// Expand the target step (start at depth 0)
		expandedSteps, err := expandStep(targetStep, expFormula.Template, 0, vars)
		if err != nil {
			return nil, fmt.Errorf("expand %q: %w", rule.Target, err)
		}
		expandedSteps, err = materializeExpandedStepConditions(expandedSteps, mergeConditionVars(parentVars, vars))
		if err != nil {
			return nil, fmt.Errorf("expand %q: %w", rule.Target, err)
		}
		if err := validateExpandedStepTimeouts(expandedSteps, fmt.Sprintf("expand %q", rule.Target)); err != nil {
			return nil, err
		}

		// Propagate target step's dependencies to root steps of the expansion.
		// Root steps are those whose needs/dependsOn only reference IDs within
		// the expansion (or are empty) — they are the entry points.
		propagateTargetDeps(targetStep, expandedSteps)

		// Replace the target step with expanded steps
		result = replaceStep(result, rule.Target, expandedSteps)
		expanded[rule.Target] = true

		// Update dependencies: any step that depended on the target should now
		// depend on the last step of the expansion
		if len(expandedSteps) > 0 {
			lastStepID := expandedSteps[len(expandedSteps)-1].ID
			result = UpdateDependenciesForExpansion(result, rule.Target, lastStepID)
		}

		// Rebuild stepMap from result so subsequent iterations see resolved deps
		stepMap = buildStepMap(result)
	}

	// Apply map rules (pattern matching)
	for _, rule := range compose.Map {
		expFormula, err := loadResolvedExpansionFormula(parser, rule.With, "map")
		if err != nil {
			return nil, err
		}

		// Merge formula default vars with rule overrides
		vars := mergeVars(expFormula, resolveOverrideVars(rule.Vars, parentVars))

		// Find all matching steps (including nested children)
		// Rebuild stepMap to capture any changes from previous expansions
		stepMap = buildStepMap(result)
		var toExpand []*Step
		for id, step := range stepMap {
			if MatchGlob(rule.Select, id) && !expanded[id] {
				toExpand = append(toExpand, step)
			}
		}

		// Expand each matching step
		for _, targetStep := range toExpand {
			expandedSteps, err := expandStep(targetStep, expFormula.Template, 0, vars)
			if err != nil {
				return nil, fmt.Errorf("map %q -> %q: %w", rule.Select, targetStep.ID, err)
			}
			expandedSteps, err = materializeExpandedStepConditions(expandedSteps, mergeConditionVars(parentVars, vars))
			if err != nil {
				return nil, fmt.Errorf("map %q -> %q: %w", rule.Select, targetStep.ID, err)
			}
			if err := validateExpandedStepTimeouts(expandedSteps, fmt.Sprintf("map %q -> %q", rule.Select, targetStep.ID)); err != nil {
				return nil, err
			}

			// Propagate target step's dependencies to root steps of the expansion
			propagateTargetDeps(targetStep, expandedSteps)

			result = replaceStep(result, targetStep.ID, expandedSteps)
			expanded[targetStep.ID] = true

			// Update dependencies: any step that depended on the target should now
			// depend on the last step of the expansion
			if len(expandedSteps) > 0 {
				lastStepID := expandedSteps[len(expandedSteps)-1].ID
				result = UpdateDependenciesForExpansion(result, targetStep.ID, lastStepID)
			}

			// stepMap is rebuilt at the top of the outer loop (line 125)
		}
	}

	validationSteps := result
	if parentVars != nil {
		filteredSteps, err := FilterStepsByCondition(result, parentVars)
		if err != nil {
			return nil, fmt.Errorf("filtering conditioned steps after expansion: %w", err)
		}
		validationSteps = filteredSteps
	}

	// Validate no duplicate step IDs after expansion.
	if dups := findDuplicateStepIDs(validationSteps); len(dups) > 0 {
		return nil, fmt.Errorf("duplicate step IDs after expansion: %v", dups)
	}

	return result, nil
}

func loadResolvedExpansionFormula(parser *Parser, name, context string) (*Formula, error) {
	expFormula, err := parser.LoadByName(name)
	if err != nil {
		return nil, fmt.Errorf("%s: loading %q: %w", context, name, err)
	}

	resolved, err := parser.Resolve(expFormula)
	if err != nil {
		return nil, fmt.Errorf("%s: resolving %q: %w", context, name, err)
	}

	if resolved.Type != TypeExpansion {
		return nil, fmt.Errorf("%s: %q is not an expansion formula (type=%s)", context, name, resolved.Type)
	}

	if len(resolved.Template) == 0 {
		return nil, fmt.Errorf("%s: %q has no template steps", context, name)
	}

	return resolved, nil
}

func resolveOverrideVars(overrides map[string]string, parentVars map[string]string) map[string]string {
	if len(overrides) == 0 {
		return nil
	}
	resolved := make(map[string]string, len(overrides))
	for name, value := range overrides {
		if len(parentVars) == 0 {
			resolved[name] = value
			continue
		}
		resolved[name] = substituteVars(value, parentVars)
	}
	return resolved
}

// findDuplicateStepIDs returns any duplicate step IDs found in the steps slice.
// It recursively checks all children.
func findDuplicateStepIDs(steps []*Step) []string {
	seen := make(map[string]int)
	countStepIDs(steps, seen)

	var dups []string
	for id, count := range seen {
		if count > 1 {
			dups = append(dups, id)
		}
	}
	return dups
}

// countStepIDs counts occurrences of each step ID recursively.
func countStepIDs(steps []*Step, counts map[string]int) {
	for _, step := range steps {
		counts[step.ID]++
		if len(step.Children) > 0 {
			countStepIDs(step.Children, counts)
		}
	}
}

// expandStep expands a target step using the given template.
// Returns the expanded steps with placeholders substituted.
// The depth parameter tracks recursion depth for children; if it exceeds
// DefaultMaxExpansionDepth, an error is returned.
// The vars parameter provides variable values for {varname} substitution.
func expandStep(target *Step, template []*Step, depth int, vars map[string]string) ([]*Step, error) {
	if depth > DefaultMaxExpansionDepth {
		return nil, fmt.Errorf("expansion depth limit exceeded: max %d levels (currently at %d) - step %q",
			DefaultMaxExpansionDepth, depth, target.ID)
	}

	result := make([]*Step, 0, len(template))

	for _, tmpl := range template {
		expanded := cloneStep(tmpl)
		expanded.ID = substituteVars(substituteTargetPlaceholders(tmpl.ID, target), vars)
		expanded.Title = substituteVars(substituteTargetPlaceholders(tmpl.Title, target), vars)
		expanded.Description = substituteVars(substituteTargetPlaceholders(tmpl.Description, target), vars)
		expanded.Notes = substituteVars(substituteTargetPlaceholders(tmpl.Notes, target), vars)
		expanded.Assignee = substituteVars(tmpl.Assignee, vars)
		// Keep condition expressions intact for the normal condition-filtering
		// pass, which understands the {{var}} syntax. Eager single-brace var
		// substitution here can corrupt "!{{flag}}" into "!{value}".
		expanded.Condition = substituteTargetPlaceholders(tmpl.Condition, target)
		expanded.Expand = substituteVars(substituteTargetPlaceholders(tmpl.Expand, target), vars)
		expanded.WaitsFor = substituteVars(substituteTargetPlaceholders(tmpl.WaitsFor, target), vars)
		expanded.Timeout = substituteVars(substituteTargetPlaceholders(tmpl.Timeout, target), vars)

		// Substitute placeholders in labels
		if len(expanded.Labels) > 0 {
			for i, l := range expanded.Labels {
				expanded.Labels[i] = substituteVars(substituteTargetPlaceholders(l, target), vars)
			}
		}

		// Substitute placeholders in dependencies
		if len(expanded.DependsOn) > 0 {
			for i, d := range expanded.DependsOn {
				expanded.DependsOn[i] = substituteVars(substituteTargetPlaceholders(d, target), vars)
			}
		}

		if len(expanded.Needs) > 0 {
			for i, n := range expanded.Needs {
				expanded.Needs[i] = substituteVars(substituteTargetPlaceholders(n, target), vars)
			}
		}

		if len(expanded.Metadata) > 0 {
			for k, v := range expanded.Metadata {
				expanded.Metadata[k] = substituteVars(substituteTargetPlaceholders(v, target), vars)
			}
		}

		if len(expanded.ExpandVars) > 0 {
			for k, v := range expanded.ExpandVars {
				expanded.ExpandVars[k] = substituteVars(substituteTargetPlaceholders(v, target), vars)
			}
		}

		if expanded.Gate != nil {
			expanded.Gate.Type = substituteVars(substituteTargetPlaceholders(expanded.Gate.Type, target), vars)
			expanded.Gate.ID = substituteVars(substituteTargetPlaceholders(expanded.Gate.ID, target), vars)
			expanded.Gate.Timeout = substituteVars(substituteTargetPlaceholders(expanded.Gate.Timeout, target), vars)
		}

		if expanded.Loop != nil {
			expanded.Loop.Until = substituteVars(substituteTargetPlaceholders(expanded.Loop.Until, target), vars)
			expanded.Loop.Range = substituteVars(substituteTargetPlaceholders(expanded.Loop.Range, target), vars)
			expanded.Loop.Var = substituteVars(substituteTargetPlaceholders(expanded.Loop.Var, target), vars)
			if len(expanded.Loop.Body) > 0 {
				body, err := expandStep(target, expanded.Loop.Body, depth+1, vars)
				if err != nil {
					return nil, err
				}
				expanded.Loop.Body = body
			}
		}

		if expanded.OnComplete != nil {
			expanded.OnComplete.ForEach = substituteVars(substituteTargetPlaceholders(expanded.OnComplete.ForEach, target), vars)
			expanded.OnComplete.Bond = substituteVars(substituteTargetPlaceholders(expanded.OnComplete.Bond, target), vars)
			if len(expanded.OnComplete.Vars) > 0 {
				for k, v := range expanded.OnComplete.Vars {
					expanded.OnComplete.Vars[k] = substituteVars(substituteTargetPlaceholders(v, target), vars)
				}
			}
		}

		if expanded.Ralph != nil && expanded.Ralph.Check != nil {
			expanded.Ralph.Check.Mode = substituteVars(substituteTargetPlaceholders(expanded.Ralph.Check.Mode, target), vars)
			expanded.Ralph.Check.Path = substituteVars(substituteTargetPlaceholders(expanded.Ralph.Check.Path, target), vars)
			expanded.Ralph.Check.Timeout = substituteVars(substituteTargetPlaceholders(expanded.Ralph.Check.Timeout, target), vars)
		}

		// Handle children recursively with depth tracking
		if len(expanded.Children) > 0 {
			children, err := expandStep(target, expanded.Children, depth+1, vars)
			if err != nil {
				return nil, err
			}
			expanded.Children = children
		}

		result = append(result, expanded)
	}

	return result, nil
}

func validateExpandedStepTimeouts(steps []*Step, context string) error {
	var errs []string
	validateNestedStepTimeoutsWithOptions(steps, &errs, context, nil, true)
	if len(errs) > 0 {
		return fmt.Errorf("%s: timeout validation failed:\n  - %s", context, strings.Join(errs, "\n  - "))
	}
	return nil
}

func mergeConditionVars(base map[string]string, overrides map[string]string) map[string]string {
	if base == nil && overrides == nil {
		return nil
	}

	merged := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	return merged
}

func materializeExpandedStepConditions(steps []*Step, vars map[string]string) ([]*Step, error) {
	if vars == nil {
		return steps, nil
	}

	result := make([]*Step, 0, len(steps))
	for _, step := range steps {
		resolvable, err := canResolveStepCondition(step.Condition, vars)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", step.ID, err)
		}

		if resolvable {
			include, err := EvaluateStepCondition(step.Condition, vars)
			if err != nil {
				return nil, fmt.Errorf("step %q: %w", step.ID, err)
			}
			if !include {
				continue
			}
		}

		clone := cloneStep(step)
		if resolvable {
			clone.Condition = ""
		}
		if len(step.Children) > 0 {
			children, err := materializeExpandedStepConditions(step.Children, vars)
			if err != nil {
				return nil, err
			}
			clone.Children = children
		}
		result = append(result, clone)
	}

	return result, nil
}

func canResolveStepCondition(condition string, vars map[string]string) (bool, error) {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return true, nil
	}

	if m := stepCondVarPattern.FindStringSubmatch(condition); m != nil {
		_, ok := vars[m[1]]
		return ok, nil
	}
	if m := stepCondNegatedVarPattern.FindStringSubmatch(condition); m != nil {
		_, ok := vars[m[1]]
		return ok, nil
	}
	if m := stepCondComparePattern.FindStringSubmatch(condition); m != nil {
		_, ok := vars[m[1]]
		return ok, nil
	}

	return false, fmt.Errorf("invalid step condition format: %q (expected {{var}} or {{var}} == value)", condition)
}

// substituteTargetPlaceholders replaces {target} and {target.*} placeholders.
func substituteTargetPlaceholders(s string, target *Step) string {
	if s == "" {
		return s
	}

	// Replace {target} with target step ID
	s = strings.ReplaceAll(s, "{target}", target.ID)

	// Replace {target.id} with target step ID
	s = strings.ReplaceAll(s, "{target.id}", target.ID)

	// Replace {target.title} with target step title
	s = strings.ReplaceAll(s, "{target.title}", target.Title)

	// Replace {target.description} with target step description
	s = strings.ReplaceAll(s, "{target.description}", target.Description)

	return s
}

// mergeVars merges formula default vars with rule overrides.
// Override values take precedence over defaults.
func mergeVars(formula *Formula, overrides map[string]string) map[string]string {
	result := make(map[string]string)

	// Start with formula defaults
	for name, def := range formula.Vars {
		if def.Default != nil {
			result[name] = *def.Default
		}
	}

	// Apply overrides (these win)
	for name, value := range overrides {
		result[name] = value
	}

	return result
}

// buildStepMap creates a map of step ID to step (recursive).
func buildStepMap(steps []*Step) map[string]*Step {
	result := make(map[string]*Step)
	for _, step := range steps {
		result[step.ID] = step
		// Add children recursively
		for id, child := range buildStepMap(step.Children) {
			result[id] = child
		}
	}
	return result
}

// replaceStep replaces a step with the given ID with a slice of new steps.
// Searches recursively through children to find and replace the target.
func replaceStep(steps []*Step, targetID string, replacement []*Step) []*Step {
	result := make([]*Step, 0, len(steps)+len(replacement)-1)

	for _, step := range steps {
		if step.ID == targetID {
			// Replace with expanded steps
			result = append(result, replacement...)
		} else {
			// Keep the step, but check children
			if len(step.Children) > 0 {
				// Clone step and replace in children
				clone := cloneStep(step)
				clone.Children = replaceStep(step.Children, targetID, replacement)
				result = append(result, clone)
			} else {
				result = append(result, step)
			}
		}
	}

	return result
}

// UpdateDependenciesForExpansion updates dependency references after expansion.
// When step X is expanded into X.draft, X.refine-1, etc., any step that
// depended on X should now depend on the last step in the expansion.
func UpdateDependenciesForExpansion(steps []*Step, expandedID string, lastExpandedStepID string) []*Step {
	result := make([]*Step, len(steps))

	for i, step := range steps {
		clone := cloneStep(step)

		// Update DependsOn references
		for j, dep := range clone.DependsOn {
			if dep == expandedID {
				clone.DependsOn[j] = lastExpandedStepID
			}
		}

		// Update Needs references
		for j, need := range clone.Needs {
			if need == expandedID {
				clone.Needs[j] = lastExpandedStepID
			}
		}

		// Handle children recursively
		if len(step.Children) > 0 {
			clone.Children = UpdateDependenciesForExpansion(step.Children, expandedID, lastExpandedStepID)
		}

		result[i] = clone
	}

	return result
}

// propagateTargetDeps copies the target step's Needs and DependsOn to the root
// steps of an expansion. Root steps are those whose existing dependencies only
// reference other steps within the expansion (i.e., they have no external deps
// from the template). This preserves cross-expansion dependency chains that would
// otherwise be lost when the target step is replaced.
func propagateTargetDeps(target *Step, expandedSteps []*Step) {
	if len(target.Needs) == 0 && len(target.DependsOn) == 0 {
		return
	}

	expandedIDs := make(map[string]bool, len(expandedSteps))
	for _, s := range expandedSteps {
		expandedIDs[s.ID] = true
	}

	for _, s := range expandedSteps {
		isRoot := true
		for _, n := range s.Needs {
			if expandedIDs[n] {
				isRoot = false
				break
			}
		}
		if isRoot {
			for _, d := range s.DependsOn {
				if expandedIDs[d] {
					isRoot = false
					break
				}
			}
		}
		if isRoot {
			// Prepend target's deps (new slice to avoid aliasing)
			if len(target.Needs) > 0 {
				s.Needs = append(append([]string{}, target.Needs...), s.Needs...)
			}
			if len(target.DependsOn) > 0 {
				s.DependsOn = append(append([]string{}, target.DependsOn...), s.DependsOn...)
			}
		}
	}
}

// MaterializeExpansion converts a standalone expansion formula into a cookable
// form by expanding its Template into Steps. A synthetic target step is created
// using targetID as the step ID and the formula's own name/description for
// {target.title} and {target.description} placeholders.
//
// This enables expansion formulas to be directly instantiated via wisp/pour
// without requiring a Compose wrapper (bd-qzb).
//
// No-op if the formula is not an expansion type, has no Template, or already
// has Steps.
func MaterializeExpansion(f *Formula, targetID string, vars map[string]string) error {
	if f.Type != TypeExpansion || len(f.Template) == 0 || len(f.Steps) > 0 {
		return nil
	}

	target := &Step{
		ID:          targetID,
		Title:       f.Formula,
		Description: f.Description,
	}

	expandedSteps, err := expandStep(target, f.Template, 0, vars)
	if err != nil {
		return fmt.Errorf("materializing expansion %q: %w", f.Formula, err)
	}
	validationSteps, err := FilterStepsByCondition(expandedSteps, vars)
	if err != nil {
		return fmt.Errorf("materializing expansion %q: filtering conditioned steps: %w", f.Formula, err)
	}
	if err := validateExpandedStepTimeouts(validationSteps, fmt.Sprintf("materializing expansion %q", f.Formula)); err != nil {
		return err
	}
	if dups := findDuplicateStepIDs(validationSteps); len(dups) > 0 {
		return fmt.Errorf("materializing expansion %q: duplicate step IDs after expansion: %v", f.Formula, dups)
	}

	f.Steps = expandedSteps
	return nil
}

// MaterializeExpansionForTarget expands an expansion formula's template using
// the provided synthetic target step. Unlike MaterializeExpansion, callers can
// control the target title/description used by {target.*} placeholders.
func MaterializeExpansionForTarget(f *Formula, target *Step, vars map[string]string) error {
	if f.Type != TypeExpansion || len(f.Template) == 0 || len(f.Steps) > 0 {
		return nil
	}
	if target == nil {
		return fmt.Errorf("materializing expansion %q: target is nil", f.Formula)
	}

	expandedSteps, err := expandStep(target, f.Template, 0, vars)
	if err != nil {
		return fmt.Errorf("materializing expansion %q: %w", f.Formula, err)
	}
	validationSteps, err := FilterStepsByCondition(expandedSteps, vars)
	if err != nil {
		return fmt.Errorf("materializing expansion %q: filtering conditioned steps: %w", f.Formula, err)
	}
	if err := validateExpandedStepTimeouts(validationSteps, fmt.Sprintf("materializing expansion %q", f.Formula)); err != nil {
		return err
	}
	if dups := findDuplicateStepIDs(validationSteps); len(dups) > 0 {
		return fmt.Errorf("materializing expansion %q: duplicate step IDs after expansion: %v", f.Formula, dups)
	}

	f.Steps = expandedSteps
	return nil
}

// ApplyInlineExpansions applies Step.Expand fields to inline expansions.
// Steps with the Expand field set are replaced by the referenced expansion template.
// The step's ExpandVars are passed as variable overrides to the expansion.
//
// This differs from compose.Expand in that the expansion is declared inline on the
// step itself rather than in a central compose section.
//
// Returns a new steps slice with inline expansions applied.
// The original steps slice is not modified.
func ApplyInlineExpansions(steps []*Step, parser *Parser) ([]*Step, error) {
	return ApplyInlineExpansionsWithVars(steps, parser, nil)
}

// ApplyInlineExpansionsWithVars applies Step.Expand fields to inline expansions
// using vars for condition filtering during expansion-time validation.
func ApplyInlineExpansionsWithVars(steps []*Step, parser *Parser, vars map[string]string) ([]*Step, error) {
	if parser == nil {
		return steps, nil
	}

	return applyInlineExpansionsRecursive(steps, parser, vars, 0)
}

// applyInlineExpansionsRecursive handles inline expansions for a slice of steps.
// depth tracks recursion to prevent infinite expansion loops.
func applyInlineExpansionsRecursive(steps []*Step, parser *Parser, vars map[string]string, depth int) ([]*Step, error) {
	if depth > DefaultMaxExpansionDepth {
		return nil, fmt.Errorf("inline expansion depth limit exceeded: max %d levels", DefaultMaxExpansionDepth)
	}

	var result []*Step

	for _, step := range steps {
		// Check if this step has an inline expansion
		if step.Expand != "" {
			expFormula, err := loadResolvedExpansionFormula(parser, step.Expand, fmt.Sprintf("inline expand on step %q", step.ID))
			if err != nil {
				return nil, err
			}

			// Merge formula default vars with step's ExpandVars overrides
			expansionVars := mergeVars(expFormula, step.ExpandVars)

			// Expand the step using the template (reuse existing expandStep)
			expandedSteps, err := expandStep(step, expFormula.Template, 0, expansionVars)
			if err != nil {
				return nil, fmt.Errorf("inline expand on step %q: %w", step.ID, err)
			}
			conditionVars := mergeConditionVars(vars, expansionVars)
			expandedSteps, err = materializeExpandedStepConditions(expandedSteps, conditionVars)
			if err != nil {
				return nil, fmt.Errorf("inline expand on step %q: %w", step.ID, err)
			}
			if err := validateExpandedStepTimeouts(expandedSteps, fmt.Sprintf("inline expand on step %q", step.ID)); err != nil {
				return nil, err
			}

			// Propagate the original step's dependencies to root steps of the expansion
			propagateTargetDeps(step, expandedSteps)

			// Recursively process expanded steps for nested inline expansions
			processedSteps, err := applyInlineExpansionsRecursive(expandedSteps, parser, conditionVars, depth+1)
			if err != nil {
				return nil, err
			}

			result = append(result, processedSteps...)
		} else {
			// No inline expansion - keep the step, but process children recursively
			clone := cloneStep(step)

			if len(step.Children) > 0 {
				processedChildren, err := applyInlineExpansionsRecursive(step.Children, parser, vars, depth)
				if err != nil {
					return nil, err
				}
				clone.Children = processedChildren
			}

			result = append(result, clone)
		}
	}

	validationSteps := result
	if vars != nil {
		filteredSteps, err := FilterStepsByCondition(result, vars)
		if err != nil {
			return nil, fmt.Errorf("filtering conditioned steps after inline expansion: %w", err)
		}
		validationSteps = filteredSteps
	}

	if dups := findDuplicateStepIDs(validationSteps); len(dups) > 0 {
		return nil, fmt.Errorf("duplicate step IDs after inline expansion: %v", dups)
	}

	return result, nil
}
