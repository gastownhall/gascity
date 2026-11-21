package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/featureflags"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// rigNamedSessionOrderFixture is #8436's shape: an unrelocated city whose
// rig has its own bead store, an on-demand configured named session in that
// rig, and a rig-scoped formulas v2 order whose step names the session as its
// explicit assignee.
type rigNamedSessionOrderFixture struct {
	cityPath string
	rigPath  string
	cfg      *config.City
	order    orders.Order
	city     *beads.MemStore
	rig      *beads.MemStore
}

func newRigNamedSessionOrderFixture(t *testing.T) rigNamedSessionOrderFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "refine.toml"), []byte(`
formula = "refine"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Refine"
assignee = "myrig/refinery"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	maxOne := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "myrig", StartCommand: "true", MaxActiveSessions: &maxOne},
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &maxOne},
			{Name: config.ControlDispatcherAgentName, Dir: "myrig", MaxActiveSessions: &maxOne},
		},
		NamedSessions: []config.NamedSession{{Template: "refinery", Dir: "myrig", Mode: "on_demand"}},
		Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
	}
	prevFlags := featureflags.Snapshot()
	applyFeatureFlags(cfg)
	t.Cleanup(func() { featureflags.Apply(prevFlags) })
	return rigNamedSessionOrderFixture{
		cityPath: cityPath,
		rigPath:  rigPath,
		cfg:      cfg,
		order:    orders.Order{Name: "refine", Rig: "myrig", Formula: "refine", Trigger: "cooldown", Interval: "15m", FormulaLayer: formulaDir},
		city:     beads.NewMemStore(),
		rig:      beads.NewMemStore(),
	}
}

// dispatch fires the order once through a dispatcher whose session accessor
// is the runtime-owned city sessions store, and returns the session the
// routed step was assigned to.
func (f rigNamedSessionOrderFixture) dispatch(t *testing.T) string {
	t.Helper()
	f.completePriorRuns(t)
	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa: []orders.Order{f.order},
		storeFn: func(execStoreTarget) (beads.Store, error) {
			return f.rig, nil
		},
		sessions:             func() beads.SessionStore { return beadstest.Sessions(f.city) },
		cfg:                  f.cfg,
		cityName:             "test-city",
		cityPath:             f.cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	// Each dispatch is a fresh cooldown window.
	m.dispatch(context.Background(), f.cityPath, time.Now().Add(time.Duration(len(f.workSteps(t)))*time.Hour))
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("order dispatch did not drain")
	}
	if rec.hasType(events.OrderFailed) || !rec.hasType(events.OrderCompleted) {
		t.Fatalf("order events = %+v, want completed without failure", rec.events)
	}
	steps := f.workSteps(t)
	if len(steps) == 0 {
		t.Fatal("the rig store holds no routed work step")
	}
	return steps[len(steps)-1].Assignee
}

// completePriorRuns closes every open bead an earlier dispatch left in the rig
// store, so the order's open-work suppression lets the next tick fire it.
func (f rigNamedSessionOrderFixture) completePriorRuns(t *testing.T) {
	t.Helper()
	open, err := f.rig.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range open {
		if err := f.rig.Close(b.ID); err != nil {
			t.Fatalf("completing prior run bead %s: %v", b.ID, err)
		}
	}
}

// workSteps lists the order's routed work steps in the rig store, oldest first.
func (f rigNamedSessionOrderFixture) workSteps(t *testing.T) []beads.Bead {
	t.Helper()
	all, err := f.rig.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	var steps []beads.Bead
	for _, b := range all {
		if b.Title == "Refine" {
			steps = append(steps, b)
		}
	}
	return steps
}

func (f rigNamedSessionOrderFixture) assertResidency(t *testing.T, id string) {
	t.Helper()
	if n := countSessionBeads(t, f.rig); n != 0 {
		t.Errorf("the rig workflow store holds %d session bead(s); routing materialized the named session where no session reader reads", n)
	}
	if n := countSessionBeads(t, f.city); n != 1 {
		t.Errorf("the city session store holds %d session bead(s), want exactly one", n)
	}
	b, err := f.city.Get(id)
	if err != nil {
		t.Fatalf("the routed session %q is not in the city session store: %v", id, err)
	}
	if !session.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
		t.Errorf("city session bead %s = type %q status %q, want an open session", id, b.Type, b.Status)
	}
	for _, step := range f.workSteps(t) {
		if got := step.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "rig:myrig" {
			t.Errorf("work step %s gc.root_store_ref = %q, want rig:myrig", step.ID, got)
		}
	}
}

// TestOrderDispatchMaterializesRigNamedSessionInTheCitySessionStore is AC-1:
// a rig-scoped formulas v2 order whose step names an on-demand configured
// named session as its assignee binds one session id in the city's session
// store on both routes, while the workflow stays in the rig store and the rig
// store gains no session row (#8436).
func TestOrderDispatchMaterializesRigNamedSessionInTheCitySessionStore(t *testing.T) {
	f := newRigNamedSessionOrderFixture(t)
	first := f.dispatch(t)
	if first == "" {
		t.Fatal("the routed step has no assignee; want the named session's id")
	}
	f.assertResidency(t, first)
	second := f.dispatch(t)
	if second != first {
		t.Errorf("second route bound session %q, want the first route's %q", second, first)
	}
	if n := len(f.workSteps(t)); n != 2 {
		t.Errorf("rig store holds %d routed work steps, want 2", n)
	}
	f.assertResidency(t, first)
}

// TestOrderDispatchReopensPlainlyClosedNamedSessionInTheCitySessionStore is
// AC-2: a configured named session closed with a plain status close (its
// identifiers not retired) is reopened in place, same id, in the city's
// session store by the next route; the rig store gains no session row.
func TestOrderDispatchReopensPlainlyClosedNamedSessionInTheCitySessionStore(t *testing.T) {
	f := newRigNamedSessionOrderFixture(t)
	first := f.dispatch(t)
	if first == "" {
		t.Fatal("the routed step has no assignee; want the named session's id")
	}
	if err := f.city.Close(first); err != nil {
		t.Fatalf("plain close of %s: %v", first, err)
	}
	again := f.dispatch(t)
	if again != first {
		t.Errorf("route after a plain close bound session %q, want the reopened %q", again, first)
	}
	f.assertResidency(t, first)
}

// TestCityRuntimeBindsTheOrderDispatcherToItsSessionsStore is AC-7's wiring:
// the controller's order dispatcher resolves and materializes the sessions a
// dispatched step names through the runtime's own sessions store, both the
// dispatcher the runtime is built with and one a reload installs. Unbound, the
// dispatcher would open the city store once per dispatch instead, past the
// runtime's cache, so the session census would not see a new row until a poke.
func TestCityRuntimeBindsTheOrderDispatcherToItsSessionsStore(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := "[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"1h\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "orders", "digest.toml"), []byte(digest), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
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
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	assertBound := func(when string, od orderDispatcher) {
		t.Helper()
		mem, ok := od.(*memoryOrderDispatcher)
		if !ok {
			t.Fatalf("%s: order dispatcher = %T, want *memoryOrderDispatcher", when, od)
		}
		if mem.sessions == nil {
			t.Fatalf("%s: the order dispatcher has no sessions accessor; graph routing would open the city store per dispatch", when)
		}
		if got, want := mem.sessions(), cr.sessionsBeadStore(); !beads.SameStore(got, want) {
			t.Errorf("%s: the order dispatcher routes sessions through %v, want the runtime's sessions store %v", when, got, want)
		}
	}
	assertBound("as built", cr.od)

	next := buildOrderDispatcherFromOrderSet(cr.storageRoutes, cityPath, cfg, []orders.Order{{Name: "digest", Exec: "true", Trigger: "cooldown", Interval: "1h"}}, events.Discard, io.Discard)
	built := cr.od
	cr.replaceOrderDispatcher(next)
	cancelInflight(built)
	assertBound("after a reload's rebuild", next)
}
