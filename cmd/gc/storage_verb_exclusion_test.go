package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func stubSessionInventory(t *testing.T, names []string, complete bool, err error) {
	t.Helper()
	prev := storageVerbSessionInventory
	t.Cleanup(func() { storageVerbSessionInventory = prev })
	storageVerbSessionInventory = func(*config.City, string) ([]string, bool, error) { return names, complete, err }
}

func stubOtherCities(t *testing.T, entries ...supervisor.CityEntry) {
	t.Helper()
	prev := storageVerbOtherCities
	t.Cleanup(func() { storageVerbOtherCities = prev })
	storageVerbOtherCities = func(string) ([]supervisor.CityEntry, error) { return entries, nil }
}

// exclusionCity is a split city with one infrastructure bead to migrate, no
// controller answering its ping, no session running and no other city.
func exclusionCity(t *testing.T) (storageOperatorRequest, beads.Store) {
	t.Helper()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "carried", Type: "session"})
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubOtherCities(t)
	return request, source
}

func migrateFor(t *testing.T, request storageOperatorRequest) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageMigrate(context.Background(), request, &stdout, &stderr)
	return code, stderr.String()
}

// TestStorageMigrateRefusesWhileTheControllerLockIsHeld: the lock, not the
// ping, is what proves no controller owns the city. A controller that is
// booting (socket not open yet) or `gc stop` retiring the provider holds the
// lock and answers no ping; the migration used to run under both.
func TestStorageMigrateRefusesWhileTheControllerLockIsHeld(t *testing.T) {
	request, _ := exclusionCity(t)
	held, err := acquireControllerLock(request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup

	code, stderr := migrateFor(t, request)
	if code == 0 {
		t.Fatal("the migration ran while another process held the controller lock")
	}
	if !strings.Contains(stderr, "controller lock") {
		t.Errorf("refusal %q does not name the controller lock", stderr)
	}
	assertNoConvergenceMarker(t, mustResolveInfraTarget(t, request.CityPath, request.Cfg), "a refused migration records nothing")
}

// TestStorageMigrateHoldsTheControllerLockForItsWholeRun: mid-copy, the lock
// is held (no supervisor restart nor second gc start can take the city), and
// it is released when the command returns.
func TestStorageMigrateHoldsTheControllerLockForItsWholeRun(t *testing.T) {
	request, source := exclusionCity(t)
	heldMidRun := false
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
		probe, err := acquireControllerLock(cityPath)
		if err == nil {
			probe.Close() //nolint:errcheck // probe only
		}
		heldMidRun = heldMidRun || errors.Is(err, errControllerAlreadyRunning)
		return source, nil
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })

	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("migration = %d: %s", code, stderr)
	}
	if !heldMidRun {
		t.Fatal("the migration opened its source without holding the controller lock")
	}
	after, err := acquireControllerLock(request.CityPath)
	if err != nil {
		t.Fatalf("the migration left the controller lock held: %v", err)
	}
	after.Close() //nolint:errcheck // test cleanup
}

func TestStorageMigrateRefusesARegisteredCity(t *testing.T) {
	request, _ := exclusionCity(t)
	t.Setenv("GC_HOME", t.TempDir())
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(request.CityPath, "registered-city"); err != nil {
		t.Fatal(err)
	}
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "registered with the supervisor") || !strings.Contains(stderr, "gc stop") {
		t.Fatalf("migration of a registered city = %d, stderr %q; want a refusal naming gc stop", code, stderr)
	}
}

func TestStorageMigrateRefusesWhileSessionsRun(t *testing.T) {
	request, _ := exclusionCity(t)
	stubSessionInventory(t, []string{"lock-city--mayor"}, true, nil)
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "lock-city--mayor") {
		t.Fatalf("migration with a running session = %d, stderr %q; want a refusal naming it", code, stderr)
	}
}

func TestStorageMigrateIncompleteInventoryNeedsTheRemoteRuntimeAttestation(t *testing.T) {
	request, _ := exclusionCity(t)
	stubSessionInventory(t, nil, false, nil)
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "--force=remote-runtime") {
		t.Fatalf("migration over an incomplete inventory = %d, stderr %q; want a refusal naming the attestation", code, stderr)
	}
	request.Force = storageForce{RemoteRuntime: true}
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("attested migration = %d: %s", code, stderr)
	}
	log, err := os.ReadFile(filepath.Join(request.CityPath, ".gc", storageAttestationLog))
	if err != nil || !strings.Contains(string(log), "#gc:attested remote-runtime") {
		t.Errorf("the attestation was not recorded: %q, %v", log, err)
	}
}

func TestStorageMigrateRefusesARemoteRuntimeWithoutTheAttestation(t *testing.T) {
	request, _ := exclusionCity(t)
	t.Setenv("GC_SESSION", "")
	request.Cfg.Session.Provider = "k8s"
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, `"k8s"`) || !strings.Contains(stderr, "--force=remote-runtime") {
		t.Fatalf("migration on k8s = %d, stderr %q; want the remote-runtime refusal", code, stderr)
	}
	request.Force = storageForce{RemoteRuntime: true}
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("attested k8s migration = %d: %s", code, stderr)
	}
}

func TestStorageRecoverStrandedRefusesWhileTheControllerLockIsHeld(t *testing.T) {
	request, _ := exclusionCity(t)
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("migration = %d: %s", code, stderr)
	}
	held, err := acquireControllerLock(request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	var stdout, stderr bytes.Buffer
	if code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr); code == 0 {
		t.Fatal("recover-stranded ran while another process held the controller lock")
	}
	if !strings.Contains(stderr.String(), "controller lock") {
		t.Errorf("refusal %q does not name the controller lock", stderr.String())
	}
}

func TestParseStorageForce(t *testing.T) {
	for _, bad := range [][]string{{storageForceBare}, {""}, {"everything"}, {"shared-store:"}} {
		if _, err := parseStorageForce(bad); err == nil {
			t.Errorf("parseStorageForce(%q) accepted", bad)
		}
	}
	got, err := parseStorageForce([]string{"remote-runtime", "shared-store:prj-a"})
	if err != nil || !got.RemoteRuntime || !got.attestsSharedStore("prj-a") || got.attestsSharedStore("prj-b") {
		t.Fatalf("parseStorageForce = (%+v, %v)", got, err)
	}
}

// TestStorageMigrateBareForceRefuses goes through the command: a bare
// --force names no hazard and refuses before anything is resolved.
func TestStorageMigrateBareForceRefuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStorageCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"migrate", "--from-work", "--fleet-stopped", "--force"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err == nil {
		t.Fatal("a bare --force was accepted")
	}
	if !strings.Contains(stderr.String(), "bare --force attests to nothing") {
		t.Errorf("stderr %q does not explain the bare --force refusal", stderr.String())
	}
}

// sharedStoreCities makes this city and another registered city share one
// beads project, the shape of two cities on one database.
func sharedStoreCities(t *testing.T, request storageOperatorRequest) string {
	t.Helper()
	writeScopeProjectID(t, request.CityPath, "prj-shared")
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeProjectID(t, other, "prj-shared")
	stubOtherCities(t, supervisor.CityEntry{Path: other, Name: "other-city"})
	return other
}

func TestStorageMigrateRefusesASharedStore(t *testing.T) {
	request, _ := exclusionCity(t)
	sharedStoreCities(t, request)
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "other-city") || !strings.Contains(stderr, "--force=shared-store:prj-shared") {
		t.Fatalf("migration over a shared store = %d, stderr %q; want a refusal naming the other city and the attestation", code, stderr)
	}
	request.Force = storageForce{SharedStores: []string{"prj-wrong"}}
	if code, _ := migrateFor(t, request); code == 0 {
		t.Fatal("an attestation of another identity lifted the shared-store refusal")
	}
}

func TestStorageMigrateSharedStoreAttestationStillRefusesALiveCoTenant(t *testing.T) {
	request, _ := exclusionCity(t)
	other := sharedStoreCities(t, request)
	held, err := acquireControllerLock(other)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	request.Force = storageForce{SharedStores: []string{"prj-shared"}}
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "controller is live") {
		t.Fatalf("attested migration with a live co-tenant = %d, stderr %q; want a refusal", code, stderr)
	}
}

func TestStorageMigrateSharedStoreAttestationRefusesForeignRowsAndCarriesUnattributed(t *testing.T) {
	request, source := exclusionCity(t)
	sharedStoreCities(t, request)
	request.Force = storageForce{SharedStores: []string{"prj-shared"}}
	foreign := mustCreateInfraBead(t, source, beads.Bead{Title: "theirs", Type: "message", Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "city:other-city"}})

	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, foreign.ID) {
		t.Fatalf("attested migration with another city's row = %d, stderr %q; want a refusal naming %s", code, stderr, foreign.ID)
	}
	if err := source.Delete(foreign.ID); err != nil {
		t.Fatal(err)
	}
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("attested migration of unattributed rows = %d: %s", code, stderr)
	}
}
