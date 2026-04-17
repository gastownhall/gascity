package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSweepUndesiredPoolSessionBeads_KeepsRunningSessionsOpen(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-bd-123", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	closed := sweepUndesiredPoolSessionBeads(
		store,
		sessionBeads,
		nil,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		sp,
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("running pool bead was closed: %+v", got)
	}
}

func TestCityRuntimeRequestDeferredDrainFollowUpTick_PokesOnce(t *testing.T) {
	cr := &CityRuntime{
		sessionDrains: newDrainTracker(),
		pokeCh:        make(chan struct{}, 1),
	}
	cr.sessionDrains.set("bead-1", &drainState{followUp: true})

	cr.requestDeferredDrainFollowUpTick()

	select {
	case <-cr.pokeCh:
	default:
		t.Fatal("expected deferred drain follow-up to enqueue a poke")
	}

	if ds := cr.sessionDrains.get("bead-1"); ds == nil || ds.followUp {
		t.Fatal("expected deferred drain follow-up flag to be consumed")
	}

	cr.requestDeferredDrainFollowUpTick()

	select {
	case <-cr.pokeCh:
		t.Fatal("unexpected second poke without a new deferred drain follow-up")
	default:
	}
}

func TestSweepUndesiredPoolSessionBeads_ClosesStoppedSessions(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "drained",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		sessionBeads,
		nil,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("stopped pool bead status = %q, want closed", got.Status)
	}
}

func TestSweepUndesiredPoolSessionBeads_KeepsAssignedSessionsOpen(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "asleep",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-bd-123",
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		sessionBeads,
		nil,
		[]beads.Bead{work},
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("assigned pool bead was swept closed: %+v", got)
	}
}

func TestSweepUndesiredPoolSessionBeads_SkipsPartialAssignedSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "drained",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		sessionBeads,
		nil,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		true,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("partial assigned-work snapshot should suppress sweep: %+v", got)
	}
}

func TestCityRuntimeBeadReconcileTick_TransientStoreQueryPartialKeepsRunningPoolSessionUntilRecoveryTick(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-bd-123", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	partialResult := DesiredStateResult{
		State:             map[string]TemplateParams{},
		ScaleCheckCounts:  map[string]int{"worker": 0},
		StoreQueryPartial: true,
	}
	cr.beadReconcileTick(context.Background(), partialResult, newSessionBeadSnapshot([]beads.Bead{session}), nil)

	afterPartial, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after partial tick: %v", err)
	}
	if afterPartial.Status == "closed" {
		t.Fatalf("partial tick closed running session: %+v", afterPartial)
	}
	if !sp.IsRunning("worker-bd-123") {
		t.Fatal("partial tick should not stop the running worker")
	}

	recoveredResult := DesiredStateResult{
		State:            map[string]TemplateParams{},
		ScaleCheckCounts: map[string]int{"worker": 0},
		AssignedWorkBeads: []beads.Bead{
			workBead("ga-live", "worker", "worker-bd-123", "in_progress", 5),
		},
	}
	cr.beadReconcileTick(context.Background(), recoveredResult, cr.loadSessionBeadSnapshot(), nil)

	afterRecovered, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after recovered tick: %v", err)
	}
	if afterRecovered.Status == "closed" {
		t.Fatalf("recovered tick closed running session: %+v", afterRecovered)
	}
	if state := afterRecovered.Metadata["state"]; state == "drained" || state == "asleep" {
		t.Fatalf("recovered tick state = %q, want active/awake", state)
	}
	if !sp.IsRunning("worker-bd-123") {
		t.Fatal("recovered tick should keep the worker running")
	}
}

func TestCityRuntimeBeadReconcileTick_KeepsAssignedPoolWorkerAwake(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/claude"},
		Metadata: map[string]string{
			"session_name":         "claude-mc-live",
			"template":             "gascity/claude",
			"agent_name":           "gascity/claude",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "claude-mc-live", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "claude", Dir: "gascity", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	result := DesiredStateResult{
		State:            map[string]TemplateParams{},
		ScaleCheckCounts: map[string]int{"gascity/claude": 0},
		AssignedWorkBeads: []beads.Bead{
			workBead("ga-live", "gascity/claude", "claude-mc-live", "in_progress", 5),
		},
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{session})
	cr.beadReconcileTick(context.Background(), result, sessionBeads, nil)

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("assigned pool worker was closed: %+v", got)
	}
	if state := got.Metadata["state"]; state == "drained" || state == "asleep" {
		t.Fatalf("assigned pool worker state = %q, want active/awake", state)
	}
	if !sp.IsRunning("claude-mc-live") {
		t.Fatal("assigned pool worker should still be running")
	}
}

func TestCityRuntimeBeadReconcileTick_SweepUsesOwnershipWorkSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-mc-live",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "asleep",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	result := DesiredStateResult{
		State:              map[string]TemplateParams{},
		ScaleCheckCounts:   map[string]int{"worker": 0},
		AssignedWorkBeads:  []beads.Bead{},
		OwnershipWorkBeads: []beads.Bead{workBead("ga-future", "worker", "worker-mc-live", "open", 1)},
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{session})
	cr.beadReconcileTick(context.Background(), result, sessionBeads, nil)

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead was swept closed despite future assigned work: %+v", got)
	}
}

func TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "prompts", "worker.md"), []byte("# worker\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	store := beads.NewMemStore()
	manual, err := store.Create(beads.Bead{
		Title:  "hal",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":             "helper",
			"manual_session":       "true",
			"alias":                "hal",
			"state":                "creating",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatalf("Create manual session bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:     "my-city",
			Provider: "claude",
		},
		Providers: map[string]config.ProviderSpec{
			"claude": {
				Command:    "echo",
				PromptMode: "arg",
			},
		},
		Agents: []config.Agent{{
			Name:              "helper",
			PromptTemplate:    "prompts/worker.md",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "printf 0",
		}},
	}

	sp := runtime.NewFake()
	var stderr bytes.Buffer
	var mutated bool

	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "my-city",
		cfg:                 cfg,
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              &stderr,
	}
	cr.buildFnWithSessionBeads = func(
		c *config.City,
		currentSP runtime.Provider,
		store beads.Store,
		rigStores map[string]beads.Store,
		sessionBeads *sessionBeadSnapshot,
		trace *sessionReconcilerTraceCycle,
	) DesiredStateResult {
		result := buildDesiredStateWithSessionBeads("my-city", cityPath, time.Now(), c, currentSP, store, rigStores, sessionBeads, trace, &stderr)
		if !mutated {
			if err := store.SetMetadata(manual.ID, "session_name", sessionNameFromBeadID(manual.ID)); err != nil {
				t.Fatalf("SetMetadata(session_name): %v", err)
			}
			mutated = true
		}
		return result
	}

	var prevPoolRunning map[string]bool
	var lastProviderName string
	dirty := &atomic.Bool{}
	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "test")

	if !mutated {
		t.Fatal("test setup did not mutate the manual session bead between build and reconcile")
	}
	got, err := store.Get(manual.ID)
	if err != nil {
		t.Fatalf("Get manual session bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("manual session bead was closed after refreshed overlay should have preserved it: %+v", got)
	}
	if got.Metadata["state"] == "orphaned" || got.Metadata["close_reason"] == "orphaned" {
		t.Fatalf("manual session bead was marked orphaned after refreshed overlay: %+v", got.Metadata)
	}
}

func TestCityRuntimeTickRunsOnDeathWithCanonicalRigEnv(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "3308",
		DoltUser:       "rig-user",
	})
	writeScopePassword(t, rigDir, "rig-secret")

	cfg.Workspace.Name = "my-city"
	outFile := filepath.Join(t.TempDir(), "on-death-env.txt")
	cfg.Agents[0] = config.Agent{
		Name:              "worker",
		Dir:               "demo",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}

	handlers := computePoolDeathHandlers(cfg, "my-city", cityPath, runtime.NewFake())
	if len(handlers) == 0 {
		t.Fatal("computePoolDeathHandlers returned no handlers")
	}
	prevPoolRunning := map[string]bool{}
	for sessionName, info := range handlers {
		info.Command = "printf '%s|%s|%s' \"${GC_DOLT_PORT:-}\" \"${GC_DOLT_USER:-}\" \"${GC_DOLT_PASSWORD:-}\" > " + outFile
		handlers[sessionName] = info
		prevPoolRunning[sessionName] = true
		break
	}

	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "my-city",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: beads.NewMemStore(),
		sessionDrains:       newDrainTracker(),
		poolDeathHandlers:   handlers,
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              &stderr,
		buildFnWithSessionBeads: func(_ *config.City, _ runtime.Provider, _ beads.Store, _ map[string]beads.Store, _ *sessionBeadSnapshot, _ *sessionReconcilerTraceCycle) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	dirty := &atomic.Bool{}
	var lastProviderName string
	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "test")

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v\nstderr=%s", outFile, err, stderr.String())
	}
	if got := strings.TrimSpace(string(data)); got != "3308|rig-user|rig-secret" {
		t.Fatalf("on_death env = %q, want %q", got, "3308|rig-user|rig-secret")
	}
}

func TestControlDispatcherOnlyConfig_IncludesRigScopedDispatchers(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "claude"},
			{Name: config.ControlDispatcherAgentName},
			{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		},
	}

	filtered := controlDispatcherOnlyConfig(cfg)
	if filtered == nil {
		t.Fatal("filtered config = nil")
	}
	if len(filtered.Agents) != 2 {
		t.Fatalf("len(filtered.Agents) = %d, want 2", len(filtered.Agents))
	}
	if filtered.Agents[0].QualifiedName() != "control-dispatcher" {
		t.Fatalf("filtered city dispatcher = %q, want control-dispatcher", filtered.Agents[0].QualifiedName())
	}
	if filtered.Agents[1].QualifiedName() != "gascity/control-dispatcher" {
		t.Fatalf("filtered rig dispatcher = %q, want gascity/control-dispatcher", filtered.Agents[1].QualifiedName())
	}
}

func TestCityRuntimeBuildDesiredState_StandaloneIncludesRigStores(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	var gotRigStores map[string]beads.Store

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Rigs: []config.Rig{{Name: "gascity"}}},
		sp:                  runtime.NewFake(),
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"gascity": rigStore},
		buildFnWithSessionBeads: func(_ *config.City, _ runtime.Provider, store beads.Store, rigStores map[string]beads.Store, _ *sessionBeadSnapshot, _ *sessionReconcilerTraceCycle) DesiredStateResult {
			if store != cityStore {
				t.Fatalf("store = %v, want city store", store)
			}
			gotRigStores = rigStores
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	cr.buildDesiredState(nil, nil)

	if len(gotRigStores) != 1 {
		t.Fatalf("len(rigStores) = %d, want 1", len(gotRigStores))
	}
	if gotRigStores["gascity"] != rigStore {
		t.Fatalf("rigStores[gascity] = %v, want rig store", gotRigStores["gascity"])
	}
}

func TestCityRuntimeReloadProviderSwapPreservesDrainTracker(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	// Manually initialize drain tracker (normally done in run()).
	cr.sessionDrains = newDrainTracker()

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if lastProviderName != "fail" {
		t.Fatalf("lastProviderName = %q, want fail", lastProviderName)
	}
	if cr.sessionDrains == nil {
		t.Fatal("sessionDrains = nil after provider swap, want non-nil")
	}
}

func TestCityRuntimeReloadLifecycleFailureKeepsOldConfig(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	oldCfg := cr.cfg
	oldSP := cr.sp
	oldDops := cr.dops
	oldRev := cr.configRev

	prev := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error {
		return fmt.Errorf("boom")
	}
	t.Cleanup(func() {
		cityRuntimeStartBeadsLifecycle = prev
	})

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if cr.cfg != oldCfg {
		t.Fatal("cfg changed after lifecycle reload failure")
	}
	if cr.sp != oldSP {
		t.Fatal("provider changed after lifecycle reload failure")
	}
	if cr.dops != oldDops {
		t.Fatal("drain ops changed after lifecycle reload failure")
	}
	if cr.configRev != oldRev {
		t.Fatalf("configRev = %q, want %q", cr.configRev, oldRev)
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want fake", lastProviderName)
	}
	if !strings.Contains(stderr.String(), "keeping old config") {
		t.Fatalf("stderr = %q, want keeping old config message", stderr.String())
	}
	if strings.Contains(stdout.String(), "Session provider swapped") {
		t.Fatalf("stdout = %q, want no provider swap message", stdout.String())
	}
}

func TestCityRuntimeReloadSameRevisionIsNoOp(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})

	oldCfg := cr.cfg
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if cr.cfg != oldCfg {
		t.Fatal("same-revision reload should keep existing config pointer")
	}
	if cr.configRev != configRev {
		t.Fatalf("configRev = %q, want %q", cr.configRev, configRev)
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want fake", lastProviderName)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty for same-revision reload", stdout.String())
	}
}

func TestCityRuntimeRunStopsBeforeStartedWhenCanceledDuringStartup(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	var started bool

	ctx, cancel := context.WithCancel(context.Background())
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			cancel()
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:      newDrainOps(sp),
		Rec:       events.Discard,
		OnStarted: func() { started = true },
		Stdout:    &stdout,
		Stderr:    io.Discard,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if started {
		t.Fatal("OnStarted called after cancellation")
	}
	if strings.Contains(stdout.String(), "City started.") {
		t.Fatalf("stdout = %q, want no started banner after cancellation", stdout.String())
	}
}

func writeCityRuntimeConfig(t *testing.T, tomlPath, provider string) {
	t.Helper()
	data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"" + provider + "\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
