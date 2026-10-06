package storebinding

// The namespaces an opened engine is fenced to.
//
// These rows moved up from the SQLite provider when the beads-workspace
// provider gained the same fence. The rule was never SQLite's — it follows from
// the class assignment alone — and leaving it down in one provider is what would
// let the two answer differently for the same binding.
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
)

func fenceFor(t *testing.T, classes ...coordclass.Class) []string {
	t.Helper()
	return EngineReservedPrefixes(classSet(t, classes...))
}

func TestEngineReservedPrefixesCoverEveryAssignedClass(t *testing.T) {
	got := fenceFor(t, coordclass.ClassGraph, coordclass.ClassNudges)
	want := append(
		config.ReservedClassPrefixesFor(config.BeadClassGraph),
		config.ReservedClassPrefixesFor(config.BeadClassNudges)...,
	)
	if len(got) != len(want) {
		t.Fatalf("EngineReservedPrefixes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EngineReservedPrefixes = %v, want %v", got, want)
		}
	}
}

func TestEngineServingWorkIsLeftUnfenced(t *testing.T) {
	if got := fenceFor(t, coordclass.ClassWork, coordclass.ClassGraph); got != nil {
		t.Errorf("EngineReservedPrefixes = %v, want nil: work beads carry the operator's configured rig prefix, so fencing this binding would refuse them", got)
	}
	// The control: drop work and the same set is fenced.
	if got := fenceFor(t, coordclass.ClassGraph); got == nil {
		t.Error("an infrastructure-only binding was left unfenced")
	}
}

func TestEngineReservedPrefixesAreEmptyForNoClasses(t *testing.T) {
	if got := EngineReservedPrefixes(ClassSet{}); got != nil {
		t.Errorf("EngineReservedPrefixes = %v, want nil", got)
	}
}

// TestEveryNonWorkClassHasAFenceableNamespace pins the rule over the whole class
// vocabulary rather than over the classes the other rows happen to name.
//
// Work is the one class that unfences a binding on purpose. Every other class
// has to bring a namespace, because a class that brings none contributes nothing
// to the fence and its own beads are then refused by the binding that holds
// them. Add a coordination class without a row in the reserved-prefix table and
// this fails at the class — not in production at that class's first write.
func TestEveryNonWorkClassHasAFenceableNamespace(t *testing.T) {
	for _, class := range coordclass.Classes() {
		if class == coordclass.ClassWork {
			continue
		}
		if got := fenceFor(t, class); len(got) == 0 {
			t.Errorf("class %v brings no namespace; a binding serving it alongside another class would refuse every bead of its own", class)
		}
	}
}

// TestOnlyWorkUnfencesABinding is the must-be-silent counterpart: the unfencing
// test is on the work CLASS, not on "some class brought no prefix". Rewrite it
// as the latter and a future unregistered class silently unfences every
// namespace it is co-assigned with, which is the failure this shape rules out.
func TestOnlyWorkUnfencesABinding(t *testing.T) {
	for _, class := range coordclass.Classes() {
		got := fenceFor(t, class)
		if class == coordclass.ClassWork {
			if got != nil {
				t.Errorf("a work-serving binding was fenced to %v", got)
			}
			continue
		}
		if got == nil {
			t.Errorf("class %v unfenced the binding; only work may do that", class)
		}
	}
}
