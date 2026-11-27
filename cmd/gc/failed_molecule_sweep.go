package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
)

const (
	// failedMoleculeSweepGrace is how long a molecule_failed chain stays open
	// before the sweep closes it, leaving its same-key caller (sling, gc
	// formula cook, a drain) the window to re-enter and close it itself.
	failedMoleculeSweepGrace = 10 * time.Minute
	// failedMoleculeSweepMaxChains bounds the chains one pass attempts per
	// store.
	failedMoleculeSweepMaxChains = 50
	// failedMoleculeSweepWatchdogInterval is how often the orders lane runs
	// the sweep.
	failedMoleculeSweepWatchdogInterval = 5 * time.Minute
)

// failedMoleculeSweepWallBudget is the wall clock one watchdog pass may spend
// across every store before no store reads past its first chain or owner. The
// orders lane runs the pass ahead of order dispatch, and on a bd work ledger
// (about 0.75 s a read, wisp_gc_work_lane.go) each chain costs a few reads.
// Package var so tests can change it.
var failedMoleculeSweepWallBudget = 5 * time.Second

// failedMoleculeSweepTarget is one store the sweep works in: the federated
// leg it reads the store through, the molecule target it closes through, and
// how its log lines name it.
type failedMoleculeSweepTarget struct {
	label string
	leg   beads.FederatedReader
	store beads.MoleculeTarget
}

// failedMoleculeSweepPass totals one watchdog pass over every store for the
// orders lane's trace.
type failedMoleculeSweepPass struct {
	closedChains int
	closedBeads  int
	errors       int
	// more is set when a store's sweep stopped at the pass's budget, or at a
	// done ctx, with chains it had not decided on; the next pass goes on from
	// them.
	more     bool
	duration time.Duration
}

// runFailedMoleculeSweepWatchdog closes the molecule_failed chains no caller
// will ever re-enter (sourceworkflow.SweepFailedMolecules) once their failure
// stamp is failedMoleculeSweepGrace old. A keyless mint, such as an order
// dispatch, that failed part-way leaves such a chain: open, unlabelled and
// unclaimable, with no other owner. The orders lane runs it with its pass's
// config snapshot, at most once per failedMoleculeSweepWatchdogInterval, over
// every store a molecule is minted in (failedMoleculeSweepTargets). Each store
// attempts at most failedMoleculeSweepMaxChains chains, and once
// failedMoleculeSweepWallBudget has passed no store reads past its first chain
// or owner, so a store with a backlog cannot starve the ones after it and the
// pass takes about the budget plus one chain per store. A store whose sweep
// stopped goes on next pass from the failed bead it stopped at, so chains
// their owners keep cannot hold back the ones behind them. A done ctx ends the
// pass. A store's error is logged and the pass goes on to the next store; a
// plan that fails is logged and sweeps nothing. The pass's totals wait for the
// lane's trace (takeFailedMoleculeSweepPass).
func (cr *CityRuntime) runFailedMoleculeSweepWatchdog(ctx context.Context, cfg *config.City, now time.Time) {
	if !cr.failedMoleculeSweepWatchdogLast.IsZero() && now.Sub(cr.failedMoleculeSweepWatchdogLast) < clock.Backstop(failedMoleculeSweepWatchdogInterval) {
		return
	}
	cr.failedMoleculeSweepWatchdogLast = now
	started := time.Now()
	cutoff := now.Add(-failedMoleculeSweepGrace)
	budget := sourceworkflow.FailedMoleculeSweepBudget{
		MaxChains: failedMoleculeSweepMaxChains,
		Deadline:  started.Add(failedMoleculeSweepWallBudget),
	}
	var pass failedMoleculeSweepPass
	targets, err := cr.failedMoleculeSweepTargets(cfg)
	if err != nil {
		cr.logFailedMoleculeSweep("store legs", sourceworkflow.FailedMoleculeSweep{}, err)
		pass.errors++
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		budget.From = cr.failedMoleculeSweepResume[target.label]
		result, err := sourceworkflow.SweepFailedMolecules(ctx, target.leg, target.store, cutoff, budget)
		cr.resumeFailedMoleculeSweepAt(target.label, result.Next)
		cr.logFailedMoleculeSweep(target.label, result, err)
		pass.closedChains += len(result.Chains)
		pass.closedBeads += result.Closed
		pass.more = pass.more || result.More
		if err != nil {
			pass.errors++
		}
	}
	pass.duration = time.Since(started)
	cr.failedMoleculeSweepLastPass = &pass
}

// resumeFailedMoleculeSweepAt records next, where the sweep of the store
// labeled label stopped, as where that store's next sweep starts; the zero
// cursor, from a sweep that finished, forgets it.
func (cr *CityRuntime) resumeFailedMoleculeSweepAt(label string, next sourceworkflow.FailedMoleculeSweepCursor) {
	if next == (sourceworkflow.FailedMoleculeSweepCursor{}) {
		delete(cr.failedMoleculeSweepResume, label)
		return
	}
	if cr.failedMoleculeSweepResume == nil {
		cr.failedMoleculeSweepResume = make(map[string]sourceworkflow.FailedMoleculeSweepCursor)
	}
	cr.failedMoleculeSweepResume[label] = next
}

// failedMoleculeSweepTargets is every store a molecule is minted in, each
// physical store once: the graph-class store, the city work store and each rig
// work store whose rig is not suspended, in that order. That census decides
// which stores the sweep works in; the epoch's Molecule{Roots} plan over it
// decides how each is read: through a federated leg, which reads past the rows
// of a class the store does not hold, as each store of a split city would. Each
// leg closes through the census's own store for it, which the sweep checks is
// the store the leg reads (sourceworkflow.SweepFailedMolecules).
func (cr *CityRuntime) failedMoleculeSweepTargets(cfg *config.City) ([]failedMoleculeSweepTarget, error) {
	stores := cr.cityStores()
	suspended := buildEffectiveSuspendedRigNames(cfg, loadSuspensionStateBestEffort(cr.cityPath))
	graph := failedMoleculeSweepTarget{label: "graph store", store: beads.GraphMolecule(stores.Graph())}
	var refs []storeref.StoreRef
	census := make(map[storeref.StoreRef]failedMoleculeSweepTarget)
	addWork := func(ref storeref.StoreRef, label string, store beads.MoleculeTarget) {
		if store.Absent() {
			return
		}
		refs = append(refs, ref)
		census[ref] = failedMoleculeSweepTarget{label: label, store: store}
	}
	addWork(storeref.WorkRef, "city work store", beads.WorkMolecule(stores.Work()))
	for _, rig := range stores.Rigs() {
		if !suspended[rig.Name] {
			addWork(storeref.RigRef(rig.Name), fmt.Sprintf("rig %q work store", rig.Name), beads.WorkMolecule(rig.Work))
		}
	}
	if graph.store.Absent() && len(refs) == 0 {
		return nil, nil
	}
	plan, err := stores.Plan(storeref.Molecule{Kind: storeref.MoleculeRoots, Scope: storeref.WorkRef, Work: refs})
	if err != nil {
		return nil, fmt.Errorf("planning: %w", err)
	}
	var targets []failedMoleculeSweepTarget
	storeref.EachTypedLeg(plan, func(l storeref.TypedLeg) {
		target := graph
		if l.Role != storeref.RoleAuthority {
			target = census[l.Ref]
		}
		target.leg = l.Reader
		targets = append(targets, target)
	})
	return targets, nil
}

// takeFailedMoleculeSweepPass returns the totals of the watchdog pass run
// since the last call, or nil when none ran, and clears them.
func (cr *CityRuntime) takeFailedMoleculeSweepPass() *failedMoleculeSweepPass {
	pass := cr.failedMoleculeSweepLastPass
	cr.failedMoleculeSweepLastPass = nil
	return pass
}

// logFailedMoleculeSweep writes one stderr line per chain a sweep of label
// closed, with the number of beads it closed, one when the sweep stopped at
// its budget or a done ctx with chains left, and one per error it returned.
func (cr *CityRuntime) logFailedMoleculeSweep(label string, result sourceworkflow.FailedMoleculeSweep, err error) {
	if cr.stderr == nil {
		return
	}
	for i, id := range result.Chains {
		fmt.Fprintf(cr.stderr, "%s: failed-molecule sweep: closed failed chain %s (%d bead(s)) in %s\n", cr.logPrefix, id, result.ChainClosed[i], label) //nolint:errcheck // best-effort stderr
	}
	if result.More {
		fmt.Fprintf(cr.stderr, "%s: failed-molecule sweep: %s: stopped with failed chains left for the next pass\n", cr.logPrefix, label) //nolint:errcheck // best-effort stderr
	}
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: failed-molecule sweep: %s: %v\n", cr.logPrefix, label, err) //nolint:errcheck // best-effort stderr
	}
}
