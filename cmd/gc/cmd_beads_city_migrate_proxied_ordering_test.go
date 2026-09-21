package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// planMigrateProxiedScopes puts the city first because a rig that shares the
// city's Dolt data dir cannot be proxied while the city still is not, and
// classifyMigrateProxiedScope reads SharedRootRel != "" as proof the city's turn
// already made a repository at that root. The loop did not stop when the city
// failed, so bd was handed the city's still-direct data dir and committed the
// rig's flip against it: a bd-proxied rig over a root gc still legacy-manages,
// where the next `gc start` and the next `bd` in the rig contend for the same
// Dolt store. The reverse direction (rig fails, city succeeded) was the only one
// covered.
func TestMigrateProxiedStopsSharedRootRigsWhenTheCityScopeFails(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	seedCityDatabaseDir(t, city, "sp")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))

	failCity := true
	calls := &[]migrateProxiedBdCall{}
	stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "migrate" && args[len(args)-1] == "--help" {
			return []byte("--json  emit JSON"), nil
		}
		*calls = append(*calls, migrateProxiedBdCall{scope: scopeRoot, args: args})
		switch args[0] {
		case "migrate":
			if failCity && normalizePathForCompare(scopeRoot) == normalizePathForCompare(city) {
				return []byte("migrate.lock held by another workspace"), errors.New("exit status 1")
			}
			return migrateProxiedFlipMetadata(t, scopeRoot)
		case "ping":
			return []byte(`{"status":"ok"}`), nil
		}
		return nil, fmt.Errorf("unexpected bd command %v", args)
	})

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code == 0 {
		t.Fatalf("a failing city migration reported success\nstdout=%s", stdout.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if status := migrateProxiedScopeResultFor(t, report, "city").Status; status != migrateProxiedStatusFailed {
		t.Fatalf("city status = %q, want failed; this test no longer reproduces the ordering case", status)
	}
	for _, call := range *calls {
		if normalizePathForCompare(call.scope) == normalizePathForCompare(rig) {
			t.Fatalf("bd ran against a rig standing on the failed city's root: %+v", call)
		}
	}
	// Untouched is what makes the documented rerun work: the rig is still
	// direct and carries no dolt_data_dir, so the next run classifies it from
	// scratch once the city is proxied.
	if mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, scopeMetadataJSONPath(rig)); err != nil || !ok || mode != "server" {
		t.Errorf("skipped rig dolt_mode = %q (ok=%v, err=%v), want server", mode, ok, err)
	}
	rigResult := migrateProxiedScopeResultFor(t, report, "rig:spike")
	if rigResult.Status != migrateProxiedStatusSkipped {
		t.Errorf("rig status = %q, want %q", rigResult.Status, migrateProxiedStatusSkipped)
	}
	if report.Skipped != 1 {
		t.Errorf("report skipped = %d, want 1: %+v", report.Skipped, report)
	}
	if !strings.Contains(stderr.String(), "skipped") {
		t.Errorf("stderr does not report what was not migrated: %q", stderr.String())
	}
	if dataDir, ok, err := contract.ReadMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(rig)); err != nil || (ok && strings.TrimSpace(dataDir) != "") {
		t.Errorf("skipped rig carries dolt_data_dir=%q (ok=%v, err=%v)", dataDir, ok, err)
	}

	failCity = false
	stdout.Reset()
	stderr.Reset()
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("rerun after the city failure = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	rerun := decodeMigrateProxiedReport(t, stdout.String())
	for _, scope := range rerun.Scopes {
		if scope.Status != migrateProxiedStatusMigrated && scope.Status != migrateProxiedStatusAlready {
			t.Errorf("%s = %q on the rerun: %s%s", scope.Scope, scope.Status, scope.Detail, scope.Error)
		}
	}
	if mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, scopeMetadataJSONPath(rig)); err != nil || !ok || mode != "proxied-server" {
		t.Errorf("rig dolt_mode after rerun = %q (ok=%v, err=%v)", mode, ok, err)
	}
}
