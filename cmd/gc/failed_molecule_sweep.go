package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
)

// runFailedMoleculeSweepWatchdog closes the molecule_failed chains no caller
// will ever re-enter (sourceworkflow.SweepFailedMolecules) once their failure
// stamp is failedMoleculeSweepGrace old. A keyless mint, such as an order
// dispatch, that failed part-way leaves such a chain: open, unlabelled and
// unclaimable, with no other owner. The orders lane runs it with its pass's
// config snapshot.
func (cr *CityRuntime) runFailedMoleculeSweepWatchdog(_ *config.City, now time.Time) {
	cutoff := now.Add(-failedMoleculeSweepGrace)
	graph := beads.GraphMolecule(cr.graphBeadStore())
	if graph.Absent() {
		return
	}
	result, err := sourceworkflow.SweepFailedMolecules(graph, cutoff, failedMoleculeSweepMaxChains)
	cr.logFailedMoleculeSweep("graph store", result, err)
}

// logFailedMoleculeSweep writes one stderr line per chain a sweep of label
// closed and one per error it returned.
func (cr *CityRuntime) logFailedMoleculeSweep(label string, result sourceworkflow.FailedMoleculeSweep, err error) {
	if cr.stderr == nil {
		return
	}
	for _, id := range result.Chains {
		fmt.Fprintf(cr.stderr, "%s: failed-molecule sweep: closed failed chain %s in %s\n", cr.logPrefix, id, label) //nolint:errcheck // best-effort stderr
	}
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: failed-molecule sweep: %s: %v\n", cr.logPrefix, label, err) //nolint:errcheck // best-effort stderr
	}
}
