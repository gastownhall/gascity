package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// unservableStorageCityTOML is a city whose [storage.classes] describe a partial
// split. storageSplitShapeOf classifies it storageSplitUnsupported, so
// storageBootGate refuses before it constructs a registry, resolves a plan or
// reads a byte of a binding root — which makes newCityRuntime fail
// deterministically, with no filesystem state and no store handles to clean up.
//
// That is the real production failure shape: the city_runtime_failed branch is
// reachable ONLY through storageBootGate, and a stranded-bead boot verdict is
// what took maintainer-city down this branch.
const unservableStorageCityTOML = `[workspace]
name = "wedged-city"

[session]
provider = "fake"

[daemon]
shutdown_timeout = "100ms"

[storage.classes]
work = "work"
graph = "infra"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`

// newWedgedInitCity registers a city that fails inside newCityRuntime and
// returns its path.
func newWedgedInitCity(t *testing.T) (*supervisor.Registry, string) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := shortSocketTempDir(t, "gc-wedged-city-")
	cleanupManagedDoltTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(unservableStorageCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeSpyScript(t, filepath.Join(t.TempDir(), "ops.log"))
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "wedged-city"); err != nil {
		t.Fatal(err)
	}
	return reg, cityPath
}

// initStatusEntry reports whether the registry still lists path as initializing.
func initStatusEntry(cr *cityRegistry, path string) (cityInitProgress, bool) {
	var (
		progress cityInitProgress
		ok       bool
	)
	cr.ReadCallback(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		progress, ok = initStatus[path]
	})
	return progress, ok
}

func initFailureCount(cr *cityRegistry, path string) int {
	var count int
	cr.ReadCallback(func(
		_ map[string]*managedCity,
		_ map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if rec := initFailures[path]; rec != nil {
			count = rec.count
		}
	})
	return count
}

// TestReconcileCitiesClearsInitStatusOnRuntimeFailure pins the invariant the
// retry path depends on: recording an init failure ENDS the attempt, so the city
// must no longer be listed as initializing.
//
// reconcileCities selects cities to start by skipping any with a live initStatus
// entry. A failure branch that records the backoff but leaves the entry behind
// wedges the city out of the loop permanently: it logs "next retry in 10s" and
// then never retries, because it is never selected again.
func TestReconcileCitiesClearsInitStatusOnRuntimeFailure(t *testing.T) {
	reg, cityPath := newWedgedInitCity(t)
	cr := newCityRegistry()

	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	key := canonicalTestPath(cityPath)
	if count := initFailureCount(cr, key); count != 1 {
		t.Fatalf("init failure count = %d, want 1; the city must actually have failed inside newCityRuntime for this test to mean anything (stderr: %s)", count, stderr.String())
	}
	if progress, ok := initStatusEntry(cr, key); ok {
		t.Fatalf("initStatus[%s] = %+v after an init failure, want absent; the city is wedged out of the reconcile loop and its backoff/retry can never run", key, progress)
	}
}

// elapseInitBackoff rewinds a city's recorded init backoff into the past, which
// is what the supervisor's own 10s timer does in production.
func elapseInitBackoff(cr *cityRegistry, path string) {
	cr.BatchUpdate(func(
		_ map[string]*managedCity,
		_ map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if rec := initFailures[path]; rec != nil {
			rec.backoff = time.Now().Add(-time.Second)
		}
	})
}

// TestReconcileCitiesRetriesAfterInitBackoffElapses pins the documented
// behavior: "init failure #1, next retry in 10s" must be followed by an actual
// retry once those 10s pass. With the initStatus entry leaked the city is never
// re-selected, so the promised retry never happens and the count stays at 1 for
// the life of the supervisor process.
func TestReconcileCitiesRetriesAfterInitBackoffElapses(t *testing.T) {
	reg, cityPath := newWedgedInitCity(t)
	cr := newCityRegistry()
	key := canonicalTestPath(cityPath)

	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	if count := initFailureCount(cr, key); count != 1 {
		t.Fatalf("first pass: init failure count = %d, want 1 (stderr: %s)", count, stderr.String())
	}
	if !strings.Contains(stderr.String(), "init failure #1, next retry in") {
		t.Fatalf("first pass stderr = %q, want the promised-retry line", stderr.String())
	}

	elapseInitBackoff(cr, key)
	stderr.Reset()
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	if count := initFailureCount(cr, key); count != 2 {
		t.Fatalf("after the backoff elapsed: init failure count = %d, want 2; the promised retry never ran because the leaked initStatus entry skips the city before the backoff is ever consulted (stderr: %s)", count, stderr.String())
	}
	if !strings.Contains(stderr.String(), "init failure #2") {
		t.Fatalf("second reconcile stderr = %q, want an \"init failure #2\" line proving the documented backoff/retry actually ran", stderr.String())
	}
}

// TestReconcileCitiesRetriesAfterConfigEdit is the operator-observed symptom: a
// city wedged by a leaked initStatus entry could not be recovered by editing
// city.toml, because the initializing-skip is consulted BEFORE the config-mtime
// backoff reset — so the city never re-entered the start loop at all, and only a
// supervisor restart cleared it.
//
// A config edit deliberately RESETS the failure record (the user may have fixed
// the config), so the retry's own failure is recorded fresh as #1. The proof of
// a retry is therefore that the pass attempted the city at all.
func TestReconcileCitiesRetriesAfterConfigEdit(t *testing.T) {
	reg, cityPath := newWedgedInitCity(t)
	cr := newCityRegistry()
	key := canonicalTestPath(cityPath)

	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	if count := initFailureCount(cr, key); count != 1 {
		t.Fatalf("first pass: init failure count = %d, want 1 (stderr: %s)", count, stderr.String())
	}

	// The operator's fix: edit city.toml. A newer mtime resets the backoff, so
	// the very next reconcile must attempt the city again.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(cityPath, "city.toml"), future, future); err != nil {
		t.Fatal(err)
	}

	stderr.Reset()
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	if !strings.Contains(stderr.String(), "init failure #") {
		t.Fatalf("after editing city.toml the reconcile pass never attempted the city (stderr: %q); editing city.toml resets the backoff, but the leaked initStatus entry skips the city before the backoff is ever consulted, so only a supervisor restart recovers it", stderr.String())
	}
}

// newUnloadableConfigCity registers a city whose directory exists but whose
// city.toml never parses, the shape that #5313 reports: "directory is
// missing" is too narrow a predicate, since a directory that keeps an
// invalid (or absent) city.toml sails past that check indefinitely.
func newUnloadableConfigCity(t *testing.T) (*supervisor.Registry, string) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := filepath.Join(t.TempDir(), "unloadable-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "unloadable-city"); err != nil {
		t.Fatal(err)
	}
	return reg, cityPath
}

func cityStillRegistered(t *testing.T, reg *supervisor.Registry, path string) bool {
	t.Helper()
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Path == path {
			return true
		}
	}
	return false
}

// TestReconcileCitiesAutoUnregistersRepeatedConfigLoadFailure covers
// candidate fix A for #5313: a city whose city.toml fails to load must be
// unregistered after staleCityConfigLoadFailThreshold consecutive failed
// attempts, the same way a city whose directory has vanished already is
// (staleCityDirAbsentThreshold). Without this, the init-failure backoff
// caps at 5 minutes and the retry runs forever.
func TestReconcileCitiesAutoUnregistersRepeatedConfigLoadFailure(t *testing.T) {
	reg, cityPath := newUnloadableConfigCity(t)
	cr := newCityRegistry()
	key := canonicalTestPath(cityPath)

	var stdout, stderr bytes.Buffer
	for i := 0; i < staleCityConfigLoadFailThreshold; i++ {
		reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
		// Each cycle's failure sets a backoff (10s, 20s, ...); without
		// elapsing it, only the first cycle would ever re-attempt the
		// config load and the streak would never accumulate.
		elapseInitBackoff(cr, key)
	}

	if cityStillRegistered(t, reg, key) {
		t.Fatalf("city %q should have been auto-unregistered after %d cycles of a config that never loads, but is still registered", key, staleCityConfigLoadFailThreshold)
	}
	if !strings.Contains(stderr.String(), "auto-unregistering") {
		t.Fatalf("stderr should mention auto-unregistering, got: %s", stderr.String())
	}
}

// TestReconcileCitiesDoesNotUnregisterConfigLoadFailureBeforeThreshold pins
// the threshold boundary: a city short of staleCityConfigLoadFailThreshold
// consecutive config-load failures must still be retried, not dropped.
func TestReconcileCitiesDoesNotUnregisterConfigLoadFailureBeforeThreshold(t *testing.T) {
	reg, cityPath := newUnloadableConfigCity(t)
	cr := newCityRegistry()
	key := canonicalTestPath(cityPath)

	var stdout, stderr bytes.Buffer
	for i := 0; i < staleCityConfigLoadFailThreshold-1; i++ {
		reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
		elapseInitBackoff(cr, key)
	}

	if !cityStillRegistered(t, reg, key) {
		t.Fatalf("city %q should still be registered after %d cycles (threshold is %d)", key, staleCityConfigLoadFailThreshold-1, staleCityConfigLoadFailThreshold)
	}
}

// TestReconcileCitiesResetsConfigFailCounterWhenConfigLoadSucceeds proves
// the auto-unregister above only fires on a genuine consecutive streak: once
// city.toml parses again, the config-load-failure count must reset even if
// the city goes on to fail init for an unrelated reason (here, the same
// storage-shape failure exercised by newWedgedInitCity). Otherwise a city
// that fails to load once, gets fixed, and later fails init some other way
// could accumulate toward the threshold across unrelated failure kinds.
func TestReconcileCitiesResetsConfigFailCounterWhenConfigLoadSucceeds(t *testing.T) {
	reg, cityPath := newUnloadableConfigCity(t)
	cr := newCityRegistry()
	key := canonicalTestPath(cityPath)

	var stdout, stderr bytes.Buffer
	for i := 0; i < staleCityConfigLoadFailThreshold-1; i++ {
		reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
		elapseInitBackoff(cr, key)
	}

	// Fix the config: it now loads, but still fails later in newCityRuntime
	// (unservableStorageCityTOML), a different failure kind entirely.
	cleanupManagedDoltTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(unservableStorageCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeSpyScript(t, filepath.Join(t.TempDir(), "ops.log"))
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	stderr.Reset()
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	var configFail int
	cr.ReadCallback(func(
		_ map[string]*managedCity,
		_ map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if rec := initFailures[key]; rec != nil {
			configFail = rec.configFail
		}
	})
	if configFail != 0 {
		t.Fatalf("configFail = %d after city.toml loaded successfully, want 0 (stderr: %s)", configFail, stderr.String())
	}
	if !cityStillRegistered(t, reg, key) {
		t.Fatal("city should still be registered: it never reached the config-load-failure threshold")
	}
}
