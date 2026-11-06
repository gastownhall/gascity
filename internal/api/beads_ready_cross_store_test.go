package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestBeadsReadyAppliesTheCrossStoreRuleOnASplitCity: GET /beads/ready (the
// dashboard's ready read) applies the same rule as `gc ready` on a split city.
// A bd work row whose blocker is a graph step held open in the binding is not
// served (bd alone would serve it), and is served once the step closes.
func TestBeadsReadyAppliesTheCrossStoreRuleOnASplitCity(t *testing.T) {
	work := splittest.NewNativeWorkStore(t, "gc")
	binding, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
	last := 4
	step, err := binding.Create(beads.Bead{Title: "graph step", Type: "task", Priority: &last})
	if err != nil {
		t.Fatal(err)
	}
	row, err := work.Create(beads.Bead{Title: "held work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(row.ID, step.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	fs := newFakeState(t)
	fs.cityBeadStore = work
	fs.stores = map[string]beads.Store{}
	fs.graphBeadStore = binding
	h := newTestCityHandler(t, fs)

	ready := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/beads/ready"), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /beads/ready = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Items []beads.Bead `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, b := range body.Items {
			ids = append(ids, b.ID)
		}
		return ids
	}
	if got := ready(); slices.Contains(got, row.ID) {
		t.Fatalf("/beads/ready = %v served %s while its graph blocker %s is open", got, row.ID, step.ID)
	}
	if err := binding.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	if got := ready(); !slices.Contains(got, row.ID) {
		t.Fatalf("/beads/ready = %v withholds %s after its blocker closed", got, row.ID)
	}
}

// edgeFailingStore is a bd-semantics leg (no stranding listing) whose edge
// reads fail, so the cross-store rule cannot judge its rows.
type edgeFailingStore struct{ beads.Store }

func (edgeFailingStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New("edge read refused")
}

// TestBeadsReadyDropsALegItCannotGate: on a split city, a leg whose rows the
// cross-store rule cannot judge serves none of them, and the response is
// marked partial; the other legs still answer.
func TestBeadsReadyDropsALegItCannotGate(t *testing.T) {
	work := beads.NewMemStore()
	row, err := work.Create(beads.Bead{Title: "unjudged work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
	step, err := binding.Create(beads.Bead{Title: "graph step", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeState(t)
	fs.cityBeadStore = edgeFailingStore{Store: work}
	fs.stores = map[string]beads.Store{}
	fs.graphBeadStore = binding
	rec := httptest.NewRecorder()
	newTestCityHandler(t, fs).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/beads/ready"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /beads/ready = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items   []beads.Bead `json:"items"`
		Partial bool         `json:"partial"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range body.Items {
		ids = append(ids, b.ID)
	}
	if slices.Contains(ids, row.ID) {
		t.Fatalf("/beads/ready = %v served %s, which the cross-store rule could not judge", ids, row.ID)
	}
	if !body.Partial || !slices.Contains(ids, step.ID) {
		t.Fatalf("/beads/ready = %v partial=%v; want the graph leg served and the response partial", ids, body.Partial)
	}
}

// Ready must apply native input admission in both storage layouts, without
// losing unrelated rows from the serving leg when an input is held.
func TestBeadsReadyNativeInputAdmission(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "unified", true: "split"}[split], func(t *testing.T) {
			work := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "input", Status: "open"},
				{ID: "convoy", Type: "convoy", Status: "open"},
			}, []beads.Dep{{IssueID: "convoy", DependsOnID: "input", Type: "tracks"}})
			graph := work
			if split {
				graph = beads.NewMemStoreFrom(100, nil, nil)
			}
			create := func(b beads.Bead) beads.Bead {
				t.Helper()
				got, err := graph.Create(b)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			root := create(beads.Bead{Title: "native root", Type: "molecule", Metadata: map[string]string{"gc.input_convoy_id": "convoy"}})
			step := create(beads.Bead{Title: "native step", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
			plain := create(beads.Bead{Title: "plain sibling"})
			fs := newFakeState(t)
			fs.cityBeadStore, fs.graphBeadStore = work, graph
			fs.stores = map[string]beads.Store{}
			h := newTestCityHandler(t, fs)
			for _, held := range []bool{false, true} {
				if held {
					if err := work.Update("input", beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
						t.Fatal(err)
					}
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/beads/ready"), nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("ready = %d: %s", rec.Code, rec.Body.String())
				}
				var body ListBody[beads.Bead]
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Partial || len(body.PartialErrors) != 0 {
					t.Fatalf("partial ready: %+v", body)
				}
				has := func(id string) bool {
					return slices.ContainsFunc(body.Items, func(b beads.Bead) bool { return b.ID == id })
				}
				if has(step.ID) == held || !has(plain.ID) {
					t.Fatalf("held=%v items=%+v", held, body.Items)
				}
			}
		})
	}
}

// A failed Ready reader still participates in the named input frame: dropping
// it would incorrectly prove unique input residence from only the healthy leg.
func TestBeadsReadyRetainsFailedInputScope(t *testing.T) {
	work := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "input", Status: "open"},
		{ID: "convoy", Type: "convoy", Status: "open"},
		{ID: "root", Type: "molecule", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "convoy"}},
		{ID: "step", Status: "open", Metadata: map[string]string{"gc.root_bead_id": "root"}},
	}, []beads.Dep{{IssueID: "convoy", DependsOnID: "input", Type: "tracks"}})
	fs := newFakeState(t)
	fs.cityBeadStore = work
	fs.stores = map[string]beads.Store{"unavailable": failedInputScope{Store: beads.NewMemStore()}}
	rec := httptest.NewRecorder()
	newTestCityHandler(t, fs).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/beads/ready"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready = %d: %s", rec.Code, rec.Body.String())
	}
	var body ListBody[beads.Bead]
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Partial || slices.ContainsFunc(body.Items, func(b beads.Bead) bool { return b.ID == "step" }) {
		t.Fatalf("unknown input scope advertised native work: %+v", body)
	}
}

type failedInputScope struct{ beads.Store }

func (failedInputScope) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, errors.New("input scope unavailable")
}

func (failedInputScope) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("input scope unavailable")
}
