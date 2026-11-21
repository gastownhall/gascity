package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

const (
	// failedMoleculeSweepGrace is how long a molecule_failed chain stays open
	// before the sweep closes it, leaving its same-key caller (sling, gc
	// formula cook, a drain) the window to re-enter and close it itself.
	failedMoleculeSweepGrace = 10 * time.Minute
	// failedMoleculeSweepMaxChains bounds the chains one pass closes per store.
	failedMoleculeSweepMaxChains = 50
	// failedMoleculeSweepWatchdogInterval is how often the orders lane runs
	// the sweep.
	failedMoleculeSweepWatchdogInterval = 5 * time.Minute
)

// failedMoleculeSweepTarget is one store the sweep reads and how its log
// lines name it.
type failedMoleculeSweepTarget struct {
	label string
	store beads.MoleculeTarget
}

// failedMoleculeSweepPass totals one watchdog pass over every store for the
// orders lane's trace.
type failedMoleculeSweepPass struct {
	closedChains int
	closedBeads  int
	errors       int
	duration     time.Duration
}

// runFailedMoleculeSweepWatchdog closes the molecule_failed chains no caller
// will ever re-enter (sourceworkflow.SweepFailedMolecules) once their failure
// stamp is failedMoleculeSweepGrace old. A keyless mint, such as an order
// dispatch, that failed part-way leaves such a chain: open, unlabelled and
// unclaimable, with no other owner. The orders lane runs it with its pass's
// config snapshot, at most once per failedMoleculeSweepWatchdogInterval, over
// every store a molecule is minted in (failedMoleculeSweepTargets). A store's
// error is logged and the pass goes on to the next store; the pass's totals
// wait for the lane's trace (takeFailedMoleculeSweepPass).
func (cr *CityRuntime) runFailedMoleculeSweepWatchdog(cfg *config.City, now time.Time) {
	if !cr.failedMoleculeSweepWatchdogLast.IsZero() && now.Sub(cr.failedMoleculeSweepWatchdogLast) < clock.Backstop(failedMoleculeSweepWatchdogInterval) {
		return
	}
	cr.failedMoleculeSweepWatchdogLast = now
	started := time.Now()
	cutoff := now.Add(-failedMoleculeSweepGrace)
	var pass failedMoleculeSweepPass
	for _, target := range cr.failedMoleculeSweepTargets(cfg) {
		result, err := sourceworkflow.SweepFailedMolecules(target.store, cutoff, failedMoleculeSweepMaxChains)
		cr.logFailedMoleculeSweep(target.label, result, err)
		pass.closedChains += len(result.Chains)
		pass.closedBeads += result.Closed
		if err != nil {
			pass.errors++
		}
	}
	pass.duration = time.Since(started)
	cr.failedMoleculeSweepLastPass = &pass
}

// failedMoleculeSweepTargets is every store a molecule is minted in: the
// graph-class store, the city work store and each rig work store whose rig is
// not suspended, each physical store once, in that order.
func (cr *CityRuntime) failedMoleculeSweepTargets(cfg *config.City) []failedMoleculeSweepTarget {
	stores := cr.cityStores()
	suspended := buildEffectiveSuspendedRigNames(cfg, loadSuspensionStateBestEffort(cr.cityPath))
	var targets []failedMoleculeSweepTarget
	seen := make(map[string]bool)
	add := func(label string, key string, store beads.MoleculeTarget) {
		if store.Absent() || seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, failedMoleculeSweepTarget{label: label, store: store})
	}
	add("graph store", beads.PhysicalStoreKey(stores.Graph()), beads.GraphMolecule(stores.Graph()))
	add("city work store", beads.PhysicalStoreKey(stores.Work()), beads.WorkMolecule(stores.Work()))
	for _, rig := range stores.Rigs() {
		if suspended[rig.Name] {
			continue
		}
		add(fmt.Sprintf("rig %q work store", rig.Name), beads.PhysicalStoreKey(rig.Work), beads.WorkMolecule(rig.Work))
	}
	return targets
}

// takeFailedMoleculeSweepPass returns the totals of the watchdog pass run
// since the last call, or nil when none ran, and clears them.
func (cr *CityRuntime) takeFailedMoleculeSweepPass() *failedMoleculeSweepPass {
	pass := cr.failedMoleculeSweepLastPass
	cr.failedMoleculeSweepLastPass = nil
	return pass
}

// logFailedMoleculeSweep writes one stderr line per chain a sweep of label
// closed, with the number of beads it closed, and one per error it returned.
func (cr *CityRuntime) logFailedMoleculeSweep(label string, result sourceworkflow.FailedMoleculeSweep, err error) {
	if cr.stderr == nil {
		return
	}
	for i, id := range result.Chains {
		fmt.Fprintf(cr.stderr, "%s: failed-molecule sweep: closed failed chain %s (%d bead(s)) in %s\n", cr.logPrefix, id, result.ChainClosed[i], label) //nolint:errcheck // best-effort stderr
	}
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: failed-molecule sweep: %s: %v\n", cr.logPrefix, label, err) //nolint:errcheck // best-effort stderr
	}
}
