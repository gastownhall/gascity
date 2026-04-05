package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
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

	// Initialize convergence handler (mimics initConvergenceHandler).
	adapter := newConvergenceStoreAdapter(store, []string{sharedTestFormulaDir})
	emitter := &convergenceEventEmitter{rec: cr.rec}
	cr.convStoreAdapter = adapter
	cr.convHandler = &convergence.Handler{
		Store:   adapter,
		Emitter: emitter,
	}

	return cr, store
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
	meta, err := cr.convHandler.Store.GetMetadata(created.BeadID)
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

	// Temporarily replace convHandler with nil to cause a panic
	// when handleConvergenceRequest tries to access it for "approve".
	savedHandler := cr.convHandler
	cr.convHandler = nil

	reply := cr.safeHandleConvergenceRequest(context.Background(), convergenceRequest{
		Command: "approve",
		BeadID:  "nonexistent",
	})
	// safeHandleConvergenceRequest should return error, not panic.
	if reply.Error == "" {
		t.Error("expected error reply from nil handler")
	}

	cr.convHandler = savedHandler
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
	adapter := cr.convHandler.Store.(*convergenceStoreAdapter)
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
	meta, _ := cr.convHandler.Store.GetMetadata(created.BeadID)
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

	adapter := cr.convHandler.Store.(*convergenceStoreAdapter)
	if err := adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}

	if err := store.Delete(created.FirstWispID); err != nil {
		t.Fatalf("deleting wisp: %v", err)
	}

	cr.convergenceTick(context.Background())

	meta, err := cr.convHandler.Store.GetMetadata(created.BeadID)
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
	adapter := cr.convHandler.Store.(*convergenceStoreAdapter)
	if adapter.activeIndex == nil {
		t.Error("active index should be populated after startup reconcile")
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

	// CloseBead should remove from index.
	_ = adapter.CloseBead(b.ID)
	if _, ok := adapter.activeIndex[b.ID]; ok {
		t.Error("bead should not be in index after CloseBead")
	}
}
