package storeref

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The T7b.3 storeref pieces: the WorkFederation intent (its corpus rows are in
// stores_plan_test.go), BindLeg, and the sessions leg (SessionsLeg,
// SessionStoreOn). The API's leg selection against today's leg builders is
// internal/api's work_federation_legs_equivalence_test.go.

// TestWorkFederationRefusesAnUnknownKind: a kind the planner does not know is
// a plan error, not an empty or a default plan.
func TestWorkFederationRefusesAnUnknownKind(t *testing.T) {
	s := NewStores(newT2().topo, StoresOptions{})
	if _, err := s.Plan(WorkFederation{Kind: WorkKind(9), SuspendedRigs: WorkReadsSuspendedRigs}); err == nil {
		t.Error("an unknown work kind planned")
	}
}

// TestWorkFederationIsNotATopologyQuestion: the plane topology planner
// refuses a WorkFederation, as it does every Stores.Plan intent: relocation is
// the epoch's handle identity, never a topology's bindings.
func TestWorkFederationIsNotATopologyQuestion(t *testing.T) {
	if _, err := Plan(WorkFederation{Kind: WorkList, SuspendedRigs: WorkReadsSuspendedRigs}, newT2().topo); err == nil {
		t.Error("Plan(topology) planned a WorkFederation")
	}
}

// TestWorkFederationSkipsByRefNotBySuspension: Skip is the caller's census.
// A rig the epoch marks suspended is read unless Skip names it, and a rig
// Skip names is dropped whatever the epoch says.
func TestWorkFederationSkipsByRefNotBySuspension(t *testing.T) {
	f := newT2()
	for i := range f.topo.Rigs {
		if f.topo.Rigs[i].Ref == RigRef("bravo") {
			f.topo.Rigs[i].Suspended = true
		}
	}
	s := NewStores(f.topo, StoresOptions{})
	refs := func(in WorkFederation) string {
		plan, err := s.Plan(in)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		EachTypedLeg(plan, func(l TypedLeg) { out = append(out, legName(l.Ref)) })
		return fmt.Sprint(out)
	}
	if got, want := refs(WorkFederation{Kind: WorkSweep, SuspendedRigs: WorkSkipsRigs, Skip: []StoreRef{RigRef("alpha")}}), `[the city work store rig:bravo class:gmnos]`; got != want {
		t.Errorf("Sweep skipping alpha = %s, want %s", got, want)
	}
	if got, want := refs(WorkFederation{Kind: WorkList, CityName: "c", SuspendedRigs: WorkSkipsRigs}), `[rig:alpha rig:bravo the city work store class:gmnos]`; got != want {
		t.Errorf("List skipping nothing = %s, want %s", got, want)
	}
}

// TestWorkFederationDropsASkippedRigBeforeDedupe: a skipped rig whose store
// another read rig shares leaves that store read, through the rig that is
// read (the sweep's and the convoy readers' rule, and status's from T7e.2).
func TestWorkFederationDropsASkippedRigBeforeDedupe(t *testing.T) {
	f := newT2()
	for i := range f.topo.Rigs {
		if f.topo.Rigs[i].Ref == RigRef("bravo") {
			f.topo.Rigs[i].Store = f.rigs["alpha"]
		}
	}
	s := NewStores(f.topo, StoresOptions{})
	plan, err := s.Plan(WorkFederation{Kind: WorkList, CityName: "c", SuspendedRigs: WorkSkipsRigs, Skip: []StoreRef{RigRef("alpha")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.String(), `Union(first-leg-wins): rig:bravo[Authority,Fatal,federated] > ""[Authority,Fatal,federated] > class:gmnos[FederationTail,Fatal,federated]`; got != want {
		t.Errorf("plan = %s\nwant   %s", got, want)
	}
}

// TestWorkFederationLegsReadFederated: every leg's reader is federated, the
// strict reader declares nothing, and each reads its own store.
func TestWorkFederationLegsReadFederated(t *testing.T) {
	for _, opts := range []StoresOptions{{}, {Projection: taggedProjection{}}} {
		f := newT2()
		s := NewStores(f.topo, opts)
		for _, in := range []WorkFederation{
			{Kind: WorkList, CityName: "c", SuspendedRigs: WorkReadsSuspendedRigs},
			{Kind: WorkSweep, SuspendedRigs: WorkReadsSuspendedRigs},
			{Kind: WorkScope, Scope: RigRef("alpha")},
		} {
			plan, err := s.Plan(in)
			if err != nil {
				t.Fatal(err)
			}
			EachTypedLeg(plan, func(l TypedLeg) {
				if l.Reader.Role() != beads.FederatedLegRole || l.Strict.Role() != beads.NoLegRole {
					t.Errorf("%s %s: reader %v, strict %v; want federated, none", in.Kind, legName(l.Ref), l.Reader.Role(), l.Strict.Role())
				}
				if !beads.SameStore(l.Reader, l.planned.Leg.Store) || !beads.SameStore(l.Strict, l.planned.Leg.Store) {
					t.Errorf("%s %s: a reader is not over the leg's store", in.Kind, legName(l.Ref))
				}
			})
		}
	}
}

// TestSweepWritesThroughTheWorkSweepLegs: Sweep (T7b's executor, unchanged)
// over WorkSweep writes into each leg's own store, the graph and sessions
// legs' stores being the ones under the epoch's class handles, and touches no
// skipped rig.
func TestSweepWritesThroughTheWorkSweepLegs(t *testing.T) {
	f := newT2()
	s := NewStores(f.topo, StoresOptions{})
	plan, err := s.Plan(WorkFederation{Kind: WorkSweep, SuspendedRigs: WorkSkipsRigs, Skip: []StoreRef{RigRef("bravo")}})
	if err != nil {
		t.Fatal(err)
	}
	var swept []string
	if _, err := Sweep(plan, func(l TypedLeg, w beads.ResidentStore) error {
		if _, err := w.Create(beads.Bead{ID: fmt.Sprintf("sw-%d", len(swept)), Title: "swept"}); err != nil {
			return err
		}
		swept = append(swept, storeNameOf(l.planned.Leg.Store))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(swept), `[work rig:alpha class:gmnos]`; got != want {
		t.Errorf("swept %s, want %s", got, want)
	}
	if !beads.SameStore(s.Graph(), f.bindings["class:gmnos"]) || !beads.SameStore(s.Sessions(), f.bindings["class:gmnos"]) {
		t.Fatal("fixture: graph and sessions are not the one binding")
	}
	if _, err := f.bindings["class:gmnos"].MemStore.Get("sw-2"); err != nil {
		t.Errorf("the graph/sessions leg's write did not land in the binding: %v", err)
	}
	if rows, _ := f.rigs["bravo"].List(beads.ListQuery{AllowScan: true}); len(rows) != 0 {
		t.Errorf("the skipped rig was written: %v", rows)
	}
}

// TestBindLegKeepsTheShape: a bound leg reads and writes the bind's store and
// keeps its ref, role, policy and gate roles; the leg it was given still reads
// its own store; it is BindLegs over the one leg.
func TestBindLegKeepsTheShape(t *testing.T) {
	f := newT2()
	s := NewStores(f.topo, StoresOptions{})
	plan, err := s.Plan(WorkFederation{Kind: WorkSweep, SuspendedRigs: WorkReadsSuspendedRigs})
	if err != nil {
		t.Fatal(err)
	}
	clone := newNamedStore("clone")
	clone.seed(t, "cl-1")
	bind := func(s beads.Store) (beads.Store, error) {
		if beads.IsNilStore(s) {
			return nil, errors.New("no store to bind")
		}
		return clone, nil
	}
	whole, err := BindLegs(plan, bind)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	EachTypedLeg(plan, func(l TypedLeg) {
		bound, err := BindLeg(l, bind)
		if err != nil {
			t.Fatal(err)
		}
		if bound.Ref != l.Ref || bound.Role != l.Role || bound.OnError != l.OnError {
			t.Errorf("leg %s: bound %v/%v/%v, planned %v/%v/%v", legName(l.Ref), bound.Ref, bound.Role, bound.OnError, l.Ref, l.Role, l.OnError)
		}
		if bound.Reader.Role() != l.Reader.Role() || bound.Strict.Role() != beads.NoLegRole {
			t.Errorf("leg %s: bound reader %v, strict %v; want %v, none", legName(l.Ref), bound.Reader.Role(), bound.Strict.Role(), l.Reader.Role())
		}
		if !beads.SameStore(bound.Reader, clone) || !beads.SameStore(bound.Strict, clone) {
			t.Errorf("leg %s: a bound reader is not over the clone", legName(l.Ref))
		}
		if _, err := bound.Reader.Get("cl-1"); err != nil {
			t.Errorf("leg %s: the bound reader does not read the clone: %v", legName(l.Ref), err)
		}
		if beads.SameStore(l.Reader, clone) || beads.SameStore(l.Strict, clone) {
			t.Errorf("leg %s: BindLeg rebound the leg it was given", legName(l.Ref))
		}
		if bound.planned != whole.Legs[i] {
			t.Errorf("leg %s: BindLeg %+v, BindLegs %+v", legName(l.Ref), bound.planned, whole.Legs[i])
		}
		i++
	})
	errDown := errors.New("down")
	EachTypedLeg(plan, func(l TypedLeg) {
		if _, err := BindLeg(l, func(beads.Store) (beads.Store, error) { return nil, errDown }); !errors.Is(err, errDown) {
			t.Errorf("a failing bind = %v, want its error", err)
		}
		if _, err := BindLeg(l, func(beads.Store) (beads.Store, error) { return nil, nil }); err == nil {
			t.Error("a bind that answered no store was accepted")
		}
	})
	if _, err := BindLeg(TypedLeg{}, bind); err == nil {
		t.Error("BindLeg bound a zero leg")
	}
}

// TestSessionsLegHandsOutTheSessionsHandle: the sessions leg is the epoch's
// sessions placement, read strict; SessionStoreOn gives the session handle
// over it, or over the instance BindLeg bound it to, and over no other leg.
func TestSessionsLegHandsOutTheSessionsHandle(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    topoFixture
		opts StoresOptions
	}{
		{"T0", newT0(), StoresOptions{}},
		{"T0g", newT0(), StoresOptions{Projection: taggedProjection{}}},
		{"T2", newT2(), StoresOptions{}},
	} {
		s := NewStores(tc.f.topo, tc.opts)
		leg, ok := s.SessionsLeg()
		if !ok {
			t.Fatalf("%s: no sessions leg", tc.name)
		}
		if leg.Reader.Role() != beads.NoLegRole || leg.Strict.Role() != beads.NoLegRole {
			t.Errorf("%s: sessions leg reads %v/%v, want strict", tc.name, leg.Reader.Role(), leg.Strict.Role())
		}
		h, err := SessionStoreOn(leg)
		if err != nil {
			t.Fatal(err)
		}
		if !beads.SameStore(h, s.Sessions()) || !beads.SameStore(leg.Reader, s.Sessions()) {
			t.Errorf("%s: the sessions handle is not over the epoch's sessions store", tc.name)
		}
		clone := newNamedStore("clone")
		bound, err := BindLeg(leg, func(beads.Store) (beads.Store, error) { return clone, nil })
		if err != nil {
			t.Fatal(err)
		}
		if h, err := SessionStoreOn(bound); err != nil || !beads.SameStore(h, clone) {
			t.Errorf("%s: SessionStoreOn(bound) = %v, %v; want a handle over the clone", tc.name, h, err)
		}
		plan, err := s.Plan(WorkFederation{Kind: WorkScope, Scope: WorkRef})
		if err != nil {
			t.Fatal(err)
		}
		EachTypedLeg(plan, func(l TypedLeg) {
			if _, err := SessionStoreOn(l); err == nil {
				t.Errorf("%s: SessionStoreOn took the city work leg", tc.name)
			}
		})
		if _, err := SessionStoreOn(TypedLeg{}); err == nil {
			t.Errorf("%s: SessionStoreOn took a zero leg", tc.name)
		}
	}
	if _, ok := NewStores(Topology{}, StoresOptions{}).SessionsLeg(); ok {
		t.Error("an epoch with no sessions store has a sessions leg")
	}
}
