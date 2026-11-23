package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// errClaimViewRead is what failingClaimViewStore returns from Get.
var errClaimViewRead = errors.New("claim view read failed (demand_claim_view_test)")

// failingClaimViewStore answers Get with an error, so a test can ask what the
// probe does when the claim view cannot answer.
type failingClaimViewStore struct {
	beads.Store
}

func (s *failingClaimViewStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errClaimViewRead
}

// seededView is a MemStore with explicit ids, holding one row in each state
// the claim view can hold a cross-store row's copy in.
func seededView(t *testing.T) *beads.MemStore {
	t.Helper()
	view := beads.NewMemStore()
	view.HonorExplicitIDs = true
	create := func(id string) {
		t.Helper()
		if _, err := view.Create(beads.Bead{ID: id, Title: "row " + id, Type: "task", Status: "open"}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	create("open-1")
	create("claimed-1")
	who := "other-session"
	if err := view.Update("claimed-1", beads.UpdateOpts{Assignee: &who}); err != nil {
		t.Fatalf("assigning claimed-1: %v", err)
	}
	create("closed-1")
	if err := view.Close("closed-1"); err != nil {
		t.Fatalf("closing closed-1: %v", err)
	}
	return view
}

// TestClaimViewProbeAsksTheClaimsOwnQuestion is the design's verdict table
// (#5857): a row the claim view does not hold is city-delivered work the claim
// can win (ErrNotFound), a row it holds open and unassigned is winnable, a row
// it holds in any other state (assigned, in progress, closed) is refused by
// the claim CAS before any write, and a read that fails answers nothing.
func TestClaimViewProbeAsksTheClaimsOwnQuestion(t *testing.T) {
	view := seededView(t)
	probe := claimViewProbe{}

	for _, tc := range []struct {
		id       string
		want     claimViewVerdict
		wantErr  error
		wantRead bool
	}{
		{id: "missing-1", want: claimViewWin},
		{id: "open-1", want: claimViewWin, wantRead: true},
		{id: "claimed-1", want: claimViewShadow, wantRead: true},
		{id: "closed-1", want: claimViewShadow, wantRead: true},
	} {
		got, err := probe.check(view, tc.id)
		if got != tc.want {
			t.Fatalf("check(%s) verdict = %v, want %v (err=%v)", tc.id, got, tc.want, err)
		}
		if tc.wantErr == nil && err != nil {
			t.Fatalf("check(%s) err = %v, want nil", tc.id, err)
		}
	}
	if probe.stats.Probed != 4 {
		t.Fatalf("Probed = %d, want 4 (one read per asked id)", probe.stats.Probed)
	}

	got, err := probe.check(&failingClaimViewStore{Store: view}, "open-1")
	if got != claimViewUnknown {
		t.Fatalf("check(failing view) verdict = %v, want %v", got, claimViewUnknown)
	}
	if !errors.Is(err, errClaimViewRead) {
		t.Fatalf("check(failing view) err = %v, want %v", err, errClaimViewRead)
	}
	if probe.stats.Probed != 5 {
		t.Fatalf("Probed = %d, want 5 after the failing read", probe.stats.Probed)
	}

	// A nil view is a leg without a claim view: today's counting, no probe.
	if got, err := probe.check(nil, "open-1"); got != claimViewWin || err != nil {
		t.Fatalf("check(nil) = %v, %v, want %v, nil", got, err, claimViewWin)
	}
	if probe.stats.Probed != 5 {
		t.Fatalf("Probed = %d, want 5: a nil view reads nothing", probe.stats.Probed)
	}
}

// TestClaimViewProbeCachesPerStoreAndID pins the design's cost bound: one
// claim-view read per (store, id) per pass, so a row two of a pool's demand
// legs both ask about costs one `bd show`, not two.
func TestClaimViewProbeCachesPerStoreAndID(t *testing.T) {
	viewA := &recordingClaimViewStore{Store: seededView(t)}
	viewB := &recordingClaimViewStore{Store: seededView(t)}
	probe := claimViewProbe{}

	for i := 0; i < 2; i++ {
		if _, err := probe.check(viewA, "open-1"); err != nil {
			t.Fatalf("check(viewA, open-1) #%d: %v", i+1, err)
		}
	}
	if got := viewA.getIDs(); len(got) != 1 || got[0] != "open-1" {
		t.Fatalf("viewA read %v for the same (store, id) twice, want one read: %v", got, got)
	}
	if probe.stats.Probed != 1 {
		t.Fatalf("Probed = %d, want 1: the second ask of the same (store, id) is served from the cache", probe.stats.Probed)
	}

	// The cache is per (store, id): another view, or another id on the same
	// view, is a new question with a new read.
	if _, err := probe.check(viewB, "open-1"); err != nil {
		t.Fatalf("check(viewB, open-1): %v", err)
	}
	if _, err := probe.check(viewA, "closed-1"); err != nil {
		t.Fatalf("check(viewA, closed-1): %v", err)
	}
	if want := []string{"open-1", "closed-1"}; len(viewA.getIDs()) != 2 || viewA.getIDs()[0] != "open-1" {
		t.Fatalf("viewA reads = %v, want %v", viewA.getIDs(), want)
	}
	if got := viewB.getIDs(); len(got) != 1 {
		t.Fatalf("viewB reads = %v, want one read", got)
	}
	if probe.stats.Probed != 3 {
		t.Fatalf("Probed = %d, want 3", probe.stats.Probed)
	}
}

// TestDemandClaimViewReadFailureWithholdsTheRowAndMarksPartial is the design's
// error row (#5857): a cross-store row whose claim-view read fails with an
// error that is not not-found is a row the pass cannot prove the claim can win,
// so it is not counted — and the template's count is marked partial with an
// error naming the template, the row, the store the row was counted from, and
// the cause, so the decide retains capacity instead of silently shrinking it.
func TestDemandClaimViewReadFailureWithholdsTheRowAndMarksPartial(t *testing.T) {
	cfg := agreementConfig()
	cityStore := beads.NewMemStore()
	cityStore.HonorExplicitIDs = true
	rigStore := beads.NewMemStore()
	rigStore.HonorExplicitIDs = true

	// E: open, unassigned and routed in the city store, with a claim view that
	// cannot answer for it (a read failure that is not not-found).
	if _, err := cityStore.Create(beads.Bead{
		ID: "e", Title: "row e", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
	}); err != nil {
		t.Fatalf("seeding e: %v", err)
	}

	targets := []defaultScaleCheckTarget{
		{template: agreementTemplate, store: rigStore, storeKey: "rig:rig"},
	}
	targets = append(targets, crossStoreDemandLegs([]defaultScaleCheckTarget{
		{template: agreementTemplate, store: cityStore, storeKey: "city"},
	}, &failingClaimViewStore{Store: rigStore})...)

	counts, demand, partialTemplates, errs, _ := defaultScaleCheckCountsAndDemand(cfg, targets, newReadyDemandCache())

	if got := counts[agreementTemplate]; got != 0 {
		t.Fatalf("counts[%s] = %d, want 0: an unproven cross-store row is not demand", agreementTemplate, got)
	}
	entry := demand[agreementTemplate]
	for _, id := range []string{"e"} {
		if slices.Contains(entry.WorkBeadIDs, id) {
			t.Fatalf("WorkBeadIDs lists %s: the pass could not prove the claim can win it", id)
		}
		if _, listed := entry.StoreRefs[id]; listed {
			t.Fatalf("StoreRefs lists %s: an unproven cross-store row is not demand", id)
		}
	}
	if !partialTemplates[agreementTemplate] {
		t.Fatalf("partialTemplates = %v, want %s marked partial: its count is a partial of one demand source", partialTemplates, agreementTemplate)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly the claim-view read failure", errs)
	}
	if !errors.Is(errs[0], errClaimViewRead) {
		t.Fatalf("errs[0] = %v, want it to wrap the read failure %v", errs[0], errClaimViewRead)
	}
	for _, want := range []string{agreementTemplate, "e", "city", errClaimViewRead.Error()} {
		if !strings.Contains(errs[0].Error(), want) {
			t.Fatalf("errs[0] = %q, want it to name %q", errs[0], want)
		}
	}
}

// TestCrossStoreDemandLegsCarryOwnClaimView pins the design's leg rule
// (#5857): the only demand legs that carry a claim view are the cross-store
// (city) legs appended for a rig pool — a pool on the generic default probe
// and a rig pool backing a named session on the on-demand probe — and each
// carries that pool's own store, where a seat of the pool resolves the leg's
// ids when it claims. Every other leg (a pool's own store, a city-scoped
// pool, a store-scoped control dispatcher) carries none, so its counting is
// today's.
func TestCrossStoreDemandLegsCarryOwnClaimView(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{
			// A generic rig pool on the default probe.
			{Name: "worker", Dir: "gascity", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			// A rig pool that backs a named session (on-demand).
			{Name: "nworker", Dir: "gascity", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
			// A city-scoped pool: its own store IS the city store.
			{Name: "cityworker", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
			// A store-scoped control dispatcher on a rig.
			{Name: config.ControlDispatcherAgentName, Dir: "gascity", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
		NamedSessions: []config.NamedSession{{Template: "nworker", Dir: "gascity", Mode: "on_demand"}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": rigStore}

	targets := buildDemandTargets("gc", cityPath, cfg, cityStore, rigStores, nil, nil, noProbeEnv, io.Discard)

	legsByTemplate := func(list []defaultScaleCheckTarget, template string) map[string]defaultScaleCheckTarget {
		out := map[string]defaultScaleCheckTarget{}
		for _, leg := range list {
			if leg.template == template {
				out[leg.storeKey] = leg
			}
		}
		return out
	}
	// assertPoolLegs checks one rig pool's legs in one target list: an own
	// leg on the pool's own store with no claim view, and a cross-store city
	// leg whose claim view is that pool's own store.
	assertPoolLegs := func(list []defaultScaleCheckTarget, template string) {
		t.Helper()
		legs := legsByTemplate(list, template)
		own, ok := legs["rig:gascity"]
		if !ok {
			t.Fatalf("%s: no own rig leg (legs=%v)", template, legs)
		}
		if own.claimView != nil {
			t.Fatalf("%s: own leg carries claim view %v: a pool's own store needs no probe to ask the claim's question", template, own.claimView)
		}
		city, ok := legs["city"]
		if !ok {
			t.Fatalf("%s: no cross-store city leg: routed demand delivered there would be invisible to the pool (legs=%v)", template, legs)
		}
		if city.claimView == nil {
			t.Fatalf("%s: city leg carries no claim view: its rows would be counted without asking whether the claim can win them", template)
		}
		if !beads.SameStore(city.claimView, rigStore) {
			t.Fatalf("%s: city leg's claim view = %v, want the pool's own rig store, where the seat's claim resolves ids", template, city.claimView)
		}
	}

	// The generic pool's legs are in the pool pipeline only.
	assertPoolLegs(targets.defaultScaleTargets, "gascity/worker")
	// The named-backing pool's legs are in both lists: the pool pipeline's
	// copy is the one that counts, and the named list's copy preserves the
	// same legs for partial-query retention.
	assertPoolLegs(targets.defaultScaleTargets, "gascity/nworker")
	assertPoolLegs(targets.defaultNamedScaleTargets, "gascity/nworker")

	// A city-scoped pool has one leg, its own, on the city store, with no
	// claim view: no cross-store delivery applies to it.
	cityLegs := legsByTemplate(targets.defaultScaleTargets, "cityworker")
	if len(cityLegs) != 1 {
		t.Fatalf("cityworker: legs = %v, want exactly its own city leg", cityLegs)
	}
	if cityLegs["city"].claimView != nil {
		t.Fatalf("cityworker: own city leg carries claim view %v: a city-scoped pool owns its own store", cityLegs["city"].claimView)
	}
	// A store-scoped control dispatcher keeps its legs on the stores it owns:
	// its own rig leg, and no city leg, none with a claim view.
	dispatcherLegs := legsByTemplate(targets.defaultScaleTargets, "gascity/"+config.ControlDispatcherAgentName)
	if len(dispatcherLegs) != 1 {
		t.Fatalf("control dispatcher: legs = %v, want exactly its own rig leg", dispatcherLegs)
	}
	if dispatcherLegs["rig:gascity"].claimView != nil {
		t.Fatalf("control dispatcher: own leg carries claim view %v", dispatcherLegs["rig:gascity"].claimView)
	}

	// No leg anywhere carries a claim view other than a rig pool's own store.
	for _, list := range [][]defaultScaleCheckTarget{targets.defaultScaleTargets, targets.defaultNamedScaleTargets} {
		for _, leg := range list {
			if leg.claimView != nil && !beads.SameStore(leg.claimView, rigStore) {
				t.Fatalf("leg %s/%s carries unexpected claim view %v", leg.template, leg.storeKey, leg.claimView)
			}
		}
	}
}
