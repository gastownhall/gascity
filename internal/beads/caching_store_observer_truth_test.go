package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type laggedGetAfterUpdateStore struct {
	Store
	stale           Bead
	lagNextGet      bool
	concurrentTitle string
}

type laggedGetAfterReleaseStore struct {
	Store
	stale      Bead
	lagNextGet bool
}

type postCommitDepListFailOnceStore struct {
	Store
	failNextDepList bool
	depListFailures int
	scope           string
}

type postCommitConditionalRefreshFailStore struct {
	*casBackingStore
}

type batchDepStrippingStore struct {
	depStrippingStore
}

func (s *batchDepStrippingStore) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *postCommitConditionalRefreshFailStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	err := s.casBackingStore.UpdateIfMatch(id, expectedRevision, opts)
	if err == nil {
		s.failNextGet = true
	}
	return err
}

func (s *postCommitConditionalRefreshFailStore) CloseIfMatch(id string, expectedRevision int64) error {
	err := s.casBackingStore.CloseIfMatch(id, expectedRevision)
	if err == nil {
		s.failNextGet = true
	}
	return err
}

func (s *postCommitConditionalRefreshFailStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	swapped, err := s.casBackingStore.CompareAndSetMetadataKey(id, key, expected, next)
	if err == nil && swapped {
		s.failNextGet = true
	}
	return swapped, err
}

func (s *postCommitDepListFailOnceStore) armAfterCommit(err error) error {
	if err == nil {
		s.failNextDepList = true
	}
	return err
}

func (s *postCommitDepListFailOnceStore) CacheMutationScope() string {
	return s.scope
}

func (s *postCommitDepListFailOnceStore) Update(id string, opts UpdateOpts) error {
	return s.armAfterCommit(s.Store.Update(id, opts))
}

func (s *postCommitDepListFailOnceStore) Reopen(id string) error {
	return s.armAfterCommit(s.Store.Reopen(id))
}

func (s *postCommitDepListFailOnceStore) Close(id string) error {
	return s.armAfterCommit(s.Store.Close(id))
}

func (s *postCommitDepListFailOnceStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	count, err := s.Store.CloseAll(ids, metadata)
	return count, s.armAfterCommit(err)
}

func (s *postCommitDepListFailOnceStore) Tx(commitMsg string, fn func(Tx) error) error {
	return s.armAfterCommit(s.Store.Tx(commitMsg, fn))
}

func (s *postCommitDepListFailOnceStore) DeleteBatch(ids []string) error {
	deleter, ok := s.Store.(BatchDeleter)
	if !ok {
		return errors.New("batch delete unsupported")
	}
	return deleter.DeleteBatch(ids)
}

func (s *postCommitDepListFailOnceStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	applier, ok := GraphApplyFor(s.Store)
	if !ok {
		return nil, errors.New("graph apply unsupported")
	}
	result, err := applier.ApplyGraphPlan(ctx, plan)
	return result, s.armAfterCommit(err)
}

func (s *postCommitDepListFailOnceStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if released {
		err = s.armAfterCommit(err)
	}
	return released, err
}

func (s *postCommitDepListFailOnceStore) SetMetadata(id, key, value string) error {
	return s.armAfterCommit(s.Store.SetMetadata(id, key, value))
}

func (s *postCommitDepListFailOnceStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.armAfterCommit(s.Store.SetMetadataBatch(id, kvs))
}

func (s *postCommitDepListFailOnceStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.armAfterCommit(s.Store.DepAdd(issueID, dependsOnID, depType))
}

func (s *postCommitDepListFailOnceStore) DepRemove(issueID, dependsOnID string) error {
	return s.armAfterCommit(s.Store.DepRemove(issueID, dependsOnID))
}

func (s *postCommitDepListFailOnceStore) DepList(id, direction string) ([]Dep, error) {
	if s.failNextDepList {
		s.failNextDepList = false
		s.depListFailures++
		return nil, errors.New("injected post-commit dependency read failure")
	}
	return s.Store.DepList(id, direction)
}

// revisionlessExecUpdateStore models the production exec-backed Store wire:
// successful writes are visible through a complete follow-up read, but the
// backing cannot carry Bead.Revision and every authoritative row reports zero.
type revisionlessExecUpdateStore struct {
	Store
	staleAfterUpdate *Bead
	lagNextGet       bool
	concurrentTitle  string
}

func (s *revisionlessExecUpdateStore) Update(id string, opts UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	if s.concurrentTitle != "" {
		title := s.concurrentTitle
		return s.Store.Update(id, UpdateOpts{Title: &title})
	}
	s.lagNextGet = s.staleAfterUpdate != nil
	return nil
}

func (s *revisionlessExecUpdateStore) Get(id string) (Bead, error) {
	if s.lagNextGet && s.staleAfterUpdate != nil && s.staleAfterUpdate.ID == id {
		s.lagNextGet = false
		bead := cloneBead(*s.staleAfterUpdate)
		bead.Revision = 0
		return bead, nil
	}
	bead, err := s.Store.Get(id)
	bead.Revision = 0
	return bead, err
}

func (s *revisionlessExecUpdateStore) List(query ListQuery) ([]Bead, error) {
	items, err := s.Store.List(query)
	for i := range items {
		items[i].Revision = 0
	}
	return items, err
}

func (s *laggedGetAfterReleaseStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if released && err == nil {
		s.lagNextGet = true
	}
	return released, err
}

func (s *laggedGetAfterReleaseStore) Get(id string) (Bead, error) {
	if s.lagNextGet && id == s.stale.ID {
		s.lagNextGet = false
		return cloneBead(s.stale), nil
	}
	return s.Store.Get(id)
}

func (s *laggedGetAfterUpdateStore) Update(id string, opts UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	if s.concurrentTitle != "" {
		title := s.concurrentTitle
		return s.Store.Update(id, UpdateOpts{Title: &title})
	}
	s.lagNextGet = true
	return nil
}

func (s *laggedGetAfterUpdateStore) Get(id string) (Bead, error) {
	if s.lagNextGet && id == s.stale.ID {
		s.lagNextGet = false
		return cloneBead(s.stale), nil
	}
	return s.Store.Get(id)
}

func TestCachingStoreLegacyClosePublishesAuthoritativeClosedObservation(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "legacy close observation"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := closeAllCapabilityStrippedStore{Store: base}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A successful legacy Close is a committed close: publish the close edge so
	// type-driven consumers (eventexport, bead.closed order gates) observe it.
	if len(notes) != 1 || notes[0].eventType != "bead.closed" {
		t.Fatalf("notifications = %+v, want one authoritative bead.closed", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" {
		t.Fatalf("closed payload = %#v ok=%v, want exact closed row", payload, ok)
	}
}

func TestMemStoreCloseTransitionDoesNotResurrectRemovedDependency(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "removed blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	target, err := base.Create(Bead{Title: "target", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := base.DepRemove(target.ID, blocker.ID); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}

	transition, err := base.CloseWithReasonIfOpen(target.ID, "done")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if len(transition.Before.Dependencies) != 0 || len(transition.After.Dependencies) != 0 {
		t.Fatalf("removed dependency resurrected: before=%#v after=%#v", transition.Before.Dependencies, transition.After.Dependencies)
	}
}

func TestCachingStoreConditionalRefreshSuppressesLaggedReplacementEvents(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CachingStore, Bead) error
		stale  func(Bead) Bead
	}{
		{
			name: "UpdateIfMatch",
			mutate: func(cache *CachingStore, before Bead) error {
				title := "durable update"
				return cache.UpdateIfMatch(before.ID, before.Revision, UpdateOpts{Title: &title})
			},
			stale: cloneBead,
		},
		{
			name: "CloseIfMatchOlderRevision",
			mutate: func(cache *CachingStore, before Bead) error {
				return cache.CloseIfMatch(before.ID, before.Revision)
			},
			stale: func(before Bead) Bead {
				before.Revision--
				return before
			},
		},
		{
			name: "CompareAndSetMetadataKey",
			mutate: func(cache *CachingStore, before Bead) error {
				ok, err := cache.CompareAndSetMetadataKey(before.ID, "phase", "", "durable")
				if err == nil && !ok {
					t.Fatalf("CompareAndSetMetadataKey swapped = false, want true")
				}
				return err
			},
			stale: cloneBead,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backing := &casBackingStore{Store: NewMemStore()}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			created, err := cache.Create(Bead{Title: "conditional lag target"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			before, err := cache.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			notes = nil
			stale := tt.stale(cloneBead(before))
			backing.staleNextGet = &stale

			if err := tt.mutate(cache, before); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			if len(notes) != 0 {
				t.Fatalf("notifications = %+v, want no lagged replacement event", notes)
			}
		})
	}
}

func TestCachingStoreCloseIfMatchNoOpRecloseEmitsNothing(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "already closed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get closed bead: %v", err)
	}

	backing := &casBackingStore{Store: base}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.CloseIfMatch(closed.ID, closed.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none for no-op reclose", notes)
	}
}

func TestCachingStoreUpdateSuppressesLaggedPostWriteObservation(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &laggedGetAfterUpdateStore{Store: base, stale: cloneBead(created)}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "durable title"
	if err := cache.Update(created.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want no stale replacement event", notes)
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[created.ID]
	cache.mu.RUnlock()
	if !dirty {
		t.Fatal("lagged post-write observation left the cache row clean")
	}
}

func TestCachingStoreFailedPostCommitRefreshEventuallyPublishesCompleteUpdate(t *testing.T) {
	tests := []struct {
		name               string
		prepareBeforePrime func(*MemStore, Bead, Bead) error
		prepareAfterPrime  func(*CachingStore, Bead, Bead) error
		mutate             func(*CachingStore, Bead, Bead) error
		getAfterMutation   bool
		// wantEventType is the eventual recovered edge; empty means bead.updated.
		// A committed close on a capability-less backing recovers as bead.closed so
		// type-driven consumers (eventexport, bead.closed order gates) see the edge.
		wantEventType string
	}{
		{
			name: "Update survives authoritative Get",
			mutate: func(cache *CachingStore, target, _ Bead) error {
				title := "after update"
				return cache.Update(target.ID, UpdateOpts{Title: &title})
			},
			getAfterMutation: true,
		},
		{
			name: "Reopen",
			prepareBeforePrime: func(base *MemStore, target, _ Bead) error {
				return base.Close(target.ID)
			},
			mutate: func(cache *CachingStore, target, _ Bead) error {
				return cache.Reopen(target.ID)
			},
			getAfterMutation: true,
		},
		{
			name: "ReleaseIfCurrent",
			prepareBeforePrime: func(base *MemStore, target, _ Bead) error {
				status := "in_progress"
				assignee := "pending-publication-worker"
				return base.Update(target.ID, UpdateOpts{Status: &status, Assignee: &assignee})
			},
			mutate: func(cache *CachingStore, target, _ Bead) error {
				released, err := cache.ReleaseIfCurrent(target.ID, "pending-publication-worker")
				if err == nil && !released {
					return errors.New("ReleaseIfCurrent unexpectedly reported no release")
				}
				return err
			},
			getAfterMutation: true,
		},
		{
			name: "legacy Close",
			mutate: func(cache *CachingStore, target, _ Bead) error {
				return cache.Close(target.ID)
			},
			wantEventType: "bead.closed",
		},
		{
			name: "capability-less status close",
			prepareBeforePrime: func(base *MemStore, target, blocker Bead) error {
				return base.DepAdd(target.ID, blocker.ID, "blocks")
			},
			mutate: func(cache *CachingStore, target, _ Bead) error {
				status := "closed"
				return cache.Update(target.ID, UpdateOpts{Status: &status})
			},
			getAfterMutation: true,
			wantEventType:    "bead.closed",
		},
		{
			name: "legacy CloseAll",
			mutate: func(cache *CachingStore, target, _ Bead) error {
				count, _ := cache.CloseAll([]string{target.ID}, map[string]string{"reason": "pending publication"})
				if count != 1 {
					return errors.New("CloseAll unexpectedly reported no close")
				}
				return nil
			},
			getAfterMutation: true,
			wantEventType:    "bead.closed",
		},
		{
			name: "transaction update",
			mutate: func(cache *CachingStore, target, _ Bead) error {
				return cache.Tx("pending publication update", func(tx Tx) error {
					title := "after transaction update"
					return tx.Update(target.ID, UpdateOpts{Title: &title})
				})
			},
			getAfterMutation: true,
		},
		{
			name: "SetMetadata",
			mutate: func(cache *CachingStore, target, _ Bead) error {
				return cache.SetMetadata(target.ID, "phase", "single")
			},
		},
		{
			name: "SetMetadataBatch",
			mutate: func(cache *CachingStore, target, _ Bead) error {
				return cache.SetMetadataBatch(target.ID, map[string]string{
					"phase": "batch",
					"owner": "pending-publication-test",
				})
			},
		},
		{
			name: "DepAdd",
			mutate: func(cache *CachingStore, target, blocker Bead) error {
				return cache.DepAdd(target.ID, blocker.ID, "blocks")
			},
		},
		{
			name: "DepRemove",
			prepareBeforePrime: func(base *MemStore, target, blocker Bead) error {
				return base.DepAdd(target.ID, blocker.ID, "blocks")
			},
			mutate: func(cache *CachingStore, target, blocker Bead) error {
				return cache.DepRemove(target.ID, blocker.ID)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := NewMemStore()
			priority := 2
			target, err := base.Create(Bead{
				Title:       "before",
				Priority:    &priority,
				Description: "preserved description",
				Labels:      []string{"preserved-label"},
				Metadata:    StringMap{"preserved": "metadata"},
				NoHistory:   true,
			})
			if err != nil {
				t.Fatalf("Create target: %v", err)
			}
			blocker, err := base.Create(Bead{Title: "blocker"})
			if err != nil {
				t.Fatalf("Create blocker: %v", err)
			}
			if tt.prepareBeforePrime != nil {
				if err := tt.prepareBeforePrime(base, target, blocker); err != nil {
					t.Fatalf("prepare before Prime: %v", err)
				}
			}

			backing := &postCommitDepListFailOnceStore{Store: depStrippingStore{MemStore: base}}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if tt.prepareAfterPrime != nil {
				if err := tt.prepareAfterPrime(cache, target, blocker); err != nil {
					t.Fatalf("prepare after Prime: %v", err)
				}
			}
			notes = nil

			if err := tt.mutate(cache, target, blocker); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			if backing.depListFailures != 1 {
				t.Fatalf("post-commit DepList failures = %d, want 1", backing.depListFailures)
			}
			if tt.getAfterMutation {
				got, err := cache.Get(target.ID)
				if err != nil {
					t.Fatalf("authoritative Get after failed refresh: %v", err)
				}
				if tt.name == "Update survives authoritative Get" && got.Title != "after update" {
					t.Fatalf("authoritative Get title = %q, want %q", got.Title, "after update")
				}
			}

			expireCacheMutationRecencyForTest(cache, target.ID)
			cache.runReconciliation()
			cache.runReconciliation()
			wantEventType := tt.wantEventType
			if wantEventType == "" {
				wantEventType = "bead.updated"
			}
			assertSingleCompleteNotification(t, notes, base, target.ID, wantEventType)
		})
	}
}

func TestCachingStorePendingPublicationSurvivesReplacementHandle(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{
		Title:       "before replacement",
		Description: "preserved across replacement",
		Metadata:    StringMap{"preserved": "metadata"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	scope := "/pending-publication-test/" + t.Name()
	oldBacking := &postCommitDepListFailOnceStore{
		Store: depStrippingStore{MemStore: base},
		scope: scope,
	}
	var oldNotes []cacheWriteNotification
	oldCache := NewCachingStoreForTest(oldBacking, func(eventType, beadID string, payload json.RawMessage) {
		oldNotes = append(oldNotes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := oldCache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime old cache: %v", err)
	}

	title := "after replacement"
	if err := oldCache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update old cache: %v", err)
	}
	if oldBacking.depListFailures != 1 {
		t.Fatalf("old-cache post-commit DepList failures = %d, want 1", oldBacking.depListFailures)
	}
	if len(oldNotes) != 0 {
		t.Fatalf("old-cache notifications = %+v, want none without a complete snapshot", oldNotes)
	}

	replacementBacking := &postCommitDepListFailOnceStore{
		Store: depStrippingStore{MemStore: base},
		scope: scope,
	}
	var replacementNotes []cacheWriteNotification
	replacement := NewCachingStoreForTest(replacementBacking, func(eventType, beadID string, payload json.RawMessage) {
		replacementNotes = append(replacementNotes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if oldCache.mutationScopeMu != replacement.mutationScopeMu {
		t.Fatal("replacement cache does not share the old cache's durable mutation scope")
	}
	if err := replacement.Prime(context.Background()); err != nil {
		t.Fatalf("Prime replacement cache: %v", err)
	}
	got, err := replacement.Get(target.ID)
	if err != nil {
		t.Fatalf("Get replacement cache: %v", err)
	}
	if got.Title != title {
		t.Fatalf("replacement was not primed at durable end-state: title = %q, want %q", got.Title, title)
	}

	replacement.runReconciliation()
	replacement.runReconciliation()
	assertSingleCompleteUpdatedNotification(t, replacementNotes, base, target.ID)
	if len(oldNotes) != 0 {
		t.Fatalf("old-cache notifications after replacement convergence = %+v, want replacement observer ownership", oldNotes)
	}
}

func expireCacheMutationRecencyForTest(cache *CachingStore, id string) {
	cache.mu.Lock()
	cache.localBeadAt[id] = time.Now().Add(-6 * time.Second)
	cache.mu.Unlock()
}

func assertSingleCompleteUpdatedNotification(t *testing.T, notes []cacheWriteNotification, base *MemStore, id string) {
	t.Helper()
	assertSingleCompleteNotification(t, notes, base, id, "bead.updated")
}

func assertSingleCompleteNotification(t *testing.T, notes []cacheWriteNotification, base *MemStore, id, eventType string) {
	t.Helper()
	if len(notes) != 1 || notes[0].eventType != eventType || notes[0].beadID != id {
		t.Fatalf("notifications = %+v, want exactly one eventual %s for %s", notes, eventType, id)
	}
	got, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok {
		t.Fatalf("DecodeBeadEventPayload(%s) failed", notes[0].payload)
	}
	want, err := base.Get(id)
	if err != nil {
		t.Fatalf("Get durable row: %v", err)
	}
	deps, err := base.DepList(id, "down")
	if err != nil {
		t.Fatalf("DepList durable row: %v", err)
	}
	want.Dependencies = cloneDeps(deps)
	want.Needs = nil
	if beadChanged(want, got, false) || !want.UpdatedAt.Equal(got.UpdatedAt) || want.NoHistory != got.NoHistory {
		t.Fatalf("%s payload = %+v, want complete durable snapshot %+v", eventType, got, want)
	}
}

func TestCachingStoreFailedCreatedRefreshEventuallyPublishesCompleteCreation(t *testing.T) {
	t.Run("transaction create", func(t *testing.T) {
		base := NewMemStore()
		blocker, err := base.Create(Bead{Title: "transaction blocker"})
		if err != nil {
			t.Fatalf("Create blocker: %v", err)
		}
		backing := &postCommitDepListFailOnceStore{Store: depStrippingStore{MemStore: base}}
		var notes []cacheWriteNotification
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}

		var created Bead
		if err := cache.Tx("pending transaction create", func(tx Tx) error {
			var createErr error
			created, createErr = tx.Create(Bead{Title: "transaction-created", Needs: []string{blocker.ID}})
			return createErr
		}); err != nil {
			t.Fatalf("Tx: %v", err)
		}
		if backing.depListFailures != 1 {
			t.Fatalf("post-commit DepList failures = %d, want 1", backing.depListFailures)
		}
		if _, err := cache.Get(created.ID); err != nil {
			t.Fatalf("authoritative Get after failed refresh: %v", err)
		}
		expireCacheMutationRecencyForTest(cache, created.ID)
		cache.runReconciliation()
		cache.runReconciliation()
		assertSingleCompleteNotification(t, notes, base, created.ID, "bead.created")
	})

	t.Run("graph apply", func(t *testing.T) {
		base := NewMemStore()
		graphBacking := &storageGraphApplyRecordingStore{Store: depStrippingStore{MemStore: base}}
		backing := &postCommitDepListFailOnceStore{Store: graphBacking}
		var notes []cacheWriteNotification
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		applier, ok := GraphApplyFor(cache)
		if !ok {
			t.Fatal("GraphApplyFor(cache) = false")
		}
		result, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
			Nodes: []GraphApplyNode{{Key: "root", Title: "graph-created"}},
		})
		if err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		id := result.IDs["root"]
		if backing.depListFailures != 1 {
			t.Fatalf("post-commit DepList failures = %d, want 1", backing.depListFailures)
		}
		if _, err := cache.Get(id); err != nil {
			t.Fatalf("authoritative Get after failed refresh: %v", err)
		}
		expireCacheMutationRecencyForTest(cache, id)
		cache.runReconciliation()
		cache.runReconciliation()
		assertSingleCompleteNotification(t, notes, base, id, "bead.created")
	})
}

func TestCachingStoreConditionalFailedRefreshEventuallyPublishesCompleteState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CachingStore, Bead) error
	}{
		{
			name: "UpdateIfMatch",
			mutate: func(cache *CachingStore, before Bead) error {
				title := "conditional update"
				return cache.UpdateIfMatch(before.ID, before.Revision, UpdateOpts{Title: &title})
			},
		},
		{
			name: "CompareAndSetMetadataKey",
			mutate: func(cache *CachingStore, before Bead) error {
				swapped, err := cache.CompareAndSetMetadataKey(before.ID, "phase", "", "conditional")
				if err == nil && !swapped {
					return errors.New("CompareAndSetMetadataKey unexpectedly lost")
				}
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := NewMemStore()
			blocker, err := base.Create(Bead{Title: "conditional blocker"})
			if err != nil {
				t.Fatalf("Create blocker: %v", err)
			}
			target, err := base.Create(Bead{Title: "before conditional"})
			if err != nil {
				t.Fatalf("Create target: %v", err)
			}
			if err := base.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
				t.Fatalf("DepAdd: %v", err)
			}
			backing := &postCommitConditionalRefreshFailStore{casBackingStore: &casBackingStore{
				Store: depStrippingStore{MemStore: base},
			}}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			before, err := cache.Get(target.ID)
			if err != nil {
				t.Fatalf("Get before mutation: %v", err)
			}

			if err := tt.mutate(cache, before); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			if len(notes) != 0 {
				t.Fatalf("notifications before recovery = %+v, want none", notes)
			}
			if _, err := cache.Get(target.ID); err != nil {
				t.Fatalf("authoritative Get after failed refresh: %v", err)
			}
			expireCacheMutationRecencyForTest(cache, target.ID)
			cache.runReconciliation()
			cache.runReconciliation()
			assertSingleCompleteUpdatedNotification(t, notes, base, target.ID)
		})
	}
}

func TestCachingStoreCloseIfMatchFailedRefreshEventuallyPublishesCompleteClose(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "conditional close blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	target, err := base.Create(Bead{Title: "conditional close target"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := base.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	backing := &postCommitConditionalRefreshFailStore{casBackingStore: &casBackingStore{
		Store: depStrippingStore{MemStore: base},
	}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before, err := cache.Get(target.ID)
	if err != nil {
		t.Fatalf("Get before close: %v", err)
	}

	if err := cache.CloseIfMatch(target.ID, before.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications before recovery = %+v, want none", notes)
	}
	if _, err := cache.Get(target.ID); err != nil {
		t.Fatalf("authoritative Get after failed refresh: %v", err)
	}
	expireCacheMutationRecencyForTest(cache, target.ID)
	cache.runReconciliation()
	cache.runReconciliation()
	assertSingleCompleteNotification(t, notes, base, target.ID, "bead.closed")
}

func TestCachingStoreCloseIfMatchFailedRefreshNoOpRecloseEmitsNothing(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "already closed conditional target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(target.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := base.Get(target.ID)
	if err != nil {
		t.Fatalf("Get closed target: %v", err)
	}
	backing := &postCommitConditionalRefreshFailStore{casBackingStore: &casBackingStore{Store: base}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.CloseIfMatch(target.ID, closed.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	cache.runReconciliation()
	cache.runReconciliation()
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none for a proven no-op reclose", notes)
	}
}

func TestCachingStorePendingConditionalCloseCoalescesWithImmediateReopen(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "close then reopen"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &postCommitConditionalRefreshFailStore{casBackingStore: &casBackingStore{Store: base}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before, err := cache.Get(target.ID)
	if err != nil {
		t.Fatalf("Get before close: %v", err)
	}

	if err := cache.CloseIfMatch(target.ID, before.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	if err := cache.Reopen(target.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	// The pending close edge is preserved for accounting even though the reopen
	// followed immediately: eventexport DROPS bead.updated, so a coalesced-away
	// close would lose the close edge entirely across a close→reopen sequence.
	// The lifecycle EDGE (bead.closed) therefore survives while both payloads
	// always carry the latest complete durable snapshot — here the reopened
	// (status open) row.
	if len(notes) != 2 || notes[0].eventType != "bead.closed" || notes[1].eventType != "bead.updated" {
		t.Fatalf("notifications = %+v, want ordered [bead.closed bead.updated]", notes)
	}
	updated, ok := DecodeBeadEventPayload(notes[1].payload)
	if !ok || updated.ID != target.ID || updated.Status == "closed" {
		t.Fatalf("updated payload = %+v ok=%v, want complete reopened durable row", updated, ok)
	}
	closed, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || closed.ID != target.ID {
		t.Fatalf("closed payload = %+v ok=%v, want the preserved close edge", closed, ok)
	}
}

func TestCachingStorePendingUpdatePrecedesLaterReconciledClose(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "update then close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &postCommitDepListFailOnceStore{Store: depStrippingStore{MemStore: base}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	title := "updated before close"
	if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := base.Close(target.ID); err != nil {
		t.Fatalf("out-of-band Close: %v", err)
	}

	expireCacheMutationRecencyForTest(cache, target.ID)
	cache.runReconciliation()
	cache.runReconciliation()
	if len(notes) != 2 || notes[0].eventType != "bead.updated" || notes[1].eventType != "bead.closed" {
		t.Fatalf("notifications = %+v, want ordered [bead.updated bead.closed]", notes)
	}
	for i, note := range notes {
		payload, ok := DecodeBeadEventPayload(note.payload)
		if !ok || payload.ID != target.ID || payload.Title != title || payload.Status != "closed" {
			t.Fatalf("notification %d payload = %+v ok=%v, want complete final closed row", i, payload, ok)
		}
	}
}

func TestCachingStorePendingUpdatePrecedesDeleteWithCompletePayload(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CachingStore, string) error
	}{
		{
			name: "Delete",
			mutate: func(cache *CachingStore, id string) error {
				return cache.Delete(id)
			},
		},
		{
			name: "DeleteBatch",
			mutate: func(cache *CachingStore, id string) error {
				return cache.DeleteBatch([]string{id})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := NewMemStore()
			blocker, err := base.Create(Bead{Title: "delete blocker"})
			if err != nil {
				t.Fatalf("Create blocker: %v", err)
			}
			target, err := base.Create(Bead{Title: "before delete"})
			if err != nil {
				t.Fatalf("Create target: %v", err)
			}
			if err := base.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
				t.Fatalf("DepAdd: %v", err)
			}
			var store Store = depStrippingStore{MemStore: base}
			if tt.name == "DeleteBatch" {
				store = &batchDepStrippingStore{depStrippingStore: depStrippingStore{MemStore: base}}
			}
			backing := &postCommitDepListFailOnceStore{Store: store}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			title := "updated before delete"
			if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("Update: %v", err)
			}

			if err := tt.mutate(cache, target.ID); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if len(notes) != 2 || notes[0].eventType != "bead.updated" || notes[1].eventType != "bead.deleted" {
				t.Fatalf("notifications = %+v, want ordered [bead.updated bead.deleted]", notes)
			}
			updated, ok := DecodeBeadEventPayload(notes[0].payload)
			if !ok || updated.Title != title || len(updated.Dependencies) != 1 || updated.Dependencies[0].DependsOnID != blocker.ID {
				t.Fatalf("pending update payload = %+v ok=%v, want complete dependency-bearing snapshot", updated, ok)
			}
		})
	}
}

func TestCachingStoreImmediatePendingCloseRecoveryEvictsActiveFallback(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "recent transaction close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &postCommitDepListFailOnceStore{Store: depStrippingStore{MemStore: base}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Tx("close with failed refresh", func(tx Tx) error {
		return tx.Close(target.ID)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	// Do not expire local mutation recency: the ordinary merge must fence its
	// stale active row, while the validated targeted closed recovery still
	// removes that fallback from the active cache.
	cache.runReconciliation()
	cache.mu.RLock()
	_, cached := cache.beads[target.ID]
	cache.mu.RUnlock()
	if cached {
		t.Fatal("authoritative closed recovery left a stale active row cached")
	}
	// The tx close committed; its failed refresh retained a close-shaped intent,
	// so recovery must deliver the close edge (not an update that consumers drop).
	if len(notes) != 1 || notes[0].eventType != "bead.closed" {
		t.Fatalf("notifications = %+v, want one recovered close", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" {
		t.Fatalf("recovered payload = %+v ok=%v, want closed durable row", payload, ok)
	}
}

func TestCachingStoreUpdatePublishesExecRevisionlessAuthoritativeObservation(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "authoritative blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	created, err := base.Create(Bead{
		Title:       "before",
		Description: "preserved authoritative field",
		Metadata:    StringMap{"existing": "kept"},
	})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := base.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	backing := &revisionlessExecUpdateStore{Store: base}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "after"
	if err := cache.Update(created.ID, UpdateOpts{
		Title:    &title,
		Metadata: StringMap{"phase": "updated"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want exactly one ordered bead.updated for %s", notes, created.ID)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok {
		t.Fatalf("DecodeBeadEventPayload(%s) failed", notes[0].payload)
	}
	if payload.Title != title || payload.Description != "preserved authoritative field" ||
		payload.Metadata["existing"] != "kept" || payload.Metadata["phase"] != "updated" {
		t.Fatalf("bead.updated payload = %+v, want complete authoritative post-update row", payload)
	}
	if len(payload.Dependencies) != 1 || payload.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("bead.updated dependencies = %#v, want authoritative blocker %s", payload.Dependencies, blocker.ID)
	}
	if payload.Status == "closed" {
		t.Fatalf("ordinary update payload inferred a close: %+v", payload)
	}
}

func TestCachingStoreUpdateSuppressesExecRevisionlessLaggedObservation(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale := cloneBead(created)
	backing := &revisionlessExecUpdateStore{Store: base, staleAfterUpdate: &stale}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "durable title"
	if err := cache.Update(created.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want no event for revisionless read that does not reflect opts", notes)
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[created.ID]
	cache.mu.RUnlock()
	if !dirty {
		t.Fatal("revisionless lagged post-write observation left the cache row clean")
	}
}

func TestCachingStoreUpdatePublishesExecRevisionlessVerbatimLaterObservationWithDependencies(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "revisionless blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	created, err := base.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := base.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	backing := &revisionlessExecUpdateStore{
		Store:           base,
		concurrentTitle: "concurrent durable title",
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	requestedTitle := "requested title"
	if err := cache.Update(created.ID, UpdateOpts{Title: &requestedTitle}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" {
		t.Fatalf("notifications = %+v, want one exact bead.updated", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok {
		t.Fatalf("DecodeBeadEventPayload(%s) failed", notes[0].payload)
	}
	if payload.Title != backing.concurrentTitle {
		t.Fatalf("payload title = %q, want later durable title %q", payload.Title, backing.concurrentTitle)
	}
	if len(payload.Dependencies) != 1 || payload.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("payload dependencies = %#v, want blocker %s", payload.Dependencies, blocker.ID)
	}
}

func TestCachingStoreUpdatePublishesVerbatimLaterObservationWithDependencies(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	created, err := base.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := base.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	backing := &laggedGetAfterUpdateStore{
		Store:           base,
		concurrentTitle: "concurrent durable title",
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	requestedTitle := "requested title"
	if err := cache.Update(created.ID, UpdateOpts{Title: &requestedTitle}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" {
		t.Fatalf("notifications = %+v, want one exact bead.updated", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok {
		t.Fatalf("DecodeBeadEventPayload(%s) failed", notes[0].payload)
	}
	if payload.Title != backing.concurrentTitle {
		t.Fatalf("payload title = %q, want later durable title %q", payload.Title, backing.concurrentTitle)
	}
	if len(payload.Dependencies) != 1 || payload.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("payload dependencies = %#v, want blocker %s", payload.Dependencies, blocker.ID)
	}
}

func TestCachingStoreReleaseIfCurrentRefreshFailureEmitsNothing(t *testing.T) {
	status := "in_progress"
	backing := &releaseRefreshFailOnceStore{Store: NewMemStore()}
	created, err := backing.Create(Bead{Title: "release target", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Update(created.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	released, err := cache.ReleaseIfCurrent(created.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none after failed refresh", notes)
	}
}

func TestCachingStoreReleaseIfCurrentSuppressesLaggedObservation(t *testing.T) {
	status := "in_progress"
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "lagged release", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Update(created.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}
	before, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before release: %v", err)
	}
	backing := &laggedGetAfterReleaseStore{Store: base, stale: before}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	released, err := cache.ReleaseIfCurrent(created.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want no lagged replacement event", notes)
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[created.ID]
	cache.mu.RUnlock()
	if !dirty {
		t.Fatal("lagged release observation left the cache row clean")
	}
}

func TestCachingStoreTxClosePublishesUpdatedWithoutTransitionOwnership(t *testing.T) {
	backing := NewMemStore()
	created, err := backing.Create(Bead{Title: "tx close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.Tx("ordinary tx close", func(tx Tx) error {
		return tx.Close(created.ID)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" {
		t.Fatalf("notifications = %+v, want one conservative bead.updated", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" {
		t.Fatalf("updated payload = %#v ok=%v, want exact closed row", payload, ok)
	}
}

// pendingRecoveryFaultStore drives the failed-refresh → reconcile-recovery
// paths. A successful Update arms a one-shot post-commit DepList failure (which
// retains a pending intent), while independent toggles let a test fail the
// reconcile full scan (failList) or corrupt the recovery Get's returned ID once
// (corruptNextGet).
type pendingRecoveryFaultStore struct {
	*MemStore
	failNextDepList bool
	failList        bool
	corruptNextGet  bool
}

func (s *pendingRecoveryFaultStore) Update(id string, opts UpdateOpts) error {
	err := s.MemStore.Update(id, opts)
	if err == nil {
		s.failNextDepList = true
	}
	return err
}

func (s *pendingRecoveryFaultStore) DepList(id, direction string) ([]Dep, error) {
	if s.failNextDepList {
		s.failNextDepList = false
		return nil, errors.New("injected post-commit dependency read failure")
	}
	return s.MemStore.DepList(id, direction)
}

func (s *pendingRecoveryFaultStore) List(query ListQuery) ([]Bead, error) {
	if s.failList {
		return nil, errors.New("injected full scan failure")
	}
	return s.MemStore.List(query)
}

func (s *pendingRecoveryFaultStore) Get(id string) (Bead, error) {
	b, err := s.MemStore.Get(id)
	if err != nil {
		return b, err
	}
	if s.corruptNextGet {
		s.corruptNextGet = false
		b.ID += "-corrupt"
	}
	return b, nil
}

// closeAllSingleAncillaryErrorStore closes the requested beads durably but
// returns an ancillary error and forces the post-close refresh to fail once, so
// the legacy CloseAll path must decide whether a single proven-committed ID
// retains its publication intent.
type closeAllSingleAncillaryErrorStore struct {
	*MemStore
	closeErr        error
	failNextDepList bool
}

func (s *closeAllSingleAncillaryErrorStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := s.MemStore.CloseAll(ids, metadata)
	if err != nil {
		return n, err
	}
	s.failNextDepList = true
	return n, s.closeErr
}

func (s *closeAllSingleAncillaryErrorStore) DepList(id, direction string) ([]Dep, error) {
	if s.failNextDepList {
		s.failNextDepList = false
		return nil, errors.New("injected post-close-all dependency read failure")
	}
	return s.MemStore.DepList(id, direction)
}

// B1: a dependency-stripping backing returns List rows without dependency
// fields, so an out-of-band update discovered by reconciliation must still carry
// the complete deps (sourced from the separate depMap) in its bead.updated
// observer payload.
func TestCachingStoreReconcileUpdatePayloadCarriesCompleteDependencies(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "reconcile dep blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	target, err := base.Create(Bead{Title: "reconcile dep target", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	backing := depStrippingStore{MemStore: base}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "reconcile-updated title"
	if err := base.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("out-of-band Update: %v", err)
	}
	cache.runReconciliation()

	var updated *cacheWriteNotification
	for i := range notes {
		if notes[i].eventType == "bead.updated" && notes[i].beadID == target.ID {
			updated = &notes[i]
		}
	}
	if updated == nil {
		t.Fatalf("notifications = %+v, want a bead.updated for %s", notes, target.ID)
	}
	payload, ok := DecodeBeadEventPayload(updated.payload)
	if !ok {
		t.Fatal("DecodeBeadEventPayload failed")
	}
	if payload.Title != title {
		t.Fatalf("payload title = %q, want %q", payload.Title, title)
	}
	if len(payload.Dependencies) != 1 || payload.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("reconcile bead.updated payload dependencies = %+v, want complete [%s]", payload.Dependencies, blocker.ID)
	}
}

// B2: a failed-refresh Update retains a pending intent; even when the reconcile
// full scan itself fails, a targeted Get/DepList recovery must still publish the
// complete bead.updated rather than stranding the intent behind the List outage.
func TestCachingStorePendingRecoveryPublishesDespiteListFailure(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "list-failure blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	target, err := base.Create(Bead{Title: "list-failure target", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	backing := &pendingRecoveryFaultStore{MemStore: base}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "updated before list outage"
	if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications before recovery = %+v, want none", notes)
	}
	expireCacheMutationRecencyForTest(cache, target.ID)

	backing.failList = true
	cache.runReconciliation()
	assertSingleCompleteUpdatedNotification(t, notes, base, target.ID)
}

// B3: a busy cache keeps advancing lastFreshAt/LastReconcileAt on every write; a
// full-scan deadline anchored on that clock alone would postpone recovery of an
// outstanding pending intent forever. The pending deadline anchors on the intent
// itself so the reconcile delay is bounded (0 once past the intent's deadline).
func TestCachingStoreReconcileDelayNotStarvedByBusyWrites(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "pending starvation target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(base, func(string, string, json.RawMessage) {})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	now := time.Now()
	cache.mu.Lock()
	cache.noteLocalMutationLocked(target.ID)
	cache.mu.Unlock()
	cache.retainPendingPublicationAt(target.ID, "bead.updated", now.Add(-2*cacheReconcileIntervalSmall), true)
	if !cache.pendingPublications.hasAny() {
		t.Fatal("pending intent was not retained")
	}

	for i := 0; i < 5; i++ {
		cache.mu.Lock()
		cache.lastFreshAt = now
		cache.stats.LastReconcileAt = now
		cache.mu.Unlock()
		if got := cache.nextReconcileDelay(now); got != 0 {
			t.Fatalf("iteration %d: nextReconcileDelay = %v, want 0 (recovery must not be starved by busy writes)", i, got)
		}
	}

	control := NewCachingStoreForTest(NewMemStore(), func(string, string, json.RawMessage) {})
	if err := control.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(control): %v", err)
	}
	control.mu.Lock()
	control.lastFreshAt = now
	control.stats.LastReconcileAt = now
	control.mu.Unlock()
	if got := control.nextReconcileDelay(now); got == 0 {
		t.Fatal("control cache without a pending intent should still be postponed by busy writes")
	}
}

// B7: a single-ID legacy CloseAll that reports n == 1 is proven committed even
// when the backing returns an ancillary error, so a failed post-write refresh
// must still retain the intent and reconciliation must publish the complete row.
func TestCachingStoreSingleIDCloseAllPartialCommitRetainsPending(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "single-id close-all target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := closeAllCapabilityStrippedStore{Store: &closeAllSingleAncillaryErrorStore{
		MemStore: base,
		closeErr: errors.New("ancillary close-all error"),
	}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	n, err := cache.CloseAll([]string{target.ID}, nil)
	if n != 1 {
		t.Fatalf("CloseAll count = %d, want 1", n)
	}
	if err == nil {
		t.Fatal("CloseAll error = nil, want the injected ancillary error")
	}
	if len(notes) != 0 {
		t.Fatalf("notifications before recovery = %+v, want none", notes)
	}

	cache.runReconciliation()
	cache.runReconciliation()
	// A single-ID CloseAll reporting n==1 is a proven committed close even with an
	// ancillary error, so its failed refresh recovers as a close edge.
	assertSingleCompleteNotification(t, notes, base, target.ID, "bead.closed")
}

// C1: a failed-refresh Tx create (pending [created]) followed by a failed-refresh
// conditional close (pending [created, closed]) must recover BOTH lifecycle
// edges, because eventexport drops bead.updated and needs both created and
// closed to keep its accounting.
func TestCachingStorePendingCreatedThenClosedRecoveryEmitsBothEdges(t *testing.T) {
	base := NewMemStore()
	backing := &postCommitConditionalRefreshFailStore{casBackingStore: &casBackingStore{Store: base}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Fail the post-commit refresh of the Tx create so it retains pending [created].
	backing.failNextGet = true
	var created Bead
	if err := cache.Tx("pending create", func(tx Tx) error {
		var createErr error
		created, createErr = tx.Create(Bead{Title: "created then closed"})
		return createErr
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	current, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after failed create refresh: %v", err)
	}
	// A conditional close whose post-close refresh also fails joins bead.closed
	// onto the still-unpublished creation.
	if err := cache.CloseIfMatch(created.ID, current.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}

	pub, ok := cache.pendingPublications.current(created.ID)
	if !ok {
		t.Fatal("pending intent was not retained across create+close")
	}
	if !pub.containsEvent("bead.created") || !pub.containsEvent("bead.closed") {
		t.Fatalf("pending sequence = %+v, want both created and closed edges", pub.events())
	}

	if _, err := cache.Get(created.ID); err != nil {
		t.Fatalf("Get after failed close refresh: %v", err)
	}
	expireCacheMutationRecencyForTest(cache, created.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	if len(notes) != 2 || notes[0].eventType != "bead.created" || notes[1].eventType != "bead.closed" {
		t.Fatalf("notifications = %+v, want ordered [bead.created bead.closed]", notes)
	}
	for i, note := range notes {
		payload, ok := DecodeBeadEventPayload(note.payload)
		if !ok || payload.ID != created.ID || payload.Status != "closed" {
			t.Fatalf("notification %d payload = %+v ok=%v, want the final closed row", i, payload, ok)
		}
	}
}

// C2: a failed-refresh conditional close (pending [closed]) followed by a
// failed-refresh SetMetadata (pending [closed, updated]) must recover the close
// edge before the update, keeping the close exportable.
func TestCachingStorePendingCloseThenFailedMetadataEmitsCloseThenUpdate(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "close then metadata"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &postCommitConditionalRefreshFailStore{casBackingStore: &casBackingStore{Store: base}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before, err := cache.Get(target.ID)
	if err != nil {
		t.Fatalf("Get before close: %v", err)
	}

	if err := cache.CloseIfMatch(target.ID, before.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	// Fail the metadata post-write refresh so it joins bead.updated after the close.
	backing.failNextGet = true
	if err := cache.SetMetadata(target.ID, "note", "recorded"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	pub, ok := cache.pendingPublications.current(target.ID)
	if !ok {
		t.Fatal("pending intent was not retained across close+metadata")
	}
	events := pub.events()
	if len(events) != 2 || events[0].eventType != "bead.closed" || events[1].eventType != "bead.updated" {
		t.Fatalf("pending sequence = %+v, want ordered [bead.closed bead.updated]", events)
	}

	if _, err := cache.Get(target.ID); err != nil {
		t.Fatalf("Get after failed metadata refresh: %v", err)
	}
	expireCacheMutationRecencyForTest(cache, target.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	if len(notes) != 2 || notes[0].eventType != "bead.closed" || notes[1].eventType != "bead.updated" {
		t.Fatalf("notifications = %+v, want ordered [bead.closed bead.updated]", notes)
	}
	for i, note := range notes {
		payload, ok := DecodeBeadEventPayload(note.payload)
		if !ok || payload.ID != target.ID || payload.Status != "closed" || payload.Metadata["note"] != "recorded" {
			t.Fatalf("notification %d payload = %+v ok=%v, want the final closed row with metadata", i, payload, ok)
		}
	}
}

// C3: an outstanding pending intent must survive any operation that advances the
// shared mutation generation without producing its own replacement snapshot,
// because reconciliation would otherwise reject the resequenced intent as stale.
func TestCachingStorePendingRecoverySurvivesGenerationRollForward(t *testing.T) {
	t.Run("ApplyEvent", func(t *testing.T) {
		base := NewMemStore()
		target, err := base.Create(Bead{Title: "roll-forward apply-event"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		backing := &postCommitDepListFailOnceStore{Store: base}
		var notes []cacheWriteNotification
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		title := "phase-1"
		if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		// An out-of-band change the backing already reflects, applied as an event,
		// advances the shared generation and resequences the pending intent.
		newTitle := "phase-2"
		if err := base.Update(target.ID, UpdateOpts{Title: &newTitle}); err != nil {
			t.Fatalf("out-of-band Update: %v", err)
		}
		durable, err := base.Get(target.ID)
		if err != nil {
			t.Fatalf("Get durable: %v", err)
		}
		payload, mErr := json.Marshal(durable)
		if mErr != nil {
			t.Fatalf("marshal event: %v", mErr)
		}
		cache.ApplyEvent("bead.updated", payload)

		expireCacheMutationRecencyForTest(cache, target.ID)
		cache.runReconciliation()
		cache.runReconciliation()
		assertSingleCompleteUpdatedNotification(t, notes, base, target.ID)
	})

	t.Run("ApplyDepEvent", func(t *testing.T) {
		base := NewMemStore()
		target, err := base.Create(Bead{Title: "roll-forward dep-event"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		backing := &postCommitDepListFailOnceStore{Store: base}
		var notes []cacheWriteNotification
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		title := "phase-1"
		if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		cache.ApplyDepEvent(target.ID, nil)

		expireCacheMutationRecencyForTest(cache, target.ID)
		cache.runReconciliation()
		cache.runReconciliation()
		assertSingleCompleteUpdatedNotification(t, notes, base, target.ID)
	})

	t.Run("LosingConditionalWrite", func(t *testing.T) {
		base := NewMemStore()
		target, err := base.Create(Bead{Title: "roll-forward cas-loser"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		backing := &casBackingStore{Store: base}
		var notes []cacheWriteNotification
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		title := "phase-1"
		backing.failNextGet = true
		if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		// A losing metadata CAS (wrong expected) advances the shared generation via
		// the conditional-write evict without emitting a replacement snapshot.
		swapped, err := cache.CompareAndSetMetadataKey(target.ID, "phase", "wrong-expected", "next")
		if err != nil {
			t.Fatalf("CompareAndSetMetadataKey: %v", err)
		}
		if swapped {
			t.Fatal("CompareAndSetMetadataKey swapped = true, want a clean loss")
		}

		expireCacheMutationRecencyForTest(cache, target.ID)
		cache.runReconciliation()
		cache.runReconciliation()
		assertSingleCompleteUpdatedNotification(t, notes, base, target.ID)
	})
}

// C4: a corrupt recovery read (a bead with the wrong ID) must publish nothing
// and absorb nothing for that intent; a later correct read publishes exactly
// once. The intent is never dropped by the corrupt read.
func TestCachingStorePendingRecoveryRejectsWrongIDThenPublishesOnce(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "wrong-id recovery target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &pendingRecoveryFaultStore{MemStore: base}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	title := "wrong-id updated"
	if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	expireCacheMutationRecencyForTest(cache, target.ID)
	backing.corruptNextGet = true
	cache.runReconciliation()
	if len(notes) != 0 {
		t.Fatalf("first reconcile emitted %+v, want nothing for the corrupt read", notes)
	}
	if _, ok := cache.pendingPublications.current(target.ID); !ok {
		t.Fatal("pending intent was dropped by the corrupt read, must survive for a later correct read")
	}

	cache.runReconciliation()
	assertSingleCompleteUpdatedNotification(t, notes, base, target.ID)
}

// C5: a recovered publication's observer callback must receive the ORIGINAL
// occurrence time of the durable mutation, not the (later) reconciliation
// delivery time, so delayed recovery does not inflate run-step duration.
func TestCachingStoreRecoveredPublicationCarriesOriginalOccurrenceTime(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "occurredAt target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &postCommitDepListFailOnceStore{Store: base}
	type capture struct {
		eventType  string
		occurredAt time.Time
	}
	var captures []capture
	cache := NewCachingStoreWithEventTimestamp(backing, func(eventType, _, _, _, _ string, _ json.RawMessage, occurredAt time.Time) {
		captures = append(captures, capture{eventType: eventType, occurredAt: occurredAt})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	title := "occurredAt update"
	if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	pub, ok := cache.pendingPublications.current(target.ID)
	if !ok {
		t.Fatal("failed-refresh update did not retain a pending intent")
	}
	events := pub.events()
	if len(events) != 1 || events[0].occurredAt.IsZero() {
		t.Fatalf("pending events = %+v, want one with a retained occurrence time", events)
	}
	retainedAt := events[0].occurredAt

	expireCacheMutationRecencyForTest(cache, target.ID)
	beforeReconcile := time.Now()
	cache.runReconciliation()
	cache.runReconciliation()

	var updated *capture
	for i := range captures {
		if captures[i].eventType == "bead.updated" {
			updated = &captures[i]
		}
	}
	if updated == nil {
		t.Fatalf("captures = %+v, want a recovered bead.updated", captures)
	}
	if !updated.occurredAt.Equal(retainedAt) {
		t.Fatalf("recovered occurredAt = %s, want original occurrence %s", updated.occurredAt, retainedAt)
	}
	if !updated.occurredAt.Before(beforeReconcile) {
		t.Fatalf("recovered occurredAt = %s not before reconcile delivery %s (delivery time leaked)", updated.occurredAt, beforeReconcile)
	}
}

// C6: the gate reserved for a failed-refresh publication must not be overtaken
// by a later suppressed-close barrier enqueued behind it. The pending
// publication is delivered first; the barrier's receipt only fires afterward.
func TestCachingStorePendingGateBlocksSuppressedCloseBarrier(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "gate barrier target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &postCommitDepListFailOnceStore{Store: base}
	var order []string
	cache := NewCachingStoreForTest(backing, func(eventType, _ string, _ json.RawMessage) {
		order = append(order, "observer:"+eventType)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "gated update"
	if err := cache.Update(target.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, ok := cache.pendingPublications.current(target.ID); !ok {
		t.Fatal("failed-refresh update did not retain a pending intent")
	}

	// A suppressed close enqueues an ordered barrier BEHIND the pending gate.
	delivery, err := cache.CloseWithoutObserverWithDelivery(target.ID)
	if err != nil {
		t.Fatalf("CloseWithoutObserverWithDelivery: %v", err)
	}
	delivery.AfterDelivery(func() { order = append(order, "barrier") })

	expireCacheMutationRecencyForTest(cache, target.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	if len(order) != 2 || order[0] != "observer:bead.updated" || order[1] != "barrier" {
		t.Fatalf("delivery order = %v, want [observer:bead.updated barrier]", order)
	}
}

// G1: an out-of-band delete of a bead with a pending intent must clear the
// intent and release its gate placeholder, otherwise the gate blocks that ID's
// ordered queue forever. A later notification for the same ID must not be
// blocked.
func TestCachingStoreOutOfBandDeleteClearsAbandonedPendingGate(t *testing.T) {
	base := NewMemStore()
	target, err := base.Create(Bead{Title: "out-of-band delete target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &postCommitConditionalRefreshFailStore{casBackingStore: &casBackingStore{Store: base}}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before, err := cache.Get(target.ID)
	if err != nil {
		t.Fatalf("Get before update: %v", err)
	}

	// A failed-refresh conditional update evicts the row and retains pending
	// [updated] with a reserved gate placeholder.
	title := "abandoned update"
	if err := cache.UpdateIfMatch(target.ID, before.Revision, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch: %v", err)
	}
	if _, ok := cache.pendingPublications.current(target.ID); !ok {
		t.Fatal("failed-refresh conditional update did not retain a pending intent")
	}

	if err := base.Delete(target.ID); err != nil {
		t.Fatalf("out-of-band Delete: %v", err)
	}

	expireCacheMutationRecencyForTest(cache, target.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	if len(notes) != 0 {
		t.Fatalf("reconcile emitted %+v for an out-of-band-deleted pending bead, want nothing", notes)
	}
	if _, ok := cache.pendingPublications.current(target.ID); ok {
		t.Fatal("pending intent for an out-of-band-deleted bead was not cleared")
	}

	// A later notification for the same id must not be blocked by a leaked gate.
	drain, laterDelivery := cache.enqueueOrderedChange("bead.created", Bead{ID: target.ID, Title: "recreated"})
	if drain != nil {
		drain()
	}
	if !laterDelivery.isDelivered() {
		t.Fatal("a later notification for the id was blocked by a leaked pending gate placeholder")
	}
}

// G2: if a resolved gate's clearIfToken fails (a surviving intent advanced
// concurrently), the surviving intent still references the consumed gate number.
// Re-reserving a placeholder restores its resolvability instead of dropping the
// publication with "gate missing". Driven at the queue-helper level because the
// end-to-end interleaving is unreachable under the mutation-scope locking.
func TestReleaseAndReReserveOrderedPendingGateKeepsSurvivingIntentResolvable(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), func(string, string, json.RawMessage) {})
	const id = "gc-1"
	const gate = uint64(7)

	cache.reserveOrderedPendingGate(id, gate)
	notif, ok := cache.prepareObserverNotification("bead.updated", Bead{ID: id, Title: "first"})
	if !ok {
		t.Fatal("prepareObserverNotification failed")
	}
	drain, _, resolved := cache.resolveOrderedPendingGate(id, gate, []cacheObserverNotification{notif})
	if !resolved {
		t.Fatal("first resolve did not find the reserved gate")
	}
	if drain != nil {
		drain()
	}

	// The gate number is now consumed: a second resolve misses, proving a
	// surviving intent that still references it would be dropped ("gate missing").
	if _, _, reResolved := cache.resolveOrderedPendingGate(id, gate, []cacheObserverNotification{notif}); reResolved {
		t.Fatal("consumed gate resolved a second time; expected a miss")
	}

	// The G2 defense re-reserves a placeholder for the surviving intent.
	cache.reserveOrderedPendingGate(id, gate)
	notif2, ok := cache.prepareObserverNotification("bead.updated", Bead{ID: id, Title: "surviving"})
	if !ok {
		t.Fatal("prepareObserverNotification failed")
	}
	drain2, deliveries, resolved2 := cache.resolveOrderedPendingGate(id, gate, []cacheObserverNotification{notif2})
	if !resolved2 {
		t.Fatal("re-reserved gate was not resolvable; surviving intent would be dropped")
	}
	if drain2 != nil {
		drain2()
	}
	if len(deliveries) != 1 || !deliveries[0].isDelivered() {
		t.Fatalf("re-reserved gate resolution did not deliver: %+v", deliveries)
	}
}
