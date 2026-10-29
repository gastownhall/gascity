package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// S4a: one placement rule for graph beads on split cities.
//
// Every test here runs on a REAL split fixture — the city binding is a SQLite
// store (newSplitEnvClassLeaf, the engine internal/storebinding/sqlite opens)
// and each rig is a real file-backed work store — and pairs it with a
// single-store control that proves the rule is invisible to a city that
// relocates nothing.

// s4RigFixture is one split or single-store city with two rigs whose work
// stores are real file stores.
type s4RigFixture struct {
	cityPath string
	cfg      *config.City
	routes   *storageRoutes
	work     beads.Store
	binding  beads.Store // nil on a single-store city
	rigs     map[string]beads.Store
	markers  string // binding root holding the per-rig records
}

func newS4RigFixture(t *testing.T, split bool) s4RigFixture {
	t.Helper()
	cityPath := t.TempDir()
	f := s4RigFixture{cityPath: cityPath, rigs: map[string]beads.Store{}}
	f.cfg = &config.City{Workspace: config.Workspace{Name: "s4-city"}}
	f.cfg.FormulaLayers = config.FormulaLayers{City: []string{convergenceResidenceFormulaDir(t)}}
	open := func(dir, prefix string) beads.Store {
		t.Helper()
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(dir, ".gc", "beads.json"), beads.WithFileStoreIDPrefix(prefix))
		if err != nil {
			t.Fatalf("opening the file store at %s: %v", dir, err)
		}
		return store
	}
	f.work = open(cityPath, "gc")
	for _, rig := range []struct{ name, prefix string }{{"ra", "ra"}, {"rb", "rb"}} {
		dir := filepath.Join(cityPath, "rigs", rig.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f.cfg.Rigs = append(f.cfg.Rigs, config.Rig{Name: rig.name, Path: dir, Prefix: rig.prefix})
		f.rigs[rig.name] = open(dir, rig.prefix)
	}
	if split {
		f.binding = newSplitEnvClassLeaf(t)
		f.markers = t.TempDir()
		f.routes = splitEnvRoutes(f.binding)
		f.routes.rigMarkers = &rigGraphMarkers{bindingRoot: f.markers, cityPath: cityPath, rigs: f.cfg.Rigs}
	}
	seedCLIStorageRoutes(t, cityPath, f.routes)
	return f
}

func (f s4RigFixture) rigPath(name string) string {
	for _, rig := range f.cfg.Rigs {
		if rig.Name == name {
			return rig.Path
		}
	}
	return ""
}

func (f s4RigFixture) runtime() *CityRuntime {
	return &CityRuntime{
		cityPath:            f.cityPath,
		cityName:            f.cfg.Workspace.Name,
		cfg:                 f.cfg,
		rec:                 events.Discard,
		storageRoutes:       f.routes,
		standaloneCityStore: f.work,
		standaloneRigStores: f.rigs,
	}
}

// markImported writes the records S4b's importer leaves for a finished rig
// import: the marker beside the binding and a complete cleared note.
func (f s4RigFixture) markImported(t *testing.T, rig string, complete bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.markers, rigGraphImportMarkerName(rig)), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := rigGraphClearedNotePath(f.cityPath, rig)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(infraClearedNote{Complete: complete})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// failingReadStore fails every read, so a test can prove a leg is not read.
type failingReadStore struct{ beads.Store }

var errLegRead = errors.New("this leg must not be read")

func (failingReadStore) Get(string) (beads.Bead, error)                  { return beads.Bead{}, errLegRead }
func (failingReadStore) List(beads.ListQuery) ([]beads.Bead, error)      { return nil, errLegRead }
func (failingReadStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) { return nil, errLegRead }

// TestGraphPlacementRuleTable is R-S4.1 for every scope on both topologies: a
// split city places every new graph root in the binding, a single-store city
// returns the very store instance it was handed.
func TestGraphPlacementRuleTable(t *testing.T) {
	for _, split := range []bool{true, false} {
		f := newS4RigFixture(t, split)
		for _, tc := range []struct {
			name  string
			scope graphScope
			store beads.Store
		}{
			{"city", cityGraphScope(), f.work},
			{"rig ra", rigGraphScope("ra"), f.rigs["ra"]},
			{"rig rb", rigGraphScope("rb"), f.rigs["rb"]},
		} {
			got := graphPlacementForScope(f.routes, f.cfg, f.cityPath, tc.scope, tc.store)
			want := tc.store
			if split {
				want = f.binding
			}
			if got != want {
				t.Errorf("split=%v %s: graphPlacementForScope = %T(%p), want %T(%p)", split, tc.name, got, got, want, want)
			}
		}
	}
}

// TestRigGraphRelicFreePredicate pins the file-read predicate the relic legs
// retire on, and its pessimistic default.
func TestRigGraphRelicFreePredicate(t *testing.T) {
	f := newS4RigFixture(t, true)
	if f.routes.rigGraphRelicFree("ra") {
		t.Fatal("a rig with no import records reads relic-free; the default must keep the relic leg")
	}
	if !graphRelicLegForScope(f.routes, rigGraphScope("ra")) {
		t.Fatal("graphRelicLegForScope for a rig awaiting import = false")
	}

	f.markImported(t, "ra", false)
	if f.routes.rigGraphRelicFree("ra") {
		t.Error("a rig whose clear is still pending reads relic-free; twins exist until the clear completes")
	}
	f.markImported(t, "ra", true)
	if !f.routes.rigGraphRelicFree("ra") {
		t.Error("a rig with an import marker and a complete cleared note does not read relic-free")
	}
	if graphRelicLegForScope(f.routes, rigGraphScope("ra")) {
		t.Error("graphRelicLegForScope still keeps the leg for an imported rig")
	}

	if err := os.WriteFile(filepath.Join(f.markers, rigGraphCleanMarkerName("rb")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !f.routes.rigGraphRelicFree("rb") {
		t.Error("a rig with a clean census record does not read relic-free")
	}

	if err := os.WriteFile(rigGraphClearedNotePath(f.cityPath, "rc"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.markers, rigGraphImportMarkerName("rc")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if f.routes.rigGraphRelicFree("rc") {
		t.Error("an unreadable cleared note reads relic-free; an unread record has proven nothing")
	}
	for _, bad := range []string{"", "..", "a/b", "a\\b"} {
		if f.routes.rigGraphRelicFree(bad) {
			t.Errorf("rig name %q reads relic-free", bad)
		}
	}
	if (&storageRoutes{}).rigGraphRelicFree("rb") {
		t.Error("routes that do not know where the records live read relic-free")
	}
	if graphRelicLegForScope(f.routes, cityGraphScope()) {
		t.Error("the city scope has a relic leg; its work ledger holds the retained copies, which must never be re-read")
	}
	single := newS4RigFixture(t, false)
	if graphRelicLegForScope(single.routes, rigGraphScope("ra")) || splitRigGraphRelicFree(single.routes, "ra") {
		t.Error("a single-store city has a relic answer; there the rig store IS where the rig's graph rows live")
	}
}

// TestOpenStorageRoutesLocatesTheRigRecords pins that the routes both planes
// open carry the binding root and the rigs, so the predicate is live in
// production and not only in fixtures.
func TestOpenStorageRoutesLocatesTheRigRecords(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	cfg.Rigs = []config.Rig{{Name: "ra", Path: filepath.Join(root, "rigs", "ra")}}
	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	target := mustResolveInfraTarget(t, root, cfg)
	routes, err := openStorageRoutes(plan, target, cfg, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = routes.close() })
	if routes.rigMarkers == nil || routes.rigMarkers.bindingRoot != target.Root || len(routes.rigMarkers.rigs) != 1 {
		t.Fatalf("openStorageRoutes rigMarkers = %+v, want the binding root %q and the city's rig", routes.rigMarkers, target.Root)
	}
	if err := os.WriteFile(filepath.Join(target.Root, rigGraphCleanMarkerName("ra")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !splitRigGraphRelicFree(routes, "ra") {
		t.Error("a clean record beside the real binding root is not seen")
	}
	if got := routes.rigGraphScopeForDir(root, filepath.Join(root, "rigs", "ra")); got.rig != "ra" {
		t.Errorf("rigGraphScopeForDir = %+v, want rig ra", got)
	}
}

// bindingGetFault fails every Get with a non-not-found error.
type bindingGetFault struct{ beads.Store }

var errBindingDown = errors.New("binding briefly unreachable")

func (bindingGetFault) Get(string) (beads.Bead, error) { return beads.Bead{}, errBindingDown }

// TestGraphResidenceNeverFallsBackOnAFailedBindingRead: a binding read that
// FAILED says nothing about absence, so the relic leg must not answer it — a
// relic copy found that way could be an import's stale twin.
func TestGraphResidenceNeverFallsBackOnAFailedBindingRead(t *testing.T) {
	relic := beads.NewMemStore()
	held, err := relic.Create(beads.Bead{Title: "relic"})
	if err != nil {
		t.Fatal(err)
	}
	_, store, err := graphResidenceForTree(bindingGetFault{Store: beads.NewMemStore()}, relic, held.ID, nil)
	if !errors.Is(err, errBindingDown) || store == relic {
		t.Fatalf("graphResidenceForTree over a failing binding = (store %T, err %v), want the binding's error and no fallback", store, err)
	}
	// Control: a clean miss does fall back.
	if _, store, err := graphResidenceForTree(beads.NewMemStoreFrom(100, nil, nil), relic, held.ID, nil); err != nil || store != relic {
		t.Fatalf("clean miss = (store %T, err %v), want the relic leg", store, err)
	}
}

// relicGetFault is a rig store whose backing does not answer: its next fails
// Gets fail with a non-not-found error, and every Get does while fails is
// negative.
type relicGetFault struct {
	beads.Store
	fails int
}

var errRelicDown = errors.New("transient: rig store connection refused")

func (s *relicGetFault) Get(id string) (beads.Bead, error) {
	if s.fails == 0 {
		return s.Store.Get(id)
	}
	if s.fails > 0 {
		s.fails--
	}
	return beads.Bead{}, errRelicDown
}

// TestGraphResidenceNeverReadsAFailedRelicReadAsAMiss is the relic-side twin
// of the test above: a relic read that FAILED says nothing about absence
// either. Joined with the binding's miss it would read as not found, and a
// write on a pre-S4 tree would go to the binding instead of failing.
func TestGraphResidenceNeverReadsAFailedRelicReadAsAMiss(t *testing.T) {
	relic := &relicGetFault{Store: beads.NewMemStore(), fails: -1}
	_, store, err := graphResidenceForTree(beads.NewMemStore(), relic, "ra-1", nil)
	if !errors.Is(err, errRelicDown) || errors.Is(err, beads.ErrNotFound) || store != nil {
		t.Fatalf("graphResidenceForTree over a failing relic = (store %T, err %v), want the relic's error, no not-found and no store", store, err)
	}
	// Control: a miss on both legs is still a not-found.
	relic.fails = 0
	if _, _, err := graphResidenceForTree(beads.NewMemStore(), relic, "ra-1", nil); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("a miss on both legs = %v, want a not-found", err)
	}
}

// TestGraphResidenceAnswersAnOutOfScopeRowFromTheBinding: an id the shared
// binding holds resolves to the binding's row even when that row belongs to
// another scope, the twin rule control dispatch and the API follow. The relic
// leg's copy of the id (an import's twin) is never served in its place.
func TestGraphResidenceAnswersAnOutOfScopeRowFromTheBinding(t *testing.T) {
	binding := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ra-1", Title: "binding copy"}}, nil)
	relic := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ra-1", Title: "relic copy"}}, nil)
	b, store, err := graphResidenceForTree(binding, relic, "ra-1", func(beads.Bead) bool { return false })
	if !errors.Is(err, errGraphOutOfScope) || !errors.Is(err, beads.ErrNotFound) || store == relic {
		t.Fatalf("out-of-scope binding row = (%q from %T, err %v), want the out-of-scope not-found and no relic answer", b.Title, store, err)
	}
}

// writeS4FileCityTOML declares the file bead provider and the fixture's rigs,
// so opening a scope store never starts a Dolt server.
func writeS4FileCityTOML(t *testing.T, f s4RigFixture) {
	t.Helper()
	body := "[workspace]\nname = \"s4-city\"\n\n[beads]\nprovider = \"file\"\n"
	for _, rig := range f.cfg.Rigs {
		body += fmt.Sprintf("\n[[rigs]]\nname = %q\npath = %q\nprefix = %q\n", rig.Name, rig.Path, rig.Prefix)
	}
	if err := os.WriteFile(filepath.Join(f.cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
}
