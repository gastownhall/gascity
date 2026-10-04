package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// AC-X's rule for this command is that any gc-side residue is handled by a
// documented gc command or by the lifecycle, never by hand. These are the
// cases where it was not: a scope the command could not finish or resume, a
// scope it would have migrated onto the wrong store, and gc's own runtime
// publication left behind for an operator to delete.

// A rig's dolt_data_dir is written before bd runs, so a `bd migrate` that
// fails leaves the rig carrying the key and nothing else. Rerunning is the
// documented recovery — the command is per-scope idempotent — but the rerun
// refused, because the classifier looked for the rig's store under the rig's
// own .beads/dolt while the key it had just written points at the city's.
// A scope that cannot be resumed has to be finished by hand, which is the one
// thing this command may not require.
func TestMigrateProxiedResumesARigAfterAPartialFailure(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	seedCityDatabaseDir(t, city, "sp")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))

	failRig := true
	stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[len(args)-1] == "--help" {
			return []byte("--json  emit JSON"), nil
		}
		switch args[0] {
		case "migrate":
			if failRig && normalizePathForCompare(scopeRoot) == normalizePathForCompare(rig) {
				return []byte("boom"), errors.New("exit status 1")
			}
			return migrateProxiedFlipMetadata(t, scopeRoot)
		case "ping":
			return []byte(`{"status":"ok"}`), nil
		}
		return nil, fmt.Errorf("unexpected bd command %v", args)
	})

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code == 0 {
		t.Fatalf("a failing rig migration reported success\nstdout=%s", stdout.String())
	}
	if dataDir, ok, err := contract.ReadMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(rig)); err != nil || !ok || dataDir == "" {
		t.Fatalf("the failed run did not leave the rig's dolt_data_dir behind (%q, ok=%v, err=%v); this test no longer reproduces the resume case", dataDir, ok, err)
	}

	failRig = false
	stdout.Reset()
	stderr.Reset()
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("rerun after a partial failure = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	for _, scope := range report.Scopes {
		if scope.Status == migrateProxiedStatusFailed {
			t.Errorf("%s failed on the rerun: %s", scope.Scope, scope.Error)
		}
	}
	if mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, scopeMetadataJSONPath(rig)); err != nil || !ok || mode != "proxied-server" {
		t.Errorf("rig dolt_mode after rerun = %q (ok=%v, err=%v)", mode, ok, err)
	}
}

// A rig whose own .beads/dolt holds something that is not a Dolt repository,
// and whose database is not in the city's data dir either, was answered with
// `dolt init` into that directory: bd would then migrate onto the brand-new
// empty repository and whatever was there would still be there, unreferenced.
// gc cannot tell which store is the real one, so it must say so.
func TestMigrateProxiedRefusesARigWithAStoreItCannotPlace(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	// The rig's data dir holds a database, but not under a Dolt root — gc's
	// multi-database layout — and the city's data dir does not hold it at all.
	if err := os.MkdirAll(filepath.Join(rig, ".beads", "dolt", "sp", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code == 0 {
		t.Fatalf("migrate-proxied accepted a rig whose store it cannot place\nstdout=%s", stdout.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	rigResult := migrateProxiedScopeResultFor(t, report, "rig:spike")
	if rigResult.Status != migrateProxiedStatusFailed {
		t.Fatalf("rig status = %q, want failed", rigResult.Status)
	}
	if !strings.Contains(rigResult.Error, "own") || !strings.Contains(rigResult.Error, filepath.Join(rig, ".beads", "dolt")) {
		t.Errorf("rig refusal does not name the store it will not touch: %q", rigResult.Error)
	}
	for _, call := range *calls {
		if normalizePathForCompare(call.scope) == normalizePathForCompare(rig) && call.args[0] == "migrate" {
			t.Fatalf("bd migrate ran against the refused rig: %+v", call)
		}
	}
	if _, err := os.Stat(filepath.Join(rig, ".beads", "dolt", ".dolt")); !os.IsNotExist(err) {
		t.Errorf("the refused rig's data dir was dolt-inited anyway: %v", err)
	}
}

// The city's own store is not somewhere else by design: its data dir is where
// the databases live. A data dir that holds databases but not this city's is
// an unexplained layout, and migrating onto a fresh repository beside them
// would orphan every one — silently, because bd has nothing to compare against.
func TestMigrateProxiedRefusesACityWhoseDatabaseIsNotInItsDataDir(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	seedCityDatabaseDir(t, city, "somebody-elses-db")
	calls := stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code == 0 {
		t.Fatalf("migrate-proxied accepted a city whose database is not in its data dir\nstdout=%s", stdout.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	cityResult := migrateProxiedScopeResultFor(t, report, "city")
	if cityResult.Status != migrateProxiedStatusFailed {
		t.Fatalf("city status = %q, want failed", cityResult.Status)
	}
	if !strings.Contains(cityResult.Error, "hq") {
		t.Errorf("city refusal does not name the database it could not find: %q", cityResult.Error)
	}
	if len(*calls) != 0 {
		t.Fatalf("bd ran against a refused city: %+v", *calls)
	}
}

// An empty data dir is a different answer: there is nothing to orphan, so the
// city migrates and comes up empty, which is what it already was.
func TestMigrateProxiedMigratesACityWithAnEmptyDataDir(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
}

// A doltlite scope has no server topology to migrate. gc recognizes one by
// either metadata field (cmd/gc/cmd_bd_store_bridge.go), so a scope that says
// doltlite in `database` while leaving `backend` at the dolt default reached
// the migratable arm and would have had bd's server-to-proxied migration run
// against an embedded store.
func TestMigrateProxiedRefusesADoltliteScope(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"backend":  {"backend": "doltlite", "database": "dolt", "dolt_database": "hq"},
		"database": {"backend": "dolt", "database": "doltlite", "dolt_database": "hq"},
	} {
		t.Run(name, func(t *testing.T) {
			city, _ := newLegacyManagedCityFixture(t)
			writeScopeMetadataRaw(t, city, meta)
			calls := stubMigrateProxiedBdFlippingMetadata(t)

			var stdout, stderr bytes.Buffer
			if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code == 0 {
				t.Fatalf("migrate-proxied accepted a doltlite city\nstdout=%s", stdout.String())
			}
			report := decodeMigrateProxiedReport(t, stdout.String())
			cityResult := migrateProxiedScopeResultFor(t, report, "city")
			if !strings.Contains(strings.ToLower(cityResult.Error), "doltlite") {
				t.Errorf("refusal does not name doltlite: %q", cityResult.Error)
			}
			if len(*calls) != 0 {
				t.Fatalf("bd ran against a doltlite city: %+v", *calls)
			}
		})
	}
}

// The stopped fence is re-checked immediately before `bd migrate`, but
// `dolt init` runs before that — against the same data directory, and against
// a server that can have come back between the entry check and this scope's
// turn. It was the one write in the sequence that happened outside the fence,
// so the check is driven per scope here rather than through the whole command:
// what it pins is the ordering inside one scope's work, and a published
// runtime state would be refused at the entry fence long before this.
func TestMigrateProxiedFencesBeforeDoltInit(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	seedCityDatabaseDir(t, city, "hq")
	calls := stubMigrateProxiedBdFlippingMetadata(t)

	inited := false
	previous := runDoltInitDataDir
	runDoltInitDataDir = func(dataDir string) ([]byte, error) {
		inited = true
		initDoltRootMarker(t, dataDir)
		return nil, nil
	}
	t.Cleanup(func() { runDoltInitDataDir = previous })

	// gc's server is back up by the time this scope's turn comes: a reconcile
	// tick, or a stray `gc bd`, between the entry fence and here.
	writeManagedDoltStateFile(t, city)

	scope := migrateProxiedScope{Label: "city", Name: "city", Path: normalizePathForCompare(city), Prefix: "ci", IsCity: true}
	err := migrateProxiedScopeNow(city, scope, migrateProxiedClassification{NeedsDoltInit: true})
	if err == nil {
		t.Fatal("migrateProxiedScopeNow ran with gc's server back up")
	}
	if !strings.Contains(err.Error(), "gc stop") {
		t.Errorf("refusal does not tell the operator what to do: %q", err)
	}
	if inited {
		t.Error("dolt init ran against a city with a live gc-managed server")
	}
	if len(*calls) != 0 {
		t.Fatalf("bd ran with gc's server back up: %+v", *calls)
	}
}

// gc's Dolt pack publication is gc's own state, and after the migration it
// describes a server that will never run again for this city. Nothing in the
// ordinary lifecycle retires it — clearManagedDoltRuntimeStateUnlessBound
// returns early for a scope with a complete bd binding, which is precisely
// what the migration just created — so the command that created the condition
// retires it.
func TestMigrateProxiedRetiresGCsOwnRuntimePublication(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	seedCityDatabaseDir(t, city, "sp")
	seedCityDatabaseDir(t, city, "hq")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	stubMigrateProxiedBdFlippingMetadata(t)

	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatal(err)
	}
	residue := []string{layout.StateFile, layout.LogFile, layout.ConfigFile, layout.LockFile, layout.PIDFile, managedDoltStatePath(city)}
	if err := os.MkdirAll(layout.PackStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range residue {
		if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, scope := range []string{city, rig} {
		if err := os.WriteFile(filepath.Join(scope, ".beads", "dolt-server.port"), []byte("35402\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The published runtime state is the entry fence's refusal, so the run
	// starts from the state `gc stop` leaves: everything but that one file.
	if err := os.Remove(managedDoltStatePath(city)); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	for _, path := range residue {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("gc runtime residue survived the migration: %s (%v)", path, err)
		}
	}
	for _, scope := range []string{city, rig} {
		if _, err := os.Stat(filepath.Join(scope, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
			t.Errorf("%s still carries gc's managed port mirror", scope)
		}
	}
}

// --dry-run reports the plan and writes nothing, residue included.
func TestMigrateProxiedDryRunRetiresNothing(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	seedCityDatabaseDir(t, city, "hq")
	stubMigrateProxiedBdFlippingMetadata(t)

	layout, err := resolveManagedDoltRuntimeLayout(city)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.PackStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.LogFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	portMirror := filepath.Join(city, ".beads", "dolt-server.port")
	if err := os.WriteFile(portMirror, []byte("35402\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{DryRun: true, JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run = %d, want 0\nstderr=%s", code, stderr.String())
	}
	for _, path := range []string{layout.LogFile, portMirror} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("dry run removed %s: %v", path, err)
		}
	}
}

func migrateProxiedScopeResultFor(t *testing.T, report migrateProxiedReport, scope string) migrateProxiedScopeResult {
	t.Helper()
	for _, result := range report.Scopes {
		if result.Scope == scope {
			return result
		}
	}
	t.Fatalf("report has no scope %q: %+v", scope, report.Scopes)
	return migrateProxiedScopeResult{}
}

// migrateProxiedFlipMetadata is rc.2's observable effect of a successful
// migrate: dolt_mode flips in metadata.json and nothing else moves.
func migrateProxiedFlipMetadata(t *testing.T, scopeRoot string) ([]byte, error) {
	t.Helper()
	path := scopeMetadataJSONPath(scopeRoot)
	database, _, err := contract.ReadDoltDatabase(fsys.OSFS{}, path)
	if err != nil {
		return nil, err
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, path, contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: database,
	}); err != nil {
		return nil, err
	}
	return []byte(`{"target_mode":"proxied-server"}`), nil
}
