package molecule

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
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

func decodeNativeStepDependencies(raw, stepID string) ([]string, bool) {
	if raw == "" || !validNativeStepID(stepID) {
		return nil, false
	}
	var dependencies []string
	if err := json.Unmarshal([]byte(raw), &dependencies); err != nil || dependencies == nil {
		return nil, false
	}
	previous := ""
	for _, dependency := range dependencies {
		if !validNativeStepID(dependency) || dependency == stepID || (previous != "" && dependency <= previous) {
			return nil, false
		}
		previous = dependency
	}
	encoded, err := json.Marshal(dependencies)
	if err != nil || string(encoded) != raw {
		return nil, false
	}
	return dependencies, true
}

func normalizeNativeStepDependencies(stepID string, dependencies []string) ([]string, bool) {
	unique := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		if !validNativeStepID(dependency) {
			return nil, false
		}
		if dependency != stepID {
			unique[dependency] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(unique))
	for dependency := range unique {
		normalized = append(normalized, dependency)
	}
	sort.Strings(normalized)
	return normalized, true
}

// preserveAttachedNativeStepTopology carries an immutable topology fact from a
// control bead to a new physical occurrence of the same semantic step.
func preserveAttachedNativeStepTopology(parent beads.Bead, recipe *formula.Recipe) {
	if recipe == nil || len(recipe.Steps) == 0 {
		return
	}
	root := &recipe.Steps[0]
	for i := range recipe.Steps {
		if recipe.Steps[i].IsRoot {
			root = &recipe.Steps[i]
			break
		}
	}
	parentStepID := parent.Metadata[beadmeta.StepIDMetadataKey]
	rootStepID := root.Metadata[beadmeta.StepIDMetadataKey]
	if !validNativeStepID(parentStepID) || rootStepID != parentStepID {
		return
	}
	raw := parent.Metadata[beadmeta.NativeStepDependenciesMetadataKey]
	if _, complete := decodeNativeStepDependencies(raw, parentStepID); !complete {
		delete(root.Metadata, beadmeta.NativeStepDependenciesMetadataKey)
		return
	}
	root.Metadata[beadmeta.NativeStepDependenciesMetadataKey] = raw
}

// applyExternalNativeStepDependencies adds native edges for physical
// ExternalDeps. If any prerequisite lacks a native identity, the target fact is
// omitted rather than publishing an incomplete dependency set as authoritative.
func applyExternalNativeStepDependencies(store beads.Store, steps []formula.RecipeStep, externalDeps []ExternalDep) error {
	type accumulator struct {
		complete     bool
		dependencies []string
	}
	stepIndexes := make(map[string]int, len(steps))
	for i := range steps {
		stepIndexes[steps[i].ID] = i
	}
	byStep := make(map[string]*accumulator)
	for _, dependency := range externalDeps {
		if dependency.StepID == "" || dependency.DependsOnID == "" || dependency.Type == "parent-child" {
			continue
		}
		if _, exists := stepIndexes[dependency.StepID]; !exists {
			continue
		}
		current := byStep[dependency.StepID]
		if current == nil {
			current = &accumulator{complete: true}
			byStep[dependency.StepID] = current
		}
		predecessor, err := store.Get(dependency.DependsOnID)
		if err != nil {
			return fmt.Errorf("resolving external dependency %q for step %q native topology: %w", dependency.DependsOnID, dependency.StepID, err)
		}
		predecessorStepID := predecessor.Metadata[beadmeta.StepIDMetadataKey]
		if !validNativeStepID(predecessorStepID) {
			current.complete = false
			continue
		}
		current.dependencies = append(current.dependencies, predecessorStepID)
	}
	for stepID, current := range byStep {
		step := &steps[stepIndexes[stepID]]
		if !current.complete {
			delete(step.Metadata, beadmeta.NativeStepDependenciesMetadataKey)
			continue
		}
		nativeStepID := step.Metadata[beadmeta.StepIDMetadataKey]
		local, complete := decodeNativeStepDependencies(step.Metadata[beadmeta.NativeStepDependenciesMetadataKey], nativeStepID)
		if !complete {
			delete(step.Metadata, beadmeta.NativeStepDependenciesMetadataKey)
			continue
		}
		dependencies, complete := normalizeNativeStepDependencies(nativeStepID, append(local, current.dependencies...))
		if !complete {
			delete(step.Metadata, beadmeta.NativeStepDependenciesMetadataKey)
			continue
		}
		encoded, err := json.Marshal(dependencies)
		if err != nil {
			delete(step.Metadata, beadmeta.NativeStepDependenciesMetadataKey)
			continue
		}
		step.Metadata[beadmeta.NativeStepDependenciesMetadataKey] = string(encoded)
	}
	return nil
}
