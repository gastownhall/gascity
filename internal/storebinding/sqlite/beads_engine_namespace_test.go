package sqlite

// The namespaces an opened engine is fenced to.
//
// A class binding claims id namespaces, and the fence is what keeps that claim
// true against a caller-pinned id (beads.WithSQLiteStoreReservedIDPrefixes).
// The set has to be derived from the classes the binding was ASSIGNED, not from
// the class this provider happens to name its file after: a whole split serves
// all five infrastructure classes from one ledger and legitimately holds every
// one of their namespaces.
//
// The work class is the case that decides the shape of the rule. Work beads
// carry the rig or HQ prefix an operator configured, which is not a reserved
// namespace and not knowable here, so a binding that serves work claims no
// namespace at all and must be left unfenced. Fencing it to the infrastructure
// prefixes would refuse every work bead the binding exists to hold.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func classSet(t *testing.T, classes ...coordclass.Class) storebinding.ClassSet {
	t.Helper()
	set, err := storebinding.NewClassSet(classes...)
	if err != nil {
		t.Fatalf("NewClassSet(%v): %v", classes, err)
	}
	return set
}

func TestEngineFenceCoversEveryAssignedClass(t *testing.T) {
	got := engineReservedPrefixes(classSet(t, coordclass.ClassGraph, coordclass.ClassNudges))
	want := append(
		config.ReservedClassPrefixesFor(config.BeadClassGraph),
		config.ReservedClassPrefixesFor(config.BeadClassNudges)...,
	)
	if len(got) != len(want) {
		t.Fatalf("engineReservedPrefixes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("engineReservedPrefixes = %v, want %v", got, want)
		}
	}
}

func TestEngineServingWorkIsLeftUnfenced(t *testing.T) {
	if got := engineReservedPrefixes(classSet(t, coordclass.ClassWork, coordclass.ClassGraph)); got != nil {
		t.Errorf("engineReservedPrefixes = %v, want nil: work beads carry the operator's configured rig prefix, so fencing this binding would refuse them", got)
	}
	// The control: drop work and the same set is fenced.
	if got := engineReservedPrefixes(classSet(t, coordclass.ClassGraph)); got == nil {
		t.Error("an infrastructure-only binding was left unfenced")
	}
}

func TestEngineFenceIsEmptyForNoClasses(t *testing.T) {
	if got := engineReservedPrefixes(storebinding.ClassSet{}); got != nil {
		t.Errorf("engineReservedPrefixes = %v, want nil", got)
	}
}
