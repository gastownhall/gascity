package main

import (
	"bytes"
	"context"
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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
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
		nil,
		sessionBeads,
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

func newTestCityRuntime(t *testing.T, params CityRuntimeParams) *CityRuntime {
	t.Helper()

	cr := newCityRuntime(params)
	t.Cleanup(cr.shutdown)
	return cr
}

func TestFilterReleasedAssignedWorkBeads_PreservesSameIDUnreleasedWork(t *testing.T) {
	assigned := []beads.Bead{
		{ID: "gc-1", Title: "released city work"},
		{ID: "gc-1", Title: "live rig work"},
		{ID: "gc-2", Title: "unrelated work"},
	}

	got := filterReleasedAssignedWorkBeads(assigned, []releasedPoolAssignment{{ID: "gc-1", Index: 0}})

	if len(got) != 2 {
		t.Fatalf("filtered length = %d, want 2: %#v", len(got), got)
	}
	if got[0].Title != "live rig work" || got[1].Title != "unrelated work" {
		t.Fatalf("filtered = %#v, want live same-ID work and unrelated work", got)
	}
}

func TestFilterReleasedAssignedWorkBeads_IgnoresMismatchedReleasedIndex(t *testing.T) {
	assigned := []beads.Bead{
		{ID: "gc-1", Title: "first work"},
		{ID: "gc-2", Title: "second work"},
	}

	got := filterReleasedAssignedWorkBeads(assigned, []releasedPoolAssignment{{ID: "gc-2", Index: 0}})

	if len(got) != len(assigned) {
		t.Fatalf("filtered length = %d, want %d: %#v", len(got), len(assigned), got)
	}
	for i := range assigned {
		if got[i].ID != assigned[i].ID {
			t.Fatalf("filtered[%d] = %q, want %q", i, got[i].ID, assigned[i].ID)
		}
	}
}

type sessionSnapshotListFailStore struct {
	beads.Store
}

func (s sessionSnapshotListFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == sessionBeadLabel {
		return nil, errors.New("session snapshot unavailable")
	}
	return s.Store.List(query)
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

func TestCityRuntimeShutdownMarksCityStopSleepReason(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "control-dispatcher",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "control-dispatcher",
			"template":     "control-dispatcher",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cr := &CityRuntime{
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		},
		sp:                  runtime.NewFake(),
		rec:                 events.Discard,
		standaloneCityStore: store,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	cr.shutdown()

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["sleep_reason"] != sleepReasonCityStop {
		t.Fatalf("sleep_reason = %q, want %q", got.Metadata["sleep_reason"], sleepReasonCityStop)
	}
}

func TestCityRuntimeDemandSnapshotReusesStablePatrolDemand(t *testing.T) {
	buildCalls := 0
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: t.TempDir(),
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
		},
		cs: &controllerState{
			eventProv: events.NewFake(),
		},
		stderr: io.Discard,
	}
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		buildCalls++
		return DesiredStateResult{
			State: map[string]TemplateParams{
				"worker-bd-1": {SessionName: "worker-bd-1"},
			},
		}
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "bead-1",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-bd-1",
			"template":     "worker",
			"state":        "active",
		},
	}})

	first := cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)
	second := cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)

	if buildCalls != 1 {
		t.Fatalf("buildDesiredState call count = %d, want 1 for stable patrol reuse", buildCalls)
	}
	if len(first.result.State) != 1 || len(second.result.State) != 1 {
		t.Fatalf("cached demand snapshot lost desired state: first=%d second=%d", len(first.result.State), len(second.result.State))
	}

	changedSessionBeads := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "bead-1",
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-bd-1",
			"template":             "worker",
			"state":                "active",
			"pending_create_claim": "true",
		},
	}})
	_ = cr.loadDemandSnapshot(changedSessionBeads, nil, "patrol", false)
	if buildCalls != 2 {
		t.Fatalf("buildDesiredState call count after session change = %d, want 2", buildCalls)
	}

	_ = cr.loadDemandSnapshot(changedSessionBeads, nil, "poke", false)
	if buildCalls != 3 {
		t.Fatalf("buildDesiredState call count after poke = %d, want 3", buildCalls)
	}
}

func TestCityRuntimeDemandSnapshotRetainsOnlyPoolScaleCheckPartials(t *testing.T) {
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "session-worker",
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
		},
	}})

	tests := []struct {
		name   string
		result DesiredStateResult
		want   int
	}{
		{
			name: "pool partial retains awake pool session",
			result: DesiredStateResult{
				State:                          map[string]TemplateParams{},
				ScaleCheckCounts:               map[string]int{"worker": 0},
				ScaleCheckPartialTemplates:     map[string]bool{"worker": true},
				PoolScaleCheckPartialTemplates: map[string]bool{"worker": true},
			},
			want: 1,
		},
		{
			name: "named partial does not retain generic pool session",
			result: DesiredStateResult{
				State:                           map[string]TemplateParams{},
				ScaleCheckCounts:                map[string]int{"worker": 0},
				ScaleCheckPartialTemplates:      map[string]bool{"worker": true},
				NamedScaleCheckPartialTemplates: map[string]bool{"worker": true},
			},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cr := &CityRuntime{
				cityName: "test-city",
				cityPath: t.TempDir(),
				cfg: &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Agents: []config.Agent{{
						Name:              "worker",
						MinActiveSessions: intPtr(0),
						MaxActiveSessions: intPtr(5),
					}},
				},
				cs: &controllerState{
					eventProv: events.NewFake(),
				},
				stderr: io.Discard,
			}
			cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
				return tc.result
			}

			snapshot := cr.loadDemandSnapshot(sessionBeads, nil, "poke", false)

			if got := snapshot.result.PoolDesiredCounts["worker"]; got != tc.want {
				t.Fatalf("PoolDesiredCounts[worker] = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCityRuntimeAsyncStartLimiterUsesMaxWakesPerTick(t *testing.T) {
	maxWakes := 7
	cfg := &config.City{Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes}}
	cr := &CityRuntime{cfg: cfg}

	if got := cr.ensureAsyncStartLimiter().capacity(); got != maxWakes {
		t.Fatalf("limiter cap = %d, want %d", got, maxWakes)
	}

	maxWakes = 2
	if got := cr.ensureAsyncStartLimiter().capacity(); got != maxWakes {
		t.Fatalf("limiter cap after config change = %d, want %d", got, maxWakes)
	}
}

func TestCityRuntimeAsyncStartLimiterResizePreservesInFlightBudget(t *testing.T) {
	maxWakes := 3
	cfg := &config.City{Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes}}
	cr := &CityRuntime{cfg: cfg}
	limiter := cr.ensureAsyncStartLimiter()

	var releases []func()
	for i := 0; i < maxWakes; i++ {
		release, reserved, outcome := reserveAsyncStartSlot(context.Background(), limiter)
		if !reserved {
			t.Fatalf("reserve initial slot = %s, want success", outcome)
		}
		releases = append(releases, release)
	}

	maxWakes = 2
	resized := cr.ensureAsyncStartLimiter()
	if resized != limiter {
		t.Fatal("resized limiter should preserve the same in-flight reservation counter")
	}
	if got := resized.capacity(); got != maxWakes {
		t.Fatalf("resized cap = %d, want %d", got, maxWakes)
	}
	if _, reserved, outcome := reserveAsyncStartSlot(context.Background(), resized); reserved || outcome != "deferred_by_async_start_limit" {
		t.Fatalf("reserve while old slots exceed resized cap = reserved %v outcome %q, want deferred", reserved, outcome)
	}

	releases[0]()
	if _, reserved, outcome := reserveAsyncStartSlot(context.Background(), resized); reserved || outcome != "deferred_by_async_start_limit" {
		t.Fatalf("reserve at resized cap = reserved %v outcome %q, want deferred", reserved, outcome)
	}
	releases[1]()
	release, reserved, outcome := reserveAsyncStartSlot(context.Background(), resized)
	if !reserved {
		t.Fatalf("reserve below resized cap = %s, want success", outcome)
	}
	release()
	releases[2]()
}

type recordingOrderDispatcher struct {
	called      atomic.Bool
	calls       atomic.Int32
	onDispatch  func(context.Context, string, time.Time)
	drainCalls  int
	drainCtxErr error
}

func (r *recordingOrderDispatcher) dispatch(ctx context.Context, cityRoot string, now time.Time) {
	r.calls.Add(1)
	r.called.Store(true)
	if r.onDispatch != nil {
		r.onDispatch(ctx, cityRoot, now)
	}
}

func (r *recordingOrderDispatcher) drain(ctx context.Context) bool {
	r.drainCalls++
	r.drainCtxErr = ctx.Err()
	return true
}

type blockingOrderDispatcher struct {
	mu         sync.Mutex
	drainCalls int
	ctxErrs    []error
	release    chan struct{}
	drained    chan struct{}
}

func newBlockingOrderDispatcher() *blockingOrderDispatcher {
	return &blockingOrderDispatcher{
		release: make(chan struct{}),
		drained: make(chan struct{}, 16),
	}
}

func (b *blockingOrderDispatcher) dispatch(context.Context, string, time.Time) {}

func (b *blockingOrderDispatcher) drain(ctx context.Context) bool {
	b.mu.Lock()
	b.drainCalls++
	b.ctxErrs = append(b.ctxErrs, ctx.Err())
	b.mu.Unlock()
	b.drained <- struct{}{}
	select {
	case <-b.release:
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *blockingOrderDispatcher) waitForDrainCalls(t *testing.T, want int) {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	for {
		b.mu.Lock()
		got := b.drainCalls
		b.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-b.drained:
		case <-deadline:
			t.Fatalf("drainCalls = %d, want at least %d", got, want)
		}
	}
}

func (b *blockingOrderDispatcher) drainContextErrors() []error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]error(nil), b.ctxErrs...)
}

func TestCityRuntimeTickDispatchesOrdersBeforeDemandSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	od := &recordingOrderDispatcher{}
	cr := &CityRuntime{
		cityName:            "test-city",
		cityPath:            t.TempDir(),
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		od:                  od,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		if !od.called.Load() {
			t.Fatal("order dispatch should happen before demand snapshot build")
		}
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")

	if !od.called.Load() {
		t.Fatal("order dispatcher was not called")
	}
}

func TestCityRuntimeRunDispatchesOrdersBeforeStartupReconcile(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	od := &recordingOrderDispatcher{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var started atomic.Bool
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			if !od.called.Load() {
				t.Fatal("order dispatch should happen before startup reconcile")
			}
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops: newDrainOps(sp),
		Rec:  events.Discard,
		OnStarted: func() {
			started.Store(true)
			cancel()
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cr.od = od

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if !started.Load() {
		t.Fatal("OnStarted was not called")
	}
	if got := od.calls.Load(); got != 1 {
		t.Fatalf("order dispatch calls = %d, want 1", got)
	}
}

func TestCityRuntimeRunStartupOrderDispatchPanicIsRecovered(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	od := &recordingOrderDispatcher{
		onDispatch: func(context.Context, string, time.Time) {
			panic("startup order boom")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stderr bytes.Buffer
	var started atomic.Bool
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops: newDrainOps(sp),
		Rec:  events.Discard,
		OnStarted: func() {
			started.Store(true)
			cancel()
		},
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	cr.od = od

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if !started.Load() {
		t.Fatal("OnStarted was not called after recovered startup order panic")
	}
	if got := od.calls.Load(); got != 1 {
		t.Fatalf("order dispatch calls = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "trigger=startup-orders") {
		t.Fatalf("stderr = %q, want startup-orders panic trigger", stderr.String())
	}
	if !strings.Contains(stderr.String(), "startup order boom") {
		t.Fatalf("stderr = %q, want recovered panic detail", stderr.String())
	}
}

func TestOrderTrackingSweepWatchdogOnlyClosesSweepOrderTracking(t *testing.T) {
	store := beads.NewMemStore()
	sweepTracking, err := store.Create(beads.Bead{
		Title:  "order:" + orderTrackingSweepOrder,
		Labels: []string{"order-run:" + orderTrackingSweepOrder, labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(sweep): %v", err)
	}
	mergeTracking, err := store.Create(beads.Bead{
		Title:  "order:pr-merge-queue",
		Labels: []string{"order-run:pr-merge-queue", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(merge): %v", err)
	}

	cr := &CityRuntime{
		cityName:            "test-city",
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: store,
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}
	cr.runOrderTrackingSweepWatchdog(time.Now().Add(orderTrackingSweepWatchdogStaleAfter + time.Second))

	gotSweep, err := store.Get(sweepTracking.ID)
	if err != nil {
		t.Fatalf("Get(sweep): %v", err)
	}
	if gotSweep.Status != "closed" {
		t.Fatalf("sweep tracking status = %s, want closed", gotSweep.Status)
	}
	gotMerge, err := store.Get(mergeTracking.ID)
	if err != nil {
		t.Fatalf("Get(merge): %v", err)
	}
	if gotMerge.Status != "open" {
		t.Fatalf("merge tracking status = %s, want open", gotMerge.Status)
	}
}

func TestCityRuntimeDemandSnapshotRefreshesWhenDemandCommandsAreCustom(t *testing.T) {
	cases := []struct {
		name       string
		agent      config.Agent
		wantBuilds int
	}{
		{
			name: "custom scale_check",
			agent: config.Agent{
				Name:       "worker",
				ScaleCheck: "test -f external-queue && echo 1 || echo 0",
			},
			wantBuilds: 2,
		},
		{
			name: "custom work_query",
			agent: config.Agent{
				Name:      "worker",
				WorkQuery: "gh issue list --json number --limit 1",
			},
			wantBuilds: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buildCalls := 0
			cr := &CityRuntime{
				cityName: "test-city",
				cityPath: t.TempDir(),
				cfg: &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Agents:    []config.Agent{tc.agent},
				},
				cs: &controllerState{
					eventProv: events.NewFake(),
				},
				stderr: io.Discard,
			}
			cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
				buildCalls++
				return DesiredStateResult{State: map[string]TemplateParams{}}
			}

			sessionBeads := newSessionBeadSnapshot(nil)
			_ = cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)
			_ = cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)

			if buildCalls != tc.wantBuilds {
				t.Fatalf("buildDesiredState call count = %d, want %d", buildCalls, tc.wantBuilds)
			}
		})
	}
}

func TestCityRuntimeDemandSnapshotDoesNotRunControllerWorkQuery(t *testing.T) {
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: t.TempDir(),
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents: []config.Agent{{
				Name:      "worker",
				WorkQuery: `printf '[{"id":"work-1"}]'`,
			}},
		},
		cs: &controllerState{
			eventProv: events.NewFake(),
		},
		stderr: io.Discard,
	}
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	snapshot := cr.loadDemandSnapshot(newSessionBeadSnapshot(nil), nil, "patrol", false)

	if len(snapshot.result.WorkSet) != 0 {
		t.Fatalf("WorkSet = %#v, want empty; controller demand must not run work_query", snapshot.result.WorkSet)
	}
}

func TestCityRuntimeDemandSnapshotReplaysACPRoutesOnCacheHit(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	sp := sessionauto.New(defaultSP, acpSP)
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: t.TempDir(),
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
		},
		sp: sp,
		cs: &controllerState{
			eventProv: events.NewFake(),
		},
		demandSnapshot: &runtimeDemandSnapshot{
			createdAt:          time.Now(),
			sessionFingerprint: "",
			result: DesiredStateResult{State: map[string]TemplateParams{
				"headless-agent": {
					SessionName: "headless-agent",
					IsACP:       true,
				},
			}},
		},
		stderr: io.Discard,
	}

	_ = cr.loadDemandSnapshot(nil, nil, "patrol", false)

	if err := sp.Attach("headless-agent"); err == nil || !strings.Contains(err.Error(), "ACP transport") {
		t.Fatalf("Attach(headless-agent) error = %v, want ACP transport route", err)
	}
}

// Pool session beads in the "creating" window (tmux not yet up, work not yet
// assigned) must not be swept. Otherwise the sweep runs on the same tick the
// pool creates the bead, observes zero assigned work, and closes it — the
// pool re-spawns on the next tick, same fate, and the pool spins forever
// without a session reaching the ready state.
func TestSweepUndesiredPoolSessionBeads_SkipsCreatingState(t *testing.T) {
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
			"state":                "creating",
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
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — creating state must be preserved", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("bead in creating state was swept closed: %+v", got)
	}
}

// Age grace period: pool session beads that have moved past "creating" but
// are still younger than staleCreatingStateTimeout must not be swept. The
// tmux wake pipeline and work assignment happen across multiple ticks after
// state=creation_complete is set; sweeping in that window causes the same
// spin as sweeping during creation.
func TestSweepUndesiredPoolSessionBeads_SkipsRecentlyCreated(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-recent",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// Post-creating: state/state_reason are advanced but last_woke
			// hasn't landed yet. The real-world state observed as being
			// swept incorrectly.
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
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
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — bead within staleCreatingStateTimeout window must survive", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("recently-created post-creating bead was swept: %+v", got)
	}
}

// Stale creating-state beads (CreatedAt older than staleCreatingStateTimeout)
// MUST be sweepable. Without this, a bead wedged in `creating` past the
// timeout would be permanently immune from this sweep path, breaking the
// symmetry with sessionStartRequested.
func TestSweepUndesiredPoolSessionBeads_SweepsStaleCreatingState(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-stale",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "creating",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead.CreatedAt = time.Now().Add(-2 * time.Minute)
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — stale creating bead must be sweepable", closed)
	}
}

// Stale post-creating beads (state=active, last_woke_at="",
// creation_complete_at older than staleCreatingStateTimeout) MUST be
// sweepable. Without this, the grace window would never expire.
func TestSweepUndesiredPoolSessionBeads_SweepsLongStuckActiveWithoutWake(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-stale-active",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
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
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — bead beyond staleCreatingStateTimeout must be sweepable", closed)
	}
}

// Missing creation_complete_at (older beads predating the per-start marker,
// or beads produced by paths that don't stamp the marker) MUST be sweepable
// rather than protected indefinitely.
func TestSweepUndesiredPoolSessionBeads_SweepsActiveWithoutCreationCompleteAt(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-no-marker",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"state_reason":         "creation_complete",
			// creation_complete_at intentionally absent.
			"continuation_epoch": "1",
			"generation":         "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — bead without creation_complete_at must be sweepable", closed)
	}
}

// The reconciler's healStatePatch rewrites a live bead from state=active
// to state=awake (session_reconcile.go). "awake" is semantically
// equivalent to "active" in this codebase, and both must receive the
// same post-create sweep protection — otherwise the same spin loop
// reopens on the alias path.
func TestSweepUndesiredPoolSessionBeads_SkipsAwakeStateInPreWakeWindow(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-awake",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// healStatePatch rewrote state=active → state=awake while the
			// runtime was alive; the pre-wake condition is preserved
			// because last_woke_at has not yet landed (or was cleared).
			"state":                "awake",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			"last_woke_at":         "",
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
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — state=awake in pre-wake window must receive same protection as state=active", closed)
	}
}

// Recovery of an already-active bead (recoverRunningPendingCreate path:
// state=active + pending_create_claim=true + alive runtime) must produce
// a fresh creation_complete_at so the healed bead stays protected in the
// pre-wake window on the following tick. This test asserts the sweep's
// side of that contract — a state=active bead with a fresh
// creation_complete_at and empty last_woke_at survives the sweep.
func TestSweepUndesiredPoolSessionBeads_SkipsRecoveredActiveBead(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-recovered",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// Post-recovery shape: state was already active, recovery just
			// cleared pending_create_claim and stamped a fresh marker.
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			"last_woke_at":         "",
			// Historical counters survive recovery.
			"wake_attempts":      "1",
			"continuation_epoch": "1",
			"generation":         "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — recovered active bead with fresh marker must survive pre-wake", closed)
	}
}

// Crashed-then-recently-restarted beads: wake_attempts/churn_count are
// preserved across a successful restart (CommitStartedPatch does not reset
// them), so the post-create guard CANNOT be keyed on those counters or a
// legitimate restart after a prior crash would fall into the same spin
// loop. Gating on a fresh creation_complete_at lets a just-restarted bead
// survive the pre-wake window even when its historical counters are
// non-zero.
func TestSweepUndesiredPoolSessionBeads_SkipsFreshRestartAfterPriorCrash(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-restart-after-crash",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// Just-restarted after a prior crash: state transitioned back
			// to active with a fresh creation_complete_at, but historical
			// failure counters remain because clearWakeFailures only fires
			// after the session is stable-long-enough.
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			"wake_attempts":        "2",
			"churn_count":          "1",
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
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — fresh restart after prior crash must survive the pre-wake window", closed)
	}
}

// Crashed beads (state=active, last_woke_at="" cleared by checkStability,
// creation_complete_at stale because the last successful start was long
// ago) MUST be sweepable. checkStability/checkChurn/start-failure do not
// touch creation_complete_at, so an old marker is the signal that the
// state=active+empty-last_woke_at shape came from a crash-clear rather
// than a fresh start.
func TestSweepUndesiredPoolSessionBeads_SweepsCrashedActiveBead(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-crashed",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
			"last_woke_at":         "",
			"wake_attempts":        "1",
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
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — crashed bead with stale creation_complete_at must be swept", closed)
	}
}

func TestSweepUndesiredPoolSessionBeads_SkipsPendingCreateClaim(t *testing.T) {
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
			"pending_create_claim": "true",
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
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — pending_create_claim must be preserved", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("bead with pending_create_claim was swept closed: %+v", got)
	}
}

// #1460: pending_create_claim stays protected only for the pending-create
// lease. Once a never-started create ages past that lease, the sweep must
// reap it instead of preserving the pool slot forever.
func TestSweepUndesiredPoolSessionBeads_SweepsExpiredPendingCreateClaimLease(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Now().UTC()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":              "worker-bd-stale-claim",
			"template":                  "worker",
			"agent_name":                "worker",
			"pool_slot":                 "1",
			"state":                     "creating",
			poolManagedMetadataKey:      boolMetadata(true),
			"pending_create_claim":      "true",
			"pending_create_started_at": pendingCreateStartedAtNow(now.Add(-(pendingCreateNeverStartedTimeout + time.Second))),
			"continuation_epoch":        "1",
			"generation":                "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead.CreatedAt = now.Add(-24 * time.Hour)
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — expired pending_create_claim lease must be reaped", closed)
	}
}

func TestSweepUndesiredPoolSessionBeads_UsesPendingCreateStartedAtForCreatingState(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Now().UTC()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":              "worker-bd-fresh-create",
			"template":                  "worker",
			"agent_name":                "worker",
			"pool_slot":                 "1",
			poolManagedMetadataKey:      boolMetadata(true),
			"state":                     "creating",
			"pending_create_started_at": pendingCreateStartedAtNow(now.Add(-30 * time.Second)),
			"continuation_epoch":        "1",
			"generation":                "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead.CreatedAt = now.Add(-2 * time.Minute)
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — fresh pending_create_started_at must keep old creating bead alive", closed)
	}
}

func TestIsStaleCreatingTreatsZeroPendingCreateStartedAtAsMissing(t *testing.T) {
	now := time.Now().UTC()
	bead := beads.Bead{
		Metadata: map[string]string{
			"state":                     "creating",
			"pending_create_started_at": (time.Time{}).UTC().Format(time.RFC3339),
		},
		CreatedAt: now,
	}

	if isStaleCreating(bead) {
		t.Fatal("zero pending_create_started_at should fall back to fresh CreatedAt")
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
		nil,
		sessionBeads,
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
	if _, err := store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-bd-123",
	}); err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
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
		nil,
		sessionBeads,
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

func TestCityRuntimeBeadReconcileTick_ScaleCheckPartialKeepsOnlyAffectedPoolSession(t *testing.T) {
	store := beads.NewMemStore()
	worker, err := store.Create(beads.Bead{
		ID:     "session-worker",
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
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create worker session: %v", err)
	}
	helper, err := store.Create(beads.Bead{
		ID:     "session-helper",
		Title:  "helper",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:helper"},
		Metadata: map[string]string{
			"session_name":         "helper-bd-123",
			"template":             "helper",
			"agent_name":           "helper",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create helper session: %v", err)
	}

	sp := runtime.NewFake()
	for _, name := range []string{"worker-bd-123", "helper-bd-123"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("Start(%s): %v", name, err)
		}
	}

	cityPath := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{
		{
			Name:              "worker",
			StartCommand:      "echo",
			ScaleCheck:        "exit 42",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		},
		{
			Name:              "helper",
			StartCommand:      "echo",
			ScaleCheck:        "printf 0",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		},
	}}
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "maintainer-city",
		cfg:                 cfg,
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{worker, helper})
	var stderr strings.Builder
	result := buildDesiredStateWithSessionBeads("maintainer-city", cityPath, time.Now().UTC(), cfg, sp, store, nil, snapshot, nil, &stderr)
	if result.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = true, want false for scoped scale_check failure; stderr=%s", stderr.String())
	}
	if !result.ScaleCheckPartialTemplates["worker"] || result.ScaleCheckPartialTemplates["helper"] {
		t.Fatalf("ScaleCheckPartialTemplates = %v, want only worker", result.ScaleCheckPartialTemplates)
	}
	cr.beadReconcileTick(context.Background(), result, snapshot, nil)

	if drain := cr.sessionDrains.get(worker.ID); drain != nil {
		t.Fatalf("affected worker session was scheduled for drain: reason=%s", drain.reason)
	}
	if cr.sessionDrains.get(helper.ID) == nil {
		t.Fatal("unaffected helper session was not scheduled for drain")
	}
	if !sp.IsRunning("worker-bd-123") {
		t.Fatal("affected worker session should remain running")
	}
	if !sp.IsRunning("helper-bd-123") {
		t.Fatal("helper drain should be asynchronous and not stop immediately")
	}
}

func TestCityRuntimeBeadReconcileTick_ScaleCheckPartialPreservesDormantAffectedPoolSessionWithoutDrain(t *testing.T) {
	store := beads.NewMemStore()
	worker, err := store.Create(beads.Bead{
		ID:     "session-worker",
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
			"state":                "asleep",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create worker session: %v", err)
	}

	sp := runtime.NewFake()
	cityPath := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{
		Name:              "worker",
		StartCommand:      "echo",
		ScaleCheck:        "exit 42",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(5),
	}}}
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "maintainer-city",
		cfg:                 cfg,
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{worker})
	var stderr strings.Builder
	result := buildDesiredStateWithSessionBeads("maintainer-city", cityPath, time.Now().UTC(), cfg, sp, store, nil, snapshot, nil, &stderr)
	if _, ok := result.State["worker-bd-123"]; !ok {
		t.Fatalf("affected dormant worker session not preserved in desired state: keys=%v stderr=%s", mapKeys(result.State), stderr.String())
	}

	cr.beadReconcileTick(context.Background(), result, snapshot, nil)

	if drain := cr.sessionDrains.get(worker.ID); drain != nil {
		t.Fatalf("affected dormant worker session was scheduled for drain: reason=%s", drain.reason)
	}
	got, err := store.Get(worker.ID)
	if err != nil {
		t.Fatalf("Get worker session: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("affected dormant worker session was closed: %+v", got)
	}
	if state := got.Metadata["state"]; state != "asleep" {
		t.Fatalf("affected dormant worker state = %q, want asleep", state)
	}
	if sp.IsRunning("worker-bd-123") {
		t.Fatal("affected dormant worker should not be woken by scale_check retention")
	}
}

func TestCityRuntimeBeadReconcileTick_StoreQueryPartialDoesNotReleaseAssignedWork(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		ID:       "ga-live",
		Title:    "live assigned work from partial snapshot",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-session",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work.Status = inProgress

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

	cr.beadReconcileTick(context.Background(), DesiredStateResult{
		State:              map[string]TemplateParams{},
		ScaleCheckCounts:   map[string]int{"worker": 0},
		AssignedWorkBeads:  []beads.Bead{work},
		AssignedWorkStores: []beads.Store{store},
		StoreQueryPartial:  true,
	}, newSessionBeadSnapshot(nil), nil)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work after partial tick: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-session" {
		t.Fatalf("partial assigned-work snapshot released work: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

func TestCityRuntimeBeadReconcileTick_SessionQueryPartialDoesNotReleaseAssignedWork(t *testing.T) {
	base := beads.NewMemStore()
	store := sessionSnapshotListFailStore{Store: base}
	work, err := base.Create(beads.Bead{
		ID:       "ga-live",
		Title:    "live assigned work from partial session snapshot",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-session",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := base.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work.Status = inProgress

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

	cr.beadReconcileTick(context.Background(), DesiredStateResult{
		State:              map[string]TemplateParams{},
		ScaleCheckCounts:   map[string]int{"worker": 0},
		AssignedWorkBeads:  []beads.Bead{work},
		AssignedWorkStores: []beads.Store{store},
	}, nil, nil)

	got, err := base.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work after partial tick: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-session" {
		t.Fatalf("partial session snapshot released work: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

func TestCityRuntimeTick_LogsWispGCPurgeCountWithNonFatalError(t *testing.T) {
	store := beads.NewMemStore()
	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{},
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		wg:                  fixedWispGC{purged: 2, err: fmt.Errorf("delete failed")},
		rec:                 events.Discard,
		logPrefix:           "test-city",
		stdout:              &stdout,
		stderr:              &stderr,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")

	if !strings.Contains(stderr.String(), "test-city: wisp gc: delete failed") {
		t.Fatalf("stderr = %q, want wisp gc error", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Bead GC: purged 2 expired bead(s)") {
		t.Fatalf("stdout = %q, want purge count despite non-fatal error", stdout.String())
	}
}

func TestCityRuntimeTick_PrefixesEachJoinedWispGCErrorLine(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{},
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		wg: fixedWispGC{err: fmt.Errorf("%s\n%s",
			"deleting expired bead \"mol-1\": delete failed",
			"listing closed order-tracking beads: list failed",
		)},
		rec:       events.Discard,
		logPrefix: "test-city",
		stdout:    io.Discard,
		stderr:    &stderr,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")

	got := stderr.String()
	for _, want := range []string{
		"test-city: wisp gc: deleting expired bead \"mol-1\": delete failed\n",
		"test-city: wisp gc: listing closed order-tracking beads: list failed\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr = %q, want line %q", got, want)
		}
	}
}

type fixedWispGC struct {
	purged int
	err    error
}

func (f fixedWispGC) shouldRun(time.Time) bool {
	return true
}

func (f fixedWispGC) runGC(beads.Store, time.Time) (int, error) {
	return f.purged, f.err
}

func TestCityRuntimeBeadReconcileTick_KeepsAssignedPoolWorkerAwake(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/claude"},
		Metadata: map[string]string{
			"session_name":         "claude-real-world-app-live",
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
	if err := sp.Start(context.Background(), "claude-real-world-app-live", runtime.Config{}); err != nil {
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
			workBead("ga-live", "gascity/claude", "claude-real-world-app-live", "in_progress", 5),
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
	if !sp.IsRunning("claude-real-world-app-live") {
		t.Fatal("assigned pool worker should still be running")
	}
}

func TestCityRuntimeBeadReconcileTick_SweepRespectsLiveAssignedWork(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-real-world-app-live",
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

	// Persist an open work bead assigned to the session. GCSweepSessionBeads
	// now runs a live store query via sessionHasOpenAssignedWork, so the
	// bead must live in the store itself — a pre-computed snapshot is no
	// longer consulted.
	if _, err := store.Create(beads.Bead{
		ID:       "ga-future",
		Title:    "future work",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-real-world-app-live",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	}); err != nil {
		t.Fatalf("Create work bead: %v", err)
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
		State:             map[string]TemplateParams{},
		ScaleCheckCounts:  map[string]int{"worker": 0},
		AssignedWorkBeads: []beads.Bead{},
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{session})
	cr.beadReconcileTick(context.Background(), result, sessionBeads, nil)

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead was swept closed despite live assigned work: %+v", got)
	}
}

func TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync(t *testing.T) {
	skipSlowCmdGCTest(t, "runs a full runtime tick/reconcile path; run make test-cmd-gc-process for full coverage")
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

	handlers := computePoolDeathHandlers(cfg, "my-city", cityPath, runtime.NewFake(), nil)
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

func TestCityRuntimeTickSkipsOnDeathWhenSessionListingIsPartial(t *testing.T) {
	cityPath := t.TempDir()
	outFile := filepath.Join(cityPath, "on-death.txt")
	sessionName := "worker-1"

	prevPoolRunning := map[string]bool{sessionName: true}
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:  cityPath,
		cityName:  "my-city",
		logPrefix: "gc start",
		cfg:       &config.City{},
		sp: &partialListPoolProvider{
			Fake:      runtime.NewFake(),
			listNames: []string{},
			listErr:   &runtime.PartialListError{Err: runtime.ErrSessionNotFound},
		},
		standaloneCityStore: beads.NewMemStore(),
		sessionDrains:       newDrainTracker(),
		poolDeathHandlers: map[string]poolDeathInfo{
			sessionName: {
				Command: "printf fired > " + shellQuotePath(outFile),
				Dir:     cityPath,
			},
		},
		rec:    events.Discard,
		stdout: io.Discard,
		stderr: &stderr,
		buildFnWithSessionBeads: func(_ *config.City, _ runtime.Provider, _ beads.Store, _ map[string]beads.Store, _ *sessionBeadSnapshot, _ *sessionReconcilerTraceCycle) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	dirty := &atomic.Bool{}
	var lastProviderName string
	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "test")

	if _, err := os.Stat(outFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("on_death output err = %v, want no hook execution", err)
	}
	if !prevPoolRunning[sessionName] {
		t.Fatalf("prevPoolRunning[%q] = false, want previous state preserved on partial list", sessionName)
	}
	if !strings.Contains(stderr.String(), "pool death check skipped due to partial session listing") {
		t.Fatalf("stderr = %q, want partial-list warning", stderr.String())
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
	cr := newTestCityRuntime(t, CityRuntimeParams{
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

func TestCityRuntimeReloadProviderSwapFailsOnPartialSessionListing(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := &partialListPoolProvider{
		Fake:    runtime.NewFake(),
		listErr: &runtime.PartialListError{Err: runtime.ErrSessionNotFound},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
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

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want fake", lastProviderName)
	}
	if strings.Contains(stdout.String(), "Session provider swapped") {
		t.Fatalf("stdout = %q, want no provider swap message", stdout.String())
	}
}

func TestCityRuntimeReloadProviderSwapFailsOnSessionListingError(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := &partialListPoolProvider{
		Fake:    runtime.NewFake(),
		listErr: errors.New("backend unavailable"),
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
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

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want fake", lastProviderName)
	}
	if strings.Contains(stdout.String(), "Session provider swapped") {
		t.Fatalf("stdout = %q, want no provider swap message", stdout.String())
	}
}

func TestCityRuntimeReloadAllowsRegistryAliasDifferentFromWorkspaceName(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfigNamed(t, tomlPath, "workspace-name", "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "machine-alias",
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

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "machine-alias", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	writeCityRuntimeConfigNamed(t, tomlPath, "workspace-name", "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if lastProviderName != "fail" {
		t.Fatalf("lastProviderName = %q, want fail; stderr=%q", lastProviderName, stderr.String())
	}
	if strings.Contains(stderr.String(), "workspace.name changed") {
		t.Fatalf("reload treated registry alias as workspace drift: %s", stderr.String())
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
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
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

	data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "config reload: boom") {
		t.Fatalf("reply.Error = %q, want lifecycle error", reply.Error)
	}
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
	if !strings.Contains(stderr.String(), "config reload: boom (keeping old config)") {
		t.Fatalf("stderr = %q, want lifecycle failure", stderr.String())
	}
	if strings.Contains(stdout.String(), "Session provider swapped") {
		t.Fatalf("stdout = %q, want no provider swap message", stdout.String())
	}
	if strings.Contains(stdout.String(), "Config reloaded:") {
		t.Fatalf("stdout = %q, want no reload success message", stdout.String())
	}
}

func TestCityRuntimeReloadRetriesTransientLifecycleFailure(t *testing.T) {
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
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
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
	oldRev := cr.configRev

	prevStart := cityRuntimeStartBeadsLifecycle
	prevDelay := cityRuntimeReloadLifecycleRetryDelay
	var calls int
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("init city beads: exec beads init: signal: terminated")
		}
		return nil
	}
	cityRuntimeReloadLifecycleRetryDelay = 0
	t.Cleanup(func() {
		cityRuntimeStartBeadsLifecycle = prevStart
		cityRuntimeReloadLifecycleRetryDelay = prevDelay
	})

	data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if calls != 2 {
		t.Fatalf("cityRuntimeStartBeadsLifecycle calls = %d, want 2", calls)
	}
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeApplied)
	}
	if reply.Error != "" {
		t.Fatalf("reply.Error = %q, want empty", reply.Error)
	}
	if !warningsContain(reply.Warnings, "transient bead lifecycle failure") {
		t.Fatalf("reply.Warnings = %v, want transient retry warning", reply.Warnings)
	}
	if cr.cfg == oldCfg {
		t.Fatal("cfg did not change after successful retry")
	}
	if cr.configRev == oldRev {
		t.Fatalf("configRev = %q, want new revision", cr.configRev)
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want fake", lastProviderName)
	}
	if !strings.Contains(stdout.String(), "Config reloaded:") {
		t.Fatalf("stdout = %q, want reload success message", stdout.String())
	}
	if strings.Contains(stderr.String(), "keeping old config") {
		t.Fatalf("stderr = %q, want no reload failure", stderr.String())
	}
}

func TestCityRuntimeReloadStrictWarningsReturnedOnFailure(t *testing.T) {
	oldStrict := strictMode
	strictMode = true
	t.Cleanup(func() { strictMode = oldStrict })

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
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err := os.WriteFile(tomlPath, []byte(`include = ["override.toml"]

[workspace]
name = "test-city"
install_agent_hooks = ["claude"]

[session]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "override.toml"), []byte(`[workspace]
install_agent_hooks = ["codex"]
`), 0o644); err != nil {
		t.Fatalf("write override.toml: %v", err)
	}

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "strict mode: 1 collision warning(s)") {
		t.Fatalf("reply.Error = %q", reply.Error)
	}
	if !warningsContain(reply.Warnings, "workspace.install_agent_hooks redefined") {
		t.Fatalf("reply.Warnings = %v, want collision warning", reply.Warnings)
	}
	if !warningsContain(reply.Warnings, reloadStrictWarningHint) {
		t.Fatalf("reply.Warnings = %v, want strict recovery hint", reply.Warnings)
	}
	if !strings.Contains(stderr.String(), "gc reload: warning: workspace.install_agent_hooks redefined") {
		t.Fatalf("stderr = %q, want warning details", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc reload: warning: "+reloadStrictWarningHint) {
		t.Fatalf("stderr = %q, want strict recovery hint", stderr.String())
	}
	if strings.Contains(stderr.String(), "gc start:") {
		t.Fatalf("stderr = %q, want reload-specific prefix without gc start", stderr.String())
	}
}

func TestCityRuntimeReloadNonStrictWarningsReturnedOnValidationFailure(t *testing.T) {
	oldStrict := strictMode
	strictMode = false
	t.Cleanup(func() { strictMode = oldStrict })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	oldCfg := cr.cfg

	if err := os.WriteFile(tomlPath, []byte(`include = ["override.toml"]

[workspace]
name = "test-city"
install_agent_hooks = ["claude"]

[session]
provider = "fake"

[[agent]]
name = "bad name"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "override.toml"), []byte(`[workspace]
install_agent_hooks = ["codex"]
`), 0o644); err != nil {
		t.Fatalf("write override.toml: %v", err)
	}

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "validating agents") {
		t.Fatalf("reply.Error = %q, want validation failure", reply.Error)
	}
	if !warningsContain(reply.Warnings, "workspace.install_agent_hooks redefined") {
		t.Fatalf("reply.Warnings = %v, want composition warning", reply.Warnings)
	}
	if !strings.Contains(stderr.String(), "gc reload: warning: workspace.install_agent_hooks redefined") {
		t.Fatalf("stderr = %q, want warning details", stderr.String())
	}
	if cr.cfg != oldCfg {
		t.Fatal("cfg changed after validation reload failure")
	}
}

func TestCityRuntimeFailActiveReloadRepliesAndClears(t *testing.T) {
	doneCh := make(chan reloadControlReply, 1)
	cr := &CityRuntime{
		activeReload: &reloadRequest{doneCh: doneCh},
	}

	cr.failActiveReload("Reload canceled because the controller is shutting down.")

	if cr.activeReload != nil {
		t.Fatal("activeReload was not cleared")
	}
	select {
	case reply := <-doneCh:
		if reply.Outcome != reloadOutcomeFailed {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
		}
		if !strings.Contains(reply.Error, "shutting down") {
			t.Fatalf("reply.Error = %q, want shutdown reason", reply.Error)
		}
	default:
		t.Fatal("active reload did not receive cancellation reply")
	}
}

func TestCityRuntimeHandleReloadRequestInitializesConfigDirty(t *testing.T) {
	acceptedCh := make(chan reloadControlReply, 1)
	req := &reloadRequest{
		acceptedCh: acceptedCh,
		doneCh:     make(chan reloadControlReply, 1),
	}
	cr := &CityRuntime{
		pokeCh: make(chan struct{}, 1),
	}

	cr.handleReloadRequest(req)

	if cr.configDirty == nil {
		t.Fatal("configDirty was not initialized")
	}
	if !cr.configDirty.Load() {
		t.Fatal("configDirty = false, want reload request to mark dirty")
	}
	if cr.activeReload != req {
		t.Fatal("activeReload was not recorded")
	}
	select {
	case <-cr.pokeCh:
	default:
		t.Fatal("reload request did not enqueue poke")
	}
	select {
	case reply := <-acceptedCh:
		if reply.Outcome != reloadOutcomeAccepted {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeAccepted)
		}
	default:
		t.Fatal("reload request did not receive accepted reply")
	}
}

func TestCityRuntimeReloadSameRevisionIsNoOp(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)

	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
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

func TestCityRuntimeReloadRetainsTimedOutDispatcherForShutdownDrain(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)

	od := newBlockingOrderDispatcher()
	var stdout bytes.Buffer
	cr := &CityRuntime{
		cityPath:   cityPath,
		cityName:   "test-city",
		tomlPath:   tomlPath,
		configRev:  configRev,
		cfg:        cfg,
		sp:         runtime.NewFake(),
		dops:       newDrainOps(runtime.NewFake()),
		od:         od,
		rec:        events.Discard,
		logPrefix:  "gc start",
		stdout:     &stdout,
		stderr:     io.Discard,
		configName: "test-city",
	}

	writeCityRuntimeConfigWithShutdownTimeout(t, tomlPath, "fake", "1s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lastProviderName := "fake"
	cr.reloadConfig(ctx, &lastProviderName, cityPath)
	od.waitForDrainCalls(t, 1)

	shutdownDone := make(chan struct{})
	go func() {
		cr.shutdown()
		close(shutdownDone)
	}()
	od.waitForDrainCalls(t, 2)
	close(od.release)
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not return after retained dispatcher was released")
	}
}

func TestCityRuntimeReloadDrainShortCircuitsOnTickContextCancel(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	od := newBlockingOrderDispatcher()
	cr := &CityRuntime{
		cityPath:   cityPath,
		cityName:   "test-city",
		tomlPath:   tomlPath,
		configRev:  configRev,
		cfg:        cfg,
		sp:         runtime.NewFake(),
		dops:       newDrainOps(runtime.NewFake()),
		od:         od,
		rec:        events.Discard,
		logPrefix:  "gc start",
		stdout:     io.Discard,
		stderr:     io.Discard,
		configName: "test-city",
	}

	writeCityRuntimeConfigWithShutdownTimeout(t, tomlPath, "fake", "1s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lastProviderName := "fake"
	start := time.Now()
	cr.reloadConfig(ctx, &lastProviderName, cityPath)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("reload drain took %s after tick context cancellation, want <200ms", elapsed)
	}
	errs := od.drainContextErrors()
	if len(errs) == 0 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("drain ctx error = %v, want context.Canceled", errs)
	}
	close(od.release)
}

func TestCityRuntimeReloadDrainBoundedByTimeout(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	od := newBlockingOrderDispatcher()
	cr := &CityRuntime{
		cityPath:   cityPath,
		cityName:   "test-city",
		tomlPath:   tomlPath,
		configRev:  configRev,
		cfg:        cfg,
		sp:         runtime.NewFake(),
		dops:       newDrainOps(runtime.NewFake()),
		od:         od,
		rec:        events.Discard,
		logPrefix:  "gc start",
		stdout:     io.Discard,
		stderr:     io.Discard,
		configName: "test-city",
	}

	writeCityRuntimeConfigWithShutdownTimeout(t, tomlPath, "fake", "1s")
	lastProviderName := "fake"
	start := time.Now()
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)
	elapsed := time.Since(start)
	if elapsed < reloadOrderDrainTimeout || elapsed > reloadOrderDrainTimeout+500*time.Millisecond {
		t.Fatalf("reload elapsed = %s, want bounded near %s", elapsed, reloadOrderDrainTimeout)
	}
	close(od.release)
}

func TestCityRuntimeRunReloadsConfigBeforeStartupReconcile(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	if err := os.WriteFile(tomlPath, []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "fake"

[[agent]]
name = "fresh-agent"
`), 0o644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var sawFreshAgent atomic.Bool
	cr := newCityRuntime(CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(cfg *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
			for _, agent := range cfg.Agents {
				if agent.Name == "fresh-agent" {
					sawFreshAgent.Store(true)
				}
			}
			cancel()
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if !sawFreshAgent.Load() {
		t.Fatalf("startup did not see reloaded fresh-agent; agents = %#v", cr.cfg.Agents)
	}
}

func TestNewCityRuntimeUsesRegisteredAliasForEffectiveIdentity(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfigNamed(t, tomlPath, "declared-city", "fake")

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "machine-alias",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})

	if got := cr.cfg.EffectiveCityName(); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cr.cfg); got != "ma" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "ma")
	}
	if got := cr.cfg.Workspace.Name; got != "declared-city" {
		t.Fatalf("Workspace.Name = %q, want %q", got, "declared-city")
	}
}

func TestCityRuntimeReloadKeepsRegisteredAliasForEffectiveIdentity(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfigNamed(t, tomlPath, "declared-city", "fake")

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)

	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "machine-alias",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})

	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(tomlPath, append(data, []byte("\n# reload\n")...), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if got := cr.cfg.EffectiveCityName(); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() after reload = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cr.cfg); got != "ma" {
		t.Fatalf("EffectiveHQPrefix() after reload = %q, want %q", got, "ma")
	}
	if got := cr.cfg.Workspace.Name; got != "declared-city" {
		t.Fatalf("Workspace.Name after reload = %q, want %q", got, "declared-city")
	}
	if cr.configRev == configRev {
		t.Fatal("configRev did not change after accepted reload")
	}
}

func TestCityRuntimeManualReloadReplyWaitsForTickCompletion(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)

	doneCh := make(chan reloadControlReply, 1)
	dirty := &atomic.Bool{}
	dirty.Store(true)
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:    cityPath,
		CityName:    "test-city",
		TomlPath:    tomlPath,
		ConfigRev:   configRev,
		ConfigDirty: dirty,
		Cfg:         cfg,
		SP:          sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			select {
			case reply := <-doneCh:
				t.Fatalf("manual reload replied before desired-state rebuild: %+v", reply)
			default:
			}
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	t.Cleanup(cr.shutdown)
	cr.activeReload = &reloadRequest{doneCh: doneCh}
	lastProviderName := "fake"
	var prevPoolRunning map[string]bool

	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "poke")

	select {
	case reply := <-doneCh:
		if reply.Outcome != reloadOutcomeNoChange {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeNoChange)
		}
	default:
		t.Fatal("manual reload did not reply after tick completion")
	}
	if cr.activeReload != nil {
		t.Fatal("activeReload was not cleared")
	}
}

func TestCityRuntimeReloadRestartsConfigWatcherWithNewPackTargets(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfigWithIncludes(t, tomlPath, nil)

	packFile := filepath.Join(cityPath, "packs", "extra", "docs", "note.txt")
	if err := os.MkdirAll(filepath.Dir(packFile), 0o755); err != nil {
		t.Fatalf("mkdir pack docs: %v", err)
	}
	if err := os.WriteFile(packFile, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("seed pack file: %v", err)
	}

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	sp := runtime.NewFake()
	dirty := &atomic.Bool{}
	pokeCh := make(chan struct{}, 8)
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:     cityPath,
		CityName:     "test-city",
		TomlPath:     tomlPath,
		WatchTargets: config.WatchTargets(prov, cfg, cityPath),
		ConfigRev:    configRev,
		ConfigDirty:  dirty,
		Cfg:          cfg,
		SP:           sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		PokeCh: pokeCh,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	cr.restartConfigWatcher()
	defer cr.stopConfigWatcher()

	writeCityRuntimeConfigWithIncludes(t, tomlPath, []string{"packs/extra"})
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	packDir := filepath.Join(cityPath, "packs", "extra")
	foundPackTarget := false
	for _, target := range cr.watchTargets {
		if target.Path == packDir && target.Recursive {
			foundPackTarget = true
			break
		}
	}
	if !foundPackTarget {
		t.Fatalf("watchTargets = %#v, want recursive pack target %q", cr.watchTargets, packDir)
	}

	drainPokes := func() {
		for {
			select {
			case <-pokeCh:
			default:
				return
			}
		}
	}
	time.Sleep(25 * time.Millisecond)
	drainPokes()
	dirty.Store(false)

	if err := os.WriteFile(packFile, []byte("second\n"), 0o644); err != nil {
		t.Fatalf("edit pack file: %v", err)
	}
	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for watcher poke after editing newly watched pack file; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after editing newly watched pack file; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCityRuntimeManualReloadPanicAfterReloadKeepsReloadReplyAndClears(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)

	doneCh := make(chan reloadControlReply, 1)
	dirty := &atomic.Bool{}
	dirty.Store(true)
	sp := runtime.NewFake()
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:    cityPath,
		CityName:    "test-city",
		TomlPath:    tomlPath,
		ConfigRev:   configRev,
		ConfigDirty: dirty,
		Cfg:         cfg,
		SP:          sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			panic("manual reload boom")
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	cr.activeReload = &reloadRequest{doneCh: doneCh}
	lastProviderName := "fake"
	var prevPoolRunning map[string]bool

	cr.safeTick(func() {
		cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "poke")
	}, "poke")

	if cr.activeReload != nil {
		t.Fatal("activeReload was not cleared after recovered panic")
	}
	select {
	case reply := <-doneCh:
		if reply.Outcome != reloadOutcomeNoChange {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeNoChange)
		}
	default:
		t.Fatal("manual reload did not receive reload reply after recovered panic")
	}
	if !strings.Contains(stderr.String(), "manual reload boom") {
		t.Fatalf("stderr = %q, want recovered panic log", stderr.String())
	}
}

func TestCityRuntimeWatchReloadPanicRestoresDirty(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	dirty := &atomic.Bool{}
	dirty.Store(true)
	sp := runtime.NewFake()
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:    cityPath,
		CityName:    "test-city",
		TomlPath:    tomlPath,
		ConfigRev:   configRev,
		ConfigDirty: dirty,
		Cfg:         cfg,
		SP:          sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			panic("watch reload boom")
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	lastProviderName := "fake"
	var prevPoolRunning map[string]bool

	cr.safeTick(func() {
		cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "patrol")
	}, "patrol")

	if !dirty.Load() {
		t.Fatal("dirty flag was not restored after recovered watch reload panic")
	}
	if !strings.Contains(stderr.String(), "watch reload boom") {
		t.Fatalf("stderr = %q, want recovered panic log", stderr.String())
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
	od := &recordingOrderDispatcher{}

	ctx, cancel := context.WithCancel(context.Background())
	cr := newTestCityRuntime(t, CityRuntimeParams{
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
	cr.od = od

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if started {
		t.Fatal("OnStarted called after cancellation")
	}
	if got := od.calls.Load(); got != 1 {
		t.Fatalf("order dispatch calls = %d, want startup dispatch before cancellation", got)
	}
	if strings.Contains(stdout.String(), "City started.") {
		t.Fatalf("stdout = %q, want no started banner after cancellation", stdout.String())
	}
}

// safeTick must swallow panics so a transient failure in the reconciler
// tick body (e.g. Dolt EOF triggering a downstream nil deref) does not
// cascade through the supervisor's per-city panic recovery into
// cityRuntime.shutdown() -> gracefulStopAll (issue #663).
func TestCityRuntimeSafeTick_RecoversFromPanicAndLogsTrigger(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityName:  "test-city",
		logPrefix: "test-city",
		stderr:    &stderr,
	}

	called := false
	cr.safeTick(func() {
		called = true
		panic("simulated dolt eof cascade")
	}, "patrol")

	if !called {
		t.Fatal("tick body was not invoked")
	}
	got := stderr.String()
	if !strings.Contains(got, "panicked") {
		t.Errorf("stderr = %q, want to contain 'panicked'", got)
	}
	if !strings.Contains(got, "trigger=patrol") {
		t.Errorf("stderr = %q, want to contain 'trigger=patrol'", got)
	}
	if !strings.Contains(got, "simulated dolt eof cascade") {
		t.Errorf("stderr = %q, want to contain panic payload", got)
	}
	if !strings.Contains(got, "type=string") {
		t.Errorf("stderr = %q, want to contain panic value type (helps distinguish errors from strings)", got)
	}
	if !strings.Contains(got, "goroutine ") {
		t.Errorf("stderr = %q, want to contain a stack trace so latent bugs stay diagnosable", got)
	}
}

// safeTick must forward normal (non-panicking) returns unchanged so the
// wrapper is transparent in the common case.
func TestCityRuntimeSafeTick_PassesThroughWhenNoPanic(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityName:  "test-city",
		logPrefix: "test-city",
		stderr:    &stderr,
	}
	called := false
	cr.safeTick(func() { called = true }, "poke")
	if !called {
		t.Fatal("tick body was not invoked")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on clean tick", stderr.String())
	}
}

// A panic during startup reconciliation must NOT cause run() to exit
// or call shutdown(): the supervisor loop must survive a transient
// bead-store failure (or the nil deref it would trigger) without
// restarting the whole city. Regression for #663.
//
// Sequence: first BuildFn call fires inside the startup safeTick
// closure and panics — safeTick recovers, trace ends Aborted via
// defer. Because configDirty is still true, the post-startup
// startup-poke branch invokes cr.tick(), which calls BuildFn a second
// time; that call cancels ctx and run() exits cleanly.
func TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.PatrolInterval = "1ms"
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer

	var buildCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			n := buildCalls.Add(1)
			if n == 1 {
				panic("simulated dolt eof nil deref")
			}
			cancel()
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

	// Prime configDirty so the post-startup startup-poke branch fires
	// cr.tick() and drives a second BuildFn call that cancels ctx.
	var dirty atomic.Bool
	dirty.Store(true)
	cr.configDirty = &dirty

	done := make(chan struct{})
	go func() {
		cr.run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("run did not return within 5s after panic+cancel")
	}

	if buildCalls.Load() < 2 {
		t.Fatalf("BuildFn invoked %d time(s), want >= 2 (startup panic + startup-poke recovery)", buildCalls.Load())
	}
	if !strings.Contains(stderr.String(), "panicked") {
		t.Errorf("stderr = %q, want to contain 'panicked' (safeTick must log)", stderr.String())
	}
}

func TestCityRuntimeRun_RetriesStartupAfterRecoveredPanicBeforeStarted(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.PatrolInterval = "1ms"
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer

	var buildCalls atomic.Int32
	var started atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			n := buildCalls.Add(1)
			if n == 1 {
				panic("simulated startup panic")
			}
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops: newDrainOps(sp),
		Rec:  events.Discard,
		OnStarted: func() {
			started.Store(true)
			cancel()
		},
		Stdout: &stdout,
		Stderr: &stderr,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	done := make(chan struct{})
	go func() {
		cr.run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("run did not return within 5s after startup retry")
	}

	if buildCalls.Load() < 2 {
		t.Fatalf("BuildFn invoked %d time(s), want startup retry after recovered panic", buildCalls.Load())
	}
	if !started.Load() {
		t.Fatal("OnStarted was not called after successful startup retry")
	}
	if !strings.Contains(stdout.String(), "City started.") {
		t.Fatalf("stdout = %q, want started banner after retry", stdout.String())
	}
	if !strings.Contains(stderr.String(), "simulated startup panic") {
		t.Fatalf("stderr = %q, want recovered startup panic log", stderr.String())
	}
}

type panicOnceConvergenceListStore struct {
	beads.Store
	panicked atomic.Bool
}

func (s *panicOnceConvergenceListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == "convergence" && !s.panicked.Swap(true) {
		panic("convergence startup list boom")
	}
	return s.Store.List(query)
}

type errorConvergenceListStore struct {
	beads.Store
}

func (s *errorConvergenceListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == "convergence" {
		return nil, fmt.Errorf("convergence list unavailable")
	}
	return s.Store.List(query)
}

func TestCityRuntimeRun_ConvergenceStartupErrorDoesNotBlockStarted(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.PatrolInterval = "1ms"
	sp := runtime.NewFake()
	store := &errorConvergenceListStore{Store: beads.NewMemStore()}
	var stderr bytes.Buffer
	var started atomic.Bool

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:             newDrainOps(sp),
		Rec:              events.Discard,
		ConvergenceReqCh: make(chan convergenceRequest, 1),
		OnStarted: func() {
			started.Store(true)
			cancel()
		},
		Stdout: io.Discard,
		Stderr: &stderr,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)

	done := make(chan struct{})
	go func() {
		cr.run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("run did not return after convergence startup list error")
	}
	if !started.Load() {
		t.Fatal("OnStarted was not called after non-panic convergence startup error")
	}
	if !strings.Contains(stderr.String(), "convergence list unavailable") {
		t.Fatalf("stderr = %q, want convergence list error", stderr.String())
	}
}

func TestCityRuntimeRun_RetriesConvergenceStartupUntilIndexPopulated(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.PatrolInterval = "1ms"
	sp := runtime.NewFake()
	store := &panicOnceConvergenceListStore{Store: beads.NewMemStore()}
	var stderr bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:             newDrainOps(sp),
		Rec:              events.Discard,
		ConvergenceReqCh: make(chan convergenceRequest, 1),
		Stdout:           io.Discard,
		Stderr:           &stderr,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)

	done := make(chan struct{})
	go func() {
		cr.run(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for {
		if cr.convStoreAdapter != nil && cr.convStoreAdapter.activeIndex != nil {
			cancel()
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("convergence active index was not populated after retry")
		case <-time.After(time.Millisecond):
		}
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after convergence retry test cancellation")
	}
	if !store.panicked.Load() {
		t.Fatal("test store did not inject convergence startup panic")
	}
	if !strings.Contains(stderr.String(), "convergence startup list boom") {
		t.Fatalf("stderr = %q, want recovered convergence startup panic log", stderr.String())
	}
}

func TestCityRuntimeRunShutsDownSessionsOnContextCancel(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.ShutdownTimeout = "20ms"

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "probe-session", runtime.Config{}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	var stdout bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			cancel()
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

	cr.run(ctx)

	if sp.IsRunning("probe-session") {
		t.Fatal("probe-session still running after runtime cancellation")
	}

	var stopCalls int
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == "probe-session" {
			stopCalls++
		}
	}
	if stopCalls == 0 {
		t.Fatalf("expected forced stop during shutdown, calls=%+v", sp.Calls)
	}
	if !strings.Contains(stdout.String(), "Stopped agent 'probe-session'") {
		t.Fatalf("stdout = %q, want shutdown stop message", stdout.String())
	}
}

// orderingFakeProvider appends "stop:<name>" to seq when Stop is called so
// tests can assert ordering relative to other lifecycle events.
type orderingFakeProvider struct {
	*runtime.Fake
	mu  sync.Mutex
	seq []string
}

func (p *orderingFakeProvider) Stop(name string) error {
	p.mu.Lock()
	p.seq = append(p.seq, "stop:"+name)
	p.mu.Unlock()
	return p.Fake.Stop(name)
}

func (p *orderingFakeProvider) events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seq...)
}

type interruptStopsProvider struct {
	*runtime.Fake
}

func (p *interruptStopsProvider) Interrupt(name string) error {
	if err := p.Fake.Interrupt(name); err != nil {
		return err
	}
	return p.Stop(name)
}

// TestCityRuntimeShutdownDrainsOrderDispatch verifies shutdown invokes
// orderDispatcher.drain with a fresh (non-canceled) context before
// stopping sessions — regression for #991.
func TestCityRuntimeShutdownDrainsOrderDispatch(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.ShutdownTimeout = "1s"

	sp := runtime.NewFake()
	od := &recordingOrderDispatcher{}

	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:       cfg,
		sp:        sp,
		od:        od,
		rec:       events.Discard,
		logPrefix: "gc start",
		stdout:    &stdout,
		stderr:    &stderr,
	}

	cr.shutdown()

	if od.drainCalls != 1 {
		t.Fatalf("drainCalls = %d, want 1", od.drainCalls)
	}
	if od.drainCtxErr != nil {
		t.Fatalf("drain received a canceled ctx (%v); shutdown must pass a fresh context", od.drainCtxErr)
	}
}

func TestCityRuntimeShutdownPreservesFullGracefulBudgetWithOrders(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.ShutdownTimeout = "1s"

	sp := &interruptStopsProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "probe", runtime.Config{}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	od := &recordingOrderDispatcher{}

	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:       cfg,
		sp:        sp,
		od:        od,
		rec:       events.Discard,
		logPrefix: "gc start",
		stdout:    &stdout,
		stderr:    &stderr,
	}

	cr.shutdown()

	if !strings.Contains(stdout.String(), "waiting 1s") {
		t.Fatalf("stdout = %q, want full 1s graceful session budget", stdout.String())
	}
}

// TestCityRuntimeShutdownBlockedDispatchPersistsOutcomeBeforeGracefulStop
// is the AC regression for #991: "a blocked/fake dispatch cannot let
// controller exit before the tracking bead is closed or failure metadata
// is persisted." It starts a real memoryOrderDispatcher, wedges its exec
// until after shutdown is invoked, and asserts both that the tracking
// bead is closed before shutdown returns AND that session Stop happens
// AFTER the dispatch finishes — proving drain blocks gracefulStopAll.
func TestCityRuntimeShutdownBlockedDispatchPersistsOutcomeBeforeGracefulStop(t *testing.T) {
	store := beads.NewMemStore()
	release := make(chan struct{})
	execStarted := make(chan struct{})
	execDone := make(chan struct{})

	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		close(execStarted)
		<-release
		close(execDone)
		return []byte("ok\n"), nil
	}

	ad := buildOrderDispatcherFromListExec(
		[]orders.Order{{Name: "blocked", Trigger: "cooldown", Interval: "2m", Exec: "scripts/blocked.sh"}},
		store, nil, fakeExec, nil,
	)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	<-execStarted

	sp := &orderingFakeProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "probe", runtime.Config{}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	cfg := &config.City{}
	cfg.Daemon.ShutdownTimeout = "200ms"

	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:       cfg,
		sp:        sp,
		od:        ad,
		rec:       events.Discard,
		logPrefix: "gc start",
		stdout:    &stdout,
		stderr:    &stderr,
	}

	shutdownDone := make(chan struct{})
	go func() {
		cr.shutdown()
		close(shutdownDone)
	}()

	// shutdown must not return while exec is blocked.
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before drain waited for in-flight dispatch")
	case <-time.After(100 * time.Millisecond):
	}

	// Session must not have been stopped yet — drain is still waiting.
	if got := sp.events(); len(got) != 0 {
		t.Fatalf("session lifecycle ran before drain completed: %v", got)
	}

	close(release)
	<-execDone

	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return after dispatch completed")
	}

	// Tracking bead outcome must be persisted before shutdown returned.
	all, err := store.ListByLabel("order-run:blocked", 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	foundExecLabel := false
	for _, b := range all {
		for _, l := range b.Labels {
			if l == "exec" {
				foundExecLabel = true
			}
		}
	}
	if !foundExecLabel {
		t.Fatalf("tracking bead missing exec outcome label after shutdown; beads=%+v", all)
	}

	// gracefulStopAll must have run after drain.
	got := sp.events()
	if len(got) == 0 || got[0] != "stop:probe" {
		t.Fatalf("expected stop:probe after drain, got %v", got)
	}
}

func TestCityRuntimeShutdownPreservesFullGracefulBudgetWhenNoOrders(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.ShutdownTimeout = "1s"

	sp := &interruptStopsProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "probe", runtime.Config{}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:       cfg,
		sp:        sp,
		rec:       events.Discard,
		logPrefix: "gc start",
		stdout:    &stdout,
		stderr:    &stderr,
	}

	cr.shutdown()

	if !strings.Contains(stdout.String(), "waiting 1s") {
		t.Fatalf("stdout = %q, want full 1s graceful session budget", stdout.String())
	}
}

func TestCityRuntimeShutdownZeroTimeoutDoesNotWaitForOrderDrain(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.ShutdownTimeout = "0s"

	od := newBlockingOrderDispatcher()
	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:       cfg,
		sp:        runtime.NewFake(),
		od:        od,
		rec:       events.Discard,
		logPrefix: "gc start",
		stdout:    &stdout,
		stderr:    &stderr,
	}

	done := make(chan struct{})
	go func() {
		cr.shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("shutdown waited on order drain despite shutdown_timeout=0s")
	}
	close(od.release)
}

func TestCityRuntimeShutdownWarnsWhenSessionListingIsPartial(t *testing.T) {
	sp := &partialListPoolProvider{
		Fake:      runtime.NewFake(),
		listNames: []string{"visible"},
		listErr:   &runtime.PartialListError{Err: runtime.ErrSessionNotFound},
	}
	if err := sp.Start(context.Background(), "visible", runtime.Config{}); err != nil {
		t.Fatalf("start visible session: %v", err)
	}
	if err := sp.Start(context.Background(), "hidden", runtime.Config{}); err != nil {
		t.Fatalf("start hidden session: %v", err)
	}

	cfg := &config.City{}
	cfg.Daemon.ShutdownTimeout = "0s"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cfg:       cfg,
		sp:        sp,
		rec:       events.Discard,
		logPrefix: "gc start",
		stdout:    &stdout,
		stderr:    &stderr,
	}

	cr.shutdown()

	if sp.IsRunning("visible") {
		t.Fatal("visible session still running after shutdown")
	}
	if !sp.IsRunning("hidden") {
		t.Fatal("hidden session unexpectedly stopped; partial listing should only stop visible sessions")
	}
	if !strings.Contains(stderr.String(), "shutdown session listing partially failed") {
		t.Fatalf("stderr = %q, want partial-list shutdown warning", stderr.String())
	}
}

func writeCityRuntimeConfig(t *testing.T, tomlPath, provider string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	writeCityRuntimeConfigNamed(t, tomlPath, "test-city", provider)
}

func loadCityRuntimeControllerConfig(t *testing.T, cityPath string) (*config.City, string) {
	t.Helper()
	cfg, prov, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	applyFeatureFlags(cfg)
	return cfg, config.Revision(fsys.OSFS{}, prov, cfg, cityPath)
}

func writeCityRuntimeConfigNamed(t *testing.T, tomlPath, name, provider string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	data := []byte("[workspace]\nname = \"" + name + "\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"" + provider + "\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func writeCityRuntimeConfigWithShutdownTimeout(t *testing.T, tomlPath, provider, timeout string) {
	t.Helper()
	data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"" + provider + "\"\n\n[daemon]\nshutdown_timeout = \"" + timeout + "\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func warningsContain(warnings []string, substr string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, substr) {
			return true
		}
	}
	return false
}

func writeCityRuntimeConfigWithIncludes(t *testing.T, tomlPath string, includes []string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	var quoted []string
	for _, include := range includes {
		quoted = append(quoted, fmt.Sprintf("%q", include))
	}
	includesLine := ""
	if len(quoted) > 0 {
		includesLine = "includes = [" + strings.Join(quoted, ", ") + "]\n"
	}
	data := []byte("[workspace]\nname = \"test-city\"\n" + includesLine + "\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
