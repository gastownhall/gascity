package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestControllerReconcileStartsTheRigOrderNamedSessionWithoutAPhantomRigHolder
// is #8436's controller consequence (AC-7). A rig order's routing materializes
// its on-demand named session into the runtime-owned session store — the city's
// sessions class — and pours the graph into the rig store. The controller then
// reconciles over the split stores exactly as its tick does: the session must
// be visible to session listing, its runtime must start, and no session row may
// sit in the rig store for the pool census to count as a phantom holder of the
// pool's name. At base the writer stranded the row in the rig store: the
// sessions leg listed nothing, the runtime never started, and the census leg
// over the rig store counted the stranded row as the name's holder.
func TestControllerReconcileStartsTheRigOrderNamedSessionWithoutAPhantomRigHolder(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(cityPath, "rigs", "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	rig := beads.NewMemStore()
	rigStores := map[string]beads.Store{"myrig": rig}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "myrig", Provider: "test-agent", StartCommand: "true"},
			{Name: config.ControlDispatcherAgentName},
			{Name: config.ControlDispatcherAgentName, Dir: "myrig"},
		},
		NamedSessions: []config.NamedSession{{Template: "refinery", Dir: "myrig", Mode: "on_demand"}},
		Providers:     map[string]config.ProviderSpec{"test-agent": {DisplayName: "Test Agent"}},
	}

	// Phase A — the rig order dispatches through the real writer: routing
	// materializes the on-demand named session and the molecule pours the
	// graph into the rig store, its work step assigned to the named identity.
	formulaDir := t.TempDir()
	writeFile(t, filepath.Join(formulaDir, "rig-graph.formula.toml"), `
formula = "rig-graph"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Rig work"
assignee = "myrig/refinery"
`)
	tracking, err := rig.Create(beads.Bead{Title: "order:rig-order", Labels: []string{"order-run:rig-order", labelOrderTracking}})
	if err != nil {
		t.Fatal(err)
	}
	// The controller binds its runtime-owned sessions store as the
	// dispatcher's accessor (bindOrderDispatcherSessions); borrow the same
	// city store here.
	cityStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city store: %v", err)
	}
	var rec memRecorder
	m := &memoryOrderDispatcher{
		cfg:      cfg,
		cityName: "test-city",
		rec:      &rec,
		stderr:   io.Discard,
		sessions: func() beads.SessionStore { return cliStores(cityPath, cityStore).Sessions() },
	}
	m.dispatchWisp(context.Background(), rig, execStoreTarget{ScopeRoot: rigPath, ScopeKind: "rig", RigName: "myrig"},
		orders.Order{Name: "rig-order", Rig: "myrig", Formula: "rig-graph", Trigger: "cooldown", Interval: "15m", FormulaLayer: formulaDir},
		cityPath, tracking.ID, nil)
	if rec.hasType(events.OrderFailed) {
		t.Fatalf("dispatch failed: %+v", rec.events)
	}

	// Phase B — the controller tick over the split stores: desired state with
	// the rig store as a census leg, then the named-demand reconcile.
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)}
	var stdout, stderr bytes.Buffer
	sessionSnap, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	result := buildDesiredStateWithSessionBeadsAt("test-city", cityPath, clk.Now().UTC(), clk.Now().UTC(), cfg, sp, cityStore, rigStores, sessionSnap, nil, &stderr)
	cfgNames := configuredSessionNames(cfg, "test-city", cityStore)
	syncSessionBeads(cityPath, cityStore, result.State, sp, cfgNames, cfg, clk, &stderr, true)
	sessions, err := loadSessionBeads(cityStore)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, result.AssignedWorkBeads, sessionInfosFromBeads(sessions), result.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, result.NamedSessionDemand, cfg)
	reconcileSnap := newSessionBeadSnapshot(sessions)
	woken := reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath, reconcileSnap.OpenForReconcile(), reconcileSnap, result.State, cfgNames, cfg, sp,
		cityStore, testSeatWork(cityPath, cfg, cityStore, rigStores), nil, result.AssignedWorkBeads, rigStores, nil, newDrainTracker(), nil, poolDesired,
		result.NamedSessionDemand, result.NamedSessionRoutedDemand, result.StoreQueryPartial, nil, "test-city",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
		withReadyAssignedFlags(readyAssignedFlagsForBeads(result.ReadyAssigned, result.AssignedWorkBeads, result.AssignedWorkStoreRefs)),
	)

	// 1. Visible to session listing: the sessions leg lists exactly the
	// materialized named session under its runtime name.
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "myrig/refinery")
	listed, err := sessionFrontDoor(cityStore).ListAll(sessionpkg.ListAllOptions{})
	if err != nil {
		t.Fatalf("listing sessions: %v", err)
	}
	if len(listed) != 1 || listed[0].SessionName != runtimeName {
		t.Fatalf("sessions leg listed %+v; want exactly the named session %q: a session row outside the runtime-owned store is invisible to listing (#8436)", listed, runtimeName)
	}

	// 2. The runtime starts: the reconcile woke the named session for its
	// assigned rig work and the provider holds it running.
	if woken < 1 || !sp.IsRunning(runtimeName) {
		t.Fatalf("reconcile woke %d sessions, %q running=%v; want the named session started (starts=%v)",
			woken, runtimeName, sp.IsRunning(runtimeName), providerStartNames(sp))
	}

	// 3. No phantom rig-store holder: the rig census leg lists no session row,
	// and no second row appeared anywhere, so nothing but the real session can
	// hold the pool's name.
	rigListed, err := sessionFrontDoor(rig).ListAll(sessionpkg.ListAllOptions{})
	if err != nil {
		t.Fatalf("listing rig sessions: %v", err)
	}
	if len(rigListed) != 0 {
		t.Fatalf("rig store listed %d session row(s) (%v); a stranded row there holds the pool's name where no session reader finds it (#8436)",
			len(rigListed), rigListed)
	}
	if got := len(result.SessionOccupancyInfos); got != 1 {
		t.Fatalf("session census counted %d open sessions across the legs; want exactly the one named session (no phantom holder)", got)
	}
}

// providerStartNames returns the session names the provider was asked to start,
// for failure messages.
func providerStartNames(sp *runtime.Fake) []string {
	var names []string
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Start" {
			names = append(names, call.Name)
		}
	}
	return names
}
