package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// TestSourceWorkflowStoresLeadsWithRelocatedGraphStore pins the leg order and
// the strict policy the split city depends on, at the enumerator itself.
func TestSourceWorkflowStoresLeadsWithRelocatedGraphStore(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityBeadStore = beads.NewMemStore()
	graph := beads.NewMemStore()
	state.graphBeadStore = graph
	state.stores = map[string]beads.Store{"alpha": beads.NewMemStore()}
	s := &Server{state: state}

	stores := s.sourceWorkflowStores()
	if len(stores) != 3 {
		t.Fatalf("sourceWorkflowStores() returned %d entries, want graph + city + rig", len(stores))
	}
	if stores[0].Store != graph {
		t.Fatalf("stores[0].Store = %p, want the relocated graph store %p (graph-first)", stores[0].Store, graph)
	}
	if stores[0].StoreRef != sourceworkflow.GraphStoreRef("bright-lights") {
		t.Fatalf("stores[0].StoreRef = %q, want %q", stores[0].StoreRef, sourceworkflow.GraphStoreRef("bright-lights"))
	}
	if !stores[0].Strict {
		t.Fatal("the graph leg is not strict; a fault on the store that holds the answer would degrade to a warning")
	}
	if stores[1].StoreRef != "city:bright-lights" || stores[2].StoreRef != "rig:alpha" {
		t.Fatalf("work legs = %q, %q; want city:bright-lights then rig:alpha", stores[1].StoreRef, stores[2].StoreRef)
	}
	for _, info := range stores[1:] {
		if info.Strict {
			t.Fatalf("work leg %q is strict; only the selected source store and the graph binding are", info.StoreRef)
		}
	}
}

// TestSourceWorkflowStoresOmitsGraphLegOnSingleStoreCity is decision (4): where
// the graph class is not relocated the graph store IS the work store, so adding
// it would scan one store twice. The single-store enumeration stays
// byte-identical to what it was before the graph leg existed.
func TestSourceWorkflowStoresOmitsGraphLegOnSingleStoreCity(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": beads.NewMemStore()}
	s := &Server{state: state}

	if state.GraphBeadStore().Store != state.CityBeadStore() {
		t.Fatal("fixture is not a single-store city")
	}
	stores := s.sourceWorkflowStores()
	if len(stores) != 2 {
		t.Fatalf("sourceWorkflowStores() returned %d entries, want exactly the city and rig work legs", len(stores))
	}
	for _, info := range stores {
		if strings.HasPrefix(info.StoreRef, sourceworkflow.GraphStoreRefPrefix+":") {
			t.Fatalf("single-store city enumerated a graph leg %q; the work store would be scanned twice", info.StoreRef)
		}
		if info.Strict {
			t.Fatalf("work leg %q is strict on a single-store city", info.StoreRef)
		}
	}
}

// TestSlingStandaloneFormulaRoutesTheWispRootInTheGraphBinding is #6054 on the
// API path: a standalone --formula launch mints its wisp root in the relocated
// graph binding, so apiBeadRouter must stamp gc.routed_to there rather than in
// the target's work store, which has never held the root.
func TestSlingStandaloneFormulaRoutesTheWispRootInTheGraphBinding(t *testing.T) {
	h, state := newSlingTestServer(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	if err := os.WriteFile(filepath.Join(formulaDir, "split-root-only.toml"), []byte(`
formula = "split-root-only"
version = 1
phase = "vapor"

[[steps]]
id = "work"
title = "Work"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	state.cfg.Agents = []config.Agent{{
		Name:              "worker",
		Dir:               "myrig",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}}
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	state.graphBeadStore = graph

	body := `{"target":"myrig/worker","formula":"split-root-only"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	roots, err := graph.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("list graph binding: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("graph binding holds %d beads, want the one wisp root: %+v", len(roots), roots)
	}
	root := roots[0]
	if got := root.Metadata[beadmeta.RoutedToMetadataKey]; got != "myrig/worker" {
		t.Errorf("binding-resident wisp root %s gc.routed_to = %q, want myrig/worker", root.ID, got)
	}
	if _, err := state.stores["myrig"].Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("wisp root %s resolves in the rig work store (err=%v); it must live only in the graph binding", root.ID, err)
	}
}

// TestSlingStandaloneFormulaOnASingleStoreCityRigTargetIsUnchanged pins that
// the #6054 fix leaves a city that relocates nothing exactly as it was: the
// API hands the sling GraphBeadStore() (the city store) as its graph store, so
// a rig-targeted --formula mints its root in the city store while routing goes
// through the rig store, and the sling fails with 400. That pre-existing
// non-split defect is tracked separately (ga-i0prlx); this row only guards that the
// relocated-graph fix did not change it.
func TestSlingStandaloneFormulaOnASingleStoreCityRigTargetIsUnchanged(t *testing.T) {
	h, state := newSlingTestServer(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	if err := os.WriteFile(filepath.Join(formulaDir, "split-root-only.toml"), []byte(`
formula = "split-root-only"
version = 1
phase = "vapor"

[[steps]]
id = "work"
title = "Work"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	state.cfg.Agents = []config.Agent{{
		Name:              "worker",
		Dir:               "myrig",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}}
	state.cityBeadStore = beads.NewMemStore()
	if state.GraphBeadStore().Store != state.CityBeadStore() {
		t.Fatal("fixture is not a single-store city")
	}
	if state.stores["myrig"] == state.CityBeadStore() {
		t.Fatal("fixture rig store is the city store; the row needs them distinct")
	}

	body := `{"target":"myrig/worker","formula":"split-root-only"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unchanged single-store behavior); body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bead not found") {
		t.Fatalf("body = %s, want the unchanged routing failure", rec.Body.String())
	}
}
