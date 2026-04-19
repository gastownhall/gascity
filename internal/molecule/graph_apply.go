package molecule

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// graphApplyEnabled controls whether Instantiate uses the GraphApplyStore
// batch path. When false, falls back to sequential bead creation.
// Set by the daemon config loader from [daemon] formula_v2.
//
// Stored as atomic.Bool so config reload can race safely with in-flight
// instantiation. Each instantiate call snapshots the value once via
// IsGraphApplyEnabled.
var graphApplyEnabled atomic.Bool

// SetGraphApplyEnabled sets the graph-apply batch instantiation flag. Safe
// for concurrent use with IsGraphApplyEnabled; intended for the daemon
// config loader and tests.
func SetGraphApplyEnabled(v bool) {
	graphApplyEnabled.Store(v)
}

// IsGraphApplyEnabled reports whether graph-apply batch instantiation is
// allowed. Safe for concurrent use.
func IsGraphApplyEnabled() bool {
	return graphApplyEnabled.Load()
}

func graphApplyTracef(format string, args ...any) {
	path := os.Getenv("GC_SLING_TRACE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()                                                                                    //nolint:errcheck // best-effort trace log
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...)) //nolint:errcheck
}

func instantiateViaGraphApply(ctx context.Context, applier beads.GraphApplyStore, recipe *formula.Recipe, opts Options) (*Result, error) {
	graphApplyTracef("graph-apply enter recipe=%s applier=%T", recipe.Name, applier)
	plan, graphWorkflow, rootKey, err := buildRecipeApplyPlan(recipe, opts)
	if err != nil {
		graphApplyTracef("graph-apply plan-error recipe=%s err=%v", recipe.Name, err)
		return nil, err
	}
	applied, err := applier.ApplyGraphPlan(ctx, plan)
	if err != nil {
		graphApplyTracef("graph-apply apply-error recipe=%s err=%v", recipe.Name, err)
		return nil, err
	}
	if err := beads.ValidateGraphApplyResult(plan, applied); err != nil {
		graphApplyTracef("graph-apply validate-error recipe=%s err=%v", recipe.Name, err)
		return nil, err
	}
	graphApplyTracef("graph-apply applied recipe=%s nodes=%d", recipe.Name, len(applied.IDs))
	rootID := applied.IDs[rootKey]
	if rootID == "" {
		return nil, fmt.Errorf("graph apply result missing root ID for %q", rootKey)
	}
	return &Result{
		RootID:        rootID,
		GraphWorkflow: graphWorkflow,
		IDMapping:     applied.IDs,
		Created:       len(applied.IDs),
	}, nil
}

func instantiateFragmentViaGraphApply(ctx context.Context, store beads.Store, applier beads.GraphApplyStore, recipe *formula.FragmentRecipe, opts FragmentOptions) (*FragmentResult, error) {
	graphApplyTracef("graph-apply fragment-enter root=%s applier=%T", opts.RootID, applier)
	plan, err := buildFragmentApplyPlan(store, recipe, opts)
	if err != nil {
		graphApplyTracef("graph-apply fragment-plan-error root=%s err=%v", opts.RootID, err)
		return nil, err
	}
	applied, err := applier.ApplyGraphPlan(ctx, plan)
	if err != nil {
		graphApplyTracef("graph-apply fragment-apply-error root=%s err=%v", opts.RootID, err)
		return nil, err
	}
	if err := beads.ValidateGraphApplyResult(plan, applied); err != nil {
		graphApplyTracef("graph-apply fragment-validate-error root=%s err=%v", opts.RootID, err)
		return nil, err
	}
	graphApplyTracef("graph-apply fragment-applied root=%s nodes=%d", opts.RootID, len(applied.IDs))
	return &FragmentResult{
		IDMapping: applied.IDs,
		Created:   len(applied.IDs),
	}, nil
}

func buildRecipeApplyPlan(recipe *formula.Recipe, opts Options) (*beads.GraphApplyPlan, bool, string, error) {
	if recipe == nil {
		return nil, false, "", fmt.Errorf("recipe is nil")
	}
	if len(recipe.Steps) == 0 {
		return nil, false, "", fmt.Errorf("recipe %q has no steps", recipe.Name)
	}

	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	priorityOverride := clonePriority(opts.PriorityOverride)
	graphWorkflow := len(recipe.Steps) > 0 && recipe.Steps[0].Metadata["gc.kind"] == "workflow"
	rootKey := recipe.Steps[0].ID
	rootIncluded := false

	plan := &beads.GraphApplyPlan{
		CommitMessage: fmt.Sprintf("gc: instantiate %s", recipe.Name),
		Nodes:         make([]beads.GraphApplyNode, 0, len(recipe.Steps)),
		Edges:         make([]beads.GraphApplyEdge, 0, len(recipe.Deps)),
	}

	for i, step := range recipe.Steps {
		if recipe.RootOnly && i > 0 {
			break
		}
		node, err := recipeStepToGraphNode(step, vars, priorityOverride)
		if err != nil {
			return nil, false, "", err
		}
		if step.IsRoot {
			rootIncluded = true
			if !opts.PreserveRootType && step.Metadata["gc.kind"] != "workflow" {
				node.Type = "molecule"
			}
			if opts.Title != "" {
				node.Title = opts.Title
			}
			if opts.ParentID != "" && step.Metadata["gc.kind"] != "workflow" {
				node.ParentID = opts.ParentID
			}
			if opts.IdempotencyKey != "" {
				if node.Metadata == nil {
					node.Metadata = make(map[string]string, 1)
				}
				node.Metadata["idempotency_key"] = opts.IdempotencyKey
			}
		} else {
			if node.Metadata == nil {
				node.Metadata = make(map[string]string, 1)
			}
			if node.Metadata["gc.step_ref"] == "" {
				node.Metadata["gc.step_ref"] = step.ID
			}
			if (graphWorkflow || step.Metadata["gc.kind"] != "") && node.Metadata["gc.root_bead_id"] == "" {
				if node.MetadataRefs == nil {
					node.MetadataRefs = make(map[string]string, 1)
				}
				node.MetadataRefs["gc.root_bead_id"] = rootKey
			}
			if logicalStepID, ok := logicalRecipeStepID(step); ok {
				if node.MetadataRefs == nil {
					node.MetadataRefs = make(map[string]string, 1)
				}
				node.MetadataRefs["gc.logical_bead_id"] = logicalStepID
			}
			if node.Assignee != "" {
				node.AssignAfterCreate = true
			}
		}
		// Same residual-var guard as Instantiate — see #618.
		if strings.Contains(node.Title, "{{") {
			if residual := formula.CheckResidualVars(node.Title); len(residual) > 0 {
				return nil, false, "", fmt.Errorf("step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?", step.ID, strings.Join(residual, ", "))
			}
		}

		plan.Nodes = append(plan.Nodes, node)
	}
	if !rootIncluded {
		return nil, false, "", fmt.Errorf("recipe %q root step was not included", recipe.Name)
	}

	included := make(map[string]bool, len(plan.Nodes))
	for _, node := range plan.Nodes {
		included[node.Key] = true
	}

	for _, dep := range recipe.Deps {
		if !included[dep.StepID] || !included[dep.DependsOnID] {
			continue
		}
		plan.Edges = append(plan.Edges, beads.GraphApplyEdge{
			FromKey:  dep.StepID,
			ToKey:    dep.DependsOnID,
			Type:     dep.Type,
			Metadata: dep.Metadata,
		})
		if dep.Type == "parent-child" {
			setNodeParentRef(plan.Nodes, dep.StepID, dep.DependsOnID, "")
		}
	}

	// Connect non-root steps to the root via a non-blocking dependency so
	// bd delete --cascade from the root still discovers all workflow beads
	// through the dependency graph without making the workflow root a
	// readiness blocker for finalizers and teardown work.
	if graphWorkflow && rootKey != "" {
		for _, node := range plan.Nodes {
			if node.Key == rootKey {
				continue
			}
			plan.Edges = append(plan.Edges, beads.GraphApplyEdge{
				FromKey: node.Key,
				ToKey:   rootKey,
				Type:    "tracks",
			})
		}
	}

	return plan, graphWorkflow, rootKey, nil
}

func buildFragmentApplyPlan(store beads.Store, recipe *formula.FragmentRecipe, opts FragmentOptions) (*beads.GraphApplyPlan, error) {
	if recipe == nil {
		return nil, fmt.Errorf("recipe is nil")
	}
	if opts.RootID == "" {
		return nil, fmt.Errorf("fragment instantiation requires RootID")
	}
	if len(recipe.Steps) == 0 {
		return &beads.GraphApplyPlan{}, nil
	}

	existingLogicalBeadIDs, err := existingLogicalBeadIDIndex(store, opts.RootID)
	if err != nil {
		return nil, fmt.Errorf("indexing existing logical beads: %w", err)
	}
	priorityOverride := clonePriority(opts.PriorityOverride)
	if priorityOverride == nil {
		root, err := store.Get(opts.RootID)
		if err != nil {
			return nil, fmt.Errorf("loading root bead %s: %w", opts.RootID, err)
		}
		priorityOverride = clonePriority(root.Priority)
	}
	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	externalDepsByStep := make(map[string][]ExternalDep)
	for _, dep := range opts.ExternalDeps {
		if dep.StepID == "" || dep.DependsOnID == "" {
			continue
		}
		if dep.Type == "" {
			dep.Type = "blocks"
		}
		externalDepsByStep[dep.StepID] = append(externalDepsByStep[dep.StepID], dep)
	}

	plan := &beads.GraphApplyPlan{
		CommitMessage: fmt.Sprintf("gc: instantiate fragment into %s", opts.RootID),
		Nodes:         make([]beads.GraphApplyNode, 0, len(recipe.Steps)),
		Edges:         make([]beads.GraphApplyEdge, 0, len(recipe.Deps)+len(opts.ExternalDeps)),
	}

	for _, step := range recipe.Steps {
		node, err := recipeStepToGraphNode(step, vars, priorityOverride)
		if err != nil {
			return nil, err
		}
		if node.Metadata == nil {
			node.Metadata = make(map[string]string, 2)
		}
		if node.Metadata["gc.step_ref"] == "" {
			node.Metadata["gc.step_ref"] = step.ID
		}
		node.Metadata["gc.root_bead_id"] = opts.RootID
		if logicalStepID, ok := logicalRecipeStepID(step); ok {
			if existingLogicalBeadID := existingLogicalBeadIDs[logicalStepID]; existingLogicalBeadID != "" {
				node.Metadata["gc.logical_bead_id"] = existingLogicalBeadID
			} else {
				if node.MetadataRefs == nil {
					node.MetadataRefs = make(map[string]string, 1)
				}
				node.MetadataRefs["gc.logical_bead_id"] = logicalStepID
			}
		}
		if node.Assignee != "" {
			node.AssignAfterCreate = true
		}
		for _, dep := range externalDepsByStep[step.ID] {
			if dep.Type == "parent-child" {
				node.ParentID = dep.DependsOnID
			}
			plan.Edges = append(plan.Edges, beads.GraphApplyEdge{
				FromKey: step.ID,
				ToID:    dep.DependsOnID,
				Type:    dep.Type,
			})
		}
		// Same residual-var guard as buildRecipeApplyPlan — see #618.
		if strings.Contains(node.Title, "{{") {
			if residual := formula.CheckResidualVars(node.Title); len(residual) > 0 {
				return nil, fmt.Errorf("step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?", step.ID, strings.Join(residual, ", "))
			}
		}

		plan.Nodes = append(plan.Nodes, node)
	}

	for _, dep := range recipe.Deps {
		plan.Edges = append(plan.Edges, beads.GraphApplyEdge{
			FromKey:  dep.StepID,
			ToKey:    dep.DependsOnID,
			Type:     dep.Type,
			Metadata: dep.Metadata,
		})
		if dep.Type == "parent-child" {
			setNodeParentRef(plan.Nodes, dep.StepID, dep.DependsOnID, "")
		}
	}

	// Connect fragment steps to the root via a non-blocking dependency so
	// cascade deletion from the root still discovers them through the
	// dependency graph without introducing artificial blockers.
	if opts.RootID != "" {
		for _, node := range plan.Nodes {
			plan.Edges = append(plan.Edges, beads.GraphApplyEdge{
				FromKey: node.Key,
				ToID:    opts.RootID,
				Type:    "tracks",
			})
		}
	}

	return plan, nil
}

func recipeStepToGraphNode(step formula.RecipeStep, vars map[string]string, priorityOverride *int) (beads.GraphApplyNode, error) { //nolint:unparam // error return reserved for future validation
	b := stepToBead(step, vars, priorityOverride)
	return beads.GraphApplyNode{
		Key:         step.ID,
		Title:       b.Title,
		Type:        b.Type,
		Priority:    clonePriority(b.Priority),
		Description: b.Description,
		Assignee:    b.Assignee,
		From:        b.From,
		Labels:      slices.Clone(b.Labels),
		Metadata:    maps.Clone(b.Metadata),
	}, nil
}

func setNodeParentRef(nodes []beads.GraphApplyNode, stepID, parentKey, parentID string) {
	for i := range nodes {
		if nodes[i].Key != stepID {
			continue
		}
		if parentKey != "" {
			nodes[i].ParentKey = parentKey
		}
		if parentID != "" {
			nodes[i].ParentID = parentID
		}
		return
	}
}
