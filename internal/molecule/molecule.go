// Package molecule instantiates compiled formula recipes as bead molecules
// in a Store. It composes the formula compilation layer (Layer 2) with the
// bead store (Layer 1) to implement Gas City's mechanism #7.
//
// The primary entry points are Cook (compile + instantiate) and Instantiate
// (instantiate a pre-compiled Recipe).
package molecule

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// Options configures molecule instantiation.
type Options struct {
	// Title overrides the root bead's title. If empty, the formula's
	// default title (or {{title}} placeholder after substitution) is used.
	Title string

	// Vars provides variable values for {{placeholder}} substitution in
	// titles, descriptions, and notes. Formula defaults are applied first;
	// these values take precedence.
	Vars map[string]string

	// ParentID attaches the molecule to an existing bead. When set, the
	// root bead's ParentID is set to this value.
	ParentID string

	// IdempotencyKey is set as metadata on the root bead atomically with
	// creation. Used by the convergence loop to prevent duplicate wisps
	// on crash-retry.
	IdempotencyKey string
}

// FragmentOptions configures instantiation of a rootless recipe fragment into
// an existing workflow root.
type FragmentOptions struct {
	// RootID is the existing workflow root bead ID to stamp onto all created
	// beads as gc.root_bead_id.
	RootID string

	// Vars provides variable values for {{placeholder}} substitution.
	Vars map[string]string

	// ExternalDeps wires fragment steps to already-existing bead IDs.
	// These deps are embedded at create time so readiness and assignment are
	// correct before the fragment becomes visible to workers.
	ExternalDeps []ExternalDep
}

// ExternalDep binds a fragment step to an already-existing bead.
type ExternalDep struct {
	StepID      string
	DependsOnID string
	Type        string
}

// Result holds the outcome of molecule instantiation.
type Result struct {
	// RootID is the store-assigned ID of the root bead.
	RootID string

	// GraphWorkflow reports whether the instantiated recipe root is a graph-first
	// workflow head instead of a legacy molecule root.
	GraphWorkflow bool

	// IDMapping maps recipe step IDs to store-assigned bead IDs.
	IDMapping map[string]string

	// Created is the total number of beads created.
	Created int
}

// FragmentResult reports the outcome of fragment instantiation.
type FragmentResult struct {
	IDMapping map[string]string
	Created   int
}

// Cook compiles a formula by name and instantiates it as a molecule.
// This is the convenience wrapper that most callers should use.
func Cook(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, opts Options) (*Result, error) {
	recipe, err := formula.Compile(ctx, formulaName, searchPaths, opts.Vars)
	if err != nil {
		return nil, fmt.Errorf("compiling formula %q: %w", formulaName, err)
	}
	return Instantiate(ctx, store, recipe, opts)
}

// CookOn compiles a formula and attaches it to an existing bead.
// Shorthand for Cook with opts.ParentID set.
func CookOn(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, opts Options) (*Result, error) {
	if opts.ParentID == "" {
		return nil, fmt.Errorf("CookOn requires Options.ParentID")
	}
	return Cook(ctx, store, formulaName, searchPaths, opts)
}

// Instantiate creates beads from a pre-compiled Recipe. Use this when
// you need to inspect or modify the Recipe before instantiation.
//
// Steps are created in order (root first, then children depth-first).
// Dependencies are wired after all beads exist. On partial failure,
// already-created beads are marked with "molecule_failed" metadata
// for cleanup.
func Instantiate(ctx context.Context, store beads.Store, recipe *formula.Recipe, opts Options) (*Result, error) {
	_ = ctx // reserved for future cancellation support

	if recipe == nil {
		return nil, fmt.Errorf("recipe is nil")
	}
	if len(recipe.Steps) == 0 {
		return nil, fmt.Errorf("recipe %q has no steps", recipe.Name)
	}

	// Merge variable defaults from recipe with caller-provided vars.
	vars := applyVarDefaults(opts.Vars, recipe.Vars)

	// Build the list of beads to create.
	idMapping := make(map[string]string, len(recipe.Steps))
	var createdIDs []string
	embeddedDeps := make(map[string]bool)
	pendingAssignees := make(map[string]string)
	graphWorkflow := len(recipe.Steps) > 0 && recipe.Steps[0].Metadata["gc.kind"] == "workflow"

	for i, step := range recipe.Steps {
		// For RootOnly recipes, only create the root bead.
		if recipe.RootOnly && i > 0 {
			break
		}

		b := stepToBead(step, vars)
		hasFutureBlocker := false
		for _, dep := range recipe.Deps {
			if dep.StepID != step.ID || dep.Type == "parent-child" {
				continue
			}
			dependsOnBeadID, ok := idMapping[dep.DependsOnID]
			if !ok || dependsOnBeadID == "" {
				hasFutureBlocker = true
				continue
			}
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dependsOnBeadID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dependsOnBeadID)
			}
			embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] = true
		}
		// Root bead overrides.
		if step.IsRoot {
			if step.Metadata["gc.kind"] != "workflow" {
				b.Type = "molecule"
			}
			b.Ref = recipe.Name
			if opts.Title != "" {
				b.Title = opts.Title
			}
			if opts.ParentID != "" && step.Metadata["gc.kind"] != "workflow" {
				b.ParentID = opts.ParentID
			}
			if opts.IdempotencyKey != "" {
				if b.Metadata == nil {
					b.Metadata = make(map[string]string, 1)
				}
				b.Metadata["idempotency_key"] = opts.IdempotencyKey
			}
		} else {
			// Non-root beads: resolve ParentID from the parent-child deps.
			for _, dep := range recipe.Deps {
				if dep.StepID == step.ID && dep.Type == "parent-child" {
					if parentBeadID, ok := idMapping[dep.DependsOnID]; ok {
						b.ParentID = parentBeadID
					}
					break
				}
			}
			// Set Ref to the step ID suffix (after the formula name prefix).
			b.Ref = step.ID
			if b.Metadata == nil {
				b.Metadata = make(map[string]string, 1)
			}
			if b.Metadata["gc.step_ref"] == "" {
				b.Metadata["gc.step_ref"] = step.ID
			}

			if graphWorkflow || step.Metadata["gc.kind"] != "" {
				if rootBeadID, ok := idMapping[recipe.Steps[0].ID]; ok && rootBeadID != "" {
					b.Metadata["gc.root_bead_id"] = rootBeadID
				}
			}

			// Inline Ralph attempt beads need the actual logical bead ID at runtime.
			// Stamp it during instantiation while the recipe-step -> bead mapping is live.
			if logicalStepID, ok := logicalRecipeStepID(step); ok {
				if logicalBeadID, exists := idMapping[logicalStepID]; exists {
					if b.Metadata == nil {
						b.Metadata = make(map[string]string, 1)
					}
					b.Metadata["gc.logical_bead_id"] = logicalBeadID
				}
			}

			// Graph-first workflows must not expose partially wired steps to
			// live workers. Create non-root beads unassigned, wire the full graph,
			// then assign them in a final pass.
			if graphWorkflow && b.Assignee != "" && hasFutureBlocker {
				pendingAssignees[step.ID] = b.Assignee
				b.Assignee = ""
			}
		}

		created, err := store.Create(b)
		if err != nil {
			// Best-effort cleanup: mark already-created beads as failed.
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("creating bead for step %q: %w", step.ID, err)
		}

		idMapping[step.ID] = created.ID
		createdIDs = append(createdIDs, created.ID)

	}

	// Wire dependencies using the IDMapping.
	if !recipe.RootOnly {
		for _, dep := range recipe.Deps {
			fromID, fromOK := idMapping[dep.StepID]
			toID, toOK := idMapping[dep.DependsOnID]
			if !fromOK || !toOK {
				continue // step was filtered out (RootOnly or condition)
			}
			// Skip parent-child deps — already handled via ParentID field.
			if dep.Type == "parent-child" {
				continue
			}
			if embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] {
				continue
			}
			if err := store.DepAdd(fromID, toID, dep.Type); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("wiring dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
			}
		}
	}

	if graphWorkflow {
		for stepID, assignee := range pendingAssignees {
			if assignee == "" {
				continue
			}
			beadID, ok := idMapping[stepID]
			if !ok || beadID == "" {
				continue
			}
			if err := store.Update(beadID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("assigning graph step %q: %w", stepID, err)
			}
		}
	}

	rootID := ""
	if len(createdIDs) > 0 {
		rootID = createdIDs[0]
	}

	return &Result{
		RootID:        rootID,
		GraphWorkflow: graphWorkflow,
		IDMapping:     idMapping,
		Created:       len(createdIDs),
	}, nil
}

// InstantiateFragment creates beads from a rootless recipe fragment and stamps
// them onto an existing workflow root.
func InstantiateFragment(ctx context.Context, store beads.Store, recipe *formula.FragmentRecipe, opts FragmentOptions) (*FragmentResult, error) {
	_ = ctx

	if recipe == nil {
		return nil, fmt.Errorf("recipe is nil")
	}
	if opts.RootID == "" {
		return nil, fmt.Errorf("fragment instantiation requires RootID")
	}
	if len(recipe.Steps) == 0 {
		return &FragmentResult{IDMapping: map[string]string{}}, nil
	}

	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	idMapping := make(map[string]string, len(recipe.Steps))
	var createdIDs []string
	embeddedDeps := make(map[string]bool)
	pendingAssignees := make(map[string]string)
	existingLogicalBeadIDs, err := existingLogicalBeadIDIndex(store, opts.RootID)
	if err != nil {
		return nil, fmt.Errorf("indexing existing logical beads: %w", err)
	}
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

	for _, step := range recipe.Steps {
		b := stepToBead(step, vars)
		hasFutureBlocker := false
		for _, dep := range recipe.Deps {
			if dep.StepID != step.ID || dep.Type == "parent-child" {
				continue
			}
			dependsOnBeadID, ok := idMapping[dep.DependsOnID]
			if !ok || dependsOnBeadID == "" {
				hasFutureBlocker = true
				continue
			}
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dependsOnBeadID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dependsOnBeadID)
			}
			embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] = true
		}
		for _, dep := range externalDepsByStep[step.ID] {
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dep.DependsOnID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dep.DependsOnID)
			}
		}

		if b.Metadata == nil {
			b.Metadata = make(map[string]string, 2)
		}
		if b.Metadata["gc.step_ref"] == "" {
			b.Metadata["gc.step_ref"] = step.ID
		}
		b.Metadata["gc.root_bead_id"] = opts.RootID
		b.Ref = step.ID

		if logicalStepID, ok := logicalRecipeStepID(step); ok {
			if logicalBeadID, exists := idMapping[logicalStepID]; exists {
				b.Metadata["gc.logical_bead_id"] = logicalBeadID
			} else if logicalBeadID := existingLogicalBeadIDs[logicalStepID]; logicalBeadID != "" {
				b.Metadata["gc.logical_bead_id"] = logicalBeadID
			}
		}

		if b.Assignee != "" && hasFutureBlocker {
			pendingAssignees[step.ID] = b.Assignee
			b.Assignee = ""
		}

		created, err := store.Create(b)
		if err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("creating fragment bead for step %q: %w", step.ID, err)
		}
		idMapping[step.ID] = created.ID
		createdIDs = append(createdIDs, created.ID)
	}

	for _, dep := range recipe.Deps {
		fromID, fromOK := idMapping[dep.StepID]
		toID, toOK := idMapping[dep.DependsOnID]
		if !fromOK || !toOK || dep.Type == "parent-child" {
			continue
		}
		if embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] {
			continue
		}
		if err := store.DepAdd(fromID, toID, dep.Type); err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("wiring fragment dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
		}
	}

	for stepID, assignee := range pendingAssignees {
		if assignee == "" {
			continue
		}
		beadID, ok := idMapping[stepID]
		if !ok || beadID == "" {
			continue
		}
		if err := store.Update(beadID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("assigning fragment step %q: %w", stepID, err)
		}
	}

	return &FragmentResult{
		IDMapping: idMapping,
		Created:   len(createdIDs),
	}, nil
}

// stepToBead converts a RecipeStep to a Bead with variable substitution.
func stepToBead(step formula.RecipeStep, vars map[string]string) beads.Bead {
	stepType := step.Type
	if stepType == "" {
		stepType = "task"
	}

	b := beads.Bead{
		Title:       formula.Substitute(step.Title, vars),
		Description: formula.Substitute(step.Description, vars),
		Type:        stepType,
		Labels:      step.Labels,
		Assignee:    step.Assignee,
	}

	// Merge step metadata + notes into bead metadata.
	if len(step.Metadata) > 0 || step.Notes != "" {
		b.Metadata = make(map[string]string, len(step.Metadata)+1)
		for k, v := range step.Metadata {
			b.Metadata[k] = formula.Substitute(v, vars)
		}
		if step.Notes != "" {
			b.Metadata["notes"] = formula.Substitute(step.Notes, vars)
		}
	}

	return b
}

// applyVarDefaults merges formula variable defaults with caller-provided
// vars. Caller values take precedence over defaults.
func applyVarDefaults(vars map[string]string, defs map[string]*formula.VarDef) map[string]string {
	result := make(map[string]string, len(vars)+len(defs))
	for name, def := range defs {
		if def != nil && def.Default != nil {
			result[name] = *def.Default
		}
	}
	for k, v := range vars {
		result[k] = v
	}
	return result
}

// markFailed sets "molecule_failed" metadata on all created beads.
// Best-effort: errors are silently ignored since we're already in an
// error path.
func markFailed(store beads.Store, ids []string) {
	for _, id := range ids {
		_ = store.SetMetadata(id, "molecule_failed", "true")
	}
}

func logicalRecipeStepID(step formula.RecipeStep) (string, bool) {
	kind := step.Metadata["gc.kind"]
	if attempt := step.Metadata["gc.attempt"]; attempt != "" {
		switch kind {
		case "run", "scope":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".run."+attempt); ok {
				return trimmed, true
			}
		case "check":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".check."+attempt); ok {
				return trimmed, true
			}
		}
	}
	if logicalID := step.Metadata["gc.ralph_step_id"]; logicalID != "" {
		switch kind {
		case "run", "check", "scope":
			return logicalID, true
		}
	}
	if kind != "run" && kind != "check" && kind != "scope" {
		return "", false
	}
	for _, prefix := range []string{".run.", ".check."} {
		if idx := strings.LastIndex(step.ID, prefix); idx > 0 {
			return step.ID[:idx], true
		}
	}
	return "", false
}

func trimAttemptSuffix(id, suffix string) (string, bool) {
	if suffix == "" || !strings.HasSuffix(id, suffix) {
		return "", false
	}
	return strings.TrimSuffix(id, suffix), true
}

func existingLogicalBeadIDIndex(store beads.Store, rootID string) (map[string]string, error) {
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	index := make(map[string]string)
	for _, bead := range all {
		if bead.Metadata["gc.kind"] != "ralph" {
			continue
		}
		if bead.ID != rootID && bead.Metadata["gc.root_bead_id"] != rootID {
			continue
		}
		if stepRef := bead.Metadata["gc.step_ref"]; stepRef != "" {
			index[stepRef] = bead.ID
		}
		if stepID := bead.Metadata["gc.step_id"]; stepID != "" {
			index[stepID] = bead.ID
		}
	}
	return index, nil
}
