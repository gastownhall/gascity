// Package molecule instantiates compiled formula recipes as bead molecules
// in a Store. It composes the formula compilation layer (Layer 2) with the
// bead store (Layer 1) to implement Gas City's mechanism #7.
//
// The primary entry points are Cook (compile + instantiate) and Instantiate
// (instantiate a pre-compiled Recipe).
package molecule

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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

	// PriorityOverride forces every created bead to use the given priority.
	// When nil, each step's compiled priority is used.
	PriorityOverride *int

	// PreserveRootType keeps the root bead's declared type instead of
	// coercing legacy non-workflow roots to molecule containers. Attach uses
	// this for executable sub-DAG roots such as retry attempts.
	PreserveRootType bool

	// DeferAssignees creates assignable beads without an assignee and stores
	// the intended assignee in metadata for later activation.
	DeferAssignees bool
}

const (
	// DeferredAssigneeMetadataKey stores an assignee withheld during speculative
	// molecule creation. Activating the molecule restores the value as Assignee.
	DeferredAssigneeMetadataKey = "gc.deferred_assignee"

	// DeferredRoutedToMetadataKey stores gc.routed_to withheld during
	// speculative molecule creation.
	DeferredRoutedToMetadataKey = "gc.deferred_routed_to"

	// DeferredExecutionRoutedToMetadataKey stores gc.execution_routed_to withheld
	// during speculative molecule creation.
	DeferredExecutionRoutedToMetadataKey = "gc.deferred_execution_routed_to"

	// DeferredTypeMetadataKey stores the bead type withheld during speculative
	// molecule creation. Speculative actionable work is created as a ready-
	// excluded type and restored on activation.
	DeferredTypeMetadataKey = "gc.deferred_type"
)

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

	// PriorityOverride forces every created bead to use the given priority.
	// When nil, the existing workflow root's priority is inherited.
	PriorityOverride *int
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
	compileVars := opts.Vars
	if compileVars == nil {
		compileVars = map[string]string{}
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, compileVars)
	if err != nil {
		return nil, fmt.Errorf("compiling formula %q: %w", formulaName, err)
	}
	if err := ValidateRecipeRuntimeVars(recipe, opts); err != nil {
		return nil, err
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

// AttachOptions configures graph-attach mode for late-bound DAG expansion.
type AttachOptions struct {
	// Title overrides the sub-DAG root bead's title.
	Title string

	// Vars provides variable values for {{placeholder}} substitution.
	Vars map[string]string

	// IdempotencyKey prevents duplicate Attach calls. If non-empty, Attach
	// checks for an existing sub-DAG root with this key before creating beads.
	// Stored as gc.idempotency_key on the sub-DAG root bead.
	IdempotencyKey string

	// ExpectedEpoch enables optimistic concurrency control. If > 0, Attach
	// reads gc.control_epoch from the attach bead and aborts with
	// ErrEpochConflict if it doesn't match. On success, the epoch is
	// incremented atomically with the dep wiring.
	//
	// Callers should always use IdempotencyKey together with ExpectedEpoch
	// to ensure crash-recovery correctness.
	ExpectedEpoch int
}

// ErrEpochConflict is returned when AttachOptions.ExpectedEpoch does not match
// the attach bead's gc.control_epoch. This indicates another processor already
// advanced the control bead.
var ErrEpochConflict = errors.New("attach epoch conflict")

// AttachResult holds the outcome of a graph-attach operation.
type AttachResult struct {
	// RootID is the store-assigned ID of the sub-DAG root bead.
	RootID string

	// WorkflowRootID is the gc.root_bead_id inherited from the parent workflow.
	WorkflowRootID string

	// Created is the total number of beads created in the sub-DAG.
	Created int

	// IDMapping maps recipe step IDs to store-assigned bead IDs.
	IDMapping map[string]string

	// Duplicate is true when IdempotencyKey matched an existing sub-DAG.
	// RootID and IDMapping are populated from the existing sub-DAG.
	Duplicate bool
}

// Attach grafts a compiled recipe as a sub-DAG onto an existing workflow bead.
// The attach bead gains a blocking dependency on the sub-DAG root, preventing
// it from closing until the sub-DAG completes. All sub-DAG beads inherit the
// parent workflow's gc.root_bead_id.
//
// This is the core primitive for late-bound DAG expansion — any agent, script,
// or workflow step can call it to expand a bead into a sub-workflow at runtime.
//
// NOTE: Attach mutates the input recipe's Steps metadata in-place, stamping
// gc.root_bead_id, gc.root_store_ref, and gc.idempotency_key onto steps.
// Callers must not reuse the recipe after calling Attach.
//
// Idempotency: if IdempotencyKey is set and a sub-DAG root with that key
// already exists under the attach bead's workflow, the existing result is
// returned with Duplicate=true and no new beads are created.
//
// Fencing: if ExpectedEpoch is set, Attach verifies the attach bead's
// gc.control_epoch matches before proceeding. On success, the epoch is
// incremented. This prevents concurrent processors from spawning duplicate
// attempts.
func Attach(ctx context.Context, store beads.Store, recipe *formula.Recipe, attachBeadID string, opts AttachOptions) (*AttachResult, error) {
	if recipe == nil {
		return nil, fmt.Errorf("recipe is nil")
	}
	if attachBeadID == "" {
		return nil, fmt.Errorf("attach bead ID is required")
	}

	parentBead, err := store.Get(attachBeadID)
	if err != nil {
		return nil, fmt.Errorf("attach bead %s: %w", attachBeadID, err)
	}

	rootBeadID := parentBead.Metadata["gc.root_bead_id"]
	if rootBeadID == "" {
		rootBeadID = attachBeadID
	}
	rootStoreRef := parentBead.Metadata["gc.root_store_ref"]

	// Idempotency: check for existing sub-DAG with the same key.
	// This runs before epoch fencing so that crash-retries with stale epochs
	// still return the existing result instead of failing.
	if opts.IdempotencyKey != "" {
		if existing, err := findExistingAttach(store, recipe, rootBeadID, attachBeadID, opts.IdempotencyKey, opts.ExpectedEpoch); err != nil {
			return nil, fmt.Errorf("idempotency check: %w", err)
		} else if existing != nil {
			return existing, nil
		}
	}

	// Epoch fencing: verify no concurrent processor has advanced the control bead.
	// Only checked for new attaches (not duplicates, which return above).
	if opts.ExpectedEpoch > 0 {
		currentEpoch := 0
		if raw := parentBead.Metadata["gc.control_epoch"]; raw != "" {
			currentEpoch, _ = strconv.Atoi(raw)
		}
		if currentEpoch != opts.ExpectedEpoch {
			return nil, ErrEpochConflict
		}
	}
	if err := ValidateRecipeRuntimeVars(recipe, Options{Title: opts.Title, Vars: opts.Vars}); err != nil {
		return nil, fmt.Errorf("validate runtime vars: %w", err)
	}

	// Stamp every step with the parent workflow's graph metadata.
	for i := range recipe.Steps {
		if recipe.Steps[i].Metadata == nil {
			recipe.Steps[i].Metadata = make(map[string]string)
		}
		recipe.Steps[i].Metadata["gc.root_bead_id"] = rootBeadID
		if rootStoreRef != "" {
			recipe.Steps[i].Metadata["gc.root_store_ref"] = rootStoreRef
		}
	}

	// Stamp idempotency key on the root step.
	if opts.IdempotencyKey != "" && len(recipe.Steps) > 0 {
		if recipe.Steps[0].Metadata == nil {
			recipe.Steps[0].Metadata = make(map[string]string)
		}
		recipe.Steps[0].Metadata["gc.idempotency_key"] = opts.IdempotencyKey
	}

	result, err := Instantiate(ctx, store, recipe, Options{
		Title:            opts.Title,
		Vars:             opts.Vars,
		PriorityOverride: clonePriority(parentBead.Priority),
		PreserveRootType: true,
	})
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}

	// Wire blocking dep: attach bead blocks on sub-DAG root.
	if err := store.DepAdd(attachBeadID, result.RootID, "blocks"); err != nil {
		return nil, fmt.Errorf("dep %s -> %s: %w", attachBeadID, result.RootID, err)
	}

	// Increment epoch after successful attach.
	if opts.ExpectedEpoch > 0 {
		nextEpoch := strconv.Itoa(opts.ExpectedEpoch + 1)
		if err := store.SetMetadata(attachBeadID, "gc.control_epoch", nextEpoch); err != nil {
			return nil, fmt.Errorf("incrementing epoch on %s: %w", attachBeadID, err)
		}
	}

	return &AttachResult{
		RootID:         result.RootID,
		WorkflowRootID: rootBeadID,
		Created:        result.Created,
		IDMapping:      result.IDMapping,
	}, nil
}

// findExistingAttach checks if a sub-DAG root with the given idempotency key
// already exists in the workflow. Returns nil if not found.
func findExistingAttach(store beads.Store, recipe *formula.Recipe, rootBeadID, attachBeadID, key string, expectedEpoch int) (*AttachResult, error) {
	all, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			"gc.idempotency_key": key,
			"gc.root_bead_id":    rootBeadID,
		},
	})
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		if b.Metadata["gc.idempotency_key"] != key {
			continue
		}
		if b.Metadata["gc.root_bead_id"] != rootBeadID {
			continue
		}
		if b.Metadata["molecule_failed"] == "true" {
			return nil, fmt.Errorf("existing attach root %s for idempotency key %q is marked molecule_failed", b.ID, key)
		}
		// Found existing sub-DAG root. Ensure dep is wired.
		deps, err := store.DepList(attachBeadID, "down")
		if err != nil {
			return nil, err
		}
		depExists := false
		for _, d := range deps {
			if d.DependsOnID == b.ID && d.Type == "blocks" {
				depExists = true
				break
			}
		}
		if !depExists {
			if err := store.DepAdd(attachBeadID, b.ID, "blocks"); err != nil {
				return nil, err
			}
		}
		if err := advanceAttachEpochIfNeeded(store, attachBeadID, expectedEpoch); err != nil {
			return nil, err
		}
		idMapping, err := existingAttachIDMapping(store, recipe, rootBeadID, b)
		if err != nil {
			return nil, err
		}
		return &AttachResult{
			RootID:         b.ID,
			WorkflowRootID: rootBeadID,
			IDMapping:      idMapping,
			Duplicate:      true,
		}, nil
	}
	return nil, nil
}

func advanceAttachEpochIfNeeded(store beads.Store, attachBeadID string, expectedEpoch int) error {
	if expectedEpoch <= 0 {
		return nil
	}
	attachBead, err := store.Get(attachBeadID)
	if err != nil {
		return err
	}
	currentEpoch, _ := strconv.Atoi(strings.TrimSpace(attachBead.Metadata["gc.control_epoch"]))
	if currentEpoch != expectedEpoch {
		return nil
	}
	nextEpoch := expectedEpoch + 1
	return store.SetMetadata(attachBeadID, "gc.control_epoch", strconv.Itoa(nextEpoch))
}

func existingAttachIDMapping(store beads.Store, recipe *formula.Recipe, rootBeadID string, root beads.Bead) (map[string]string, error) {
	idMapping := map[string]string{}
	if recipe == nil {
		return idMapping, nil
	}
	wantedRefs := map[string][]string{}
	for i, step := range recipe.Steps {
		if i == 0 || step.IsRoot {
			idMapping[step.ID] = root.ID
			continue
		}
		for _, ref := range attachStepRefs(step) {
			wantedRefs[ref] = append(wantedRefs[ref], step.ID)
		}
	}
	if len(wantedRefs) == 0 {
		return idMapping, nil
	}
	all, err := store.List(beads.ListQuery{
		Metadata: map[string]string{"gc.root_bead_id": rootBeadID},
	})
	if err != nil {
		return nil, err
	}
	for _, bead := range all {
		if bead.Metadata["molecule_failed"] == "true" {
			continue
		}
		ref := strings.TrimSpace(bead.Metadata["gc.step_ref"])
		if ref == "" {
			ref = strings.TrimSpace(bead.Ref)
		}
		if ref == "" {
			continue
		}
		for _, stepID := range wantedRefs[ref] {
			if idMapping[stepID] == "" {
				idMapping[stepID] = bead.ID
			}
		}
	}
	return idMapping, nil
}

func attachStepRefs(step formula.RecipeStep) []string {
	refs := make([]string, 0, 2)
	if ref := strings.TrimSpace(step.Metadata["gc.step_ref"]); ref != "" {
		refs = append(refs, ref)
	}
	if id := strings.TrimSpace(step.ID); id != "" {
		duplicate := false
		for _, ref := range refs {
			if ref == id {
				duplicate = true
				break
			}
		}
		if !duplicate {
			refs = append(refs, id)
		}
	}
	return refs
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
	if !opts.DeferAssignees && IsGraphApplyEnabled() {
		if applier, ok := store.(beads.GraphApplyStore); ok {
			return instantiateViaGraphApply(ctx, applier, recipe, opts)
		}
		graphApplyTracef("graph-apply unavailable recipe=%s store=%T", recipe.Name, store)
	}

	// Merge variable defaults from recipe with caller-provided vars.
	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	priorityOverride := clonePriority(opts.PriorityOverride)

	// Build the list of beads to create.
	idMapping := make(map[string]string, len(recipe.Steps))
	createdParentByStep := make(map[string]string, len(recipe.Steps))
	var createdIDs []string
	embeddedDeps := make(map[string]bool)
	pendingAssignees := make(map[string]string)
	graphWorkflow := preservesGraphActionTypes(recipe)

	for i, step := range recipe.Steps {
		// For RootOnly recipes, only create the root bead.
		if recipe.RootOnly && i > 0 {
			break
		}

		b := stepToBead(step, vars, priorityOverride)
		if opts.DeferAssignees {
			deferBeadRouting(&b)
		}
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
			if !opts.PreserveRootType && !preserveExecutableRootType(step) {
				b.Type = "molecule"
			}
			b.Ref = recipe.Name
			if opts.Title != "" {
				b.Title = formula.Substitute(opts.Title, vars)
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
			// graph.v2 workflows and their retry/Ralph attempt sub-recipes
			// use step beads as independently routable actionable work, not
			// scaffolding — skip the #1039 coercion so Ready() still surfaces
			// them for worker claim.
			if !graphWorkflow {
				b.Type = nonRootStepBeadType(b.Type)
			}
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
				if b.Metadata["gc.root_bead_id"] == "" {
					if rootBeadID, ok := idMapping[recipe.Steps[0].ID]; ok && rootBeadID != "" {
						b.Metadata["gc.root_bead_id"] = rootBeadID
					}
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

		// Catch unresolved {{...}} in the bead title — the field agents see
		// first. Unresolved placeholders here cause agent churn (#618).
		// Description is intentionally excluded: formulas may embed {{...}}
		// as agent-readable templates resolved at claim time.
		if strings.Contains(b.Title, "{{") {
			if residual := formula.CheckResidualVars(b.Title); len(residual) > 0 {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?", step.ID, strings.Join(residual, ", "))
			}
		}
		if err := validateTimeoutMetadataVars(step.ID, b.Metadata); err != nil {
			markFailed(store, createdIDs)
			return nil, err
		}

		created, err := store.Create(b)
		if err != nil {
			// Best-effort cleanup: mark already-created beads as failed.
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("creating bead for step %q: %w", step.ID, err)
		}

		idMapping[step.ID] = created.ID
		createdParentByStep[step.ID] = created.ParentID
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
			if embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] {
				continue
			}
			if dep.Type == "parent-child" && createdParentByStep[dep.StepID] != toID {
				parentID := toID
				if err := store.Update(fromID, beads.UpdateOpts{ParentID: &parentID}); err != nil {
					markFailed(store, createdIDs)
					return nil, fmt.Errorf("setting parent for dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
				}
				createdParentByStep[dep.StepID] = toID
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
	priorityOverride := clonePriority(opts.PriorityOverride)
	if priorityOverride == nil {
		root, err := store.Get(opts.RootID)
		if err != nil {
			return nil, fmt.Errorf("loading root bead %s: %w", opts.RootID, err)
		}
		priorityOverride = clonePriority(root.Priority)
	}
	if IsGraphApplyEnabled() {
		if applier, ok := store.(beads.GraphApplyStore); ok {
			opts.PriorityOverride = priorityOverride
			return instantiateFragmentViaGraphApply(ctx, store, applier, recipe, opts)
		}
		graphApplyTracef("graph-apply fragment-unavailable root=%s store=%T", opts.RootID, store)
	}

	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	idMapping := make(map[string]string, len(recipe.Steps))
	var createdIDs []string
	createdParentByStep := make(map[string]string, len(recipe.Steps))
	embeddedDeps := make(map[string]bool)
	pendingAssignees := make(map[string]string)
	existingLogicalBeadIDs, err := existingLogicalBeadIDIndex(store, opts.RootID)
	if err != nil {
		return nil, fmt.Errorf("indexing existing logical beads: %w", err)
	}
	externalDepsByStep, err := groupExternalDeps(opts.ExternalDeps)
	if err != nil {
		return nil, err
	}
	recipeParentByStep := recipeParentDeps(recipe.Deps)

	for _, step := range recipe.Steps {
		b := stepToBead(step, vars, priorityOverride)
		// Fragment entries stay "task" — unlike formula scaffolding steps,
		// fanout-expanded fragment beads are actionable work that pool
		// workers claim from `bd ready`. Do not apply nonRootStepBeadType
		// here (#1039).
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
			if dep.Type == "parent-child" {
				continue
			}
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dep.DependsOnID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dep.DependsOnID)
			}
		}
		for _, dep := range recipe.Deps {
			if dep.StepID == step.ID && dep.Type == "parent-child" {
				if parentBeadID, ok := idMapping[dep.DependsOnID]; ok {
					b.ParentID = parentBeadID
					break
				}
			}
		}
		for _, dep := range externalDepsByStep[step.ID] {
			if b.ParentID == "" && recipeParentByStep[step.ID] == "" && dep.Type == "parent-child" && dep.DependsOnID != "" {
				b.ParentID = dep.DependsOnID
				break
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

		// Same residual-var guard as Instantiate — see #618.
		if strings.Contains(b.Title, "{{") {
			if residual := formula.CheckResidualVars(b.Title); len(residual) > 0 {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?", step.ID, strings.Join(residual, ", "))
			}
		}
		if err := validateTimeoutMetadataVars(step.ID, b.Metadata); err != nil {
			markFailed(store, createdIDs)
			return nil, err
		}

		created, err := store.Create(b)
		if err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("creating fragment bead for step %q: %w", step.ID, err)
		}
		idMapping[step.ID] = created.ID
		createdParentByStep[step.ID] = created.ParentID
		createdIDs = append(createdIDs, created.ID)
	}

	for _, dep := range recipe.Deps {
		fromID, fromOK := idMapping[dep.StepID]
		toID, toOK := idMapping[dep.DependsOnID]
		if !fromOK || !toOK {
			continue
		}
		if embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] {
			continue
		}
		if dep.Type == "parent-child" && createdParentByStep[dep.StepID] != toID {
			parentID := toID
			if err := store.Update(fromID, beads.UpdateOpts{ParentID: &parentID}); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("setting fragment parent for dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
			}
			createdParentByStep[dep.StepID] = toID
		}
		if err := store.DepAdd(fromID, toID, dep.Type); err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("wiring fragment dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
		}
	}
	for stepID, deps := range externalDepsByStep {
		if recipeParentByStep[stepID] != "" {
			continue
		}
		fromID, fromOK := idMapping[stepID]
		if !fromOK {
			continue
		}
		for _, dep := range deps {
			if dep.Type != "parent-child" {
				continue
			}
			if err := store.DepAdd(fromID, dep.DependsOnID, dep.Type); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("wiring external fragment dep %s->%s: %w", stepID, dep.DependsOnID, err)
			}
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

func recipeParentDeps(deps []formula.RecipeDep) map[string]string {
	parents := make(map[string]string)
	for _, dep := range deps {
		if dep.Type == "parent-child" && dep.StepID != "" && dep.DependsOnID != "" && parents[dep.StepID] == "" {
			parents[dep.StepID] = dep.DependsOnID
		}
	}
	return parents
}

func groupExternalDeps(deps []ExternalDep) (map[string][]ExternalDep, error) {
	byStep := make(map[string][]ExternalDep)
	parentByStep := make(map[string]string)
	for _, dep := range deps {
		if dep.StepID == "" || dep.DependsOnID == "" {
			continue
		}
		if dep.Type == "" {
			dep.Type = "blocks"
		}
		if dep.Type == "parent-child" {
			if parentByStep[dep.StepID] != "" {
				return nil, fmt.Errorf("step %q has multiple external parent-child deps", dep.StepID)
			}
			parentByStep[dep.StepID] = dep.DependsOnID
		}
		byStep[dep.StepID] = append(byStep[dep.StepID], dep)
	}
	return byStep, nil
}

// nonRootStepBeadType returns the type to stamp on a non-root formula step
// bead. Beads typed "task" (the compiler's default — either from an explicit
// TOML `type = "task"` or an unset type) become "step" so Ready() and
// `bd ready` skip formula scaffolding (#1039). Other explicit types
// ("bug", "epic", ...) and the "gate" type produced by deferBeadRouting
// are preserved.
func nonRootStepBeadType(currentType string) string {
	if currentType == "task" {
		return "step"
	}
	return currentType
}

func preservesGraphActionTypes(recipe *formula.Recipe) bool {
	if recipe == nil || len(recipe.Steps) == 0 {
		return false
	}
	root := recipe.Steps[0]
	if root.Metadata["gc.kind"] == "workflow" {
		return true
	}
	return root.Metadata["gc.attempt"] != "" && root.Metadata["gc.step_ref"] != ""
}

// stepToBead converts a RecipeStep to a Bead with variable substitution.
func stepToBead(step formula.RecipeStep, vars map[string]string, priorityOverride *int) beads.Bead {
	stepType := step.Type
	if stepType == "" {
		stepType = "task"
	}

	b := beads.Bead{
		Title:       formula.Substitute(step.Title, vars),
		Description: formula.Substitute(step.Description, vars),
		Type:        stepType,
		Priority:    resolveStepPriority(step, priorityOverride),
		Labels:      substituteLabels(step.Labels, vars),
		Assignee:    formula.Substitute(step.Assignee, vars),
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

func preserveExecutableRootType(step formula.RecipeStep) bool {
	switch step.Metadata["gc.kind"] {
	case "workflow", "wisp":
		return true
	default:
		return false
	}
}

func validateTimeoutMetadataVars(stepID string, metadata map[string]string) error {
	for _, key := range []string{"gc.step_timeout", "gc.check_timeout"} {
		raw := metadata[key]
		if raw == "" {
			continue
		}
		if residual := formula.CheckResidualTimeoutVars(raw); len(residual) > 0 {
			return fmt.Errorf("step %q: metadata %s contains unresolved timeout variable(s) %s — missing or misspelled --var(s)?", stepID, key, strings.Join(residual, ", "))
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("step %q: metadata %s has invalid timeout %q: %w", stepID, key, raw, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("step %q: metadata %s timeout must be positive, got %v", stepID, key, parsed)
		}
	}
	return nil
}

func deferBeadRouting(b *beads.Bead) {
	beadType := b.Type
	if beadType == "" {
		beadType = "task"
	}
	if !beads.IsReadyExcludedType(beadType) {
		ensureBeadMetadata(b)
		b.Metadata[DeferredTypeMetadataKey] = beadType
		b.Type = "gate"
	}
	if b.Assignee != "" {
		ensureBeadMetadata(b)
		b.Metadata[DeferredAssigneeMetadataKey] = b.Assignee
		b.Assignee = ""
	}
	deferBeadMetadataValue(b, "gc.routed_to", DeferredRoutedToMetadataKey)
	deferBeadMetadataValue(b, "gc.execution_routed_to", DeferredExecutionRoutedToMetadataKey)
}

func deferBeadMetadataValue(b *beads.Bead, sourceKey, deferredKey string) {
	if b.Metadata == nil {
		return
	}
	if value := b.Metadata[sourceKey]; value != "" {
		b.Metadata[deferredKey] = value
		delete(b.Metadata, sourceKey)
	}
}

func ensureBeadMetadata(b *beads.Bead) {
	if b.Metadata == nil {
		b.Metadata = make(map[string]string, 1)
	}
}

// substituteLabels applies variable substitution to each label.
func substituteLabels(labels []string, vars map[string]string) []string {
	if len(labels) == 0 {
		return labels
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = formula.Substitute(l, vars)
	}
	return out
}

func resolveStepPriority(step formula.RecipeStep, priorityOverride *int) *int {
	if priorityOverride != nil {
		return clonePriority(priorityOverride)
	}
	return clonePriority(step.Priority)
}

func clonePriority(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
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

// ValidateRecipeRuntimeVars validates runtime variables for a compiled recipe.
// It checks declared formula vars, unresolved title placeholders, and avoids
// duplicating title errors for vars that were already reported as required.
func ValidateRecipeRuntimeVars(recipe *formula.Recipe, opts Options) error {
	validationVars := runtimeValidationVars(recipe, opts)
	validationErrs, missingRequired := formula.CollectVarValidationErrors(recipe.Vars, validationVars)
	titleErrs := unresolvedTitleValidationErrorsWithVars(recipe, opts, validationVars, missingRequired)
	if len(validationErrs) == 0 && len(titleErrs) == 0 {
		return nil
	}
	errs := make([]string, 0, len(validationErrs)+len(titleErrs))
	errs = append(errs, validationErrs...)
	errs = append(errs, titleErrs...)
	return fmt.Errorf("variable validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

func runtimeValidationVars(recipe *formula.Recipe, opts Options) map[string]string {
	if opts.Title == "" || recipe == nil || recipe.Vars == nil {
		return opts.Vars
	}
	if _, ok := recipe.Vars["title"]; !ok {
		return opts.Vars
	}
	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	result := make(map[string]string, len(vars)+1)
	for k, v := range vars {
		result[k] = v
	}
	result["title"] = formula.Substitute(opts.Title, vars)
	return result
}

func unresolvedTitleValidationErrorsWithVars(recipe *formula.Recipe, opts Options, providedVars map[string]string, missingRequired map[string]bool) []string {
	if recipe == nil || len(recipe.Steps) == 0 {
		return nil
	}
	vars := applyVarDefaults(providedVars, recipe.Vars)
	errs := make([]string, 0)
	for i, step := range recipe.Steps {
		if recipe.RootOnly && i > 0 {
			break
		}
		rawTitle := step.Title
		if step.IsRoot && opts.Title != "" {
			rawTitle = opts.Title
		}
		title := formula.Substitute(rawTitle, vars)
		if !strings.Contains(title, "{{") {
			continue
		}
		residual := formula.CheckResidualVars(title)
		unexplained := make([]string, 0, len(residual))
		for _, name := range residual {
			if missingRequired[name] {
				continue
			}
			unexplained = append(unexplained, name)
		}
		if len(unexplained) == 0 {
			continue
		}
		errs = append(errs,
			fmt.Sprintf(`step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?`,
				step.ID, strings.Join(unexplained, ", ")))
	}
	return errs
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
		// v1 patterns: kind-specific suffix stripping.
		switch kind {
		case "run", "scope":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".run."+attempt); ok {
				return trimmed, true
			}
		case "check":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".check."+attempt); ok {
				return trimmed, true
			}
		case "retry-run":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".run."+attempt); ok {
				return trimmed, true
			}
		case "retry-eval":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".eval."+attempt); ok {
				return trimmed, true
			}
		}

		// v2 patterns: attempt/iteration suffix stripping.
		// v2 beads keep their original kind but have gc.attempt set.
		if trimmed, ok := trimAttemptSuffix(step.ID, ".attempt."+attempt); ok {
			return trimmed, true
		}
		if trimmed, ok := trimAttemptSuffix(step.ID, ".iteration."+attempt); ok {
			return trimmed, true
		}
	}
	if logicalID := step.Metadata["gc.ralph_step_id"]; logicalID != "" {
		switch kind {
		case "run", "check", "scope":
			return logicalID, true
		}
	}
	if kind != "run" && kind != "check" && kind != "scope" && kind != "retry-run" && kind != "retry-eval" {
		return "", false
	}
	for _, prefix := range []string{".run.", ".check.", ".eval."} {
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
	all, err := store.List(beads.ListQuery{
		Metadata: map[string]string{"gc.root_bead_id": rootID},
	})
	if err != nil {
		return nil, err
	}
	index := make(map[string]string)
	for _, bead := range all {
		switch bead.Metadata["gc.kind"] {
		case "ralph", "retry":
		default:
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
