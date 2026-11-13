package main

// What the guided split repairs before its copy and proves after it (S-3
// acceptance 2 and 4; the workarounds the five live splits did by hand).
//
// Repair. Once the city's copy has converged and the rigs are imported, still
// inside the window, the split runs the doctor's dangling-blocking-edges
// fixer, as `gc doctor --check dangling-blocking-edges --fix` would
// (danglingBlockingEdgesCheck; it judges a split city's stores, so it runs
// once the city is split): a malformed "<type>:<id>" blocks literal, which bd
// read as no blocker but no split store resolves, is rewritten as the typed
// edge it meant when <id> stayed in the row's store, or removed and recorded
// on the row when <id> moved to the binding; a blocking edge onto a bead no
// store holds is removed. Every live split ran that fix by hand afterwards,
// while gc ready held the rows. The snapshot, taken before, holds the edges
// as they were. A fix that cannot run (a store that would not read) does not
// stop the split: preflight named those rows, and the closing report names
// the doctor command that repairs them.
//
// Prove. Before the copy the split reads the city's ready set as `gc ready`
// reads it, and records it in its journal; after the rig imports it reads it
// again through the split city's routes. They must be equal except for what
// the split is known to change:
//
//   - a row that waits on an "external:<id>" reference whose bead no store
//     holds: bd serves it, and the split city enforces the reference
//     (preflight's `external refs` note);
//   - a cross-store gate the split minted;
//   - a row the dangling-edge repair may release: one holding a blocking edge
//     whose target its store does not hold, which a reader may count blocked
//     before the split;
//   - a ready row that carries a defer_until: its deferral has come due, so
//     the clock, not the split, made it ready. A deferral that passes while
//     the window runs is one; a bd deferral already past its date, which bd
//     keeps parked until its own sweep and the binding serves once due, is
//     the other.
//
// Any other row ready on one side and not the other fails the split before
// it is recorded complete, naming each: the city is split and serves, and
// both rollbacks stay open.

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/readywork"
)

// splitDanglingEdgeRepairs runs the dangling-blocking-edges fixer over the
// split city; a test seam, as splitDemandRepairs is.
var splitDanglingEdgeRepairs = func(cfg *config.City, cityPath string) error {
	return newDanglingBlockingEdgesCheck(cfg, cityPath, perRunStoreFactory(openStoreForCity(cityPath))).Fix(nil)
}

// splitReadySnapshot is a city's ready set: every ready row's id, and the
// rows the split is known to change (a row waiting on an external reference
// bd does not enforce, a cross-store gate, a row the repair may release, a
// row whose deferral came due).
type splitReadySnapshot struct {
	IDs       []string `json:"ids"`
	Explained []string `json:"explained,omitempty"`
}

// splitReadyRead reads the city's ready set under cfg; a test seam.
var splitReadyRead = readSplitReadySet

// readSplitReadySet reads cityPath's ready set under cfg, as `gc ready` reads
// it in-process: the city's store, every serving rig's, and the class binding
// cfg routes to.
func readSplitReadySet(cityPath string, cfg *config.City) (splitReadySnapshot, error) {
	var out splitReadySnapshot
	cityStore, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return out, fmt.Errorf("opening the city's store: %w", err)
	}
	defer closeBeadStoreHandle(cityStore) //nolint:errcheck // read-only
	rigStores, err := readyRigLegStores(cfg, cityPath)
	if err != nil {
		return out, err
	}
	defer func() {
		for _, s := range rigStores {
			closeBeadStoreHandle(s) //nolint:errcheck // read-only
		}
	}()
	legs, err := readyFederationLegs(cityPath, loadedCityName(cfg, cityPath), cfg, cityStore, rigStores)
	if err != nil {
		return out, err
	}
	rows, err := readywork.Read(legs, readywork.Query{})
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out.IDs = append(out.IDs, r.ID)
		// A ready row with a defer_until is one whose deferral came due.
		if splitWaitsOnExternal(r) || hasLabel(r.Labels, dispatch.CrossStoreGateLabel) || r.DeferUntil != nil {
			out.Explained = append(out.Explained, r.ID)
		}
	}
	sort.Strings(out.IDs)
	sort.Strings(out.Explained)
	return out, nil
}

// splitWaitsOnExternal reports a ready row with a blocking edge onto an
// "external:<id>" bead reference.
func splitWaitsOnExternal(r readywork.ReadyBead) bool {
	for _, d := range r.Dependencies {
		if !beads.IsReadyBlockingDependencyType(d.Type) || !strings.HasPrefix(strings.TrimSpace(d.DependsOnID), "external:") {
			continue
		}
		if target, capability := beads.ExternalBlockerTarget(d.DependsOnID); !capability && target != "" {
			return true
		}
	}
	return false
}

// splitReadinessDifference compares the ready set before the split with the
// one after it, and returns the rows that changed for no reason the split
// records, each with which way it changed; nil when there are none.
func splitReadinessDifference(before, after splitReadySnapshot) []string {
	explained := map[string]bool{}
	for _, id := range append(slices.Clone(before.Explained), after.Explained...) {
		explained[id] = true
	}
	inBefore, inAfter := map[string]bool{}, map[string]bool{}
	for _, id := range before.IDs {
		inBefore[id] = true
	}
	for _, id := range after.IDs {
		inAfter[id] = true
	}
	var out []string
	for _, id := range before.IDs {
		if !inAfter[id] && !explained[id] {
			out = append(out, id+" (ready before the split, not after)")
		}
	}
	for _, id := range after.IDs {
		if !inBefore[id] && !explained[id] {
			out = append(out, id+" (ready after the split, not before)")
		}
	}
	return out
}

// splitReadyBeforeCopy records the city's ready set before the copy in the
// journal, with the rows the repair after the copy may release explained.
// The caller holds the lock, the guard and the window.
func (r *splitRun) splitReadyBeforeCopy() error {
	before, err := splitReadyRead(r.request.CityPath, r.request.Cfg)
	if err != nil {
		return fmt.Errorf("reading the ready set before the copy: %w", err)
	}
	repairable, err := splitRepairableRows(r.request.CityPath)
	if err != nil {
		return fmt.Errorf("reading the rows the dangling-edge repair may release: %w", err)
	}
	before.Explained = append(before.Explained, repairable...)
	sort.Strings(before.Explained)
	before.Explained = slices.Compact(before.Explained)
	r.journal.ReadyBefore = &before
	return nil
}

// splitRepairableRows lists the open work rows the dangling-edge repair may
// settle: each holds a blocking edge whose target the work store does not
// hold (a malformed "<type>:<id>" literal among them). Before the split a
// reader may count such a row blocked; once the repair rewrites or removes
// the edge it is ready.
func splitRepairableRows(cityPath string) ([]string, error) {
	store, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return nil, err
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // read-only
	var ids []string
	for _, status := range []string{"open", "in_progress"} {
		rows, err := store.List(beads.ListQuery{Status: status, AllowScan: true, Live: true, TierMode: beads.TierBoth})
		if err != nil {
			return nil, fmt.Errorf("listing %s rows: %w", status, err)
		}
		for _, b := range rows {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	deps, err := doctorDepsFor(store, ids)
	if err != nil {
		return nil, err
	}
	targets := map[string]bool{}
	for _, rowDeps := range deps {
		for _, d := range rowDeps {
			if target, capability := beads.ExternalBlockerTarget(d.DependsOnID); beads.IsReadyBlockingDependencyType(d.Type) && !capability && target != "" {
				targets[target] = true
			}
		}
	}
	held, err := beads.GetMany(store, sortedMapKeys(targets))
	if err != nil {
		return nil, err
	}
	var out []string
	for row, rowDeps := range deps {
		for _, d := range rowDeps {
			target, capability := beads.ExternalBlockerTarget(d.DependsOnID)
			if !beads.IsReadyBlockingDependencyType(d.Type) || capability || target == "" {
				continue
			}
			if _, ok := held[target]; !ok {
				out = append(out, row)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// splitRepairAndProve runs the dangling-edge repair over the split city, then
// reads its ready set and compares it with the one recorded before the copy.
func (r *splitRun) splitRepairAndProve(cfg *config.City) error {
	// The routes this process resolved before the split are the unsplit
	// city's; the repair and the read take the split city's.
	retireCLIStorageRoutesFor(r.request.CityPath, nil)
	resetCLIResidencyBindings()
	if err := splitDanglingEdgeRepairs(cfg, r.request.CityPath); err != nil {
		r.journal.DanglingRepair = err.Error()
		fmt.Fprintf(r.stderr, "%s: WARNING: the dangling-edge repair did not run to the end (%v); the split goes on, and `gc doctor --check dangling-blocking-edges --fix` repairs the rest\n", r.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	if r.journal.ReadyBefore == nil {
		fmt.Fprintf(r.stderr, "%s: no ready set was recorded before the copy (a split begun by an earlier build); readiness is not compared\n", r.logPrefix) //nolint:errcheck // best-effort stderr
		return nil
	}
	after, err := splitReadyRead(r.request.CityPath, cfg)
	if err != nil {
		return fmt.Errorf("reading the ready set after the split: %w", err)
	}
	if diff := splitReadinessDifference(*r.journal.ReadyBefore, after); len(diff) > 0 {
		return fmt.Errorf("the split changed which beads are ready, for no reason it records: %s. Inspect them with `gc ready` and `gc bd show <id>`. The city is split, and the split stays incomplete until they are resolved; either way back puts it back: %s", strings.Join(diff, ", "), splitRollbackChoices(r.journal))
	}
	fmt.Fprintf(r.stdout, "%s: the ready set is the same before and after the split (%d bead(s); %d waiting on an external reference or a gate)\n", r.logPrefix, len(after.IDs), len(after.Explained)) //nolint:errcheck // best-effort stdout
	return nil
}

// splitRollbackChoices names the ways back from a split, for a refusal that
// stops the run before its closing report.
func splitRollbackChoices(j storageSplitJournal) string {
	choices := fmt.Sprintf("`%s` keeps every bead written since", storageReverseInstruction(reverseToWork, nil))
	if j.Snapshot != "" {
		choices += fmt.Sprintf("; `%s` puts the city back exactly as the pinned snapshot %s found it", snapshotCityRestoreInstruction(j.Snapshot, j.SnapshotTo), j.Snapshot)
	}
	return choices
}

// splitClosingRollbacks is the closing report's account of the two ways back
// and of the reaper's guard.
func splitClosingRollbacks(w io.Writer, j storageSplitJournal) {
	fmt.Fprintf(w, "To roll back, keeping every bead written since: `%s`.\n", storageReverseInstruction(reverseToWork, nil)) //nolint:errcheck // best-effort stdout
	if j.Snapshot != "" {
		fmt.Fprintf(w, "To put the city back exactly as it was before the split, discarding what was written since: `%s` (snapshot %s stays pinned).\n", snapshotCityRestoreInstruction(j.Snapshot, j.SnapshotTo), j.Snapshot) //nolint:errcheck // best-effort stdout
	}
	if j.DanglingRepair != "" {
		fmt.Fprintf(w, "The dangling-edge repair stopped (%s): run `gc doctor --check dangling-blocking-edges --fix` before `gc start`.\n", j.DanglingRepair) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(w, "The split can let the core reaper reach stale issues it had no time for before. A burst of more than GC_REAPER_STALE_ISSUE_BURST_MAX (default 25) is held, not closed, and escalated; review it and release it with GC_REAPER_STALE_ISSUE_BURST_ACK=<N>.\n") //nolint:errcheck // best-effort stdout
}
