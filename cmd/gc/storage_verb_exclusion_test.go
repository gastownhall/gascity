package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func stubOtherCities(t *testing.T, paths ...string) {
	t.Helper()
	prev := storeSharerCities
	t.Cleanup(func() { storeSharerCities = prev })
	storeSharerCities = func(string) ([]string, error) { return paths, nil }
}

// stubStoreIdentity gives every scope the same Dolt database at the same
// endpoint, whatever its project id says.
func stubStoreIdentity(t *testing.T, id storeDoltIdentity) {
	t.Helper()
	prev := storeDoltIdentityOf
	t.Cleanup(func() { storeDoltIdentityOf = prev })
	storeDoltIdentityOf = func(string, string) storeDoltIdentity { return id }
}

const sharedToken = "bd_shared@loopback:3307"

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

// sharedStoreCities makes this city and another registered city use one Dolt
// database at one endpoint. Their beads project ids DIFFER: the project id
// does not decide, the resolved database and endpoint do.
func sharedStoreCities(t *testing.T, request storageOperatorRequest) string {
	t.Helper()
	writeScopeProjectID(t, request.CityPath, "prj-mine")
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeProjectID(t, other, "prj-theirs")
	stubOtherCities(t, other)
	stubStoreIdentity(t, storeDoltIdentity{Database: "bd_shared", Endpoint: "loopback:3307"})
	return other
}

func TestStorageMigrateRefusesASharedStore(t *testing.T) {
	request, _ := exclusionCity(t)
	sharedStoreCities(t, request)
	code, stderr := migrateFor(t, request)
	other := storeSharerCitiesForTest(t)
	if code == 0 || !strings.Contains(stderr, other) || !strings.Contains(stderr, "--force=shared-store:"+sharedToken) {
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
	request.Force = storageForce{SharedStores: []string{sharedToken}}
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "holds its controller lock") {
		t.Fatalf("attested migration with a live co-tenant = %d, stderr %q; want a refusal", code, stderr)
	}
}

// TestStorageMigrateSharedStoreAttestationRefusesAnUnregisteredCoTenant: the
// shared-store check reads only the supervisor registry, and `gc stop`
// unregisters the city it stops. So a live co-tenant's refusal names a stop
// that keeps the registration, and an attestation that finds no registered
// co-tenant refuses. Re-run after the co-tenant was unregistered, the
// migration used to find no co-tenant, skip the foreign-row refusal, and carry
// and clear the co-tenant's rows as this city's.
func TestStorageMigrateSharedStoreAttestationRefusesAnUnregisteredCoTenant(t *testing.T) {
	request, source := exclusionCity(t)
	other := sharedStoreCities(t, request)
	request.Force = storageForce{SharedStores: []string{sharedToken}}
	theirs := mustCreateInfraBead(t, source, beads.Bead{Title: "their mail", Type: "message", Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "city:other-city"}})

	held, err := acquireControllerLock(other)
	if err != nil {
		t.Fatal(err)
	}
	code, stderr := migrateFor(t, request)
	held.Close() //nolint:errcheck // the co-tenant stops
	if code == 0 || !strings.Contains(stderr, other) || !strings.Contains(stderr, "`"+storageCoTenantStopCommand+"`") {
		t.Fatalf("attested migration with a live co-tenant = %d, stderr %q; want a refusal naming %s and `%s`", code, stderr, other, storageCoTenantStopCommand)
	}
	if strings.Contains(stderr, storageStopCommand+" "+other) {
		t.Errorf("the live co-tenant's remedy prescribes `%s %s`, which unregisters it: %q", storageStopCommand, other, stderr)
	}

	// What `gc stop` does to the co-tenant: the registry lists it no more.
	stubOtherCities(t)
	sourceBefore := infraStoreFingerprint(t, source)
	code, stderr = migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, "no other registered city") || !strings.Contains(stderr, "gc register") {
		t.Fatalf("attested migration with the co-tenant unregistered = %d, stderr %q; want a refusal naming its re-registration", code, stderr)
	}
	if after := infraStoreFingerprint(t, source); !slices.Equal(sourceBefore, after) {
		t.Errorf("the refused migration changed the work store that holds co-tenant row %s: before %v, after %v", theirs.ID, sourceBefore, after)
	}
	assertNoConvergenceMarker(t, mustResolveInfraTarget(t, request.CityPath, request.Cfg), "a refused migration records nothing")
	if _, err := os.Stat(filepath.Join(request.CityPath, ".gc", storageAttestationLog)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an attestation that lifted nothing was recorded (stat: %v)", err)
	}

	// The remedy: registered again, the co-tenant is seen, and the foreign-row
	// refusal names its row.
	stubOtherCities(t, other)
	if code, stderr := migrateFor(t, request); code == 0 || !strings.Contains(stderr, theirs.ID) {
		t.Fatalf("attested migration with the co-tenant registered again = %d, stderr %q; want the refusal naming %s", code, stderr, theirs.ID)
	}
}

// TestStorageAttestationRecordsOnlyWhatItLifted: the attestation log records
// the shared-store attestation that lifted this run's refusal. One naming
// another store lifted nothing, so it is not recorded.
func TestStorageAttestationRecordsOnlyWhatItLifted(t *testing.T) {
	request, _ := exclusionCity(t)
	sharedStoreCities(t, request)
	const elsewhere = "bd_elsewhere@loopback:3399"
	request.Force = storageForce{SharedStores: []string{sharedToken, elsewhere}}
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("attested migration = %d: %s", code, stderr)
	}
	log, err := os.ReadFile(filepath.Join(request.CityPath, ".gc", storageAttestationLog))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "#gc:attested "+storageHazardSharedStore+":"+sharedToken+" ") {
		t.Errorf("the attestation that lifted the refusal was not recorded: %q", log)
	}
	if strings.Contains(string(log), elsewhere) {
		t.Errorf("an attestation naming another store, which lifted nothing, was recorded: %q", log)
	}
}

func TestStorageMigrateSharedStoreAttestationRefusesForeignRowsAndCarriesUnattributed(t *testing.T) {
	request, source := exclusionCity(t)
	sharedStoreCities(t, request)
	request.Force = storageForce{SharedStores: []string{sharedToken}}
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

// TestStorageMigrateSharedStoreAttestationKeepsEveryCoTenantStopped: under the
// attestation a co-tenant is kept stopped, not just found stopped. Its
// supervisor restarts it within its backoff and any `gc start` can start it,
// so every time the migration opens the shared source, a start of the
// co-tenant (create its .gc, take its controller lock) must still be
// excluded, including for a co-tenant that never started and has no .gc yet.
// The lock is released when the command returns.
func TestStorageMigrateSharedStoreAttestationKeepsEveryCoTenantStopped(t *testing.T) {
	for _, neverStarted := range []bool{false, true} {
		t.Run(fmt.Sprintf("never_started=%v", neverStarted), func(t *testing.T) {
			request, source := exclusionCity(t)
			other := sharedStoreCities(t, request)
			if neverStarted {
				if err := os.RemoveAll(filepath.Join(other, ".gc")); err != nil {
					t.Fatal(err)
				}
			}
			request.Force = storageForce{SharedStores: []string{sharedToken}}
			opens, admitted := 0, 0
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) {
				opens++
				if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
					t.Fatal(err)
				}
				start, err := acquireControllerLock(other)
				if err == nil {
					admitted++
					start.Close() //nolint:errcheck // the co-tenant's start got the lock
				} else if !errors.Is(err, errControllerAlreadyRunning) {
					t.Errorf("starting the co-tenant mid-run: %v", err)
				}
				return source, nil
			}
			t.Cleanup(func() { openInfraMigrationSource = prev })

			if code, stderr := migrateFor(t, request); code != 0 {
				t.Fatalf("attested migration = %d: %s", code, stderr)
			}
			if opens == 0 {
				t.Fatal("the migration never opened its source; nothing was proven")
			}
			if admitted > 0 {
				t.Fatalf("co-tenant %s took its controller lock during %d of the migration's %d source opens; the shared store was not kept quiescent", other, admitted, opens)
			}
			after, err := acquireControllerLock(other)
			if err != nil {
				t.Fatalf("the migration left co-tenant %s's controller lock held: %v", other, err)
			}
			after.Close() //nolint:errcheck // test cleanup
		})
	}
}

// TestSharedStoreRefusalReleasesTheCoTenantLocksItTook: a live co-tenant
// refuses the attested run, and every co-tenant lock the check took before
// reaching it is released with the refusal.
func TestSharedStoreRefusalReleasesTheCoTenantLocksItTook(t *testing.T) {
	request, _ := exclusionCity(t)
	stopped := sharedStoreCities(t, request)
	live := t.TempDir()
	if err := os.WriteFile(filepath.Join(live, "city.toml"), []byte("[workspace]\nname = \"live-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(live, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubOtherCities(t, stopped, live)
	held, err := acquireControllerLock(live)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	request.Force = storageForce{SharedStores: []string{sharedToken}}

	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, live) || !strings.Contains(stderr, "holds its controller lock") {
		t.Fatalf("attested migration with a live co-tenant = %d, stderr %q; want a refusal naming %s", code, stderr, live)
	}
	after, err := acquireControllerLock(stopped)
	if err != nil {
		t.Fatalf("the refused migration left co-tenant %s's controller lock held: %v", stopped, err)
	}
	after.Close() //nolint:errcheck // test cleanup
}

// TestForeignCityRows pins whose rows a shared-store attestation carries:
// rows naming no scope, this city or one of its rigs. A city: ref naming
// another city and a rig: ref naming none of this city's rigs refuse.
func TestForeignCityRows(t *testing.T) {
	rows := map[string]string{
		"unattributed": "",
		"this-city":    "city:mine",
		"other-city":   "city:theirs",
		"own-rig":      "rig:packs",
		"their-rig":    "rig:their-rig",
		"spaced":       " rig:packs ",
	}
	got := foreignCityRows(rows, "mine", []config.Rig{{Name: "packs"}})
	want := []string{"other-city", "their-rig"}
	if !slices.Equal(got, want) {
		t.Fatalf("foreignCityRows = %v, want %v", got, want)
	}
	if got := foreignCityRows(map[string]string{"rig-row": "rig:packs"}, "mine", nil); !slices.Equal(got, []string{"rig-row"}) {
		t.Fatalf("a rig row of a city with no rigs = %v, want it foreign", got)
	}
}

// TestStorageMigrateSharedStoreAttestationRefusesACoTenantsRigRows: a
// co-tenant's rig can live on this city's work store (findStoreSharers
// compares this city's store with every other city's rig stores), and its
// rows name that rig, not that city. Such a row refuses like a city: row;
// a row naming one of this city's own rigs is still carried.
func TestStorageMigrateSharedStoreAttestationRefusesACoTenantsRigRows(t *testing.T) {
	request, source := exclusionCity(t)
	sharedStoreCities(t, request)
	request.Force = storageForce{SharedStores: []string{sharedToken}}
	request.Cfg.Rigs = []config.Rig{{Name: "packs", Prefix: "gp"}}
	theirs := mustCreateInfraBead(t, source, beads.Bead{Title: "their rig's mail", Type: "message", Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:their-rig"}})

	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, theirs.ID) {
		t.Fatalf("attested migration with a co-tenant's rig row = %d, stderr %q; want a refusal naming %s", code, stderr, theirs.ID)
	}
	assertNoConvergenceMarker(t, mustResolveInfraTarget(t, request.CityPath, request.Cfg), "a refused migration records nothing")
	if err := source.Delete(theirs.ID); err != nil {
		t.Fatal(err)
	}
	ours := mustCreateInfraBead(t, source, beads.Bead{Title: "our rig's mail", Type: "message", Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"}})
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("attested migration of this city's own rig row = %d: %s", code, stderr)
	}
	if _, err := openMigratedDestination(t, mustResolveInfraTarget(t, request.CityPath, request.Cfg)).Get(ours.ID); err != nil {
		t.Fatalf("this city's own rig row %s was not carried into the binding: %v", ours.ID, err)
	}
}

// TestStorageRecoverStrandedSharedStoreAttestationRefusesForeignRows: the
// repair copies whatever the work store holds outside the binding and its
// manifest, and nothing in that asks whose a row is. Under the shared-store
// attestation a stranded row another city's scope names refuses, as the
// migration's does, and the run writes nothing: not the binding, not the
// manifest, not the source.
func TestStorageRecoverStrandedSharedStoreAttestationRefusesForeignRows(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	stubInfraControllerPing(t, 0)
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	other := sharedStoreCities(t, request)
	request.Force = storageForce{SharedStores: []string{sharedToken}}
	recoverFor := func() (int, string) {
		var stdout, stderr bytes.Buffer
		code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}

	foreign := mustCreateInfraBead(t, source, beads.Bead{Title: "their order vote", Type: "task", Labels: []string{"order-tracking"}, Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "city:other-city"}})
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, foreign.ID) {
		t.Fatalf("the fixture did not strand %s (stranded: %v); the scenario under test was never created", foreign.ID, got)
	}
	sourceBefore := infraStoreFingerprint(t, source)
	manifestBefore := manifestIDs(t, target)
	bindingBefore := bindingFingerprint(t, target)

	code, out := recoverFor()
	if code == 0 || !strings.Contains(out, foreign.ID) || !strings.Contains(out, "another city's scope") {
		t.Fatalf("attested recovery of another city's row = %d, output %q; want a refusal naming %s", code, out, foreign.ID)
	}
	if after := infraStoreFingerprint(t, source); !slices.Equal(sourceBefore, after) {
		t.Errorf("the refused recovery changed the work store: before %v, after %v", sourceBefore, after)
	}
	if after := manifestIDs(t, target); !slices.Equal(manifestBefore, after) {
		t.Errorf("the refused recovery changed the manifest: before %v, after %v", manifestBefore, after)
	}
	if after := bindingFingerprint(t, target); !slices.Equal(bindingBefore, after) {
		t.Errorf("the refused recovery changed the binding: before %v, after %v", bindingBefore, after)
	}
	after, err := acquireControllerLock(other)
	if err != nil {
		t.Fatalf("the refused recovery left co-tenant %s's controller lock held: %v", other, err)
	}
	after.Close() //nolint:errcheck // test cleanup

	// The control: with the foreign row gone, an unattributed stranded row is
	// the attested part, and the same recovery carries it.
	if err := source.Delete(foreign.ID); err != nil {
		t.Fatal(err)
	}
	ours := mustCreateInfraBead(t, source, beads.Bead{Title: "our order vote", Type: "task", Labels: []string{"order-tracking"}})
	if code, out := recoverFor(); code != 0 {
		t.Fatalf("attested recovery of this city's row = %d: %s", code, out)
	}
	if got := manifestIDs(t, target); !slices.Contains(got, ours.ID) {
		t.Errorf("the recovery did not record %s in the manifest: %v", ours.ID, got)
	}
}

// TestStorageRecoverStrandedSharedStoreAttestationRefusesAnUnregisteredCoTenant:
// the repair takes the migration's shared-store check. Under an attestation
// that finds no registered co-tenant (`gc stop` unregistered it) it refuses
// before anything is written, rather than recovering the co-tenant's stranded
// row as this city's.
func TestStorageRecoverStrandedSharedStoreAttestationRefusesAnUnregisteredCoTenant(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	stubInfraControllerPing(t, 0)
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true, Force: storageForce{SharedStores: []string{sharedToken}}}
	sharedStoreCities(t, request)
	stubOtherCities(t)
	theirs := mustCreateInfraBead(t, source, beads.Bead{Title: "their order vote", Type: "task", Labels: []string{"order-tracking"}, Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "city:other-city"}})
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, theirs.ID) {
		t.Fatalf("the fixture did not strand %s (stranded: %v); the scenario under test was never created", theirs.ID, got)
	}
	sourceBefore := infraStoreFingerprint(t, source)
	manifestBefore := manifestIDs(t, target)
	bindingBefore := bindingFingerprint(t, target)

	var stdout, stderr bytes.Buffer
	if code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "no other registered city") {
		t.Fatalf("attested recovery with no registered co-tenant = %d, output %q; want a refusal", code, stdout.String()+stderr.String())
	}
	if after := infraStoreFingerprint(t, source); !slices.Equal(sourceBefore, after) {
		t.Errorf("the refused recovery changed the work store: before %v, after %v", sourceBefore, after)
	}
	if after := manifestIDs(t, target); !slices.Equal(manifestBefore, after) {
		t.Errorf("the refused recovery changed the manifest: before %v, after %v", manifestBefore, after)
	}
	if after := bindingFingerprint(t, target); !slices.Equal(bindingBefore, after) {
		t.Errorf("the refused recovery changed the binding: before %v, after %v", bindingBefore, after)
	}
}

// bindingFingerprint is infraStoreFingerprint of the binding, opened and
// closed around the read so no handle outlives it.
func bindingFingerprint(t *testing.T, target infraBindingTarget) []string {
	t.Helper()
	store, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the binding: %v", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // test read
	return infraStoreFingerprint(t, store)
}

func storeSharerCitiesForTest(t *testing.T) string {
	t.Helper()
	paths, err := storeSharerCities("")
	if err != nil || len(paths) != 1 {
		t.Fatalf("stubbed registry = %v, %v", paths, err)
	}
	return paths[0]
}

// TestRemoteRuntimeIsReadFromTheCityNotTheShell: an operator shell's
// GC_SESSION override does not hide a city's k8s sessions, and a pack runtime
// is not presumed remote.
func TestRemoteRuntimeIsReadFromTheCityNotTheShell(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	if name, remote := cityRunsRemoteSessions(&config.City{Session: config.SessionConfig{Provider: "k8s"}}); !remote || name != "k8s" {
		t.Fatalf("a k8s city under GC_SESSION=fake = (%q, %v), want remote", name, remote)
	}
	cfg := &config.City{Session: config.SessionConfig{Provider: "mybox"}, Runtimes: map[string]config.DiscoveredRuntime{"mybox": {Name: "mybox"}}}
	if _, remote := cityRunsRemoteSessions(cfg); remote {
		t.Fatal("a pack runtime was presumed remote")
	}
	t.Setenv("GC_SESSION", "ssh:box")
	if _, remote := cityRunsRemoteSessions(&config.City{}); !remote {
		t.Fatal("a remote GC_SESSION override was ignored")
	}
}

// TestExecRuntimeCountsAsRemote is owner decision D4: an exec: session
// runtime is remote unless its handshake declares locality=local, and none can
// yet. An exec: city refuses the split without --force=remote-runtime.
func TestExecRuntimeCountsAsRemote(t *testing.T) {
	request, _ := exclusionCity(t)
	t.Setenv("GC_SESSION", "")
	request.Cfg.Session.Provider = "exec:/opt/gc/gc-session-k8s"
	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, `"exec:/opt/gc/gc-session-k8s"`) || !strings.Contains(stderr, "--force=remote-runtime") {
		t.Fatalf("migration on an exec: runtime = %d %q, want the remote-runtime refusal", code, stderr)
	}
	// The GC_SESSION override can only add remoteness.
	t.Setenv("GC_SESSION", "exec:/opt/gc/anything")
	if _, remote := cityRunsRemoteSessions(&config.City{}); !remote {
		t.Error("an exec: GC_SESSION override was not remote")
	}
	t.Setenv("GC_SESSION", "")
	// The one seam PAR-EXEC fills: a handshake declaring locality=local.
	prev := execRuntimeDeclaresLocal
	t.Cleanup(func() { execRuntimeDeclaresLocal = prev })
	execRuntimeDeclaresLocal = func(string) bool { return true }
	if _, remote := cityRunsRemoteSessions(request.Cfg); remote {
		t.Error("an exec: runtime declaring locality=local still counted as remote")
	}
	request.Force = storageForce{RemoteRuntime: true}
	execRuntimeDeclaresLocal = prev
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("attested migration on an exec: runtime = %d %s", code, stderr)
	}
}
