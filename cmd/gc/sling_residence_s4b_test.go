package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The gc sling by-id read on a split city (design S4 §2.3): the id's prefix
// picks the SCOPE, residence picks the STORE. A rig-prefixed id can live in
// the city binding — MC's binding holds ~648 rig-prefixed relics the city
// migration moved, and `gc storage migrate --from-rigs` keeps ids — so the
// probe asks the binding first, and openSlingStore must hand sling the store
// the probe found the bead in.

// seedS4BindingRelic writes a rig-prefixed row into the binding the way the
// importer does: through the engine's foreign-id create.
func seedS4BindingRelic(t *testing.T, binding beads.Store, id, title string) beads.Bead {
	t.Helper()
	creator, ok := binding.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("binding %T cannot keep a foreign id", binding)
	}
	b, err := creator.CreateWithForeignID(beads.Bead{ID: id, Title: title, Type: "task"})
	if err != nil {
		t.Fatalf("seeding %s in the binding: %v", id, err)
	}
	return b
}

func seedS4RigRow(t *testing.T, rig beads.Store, id, title string) {
	t.Helper()
	fs, ok := rig.(*beads.FileStore)
	if !ok {
		t.Fatalf("rig store %T is not a file store", rig)
	}
	fs.HonorExplicitIDs = true
	if _, err := fs.Create(beads.Bead{ID: id, Title: title, Type: "task"}); err != nil {
		t.Fatalf("seeding %s in the rig: %v", id, err)
	}
}

// slingStoreFor runs the probe and openSlingStore as cmdSling does and
// returns the scope dir and the bead as read from the store sling acts on.
func slingStoreFor(t *testing.T, f s4RigFixture, id string) (string, beads.Bead) {
	t.Helper()
	probe, err := probeExistingSlingSourceBead(f.cfg, f.cityPath, id)
	if err != nil {
		t.Fatalf("probe %s: %v", id, err)
	}
	if !probe.exists {
		t.Fatalf("probe did not find %s", id)
	}
	dir, store, code, msg := openSlingStore(f.cfg, f.cityPath, id, probe, config.Agent{})
	if code != "" {
		t.Fatalf("openSlingStore %s: %s %s", id, code, msg)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("the store openSlingStore returned does not hold %s, so the probe and the write disagree: %v", id, err)
	}
	return dir, got
}

// TestSlingProbeAndStoreAgreeOnARigPrefixedBindingRelic is the agreement
// test: the bead is found in the binding, sling acts on the binding, and the
// scope is still the rig's.
func TestSlingProbeAndStoreAgreeOnARigPrefixedBindingRelic(t *testing.T) {
	f := newS4RigFixture(t, true)
	writeS4FileCityTOML(t, f)
	want := seedS4BindingRelic(t, f.binding, "ra-relic1", "imported relic")
	dir, got := slingStoreFor(t, f, want.ID)
	if got.Title != want.Title {
		t.Errorf("sling acts on %q, want the binding row %q", got.Title, want.Title)
	}
	if dir != resolveStoreScopeRoot(f.cityPath, f.rigPath("ra")) {
		t.Errorf("scope dir = %s, want rig ra's (the prefix still picks the scope)", dir)
	}
}

// TestSlingResolvesATwinToTheBindingRow: an id held by both the rig store and
// the binding (between an import's copy and its clear) is the binding's.
func TestSlingResolvesATwinToTheBindingRow(t *testing.T) {
	f := newS4RigFixture(t, true)
	writeS4FileCityTOML(t, f)
	seedS4RigRow(t, f.rigs["ra"], "ra-twin1", "frozen rig copy")
	seedS4BindingRelic(t, f.binding, "ra-twin1", "binding row")
	_, got := slingStoreFor(t, f, "ra-twin1")
	if got.Title != "binding row" {
		t.Errorf("a twin resolved to %q, want the binding row", got.Title)
	}
}

// TestSlingOpensTheRigStoreForRigWork: rig work the binding does not hold is
// read and slung in the rig store, exactly as before.
func TestSlingOpensTheRigStoreForRigWork(t *testing.T) {
	f := newS4RigFixture(t, true)
	writeS4FileCityTOML(t, f)
	seedS4RigRow(t, f.rigs["ra"], "ra-work1", "rig work")
	dir, got := slingStoreFor(t, f, "ra-work1")
	if got.Title != "rig work" || dir != resolveStoreScopeRoot(f.cityPath, f.rigPath("ra")) {
		t.Errorf("rig work = %q in %s, want it from rig ra", got.Title, dir)
	}
}

// TestSlingSingleStoreCityNeverProbesABinding: a city that relocates nothing
// reads the rig store by prefix, as it always did.
func TestSlingSingleStoreCityNeverProbesABinding(t *testing.T) {
	f := newS4RigFixture(t, false)
	writeS4FileCityTOML(t, f)
	seedS4RigRow(t, f.rigs["ra"], "ra-work1", "rig work")
	dir, got := slingStoreFor(t, f, "ra-work1")
	if got.Title != "rig work" || dir != resolveStoreScopeRoot(f.cityPath, f.rigPath("ra")) {
		t.Errorf("single-store sling = %q in %s, want the rig store by prefix", got.Title, dir)
	}
	probe, err := probeExistingSlingSourceBead(f.cfg, f.cityPath, "ra-absent")
	if err != nil || probe.exists {
		t.Errorf("single-store probe of an absent id = %+v %v", probe, err)
	}
}

// slingBindingReadFails is a binding whose by-id read fails (a locked or
// unreachable database), not a clean miss.
type slingBindingReadFails struct{ beads.Store }

func (slingBindingReadFails) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("binding read failed: database is locked")
}

// TestSlingRigWorkSurvivesABindingReadFailure: the binding probe is only the
// residence check; a binding that cannot be read must not fail a sling of
// rig work the rig store holds. Only when the rig store misses too does the
// probe report the binding read failure, by name.
func TestSlingRigWorkSurvivesABindingReadFailure(t *testing.T) {
	f := newS4RigFixture(t, true)
	writeS4FileCityTOML(t, f)
	routes := splitEnvRoutes(slingBindingReadFails{f.binding})
	routes.rigMarkers = f.routes.rigMarkers
	seedCLIStorageRoutes(t, f.cityPath, routes)
	seedS4RigRow(t, f.rigs["ra"], "ra-work1", "rig work")
	dir, got := slingStoreFor(t, f, "ra-work1")
	if got.Title != "rig work" || dir != resolveStoreScopeRoot(f.cityPath, f.rigPath("ra")) {
		t.Errorf("rig work = %q in %s, want it from rig ra", got.Title, dir)
	}
	_, err := probeExistingSlingSourceBead(f.cfg, f.cityPath, "ra-absent")
	if err == nil || !strings.Contains(err.Error(), "database is locked") || !strings.Contains(err.Error(), "binding") {
		t.Errorf("a miss in both stores with an unreadable binding = %v, want the binding read failure named", err)
	}
}
