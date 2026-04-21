package formula

import (
	"context"
	"fmt"
)

// FragmentRecipe is a compiled rootless subgraph that can be instantiated into
// an existing workflow root at runtime.
type FragmentRecipe struct {
	Name    string
	Steps   []RecipeStep
	Deps    []RecipeDep
	Vars    map[string]*VarDef
	Entries []string
	Sinks   []string
}

// CompileExpansionFragment compiles an expansion formula into a rootless graph
// fragment using the provided synthetic target step for {target.*}
// substitutions. This is used by runtime fan-out to materialize item-specific
// subgraphs into an existing workflow.
func CompileExpansionFragment(_ context.Context, name string, searchPaths []string, target *Step, vars map[string]string) (*FragmentRecipe, error) {
	parser := NewParser(searchPaths...)

	f, err := parser.LoadByName(name)
	if err != nil {
		return nil, fmt.Errorf("loading expansion %q: %w", name, err)
	}
	resolved, err := parser.Resolve(f)
	if err != nil {
		return nil, fmt.Errorf("resolving expansion %q: %w", name, err)
	}
	if resolved.Type != TypeExpansion {
		return nil, fmt.Errorf("%q is not an expansion formula (type=%s)", name, resolved.Type)
	}

	// Same required-var validation as Compile — see #618.
	if len(vars) > 0 {
		if err := ValidateVars(resolved, vars); err != nil {
			return nil, fmt.Errorf("expansion %q: %w", name, err)
		}
	}

	expansionVars := ApplyDefaults(resolved, vars)
	if err := MaterializeExpansionForTarget(resolved, target, expansionVars); err != nil {
		return nil, err
	}
	filteredSteps, err := FilterStepsByCondition(resolved.Steps, expansionVars)
	if err != nil {
		return nil, fmt.Errorf("filtering conditioned steps in expansion %q: %w", name, err)
	}
	resolved.Steps = filteredSteps

	controlFlowSteps, err := ApplyControlFlow(resolved.Steps, resolved.Compose)
	if err != nil {
		return nil, fmt.Errorf("applying control flow to expansion %q: %w", name, err)
	}
	resolved.Steps = controlFlowSteps

	if len(resolved.Advice) > 0 {
		resolved.Steps = ApplyAdvice(resolved.Steps, resolved.Advice)
	}

	inlineExpandedSteps, err := ApplyInlineExpansionsWithVars(resolved.Steps, parser, expansionVars)
	if err != nil {
		return nil, fmt.Errorf("applying inline expansions to expansion %q: %w", name, err)
	}
	resolved.Steps = inlineExpandedSteps

	if resolved.Compose != nil && (len(resolved.Compose.Expand) > 0 || len(resolved.Compose.Map) > 0) {
		expandedSteps, expandErr := ApplyExpansionsWithVars(resolved.Steps, resolved.Compose, parser, expansionVars)
		if expandErr != nil {
			return nil, fmt.Errorf("applying expansions to expansion %q: %w", name, expandErr)
		}
		resolved.Steps = expandedSteps
	}

	if resolved.Compose != nil && len(resolved.Compose.Aspects) > 0 {
		for _, aspectName := range resolved.Compose.Aspects {
			aspectFormula, loadErr := parser.LoadByName(aspectName)
			if loadErr != nil {
				return nil, fmt.Errorf("loading aspect %q: %w", aspectName, loadErr)
			}
			if aspectFormula.Type != TypeAspect {
				return nil, fmt.Errorf("%q is not an aspect formula (type=%s)", aspectName, aspectFormula.Type)
			}
			if len(aspectFormula.Advice) == 0 {
				continue
			}
			resolved.Steps = ApplyAdvice(resolved.Steps, aspectFormula.Advice)
		}
	}

	filteredSteps, err = FilterStepsByCondition(resolved.Steps, expansionVars)
	if err != nil {
		return nil, fmt.Errorf("filtering conditioned steps in expansion %q: %w", name, err)
	}
	resolved.Steps = filteredSteps

	retrySteps, err := ApplyRetries(resolved.Steps)
	if err != nil {
		return nil, fmt.Errorf("applying retry transforms to expansion %q: %w", name, err)
	}
	resolved.Steps = retrySteps

	ralphSteps, err := ApplyRalph(resolved.Steps)
	if err != nil {
		return nil, fmt.Errorf("applying ralph transforms to expansion %q: %w", name, err)
	}
	resolved.Steps = ralphSteps

	graphWorkflow, err := isGraphWorkflow(resolved, IsFormulaV2Enabled())
	if err != nil {
		return nil, err
	}
	if graphWorkflow {
		ApplyFragmentGraphControls(resolved)
	}

	recipe, err := toRecipeWithGraph(resolved, graphWorkflow)
	if err != nil {
		return nil, fmt.Errorf("flattening expansion %q: %w", name, err)
	}
	fragment := stripFragmentRecipe(recipe)
	fragment.Entries = fragmentEntryStepIDs(fragment)
	fragment.Sinks = fragmentSinkStepIDs(fragment)
	return fragment, nil
}

func stripFragmentRecipe(recipe *Recipe) *FragmentRecipe {
	if recipe == nil {
		return &FragmentRecipe{}
	}

	stepSet := make(map[string]struct{}, len(recipe.Steps))
	steps := make([]RecipeStep, 0, len(recipe.Steps))
	for _, step := range recipe.Steps {
		if step.IsRoot {
			continue
		}
		if step.Metadata["gc.kind"] == "workflow-finalize" {
			continue
		}
		steps = append(steps, step)
		stepSet[step.ID] = struct{}{}
	}

	deps := make([]RecipeDep, 0, len(recipe.Deps))
	for _, dep := range recipe.Deps {
		if _, ok := stepSet[dep.StepID]; !ok {
			continue
		}
		if _, ok := stepSet[dep.DependsOnID]; !ok {
			continue
		}
		deps = append(deps, dep)
	}

	return &FragmentRecipe{
		Name:  recipe.Name,
		Steps: steps,
		Deps:  deps,
		Vars:  recipe.Vars,
	}
}

// ApplyFragmentRecipeGraphControls synthesizes scope-check control nodes for a
// compiled fragment after runtime metadata propagation.
func ApplyFragmentRecipeGraphControls(fragment *FragmentRecipe) {
	if fragment == nil || len(fragment.Steps) == 0 {
		return
	}

	existingStepIDs := make(map[string]struct{}, len(fragment.Steps))
	replacements := make(map[string]string)
	controls := make([]RecipeStep, 0)
	controlDeps := make([]RecipeDep, 0)
	for _, step := range fragment.Steps {
		existingStepIDs[step.ID] = struct{}{}
	}

	for _, step := range fragment.Steps {
		if !recipeStepNeedsScopeCheck(step) {
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
		controls = append(controls, RecipeStep{
			ID:       controlID,
			Title:    "Finalize scope for " + step.Title,
			Type:     "task",
			Metadata: meta,
		})
		controlDeps = append(controlDeps, RecipeDep{
			StepID:      controlID,
			DependsOnID: step.ID,
			Type:        "blocks",
		})
	}

	if len(controls) == 0 {
		return
	}

	for i := range fragment.Deps {
		if replacement, ok := replacements[fragment.Deps[i].DependsOnID]; ok {
			fragment.Deps[i].DependsOnID = replacement
		}
	}
	fragment.Steps = append(fragment.Steps, controls...)
	fragment.Deps = append(fragment.Deps, controlDeps...)
	fragment.Entries = fragmentEntryStepIDs(fragment)
	fragment.Sinks = fragmentSinkStepIDs(fragment)
}

func recipeStepNeedsScopeCheck(step RecipeStep) bool {
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

func fragmentEntryStepIDs(fragment *FragmentRecipe) []string {
	if fragment == nil || len(fragment.Steps) == 0 {
		return nil
	}

	inDegree := make(map[string]int, len(fragment.Steps))
	for _, step := range fragment.Steps {
		inDegree[step.ID] = 0
	}
	for _, dep := range fragment.Deps {
		if dep.Type == "parent-child" {
			continue
		}
		if _, ok := inDegree[dep.StepID]; ok {
			inDegree[dep.StepID]++
		}
	}

	entries := make([]string, 0)
	for _, step := range fragment.Steps {
		if inDegree[step.ID] == 0 {
			entries = append(entries, step.ID)
		}
	}
	return entries
}

func fragmentSinkStepIDs(fragment *FragmentRecipe) []string {
	if fragment == nil || len(fragment.Steps) == 0 {
		return nil
	}

	referenced := make(map[string]struct{}, len(fragment.Steps))
	for _, dep := range fragment.Deps {
		if dep.Type == "parent-child" {
			continue
		}
		referenced[dep.DependsOnID] = struct{}{}
	}

	sinks := make([]string, 0)
	for _, step := range fragment.Steps {
		if _, ok := referenced[step.ID]; ok {
			continue
		}
		switch step.Metadata["gc.kind"] {
		case "workflow-finalize", "spec":
			continue
		}
		sinks = append(sinks, step.ID)
	}
	return sinks
}
