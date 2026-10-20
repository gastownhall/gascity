package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/testutil"
)

// activeStepFixture is a city as the controller holds it, with one rig whose
// work the step reminder reads: split (a relocated graph binding holds roots
// and steps) or single-store (the rig store holds everything).
type activeStepFixture struct {
	cityPath string
	rigPath  string
	cfg      *config.City
	city     beads.Store
	rig      beads.Store
	binding  beads.Store // nil for a single-store city
}

const (
	activeStepAlias = "alias-1"
	activeStepName  = "name-1"
	activeStepID    = "gcs-1"
)

var activeStepAssignees = []string{activeStepAlias, activeStepName, activeStepID}

func newActiveStepFixture(t *testing.T, split bool) activeStepFixture {
	t.Helper()
	cityPath := t.TempDir()
	f := activeStepFixture{
		cityPath: cityPath,
		rigPath:  filepath.Join(cityPath, "rigs", "alpha"),
		city:     beads.NewMemStore(),
		rig:      beads.NewMemStore(),
	}
	f.cfg = &config.City{
		Workspace: config.Workspace{Name: "step-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: f.rigPath, Prefix: "ra"}},
	}
	if split {
		f.binding = beads.NewMemStore()
	}
	return f
}

func (f activeStepFixture) routes() *storageRoutes {
	if f.binding == nil {
		return nil
	}
	return splitRoutes(f.binding)
}

func (f activeStepFixture) controller() *controllerState {
	return &controllerState{
		cfg:           f.cfg,
		cityBeadStore: f.city,
		beadStores:    map[string]beads.Store{"alpha": f.rig},
		storageRoutes: f.routes(),
		cityName:      f.cfg.Workspace.Name,
		cityPath:      f.cityPath,
	}
}

// graph is the graph store a hook reading this rig's work resolves.
func (f activeStepFixture) graph() beads.Store {
	return resolveGraphStore(f.routes(), f.rig, f.cfg, f.cityPath, nil)
}

func (f activeStepFixture) create(t *testing.T, store beads.Store, b beads.Bead, status string) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create %q: %v", b.Title, err)
	}
	if status != "" && status != created.Status {
		if err := store.Update(created.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update %q to %s: %v", b.Title, status, err)
		}
	}
	return created
}

// activeStepScenarios seeds every resolution branch the reminder takes: work
// beads in the rig store, roots and steps in the graph store.
func activeStepScenarios() map[string]func(t *testing.T, f activeStepFixture) string {
	return map[string]func(t *testing.T, f activeStepFixture) string{
		"nothing": func(*testing.T, activeStepFixture) string { return "" },
		"assigned molecule, in-progress step": func(t *testing.T, f activeStepFixture) string {
			root := f.create(t, f.graph(), beads.Bead{Title: "patrol", Type: "molecule", Assignee: activeStepName}, "in_progress")
			step := f.create(t, f.graph(), beads.Bead{Title: "inspect", Type: "step", ParentID: root.ID, Description: "inspect the queue"}, "in_progress")
			return step.ID
		},
		"assigned molecule, entry step": func(t *testing.T, f activeStepFixture) string {
			root := f.create(t, f.graph(), beads.Bead{Title: "patrol", Type: "molecule", Assignee: activeStepID}, "in_progress")
			first := f.create(t, f.graph(), beads.Bead{Title: "first", Type: "step", ParentID: root.ID, Description: "start here"}, "")
			f.create(t, f.graph(), beads.Bead{Title: "second", Type: "step", ParentID: root.ID}, "")
			return first.ID
		},
		"claimed bridge": func(t *testing.T, f activeStepFixture) string {
			root := f.create(t, f.graph(), beads.Bead{Title: "attached", Type: "molecule"}, "")
			step := f.create(t, f.graph(), beads.Bead{Title: "bridged", Type: "step", ParentID: root.ID, Description: "the bridged step"}, "in_progress")
			f.create(t, f.rig, beads.Bead{Title: "source", Type: "task", Assignee: activeStepAlias, Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}}, "in_progress")
			return step.ID
		},
		"routed bridge": func(t *testing.T, f activeStepFixture) string {
			root := f.create(t, f.graph(), beads.Bead{Title: "attached", Type: "molecule"}, "")
			step := f.create(t, f.graph(), beads.Bead{Title: "entry", Type: "step", ParentID: root.ID, Description: "the entry step"}, "")
			f.create(t, f.rig, beads.Bead{Title: "routed source", Type: "task", Metadata: map[string]string{
				beadmeta.MoleculeIDMetadataKey: root.ID,
				beadmeta.RoutedToMetadataKey:   activeStepName,
			}}, "")
			return step.ID
		},
		"legacy description": func(t *testing.T, f activeStepFixture) string {
			f.create(t, f.rig, beads.Bead{Title: "no description", Type: "task", Assignee: activeStepID}, "in_progress")
			work := f.create(t, f.rig, beads.Bead{Title: "plain work", Type: "task", Assignee: activeStepID, Description: "do the work"}, "in_progress")
			return work.ID
		},
	}
}

// TestControllerActiveWorkStepIsTheInProcessResolution is the CLI == API
// property the endpoint rests on: over the same stores, on every resolution
// branch and for both a split and a single-store city, the controller names the
// bead the hook's in-process read names, with the same title and description.
func TestControllerActiveWorkStepIsTheInProcessResolution(t *testing.T) {
	for _, split := range []bool{true, false} {
		for name, seed := range activeStepScenarios() {
			t.Run(map[bool]string{true: "split/", false: "single store/"}[split]+name, func(t *testing.T) {
				f := newActiveStepFixture(t, split)
				wantID := seed(t, f)
				want, err := resolveActiveWispStep(f.rig, f.graph(), activeStepAssignees)
				if err != nil {
					t.Fatalf("in-process resolution: %v", err)
				}
				if stepID(want) != map[bool]string{true: wantID, false: "<none>"}[wantID != ""] {
					t.Fatalf("in-process resolution named %q, want %q: the fixture does not take its branch", stepID(want), wantID)
				}
				got, err := f.controller().ActiveWorkStep(context.Background(), activeStepAssignees, f.rigPath)
				if err != nil {
					t.Fatalf("ActiveWorkStep: %v", err)
				}
				switch {
				case want == nil && got != nil:
					t.Fatalf("controller named %s, in-process names none", got.ID)
				case want != nil && got == nil:
					t.Fatalf("controller named none, in-process names %s", want.ID)
				case want != nil && (got.ID != want.ID || got.Title != want.Title || got.Description != want.Description):
					t.Fatalf("controller named %+v, in-process names {%s %s %s}", *got, want.ID, want.Title, want.Description)
				}
			})
		}
	}
}

// TestControllerActiveWorkStepReadsPastAWarmCache pins freshness: the
// controller's stores are CachingStores, and a step written behind a primed
// cache (an agent's `bd` write the cache has not absorbed) is named by the very
// next answer.
func TestControllerActiveWorkStepReadsPastAWarmCache(t *testing.T) {
	f := newActiveStepFixture(t, true)
	backing := f.rig
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cs := f.controller()
	cs.beadStores["alpha"] = cache
	written := f.create(t, backing, beads.Bead{Title: "written by an agent's bd", Type: "task", Assignee: activeStepID, Description: "fresh"}, "in_progress")
	got, err := cs.ActiveWorkStep(context.Background(), activeStepAssignees, f.rigPath)
	if err != nil {
		t.Fatalf("ActiveWorkStep: %v", err)
	}
	if got == nil || got.ID != written.ID {
		t.Fatalf("ActiveWorkStep = %+v, want %s, written behind the controller's warm cache; the endpoint must read live", got, written.ID)
	}
}

// TestControllerActiveWorkStepReadsTheScopeStore pins which store a scope
// names: the rig whose path it is, the city for an empty scope or the city's
// own path, and nothing for any other path, which the hook then opens itself.
func TestControllerActiveWorkStepReadsTheScopeStore(t *testing.T) {
	f := newActiveStepFixture(t, false)
	rigWork := f.create(t, f.rig, beads.Bead{Title: "rig work", Type: "task", Assignee: activeStepID, Description: "in the rig"}, "in_progress")
	cityWork := f.create(t, f.city, beads.Bead{Title: "city work", Type: "task", Assignee: activeStepID, Description: "in the city"}, "in_progress")
	cs := f.controller()
	for _, tc := range []struct {
		scope, want string
	}{
		{scope: f.rigPath, want: rigWork.ID},
		{scope: f.rigPath + "/", want: rigWork.ID},
		{scope: "", want: cityWork.ID},
		{scope: f.cityPath, want: cityWork.ID},
	} {
		got, err := cs.ActiveWorkStep(context.Background(), activeStepAssignees, tc.scope)
		if err != nil {
			t.Fatalf("ActiveWorkStep(scope %q): %v", tc.scope, err)
		}
		if got == nil || got.ID != tc.want {
			t.Fatalf("ActiveWorkStep(scope %q) = %+v, want %s", tc.scope, got, tc.want)
		}
	}
	if _, err := cs.ActiveWorkStep(context.Background(), activeStepAssignees, filepath.Join(f.cityPath, "elsewhere")); !errors.Is(err, api.ErrActiveWorkStepScopeNotHeld) {
		t.Fatalf("ActiveWorkStep(unheld scope) = %v, want api.ErrActiveWorkStepScopeNotHeld", err)
	}
}

// TestControllerActiveWorkStepRefusesWhileAConfigReloadIsPending pins the
// freshness guard ReadyWork has: the config decides which store a scope names,
// so the controller answers only under a config it can show is current.
func TestControllerActiveWorkStepRefusesWhileAConfigReloadIsPending(t *testing.T) {
	f := newActiveStepFixture(t, true)
	cs := f.controller()
	var dirty atomic.Bool
	dirty.Store(true)
	cs.configDirty = &dirty
	if _, err := cs.ActiveWorkStep(context.Background(), activeStepAssignees, f.rigPath); !errors.Is(err, api.ErrReadyWorkConfigNotCurrent) {
		t.Fatalf("ActiveWorkStep with a pending reload = %v, want api.ErrReadyWorkConfigNotCurrent", err)
	}
}

// stepHookCity is the hook budget fixture's city with an active formula step,
// and a controller holding that city's store, as the supervisor would.
func stepHookCity(t *testing.T) (hookBudgetFixture, *controllerState) {
	t.Helper()
	f := newHookBudgetFixture(t)
	f.activeStep(t)
	return f, hookCityController(t, f)
}

// hookCityController is a controller holding the hook budget fixture city's
// store, as the supervisor would.
func hookCityController(t *testing.T, f hookBudgetFixture) *controllerState {
	t.Helper()
	cfg, err := loadCityConfig(f.cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	resetHookBudgetProcessState(t)
	return &controllerState{cfg: cfg, cityBeadStore: store, beadStores: map[string]beads.Store{}, cityName: loadedCityName(cfg, f.cityDir), cityPath: f.cityDir}
}

// withActiveWorkStepClient points the step reminder at client for the test.
func withActiveWorkStepClient(t *testing.T, client *api.Client, reason string) {
	t.Helper()
	prev := activeWorkStepAPIClient
	activeWorkStepAPIClient = func(string) (*api.Client, string) { return client, reason }
	t.Cleanup(func() { activeWorkStepAPIClient = prev })
}

// countStoreOpens counts the stores the test's process opens from here on.
func countStoreOpens(t *testing.T) *atomic.Int64 {
	t.Helper()
	var opens atomic.Int64
	prev := observeOpenedStore
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		opens.Add(1)
		return store
	}
	t.Cleanup(func() { observeOpenedStore = prev })
	return &opens
}

// TestWispStepInjectionAnswersThroughTheController pins the CLI half: with a
// controller up, the step reminder is the controller's answer, byte for byte
// what the in-process read writes, and the hook opens no store for it.
func TestWispStepInjectionAnswersThroughTheController(t *testing.T) {
	f, cs := stepHookCity(t)
	want := wispStepInjectionContentInProcess(f.cityDir)
	if !strings.Contains(want, "inspect the queue") {
		t.Fatalf("in-process reminder = %q, want the fixture's active step", want)
	}
	resetHookBudgetProcessState(t)
	withActiveWorkStepClient(t, readyWorkTestServer(t, cs), "")
	opens := countStoreOpens(t)
	if got := wispStepInjectionContent(f.cityDir); got != want {
		t.Fatalf("reminder through the controller =\n%q\nwant the in-process bytes\n%q", got, want)
	}
	if n := opens.Load(); n != 0 {
		t.Fatalf("the hook opened %d stores for a reminder the controller answered, want 0", n)
	}
}

// blockingListStore blocks every List until release closes, standing in for a
// controller whose store reads hang.
type blockingListStore struct {
	beads.Store
	release <-chan struct{}
}

func (s blockingListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-s.release
	return s.Store.List(q)
}

// TestWispStepInjectionFallsBackOnAnyControllerFailure pins the safe route:
// no controller, a controller that refuses, and one that does not answer in
// time are all answered by the in-process read, unchanged, and a hung
// controller costs the hook at most activeWorkStepAPITimeout.
func TestWispStepInjectionFallsBackOnAnyControllerFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client func(t *testing.T, cs *controllerState) *api.Client
	}{
		{name: "no controller", client: func(*testing.T, *controllerState) *api.Client { return nil }},
		{name: "refusing controller", client: func(t *testing.T, cs *controllerState) *api.Client {
			var dirty atomic.Bool
			dirty.Store(true)
			cs.configDirty = &dirty
			return readyWorkTestServer(t, cs)
		}},
		{name: "hung controller", client: func(t *testing.T, cs *controllerState) *api.Client {
			release := make(chan struct{})
			cs.cityBeadStore = blockingListStore{Store: cs.cityBeadStore, release: release}
			client := readyWorkTestServer(t, cs)
			t.Cleanup(func() { close(release) })
			prev := activeWorkStepAPITimeout
			activeWorkStepAPITimeout = 50 * time.Millisecond
			t.Cleanup(func() { activeWorkStepAPITimeout = prev })
			return client
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cs := stepHookCity(t)
			want := wispStepInjectionContentInProcess(f.cityDir)
			resetHookBudgetProcessState(t)
			withActiveWorkStepClient(t, tc.client(t, cs), "controller-down")
			start := time.Now()
			got := wispStepInjectionContent(f.cityDir)
			if got != want {
				t.Fatalf("reminder = %q, want the in-process read %q", got, want)
			}
			// The hung controller never answers on its own (its store blocks until
			// cleanup), so any finite wait shows the bound held.
			if elapsed := time.Since(start); elapsed > testutil.GoroutineRaceTimeout {
				t.Fatalf("the fallback took %s; a hung controller must cost at most activeWorkStepAPITimeout", elapsed)
			}
		})
	}
}

// signalingStore closes arrived on its first List: the controller has started
// answering a request.
type signalingStore struct {
	beads.Store
	once    *sync.Once
	arrived chan struct{}
}

func (s signalingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.once.Do(func() { close(s.arrived) })
	return s.Store.List(q)
}

// requireLookupOverlapsFirstOpen runs hook with a controller that answers the
// step reminder and returns its stdout, and fails unless the controller has the request by the time
// the hook opens its first store: the lookup must run while the hook does its
// own work, not after it. The hook's first open waits up to a bound for the
// request, so the check is not a race; a lookup started after that open never
// arrives within it.
func requireLookupOverlapsFirstOpen(t *testing.T, cs *controllerState, hook func(stdout, stderr io.Writer) int) string {
	t.Helper()
	arrived := make(chan struct{})
	cs.cityBeadStore = signalingStore{Store: cs.cityBeadStore, once: &sync.Once{}, arrived: arrived}
	withActiveWorkStepClient(t, readyWorkTestServer(t, cs), "")
	var firstOpen sync.Once
	inFlight := false
	prev := observeOpenedStore
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		firstOpen.Do(func() {
			select {
			case <-arrived:
				inFlight = true
			case <-time.After(testutil.GoroutineRaceTimeout):
			}
		})
		return store
	}
	t.Cleanup(func() { observeOpenedStore = prev })
	// run() opens the invocation store memo for these hook verbs; the
	// in-process call does the same, as recordHookCommand does.
	resetHookBudgetProcessState(t)
	endStoreMemo := beginInvocationStoreMemo()
	var stdout, stderr strings.Builder
	code := hook(&stdout, &stderr)
	if err := endStoreMemo(); err != nil {
		t.Errorf("releasing the invocation's stores: %v", err)
	}
	observeOpenedStore = prev
	resetHookBudgetProcessState(t)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	if !inFlight {
		t.Fatal("the hook opened its first store before the controller had the step reminder request; the lookup must start before the hook's own reads")
	}
	return stdout.String()
}

// TestNudgeDrainAsksForTheStepReminderBeforeResolvingItsTarget pins the overlap
// for the UserPromptSubmit hook, and that the reminder it writes is the one the
// in-process read writes.
func TestNudgeDrainAsksForTheStepReminderBeforeResolvingItsTarget(t *testing.T) {
	f, cs := stepHookCity(t)
	f.queueNudge(t)
	want := recordHookCommand(t, func(stdout, stderr io.Writer) int { return nudgeDrainInject(t, f, stdout, stderr) })
	// The reference run delivered the nudge; queue it again for the routed run.
	f.queueNudge(t)
	stdout := requireLookupOverlapsFirstOpen(t, cs, func(stdout, stderr io.Writer) int {
		return nudgeDrainInject(t, f, stdout, stderr)
	})
	if got, wantOut := normalizeHookBudgetOutput(t, stdout), normalizeHookBudgetOutput(t, want.stdout); got != wantOut {
		t.Fatalf("drain through the controller wrote\n%s\nwant the in-process output\n%s", got, wantOut)
	}
	if !strings.Contains(stdout, "inspect the queue") || !strings.Contains(stdout, "review queued work") {
		t.Fatalf("drain output = %q, want the step reminder and the queued nudge", stdout)
	}
}

// TestPrimeHookAsksForTheStepReminderBeforeProvingItsSession pins the overlap
// for the SessionStart hook, and that the context it writes is the one the
// in-process read writes.
func TestPrimeHookAsksForTheStepReminderBeforeProvingItsSession(t *testing.T) {
	f, cs := stepHookCity(t)
	want := recordHookCommand(t, func(stdout, stderr io.Writer) int { return primeHook(t, f, stdout, stderr) })
	stdout := requireLookupOverlapsFirstOpen(t, cs, func(stdout, stderr io.Writer) int {
		return primeHook(t, f, stdout, stderr)
	})
	if got, wantOut := normalizeHookBudgetOutput(t, stdout), normalizeHookBudgetOutput(t, want.stdout); got != wantOut {
		t.Fatalf("prime through the controller wrote\n%s\nwant the in-process output\n%s", got, wantOut)
	}
	if !strings.Contains(stdout, "inspect the queue") {
		t.Fatalf("prime output = %q, want the step reminder", stdout)
	}
}

// arrivingBlockingListStore announces each List on arrived and then blocks it
// until release closes, standing in for a controller that has a request in
// hand and does not answer.
type arrivingBlockingListStore struct {
	beads.Store
	arrived chan<- struct{}
	release <-chan struct{}
}

func (s arrivingBlockingListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	select {
	case s.arrived <- struct{}{}:
	default:
	}
	<-s.release
	return s.Store.List(q)
}

// TestWispStepLookupStopEndsTheRequest pins that a hook that returns before it
// needs the reminder does not wait for a controller that has its request and
// is not answering: stop cancels the request and returns, long before the
// lookup's own bound.
func TestWispStepLookupStopEndsTheRequest(t *testing.T) {
	f, cs := stepHookCity(t)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	cs.cityBeadStore = arrivingBlockingListStore{Store: cs.cityBeadStore, arrived: arrived, release: release}
	withActiveWorkStepClient(t, readyWorkTestServer(t, cs), "")
	// Registered after the server, so it runs before the server's close, which
	// waits for the blocked handler.
	t.Cleanup(func() { close(release) })
	prev := activeWorkStepAPITimeout
	activeWorkStepAPITimeout = 10 * time.Minute
	t.Cleanup(func() { activeWorkStepAPITimeout = prev })

	lookup := startWispStepLookup(f.cityDir, io.Discard)
	select {
	case <-arrived:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("the controller never received the step reminder request")
	}
	stopped := make(chan struct{})
	go func() {
		lookup.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("stop waited on a controller holding the request; it must cancel it")
	}
}

// TestWispStepLookupIsBoundedWhileItsClientResolves pins the bound where no
// context reaches: finding a supervisor-managed city's controller pings its
// sockets and lists the supervisor's cities before the request is sent. A
// resolution that hangs costs the hook activeWorkStepAPITimeout, after which it
// writes the in-process reminder, and stop is bounded the same way.
func TestWispStepLookupIsBoundedWhileItsClientResolves(t *testing.T) {
	f, _ := stepHookCity(t)
	want := wispStepInjectionContentInProcess(f.cityDir)
	resetHookBudgetProcessState(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	prev := activeWorkStepAPIClient
	activeWorkStepAPIClient = func(string) (*api.Client, string) {
		<-release
		return nil, "released"
	}
	t.Cleanup(func() { activeWorkStepAPIClient = prev })
	prevBound := activeWorkStepAPITimeout
	activeWorkStepAPITimeout = 100 * time.Millisecond // the bound under test
	t.Cleanup(func() { activeWorkStepAPITimeout = prevBound })

	got := make(chan string, 1)
	go func() { got <- startWispStepLookup(f.cityDir, io.Discard).content() }()
	select {
	case reminder := <-got:
		if reminder != want {
			t.Fatalf("reminder after the bound = %q, want the in-process read %q", reminder, want)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("content waited on a client that never resolved; it must give up at activeWorkStepAPITimeout")
	}
	stopped := make(chan struct{})
	go func() {
		startWispStepLookup(f.cityDir, io.Discard).stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("stop waited on a client that never resolved; it must give up at activeWorkStepAPITimeout")
	}
}

// TestWispStepLookupNeedsAnIdentity pins that a hook with no identity asks
// nothing: there is no reminder to look up for nobody.
func TestWispStepLookupNeedsAnIdentity(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_SESSION_ID", "")
	asked := false
	prev := activeWorkStepAPIClient
	activeWorkStepAPIClient = func(string) (*api.Client, string) { asked = true; return nil, "" }
	t.Cleanup(func() { activeWorkStepAPIClient = prev })
	if got := wispStepInjectionContent(t.TempDir()); got != "" {
		t.Fatalf("reminder with no identity = %q, want none", got)
	}
	if asked {
		t.Fatal("the reminder asked the controller for a hook with no identity")
	}
}
