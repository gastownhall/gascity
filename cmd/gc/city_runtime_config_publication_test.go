package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// supersedingCityConfig is a valid city.toml that differs from every
// writeCityRuntimeConfig* fixture, so writing it mid-reload makes the
// candidate revision stale.
const supersedingCityConfig = "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"2s\"\n"

type rewriteConfigOnListProvider struct {
	*runtime.Fake
	once    sync.Once
	rewrite func()
}

func (p *rewriteConfigOnListProvider) ListRunning(prefix string) ([]string, error) {
	if p.rewrite != nil {
		p.once.Do(p.rewrite)
	}
	return p.Fake.ListRunning(prefix)
}

func stubReloadBeadsLifecycle(t *testing.T, fn func(string, string, *config.City, io.Writer) error) {
	t.Helper()
	previous := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = fn
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = previous })
}

func newPublicationTestRuntime(t *testing.T, cityPath string, sp runtime.Provider, stdout, stderr io.Writer) (*CityRuntime, *controllerState, *config.City) {
	t.Helper()
	tomlPath := filepath.Join(cityPath, "city.toml")
	initialCfg, initialRevision := loadCityRuntimeControllerConfig(t, cityPath)
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: initialRevision,
		Cfg:       initialCfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: stdout,
		Stderr: stderr,
	})
	cs := newControllerState(context.Background(), initialCfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	return cr, cs, initialCfg
}

func TestCityRuntimePublishRejectedByControllerStateKeepsLoopGeneration(t *testing.T) {
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
	cr := &CityRuntime{cfg: runtimeCfg, sp: runtimeProvider, dops: runtimeDrainOps, cs: cs}

	staleProvider := runtime.NewFake()
	if cr.publishRuntimeConfig(&config.City{Workspace: config.Workspace{Name: "stale"}}, staleProvider, newDrainOps(staleProvider), "stale-revision") {
		t.Fatal("stale runtime config publication was accepted")
	}
	if cr.cfg != runtimeCfg || cr.sp != runtimeProvider || cr.dops != runtimeDrainOps {
		t.Fatal("rejected publication changed CityRuntime config, provider, or drain operations")
	}
	if cs.Config() != currentCfg || cs.SessionProvider() != currentProvider {
		t.Fatal("rejected publication changed controller state")
	}
}

func TestCityRuntimePublishAcceptedConfigUpdatesBothSnapshots(t *testing.T) {
	cityPath := t.TempDir()
	writeCityRuntimeConfig(t, filepath.Join(cityPath, "city.toml"), "fake")
	currentCfg, revision := loadCityRuntimeControllerConfig(t, cityPath)
	currentProvider := runtime.NewFake()
	cs := newControllerState(context.Background(), currentCfg, currentProvider, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr := &CityRuntime{cfg: currentCfg, sp: currentProvider, dops: newDrainOps(currentProvider), cs: cs}

	nextCfg, _ := loadCityRuntimeControllerConfig(t, cityPath)
	nextProvider := runtime.NewFake()
	nextDrainOps := newDrainOps(nextProvider)
	if !cr.publishRuntimeConfig(nextCfg, nextProvider, nextDrainOps, revision) {
		t.Fatal("current runtime config publication was rejected")
	}
	if cr.cfg != nextCfg || cr.sp != nextProvider || cr.dops != nextDrainOps {
		t.Fatal("accepted publication did not update CityRuntime")
	}
	if cs.Config() != nextCfg || cs.SessionProvider() != nextProvider {
		t.Fatal("accepted publication did not update controller state")
	}
}

// A retry must stay pending for the next tick without poking: a rejection
// that repeats on the same on-disk revision would otherwise hot-loop full
// reconciliation ticks. The writer that superseded the candidate (the config
// watcher or an API mutation) already pokes.
func TestCityRuntimeSupersededReloadRetryIsLevelTriggered(t *testing.T) {
	cr := &CityRuntime{pokeCh: make(chan struct{}, 1)}
	cr.requestConfigReloadRetry()

	if cr.configDirty == nil || !cr.configDirty.Load() {
		t.Fatal("retry did not leave config reload dirty")
	}
	select {
	case <-cr.pokeCh:
		t.Fatal("retry poked the controller; a repeatable rejection would hot-loop")
	default:
	}
}

func TestCityRuntimeReloadRejectsRevisionSupersededDuringPreparation(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	provider := runtime.NewFake()
	var stderr bytes.Buffer
	cr, cs, initialCfg := newPublicationTestRuntime(t, cityPath, provider, io.Discard, &stderr)

	oldCfg, oldProvider, oldDops, oldRevision := cr.cfg, cr.sp, cr.dops, cr.configRev
	writeCityRuntimeConfigWithOneSecondShutdownTimeout(t, tomlPath)
	// A newer config lands on disk while the reload is still preparing its
	// candidate (here: during the bead lifecycle step).
	stubReloadBeadsLifecycle(t, func(string, string, *config.City, io.Writer) error {
		return os.WriteFile(tomlPath, []byte(supersedingCityConfig), 0o644)
	})

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
		t.Fatal("superseded reload did not leave a retry pending")
	}
}

// An API config mutation that lands while the loop is preparing an older
// candidate wins: the loop must not adopt the candidate the controller state
// rejected, and the retry must converge both snapshots on the mutation.
func TestCityRuntimeReloadSupersededByAPIMutationConvergesOnRetry(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	provider := runtime.NewFake()
	cr, cs, _ := newPublicationTestRuntime(t, cityPath, provider, io.Discard, io.Discard)

	oldCfg, oldRevision := cr.cfg, cr.configRev
	writeCityRuntimeConfigWithOneSecondShutdownTimeout(t, tomlPath)
	mutated := false
	stubReloadBeadsLifecycle(t, func(string, string, *config.City, io.Writer) error {
		if mutated {
			return nil
		}
		mutated = true
		return cs.CreateAgent(config.Agent{Name: "helper"})
	})

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)
	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "superseded") {
		t.Fatalf("first reload reply = %+v, want superseded failure", reply)
	}
	if cr.cfg != oldCfg || cr.configRev != oldRevision {
		t.Fatal("loop adopted the candidate the controller state rejected")
	}
	if !configHasAgent(cs.Config(), "helper") {
		t.Fatal("API mutation was overwritten by the superseded reload")
	}
	if cr.configDirty == nil || !cr.configDirty.Load() {
		t.Fatal("superseded reload did not leave a retry pending")
	}

	reply = cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("retry reply = %+v, want applied", reply)
	}
	if !configHasAgent(cr.cfg, "helper") || cs.Config() != cr.cfg {
		t.Fatal("retry did not converge the loop and controller state on the API mutation")
	}
	if cs.configMutationPending.Load() {
		t.Fatal("retry left the API mutation pending")
	}
}

func TestCityRuntimeProviderReloadSupersededDuringCensusDoesNotPublishSwap(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	provider := &rewriteConfigOnListProvider{Fake: runtime.NewFake()}
	var stdout bytes.Buffer
	cr, cs, _ := newPublicationTestRuntime(t, cityPath, provider, &stdout, io.Discard)

	stubReloadBeadsLifecycle(t, func(string, string, *config.City, io.Writer) error { return nil })
	writeCityRuntimeConfig(t, tomlPath, "fail")
	provider.rewrite = func() {
		if err := os.WriteFile(tomlPath, []byte(supersedingCityConfig), 0o644); err != nil {
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

// A provider swap stops every running session, which cannot be undone, so a
// candidate that is already stale must be rejected before that happens.
func TestCityRuntimeProviderReloadAlreadySupersededSkipsProviderEffects(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	provider := &rewriteConfigOnListProvider{Fake: runtime.NewFake()}
	var stdout bytes.Buffer
	cr, cs, _ := newPublicationTestRuntime(t, cityPath, provider, &stdout, io.Discard)

	writeCityRuntimeConfig(t, tomlPath, "fail")
	stubReloadBeadsLifecycle(t, func(string, string, *config.City, io.Writer) error {
		return os.WriteFile(tomlPath, []byte(supersedingCityConfig), 0o644)
	})
	listed := false
	provider.rewrite = func() { listed = true }

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "superseded before provider effects") {
		t.Fatalf("reload reply = %+v, want superseded-before-provider-effects failure", reply)
	}
	if listed {
		t.Fatal("stale provider reload listed sessions for the provider swap")
	}
	if cr.sp != provider || cs.SessionProvider() != provider || lastProviderName != "fake" {
		t.Fatal("stale provider reload published the candidate provider")
	}
}
