package main

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestMoleculeAutocloseDefersSidecarCleanupUntilRootLifecycle(t *testing.T) {
	base := beads.NewMemStore()
	work, err := base.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	root, err := base.Create(beads.Bead{
		Title: "queued workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   work.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	spec, err := base.Create(beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "implement",
			"gc.spec_for_ref": "implement",
		},
	})
	if err != nil {
		t.Fatalf("Create(spec): %v", err)
	}
	if err := base.Close(work.ID); err != nil {
		t.Fatalf("Close(work): %v", err)
	}

	rec := events.NewFake()
	updatedObserverEntered := make(chan struct{})
	releaseUpdatedObserver := make(chan struct{})
	defer func() {
		select {
		case <-releaseUpdatedObserver:
		default:
			close(releaseUpdatedObserver)
		}
	}()
	var blockFirstUpdated sync.Once
	cache := beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		if eventType == events.BeadUpdated && beadID == root.ID {
			blockFirstUpdated.Do(func() {
				close(updatedObserverEntered)
				<-releaseUpdatedObserver
			})
		}
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	priorUpdateDone := make(chan error, 1)
	go func() { priorUpdateDone <- cache.SetMetadata(root.ID, "prior_update", "queued") }()
	select {
	case <-updatedObserverEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior root observer was not invoked")
	}

	var stdout bytes.Buffer
	autocloseDone := make(chan moleculeAutocloseCompletion, 1)
	go func() {
		autocloseDone <- doMoleculeAutocloseWith(cache, "", rec, work.ID, &stdout)
	}()
	var completion moleculeAutocloseCompletion
	select {
	case completion = <-autocloseDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose waited for the blocked root observer")
	}

	rootBeforeRelease, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root before release): %v", err)
	}
	if rootBeforeRelease.Status != "closed" {
		t.Fatalf("root status before observer release = %q, want durable close", rootBeforeRelease.Status)
	}
	specBeforeRelease, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec before release): %v", err)
	}
	if specBeforeRelease.Status != "open" {
		t.Fatalf("spec status before root lifecycle delivery = %q, want open", specBeforeRelease.Status)
	}

	close(releaseUpdatedObserver)
	select {
	case err := <-priorUpdateDone:
		if err != nil {
			t.Fatalf("SetMetadata(prior_update): %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior root update did not finish after observer release")
	}
	waitDone := make(chan struct{})
	go func() {
		completion.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose completion did not include deferred sidecar cleanup")
	}

	specAfter, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec after release): %v", err)
	}
	if specAfter.Status != "closed" {
		t.Fatalf("spec status after root lifecycle delivery = %q, want closed", specAfter.Status)
	}
	if got := specAfter.Metadata["close_reason"]; got != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("spec close_reason = %q, want %q", got, sourceworkflow.WorkflowSpecSidecarClosedReason)
	}

	recorded, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	type lifecycleEvent struct {
		eventType string
		subject   string
	}
	var got []lifecycleEvent
	for _, event := range recorded {
		if event.Type == events.BeadClosed && (event.Subject == root.ID || event.Subject == spec.ID) ||
			event.Type == events.MoleculeResolved && event.Subject == root.ID {
			got = append(got, lifecycleEvent{eventType: event.Type, subject: event.Subject})
		}
	}
	want := []lifecycleEvent{
		{eventType: events.BeadClosed, subject: root.ID},
		{eventType: events.MoleculeResolved, subject: root.ID},
		{eventType: events.BeadClosed, subject: spec.ID},
	}
	if len(got) != len(want) {
		t.Fatalf("root/sidecar lifecycle = %+v, want %+v (all events: %+v)", got, want, recorded)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("root/sidecar lifecycle = %+v, want %+v (all events: %+v)", got, want, recorded)
		}
	}
}

func TestMoleculeAutocloseLosingSourceRaceDoesNotCloseSidecarsBeforeWinnerLifecycle(t *testing.T) {
	base := beads.NewMemStore()
	work, err := base.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	root, err := base.Create(beads.Bead{
		Title: "racing workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    work.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	spec, err := base.Create(beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(spec): %v", err)
	}
	if err := base.Close(work.ID); err != nil {
		t.Fatalf("Close(work): %v", err)
	}

	rec := events.NewFake()
	winnerObserverEntered := make(chan struct{})
	releaseWinnerObserver := make(chan struct{})
	defer func() {
		select {
		case <-releaseWinnerObserver:
		default:
			close(releaseWinnerObserver)
		}
	}()
	var blockWinnerObserver sync.Once
	winnerCache := beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		if eventType == events.BeadUpdated && beadID == root.ID {
			blockWinnerObserver.Do(func() {
				close(winnerObserverEntered)
				<-releaseWinnerObserver
			})
		}
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
	})
	loserCache := beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
	})
	if err := winnerCache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive(winner): %v", err)
	}
	if err := loserCache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive(loser): %v", err)
	}

	priorUpdateDone := make(chan error, 1)
	go func() { priorUpdateDone <- winnerCache.SetMetadata(root.ID, "prior_update", "queued") }()
	select {
	case <-winnerObserverEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("winner's prior root observer was not invoked")
	}

	winnerGate := &sourceAutocloseDiscoveryGateStore{
		Store:   winnerCache,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	loserGate := &sourceAutocloseDiscoveryGateStore{
		Store:   loserCache,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	type autocloseResult struct {
		completion moleculeAutocloseCompletion
		name       string
	}
	results := make(chan autocloseResult, 2)
	go func() {
		results <- autocloseResult{
			completion: doMoleculeAutocloseWith(base, "", rec, work.ID, io.Discard, winnerGate),
			name:       "winner",
		}
	}()
	go func() {
		results <- autocloseResult{
			completion: doMoleculeAutocloseWith(base, "", rec, work.ID, io.Discard, loserGate),
			name:       "loser",
		}
	}()
	for name, entered := range map[string]<-chan struct{}{
		"winner": winnerGate.entered,
		"loser":  loserGate.entered,
	} {
		select {
		case <-entered:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatalf("%s autoclose did not discover the live root", name)
		}
	}

	close(winnerGate.release)
	var winnerCompletion moleculeAutocloseCompletion
	select {
	case result := <-results:
		if result.name != "winner" {
			t.Fatalf("first autoclose result = %s, want winner", result.name)
		}
		winnerCompletion = result.completion
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("winner autoclose did not return after its durable root close")
	}
	closedRoot, err := base.Get(root.ID)
	if err != nil || closedRoot.Status != "closed" {
		t.Fatalf("winner root = %+v err=%v, want durable closed", closedRoot, err)
	}

	close(loserGate.release)
	var loserCompletion moleculeAutocloseCompletion
	select {
	case result := <-results:
		if result.name != "loser" {
			t.Fatalf("second autoclose result = %s, want loser", result.name)
		}
		loserCompletion = result.completion
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("loser autoclose did not return after observing the winner")
	}

	beforeDelivery, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec before winner delivery): %v", err)
	}
	if beforeDelivery.Status != "open" {
		t.Fatalf("losing autoclose closed sidecar before winner root lifecycle: status=%q", beforeDelivery.Status)
	}

	close(releaseWinnerObserver)
	select {
	case err := <-priorUpdateDone:
		if err != nil {
			t.Fatalf("SetMetadata(prior_update): %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("winner's prior update did not finish after observer release")
	}
	for name, completion := range map[string]moleculeAutocloseCompletion{
		"winner": winnerCompletion,
		"loser":  loserCompletion,
	} {
		done := make(chan struct{})
		go func() {
			completion.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatalf("%s completion did not finish after winner lifecycle delivery", name)
		}
	}
	afterDelivery, err := base.Get(spec.ID)
	if err != nil || afterDelivery.Status != "closed" {
		t.Fatalf("sidecar after winner lifecycle = %+v err=%v, want closed", afterDelivery, err)
	}

	recorded, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("List events: %v", err)
	}
	rootClosedAt, rootResolvedAt, sidecarClosedAt := -1, -1, -1
	for i, event := range recorded {
		switch {
		case event.Type == events.BeadClosed && event.Subject == root.ID:
			rootClosedAt = i
		case event.Type == events.MoleculeResolved && event.Subject == root.ID:
			rootResolvedAt = i
		case event.Type == events.BeadClosed && event.Subject == spec.ID:
			sidecarClosedAt = i
		}
	}
	if rootClosedAt < 0 || rootResolvedAt <= rootClosedAt || sidecarClosedAt <= rootResolvedAt {
		t.Fatalf("root/sidecar lifecycle order = closed@%d resolved@%d sidecar@%d: %+v", rootClosedAt, rootResolvedAt, sidecarClosedAt, recorded)
	}
}

type sourceAutocloseDiscoveryGateStore struct {
	beads.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *sourceAutocloseDiscoveryGateStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *sourceAutocloseDiscoveryGateStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err == nil && query.Metadata[beadmeta.SourceBeadIDMetadataKey] != "" && !query.IncludeClosed {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	return rows, err
}

func (s *sourceAutocloseDiscoveryGateStore) CloseWithReasonIfOpen(id, reason string) (beads.CloseTransition, error) {
	closer, ok := beads.CloseTransitionerFor(s.Store)
	if !ok {
		return beads.CloseTransition{}, beads.ErrCloseTransitionUnsupported
	}
	return closer.CloseWithReasonIfOpen(id, reason)
}

func (s *sourceAutocloseDiscoveryGateStore) CloseObserverSuppressorHandle() (beads.CloseObserverSuppressor, bool) {
	return beads.CloseObserverSuppressorFor(s.Store)
}

func (s *sourceAutocloseDiscoveryGateStore) ObserverBarrierHandle() (beads.ObserverBarrier, bool) {
	return beads.ObserverBarrierFor(s.Store)
}

func (s *sourceAutocloseDiscoveryGateStore) WithLifecycleMetadataTransaction(id string, fn func(beads.LifecycleMetadataTransaction) error) error {
	return beads.WithLifecycleMetadataTransaction(s.Store, id, fn)
}

func TestMoleculeAutocloseCompletionWaitsForQueuedSidecarLifecycle(t *testing.T) {
	base := beads.NewMemStore()
	work, err := base.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	root, err := base.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   work.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	spec, err := base.Create(beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "implement",
			"gc.spec_for_ref": "implement",
		},
	})
	if err != nil {
		t.Fatalf("Create(spec): %v", err)
	}
	if err := base.Close(work.ID); err != nil {
		t.Fatalf("Close(work): %v", err)
	}

	rec := events.NewFake()
	sidecarObserverEntered := make(chan struct{})
	releaseSidecarObserver := make(chan struct{})
	defer func() {
		select {
		case <-releaseSidecarObserver:
		default:
			close(releaseSidecarObserver)
		}
	}()
	var blockFirstSidecarUpdate sync.Once
	cache := beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		if eventType == events.BeadUpdated && beadID == spec.ID {
			blockFirstSidecarUpdate.Do(func() {
				close(sidecarObserverEntered)
				<-releaseSidecarObserver
			})
		}
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	priorSidecarUpdateDone := make(chan error, 1)
	go func() { priorSidecarUpdateDone <- cache.SetMetadata(spec.ID, "prior_update", "queued") }()
	select {
	case <-sidecarObserverEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior sidecar observer was not invoked")
	}

	var stdout bytes.Buffer
	completion := doMoleculeAutocloseWith(cache, "", rec, work.ID, &stdout)
	waitDone := make(chan struct{})
	go func() {
		completion.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("autoclose completion finished while sidecar lifecycle delivery was queued")
	case <-time.After(25 * time.Millisecond):
	}

	beforeRelease, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("List before sidecar release: %v", err)
	}
	rootClosedAt, rootResolvedAt := -1, -1
	for i, event := range beforeRelease {
		switch {
		case event.Type == events.BeadClosed && event.Subject == root.ID:
			rootClosedAt = i
		case event.Type == events.MoleculeResolved && event.Subject == root.ID:
			rootResolvedAt = i
		case event.Type == events.BeadClosed && event.Subject == spec.ID:
			t.Fatalf("sidecar bead.closed published before its queued observer was released: %+v", beforeRelease)
		}
	}
	if rootClosedAt < 0 || rootResolvedAt <= rootClosedAt {
		t.Fatalf("root lifecycle before sidecar release = %+v, want bead.closed then molecule.resolved", beforeRelease)
	}

	close(releaseSidecarObserver)
	select {
	case err := <-priorSidecarUpdateDone:
		if err != nil {
			t.Fatalf("SetMetadata(prior_update): %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior sidecar update did not finish after observer release")
	}
	select {
	case <-waitDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose completion did not finish after sidecar lifecycle delivery")
	}

	afterRelease, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("List after sidecar release: %v", err)
	}
	sidecarClosedAt := -1
	for i, event := range afterRelease {
		if event.Type == events.BeadClosed && event.Subject == spec.ID {
			sidecarClosedAt = i
			break
		}
	}
	if sidecarClosedAt <= rootResolvedAt {
		t.Fatalf("lifecycle order = root resolved@%d sidecar closed@%d, want sidecar last: %+v", rootResolvedAt, sidecarClosedAt, afterRelease)
	}
}

func TestMoleculeAutocloseDefersResolutionUntilQueuedCloseObserver(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "queued molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("Close setup root: %v", err)
	}

	rec := events.NewFake()
	updatedObserverEntered := make(chan struct{})
	releaseUpdatedObserver := make(chan struct{})
	defer func() {
		select {
		case <-releaseUpdatedObserver:
		default:
			close(releaseUpdatedObserver)
		}
	}()
	cache := beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		if eventType == events.BeadUpdated && beadID == root.ID {
			close(updatedObserverEntered)
			<-releaseUpdatedObserver
		}
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	reopenDone := make(chan error, 1)
	go func() { reopenDone <- cache.Reopen(root.ID) }()
	select {
	case <-updatedObserverEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reopen observer was not invoked")
	}

	var stdout bytes.Buffer
	announceDone := make(chan moleculeCloseAnnouncement, 1)
	go func() {
		announceDone <- announceClosedMoleculeResult(cache, rec, root, moleculeAutocloseReason, &stdout)
	}()
	var announcement moleculeCloseAnnouncement
	select {
	case announcement = <-announceDone:
		if !announcement.closed {
			t.Error("announceClosedMoleculeResult returned closed=false")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Error("announceClosedMoleculeResult waited for an in-flight observer callback")
	}
	select {
	case <-announcement.lifecycleDone:
		t.Error("lifecycle completion finished before the queued bead.closed observer")
	default:
	}

	beforeRelease, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("List before observer release: %v", err)
	}
	if got := len(eventsOfType(beforeRelease, events.MoleculeResolved)); got != 0 {
		t.Errorf("molecule.resolved events before queued bead.closed delivery = %d, want 0: %+v", got, beforeRelease)
	}

	close(releaseUpdatedObserver)
	select {
	case err := <-reopenDone:
		if err != nil {
			t.Fatalf("Reopen: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Reopen did not finish after observer release")
	}
	select {
	case <-announcement.lifecycleDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("lifecycle completion did not finish after queued observer delivery")
	}

	afterRelease, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("List after observer release: %v", err)
	}
	closedAt, resolvedAt := -1, -1
	closedCount, resolvedCount := 0, 0
	for i, event := range afterRelease {
		if event.Subject != root.ID {
			continue
		}
		switch event.Type {
		case events.BeadClosed:
			closedCount++
			closedAt = i
		case events.MoleculeResolved:
			resolvedCount++
			resolvedAt = i
		}
	}
	if closedCount != 1 || resolvedCount != 1 {
		t.Fatalf("lifecycle counts = bead.closed:%d molecule.resolved:%d, want 1 each: %+v", closedCount, resolvedCount, afterRelease)
	}
	if closedAt >= resolvedAt {
		t.Fatalf("lifecycle order = bead.closed@%d molecule.resolved@%d, want bead.closed first: %+v", closedAt, resolvedAt, afterRelease)
	}
}

func TestMoleculeAutocloseReentrantObserverPreservesLifecycleOrder(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "reentrant molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("Close setup root: %v", err)
	}

	rec := events.NewFake()
	var (
		cache     *beads.CachingStore
		stdout    bytes.Buffer
		announced bool
	)
	cache = beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
		if eventType == events.BeadUpdated && beadID == root.ID {
			announced = announceClosedMolecule(cache, rec, root, moleculeAutocloseReason, &stdout)
		}
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	reopenDone := make(chan error, 1)
	go func() { reopenDone <- cache.Reopen(root.ID) }()
	select {
	case err := <-reopenDone:
		if err != nil {
			t.Fatalf("Reopen: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reentrant autoclose deadlocked inside the observer callback")
	}
	if !announced {
		t.Fatal("reentrant announceClosedMolecule returned false")
	}

	recorded, err := rec.List(events.Filter{Subject: root.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{events.BeadUpdated, events.BeadClosed, events.MoleculeResolved}
	if len(recorded) != len(want) {
		t.Fatalf("lifecycle events = %+v, want types %v", recorded, want)
	}
	for i, event := range recorded {
		if event.Type != want[i] {
			t.Fatalf("lifecycle event %d = %q, want %q: %+v", i, event.Type, want[i], recorded)
		}
	}
}

func TestMoleculeAutocloseLegacyCacheDefersLifecycleUntilQueuedMetadataObserver(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "legacy queued molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := events.NewFake()
	updatedObserverEntered := make(chan struct{})
	releaseUpdatedObserver := make(chan struct{})
	defer func() {
		select {
		case <-releaseUpdatedObserver:
		default:
			close(releaseUpdatedObserver)
		}
	}()
	var blockFirstUpdated sync.Once
	cache := beads.NewCachingStoreForTest(
		&moleculeAutocloseUnsupportedTransitionStore{Store: base},
		func(eventType, beadID string, payload json.RawMessage) {
			if eventType == events.BeadUpdated && beadID == root.ID {
				blockFirstUpdated.Do(func() {
					close(updatedObserverEntered)
					<-releaseUpdatedObserver
				})
			}
			rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
		},
	)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	priorUpdateDone := make(chan error, 1)
	go func() { priorUpdateDone <- cache.SetMetadata(root.ID, "prior_update", "queued") }()
	select {
	case <-updatedObserverEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior metadata observer was not invoked")
	}

	var stdout bytes.Buffer
	announceDone := make(chan bool, 1)
	go func() {
		announceDone <- announceClosedMolecule(cache, rec, root, moleculeAutocloseReason, &stdout)
	}()
	select {
	case announced := <-announceDone:
		if !announced {
			t.Error("announceClosedMolecule returned false")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("legacy announceClosedMolecule waited for an in-flight observer callback")
	}

	beforeRelease, err := rec.List(events.Filter{Subject: root.ID})
	if err != nil {
		t.Fatalf("List before observer release: %v", err)
	}
	for _, event := range beforeRelease {
		if event.Type == events.BeadClosed || event.Type == events.MoleculeResolved {
			t.Fatalf("%s published before queued bead.updated delivery: %+v", event.Type, beforeRelease)
		}
	}

	close(releaseUpdatedObserver)
	select {
	case err := <-priorUpdateDone:
		if err != nil {
			t.Fatalf("SetMetadata(prior_update): %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior metadata update did not finish after observer release")
	}

	recorded, err := rec.List(events.Filter{Subject: root.ID})
	if err != nil {
		t.Fatalf("List after observer release: %v", err)
	}
	want := []string{
		events.BeadUpdated, // prior metadata write
		events.BeadUpdated, // close reason and durable lifecycle intent
		events.BeadUpdated, // pending marker published last
		events.BeadClosed,
		events.MoleculeResolved,
		events.BeadUpdated, // completed-publication marker stamped before pending clears
		events.BeadUpdated, // pending marker cleanup
		events.BeadUpdated, // lifecycle intent cleanup
	}
	if len(recorded) != len(want) {
		t.Fatalf("lifecycle events = %+v, want types %v", recorded, want)
	}
	for i, event := range recorded {
		if event.Type != want[i] {
			t.Fatalf("lifecycle event %d = %q, want %q: %+v", i, event.Type, want[i], recorded)
		}
	}

	decodeSnapshot := func(index int) beads.Bead {
		t.Helper()
		var snapshot beads.Bead
		if err := json.Unmarshal(recorded[index].Payload, &snapshot); err != nil {
			t.Fatalf("decode lifecycle event %d payload: %v", index, err)
		}
		return snapshot
	}
	prior := decodeSnapshot(0)
	prepared := decodeSnapshot(1)
	pending := decodeSnapshot(2)
	closed := decodeSnapshot(3)
	completed := decodeSnapshot(5)
	markerCleared := decodeSnapshot(6)
	intentCleared := decodeSnapshot(7)
	intentRaw := prepared.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]
	decodedIntent, err := decodeMoleculeLifecycleIntent(intentRaw)
	if err != nil {
		t.Fatalf("decode prepared intent: %v", err)
	}
	completedID := decodedIntent.IntentID
	if prior.Status != "open" || prior.Metadata["prior_update"] != "queued" || prior.Metadata["close_reason"] != "" {
		t.Fatalf("prior metadata snapshot = %+v, want only queued prior update on open root", prior)
	}
	if prepared.Status != "open" || prepared.Metadata["close_reason"] != moleculeAutocloseReason ||
		intentRaw == "" || prepared.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" {
		t.Fatalf("prepared metadata snapshot = %+v, want reason+intent before pending marker", prepared)
	}
	if pending.Status != "open" || pending.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != intentRaw ||
		pending.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("pending metadata snapshot = %+v, want v1 marker after durable intent", pending)
	}
	if closed.Status != "closed" || closed.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != intentRaw ||
		closed.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("bead.closed snapshot = %+v, want closed root with pending intent", closed)
	}
	// The completed marker is stamped before the pending marker is cleared, so a
	// crash between the two still records that this intent was published.
	if completed.Status != "closed" || completed.Metadata[beadmeta.MoleculeLifecycleCompletedMetadataKey] != completedID ||
		completed.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("completed-marker snapshot = %+v, want completed=%q with pending still set", completed, completedID)
	}
	if markerCleared.Status != "closed" || markerCleared.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
		markerCleared.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != intentRaw ||
		markerCleared.Metadata[beadmeta.MoleculeLifecycleCompletedMetadataKey] != completedID {
		t.Fatalf("marker-clear snapshot = %+v, want intent retained and completed marker set until marker clears", markerCleared)
	}
	if intentCleared.Status != "closed" || intentCleared.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
		intentCleared.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" ||
		intentCleared.Metadata[beadmeta.MoleculeLifecycleCompletedMetadataKey] != completedID {
		t.Fatalf("intent-clear snapshot = %+v, want lifecycle intent removed and completed marker retained", intentCleared)
	}

	projector := runproj.NewProjector()
	projector.Apply(recorded)
	projected := projector.Beads()
	if len(projected) != 1 || projected[0].ID != root.ID || projected[0].Status != "closed" {
		t.Fatalf("projected beads = %+v, want molecule %s closed", projected, root.ID)
	}
}
