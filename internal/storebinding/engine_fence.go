package storebinding

// The namespaces an opened engine is fenced to.
//
// A class binding claims id namespaces, and the pinned-id fence
// (engdocs/architecture/beads.md, invariant 16) is what keeps that claim true
// against a caller-pinned id. Which namespaces a binding claims follows from the
// classes it was ASSIGNED, not from the provider serving it — so the derivation
// lives here, above every provider, and two providers cannot answer it
// differently for the same assignment.

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// EngineReservedPrefixes returns the id namespaces a binding serving classes may
// hold, for the pinned-id fence. Nil leaves the store unfenced.
//
// Work is the one class that unfences a binding, and it unfences the whole
// binding: work beads carry whatever prefix an operator configured for the rig
// or HQ, which is not knowable here, so a binding that serves work claims
// nothing and every pinned id has to be let through. Fencing it to the
// infrastructure prefixes would refuse the work beads it exists to hold.
//
// That test is on the CLASS, not on "this class happens to have no registered
// prefix". The difference decides which way an unregistered class fails. Reading
// an empty prefix list as "unfenced" would drop the fence for every namespace
// the class is co-assigned with — silently, and with the binding still claiming
// them. Naming work explicitly means a class missing from the prefix table
// instead contributes nothing to a fence that stays on, so its own writes are
// refused: loud, local, and impossible to mistake for working.
// TestEveryNonWorkClassHasAFenceableNamespace catches it before it ships either
// way.
func EngineReservedPrefixes(classes ClassSet) []string {
	served := classes.Classes()
	if len(served) == 0 || classes.Has(coordclass.ClassWork) {
		return nil
	}
	prefixes := make([]string, 0, len(served))
	for _, class := range served {
		prefixes = append(prefixes, config.ReservedClassPrefixesFor(class.String())...)
	}
	return prefixes
}
