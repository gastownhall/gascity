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

// rigAddSeedFileStore writes rows into the file store at rigPath/.gc/beads.json.
func rigAddSeedFileStore(t *testing.T, rigPath, prefix string, rows ...beads.Bead) {
	t.Helper()
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(rigPath, ".gc", "beads.json"), beads.WithFileStoreIDPrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if _, err := store.Create(b); err != nil {
			t.Fatal(err)
		}
	}
}

// rigAddGraphRow is a pre-S4 rig graph row: a workflow step.
func rigAddGraphRow(prefix string) beads.Bead {
	return beads.Bead{Title: "a pre-S4 step", Type: "task", Metadata: beads.StringMap{"gc.kind": "task", "gc.root_bead_id": prefix + "-root"}}
}

// rigAddWorkRow is plain rig work.
func rigAddWorkRow() beads.Bead { return beads.Bead{Title: "some work", Type: "task"} }

// TestRigAddWritesACleanRecordOnlyWhenItsCensusFindsNoInfrastructureRow: `gc
// rig add` (new, re-add or --adopt) on a converged split city records a rig
// clean exactly when a census of its store finds no infrastructure row. A
// store holding a graph row must not be recorded clean: that would retire its
// relic legs before an import moved the row.
func TestRigAddWritesACleanRecordOnlyWhenItsCensusFindsNoInfrastructureRow(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	for _, tc := range []struct {
		name      string
		seed      []beads.Bead
		wantClean bool
	}{
		{name: "re-add of an existing rig whose store holds a graph row", seed: []beads.Bead{rigAddWorkRow(), rigAddGraphRow("or")}},
		{name: "re-add of an existing rig with nothing to import", seed: []beads.Bead{rigAddWorkRow()}, wantClean: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			rigPath := filepath.Join(t.TempDir(), "old-rig")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			rigAddSeedFileStore(t, rigPath, "or", tc.seed...)
			target := rigAddSplitCity(t, cityPath, "\n[[rigs]]\nname = \"old-rig\"\nprefix = \"or\"\n",
				fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"old-rig\"\npath = %q\n", rigPath))
			var stdout, stderr bytes.Buffer
			if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr); code != 0 {
				t.Fatalf("re-add = %d: %s", code, stderr.String())
			}
			if got := fileExists(newRigImportLocation(target, cityPath, "old-rig").CleanPath()); got != tc.wantClean {
				t.Errorf("re-add recorded clean = %v, want %v: %s", got, tc.wantClean, stderr.String())
			}
		})
	}

	for _, tc := range []struct {
		name      string
		seed      []beads.Bead
		wantClean bool
	}{
		{name: "adopted store holding only work", seed: []beads.Bead{rigAddWorkRow()}, wantClean: true},
		{name: "adopted store holding a graph row", seed: []beads.Bead{rigAddWorkRow(), rigAddGraphRow("ad")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			rigPath := filepath.Join(t.TempDir(), "adopted-rig")
			if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"name":"adopted-rig","issue_prefix":"ad"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: ad\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			rigAddSeedFileStore(t, rigPath, "ad", tc.seed...)
			target := rigAddSplitCity(t, cityPath, "", "")
			// Scoped file roots: the adopted rig is served from its own
			// .gc/beads.json, not the city's.
			if err := ensureScopedFileStoreLayout(cityPath); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ad", "", false, true, &stdout, &stderr); code != 0 {
				t.Fatalf("add --adopt = %d: %s", code, stderr.String())
			}
			if got := fileExists(newRigImportLocation(target, cityPath, "adopted-rig").CleanPath()); got != tc.wantClean {
				t.Errorf("adopt recorded clean = %v, want %v: %s", got, tc.wantClean, stderr.String())
			}
		})
	}

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
