package formula

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
	parser := NewParser(searchPaths...).SetSource(SourceFromEnv())
	v2Enabled := IsFormulaV2Enabled()
	var composedRequirements []formulaCompilerConstraint
	collectComposedRequirements := func(f *Formula) error {
		constraints, err := formulaCompilerConstraints(f)
		if err != nil {
			return err
		}
		composedRequirements = append(composedRequirements, constraints...)
		return nil
	}

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
	if err := validateCompileTimeVars(resolved, vars); err != nil {
		return nil, fmt.Errorf("expansion %q: %w", name, err)
	}
	if err := MaterializeExpansionForTarget(resolved, target, expansionVars); err != nil {
		return nil, err
	}
	filteredSteps, err := FilterStepsByCondition(resolved.Steps, expansionVars)
	if err != nil {
		return nil, fmt.Errorf("filtering conditioned steps in expansion %q: %w", name, err)
	}
	resolved.Steps = filteredSteps

	controlFlowSteps, err := ApplyControlFlowWithVars(resolved.Steps, resolved.Compose, expansionVars)
	if err != nil {
		return nil, fmt.Errorf("applying control flow to expansion %q: %w", name, err)
	}
	resolved.Steps = controlFlowSteps

	if len(resolved.Advice) > 0 {
		resolved.Steps = ApplyAdvice(resolved.Steps, resolved.Advice)
	}

	inlineExpandedSteps, err := applyInlineExpansionsWithVars(resolved.Steps, parser, expansionVars, collectComposedRequirements)
	if err != nil {
		return nil, fmt.Errorf("applying inline expansions to expansion %q: %w", name, err)
	}
	resolved.Steps = inlineExpandedSteps

	if resolved.Compose != nil && (len(resolved.Compose.Expand) > 0 || len(resolved.Compose.Map) > 0) {
		expandedSteps, expandErr := applyExpansionsWithVars(resolved.Steps, resolved.Compose, parser, expansionVars, collectComposedRequirements)
		if expandErr != nil {
			return nil, fmt.Errorf("applying expansions to expansion %q: %w", name, expandErr)
		}
		resolved.Steps = expandedSteps
	}

	if resolved.Compose != nil && len(resolved.Compose.Aspects) > 0 {
		for _, aspectName := range resolved.Compose.Aspects {
			aspectFormula, loadErr := loadResolvedAspectFormula(parser, aspectName, collectComposedRequirements)
			if loadErr != nil {
				return nil, loadErr
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

	if err := addFormulaCompilerConstraints(resolved, composedRequirements); err != nil {
		return nil, err
	}
	if err := validateExplicitGraphCompilerRequirement(resolved); err != nil {
		return nil, err
	}

	retrySteps, err := ApplyRetries(resolved.Steps)
	if err != nil {
		return nil, fmt.Errorf("applying retry transforms to expansion %q: %w", name, err)
	}
	resolved.Steps = retrySteps

	// Resolve "../assets/..." check paths whose {target}/{{var}} placeholders
	// MaterializeExpansionForTarget has now substituted, before ApplyRalph
	// freezes them into gc.check_path. Without this a templated asset check
	// materializes a relative path the runtime cannot find in the layer tree.
	if err := parser.resolveExpandedCheckPaths(resolved); err != nil {
		return nil, fmt.Errorf("resolving expanded check paths in expansion %q: %w", name, err)
	}

	ralphSteps, err := ApplyRalph(resolved.Steps)
	if err != nil {
		return nil, fmt.Errorf("applying ralph transforms to expansion %q: %w", name, err)
	}
	resolved.Steps = ralphSteps

	if err := ValidateHostRequirements(resolved, v2Enabled); err != nil {
		return nil, err
	}

	graphWorkflow, err := isGraphWorkflow(resolved, v2Enabled)
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
		if step.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize {
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
			beadmeta.KindMetadataKey:       beadmeta.KindScopeCheck,
			beadmeta.ScopeRefMetadataKey:   step.Metadata[beadmeta.ScopeRefMetadataKey],
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleControl,
			beadmeta.ControlForMetadataKey: step.ID,
		}
		for _, key := range []string{beadmeta.StepIDMetadataKey, beadmeta.RalphStepIDMetadataKey, beadmeta.AttemptMetadataKey, beadmeta.OnFailMetadataKey} {
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
	if step.Metadata[beadmeta.ScopeRefMetadataKey] == "" {
		return false
	}
	if step.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
		return false
	}
	return !beadmeta.IsScopeCheckExemptKind(step.Metadata[beadmeta.KindMetadataKey])
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
		switch step.Metadata[beadmeta.KindMetadataKey] {
		case "workflow-finalize", "spec":
			continue
		}
		sinks = append(sinks, step.ID)
	}
	return sinks
}
