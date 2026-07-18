package sling

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/telemetry"
)

const (
	// Dolt-backed stores can briefly lag across connections after graph root
	// creation. Keep the verification retry bounded while covering the common
	// sub-second read-after-write delay observed by workflow launch paths.
	sourceWorkflowLaunchVisibilityAttempts   = 5
	sourceWorkflowLaunchVisibilityRetryDelay = 100 * time.Millisecond
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
	if !opts.Force && rigSuspended(deps.Cfg, a.Dir) {
		result.SuspendedRig = a.Dir
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

	// Dependency cycle check: reject slings that would create a deadlock.
	if shouldCheckDepCycle(opts) {
		if err := DetectCycle(opts.BeadOrFormula, deps.Store); err != nil {
			return result, err
		}
	}

	// Pre-flight idempotency check.
	if shouldCheckBeadState(opts) {
		if resolveIdempotentShortCircuit(opts, a, deps, querier, &result) {
			return result, nil
		}
	}
	if shouldValidateBuiltInRouteStoreReachable(opts, deps) {
		if err := validateBuiltInRouteStoreReachable(deps, opts.BeadOrFormula, a); err != nil {
			return result, fmt.Errorf("%w", err)
		}
	}

	// Reassign: make the bead claimable by the target pool/agent before
	// routing — clear any existing assignee and reopen it if a prior actor
	// left it in_progress. Without this, a bead claimed by `bd update --claim`
	// (status=in_progress, assignee=<actor>) stays invisible to the pool's
	// claim filter even after sling sets gc.routed_to: clearing the assignee
	// alone is not enough because IsReadyCandidate requires status=open. See
	// gastownhall/gascity#1007 (assignee) and #3231 (status).
	if shouldReopenForReassign(opts) {
		if err := reopenForReassign(opts.BeadOrFormula, deps); err != nil {
			return result, fmt.Errorf("reopening %s for reassign: %w", opts.BeadOrFormula, err)
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

// resolveIdempotentShortCircuit runs the plain-bead pre-flight idempotency
// check and reports whether the sling is a settled no-op. When it returns true,
// result is populated for an early idempotent return; otherwise any bead-state
// warnings are appended to result and the sling proceeds. An explicit --on
// formula on a routed-but-unmoleculed root is not treated as idempotent, so the
// formula still attaches. If the molecule-attachment probe cannot complete, the
// fail-closed idempotent state is preserved and the probe failure is surfaced
// as a bead warning rather than silently flipping into a mutating attach path.
func resolveIdempotentShortCircuit(opts SlingOpts, a config.Agent, deps SlingDeps, querier BeadQuerier, result *SlingResult) bool {
	check := CheckBeadStateWithOptions(querier, opts.BeadOrFormula, a, deps, BeadCheckOptions{
		NoConvoy: opts.NoConvoy,
	})
	if check.Idempotent {
		needsAttach, probeErr := onFormulaNeedsAttachment(opts, querier, deps)
		switch {
		case probeErr != nil:
			// The attachment probe failed, so we cannot prove the routed bead
			// lacks a live molecule. Preserve the fail-closed idempotent result
			// instead of risking a duplicate attachment, and surface the probe
			// failure so it is not silently swallowed.
			result.BeadWarnings = append(result.BeadWarnings, fmt.Sprintf(
				"could not verify molecule attachment for %s; treating --on as an idempotent no-op: %v",
				opts.BeadOrFormula, probeErr))
		case needsAttach:
			// The bead is routed to the target but carries no molecule — an
			// earlier plain sling routed it raw. Do not treat --on as an
			// idempotent no-op; fall through so the formula attaches.
			check.Idempotent = false
		}
	}
	if !check.Idempotent {
		result.BeadWarnings = append(result.BeadWarnings, check.Warnings...)
		return false
	}
	result.Idempotent = true
	result.DryRun = opts.DryRun
	result.BeadID = opts.BeadOrFormula
	result.Method = "bead"
	// Honor --nudge even when the route is already in place. The bead is routed
	// to the target, but a warm pool slot may have missed its wake (its startup
	// nudge was swallowed, or work was routed after it went idle). Re-slinging
	// with --nudge must still deliver a wake; otherwise the idempotent
	// short-circuit silently drops it and the slot sits idle on work it never
	// began. The claim path is idempotent/CAS-safe, so a redundant nudge is
	// harmless. Suppressed for dry-run, which must not mutate or signal anything.
	if opts.Nudge && !opts.DryRun {
		result.NudgeAgent = &a
	}
	return true
}

// rigSuspended reports whether the named rig is marked suspended in config.
// The pool reconciler skips suspended rigs entirely, so a bead routed into
// one stalls silently — no worker ever spawns to claim it.
func rigSuspended(cfg *config.City, rigName string) bool {
	if cfg == nil || rigName == "" {
		return false
	}
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
			return r.Suspended
		}
	}
	return false
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

func shouldCheckDepCycle(opts SlingOpts) bool {
	// Only meaningful for plain-bead slinging where a bead ID is known.
	// Formula slinging creates new molecules whose deps aren't bead-graph deps.
	// Force and dry-run bypass cycle detection intentionally.
	return !opts.IsFormula && opts.OnFormula == "" && !opts.Force && !opts.DryRun && !opts.InlineText
}

func shouldGuardCrossRig(opts SlingOpts) bool {
	return !opts.IsFormula && !opts.Force && !opts.DryRun
}

func shouldCheckBeadState(opts SlingOpts) bool {
	return !opts.IsFormula && !opts.Force && (!opts.DryRun || !opts.InlineText)
}

// onFormulaNeedsAttachment reports whether this is an --on sling whose target
// bead the caller has already determined reads Idempotent (gc.routed_to ==
// target, or pool-labeled) but that has no attached molecule yet. The
// routed-idempotency check treats such a bead as a done no-op, but a bead can be
// routed raw by an earlier plain sling; a later `--on <formula>` must still
// attach the formula, or the repair root sits routed-but-unfanned. When a
// molecule is already attached, --on stays idempotent (skip), and re-attach is
// handled by the attachment path (CheckNoMoleculeChildren errors on a live
// molecule; a stale one is burned).
//
// The returned error is non-nil only when the molecule-attachment probe could
// not complete. In that case the result is (false, err): the caller cannot
// prove the bead is unmoleculed, so it must preserve the fail-closed idempotent
// state rather than clear it and risk minting a duplicate attachment.
func onFormulaNeedsAttachment(opts SlingOpts, querier BeadQuerier, deps SlingDeps) (bool, error) {
	if opts.OnFormula == "" {
		return false, nil
	}
	hasMolecule, err := HasMoleculeChildren(querier, opts.BeadOrFormula, deps.Store)
	if err != nil {
		return false, err
	}
	if hasMolecule {
		return false, nil
	}
	// No molecule attached. Only override idempotency for an UNCLAIMED bead — the
	// routed-raw footgun (gc.routed_to set, no assignee, no molecule). If a worker
	// has already claimed it (assignee set), leave it idempotent rather than
	// re-attaching a formula onto work in progress.
	bead, ok := BeadFromGetters(opts.BeadOrFormula, querier, deps.Store)
	if !ok {
		return false, nil
	}
	return strings.TrimSpace(bead.Assignee) == "", nil
}

func shouldValidateBuiltInRouteStoreReachable(opts SlingOpts, deps SlingDeps) bool {
	return deps.Router != nil && !opts.IsFormula && !opts.DryRun
}

// shouldReopenForReassign reports whether the pre-flight reassign reopen should
// run. Reassign reopens opts.BeadOrFormula, so it is only meaningful when that
// value is a real bead ID: a plain-bead route or an --on-formula attach, both
// !IsFormula. A standalone formula launch sets BeadOrFormula to the formula
// NAME, so reopening it would clear/reopen an unrelated bead that happens to
// share the name, or fail the launch on a formula-name store lookup — hence the
// !IsFormula guard, mirroring the auto-convoy block. Dry-run never mutates.
func shouldReopenForReassign(opts SlingOpts) bool {
	return opts.Reassign && !opts.IsFormula && !opts.DryRun
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
	searchPaths := SlingFormulaSearchPaths(deps, a)
	inv, isGraph, err := prepareGraphV2FormulaInvocation(context.Background(), opts.BeadOrFormula, "", opts, deps, a)
	if err != nil {
		return SlingResult{Target: a.QualifiedName()}, fmt.Errorf("instantiating formula %q: %w", opts.BeadOrFormula, err)
	}
	formulaVars := BuildSlingFormulaVars(opts.BeadOrFormula, "", opts.Vars, a, deps)
	if isGraph {
		formulaVars = inv.Vars
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), opts.BeadOrFormula, searchPaths, formulaVars)
	if err != nil {
		return SlingResult{Target: a.QualifiedName()}, fmt.Errorf("instantiating formula %q: %w", opts.BeadOrFormula, err)
	}
	if a.SupportsMultipleSessions() && !formula.RecipeHasReadySurface(recipe) {
		return SlingResult{Target: a.QualifiedName(), FormulaName: opts.BeadOrFormula, Deprecations: inv.Deprecations}, fmt.Errorf("formula %q root is a molecule container, not Ready-visible work; scale-from-zero pools will not wake for this wisp. Convert the formula to phase=\"vapor\"/root-only or formulas v2 before routing it to a pool", opts.BeadOrFormula)
	}
	// Compile-once (S14): the recipe compiled above for the ready-surface check
	// is the same one instantiated here — no redundant disk compile, and the
	// isGraph/routing decision cannot drift from what is materialized.
	mResult, err := InstantiateCompiledSlingFormula(context.Background(), recipe, opts.BeadOrFormula, molecule.Options{
		Title: opts.Title,
		Vars:  formulaVars,
	}, "", opts.ScopeKind, opts.ScopeRef, a, deps, opts.Force)
	if err != nil {
		return SlingResult{Target: a.QualifiedName()}, fmt.Errorf("instantiating formula %q: %w", opts.BeadOrFormula, err)
	}
	if mResult.GraphWorkflow || IsGraphWorkflowAttachment(deps.Store, mResult.RootID) {
		wfResult, wfErr := doStartGraphWorkflow(mResult.RootID, "", a, method, deps)
		wfResult.FormulaName = opts.BeadOrFormula
		wfResult.Deprecations = append(wfResult.Deprecations, inv.Deprecations...)
		return wfResult, wfErr
	}
	result := SlingResult{Target: a.QualifiedName(), FormulaName: opts.BeadOrFormula, Deprecations: inv.Deprecations}
	if hint := rootOnlyVaporPourHint(opts.BeadOrFormula, recipe); hint != "" {
		result.BeadWarnings = append(result.BeadWarnings, hint)
	}
	return finalize(opts, deps, mResult.RootID, method, result)
}

// rootOnlyVaporPourHint returns a sling-time diagnostic when a formula compiled
// to a root-only wisp specifically because it is a vapor formula without
// pour = true (cause (a) of the compile.go rootOnly rule). It deliberately stays
// silent for the genuinely step-less formula (cause (b), len(steps) == 0): there
// is no pour override to suggest there, so conflating the two would mislead. The
// hint surfaces via SlingResult.BeadWarnings; it changes neither routing nor the
// materialized wisp.
func rootOnlyVaporPourHint(formulaName string, recipe *formula.Recipe) string {
	if recipe == nil || !recipe.RootOnly || recipe.Pour || recipe.Phase != "vapor" {
		return ""
	}
	return fmt.Sprintf("note: %q is a vapor formula without `pour = true`; only the root step was materialized. Add `pour = true` for eager child-step expansion (see internal/formula/compile.go rootOnly rule).", formulaName)
}

// slingOnFormula handles the --on formula attachment path.
func slingOnFormula(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID string, result SlingResult) (SlingResult, error) {
	return attachFormulaToBead(opts, deps, querier, beadID, opts.OnFormula, "on-formula", "formula", result)
}

// slingDefaultFormula handles the default formula attachment path.
func slingDefaultFormula(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID string, result SlingResult) (SlingResult, error) {
	return attachFormulaToBead(opts, deps, querier, beadID, opts.Target.EffectiveDefaultSlingFormula(), "default-on-formula", "default formula", result)
}

// attachFormulaToBead runs the shared formula-attachment pipeline for both the
// --on-formula and default-formula paths: prepare the graph invocation,
// validate runtime vars, then either drive the graph-v2 branch
// (lock -> snapshot -> instantiate -> start -> rollback) or the legacy branch
// (check attachments -> instantiate -> set molecule_id -> finalize). The
// caller supplies the formula name, the sling method, and the error-label
// prefix ("formula" vs "default formula"); graph-vs-legacy behavior is
// byte-identical across both entry points.
func attachFormulaToBead(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID, formulaName, method, errLabel string, result SlingResult) (SlingResult, error) {
	a := opts.Target
	formulaVars := BuildSlingFormulaVars(formulaName, beadID, opts.Vars, a, deps)
	searchPaths := SlingFormulaSearchPaths(deps, a)
	graphInv, isGraph, err := prepareGraphV2FormulaInvocation(context.Background(), formulaName, beadID, opts, deps, a)
	if err != nil {
		return result, fmt.Errorf("instantiating %s %q on %s: %w", errLabel, formulaName, beadID, err)
	}
	if isGraph {
		formulaVars = graphInv.Vars
		result.Deprecations = append(result.Deprecations, graphInv.Deprecations...)
		if err := validateSlingFormulaRuntimeVars(context.Background(), formulaName, searchPaths, molecule.Options{
			Title: opts.Title,
			Vars:  formulaVars,
		}); err != nil {
			return result, fmt.Errorf("instantiating %s %q on %s: %w", errLabel, formulaName, beadID, err)
		}
		return withGraphV2SourceWorkflowLock(context.Background(), deps, beadID, func() (SlingResult, error) {
			if err := CheckNoMoleculeChildrenAllowLiveWorkflow(querier, beadID, deps.Store, &result); err != nil {
				return result, fmt.Errorf("%w", err)
			}
			if err := checkLegacySourceWorkflowConflict(deps, beadID); err != nil {
				return result, fmt.Errorf("%w", err)
			}
			replacedSnapshot, err := snapshotGraphV2ReplacementRoot(deps.Store, formulaName, formulaVars, opts.ScopeKind, opts.ScopeRef, opts.Force)
			if err != nil {
				return result, fmt.Errorf("instantiating %s %q on %s: %w", errLabel, formulaName, beadID, err)
			}
			mResult, err := InstantiateSlingFormula(context.Background(), formulaName, searchPaths, molecule.Options{
				Title:            opts.Title,
				Vars:             formulaVars,
				PriorityOverride: BeadPriorityOverride(deps.Store, graphInv.InputConvoy),
			}, "", opts.ScopeKind, opts.ScopeRef, a, deps, opts.Force)
			if err != nil {
				return result, fmt.Errorf("instantiating %s %q on %s: %w", errLabel, formulaName, beadID, err)
			}
			wfResult, wfErr := doStartGraphWorkflow(mResult.RootID, "", a, method, deps)
			wfResult.FormulaName = formulaName
			if wfErr != nil {
				if rollbackErr := rollbackGraphV2ReplacementLaunch(deps.Store, mResult.RootID, replacedSnapshot); rollbackErr != nil {
					return wfResult, errors.Join(wfErr, rollbackErr)
				}
			}
			return wfResult, wfErr
		})
	}
	if err := validateSlingFormulaRuntimeVars(context.Background(), formulaName, searchPaths, molecule.Options{
		Title: opts.Title,
		Vars:  formulaVars,
	}); err != nil {
		return result, fmt.Errorf("instantiating %s %q on %s: %w", errLabel, formulaName, beadID, err)
	}
	// The graph path returned above, so this is the legacy (non-graph) region:
	// isGraph is always false here, so the former `isGraph && opts.Force`
	// live-workflow allowance could never fire. Attachments are always checked
	// with CheckNoMoleculeChildren on this path.
	if err := CheckNoMoleculeChildren(querier, beadID, deps.Store, &result); err != nil {
		return result, fmt.Errorf("%w", err)
	}
	run := func() (SlingResult, error) {
		mResult, err := InstantiateSlingFormula(context.Background(), formulaName, SlingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             formulaVars,
			PriorityOverride: BeadPriorityOverride(querier, beadID),
		}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			return result, fmt.Errorf("instantiating %s %q on %s: %w", errLabel, formulaName, beadID, err)
		}
		wispRootID := mResult.RootID
		if mResult.GraphWorkflow || IsGraphWorkflowAttachment(deps.Store, wispRootID) {
			wfResult, wfErr := doStartGraphWorkflow(mResult.RootID, beadID, a, method, deps)
			wfResult.FormulaName = formulaName
			return wfResult, wfErr
		}
		if err := deps.Store.SetMetadata(beadID, beadmeta.MoleculeIDMetadataKey, wispRootID); err != nil {
			result.MetadataErrors = append(result.MetadataErrors,
				fmt.Sprintf("setting molecule_id on %s: %v", beadID, err))
		}
		result.WispRootID = wispRootID
		result.FormulaName = formulaName
		// Route the SOURCE bead, not wispRootID. An attached wisp (--on
		// <formula> or default formula) is driven through its source bead: the
		// source carries gc.routed_to + molecule_id and is the claimable unit
		// of work, while the wisp root is deliberately left unrouted (and, when
		// root-only, privatized out of Ready() by privatizeAttachedRootOnlyWisp).
		// ApplyGraphRouting likewise stamps no routing on an attached recipe
		// (graphroute: sourceBeadID != "" early-return). This is the
		// intentional counterpart to slingFormula, which routes the standalone
		// wisp root. Do not "fix" this to wispRootID — it would orphan the
		// work. See gastownhall/gascity#2848 and TestOnFormulaAttachesAndRoutes.
		return finalize(opts, deps, beadID, method, result)
	}
	runGraph := func() (pendingSourceWorkflowLaunch, error) {
		mResult, err := InstantiateSlingFormula(context.Background(), formulaName, SlingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             formulaVars,
			PriorityOverride: BeadPriorityOverride(querier, beadID),
		}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			return pendingSourceWorkflowLaunch{}, fmt.Errorf("instantiating %s %q on %s: %w", errLabel, formulaName, beadID, err)
		}
		return pendingGraphWorkflowLaunch(mResult.RootID, beadID, a, method, formulaName, deps), nil
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
		if err := validateBuiltInRouteStoreReachable(deps, beadID, a); err != nil {
			telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), method, err)
			return result, fmt.Errorf("%w", err)
		}
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
		if err := deps.Store.SetMetadata(beadID, beadmeta.MergeStrategyMetadataKey, opts.Merge); err != nil {
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
				// Use a "tracks" dep (convoy → bead) instead of parent-child
				// so the bead's existing parent (e.g. its epic) is preserved.
				// bd update --parent evicts any prior parent-child edge; the
				// tracks dep is additive and does not disturb the epic
				// rollup.
				if err := convoycore.TrackItem(deps.Store, convoy.ID, beadID); err != nil {
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

func validateBuiltInRouteStoreReachable(deps SlingDeps, beadID string, a config.Agent) error {
	if deps.Cfg == nil || IsCustomSlingQuery(a) {
		return nil
	}
	if agentutil.AgentReachesWorkflowStore(deps.StoreRef, &a, deps.CityPath, deps.Cfg) {
		return nil
	}
	return &CrossStoreRouteError{
		BeadID:            beadID,
		StoreRef:          deps.StoreRef,
		Target:            a.QualifiedName(),
		ReachableStoreRef: agentutil.AgentReachableStoreLabel(&a, deps.CityPath, deps.CityName, deps.Cfg),
	}
}

// doStartGraphWorkflow performs post-instantiation graph workflow setup.
func doStartGraphWorkflow(rootID, sourceBeadID string, a config.Agent, method string, deps SlingDeps) (SlingResult, error) {
	var result SlingResult
	result.Target = a.QualifiedName()
	result.Method = method
	result.WorkflowID = rootID
	result.BeadID = rootID

	SlingTracef("workflow-start begin root=%s source=%s agent=%s method=%s", rootID, sourceBeadID, a.QualifiedName(), method)

	// The workflow root and its graph-routing metadata live in the graph store;
	// the source bead it was launched from stays in the work store (deps.Store).
	graphStore := deps.graphStore()
	if err := PromoteWorkflowLaunchBead(graphStore, rootID); err != nil {
		return result, fmt.Errorf("setting workflow root %s in_progress: %w", rootID, err)
	}
	if sourceBeadID != "" {
		if err := graphStore.SetMetadata(rootID, beadmeta.SourceBeadIDMetadataKey, sourceBeadID); err != nil {
			return result, fmt.Errorf("setting gc.source_bead_id on workflow %s: %w", rootID, err)
		}
		if sourceStoreRef := strings.TrimSpace(deps.StoreRef); sourceStoreRef != "" {
			if err := graphStore.SetMetadata(rootID, sourceworkflow.SourceStoreRefMetadataKey, sourceStoreRef); err != nil {
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
		if len(blockingRoots) == 0 && previousWorkflowID != "" {
			root, ok, reason, err := sourceWorkflowRootByID(deps, sourceBeadID, previousWorkflowID, deps.StoreRef)
			if err != nil {
				return fmt.Errorf("get previous workflow %s for %s: %w", previousWorkflowID, sourceBeadID, err)
			}
			if ok {
				depsTracef(deps, "source-workflow prelaunch-direct-match source=%s workflow=%s store=%s", sourceBeadID, previousWorkflowID, root.storeRef)
				blockingRoots = append(blockingRoots, root)
			} else {
				depsTracef(deps, "source-workflow prelaunch-direct-skip source=%s workflow=%s reason=%s", sourceBeadID, previousWorkflowID, reason)
			}
		}
		if len(blockingRoots) > 0 {
			if !force {
				return &sourceworkflow.ConflictError{
					SourceBeadID: sourceBeadID,
					WorkflowIDs:  blockingWorkflowIDs(blockingRoots),
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
		roots, err = waitForSourceWorkflowLaunchVisible(ctx, deps, sourceBeadID, result.WorkflowID, launch.storeRef)
		if err != nil {
			// A transient store error while re-listing is recoverable:
			// the finalize already succeeded, the lock is still held, and
			// the underlying stores may briefly be unavailable. Warn and
			// continue.
			result.MetadataErrors = append(result.MetadataErrors,
				fmt.Sprintf("verify live workflows for %s: %v", sourceBeadID, err))
			return nil
		}
		if roots == nil {
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
			result = SlingResult{}
			if rollbackErr := rollbackSourceWorkflowReplacement(launch, deps.Store, sourceBeadID, previousWorkflowID, restoreState); rollbackErr != nil {
				return errors.Join(invariantErr, rollbackErr)
			}
			return invariantErr
		}
		return nil
	})
	return result, err
}

func withGraphV2SourceWorkflowLock(ctx context.Context, deps SlingDeps, sourceBeadID string, fn func() (SlingResult, error)) (SlingResult, error) {
	sourceBeadID = sourceworkflow.NormalizeSourceBeadID(sourceBeadID)
	if sourceBeadID == "" {
		return fn()
	}
	var result SlingResult
	err := sourceworkflow.WithLock(ctx, deps.CityPath, sourceWorkflowLockScope(deps), sourceBeadID, func() error {
		var err error
		result, err = fn()
		return err
	})
	return result, err
}

func waitForSourceWorkflowLaunchVisible(ctx context.Context, deps SlingDeps, sourceBeadID, workflowID, storeRef string) ([]sourceWorkflowRoot, error) {
	var roots []sourceWorkflowRoot
	for attempt := 1; attempt <= sourceWorkflowLaunchVisibilityAttempts; attempt++ {
		var err error
		roots, err = listSourceWorkflowRoots(deps, sourceBeadID)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(roots, func(root sourceWorkflowRoot) bool {
			return sameWorkflowRoot(root, workflowID, storeRef)
		}) {
			depsTracef(deps, "source-workflow launch-visibility attempt=%d source=%s workflow=%s result=list-match roots=%d", attempt, sourceBeadID, workflowID, len(roots))
			return roots, nil
		}
		root, ok, reason, err := sourceWorkflowRootByID(deps, sourceBeadID, workflowID, storeRef)
		if err != nil {
			depsTracef(deps, "source-workflow launch-visibility attempt=%d source=%s workflow=%s result=direct-error err=%v", attempt, sourceBeadID, workflowID, err)
			return nil, err
		}
		if ok {
			depsTracef(deps, "source-workflow launch-visibility attempt=%d source=%s workflow=%s result=direct-match roots=%d", attempt, sourceBeadID, workflowID, len(roots))
			return []sourceWorkflowRoot{root}, nil
		}
		depsTracef(deps, "source-workflow launch-visibility attempt=%d source=%s workflow=%s result=retry roots=%d direct=%s", attempt, sourceBeadID, workflowID, len(roots), reason)
		if attempt == sourceWorkflowLaunchVisibilityAttempts {
			return nil, nil
		}
		timer := time.NewTimer(sourceWorkflowLaunchVisibilityRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, nil
}

func sourceWorkflowRootByID(deps SlingDeps, sourceBeadID, workflowID, sourceStoreRef string) (sourceWorkflowRoot, bool, string, error) {
	workflowID = strings.TrimSpace(workflowID)
	if workflowID == "" {
		return sourceWorkflowRoot{}, false, "empty_workflow_id", nil
	}
	sourceStoreRef = strings.TrimSpace(sourceStoreRef)
	if deps.SourceWorkflowStores == nil {
		return sourceWorkflowRootByIDInStore(deps.Store, sourceBeadID, workflowID, sourceStoreRef, sourceStoreRef)
	}
	stores, err := deps.SourceWorkflowStores()
	if err != nil {
		return sourceWorkflowRoot{}, false, "stores_error", err
	}
	reason := "not_found"
	for _, info := range stores {
		if info.Store == nil {
			continue
		}
		rootStoreRef := strings.TrimSpace(info.StoreRef)
		root, ok, storeReason, err := sourceWorkflowRootByIDInStore(info.Store, sourceBeadID, workflowID, sourceStoreRef, rootStoreRef)
		if err != nil {
			return sourceWorkflowRoot{}, false, storeReason, err
		}
		if ok {
			return root, true, storeReason, nil
		}
		if storeReason != "not_found" {
			reason = storeReason
		}
	}
	return sourceWorkflowRoot{}, false, reason, nil
}

func sourceWorkflowRootByIDInStore(store beads.Store, sourceBeadID, workflowID, sourceStoreRef, rootStoreRef string) (sourceWorkflowRoot, bool, string, error) {
	if store == nil {
		return sourceWorkflowRoot{}, false, "not_found", nil
	}
	root, err := store.Get(workflowID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return sourceWorkflowRoot{}, false, "not_found", nil
		}
		return sourceWorkflowRoot{}, false, "get_error", err
	}
	// The launch boundary protects a live-workflow singleton invariant. A
	// closed root may prove that creation happened, but it is not a live root
	// and must not leave source.workflow_id pointing at completed graph state.
	if root.Status == "closed" {
		return sourceWorkflowRoot{}, false, "closed", nil
	}
	if !sourceworkflow.IsWorkflowRoot(root) {
		return sourceWorkflowRoot{}, false, "not_workflow_root", nil
	}
	if !sourceworkflow.WorkflowMatchesSource(root, sourceBeadID, sourceStoreRef, rootStoreRef) {
		return sourceWorkflowRoot{}, false, "source_mismatch", nil
	}
	return sourceWorkflowRoot{
		root:     root,
		store:    store,
		storeRef: strings.TrimSpace(rootStoreRef),
	}, true, "matched", nil
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
		if err := deps.Store.SetMetadata(child.ID, beadmeta.MoleculeIDMetadataKey, mResult.RootID); err != nil {
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
	isGraph, _, err := graphv2.IsGraphV2Formula(formulaName, searchPaths)
	if err != nil {
		return false, err
	}
	if isGraph {
		return true, nil
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, vars)
	if err != nil {
		return false, err
	}
	return graphroute.IsCompiledGraphWorkflow(recipe), nil
}

func prepareGraphV2FormulaInvocation(ctx context.Context, formulaName, targetID string, opts SlingOpts, deps SlingDeps, a config.Agent) (graphv2.Invocation, bool, error) {
	searchPaths := SlingFormulaSearchPaths(deps, a)
	vars := buildGraphV2SlingFormulaVars(formulaName, targetID, opts.Vars, a, deps)
	inv, err := graphv2.PrepareInvocation(ctx, deps.Store, formulaName, searchPaths, targetID, vars)
	if err != nil {
		return graphv2.Invocation{}, false, err
	}
	isGraph := formula.UsesGraphCompiler(inv.Formula)
	return inv, isGraph, nil
}

func validateSlingFormulaRuntimeVars(ctx context.Context, formulaName string, searchPaths []string, opts molecule.Options) error {
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, opts.Vars)
	if err != nil {
		return err
	}
	return molecule.ValidateRecipeRuntimeVars(recipe, opts)
}

func checkLegacySourceWorkflowConflict(deps SlingDeps, beadID string) error {
	roots, err := listSourceWorkflowRoots(deps, beadID)
	if err != nil {
		return fmt.Errorf("list live workflows for %s: %w", beadID, err)
	}
	if len(roots) == 0 {
		return nil
	}
	return &sourceworkflow.ConflictError{
		SourceBeadID: beadID,
		WorkflowIDs:  blockingWorkflowIDs(roots),
	}
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

func listContainerChildren(querier BeadChildQuerier, containerID string, includeClosed bool) ([]beads.Bead, error) {
	if store, ok := querier.(beads.Store); ok {
		return convoycore.Members(store, containerID, includeClosed)
	}
	return querier.List(beads.ListQuery{
		ParentID:      containerID,
		IncludeClosed: includeClosed,
		Sort:          beads.SortCreatedAsc,
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

	useFormula := opts.OnFormula
	if useFormula == "" && !opts.IsFormula && !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
		useFormula = a.EffectiveDefaultSlingFormula()
	}

	if !beads.IsContainerType(b.Type) {
		singleOpts := opts
		singleOpts.IsFormula = false
		singleDeps := deps
		singleDeps.ValidationQuerier = containerQuerier
		return DoSling(singleOpts, singleDeps, querier)
	}

	if useFormula != "" {
		_, isGraph, err := prepareGraphV2FormulaInvocation(context.Background(), useFormula, b.ID, opts, deps, a)
		if err != nil {
			return SlingResult{}, fmt.Errorf("instantiating formula %q on %s %s: %w", useFormula, b.Type, b.ID, err)
		}
		if isGraph {
			singleDeps := deps
			singleDeps.ValidationQuerier = containerQuerier
			return DoSling(opts, singleDeps, containerQuerier)
		}
	}

	children, err := listContainerChildren(querier, b.ID, true)
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
			check := CheckBeadStateWithOptions(querier, child.ID, a, deps, BeadCheckOptions{
				NoConvoy: opts.NoConvoy,
			})
			if check.Idempotent {
				childResult.Skipped = true
				batchResult.Children = append(batchResult.Children, childResult)
				idempotent++
				continue
			}
			batchResult.BeadWarnings = append(batchResult.BeadWarnings, check.Warnings...)
		}

		if shouldValidateBuiltInRouteStoreReachable(opts, deps) {
			if err := validateBuiltInRouteStoreReachable(deps, child.ID, a); err != nil {
				childResult.Failed = true
				childResult.FailReason = err.Error()
				batchResult.Children = append(batchResult.Children, childResult)
				childErrors = append(childErrors, err)
				telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
				failed++
				continue
			}
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
			if err := validateBuiltInRouteStoreReachable(deps, child.ID, a); err != nil {
				childResult.Failed = true
				childResult.FailReason = err.Error()
				batchResult.Children = append(batchResult.Children, childResult)
				childErrors = append(childErrors, err)
				telemetry.RecordSling(context.Background(), a.QualifiedName(), TargetType(&a), batchMethod, err)
				failed++
				continue
			}
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

// reopenForReassign makes a bead claimable by a target pool before routing:
// it clears any assignee and reopens the bead if a prior actor left it
// in_progress. It checks the city primary store (deps.Store) first; if the
// bead is not there it sweeps the source-workflow stores
// (deps.SourceWorkflowStores) so rig-prefixed beads — whose record lives in a
// rig store, not deps.Store — are still reopened. No-op when the bead is
// already open and unassigned, no store is available, or the bead is absent
// from every store. Errors on a real primary-store read failure, a store-Update
// failure, or a SourceWorkflowStores listing/read failure. See
// SlingOpts.Reassign, #1007, #3408 (assignee), and #3231 (status).
func reopenForReassign(beadID string, deps SlingDeps) error {
	if deps.Store != nil {
		b, err := deps.Store.Get(beadID)
		if err == nil {
			return reopenForReassignInStore(deps.Store, beadID, b)
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("reading %s from primary store to reopen for reassign: %w", beadID, err)
		}
		// ErrNotFound: the record is not in the city primary store. For
		// rig-prefixed beads it lives in a rig store, so fall through to the
		// source-workflow sweep below.
	}
	// Sweep the source-workflow stores and reopen the bead in whichever one
	// holds it. Mirrors the multi-store pattern in sourceWorkflowRootByID,
	// which likewise consults the workflow stores when deps.Store lacks (or
	// omits) the bead.
	if deps.SourceWorkflowStores == nil {
		return nil
	}
	stores, err := deps.SourceWorkflowStores()
	if err != nil {
		return fmt.Errorf("listing source-workflow stores to reopen %s for reassign: %w", beadID, err)
	}
	for _, info := range stores {
		if info.Store == nil {
			continue
		}
		b, err := info.Store.Get(beadID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return fmt.Errorf("reading %s from store %q to reopen for reassign: %w", beadID, strings.TrimSpace(info.StoreRef), err)
		}
		return reopenForReassignInStore(info.Store, beadID, b)
	}
	return nil
}

// reopenForReassignInStore clears b's assignee and resets an in_progress
// status back to open in a single update, returning nil without writing when
// the bead is already open and unassigned so no spurious store write occurs.
// The status reset is what makes a bead that an order or human previously
// claimed (status=in_progress) claimable again — IsReadyCandidate requires
// status=open, so clearing the assignee alone leaves it routed-but-unclaimable
// (gastownhall/gascity#3231).
func reopenForReassignInStore(store beads.Store, beadID string, b beads.Bead) error {
	var update beads.UpdateOpts
	if strings.TrimSpace(b.Assignee) != "" {
		empty := ""
		update.Assignee = &empty
	}
	if b.Status == "in_progress" {
		open := "open"
		update.Status = &open
	}
	if update.Assignee == nil && update.Status == nil {
		return nil
	}
	return store.Update(beadID, update)
}
