package api

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/storeref"
)

// t7dEpochState holds one Stores epoch until remint, as a production holder
// does (cmd/gc's storesEpochCache re-mints on a change of routes, rig set or
// city store, not on a relic-free file appearing).
type t7dEpochState struct {
	relicReportingState
	epoch storeref.Stores
}

func (s *t7dEpochState) Stores() storeref.Stores { return s.epoch }

func (s *t7dEpochState) remint() { s.epoch = s.relicReportingState.Stores() }

// t7dGateOff runs a pin with the split-strict gate off. The pins here hold
// the stores a reader reads on the shapes a city serves; under the gate a
// work view hides the graph rows the fixtures seed in a city or rig store, and
// a single-store city's class view is a graph leg of its own, so the answers
// differ on both sides alike. TestWorkflowLegsMatchTheFrozenReaders runs every
// reader, frozen and live, gate on and off.
func t7dGateOff(t *testing.T) {
	t.Helper()
	t.Setenv(splittest.StrictEnv, "")
}

func t7dRefs(infos []workflowStoreInfo) string {
	refs := make([]string, 0, len(infos))
	for _, info := range infos {
		refs = append(refs, info.ref)
	}
	return strings.Join(refs, " ")
}

// TestWorkflowLegsReadARigRetiredAfterMintUntilReMint pins where the workflow
// legs take the S4 relic-free bit: from the epoch, minted once
// (Leg.RelicFree), where the hand-rolled list asked RigGraphRelicFree on
// every read. A rig proven relic-free after the epoch was minted is still
// read as a relic leg until the next epoch, which costs one relic read a
// workflow read and finds nothing there; the bit never reverts within an
// epoch (S5-G's --to-rigs re-mints it). A re-mint retires the leg.
func TestWorkflowLegsReadARigRetiredAfterMintUntilReMint(t *testing.T) {
	t7dGateOff(t)
	base, _, rig := newS4APIState(t, true)
	legacy := workflowRoot(t, rig, "pre-S4 rig root")
	state := &t7dEpochState{relicReportingState: base}
	state.remint()

	base.relicFree["myrig"] = true
	if !base.RigGraphRelicFree("myrig") {
		t.Fatal("the fixture did not prove the rig relic-free")
	}
	if got := t7dRefs(workflowStores(state)); !strings.Contains(got, "rig:myrig") {
		t.Errorf("workflowStores in the epoch minted before the proof = %s, want the rig still read", got)
	}
	if !isGraphConvoyID(New(state), legacy.ID) {
		t.Error("isGraphConvoyID dropped the rig leg mid-epoch")
	}

	state.remint()
	if got := t7dRefs(workflowStores(state)); strings.Contains(got, "rig:myrig") {
		t.Errorf("workflowStores after a re-mint = %s, want the relic-free rig retired", got)
	}
	if isGraphConvoyID(New(state), legacy.ID) {
		t.Error("isGraphConvoyID still reads a relic-free rig after a re-mint")
	}
	if got := t7dRefs(workflowStoresIncludingRetiredRigs(state)); !strings.Contains(got, "rig:myrig") {
		t.Errorf("workflowStoresIncludingRetiredRigs = %s, want the retired rig kept for rig work", got)
	}
}

// TestIsGraphConvoyIDAsksTheStoresInNameOrder pins isGraphConvoyID's leg
// order, kept from its BeadStores() walk: after the graph binding, the city
// and rig legs by name, the city at its own name's sorted position, and the
// first leg that reads the id answers. An id a rig sorting before the city
// holds as plain work, and the city as a workflow root, is the rig's: not a
// graph convoy. The same pair with the rig sorting after the city is the
// city's.
func TestIsGraphConvoyIDAsksTheStoresInNameOrder(t *testing.T) {
	t7dGateOff(t)
	for _, tc := range []struct {
		rig  string
		want bool
	}{
		{rig: "alpha", want: false}, // alpha < test-city: the rig answers
		{rig: "zeta", want: true},   // test-city < zeta: the city answers
	} {
		state := newFakeState(t)
		state.cityName = t7dCity
		city := beads.NewMemStore()
		state.cityBeadStore = city
		cityRoot, err := city.Create(beads.Bead{Title: "graph", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
		if err != nil {
			t.Fatal(err)
		}
		pinned := &beads.MemStore{HonorExplicitIDs: true}
		if _, err := pinned.Create(beads.Bead{ID: cityRoot.ID, Title: "plain", Type: "task"}); err != nil {
			t.Fatal(err)
		}
		state.stores = map[string]beads.Store{tc.rig: pinned}
		if got := isGraphConvoyID(New(state), cityRoot.ID); got != tc.want {
			t.Errorf("rig %s: isGraphConvoyID(%s) = %v, want %v (the first store in name order answers)", tc.rig, cityRoot.ID, got, tc.want)
		}
		if got := frozenIsGraphConvoyID(New(state), cityRoot.ID); got != tc.want {
			t.Errorf("rig %s: the frozen isGraphConvoyID(%s) = %v, want %v", tc.rig, cityRoot.ID, got, tc.want)
		}
	}
}

// TestWorkflowLegsSkipARigNamedLikeTheCity pins the workflow scan's skip of a
// rig named like the city (the BeadStores() map it ranged lists that rig in
// the city's place, and it skipped the name), while a ref still names the rig
// and isGraphConvoyID reads it in the city's place, as before.
func TestWorkflowLegsSkipARigNamedLikeTheCity(t *testing.T) {
	t7dGateOff(t)
	fx := t7dShapes["unsplit, rig named like city"].fixtures(t)()
	if got := t7dRefs(workflowStores(fx.state)); strings.Contains(got, "rig:"+t7dCity) || !strings.Contains(got, "city:"+t7dCity) {
		t.Errorf("workflowStores = %s, want the city and no rig named like it", got)
	}
	info, ok := workflowStoreByRef(fx.state, "rig:"+t7dCity)
	if !ok || fx.name(info.leg.Reader) != "named" {
		t.Errorf("workflowStoreByRef(rig:%s) = %v, %s; want the rig's own store", t7dCity, ok, fx.t7dInfo(info, ok))
	}
	// co-1 is a workflow root in the city and in the rig named like it, and
	// plain work in alpha, which sorts first: alpha answers, so not a graph
	// convoy. tx7-wf lives only in that rig: it is read in the city's place.
	s := New(fx.state)
	if isGraphConvoyID(s, "co-1") || !isGraphConvoyID(s, "tx7-wf") {
		t.Errorf("isGraphConvoyID(co-1, tx7-wf) = (%v, %v), want (false, true)", isGraphConvoyID(s, "co-1"), isGraphConvoyID(s, "tx7-wf"))
	}
}

// TestIsGraphConvoyIDReadsNoCityBehindARelicFreeRigNamedLikeIt pins
// BeadStores()' shadow: a rig named like the city replaces the city in the
// map isGraphConvoyID walked, so once that rig is relic-free (its leg
// retired) neither store is read there. A graph run stranded in the city
// store is then not a graph convoy here: the convoy GET and DELETE answer
// 404, as before, rather than serving or tearing down its tree.
func TestIsGraphConvoyIDReadsNoCityBehindARelicFreeRigNamedLikeIt(t *testing.T) {
	t7dGateOff(t)
	fx := t7dShapes["stranded, relic-free rig named like city"].fixtures(t)()
	s := New(fx.state)
	if isGraphConvoyID(s, "tc3-wf") || frozenIsGraphConvoyID(s, "tc3-wf") {
		t.Errorf("isGraphConvoyID(tc3-wf) = %v, frozen %v; want false: the city sits behind a retired rig named like it", isGraphConvoyID(s, "tc3-wf"), frozenIsGraphConvoyID(s, "tc3-wf"))
	}
	if !isGraphConvoyID(s, "gcg1-wf") {
		t.Error("the binding's graph convoy is no longer recognized")
	}
}

// TestWorkflowLegsReadASuspendedRig pins the suspended-rig choice every
// workflow reader states (storeref.WorkflowReadsSuspendedRigs): each read
// BeadStores(), which lists a suspended rig, so a suspended rig's roots are
// still found, deleted and canceled.
func TestWorkflowLegsReadASuspendedRig(t *testing.T) {
	t7dGateOff(t)
	for _, shape := range []string{"unsplit, suspended rig", "whole split, suspended rig"} {
		fx := t7dShapes[shape].fixtures(t)()
		if !fx.state.Stores().Topology().Rigs[1].Suspended {
			t.Fatalf("%s: the fixture's epoch does not mark beta suspended", shape)
		}
		if got := t7dRefs(workflowStores(fx.state)); !strings.Contains(got, "rig:beta") {
			t.Errorf("%s: workflowStores = %s, want the suspended rig read", shape, got)
		}
		if _, err := New(fx.state).buildWorkflowSnapshot("run-5", "", "", 0); err != nil {
			t.Errorf("%s: the suspended rig's run: %v", shape, err)
		}
	}
}

// TestWorkflowLegsReadARigOverTheCityStoreOnce is the declared divergence of
// the shared-store shape: a rig whose store is the city's own. The hand list
// named that store twice, as city:<city> and rig:<name>, and read it under
// both; a plan reads a store once, so the legs name it once, as the city. A
// ref naming the rig still resolves, to that one leg, under the rig's own ref
// and scope (workflowLegNamed), as BeadStore(name) did, so a store_ref a
// client holds keeps resolving.
func TestWorkflowLegsReadARigOverTheCityStoreOnce(t *testing.T) {
	t7dGateOff(t)
	state := newFakeState(t)
	state.cityName = t7dCity
	city := beads.NewMemStore()
	state.cityBeadStore = city
	state.stores = map[string]beads.Store{"alpha": city}

	if got, want := t7dRefs(workflowStores(state)), "city:"+t7dCity; got != want {
		t.Errorf("workflowStores = %s, want %s", got, want)
	}
	if got, want := t7dRefs(frozenWorkflowStores(state)), "city:"+t7dCity+" rig:alpha"; got != want {
		t.Errorf("the frozen workflowStores = %s, want %s", got, want)
	}
	info, ok := workflowStoreByRef(state, "rig:alpha")
	if !ok || info.ref != "rig:alpha" || info.scopeKind != beadmeta.ScopeKindRig || info.scopeRef != "alpha" || !beads.SameStore(info.leg.Reader, city) || !beads.SameStore(info.store, city) {
		t.Errorf("workflowStoreByRef(rig:alpha) = %v, %+v; want the shared leg under rig:alpha", ok, info)
	}
}

// TestWorkflowLegsMintTheHandleOnTheEntrysOwnStore pins how workflowLegs'
// resident picks its leg: by the entry's store, never by position. Each
// entry's handle writes to the store its leg reads, a rig named like the
// city (a leg the scan skips, sorting first here) included; an entry with no
// leg (an order entry, a zero entry) gets the zero handle rather than the
// plan's first leg, the graph binding.
func TestWorkflowLegsMintTheHandleOnTheEntrysOwnStore(t *testing.T) {
	t7dGateOff(t)
	fx := t7dShapes["skipped rig sorts first, split"].fixtures(t)()
	legs, resident := workflowLegs(fx.state, workflowLegQuery{keepRigNamedLikeCity: true})
	scanned, _ := workflowLegs(fx.state, workflowLegQuery{})
	for _, info := range append(legs, scanned...) {
		if got, want := fx.name(resident(info)), fx.name(info.leg.Reader); got != want {
			t.Errorf("%s: the writable handle writes to %s, its leg reads %s", info.ref, got, want)
		}
	}
	if zero := resident(workflowStoreInfo{}); !beads.IsNilStore(zero) {
		t.Errorf("an entry with no leg got a handle on %s, want none", fx.name(zero))
	}
}
