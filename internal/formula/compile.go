package formula

import (
	"context"
	"fmt"
	"log"
	"maps"
)

// Compile loads a formula by name and runs the full compilation pipeline.
// The returned Recipe contains {{variable}} placeholders — substitution
// happens at instantiation time, not compilation time.
//
// vars is used only for compile-time step condition filtering: steps whose
// condition field evaluates to false given vars are excluded. Pass nil to
// use formula-defined variable defaults for condition evaluation.
//
// The pipeline stages are:
//  1. LoadByName — load formula TOML from search paths
//  2. Resolve — resolve inheritance (extends chains)
//  3. ApplyControlFlow — loops, branches, gates
//  4. ApplyAdvice — inline advice rules
//  5. ApplyInlineExpansions — step-level expand field
//  6. ApplyExpansions — compose.expand/map operators
//  7. Aspect loading + ApplyAdvice for each compose.aspects entry
//  8. FilterStepsByCondition — compile-time step filtering
//  9. MaterializeExpansion — standalone expansion formula handling
//  10. ApplyRalph — expand inline Ralph run/check steps
//  11. toRecipe — flatten step tree to Recipe
func Compile(_ context.Context, name string, searchPaths []string, vars map[string]string) (*Recipe, error) {
	parser := NewParser(searchPaths...)

	// Stage 1: Load formula by name
	f, err := parser.LoadByName(name)
	if err != nil {
		return nil, fmt.Errorf("loading formula %q: %w", name, err)
	}

	// Stage 2: Resolve inheritance
	resolved, err := parser.Resolve(f)
	if err != nil {
		return nil, fmt.Errorf("resolving formula %q: %w", name, err)
	}

	// vars is nil in include-all-steps mode (e.g. order dispatch); skip validation.
	if vars != nil {
		if err := ValidateVars(resolved, vars); err != nil {
			return nil, fmt.Errorf("formula %q: %w", name, err)
		}
	}

	compileVars := make(map[string]string)
	for vname, def := range resolved.Vars {
		if def != nil && def.Default != nil {
			compileVars[vname] = *def.Default
		}
	}
	for k, v := range vars {
		compileVars[k] = v
	}

	// Stage 3: Apply control flow operators — loops, branches, gates
	controlFlowSteps, err := ApplyControlFlow(resolved.Steps, resolved.Compose)
	if err != nil {
		return nil, fmt.Errorf("applying control flow to %q: %w", name, err)
	}
	resolved.Steps = controlFlowSteps

	// Stage 4: Apply advice transformations
	if len(resolved.Advice) > 0 {
		resolved.Steps = ApplyAdvice(resolved.Steps, resolved.Advice)
	}

	// Stage 5: Apply inline step expansions
	inlineExpandedSteps, err := ApplyInlineExpansions(resolved.Steps, parser)
	if err != nil {
		return nil, fmt.Errorf("applying inline expansions to %q: %w", name, err)
	}
	resolved.Steps = inlineExpandedSteps

	// Stage 6: Apply expansion operators (compose.expand/map)
	if resolved.Compose != nil && (len(resolved.Compose.Expand) > 0 || len(resolved.Compose.Map) > 0) {
		expandedSteps, err := ApplyExpansionsWithVars(resolved.Steps, resolved.Compose, parser, compileVars)
		if err != nil {
			return nil, fmt.Errorf("applying expansions to %q: %w", name, err)
		}
		resolved.Steps = expandedSteps
	}

	// Stage 7: Apply aspects from compose.aspects
	if resolved.Compose != nil && len(resolved.Compose.Aspects) > 0 {
		for _, aspectName := range resolved.Compose.Aspects {
			aspectFormula, err := parser.LoadByName(aspectName)
			if err != nil {
				return nil, fmt.Errorf("loading aspect %q: %w", aspectName, err)
			}
			if aspectFormula.Type != TypeAspect {
				return nil, fmt.Errorf("%q is not an aspect formula (type=%s)", aspectName, aspectFormula.Type)
			}
			if len(aspectFormula.Advice) > 0 {
				resolved.Steps = ApplyAdvice(resolved.Steps, aspectFormula.Advice)
			}
		}
	}

	// Stage 8: Apply step condition filtering
	filteredSteps, err := FilterStepsByCondition(resolved.Steps, compileVars)
	if err != nil {
		return nil, fmt.Errorf("filtering steps by condition: %w", err)
	}
	resolved.Steps = filteredSteps

	// Stage 9: Handle standalone expansion formulas
	if resolved.Type == TypeExpansion && len(resolved.Template) > 0 {
		expansionVars := make(map[string]string)
		for vname, def := range resolved.Vars {
			if def != nil && def.Default != nil {
				expansionVars[vname] = *def.Default
			}
		}
		for k, v := range vars {
			expansionVars[k] = v
		}
		if err := MaterializeExpansion(resolved, "main", expansionVars); err != nil {
			return nil, fmt.Errorf("standalone expansion %q: %w", name, err)
		}
	}

	// Stage 10: Expand inline retry-managed steps.
	retrySteps, err := ApplyRetries(resolved.Steps)
	if err != nil {
		return nil, fmt.Errorf("applying retry transforms to %q: %w", name, err)
	}
	resolved.Steps = retrySteps

	// Stage 11: Expand inline Ralph steps
	ralphSteps, err := ApplyRalph(resolved.Steps)
	if err != nil {
		return nil, fmt.Errorf("applying ralph transforms to %q: %w", name, err)
	}
	resolved.Steps = ralphSteps

	// Stage 12: Add graph-first control beads for v2 workflow formulas.
	ApplyGraphControls(resolved)

	// Stage 13: Flatten to Recipe
	return toRecipe(resolved)
}

// toRecipe converts a resolved Formula into a Recipe by flattening the
// step tree into an ordered list with namespaced IDs and dependency edges.
func toRecipe(f *Formula) (*Recipe, error) {
	r := &Recipe{
		Name:        f.Formula,
		Description: f.Description,
		Vars:        f.Vars,
		Phase:       f.Phase,
		Pour:        f.Pour,
	}

	graphWorkflow := isGraphWorkflow(f)

	// Determine root title: use {{title}} placeholder if the variable
	// is defined, otherwise fall back to formula name.
	rootTitle := f.Formula
	if _, hasTitle := f.Vars["title"]; hasTitle {
		rootTitle = "{{title}}"
	}
	rootDesc := f.Description
	if _, hasDesc := f.Vars["desc"]; hasDesc {
		rootDesc = "{{desc}}"
	}

	// Root step
	rootType := "molecule"
	if graphWorkflow {
		rootType = "task"
	}

	rootStep := RecipeStep{
		ID:          f.Formula,
		Title:       rootTitle,
		Description: rootDesc,
		Type:        rootType,
		IsRoot:      true,
	}
	if graphWorkflow {
		rootStep.Metadata = map[string]string{"gc.kind": "workflow"}
		if f.Version >= 2 {
			rootStep.Metadata["gc.formula_contract"] = "graph.v2"
		}
	}
	defPriority := 2
	rootStep.Priority = &defPriority
	r.Steps = append(r.Steps, rootStep)

	// Determine RootOnly: vapor-phase formulas that don't explicitly
	// request pour get root-only by default.
	if !f.Pour && f.Phase == "vapor" {
		r.RootOnly = true
	}

	// Flatten step tree
	idMapping := make(map[string]string) // step.ID -> namespaced ID
	flattenSteps(f.Steps, f.Formula, idMapping, &r.Steps, &r.Deps, graphWorkflow)

	// Collect dependency edges from depends_on/needs/waits_for
	collectRecipeDeps(f.Steps, idMapping, &r.Deps)
	if graphWorkflow {
		addWorkflowRootDeps(f.Formula, f.Steps, idMapping, &r.Deps)
		if f.Version >= 2 {
			orderedSteps, err := orderGraphRecipeSteps(f.Formula, r.Steps, r.Deps)
			if err != nil {
				return nil, err
			}
			r.Steps = orderedSteps
		}
	}

	return r, nil
}

func orderGraphRecipeSteps(rootID string, steps []RecipeStep, deps []RecipeDep) ([]RecipeStep, error) {
	if len(steps) <= 2 {
		return steps, nil
	}

	stepByID := make(map[string]RecipeStep, len(steps))
	order := make(map[string]int, len(steps))
	inDegree := make(map[string]int, len(steps))
	edges := make(map[string][]string, len(steps))

	root := steps[0]
	orderedIDs := make([]string, 0, len(steps)-1)
	for i, step := range steps {
		stepByID[step.ID] = step
		order[step.ID] = i
		if step.ID == rootID {
			root = step
			continue
		}
		orderedIDs = append(orderedIDs, step.ID)
		inDegree[step.ID] = 0
	}

	for _, dep := range deps {
		if dep.Type == "parent-child" || dep.StepID == rootID {
			continue
		}
		if _, ok := inDegree[dep.StepID]; !ok {
			continue
		}
		if _, ok := inDegree[dep.DependsOnID]; !ok {
			continue
		}
		edges[dep.DependsOnID] = append(edges[dep.DependsOnID], dep.StepID)
		inDegree[dep.StepID]++
	}

	ready := make([]string, 0)
	for _, id := range orderedIDs {
		if inDegree[id] == 0 {
			ready = append(ready, id)
		}
	}

	result := make([]RecipeStep, 0, len(steps))
	result = append(result, root)
	for len(ready) > 0 {
		bestIdx := 0
		for i := 1; i < len(ready); i++ {
			if order[ready[i]] < order[ready[bestIdx]] {
				bestIdx = i
			}
		}
		id := ready[bestIdx]
		ready = append(ready[:bestIdx], ready[bestIdx+1:]...)
		result = append(result, stepByID[id])

		for _, next := range edges[id] {
			inDegree[next]--
			if inDegree[next] == 0 {
				ready = append(ready, next)
			}
		}
	}

	if len(result) != len(steps) {
		return nil, fmt.Errorf("graph.v2 formula %q contains a dependency cycle", rootID)
	}
	return result, nil
}

// flattenSteps recursively converts formula Steps into RecipeSteps,
// generating namespaced IDs and parent-child dependency edges where applicable.
func flattenSteps(steps []*Step, parentID string, idMapping map[string]string, out *[]RecipeStep, deps *[]RecipeDep, graphWorkflow bool) {
	for _, step := range steps {
		issueID := parentID + "." + step.ID
		idMapping[step.ID] = issueID

		// Determine type (children promote to epic)
		stepType := step.Type
		if stepType == "" {
			stepType = "task"
		}
		if len(step.Children) > 0 {
			stepType = "epic"
		}

		metadata := step.Metadata
		if isSourceSpecStep(step) {
			metadata = maps.Clone(step.Metadata)
			if specForRef := metadata["gc.spec_for_ref"]; specForRef != "" {
				if mapped, ok := idMapping[specForRef]; ok {
					metadata["gc.spec_for_ref"] = mapped
				}
			}
		}

		rs := RecipeStep{
			ID:          issueID,
			Title:       step.Title,
			Description: step.Description,
			Notes:       step.Notes,
			Type:        stepType,
			Priority:    step.Priority,
			Labels:      step.Labels,
			Assignee:    step.Assignee,
			Metadata:    metadata,
		}

		// Add gate label for waits_for field
		if step.WaitsFor != "" {
			rs.Labels = append(rs.Labels, "gate:"+step.WaitsFor)
		}

		*out = append(*out, rs)

		// Ralph-generated graph nodes intentionally avoid parent-child semantics.
		// They are linked only through explicit blocking deps.
		if !graphWorkflow && !isDetachedGraphStep(step) {
			*deps = append(*deps, RecipeDep{
				StepID:      issueID,
				DependsOnID: parentID,
				Type:        "parent-child",
			})
		}

		// Gate issue synthesis
		if step.Gate != nil {
			gateID := parentID + ".gate-" + step.ID
			gateTitle := fmt.Sprintf("Gate: %s", step.Gate.Type)
			if step.Gate.ID != "" {
				gateTitle = fmt.Sprintf("Gate: %s %s", step.Gate.Type, step.Gate.ID)
			}

			gateStep := RecipeStep{
				ID:          gateID,
				Title:       gateTitle,
				Description: fmt.Sprintf("Async gate for step %s", step.ID),
				Type:        "gate",
				Gate: &RecipeGate{
					Type:    step.Gate.Type,
					ID:      step.Gate.ID,
					Timeout: step.Gate.Timeout,
				},
			}
			defP := 2
			gateStep.Priority = &defP
			*out = append(*out, gateStep)

			idMapping["gate-"+step.ID] = gateID

			// Gate is a child of the parent
			if !graphWorkflow {
				*deps = append(*deps, RecipeDep{
					StepID:      gateID,
					DependsOnID: parentID,
					Type:        "parent-child",
				})
			}
			// Step depends on gate (gate blocks the step)
			*deps = append(*deps, RecipeDep{
				StepID:      issueID,
				DependsOnID: gateID,
				Type:        "blocks",
			})
		}

		// Recurse into children
		if len(step.Children) > 0 {
			flattenSteps(step.Children, issueID, idMapping, out, deps, graphWorkflow)
		}
	}
}

// FormulaV2Enabled controls whether graph.v2 formula compilation is
// allowed. When false, isGraphWorkflow always returns false regardless of
// the formula's Version field, causing v2 formulas to compile as v1.
// Set by the daemon config loader from [daemon] formula_v2.
var FormulaV2Enabled bool

func isGraphWorkflow(f *Formula) bool {
	if f == nil {
		return false
	}
	if !FormulaV2Enabled {
		if f.Version >= 2 {
			log.Printf("formula declares version %d but formula_v2 is disabled; compiling as v1", f.Version)
		}
		return false
	}
	if f.Version >= 2 {
		return true
	}
	return hasDetachedGraphSteps(f.Steps)
}

func isDetachedGraphStep(step *Step) bool {
	if step == nil {
		return false
	}
	switch step.Metadata["gc.kind"] {
	case "ralph", "run", "check", "retry", "retry-run", "retry-eval":
		return true
	default:
		return false
	}
}

func hasDetachedGraphSteps(steps []*Step) bool {
	for _, step := range steps {
		if isDetachedGraphStep(step) {
			return true
		}
		if hasDetachedGraphSteps(step.Children) {
			return true
		}
	}
	return false
}

func addWorkflowRootDeps(rootID string, steps []*Step, idMapping map[string]string, deps *[]RecipeDep) {
	for _, step := range steps {
		if step != nil && step.Metadata["gc.kind"] == "workflow-finalize" {
			if issueID, ok := idMapping[step.ID]; ok {
				*deps = append(*deps, RecipeDep{
					StepID:      rootID,
					DependsOnID: issueID,
					Type:        "blocks",
				})
			}
			return
		}
	}
	for _, step := range steps {
		if !isWorkflowRootBlocker(step) {
			continue
		}
		issueID, ok := idMapping[step.ID]
		if !ok {
			continue
		}
		*deps = append(*deps, RecipeDep{
			StepID:      rootID,
			DependsOnID: issueID,
			Type:        "blocks",
		})
	}
}

func isWorkflowRootBlocker(step *Step) bool {
	if step == nil {
		return false
	}
	switch step.Metadata["gc.kind"] {
	case "run", "check", "retry-run", "retry-eval", "spec":
		return false
	default:
		return true
	}
}

// collectRecipeDeps traverses the step tree and collects dependency edges
// from depends_on, needs, and waits_for fields.
func collectRecipeDeps(steps []*Step, idMapping map[string]string, deps *[]RecipeDep) {
	for _, step := range steps {
		issueID := idMapping[step.ID]

		// depends_on
		for _, depID := range step.DependsOn {
			if depIssueID, ok := idMapping[depID]; ok {
				*deps = append(*deps, RecipeDep{
					StepID:      issueID,
					DependsOnID: depIssueID,
					Type:        "blocks",
				})
			}
		}

		// needs (alias for sibling dependencies)
		for _, needID := range step.Needs {
			if needIssueID, ok := idMapping[needID]; ok {
				*deps = append(*deps, RecipeDep{
					StepID:      issueID,
					DependsOnID: needIssueID,
					Type:        "blocks",
				})
			}
		}

		// waits_for (fanout gate dependency)
		if step.WaitsFor != "" {
			waitsForSpec := ParseWaitsFor(step.WaitsFor)
			if waitsForSpec != nil {
				spawnerStepID := waitsForSpec.SpawnerID
				if spawnerStepID == "" && len(step.Needs) > 0 {
					spawnerStepID = step.Needs[0]
				}
				if spawnerStepID != "" {
					if spawnerIssueID, ok := idMapping[spawnerStepID]; ok {
						metadata := fmt.Sprintf(`{"gate":%q}`, waitsForSpec.Gate)
						*deps = append(*deps, RecipeDep{
							StepID:      issueID,
							DependsOnID: spawnerIssueID,
							Type:        "waits-for",
							Metadata:    metadata,
						})
					}
				}
			}
		}

		// Recurse
		if len(step.Children) > 0 {
			collectRecipeDeps(step.Children, idMapping, deps)
		}
	}
}
