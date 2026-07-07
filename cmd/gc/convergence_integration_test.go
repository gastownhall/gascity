package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
)

// setupConvergenceRuntime creates a CityRuntime with a MemStore and
// convergence handler initialized, suitable for integration tests.
// No socket is started — tests interact via handleConvergenceRequest
// or the convergenceReqCh channel.
func setupConvergenceRuntime(t *testing.T) (*CityRuntime, *beads.MemStore) {
	t.Helper()

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
	}
	sp := runtime.NewFake()
	convergenceReqCh := make(chan convergenceRequest, 16)

	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test",
		cfg:      cfg,
		sp:       sp,
		buildFn: func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
			return DesiredStateResult{}
		},
		rec:                 events.Discard,
		convergenceReqCh:    convergenceReqCh,
		standaloneCityStore: store,
		logPrefix:           "gc test",
		stdout:              &bytes.Buffer{},
		stderr:              &bytes.Buffer{},
	}

	// Initialize the city/HQ convergence scope (mimics initConvergenceHandler).
	cr.convScopes = map[string]*convergenceScope{
		"": cr.newConvergenceScope("", store, cr.cityPath, []string{sharedTestFormulaDir}),
	}

	return cr, store
}

// hqScope returns the city/HQ convergence scope from a test runtime,
// failing the test if convergence was not initialized.
func hqScope(t *testing.T, cr *CityRuntime) *convergenceScope {
	t.Helper()
	scope := cr.convScopes[""]
	if scope == nil {
		t.Fatal("city/HQ convergence scope not initialized")
	}
	return scope
}

// addConvergenceRigScope attaches a rig-scoped convergence scope backed by
// the given store, mimicking initConvergenceHandler's per-rig wiring.
func addConvergenceRigScope(cr *CityRuntime, rig string, store beads.Store) *convergenceScope {
	return addConvergenceRigScopeAt(cr, rig, store, filepath.Join(cr.cityPath, "rigs", rig))
}

func addConvergenceRigScopeAt(cr *CityRuntime, rig string, store beads.Store, storePath string) *convergenceScope {
	scope := cr.newConvergenceScope(rig, store, storePath, []string{sharedTestFormulaDir})
	cr.convScopes[rig] = scope
	if cr.cfg != nil {
		found := false
		for i := range cr.cfg.Rigs {
			if cr.cfg.Rigs[i].Name == rig {
				found = true
				break
			}
		}
		if !found {
			cr.cfg.Rigs = append(cr.cfg.Rigs, config.Rig{Name: rig, Path: storePath})
		}
	}
	return scope
}

type convergenceListErrorStore struct {
	beads.Store
	err error
}

func (s convergenceListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

type getPanicStore struct {
	beads.Store
}

func (s getPanicStore) Get(string) (beads.Bead, error) {
	panic("injected convergence store get panic")
}

// sendAndReceive sends a convergence request via handleConvergenceRequest
// and returns the reply.
func sendAndReceive(t *testing.T, cr *CityRuntime, req convergenceRequest) convergenceReply {
	t.Helper()
	return cr.handleConvergenceRequest(context.Background(), req)
}

// --- Channel-level tests ---

func TestConvergence_CreateReply(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)

	reply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "3",
		},
	})
	if reply.Error != "" {
		t.Fatalf("unexpected error: %s", reply.Error)
	}

	var result convergence.CreateResult
	if err := json.Unmarshal(reply.Result, &result); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}
	if result.BeadID == "" {
		t.Error("expected non-empty bead ID")
	}
	if result.FirstWispID == "" {
		t.Error("expected non-empty first wisp ID")
	}
}

func TestConvergence_StopCommand(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)

	// Create a loop first.
	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "5",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling create result: %v", err)
	}

	// Stop the loop.
	stopReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "stop",
		BeadID:  created.BeadID,
		User:    "test-operator",
	})
	if stopReply.Error != "" {
		t.Fatalf("stop error: %s", stopReply.Error)
	}

	// Verify state is terminated.
	meta, err := hqScope(t, cr).handler.Store.GetMetadata(created.BeadID)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if meta[convergence.FieldState] != convergence.StateTerminated {
		t.Errorf("state = %q, want %q", meta[convergence.FieldState], convergence.StateTerminated)
	}
}

func TestConvergence_UnknownCommand(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)

	reply := sendAndReceive(t, cr, convergenceRequest{
		Command: "bogus",
	})
	if reply.Error == "" {
		t.Fatal("expected error for unknown command")
	}
}

func TestConvergence_PanicRecovery(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)

	// Clear convScopes so handleConvergenceRequest hits the
	// "convergence not available" guard for "approve".
	savedScopes := cr.convScopes
	cr.convScopes = nil

	reply := cr.safeHandleConvergenceRequest(context.Background(), convergenceRequest{
		Command: "approve",
		BeadID:  "nonexistent",
	})
	// safeHandleConvergenceRequest should return error, not panic.
	if reply.Error == "" {
		t.Error("expected error reply when convergence is unavailable")
	}

	cr.convScopes = savedScopes
}

func TestConvergence_TickProcessesClosedWisp(t *testing.T) {
	cr, store := setupConvergenceRuntime(t)

	// Create a convergence loop.
	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "5",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}

	// Populate the active index so convergenceTick works.
	adapter := hqScope(t, cr).adapter
	if err := adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}

	// Close the active wisp to simulate it finishing.
	if err := store.Close(created.FirstWispID); err != nil {
		t.Fatalf("closing wisp: %v", err)
	}

	// Run convergenceTick — it should detect the closed wisp and process it.
	cr.convergenceTick(context.Background())

	// After processing, active_wisp should have changed (iterated to next wisp
	// or terminated, depending on gate mode — manual mode transitions to waiting_manual).
	meta, _ := hqScope(t, cr).handler.Store.GetMetadata(created.BeadID)
	state := meta[convergence.FieldState]
	// With manual gate mode, closing a wisp transitions to waiting_manual.
	if state != convergence.StateWaitingManual {
		t.Errorf("state after tick = %q, want %q", state, convergence.StateWaitingManual)
	}
}

func TestConvergence_TickRecoversMissingActiveWisp(t *testing.T) {
	cr, store := setupConvergenceRuntime(t)

	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "5",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}

	adapter := hqScope(t, cr).adapter
	if err := adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}

	if err := store.Delete(created.FirstWispID); err != nil {
		t.Fatalf("deleting wisp: %v", err)
	}

	cr.convergenceTick(context.Background())

	meta, err := hqScope(t, cr).handler.Store.GetMetadata(created.BeadID)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if meta[convergence.FieldActiveWisp] == "" {
		t.Fatal("active_wisp should be repaired after tick recovery")
	}
	if meta[convergence.FieldActiveWisp] == created.FirstWispID {
		t.Fatalf("active_wisp = %q, want replacement wisp", meta[convergence.FieldActiveWisp])
	}
	if _, err := store.Get(meta[convergence.FieldActiveWisp]); err != nil {
		t.Fatalf("repaired active_wisp %q should exist: %v", meta[convergence.FieldActiveWisp], err)
	}
}

func TestConvergence_StartupReconcile(t *testing.T) {
	cr, store := setupConvergenceRuntime(t)

	// Create a convergence bead that looks like it was interrupted mid-creation.
	b, err := store.Create(beads.Bead{
		Title:  "interrupted",
		Type:   "convergence",
		Status: "in_progress",
	})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, convergence.FieldState, convergence.StateCreating); err != nil {
		t.Fatalf("setting state: %v", err)
	}

	// Run startup reconcile.
	cr.convergenceStartupReconcile(context.Background())

	// The bead should now be terminated and closed.
	updated, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	if updated.Status != "closed" {
		t.Errorf("bead status = %q, want %q", updated.Status, "closed")
	}
	if updated.Metadata[convergence.FieldState] != convergence.StateTerminated {
		t.Errorf("state = %q, want %q", updated.Metadata[convergence.FieldState], convergence.StateTerminated)
	}

	// The active index should be populated after startup reconcile.
	adapter := hqScope(t, cr).adapter
	if adapter.activeIndex == nil {
		t.Error("active index should be populated after startup reconcile")
	}
}

// --- Rig-scoped convergence tests (issue #2357) ---

func TestConvergence_CreateRoutesToRigStore(t *testing.T) {
	cr, cityStore := setupConvergenceRuntime(t)
	rigStore := beads.NewMemStore()
	addConvergenceRigScope(cr, "gascity-prod", rigStore)

	reply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "3",
			"rig":            "gascity-prod",
		},
	})
	if reply.Error != "" {
		t.Fatalf("unexpected error: %s", reply.Error)
	}
	var result convergence.CreateResult
	if err := json.Unmarshal(reply.Result, &result); err != nil {
		t.Fatalf("unmarshaling result: %v", err)
	}

	// The convergence bead must land in the rig store, not city/HQ.
	if _, err := rigStore.Get(result.BeadID); err != nil {
		t.Errorf("convergence bead %q not found in rig store: %v", result.BeadID, err)
	}
	if _, err := cityStore.Get(result.BeadID); err == nil {
		t.Errorf("convergence bead %q leaked into the city/HQ store", result.BeadID)
	}

	// The bead records its owning rig for status/list and audit.
	bead, err := rigStore.Get(result.BeadID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bead.Metadata[convergence.FieldRig] != "gascity-prod" {
		t.Errorf("rig metadata = %q, want %q", bead.Metadata[convergence.FieldRig], "gascity-prod")
	}
}

func TestConvergence_CreateUnknownRigErrors(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)

	reply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "3",
			"rig":            "no-such-rig",
		},
	})
	// An unknown --rig must fail loudly, not silently write to HQ.
	if reply.Error == "" {
		t.Fatal("expected error for unknown rig, got success")
	}
	if !strings.Contains(reply.Error, "no-such-rig") {
		t.Errorf("error = %q, want it to name the unknown rig", reply.Error)
	}
}

func TestConvergence_TickDrivesRigScopedLoop(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	rigStore := beads.NewMemStore()
	rigScope := addConvergenceRigScope(cr, "gascity-prod", rigStore)

	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "5",
			"rig":            "gascity-prod",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}

	// Populate the rig scope's active index so convergenceTick processes it.
	if err := rigScope.adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}

	// Close the active wisp to simulate it finishing.
	if err := rigStore.Close(created.FirstWispID); err != nil {
		t.Fatalf("closing wisp: %v", err)
	}

	// convergenceTick must iterate the rig scope, not just city/HQ — without
	// this the rig-scoped loop would be created but never driven.
	cr.convergenceTick(context.Background())

	bead, err := rigStore.Get(created.BeadID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state := bead.Metadata[convergence.FieldState]; state != convergence.StateWaitingManual {
		t.Errorf("rig-scoped loop state after tick = %q, want %q", state, convergence.StateWaitingManual)
	}
}

func TestConvergence_StartupReconcileCoversRigScopes(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	rigStore := beads.NewMemStore()
	rigScope := addConvergenceRigScope(cr, "data-rig", rigStore)

	// A convergence bead interrupted mid-creation in the rig store.
	b, err := rigStore.Create(beads.Bead{Title: "interrupted", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	if err := rigStore.SetMetadata(b.ID, convergence.FieldState, convergence.StateCreating); err != nil {
		t.Fatalf("setting state: %v", err)
	}

	cr.convergenceStartupReconcile(context.Background())

	// The interrupted rig bead should be terminated and closed.
	updated, err := rigStore.Get(b.ID)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	if updated.Status != "closed" {
		t.Errorf("rig bead status = %q, want closed", updated.Status)
	}
	// Both scopes' active indexes must be populated.
	if rigScope.adapter.activeIndex == nil {
		t.Error("rig scope active index should be populated after startup reconcile")
	}
	if hqScope(t, cr).adapter.activeIndex == nil {
		t.Error("city scope active index should be populated after startup reconcile")
	}
}

func TestConvergence_GateConditionUsesRigStorePath(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	rigStore := beads.NewMemStore()
	rigPath := filepath.Join(cr.cityPath, "rigs", "gascity-prod")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("creating rig path: %v", err)
	}
	rigScope := addConvergenceRigScopeAt(cr, "gascity-prod", rigStore, rigPath)

	outputPath := filepath.Join(t.TempDir(), "beads-dir.txt")
	scriptPath := filepath.Join(t.TempDir(), "gate.sh")
	script := "#!/bin/sh\nprintf '%s' \"$BEADS_DIR\" > " + strconv.Quote(outputPath) + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing gate script: %v", err)
	}

	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "2",
			"gate_mode":      convergence.GateModeCondition,
			"gate_condition": scriptPath,
			"rig":            "gascity-prod",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if err := rigScope.adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}
	if err := rigStore.Close(created.FirstWispID); err != nil {
		t.Fatalf("closing wisp: %v", err)
	}

	cr.convergenceTick(context.Background())

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading gate output: %v", err)
	}
	if got, want := string(data), filepath.Join(rigPath, ".beads"); got != want {
		t.Fatalf("BEADS_DIR = %q, want %q", got, want)
	}
}

func TestConvergence_TickIsolatesPanickingScope(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	cr.convScopes[""] = cr.newConvergenceScope("", getPanicStore{Store: beads.NewMemStore()}, cr.cityPath, []string{sharedTestFormulaDir})
	hqScope(t, cr).adapter.activeIndex = map[string]string{"panic-root": "test-agent"}

	rigStore := beads.NewMemStore()
	rigScope := addConvergenceRigScope(cr, "healthy-rig", rigStore)
	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "5",
			"rig":            "healthy-rig",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if err := rigScope.adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}
	if err := rigStore.Close(created.FirstWispID); err != nil {
		t.Fatalf("closing wisp: %v", err)
	}

	cr.convergenceTick(context.Background())

	bead, err := rigStore.Get(created.BeadID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state := bead.Metadata[convergence.FieldState]; state != convergence.StateWaitingManual {
		t.Fatalf("healthy rig state = %q, want %q", state, convergence.StateWaitingManual)
	}
}

func TestConvergence_StartupReconcileMarksFailedScopeComplete(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	cr.convScopes[""] = cr.newConvergenceScope("", convergenceListErrorStore{
		Store: beads.NewMemStore(),
		err:   errors.New("injected list failure"),
	}, cr.cityPath, []string{sharedTestFormulaDir})

	rigStore := beads.NewMemStore()
	rigScope := addConvergenceRigScope(cr, "healthy-rig", rigStore)
	b, err := rigStore.Create(beads.Bead{Title: "interrupted", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	if err := rigStore.SetMetadata(b.ID, convergence.FieldState, convergence.StateCreating); err != nil {
		t.Fatalf("setting state: %v", err)
	}

	cr.convergenceStartupReconcile(context.Background())

	if !convergenceStartupComplete(cr) {
		t.Fatal("startup should be complete after isolating the failed scope")
	}
	if rigScope.adapter.activeIndex == nil {
		t.Fatal("healthy rig active index should be populated")
	}
	updated, err := rigStore.Get(b.ID)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	if updated.Status != "closed" {
		t.Fatalf("healthy rig interrupted bead status = %q, want closed", updated.Status)
	}
}

func TestConvergence_StartupReconcileRetriesFailedScopeOnTick(t *testing.T) {
	cr, store := setupConvergenceRuntime(t)
	hqScope(t, cr).store = convergenceListErrorStore{
		Store: store,
		err:   errors.New("injected list failure"),
	}

	b, err := store.Create(beads.Bead{Title: "active", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	for key, value := range map[string]string{
		convergence.FieldState:  convergence.StateActive,
		convergence.FieldTarget: "test-agent",
	} {
		if err := store.SetMetadata(b.ID, key, value); err != nil {
			t.Fatalf("setting %s: %v", key, err)
		}
	}

	cr.convergenceStartupReconcile(context.Background())

	if !convergenceStartupComplete(cr) {
		t.Fatal("startup should complete after isolating the failed scope")
	}
	scope := hqScope(t, cr)
	if !scope.needsStartupReconcile {
		t.Fatal("failed scope should remain eligible for later startup reconcile retry")
	}
	if _, ok := scope.adapter.activeIndex[b.ID]; ok {
		t.Fatal("failed startup reconcile should not make active bead visible until retry succeeds")
	}

	scope.store = store
	cr.convergenceTick(context.Background())

	if scope.needsStartupReconcile {
		t.Fatal("successful tick retry should clear startup reconcile retry marker")
	}
	if got := scope.adapter.activeIndex[b.ID]; got != "test-agent" {
		t.Fatalf("active index[%s] = %q, want test-agent", b.ID, got)
	}
}

func TestConvergenceScopeForRigUnboundAndUnknown(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, cr *CityRuntime)
		wantError string
	}{
		{
			name: "unbound rig fails loud (#2357)",
			setup: func(t *testing.T, cr *CityRuntime) {
				t.Helper()
				cr.cfg.Rigs = []config.Rig{{Name: "prod"}}
			},
			wantError: "no bead store",
		},
		{
			name: "unregistered rig",
			setup: func(t *testing.T, cr *CityRuntime) {
				t.Helper()
				cr.cfg.Rigs = nil
			},
			wantError: "is not registered in this city",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cr, _ := setupConvergenceRuntime(t)
			tc.setup(t, cr)

			scope, err := cr.convergenceScopeForRig("prod")
			if err == nil {
				t.Fatal("expected error for unbound/unknown rig")
			}
			if scope != nil {
				t.Fatalf("scope = %#v, want nil", scope)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %q, want diagnostic containing %q", err, tc.wantError)
			}
		})
	}
}

// TestConvergenceRebuildAddsRigScope proves the #2403 fix: a rig added by a
// config reload is picked up live by rebuildConvergenceHandler instead of
// requiring a controller restart. The new scope is marked
// needsStartupReconcile so the tick machinery populates its active index.
func TestConvergenceRebuildAddsRigScope(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	if _, err := cr.convergenceScopeForRig("prod"); err == nil {
		t.Fatal("prod scope should not exist before reload")
	}

	rigStore := beads.NewMemStore()
	rigPath := filepath.Join(cr.cityPath, "rigs", "prod")
	cr.cfg.Rigs = []config.Rig{{Name: "prod", Path: rigPath}}
	cr.standaloneRigStores = map[string]beads.Store{"prod": rigStore}

	cr.rebuildConvergenceHandler()

	scope, err := cr.convergenceScopeForRig("prod")
	if err != nil {
		t.Fatalf("prod scope missing after rebuild: %v", err)
	}
	if scope == nil {
		t.Fatal("prod scope nil after rebuild")
	}
	if !scope.needsStartupReconcile {
		t.Fatal("rebuilt scope should be marked needsStartupReconcile")
	}

	// The next tick reconciles and populates the new scope's active index.
	cr.convergenceTickScope(context.Background(), scope)
	if scope.needsStartupReconcile {
		t.Fatal("scope should be reconciled after a tick")
	}
	if scope.adapter.activeIndex == nil {
		t.Fatal("active index should be populated after a tick")
	}
}

// TestConvergenceRebuildDropsRemovedRigScope proves a rig removed by a
// config reload no longer resolves to a scope after rebuild.
func TestConvergenceRebuildDropsRemovedRigScope(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	rigStore := beads.NewMemStore()
	rigPath := filepath.Join(cr.cityPath, "rigs", "prod")
	cr.cfg.Rigs = []config.Rig{{Name: "prod", Path: rigPath}}
	cr.standaloneRigStores = map[string]beads.Store{"prod": rigStore}
	cr.rebuildConvergenceHandler()
	if _, err := cr.convergenceScopeForRig("prod"); err != nil {
		t.Fatalf("prod scope should exist after first rebuild: %v", err)
	}

	// Remove the rig from config and rebuild.
	cr.cfg.Rigs = nil
	cr.standaloneRigStores = nil
	cr.rebuildConvergenceHandler()

	scope, err := cr.convergenceScopeForRig("prod")
	if err == nil {
		t.Fatal("prod scope should be gone after removal rebuild")
	}
	if scope != nil {
		t.Fatalf("scope = %#v, want nil", scope)
	}
	if !strings.Contains(err.Error(), "is not registered in this city") {
		t.Fatalf("error = %q, want not-registered diagnostic", err)
	}
}

func TestConvergence_LifecycleCommandsRouteToRigScope(t *testing.T) {
	commands := []struct {
		name      string
		wantState string
	}{
		{name: "stop", wantState: convergence.StateTerminated},
		{name: "approve", wantState: convergence.StateTerminated},
		{name: "iterate", wantState: convergence.StateActive},
	}

	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			cr, _ := setupConvergenceRuntime(t)
			rigStore := beads.NewMemStore()
			rigScope := addConvergenceRigScope(cr, "gascity-prod", rigStore)

			createReply := sendAndReceive(t, cr, convergenceRequest{
				Command: "create",
				Params: map[string]string{
					"formula":        "test-formula",
					"target":         "test-agent",
					"max_iterations": "5",
					"rig":            "gascity-prod",
				},
			})
			if createReply.Error != "" {
				t.Fatalf("create error: %s", createReply.Error)
			}
			var created convergence.CreateResult
			if err := json.Unmarshal(createReply.Result, &created); err != nil {
				t.Fatalf("unmarshaling: %v", err)
			}
			if tc.name == "approve" || tc.name == "iterate" {
				if err := rigScope.adapter.populateIndex(); err != nil {
					t.Fatalf("populateIndex: %v", err)
				}
				if err := rigStore.Close(created.FirstWispID); err != nil {
					t.Fatalf("closing wisp: %v", err)
				}
				cr.convergenceTick(context.Background())
			}

			noRigReply := sendAndReceive(t, cr, convergenceRequest{
				Command: tc.name,
				BeadID:  created.BeadID,
				User:    "test-operator",
			})
			if noRigReply.Error == "" {
				t.Fatalf("%s without --rig should not find the rig-scoped loop", tc.name)
			}

			reply := sendAndReceive(t, cr, convergenceRequest{
				Command: tc.name,
				BeadID:  created.BeadID,
				User:    "test-operator",
				Params:  map[string]string{"rig": "gascity-prod"},
			})
			if reply.Error != "" {
				t.Fatalf("%s error: %s", tc.name, reply.Error)
			}
			bead, err := rigStore.Get(created.BeadID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if state := bead.Metadata[convergence.FieldState]; state != tc.wantState {
				t.Fatalf("state after %s = %q, want %q", tc.name, state, tc.wantState)
			}
		})
	}
}

func TestConvergence_StopRoutesToRigScope(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	rigStore := beads.NewMemStore()
	addConvergenceRigScope(cr, "gascity-prod", rigStore)

	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula":        "test-formula",
			"target":         "test-agent",
			"max_iterations": "5",
			"rig":            "gascity-prod",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}

	// Stopping without the rig param looks in city/HQ and cannot find the
	// rig-scoped bead — it must fail rather than affecting the wrong store.
	noRigReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "stop",
		BeadID:  created.BeadID,
		User:    "test-operator",
	})
	if noRigReply.Error == "" {
		t.Error("stop without --rig should not find the rig-scoped loop")
	}

	// With the rig param, stop routes to the rig scope and terminates it.
	stopReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "stop",
		BeadID:  created.BeadID,
		User:    "test-operator",
		Params:  map[string]string{"rig": "gascity-prod"},
	})
	if stopReply.Error != "" {
		t.Fatalf("stop error: %s", stopReply.Error)
	}
	bead, err := rigStore.Get(created.BeadID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bead.Metadata[convergence.FieldState] != convergence.StateTerminated {
		t.Errorf("state = %q, want %q", bead.Metadata[convergence.FieldState], convergence.StateTerminated)
	}
}

func TestConvergence_RetryRoutesToRigScope(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)
	rigStore := beads.NewMemStore()
	addConvergenceRigScope(cr, "gascity-prod", rigStore)

	// Create a rig-scoped loop and stop it so it is terminated and retryable.
	createReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "create",
		Params: map[string]string{
			"formula": "test-formula", "target": "test-agent",
			"max_iterations": "5", "rig": "gascity-prod",
		},
	})
	if createReply.Error != "" {
		t.Fatalf("create error: %s", createReply.Error)
	}
	var created convergence.CreateResult
	if err := json.Unmarshal(createReply.Result, &created); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	stopReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "stop", BeadID: created.BeadID, User: "test-operator",
		Params: map[string]string{"rig": "gascity-prod"},
	})
	if stopReply.Error != "" {
		t.Fatalf("stop error: %s", stopReply.Error)
	}

	// Retry without --rig looks in city/HQ and cannot find the rig loop.
	noRigReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "retry", BeadID: created.BeadID,
	})
	if noRigReply.Error == "" {
		t.Error("retry without --rig should not find the rig-scoped loop")
	}

	// Retry with --rig routes to the rig scope; the new loop lands there.
	retryReply := sendAndReceive(t, cr, convergenceRequest{
		Command: "retry", BeadID: created.BeadID,
		Params: map[string]string{"rig": "gascity-prod"},
	})
	if retryReply.Error != "" {
		t.Fatalf("retry error: %s", retryReply.Error)
	}
	var retried convergence.RetryResult
	if err := json.Unmarshal(retryReply.Result, &retried); err != nil {
		t.Fatalf("unmarshaling retry result: %v", err)
	}
	bead, err := rigStore.Get(retried.NewBeadID)
	if err != nil {
		t.Fatalf("retry bead %q not found in rig store: %v", retried.NewBeadID, err)
	}
	if bead.Metadata[convergence.FieldRig] != "gascity-prod" {
		t.Errorf("retry bead rig = %q, want %q", bead.Metadata[convergence.FieldRig], "gascity-prod")
	}
}

func TestConvergence_EnqueueTimeout(t *testing.T) {
	cr, _ := setupConvergenceRuntime(t)

	// Fill the channel to capacity.
	for i := 0; i < cap(cr.convergenceReqCh); i++ {
		cr.convergenceReqCh <- convergenceRequest{
			Command: "create",
			replyCh: make(chan convergenceReply, 1),
		}
	}

	// Try to send one more — should not block (we use a select with timeout).
	done := make(chan bool, 1)
	go func() {
		select {
		case cr.convergenceReqCh <- convergenceRequest{
			Command: "create",
			replyCh: make(chan convergenceReply, 1),
		}:
			done <- false // should not succeed immediately
		case <-time.After(50 * time.Millisecond):
			done <- true // timeout is expected
		}
	}()

	select {
	case timedOut := <-done:
		if !timedOut {
			t.Error("expected channel send to block when full")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("test timed out")
	}

	// Drain the channel.
	for len(cr.convergenceReqCh) > 0 {
		<-cr.convergenceReqCh
	}
}

func TestConvergenceStore_PourSpeculativeWispDefersAssignmentsUntilActivation(t *testing.T) {
	dir := t.TempDir()
	formulaText := `formula = "assigned-flow"
version = 1

[[steps]]
id = "work"
title = "Work"
assignee = "worker"
metadata = { "gc.routed_to" = "pool/worker", "gc.execution_routed_to" = "pool/worker" }
`
	if err := os.WriteFile(filepath.Join(dir, "assigned-flow.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	adapter := newConvergenceStoreAdapter(store, []string{dir})
	parent, err := store.Create(beads.Bead{Title: "root", Type: "convergence"})
	if err != nil {
		t.Fatalf("creating parent: %v", err)
	}

	wispID, err := adapter.PourSpeculativeWisp(parent.ID, "assigned-flow",
		convergence.IdempotencyKey(parent.ID, 1), nil, "")
	if err != nil {
		t.Fatalf("PourSpeculativeWisp: %v", err)
	}

	children, err := store.Children(wispID)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
	if children[0].Assignee != "" {
		t.Fatalf("speculative child assignee = %q, want empty", children[0].Assignee)
	}
	if children[0].Type != "gate" {
		t.Fatalf("speculative child type = %q, want gate", children[0].Type)
	}
	if got := children[0].Metadata[molecule.DeferredAssigneeMetadataKey]; got != "worker" {
		t.Fatalf("deferred assignee metadata = %q, want worker", got)
	}
	if got := children[0].Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("speculative child gc.routed_to = %q, want empty", got)
	}
	if got := children[0].Metadata[molecule.DeferredRoutedToMetadataKey]; got != "pool/worker" {
		t.Fatalf("deferred gc.routed_to metadata = %q, want pool/worker", got)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == children[0].ID {
			t.Fatalf("speculative child %s appeared in Ready before activation", bead.ID)
		}
	}

	if err := adapter.ActivateWisp(wispID); err != nil {
		t.Fatalf("ActivateWisp: %v", err)
	}
	activated, err := store.Get(children[0].ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if activated.Assignee != "worker" {
		t.Fatalf("activated child assignee = %q, want worker", activated.Assignee)
	}
	if activated.Type != "task" {
		t.Fatalf("activated child type = %q, want task", activated.Type)
	}
	if activated.Metadata["gc.routed_to"] != "pool/worker" {
		t.Fatalf("activated child gc.routed_to = %q, want pool/worker", activated.Metadata["gc.routed_to"])
	}
	if activated.Metadata["gc.execution_routed_to"] != "pool/worker" {
		t.Fatalf("activated child gc.execution_routed_to = %q, want pool/worker", activated.Metadata["gc.execution_routed_to"])
	}
}

func TestConvergenceStoreRejectsGraphV2Wisp(t *testing.T) {
	dir := t.TempDir()
	formulaText := `formula = "graph-flow"
version = 1
contract = "graph.v2"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
`
	if err := os.WriteFile(filepath.Join(dir, "graph-flow.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	adapter := newConvergenceStoreAdapter(store, []string{dir})
	parent, err := store.Create(beads.Bead{Title: "root", Type: "convergence"})
	if err != nil {
		t.Fatalf("creating parent: %v", err)
	}

	_, err = adapter.PourSpeculativeWisp(parent.ID, "graph-flow", convergence.IdempotencyKey(parent.ID, 1), nil, "")
	if err == nil {
		t.Fatal("PourSpeculativeWisp succeeded, want v2 formula rejection")
	}
	if !strings.Contains(err.Error(), "do not support v2 formula") {
		t.Fatalf("error = %q, want v2 formula rejection", err)
	}
}

// --- Active index tests ---

func TestConvergenceIndex_PopulateAndQuery(t *testing.T) {
	store := beads.NewMemStore()
	adapter := newConvergenceStoreAdapter(store, nil)

	// Create some convergence beads in various states.
	active, _ := store.Create(beads.Bead{Title: "active", Type: "convergence", Status: "in_progress"})
	_ = store.SetMetadata(active.ID, convergence.FieldState, convergence.StateActive)
	_ = store.SetMetadata(active.ID, convergence.FieldTarget, "agent-1")

	waiting, _ := store.Create(beads.Bead{Title: "waiting", Type: "convergence", Status: "in_progress"})
	_ = store.SetMetadata(waiting.ID, convergence.FieldState, convergence.StateWaitingManual)
	_ = store.SetMetadata(waiting.ID, convergence.FieldTarget, "agent-2")

	terminated, _ := store.Create(beads.Bead{Title: "terminated", Type: "convergence", Status: "closed"})
	_ = store.SetMetadata(terminated.ID, convergence.FieldState, convergence.StateTerminated)

	if err := adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}

	ids := adapter.activeBeadIDs()
	if len(ids) != 2 {
		t.Errorf("activeBeadIDs count = %d, want 2", len(ids))
	}

	// CountActiveConvergenceLoops should use the index.
	count1, _ := adapter.CountActiveConvergenceLoops("agent-1")
	if count1 != 1 {
		t.Errorf("count for agent-1 = %d, want 1", count1)
	}
	count2, _ := adapter.CountActiveConvergenceLoops("agent-2")
	if count2 != 1 {
		t.Errorf("count for agent-2 = %d, want 1", count2)
	}
	count3, _ := adapter.CountActiveConvergenceLoops("no-such-agent")
	if count3 != 0 {
		t.Errorf("count for no-such-agent = %d, want 0", count3)
	}
}

func TestConvergenceIndex_MaintainedOnStateTransitions(t *testing.T) {
	store := beads.NewMemStore()
	adapter := newConvergenceStoreAdapter(store, nil)

	// Start with an empty index.
	adapter.activeIndex = make(map[string]string)

	// Create a bead and transition through states.
	b, _ := store.Create(beads.Bead{Title: "test", Type: "convergence", Status: "in_progress"})
	_ = store.SetMetadata(b.ID, convergence.FieldTarget, "agent-x")

	// Setting state=active should add to index.
	_ = adapter.SetMetadata(b.ID, convergence.FieldState, convergence.StateActive)
	if _, ok := adapter.activeIndex[b.ID]; !ok {
		t.Error("bead should be in index after state=active")
	}

	// Setting state=terminated should remove from index.
	_ = adapter.SetMetadata(b.ID, convergence.FieldState, convergence.StateTerminated)
	if _, ok := adapter.activeIndex[b.ID]; ok {
		t.Error("bead should not be in index after state=terminated")
	}

	// Setting state=waiting_manual should add to index.
	_ = adapter.SetMetadata(b.ID, convergence.FieldState, convergence.StateWaitingManual)
	if _, ok := adapter.activeIndex[b.ID]; !ok {
		t.Error("bead should be in index after state=waiting_manual")
	}

	// CloseBead should remove from index AND stamp close_reason on the
	// underlying bead so bd's validation.on-close=error accepts the
	// close.
	if got := len(convergence.CloseReasonHandlerCleanup); got < 20 {
		t.Fatalf("CloseReasonHandlerCleanup = %q (%d chars), want >=20", convergence.CloseReasonHandlerCleanup, got)
	}
	_ = adapter.CloseBead(b.ID, convergence.CloseReasonHandlerCleanup)
	if _, ok := adapter.activeIndex[b.ID]; ok {
		t.Error("bead should not be in index after CloseBead")
	}
	closed, _ := store.Get(b.ID)
	if got := closed.Metadata["close_reason"]; got != convergence.CloseReasonHandlerCleanup {
		t.Errorf("close_reason = %q, want %q", got, convergence.CloseReasonHandlerCleanup)
	}
	if closed.Status != "closed" {
		t.Errorf("status = %q, want closed", closed.Status)
	}
}
