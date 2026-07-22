package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestControllerBeadCloseAutocloseImmediatelyClosesCrossStoreSourceWorkflow(t *testing.T) {
	previousDispatch := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previousDispatch })

	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "city"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   filepath.Join(cityPath, "rigs", "alpha"),
			Prefix: "alpha",
		}},
	}

	sourceStore := beads.NewMemStore()
	source, err := sourceStore.Create(beads.Bead{Title: "remote source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := sourceStore.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}

	graphStore := beads.NewMemStore()
	root, err := graphStore.Create(beads.Bead{
		Title: "remote-source workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
			beadmeta.SourceStoreRefMetadataKey:  "rig:alpha",
			beadmeta.RootStoreRefMetadataKey:    "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("Create graph root: %v", err)
	}

	recorder := events.NewFake()
	cs := &controllerState{
		cfg:           cfg,
		cityName:      "test-city",
		cityPath:      cityPath,
		cityBeadStore: graphStore,
		beadStores:    map[string]beads.Store{"alpha": sourceStore},
		eventProv:     recorder,
	}

	cs.runBeadCloseAutoclose(source.ID, sourceStore, "rig:alpha")

	after, err := graphStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get graph root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("graph root status = %q, want immediate close from the controller event path", after.Status)
	}
	assertSingleOrderedMoleculeLifecycle(t, recorder.Events)
}

func TestControllerBeadCloseAutocloseFallsBackWhenGraphStoreIsUnavailable(t *testing.T) {
	previousDispatch := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previousDispatch })

	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "terminal step",
		Type:  "step",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}
	if err := store.Close(step.ID); err != nil {
		t.Fatalf("Close step: %v", err)
	}

	recorder := events.NewFake()
	cs := &controllerState{eventProv: recorder}
	cs.runBeadCloseAutoclose(step.ID, store, "")

	after, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want close through source-store fallback", after.Status)
	}
	assertSingleOrderedMoleculeLifecycle(t, recorder.Events)
}

func TestControllerBeadCloseAutocloseKeepsSourceRefForAliasedGraphStore(t *testing.T) {
	previousDispatch := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previousDispatch })

	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "city"},
		Rigs: []config.Rig{{
			Name:   "alpha",
			Path:   filepath.Join(cityPath, "rigs", "alpha"),
			Prefix: "alpha",
		}},
	}
	store := beads.NewMemStore()
	source, err := store.Create(beads.Bead{Title: "legacy source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := store.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title: "legacy same-store workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create graph root: %v", err)
	}

	recorder := events.NewFake()
	cs := &controllerState{
		cfg:           cfg,
		cityName:      "test-city",
		cityPath:      cityPath,
		cityBeadStore: store,
		beadStores:    map[string]beads.Store{"alpha": store},
		eventProv:     recorder,
	}
	cs.runBeadCloseAutoclose(source.ID, store, "rig:alpha")

	after, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get graph root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("graph root status = %q, want close using the aliased source-store ref", after.Status)
	}
	assertSingleOrderedMoleculeLifecycle(t, recorder.Events)
}

func TestMoleculeAutocloseCompletionWaitContext(t *testing.T) {
	t.Run("abandons remaining waits on context cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		completion := moleculeAutocloseCompletion{lifecycleDone: []<-chan struct{}{make(chan struct{})}}
		retry, completed := completion.WaitContext(ctx)
		if completed {
			t.Fatal("completed = true, want false when the context is canceled before delivery")
		}
		if retry {
			t.Fatal("retry = true, want false for an abandoned wait")
		}
	})

	t.Run("completes when deliveries resolve", func(t *testing.T) {
		closed := make(chan struct{})
		close(closed)
		completion := moleculeAutocloseCompletion{
			lifecycleDone: []<-chan struct{}{closed},
			retryNeeded:   true,
		}
		retry, completed := completion.WaitContext(context.Background())
		if !completed || !retry {
			t.Fatalf("WaitContext = (retry=%t, completed=%t), want (true, true)", retry, completed)
		}
	})

	t.Run("nil context falls back to an unbounded wait", func(t *testing.T) {
		closed := make(chan struct{})
		close(closed)
		completion := moleculeAutocloseCompletion{lifecycleDone: []<-chan struct{}{closed}}
		retry, completed := completion.WaitContext(nil)
		if !completed || retry {
			t.Fatalf("WaitContext(nil) = (retry=%t, completed=%t), want (false, true)", retry, completed)
		}
	})
}

func TestStopBeadEventWorkersUnblocksAbandonedAutocloseAtShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cs := &controllerState{cacheCtx: ctx}
	// beadEventWorkerState defaults to beadEventWorkersRunning.
	if !cs.beginBeadEventWorker() {
		t.Fatal("beginBeadEventWorker = false, want a running worker slot")
	}

	// A dispatched autoclose whose lifecycle delivery never fires, mirroring a
	// suppressed-close receipt stuck behind an unresolved pending gate.
	blocked := moleculeAutocloseCompletion{lifecycleDone: []<-chan struct{}{make(chan struct{})}}
	go func() {
		defer cs.endBeadEventWorker()
		blocked.WaitContext(cs.cacheCtx)
	}()

	done := make(chan struct{})
	go func() {
		cs.stopBeadEventWorkers()
		close(done)
	}()

	// Shutdown cancels the cache context; the bounded wait must release so
	// stopBeadEventWorkers can finish rather than hang on beadEventAutoWorkers.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopBeadEventWorkers hung on a never-delivered autoclose completion")
	}
}
