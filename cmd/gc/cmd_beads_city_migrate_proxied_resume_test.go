package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeInterruptedMigrationJournal reproduces what bd leaves behind when it dies
// between `prepared` and `committed`: metadata.json already flipped to
// proxied-server, and the in-flight journal still on disk. Verified live against
// bd v1.3.0-rc.2 with BEADS_MIGRATION_FAIL_PHASE=target_configured.
func writeInterruptedMigrationJournal(t *testing.T, scopeRoot string) {
	t.Helper()
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	database, _, err := contract.ReadDoltDatabase(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, path, contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: database,
	}); err != nil {
		t.Fatal(err)
	}
	journal := `{"version":1,"source_mode":"server","target_mode":"proxied-server","shared":false,` +
		`"root_path":"` + filepath.Join(scopeRoot, ".beads", "dolt") + `","ownership":"managed-local",` +
		`"attempt":1,"phase":"target_configured"}`
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", contract.MigrateDoltModeJournalFile), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateProxiedResumesAnInterruptedMigration pins the repair the
// already-proxied arm used to skip.
//
// bd writes dolt_mode=proxied-server at phase `prepared` and removes its journal
// only at `committed`, so an interrupted flip reads as already-migrated from
// metadata alone. gc reported `already-migrated` and exited 0 for a scope that
// might have no proxied sidecar, un-retired dolt-server controls, and was never
// pinged. bd itself can resume from its own journal — rerunning the verb is the
// repair, and gc was the only thing that never ran it.
func TestMigrateProxiedResumesAnInterruptedMigration(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	seedCityDatabaseDir(t, city, "hq")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	writeInterruptedMigrationJournal(t, city)

	var migrated, pinged int
	stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "migrate" && args[len(args)-1] == "--help" {
			return []byte("--json  emit JSON"), nil
		}
		switch args[0] {
		case "migrate":
			migrated++
			// bd's resume replays the remaining phases and removes the journal.
			if err := os.Remove(filepath.Join(scopeRoot, ".beads", contract.MigrateDoltModeJournalFile)); err != nil {
				return nil, err
			}
			return []byte(`{"target_mode":"proxied-server"}`), nil
		case "ping":
			pinged++
			return []byte(`{"status":"ok"}`), nil
		}
		return nil, fmt.Errorf("unexpected bd command %v", args)
	})

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if len(report.Scopes) != 1 {
		t.Fatalf("report = %+v, want one scope", report)
	}
	if got := report.Scopes[0].Status; got != migrateProxiedStatusMigrated {
		t.Fatalf("scope status = %q, want %q for a resumed migration", got, migrateProxiedStatusMigrated)
	}
	if migrated != 1 {
		t.Fatalf("bd migrate ran %d time(s), want exactly one resume", migrated)
	}
	if pinged != 1 {
		t.Fatalf("bd ping ran %d time(s), want the resumed scope verified", pinged)
	}
	if _, err := os.Stat(filepath.Join(city, ".beads", contract.MigrateDoltModeJournalFile)); !os.IsNotExist(err) {
		t.Fatalf("the in-flight journal survived the resume: %v", err)
	}
}

// TestRequireScopeMigrationCommittedSeesBdsJournal is the guard that could never
// fire: it stat'd "migrate-dolt-mode.json" while bd writes
// "dolt-mode-migration.json".
func TestRequireScopeMigrationCommittedSeesBdsJournal(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	writeInterruptedMigrationJournal(t, city)

	err := requireScopeMigrationCommitted(city)
	if err == nil {
		t.Fatal("requireScopeMigrationCommitted = nil for a scope with a live bd journal")
	}
	if !strings.Contains(err.Error(), contract.MigrateDoltModeJournalFile) {
		t.Fatalf("error = %v, want it to name %s", err, contract.MigrateDoltModeJournalFile)
	}
	if err := os.Remove(filepath.Join(city, ".beads", contract.MigrateDoltModeJournalFile)); err != nil {
		t.Fatal(err)
	}
	if err := requireScopeMigrationCommitted(city); err != nil {
		t.Fatalf("requireScopeMigrationCommitted after commit = %v, want nil", err)
	}
}

// TestMigrateProxiedRetiresOnlyTheNamedCitysResidue pins the target of a
// destructive step. `gc --city A beads city migrate-proxied` run from a
// gc-spawned city-B agent session carries B's GC_PACK_STATE_DIR; the residue
// sweep must follow --city, not the environment.
func TestMigrateProxiedRetiresOnlyTheNamedCitysResidue(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	seedCityDatabaseDir(t, city, "hq")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	stubMigrateProxiedBdFlippingMetadata(t)

	foreign := t.TempDir()
	foreignFiles := []string{"dolt-provider-state.json", "dolt.pid", "dolt.lock", "dolt-config.yaml", "dolt.log"}
	for _, name := range foreignFiles {
		if err := os.WriteFile(filepath.Join(foreign, name), []byte("city B"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GC_PACK_STATE_DIR", foreign)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	for _, name := range foreignFiles {
		if _, err := os.Stat(filepath.Join(foreign, name)); err != nil {
			t.Errorf("migrate-proxied removed another city's runtime file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("migrate-proxied removed another city's pack state dir: %v", err)
	}
}
