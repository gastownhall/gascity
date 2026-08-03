package molecule

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestRecipeNativeStepDependenciesStampCanonicalRecipeTopology(t *testing.T) {
	recipe := &formula.Recipe{
		Steps: []formula.RecipeStep{
			{ID: "workflow", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "native-root"}},
			{ID: "prepare", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "native-prepare"}},
			{ID: "build", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "native-build"}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "prepare", DependsOnID: "workflow", Type: "parent-child"},
			{StepID: "build", DependsOnID: "prepare", Type: "blocks"},
		},
	}

	stamped := recipeWithNativeStepDependencies(recipe)
	if got, want := stamped.Steps[0].Metadata["gc.native_step_dependencies.v1"], "[]"; got != want {
		t.Fatalf("root topology = %q, want %q", got, want)
	}
	if got, want := stamped.Steps[1].Metadata["gc.native_step_dependencies.v1"], "[]"; got != want {
		t.Fatalf("parent-only topology = %q, want %q", got, want)
	}
	if got, want := stamped.Steps[2].Metadata["gc.native_step_dependencies.v1"], `["native-prepare"]`; got != want {
		t.Fatalf("build topology = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(recipe.Steps[2].Metadata, map[string]string{beadmeta.StepIDMetadataKey: "native-build"}) {
		t.Fatalf("input recipe mutated: %#v", recipe.Steps[2].Metadata)
	}
}

func TestRecipeNativeStepDependenciesOmitUnsafeTopology(t *testing.T) {
	recipe := &formula.Recipe{
		Steps: []formula.RecipeStep{
			{ID: "source", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "native-source"}},
			{ID: "target", Metadata: map[string]string{beadmeta.StepIDMetadataKey: " "}},
			{ID: "self", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "native-self"}},
			{ID: "empty", Metadata: map[string]string{beadmeta.StepIDMetadataKey: ""}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "source", DependsOnID: "target", Type: "blocks"},
			{StepID: "self", DependsOnID: "self", Type: "blocks"},
		},
	}

	stamped := recipeWithNativeStepDependencies(recipe)
	for _, index := range []int{0, 1, 2, 3} {
		if got := stamped.Steps[index].Metadata["gc.native_step_dependencies.v1"]; got != "" {
			t.Fatalf("step %q topology = %q, want omitted", stamped.Steps[index].ID, got)
		}
	}
}

func TestCompiledGraphRecipeStampsNativeStepTopology(t *testing.T) {
	formulaDir := t.TempDir()
	const formulaName = "native-step-topology"
	formulaBytes := []byte(`formula = "native-step-topology"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "first"
title = "First"

[[steps]]
id = "second"
title = "Second"
needs = ["first"]
`)
	if err := os.WriteFile(filepath.Join(formulaDir, formulaName+".toml"), formulaBytes, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	recipe, err := formula.Compile(context.Background(), formulaName, []string{formulaDir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	plan, _, _, err := buildRecipeApplyPlan(recipe, Options{})
	if err != nil {
		t.Fatalf("buildRecipeApplyPlan: %v", err)
	}
	nodes := make(map[string]beads.GraphApplyNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.Key] = node
	}
	nativeByRecipeID := make(map[string]string, len(recipe.Steps))
	for _, step := range recipe.Steps {
		want := step.Metadata[beadmeta.StepIDMetadataKey]
		if want == "" {
			want = step.ID
		}
		nativeByRecipeID[step.ID] = want
		if got := nodes[step.ID].Metadata[beadmeta.StepIDMetadataKey]; got != want {
			t.Fatalf("node %q gc.step_id = %q, want %q", step.ID, got, want)
		}
	}
	for _, step := range recipe.Steps {
		dependencies := make([]string, 0)
		for _, dep := range recipe.Deps {
			if dep.StepID == step.ID && dep.Type != "parent-child" {
				dependencies = append(dependencies, nativeByRecipeID[dep.DependsOnID])
			}
		}
		sort.Strings(dependencies)
		want, err := json.Marshal(dependencies)
		if err != nil {
			t.Fatalf("marshal expected topology: %v", err)
		}
		if got := nodes[step.ID].Metadata[beadmeta.NativeStepDependenciesMetadataKey]; got != string(want) {
			t.Fatalf("node %q topology = %q, want %q", step.ID, got, want)
		}
	}
}

func TestCompiledReviewQuorumCollapsesRetryMachineryIntoNativeSteps(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	searchDir := filepath.Join(cwd, "..", "bootstrap", "packs", "core", "formulas")
	recipe, err := formula.Compile(context.Background(), "mol-review-quorum", []string{searchDir}, map[string]string{
		"subject":           "PR-123",
		"lane_one_id":       "primary",
		"lane_one_provider": "provider-a",
		"lane_one_model":    "model-a",
		"lane_one_target":   "target-a",
		"lane_two_id":       "secondary",
		"lane_two_provider": "provider-b",
		"lane_two_model":    "model-b",
		"lane_two_target":   "target-b",
		"synthesis_target":  "review-synthesis",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	plan, _, _, err := buildRecipeApplyPlan(recipe, Options{})
	if err != nil {
		t.Fatalf("buildRecipeApplyPlan: %v", err)
	}
	nodes := make(map[string]beads.GraphApplyNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.Key] = node
	}

	for _, key := range []string{
		"mol-review-quorum.review-lane-one",
		"mol-review-quorum.review-lane-one.attempt.1",
		"mol-review-quorum.review-lane-two",
		"mol-review-quorum.review-lane-two.attempt.1",
	} {
		if got, want := nodes[key].Metadata[beadmeta.NativeStepDependenciesMetadataKey], "[]"; got != want {
			t.Fatalf("node %q topology = %q, want %q", key, got, want)
		}
	}
	if got, want := nodes["mol-review-quorum.synthesize-review-quorum"].Metadata[beadmeta.NativeStepDependenciesMetadataKey], `["review-lane-one","review-lane-two"]`; got != want {
		t.Fatalf("synthesis topology = %q, want %q", got, want)
	}
}

func TestNativeStepDependenciesMaterializeThroughGraphAndSequentialPaths(t *testing.T) {
	recipe := &formula.Recipe{
		Name: "native-topology",
		Steps: []formula.RecipeStep{
			{ID: "native-topology", IsRoot: true, Metadata: map[string]string{beadmeta.StepIDMetadataKey: "root"}},
			{ID: "native-topology.first", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "first"}},
			{ID: "native-topology.second", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "second"}},
		},
		Deps: []formula.RecipeDep{{StepID: "native-topology.second", DependsOnID: "native-topology.first", Type: "blocks"}},
	}

	plan, _, _, err := buildRecipeApplyPlan(recipe, Options{})
	if err != nil {
		t.Fatalf("buildRecipeApplyPlan: %v", err)
	}
	if got, want := plan.Nodes[2].Metadata[beadmeta.NativeStepDependenciesMetadataKey], `["first"]`; got != want {
		t.Fatalf("graph node topology = %q, want %q", got, want)
	}

	store := beads.NewMemStore()
	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	second, err := store.Get(result.IDMapping["native-topology.second"])
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if got, want := second.Metadata[beadmeta.NativeStepDependenciesMetadataKey], `["first"]`; got != want {
		t.Fatalf("sequential bead topology = %q, want %q", got, want)
	}
}

func TestAttachPreservesNativeStepDependenciesAcrossRetryAttempts(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title: "Build retry control",
		Metadata: map[string]string{
			beadmeta.StepIDMetadataKey:                 "build",
			beadmeta.StepRefMetadataKey:                "workflow.build",
			beadmeta.NativeStepDependenciesMetadataKey: `["prepare"]`,
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	recipe := &formula.Recipe{
		Name: "workflow.build.attempt.2",
		Steps: []formula.RecipeStep{{
			ID:     "workflow.build.attempt.2",
			Title:  "Build",
			IsRoot: true,
			Metadata: map[string]string{
				beadmeta.StepIDMetadataKey:  "build",
				beadmeta.StepRefMetadataKey: "workflow.build.attempt.2",
			},
		}},
	}

	result, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	attempt, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if got, want := attempt.Metadata[beadmeta.NativeStepDependenciesMetadataKey], `["prepare"]`; got != want {
		t.Fatalf("retry attempt topology = %q, want immutable %q", got, want)
	}
}

func TestAttachKeepsRetryTopologyUnknownWhenParentTopologyIsUnknown(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title: "Build retry control",
		Metadata: map[string]string{
			beadmeta.StepIDMetadataKey:  "build",
			beadmeta.StepRefMetadataKey: "workflow.build",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	recipe := &formula.Recipe{
		Name: "workflow.build.attempt.2",
		Steps: []formula.RecipeStep{{
			ID:     "workflow.build.attempt.2",
			Title:  "Build",
			IsRoot: true,
			Metadata: map[string]string{
				beadmeta.StepIDMetadataKey:  "build",
				beadmeta.StepRefMetadataKey: "workflow.build.attempt.2",
			},
		}},
	}

	result, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	attempt, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if got, present := attempt.Metadata[beadmeta.NativeStepDependenciesMetadataKey]; present {
		t.Fatalf("retry attempt topology = %q, want omitted UNKNOWN", got)
	}
}

func TestInstantiateFragmentIncludesCompleteExternalNativeStepDependencies(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "Workflow"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	predecessor, err := store.Create(beads.Bead{
		Title: "Prepare",
		Metadata: map[string]string{
			beadmeta.StepIDMetadataKey:     "prepare",
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create predecessor: %v", err)
	}
	fragment := &formula.FragmentRecipe{
		Name:    "late-build",
		Steps:   []formula.RecipeStep{{ID: "build", Title: "Build"}},
		Entries: []string{"build"},
		Sinks:   []string{"build"},
	}
	opts := FragmentOptions{
		RootID: root.ID,
		ExternalDeps: []ExternalDep{{
			StepID:      "build",
			DependsOnID: predecessor.ID,
			Type:        "blocks",
		}},
	}
	plan, err := buildFragmentApplyPlan(store, fragment, opts)
	if err != nil {
		t.Fatalf("buildFragmentApplyPlan: %v", err)
	}
	if got, want := plan.Nodes[0].Metadata[beadmeta.NativeStepDependenciesMetadataKey], `["prepare"]`; got != want {
		t.Fatalf("graph fragment topology = %q, want %q", got, want)
	}

	result, err := InstantiateFragment(context.Background(), store, fragment, opts)
	if err != nil {
		t.Fatalf("InstantiateFragment: %v", err)
	}
	build, err := store.Get(result.IDMapping["build"])
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if got, want := build.Metadata[beadmeta.NativeStepDependenciesMetadataKey], `["prepare"]`; got != want {
		t.Fatalf("fragment topology = %q, want %q", got, want)
	}
}

func TestInstantiateFragmentOmitsExternalTopologyOutsideExactRoot(t *testing.T) {
	for _, tc := range []struct {
		name            string
		predecessorRoot string
	}{
		{name: "missing root"},
		{name: "foreign root", predecessorRoot: "gcg-foreign"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			root, err := store.Create(beads.Bead{Title: "Workflow"})
			if err != nil {
				t.Fatalf("create root: %v", err)
			}
			predecessor, err := store.Create(beads.Bead{
				Title: "Prepare",
				Metadata: map[string]string{
					beadmeta.StepIDMetadataKey:     "prepare",
					beadmeta.RootBeadIDMetadataKey: tc.predecessorRoot,
				},
			})
			if err != nil {
				t.Fatalf("create predecessor: %v", err)
			}
			fragment := &formula.FragmentRecipe{
				Name:    "late-build",
				Steps:   []formula.RecipeStep{{ID: "build", Title: "Build"}},
				Entries: []string{"build"},
				Sinks:   []string{"build"},
			}
			opts := FragmentOptions{
				RootID: root.ID,
				ExternalDeps: []ExternalDep{{
					StepID:      "build",
					DependsOnID: predecessor.ID,
					Type:        "blocks",
				}},
			}

			plan, err := buildFragmentApplyPlan(store, fragment, opts)
			if err != nil {
				t.Fatalf("buildFragmentApplyPlan: %v", err)
			}
			if got, present := plan.Nodes[0].Metadata[beadmeta.NativeStepDependenciesMetadataKey]; present {
				t.Fatalf("graph fragment topology = %q, want omitted UNKNOWN", got)
			}

			result, err := InstantiateFragment(context.Background(), store, fragment, opts)
			if err != nil {
				t.Fatalf("InstantiateFragment: %v", err)
			}
			build, err := store.Get(result.IDMapping["build"])
			if err != nil {
				t.Fatalf("get build: %v", err)
			}
			if got, present := build.Metadata[beadmeta.NativeStepDependenciesMetadataKey]; present {
				t.Fatalf("fragment topology = %q, want omitted UNKNOWN", got)
			}
		})
	}
}

func TestInstantiateFragmentOmitsTopologyWhenExternalNativeStepIsUnknown(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "Workflow"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	unknownPredecessor, err := store.Create(beads.Bead{Title: "Unidentified prerequisite"})
	if err != nil {
		t.Fatalf("create predecessor: %v", err)
	}
	fragment := &formula.FragmentRecipe{
		Name:    "late-build",
		Steps:   []formula.RecipeStep{{ID: "build", Title: "Build"}},
		Entries: []string{"build"},
		Sinks:   []string{"build"},
	}
	opts := FragmentOptions{
		RootID: root.ID,
		ExternalDeps: []ExternalDep{{
			StepID:      "build",
			DependsOnID: unknownPredecessor.ID,
			Type:        "blocks",
		}},
	}
	plan, err := buildFragmentApplyPlan(store, fragment, opts)
	if err != nil {
		t.Fatalf("buildFragmentApplyPlan: %v", err)
	}
	if got, present := plan.Nodes[0].Metadata[beadmeta.NativeStepDependenciesMetadataKey]; present {
		t.Fatalf("graph fragment topology = %q, want omitted UNKNOWN", got)
	}

	result, err := InstantiateFragment(context.Background(), store, fragment, opts)
	if err != nil {
		t.Fatalf("InstantiateFragment: %v", err)
	}
	build, err := store.Get(result.IDMapping["build"])
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if got, present := build.Metadata[beadmeta.NativeStepDependenciesMetadataKey]; present {
		t.Fatalf("fragment topology = %q, want omitted UNKNOWN", got)
	}
}
