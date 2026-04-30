package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
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

func TestControllerStateRuntimeUpdateDoesNotDropPendingMutationRigs(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	current := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs:      []config.Rig{{Name: "alpha", Path: t.TempDir()}},
	}
	stale := &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}

	cs := newControllerState(context.Background(), current, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.configMutationPending.Store(true)

	cs.updateFromRuntime(stale, runtime.NewFake())

	if got := cs.Config(); got != current {
		t.Fatalf("Config() = %+v, want pending mutation config with rig alpha", got)
	}
	if !cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker cleared by stale runtime update")
	}

	cs.updateFromRuntime(current, runtime.NewFake())

	if cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker not cleared after matching runtime update")
	}
}

func TestControllerStateRuntimeUpdateAfterMutationPreservesCurrentStores(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "alpha")
	current := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   rigDir,
			Prefix: "al",
		}},
	}
	rigStore := beads.NewMemStore()
	cityStore := beads.NewMemStore()
	cs := &controllerState{
		cfg:           current,
		sp:            runtime.NewFake(),
		beadStores:    map[string]beads.Store{"alpha": rigStore},
		cityBeadStore: cityStore,
		cityName:      "city1",
		cityPath:      cityDir,
	}
	cs.configMutationPending.Store(true)

	next := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   rigDir,
			Prefix: "al",
		}},
	}
	cs.updateFromRuntime(next, runtime.NewFake())

	if got := cs.BeadStore("alpha"); got != rigStore {
		t.Fatalf("BeadStore(alpha) = %T %p, want original store %T %p", got, got, rigStore, rigStore)
	}
	if got := cs.CityBeadStore(); got != cityStore {
		t.Fatalf("CityBeadStore() = %T %p, want original store %T %p", got, got, cityStore, cityStore)
	}
	if cs.Config() != next {
		t.Fatal("Config() was not advanced to runtime snapshot")
	}
	if cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker not cleared after matching runtime update")
	}
}

func TestControllerStateCreateRigPokesReconciler(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}

	if err := cs.CreateRig(config.Rig{Name: "rig1", Path: t.TempDir()}); err != nil {
		t.Fatalf("CreateRig: %v", err)
	}

	select {
	case <-cs.pokeCh:
	default:
		t.Fatal("CreateRig did not poke the reconciler")
	}
	if !cs.configDirty.Load() {
		t.Fatal("CreateRig did not mark config dirty")
	}
	if got := cs.Config(); got == nil || len(got.Rigs) != 1 || got.Rigs[0].Name != "rig1" {
		t.Fatalf("Config() rigs = %+v, want in-memory rig snapshot to include rig1", got.Rigs)
	}
}

func TestControllerStateCreateRigInitializesStoreBeforePublishing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("enable scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("init city store: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)

	rigDir := filepath.Join(cityDir, "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	if err := cs.CreateRig(config.Rig{Name: "alpha", Path: rigDir, Prefix: "al"}); err != nil {
		t.Fatalf("CreateRig: %v", err)
	}

	store := cs.BeadStore("alpha")
	if store == nil {
		t.Fatal("BeadStore(alpha) = nil")
	}
	created, err := store.Create(beads.Bead{Title: "first rig bead", Type: "task"})
	if err != nil {
		t.Fatalf("newly published rig store Create: %v", err)
	}
	if _, err := store.Get(created.ID); err != nil {
		t.Fatalf("newly published rig store Get(%q): %v", created.ID, err)
	}
}

func TestControllerStateMutationRollsBackWhenRefreshFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "broken.toml"), []byte("["), 0o644); err != nil {
		t.Fatalf("write broken include: %v", err)
	}

	original := []byte("include = [\"broken.toml\"]\n\n[workspace]\nname = \"city1\"\n")
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, original, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}

	err := cs.CreateRig(config.Rig{Name: "rig1", Path: t.TempDir()})
	if err == nil {
		t.Fatal("CreateRig should fail when refreshing the updated snapshot fails")
	}

	restored, readErr := os.ReadFile(tomlPath)
	if readErr != nil {
		t.Fatalf("read restored city.toml: %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("city.toml = %q, want rollback to %q", restored, original)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "site.toml")); !os.IsNotExist(err) {
		t.Fatalf(".gc/site.toml stat err = %v, want file removed on rollback", err)
	}

	select {
	case <-cs.pokeCh:
		t.Fatal("CreateRig should not poke the reconciler after rollback")
	default:
	}
	if cs.configDirty.Load() {
		t.Fatal("CreateRig should not mark config dirty after rollback")
	}
	if got := cs.Config(); got == nil || len(got.Rigs) != 0 {
		t.Fatalf("Config() rigs = %+v, want rollback to preserve in-memory config", got.Rigs)
	}
}

func TestControllerStateMutationRollsBackAgentOverrideWhenRefreshFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "broken.toml"), []byte("["), 0o644); err != nil {
		t.Fatalf("write broken include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	agentDir := filepath.Join(cityDir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatalf("write prompt template: %v", err)
	}

	original := []byte("include = [\"broken.toml\"]\n\n[workspace]\nname = \"city1\"\n")
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, original, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}

	err := cs.SuspendAgent("worker")
	if err == nil {
		t.Fatal("SuspendAgent should fail when refreshing the updated snapshot fails")
	}

	if _, err := os.Stat(filepath.Join(agentDir, "agent.toml")); !os.IsNotExist(err) {
		t.Fatalf("agent.toml stat err = %v, want file removed on rollback", err)
	}
	restored, readErr := os.ReadFile(tomlPath)
	if readErr != nil {
		t.Fatalf("read restored city.toml: %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("city.toml = %q, want rollback to %q", restored, original)
	}
	if cs.configDirty.Load() {
		t.Fatal("SuspendAgent should not mark config dirty after rollback")
	}
}

func TestControllerStateAppliesCacheReconcileBeadEventsToStores(t *testing.T) {
	backing := beads.NewMemStore()
	created, err := backing.Create(beads.Bead{Title: "root"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	updated := created
	updated.Status = "in_progress"
	payload, err := json.Marshal(updated)
	if err != nil {
		t.Fatalf("marshal updated bead: %v", err)
	}
	cs := &controllerState{
		beadStores: map[string]beads.Store{"alpha": cached},
		pokeCh:     make(chan struct{}, 1),
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "cache-reconcile",
		Subject: created.ID,
		Payload: payload,
	})

	items, err := cached.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("cached items = %+v, want only %s", items, created.ID)
	}
	if items[0].Status != "in_progress" {
		t.Fatalf("status after cache-reconcile event = %q, want in_progress", items[0].Status)
	}
}

func TestControllerStateBeadEventsRespectStorePrefixes(t *testing.T) {
	cityBacking := beads.NewMemStore()
	rigBacking := beads.NewMemStore()
	cityCache := beads.NewCachingStoreForTestWithPrefix(cityBacking, "mc", nil)
	rigCache := beads.NewCachingStoreForTestWithPrefix(rigBacking, "ga", nil)
	for name, cache := range map[string]*beads.CachingStore{
		"city": cityCache,
		"rig":  rigCache,
	} {
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime(%s): %v", name, err)
		}
	}

	payload, err := json.Marshal(beads.Bead{
		ID:     "mc-source",
		Title:  "city source",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("marshal city bead: %v", err)
	}
	cs := &controllerState{
		cityBeadStore: cityCache,
		beadStores:    map[string]beads.Store{"gascity": rigCache},
		pokeCh:        make(chan struct{}, 1),
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadCreated,
		Actor:   "bd-hook",
		Subject: "mc-source",
		Payload: payload,
	})

	cityItems, err := cityCache.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List city cache: %v", err)
	}
	if len(cityItems) != 1 || cityItems[0].ID != "mc-source" {
		t.Fatalf("city cache items = %+v, want mc-source", cityItems)
	}
	rigItems, err := rigCache.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List rig cache: %v", err)
	}
	if len(rigItems) != 0 {
		t.Fatalf("rig cache items = %+v, want no city bead", rigItems)
	}

	payload, err = json.Marshal(beads.Bead{
		ID:     "ga-rig",
		Title:  "rig work",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("marshal rig bead: %v", err)
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadCreated,
		Actor:   "bd-hook",
		Subject: "ga-rig",
		Payload: payload,
	})

	cityItems, err = cityCache.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List city cache after rig event: %v", err)
	}
	if len(cityItems) != 1 || cityItems[0].ID != "mc-source" {
		t.Fatalf("city cache items after rig event = %+v, want only mc-source", cityItems)
	}
	rigItems, err = rigCache.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List rig cache after rig event: %v", err)
	}
	if len(rigItems) != 1 || rigItems[0].ID != "ga-rig" {
		t.Fatalf("rig cache items after rig event = %+v, want ga-rig", rigItems)
	}
}

func TestControllerStateBeadEventsUseScopePrefixWhenConfiguredPrefixDrifts(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}}}
	bdStore := bdStoreForRig(rigDir, cityDir, cfg, cfg.Rigs[0].EffectivePrefix())
	rigCache := beads.NewCachingStoreForTestWithPrefix(beads.NewMemStore(), bdStore.IDPrefix(), nil)
	if err := rigCache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime rig cache: %v", err)
	}

	payload, err := json.Marshal(beads.Bead{
		ID:     "repo-owned",
		Title:  "rig-owned work",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("marshal rig bead: %v", err)
	}
	cs := &controllerState{
		beadStores: map[string]beads.Store{"repo": rigCache},
		pokeCh:     make(chan struct{}, 1),
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadCreated,
		Actor:   "bd-hook",
		Subject: "repo-owned",
		Payload: payload,
	})

	rigItems, err := rigCache.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List rig cache: %v", err)
	}
	if len(rigItems) != 1 || rigItems[0].ID != "repo-owned" {
		t.Fatalf("rig cache items = %+v, want repo-owned", rigItems)
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
	store := cs.openRigStore("file", "rig1", rigDir, "rg", nil)
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
		{
			name: "enable order",
			mutate: func(cs *controllerState) error {
				return cs.EnableOrder("nightly", "rig1")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Orders.Overrides) != 1 || cfg.Orders.Overrides[0].Name != "nightly" || cfg.Orders.Overrides[0].Rig != "rig1" {
					t.Fatalf("order overrides = %+v, want nightly/rig1", cfg.Orders.Overrides)
				}
				if cfg.Orders.Overrides[0].Enabled == nil || !*cfg.Orders.Overrides[0].Enabled {
					t.Fatalf("order override enabled = %v, want true", cfg.Orders.Overrides[0].Enabled)
				}
			},
		},
		{
			name: "disable order",
			mutate: func(cs *controllerState) error {
				return cs.DisableOrder("nightly", "rig1")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Orders.Overrides) != 1 || cfg.Orders.Overrides[0].Enabled == nil || *cfg.Orders.Overrides[0].Enabled {
					t.Fatalf("order overrides = %+v, want disabled nightly override", cfg.Orders.Overrides)
				}
			},
		},
		{
			name: "create agent",
			mutate: func(cs *controllerState) error {
				return cs.CreateAgent(config.Agent{Name: "helper", Dir: "rig1", Provider: "codex"})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Agents) != 2 {
					t.Fatalf("agents = %+v, want two", cfg.Agents)
				}
				if cfg.Agents[1].QualifiedName() != "rig1/helper" || cfg.Agents[1].Provider != "codex" {
					t.Fatalf("created agent = %+v, want rig1/helper with codex provider", cfg.Agents[1])
				}
			},
		},
		{
			name: "update agent",
			mutate: func(cs *controllerState) error {
				return cs.UpdateAgent("rig1/worker", api.AgentUpdate{Provider: "codex", Scope: "rig", Suspended: boolPtr(true)})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if cfg.Agents[0].Provider != "codex" || cfg.Agents[0].Scope != "rig" || !cfg.Agents[0].Suspended {
					t.Fatalf("updated agent = %+v, want provider/scope/suspended", cfg.Agents[0])
				}
			},
		},
		{
			name: "delete agent",
			mutate: func(cs *controllerState) error {
				return cs.DeleteAgent("rig1/worker")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Agents) != 0 {
					t.Fatalf("agents = %+v, want none", cfg.Agents)
				}
			},
		},
		{
			name: "create rig",
			mutate: func(cs *controllerState) error {
				return cs.CreateRig(config.Rig{Name: "rig2", Path: t.TempDir(), Prefix: "r2"})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Rigs) != 2 {
					t.Fatalf("rigs = %+v, want two", cfg.Rigs)
				}
				if cfg.Rigs[1].Name != "rig2" || cfg.Rigs[1].Prefix != "r2" {
					t.Fatalf("created rig = %+v, want rig2/r2", cfg.Rigs[1])
				}
			},
		},
		{
			name: "update rig",
			mutate: func(cs *controllerState) error {
				return cs.UpdateRig("rig1", api.RigUpdate{Path: t.TempDir(), Prefix: "rg", Suspended: boolPtr(true)})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if cfg.Rigs[0].Prefix != "rg" || !cfg.Rigs[0].Suspended {
					t.Fatalf("updated rig = %+v, want prefix/suspended", cfg.Rigs[0])
				}
			},
		},
		{
			name: "delete rig",
			mutate: func(cs *controllerState) error {
				return cs.DeleteRig("rig1")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Rigs) != 0 || len(cfg.Agents) != 0 {
					t.Fatalf("config after DeleteRig: rigs=%+v agents=%+v, want none", cfg.Rigs, cfg.Agents)
				}
			},
		},
		{
			name: "create provider",
			mutate: func(cs *controllerState) error {
				return cs.CreateProvider("codex-local", config.ProviderSpec{Command: "codex", PromptMode: "arg"})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				spec, ok := cfg.Providers["codex-local"]
				if !ok || spec.Command != "codex" || spec.PromptMode != "arg" {
					t.Fatalf("providers = %+v, want codex-local provider", cfg.Providers)
				}
			},
		},
		{
			name: "update provider",
			initial: func(cfg *config.City) {
				cfg.Providers = map[string]config.ProviderSpec{"codex-local": {Command: "codex"}}
			},
			mutate: func(cs *controllerState) error {
				return cs.UpdateProvider("codex-local", api.ProviderUpdate{
					DisplayName:  stringPtr("Codex Local"),
					Command:      stringPtr("codex-wrapper"),
					Args:         []string{"--quiet"},
					PromptMode:   stringPtr("flag"),
					PromptFlag:   stringPtr("--prompt"),
					ReadyDelayMs: intPtr(25),
					Env:          map[string]string{"GC_TEST": "1"},
				})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				spec := cfg.Providers["codex-local"]
				if spec.DisplayName != "Codex Local" || spec.Command != "codex-wrapper" || spec.PromptMode != "flag" || spec.PromptFlag != "--prompt" || spec.ReadyDelayMs != 25 {
					t.Fatalf("updated provider = %+v, want scalar updates", spec)
				}
				if len(spec.Args) != 1 || spec.Args[0] != "--quiet" || spec.Env["GC_TEST"] != "1" {
					t.Fatalf("updated provider args/env = args:%+v env:%+v, want replacement args and merged env", spec.Args, spec.Env)
				}
			},
		},
		{
			name: "delete provider",
			initial: func(cfg *config.City) {
				cfg.Providers = map[string]config.ProviderSpec{"codex-local": {Command: "codex"}}
			},
			mutate: func(cs *controllerState) error {
				return cs.DeleteProvider("codex-local")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Providers) != 0 {
					t.Fatalf("providers = %+v, want none", cfg.Providers)
				}
			},
		},
		{
			name: "set agent patch",
			mutate: func(cs *controllerState) error {
				return cs.SetAgentPatch(config.AgentPatch{Dir: "rig1", Name: "worker", Suspended: boolPtr(true)})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Patches.Agents) != 1 || cfg.Patches.Agents[0].Suspended == nil || !*cfg.Patches.Agents[0].Suspended {
					t.Fatalf("agent patches = %+v, want suspended patch", cfg.Patches.Agents)
				}
			},
		},
		{
			name: "delete agent patch",
			initial: func(cfg *config.City) {
				cfg.Patches.Agents = []config.AgentPatch{{Dir: "rig1", Name: "worker", Suspended: boolPtr(true)}}
			},
			mutate: func(cs *controllerState) error {
				return cs.DeleteAgentPatch("rig1/worker")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Patches.Agents) != 0 {
					t.Fatalf("agent patches = %+v, want none", cfg.Patches.Agents)
				}
			},
		},
		{
			name: "set rig patch",
			mutate: func(cs *controllerState) error {
				return cs.SetRigPatch(config.RigPatch{Name: "rig1", Prefix: stringPtr("rp")})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Patches.Rigs) != 1 || cfg.Patches.Rigs[0].Prefix == nil || *cfg.Patches.Rigs[0].Prefix != "rp" {
					t.Fatalf("rig patches = %+v, want prefix patch", cfg.Patches.Rigs)
				}
			},
		},
		{
			name: "delete rig patch",
			initial: func(cfg *config.City) {
				cfg.Patches.Rigs = []config.RigPatch{{Name: "rig1", Prefix: stringPtr("rp")}}
			},
			mutate: func(cs *controllerState) error {
				return cs.DeleteRigPatch("rig1")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Patches.Rigs) != 0 {
					t.Fatalf("rig patches = %+v, want none", cfg.Patches.Rigs)
				}
			},
		},
		{
			name: "set provider patch",
			mutate: func(cs *controllerState) error {
				return cs.SetProviderPatch(config.ProviderPatch{Name: "codex-local", Command: stringPtr("codex-wrapper")})
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Patches.Providers) != 1 || cfg.Patches.Providers[0].Command == nil || *cfg.Patches.Providers[0].Command != "codex-wrapper" {
					t.Fatalf("provider patches = %+v, want command patch", cfg.Patches.Providers)
				}
			},
		},
		{
			name: "delete provider patch",
			initial: func(cfg *config.City) {
				cfg.Patches.Providers = []config.ProviderPatch{{Name: "codex-local", Command: stringPtr("codex-wrapper")}}
			},
			mutate: func(cs *controllerState) error {
				return cs.DeleteProviderPatch("codex-local")
			},
			verify: func(t *testing.T, cfg *config.City) {
				t.Helper()
				if len(cfg.Patches.Providers) != 0 {
					t.Fatalf("provider patches = %+v, want none", cfg.Patches.Providers)
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
			if cs.configDirty == nil || !cs.configDirty.Load() {
				t.Fatal("expected controller mutation to mark config dirty")
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
		editor:      configedit.NewEditor(fsys.OSFS{}, tomlPath),
		pokeCh:      make(chan struct{}, 1),
		configDirty: &atomic.Bool{},
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

func TestBuildStoresBdProviderUsesPassedConfigForRigEnv(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	capturePath := filepath.Join(t.TempDir(), "bd.env")
	binDir := t.TempDir()
	fakeBD := filepath.Join(binDir, "bd")
	script := "#!/bin/sh\n" +
		"printf 'GC_RIG=%s\\nGC_RIG_ROOT=%s\\nBEADS_DIR=%s\\n' \"${GC_RIG:-}\" \"${GC_RIG_ROOT:-}\" \"${BEADS_DIR:-}\" > \"$BD_ENV_CAPTURE\"\n" +
		"printf '[]\\n'\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_ENV_CAPTURE", capturePath)
	t.Setenv("GC_BEADS", "bd")

	staleCfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	nextCfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   rigDir,
			Prefix: "al",
		}},
	}
	cs := &controllerState{
		cfg:      staleCfg,
		cityName: "test-city",
		cityPath: cityDir,
	}

	stores := cs.buildStores(nextCfg)
	if stores["alpha"] == nil {
		t.Fatal("buildStores did not create alpha store")
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured bd env: %v", err)
	}
	env := string(data)
	if !strings.Contains(env, "GC_RIG=alpha\n") {
		t.Fatalf("captured env missing GC_RIG=alpha; got:\n%s", env)
	}
	if !strings.Contains(env, "GC_RIG_ROOT="+rigDir+"\n") {
		t.Fatalf("captured env missing rig root %q; got:\n%s", rigDir, env)
	}
	if !strings.Contains(env, "BEADS_DIR="+filepath.Join(rigDir, ".beads")+"\n") {
		t.Fatalf("captured env missing rig BEADS_DIR; got:\n%s", env)
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
