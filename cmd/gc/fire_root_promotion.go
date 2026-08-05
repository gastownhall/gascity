package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sling"
)

// fireRootCooker cooks one ready fire-root's declared formula with its declared
// vars. The controller wires the real in-process molecule-materialization path
// (CityRuntime.cookFireRoot); tests inject a recorder. It is handed only the
// fire-root bead, the declared formula name, and the parsed gc.fire_vars —
// nothing else — so the promotion stays judgment-free (ZFC): the cooker executes
// exactly what the bead declared. The decision (which formula, what vars, when)
// was baked into the fire-root's metadata and dependency graph at bead-creation,
// never re-derived in Go.
type fireRootCooker func(fireRoot beads.Bead, formulaName string, vars map[string]string) error

// promoteReadyFireRoots auto-cooks every bd-ready "fire-root" — an open,
// dependency-satisfied, unassigned bead that DECLARES gc.fire_formula — that has
// not already been fired, and returns the number of fire-roots it cooked.
//
// It is the formula-cook sibling of restoreCarriedWorkRoutes. Where that
// re-stamps gc.routed_to from a pool route a bead already carries, this cooks a
// formula a bead already declares (gc.fire_formula + gc.fire_vars). Both run on
// the controller patrol tick, and both are judgment-free (ZFC): the controller
// only executes a decision the bead's own metadata already encodes. The GO is
// the blocked→ready transition itself — a fire-root blocked-by an open
// authorization/QA/base-landed gate bead is not ready, so store.Ready() does not
// surface it; closing the gate (bead-state, not a mail) makes it ready and the
// next tick fires it. No message bead is read or written anywhere on this path
// (sc-jrp84z: mail is notification-only; beads+hooks propel work).
//
// Idempotency (cannot double-fire): a fire-root that already has a live molecule
// or workflow child has been fired, so HasMoleculeChildren short-circuits it.
// The probe fails closed — an inconclusive probe skips the cook rather than risk
// a duplicate fire. A bead with no gc.fire_formula, or a control-dispatcher /
// workflow-topology bead (gc.kind set), is left untouched: which formula a plain
// bead should run is its owner's judgment, exactly as restoreCarriedWorkRoutes
// leaves an unrouted bead for its owner to sling.
func promoteReadyFireRoots(store beads.Store, cook fireRootCooker) (int, error) {
	if store == nil || cook == nil {
		return 0, nil
	}
	// store.Ready() resolves the dependency graph: it returns open, non-excluded,
	// non-deferred beads whose every blocks/waits-for/conditional-blocks
	// dependency is closed. That dependency-satisfied set is exactly the fire GO —
	// a fire-root gated by an open bead is absent until the gate closes.
	ready, err := store.Ready()
	if err != nil {
		return 0, fmt.Errorf("listing ready fire-roots: %w", err)
	}
	var (
		fired int
		errs  []error
	)
	for _, b := range ready {
		formulaName := strings.TrimSpace(b.Metadata[beadmeta.FireFormulaMetadataKey])
		if formulaName == "" {
			continue // ZFC: only a bead that declares a fire-formula is a fire-root
		}
		// Control-dispatcher (retry, ralph, …) and workflow-topology (scope, spec)
		// beads carry gc.kind; a formula-cook is never the owner's intent there, so
		// they are skipped — the same exclusion carriedPoolRoute applies to a bare
		// gc.run_target. A cooked workflow root also carries gc.kind=workflow, so
		// this keeps the promotion from ever treating its own output as a fire-root.
		if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) != "" {
			continue
		}
		// An assigned fire-root is already being handled by a claimant; leave it.
		if strings.TrimSpace(b.Assignee) != "" {
			continue
		}
		// Already fired? A live (non-closed) molecule/workflow attachment — via the
		// fire-root's molecule_id/workflow_id forward pointer or a parented
		// workflow/molecule root — means the convoy already exists; re-cooking would
		// double-fire. Fail closed on an inconclusive probe (skip, do not cook).
		hasChild, probeErr := sling.HasMoleculeChildren(store, b.ID, store)
		if probeErr != nil {
			errs = append(errs, fmt.Errorf("fire-root %s: probing for a live convoy: %w", b.ID, probeErr))
			continue
		}
		if hasChild {
			continue
		}
		// gc.fire_vars reuses the gc.graphv2_vars.v1 map shape; the canonical decoder
		// drops reserved runtime vars and hands the declared vars to the cook 1:1.
		vars, parseErr := graphv2.ParseRuntimeVarsMetadata(b.Metadata[beadmeta.FireVarsMetadataKey])
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("fire-root %s: parsing gc.fire_vars: %w", b.ID, parseErr))
			continue
		}
		if cookErr := cook(b, formulaName, vars); cookErr != nil {
			errs = append(errs, fmt.Errorf("fire-root %s: cooking %q: %w", b.ID, formulaName, cookErr))
			continue
		}
		fired++
	}
	return fired, errors.Join(errs...)
}

// fireRootCookFunc materializes a ready fire-root's declared formula in the given
// store. It is a CityRuntime seam: production selects cookFireRoot, tests inject a
// recorder (mirrors buildFn). storeRef/rigName scope the source linkage and the
// formula search paths to the fire-root's resident store.
type fireRootCookFunc func(store beads.Store, storeRef, rigName string, fireRoot beads.Bead, formulaName string, vars map[string]string) error

// promoteReadyFireRoots auto-cooks ready fire-roots across the city store and
// every rig store, so a build-from-plan (or any declared) fire-root fires the
// moment its blocked-by preconditions clear — no manual `gc sling` (sc-jrp84z).
// It is the judgment-free sibling of recoverUnroutedWorkRoutes and runs at the
// same patrol-tick and startup call sites. Best-effort: a per-store failure is
// logged and the remaining stores still run.
func (cr *CityRuntime) promoteReadyFireRoots() {
	cook := cr.fireRootCook
	if cook == nil {
		cook = cr.cookFireRoot
	}
	type fireScope struct {
		label    string
		storeRef string
		rigName  string
		store    beads.Store
	}
	scopes := []fireScope{{label: "city", storeRef: "city:" + cr.cityName, store: cr.cityBeadStore()}}
	for name, store := range cr.rigBeadStores() {
		scopes = append(scopes, fireScope{label: "rig " + name, storeRef: "rig:" + name, rigName: name, store: store})
	}
	for _, sc := range scopes {
		if sc.store == nil {
			continue
		}
		sc := sc
		bind := func(fireRoot beads.Bead, formulaName string, vars map[string]string) error {
			return cook(sc.store, sc.storeRef, sc.rigName, fireRoot, formulaName, vars)
		}
		fired, err := promoteReadyFireRoots(sc.store, bind)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: fire-root promotion (%s): %v\n", cr.logPrefix, sc.label, err) //nolint:errcheck // best-effort stderr
		}
		if fired > 0 {
			fmt.Fprintf(cr.stderr, "%s: fire-root promotion (%s): cooked %d ready fire-root(s)\n", cr.logPrefix, sc.label, fired) //nolint:errcheck // best-effort stderr
		}
	}
}

// cookFireRoot is the controller's real fire-root cook: it materializes the
// declared formula in-process via molecule.Cook — the same molecule layer the
// order dispatcher instantiates through, never a `gc sling` shell-out — passing
// the declared vars into both the compile and the instantiate so they reach the
// materialized root 1:1 (gc.var.<name>). It then links the cooked root to the
// fire-root so the promotion is idempotent, and the controller's own graph-dispatch
// reconcile advances the cooked workflow exactly as it does an order wisp.
func (cr *CityRuntime) cookFireRoot(store beads.Store, storeRef, rigName string, fireRoot beads.Bead, formulaName string, vars map[string]string) error {
	res, err := molecule.Cook(context.Background(), store, formulaName, cr.fireFormulaSearchPaths(rigName), molecule.Options{
		Vars:     vars,
		ParentID: fireRoot.ID,
	})
	if err != nil {
		return err
	}
	if res == nil || strings.TrimSpace(res.RootID) == "" {
		return fmt.Errorf("cook of %q produced no root", formulaName)
	}
	return linkFiredRoot(store, storeRef, fireRoot.ID, res)
}

// linkFiredRoot records the fire-root ⇄ cooked-root linkage that makes the
// promotion idempotent and keeps the cooked workflow visible to source-workflow
// lookups. molecule.Cook does not apply Options.ParentID to a graph workflow-kind
// root (internal/molecule/molecule.go skips the parent coercion for
// gc.kind=workflow) and never stamps a source pointer, so a graph fire-root would
// otherwise have no link back at all. It therefore mirrors the two-directional
// stamp the sling graph-launch performs (internal/sling doStartGraphWorkflow) —
// gc.source_bead_id + gc.source_store_ref on the root, and the "workflow_id"
// forward pointer HasMoleculeChildren reads on the fire-root — and stamps the
// legacy molecule_id forward pointer otherwise. Best-effort per stamp: the fire
// already succeeded, so a metadata error is surfaced but does not unwind the
// materialized convoy.
func linkFiredRoot(store beads.Store, storeRef, fireRootID string, res *molecule.Result) error {
	if res.GraphWorkflow {
		var errs []error
		if err := store.SetMetadata(res.RootID, beadmeta.SourceBeadIDMetadataKey, fireRootID); err != nil {
			errs = append(errs, fmt.Errorf("setting gc.source_bead_id on %s: %w", res.RootID, err))
		}
		if ref := strings.TrimSpace(storeRef); ref != "" {
			if err := store.SetMetadata(res.RootID, beadmeta.SourceStoreRefMetadataKey, ref); err != nil {
				errs = append(errs, fmt.Errorf("setting gc.source_store_ref on %s: %w", res.RootID, err))
			}
		}
		// "workflow_id" is the non-gc dispatch forward pointer (not gc.workflow_id);
		// it is the key HasMoleculeChildren reads to see the fire-root's live root.
		if err := store.SetMetadata(fireRootID, "workflow_id", res.RootID); err != nil {
			errs = append(errs, fmt.Errorf("setting workflow_id on %s: %w", fireRootID, err))
		}
		return errors.Join(errs...)
	}
	if err := store.SetMetadata(fireRootID, beadmeta.MoleculeIDMetadataKey, res.RootID); err != nil {
		return fmt.Errorf("setting molecule_id on %s: %w", fireRootID, err)
	}
	return nil
}

// fireFormulaSearchPaths resolves the formula search paths for a fire-root's
// resident store: a rig store uses that rig's formula layers (falling back to the
// city layers), the city store uses the city layers. Mirrors
// workflowFormulaSearchPaths' rig→city fallback without the graph-routing hints a
// freshly declared fire-root does not yet carry.
func (cr *CityRuntime) fireFormulaSearchPaths(rigName string) []string {
	if cr.cfg == nil {
		return nil
	}
	if rigName != "" {
		if paths := cr.cfg.FormulaLayers.SearchPaths(rigName); len(paths) > 0 {
			return paths
		}
	}
	return cr.cfg.FormulaLayers.City
}
