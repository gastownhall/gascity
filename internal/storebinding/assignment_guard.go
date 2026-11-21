package storebinding

import (
	"github.com/gastownhall/gascity/internal/beads"
)

// AssignmentGuardedUpdaterFor resolves the status+assignee guarded-update
// capability from a graph class front door: the canonical Beads projection
// carries it when its backing store has one, and every other GraphStore
// implementation — an engine whose closed contract has no assignment guard —
// answers false, so an exact claim routed to that binding stays
// revision-fenced or fails closed rather than reaching for a write the
// engine cannot fence.
//
// The guard is deliberately NOT on the GraphStore contract: only the exact
// claim's binding leg needs it, and a front door that cannot fence a write
// should not advertise a method that pretends it can.
func AssignmentGuardedUpdaterFor(graph GraphStore) (beads.AssignmentGuardedUpdater, bool) {
	adapter, ok := graph.(*beadsGraphAdapter)
	if !ok {
		return nil, false
	}
	guard, ok := adapter.store.(beads.AssignmentGuardedUpdater)
	if !ok {
		return nil, false
	}
	return guard, true
}
