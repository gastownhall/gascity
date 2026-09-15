package storeref

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// A rig leg whose two prefixes disagree is reported, and the report names both
// so a consumer can say which rig to reconcile.
func TestPrefixFaultsReportsARigWhoseDeclaredPrefixIsNotItsConfiguredOne(t *testing.T) {
	rig := newPrefixed("ga")
	topo := Topology{
		Work: Leg{Ref: WorkRef, Store: newPrefixed("gcg"), Prefix: "gcg"},
		Rigs: []Leg{{Ref: RigRef("frontend"), Store: rig, Prefix: "fe"}},
	}

	faults := topo.PrefixFaults()
	if len(faults) != 1 {
		t.Fatalf("PrefixFaults() = %v, want exactly the frontend rig", faults)
	}
	want := PrefixFault{Ref: RigRef("frontend"), Configured: "fe", Declared: "ga"}
	if faults[0] != want {
		t.Fatalf("PrefixFaults()[0] = %+v, want %+v", faults[0], want)
	}
	for _, part := range []string{"rig:frontend", `"fe"`, `"ga"`} {
		if !strings.Contains(faults[0].String(), part) {
			t.Errorf("PrefixFault.String() = %q, want it to name %s", faults[0].String(), part)
		}
	}
}

// The bounding rows. Each is a leg the by-id plan may well gate out, and none of
// them is a FAULT: a consumer that treated them as one would be unable to prove
// absence on any ordinary multi-rig city.
func TestPrefixFaultsIsSilentOnEveryLegThatContradictsNothing(t *testing.T) {
	work := newPrefixed("gcg")
	for _, tc := range []struct {
		name string
		rig  Leg
	}{
		{
			// The ordinary gated-out leg: the plan skips it for an id outside
			// "fe", and it has said nothing to make that skip wrong.
			name: "prefixes agree",
			rig:  Leg{Ref: RigRef("frontend"), Store: newPrefixed("fe"), Prefix: "fe"},
		},
		{
			// A bd/Dolt work store with no configured mint prefix. It has made
			// no claim to contradict, and faulting it would fault most cities.
			name: "store declares an empty prefix",
			rig:  Leg{Ref: RigRef("frontend"), Store: newPrefixed(""), Prefix: "fe"},
		},
		{
			name: "store declares no prefix at all",
			rig:  Leg{Ref: RigRef("frontend"), Store: beads.NewMemStore(), Prefix: "fe"},
		},
		{
			name: "no store",
			rig:  Leg{Ref: RigRef("frontend"), Prefix: "fe"},
		},
		{
			// dedupeLegs collapses this leg onto the work leg, which every by-id
			// plan reads unconditionally, so no prefix gates it out of anything.
			name: "the rig leg IS the work store",
			rig:  Leg{Ref: RigRef("frontend"), Store: work, Prefix: "fe"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			topo := Topology{Work: Leg{Ref: WorkRef, Store: work, Prefix: "gcg"}, Rigs: []Leg{tc.rig}}
			if faults := topo.PrefixFaults(); len(faults) != 0 {
				t.Fatalf("PrefixFaults() = %v, want none", faults)
			}
		})
	}
}

// Faults come back in the plan's own rig order, so two reports of one city read
// the same way twice.
func TestPrefixFaultsFollowsThePlansRigOrder(t *testing.T) {
	topo := Topology{
		Work: Leg{Ref: WorkRef, Store: newPrefixed("gcg"), Prefix: "gcg"},
		Rigs: []Leg{
			{Ref: RigRef("frontend"), Store: newPrefixed("ga"), Prefix: "fe"},
			{Ref: RigRef("backend"), Store: newPrefixed("ga"), Prefix: "be"},
		},
	}

	faults := topo.PrefixFaults()
	if len(faults) != 2 {
		t.Fatalf("PrefixFaults() = %v, want both rigs", faults)
	}
	if faults[0].Ref != RigRef("backend") || faults[1].Ref != RigRef("frontend") {
		t.Fatalf("PrefixFaults() refs = %q, %q, want rig-ref ascending", faults[0].Ref, faults[1].Ref)
	}
}
