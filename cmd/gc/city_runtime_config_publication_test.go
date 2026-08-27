package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

type rewriteConfigOnListProvider struct {
	*runtime.Fake
	once    sync.Once
	rewrite func()
}

type triggerOnNthDoneContext struct {
	context.Context
	mu      sync.Mutex
	calls   int
	trigger int
	fn      func()
}

func (c *triggerOnNthDoneContext) Done() <-chan struct{} {
	c.mu.Lock()
	c.calls++
	var fn func()
	if c.calls == c.trigger {
		fn = c.fn
		c.fn = nil
	}
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
	return c.Context.Done()
}

type immediateOrderDispatcher struct{}

func (immediateOrderDispatcher) dispatch(context.Context, string, time.Time) {}
func (immediateOrderDispatcher) drain(context.Context) bool                  { return true }

func (p *rewriteConfigOnListProvider) ListRunning(prefix string) ([]string, error) {
	if p.rewrite != nil {
		p.once.Do(p.rewrite)
	}
	return p.Fake.ListRunning(prefix)
}

func TestCityRuntimeRejectsConfigPublicationRejectedByControllerState(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"current\"\n"), 0o644); err != nil {
		t.Fatalf("write current config: %v", err)
	}

	currentCfg := &config.City{Workspace: config.Workspace{Name: "current"}}
	currentProvider := runtime.NewFake()
	cs := &controllerState{
		cfg:      currentCfg,
		sp:       currentProvider,
		cityName: "current",
		cityPath: cityPath,
	}

	runtimeCfg := &config.City{Workspace: config.Workspace{Name: "runtime-before"}}
	runtimeProvider := runtime.NewFake()
	runtimeDrainOps := newDrainOps(runtimeProvider)
	cr := &CityRuntime{
		cfg:  runtimeCfg,
		sp:   runtimeProvider,
		dops: runtimeDrainOps,
		cs:   cs,
	}

	staleCfg := &config.City{Workspace: config.Workspace{Name: "stale"}}
	staleProvider := runtime.NewFake()
	if cr.publishRuntimeConfig(staleCfg, staleProvider, newDrainOps(staleProvider), "stale-revision") {
		t.Fatal("stale runtime config publication was accepted")
	}

	if cr.cfg != runtimeCfg || cr.sp != runtimeProvider || cr.dops != runtimeDrainOps {
		t.Fatal("rejected publication changed CityRuntime config, provider, or drain operations")
	}
	if cs.Config() != currentCfg || cs.SessionProvider() != currentProvider {
		t.Fatal("rejected publication changed controller state")
	}
}

func TestCityRuntimePublishesAcceptedConfigToBothSnapshots(t *testing.T) {
	currentCfg := &config.City{Workspace: config.Workspace{Name: "current"}}
	currentProvider := runtime.NewFake()
	cs := &controllerState{
		cfg:           currentCfg,
		sp:            currentProvider,
		cityBeadStore: beads.NewMemStore(),
		beadStores:    map[string]beads.Store{},
	}
	cr := &CityRuntime{cfg: currentCfg, sp: currentProvider, dops: newDrainOps(currentProvider), cs: cs}

	nextCfg := &config.City{Workspace: config.Workspace{Name: "next"}}
	nextProvider := runtime.NewFake()
	nextDrainOps := newDrainOps(nextProvider)
	if !cr.publishRuntimeConfig(nextCfg, nextProvider, nextDrainOps, "") {
		t.Fatal("current runtime config publication was rejected")
	}

	if cr.cfg != nextCfg || cr.sp != nextProvider || cr.dops != nextDrainOps {
		t.Fatal("accepted publication did not update CityRuntime atomically")
	}
	if cs.Config() != nextCfg || cs.SessionProvider() != nextProvider {
		t.Fatal("accepted publication did not update controller state")
	}
}

func TestCityRuntimeConfigReloadRetryIsLevelTriggeredAndImmediate(t *testing.T) {
	cr := &CityRuntime{pokeCh: make(chan struct{}, 1)}
	cr.requestConfigReloadRetry()

	if cr.configDirty == nil || !cr.configDirty.Load() {
		t.Fatal("retry did not leave config reload dirty")
	}
	select {
	case <-cr.pokeCh:
	default:
		t.Fatal("retry did not request an immediate controller tick")
	}
}

func TestCityRuntimeReloadDoesNotWaitForInFlightConfigMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs := &controllerState{
			editor:                configedit.NewEditor(fsys.OSFS{}, filepath.Join(t.TempDir(), "city.toml")),
			configMutationBarrier: make(chan struct{}, 1),
		}
		mutationEntered := make(chan struct{})
		releaseMutation := make(chan struct{})
		mutationDone := make(chan error, 1)
		go func() {
			mutationDone <- cs.MutatePackConfig(context.Background(), func() error {
				close(mutationEntered)
				<-releaseMutation
				return nil
			})
		}()
		<-mutationEntered

		cr := &CityRuntime{
			cs:          cs,
			tomlPath:    filepath.Join(t.TempDir(), "city.toml"),
			cityName:    "test-city",
			configDirty: &atomic.Bool{},
			pokeCh:      make(chan struct{}, 1),
			stdout:      io.Discard,
			stderr:      io.Discard,
		}
		lastProviderName := "fake"
		reloadDone := make(chan reloadControlReply, 1)
		go func() {
			reloadDone <- cr.reloadConfigTraced(context.Background(), &lastProviderName, t.TempDir(), nil, reloadSourceWatch)
		}()
		synctest.Wait()

		var reply reloadControlReply
		completedBeforeCommit := false
		select {
		case reply = <-reloadDone:
			completedBeforeCommit = true
		default:
		}
		retryDirty := cr.configDirty.Load()
		retryPoked := false
		select {
		case <-cr.pokeCh:
			retryPoked = true
		default:
		}

		close(releaseMutation)
		synctest.Wait()
		if err := <-mutationDone; err != nil {
			t.Fatalf("pack mutation: %v", err)
		}
		if !completedBeforeCommit {
			<-reloadDone
			t.Fatal("config reload blocked behind an in-flight pack mutation")
		}
		if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "config mutation") {
			t.Fatalf("reload reply = %+v, want retryable in-flight mutation failure", reply)
		}
		if !retryDirty || retryPoked {
			t.Fatalf("reload retry dirty/poked = %t/%t, want true/false", retryDirty, retryPoked)
		}
	})
}

func TestCityRuntimeSameRevisionMetadataRefreshRejectsSupersededSnapshot(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	writeBackendMetadata(t, cityPath, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)

	initialCfg, initialRevision := loadCityRuntimeControllerConfig(t, cityPath)
	provider := runtime.NewFake()
	cs := newControllerState(context.Background(), initialCfg, provider, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: initialRevision,
		Cfg:       initialCfg,
		SP:        provider,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(provider),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cr.setControllerState(cs)
	cr.od = immediateOrderDispatcher{}
	cr.orderSetSignature = "force-rescan"
	writeBackendMetadata(t, cityPath, `{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`)

	var mutationErr error
	ctx := &triggerOnNthDoneContext{
		Context: context.Background(),
		trigger: 2,
		fn: func() {
			mutationErr = cs.MutatePackConfig(context.Background(), func() error {
				writeCityRuntimeConfigWithOneSecondShutdownTimeout(t, tomlPath)
				return nil
			})
			writeBackendMetadata(t, cityPath, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
		},
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual)

	if mutationErr != nil {
		t.Fatalf("newer config mutation: %v", mutationErr)
	}
	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "superseded") {
		t.Fatalf("reload reply = %+v, want superseded failure", reply)
	}
	if got := cs.Config().Daemon.ShutdownTimeoutDuration(); got != time.Second {
		t.Fatalf("controller timeout = %v, want newer one-second mutation", got)
	}
	if cr.configDirty == nil || !cr.configDirty.Load() {
		t.Fatal("superseded same-revision reload did not schedule a retry")
	}
}

func TestCityRuntimeReloadRejectsRevisionSupersededDuringPreparation(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	initialCfg, initialRevision := loadCityRuntimeControllerConfig(t, cityPath)
	provider := runtime.NewFake()
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: initialRevision,
		Cfg:       initialCfg,
		SP:        provider,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(provider),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	cs := newControllerState(context.Background(), initialCfg, provider, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	oldCfg, oldProvider, oldDops, oldRevision := cr.cfg, cr.sp, cr.dops, cr.configRev
	writeCityRuntimeConfigWithOneSecondShutdownTimeout(t, tomlPath)

	previousLifecycle := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error {
		data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"2s\"\n")
		return os.WriteFile(tomlPath, data, 0o644)
	}
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previousLifecycle })

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "superseded during runtime publication") {
		t.Fatalf("reload reply = %+v, want superseded failure", reply)
	}
	if cr.cfg != oldCfg || cr.sp != oldProvider || cr.dops != oldDops || cr.configRev != oldRevision {
		t.Fatal("superseded reload changed the loop-owned runtime generation")
	}
	if cs.Config() != initialCfg || cs.SessionProvider() != provider {
		t.Fatal("superseded reload changed the API-visible runtime generation")
	}
	if cr.configDirty == nil || !cr.configDirty.Load() {
		t.Fatal("superseded reload did not leave an authoritative retry pending")
	}
}

func TestCityRuntimeProviderReloadSupersededDuringCensusDoesNotPublishSwap(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	initialCfg, initialRevision := loadCityRuntimeControllerConfig(t, cityPath)
	provider := &rewriteConfigOnListProvider{Fake: runtime.NewFake()}
	var stdout bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: initialRevision,
		Cfg:       initialCfg,
		SP:        provider,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(provider),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), initialCfg, provider, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	previousLifecycle := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error { return nil }
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previousLifecycle })
	writeCityRuntimeConfig(t, tomlPath, "fail")
	provider.rewrite = func() {
		data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"2s\"\n")
		if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
			t.Errorf("write superseding config: %v", err)
		}
	}

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "superseded during runtime publication") {
		t.Fatalf("reload reply = %+v, want superseded failure", reply)
	}
	if cr.sp != provider || cs.SessionProvider() != provider {
		t.Fatal("superseded provider reload published the candidate provider")
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want unchanged fake", lastProviderName)
	}
	if strings.Contains(stdout.String(), "Session provider swapped") {
		t.Fatalf("stdout = %q, want no provider-swap notification", stdout.String())
	}
}
