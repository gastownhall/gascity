package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestRunMoleculeLifecycleRecoveryLoopStartupCoalescesAndTicks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	notify := make(chan struct{}, 1)
	ticks := make(chan time.Time, 1)
	retryTicks := make(chan time.Time, 1)
	retryRequests := make(chan time.Duration, 4)
	entered := make(chan int, 4)
	release := make(chan struct{}, 4)
	done := make(chan struct{})

	var mu sync.Mutex
	calls := 0
	recoverFn := func() bool {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		entered <- call
		<-release
		return call == 2
	}

	go func() {
		defer close(done)
		runMoleculeLifecycleRecoveryLoop(ctx, notify, ticks, recoverFn, func(delay time.Duration) <-chan time.Time {
			retryRequests <- delay
			return retryTicks
		})
	}()

	waitForRecoveryCall(t, entered, 1, "startup")
	for range 3 {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	release <- struct{}{}
	waitForRecoveryCall(t, entered, 2, "coalesced notification")
	release <- struct{}{}

	select {
	case delay := <-retryRequests:
		if delay != moleculeLifecycleRecoveryRetryFloor {
			t.Fatalf("first retry delay = %s, want %s", delay, moleculeLifecycleRecoveryRetryFloor)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transient recovery result did not schedule a retry")
	}
	select {
	case call := <-entered:
		t.Fatalf("recovery hot-looped as call %d before the retry timer fired", call)
	case <-time.After(25 * time.Millisecond):
	}
	retryTicks <- time.Now()
	waitForRecoveryCall(t, entered, 3, "bounded retry after transient result")
	release <- struct{}{}

	ticks <- time.Now()
	waitForRecoveryCall(t, entered, 4, "ticker")
	cancel()
	release <- struct{}{}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery loop did not stop after cancellation")
	}
}

func TestMoleculeLifecycleRecoveryRetryBackoffIsBounded(t *testing.T) {
	delay := moleculeLifecycleRecoveryRetryFloor
	for delay < moleculeLifecycleRecoveryRetryMax {
		next := nextMoleculeLifecycleRecoveryRetryDelay(delay)
		if next <= delay || next > moleculeLifecycleRecoveryRetryMax {
			t.Fatalf("next retry delay after %s = %s, want increasing value capped at %s", delay, next, moleculeLifecycleRecoveryRetryMax)
		}
		delay = next
	}
	if got := nextMoleculeLifecycleRecoveryRetryDelay(delay); got != moleculeLifecycleRecoveryRetryMax {
		t.Fatalf("retry delay after cap = %s, want %s", got, moleculeLifecycleRecoveryRetryMax)
	}
}

func TestControllerMoleculeLifecycleRecoveryScansCurrentDistinctStores(t *testing.T) {
	previousRecover := controllerRecoverMoleculeLifecycleIntents
	t.Cleanup(func() { controllerRecoverMoleculeLifecycleIntents = previousRecover })

	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	eventProvider := events.NewFake()
	type recoveryCall struct {
		store beads.Store
		rec   events.Recorder
	}
	calls := make(chan recoveryCall, 4)
	controllerRecoverMoleculeLifecycleIntents = func(
		store beads.Store,
		_ string,
		_ moleculeLifecycleStoreResolver,
		rec events.Recorder,
	) bool {
		calls <- recoveryCall{store: store, rec: rec}
		return false
	}

	cs := &controllerState{
		cityBeadStore: cityStore,
		beadStores: map[string]beads.Store{
			"duplicate-city": cityStore,
			"rig":            rigStore,
		},
		eventProv: eventProvider,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs.startMoleculeLifecycleRecovery(ctx)
	cs.startMoleculeLifecycleRecovery(ctx) // Idempotent: one controller-owned loop.

	seen := make(map[beads.Store]int)
	for range 2 {
		select {
		case call := <-calls:
			if call.rec != eventProvider {
				t.Fatalf("recovery recorder = %T, want current provider %T", call.rec, eventProvider)
			}
			seen[call.store]++
		case <-time.After(2 * time.Second):
			t.Fatalf("startup recovery calls = %d, want two distinct stores", len(seen))
		}
	}
	if seen[cityStore] != 1 || seen[rigStore] != 1 {
		t.Fatalf("startup recovery calls = %#v, want city and rig once each", seen)
	}
	select {
	case call := <-calls:
		t.Fatalf("duplicate store was scanned again: %T", call.store)
	case <-time.After(25 * time.Millisecond):
	}

	replacementStore := beads.NewMemStore()
	replacementProvider := events.NewFake()
	cs.mu.Lock()
	cs.cityBeadStore = replacementStore
	cs.beadStores = map[string]beads.Store{"duplicate-current": replacementStore}
	cs.eventProv = replacementProvider
	cs.mu.Unlock()
	cs.notifyMoleculeLifecycleRecovery()

	select {
	case call := <-calls:
		if call.store != replacementStore {
			t.Fatalf("notified recovery store = %T, want replacement store", call.store)
		}
		if call.rec != replacementProvider {
			t.Fatalf("notified recovery recorder = %T, want replacement provider", call.rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification did not scan the controller's current store and provider")
	}
	select {
	case call := <-calls:
		t.Fatalf("replacement duplicate store was scanned again: %T", call.store)
	case <-time.After(25 * time.Millisecond):
	}

	cs.stopBeadEventWorkers()
	cs.notifyMoleculeLifecycleRecovery() // Safe after stop and on a zero-value controller.
	(&controllerState{}).notifyMoleculeLifecycleRecovery()
}

func TestControllerMoleculeLifecycleRecoveryClosesRootMissedBeforeConstruction(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "workflow root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "terminal step",
		Type:     "step",
		ParentID: root.ID,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("Close child before controller construction: %v", err)
	}
	closedChild, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get closed child: %v", err)
	}
	payload, err := json.Marshal(closedChild)
	if err != nil {
		t.Fatalf("Marshal closed child: %v", err)
	}

	rec := events.NewFake()
	rec.Record(events.Event{Type: events.BeadClosed, Subject: child.ID, Payload: payload})
	headBeforeController, err := rec.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq before controller: %v", err)
	}
	cs := &controllerState{
		cityBeadStore:       store,
		beadStores:          map[string]beads.Store{},
		eventProv:           rec,
		beadEventStartSeq:   headBeforeController,
		beadEventStartSeqOK: true,
	}

	// The watcher starts strictly after headBeforeController, so only the
	// authoritative-store recovery pass can observe this already-terminal graph.
	if retry := cs.recoverMoleculeLifecycleStores(); retry {
		t.Fatal("startup lifecycle recovery requested retry after a stable close")
	}
	after, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root after startup recovery: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed despite pre-controller child event", after.Status)
	}

	rootEvents := make([]events.Event, 0, 2)
	for _, event := range rec.Events {
		if event.Subject == root.ID && (event.Type == events.BeadClosed || event.Type == events.MoleculeResolved) {
			rootEvents = append(rootEvents, event)
		}
	}
	if len(rootEvents) != 2 || rootEvents[0].Type != events.BeadClosed || rootEvents[1].Type != events.MoleculeResolved {
		t.Fatalf("root lifecycle events = %+v, want one ordered bead.closed/molecule.resolved pair", rootEvents)
	}
	if rootEvents[0].Seq <= headBeforeController || rootEvents[1].Seq <= rootEvents[0].Seq {
		t.Fatalf("root lifecycle seqs = (%d, %d), want ordered seqs after pre-controller head %d", rootEvents[0].Seq, rootEvents[1].Seq, headBeforeController)
	}

	if retry := cs.recoverMoleculeLifecycleStores(); retry {
		t.Fatal("idempotent lifecycle recovery requested retry")
	}
	if got := len(rec.Events); got != 3 {
		t.Fatalf("events after idempotent recovery = %d, want original child close plus one root pair", got)
	}
}

func TestControllerMoleculeLifecycleRecoveryDiscoversUnmarkedClosedSourceWorkflow(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name: "alpha",
			Path: filepath.Join(cityPath, "alpha"),
		}},
	}
	rootStore := beads.NewMemStore()
	sourceStore := beads.NewMemStore()
	source, err := sourceStore.Create(beads.Bead{Title: "remote source", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := sourceStore.Close(source.ID); err != nil {
		t.Fatalf("Close source before controller construction: %v", err)
	}
	root, err := rootStore.Create(beads.Bead{
		Title: "stepless remote workflow",
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
		t.Fatalf("Create workflow root: %v", err)
	}

	rec := events.NewFake()
	cs := &controllerState{
		cfg:           cfg,
		cityName:      "test-city",
		cityPath:      cityPath,
		cityBeadStore: rootStore,
		beadStores:    map[string]beads.Store{"alpha": sourceStore},
		eventProv:     rec,
	}
	if retry := cs.recoverMoleculeLifecycleStores(); retry {
		t.Fatal("source-workflow recovery requested retry after a stable close")
	}
	after, err := rootStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get workflow root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("workflow root status = %q, want closed from authoritative remote source state", after.Status)
	}
	assertSingleOrderedMoleculeLifecycle(t, rec.Events)
}

func TestControllerMoleculeLifecycleRecoveryResolvesPersistedSourceStoreRef(t *testing.T) {
	tests := []struct {
		name               string
		rootSourceStoreRef string
		owningSourceClosed bool
		collisionClosed    bool
		wantRootClosed     bool
	}{
		{
			name:               "matching owning source is terminal",
			rootSourceStoreRef: "rig:alpha",
			owningSourceClosed: true,
			collisionClosed:    false,
			wantRootClosed:     true,
		},
		{
			name:               "same ID is terminal only in the wrong store",
			rootSourceStoreRef: "rig:alpha",
			owningSourceClosed: false,
			collisionClosed:    true,
			wantRootClosed:     false,
		},
		{
			name:               "unknown persisted source ref fails closed",
			rootSourceStoreRef: "rig:missing",
			owningSourceClosed: false,
			collisionClosed:    true,
			wantRootClosed:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs: []config.Rig{
					{Name: "alpha", Path: filepath.Join(cityPath, "alpha")},
					{Name: "beta", Path: filepath.Join(cityPath, "beta")},
				},
			}
			cityStore := beads.NewMemStore()
			alphaStore := beads.NewMemStore()
			betaStore := beads.NewMemStore()

			source, err := alphaStore.Create(beads.Bead{Title: "owning source", Type: "task"})
			if err != nil {
				t.Fatalf("Create owning source: %v", err)
			}
			collision, err := betaStore.Create(beads.Bead{Title: "same ID in another rig", Type: "task"})
			if err != nil {
				t.Fatalf("Create colliding source: %v", err)
			}
			cityCollision, err := cityStore.Create(beads.Bead{Title: "same ID in graph store", Type: "task"})
			if err != nil {
				t.Fatalf("Create city collision: %v", err)
			}
			if collision.ID != source.ID || cityCollision.ID != source.ID {
				t.Fatalf("test setup IDs differ: source=%q rig collision=%q city collision=%q", source.ID, collision.ID, cityCollision.ID)
			}
			if tt.owningSourceClosed {
				if err := alphaStore.Close(source.ID); err != nil {
					t.Fatalf("Close owning source: %v", err)
				}
			}
			if tt.collisionClosed {
				if err := betaStore.Close(collision.ID); err != nil {
					t.Fatalf("Close colliding source: %v", err)
				}
				if err := cityStore.Close(cityCollision.ID); err != nil {
					t.Fatalf("Close city collision: %v", err)
				}
			}

			root, err := cityStore.Create(beads.Bead{
				Title: "durable remote-source workflow",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
					beadmeta.SourceBeadIDMetadataKey:    source.ID,
					beadmeta.SourceStoreRefMetadataKey:  tt.rootSourceStoreRef,
					beadmeta.RootStoreRefMetadataKey:    "city:test-city",
				},
			})
			if err != nil {
				t.Fatalf("Create workflow root: %v", err)
			}
			_, prepared, err := prepareMoleculeLifecycleIntent(
				cityStore,
				root.ID,
				moleculeSourceAutocloseReason,
				"controller",
				time.Now().UTC(),
			)
			if err != nil {
				t.Fatalf("Prepare source lifecycle intent: %v", err)
			}

			rec := events.NewFake()
			cs := &controllerState{
				cfg:           cfg,
				cityName:      "test-city",
				cityPath:      cityPath,
				cityBeadStore: cityStore,
				beadStores: map[string]beads.Store{
					"alpha": alphaStore,
					"beta":  betaStore,
				},
				eventProv: rec,
			}
			if retry := cs.recoverMoleculeLifecycleStores(); retry {
				t.Fatal("controller recovery retry = true, want a stable recovery decision")
			}

			after, err := cityStore.Get(root.ID)
			if err != nil {
				t.Fatalf("Get recovered workflow root: %v", err)
			}
			if got := after.Status == "closed"; got != tt.wantRootClosed {
				t.Fatalf("workflow root closed = %v (status %q), want %v", got, after.Status, tt.wantRootClosed)
			}
			if tt.wantRootClosed {
				if after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
					after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
					t.Fatalf("recovered lifecycle metadata = %#v, want cleared", after.Metadata)
				}
				assertSingleOrderedMoleculeLifecycle(t, rec.Events)
				return
			}
			retained, err := decodeMoleculeLifecycleIntent(after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
			if err != nil || retained.IntentID != prepared.IntentID ||
				after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
				t.Fatalf("retained lifecycle intent = %+v err=%v metadata=%#v, want original durable owner", retained, err, after.Metadata)
			}
			if got := moleculeLifecycleTypes(rec.Events); len(got) != 0 {
				t.Fatalf("lifecycle events = %v, want none for unresolved or non-terminal owning source", got)
			}
		})
	}
}

func TestControllerMoleculeLifecycleRecoveryRequiresExactRootRefForAliasedStore(t *testing.T) {
	tests := []struct {
		name           string
		rootStoreRef   string
		wantRootClosed bool
		wantEventCount int
	}{
		{
			name:           "explicit root ref disambiguates aliased handle",
			rootStoreRef:   "city:test-city",
			wantRootClosed: true,
			wantEventCount: 2,
		},
		{
			name:           "missing root ref fails closed for aliased handle",
			wantRootClosed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs: []config.Rig{{
					Name: "alpha",
					Path: filepath.Join(cityPath, "alpha"),
				}},
			}
			sharedStore := beads.NewMemStore()
			source, err := sharedStore.Create(beads.Bead{Title: "aliased source", Type: "task"})
			if err != nil {
				t.Fatalf("Create source: %v", err)
			}
			if err := sharedStore.Close(source.ID); err != nil {
				t.Fatalf("Close source: %v", err)
			}
			root, err := sharedStore.Create(beads.Bead{
				Title: "workflow on aliased store",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
					beadmeta.SourceBeadIDMetadataKey:    source.ID,
					beadmeta.SourceStoreRefMetadataKey:  "rig:alpha",
					beadmeta.RootStoreRefMetadataKey:    tt.rootStoreRef,
				},
			})
			if err != nil {
				t.Fatalf("Create workflow root: %v", err)
			}
			if _, _, err := prepareMoleculeLifecycleIntent(
				sharedStore,
				root.ID,
				moleculeSourceAutocloseReason,
				"controller",
				time.Now().UTC(),
			); err != nil {
				t.Fatalf("Prepare source lifecycle intent: %v", err)
			}

			rec := events.NewFake()
			cs := &controllerState{
				cfg:           cfg,
				cityName:      "test-city",
				cityPath:      cityPath,
				cityBeadStore: sharedStore,
				beadStores:    map[string]beads.Store{"alpha": sharedStore},
				eventProv:     rec,
			}
			if retry := cs.recoverMoleculeLifecycleStores(); retry {
				t.Fatal("controller recovery retry = true, want a stable recovery decision")
			}

			after, err := sharedStore.Get(root.ID)
			if err != nil {
				t.Fatalf("Get workflow root: %v", err)
			}
			if got := after.Status == "closed"; got != tt.wantRootClosed {
				t.Fatalf("workflow root closed = %v (status %q), want %v", got, after.Status, tt.wantRootClosed)
			}
			if got := len(moleculeLifecycleTypes(rec.Events)); got != tt.wantEventCount {
				t.Fatalf("lifecycle event count = %d, want %d", got, tt.wantEventCount)
			}
		})
	}
}

func TestBeadEventWatcherLatestSeqFailureStillStartsLifecycleRecovery(t *testing.T) {
	previousRecover := controllerRecoverMoleculeLifecycleIntents
	t.Cleanup(func() { controllerRecoverMoleculeLifecycleIntents = previousRecover })

	provider := &lifecycleLatestSeqFailProvider{Fake: events.NewFake()}
	store := beads.NewMemStore()
	recovered := make(chan struct{}, 1)
	controllerRecoverMoleculeLifecycleIntents = func(
		gotStore beads.Store,
		_ string,
		_ moleculeLifecycleStoreResolver,
		rec events.Recorder,
	) bool {
		if gotStore != store {
			t.Errorf("recovery store = %T, want city store", gotStore)
		}
		if rec != provider {
			t.Errorf("recovery recorder = %T, want failing provider", rec)
		}
		recovered <- struct{}{}
		return false
	}

	cs := &controllerState{
		cityBeadStore:       store,
		beadStores:          map[string]beads.Store{},
		eventProv:           provider,
		beadEventStartSeqOK: false,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs.startBeadEventWatcher(ctx)

	select {
	case <-recovered:
	case <-time.After(2 * time.Second):
		t.Fatal("LatestSeq failure prevented lifecycle recovery startup")
	}
	cs.stopBeadEventWorkers()
}

func waitForRecoveryCall(t *testing.T, entered <-chan int, want int, trigger string) {
	t.Helper()
	select {
	case got := <-entered:
		if got != want {
			t.Fatalf("%s recovery call = %d, want %d", trigger, got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not trigger recovery call %d", trigger, want)
	}
}

type lifecycleLatestSeqFailProvider struct {
	*events.Fake
}

func (p *lifecycleLatestSeqFailProvider) LatestSeq() (uint64, error) {
	return 0, errors.New("injected latest sequence failure")
}
