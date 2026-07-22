package main

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestEligibleMoleculeAutocloseMemStoreTopologyReadDoesNotDeadlock(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "eligible molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	step, err := store.Create(beads.Bead{Title: "terminal step", Type: "step", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}
	if err := store.Close(step.ID); err != nil {
		t.Fatalf("Close step: %v", err)
	}

	recorder := events.NewFake()
	done := make(chan bool, 1)
	go func() {
		var stdout bytes.Buffer
		done <- doMoleculeAutocloseWith(store, "", recorder, step.ID, &stdout).Wait()
	}()
	select {
	case retry := <-done:
		if retry {
			t.Fatal("eligible autoclose retry = true, want lifecycle complete")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("eligible MemStore autoclose deadlocked during lifecycle topology reads")
	}
	after, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed", after.Status)
	}
}

func TestEligibleMoleculeAutocloseDoesNotUseAtomicShortcut(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "eligible molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	step, err := base.Create(beads.Bead{Title: "terminal step", Type: "step", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}
	if err := base.Close(step.ID); err != nil {
		t.Fatalf("Close step: %v", err)
	}

	store := &moleculeEligibilityAtomicSpyStore{Store: base, rootID: root.ID}
	rec := events.NewFake()
	var stdout bytes.Buffer
	if retry := doMoleculeAutocloseWith(store, "", rec, step.ID, &stdout).Wait(); retry {
		t.Fatal("eligible autoclose retry = true, want lifecycle complete")
	}
	if store.atomicCalls != 0 {
		t.Fatalf("atomic close shortcut calls = %d, want 0 for eligibility-gated autoclose", store.atomicCalls)
	}
	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed", after.Status)
	}
	assertSingleOrderedMoleculeLifecycle(t, rec.Events)
}

func TestEligibleMoleculeAutocloseRetainsIntentWhenEligibilityChangesBeforeClose(t *testing.T) {
	t.Run("source reopened", func(t *testing.T) {
		base := beads.NewMemStore()
		source, err := base.Create(beads.Bead{Title: "source work", Type: "task"})
		if err != nil {
			t.Fatalf("Create source: %v", err)
		}
		if err := base.Close(source.ID); err != nil {
			t.Fatalf("Close source: %v", err)
		}
		root, err := base.Create(beads.Bead{
			Title: "source workflow",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				beadmeta.SourceBeadIDMetadataKey:    source.ID,
			},
		})
		if err != nil {
			t.Fatalf("Create root: %v", err)
		}

		store := &moleculeEligibilityAtomicSpyStore{
			Store:  base,
			rootID: root.ID,
			onPending: func() error {
				return base.Reopen(source.ID)
			},
		}
		rec := events.NewFake()
		var stdout bytes.Buffer
		if retry := doMoleculeAutocloseWith(store, "", rec, source.ID, &stdout).Wait(); retry {
			t.Fatal("ineligible source autoclose retry = true, want retained without hot retry")
		}
		assertRetainedEligibleMoleculeIntent(t, base, root.ID, store, rec.Events)

		if err := base.Close(source.ID); err != nil {
			t.Fatalf("Close source again: %v", err)
		}
		assertRecoveredEligibleMoleculeLifecycle(t, store, base, root.ID, rec)
	})

	t.Run("open descendant introduced", func(t *testing.T) {
		base := beads.NewMemStore()
		root, err := base.Create(beads.Bead{Title: "molecule root", Type: "molecule"})
		if err != nil {
			t.Fatalf("Create root: %v", err)
		}
		terminalStep, err := base.Create(beads.Bead{Title: "terminal step", Type: "step", ParentID: root.ID})
		if err != nil {
			t.Fatalf("Create terminal step: %v", err)
		}
		if err := base.Close(terminalStep.ID); err != nil {
			t.Fatalf("Close terminal step: %v", err)
		}

		var introduced beads.Bead
		store := &moleculeEligibilityAtomicSpyStore{
			Store:  base,
			rootID: root.ID,
			onPending: func() error {
				var createErr error
				introduced, createErr = base.Create(beads.Bead{
					Title:    "late open descendant",
					Type:     "step",
					ParentID: root.ID,
				})
				return createErr
			},
		}
		rec := events.NewFake()
		var stdout bytes.Buffer
		if retry := doMoleculeAutocloseWith(store, "", rec, terminalStep.ID, &stdout).Wait(); retry {
			t.Fatal("ineligible descendant autoclose retry = true, want retained without hot retry")
		}
		assertRetainedEligibleMoleculeIntent(t, base, root.ID, store, rec.Events)
		if introduced.ID == "" {
			t.Fatal("open descendant was not introduced after lifecycle prepare")
		}

		if err := base.Close(introduced.ID); err != nil {
			t.Fatalf("Close introduced descendant: %v", err)
		}
		assertRecoveredEligibleMoleculeLifecycle(t, store, base, root.ID, rec)
	})
}

func TestSourceMoleculeAutocloseRevalidatesThroughOwningSourceStore(t *testing.T) {
	sourceStore := beads.NewMemStore()
	source, err := sourceStore.Create(beads.Bead{Title: "remote source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := sourceStore.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}

	graphStore := beads.NewMemStore()
	collision, err := graphStore.Create(beads.Bead{Title: "unrelated colliding graph bead", Type: "task"})
	if err != nil {
		t.Fatalf("Create graph collision: %v", err)
	}
	if collision.ID != source.ID {
		t.Fatalf("test setup IDs differ: source=%q collision=%q", source.ID, collision.ID)
	}
	root, err := graphStore.Create(beads.Bead{
		Title: "remote-source workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
			beadmeta.SourceStoreRefMetadataKey:  "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create graph root: %v", err)
	}

	rec := events.NewFake()
	var stdout bytes.Buffer
	if retry := doMoleculeAutocloseWithStoreRefs(sourceStore, "rig:alpha", graphStore, "city:test", rec, source.ID, &stdout).Wait(); retry {
		t.Fatal("remote-source autoclose retry = true, want lifecycle complete")
	}
	after, err := graphStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get graph root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("graph root status = %q, want closed from terminal owning source", after.Status)
	}
	collisionAfter, err := graphStore.Get(collision.ID)
	if err != nil {
		t.Fatalf("Get graph collision: %v", err)
	}
	if collisionAfter.Status != "open" {
		t.Fatalf("unrelated graph collision status = %q, want open", collisionAfter.Status)
	}
	assertSingleOrderedMoleculeLifecycle(t, rec.Events)
}

func TestRecoverSourceMoleculeIntentDoesNotGuessCollidingSourceStore(t *testing.T) {
	sourceStore := beads.NewMemStore()
	source, err := sourceStore.Create(beads.Bead{Title: "remote source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := sourceStore.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}

	graphStore := beads.NewMemStore()
	collision, err := graphStore.Create(beads.Bead{Title: "closed colliding graph bead", Type: "task"})
	if err != nil {
		t.Fatalf("Create graph collision: %v", err)
	}
	if collision.ID != source.ID {
		t.Fatalf("test setup IDs differ: source=%q collision=%q", source.ID, collision.ID)
	}
	if err := graphStore.Close(collision.ID); err != nil {
		t.Fatalf("Close graph collision: %v", err)
	}
	root, err := graphStore.Create(beads.Bead{
		Title: "pending remote-source workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
			beadmeta.SourceStoreRefMetadataKey:  "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create graph root: %v", err)
	}
	_, prepared, err := prepareMoleculeLifecycleIntent(
		graphStore,
		root.ID,
		moleculeSourceAutocloseReason,
		"controller",
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("Prepare source lifecycle intent: %v", err)
	}

	rec := events.NewFake()
	if retry := recoverMoleculeLifecycleIntents(graphStore, rec); retry {
		t.Fatal("conservative remote-source recovery retry = true, want retained without hot retry")
	}
	retained, err := graphStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get retained graph root: %v", err)
	}
	if retained.Status != "open" {
		t.Fatalf("recovery used colliding graph source and closed root: status=%q", retained.Status)
	}
	retainedIntent, err := decodeMoleculeLifecycleIntent(retained.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil || retainedIntent.IntentID != prepared.IntentID ||
		retained.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("retained remote intent = %+v err=%v metadata=%v, want original v1 owner", retainedIntent, err, retained.Metadata)
	}
	if got := moleculeLifecycleTypes(rec.Events); len(got) != 0 {
		t.Fatalf("remote recovery lifecycle events = %v, want none without source-store resolution", got)
	}

	var stdout bytes.Buffer
	if retry := doMoleculeAutocloseWithStoreRefs(sourceStore, "rig:alpha", graphStore, "city:test", rec, source.ID, &stdout).Wait(); retry {
		t.Fatal("resolved remote-source autoclose retry = true, want lifecycle complete")
	}
	closed, err := graphStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get recovered graph root: %v", err)
	}
	if closed.Status != "closed" ||
		closed.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
		closed.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
		t.Fatalf("resolved remote root = %+v, want closed with intent cleared", closed)
	}
	assertSingleOrderedMoleculeLifecycle(t, rec.Events)
	if retry := recoverMoleculeLifecycleIntents(graphStore, rec); retry {
		t.Fatal("idempotent remote-source recovery retry = true, want complete")
	}
	assertSingleOrderedMoleculeLifecycle(t, rec.Events)
}

func TestSourceMoleculeAutocloseFencesReopenAfterTerminalRead(t *testing.T) {
	sourceBase := beads.NewMemStore()
	source, err := sourceBase.Create(beads.Bead{Title: "remote source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := sourceBase.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}
	sourceStore := &sourceTerminalReadGateStore{
		Store:        sourceBase,
		sourceID:     source.ID,
		terminalRead: make(chan struct{}),
		release:      make(chan struct{}),
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
		},
	})
	if err != nil {
		t.Fatalf("Create graph root: %v", err)
	}

	recorder := events.NewFake()
	autocloseDone := make(chan bool, 1)
	go func() {
		var stdout bytes.Buffer
		autocloseDone <- doMoleculeAutocloseWithStoreRefs(
			sourceStore, "rig:alpha", graphStore, "city:test", recorder, source.ID, &stdout,
		).Wait()
	}()
	select {
	case <-sourceStore.terminalRead:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not reach its fenced terminal source read")
	}

	reopenDone := make(chan error, 1)
	go func() { reopenDone <- sourceBase.Reopen(source.ID) }()
	select {
	case err := <-reopenDone:
		t.Fatalf("source reopen escaped the lifecycle fence before root close: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(sourceStore.release)

	select {
	case retry := <-autocloseDone:
		if retry {
			t.Fatal("source autoclose retry = true, want lifecycle complete")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not finish after releasing the source read")
	}
	select {
	case err := <-reopenDone:
		if err != nil {
			t.Fatalf("Reopen source: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("source reopen did not finish after the root close released its fence")
	}

	after, err := graphStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get graph root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("graph root status = %q, want closed before the later source reopen", after.Status)
	}
}

func TestSourceMoleculeAutocloseRequiresPhysicalRefsAcrossStores(t *testing.T) {
	for _, tt := range []struct {
		name      string
		sourceRef string
		rootRef   string
	}{
		{name: "missing source ref", rootRef: "city:test"},
		{name: "missing root ref", sourceRef: "rig:alpha"},
		{name: "unresolved whitespace refs", sourceRef: "  ", rootRef: "  "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sourceStore := beads.NewMemStore()
			source, err := sourceStore.Create(beads.Bead{Title: "remote source", Type: "task"})
			if err != nil {
				t.Fatalf("Create source: %v", err)
			}
			if err := sourceStore.Close(source.ID); err != nil {
				t.Fatalf("Close source: %v", err)
			}
			graphStore := beads.NewMemStore()
			root, err := graphStore.Create(beads.Bead{
				Title: "remote workflow",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
					beadmeta.SourceBeadIDMetadataKey:    source.ID,
					beadmeta.SourceStoreRefMetadataKey:  "rig:alpha",
				},
			})
			if err != nil {
				t.Fatalf("Create root: %v", err)
			}

			var stdout bytes.Buffer
			rec := events.NewFake()
			if retry := doMoleculeAutocloseWithStoreRefs(
				sourceStore, tt.sourceRef, graphStore, tt.rootRef, rec, source.ID, &stdout,
			).Wait(); retry {
				t.Fatal("unresolved-ref autoclose retry = true, want conservative no-op")
			}
			after, err := graphStore.Get(root.ID)
			if err != nil {
				t.Fatalf("Get root: %v", err)
			}
			if after.Status != "open" || after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
				after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" || after.Metadata["close_reason"] != "" {
				t.Fatalf("root mutated with unresolved physical refs: %+v", after)
			}
			if got := moleculeLifecycleTypes(rec.Events); len(got) != 0 {
				t.Fatalf("lifecycle events = %v, want none", got)
			}
		})
	}
}

func TestSourceMoleculeAutocloseDoesNotMatchRefLessRootAcrossStores(t *testing.T) {
	sourceStore := beads.NewMemStore()
	source, err := sourceStore.Create(beads.Bead{Title: "remote source", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := sourceStore.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}
	graphStore := beads.NewMemStore()
	root, err := graphStore.Create(beads.Bead{
		Title: "legacy ref-less workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}

	var stdout bytes.Buffer
	if retry := doMoleculeAutocloseWithStoreRefs(
		sourceStore, "rig:alpha", graphStore, "city:test", events.Discard, source.ID, &stdout,
	).Wait(); retry {
		t.Fatal("ref-less cross-store autoclose retry = true, want conservative no-op")
	}
	after, err := graphStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if after.Status != "open" || after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" {
		t.Fatalf("ref-less cross-store root was mutated: %+v", after)
	}
}

func assertRetainedEligibleMoleculeIntent(
	t *testing.T,
	base beads.Store,
	rootID string,
	store *moleculeEligibilityAtomicSpyStore,
	recorded []events.Event,
) {
	t.Helper()
	if store.atomicCalls != 0 {
		t.Fatalf("atomic close shortcut calls = %d, want 0 for eligibility-gated autoclose", store.atomicCalls)
	}
	after, err := base.Get(rootID)
	if err != nil {
		t.Fatalf("Get retained root: %v", err)
	}
	if after.Status != "open" {
		t.Fatalf("retained root status = %q, want open", after.Status)
	}
	if after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("pending lifecycle marker = %q, want %q", after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey], moleculeLifecycleVersionV1)
	}
	intent, err := decodeMoleculeLifecycleIntent(after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil || intent.IntentID == "" {
		t.Fatalf("retained lifecycle intent = %+v, err=%v", intent, err)
	}
	if got := moleculeLifecycleTypes(recorded); len(got) != 0 {
		t.Fatalf("lifecycle events while root was ineligible = %v, want none", got)
	}
}

func assertRecoveredEligibleMoleculeLifecycle(
	t *testing.T,
	store beads.Store,
	base beads.Store,
	rootID string,
	rec *events.Fake,
) {
	t.Helper()
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("eligible lifecycle recovery retry = true, want complete")
	}
	after, err := base.Get(rootID)
	if err != nil {
		t.Fatalf("Get recovered root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("recovered root status = %q, want closed", after.Status)
	}
	if after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
		after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
		t.Fatalf("recovered lifecycle metadata = %#v, want intent cleared", after.Metadata)
	}
	assertSingleOrderedMoleculeLifecycle(t, rec.Events)

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("idempotent lifecycle recovery retry = true, want complete")
	}
	assertSingleOrderedMoleculeLifecycle(t, rec.Events)
}

func assertSingleOrderedMoleculeLifecycle(t *testing.T, recorded []events.Event) {
	t.Helper()
	got := moleculeLifecycleTypes(recorded)
	if len(got) != 2 || got[0] != events.BeadClosed || got[1] != events.MoleculeResolved {
		t.Fatalf("lifecycle events = %v, want one ordered bead.closed/molecule.resolved pair", got)
	}
}

type moleculeEligibilityAtomicSpyStore struct {
	beads.Store
	rootID      string
	atomicCalls int
	onPending   func() error
	pendingRan  bool
}

func (s *moleculeEligibilityAtomicSpyStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *moleculeEligibilityAtomicSpyStore) CloseWithReasonIfOpen(id, reason string) (beads.CloseTransition, error) {
	s.atomicCalls++
	closer, ok := beads.CloseTransitionerFor(s.Store)
	if !ok {
		return beads.CloseTransition{}, beads.ErrCloseTransitionUnsupported
	}
	return closer.CloseWithReasonIfOpen(id, reason)
}

func (s *moleculeEligibilityAtomicSpyStore) SetMetadata(id, key, value string) error {
	if err := s.Store.SetMetadata(id, key, value); err != nil {
		return err
	}
	if id == s.rootID && key == beadmeta.MoleculeLifecyclePendingMetadataKey &&
		value == moleculeLifecycleVersionV1 && !s.pendingRan {
		s.pendingRan = true
		if s.onPending != nil {
			return s.onPending()
		}
	}
	return nil
}

type sourceTerminalReadGateStore struct {
	beads.Store
	sourceID     string
	terminalRead chan struct{}
	release      chan struct{}
	mu           sync.Mutex
	reads        int
	once         sync.Once
}

func (s *sourceTerminalReadGateStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *sourceTerminalReadGateStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err == nil {
		s.gateTerminalRead(id, bead)
	}
	return bead, err
}

func (s *sourceTerminalReadGateStore) WithLifecycleMetadataTransaction(
	id string,
	fn func(beads.LifecycleMetadataTransaction) error,
) error {
	return beads.WithLifecycleMetadataTransaction(s.Store, id, func(tx beads.LifecycleMetadataTransaction) error {
		reader, ok := tx.(beads.LifecycleReadTransaction)
		if !ok {
			return beads.ErrLifecycleReadUnsupported
		}
		return fn(sourceTerminalReadGateTransaction{LifecycleReadTransaction: reader, gate: s})
	})
}

func (s *sourceTerminalReadGateStore) gateTerminalRead(id string, bead beads.Bead) {
	if id != s.sourceID || bead.Status != "closed" {
		return
	}
	s.mu.Lock()
	s.reads++
	reads := s.reads
	s.mu.Unlock()
	if reads < 2 {
		return
	}
	s.once.Do(func() {
		close(s.terminalRead)
		<-s.release
	})
}

type sourceTerminalReadGateTransaction struct {
	beads.LifecycleReadTransaction
	gate *sourceTerminalReadGateStore
}

func (tx sourceTerminalReadGateTransaction) GetByID(id string) (beads.Bead, error) {
	bead, err := tx.LifecycleReadTransaction.GetByID(id)
	if err == nil {
		tx.gate.gateTerminalRead(id, bead)
	}
	return bead, err
}
