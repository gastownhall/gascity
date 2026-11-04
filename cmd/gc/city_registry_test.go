package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestCityRegistryEmptySnapshot(t *testing.T) {
	reg := newCityRegistry()
	snap := reg.Snapshot()
	if snap == nil {
		t.Fatal("expected non-nil snapshot from new registry")
	}
	if len(snap.all) != 0 {
		t.Fatalf("expected empty snapshot, got %d entries", len(snap.all))
	}
	if snap.gen != 0 {
		t.Fatalf("expected gen=0, got %d", snap.gen)
	}
}

func TestCityRegistryPendingRequestIDCanonicalizesPath(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	reg := newCityRegistry()
	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(t.TempDir(), "city-link")
	if err := os.Symlink(cityPath, linkPath); err != nil {
		t.Fatal(err)
	}

	if err := reg.StorePendingRequestID(linkPath, "req-city"); err != nil {
		t.Fatal(err)
	}

	got, ok, err := reg.ConsumePendingRequestID(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("pending request ID not found by canonical path")
	}
	if got != "req-city" {
		t.Fatalf("request ID = %q, want req-city", got)
	}
}

func TestCityRegistryStorePendingRequestIDRejectsDuplicatePath(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	reg := newCityRegistry()
	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := reg.StorePendingRequestID(cityPath, "req-first"); err != nil {
		t.Fatal(err)
	}
	err := reg.StorePendingRequestID(cityPath, "req-second")
	if !errors.Is(err, api.ErrPendingRequestExists) {
		t.Fatalf("StorePendingRequestID duplicate error = %v, want ErrPendingRequestExists", err)
	}

	got, ok, err := reg.ConsumePendingRequestID(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "req-first" {
		t.Fatalf("consumed pending request = (%q, %t), want req-first true", got, ok)
	}
}

func TestCityRegistryConsumePendingRequestIDIsAtomic(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	reg := newCityRegistry()
	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg.StorePendingRequestID(cityPath, "req-city"); err != nil {
		t.Fatal(err)
	}

	lockPath := supervisor.RegistryPath() + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	type result struct {
		id  string
		ok  bool
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			id, ok, err := reg.ConsumePendingRequestID(cityPath)
			results <- result{id: id, ok: ok, err: err}
		}()
	}

	close(start)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	first := <-results
	second := <-results
	if first.err != nil {
		t.Fatal(first.err)
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	consumed := 0
	for _, got := range []result{first, second} {
		if got.ok {
			consumed++
			if got.id != "req-city" {
				t.Fatalf("request ID = %q, want req-city", got.id)
			}
		}
	}
	if consumed != 1 {
		t.Fatalf("consumed request ID %d times, want exactly once; first=%+v second=%+v", consumed, first, second)
	}
}

func TestCityRegistryAddRemove(t *testing.T) {
	reg := newCityRegistry()

	mc := &managedCity{
		name:   "city-a",
		cr:     &CityRuntime{cityName: "city-a"},
		status: "starting",
	}
	reg.Add("/path/a", mc)

	snap := reg.Snapshot()
	if len(snap.all) != 1 {
		t.Fatalf("expected 1 city after Add, got %d", len(snap.all))
	}
	if snap.gen != 1 {
		t.Fatalf("expected gen=1 after Add, got %d", snap.gen)
	}
	if v, ok := snap.byName["city-a"]; !ok {
		t.Fatal("city-a not found in byName")
	} else if v.Path != "/path/a" {
		t.Fatalf("city-a path = %q, want /path/a", v.Path)
	}
	if _, ok := snap.byPath["/path/a"]; !ok {
		t.Fatal("/path/a not found in byPath")
	}

	reg.Remove("/path/a")
	snap = reg.Snapshot()
	if len(snap.all) != 0 {
		t.Fatalf("expected 0 cities after Remove, got %d", len(snap.all))
	}
	if snap.gen != 2 {
		t.Fatalf("expected gen=2 after Remove, got %d", snap.gen)
	}
}

func TestCityRegistryUpdateCallback(t *testing.T) {
	reg := newCityRegistry()
	mc := &managedCity{
		name: "city-b",
		cr:   &CityRuntime{cityName: "city-b"},
	}
	reg.Add("/path/b", mc)

	// Update started flag via callback.
	reg.UpdateCallback("/path/b", func(m *managedCity) {
		m.started = true
		m.status = "ready"
	})

	snap := reg.Snapshot()
	v := snap.byName["city-b"]
	if v == nil {
		t.Fatal("city-b not found after UpdateCallback")
	}
	if !v.Started {
		t.Error("expected Started=true after UpdateCallback")
	}
	if v.Status != "ready" {
		t.Errorf("expected Status=ready, got %q", v.Status)
	}
}

func TestCityRegistryUpdateCallbackMissingCity(t *testing.T) {
	reg := newCityRegistry()
	// Should not panic when city doesn't exist.
	reg.UpdateCallback("/nonexistent", func(m *managedCity) {
		m.started = true
	})
	snap := reg.Snapshot()
	if len(snap.all) != 0 {
		t.Fatalf("expected 0 cities, got %d", len(snap.all))
	}
}

func TestCityRegistryBatchUpdate(t *testing.T) {
	reg := newCityRegistry()
	reg.Add("/path/c", &managedCity{
		name: "city-c",
		cr:   &CityRuntime{cityName: "city-c"},
	})
	reg.Add("/path/d", &managedCity{
		name: "city-d",
		cr:   &CityRuntime{cityName: "city-d"},
	})

	genBefore := reg.Snapshot().gen

	// Batch update both cities and add init state.
	reg.BatchUpdate(func(
		cities map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		cities["/path/c"].started = true
		cities["/path/d"].status = "initializing"
		initStatus["/path/e"] = cityInitProgress{name: "city-e", status: "loading_config"}
	})

	snap := reg.Snapshot()
	// Should only increment gen once for the batch.
	if snap.gen != genBefore+1 {
		t.Fatalf("expected gen=%d, got %d", genBefore+1, snap.gen)
	}
	if len(snap.all) != 3 { // c, d, and init-only e
		t.Fatalf("expected 3 entries in snapshot, got %d", len(snap.all))
	}
	if v := snap.byName["city-c"]; !v.Started {
		t.Error("city-c should be started")
	}
	if v := snap.byName["city-d"]; v.Status != "initializing" {
		t.Errorf("city-d status = %q, want initializing", v.Status)
	}
	if v := snap.byName["city-e"]; v == nil {
		t.Error("city-e (init-only) should be in snapshot")
	} else if v.Status != "loading_config" {
		t.Errorf("city-e status = %q, want loading_config", v.Status)
	}
}

func TestCityRegistryCityState(t *testing.T) {
	reg := newCityRegistry()
	cs := &controllerState{}
	mc := &managedCity{
		name: "city-f",
		cr:   &CityRuntime{cityName: "city-f", cs: cs},
	}
	reg.Add("/path/f", mc)

	// Not started — should return nil.
	if got := reg.CityState("city-f"); got != nil {
		t.Fatalf("CityState before start = %v, want nil", got)
	}

	// Mark started.
	reg.UpdateCallback("/path/f", func(m *managedCity) {
		m.started = true
	})

	// Now should return the controllerState.
	if got := reg.CityState("city-f"); got != cs {
		t.Fatalf("CityState after start = %v, want controller state", got)
	}

	// Non-existent city.
	if got := reg.CityState("nonexistent"); got != nil {
		t.Fatalf("CityState for nonexistent = %v, want nil", got)
	}
}

func TestCityRegistryTombstoned(t *testing.T) {
	reg := newCityRegistry()
	cs := &controllerState{}
	mc := &managedCity{
		name:    "city-g",
		started: true,
		cr:      &CityRuntime{cityName: "city-g", cs: cs},
	}
	reg.Add("/path/g", mc)

	// Should be accessible.
	if got := reg.CityState("city-g"); got != cs {
		t.Fatal("CityState should return state before tombstone")
	}

	// Tombstone and remove.
	mc.tombstoned.Store(true)
	reg.Remove("/path/g")

	// Should not be found at all.
	if got := reg.CityState("city-g"); got != nil {
		t.Fatalf("CityState after remove = %v, want nil", got)
	}
}

func TestCityRegistryTombstonedBeforeRemove(t *testing.T) {
	reg := newCityRegistry()
	cs := &controllerState{}
	mc := &managedCity{
		name:    "city-h",
		started: true,
		cr:      &CityRuntime{cityName: "city-h", cs: cs},
	}
	reg.Add("/path/h", mc)

	// Tombstone but don't remove yet — rebuild snapshot to pick up tombstone.
	mc.tombstoned.Store(true)
	// Force snapshot rebuild via a no-op batch update.
	reg.BatchUpdate(func(
		_ map[string]*managedCity,
		_ map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
	})

	// CityState should return nil because tombstoned.
	if got := reg.CityState("city-h"); got != nil {
		t.Fatalf("CityState after tombstone = %v, want nil", got)
	}
}

func TestCityRegistryTransientCityEventProvidersIncludesRegisteredAndPendingCities(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	registeredPath := writeCityEventLog(t, "registered-only")
	pendingPath := writeCityEventLog(t, "pending-city")
	runningPath := writeCityEventLog(t, "running-city")

	regFile := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := regFile.Register(registeredPath, "registered-only"); err != nil {
		t.Fatal(err)
	}
	if err := regFile.Register(runningPath, "running-city"); err != nil {
		t.Fatal(err)
	}

	reg := newCityRegistry()
	reg.Add(pendingPath, &managedCity{
		name:   "pending-city",
		status: "loading_config",
		cr:     &CityRuntime{cityName: "pending-city"},
	})
	reg.Add(runningPath, &managedCity{
		name:    "running-city",
		started: true,
		cr:      &CityRuntime{cityName: "running-city"},
	})

	providers := reg.TransientCityEventProviders()
	t.Cleanup(func() {
		for _, p := range providers {
			p.Close() //nolint:errcheck
		}
	})

	if _, ok := providers["registered-only"]; !ok {
		t.Fatalf("registered-only missing from transient providers: %#v", providers)
	}
	if _, ok := providers["pending-city"]; !ok {
		t.Fatalf("pending-city missing from transient providers: %#v", providers)
	}
	if _, ok := providers["running-city"]; ok {
		t.Fatalf("running-city should be handled by running-city multiplexer path, not transient providers")
	}
	for name, provider := range providers {
		if _, ok := provider.(*events.FileRecorder); ok {
			t.Fatalf("provider %q should not retain a live file recorder", name)
		}
		list, err := provider.List(events.Filter{Type: events.CityCreated})
		if err != nil {
			t.Fatalf("List(%s): %v", name, err)
		}
		if len(list) != 1 {
			t.Fatalf("List(%s) returned %d city.created events, want 1", name, len(list))
		}
	}
}

func TestCityRegistryTransientCityEventProvidersSkipMissingLogs(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	missingPath := filepath.Join(t.TempDir(), "missing-city")
	if err := os.MkdirAll(missingPath, 0o755); err != nil {
		t.Fatal(err)
	}

	regFile := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := regFile.Register(missingPath, "missing-city"); err != nil {
		t.Fatal(err)
	}

	reg := newCityRegistry()
	providers := reg.TransientCityEventProviders()
	if _, ok := providers["missing-city"]; ok {
		t.Fatalf("missing-city should be skipped when events.jsonl is absent: %#v", providers)
	}
	if _, err := os.Stat(filepath.Join(missingPath, ".gc", "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("events.jsonl stat = %v, want not exists", err)
	}
}

func writeCityEventLog(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(path, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := events.NewFileRecorder(filepath.Join(path, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(events.Event{Type: events.CityCreated, Actor: "gc", Subject: name})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCityRegistrySnapshotImmutability(t *testing.T) {
	reg := newCityRegistry()
	cs := &controllerState{}
	reg.Add("/path/i", &managedCity{
		name:    "city-i",
		started: true,
		cr:      &CityRuntime{cityName: "city-i", cs: cs},
	})

	snap1 := reg.Snapshot()

	// Add another city.
	reg.Add("/path/j", &managedCity{
		name: "city-j",
		cr:   &CityRuntime{cityName: "city-j"},
	})

	snap2 := reg.Snapshot()

	// snap1 should still have 1 entry.
	if len(snap1.all) != 1 {
		t.Fatalf("snap1 should have 1 entry, got %d", len(snap1.all))
	}
	// snap2 should have 2 entries.
	if len(snap2.all) != 2 {
		t.Fatalf("snap2 should have 2 entries, got %d", len(snap2.all))
	}
}

func TestCityRegistryConcurrentReadWrite(_ *testing.T) {
	reg := newCityRegistry()

	// Pre-populate.
	for i := 0; i < 5; i++ {
		name := "city-" + string(rune('a'+i))
		reg.Add("/path/"+name, &managedCity{
			name:    name,
			started: true,
			cr:      &CityRuntime{cityName: name, cs: &controllerState{}},
		})
	}

	// Run concurrent readers and writers for 1 second.
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 10 readers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := reg.Snapshot()
					_ = len(snap.all)
					_ = reg.CityState("city-a")
					_ = reg.ListCities()
				}
			}
		}()
	}

	// 3 writers.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := "writer-" + string(rune('0'+id))
			path := "/path/" + name
			for {
				select {
				case <-stop:
					return
				default:
					reg.Add(path, &managedCity{
						name: name,
						cr:   &CityRuntime{cityName: name},
					})
					reg.UpdateCallback(path, func(m *managedCity) {
						m.started = true
					})
					reg.Remove(path)
				}
			}
		}(i)
	}

	time.Sleep(1 * time.Second)
	close(stop)
	wg.Wait()

	// If we got here without panic or race, the test passes.
}

func TestCityRegistryGetHas(t *testing.T) {
	reg := newCityRegistry()
	mc := &managedCity{name: "city-k", cr: &CityRuntime{cityName: "city-k"}}
	reg.Add("/path/k", mc)

	if !reg.Has("/path/k") {
		t.Error("Has should return true for existing city")
	}
	if reg.Has("/nonexistent") {
		t.Error("Has should return false for nonexistent city")
	}
	// Get() was removed — all access goes through Snapshot or ReadCallback.
	// Verify ReadCallback can access the city:
	var found bool
	reg.ReadCallback(func(cities map[string]*managedCity, _ map[string]cityInitProgress, _ map[string]*initFailRecord, _ map[string]*panicRecord) {
		_, found = cities["/path/k"]
	})
	if !found {
		t.Error("ReadCallback should find the city")
	}
}

func TestCityRegistryRemovePurgesBackoffState(t *testing.T) {
	reg := newCityRegistry()
	reg.Add("/path/l", &managedCity{
		name: "city-l",
		cr:   &CityRuntime{cityName: "city-l"},
	})

	// Add backoff state.
	reg.BatchUpdate(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		panicHistory map[string]*panicRecord,
	) {
		initStatus["/path/l"] = cityInitProgress{name: "city-l", status: "loading"}
		initFailures["/path/l"] = &initFailRecord{count: 3, lastError: "boom"}
		panicHistory["/path/l"] = &panicRecord{count: 2}
	})

	// Remove purges everything.
	reg.Remove("/path/l")

	snap := reg.Snapshot()
	if len(snap.all) != 0 {
		t.Fatalf("expected 0 entries after remove, got %d", len(snap.all))
	}
}

func TestCityRegistryInitFailureInListCities(t *testing.T) {
	reg := newCityRegistry()

	// Add an init-failure entry (not in cities map).
	reg.BatchUpdate(func(
		_ map[string]*managedCity,
		_ map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		initFailures["/path/broken"] = &initFailRecord{
			count:     5,
			lastError: "config parse error",
		}
	})

	list := reg.ListCities()
	if len(list) != 1 {
		t.Fatalf("expected 1 city in list, got %d", len(list))
	}
	ci := list[0]
	if ci.Status != "init_failed" {
		t.Errorf("status = %q, want init_failed", ci.Status)
	}
	if ci.Error != "config parse error" {
		t.Errorf("error = %q, want 'config parse error'", ci.Error)
	}
	if ci.Running {
		t.Error("init-failed city should not be running")
	}
}

func TestCityRegistryByNameCollisionWithFailedCity(t *testing.T) {
	reg := newCityRegistry()

	// Add a running city named "foo"
	mc := &managedCity{
		name:    "foo",
		started: true,
		cr:      &CityRuntime{},
		cancel:  func() {},
		done:    make(chan struct{}),
	}
	reg.Add("/srv/alpha", mc)

	// Add an init failure for path "/srv/foo" — basename is "foo"
	reg.BatchUpdate(func(
		_ map[string]*managedCity,
		_ map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		initFailures["/srv/foo"] = &initFailRecord{lastError: "test failure"}
	})

	// CityState("foo") should return the running city, NOT the failed one
	cs := reg.CityState("foo")
	if cs == nil {
		t.Fatal("CityState('foo') returned nil — failed city basename overwrote running city in byName")
	}

	// Both should appear in ListCities (via snap.all)
	cities := reg.ListCities()
	if len(cities) != 2 {
		t.Fatalf("ListCities returned %d cities, want 2", len(cities))
	}
}

func cityChangeFired(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// CityChanges must fire when the set of cities or their running state
// changes, so an open supervisor event stream can attach and detach city
// providers (#6861), and must stay quiet for status-only rebuilds that the
// reconcile tick performs constantly.
func TestCityRegistryCityChangesSignalsMembershipChanges(t *testing.T) {
	reg := newCityRegistry()
	var _ api.CityChangeNotifier = reg

	ch := reg.CityChanges()
	reg.Add("/path/a", &managedCity{name: "city-a", cr: &CityRuntime{cityName: "city-a"}, status: "starting"})
	if !cityChangeFired(ch) {
		t.Fatal("CityChanges did not fire when a city was added")
	}

	ch = reg.CityChanges()
	reg.UpdateCallback("/path/a", func(m *managedCity) { m.status = "starting_agents" })
	if cityChangeFired(ch) {
		t.Fatal("CityChanges fired for a status-only update")
	}

	reg.UpdateCallback("/path/a", func(m *managedCity) { m.started = true })
	if !cityChangeFired(ch) {
		t.Fatal("CityChanges did not fire when the city started")
	}

	ch = reg.CityChanges()
	reg.Remove("/path/a")
	if !cityChangeFired(ch) {
		t.Fatal("CityChanges did not fire when the city was removed")
	}
}

// This is the durable boundary after the API stored correlation but before
// registration. Exercise the real pre-admission production seam with durable
// files, without invoking the listener-owning supervisor loop.
func TestSupervisorRegistryStartupSettlesInterruptedCreateBeforeRegistration(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	city := filepath.Join(t.TempDir(), "not-yet-created")
	const requestID = "req-interrupted-before-scaffold"
	old := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := old.StorePendingCityCreateRequestID(city, requestID); err != nil {
		t.Fatal(err)
	}
	file, err := openSupervisorEventsRecorder(supervisor.RuntimeDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	restarted := newCityRegistry()
	restarted.SetSupervisorRecorder(file)
	var stderr bytes.Buffer
	reopened := restarted.prepareSupervisorRegistry(&stderr)
	rows, err := file.ListInFlight(events.Filter{Type: events.RequestFailed})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("interrupted create terminal events=%d, want one globally observable failure; stderr=%s", len(rows), &stderr)
	}
	var failed api.RequestFailedPayload
	if err := json.Unmarshal(rows[0].Payload, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.RequestID != requestID || failed.Operation != api.RequestOperationCityCreate || failed.ErrorCode != "city_init_interrupted" {
		t.Fatalf("interrupted create failure=%+v", failed)
	}
	if id, ok, err := reopened.ConsumePendingCityRequestID(city); err != nil || ok || id != "" {
		t.Fatalf("reopened registry still fences path: (%q,%t,%v)", id, ok, err)
	}
	if _, err := os.Stat(city); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery touched uncreated city: %v", err)
	}
}

func interruptedCreateRegistry(t *testing.T) (*cityRegistry, *supervisor.Registry, *events.FileRecorder) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	file, err := events.NewFileRecorder(filepath.Join(supervisor.RuntimeDir(), "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	registry := newCityRegistry()
	registry.SetSupervisorRecorder(file)
	return registry, supervisor.NewRegistry(supervisor.RegistryPath()), file
}

func postRecoveryCity(t *testing.T, mux *api.SupervisorMux, city, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(struct {
		Dir      string `json:"dir"`
		Provider string `json:"provider"`
	}{city, "claude"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/city", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	req.Header.Set("Idempotency-Key", key)
	out := httptest.NewRecorder()
	mux.ServeHTTP(out, req)
	return out
}

func TestCityRegistryInterruptedCreateGlobalOutcomeAndNewRequest(t *testing.T) {
	registry, durable, _ := interruptedCreateRegistry(t)
	city := filepath.Join(t.TempDir(), "uncreated")
	const requestID = "req-lost-before-scaffold"
	if err := registry.StorePendingCityCreateRequestID(city, requestID); err != nil {
		t.Fatal(err)
	}
	// Reopen both the resolver and durable registry as a new process would.
	restarted := newCityRegistry()
	restarted.SetSupervisorRecorder(registry.SupervisorEventRecorder())
	if err := restarted.recoverInterruptedCityCreates(supervisor.NewRegistry(supervisor.RegistryPath())); err != nil {
		t.Fatal(err)
	}
	oldReload := reloadSupervisorNoWaitHook
	reloadSupervisorNoWaitHook = func() error { return nil }
	t.Cleanup(func() { reloadSupervisorNoWaitHook = oldReload })
	mux := api.NewSupervisorMux(restarted, mustNewCityInitService(t), false, "test", "test", time.Time{}).WithAnyHostAllowed()
	listed := httptest.NewRecorder()
	mux.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/v0/events?type=request.failed", nil))
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), requestID) || !strings.Contains(listed.Body.String(), "__supervisor__") {
		t.Fatalf("orphan failure not globally observable without a city: %d %s", listed.Code, listed.Body.String())
	}
	created := postRecoveryCity(t, mux, city, "new-create")
	if created.Code != http.StatusAccepted {
		t.Fatalf("real domain create after settlement=%d %s", created.Code, created.Body.String())
	}
	entries, err := durable.List()
	if err != nil || len(entries) != 1 || pendingRequestKey(entries[0].Path) != pendingRequestKey(city) {
		t.Fatalf("real scaffold did not register exactly its new city: %+v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(city, "city.toml")); err != nil {
		t.Fatalf("real domain scaffold absent: %v", err)
	}
	if replay := postRecoveryCity(t, mux, city, "new-create"); replay.Code != http.StatusAccepted || replay.Body.String() != created.Body.String() {
		t.Fatalf("same-process replay changed accepted body: %d %s", replay.Code, replay.Body.String())
	}
	changed := city + "-changed"
	if conflict := postRecoveryCity(t, mux, changed, "new-create"); conflict.Code != http.StatusUnprocessableEntity || !strings.Contains(conflict.Body.String(), "idempotency-mismatch") {
		t.Fatalf("changed-body retry was not refused: %d %s", conflict.Code, conflict.Body.String())
	}
	if _, err := os.Stat(changed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed-body retry touched another directory: %v", err)
	}
}

func TestCityRegistryInterruptedPartialCreatePreservesScaffoldAndGuidesOperator(t *testing.T) {
	registry, durable, file := interruptedCreateRegistry(t)
	city := filepath.Join(t.TempDir(), "partial-create")
	if err := cityInitDoInit(context.Background(), cityinit.InitRequest{Dir: city, ConfigName: "gascity", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(city, "city.toml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(city, "kept-user-file.txt")
	if err := os.WriteFile(kept, []byte("preserve this actual file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registry.StorePendingCityCreateRequestID(city, "req-partial-create"); err != nil {
		t.Fatal(err)
	}
	if err := registry.recoverInterruptedCityCreates(durable); err != nil {
		t.Fatal(err)
	}
	mux := api.NewSupervisorMux(registry, mustNewCityInitService(t), false, "test", "test", time.Time{}).WithAnyHostAllowed()
	out := postRecoveryCity(t, mux, city, "new-request-for-partial")
	if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), "gc init --no-start") {
		t.Fatalf("partial scaffold must refuse overwrite and name operator recovery: %d %s", out.Code, out.Body.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("recovery changed the existing config: %v", err)
	}
	contents, err := os.ReadFile(kept)
	if err != nil || string(contents) != "preserve this actual file" {
		t.Fatalf("recovery changed user files: %v", err)
	}
	rows, err := file.List(events.Filter{Type: events.RequestFailed})
	if err != nil || len(rows) != 1 {
		t.Fatalf("partial create terminal rows=%+v err=%v", rows, err)
	}
	var failed api.RequestFailedPayload
	if err := json.Unmarshal(rows[0].Payload, &failed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failed.ErrorMessage, city) || !strings.Contains(failed.ErrorMessage, "preserved") || !strings.Contains(failed.ErrorMessage, "gc init --no-start") {
		t.Fatalf("interrupted-create guidance=%q", failed.ErrorMessage)
	}
}

func TestCityRegistryInterruptedCreateDeduplicatesOnlyMatchingOperation(t *testing.T) {
	for _, operation := range []string{api.RequestOperationCityCreate, api.RequestOperationCityUnregister} {
		t.Run(operation, func(t *testing.T) {
			registry, durable, file := interruptedCreateRegistry(t)
			city := filepath.Join(t.TempDir(), "interrupted")
			const requestID = "req-append-before-registry-save"
			if err := registry.StorePendingCityCreateRequestID(city, requestID); err != nil {
				t.Fatal(err)
			}
			// Real file append with the pending registry still intact models a
			// process death before the registry transaction could commit.
			api.EmitTypedEvent(file, events.RequestFailed, "interrupted", api.RequestFailedPayload{RequestID: requestID, Operation: operation, ErrorCode: "prior_failure", ErrorMessage: "retained prior outcome"})
			if err := registry.recoverInterruptedCityCreates(supervisor.NewRegistry(supervisor.RegistryPath())); err != nil {
				t.Fatal(err)
			}
			rows, err := file.List(events.Filter{Type: events.RequestFailed})
			want := 1
			if operation != api.RequestOperationCityCreate {
				want = 2
			}
			if err != nil || len(rows) != want {
				t.Fatalf("terminal rows=%d want=%d err=%v", len(rows), want, err)
			}
			if _, found, err := durable.ConsumePendingCityRequestID(city); err != nil || found {
				t.Fatalf("acknowledged create remains pending: found=%t err=%v", found, err)
			}
		})
	}
}

type inFlightCreateRecoveryProvider struct {
	*events.Fake
	appends int
}

func (p *inFlightCreateRecoveryProvider) List(events.Filter) ([]events.Event, error) { return nil, nil }

func (p *inFlightCreateRecoveryProvider) ListInFlight(filter events.Filter) ([]events.Event, error) {
	return p.Fake.List(filter)
}

func (p *inFlightCreateRecoveryProvider) RecordAck(event events.Event) error {
	p.appends++
	return p.Fake.RecordAck(event)
}

func TestCityRegistryInterruptedCreateReusesRotatingTerminal(t *testing.T) {
	registry, durable, _ := interruptedCreateRegistry(t)
	city := filepath.Join(t.TempDir(), "rotating-terminal")
	const requestID = "req-terminal-in-rotating-segment"
	if err := registry.StorePendingCityCreateRequestID(city, requestID); err != nil {
		t.Fatal(err)
	}
	provider := &inFlightCreateRecoveryProvider{Fake: events.NewFake()}
	api.EmitTypedEvent(provider.Fake, events.RequestFailed, "rotating-terminal", api.RequestFailedPayload{RequestID: requestID, Operation: api.RequestOperationCityCreate, ErrorCode: "retained", ErrorMessage: "already appended before process death"})
	registry.SetSupervisorRecorder(provider)
	if err := registry.recoverInterruptedCityCreates(durable); err != nil {
		t.Fatal(err)
	}
	if provider.appends != 0 {
		t.Fatalf("appended %d duplicate outcomes while terminal was in rotation", provider.appends)
	}
}

func TestCityRegistryInterruptedCreateRetainsFailedAcknowledgment(t *testing.T) {
	registry, durable, file := interruptedCreateRegistry(t)
	city := filepath.Join(t.TempDir(), "unacknowledged")
	if err := registry.StorePendingCityCreateRequestID(city, "req-unacknowledged"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.recoverInterruptedCityCreates(durable); err == nil {
		t.Fatal("closed event recorder silently acknowledged recovery")
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).StorePendingCityCreateRequestID(city, "req-other"); !errors.Is(err, supervisor.ErrPendingCityRequestExists) {
		t.Fatalf("failed acknowledgment discarded pending request: %v", err)
	}
}

func TestSupervisorRegistryStartupRetainsUnacknowledgedRequestAndReturnsUsableRegistry(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	city := filepath.Join(t.TempDir(), "orphan")
	durable := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := durable.StorePendingCityCreateRequestID(city, "req-unavailable-events"); err != nil {
		t.Fatal(err)
	}
	if err := durable.Register(filepath.Join(t.TempDir(), "healthy"), "healthy"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(supervisor.RuntimeDir(), "events.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if recorder, err := openSupervisorEventsRecorder(supervisor.RuntimeDir(), io.Discard); err == nil {
		if err := recorder.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("fixture event-log directory did not refuse its native recorder")
	}
	// This is the state runSupervisor passes to the production startup seam
	// when its actual event recorder cannot be opened. There is no listener.
	restarted := newCityRegistry()
	var stderr bytes.Buffer
	usable := restarted.prepareSupervisorRegistry(&stderr)
	if usable == nil || !strings.Contains(stderr.String(), "recovery deferred; pending request retained") {
		t.Fatalf("event failure blocked or silently cleared registry preparation: stderr=%s", &stderr)
	}
	if id, found, err := usable.ConsumePendingCityRequestID(city); err != nil || !found || id != "req-unavailable-events" {
		t.Fatalf("unacknowledged request not retained: %q %t %v", id, found, err)
	}
	entries, err := usable.List()
	if err != nil || len(entries) != 1 || entries[0].EffectiveName() != "healthy" {
		t.Fatalf("unrelated registered city changed: %+v %v", entries, err)
	}
}
