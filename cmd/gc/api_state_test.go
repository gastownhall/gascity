package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type corruptCityAfterRemoveFS struct {
	fsys.OSFS
	triggerPath string
	cityToml    string
	fired       bool
}

func (f *corruptCityAfterRemoveFS) Remove(name string) error {
	err := f.OSFS.Remove(name)
	if err == nil && !f.fired && canonicalTestPath(name) == canonicalTestPath(f.triggerPath) {
		f.fired = true
		if writeErr := os.WriteFile(f.cityToml, []byte("["), 0o644); writeErr != nil {
			return writeErr
		}
	}
	return err
}

type corruptCityAfterRenameFS struct {
	fsys.OSFS
	triggerPath string
	cityToml    string
	fired       bool
}

func (f *corruptCityAfterRenameFS) Rename(oldpath, newpath string) error {
	err := f.OSFS.Rename(oldpath, newpath)
	if err == nil && !f.fired && canonicalTestPath(newpath) == canonicalTestPath(f.triggerPath) {
		f.fired = true
		if writeErr := os.WriteFile(f.cityToml, []byte("["), 0o644); writeErr != nil {
			return writeErr
		}
	}
	return err
}

type blockingLatestEventProvider struct {
	*events.Fake
	latestCalled chan struct{}
	allowLatest  chan struct{}
	latestOnce   sync.Once
}

func newBlockingLatestEventProvider() *blockingLatestEventProvider {
	return &blockingLatestEventProvider{
		Fake:         events.NewFake(),
		latestCalled: make(chan struct{}),
		allowLatest:  make(chan struct{}),
	}
}

func (p *blockingLatestEventProvider) LatestSeq() (uint64, error) {
	p.latestOnce.Do(func() {
		close(p.latestCalled)
	})
	<-p.allowLatest
	return p.Fake.LatestSeq()
}

type failOnceWatchEventProvider struct {
	*events.Fake
	failed chan struct{}
	once   sync.Once
}

func newFailOnceWatchEventProvider() *failOnceWatchEventProvider {
	return &failOnceWatchEventProvider{
		Fake:   events.NewFake(),
		failed: make(chan struct{}),
	}
}

func (p *failOnceWatchEventProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	var fail bool
	p.once.Do(func() {
		fail = true
		close(p.failed)
	})
	if fail {
		return nil, errors.New("injected watch setup failure")
	}
	return p.Fake.Watch(ctx, afterSeq)
}

type failAgentTomlRenameOSFS struct {
	fsys.OSFS
	target string
}

func (f *failAgentTomlRenameOSFS) Rename(oldpath, newpath string) error {
	if canonicalTestPath(newpath) == canonicalTestPath(f.target) {
		return errors.New("injected agent.toml write failure")
	}
	return f.OSFS.Rename(oldpath, newpath)
}

func TestControllerStateReadAccess(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

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
	t.Setenv("GC_BEADS", "file")

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
	t.Setenv("GC_BEADS", "file")

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
	cs.markConfigMutationPending("current-rev")

	cs.updateFromRuntime(stale, runtime.NewFake(), "stale-rev")

	if got := cs.Config(); got != current {
		t.Fatalf("Config() = %+v, want pending mutation config with rig alpha", got)
	}
	if !cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker cleared by stale runtime update")
	}

	cs.updateFromRuntime(current, runtime.NewFake(), "current-rev")

	if cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker not cleared after matching runtime update")
	}
}

func TestControllerStateRuntimeUpdateDoesNotDropPendingMutationAgents(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	rigDir := t.TempDir()
	current := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigDir}},
		Agents: []config.Agent{
			{Name: "worker", Dir: "alpha", Provider: "bash"},
			{Name: "helper", Dir: "alpha", Provider: "bash"},
		},
	}
	stale := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigDir}},
		Agents:    []config.Agent{{Name: "worker", Dir: "alpha", Provider: "bash"}},
	}

	cs := newControllerState(context.Background(), current, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.markConfigMutationPending("current-rev")

	cs.updateFromRuntime(stale, runtime.NewFake(), "stale-rev")

	if got := cs.Config(); got != current {
		t.Fatalf("Config() = %+v, want pending mutation config with helper agent", got)
	}
	if !cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker cleared by stale runtime update")
	}

	cs.updateFromRuntime(current, runtime.NewFake(), "current-rev")

	if got := cs.Config(); got != current {
		t.Fatalf("Config() = %+v, want matching runtime config applied", got)
	}
	if cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker not cleared after matching runtime update")
	}
}

func TestControllerStateCreatedAgentVisibleAfterStaleRuntimeInterleaving(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	current := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Providers: map[string]config.ProviderSpec{
			"bash": {Command: "bash"},
		},
		Rigs:   []config.Rig{{Name: "alpha", Path: rigDir}},
		Agents: []config.Agent{{Name: "worker", Dir: "alpha", Provider: "bash"}},
	}
	content, err := current.Marshal()
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), content, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), current, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	if err := cs.CreateAgent(config.Agent{Name: "helper", Dir: "alpha", Provider: "bash"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	pendingRev := cs.pendingConfigRevision()
	if pendingRev == "" {
		t.Fatal("CreateAgent did not mark a pending config revision")
	}

	stale := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Providers: map[string]config.ProviderSpec{
			"bash": {Command: "bash"},
		},
		Rigs:   []config.Rig{{Name: "alpha", Path: rigDir}},
		Agents: []config.Agent{{Name: "worker", Dir: "alpha", Provider: "bash"}},
	}
	cs.updateFromRuntime(stale, runtime.NewFake(), pendingRev)
	if got := cs.Config(); configHasAgent(got, "alpha/helper") {
		t.Fatalf("stale runtime update did not hide alpha/helper; agents = %+v", got.Agents)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cs.WaitForAgentVisibility(ctx, "alpha/helper")
	}()

	select {
	case err := <-waitErr:
		t.Fatalf("WaitForAgentVisibility returned before fresh runtime update: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	fresh, freshRev, err := cs.loadCurrentConfigSnapshot()
	if err != nil {
		t.Fatalf("load fresh config snapshot: %v", err)
	}
	cs.updateFromRuntime(fresh, runtime.NewFake(), freshRev)

	if err := <-waitErr; err != nil {
		t.Fatalf("WaitForAgentVisibility after stale runtime update: %v", err)
	}
	got := cs.Config()
	if !configHasAgent(got, "alpha/helper") {
		t.Fatalf("agents after stale runtime update = %+v, want alpha/helper still visible", got.Agents)
	}
}

func configHasAgent(cfg *config.City, qualifiedName string) bool {
	if cfg == nil {
		return false
	}
	for _, agent := range cfg.Agents {
		if agent.QualifiedName() == qualifiedName {
			return true
		}
	}
	return false
}

func TestControllerStateRuntimeUpdateIgnoresEmptyRevisionDuringPendingMutation(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	rigDir := t.TempDir()
	current := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigDir}},
		Agents: []config.Agent{
			{Name: "worker", Dir: "alpha", Provider: "bash"},
			{Name: "helper", Dir: "alpha", Provider: "bash"},
		},
	}
	stale := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigDir}},
		Agents:    []config.Agent{{Name: "worker", Dir: "alpha", Provider: "bash"}},
	}

	cs := newControllerState(context.Background(), current, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.markConfigMutationPending("current-rev")

	cs.updateFromRuntime(stale, runtime.NewFake(), "")

	if got := cs.Config(); got != current {
		t.Fatalf("Config() = %+v, want pending mutation config with helper agent", got)
	}
	if !cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker cleared by empty-revision runtime update")
	}
}

func TestControllerStateRuntimeUpdateAcceptsBuiltinAwareRevision(t *testing.T) {
	configureTestDoltIdentityEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "")

	cityDir := shortSocketTempDir(t, "gc-state-runtime-builtin-")
	cleanupManagedDoltTestCity(t, cityDir)
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"+builtinImportsTOML("core", "bd")), 0o644); err != nil {
		t.Fatalf("write initial city.toml: %v", err)
	}
	writeBuiltinImportsLock(t, cityDir, "core", "bd")

	initial, err := tryReloadConfig(tomlPath, "test", cityDir)
	if err != nil {
		t.Fatalf("initial tryReloadConfig: %v", err)
	}
	applyRuntimeCityIdentity(initial.Cfg, "test")
	cs := newControllerState(context.Background(), initial.Cfg, runtime.NewFake(), events.NewFake(), "test", cityDir)

	rigDir := t.TempDir()
	updatedToml := fmt.Sprintf("[workspace]\nname = \"test\"\n\n[[rigs]]\nname = \"alpha\"\npath = %q\n", rigDir) + builtinImportsTOML("core", "bd")
	if err := os.WriteFile(tomlPath, []byte(updatedToml), 0o644); err != nil {
		t.Fatalf("write updated city.toml: %v", err)
	}
	reloaded, err := tryReloadConfig(tomlPath, "test", cityDir)
	if err != nil {
		t.Fatalf("reloaded tryReloadConfig: %v", err)
	}
	applyRuntimeCityIdentity(reloaded.Cfg, "test")

	cs.updateFromRuntime(reloaded.Cfg, runtime.NewFake(), reloaded.Revision)

	if got := cs.Config().Rigs; len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("runtime update was not accepted; rigs = %#v", got)
	}
	requireControllerStateOrder(t, cs, "gate-sweep")
}

func TestControllerStateMutationRefreshKeepsBuiltinOrdersAndClearsPending(t *testing.T) {
	configureTestDoltIdentityEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "")

	cityDir := shortSocketTempDir(t, "gc-state-mutation-builtin-")
	cleanupManagedDoltTestCity(t, cityDir)
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"+builtinImportsTOML("core", "bd")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeBuiltinImportsLock(t, cityDir, "core", "bd")

	initial, err := tryReloadConfig(tomlPath, "test", cityDir)
	if err != nil {
		t.Fatalf("tryReloadConfig: %v", err)
	}
	applyRuntimeCityIdentity(initial.Cfg, "test")
	cs := newControllerState(context.Background(), initial.Cfg, runtime.NewFake(), events.NewFake(), "test", cityDir)

	if err := cs.EnableOrder("gate-sweep", ""); err != nil {
		t.Fatalf("EnableOrder: %v", err)
	}
	requireControllerStateOrder(t, cs, "gate-sweep")
	if !cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker was not set")
	}

	reloaded, err := tryReloadConfig(tomlPath, "test", cityDir)
	if err != nil {
		t.Fatalf("tryReloadConfig after mutation: %v", err)
	}
	applyRuntimeCityIdentity(reloaded.Cfg, "test")
	cs.updateFromRuntime(reloaded.Cfg, runtime.NewFake(), reloaded.Revision)

	if cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker was not cleared by matching runtime update")
	}
	requireControllerStateOrder(t, cs, "gate-sweep")
}

func requireControllerStateOrder(t *testing.T, cs *controllerState, want string) {
	t.Helper()

	for _, order := range cs.Orders() {
		if order.Name == want {
			return
		}
	}
	t.Fatalf("Orders() missing %q", want)
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
	cs.markConfigMutationPending("next-rev")

	next := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   rigDir,
			Prefix: "al",
		}},
	}
	cs.updateFromRuntime(next, runtime.NewFake(), "next-rev")

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

func TestControllerStateRuntimeUpdatePreservesCurrentStoresWithoutPendingMutation(t *testing.T) {
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

	next := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   rigDir,
			Prefix: "al",
		}},
	}
	nextProvider := runtime.NewFake()
	cs.updateFromRuntime(next, nextProvider, "")

	if got := cs.BeadStore("alpha"); got != rigStore {
		t.Fatalf("BeadStore(alpha) = %T %p, want original store %T %p", got, got, rigStore, rigStore)
	}
	if got := cs.CityBeadStore(); got != cityStore {
		t.Fatalf("CityBeadStore() = %T %p, want original store %T %p", got, got, cityStore, cityStore)
	}
	if cs.Config() != next {
		t.Fatal("Config() was not advanced to runtime snapshot")
	}
	if cs.SessionProvider() != nextProvider {
		t.Fatal("SessionProvider() was not advanced to runtime provider")
	}
}

func TestControllerStateRuntimeUpdateRebuildsStoresWhenBackendMetadataChanges(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	writeBackendMetadata(t, cityDir, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)

	current := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Beads:     config.BeadsConfig{Provider: "file"},
	}
	oldStore := beads.NewMemStore()
	cs := &controllerState{
		cfg:                    current,
		sp:                     runtime.NewFake(),
		beadStores:             map[string]beads.Store{},
		cityBeadStore:          oldStore,
		cityName:               "city1",
		cityPath:               cityDir,
		storeMetadataSignature: storeMetadataSignature(cityDir, current),
	}
	oldSignature := cs.storeMetadataSignature

	if !cs.runtimeUpdateCanReuseCurrentStores(current) {
		t.Fatal("precondition: matching metadata should allow store reuse")
	}

	writeBackendMetadata(t, cityDir, `{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`)
	nextProvider := runtime.NewFake()
	cs.updateFromRuntime(current, nextProvider, "")

	if got := cs.CityBeadStore(); got == oldStore {
		t.Fatal("CityBeadStore() reused stale store after backend metadata changed")
	}
	if cs.SessionProvider() != nextProvider {
		t.Fatal("SessionProvider() was not advanced after metadata-triggered update")
	}
	if cs.storeMetadataSignature == "" || cs.storeMetadataSignature == oldSignature {
		t.Fatal("store metadata signature was not refreshed after backend metadata changed")
	}
}

func writeBackendMetadata(t *testing.T, scopeRoot, data string) {
	t.Helper()
	dir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(data+"\n"), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

func TestControllerStateRuntimeUpdateIgnoresStaleRevisionWithoutPendingMutation(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "alpha")
	cityToml := fmt.Sprintf(`[workspace]
name = "city1"

[beads]
provider = "file"

[[rigs]]
name = "alpha"
path = %q
prefix = "al"

[[agent]]
name = "worker"
dir = "alpha"
provider = "bash"
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	current := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   rigDir,
			Prefix: "al",
		}},
		Agents: []config.Agent{{Name: "worker", Dir: "alpha", Provider: "bash"}},
	}
	stale := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   rigDir,
			Prefix: "al",
		}},
	}
	originalProvider := runtime.NewFake()
	cs := newControllerState(context.Background(), current, originalProvider, events.NewFake(), "city1", cityDir)

	cs.updateFromRuntime(stale, runtime.NewFake(), "stale-rev")

	if got := cs.Config(); got != current {
		t.Fatalf("Config() = %+v, want current config with worker agent", got)
	}
	if cs.SessionProvider() != originalProvider {
		t.Fatal("SessionProvider() advanced for stale runtime update")
	}
	if cs.configMutationPending.Load() {
		t.Fatal("pending mutation marker set by stale runtime update")
	}
}

func TestControllerStateCreateRigPokesReconciler(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

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

func TestControllerStateCreateRigDetectsDefaultBranch(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)

	rigDir := newRepoWithOriginHead(t, "master")
	if err := cs.CreateRig(config.Rig{Name: "rig1", Path: rigDir}); err != nil {
		t.Fatalf("CreateRig: %v", err)
	}

	got := cs.Config()
	if got == nil || len(got.Rigs) != 1 {
		t.Fatalf("Config() rigs = %+v, want one rig", got.Rigs)
	}
	if got.Rigs[0].DefaultBranch != "master" {
		t.Fatalf("DefaultBranch = %q, want %q", got.Rigs[0].DefaultBranch, "master")
	}
}

func TestControllerStateCreateRigDetectsDefaultBranchForRelativePath(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cityRigDir := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(cityRigDir, 0o755); err != nil {
		t.Fatalf("mkdir city rig: %v", err)
	}
	gitCmd(t, cityRigDir, "init")
	gitCmd(t, cityRigDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")

	otherRoot := t.TempDir()
	otherRigDir := filepath.Join(otherRoot, "rig")
	if err := os.MkdirAll(otherRigDir, 0o755); err != nil {
		t.Fatalf("mkdir other rig: %v", err)
	}
	gitCmd(t, otherRigDir, "init")
	gitCmd(t, otherRigDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	t.Chdir(otherRoot)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)

	if err := cs.CreateRig(config.Rig{Name: "rig1", Path: "rig"}); err != nil {
		t.Fatalf("CreateRig: %v", err)
	}

	got := cs.Config()
	if got == nil || len(got.Rigs) != 1 {
		t.Fatalf("Config() rigs = %+v, want one rig", got.Rigs)
	}
	if got.Rigs[0].DefaultBranch != "trunk" {
		t.Fatalf("DefaultBranch = %q, want %q", got.Rigs[0].DefaultBranch, "trunk")
	}
}

func TestDetectRigDefaultBranchSkipsEmptyPath(t *testing.T) {
	got := detectRigDefaultBranch(t.TempDir(), config.Rig{Name: "rig1"})
	if got.DefaultBranch != "" {
		t.Fatalf("DefaultBranch = %q, want empty for empty rig path", got.DefaultBranch)
	}
}

func TestControllerStateCreateRigInitializesStoreBeforePublishing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

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

	original := []byte("[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n")
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, original, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.editor = configedit.NewEditor(&corruptCityAfterRenameFS{
		triggerPath: filepath.Join(agentDir, "agent.toml"),
		cityToml:    tomlPath,
	}, tomlPath)
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

func TestControllerStateMutationRestoresFullAgentScaffoldWhenRefreshFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	agentDir := filepath.Join(cityDir, "agents", "worker")
	if err := os.MkdirAll(filepath.Join(agentDir, "skills"), 0o755); err != nil {
		t.Fatalf("mkdir agent skills: %v", err)
	}
	for rel, data := range map[string]string{
		"agent.toml":         "provider = \"claude\"\n",
		"prompt.template.md": "You are the worker.\n",
		"skills/local.md":    "skill notes\n",
	} {
		if err := os.WriteFile(filepath.Join(agentDir, rel), []byte(data), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	original := []byte("[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n")
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, original, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Providers: map[string]config.ProviderSpec{
			"claude": config.BuiltinProviderAlias("claude"),
		},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.editor = configedit.NewEditor(&corruptCityAfterRemoveFS{
		triggerPath: agentDir,
		cityToml:    tomlPath,
	}, tomlPath)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}

	err := cs.DeleteAgent("worker")
	if err == nil {
		t.Fatal("DeleteAgent should fail when refreshing the updated snapshot fails")
	}
	if !strings.Contains(err.Error(), "refreshing updated city config") {
		t.Fatalf("DeleteAgent error = %v, want refresh failure after mutation", err)
	}

	for rel, want := range map[string]string{
		"agent.toml":         "provider = \"claude\"\n",
		"prompt.template.md": "You are the worker.\n",
		"skills/local.md":    "skill notes\n",
	} {
		got, readErr := os.ReadFile(filepath.Join(agentDir, rel))
		if readErr != nil {
			t.Fatalf("read restored %s: %v", rel, readErr)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want restored %q", rel, got, want)
		}
	}
	restored, readErr := os.ReadFile(tomlPath)
	if readErr != nil {
		t.Fatalf("read restored city.toml: %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("city.toml = %q, want rollback to %q", restored, original)
	}
	if cs.configDirty.Load() {
		t.Fatal("DeleteAgent should not mark config dirty after rollback")
	}
}

// TestControllerStateMutationRestoresSymlinkedCityTomlWhenRefreshFails proves
// the controller config-mutation rollback is symlink-aware, matching the CLI
// rollback snapshots. When a forward mutation writes through a city.toml
// symlink and the post-mutation config reload fails, restore must rewrite the
// real target file and leave the live city.toml symlink intact. Before the fix,
// captureConfigMutationSnapshot/restore operated on the unresolved link path,
// so rollback replaced the symlink with a regular file and left the
// forward-modified target un-reverted.
func TestControllerStateMutationRestoresSymlinkedCityTomlWhenRefreshFails(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}

	repoCityPath := filepath.Join(repoDir, "city.toml")
	liveCityPath := filepath.Join(cityDir, "city.toml")
	original := []byte("[workspace]\nname = \"city1\"\n")
	if err := os.WriteFile(repoCityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "repo", "city.toml"), liveCityPath); err != nil {
		t.Fatal(err)
	}

	cs := &controllerState{
		cityPath: cityDir,
		cfg:      &config.City{Workspace: config.Workspace{Name: "city1"}},
	}

	mutErr := cs.mutateAndPoke(func() error {
		// Forward mutation writes through the resolved symlink target, exactly
		// like the config editor's ResolveCityRewritePath path. The broken TOML
		// then makes refreshConfigSnapshot fail and triggers rollback -- the
		// same post-mutation refresh failure the production path hits.
		resolved, err := fsys.ResolveSymlinks(fsys.OSFS{}, liveCityPath)
		if err != nil {
			return err
		}
		return fsys.WriteFileAtomic(fsys.OSFS{}, resolved, []byte("["), 0o644)
	})
	if mutErr == nil {
		t.Fatal("mutateAndPoke should fail when refreshing the post-mutation config fails")
	}
	if !strings.Contains(mutErr.Error(), "refreshing updated city config") {
		t.Fatalf("mutateAndPoke error = %v, want refresh failure after mutation", mutErr)
	}

	info, err := os.Lstat(liveCityPath)
	if err != nil {
		t.Fatalf("Lstat(live city.toml): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("rollback replaced the city.toml symlink with a regular file")
	}
	restored, readErr := os.ReadFile(repoCityPath)
	if readErr != nil {
		t.Fatalf("read repo city.toml: %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("repo city.toml = %q, want restored original %q", restored, original)
	}
}

func TestControllerStateMutationAllowsSymlinkedAgentAssets(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Providers: map[string]config.ProviderSpec{
			"codex-local": {Command: "codex"},
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

	agentDir := filepath.Join(cityDir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("provider = \"codex-local\"\n"), 0o644); err != nil {
		t.Fatalf("write agent.toml: %v", err)
	}
	sharedSkills := filepath.Join(cityDir, "shared-skills")
	if err := os.MkdirAll(sharedSkills, 0o755); err != nil {
		t.Fatalf("mkdir shared skills: %v", err)
	}
	skillsLink := filepath.Join(agentDir, "skills")
	if err := os.Symlink(sharedSkills, skillsLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}

	if err := cs.UpdateProvider("codex-local", api.ProviderUpdate{Command: stringPtr("codex-wrapper")}); err != nil {
		t.Fatalf("UpdateProvider with symlinked agent skills: %v", err)
	}

	info, err := os.Lstat(skillsLink)
	if err != nil {
		t.Fatalf("lstat skills symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("skills path mode = %v, want symlink preserved", info.Mode())
	}
	if target, err := os.Readlink(skillsLink); err != nil || target != sharedSkills {
		t.Fatalf("skills symlink target = %q, %v; want %q", target, err, sharedSkills)
	}
	got := cs.Config()
	if got == nil {
		t.Fatal("Config() = nil after UpdateProvider")
	}
	if got.Providers["codex-local"].Command != "codex-wrapper" {
		t.Fatalf("provider after UpdateProvider = %+v, want command update", got.Providers["codex-local"])
	}
}

func TestControllerStateSchema2CreateThenUpdateConventionAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[providers.codex]\nbase = \"builtin:codex\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Providers: map[string]config.ProviderSpec{
			"claude": config.BuiltinProviderAlias("claude"),
			"codex":  config.BuiltinProviderAlias("codex"),
		},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 2)
	cs.configDirty = &atomic.Bool{}

	if err := cs.CreateAgent(config.Agent{Name: "helper", Provider: "claude", Scope: "city"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := cs.UpdateAgent("helper", api.AgentUpdate{
		Provider:  "codex",
		Scope:     "city",
		Suspended: boolPtr(true),
	}); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if strings.Contains(string(raw), "[[agent]]") {
		t.Fatalf("city.toml = %q, want convention agent stored outside city.toml", raw)
	}
	data, err := os.ReadFile(filepath.Join(cityDir, "agents", "helper", "agent.toml"))
	if err != nil {
		t.Fatalf("read agent.toml: %v", err)
	}
	for _, want := range []string{
		`provider = "codex"`,
		`scope = "city"`,
		`suspended = true`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("agent.toml = %q, want %s", data, want)
		}
	}
	for _, agent := range cs.Config().Agents {
		if agent.Name == "helper" {
			if agent.Provider != "codex" || agent.Scope != "city" || !agent.Suspended {
				t.Fatalf("agent = %+v, want updated provider/scope/suspended", agent)
			}
			return
		}
	}
	t.Fatalf("Config() agents = %+v, want helper", cs.Config().Agents)
}

func TestControllerStateSchema2CreateRollsBackFreshConventionScaffoldWhenAgentTOMLWriteFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Providers: map[string]config.ProviderSpec{
			"claude": config.BuiltinProviderAlias("claude"),
		},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	agentDir := filepath.Join(cityDir, "agents", "helper")
	cs.editor = configedit.NewEditor(&failAgentTomlRenameOSFS{target: filepath.Join(agentDir, "agent.toml")}, tomlPath)
	cs.pokeCh = make(chan struct{}, 2)
	cs.configDirty = &atomic.Bool{}

	err := cs.CreateAgent(config.Agent{Name: "helper", Provider: "claude", Scope: "city"})
	if err == nil {
		t.Fatal("CreateAgent succeeded, want injected agent.toml write failure")
	}
	if _, statErr := os.Stat(agentDir); !os.IsNotExist(statErr) {
		t.Fatalf("agent dir stat err = %v, want fresh scaffold removed", statErr)
	}
	cfg, _, loadErr := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if loadErr != nil {
		t.Fatalf("LoadWithIncludes: %v", loadErr)
	}
	for _, agent := range cfg.Agents {
		if agent.Name == "helper" {
			t.Fatalf("expanded agents include ghost helper after failed create: %+v", agent)
		}
	}
	if cs.configDirty.Load() {
		t.Fatal("CreateAgent should not mark config dirty after failed agent.toml write")
	}
}

func TestControllerStateSchema2CreateRejectsSymlinkedConventionScaffoldPath(t *testing.T) {
	for _, tc := range []struct {
		name             string
		setup            func(t *testing.T, cityDir string) string
		outsideWritePath string
	}{
		{
			name: "agents root",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				outsideAgentsDir := filepath.Join(t.TempDir(), "agents")
				if err := os.MkdirAll(outsideAgentsDir, 0o755); err != nil {
					t.Fatalf("mkdir outside agents: %v", err)
				}
				agentsLink := filepath.Join(cityDir, "agents")
				if err := os.Symlink(outsideAgentsDir, agentsLink); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return agentsLink
			},
			outsideWritePath: filepath.Join("agents", "helper"),
		},
		{
			name: "agent dir",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				agentsDir := filepath.Join(cityDir, "agents")
				if err := os.MkdirAll(agentsDir, 0o755); err != nil {
					t.Fatalf("mkdir agents: %v", err)
				}
				outsideAgentDir := filepath.Join(t.TempDir(), "helper")
				if err := os.MkdirAll(outsideAgentDir, 0o755); err != nil {
					t.Fatalf("mkdir outside agent: %v", err)
				}
				agentLink := filepath.Join(agentsDir, "helper")
				if err := os.Symlink(outsideAgentDir, agentLink); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return agentLink
			},
			outsideWritePath: "helper",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")

			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
				t.Fatalf("write pack.toml: %v", err)
			}
			tomlPath := filepath.Join(cityDir, "city.toml")
			if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"city1\"\n"), 0o644); err != nil {
				t.Fatalf("write city.toml: %v", err)
			}
			linkPath := tc.setup(t, cityDir)
			outsidePath := filepath.Join(filepath.Dir(linkPath), tc.outsideWritePath)

			cs := newControllerState(context.Background(), &config.City{
				Workspace: config.Workspace{Name: "city1"},
			}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
			cs.pokeCh = make(chan struct{}, 1)
			cs.configDirty = &atomic.Bool{}

			err := cs.CreateAgent(config.Agent{Name: "helper", Provider: "claude", Scope: "city"})
			if !errors.Is(err, configedit.ErrValidation) {
				t.Fatalf("CreateAgent error = %v, want ErrValidation", err)
			}
			for _, path := range []string{
				filepath.Join(outsidePath, "agent.toml"),
				filepath.Join(outsidePath, "prompt.template.md"),
			} {
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatalf("%s stat err = %v, want no write through symlink", path, statErr)
				}
			}
			info, statErr := os.Lstat(linkPath)
			if statErr != nil {
				t.Fatalf("lstat symlink: %v", statErr)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("link mode = %v, want symlink preserved", info.Mode())
			}
			if cs.configDirty.Load() {
				t.Fatal("CreateAgent should not mark config dirty after symlink rejection")
			}
		})
	}
}

func TestControllerStateSchema2RejectsRigScopeConventionAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Providers: map[string]config.ProviderSpec{
			"claude": config.BuiltinProviderAlias("claude"),
		},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)

	if err := cs.CreateAgent(config.Agent{Name: "helper", Provider: "claude", Scope: "rig"}); !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("CreateAgent error = %v, want ErrValidation", err)
	}
	if err := cs.CreateAgent(config.Agent{Name: "helper", Provider: "claude", Scope: "city"}); err != nil {
		t.Fatalf("CreateAgent city-scoped helper: %v", err)
	}
	if err := cs.UpdateAgent("helper", api.AgentUpdate{Scope: "rig"}); !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("UpdateAgent error = %v, want ErrValidation", err)
	}
	data, err := os.ReadFile(filepath.Join(cityDir, "agents", "helper", "agent.toml"))
	if err != nil {
		t.Fatalf("read agent.toml: %v", err)
	}
	if strings.Contains(string(data), `scope = "rig"`) {
		t.Fatalf("agent.toml persisted rejected rig scope:\n%s", data)
	}
}

func TestControllerStateSchema2CreateThenDeleteConventionAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Providers: map[string]config.ProviderSpec{
			"claude": config.BuiltinProviderAlias("claude"),
		},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 2)
	cs.configDirty = &atomic.Bool{}

	if err := cs.CreateAgent(config.Agent{Name: "helper", Provider: "claude", Scope: "city"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := cs.DeleteAgent("helper"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityDir, "agents", "helper", "agent.toml")); !os.IsNotExist(err) {
		t.Fatalf("agent.toml stat err = %v, want removed file", err)
	}
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if strings.Contains(string(raw), "[[agent]]") {
		t.Fatalf("city.toml = %q, want no inline helper", raw)
	}
	for _, agent := range cs.Config().Agents {
		if agent.Name == "helper" {
			t.Fatalf("Config() agents still include helper: %+v", agent)
		}
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

func TestWrapWithCachingStoreCachesNonBdStore(t *testing.T) {
	backing := beads.NewMemStore()
	created, err := backing.Create(beads.Bead{Title: "non-bd backing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	store := wrapWithCachingStore(context.Background(), backing, nil, true)
	cached, ok := store.(*beads.CachingStore)
	if !ok {
		t.Fatalf("store type = %T, want *beads.CachingStore", store)
	}
	if cached.Backing() != backing {
		t.Fatalf("Backing = %#v, want original non-BdStore backing", cached.Backing())
	}

	items, err := cached.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("ListOpen = %#v, want only %s", items, created.ID)
	}
}

func TestWrapWithCachingStoreReturnsNilStore(t *testing.T) {
	if got := wrapWithCachingStore(context.Background(), nil, nil, true); got != nil {
		t.Fatalf("wrapWithCachingStore(nil) = %#v, want nil", got)
	}
}

// TestWrapWithCachingStoreNoBackgroundRefresh covers the suspended-rig path:
// with backgroundRefresh=false the cache still serves pre-primed reads but does
// NOT start the reconcile loop (StaggerOffsetMs stays 0), so a suspended rig
// stops costing a bd subprocess per reconcile cycle.
func TestWrapWithCachingStoreNoBackgroundRefresh(t *testing.T) {
	backing := beads.NewMemStore()
	created, err := backing.Create(beads.Bead{Title: "suspended-rig bead"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Cancellable ctx so the refresh path is reachable (Background() always
	// early-returns regardless of the flag).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := wrapWithCachingStore(ctx, backing, nil, false)
	cached, ok := store.(*beads.CachingStore)
	if !ok {
		t.Fatalf("store type = %T, want *beads.CachingStore", store)
	}
	// Pre-primed reads still work (on-demand access to a suspended rig).
	items, err := cached.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("ListOpen = %#v, want only %s", items, created.ID)
	}
	// Reconciler never armed: StartReconciler (which sets StaggerOffsetMs) was
	// not called. Give any erroneously-spawned goroutine a moment to set it.
	time.Sleep(50 * time.Millisecond)
	if got := cached.Stats().StaggerOffsetMs; got != 0 {
		t.Fatalf("StaggerOffsetMs = %d, want 0 (reconciler must not start when backgroundRefresh=false)", got)
	}
}

type closeStoreSpy struct {
	beads.Store
	closed   atomic.Int32
	closeErr error
}

func (s *closeStoreSpy) CloseStore() error {
	s.closed.Add(1)
	return s.closeErr
}

func (s *closeStoreSpy) Get(id string) (beads.Bead, error) {
	if s.closeCount() > 0 {
		return beads.Bead{}, fmt.Errorf("closeStoreSpy: %w", beads.ErrStoreClosed)
	}
	return s.Store.Get(id)
}

func (s *closeStoreSpy) closeCount() int {
	return int(s.closed.Load())
}

func setControllerStateStoreCloseDelayForTest(t *testing.T, delay time.Duration) {
	t.Helper()
	prev := controllerStateStoreCloseDelay
	controllerStateStoreCloseDelay = delay
	t.Cleanup(func() { controllerStateStoreCloseDelay = prev })
}

func waitForCloseStoreSpy(t *testing.T, store *closeStoreSpy) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := store.closeCount(); got == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("CloseStore calls = %d, want 1", store.closeCount())
}

func TestControllerStateUpdateClosesReplacedCityStore(t *testing.T) {
	prevOpen := newControllerStateOpenCityStore
	t.Cleanup(func() { newControllerStateOpenCityStore = prevOpen })
	setControllerStateStoreCloseDelayForTest(t, time.Millisecond)

	replacement := beads.NewMemStore()
	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: replacement}, nil
	}
	oldStore := &closeStoreSpy{Store: beads.NewMemStore()}
	cs := &controllerState{
		cfg:           &config.City{},
		cityPath:      t.TempDir(),
		cityBeadStore: oldStore,
		beadStores:    map[string]beads.Store{},
	}

	cs.update(&config.City{}, runtime.NewFake())

	if cs.CityBeadStore() == oldStore {
		t.Fatal("city bead store was not replaced")
	}
	waitForCloseStoreSpy(t, oldStore)
}

func TestControllerStateUpdateClosesReplacedRigStores(t *testing.T) {
	prevOpen := newControllerStateOpenCityStore
	t.Cleanup(func() { newControllerStateOpenCityStore = prevOpen })
	setControllerStateStoreCloseDelayForTest(t, time.Millisecond)

	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, nil
	}
	oldStore := &closeStoreSpy{Store: beads.NewMemStore()}
	cs := &controllerState{
		cfg:        &config.City{},
		cityPath:   t.TempDir(),
		beadStores: map[string]beads.Store{"frontend": oldStore},
	}

	cs.update(&config.City{}, runtime.NewFake())

	if _, ok := cs.BeadStores()["frontend"]; ok {
		t.Fatal("frontend rig store was not replaced")
	}
	waitForCloseStoreSpy(t, oldStore)
}

func TestCloseBeadStoreHandleUnwrapsPolicyWrappedCachingStore(t *testing.T) {
	backing := &closeStoreSpy{Store: beads.NewMemStore()}
	cache := beads.NewCachingStore(backing, nil)
	wrapped := wrapStoreWithBeadPolicies(cache, &config.City{})

	if err := closeBeadStoreHandle(wrapped); err != nil {
		t.Fatalf("closeBeadStoreHandle: %v", err)
	}
	if backing.closeCount() != 1 {
		t.Fatalf("backing CloseStore calls = %d, want 1", backing.closeCount())
	}
}

func TestControllerStateUpdateKeepsStaleRigStoreUsableDuringReload(t *testing.T) {
	prevOpen := newControllerStateOpenCityStore
	t.Cleanup(func() { newControllerStateOpenCityStore = prevOpen })
	setControllerStateStoreCloseDelayForTest(t, 200*time.Millisecond)

	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, nil
	}
	oldStore := &closeStoreSpy{Store: beads.NewMemStore()}
	created, err := oldStore.Create(beads.Bead{Title: "in-flight"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := &controllerState{
		cfg:        &config.City{},
		cityPath:   t.TempDir(),
		beadStores: map[string]beads.Store{"frontend": oldStore},
	}

	stale := cs.BeadStore("frontend")
	cs.update(&config.City{}, runtime.NewFake())

	got, err := stale.Get(created.ID)
	if err != nil {
		t.Fatalf("stale store Get after reload returned %v; want old handle usable during drain", err)
	}
	if got.ID != created.ID {
		t.Fatalf("stale store Get ID = %q, want %q", got.ID, created.ID)
	}
	waitForCloseStoreSpy(t, oldStore)
}

func TestControllerStateUpdateReturnsTypedStoreClosedAfterReloadDrain(t *testing.T) {
	prevOpen := newControllerStateOpenCityStore
	t.Cleanup(func() { newControllerStateOpenCityStore = prevOpen })
	setControllerStateStoreCloseDelayForTest(t, time.Millisecond)

	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, nil
	}
	oldStore := &closeStoreSpy{Store: beads.NewMemStore()}
	created, err := oldStore.Create(beads.Bead{Title: "in-flight"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := &controllerState{
		cfg:        &config.City{},
		cityPath:   t.TempDir(),
		beadStores: map[string]beads.Store{"frontend": oldStore},
	}

	stale := cs.BeadStore("frontend")
	cs.update(&config.City{}, runtime.NewFake())
	waitForCloseStoreSpy(t, oldStore)

	if _, err := stale.Get(created.ID); !errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("stale store Get after reload drain returned %v, want ErrStoreClosed", err)
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

func TestControllerStateAppliesBeadEventsOnlyToOwningCache(t *testing.T) {
	cityBacking := beads.NewMemStore()
	rigBacking := beads.NewMemStore()
	cityStore := beads.NewCachingStoreForTest(cityBacking, nil)
	rigStore := beads.NewCachingStoreForTest(rigBacking, nil)
	if err := cityStore.Prime(context.Background()); err != nil {
		t.Fatalf("city Prime: %v", err)
	}
	if err := rigStore.Prime(context.Background()); err != nil {
		t.Fatalf("rig Prime: %v", err)
	}

	cs := &controllerState{
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city", Prefix: "ct"},
			Rigs:      []config.Rig{{Name: "rig1", Prefix: "rw"}},
		},
		cityName:      "test-city",
		cityBeadStore: cityStore,
		beadStores:    map[string]beads.Store{"rig1": rigStore},
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadCreated,
		Subject: "rw-1",
		Payload: json.RawMessage(`{"id":"rw-1","title":"rig bead","status":"open","issue_type":"task","created_at":"2026-04-26T21:37:46Z"}`),
	})

	if _, err := cityStore.Get("rw-1"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("city cache Get(rw-1) error = %v, want ErrNotFound", err)
	}
	if got, err := rigStore.Get("rw-1"); err != nil {
		t.Fatalf("rig cache Get(rw-1): %v", err)
	} else if got.Title != "rig bead" {
		t.Fatalf("rig cache title = %q, want rig bead", got.Title)
	}
}

func TestControllerStateAppliesHyphenatedPrefixEventsOnlyToOwningCache(t *testing.T) {
	cityStore := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	rigStore := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	if err := cityStore.Prime(context.Background()); err != nil {
		t.Fatalf("city Prime: %v", err)
	}
	if err := rigStore.Prime(context.Background()); err != nil {
		t.Fatalf("rig Prime: %v", err)
	}

	cs := &controllerState{
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city", Prefix: "mlcm"},
			Rigs:      []config.Rig{{Name: "rig1", Prefix: "mc-mogbzvrs"}},
		},
		cityName:      "test-city",
		cityBeadStore: cityStore,
		beadStores:    map[string]beads.Store{"rig1": rigStore},
	}

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadCreated,
		Subject: "mc-mogbzvrs-hiv.1",
		Payload: json.RawMessage(`{"id":"mc-mogbzvrs-hiv.1","title":"rig bead","status":"open","issue_type":"task","created_at":"2026-04-26T21:37:46Z"}`),
	})

	if _, err := cityStore.Get("mc-mogbzvrs-hiv.1"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("city cache Get(hyphenated rig bead) error = %v, want ErrNotFound", err)
	}
	if got, err := rigStore.Get("mc-mogbzvrs-hiv.1"); err != nil {
		t.Fatalf("rig cache Get(hyphenated rig bead): %v", err)
	} else if got.Title != "rig bead" {
		t.Fatalf("rig cache title = %q, want rig bead", got.Title)
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
	clearGCEnv(t)
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

func TestControllerStateBuildStoresRoutesBdRigThroughStoreFactory(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	prevOpen := controllerStateOpenRigStoreAtForCity
	t.Cleanup(func() { controllerStateOpenRigStoreAtForCity = prevOpen })

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
	nativeBacking := beads.NewMemStore()
	factoryCalled := false
	controllerStateOpenRigStoreAtForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		factoryCalled = true
		if opts.ScopeRoot != rigDir {
			t.Fatalf("factory ScopeRoot = %q, want %q", opts.ScopeRoot, rigDir)
		}
		if opts.CityPath != cityDir {
			t.Fatalf("factory CityPath = %q, want %q", opts.CityPath, cityDir)
		}
		if opts.Provider != "bd" {
			t.Fatalf("factory Provider = %q, want bd", opts.Provider)
		}
		if opts.OpenBdStore == nil {
			t.Fatal("factory OpenBdStore is nil")
		}
		return beads.StoreOpenResult{
			Store: nativeBacking,
			Diagnostic: beads.BeadsDiagnostic{
				Store:               "NativeDoltStore",
				NativeStoreEligible: true,
			},
		}, nil
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

	if !factoryCalled {
		t.Fatal("buildStores did not route bd-backed rig through store factory")
	}
	frontendStore := underlyingPolicyStoreForTest(stores["frontend"])
	cached, ok := frontendStore.(*beads.CachingStore)
	if !ok {
		t.Fatalf("frontend store = %T, want caching store", frontendStore)
	}
	if cached.Backing() != nativeBacking {
		t.Fatalf("frontend backing = %T, want native factory backing", cached.Backing())
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
	t.Setenv("GC_BEADS", "file")

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
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	autoDir := filepath.Join(cityDir, "orders")
	if err := os.MkdirAll(autoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(autoDir, "digest.toml"), []byte(`
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
		verify  func(t *testing.T, cfg *config.City, cityDir string)
	}{
		{
			name: "suspend agent",
			mutate: func(cs *controllerState) error {
				return cs.SuspendAgent("rig1/worker")
			},
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, cityDir string) {
				t.Helper()
				if cfg.Rigs[0].Suspended {
					t.Fatal("city.toml should not have suspended=true after SuspendRig")
				}
				st, err := suspensionstate.Load(fsys.OSFS{}, cityDir)
				if err != nil {
					t.Fatalf("load rig state: %v", err)
				}
				if !suspensionstate.IsRigSuspended(st, "rig1") {
					t.Fatal("rig should be suspended in runtime state after SuspendRig")
				}
			},
		},
		{
			name: "resume rig",
			initial: func(cfg *config.City) {
				cfg.Rigs[0].SuspendedOnStart = true
			},
			mutate: func(cs *controllerState) error {
				return cs.ResumeRig("rig1")
			},
			verify: func(t *testing.T, cfg *config.City, cityDir string) {
				t.Helper()
				// city.toml stays untouched; the explicit resume is
				// recorded in runtime state.
				if !cfg.Rigs[0].SuspendedOnStart {
					t.Fatal("suspended_on_start should remain set in city.toml; ResumeRig records the override in runtime state")
				}
				st, err := suspensionstate.Load(fsys.OSFS{}, cityDir)
				if err != nil {
					t.Fatalf("load rig state: %v", err)
				}
				if v, ok := suspensionstate.ExplicitRig(st, "rig1"); !ok || v {
					t.Fatalf("rig should have explicit resume in runtime state; got (%v, %v)", v, ok)
				}
			},
		},
		{
			name: "suspend city",
			mutate: func(cs *controllerState) error {
				return cs.SuspendCity()
			},
			verify: func(t *testing.T, cfg *config.City, cityDir string) {
				t.Helper()
				if cfg.Workspace.Suspended || cfg.Workspace.SuspendedOnStart {
					t.Fatal("city.toml workspace must remain untouched by SuspendCity (runtime state owns the change)")
				}
				st, err := suspensionstate.Load(fsys.OSFS{}, cityDir)
				if err != nil {
					t.Fatalf("load city state: %v", err)
				}
				if !suspensionstate.IsCitySuspended(st) {
					t.Fatal("city should be explicit-suspended in runtime state after SuspendCity")
				}
			},
		},
		{
			name: "resume city",
			initial: func(cfg *config.City) {
				cfg.Workspace.SuspendedOnStart = true
			},
			mutate: func(cs *controllerState) error {
				return cs.ResumeCity()
			},
			verify: func(t *testing.T, cfg *config.City, cityDir string) {
				t.Helper()
				if !cfg.Workspace.SuspendedOnStart {
					t.Fatal("suspended_on_start should remain set in city.toml; ResumeCity records the override in runtime state")
				}
				st, err := suspensionstate.Load(fsys.OSFS{}, cityDir)
				if err != nil {
					t.Fatalf("load city state: %v", err)
				}
				if v, ok := suspensionstate.ExplicitCity(st); !ok || v {
					t.Fatalf("city should have explicit resume in runtime state; got (%v, %v)", v, ok)
				}
			},
		},
		{
			name: "enable order",
			mutate: func(cs *controllerState) error {
				return cs.EnableOrder("nightly", "rig1")
			},
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
				t.Helper()
				// patch.Suspended is the back-compat alias that writes
				// the rig's committable SuspendedOnStart default; the
				// deprecated `suspended` field stays unset.
				if cfg.Rigs[0].Prefix != "rg" || !cfg.Rigs[0].SuspendedOnStart {
					t.Fatalf("updated rig = %+v, want prefix=rg + suspended_on_start=true", cfg.Rigs[0])
				}
				if cfg.Rigs[0].Suspended {
					t.Fatalf("legacy suspended field must not be written by RigUpdate; got %+v", cfg.Rigs[0])
				}
			},
		},
		{
			name: "delete rig",
			mutate: func(cs *controllerState) error {
				return cs.DeleteRig("rig1")
			},
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			verify: func(t *testing.T, cfg *config.City, _ string) {
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
			tc.verify(t, got, filepath.Dir(tomlPath))
		})
	}
}

func TestControllerStateCitySuspensionRecordsEvents(t *testing.T) {
	cases := []struct {
		name          string
		initial       func(*config.City)
		mutate        func(*controllerState) error
		wantSuspended bool
		wantEventType string
		wantActor     string
	}{
		{
			name: "suspend city",
			mutate: func(cs *controllerState) error {
				return cs.SuspendCity()
			},
			wantSuspended: true,
			wantEventType: events.CitySuspended,
			wantActor:     "gc",
		},
		{
			name: "resume city",
			initial: func(cfg *config.City) {
				cfg.Workspace.SuspendedOnStart = true
			},
			mutate: func(cs *controllerState) error {
				return cs.ResumeCity()
			},
			wantEventType: events.CityResumed,
			wantActor:     "gc",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs, tomlPath := newControllerStateMutationHarness(t)
			ep := events.NewFake()
			cs.eventProv = ep

			if tc.initial != nil {
				cfg, err := config.Load(fsys.OSFS{}, tomlPath)
				if err != nil {
					t.Fatalf("load config: %v", err)
				}
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

			// Suspend/resume record the change in runtime state, not
			// committed config: city.toml's workspace must stay
			// untouched and the explicit preference lands in
			// .gc/runtime/suspension-state.json.
			gotCfg, err := config.Load(fsys.OSFS{}, tomlPath)
			if err != nil {
				t.Fatalf("reload config: %v", err)
			}
			if gotCfg.Workspace.Suspended {
				t.Fatalf("city.toml workspace.suspended must remain unset, got %+v", gotCfg.Workspace)
			}
			st, err := suspensionstate.Load(fsys.OSFS{}, filepath.Dir(tomlPath))
			if err != nil {
				t.Fatalf("load suspension state: %v", err)
			}
			if v, ok := suspensionstate.ExplicitCity(st); !ok || v != tc.wantSuspended {
				t.Fatalf("runtime state ExplicitCity = (%v, %v), want (%v, true)", v, ok, tc.wantSuspended)
			}

			gotEvents, err := ep.List(events.Filter{})
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			if len(gotEvents) != 1 {
				t.Fatalf("recorded events = %+v, want exactly one %s event", gotEvents, tc.wantEventType)
			}
			if gotEvents[0].Type != tc.wantEventType {
				t.Fatalf("recorded event type = %q, want %q", gotEvents[0].Type, tc.wantEventType)
			}
			if gotEvents[0].Actor != tc.wantActor {
				t.Fatalf("recorded event actor = %q, want %q", gotEvents[0].Actor, tc.wantActor)
			}
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

func TestControllerStateEstablishesBeadEventCursorBeforePrimingStores(t *testing.T) {
	ep := newBlockingLatestEventProvider()
	var storeOpened atomic.Bool
	prevCityStore := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		storeOpened.Store(true)
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() {
		newControllerStateOpenCityStore = prevCityStore
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		_ = newControllerState(ctx, &config.City{Workspace: config.Workspace{Name: "test-city"}}, runtime.NewFake(), ep, "test-city", t.TempDir())
		close(returned)
	}()

	select {
	case <-ep.latestCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("event watcher did not establish an initial cursor")
	}
	select {
	case <-returned:
		t.Fatal("newControllerState returned before the initial event cursor was established")
	default:
	}
	if storeOpened.Load() {
		t.Fatal("controller opened stores before establishing the initial event cursor")
	}

	close(ep.allowLatest)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("newControllerState did not return after the initial event cursor was established")
	}
}

func TestControllerStateBeadEventWatcherReplaysEventsAfterCachePrime(t *testing.T) {
	backing := beads.NewMemStore()
	prevCityStore := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: backing}, nil
	}
	t.Cleanup(func() {
		newControllerStateOpenCityStore = prevCityStore
	})

	ep := events.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := newControllerState(ctx, &config.City{Workspace: config.Workspace{Name: "test-city"}}, runtime.NewFake(), ep, "test-city", t.TempDir())
	cs.pokeCh = make(chan struct{}, 1)

	created, err := backing.Create(beads.Bead{
		Title: "queued work",
		Type:  "task",
		Metadata: map[string]string{
			"gc.routed_to": "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create backing bead: %v", err)
	}
	payload, err := json.Marshal(map[string]beads.Bead{"bead": created})
	if err != nil {
		t.Fatalf("marshal bead event: %v", err)
	}
	ep.Record(events.Event{
		Type:    events.BeadCreated,
		Actor:   "bd-hook",
		Subject: created.ID,
		Payload: payload,
	})
	cs.startBeadEventWatcher(ctx)

	select {
	case <-cs.pokeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("bead event written after watcher start did not poke controller")
	}

	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "claude",
		store:    cs.cityBeadStore,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts["claude"]; got != 1 {
		t.Fatalf("defaultScaleCheckCounts[claude] = %d, want 1", got)
	}
}

func TestControllerStateBeadEventWatcherRetriesSetupErrors(t *testing.T) {
	backing := beads.NewMemStore()
	prevCityStore := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: backing}, nil
	}
	t.Cleanup(func() {
		newControllerStateOpenCityStore = prevCityStore
	})

	prevRetryDelay := beadEventWatcherRetryDelay
	beadEventWatcherRetryDelay = time.Millisecond
	t.Cleanup(func() {
		beadEventWatcherRetryDelay = prevRetryDelay
	})

	ep := newFailOnceWatchEventProvider()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := newControllerState(ctx, &config.City{Workspace: config.Workspace{Name: "test-city"}}, runtime.NewFake(), ep, "test-city", t.TempDir())
	cs.pokeCh = make(chan struct{}, 1)
	cs.startBeadEventWatcher(ctx)

	select {
	case <-ep.failed:
	case <-time.After(5 * time.Second):
		t.Fatal("bead event watcher did not attempt initial watch")
	}

	created, err := backing.Create(beads.Bead{Title: "queued work", Type: "task"})
	if err != nil {
		t.Fatalf("Create backing bead: %v", err)
	}
	payload, err := json.Marshal(map[string]beads.Bead{"bead": created})
	if err != nil {
		t.Fatalf("marshal bead event: %v", err)
	}
	ep.Record(events.Event{
		Type:    events.BeadCreated,
		Actor:   "bd-hook",
		Subject: created.ID,
		Payload: payload,
	})

	select {
	case <-cs.pokeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("bead event watcher did not recover after setup watch error")
	}
}

func TestControllerStateBeadEventWatcherConsumesExternalFileEvent(t *testing.T) {
	backing := beads.NewMemStore()
	prevCityStore := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: backing}, nil
	}
	t.Cleanup(func() {
		newControllerStateOpenCityStore = prevCityStore
	})

	eventPath := filepath.Join(t.TempDir(), "events.jsonl")
	watchRecorder, err := events.NewFileRecorder(eventPath, io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder(watcher): %v", err)
	}
	t.Cleanup(func() {
		if err := watchRecorder.Close(); err != nil {
			t.Fatalf("Close(watcher): %v", err)
		}
	})
	writeRecorder, err := events.NewFileRecorder(eventPath, io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder(writer): %v", err)
	}
	t.Cleanup(func() {
		if err := writeRecorder.Close(); err != nil {
			t.Fatalf("Close(writer): %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	created, err := backing.Create(beads.Bead{Title: "queued work", Type: "task"})
	if err != nil {
		t.Fatalf("Create backing bead: %v", err)
	}
	cs := newControllerState(ctx, &config.City{Workspace: config.Workspace{Name: "test-city"}}, runtime.NewFake(), watchRecorder, "test-city", t.TempDir())
	cs.pokeCh = make(chan struct{}, 1)
	cs.startBeadEventWatcher(ctx)

	if err := backing.SetMetadata(created.ID, "gc.routed_to", "claude"); err != nil {
		t.Fatalf("SetMetadata backing bead: %v", err)
	}
	fresh, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get backing bead: %v", err)
	}
	payload, err := json.Marshal(map[string]beads.Bead{"bead": fresh})
	if err != nil {
		t.Fatalf("marshal bead event: %v", err)
	}
	writeRecorder.Record(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "bd-hook",
		Subject: created.ID,
		Payload: payload,
	})

	select {
	case <-cs.pokeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("external file bead event did not poke controller")
	}

	// This test's contract is that the watcher consumes the external file event
	// and pokes the controller (asserted above). Demand-count behavior after an
	// incremental cache apply is not asserted here: under the cache-only demand
	// read model it depends on the store shape (an unprimed *CachingStore reports
	// a partial, while a logical store is served directly), so it is covered by
	// the dedicated defaultScaleCheckCounts tests instead.
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

// fullScanFailingStore fails full-scan List calls (the async full-prime
// path) while letting status-filtered List calls (PrimeActive) through,
// modeling a backing store whose full prime fails at controller startup.
type fullScanFailingStore struct {
	beads.Store
}

func (s *fullScanFailingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.AllowScan {
		return nil, fmt.Errorf("full scan unavailable")
	}
	return s.Store.List(query)
}

// TestPrimeThenStartReconcilerArmsReconcilerOnPrimeFailure asserts the
// watchdog reconciler is started even when the async full prime fails.
// Before this contract, a single failed prime at controller startup
// permanently disabled reconciliation for that store: the cache served
// its PrimeActive-era snapshot for the life of the supervisor, kept
// fresh only by event-bus writes, so storage-level state created before
// the restart (e.g. routed pool work) stayed invisible indefinitely.
func TestPrimeThenStartReconcilerArmsReconcilerOnPrimeFailure(t *testing.T) {
	backing := &fullScanFailingStore{Store: beads.NewMemStore()}
	cs := beads.NewCachingStore(backing, nil)
	cs.SetPrimeRetryDelayForTest(func(int) time.Duration { return 0 })
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// "armed" is the FNV stagger for this agent ID; a non-zero value can
	// only have been written by StartReconciler.
	primeThenStartReconciler(ctx, cs, "armed")

	if got := cs.Stats().StaggerOffsetMs; got <= 0 {
		t.Fatalf("StaggerOffsetMs = %d, want > 0 (reconciler must arm after failed prime)", got)
	}
}

// TestPrimeThenStartReconcilerSkipsReconcilerOnShutdown asserts a
// canceled context (controller shutdown mid-prime) does NOT arm the
// reconciler — prime failure is recoverable, shutdown is not.
func TestPrimeThenStartReconcilerSkipsReconcilerOnShutdown(t *testing.T) {
	backing := &fullScanFailingStore{Store: beads.NewMemStore()}
	cs := beads.NewCachingStore(backing, nil)
	cs.SetPrimeRetryDelayForTest(func(int) time.Duration { return 0 })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	primeThenStartReconciler(ctx, cs, "armed")

	if got := cs.Stats().StaggerOffsetMs; got != 0 {
		t.Fatalf("StaggerOffsetMs = %d, want 0 (reconciler must not arm after shutdown)", got)
	}
}

// TestRigStoreBackgroundRefreshUsesEffectiveSuspension asserts the
// background-refresh gate consults the EFFECTIVE rig suspension — the
// runtime suspend/resume override layered over the rig's committable
// suspended_on_start default — not the deprecated raw [[rigs]] suspended
// field. A rig resumed at runtime must keep its cache reconciler across
// supervisor restarts, and a suspended_on_start rig must actually get
// the suspended-rig reconcile skip.
func TestRigStoreBackgroundRefreshUsesEffectiveSuspension(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	cases := []struct {
		name     string
		rig      config.Rig
		override *bool // runtime suspension override; nil = no entry
		want     bool
	}{
		{name: "active rig refreshes", rig: config.Rig{Name: "r"}, want: true},
		{name: "suspended_on_start skips refresh", rig: config.Rig{Name: "r", SuspendedOnStart: true}, want: false},
		{name: "deprecated suspended field skips refresh", rig: config.Rig{Name: "r", Suspended: true}, want: false},
		{name: "suspended_on_start with runtime resume refreshes", rig: config.Rig{Name: "r", SuspendedOnStart: true}, override: boolPtr(false), want: true},
		{name: "deprecated suspended with runtime resume refreshes", rig: config.Rig{Name: "r", Suspended: true}, override: boolPtr(false), want: true},
		{name: "active rig with runtime suspend skips refresh", rig: config.Rig{Name: "r"}, override: boolPtr(true), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var st suspensionstate.State
			if tc.override != nil {
				suspensionstate.SetRig(&st, tc.rig.Name, tc.override)
			}
			if got := rigStoreBackgroundRefresh(st, tc.rig); got != tc.want {
				t.Fatalf("rigStoreBackgroundRefresh = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestStoreMetadataSignatureChangesOnRigSuspensionFlip asserts the store
// signature reflects effective rig suspension, so a runtime
// suspend/resume flip invalidates runtimeUpdateCanReuseCurrentStores and
// the next config reload rebuilds stores with the correct
// background-refresh gate.
func TestStoreMetadataSignatureChangesOnRigSuspensionFlip(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "rig1", Path: rigDir, SuspendedOnStart: true}}}

	before := storeMetadataSignature(cityDir, cfg)

	resumed := false
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "rig1", &resumed); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}

	after := storeMetadataSignature(cityDir, cfg)
	if before == after {
		t.Fatalf("signature unchanged across rig suspension flip:\n%s", before)
	}
}

func TestConfigMutationSnapshotRestoresThroughSymlinks(t *testing.T) {
	cityDir := t.TempDir()
	checkoutDir := filepath.Join(cityDir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	links := make(map[string]string) // link path -> target path
	for link, target := range map[string]string{
		filepath.Join(cityDir, "city.toml"):        filepath.Join(checkoutDir, "city.toml"),
		filepath.Join(cityDir, ".gc", "site.toml"): filepath.Join(checkoutDir, "site.toml"),
	} {
		if err := os.WriteFile(target, []byte("original = true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		links[link] = target
	}

	snapshot, err := captureConfigMutationSnapshot(cityDir)
	if err != nil {
		t.Fatalf("captureConfigMutationSnapshot: %v", err)
	}

	for _, target := range links {
		if err := os.WriteFile(target, []byte("mutated = true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := snapshot.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for link, target := range links {
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("Lstat %s: %v", link, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s symlink was replaced by a %v entry; restore must write through the link", link, info.Mode())
		}
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", target, err)
		}
		if string(data) != "original = true\n" {
			t.Fatalf("%s target content = %q, want original bytes restored", link, data)
		}
	}
}

func TestConfigMutationSnapshotRestoresSymlinkedAgentTomlTarget(t *testing.T) {
	cityDir := t.TempDir()
	checkoutDir := filepath.Join(cityDir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(cityDir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// agents/worker/agent.toml is symlinked to an operator file checked out of
	// the agents/ tree (the ga-lurp5d "linked into a repo" case). The forward
	// agent mutation path writes/removes the resolved target, so a rollback must
	// restore the target bytes — SnapshotTree only preserves the link entry.
	target := filepath.Join(checkoutDir, "worker-agent.toml")
	original := []byte("provider = \"claude\"\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(agentDir, "agent.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	snapshot, err := captureConfigMutationSnapshot(cityDir)
	if err != nil {
		t.Fatalf("captureConfigMutationSnapshot: %v", err)
	}

	// A suspend writes through the link, mutating the resolved target content.
	if err := os.WriteFile(target, []byte("provider = \"claude\"\nsuspended = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := snapshot.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s symlink was replaced by a %v entry; restore must write through the link", link, info.Mode())
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Fatalf("agent.toml symlink target = %q, %v; want %q", got, err, target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", target, err)
	}
	if string(data) != string(original) {
		t.Fatalf("agent.toml target content = %q, want original bytes %q restored", data, original)
	}
}

func TestConfigMutationSnapshotRecreatesRemovedSymlinkedAgentTomlTarget(t *testing.T) {
	cityDir := t.TempDir()
	checkoutDir := filepath.Join(cityDir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(cityDir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// An empty resume/delete removes the resolved target through the link. The
	// rollback must recreate the operator's target bytes, not leave a dangling
	// link with the durable config gone.
	target := filepath.Join(checkoutDir, "worker-agent.toml")
	original := []byte("provider = \"claude\"\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(agentDir, "agent.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	snapshot, err := captureConfigMutationSnapshot(cityDir)
	if err != nil {
		t.Fatalf("captureConfigMutationSnapshot: %v", err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if err := snapshot.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s symlink was replaced by a %v entry; restore must keep the link", link, info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile restored %s: %v", target, err)
	}
	if string(data) != string(original) {
		t.Fatalf("agent.toml target content = %q, want original bytes %q recreated", data, original)
	}
}

func TestControllerStateSuspendRestoresSymlinkedAgentTomlTargetWhenRefreshFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city1\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	checkoutDir := filepath.Join(cityDir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	agentDir := filepath.Join(cityDir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatalf("write prompt.template.md: %v", err)
	}

	// agents/worker/agent.toml is symlinked to a checked-out operator file.
	agentTarget := filepath.Join(checkoutDir, "worker-agent.toml")
	originalAgent := []byte("provider = \"claude\"\n")
	if err := os.WriteFile(agentTarget, originalAgent, 0o644); err != nil {
		t.Fatalf("write agent target: %v", err)
	}
	agentLink := filepath.Join(agentDir, "agent.toml")
	if err := os.Symlink(agentTarget, agentLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	resolvedAgentTarget, err := fsys.ResolveSymlinks(fsys.OSFS{}, agentLink)
	if err != nil {
		t.Fatalf("resolve agent symlink: %v", err)
	}

	original := []byte("[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n")
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, original, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	cs := newControllerState(context.Background(), &config.City{
		Workspace: config.Workspace{Name: "city1"},
		Providers: map[string]config.ProviderSpec{
			"claude": config.BuiltinProviderAlias("claude"),
		},
	}, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	// The suspend write renames a temp file onto the resolved agent.toml target;
	// corrupt city.toml at that moment so the post-mutation refresh fails and
	// the rollback path runs.
	cs.editor = configedit.NewEditor(&corruptCityAfterRenameFS{
		triggerPath: resolvedAgentTarget,
		cityToml:    tomlPath,
	}, tomlPath)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}

	err = cs.SuspendAgent("worker")
	if err == nil {
		t.Fatal("SuspendAgent should fail when refreshing the updated snapshot fails")
	}
	if !strings.Contains(err.Error(), "refreshing updated city config") {
		t.Fatalf("SuspendAgent error = %v, want refresh failure after mutation", err)
	}

	info, err := os.Lstat(agentLink)
	if err != nil {
		t.Fatalf("Lstat agent symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("agent.toml symlink was replaced by a %v entry after rollback", info.Mode())
	}
	gotAgent, err := os.ReadFile(agentTarget)
	if err != nil {
		t.Fatalf("read restored agent target: %v", err)
	}
	if string(gotAgent) != string(originalAgent) {
		t.Fatalf("agent.toml target = %q, want rollback to %q", gotAgent, originalAgent)
	}
	if cs.configDirty.Load() {
		t.Fatal("SuspendAgent should not mark config dirty after rollback")
	}
}
