package molecule

import (
	"encoding/json"
	"maps"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/formula"
)

// recipeWithNativeStepDependencies derives the private, canonical native-step
// topology fact from the compiled recipe graph. It intentionally has no access
// to physical bead IDs or Needs: those are materialization details, not native
// execution topology.
//
// A missing or invalid fact stays absent (UNKNOWN). A valid step with no native
// prerequisites gets an explicit empty array (known root). The returned recipe
// is a copy, so repeated materialization never mutates its caller's recipe.
func recipeWithNativeStepDependencies(recipe *formula.Recipe) *formula.Recipe {
	if recipe == nil {
		return nil
	}

	clone := *recipe
	clone.Steps = recipeStepsWithNativeStepDependencies(recipe.Steps, recipe.Deps)
	return &clone
}

func fragmentRecipeWithNativeStepDependencies(recipe *formula.FragmentRecipe) *formula.FragmentRecipe {
	if recipe == nil {
		return nil
	}
	clone := *recipe
	clone.Steps = recipeStepsWithNativeStepDependencies(recipe.Steps, recipe.Deps)
	return &clone
}

func recipeStepsWithNativeStepDependencies(steps []formula.RecipeStep, recipeDeps []formula.RecipeDep) []formula.RecipeStep {
	clone := make([]formula.RecipeStep, len(steps))
	copy(clone, steps)
	for i := range clone {
		clone[i].Metadata = maps.Clone(steps[i].Metadata)
		delete(clone[i].Metadata, beadmeta.NativeStepDependenciesMetadataKey)
		if _, intentional := clone[i].Metadata[beadmeta.StepIDMetadataKey]; !intentional && validNativeStepID(clone[i].ID) {
			if clone[i].Metadata == nil {
				clone[i].Metadata = make(map[string]string, 1)
			}
			clone[i].Metadata[beadmeta.StepIDMetadataKey] = clone[i].ID
		}
	}

	stepCount := make(map[string]int, len(clone))
	for _, step := range clone {
		stepCount[step.ID]++
	}

	nativeByStepID := make(map[string]string, len(clone))
	invalidNativeIDs := make(map[string]bool)
	for _, step := range clone {
		nativeID := step.Metadata[beadmeta.StepIDMetadataKey]
		if !validNativeStepID(nativeID) {
			continue
		}
		if stepCount[step.ID] != 1 {
			invalidNativeIDs[nativeID] = true
			continue
		}
		nativeByStepID[step.ID] = nativeID
	}

	dependenciesByNativeID := make(map[string]map[string]struct{}, len(nativeByStepID))
	for _, nativeID := range nativeByStepID {
		if dependenciesByNativeID[nativeID] == nil {
			dependenciesByNativeID[nativeID] = make(map[string]struct{})
		}
	}
	for _, dep := range recipeDeps {
		if dep.Type == "parent-child" {
			continue
		}
		nativeID, ok := nativeByStepID[dep.StepID]
		if !ok {
			continue
		}
		dependencyNativeID, ok := nativeByStepID[dep.DependsOnID]
		if !ok || dep.StepID == dep.DependsOnID {
			invalidNativeIDs[nativeID] = true
			continue
		}
		if dependencyNativeID != nativeID {
			dependenciesByNativeID[nativeID][dependencyNativeID] = struct{}{}
		}
	}

	for i, step := range clone {
		nativeID, ok := nativeByStepID[step.ID]
		if !ok || invalidNativeIDs[nativeID] {
			continue
		}
		dependencies := make([]string, 0, len(dependenciesByNativeID[nativeID]))
		for dependency := range dependenciesByNativeID[nativeID] {
			dependencies = append(dependencies, dependency)
		}
		sort.Strings(dependencies)
		encoded, err := json.Marshal(dependencies)
		if err != nil {
			continue
		}
		if clone[i].Metadata == nil {
			clone[i].Metadata = make(map[string]string, 1)
		}
		clone[i].Metadata[beadmeta.NativeStepDependenciesMetadataKey] = string(encoded)
	}

	return clone
}

// validNativeStepID preserves the existing execution_step_id storage domain:
// an exact, nonblank UTF-8 value up to 256 bytes. It deliberately does not
// invent a new public identifier regex.
func validNativeStepID(id string) bool {
	return len(id) <= 256 && utf8.ValidString(id) && strings.TrimSpace(id) != ""
}
