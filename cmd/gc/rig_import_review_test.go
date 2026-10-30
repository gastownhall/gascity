package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// Review of #8486: the importer's data-safety must-fixes, each on real stores
// (a real SQLite binding, real file rig stores, and a real bd rig where the
// reviewer used one).

// crashOnceAt makes the import stop at step on its first pass only.
func crashOnceAt(t *testing.T, step string) {
	t.Helper()
	crashed := false
	prev := rigImportCrashPoint
	rigImportCrashPoint = func(s string) error {
		if s == step && !crashed {
			crashed = true
			return errors.New("killed at " + s)
		}
		return nil
	}
	t.Cleanup(func() { rigImportCrashPoint = prev })
}

func twoStepRig(t *testing.T) *rigImportFixture {
	t.Helper()
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "open", nil), graphRow("gp-b", "gp-a", "open", nil)},
		[]beads.Dep{{IssueID: "gp-b", DependsOnID: "gp-a", Type: "blocks"}})
	return f
}

// MF1: a crash after the rows and before their edges leaves rows the binding
// would serve as ready. The in-flight record holds boot and every CLI route
// (storageBootGate) until the import finishes.
func TestReviewHalfFinishedCopyIsNeverServed(t *testing.T) {
	f := twoStepRig(t)
	crashOnceAt(t, "rows")
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "killed at rows") {
		t.Fatalf("the crash did not stop the import: %s", stderr)
	}
	routes, err := storageBootGate(f.request.CityPath, f.cfg, "gc start", nil, io.Discard)
	if err == nil {
		_ = routes.close()
		t.Fatal("boot served a binding holding a half-finished rig copy (rows without their edges)")
	}
	if !strings.Contains(err.Error(), "in flight") || !strings.Contains(err.Error(), rigImportInstruction("packs")) {
		t.Errorf("the refusal does not say the import is in flight and name the command: %v", err)
	}
	if f.relicFree("packs") {
		t.Error("a rig with an import in flight reads as relic-free")
	}
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("the resume = %d: %s %s", code, o, e)
	}
	routes, err = storageBootGate(f.request.CityPath, f.cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("boot refuses a finished import: %v", err)
	}
	_ = routes.close()
}

// MF1: a bd CLI rig cannot report edge payloads. The plan refuses before
// anything is written, rather than copying every row and then failing at the
// first edge, forever.
func TestReviewBdRigWithEdgesRefusesBeforeAnyWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("real bd rig")
	}
	rig, dir := newRealBdRigStore(t)
	root, err := rig.Create(beads.Bead{Title: "a pre-S4 workflow", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow", beadmeta.RootBeadIDMetadataKey: "self"}})
	if err != nil {
		t.Fatal(err)
	}
	step, err := rig.Create(beads.Bead{Title: "a step", Type: "task", Metadata: beads.StringMap{"gc.kind": "task", beadmeta.RootBeadIDMetadataKey: root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.DepAdd(step.ID, root.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	f := newRigImportFixture(t, config.Rig{Name: "bdrig", Prefix: "tst", Path: dir})
	f.fixed["bdrig"] = rig
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "cannot report the payload") {
		t.Fatalf("a bd rig with an edge was not refused up front: %d %s", code, stderr)
	}
	if n := len(rigTestList(t, f.binding())); n != 0 {
		t.Errorf("the refused import wrote %d binding row(s)", n)
	}
	if fileExists(f.loc("bdrig").InFlightPath()) || fileExists(f.loc("bdrig").MarkerPath()) {
		t.Error("the refused import left a record")
	}
}

// MF1: a resume verifies the interrupted attempt's copies instead of deleting
// and re-copying them blindly: a binding copy that was written to since is
// named, and kept.
func TestReviewResumeKeepsAnUpdatedBindingCopy(t *testing.T) {
	f := twoStepRig(t)
	crashOnceAt(t, "rows")
	_, _, _ = f.importRigs()
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	title := "updated in the binding"
	if err := binding.Update("gp-a", beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-a") {
		t.Fatalf("the resume did not refuse over the changed binding copy: %d %s", code, stderr)
	}
	if got, err := f.binding().Get("gp-a"); err != nil || got.Title != title {
		t.Errorf("the resume discarded the binding copy's update: %q %v", got.Title, err)
	}
}

// MF2: prepare never deletes a stamped binding row the rig no longer holds as
// the same bead: it may be the binding's only copy.
func TestReviewPrepareKeepsTheBindingsOnlyCopy(t *testing.T) {
	f := twoStepRig(t)
	crashOnceAt(t, "rows")
	_, _, _ = f.importRigs()
	if err := f.rig("packs").Delete("gp-b"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-b") || !strings.Contains(stderr, "only copy") {
		t.Fatalf("prepare did not refuse over the binding's only copy: %d %s", code, stderr)
	}
	if _, err := f.binding().Get("gp-b"); err != nil {
		t.Errorf("prepare deleted the binding's only copy of gp-b: %v", err)
	}
}

// MF2: a cleared note with no marker means rows may already be gone from the
// rig; the import refuses. The marker-without-manifest remedy never says to
// remove the marker.
func TestReviewClearedNoteWithoutMarkerRefuses(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	loc := f.loc("packs")
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Complete: true}); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "no import marker") {
		t.Fatalf("a cleared note without a marker did not refuse: %d %s", code, stderr)
	}
	if n := len(rigTestList(t, f.binding())); n != 0 {
		t.Errorf("the refused import wrote %d binding row(s)", n)
	}
	_ = os.Remove(loc.NotePath())
	if err := os.WriteFile(loc.MarkerPath(), rigTestRecord(f.request.CityPath, f.rigPaths["packs"]), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := loc.readManifest()
	if err == nil || strings.Contains(err.Error(), "or remove the marker") || !strings.Contains(err.Error(), "Do not remove the marker") {
		t.Errorf("the marker-without-manifest remedy = %v", err)
	}
}

// MF3: a resumed import proves every carried row against the binding first:
// a rig row whose binding copy is gone is named and not deleted (the plan
// refuses it; the clear's re-proof checks again just before it deletes).
func TestReviewResumedClearKeepsRowsTheBindingLacks(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), graphRow("gp-b", "gp-a", "closed", nil)}, nil)
	crashOnceAt(t, "pending")
	_, _, _ = f.importRigs()
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Delete("gp-a"); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-a") || !strings.Contains(stderr, "only copy") {
		t.Fatalf("the resumed clear did not refuse over a row the binding lacks: %d %s", code, stderr)
	}
	if _, err := f.rig("packs").Get("gp-a"); err != nil {
		t.Errorf("the resumed clear deleted gp-a, whose binding copy is gone: %v", err)
	}
}

// MF4: an open blocker held in another rig's store is encoded as an S3b gate,
// never dropped; with a store unreadable the import refuses; a blocker no
// store holds is dropped as proven gone.
func TestReviewBlockerHeldInAnotherStoreIsGated(t *testing.T) {
	t.Run("held by another rig: gate", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
		f.seed("other", []beads.Bead{workRow("go-x", "open")}, nil)
		f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil)}, []beads.Dep{{IssueID: "gp-d", DependsOnID: "go-x", Type: "blocks"}})
		if code, o, e := f.importRigs("packs"); code != 0 {
			t.Fatalf("import = %d: %s %s", code, o, e)
		}
		binding := f.binding()
		gates, _ := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if len(gates) != 1 || gates[0].Metadata[beadmeta.CrossStoreBlockerMetadataKey] != "go-x" {
			t.Fatalf("gates = %+v, want one gate holding gp-d on go-x", gates)
		}
		deps, _ := binding.DepList("gp-d", "down")
		if len(deps) != 1 || deps[0].DependsOnID != gates[0].ID {
			t.Errorf("gp-d's edges = %v, want its gate", deps)
		}
		assertNoCrossStoreEdge(t, binding)
	})
	t.Run("a store unreadable: refuse", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
		f.open["other"] = func() (beads.Store, error) { return beads.NewMemStore(), errors.New("dolt server unreachable") }
		f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil)}, []beads.Dep{{IssueID: "gp-d", DependsOnID: "go-x", Type: "blocks"}})
		code, _, stderr := f.importRigs("packs")
		if code == 0 || !strings.Contains(stderr, "go-x") || !strings.Contains(stderr, "could not be read") {
			t.Fatalf("an unprovable blocker was not refused: %d %s", code, stderr)
		}
	})
	t.Run("absent from every store: drop", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
		f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil)}, []beads.Dep{{IssueID: "gp-d", DependsOnID: "go-gone", Type: "blocks"}})
		if code, o, e := f.importRigs("packs"); code != 0 {
			t.Fatalf("import = %d: %s %s", code, o, e)
		}
		d, _ := f.binding().Get("gp-d")
		if !strings.Contains(d.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], "absent from every store") {
			t.Errorf("the dropped edge's record = %q", d.Metadata[beadmeta.MigratedCrossEdgesMetadataKey])
		}
	})
}

// MF5: the clear removes the boundary edges before it deletes, so the delete
// bd's storage runs finds no surviving neighbor and rewrites no work row's
// text to "[deleted:<id>]". It runs on a native Dolt rig, the delete bd's CLI
// runs too: a bd CLI rig with an edge on a row it moves is refused before
// anything is written (TestReviewBdRigWithEdgesRefusesBeforeAnyWrite), so it
// never reaches the clear.
func TestReviewClearLeavesSurvivingWorkTextUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("real Dolt rig")
	}
	rig := newRealDoltRigStore(t)
	desc := "drain unit for gd-d; see gd-d for the run"
	desc2 := "context for gd-d"
	seedRealDoltRow(t, rig, graphRow("gd-d", "gd-d", "", nil), false)
	seedRealDoltRow(t, rig, beads.Bead{ID: "gd-w", Title: "a drain-unit convoy", Type: "task", Description: desc}, false)
	seedRealDoltRow(t, rig, beads.Bead{ID: "gd-w2", Title: "a note", Type: "task", Description: desc2}, false)
	if err := rig.DepAdd("gd-w", "gd-d", "tracks"); err != nil {
		t.Fatal(err)
	}
	// And a work row the moved row points AT (D -> W2): bd treats both ends
	// of an edge as neighbors.
	if err := rig.DepAdd("gd-d", "gd-w2", "related"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gd-d", "gd-w"} {
		if err := rig.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	f := newRigImportFixture(t, config.Rig{Name: "dolt", Prefix: "gd"})
	f.fixed["dolt"] = nonClosingStore{rig}
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, o, e)
	}
	for id, want := range map[string]string{"gd-w": desc, "gd-w2": desc2} {
		got, err := rig.Get(id)
		if err != nil || got.Description != want {
			t.Errorf("the clear rewrote surviving work row %s's text:\n got %q (%v)\nwant %q", id, got.Description, err, want)
		}
	}
}

// MF8: a rig store another registered city also uses is refused, by scope
// root and by Dolt database; rows scoped to another rig, and open rows
// another city's agent claims, are refused too.
func TestReviewSharedRigStoreRefuses(t *testing.T) {
	seedPacks := func(f *rigImportFixture) {
		f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	}
	t.Run("same scope root in another city", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		seedPacks(f)
		other := t.TempDir()
		toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", f.rigPaths["packs"])
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		f.otherCities = []string{other}
		code, _, stderr := f.importRigs()
		if code == 0 || !strings.Contains(stderr, "also city") {
			t.Fatalf("a shared rig store was imported: %d %s", code, stderr)
		}
		if n := len(rigTestList(t, f.binding())); n != 0 {
			t.Errorf("the refused import wrote %d binding row(s)", n)
		}
	})
	t.Run("same Dolt database in another city", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		seedPacks(f)
		other := t.TempDir()
		toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", filepath.Join(other, "elsewhere"))
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		f.otherCities = []string{other}
		prev := rigImportScopeDoltTarget
		rigImportScopeDoltTarget = func(string, string) rigDoltIdentity {
			return rigDoltIdentity{Database: "packs", Endpoint: "loopback:3399"}
		}
		t.Cleanup(func() { rigImportScopeDoltTarget = prev })
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "Dolt database") {
			t.Fatalf("a rig on another city's Dolt database was imported: %d %s", code, stderr)
		}
	})
	t.Run("a row scoped to another rig", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:other"})}, nil)
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "rig:other") {
			t.Fatalf("a row scoped to another rig was imported: %d %s", code, stderr)
		}
	})
	t.Run("an open row another city claims", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		claimed := graphRow("gp-a", "gp-a", "open", nil)
		claimed.Assignee = "qcore/gc.run-operator-36"
		f.seed("packs", []beads.Bead{claimed}, nil)
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "claimed by qcore/gc.run-operator-36") {
			t.Fatalf("a row another city claims was imported: %d %s", code, stderr)
		}
	})
}

// Should-fix: a writer that changes a rig row between the copy and the clear
// stops the clear; the row is not deleted.
func TestReviewClearRefusesARowChangedSinceTheCensus(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	prev := rigImportCrashPoint
	rigImportCrashPoint = func(step string) error {
		if step == "reserved" {
			title := "written by a straggler"
			return f.rig("packs").Update("gp-a", beads.UpdateOpts{Title: &title})
		}
		return nil
	}
	t.Cleanup(func() { rigImportCrashPoint = prev })
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "changed after the census") {
		t.Fatalf("a row changed under the clear was not refused: %d %s", code, stderr)
	}
	if got, err := f.rig("packs").Get("gp-a"); err != nil || got.Title != "written by a straggler" {
		t.Errorf("the clear removed or reverted the changed row: %+v %v", got, err)
	}
}

// Should-fix: max_hash_length is raised when the floor is above it.
func TestReviewHashFloorRaisesMaxHashLength(t *testing.T) {
	conf := &fakeRigHashConfig{values: map[string]string{bdMaxHashLengthKey: "5"}}
	floor, err := rigImportReserveIDs(conf, &infraCopyManifest{IDs: map[string]bool{"gp-k0nvi": true}})
	if err != nil || floor != 6 {
		t.Fatalf("floor = %d %v, want 6", floor, err)
	}
	if conf.values[bdMinHashLengthKey] != "6" || conf.values[bdMaxHashLengthKey] != "6" {
		t.Errorf("config = %v, want min and max 6", conf.values)
	}
}

// Should-fix: an exec provider is refused through the production config
// opener before anything is written (not after the marker).
func TestReviewExecProviderRefusesBeforeAnyWrite(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	rigImportHashConfigOpener = defaultRigImportHashConfigOpener
	if err := os.WriteFile(filepath.Join(f.request.CityPath, "city.toml"), []byte("[workspace]\nname = \"s4b\"\n\n[beads]\nprovider = \"exec:/bin/true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "exec:") {
		t.Fatalf("an exec provider rig was not refused: %d %s", code, stderr)
	}
	if fileExists(f.loc("packs").MarkerPath()) || len(rigTestList(t, f.binding())) != 0 {
		t.Error("the exec refusal came after writes")
	}
}

// Should-fix: --abandon-rig-store records the clear complete only once the
// binding is proven to hold every row the manifest records.
func TestReviewAbandonRigStore(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), graphRow("gp-b", "gp-a", "closed", nil)}, nil)
	crashOnceAt(t, "pending")
	_, _, _ = f.importRigs()
	if err := rigBootRefusal(f.request.CityPath, f.target); err == nil {
		t.Fatal("precondition: boot does not refuse the pending clear")
	}
	f.open["packs"] = func() (beads.Store, error) { return beads.NewMemStore(), errors.New("the rig volume is gone") }
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	gone, _ := binding.Get("gp-b")
	if err := binding.Delete("gp-b"); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	var stdout, stderr bytes.Buffer
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gp-b") {
		t.Fatalf("abandon proceeded with a row the binding lacks: %d %s", code, stderr.String())
	}
	binding, err = openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(gone); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	stderr.Reset()
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code != 0 {
		t.Fatalf("abandon = %d: %s", code, stderr.String())
	}
	if err := rigBootRefusal(f.request.CityPath, f.target); err != nil {
		t.Errorf("boot still refuses after the abandon: %v", err)
	}
}

// Should-fix: records speak for the store they name; a new store under a
// reused rig name is not relic-free, and the import refuses until the old
// records are moved aside.
func TestReviewReusedRigNameIsNotRelicFree(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, o, e)
	}
	if !f.relicFree("packs") {
		t.Fatal("precondition: the imported rig is not relic-free")
	}
	newPath := t.TempDir()
	f.cfg.Rigs[0].Path = newPath
	f.rigPaths["packs"] = newPath
	if f.relicFree("packs") {
		t.Error("a new store under a reused rig name reads as relic-free")
	}
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "reused") {
		t.Errorf("the import ran over another store's records: %d %s", code, stderr)
	}
}

// The same for a clean record: it speaks only for the store it censused.
func TestReviewReusedRigNameCleanRecordIsNotRelicFree(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "quiet", Prefix: "gq"})
	f.seed("quiet", []beads.Bead{workRow("gq-1", "open")}, nil)
	if code, o, e := f.importRigs(); code != 0 || !f.relicFree("quiet") {
		t.Fatalf("precondition: clean record = %d %s %s", code, o, e)
	}
	newPath := t.TempDir()
	f.cfg.Rigs[0].Path = newPath
	f.rigPaths["quiet"] = newPath
	if f.relicFree("quiet") {
		t.Error("a new store under a reused rig name reads as relic-free on the old store's clean record")
	}
}

// Should-fix: preflight words a rig refusal as the rig import's, not the
// city migration's.
func TestReviewPreflightWordsARigRefusalAsTheRigImports(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-d", "gp-d", "open", nil), workRow("gp-w", "open")}, []beads.Dep{{IssueID: "gp-w", DependsOnID: "gp-d", Type: "tracks"}})
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(f.request, &stdout, &stderr); code == 0 {
		t.Fatalf("preflight cleared an uncodable rig edge: %s", stdout.String())
	}
	if out := stdout.String(); strings.Contains(out, "the migration would refuse") || !strings.Contains(out, "would refuse the rig(s) named above") {
		t.Errorf("preflight wording = %q", out)
	}
}

// Should-fix: the importer through the production open path: a file-provider
// city whose rig store the storage-admin opener opens, no seam.
func TestReviewImportThroughTheProductionOpenPath(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), workRow("gp-w", "open")}, nil)
	if err := os.WriteFile(filepath.Join(f.request.CityPath, "city.toml"), []byte("[workspace]\nname = \"s4b\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := rigImportStoreOpener
	rigImportStoreOpener = func(cityPath string, rig config.Rig) (beads.Store, error) {
		return openPhysicalScopeStore(rig.Path, cityPath)
	}
	t.Cleanup(func() { rigImportStoreOpener = prev })
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import through openStoreAtForCity = %d: %s %s", code, o, e)
	}
	if got := rigTestIDs(rigTestList(t, f.rig("packs"))); strings.Join(got, ",") != "gp-w" {
		t.Errorf("the rig holds %v, want only gp-w", got)
	}
	if _, err := f.binding().Get("gp-a"); err != nil {
		t.Errorf("the binding does not hold gp-a: %v", err)
	}
}

// Re-review MUST: --abandon-rig-store never serves a half copy. After an
// incremental import crashed between its rows and its edges, abandon refuses
// (the in-flight record, and binding rows the manifest does not record), and
// it refuses a rig store that is still reachable.
func TestReviewAbandonRefusesAHalfCopyAndAReachableStore(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("first import = %d: %s %s", code, o, e)
	}
	f.seed("packs", []beads.Bead{graphRow("gp-x", "gp-x", "open", nil), graphRow("gp-y", "gp-x", "open", nil)},
		[]beads.Dep{{IssueID: "gp-y", DependsOnID: "gp-x", Type: "blocks"}})
	crashOnceAt(t, "rows")
	_, _, _ = f.importRigs()

	var stdout, stderr bytes.Buffer
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "reachable") {
		t.Fatalf("abandon of a reachable rig store = %d %s", code, stderr.String())
	}
	f.open["packs"] = func() (beads.Store, error) { return beads.NewMemStore(), errors.New("the rig volume is gone") }
	stderr.Reset()
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "in flight") {
		t.Fatalf("abandon over an import in flight = %d %s", code, stderr.String())
	}
	_ = os.Remove(f.loc("packs").InFlightPath())
	stderr.Reset()
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gp-x") {
		t.Fatalf("abandon over binding rows the manifest does not record = %d %s", code, stderr.String())
	}
}

// Re-review should-fix: boot reads the records on disk, so a rig removed
// from config after a crash still holds it.
func TestReviewBootRefusesAnInFlightImportOfARigNoLongerConfigured(t *testing.T) {
	f := twoStepRig(t)
	crashOnceAt(t, "rows")
	_, _, _ = f.importRigs()
	f.cfg.Rigs = nil
	if err := rigBootRefusal(f.request.CityPath, f.target); err == nil || !strings.Contains(err.Error(), "rig packs") {
		t.Fatalf("boot served a half copy of a rig dropped from config: %v", err)
	}
}

// Re-review should-fix: Dolt endpoints are compared normalized, never by
// database name alone, and an endpoint that cannot be resolved with a
// matching name refuses with a remedy.
func TestReviewSharedDoltIdentityIsNormalized(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "::1", "[::1]", "", "0.0.0.0", "::"} {
		if got, err := normalizeDoltHost(h); err != nil || got != "loopback" {
			t.Errorf("normalizeDoltHost(%q) = %q %v, want loopback", h, got, err)
		}
	}
	prevLookup := rigImportLookupHost
	rigImportLookupHost = func(host string) ([]string, error) {
		if host == "dolt.internal" {
			return []string{"10.0.0.5"}, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { rigImportLookupHost = prevLookup })
	if got, _ := normalizeDoltHost("dolt.internal"); got != "10.0.0.5" {
		t.Errorf("a host name does not resolve to its address: %q", got)
	}
	run := func(t *testing.T, self, theirs rigDoltIdentity) (int, string) {
		t.Helper()
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
		other := t.TempDir()
		toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", filepath.Join(other, "elsewhere"))
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		f.otherCities = []string{other}
		prev := rigImportScopeDoltTarget
		rigImportScopeDoltTarget = func(cityPath, _ string) rigDoltIdentity {
			if cityPath == other {
				return theirs
			}
			return self
		}
		t.Cleanup(func() { rigImportScopeDoltTarget = prev })
		code, _, stderr := f.importRigs()
		return code, stderr
	}
	t.Run("same name, different endpoints: imports", func(t *testing.T) {
		if code, stderr := run(t, rigDoltIdentity{Database: "gp", Endpoint: "loopback:3307"}, rigDoltIdentity{Database: "gp", Endpoint: "loopback:3407"}); code != 0 {
			t.Errorf("two databases that share only a name were refused: %s", stderr)
		}
	})
	t.Run("same name and endpoint: refuses", func(t *testing.T) {
		if code, stderr := run(t, rigDoltIdentity{Database: "gp", Endpoint: "loopback:3307"}, rigDoltIdentity{Database: "gp", Endpoint: "loopback:3307"}); code == 0 || !strings.Contains(stderr, "Dolt database gp") {
			t.Errorf("a shared Dolt database was imported: %s", stderr)
		}
	})
	t.Run("their endpoint unresolved, same name: refuses with a remedy", func(t *testing.T) {
		if code, stderr := run(t, rigDoltIdentity{Database: "gp", Endpoint: "loopback:3307"}, rigDoltIdentity{Database: "gp", Unresolved: true, Err: errors.New("server down")}); code == 0 || !strings.Contains(stderr, "unregister") {
			t.Errorf("an unresolved endpoint with the same name was not refused with a remedy: %s", stderr)
		}
	})
	t.Run("their endpoint unresolved, other name: imports", func(t *testing.T) {
		if code, stderr := run(t, rigDoltIdentity{Database: "gp", Endpoint: "loopback:3307"}, rigDoltIdentity{Database: "ga", Unresolved: true, Err: errors.New("server down")}); code != 0 {
			t.Errorf("an unrelated unresolved database was refused: %s", stderr)
		}
	})
}

// Re-review M8: a binding copy changed between the copy and the clear stops
// the import. Verify compares fields, and so does the clear-time re-check.
func TestReviewCorruptedBindingCopyStopsTheImport(t *testing.T) {
	for _, step := range []string{"rows", "reserved"} {
		t.Run(step, func(t *testing.T) {
			f := twoStepRig(t)
			prev := rigImportCrashPoint
			rigImportCrashPoint = func(s string) error {
				if s != step {
					return nil
				}
				binding, err := openInfraDestination(f.target)
				if err != nil {
					return err
				}
				defer closeBeadStoreHandle(binding) //nolint:errcheck
				title := "corrupted"
				return binding.Update("gp-a", beads.UpdateOpts{Title: &title})
			}
			t.Cleanup(func() { rigImportCrashPoint = prev })
			code, _, stderr := f.importRigs()
			if code == 0 || !strings.Contains(stderr, "gp-a") {
				t.Fatalf("a corrupted binding copy did not stop the import at %s: %d %s", step, code, stderr)
			}
			if _, err := f.rig("packs").Get("gp-a"); err != nil {
				t.Errorf("the rig row was removed despite a corrupted binding copy: %v", err)
			}
		})
	}
	t.Run("a missing edge at clear time", func(t *testing.T) {
		f := twoStepRig(t)
		prev := rigImportCrashPoint
		rigImportCrashPoint = func(s string) error {
			if s != "reserved" {
				return nil
			}
			binding, err := openInfraDestination(f.target)
			if err != nil {
				return err
			}
			defer closeBeadStoreHandle(binding) //nolint:errcheck
			return binding.DepRemove("gp-b", "gp-a")
		}
		t.Cleanup(func() { rigImportCrashPoint = prev })
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "lacks its edge") {
			t.Fatalf("a binding copy missing its edge did not stop the clear: %d %s", code, stderr)
		}
	})
	t.Run("a binding copy gone at clear time", func(t *testing.T) {
		f := twoStepRig(t)
		prev := rigImportCrashPoint
		rigImportCrashPoint = func(s string) error {
			if s != "reserved" {
				return nil
			}
			binding, err := openInfraDestination(f.target)
			if err != nil {
				return err
			}
			defer closeBeadStoreHandle(binding) //nolint:errcheck
			return binding.Delete("gp-a")
		}
		t.Cleanup(func() { rigImportCrashPoint = prev })
		if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "gp-a") || !strings.Contains(stderr, "only copy") {
			t.Fatalf("a binding copy removed under the clear did not stop it: %d %s", code, stderr)
		}
		if _, err := f.rig("packs").Get("gp-a"); err != nil {
			t.Errorf("the clear removed gp-a, whose binding copy is gone: %v", err)
		}
	})
}

// Re-review M12: a marker whose rig still holds rows its import carried is
// not "imported", even with a complete cleared note.
func TestReviewMarkerWithCarriedRowsLeftIsRetained(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	crashOnceAt(t, "pending")
	_, _, _ = f.importRigs()
	loc := f.loc("packs")
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Complete: true}); err != nil {
		t.Fatal(err)
	}
	rows := censusRigs(f.request.CityPath, f.cfg, f.target, false)
	if len(rows) != 1 || rows[0].State != rigCensusRetained || f.relicFree("packs") && rows[0].State == rigCensusImported {
		t.Fatalf("census = %+v, want retained", rows)
	}
}

// Re-review M15: two rigs on one database are refused by name.
func TestReviewTwoRigsOnOneDatabaseRefuse(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "one", Prefix: "ga"}, config.Rig{Name: "two", Prefix: "gb"})
	shared := beads.NewMemStoreFrom(0, []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	f.fixed["one"], f.fixed["two"] = shared, shared
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "rig one: REFUSED") || !strings.Contains(stderr, "rig two: REFUSED") || !strings.Contains(stderr, "one database") {
		t.Fatalf("two rigs on one database were not refused: %d %s", code, stderr)
	}
	if n := len(rigTestList(t, f.binding())); n != 0 {
		t.Errorf("the refused import wrote %d binding row(s)", n)
	}
}

// Re-review should-fix: openPhysicalScopeStore (through openStorageScopeStore)
// reads rows of every class, past the split-strict work view, so its callers
// are pinned: only the rig importer's opener and its census's emptiness check.
func TestReviewPhysicalScopeOpenerCallersArePinned(t *testing.T) {
	want := map[string]int{"rig_import.go": 1, "rig_import_census.go": 1}
	got := map[string]int{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		n := strings.Count(string(data), "openStorageScopeStore(") + strings.Count(string(data), "openPhysicalScopeStore(")
		if file == "cmd_storage.go" {
			n -= strings.Count(string(data), "func openPhysicalScopeStore(")
		}
		if n > 0 {
			got[file] = n
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("physical scope opener callers = %v, want %v; a new caller reads infrastructure rows from a rig ledger and needs review", got, want)
	}
}

// Approval follow-up: through the REAL resolver, another city whose Dolt
// endpoint cannot be resolved (its runtime state is gone) is compared by the
// database its rig pins in .beads/metadata.json: an unrelated name imports,
// the same name refuses, and an unreadable pin refuses.
func TestReviewUnresolvedDoltEndpointComparesByPinnedDatabase(t *testing.T) {
	writeScope := func(t *testing.T, root, config, metadata string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".beads", "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		if metadata != "" {
			if err := os.WriteFile(filepath.Join(root, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	run := func(t *testing.T, theirMetadata string) (int, string) {
		t.Helper()
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
		writeScope(t, f.rigPaths["packs"], "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: 127.0.0.1\ndolt.port: 3307\n", `{"backend":"dolt","dolt_mode":"server","dolt_database":"gp"}`)
		if self := rigImportScopeDoltTarget(f.request.CityPath, rigStoreRoot(f.request.CityPath, f.cfg.Rigs[0])); self.Database != "gp" || self.Unresolved {
			t.Fatalf("precondition: this rig's identity = %+v", self)
		}
		other := t.TempDir()
		theirs := filepath.Join(other, "rigs", "theirs")
		toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", theirs)
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		// An inherited endpoint whose city runtime state is gone: the real
		// resolver cannot say where the database lives.
		writeScope(t, theirs, "gc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\n", theirMetadata)
		if id := rigImportScopeDoltTarget(other, theirs); !id.Unresolved {
			t.Fatalf("precondition: their endpoint resolved: %+v", id)
		}
		f.otherCities = []string{other}
		code, _, stderr := f.importRigs()
		return code, stderr
	}
	t.Run("unrelated pinned name: imports", func(t *testing.T) {
		if code, stderr := run(t, `{"backend":"dolt","dolt_mode":"server","dolt_database":"zz"}`); code != 0 {
			t.Errorf("an unrelated database behind an unresolved endpoint refused the import: %s", stderr)
		}
	})
	t.Run("same pinned name: refuses", func(t *testing.T) {
		if code, stderr := run(t, `{"backend":"dolt","dolt_mode":"server","dolt_database":"gp"}`); code == 0 || !strings.Contains(stderr, "unregister") {
			t.Errorf("the same database name behind an unresolved endpoint was imported: %s", stderr)
		}
	})
	t.Run("unreadable pin: refuses", func(t *testing.T) {
		if code, stderr := run(t, ""); code == 0 || !strings.Contains(stderr, "unregister") {
			t.Errorf("an unresolved endpoint with no readable database pin was imported: %s", stderr)
		}
	})
	// Iteration-1 major: the importing rig is the unresolved side and pins
	// no database. It cannot be told apart from another city's Dolt rig, so
	// it is compared and refused, not skipped as a file store is.
	t.Run("this rig unresolved with no pin: refuses", func(t *testing.T) {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
		writeScope(t, f.rigPaths["packs"], "gc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\n", "")
		if self := rigImportScopeDoltTarget(f.request.CityPath, rigStoreRoot(f.request.CityPath, f.cfg.Rigs[0])); !self.Unresolved || self.Database != "" {
			t.Fatalf("precondition: this rig's identity = %+v, want unresolved with no database", self)
		}
		other := t.TempDir()
		theirs := filepath.Join(other, "rigs", "theirs")
		toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", theirs)
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		writeScope(t, theirs, "gc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: 127.0.0.1\ndolt.port: 3307\n", `{"backend":"dolt","dolt_mode":"server","dolt_database":"gp"}`)
		if id := rigImportScopeDoltTarget(other, theirs); id.Unresolved || id.Database != "gp" {
			t.Fatalf("precondition: their identity = %+v, want database gp, resolved", id)
		}
		f.otherCities = []string{other}
		code, _, stderr := f.importRigs()
		if code == 0 || !strings.Contains(stderr, "pins no dolt_database") {
			t.Errorf("an importing rig with an unresolved endpoint and no database pin was imported: %d %s", code, stderr)
		}
		if n := len(rigTestList(t, f.binding())); n != 0 {
			t.Errorf("the refused import wrote %d binding row(s)", n)
		}
	})
}

// payloadlessLeaf is a rig store with no edge-payload read, as the bd CLI
// store has none: the embedded interface hides the file store's DepMetadata.
type payloadlessLeaf struct{ beads.Store }

// Iteration-1 blocker: every production open hands the importer its rig
// store wrapped in the bead policy layer, which implements DepMetadataReader
// whatever its leaf can do. A rig whose leaf cannot read a payload (the bd CLI
// store the default proxied-server topology opens, DoltLite, an exec store) is
// refused by reading the payloads first, before anything is written: for an
// edge the import copies, and for a boundary edge only the clear's backup
// records. Every re-run refuses the same way, boot is not held, and preflight
// reports the refusal.
func TestReviewPolicyWrappedPayloadlessRigRefusesBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []beads.Bead
		deps []beads.Dep
	}{
		{
			name: "an edge the import copies",
			rows: []beads.Bead{graphRow("gp-a", "gp-a", "open", nil), graphRow("gp-b", "gp-a", "open", nil)},
			deps: []beads.Dep{{IssueID: "gp-b", DependsOnID: "gp-a", Type: "blocks"}},
		},
		{
			name: "a boundary edge only the backup records",
			rows: []beads.Bead{graphRow("gp-d", "gp-d", "closed", nil), workRow("gp-w", "open")},
			deps: []beads.Dep{{IssueID: "gp-w", DependsOnID: "gp-d", Type: "related"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
			f.seed("packs", tc.rows, tc.deps)
			f.open["packs"] = func() (beads.Store, error) {
				store, err := f.openFileRig("packs")
				if err != nil {
					return nil, err
				}
				return wrapStoreWithBeadPolicies(payloadlessLeaf{store}, f.cfg), nil
			}
			for run := 1; run <= 2; run++ {
				if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "cannot report the payload") {
					t.Fatalf("run %d: a policy-wrapped rig with no payload read was not refused up front: %d %s", run, code, stderr)
				}
			}
			if n := len(rigTestList(t, f.binding())); n != 0 {
				t.Errorf("the refused import wrote %d binding row(s)", n)
			}
			loc := f.loc("packs")
			for _, path := range []string{loc.InFlightPath(), loc.MarkerPath(), loc.NotePath()} {
				if fileExists(path) {
					t.Errorf("the refused import left %s", path)
				}
			}
			if err := rigBootRefusal(f.request.CityPath, f.target); err != nil {
				t.Errorf("boot refuses after an import that wrote nothing: %v", err)
			}
			var stdout, stderr bytes.Buffer
			if code := doStoragePreflight(f.request, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "cannot report the payload") {
				t.Errorf("preflight did not report the payload refusal: %d %s %s", code, stdout.String(), stderr.String())
			}
		})
	}
}

// Iteration-1 major: a row the manifest records as carried that the binding
// no longer holds refuses the plan before an incremental import copies its
// new rows. The rig holds the only copy; an edge onto it from a new row would
// otherwise be recorded as onto a bead "absent from every store" and committed
// with the copy and the manifest before the clear's re-proof refused.
func TestReviewCarriedRowTheBindingLacksRefusesTheIncrementalCopy(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	crashOnceAt(t, "pending")
	_, _, _ = f.importRigs()
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Delete("gp-a"); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	f.seed("packs", []beads.Bead{graphRow("gp-n", "gp-a", "open", nil)}, []beads.Dep{{IssueID: "gp-n", DependsOnID: "gp-a", Type: "blocks"}})
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-a") || !strings.Contains(stderr, "only copy") {
		t.Fatalf("a carried row the binding lacks did not refuse the import: %d %s", code, stderr)
	}
	if _, err := f.binding().Get("gp-n"); err == nil {
		t.Error("the refused import copied gp-n, recording its edge onto gp-a as onto a bead no store holds")
	}
	if manifest, _, err := f.loc("packs").readManifest(); err != nil || manifest == nil || manifest.IDs["gp-n"] {
		t.Errorf("the refused import rewrote the manifest: %v %v", manifest, err)
	}
	for _, id := range []string{"gp-a", "gp-n"} {
		if _, err := f.rig("packs").Get(id); err != nil {
			t.Errorf("the refused import removed %s from the rig: %v", id, err)
		}
	}
}

// Iteration-1 minor: "nothing to import" never drops the in-flight record
// over an interrupted incremental attempt's copies whose rows have since left
// the rig: the binding may hold their only copy, without their edges, and the
// record is what keeps boot from serving them.
func TestReviewNothingToImportKeepsAnInterruptedIncrementalHalfCopyHeld(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("first import = %d: %s %s", code, o, e)
	}
	f.seed("packs", []beads.Bead{graphRow("gp-x", "gp-x", "open", nil), graphRow("gp-y", "gp-x", "open", nil)},
		[]beads.Dep{{IssueID: "gp-y", DependsOnID: "gp-x", Type: "blocks"}})
	crashOnceAt(t, "rows")
	_, _, _ = f.importRigs()
	rig := f.rig("packs")
	for _, id := range []string{"gp-y", "gp-x"} {
		if err := rig.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-x, gp-y") || !strings.Contains(stderr, "only copy") {
		t.Fatalf("the re-run over an unrecorded half copy = %d: %s %s", code, stdout, stderr)
	}
	if !fileExists(f.loc("packs").InFlightPath()) {
		t.Error("the re-run removed the in-flight record over binding rows without their edges")
	}
	if err := rigBootRefusal(f.request.CityPath, f.target); err == nil {
		t.Error("boot serves the binding's unrecorded half copy")
	}
}

// Iteration-1 major: a clear stopped between two delete batches resumes on a
// Dolt rig. bd's delete rewrites each surviving neighbor's text that cites a
// deleted id to "[deleted:<id>]", finding the neighbors by their edges, so a
// chain longer than a batch would leave the next batch's first row unequal to
// its binding copy, and every resume's re-proof would refuse it. The edges a
// batch boundary cuts are removed before the first delete.
func TestReviewDoltClearStoppedBetweenDeleteBatchesResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("real Dolt rig")
	}
	prevChunk := rigImportDeleteChunk
	rigImportDeleteChunk = 4
	t.Cleanup(func() { rigImportDeleteChunk = prevChunk })
	rig := newRealDoltRigStore(t)
	const n = 10
	id := func(i int) string { return fmt.Sprintf("gd-s%02d", i) }
	want := map[string]string{}
	for i := 0; i < n; i++ {
		b := graphRow(id(i), id(0), "", nil)
		if i > 0 {
			b.Description = "runs after " + id(i-1)
		}
		want[b.ID] = b.Description
		seedRealDoltRow(t, rig, b, false)
	}
	for i := 1; i < n; i++ {
		if err := rig.DepAdd(id(i), id(i-1), "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	f := newRigImportFixture(t, config.Rig{Name: "dolt", Prefix: "gd"})
	f.fixed["dolt"] = nonClosingStore{rig}
	crashOnceAt(t, "deleting")
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "killed at deleting") {
		t.Fatalf("the crash between delete batches did not stop the import: %d %s", code, stderr)
	}
	left := rigTestList(t, rig)
	if len(left) == 0 || len(left) == n {
		t.Fatalf("precondition: the crash should fall between batches; the rig holds %v", rigTestIDs(left))
	}
	for _, b := range left {
		if b.Description != want[b.ID] {
			t.Errorf("deleting the first batch rewrote surviving %s's text to %q", b.ID, b.Description)
		}
	}
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("the resume after a crash between delete batches = %d: %s %s", code, stdout, stderr)
	}
	if left := rigTestList(t, rig); len(left) != 0 {
		t.Errorf("the rig holds %v after the resume, want none", rigTestIDs(left))
	}
	binding := f.binding()
	for i := 0; i < n; i++ {
		got, err := binding.Get(id(i))
		if err != nil || got.Description != want[id(i)] {
			t.Errorf("binding %s = %q (%v), want %q", id(i), got.Description, err, want[id(i)])
		}
		if i == 0 {
			continue
		}
		if deps, err := binding.DepList(id(i), "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != id(i-1) {
			t.Errorf("binding %s's edges = %v (%v), want its edge to %s", id(i), deps, err, id(i-1))
		}
	}
	if err := rigBootRefusal(f.request.CityPath, f.target); err != nil {
		t.Errorf("boot refuses a finished import: %v", err)
	}
}

// rigClearChunks keeps each edge-connected group in one delete batch when it
// fits, and names only the edges a batch boundary cuts.
func TestRigClearChunksKeepsGroupsWhole(t *testing.T) {
	prev := rigImportDeleteChunk
	rigImportDeleteChunk = 4
	t.Cleanup(func() { rigImportDeleteChunk = prev })
	rows := func(ids ...string) []beads.Bead {
		out := make([]beads.Bead, 0, len(ids))
		for _, id := range ids {
			out = append(out, beads.Bead{ID: id})
		}
		return out
	}
	edge := func(from, to string) beads.Dep { return beads.Dep{IssueID: from, DependsOnID: to, Type: "blocks"} }
	render := func(chunks [][]string, cut []beads.Dep) string {
		parts := make([]string, 0, len(chunks))
		for _, c := range chunks {
			parts = append(parts, strings.Join(c, " "))
		}
		edges := make([]string, 0, len(cut))
		for _, d := range cut {
			edges = append(edges, d.IssueID+">"+d.DependsOnID)
		}
		return strings.Join(parts, " | ") + " ; cut " + strings.Join(edges, " ")
	}
	for _, tc := range []struct {
		name string
		rows []beads.Bead
		down map[string][]beads.Dep
		want string
	}{
		{
			name: "a group that fits the next batch whole is not split",
			rows: rows("a", "b", "c", "x", "y", "z"),
			down: map[string][]beads.Dep{"b": {edge("b", "a")}, "x": {edge("x", "y"), edge("x", "z")}, "y": {edge("y", "z")}},
			want: "a b c | x y z ; cut ",
		},
		{
			name: "a chain longer than a batch is cut once per boundary",
			rows: rows("s1", "s2", "s3", "s4", "s5", "s6"),
			down: map[string][]beads.Dep{"s2": {edge("s2", "s1")}, "s3": {edge("s3", "s2")}, "s4": {edge("s4", "s3")}, "s5": {edge("s5", "s4")}, "s6": {edge("s6", "s5")}},
			want: "s1 s2 s3 s4 | s5 s6 ; cut s5>s4",
		},
		{
			name: "edges to rows that stay are not internal",
			rows: rows("a", "b"),
			down: map[string][]beads.Dep{"a": {edge("a", "w")}, "b": {edge("b", "external:x")}},
			want: "a b ; cut ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(rigClearChunks(tc.rows, tc.down)); got != tc.want {
				t.Errorf("rigClearChunks = %q, want %q", got, tc.want)
			}
		})
	}
}
