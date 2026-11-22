package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestMigrationWindowAdoptsAManagedDoltLeftRunning (ga-683m7f): a managed
// Dolt running on the city the verb stopped is a server something started
// and left. The window judges its sessions, records it, and stops it on
// close, so the verb leaves the provider stopped as gc stop left it.
func TestMigrationWindowAdoptsAManagedDoltLeftRunning(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{adoptable: true}
	stubMigrationWindowProvider(t, provider)
	var stderr bytes.Buffer
	window, err := openTestWindow(t, lock, cityPath, &stderr)
	if err != nil {
		t.Fatalf("openMigrationSource: %v", err)
	}
	if provider.starts != 0 || provider.lists != 1 {
		t.Errorf("starts = %d, lists = %d; want no start and one session list", provider.starts, provider.lists)
	}
	if !fileExists(migrationWindowRecordPath(cityPath)) {
		t.Error("the adopted server is not recorded, so a killed run leaves it running with nothing to stop it")
	}
	if !strings.Contains(stderr.String(), "was running although the city is stopped") {
		t.Errorf("the window does not say it adopted the server: %s", stderr.String())
	}
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if provider.stops != 1 || !provider.lockHeldAtStop {
		t.Errorf("stops = %d (lock held %v); want the adopted server stopped under the lock", provider.stops, provider.lockHeldAtStop)
	}
	if fileExists(migrationWindowRecordPath(cityPath)) {
		t.Error("the record outlived the stop")
	}
}

// TestMigrationWindowLeavesARefusedAdoptionAsItFoundIt: a foreign writer on
// the server it found refuses the window, which neither stops the server
// (the writer is using it) nor records it.
func TestMigrationWindowLeavesARefusedAdoptionAsItFoundIt(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{adoptable: true, sessions: []beads.DoltConnection{{ID: 9, User: "root", Host: "localhost", DB: "hq", Command: "Query"}}}
	stubMigrationWindowProvider(t, provider)
	_, err := openTestWindow(t, lock, cityPath, nil)
	if err == nil || !strings.Contains(err.Error(), "session 9: root@localhost on hq (Query)") || !strings.Contains(err.Error(), "found running on the stopped city") {
		t.Fatalf("openMigrationSource = %v, want the foreign session refused on the found server", err)
	}
	if provider.stops != 0 || fileExists(migrationWindowRecordPath(cityPath)) || activeMigrationWindow(cityPath) != nil {
		t.Errorf("stops = %d, record = %v, active = %v; want the server left as found", provider.stops, fileExists(migrationWindowRecordPath(cityPath)), activeMigrationWindow(cityPath) != nil)
	}
}

// TestMigrationWindowAdoptionProbeFailureRefuses: a probe that cannot tell
// is not a server that is not running.
func TestMigrationWindowAdoptionProbeFailureRefuses(t *testing.T) {
	cityPath, lock := windowCity(t)
	stubMigrationWindowProvider(t, &migrationWindowProvider{})
	migrationWindowAdoptable = func(string) (bool, error) { return false, errors.New("dolt state unreadable") }
	if _, err := openTestWindow(t, lock, cityPath, nil); err == nil || !strings.Contains(err.Error(), "dolt state unreadable") {
		t.Fatalf("openMigrationSource = %v, want the probe's failure", err)
	}
}

// TestMigrationProviderAdoptableNeedsAManagedDolt runs the real probe on
// cities that hold no gc-managed Dolt: none is adopted.
func TestMigrationProviderAdoptableNeedsAManagedDolt(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	if adopt, err := migrationProviderAdoptable(t.TempDir()); err != nil || adopt {
		t.Fatalf("migrationProviderAdoptable(no workspace) = (%v, %v), want (false, nil)", adopt, err)
	}
	city := t.TempDir()
	writeMinimalCityToml(t, city)
	t.Setenv("GC_BEADS", "file")
	if adopt, err := migrationProviderAdoptable(city); err != nil || adopt {
		t.Fatalf("migrationProviderAdoptable(file store) = (%v, %v), want (false, nil)", adopt, err)
	}
}

// TestPreflightReadsNoStoreWhoseServerIsDown: plain preflight takes the read
// gate --split takes. On a stopped managed city any read would start the
// server and leave it running (ga-683m7f), so it reads no store, still runs
// every check that reads none, and says what lets a later run read.
func TestPreflightReadsNoStoreWhoseServerIsDown(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 1)
	stubMigrationProviderDown(t, true)
	opened := false
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		opened = true
		return nil, errors.New("opened")
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, stdout.String())
	}
	out := stdout.String()
	if opened {
		t.Error("preflight opened the work store of a city whose provider is down")
	}
	if !preflightRow(out, preflightPassTag, "topology") || !preflightRow(out, preflightSkipTag, "work store") {
		t.Errorf("the checks that read no store did not run, or the work store was not skipped:\n%s", out)
	}
	if !strings.Contains(out, "this command starts nothing") || !strings.Contains(out, splitUnreadWhileRunning) {
		t.Errorf("the report does not say why nothing was read and what lets a later run read:\n%s", out)
	}

	// A blocker that reads no store is still named.
	if err := writeRemoteStage(request.CityPath, remoteStage{ControllerCity: "controller-city", ControllerPath: "/srv/controller-city"}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := doStoragePreflight(request, &stdout, &stderr); code != 1 || !preflightRow(stdout.String(), preflightBlockTag, "remote stage") {
		t.Fatalf("exit %d: the remote stage, which reads no store, was not named:\n%s", code, stdout.String())
	}
}
