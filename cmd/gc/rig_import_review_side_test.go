package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestCityMigrationRefusesARowARigImportStamped: a re-run of the CITY
// migration must never clear a row a rig import stamped rig:<rig> from a
// binding it has not yet converged on; that row is the binding's only copy.
func TestCityMigrationRefusesARowARigImportStamped(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	target := mustResolveInfraTarget(t, cityPath, cfg)
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{
		ID: "gp-imported", Title: "a rig-imported step", Type: "task",
		Metadata: beads.StringMap{infraMigrationStampKey: "rig:packs", "gc.kind": "task", "gc.root_bead_id": "gp-imported"},
	}); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)

	var log bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.serving() {
		t.Fatalf("the city migration converged over a rig-imported row: %s", log.String())
	}
	if !strings.Contains(log.String(), "gp-imported") {
		t.Errorf("the refusal does not name the rig-imported row: %s", log.String())
	}
	check := openMigratedDestination(t, target)
	if _, err := check.Get("gp-imported"); err != nil {
		t.Errorf("the city migration deleted the rig-imported row: %v", err)
	}
}

// rigAddSplitCity writes a schema-2 city whose [storage] split names a
// converged SQLite binding (database plus infra.migrated), with extra rig
// entries, and returns the binding target.
func rigAddSplitCity(t *testing.T, cityPath, rigsToml, siteToml string) infraBindingTarget {
	t.Helper()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n"+bdSQLRefusalSplitStorage+rigsToml, siteToml)
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	target := mustResolveInfraTarget(t, cityPath, cfg)
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	if err := writeInfraMigratedMarker(target); err != nil {
		t.Fatal(err)
	}
	return target
}

// TestRigAddWritesACleanRecordOnlyForAGenuinelyNewEmptyRig: re-running `gc
// rig add` on an existing rig, or adding a rig whose store already holds a
// graph row, must not record the rig clean (that would retire its relic legs
// before an import censused it). A truly new, empty rig is recorded clean.
func TestRigAddWritesACleanRecordOnlyForAGenuinelyNewEmptyRig(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	t.Run("re-add of an existing rig", func(t *testing.T) {
		cityPath := t.TempDir()
		rigPath := filepath.Join(t.TempDir(), "old-rig")
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		target := rigAddSplitCity(t, cityPath, "\n[[rigs]]\nname = \"old-rig\"\nprefix = \"or\"\n",
			fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"old-rig\"\npath = %q\n", rigPath))
		var stdout, stderr bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr); code != 0 {
			t.Fatalf("re-add = %d: %s", code, stderr.String())
		}
		if fileExists(newRigImportLocation(target, cityPath, "old-rig").CleanPath()) {
			t.Error("a re-add recorded an existing rig clean")
		}
	})

	t.Run("new rig whose store already holds a graph row", func(t *testing.T) {
		cityPath := t.TempDir()
		rigPath := filepath.Join(t.TempDir(), "full-rig")
		target := rigAddSplitCity(t, cityPath, "", "")
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(rigPath, ".gc", "beads.json"), beads.WithFileStoreIDPrefix("fr"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(beads.Bead{Title: "a pre-S4 step", Type: "task", Metadata: beads.StringMap{"gc.kind": "task", "gc.root_bead_id": "fr-root"}}); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr); code != 0 {
			t.Fatalf("add = %d: %s", code, stderr.String())
		}
		if fileExists(newRigImportLocation(target, cityPath, "full-rig").CleanPath()) {
			t.Error("a rig whose store holds a graph row was recorded clean")
		}
	})

	t.Run("genuinely new empty rig", func(t *testing.T) {
		cityPath := t.TempDir()
		rigPath := filepath.Join(t.TempDir(), "new-rig")
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		target := rigAddSplitCity(t, cityPath, "", "")
		var stdout, stderr bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr); code != 0 {
			t.Fatalf("add = %d: %s", code, stderr.String())
		}
		if !fileExists(newRigImportLocation(target, cityPath, "new-rig").CleanPath()) {
			t.Errorf("a new empty rig was not recorded clean: %s", stderr.String())
		}
	})
}
