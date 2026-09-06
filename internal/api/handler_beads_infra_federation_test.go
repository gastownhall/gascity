package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
)

// Landmine #13: on a split city the graph-class DAG (gcg- molecule roots, steps,
// control beads) lives in the relocated graph store, which BeadStores() does not
// include. The HTTP ready/list handlers must federate it — or the whole DAG is
// invisible behind an authoritative 200 — and an infra-leg hard failure is an
// authoritative failure (503), not a degraded Partial 200.
//
// The second property is this program's Invariant 0 at the API layer. A rig that
// falls over is one scope going dark and Partial says so honestly; the graph
// plane going dark means the execution DAG is gone, and a work-only 200 is
// indistinguishable from "the DAG finished". So the graph leg does not
// participate in partial degradation at all: it either answers or the request
// fails loud.
//
// Fixtures come from internal/beads/splittest, not bare MemStores: the graph leg
// mints real reserved-prefix (gcg-) ids and the work legs mint their own
// disjoint prefixes, so these tests exercise the same prefix-disjoint shape a
// real split city has. The one exception is a leg whose read must FAIL — that
// wraps a plain leaf, because splittest.StrictStore implements Handles() and
// beads.HandlesFor would resolve straight past the failure injection to the
// leaf's own reader.

// newSplitFederationState returns a fakeState shaped like a split city: an HQ
// work store, one rig work store, and a relocated graph store, all
// prefix-disjoint.
func newSplitFederationState(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = splittest.NewWorkStore(t, "hq")
	fs.stores["myrig"] = splittest.NewWorkStore(t, "ra")
	fs.graphBeadStore = splittest.NewClassStore(t, config.BeadClassGraph)
	return fs
}

// newLegacyFederationState returns a fakeState shaped like a legacy
// single-store city: the graph class is NOT relocated, so
// GraphBeadStore() == CityBeadStore() and the infra arm never fires.
func newLegacyFederationState(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = splittest.NewWorkStore(t, "hq")
	fs.stores["myrig"] = splittest.NewWorkStore(t, "ra")
	return fs
}

func seedGraphStepBead(t *testing.T, fs *fakeState) string {
	t.Helper()
	created, err := fs.graphBeadStore.Create(beads.Bead{Type: "task", Title: "graph step"})
	if err != nil {
		t.Fatalf("seed graph step: %v", err)
	}
	return created.ID
}

func bodyContainsBeadID(items []beads.Bead, id string) bool {
	for _, b := range items {
		if b.ID == id {
			return true
		}
	}
	return false
}

// listBody is the decoded shape of a ListBody[beads.Bead] response.
type listBody struct {
	Items         []beads.Bead `json:"items"`
	Total         int          `json:"total"`
	NextCursor    string       `json:"next_cursor"`
	Partial       bool         `json:"partial"`
	PartialErrors []string     `json:"partial_errors"`
}

// getListBody issues a GET against the city handler and decodes the list
// envelope, failing the test on a non-200.
func getListBody(t *testing.T, fs *fakeState, path string) listBody {
	t.Helper()
	rec := getRaw(t, fs, path)
	if rec.Code != 200 {
		t.Fatalf("GET %s status = %d, want 200 (body=%q)", path, rec.Code, rec.Body.String())
	}
	var body listBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v (body=%q)", path, err, rec.Body.String())
	}
	return body
}

func getRaw(t *testing.T, fs *fakeState, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, path), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBeadReadyFederatesGraphStore(t *testing.T) {
	fs := newSplitFederationState(t)
	graphID := seedGraphStepBead(t, fs)

	body := getListBody(t, fs, "/beads/ready")
	if !bodyContainsBeadID(body.Items, graphID) {
		t.Fatalf("ready items = %+v, want the graph-store step %q (DAG invisible behind an authoritative 200?)", body.Items, graphID)
	}
	if body.Partial {
		t.Fatalf("Partial = true, want false — a healthy graph federation is authoritative")
	}
}

// TestBeadReadyGraphLegHardFailIs503 is the Invariant 0 half: a dead graph leg
// is an authoritative failure, never a degraded Partial 200. The rig store holds
// claimable work, so a handler that degrades would have something to serve —
// that is exactly the work-only 200 this pins against.
func TestBeadReadyGraphLegHardFailIs503(t *testing.T) {
	fs := newSplitFederationState(t)
	if _, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"}); err != nil {
		t.Fatalf("seed rig work: %v", err)
	}
	fs.graphBeadStore = &failingBeadStore{Store: beads.NewMemStore(), readyErr: errors.New("infra dolt unreachable")}

	rec := getRaw(t, fs, "/beads/ready")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 when the graph plane is unreadable — a work-only Partial 200 hides the whole DAG (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestBeadReadyGraphLegPartialIsAlso503 pins the sharper edge of Invariant 0: a
// PartialResultError from the graph leg is still authoritative failure. A rig
// that skipped rows is one scope reporting a hole; the graph plane skipping rows
// means an unknown slice of the DAG is missing, and the response cannot say
// which.
func TestBeadReadyGraphLegPartialIsAlso503(t *testing.T) {
	fs := newSplitFederationState(t)
	base := beads.NewMemStore()
	survivor, err := base.Create(beads.Bead{Type: "task", Title: "surviving graph row"})
	if err != nil {
		t.Fatalf("seed graph survivor: %v", err)
	}
	fs.graphBeadStore = &failingBeadStore{
		Store:       base,
		readyResult: []beads.Bead{survivor},
		readyErr:    &beads.PartialResultError{Op: "bd ready", Err: errors.New("skipped 1 corrupt bead")},
	}

	rec := getRaw(t, fs, "/beads/ready")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 — a partial graph read leaves an unnamed hole in the DAG (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestBeadListFederatesGraphStore(t *testing.T) {
	fs := newSplitFederationState(t)
	graphID := seedGraphStepBead(t, fs)

	body := getListBody(t, fs, "/beads")
	if !bodyContainsBeadID(body.Items, graphID) {
		t.Fatalf("list items = %+v, want the graph-store bead %q (DAG invisible behind an authoritative 200?)", body.Items, graphID)
	}
}

func TestBeadListGraphLegHardFailIs503(t *testing.T) {
	fs := newSplitFederationState(t)
	if _, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"}); err != nil {
		t.Fatalf("seed rig work: %v", err)
	}
	fs.graphBeadStore = &failingBeadStore{Store: beads.NewMemStore(), listErr: errors.New("infra dolt unreachable")}

	rec := getRaw(t, fs, "/beads")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 when the graph list read hard-fails (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestBeadListGraphLegPartialIsAlso503(t *testing.T) {
	fs := newSplitFederationState(t)
	base := beads.NewMemStore()
	survivor, err := base.Create(beads.Bead{Type: "task", Title: "surviving graph row"})
	if err != nil {
		t.Fatalf("seed graph survivor: %v", err)
	}
	fs.graphBeadStore = &failingBeadStore{
		Store:      base,
		listResult: []beads.Bead{survivor},
		listErr:    &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped 1 corrupt bead")},
	}

	rec := getRaw(t, fs, "/beads")
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 — a partial graph read leaves an unnamed hole in the DAG (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestBeadListRigScopedReadExcludesGraphLeg pins that ?rig=<name> stays a
// rig-scoped read: the graph plane is not a rig, so a rig-scoped request gets no
// graph leg. The leg has no name of its own to be selected by either — legs are
// a slice, and only the named rig stores are addressable through ?rig=.
func TestBeadListRigScopedReadExcludesGraphLeg(t *testing.T) {
	fs := newSplitFederationState(t)
	graphID := seedGraphStepBead(t, fs)
	rigBead, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"})
	if err != nil {
		t.Fatalf("seed rig work: %v", err)
	}

	body := getListBody(t, fs, "/beads?rig=myrig")
	if !bodyContainsBeadID(body.Items, rigBead.ID) {
		t.Fatalf("rig-scoped items = %+v, want the rig bead %q", body.Items, rigBead.ID)
	}
	if bodyContainsBeadID(body.Items, graphID) {
		t.Fatalf("rig-scoped items include the graph bead %q; ?rig= is a rig-scoped read and the graph plane is not a rig", graphID)
	}

	// A rig name that does not exist selects nothing — there is no leg reachable
	// through ?rig= other than a configured rig store.
	unknown := getListBody(t, fs, "/beads?rig=not-a-rig")
	if len(unknown.Items) != 0 {
		t.Fatalf("?rig=not-a-rig returned %d items; only configured rig stores are addressable", len(unknown.Items))
	}
}

// TestBeadReadyLegacyCityUnchanged is the single-store byte-identity half. On a
// legacy city GraphBeadStore() == CityBeadStore(), so the infra arm never fires
// and the response is what it was before this leg existed.
//
// It is also the mutation-killer for the arm's identity guard: drop
// `graph != CityBeadStore()` and the city store is federated a second time —
// harmless for the rows (seen[] dedupes them) but NOT for the failure
// accounting, because the second attempt runs under the graph leg's fail-loud
// contract. TestBeadReadyLegacyCityDegradedCityStoreStaysPartial pins that.
func TestBeadReadyLegacyCityUnchanged(t *testing.T) {
	fs := newLegacyFederationState(t)
	if _, err := fs.cityBeadStore.Create(beads.Bead{Type: "task", Title: "city work"}); err != nil {
		t.Fatalf("seed city work: %v", err)
	}
	if _, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"}); err != nil {
		t.Fatalf("seed rig work: %v", err)
	}

	body := getListBody(t, fs, "/beads/ready")
	if body.Partial {
		t.Fatalf("Partial = true on a single-store city, want false")
	}
	if len(body.Items) != 2 {
		t.Fatalf("ready items = %d, want 2 (one city, one rig)", len(body.Items))
	}
	seen := map[string]bool{}
	for _, b := range body.Items {
		if seen[b.ID] {
			t.Fatalf("duplicate bead %q in single-store ready set", b.ID)
		}
		seen[b.ID] = true
	}
}

// TestBeadReadyLegacyCityDegradedCityStoreStaysPartial kills the mutation that
// drops the arm's `graph != CityBeadStore()` identity guard. On a legacy city
// the two accessors return the SAME store, so an unguarded arm would re-read it
// under the fail-loud contract and turn today's honest Partial 200 into a 503 —
// a single-store city taking a new failure mode from a split-city feature.
func TestBeadReadyLegacyCityDegradedCityStoreStaysPartial(t *testing.T) {
	fs := newLegacyFederationState(t)
	rigBead, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"})
	if err != nil {
		t.Fatalf("seed rig work: %v", err)
	}
	fs.cityBeadStore = &failingBeadStore{Store: beads.NewMemStore(), readyErr: errors.New("city store hiccup")}

	body := getListBody(t, fs, "/beads/ready")
	if !body.Partial {
		t.Fatalf("Partial = false, want true — a degraded city store on a legacy city degrades, it does not fail loud")
	}
	if !bodyContainsBeadID(body.Items, rigBead.ID) {
		t.Fatalf("items = %+v, want the healthy rig's bead %q still served", body.Items, rigBead.ID)
	}
}

// TestBeadListLegacyCityUnchanged is the list-side byte-identity half: a legacy
// city's rows come back exactly once, with the graph arm dead.
//
// Two independent mechanisms hold that here, and knowing which is which matters
// when this goes red: the arm's `graph != CityBeadStore()` identity guard keeps
// the leg from being minted at all, and sortedRigNames' store-identity dedup
// would collapse it even if the guard were dropped. The mutation-killer for the
// guard itself is TestBeadListLegacyCityDoesNotSmuggleCityStore.
func TestBeadListLegacyCityUnchanged(t *testing.T) {
	fs := newLegacyFederationState(t)
	cityBead, err := fs.cityBeadStore.Create(beads.Bead{Type: "task", Title: "city work"})
	if err != nil {
		t.Fatalf("seed city work: %v", err)
	}
	if _, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"}); err != nil {
		t.Fatalf("seed rig work: %v", err)
	}
	// The city store is reachable through BeadStores() the way production wires
	// it (cmd/gc/api_state.go keys it by CityName()), so the legacy response
	// already contains it exactly once.
	fs.stores[fs.CityName()] = fs.cityBeadStore

	body := getListBody(t, fs, "/beads")
	if body.Partial {
		t.Fatalf("Partial = true on a single-store city, want false (errors=%v)", body.PartialErrors)
	}
	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("list total=%d items=%d, want 2/2 — a duplicated city leg is the unguarded-merge mutation", body.Total, len(body.Items))
	}
	count := 0
	for _, b := range body.Items {
		if b.ID == cityBead.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("city bead %q appears %d times, want exactly 1", cityBead.ID, count)
	}
}

// TestBeadReadyWorkStoresDownButGraphUpIsPartial200 pins the deliberate
// consequence of counting the graph leg as a backend attempt: totalOutage means
// "every leg we asked failed", so a split city whose work stores are all down
// but whose graph plane answered gets a Partial 200 carrying the DAG, not the
// 503 a work-only fan-out would have produced. The asymmetry is the point — the
// graph plane answered authoritatively, and the rig errors are named in
// partial_errors.
func TestBeadReadyWorkStoresDownButGraphUpIsPartial200(t *testing.T) {
	fs := newSplitFederationState(t)
	graphID := seedGraphStepBead(t, fs)
	fs.cityBeadStore = &failingBeadStore{Store: beads.NewMemStore(), readyErr: errors.New("city store down")}
	fs.stores["myrig"] = &failingBeadStore{Store: beads.NewMemStore(), readyErr: errors.New("rig store down")}

	body := getListBody(t, fs, "/beads/ready")
	if !body.Partial || len(body.PartialErrors) != 2 {
		t.Fatalf("Partial=%v errors=%v, want partial with both work-store failures named", body.Partial, body.PartialErrors)
	}
	if !bodyContainsBeadID(body.Items, graphID) {
		t.Fatalf("items = %+v, want the graph step %q — the plane that answered is the DAG", body.Items, graphID)
	}
}

// TestBeadListLegacyCityDoesNotSmuggleCityStore is the mutation-killer for the
// list arm's `graph != CityBeadStore()` identity guard. It pins the byte-identity
// property directly: on a legacy city the graph arm must not change WHICH stores
// the fan-out reads. Here BeadStores() deliberately does not carry the city
// store, so an unguarded arm would mint a synthetic leg pointing at it and the
// city's beads would appear in a response that never contained them.
func TestBeadListLegacyCityDoesNotSmuggleCityStore(t *testing.T) {
	fs := newLegacyFederationState(t)
	cityBead, err := fs.cityBeadStore.Create(beads.Bead{Type: "task", Title: "city work"})
	if err != nil {
		t.Fatalf("seed city work: %v", err)
	}
	rigBead, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"})
	if err != nil {
		t.Fatalf("seed rig work: %v", err)
	}

	body := getListBody(t, fs, "/beads")
	if !bodyContainsBeadID(body.Items, rigBead.ID) {
		t.Fatalf("list items = %+v, want the rig bead %q", body.Items, rigBead.ID)
	}
	if bodyContainsBeadID(body.Items, cityBead.ID) {
		t.Fatalf("list items include city bead %q, which is not in BeadStores() — the graph arm minted a leg on a legacy city, so it is no longer byte-identical", cityBead.ID)
	}
	if body.Total != 1 {
		t.Fatalf("list total = %d, want 1", body.Total)
	}
}

// TestBeadReadyFederationOrderIsLegConcatenation pins the ordering rule the
// follow-on CLI federation (ga-oxsyu) has to match so its conformance test can
// assert CLI == API instead of inventing an oracle:
//
//	legs, in order:  city store, then rigs by name ascending, then the graph store
//	within a leg:    whatever order that leg's own Ready reader emits
//	dedupe:          first leg to return an id wins
//	merged order:    leg concatenation — deliberately NOT re-sorted, because a
//	                 global sort would change the bytes a single-store city
//	                 already serves
//
// Per-leg order is deterministic but NOT canonical across leg kinds: a
// caching-wrapped work store emits (priority, created_at, id) via
// sortBeadsReadyOrder, while the canonical relocated graph binding
// (beads.SQLiteStore) emits (created_at, id) with no priority term at all
// (sqliteReadySQL). Both sides therefore normalize with
// beads.SortBeadsReadyOrder before comparing — a load-bearing step, not a
// formality. The graph leg goes LAST, which matches the CLI composite's
// work-then-infra leg order.
func TestBeadReadyFederationOrderIsLegConcatenation(t *testing.T) {
	fs := newSplitFederationState(t)
	cityBead, err := fs.cityBeadStore.Create(beads.Bead{Type: "task", Title: "city work"})
	if err != nil {
		t.Fatalf("seed city work: %v", err)
	}
	rigBead, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"})
	if err != nil {
		t.Fatalf("seed rig work: %v", err)
	}
	graphID := seedGraphStepBead(t, fs)

	body := getListBody(t, fs, "/beads/ready")
	want := []string{cityBead.ID, rigBead.ID, graphID}
	got := make([]string, 0, len(body.Items))
	for _, b := range body.Items {
		got = append(got, b.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("ready ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ready ids = %v, want %v (leg order is city, rigs asc, graph last)", got, want)
		}
	}
}
