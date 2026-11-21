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
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/orders"
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
	formulatest.EnableV2ForTest(t)
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
	}
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
