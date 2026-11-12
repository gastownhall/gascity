package main

import (
	"github.com/gastownhall/gascity/internal/coordclass"
)

// claimStoreRef is a claim's store as the work cells key it (work_started,
// written by the seat's own `gc hook --claim`; work_ladder, written by the
// ladder step) and as the execution backstop matches them: the store the
// claim RESIDES in, folded to its scope. The city work store and every
// relocated class binding (city-scope stores) are "city"; a rig's store is
// "rig:<name>". ref is any spelling a store has on its way here: the census's
// ("" for the city work store, a bare rig name, a binding's "class:<...>"),
// a workflow's ("city:<name>", "rig:<name>"), or one already folded.
//
// It is the ONE spelling, and the residence is the one meaning, for the three
// sides that must agree on it: the hook's work_started writer
// (hookStartedKeyFor), the ladder's claim targets (ladderPass.outstanding) and
// the backstop's claim snapshot (newExecutionClaimSnapshot), which the
// ladder's targets share. The census, and every controller reader with it,
// can only key a claim by the store it found the bead in; the hook used to
// key it by the work LEG it ran in, which is not the bead's store for a graph
// step resident in a relocated binding claimed through a rig seat's leg, and
// the ladder then read six working seats' started claims as unstarted
// (maintainer-city on 8d6r4, 2026-10-20).
func claimStoreRef(ref string) string {
	return normalizeIdleClaimStoreRef(ref)
}

// storeRef is the store a claim this route took resides in, as the work
// cells key it: the binding that serves the graph class, the class the route
// claims through, folded by claimStoreRef as the census folds the same
// binding's legs. "" when the route's frame names no such binding, which the
// cells read as a store nobody could resolve (it names the bead in every
// store).
func (r *hookClaimClassRoute) storeRef() string {
	if r == nil {
		return ""
	}
	b, ok := r.topology.BindingFor(coordclass.ClassGraph)
	if !ok || b.Leg.Ref == "" {
		return ""
	}
	return claimStoreRef(string(b.Leg.Ref))
}
