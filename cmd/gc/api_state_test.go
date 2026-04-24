package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestControllerStateReadAccess(t *testing.T) {
	sp := runtime.NewFake()
	ep := events.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "rig1", Path: t.TempDir()},
		},
	}

	cs := newControllerState(context.Background(), cfg, sp, ep, "test-city", t.TempDir())

	if got := cs.CityName(); got != "test-city" {
		t.Errorf("CityName() = %q, want %q", got, "test-city")
	}
	if cs.Config() != cfg {
		t.Error("Config() returned wrong config")
	}
	if cs.SessionProvider() != sp {
		t.Error("SessionProvider() returned wrong provider")
	}
	if cs.EventProvider() != ep {
		t.Error("EventProvider() returned wrong provider")
	}

	stores := cs.BeadStores()
	if len(stores) != 2 {
		t.Errorf("BeadStores() len = %d, want 2 (city + rig)", len(stores))
	}
	if stores[cs.CityName()] == nil {
		t.Errorf("BeadStores()[%q] = nil", cs.CityName())
	}
	if cs.BeadStore("rig1") == nil {
		t.Error("BeadStore(rig1) = nil")
	}
	if cs.BeadStore("nonexistent") != nil {
		t.Error("BeadStore(nonexistent) should be nil")
	}

	provs := cs.MailProviders()
	if len(provs) != 1 {
		t.Errorf("MailProviders() len = %d, want 1", len(provs))
	}
	if cs.MailProvider("rig1") == nil {
		t.Error("MailProvider(rig1) = nil")
	}
}

func TestControllerStateConcurrentAccess(t *testing.T) {
	sp := runtime.NewFake()
	ep := events.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "rig1", Path: t.TempDir()},
		},
	}

	cs := newControllerState(context.Background(), cfg, sp, ep, "test-city", t.TempDir())

	// Concurrent readers should not race.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cs.Config()
			_ = cs.SessionProvider()
			_ = cs.BeadStores()
			_ = cs.MailProviders()
			_ = cs.EventProvider()
			_ = cs.CityName()
			_ = cs.CityPath()
		}()
	}
	wg.Wait()
}

func TestControllerStateUpdate(t *testing.T) {
	sp := runtime.NewFake()
	ep := events.NewFake()
	cfg1 := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs: []config.Rig{
			{Name: "rig1", Path: t.TempDir()},
		},
	}

	cs := newControllerState(context.Background(), cfg1, sp, ep, "city1", t.TempDir())

	if len(cs.BeadStores()) != 2 {
		t.Fatalf("initial stores = %d, want 2 (city + rig)", len(cs.BeadStores()))
	}

	// Update with new config adding a rig.
	cfg2 := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs: []config.Rig{
			{Name: "rig1", Path: t.TempDir()},
			{Name: "rig2", Path: t.TempDir()},
		},
	}

	sp2 := runtime.NewFake()
	cs.update(cfg2, sp2)

	if len(cs.BeadStores()) != 3 {
		t.Errorf("updated stores = %d, want 3 (city + 2 rigs)", len(cs.BeadStores()))
	}
	if cs.SessionProvider() != sp2 {
		t.Error("SessionProvider() not updated")
	}
	if cs.Config() != cfg2 {
		t.Error("Config() not updated")
	}
}

func TestControllerStateMutateAndPokeReloadsControllerAndRefreshesConfig(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tomlPath := writeCityTOML(t, dir, "test-city", "mayor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", dir)
	cs.pokeCh = make(chan struct{}, 1)

	sockPath := controllerSocketPath(dir)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir controller socket dir: %v", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(%q): %v", sockPath, err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	reloadCmd := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil {
			reloadCmd <- strings.TrimSpace(line)
		}
		_, _ = conn.Write([]byte("ok\n"))
	}()

	writeCityTOML(t, dir, "test-city", "mayor", "worker")

	if err := cs.mutateAndPoke(func() error { return nil }); err != nil {
		t.Fatalf("mutateAndPoke: %v", err)
	}

	select {
	case got := <-reloadCmd:
		if got != "reload" {
			t.Fatalf("reload command = %q, want reload", got)
		}
	default:
		t.Fatal("expected reload command")
	}

	select {
	case <-done:
	default:
		t.Fatal("reload listener did not finish")
	}

	select {
	case <-cs.pokeCh:
		t.Fatal("unexpected poke when reload succeeded")
	default:
	}

	if got := len(cs.Config().Agents); got != 2 {
		t.Fatalf("agent count = %d, want 2", got)
	}
	if got := cs.Config().Agents[1].Name; got != "worker" {
		t.Fatalf("second agent = %q, want worker", got)
	}
}

func TestControllerStateMutateAndPokeFallsBackToPokeWhenReloadUnavailable(t *testing.T) {
	dir := t.TempDir()
	tomlPath := writeCityTOML(t, dir, "test-city", "mayor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", dir)
	cs.pokeCh = make(chan struct{}, 1)

	if err := cs.mutateAndPoke(func() error { return nil }); err != nil {
		t.Fatalf("mutateAndPoke: %v", err)
	}

	select {
	case <-cs.pokeCh:
	default:
		t.Fatal("expected poke when reload command failed")
	}
}

func TestControllerStateBuildStoresUsesScopeLocalFileStores(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig1", Path: rigDir}},
	}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)

	rigStore := cs.BeadStore("rig1")
	if rigStore == nil {
		t.Fatal("BeadStore(rig1) = nil")
	}
	cityStore := cs.CityBeadStore()
	if cityStore == nil {
		t.Fatal("CityBeadStore() = nil")
	}

	if _, err := rigStore.Create(beads.Bead{Title: "rig bead", Type: "task"}); err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	cityList, err := cityStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("city List after rig create: %v", err)
	}
	if len(cityList) != 0 {
		t.Fatalf("city store should stay empty after rig create, got %d bead(s)", len(cityList))
	}

	if _, err := cityStore.Create(beads.Bead{Title: "city bead", Type: "task"}); err != nil {
		t.Fatalf("city Create: %v", err)
	}
	rigList, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List after city create: %v", err)
	}
	if len(rigList) != 1 || rigList[0].Title != "rig bead" {
		t.Fatalf("rig store should still contain only its own bead, got %#v", rigList)
	}
}

func TestControllerStateBuildStoresFileStoresUseLockFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig1", Path: rigDir}},
	}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)

	rigStore := cs.BeadStore("rig1")
	if rigStore == nil {
		t.Fatal("BeadStore(rig1) = nil")
	}
	if _, err := rigStore.Create(beads.Bead{Title: "rig bead", Type: "task"}); err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc", "beads.json.lock")); err != nil {
		t.Fatalf("rig lock file missing: %v", err)
	}

	cityStore := cs.CityBeadStore()
	if cityStore == nil {
		t.Fatal("CityBeadStore() = nil")
	}
	if _, err := cityStore.Create(beads.Bead{Title: "city bead", Type: "task"}); err != nil {
		t.Fatalf("city Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "beads.json.lock")); err != nil {
		t.Fatalf("city lock file missing: %v", err)
	}
}

func TestControllerStateFileRigStoreReloadsAcrossConcurrentHandles(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig1", Path: rigDir}},
	}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)
	rigStore := cs.BeadStore("rig1")
	if rigStore == nil {
		t.Fatal("BeadStore(rig1) = nil")
	}
	if _, err := rigStore.Create(beads.Bead{Title: "controller-1", Type: "task"}); err != nil {
		t.Fatalf("controller Create 1: %v", err)
	}

	otherStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, err := otherStore.Create(beads.Bead{Title: "cli", Type: "task"}); err != nil {
		t.Fatalf("cli Create: %v", err)
	}
	if _, err := rigStore.Create(beads.Bead{Title: "controller-2", Type: "task"}); err != nil {
		t.Fatalf("controller Create 2: %v", err)
	}

	reloadedStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig) reload: %v", err)
	}
	list, err := reloadedStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("reload List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("rig store bead count = %d, want 3 after interleaved writes: %#v", len(list), list)
	}
	seen := map[string]bool{}
	for _, bead := range list {
		seen[bead.Title] = true
	}
	for _, want := range []string{"controller-1", "cli", "controller-2"} {
		if !seen[want] {
			t.Fatalf("missing bead %q after interleaved writes: %#v", want, list)
		}
	}
}

func TestControllerStateLegacyFileProviderUsesSharedCityStoreWithoutCreatingRigState(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyCityStore, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}

	if _, err := legacyCityStore.Create(beads.Bead{Title: "legacy city bead", Type: "task"}); err != nil {
		t.Fatalf("legacy city Create: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig1", Path: rigDir}},
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)

	rigStore := cs.BeadStore("rig1")
	if rigStore == nil {
		t.Fatal("BeadStore(rig1) = nil")
	}
	list, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(list) != 1 || list[0].Title != "legacy city bead" {
		t.Fatalf("rig store should read legacy shared city data, got %#v", list)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("legacy rig open should not create rig .gc state, stat err = %v", err)
	}
}

func TestControllerStateLegacyFileProviderSharesRigStoreHandle(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigOne := filepath.Join(t.TempDir(), "rig1")
	rigTwo := filepath.Join(t.TempDir(), "rig2")
	if err := os.MkdirAll(rigOne, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigTwo, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "rig1", Path: rigOne},
			{Name: "rig2", Path: rigTwo},
		},
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)

	rigStoreOne := cs.BeadStore("rig1")
	rigStoreTwo := cs.BeadStore("rig2")
	if rigStoreOne == nil || rigStoreTwo == nil {
		t.Fatal("expected both rig stores")
	}
	if _, err := rigStoreOne.Create(beads.Bead{Title: "shared bead", Type: "task"}); err != nil {
		t.Fatalf("rig1 Create: %v", err)
	}
	list, err := rigStoreTwo.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig2 List: %v", err)
	}
	if len(list) != 1 || list[0].Title != "shared bead" {
		t.Fatalf("rig2 store should immediately observe shared legacy bead, got %#v", list)
	}
	reloadedCityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	cityList, err := reloadedCityStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("city List: %v", err)
	}
	if len(cityList) != 1 || cityList[0].Title != "shared bead" {
		t.Fatalf("city store should contain shared bead after reopen, got %#v", cityList)
	}
}

func TestControllerStateOpenRigStoreFileOpenErrorDoesNotFallbackToBd(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".gc", "beads.json"), []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := &controllerState{cityPath: cityDir}
	store := cs.openRigStore("file", "rig1", rigDir, "rg")
	if _, ok := store.(*beads.BdStore); ok {
		t.Fatalf("openRigStore returned %T, want file-open failure instead of bd fallback", store)
	}
	if _, err := store.Create(beads.Bead{Title: "broken", Type: "task"}); err == nil {
		t.Fatal("Create succeeded, want file-open error")
	} else if !strings.Contains(err.Error(), "open file rig store") {
		t.Fatalf("Create error = %v, want file-open failure", err)
	}
}

func TestControllerStateBuildStoresUsesScopeAwareProviderForMixedRig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   rigDir,
			Prefix: "fe",
		}},
	}

	cs := &controllerState{cityPath: cityDir, cfg: cfg}
	stores := cs.buildStores(cfg)
	store, ok := stores["frontend"]
	if !ok {
		t.Fatal("buildStores() missing frontend store")
	}
	if _, ok := store.(*beads.FileStore); ok {
		t.Fatalf("buildStores() returned %T, want scope-aware non-file store for bd-backed rig", store)
	}
}

func TestControllerStateBuildStoresUsesRigFileMarkerUnderLegacyFileCity(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   rigDir,
			Prefix: "fe",
		}},
	}

	cs := &controllerState{cityPath: cityDir, cfg: cfg}
	stores := cs.buildStores(cfg)
	rigStore, ok := stores["frontend"]
	if !ok {
		t.Fatal("buildStores() missing frontend store")
	}
	if _, err := rigStore.Create(beads.Bead{Title: "rig bead", Type: "task"}); err != nil {
		t.Fatalf("rig Create: %v", err)
	}

	cityStore, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	cityList, err := cityStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("city List: %v", err)
	}
	if len(cityList) != 0 {
		t.Fatalf("city store should stay empty after rig create, got %#v", cityList)
	}

	persistedRigStore, err := openScopeLocalFileStore(rigDir)
	if err != nil {
		t.Fatal(err)
	}
	rigList, err := persistedRigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(rigList) != 1 || rigList[0].Title != "rig bead" {
		t.Fatalf("rig store should contain its own bead, got %#v", rigList)
	}
}

func TestControllerStateNilEventProvider(t *testing.T) {
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
	}

	cs := newControllerState(context.Background(), cfg, sp, nil, "test-city", t.TempDir())

	if cs.EventProvider() != nil {
		t.Error("EventProvider() should be nil when events disabled")
	}
}

func TestControllerStateOrdersIncludeVisibleCityRoot(t *testing.T) {
	cityDir := t.TempDir()
	autoDir := filepath.Join(cityDir, "orders", "digest")
	if err := os.MkdirAll(autoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(autoDir, "order.toml"), []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "test-city"},
	}, runtime.NewFake(), events.NewFake(), "test-city", cityDir)

	aa := cs.Orders()
	if len(aa) != 1 {
		t.Fatalf("Orders() returned %d entries, want 1", len(aa))
	}
	if aa[0].Name != "digest" {
		t.Fatalf("order name = %q, want digest", aa[0].Name)
	}
}

func TestControllerStateMutationsPokeController(t *testing.T) {
	cases := []struct {
		name    string
		initial func(*config.City)
		mutate  func(*controllerState) error
		verify  func(*testing.T, *config.City)
	}{
		{
			name: "suspend agent",
			mutate: func(cs *controllerState) error {
				return cs.SuspendAgent("rig1/worker")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if !cfg.Agents[0].Suspended {
					t.Fatal("agent should be suspended after SuspendAgent")
				}
			},
		},
		{
			name: "resume agent",
			initial: func(cfg *config.City) {
				cfg.Agents[0].Suspended = true
			},
			mutate: func(cs *controllerState) error {
				return cs.ResumeAgent("rig1/worker")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if cfg.Agents[0].Suspended {
					t.Fatal("agent should not be suspended after ResumeAgent")
				}
			},
		},
		{
			name: "suspend rig",
			mutate: func(cs *controllerState) error {
				return cs.SuspendRig("rig1")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if !cfg.Rigs[0].Suspended {
					t.Fatal("rig should be suspended after SuspendRig")
				}
			},
		},
		{
			name: "resume rig",
			initial: func(cfg *config.City) {
				cfg.Rigs[0].Suspended = true
			},
			mutate: func(cs *controllerState) error {
				return cs.ResumeRig("rig1")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if cfg.Rigs[0].Suspended {
					t.Fatal("rig should not be suspended after ResumeRig")
				}
			},
		},
		{
			name: "suspend city",
			mutate: func(cs *controllerState) error {
				return cs.SuspendCity()
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if !cfg.Workspace.Suspended {
					t.Fatal("city should be suspended after SuspendCity")
				}
			},
		},
		{
			name: "resume city",
			initial: func(cfg *config.City) {
				cfg.Workspace.Suspended = true
			},
			mutate: func(cs *controllerState) error {
				return cs.ResumeCity()
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if cfg.Workspace.Suspended {
					t.Fatal("city should not be suspended after ResumeCity")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs, tomlPath := newControllerStateMutationHarness(t)

			cfg, err := config.Load(fsys.OSFS{}, tomlPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if tc.initial != nil {
				tc.initial(cfg)
				content, err := cfg.Marshal()
				if err != nil {
					t.Fatalf("marshal initial config: %v", err)
				}
				if err := os.WriteFile(tomlPath, content, 0o644); err != nil {
					t.Fatalf("write initial config: %v", err)
				}
			}

			if err := tc.mutate(cs); err != nil {
				t.Fatalf("mutation failed: %v", err)
			}
			select {
			case <-cs.pokeCh:
			default:
				t.Fatal("expected controller mutation to poke reconciler")
			}

			got, err := config.Load(fsys.OSFS{}, tomlPath)
			if err != nil {
				t.Fatalf("reload config: %v", err)
			}
			tc.verify(t, got)
		})
	}
}

func TestControllerStateMutationErrorDoesNotPokeController(t *testing.T) {
	cs, _ := newControllerStateMutationHarness(t)

	if err := cs.SuspendAgent("rig1/missing"); err == nil {
		t.Fatal("SuspendAgent unexpectedly succeeded for missing agent")
	}
	select {
	case <-cs.pokeCh:
		t.Fatal("failed mutation should not poke reconciler")
	default:
	}
}

func TestControllerStateApplyBeadEventPokesController(t *testing.T) {
	cs := &controllerState{
		pokeCh: make(chan struct{}, 1),
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "agent-runtime",
		Subject: "bd-123",
		Payload: json.RawMessage(`{"id":"bd-123"}`),
	})

	select {
	case <-cs.pokeCh:
	default:
		t.Fatal("expected bead event to poke controller")
	}
}

func TestControllerStateApplyCacheReconcileEventDoesNotPokeController(t *testing.T) {
	cs := &controllerState{
		pokeCh: make(chan struct{}, 1),
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "cache-reconcile",
		Subject: "bd-123",
		Payload: json.RawMessage(`{"id":"bd-123"}`),
	})

	select {
	case <-cs.pokeCh:
		t.Fatal("cache-reconcile event should not poke controller")
	default:
	}
}

func newControllerStateMutationHarness(t *testing.T) (*controllerState, string) {
	t.Helper()

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "rig1"},
		},
		Rigs: []config.Rig{
			{Name: "rig1", Path: rigDir},
		},
	}
	content, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, content, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	return &controllerState{
		editor: configedit.NewEditor(fsys.OSFS{}, tomlPath),
		pokeCh: make(chan struct{}, 1),
	}, tomlPath
}

// TestBuildStores_ExecProviderSetsPerRigEnv is a regression test for #391:
// when GC_BEADS=exec:<script>, each rig's store must receive distinct
// GC_BEADS_PREFIX, BEADS_DIR, GC_RIG_ROOT, and GC_RIG env vars.
// Before the fix (PR #421), all exec stores shared identical env — the
// last rig's prefix won, causing a create→orphan loop in K8s multi-prefix
// deployments.
func TestBuildStores_ExecProviderSetsPerRigEnv(t *testing.T) {
	cityDir := t.TempDir()
	envDir := t.TempDir()

	// Script that captures identity env vars to a per-rig file on list calls.
	scriptContent := "#!/bin/sh\n" +
		"op=\"$1\"; shift\n" +
		"case \"$op\" in\n" +
		"  list)\n" +
		"    env | grep -E '^(GC_BEADS_PREFIX|BEADS_DIR|GC_RIG_ROOT|GC_RIG)=' " +
		"> \"" + envDir + "/${GC_RIG}.env\"\n" +
		"    echo '[]'\n" +
		"    ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	scriptPath := filepath.Join(t.TempDir(), "beads-provider.sh")
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("writing provider script: %v", err)
	}

	t.Setenv("GC_BEADS", "exec:"+scriptPath)

	rig1Path := filepath.Join(t.TempDir(), "rig-alpha")
	rig2Path := filepath.Join(t.TempDir(), "rig-bravo")
	if err := os.MkdirAll(rig1Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rig2Path, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: rig1Path, Prefix: "al"},
			{Name: "bravo", Path: rig2Path, Prefix: "br"},
		},
	}

	cs := &controllerState{cityPath: cityDir}
	stores := cs.buildStores(cfg)

	if len(stores) != 2 {
		t.Fatalf("buildStores returned %d stores, want 2", len(stores))
	}

	// Trigger each store's script to dump its env.
	for name, store := range stores {
		if _, err := store.ListOpen(); err != nil {
			t.Fatalf("ListOpen(%s): %v", name, err)
		}
	}

	// Verify each rig received distinct, correct env vars.
	type rigExpect struct {
		rig     string
		prefix  string
		rigPath string
	}
	for _, tc := range []rigExpect{
		{"alpha", "al", rig1Path},
		{"bravo", "br", rig2Path},
	} {
		envFile := filepath.Join(envDir, tc.rig+".env")
		data, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatalf("env file for rig %q not created — script was not called with GC_RIG=%s: %v",
				tc.rig, tc.rig, err)
		}
		env := string(data)

		wantPrefix := "GC_BEADS_PREFIX=" + tc.prefix
		if !strings.Contains(env, wantPrefix) {
			t.Errorf("rig %q: want %s in env, got:\n%s", tc.rig, wantPrefix, env)
		}

		wantRigRoot := "GC_RIG_ROOT=" + tc.rigPath
		if !strings.Contains(env, wantRigRoot) {
			t.Errorf("rig %q: want %s in env, got:\n%s", tc.rig, wantRigRoot, env)
		}

		wantRig := "GC_RIG=" + tc.rig
		if !strings.Contains(env, wantRig) {
			t.Errorf("rig %q: want %s in env, got:\n%s", tc.rig, wantRig, env)
		}

		// Post-#790 contract: BEADS_DIR is intentionally empty for exec
		// stores (store_target_exec.go). Scope is communicated via
		// GC_RIG_ROOT / GC_STORE_ROOT instead. Assert we did NOT regress
		// back to a per-rig BEADS_DIR projection.
		if strings.Contains(env, "BEADS_DIR="+filepath.Join(tc.rigPath, ".beads")) {
			t.Errorf("rig %q: BEADS_DIR is projecting a rig-specific path; "+
				"exec contract (PR #790) requires BEADS_DIR to stay empty so scope "+
				"is routed via GC_RIG_ROOT/GC_STORE_ROOT. env:\n%s", tc.rig, env)
		}
	}

	// Cross-rig assertion: the two rigs must have received different prefixes.
	// This is the exact regression from #391 — before PR #421, both stores
	// got identical env, so the last rig's prefix silently won.
	// Compare extracted GC_BEADS_PREFIX values (not raw env output, whose
	// line order is non-deterministic due to Go map iteration in exec.Store).
	extractPrefix := func(envFile string) string {
		data, err := os.ReadFile(envFile)
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "GC_BEADS_PREFIX=") {
				return strings.TrimPrefix(line, "GC_BEADS_PREFIX=")
			}
		}
		return ""
	}
	alphaPrefix := extractPrefix(filepath.Join(envDir, "alpha.env"))
	bravoPrefix := extractPrefix(filepath.Join(envDir, "bravo.env"))
	if alphaPrefix == bravoPrefix {
		t.Errorf("regression: alpha and bravo exec stores received the same "+
			"GC_BEADS_PREFIX=%q — store identity is not being propagated per rig",
			alphaPrefix)
	}
}

// Verify controllerState satisfies the api.State interface at compile time.
// This uses a blank import check, not an explicit runtime assertion.
var _ interface {
	Config() *config.City
	SessionProvider() runtime.Provider
	BeadStore(string) beads.Store
	BeadStores() map[string]beads.Store
	EventProvider() events.Provider
	CityName() string
	CityPath() string
} = (*controllerState)(nil)

// Verify controllerState satisfies StateMutator at compile time.
var _ interface {
	SuspendAgent(string) error
	ResumeAgent(string) error
	SuspendRig(string) error
	ResumeRig(string) error
} = (*controllerState)(nil)
