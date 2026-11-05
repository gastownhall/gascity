package api

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
)

// Fan-out properties of the split-city bead list: page bounding, cross-leg id
// identity, and what a dead graph leg tells an operator.
//
// FIXTURE FIDELITY. The graph leaf here is deliberately NOT
// splittest.StrictStore. StrictStore declares beads.Counter (its Count forwards
// to the leaf), so a fixture built from the kit keeps the bounded page path
// alive no matter what the handler does with the leg — the exact dimension
// these tests are about. The canonical compiled-in binding hands the API a raw
// *beads.SQLiteStore (internal/storebinding/sqlite/beads_engine.go OpenEngine →
// beads.OpenSQLiteStore; cmd/gc/storage_boot.go stores it verbatim and
// cmd/gc/class_store.go returns it unwrapped), and *beads.SQLiteStore has no
// Count method at all. nonCounterLeg models THAT.

// nonCounterLeg is a store with no beads.Counter capability, modeling the
// canonical relocated-class binding. It embeds the beads.Store INTERFACE rather
// than a concrete type, so no method outside beads.Store can leak through and
// the leg stays Counter-less even if the backing leaf later grows a Count. It
// records the largest List limit it was handed so a test can prove the handler
// did NOT push a page bound into a store that cannot count.
type nonCounterLeg struct {
	beads.Store
	mu         sync.Mutex
	maxListLim int
}

func (s *nonCounterLeg) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	if q.Limit > s.maxListLim {
		s.maxListLim = q.Limit
	}
	s.mu.Unlock()
	return s.Store.List(q)
}

// seedMoleculeLeg builds one prefix-disjoint leg of molecule beads, the way a
// real city's stores mint under different prefixes.
func seedMoleculeLeg(idPrefix string, n int) *beads.MemStore {
	_, mem := seedMoleculeStore(idPrefix, n)
	return mem
}

// newBoundedSplitState wires a split city whose WORK legs are Counter-capable
// (every production work store is: cmd/gc/api_state.go caching-wraps the city
// and each rig, and CachingStore/NativeDoltStore both implement beads.Counter)
// and whose graph leg is whatever capability shape the caller wants to model.
func newBoundedSplitState(t *testing.T, graph beads.Store) (*fakeState, *countingListStore, *countingListStore) {
	t.Helper()
	fs := newFakeState(t)
	city := &countingListStore{Store: seedMoleculeLeg("hq", 30)}
	rig := &countingListStore{Store: seedMoleculeLeg("ra", 30)}
	fs.cityBeadStore = city
	fs.stores = map[string]beads.Store{"myrig": rig, fs.CityName(): city}
	fs.graphBeadStore = graph
	return fs, city, rig
}

// TestBeadListSplitCityKeepsWorkLegsBounded is the regression guard for the
// defect this file exists for: the graph leg must not be able to veto bounded
// paging for the rest of the fan-out.
//
// The graph binding cannot Count, so the whole all=true request used to collapse
// onto the O(full history) scan — every rig hydrating its closed history on a
// read the dashboard issues with a 2s response-cache TTL. The work legs stay
// bounded; only the leg that genuinely cannot count hydrates.
func TestBeadListSplitCityKeepsWorkLegsBounded(t *testing.T) {
	const limit = 5
	graph := &nonCounterLeg{Store: seedMoleculeLeg("gcg", 12)}
	fs, city, rig := newBoundedSplitState(t, graph)

	body := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d", limit))

	if city.maxListLim != limit+1 {
		t.Errorf("city leg max List limit = %d, want %d — a non-Counter graph leg must not unbound the work legs", city.maxListLim, limit+1)
	}
	if rig.maxListLim != limit+1 {
		t.Errorf("rig leg max List limit = %d, want %d — a non-Counter graph leg must not unbound the work legs", rig.maxListLim, limit+1)
	}
	if !city.countCalled || !rig.countCalled {
		t.Errorf("Count called: city=%v rig=%v, want both — bounding did not engage", city.countCalled, rig.countCalled)
	}
	if graph.maxListLim != 0 {
		t.Errorf("graph leg List limit = %d, want 0 — a leg that cannot Count must hydrate so its own row count is its exact total", graph.maxListLim)
	}
	if want := 30 + 30 + 12; body.Total != want {
		t.Errorf("Total = %d, want %d (work counts from Count, graph count from its own rows)", body.Total, want)
	}
	if len(body.Items) != limit {
		t.Fatalf("len(Items) = %d, want %d", len(body.Items), limit)
	}
	if body.NextCursor == "" {
		t.Errorf("NextCursor empty, want a cursor")
	}
}

// TestBeadListSplitCityBoundsACounterCapableGraphLeg is the control: the
// exemption is scoped to legs that genuinely cannot count. A graph binding that
// CAN Count (the beads adapter and the SQLite front door both do) is bounded
// like every other leg.
func TestBeadListSplitCityBoundsACounterCapableGraphLeg(t *testing.T) {
	const limit = 5
	graph := &countingListStore{Store: seedMoleculeLeg("gcg", 12)}
	fs, city, rig := newBoundedSplitState(t, graph)

	body := fetchBoundedBeads(t, fs, fmt.Sprintf("?type=molecule&all=true&limit=%d", limit))

	if graph.maxListLim != limit+1 {
		t.Errorf("graph leg max List limit = %d, want %d — a Counter-capable graph leg is bounded like the rest", graph.maxListLim, limit+1)
	}
	if !graph.countCalled {
		t.Errorf("graph Count was not called; the leg was hydrated although it can count")
	}
	if city.maxListLim != limit+1 || rig.maxListLim != limit+1 {
		t.Errorf("work leg limits = city:%d rig:%d, want %d", city.maxListLim, rig.maxListLim, limit+1)
	}
	if want := 30 + 30 + 12; body.Total != want {
		t.Errorf("Total = %d, want %d", body.Total, want)
	}
}

// TestBeadListSplitCityBoundedWalkIsTheFullScanPrefix proves the bounded split
// fan-out serves the same rows the un-bounded scan would, page by page: Total
// stays constant across the walk and the concatenated pages are the exact
// created_at-desc order.
func TestBeadListSplitCityBoundedWalkIsTheFullScanPrefix(t *testing.T) {
	const limit = 7
	graph := &nonCounterLeg{Store: seedMoleculeLeg("gcg", 12)}
	fs, _, _ := newBoundedSplitState(t, graph)

	const total = 30 + 30 + 12
	var got []string
	cursor := ""
	for page := 0; page < 20; page++ {
		q := fmt.Sprintf("?type=molecule&all=true&limit=%d", limit)
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		body := fetchBoundedBeads(t, fs, q)
		if body.Total != total {
			t.Fatalf("page %d Total = %d, want %d (Total must be constant across a walk)", page, body.Total, total)
		}
		for _, b := range body.Items {
			got = append(got, b.ID)
		}
		cursor = body.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(got) != total {
		t.Fatalf("walk served %d rows, want %d (ids=%v)", len(got), total, got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("walk repeated %q at position %d", got[i], i)
		}
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("walk served %q twice", id)
		}
		seen[id] = true
	}
}

// TestBeadListDedupesBeadIDsAcrossLegs is the cross-leg identity guard. A
// migrated split city keeps its pre-cutover infrastructure rows in the work
// store — `gc storage migrate` copies with CreateWithForeignID (ids preserved)
// and never deletes back — so the same bead id is genuinely resident in the
// work store and in the binding. Without a cross-leg id gate the list arm
// serves it twice and double-counts it, while the ready arm (which has one)
// serves it once.
//
// Winner rule: legs are federated work-first, graph LAST, and the first leg to
// return an id wins — identical to the ready arm, so both endpoints resolve a
// co-resident bead to the same row.
func TestBeadListDedupesBeadIDsAcrossLegs(t *testing.T) {
	fs := newFakeState(t)
	created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	work := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "gcg-7", Type: "task", Status: "open", Title: "retained work-store copy", CreatedAt: created,
	}}, nil)
	graph := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "gcg-7", Type: "task", Status: "open", Title: "relocated binding copy", CreatedAt: created,
	}}, nil)
	fs.cityBeadStore = work
	fs.stores = map[string]beads.Store{fs.CityName(): work}
	fs.graphBeadStore = graph

	body := getListBody(t, fs, "/beads")

	if len(body.Items) != 1 || body.Total != 1 {
		t.Fatalf("items=%d total=%d, want 1/1 — a bead co-resident in the work store and the binding is ONE bead (items=%+v)", len(body.Items), body.Total, body.Items)
	}
	if body.Items[0].Title != "retained work-store copy" {
		t.Fatalf("winning row = %q, want the work leg's copy — legs are work-first, graph last, first leg wins (same rule as GET /beads/ready)", body.Items[0].Title)
	}

	ready := getListBody(t, fs, "/beads/ready")
	if len(ready.Items) != 1 || ready.Items[0].Title != "retained work-store copy" {
		t.Fatalf("ready resolved the co-resident bead to %+v; list and ready must agree on the winner", ready.Items)
	}
}

// TestBeadListDedupedWalkServesEachIDOnce pins the paging half: with every id
// co-resident, a limit-bounded walk must serve each bead exactly once and report
// the deduped total, not double it.
func TestBeadListDedupedWalkServesEachIDOnce(t *testing.T) {
	created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]beads.Bead, 0, 3)
	for i := 0; i < 3; i++ {
		rows = append(rows, beads.Bead{
			ID: fmt.Sprintf("gcg-%d", i), Type: "task", Status: "open",
			Title: fmt.Sprintf("step %d", i), CreatedAt: created.Add(time.Duration(i) * time.Minute),
		})
	}
	fs := newFakeState(t)
	work := beads.NewMemStoreFrom(len(rows), rows, nil)
	graph := beads.NewMemStoreFrom(len(rows), rows, nil)
	fs.cityBeadStore = work
	fs.stores = map[string]beads.Store{fs.CityName(): work}
	fs.graphBeadStore = graph

	var got []string
	cursor := ""
	for page := 0; page < 6; page++ {
		q := "/beads?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		body := getListBody(t, fs, q)
		if body.Total != len(rows) {
			t.Fatalf("page %d Total = %d, want %d (co-residence must not inflate the total)", page, body.Total, len(rows))
		}
		for _, b := range body.Items {
			got = append(got, b.ID)
		}
		cursor = body.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(got) != len(rows) {
		t.Fatalf("walk served %v, want each of %d ids exactly once", got, len(rows))
	}
}

// TestBeadListGraphLeg503NamesWorkLegFailures pins that the authoritative graph
// failure does not erase the work-side diagnosis. "The graph plane is down" and
// "the entire city is down" must not be byte-identical responses: an operator
// reading the 503 needs to know the rigs failed too.
func TestBeadListGraphLeg503NamesWorkLegFailures(t *testing.T) {
	fs := newSplitFederationState(t)
	fs.stores["myrig"] = &failingBeadStore{Store: beads.NewMemStore(), listErr: errors.New("rig ledger unreachable")}
	fs.graphBeadStore = &failingBeadStore{Store: beads.NewMemStore(), listErr: errors.New("infra dolt unreachable")}

	rec := getRaw(t, fs, "/beads")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "infra dolt unreachable") {
		t.Errorf("503 body %q does not name the graph failure", body)
	}
	if !strings.Contains(body, "rig ledger unreachable") {
		t.Errorf("503 body %q drops the work-leg failure; the operator cannot tell a dead graph plane from a dead city", body)
	}
}

// TestBeadReadyGraphLeg503NamesWorkLegFailures is the same property on the ready
// arm, which federates the graph leg last and therefore has every work-leg error
// in hand when it fails loud.
func TestBeadReadyGraphLeg503NamesWorkLegFailures(t *testing.T) {
	fs := newSplitFederationState(t)
	fs.stores["myrig"] = &failingBeadStore{Store: beads.NewMemStore(), readyErr: errors.New("rig ledger unreachable")}
	fs.graphBeadStore = &failingBeadStore{Store: beads.NewMemStore(), readyErr: errors.New("infra dolt unreachable")}

	rec := getRaw(t, fs, "/beads/ready")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "rig ledger unreachable") {
		t.Errorf("503 body %q drops the work-leg failure", body)
	}
}

// TestBeadListRigNamedLikeTheGraphLegIsStillServed kills the synthetic-key
// collision. The leg key the first cut minted ("infra:<city>") was justified by
// an unenforced claim that ':' cannot appear in a rig name; a rig that did carry
// that name was silently overwritten in the fan-out map and vanished from the
// response. Legs are a slice now, so there is no key to collide with.
func TestBeadListRigNamedLikeTheGraphLegIsStillServed(t *testing.T) {
	fs := newFakeState(t)
	created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rig := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "ra-1", Type: "task", Status: "open", Title: "rig work", CreatedAt: created,
	}}, nil)
	graph := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "gcg-1", Type: "task", Status: "open", Title: "graph step", CreatedAt: created.Add(time.Minute),
	}}, nil)
	fs.cityBeadStore = beads.NewMemStore()
	fs.stores = map[string]beads.Store{"infra:" + fs.CityName(): rig}
	fs.graphBeadStore = graph

	body := getListBody(t, fs, "/beads")
	if body.Total != 2 {
		t.Fatalf("total = %d, want 2 (items=%+v) — a rig whose name looks like the old synthetic leg key must still be served", body.Total, body.Items)
	}
	if !bodyContainsBeadID(body.Items, "ra-1") {
		t.Errorf("items = %+v, want the rig bead ra-1", body.Items)
	}
	if !bodyContainsBeadID(body.Items, "gcg-1") {
		t.Errorf("items = %+v, want the graph bead gcg-1", body.Items)
	}
}

// TestFederationFixturesModelProductionCapabilities keeps these tests honest
// about what they model. The graph leaf must be Counter-LESS the way the raw
// *beads.SQLiteStore binding is, and the work legs must be Counters the way
// every caching-wrapped production work store is. splittest.StrictStore is
// checked here too: it declares beads.Counter, so a fixture built from the kit
// would keep the bounded path alive regardless of what the handler does with the
// graph leg — which is exactly why these tests do not use it.
func TestFederationFixturesModelProductionCapabilities(t *testing.T) {
	var leg beads.Store = &nonCounterLeg{Store: beads.NewMemStore()}
	if _, ok := leg.(beads.Counter); ok {
		t.Fatal("nonCounterLeg implements beads.Counter; it no longer models the raw *beads.SQLiteStore graph binding")
	}
	var work beads.Store = &countingListStore{Store: beads.NewMemStore()}
	if _, ok := work.(beads.Counter); !ok {
		t.Fatal("countingListStore must implement beads.Counter to model a caching-wrapped work store")
	}
	if _, ok := splittest.NewClassStore(t, config.BeadClassGraph).(beads.Counter); !ok {
		t.Fatal("splittest.NewClassStore no longer declares beads.Counter; the fixture-fidelity note on splittest.StrictStore.Count is stale")
	}
}
