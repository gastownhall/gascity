package sling

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// validateDeps checks that required SlingDeps fields are non-nil.
func validateDeps(deps SlingDeps) error {
	if deps.Cfg == nil {
		return fmt.Errorf("sling: Cfg is required")
	}
	if deps.Store == nil {
		return fmt.Errorf("sling: Store is required")
	}
	if deps.Runner == nil {
		return fmt.Errorf("sling: Runner is required")
	}
	return nil
}

// DoSling is the core logic for routing work to an agent.
// Returns structured data -- callers format display strings.
func DoSling(opts SlingOpts, deps SlingDeps, querier BeadQuerier) (SlingResult, error) {
	if err := validateDeps(deps); err != nil {
		return SlingResult{}, err
	}
	a := opts.Target
	result, preErr := preflight(opts, deps, querier)
	if preErr != nil {
		return result, preErr
	}
	if result.DryRun || result.Idempotent {
		return result, nil
	}

	beadID := opts.BeadOrFormula

	switch {
	case opts.IsFormula:
		return slingFormula(opts, deps)
	case opts.OnFormula != "":
		return slingOnFormula(opts, deps, querier, beadID, result)
	case !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "":
		return slingDefaultFormula(opts, deps, querier, beadID, result)
	default:
		return slingPlainBead(opts, deps, beadID, result)
	}
}

// preflight performs warnings, idempotency check, dry-run short-circuit,
// and cross-rig guard. Returns a partially populated result.
func preflight(opts SlingOpts, deps SlingDeps, querier BeadQuerier) (SlingResult, error) {
	a := opts.Target
	var result SlingResult
	result.Target = a.QualifiedName()

	if a.Suspended && !opts.Force {
		result.AgentSuspended = true
	}
	sp := agentutil.ScaleParamsFor(&a)
	if sp.Max == 0 && !opts.Force {
		result.PoolEmpty = true
	}

	// Cross-rig guard.
	if !opts.IsFormula && !opts.Force && !opts.DryRun {
		if msg := CheckCrossRig(opts.BeadOrFormula, a, deps.Cfg); msg != "" {
			return result, fmt.Errorf("%s", msg)
		}
	}

	// Pre-flight idempotency check.
	if !opts.IsFormula && !opts.Force {
		check := CheckBeadState(querier, opts.BeadOrFormula, a, deps)
		if check.Idempotent {
			result.Idempotent = true
			result.DryRun = opts.DryRun
			result.BeadID = opts.BeadOrFormula
			result.Method = "bead"
			return result, nil
		}
		result.BeadWarnings = append(result.BeadWarnings, check.Warnings...)
	}

	// Dry-run: return early with preview info.
	if opts.DryRun {
		result.DryRun = true
		result.BeadID = opts.BeadOrFormula
		result.Method = "bead"
		if opts.IsFormula {
			result.Method = "formula"
		} else if opts.OnFormula != "" {
			result.Method = "on-formula"
		}
		return result, nil
	}

	if opts.ScopeKind != "" && !opts.IsFormula && opts.OnFormula == "" && (opts.NoFormula || a.EffectiveDefaultSlingFormula() == "") {
		return result, fmt.Errorf("--scope-kind/--scope-ref require a formula-backed workflow launch")
	}

	return result, nil
}

// slingFormula handles the --formula dispatch path.
func slingFormula(opts SlingOpts, deps SlingDeps) (SlingResult, error) {
	a := opts.Target
	method := "formula"
	formulaVars := BuildSlingFormulaVars(opts.BeadOrFormula, "", opts.Vars, a, deps)
	mResult, err := InstantiateSlingFormula(context.Background(), opts.BeadOrFormula, SlingFormulaSearchPaths(deps, a), molecule.Options{
		Title: opts.Title,
		Vars:  formulaVars,
	}, "", opts.ScopeKind, opts.ScopeRef, a, deps)
	if err != nil {
		return SlingResult{Target: a.QualifiedName()}, fmt.Errorf("instantiating formula %q: %w", opts.BeadOrFormula, err)
	}
	if mResult.GraphWorkflow || IsGraphWorkflowAttachment(deps.Store, mResult.RootID) {
		wfResult, wfErr := doStartGraphWorkflow(mResult, "", a, method, deps)
		wfResult.FormulaName = opts.BeadOrFormula
		return wfResult, wfErr
	}
	result := SlingResult{Target: a.QualifiedName(), FormulaName: opts.BeadOrFormula}
	return finalize(opts, deps, mResult.RootID, method, result)
}

// slingOnFormula handles the --on formula attachment path.
func slingOnFormula(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID string, result SlingResult) (SlingResult, error) {
	a := opts.Target
	method := "on-formula"
	if err := CheckNoMoleculeChildren(querier, beadID, deps.Store, &result); err != nil {
		return result, fmt.Errorf("%w", err)
	}
	formulaVars := BuildSlingFormulaVars(opts.OnFormula, beadID, opts.Vars, a, deps)
	mResult, err := InstantiateSlingFormula(context.Background(), opts.OnFormula, SlingFormulaSearchPaths(deps, a), molecule.Options{
		Title:            opts.Title,
		Vars:             formulaVars,
		PriorityOverride: BeadPriorityOverride(querier, beadID),
	}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
	if err != nil {
		return result, fmt.Errorf("instantiating formula %q on %s: %w", opts.OnFormula, beadID, err)
	}
	wispRootID := mResult.RootID
	if mResult.GraphWorkflow || IsGraphWorkflowAttachment(deps.Store, wispRootID) {
		wfResult, wfErr := doStartGraphWorkflow(mResult, beadID, a, method, deps)
		wfResult.FormulaName = opts.OnFormula
		return wfResult, wfErr
	}
	if err := deps.Store.SetMetadata(beadID, "molecule_id", wispRootID); err != nil {
		result.MetadataErrors = append(result.MetadataErrors,
			fmt.Sprintf("setting molecule_id on %s: %v", beadID, err))
	}
	result.WispRootID = wispRootID
	result.FormulaName = opts.OnFormula
	return finalize(opts, deps, beadID, method, result)
}

// slingDefaultFormula handles the default formula attachment path.
func slingDefaultFormula(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID string, result SlingResult) (SlingResult, error) {
	a := opts.Target
	method := "default-on-formula"
	defaultFormula := a.EffectiveDefaultSlingFormula()
	if err := CheckNoMoleculeChildren(querier, beadID, deps.Store, &result); err != nil {
		return result, fmt.Errorf("%w", err)
	}
	defaultVars := BuildSlingFormulaVars(defaultFormula, beadID, opts.Vars, a, deps)
	mResult, err := InstantiateSlingFormula(context.Background(), defaultFormula, SlingFormulaSearchPaths(deps, a), molecule.Options{
		Title:            opts.Title,
		Vars:             defaultVars,
		PriorityOverride: BeadPriorityOverride(querier, beadID),
	}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
	if err != nil {
		return result, fmt.Errorf("instantiating default formula %q on %s: %w", defaultFormula, beadID, err)
	}
	wispRootID := mResult.RootID
	if mResult.GraphWorkflow || IsGraphWorkflowAttachment(deps.Store, wispRootID) {
		wfResult, wfErr := doStartGraphWorkflow(mResult, beadID, a, method, deps)
		wfResult.FormulaName = defaultFormula
		return wfResult, wfErr
	}
	if err := deps.Store.SetMetadata(beadID, "molecule_id", wispRootID); err != nil {
		result.MetadataErrors = append(result.MetadataErrors,
			fmt.Sprintf("setting molecule_id on %s: %v", beadID, err))
	}
	result.WispRootID = wispRootID
	result.FormulaName = defaultFormula
	return finalize(opts, deps, beadID, method, result)
}

// slingPlainBead handles plain bead routing (no formula).
func slingPlainBead(opts SlingOpts, deps SlingDeps, beadID string, result SlingResult) (SlingResult, error) {
	return finalize(opts, deps, beadID, "bead", result)
}

// finalize executes the sling command, records telemetry, sets merge
// metadata, creates auto-convoy, pokes the controller, and signals nudge.
func finalize(opts SlingOpts, deps SlingDeps, beadID, method string, result SlingResult) (SlingResult, error) {
	a := opts.Target

	// Execute routing -- prefer typed Router, fall back to shell Runner.
	slingEnv := ResolveSlingEnv(a, deps)
	rigDir := SlingDirForBead(deps.Cfg, deps.CityPath, beadID)
	if deps.Router != nil {
		req := RouteRequest{
			BeadID:  beadID,
			Target:  a.QualifiedName(),
			WorkDir: rigDir,
			Env:     slingEnv,
		}
		if err := deps.Router.Route(context.Background(), req); err != nil {
			telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), method, err)
			return result, fmt.Errorf("%w", err)
		}
	} else {
		slingCmd := BuildSlingCommand(a.EffectiveSlingQuery(), beadID)
		if _, err := deps.Runner(rigDir, slingCmd, slingEnv); err != nil {
			telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), method, err)
			return result, fmt.Errorf("%w", err)
		}
	}
	telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), method, nil)

	// Merge strategy metadata.
	if opts.Merge != "" && deps.Store != nil {
		if err := deps.Store.SetMetadata(beadID, "merge_strategy", opts.Merge); err != nil {
			result.MetadataErrors = append(result.MetadataErrors,
				fmt.Sprintf("setting merge strategy: %v", err))
		}
	}

	// Auto-convoy.
	if !opts.NoConvoy && !opts.IsFormula && deps.Store != nil {
		var convoyLabels []string
		if opts.Owned {
			convoyLabels = []string{"owned"}
		}
		convoy, err := deps.Store.Create(beads.Bead{
			Title:  fmt.Sprintf("sling-%s", beadID),
			Type:   "convoy",
			Labels: convoyLabels,
		})
		if err != nil {
			result.MetadataErrors = append(result.MetadataErrors,
				fmt.Sprintf("creating auto-convoy: %v", err))
		} else {
			parentID := convoy.ID
			if err := deps.Store.Update(beadID, beads.UpdateOpts{ParentID: &parentID}); err != nil {
				result.MetadataErrors = append(result.MetadataErrors,
					fmt.Sprintf("linking bead to convoy: %v", err))
			} else {
				result.ConvoyID = convoy.ID
			}
		}
	}

	result.BeadID = beadID
	result.Method = method

	// Poke controller.
	if !opts.SkipPoke && deps.Notify != nil {
		deps.Notify.PokeController(deps.CityPath)
	}

	// Signal nudge.
	if opts.Nudge {
		result.NudgeAgent = &a
	}

	return result, nil
}

// doStartGraphWorkflow performs post-instantiation graph workflow setup.
func doStartGraphWorkflow(mResult *molecule.Result, sourceBeadID string, a config.Agent, method string, deps SlingDeps) (SlingResult, error) {
	var result SlingResult
	result.Target = a.QualifiedName()
	result.Method = method
	result.WorkflowID = mResult.RootID
	result.BeadID = mResult.RootID

	rootID := mResult.RootID
	SlingTracef("workflow-start begin root=%s source=%s agent=%s method=%s", rootID, sourceBeadID, a.QualifiedName(), method)

	if err := PromoteWorkflowLaunchBead(deps.Store, rootID); err != nil {
		return result, fmt.Errorf("setting workflow root %s in_progress: %w", rootID, err)
	}
	if sourceBeadID != "" {
		if err := deps.Store.SetMetadata(sourceBeadID, "workflow_id", rootID); err != nil {
			return result, fmt.Errorf("setting workflow_id on %s: %w", sourceBeadID, err)
		}
	}
	telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), method, nil)
	if deps.Notify != nil {
		deps.Notify.PokeController(deps.CityPath)
	}
	if deps.Notify != nil {
		deps.Notify.PokeControlDispatch(deps.CityPath)
	}
	return result, nil
}

// DoSlingBatch handles convoy expansion before delegating to DoSling.
func DoSlingBatch(opts SlingOpts, deps SlingDeps, querier BeadChildQuerier) (SlingResult, error) {
	a := opts.Target

	// Formula mode, nil querier → delegate directly.
	if opts.IsFormula || querier == nil {
		return DoSling(opts, deps, querier)
	}

	b, err := querier.Get(opts.BeadOrFormula)
	if err != nil {
		singleOpts := opts
		singleOpts.IsFormula = false
		return DoSling(singleOpts, deps, querier)
	}
	if b.Type == "epic" {
		return SlingResult{}, fmt.Errorf("bead %s is an epic; first-class support is for convoys only", b.ID)
	}

	if !beads.IsContainerType(b.Type) {
		singleOpts := opts
		singleOpts.IsFormula = false
		return DoSling(singleOpts, deps, querier)
	}

	children, err := querier.List(beads.ListQuery{
		ParentID:      b.ID,
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return SlingResult{}, fmt.Errorf("listing children of %s: %w", b.ID, err)
	}

	var open, skipped []beads.Bead
	for _, c := range children {
		if c.Status == "open" {
			open = append(open, c)
		} else {
			skipped = append(skipped, c)
		}
	}

	if len(open) == 0 {
		return SlingResult{}, fmt.Errorf("%s %s has no open children", b.Type, b.ID)
	}

	// Cross-rig guard on container.
	if !opts.Force && !opts.DryRun {
		if msg := CheckCrossRig(b.ID, a, deps.Cfg); msg != "" {
			return SlingResult{}, fmt.Errorf("%s", msg)
		}
	}

	// Dry-run: return early with container preview info.
	if opts.DryRun {
		var batchResult SlingResult
		batchResult.DryRun = true
		batchResult.Target = a.QualifiedName()
		batchResult.BeadID = b.ID
		batchResult.ContainerType = b.Type
		batchResult.Method = "batch"
		batchResult.Total = len(children)
		batchResult.Routed = len(open)
		batchResult.Skipped = len(skipped)
		return batchResult, nil
	}

	// Pre-check molecule attachments.
	var batchResult SlingResult
	batchResult.Target = a.QualifiedName()
	batchResult.BeadID = b.ID
	batchResult.ContainerType = b.Type
	useFormula := opts.OnFormula
	if useFormula == "" && !opts.IsFormula && !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
		useFormula = a.EffectiveDefaultSlingFormula()
	}
	if useFormula != "" {
		if err := CheckBatchNoMoleculeChildren(querier, open, deps.Store, &batchResult); err != nil {
			return batchResult, fmt.Errorf("%w", err)
		}
	}

	batchMethod := "batch"
	if opts.OnFormula != "" {
		batchMethod = "batch-on"
	} else if !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
		batchMethod = "batch-default-on"
	}
	batchResult.Method = batchMethod
	batchResult.Total = len(children)

	routed := 0
	failed := 0
	idempotent := 0
	for _, child := range open {
		childResult := SlingChildResult{BeadID: child.ID}

		if !opts.Force {
			check := CheckBeadState(querier, child.ID, a, deps)
			if check.Idempotent {
				childResult.Skipped = true
				batchResult.Children = append(batchResult.Children, childResult)
				idempotent++
				continue
			}
			batchResult.BeadWarnings = append(batchResult.BeadWarnings, check.Warnings...)
		}

		// Attach wisp if --on.
		if opts.OnFormula != "" {
			childVars := BuildSlingFormulaVars(opts.OnFormula, child.ID, opts.Vars, a, deps)
			cookResult, err := molecule.Cook(context.Background(), deps.Store, opts.OnFormula, SlingFormulaSearchPaths(deps, a), molecule.Options{
				Title:            opts.Title,
				Vars:             childVars,
				PriorityOverride: ClonePriorityPtr(child.Priority),
			})
			if err != nil {
				childResult.Failed = true
				childResult.FailReason = fmt.Sprintf("instantiating formula %q: %v", opts.OnFormula, err)
				batchResult.Children = append(batchResult.Children, childResult)
				telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
				failed++
				continue
			}
			if err := deps.Store.SetMetadata(child.ID, "molecule_id", cookResult.RootID); err != nil {
				batchResult.MetadataErrors = append(batchResult.MetadataErrors,
					fmt.Sprintf("setting molecule_id on %s: %v", child.ID, err))
			}
			childResult.WispRootID = cookResult.RootID
			childResult.FormulaName = opts.OnFormula
		} else if !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
			childVars := BuildSlingFormulaVars(a.EffectiveDefaultSlingFormula(), child.ID, opts.Vars, a, deps)
			cookResult, err := molecule.Cook(context.Background(), deps.Store, a.EffectiveDefaultSlingFormula(), SlingFormulaSearchPaths(deps, a), molecule.Options{
				Title:            opts.Title,
				Vars:             childVars,
				PriorityOverride: ClonePriorityPtr(child.Priority),
			})
			if err != nil {
				childResult.Failed = true
				childResult.FailReason = fmt.Sprintf("instantiating default formula %q: %v", a.EffectiveDefaultSlingFormula(), err)
				batchResult.Children = append(batchResult.Children, childResult)
				telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
				failed++
				continue
			}
			if err := deps.Store.SetMetadata(child.ID, "molecule_id", cookResult.RootID); err != nil {
				batchResult.MetadataErrors = append(batchResult.MetadataErrors,
					fmt.Sprintf("setting molecule_id on %s: %v", child.ID, err))
			}
			childResult.WispRootID = cookResult.RootID
			childResult.FormulaName = a.EffectiveDefaultSlingFormula()
		}

		childEnv := ResolveSlingEnv(a, deps)
		slingCmd := BuildSlingCommand(a.EffectiveSlingQuery(), child.ID)
		rigDir := SlingDirForBead(deps.Cfg, deps.CityPath, child.ID)
		if _, err := deps.Runner(rigDir, slingCmd, childEnv); err != nil {
			childResult.Failed = true
			childResult.FailReason = err.Error()
			batchResult.Children = append(batchResult.Children, childResult)
			telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
			failed++
			continue
		}

		telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, nil)
		childResult.Routed = true
		batchResult.Children = append(batchResult.Children, childResult)
		routed++
	}

	// Record skipped (non-open) children with their status.
	for _, child := range skipped {
		batchResult.Children = append(batchResult.Children, SlingChildResult{
			BeadID:  child.ID,
			Status:  child.Status,
			Skipped: true,
		})
	}

	batchResult.Routed = routed
	batchResult.Failed = failed
	batchResult.Skipped = idempotent + len(skipped)
	batchResult.IdempotentCt = idempotent

	if opts.Nudge && routed > 0 {
		batchResult.NudgeAgent = &a
	}

	if failed > 0 {
		return batchResult, fmt.Errorf("%d/%d children failed", failed, len(open))
	}
	return batchResult, nil
}
