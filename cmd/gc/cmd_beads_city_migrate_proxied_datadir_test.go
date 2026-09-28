package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// classifyMigrateProxiedScope decides NeedsDoltInit against the data dir the
// scope's metadata records (scopeDoltDataDir honors dolt_data_dir, and its own
// comment says resumability depends on that), and --dry-run printed that same
// directory — but the executor ran `dolt init` in the hardcoded
// <scope>/.beads/dolt. For a city whose metadata names another dir that left a
// stray Dolt root in gc's multi-database directory while bd's migrate validated
// the recorded root and refused, identically on every rerun.
func TestMigrateProxiedInitsTheRecordedDoltDataDir(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	if err := contract.SetMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(city), "store"); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(city, ".beads", "store")
	if err := os.MkdirAll(recorded, 0o755); err != nil {
		t.Fatal(err)
	}
	seedCityDatabaseDir(t, city, "hq")
	// The legacy database has to be under the recorded root, because that is
	// the root the command is now reasoning about.
	if err := os.MkdirAll(filepath.Join(recorded, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	// stubMigrateProxiedBd owns both seams and restores both on cleanup, so the
	// recorder goes in after it.
	stubMigrateProxiedBdFlippingMetadata(t)
	var inited []string
	runDoltInitDataDir = func(dataDir string) ([]byte, error) {
		inited = append(inited, dataDir)
		initDoltRootMarker(t, dataDir)
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("migrate-proxied = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if len(inited) != 1 || inited[0] != recorded {
		t.Fatalf("dolt init ran in %v, want exactly [%s] — the directory NeedsDoltInit was decided against", inited, recorded)
	}
	if _, err := os.Stat(filepath.Join(city, ".beads", "dolt", ".dolt")); !os.IsNotExist(err) {
		t.Errorf("a stray Dolt root was created in the default data dir: %v", err)
	}
}

// The dry-run plan and the executor must name the same directory: a plan that
// says one thing and a run that does another is worse than either alone.
func TestMigrateProxiedDryRunPlansTheDirectoryItWouldInit(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	if err := contract.SetMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(city), "store"); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(city, ".beads", "store")
	if err := os.MkdirAll(filepath.Join(recorded, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true, DryRun: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	detail := migrateProxiedScopeResultFor(t, decodeMigrateProxiedReport(t, stdout.String()), "city").Detail
	if !strings.Contains(detail, "dolt init "+recorded) {
		t.Fatalf("dry-run plan = %q, want it to name %q", detail, "dolt init "+recorded)
	}
}

// The rig half of the same shape. C12 made the city's turn reason about its
// recorded data dir; sharedCityRootForRig kept probing <city>/.beads/dolt, so a
// rig whose database sits in the city's RECORDED root got rel "", was classified
// as owning its own (empty) root, and the command refused it — fail-closed, with
// no flag that points the rig at the right directory.
func TestMigrateProxiedSharesTheRecordedCityDoltDataDirWithRigs(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	if err := contract.SetMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(city), "store"); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(city, ".beads", "store")
	// Both databases live where the city actually serves from.
	for _, database := range []string{"hq", "sp"} {
		if err := os.MkdirAll(filepath.Join(recorded, database, ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rel, err := sharedCityRootForRig(city, rig)
	if err != nil {
		t.Fatalf("sharedCityRootForRig: %v", err)
	}
	want, err := filepath.Rel(filepath.Join(rig, ".beads"), recorded)
	if err != nil {
		t.Fatal(err)
	}
	if rel != filepath.ToSlash(want) {
		t.Fatalf("sharedCityRootForRig = %q, want %q — the rig's database is in the city's recorded data dir", rel, filepath.ToSlash(want))
	}

	stubMigrateProxiedBdFlippingMetadata(t)
	initDoltRootMarker(t, recorded)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("migrate-proxied = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	dataDir, ok, err := contract.ReadMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(rig))
	if err != nil || !ok {
		t.Fatalf("rig dolt_data_dir after migrate: ok=%v err=%v", ok, err)
	}
	if dataDir != filepath.ToSlash(want) {
		t.Fatalf("rig dolt_data_dir = %q, want %q", dataDir, filepath.ToSlash(want))
	}
}
