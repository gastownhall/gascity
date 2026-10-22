package main

import (
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worker"
)

// newLazyBeadResolver returns a by-id bead lookup over the topology build
// produces. build runs on the first lookup only, so a caller that never looks
// anything up never builds the topology. A clean miss on every leg is
// beads.ErrNotFound, which the worker treats as "unattributed".
func newLazyBeadResolver(build func() storeref.Topology) func(id string) (beads.Bead, error) {
	var (
		once sync.Once
		topo storeref.Topology
	)
	return func(id string) (beads.Bead, error) {
		once.Do(func() { topo = build() })
		return byIDBeadForTopology(topo, id)
	}
}

// formulaBeadResolver is the worker's formula-attribution lookup for this
// runtime: the acting bead and its run root resolved over the serving stores.
// topology is nil in production and builds the serving frame; tests inject a
// counting builder. Suspended rigs are not in the frame, so a pointer into one
// is unattributed and its store is never read.
func (cr *CityRuntime) formulaBeadResolver(cfg *config.City, topology func() storeref.Topology) func(id string) (beads.Bead, error) {
	if topology == nil {
		topology = func() storeref.Topology { return cr.residencyTopology(cr.routeRecoveryRigStores(cfg)) }
	}
	return newLazyBeadResolver(topology)
}

// formulaMemo returns the runtime's formula-name memo, creating it on first
// use. The runtime builds a worker factory per tick, so the memo lives here to
// outlast them.
func (cr *CityRuntime) formulaMemo() *worker.FormulaNameMemo {
	cr.formulaMemoOnce.Do(func() { cr.formulaNameMemo = worker.NewFormulaNameMemo() })
	return cr.formulaNameMemo
}
