package sling

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/telemetry"
)

func depsTracef(deps SlingDeps, format string, args ...any) {
	if deps.Tracer != nil {
		deps.Tracer(format, args...)
		return
	}
	SlingTracef(format, args...)
}

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

	if shouldValidateExistingBead(opts) {
		if err := validateExistingBead(opts.BeadOrFormula, deps); err != nil {
			return result, err
		}
	}
	if shouldGuardCrossRig(opts) {
		if err := CrossRigRouteError(opts.BeadOrFormula, a, deps.Cfg); err != nil {
			return result, err
		}
	}

	// Pre-flight idempotency check.
	if shouldCheckBeadState(opts) {
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

	// Reassign: clear any existing human assignee before routing so the
	// target pool/agent can claim the bead. Without this, beads claimed
	// by `bd update --claim` stay invisible to the pool's claim filter
	// even after sling sets gc.routed_to. See gastownhall/gascity#1007.
	if opts.Reassign && !opts.DryRun {
		if err := clearHumanAssignee(opts.BeadOrFormula, deps); err != nil {
			return result, fmt.Errorf("clearing assignee for %s: %w", opts.BeadOrFormula, err)
		}
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

func shouldValidateExistingBead(opts SlingOpts) bool {
	if opts.IsFormula || (opts.DryRun && opts.InlineText) {
		return false
	}
	return !opts.Force || usesFormulaBackedRoute(opts)
}

func usesFormulaBackedRoute(opts SlingOpts) bool {
	return opts.OnFormula != "" || (!opts.NoFormula && opts.Target.EffectiveDefaultSlingFormula() != "")
}

func shouldGuardCrossRig(opts SlingOpts) bool {
	return !opts.IsFormula && !opts.Force && !opts.DryRun
}

func shouldCheckBeadState(opts SlingOpts) bool {
	return !opts.IsFormula && !opts.Force && (!opts.DryRun || !opts.InlineText)
}

func validateExistingBead(beadID string, deps SlingDeps) error {
	querier := deps.ValidationQuerier
	if querier == nil {
		querier = deps.Store
	}
	return validateExistingBeadInQuerier(beadID, deps.StoreRef, querier)
}

func validateExistingBeadInQuerier(beadID, storeRef string, querier BeadQuerier) error {
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" {
		storeRef = "local"
	}
	if querier == nil {
		return &BeadLookupError{BeadID: beadID, StoreRef: storeRef, Err: errors.New("store not configured")}
	}
	exists, err := probeBeadInQuerier(querier, beadID)
	if err != nil {
		return &BeadLookupError{BeadID: beadID, StoreRef: storeRef, Err: err}
	}
	if exists {
		return nil
	}
	return &MissingBeadError{BeadID: beadID, StoreRef: storeRef}
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
		wfResult, wfErr := doStartGraphWorkflow(mResult.RootID, "", a, method, deps)
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
	formulaVars := BuildSlingFormulaVars(opts.OnFormula, beadID, opts.Vars, a, deps)
	searchPaths := SlingFormulaSearchPaths(deps, a)
	isGraph, err := isGraphSlingFormula(context.Background(), opts.OnFormula, searchPaths, formulaVars)
	if err != nil {
		return result, fmt.Errorf("instantiating formula %q on %s: %w", opts.OnFormula, beadID, err)
	}
	if err := validateSlingFormulaRuntimeVars(context.Background(), opts.OnFormula, searchPaths, molecule.Options{
		Title: opts.Title,
		Vars:  formulaVars,
	}); err != nil {
		return result, fmt.Errorf("instantiating formula %q on %s: %w", opts.OnFormula, beadID, err)
	}
	checkAttachments := CheckNoMoleculeChildren
	if isGraph && opts.Force {
		checkAttachments = CheckNoMoleculeChildrenAllowLiveWorkflow
	}
	if err := checkAttachments(querier, beadID, deps.Store, &result); err != nil {
		return result, fmt.Errorf("%w", err)
	}
	run := func() (SlingResult, error) {
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
			wfResult, wfErr := doStartGraphWorkflow(mResult.RootID, beadID, a, method, deps)
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
	runGraph := func() (pendingSourceWorkflowLaunch, error) {
		mResult, err := InstantiateSlingFormula(context.Background(), opts.OnFormula, SlingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             formulaVars,
			PriorityOverride: BeadPriorityOverride(querier, beadID),
		}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			return pendingSourceWorkflowLaunch{}, fmt.Errorf("instantiating formula %q on %s: %w", opts.OnFormula, beadID, err)
		}
		return pendingGraphWorkflowLaunch(mResult.RootID, beadID, a, method, opts.OnFormula, deps), nil
	}
	if !isGraph {
		return run()
	}
	return withSourceWorkflowLaunchLock(context.Background(), deps, beadID, opts.Force, runGraph)
}

// slingDefaultFormula handles the default formula attachment path.
func slingDefaultFormula(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID string, result SlingResult) (SlingResult, error) {
	a := opts.Target
	method := "default-on-formula"
	defaultFormula := a.EffectiveDefaultSlingFormula()
	defaultVars := BuildSlingFormulaVars(defaultFormula, beadID, opts.Vars, a, deps)
	searchPaths := SlingFormulaSearchPaths(deps, a)
	isGraph, err := isGraphSlingFormula(context.Background(), defaultFormula, searchPaths, defaultVars)
	if err != nil {
		return result, fmt.Errorf("instantiating default formula %q on %s: %w", defaultFormula, beadID, err)
	}
	if err := validateSlingFormulaRuntimeVars(context.Background(), defaultFormula, searchPaths, molecule.Options{
		Title: opts.Title,
		Vars:  defaultVars,
	}); err != nil {
		return result, fmt.Errorf("instantiating default formula %q on %s: %w", defaultFormula, beadID, err)
	}
	checkAttachments := CheckNoMoleculeChildren
	if isGraph && opts.Force {
		checkAttachments = CheckNoMoleculeChildrenAllowLiveWorkflow
	}
	if err := checkAttachments(querier, beadID, deps.Store, &result); err != nil {
		return result, fmt.Errorf("%w", err)
	}
	run := func() (SlingResult, error) {
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
			wfResult, wfErr := doStartGraphWorkflow(mResult.RootID, beadID, a, method, deps)
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
	runGraph := func() (pendingSourceWorkflowLaunch, error) {
		mResult, err := InstantiateSlingFormula(context.Background(), defaultFormula, SlingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             defaultVars,
			PriorityOverride: BeadPriorityOverride(querier, beadID),
		}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			return pendingSourceWorkflowLaunch{}, fmt.Errorf("instantiating default formula %q on %s: %w", defaultFormula, beadID, err)
		}
		return pendingGraphWorkflowLaunch(mResult.RootID, beadID, a, method, defaultFormula, deps), nil
	}
	if !isGraph {
		return run()
	}
	return withSourceWorkflowLaunchLock(context.Background(), deps, beadID, opts.Force, runGraph)
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
	slingEnv := ResolveSlingEnv(a, deps, beadID)
	rigDir := SlingDirForBead(deps.Cfg, deps.CityPath, beadID)
	if deps.Router != nil {
		req := RouteRequest{
			BeadID:  beadID,
			Target:  a.QualifiedName(),
			WorkDir: rigDir,
			Env:     slingEnv,
			Force:   opts.Force,
		}
		if err := deps.Router.Route(context.Background(), req); err != nil {
			telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), method, err)
			return result, fmt.Errorf("%w", err)
		}
	} else {
		slingCmd, slingWarn := BuildSlingCommandForAgent("sling_query", a.EffectiveSlingQuery(), beadID, deps.CityPath, deps.CityName, a, deps.Cfg.Rigs)
		if slingWarn != "" {
			depsTracef(deps, "sling-core: %s", slingWarn)
		}
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
		createAutoConvoy := true
		exists, err := ProbeBeadInStore(deps.Store, beadID)
		if err != nil {
			result.MetadataErrors = append(result.MetadataErrors,
				fmt.Sprintf("checking bead before auto-convoy: %v", err))
			createAutoConvoy = false
		} else if !exists {
			if opts.Force {
				result.MetadataErrors = append(result.MetadataErrors,
					fmt.Sprintf("forced dispatch skipped missing-bead validation for %s; no local auto-convoy created", beadID))
			} else {
				result.MetadataErrors = append(result.MetadataErrors,
					fmt.Sprintf("skipping auto-convoy: bead %s is not present in the local store", beadID))
			}
			createAutoConvoy = false
		}
		if createAutoConvoy {
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
func doStartGraphWorkflow(rootID, sourceBeadID string, a config.Agent, method string, deps SlingDeps) (SlingResult, error) {
	var result SlingResult
	result.Target = a.QualifiedName()
	result.Method = method
	result.WorkflowID = rootID
	result.BeadID = rootID

	SlingTracef("workflow-start begin root=%s source=%s agent=%s method=%s", rootID, sourceBeadID, a.QualifiedName(), method)

	if err := PromoteWorkflowLaunchBead(deps.Store, rootID); err != nil {
		return result, fmt.Errorf("setting workflow root %s in_progress: %w", rootID, err)
	}
	if sourceBeadID != "" {
		if err := deps.Store.SetMetadata(rootID, "gc.source_bead_id", sourceBeadID); err != nil {
			return result, fmt.Errorf("setting gc.source_bead_id on workflow %s: %w", rootID, err)
		}
		if sourceStoreRef := strings.TrimSpace(deps.StoreRef); sourceStoreRef != "" {
			if err := deps.Store.SetMetadata(rootID, sourceworkflow.SourceStoreRefMetadataKey, sourceStoreRef); err != nil {
				return result, fmt.Errorf("setting %s on workflow %s: %w", sourceworkflow.SourceStoreRefMetadataKey, rootID, err)
			}
		}
		// Graph workflow launches repoint the source bead at the active root so
		// witness/source lookups resume from the workflow currently in control.
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

type pendingSourceWorkflowLaunch struct {
	workflowID string
	storeRef   string
	finalize   func() (SlingResult, error)
	rollback   func() error
}

type sourceWorkflowRoot struct {
	root     beads.Bead
	store    beads.Store
	storeRef string
}

type workflowRestoreState struct {
	rootID    string
	store     beads.Store
	storeRef  string
	snapshots []sourceworkflow.WorkflowBeadSnapshot
}

func listSourceWorkflowRoots(deps SlingDeps, sourceBeadID string) ([]sourceWorkflowRoot, error) {
	sourceStoreRef := strings.TrimSpace(deps.StoreRef)
	if deps.SourceWorkflowStores == nil {
		roots, err := sourceworkflow.ListLiveRoots(deps.Store, sourceBeadID, sourceStoreRef, sourceStoreRef)
		if err != nil {
			return nil, err
		}
		out := make([]sourceWorkflowRoot, 0, len(roots))
		for _, root := range roots {
			out = append(out, sourceWorkflowRoot{
				root:     root,
				store:    deps.Store,
				storeRef: sourceStoreRef,
			})
		}
		return out, nil
	}
	stores, err := deps.SourceWorkflowStores()
	if err != nil {
		return nil, err
	}
	roots := make([]sourceWorkflowRoot, 0)
	seen := make(map[string]struct{}, len(stores))
	for i, info := range stores {
		if info.Store == nil {
			continue
		}
		rootStoreRef := strings.TrimSpace(info.StoreRef)
		matches, err := sourceworkflow.ListLiveRoots(info.Store, sourceBeadID, sourceStoreRef, rootStoreRef)
		if err != nil {
			return nil, err
		}
		for _, root := range matches {
			keyScope := rootStoreRef
			if keyScope == "" {
				keyScope = fmt.Sprintf("store#%d", i)
			}
			key := keyScope + "\x00" + root.ID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			roots = append(roots, sourceWorkflowRoot{
				root:     root,
				store:    info.Store,
				storeRef: rootStoreRef,
			})
		}
	}
	slices.SortFunc(roots, func(a, b sourceWorkflowRoot) int {
		if cmp := strings.Compare(a.storeRef, b.storeRef); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.root.ID, b.root.ID)
	})
	return roots, nil
}

func pendingGraphWorkflowLaunch(rootID, sourceBeadID string, a config.Agent, method, formulaName string, deps SlingDeps) pendingSourceWorkflowLaunch {
	return pendingSourceWorkflowLaunch{
		workflowID: rootID,
		storeRef:   strings.TrimSpace(deps.StoreRef),
		finalize: func() (SlingResult, error) {
			result, err := doStartGraphWorkflow(rootID, sourceBeadID, a, method, deps)
			result.FormulaName = formulaName
			return result, err
		},
		rollback: func() error {
			_, err := sourceworkflow.CloseWorkflowSubtree(deps.Store, rootID)
			return err
		},
	}
}

func sameWorkflowRoot(root sourceWorkflowRoot, workflowID, storeRef string) bool {
	return root.root.ID == strings.TrimSpace(workflowID) &&
		sourceworkflow.NormalizeSourceStoreRef(root.storeRef) == sourceworkflow.NormalizeSourceStoreRef(storeRef)
}

func blockingWorkflowIDs(roots []sourceWorkflowRoot) []string {
	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.root.ID == "" {
			continue
		}
		ids = append(ids, root.root.ID)
	}
	slices.Sort(ids)
	return ids
}

func snapshotBlockingWorkflowState(roots []sourceWorkflowRoot, replacement pendingSourceWorkflowLaunch) ([]workflowRestoreState, error) {
	states := make([]workflowRestoreState, 0, len(roots))
	for _, root := range roots {
		if root.root.ID == "" || sameWorkflowRoot(root, replacement.workflowID, replacement.storeRef) {
			continue
		}
		snapshots, err := sourceworkflow.SnapshotOpenWorkflowBeads(root.store, root.root.ID)
		if err != nil {
			return nil, err
		}
		states = append(states, workflowRestoreState{
			rootID:    root.root.ID,
			store:     root.store,
			storeRef:  root.storeRef,
			snapshots: snapshots,
		})
	}
	return states, nil
}

func restoreBlockingWorkflowState(states []workflowRestoreState) error {
	var restoreErr error
	for _, state := range states {
		if err := sourceworkflow.RestoreWorkflowBeads(state.store, state.snapshots); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore workflow %s in %s: %w", state.rootID, state.storeRef, err))
		}
	}
	return restoreErr
}

func rollbackSourceWorkflowReplacement(launch pendingSourceWorkflowLaunch, store beads.Store, sourceBeadID, previousWorkflowID string, states []workflowRestoreState) error {
	var rollbackErr error
	if launch.rollback != nil {
		if err := launch.rollback(); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback new workflow %s: %w", launch.workflowID, err))
		}
	}
	if sourceBeadID != "" {
		if err := store.SetMetadata(sourceBeadID, "workflow_id", previousWorkflowID); err != nil && !errors.Is(err, beads.ErrNotFound) {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore source workflow_id on %s: %w", sourceBeadID, err))
		}
	}
	if err := restoreBlockingWorkflowState(states); err != nil {
		rollbackErr = errors.Join(rollbackErr, err)
	}
	return rollbackErr
}

func withSourceWorkflowLaunchLock(ctx context.Context, deps SlingDeps, sourceBeadID string, force bool, fn func() (pendingSourceWorkflowLaunch, error)) (SlingResult, error) {
	sourceBeadID = sourceworkflow.NormalizeSourceBeadID(sourceBeadID)
	if sourceBeadID == "" {
		launch, err := fn()
		if err != nil {
			return SlingResult{}, err
		}
		return launch.finalize()
	}
	var result SlingResult
	err := sourceworkflow.WithLock(ctx, deps.CityPath, sourceWorkflowLockScope(deps), sourceBeadID, func() error {
		previousWorkflowID := ""
		sourceBead, err := deps.Store.Get(sourceBeadID)
		if err != nil && !errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("get source bead %s: %w", sourceBeadID, err)
		}
		if err == nil {
			previousWorkflowID = strings.TrimSpace(sourceBead.Metadata["workflow_id"])
		}
		roots, err := listSourceWorkflowRoots(deps, sourceBeadID)
		if err != nil {
			return fmt.Errorf("list live workflows for %s: %w", sourceBeadID, err)
		}
		blockingRoots := append([]sourceWorkflowRoot(nil), roots...)
		if len(roots) > 0 {
			if !force {
				return &sourceworkflow.ConflictError{
					SourceBeadID: sourceBeadID,
					WorkflowIDs:  blockingWorkflowIDs(roots),
				}
			}
		}
		launch, err := fn()
		if err != nil {
			return err
		}
		if launch.workflowID == "" {
			return fmt.Errorf("source workflow launch for %s returned empty workflow id", sourceBeadID)
		}
		restoreState, err := snapshotBlockingWorkflowState(blockingRoots, launch)
		if err != nil {
			if rollbackErr := rollbackSourceWorkflowReplacement(launch, deps.Store, sourceBeadID, previousWorkflowID, nil); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
		if force {
			for _, root := range blockingRoots {
				if root.root.ID == "" || sameWorkflowRoot(root, launch.workflowID, launch.storeRef) {
					continue
				}
				if _, err := sourceworkflow.CloseWorkflowSubtree(root.store, root.root.ID); err != nil {
					if rollbackErr := rollbackSourceWorkflowReplacement(launch, deps.Store, sourceBeadID, previousWorkflowID, restoreState); rollbackErr != nil {
						return errors.Join(fmt.Errorf("close superseded workflow %s for %s: %w", root.root.ID, sourceBeadID, err), rollbackErr)
					}
					return fmt.Errorf("close superseded workflow %s for %s: %w", root.root.ID, sourceBeadID, err)
				}
			}
		}
		result, err = launch.finalize()
		if err != nil {
			if rollbackErr := rollbackSourceWorkflowReplacement(launch, deps.Store, sourceBeadID, previousWorkflowID, restoreState); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
		roots, err = listSourceWorkflowRoots(deps, sourceBeadID)
		if err != nil {
			// A transient store error while re-listing is recoverable:
			// the finalize already succeeded, the lock is still held, and
			// the underlying stores may briefly be unavailable. Warn and
			// continue.
			result.MetadataErrors = append(result.MetadataErrors,
				fmt.Sprintf("verify live workflows for %s: %v", sourceBeadID, err))
			return nil
		}
		if !slices.ContainsFunc(roots, func(root sourceWorkflowRoot) bool {
			return sameWorkflowRoot(root, result.WorkflowID, launch.storeRef)
		}) {
			// Under the held lock, a successful finalize that is not
			// visible via ListLiveRoots is an invariant violation: either
			// the new root was never persisted or it no longer matches
			// the singleton predicate. This must NOT be demoted to a
			// warning — callers that rely on exactly-one-live-root will
			// otherwise proceed with a phantom success. Run the same
			// rollback the finalize-failure path uses so superseded
			// roots are restored and the source bead's workflow_id is
			// reverted to previousWorkflowID; otherwise we leave the
			// system in a worse state than the one the invariant check
			// was supposed to catch.
			invariantErr := fmt.Errorf("workflow %s not visible for source bead %s after launch", result.WorkflowID, sourceBeadID)
			if rollbackErr := rollbackSourceWorkflowReplacement(launch, deps.Store, sourceBeadID, previousWorkflowID, restoreState); rollbackErr != nil {
				return errors.Join(invariantErr, rollbackErr)
			}
			return invariantErr
		}
		return nil
	})
	return result, err
}

// attachBatchFormula launches one batch-child formula. The caller passes the
// pre-computed isGraph flag from the one-shot formula compile at the top of
// DoSlingBatch so that compiling N times for N children becomes a single
// compile per batch.
func attachBatchFormula(ctx context.Context, opts SlingOpts, deps SlingDeps, child beads.Bead, a config.Agent, formulaName, formulaLabel, method string, isGraph bool) (SlingResult, error) {
	childVars := BuildSlingFormulaVars(formulaName, child.ID, opts.Vars, a, deps)
	run := func() (SlingResult, error) {
		mResult, err := InstantiateSlingFormula(ctx, formulaName, SlingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             childVars,
			PriorityOverride: ClonePriorityPtr(child.Priority),
		}, child.ID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			return SlingResult{}, fmt.Errorf("instantiating %s %q on %s: %w", formulaLabel, formulaName, child.ID, err)
		}
		if mResult.GraphWorkflow || IsGraphWorkflowAttachment(deps.Store, mResult.RootID) {
			wfResult, wfErr := doStartGraphWorkflow(mResult.RootID, child.ID, a, method, deps)
			wfResult.FormulaName = formulaName
			return wfResult, wfErr
		}
		result := SlingResult{
			BeadID:      child.ID,
			Target:      a.QualifiedName(),
			Method:      method,
			WispRootID:  mResult.RootID,
			FormulaName: formulaName,
		}
		if err := deps.Store.SetMetadata(child.ID, "molecule_id", mResult.RootID); err != nil {
			result.MetadataErrors = append(result.MetadataErrors,
				fmt.Sprintf("setting molecule_id on %s: %v", child.ID, err))
		}
		return result, nil
	}
	runGraph := func() (pendingSourceWorkflowLaunch, error) {
		mResult, err := InstantiateSlingFormula(ctx, formulaName, SlingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             childVars,
			PriorityOverride: ClonePriorityPtr(child.Priority),
		}, child.ID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			return pendingSourceWorkflowLaunch{}, fmt.Errorf("instantiating %s %q on %s: %w", formulaLabel, formulaName, child.ID, err)
		}
		return pendingGraphWorkflowLaunch(mResult.RootID, child.ID, a, method, formulaName, deps), nil
	}
	if !isGraph {
		return run()
	}
	return withSourceWorkflowLaunchLock(ctx, deps, child.ID, opts.Force, runGraph)
}

func isGraphSlingFormula(ctx context.Context, formulaName string, searchPaths []string, vars map[string]string) (bool, error) {
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, vars)
	if err != nil {
		return false, err
	}
	return IsCompiledGraphWorkflow(recipe), nil
}

func validateSlingFormulaRuntimeVars(ctx context.Context, formulaName string, searchPaths []string, opts molecule.Options) error {
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, opts.Vars)
	if err != nil {
		return err
	}
	return molecule.ValidateRecipeRuntimeVars(recipe, opts)
}

func validateBatchSlingFormulaRuntimeVars(ctx context.Context, formulaName string, searchPaths []string, opts SlingOpts, open []beads.Bead, a config.Agent, deps SlingDeps) error {
	for _, child := range open {
		childVars := BuildSlingFormulaVars(formulaName, child.ID, opts.Vars, a, deps)
		if err := validateSlingFormulaRuntimeVars(ctx, formulaName, searchPaths, molecule.Options{
			Title: opts.Title,
			Vars:  childVars,
		}); err != nil {
			return fmt.Errorf("child %s: %w", child.ID, err)
		}
	}
	return nil
}

func sourceWorkflowLockScope(deps SlingDeps) string {
	return sourceworkflow.LockScopeForStoreRef(deps.CityPath, "", deps.StoreRef, func(rigName string) (string, bool) {
		if deps.Cfg != nil {
			for _, rig := range deps.Cfg.Rigs {
				if rig.Name != rigName {
					continue
				}
				return rig.Path, true
			}
		}
		return "", false
	})
}

// DoSlingBatch handles convoy expansion before delegating to DoSling.
func DoSlingBatch(opts SlingOpts, deps SlingDeps, querier BeadChildQuerier) (SlingResult, error) {
	a := opts.Target

	// Formula mode, nil querier → delegate directly.
	if opts.IsFormula || querier == nil {
		return DoSling(opts, deps, querier)
	}

	containerQuerier := BeadQuerier(querier)
	b, err := querier.Get(opts.BeadOrFormula)
	if err != nil {
		if !errors.Is(err, beads.ErrNotFound) {
			return SlingResult{Target: a.QualifiedName()}, &BeadLookupError{
				BeadID:   opts.BeadOrFormula,
				StoreRef: deps.StoreRef,
				Err:      err,
			}
		}
		if selected, ok := selectedStoreContainer(opts, deps); ok {
			b = selected
			// The caller's querier could not see the container, so deps.Store
			// becomes authoritative for both validation and child expansion.
			querier = deps.Store
			containerQuerier = deps.Store
		} else {
			singleOpts := opts
			singleOpts.IsFormula = false
			return DoSling(singleOpts, deps, querier)
		}
	}
	if b.Type == "epic" || beads.IsContainerType(b.Type) {
		if shouldValidateExistingBead(opts) {
			if err := validateExistingBeadInQuerier(opts.BeadOrFormula, deps.StoreRef, containerQuerier); err != nil {
				return SlingResult{Target: a.QualifiedName()}, err
			}
		}
	}
	if b.Type == "epic" {
		return SlingResult{}, fmt.Errorf("bead %s is an epic; first-class support is for convoys only", b.ID)
	}

	if !beads.IsContainerType(b.Type) {
		singleOpts := opts
		singleOpts.IsFormula = false
		singleDeps := deps
		singleDeps.ValidationQuerier = containerQuerier
		return DoSling(singleOpts, singleDeps, querier)
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
		if err := CrossRigRouteError(b.ID, a, deps.Cfg); err != nil {
			return SlingResult{}, err
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
	// isGraph is computed once per batch and threaded into every per-child
	// attachBatchFormula call. Previously the helper compiled the formula
	// once here and again per child, turning an O(1) compile into O(N) disk
	// reads + template expansions for an N-child batch.
	var isGraph bool
	if useFormula != "" {
		formulaVars := BuildSlingFormulaVars(useFormula, "", opts.Vars, a, deps)
		searchPaths := SlingFormulaSearchPaths(deps, a)
		var err error
		isGraph, err = isGraphSlingFormula(context.Background(), useFormula, searchPaths, formulaVars)
		if err != nil {
			return SlingResult{}, fmt.Errorf("instantiating formula %q on %s %s: %w", useFormula, b.Type, b.ID, err)
		}
		if err := validateBatchSlingFormulaRuntimeVars(context.Background(), useFormula, searchPaths, opts, open, a, deps); err != nil {
			return SlingResult{}, fmt.Errorf("instantiating formula %q on %s %s: %w", useFormula, b.Type, b.ID, err)
		}
		checkAttachments := CheckBatchNoMoleculeChildren
		if isGraph && opts.Force {
			checkAttachments = CheckBatchNoMoleculeChildrenAllowLiveWorkflow
		}
		if err := checkAttachments(querier, open, deps.Store, &batchResult); err != nil {
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
	// childErrors preserves typed child errors so errors.As at the top-level
	// (cmdSling) can recover a *sourceworkflow.ConflictError emitted by any
	// child and map it to exit code 3 + the cleanup hint. Stringifying into
	// childResult.FailReason alone loses the type.
	var childErrors []error
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

		if useFormula != "" {
			formulaLabel := "formula"
			if opts.OnFormula == "" {
				formulaLabel = "default formula"
			}
			formulaResult, err := attachBatchFormula(context.Background(), opts, deps, child, a, useFormula, formulaLabel, batchMethod, isGraph)
			if err != nil {
				childResult.Failed = true
				childResult.FailReason = err.Error()
				batchResult.Children = append(batchResult.Children, childResult)
				childErrors = append(childErrors, err)
				telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
				failed++
				continue
			}
			batchResult.MetadataErrors = append(batchResult.MetadataErrors, formulaResult.MetadataErrors...)
			childResult.FormulaName = formulaResult.FormulaName
			childResult.WorkflowID = formulaResult.WorkflowID
			childResult.WispRootID = formulaResult.WispRootID
			if formulaResult.WorkflowID != "" {
				childResult.Routed = true
				batchResult.Children = append(batchResult.Children, childResult)
				routed++
				continue
			}
		}

		childEnv := ResolveSlingEnvForBead(a, deps, child)
		rigDir := SlingDirForBead(deps.Cfg, deps.CityPath, child.ID)
		if deps.Router != nil {
			req := RouteRequest{
				BeadID:  child.ID,
				Target:  a.QualifiedName(),
				WorkDir: rigDir,
				Env:     childEnv,
				Force:   opts.Force,
			}
			if err := deps.Router.Route(context.Background(), req); err != nil {
				childResult.Failed = true
				childResult.FailReason = err.Error()
				batchResult.Children = append(batchResult.Children, childResult)
				childErrors = append(childErrors, err)
				telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
				failed++
				continue
			}
		} else {
			slingCmd, slingWarn := BuildSlingCommandForAgent("sling_query", a.EffectiveSlingQuery(), child.ID, deps.CityPath, deps.CityName, a, deps.Cfg.Rigs)
			if slingWarn != "" {
				depsTracef(deps, "sling-core: %s", slingWarn)
			}
			if _, err := deps.Runner(rigDir, slingCmd, childEnv); err != nil {
				childResult.Failed = true
				childResult.FailReason = err.Error()
				batchResult.Children = append(batchResult.Children, childResult)
				childErrors = append(childErrors, err)
				telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
				failed++
				continue
			}
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
		summary := fmt.Errorf("%d/%d children failed", failed, len(open))
		// errors.Join threads typed child errors through Unwrap() []error so
		// errors.As at the CLI/API boundary can recover *ConflictError and map
		// it to exit 3 + the cleanup hint; the summary stays first for the
		// human-readable message.
		joined := append([]error{summary}, childErrors...)
		return batchResult, errors.Join(joined...)
	}
	return batchResult, nil
}

func selectedStoreContainer(opts SlingOpts, deps SlingDeps) (beads.Bead, bool) {
	if deps.Store == nil {
		return beads.Bead{}, false
	}
	b, err := deps.Store.Get(opts.BeadOrFormula)
	if err != nil {
		return beads.Bead{}, false
	}
	return b, b.Type == "epic" || beads.IsContainerType(b.Type)
}

// clearHumanAssignee unsets the bead's assignee if non-empty. No-op when
// the bead is missing, the assignee is already empty, or the store is
// unavailable. Errors only on a real store-Update failure with a
// non-empty assignee. See SlingOpts.Reassign and #1007.
func clearHumanAssignee(beadID string, deps SlingDeps) error {
	if deps.Store == nil {
		return nil
	}
	b, err := deps.Store.Get(beadID)
	if err != nil {
		// Bead may not exist locally yet (e.g. with --force routing
		// against an absent bead). Nothing to clear.
		return nil
	}
	if strings.TrimSpace(b.Assignee) == "" {
		return nil
	}
	empty := ""
	return deps.Store.Update(beadID, beads.UpdateOpts{Assignee: &empty})
}
