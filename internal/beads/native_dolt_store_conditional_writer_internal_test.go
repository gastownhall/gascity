package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

func TestNativeDoltStoreConditionalUpdateRelatedFieldsThroughCache(t *testing.T) {
	backing := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	cache := NewCachingStore(backing, nil)
	parent, err := cache.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := cache.Create(Bead{Title: "child", Labels: []string{"remove", "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err = cache.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}

	writer, ok := ConditionalWriterFor(cache)
	if !ok {
		t.Fatal("cache over NativeDoltStore did not expose ConditionalWriter")
	}
	title := "updated"
	parentID := parent.ID
	err = writer.UpdateIfMatch(child.ID, child.Revision, UpdateOpts{
		Title:        &title,
		ParentID:     &parentID,
		Labels:       []string{"added"},
		RemoveLabels: []string{"remove"},
	})
	if err != nil {
		t.Fatalf("UpdateIfMatch through cache: %v", err)
	}
	after, err := cache.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision == child.Revision || after.Title != title || after.ParentID != parent.ID ||
		!slices.Contains(after.Labels, "added") || !slices.Contains(after.Labels, "keep") || slices.Contains(after.Labels, "remove") {
		t.Fatalf("conditional related after-image = %+v", after)
	}

	staleTitle := "stale"
	if err := writer.UpdateIfMatch(child.ID, child.Revision, UpdateOpts{Title: &staleTitle, Labels: []string{"stale"}}); !IsPreconditionFailed(err) {
		t.Fatalf("stale composite update = %v, want PreconditionFailedError", err)
	}
}

func TestCachingStoreRelatedFieldRefusalIsCacheNoop(t *testing.T) {
	backing := NewMemStore()
	cache := NewCachingStore(backing, nil)
	created, err := cache.Create(Bead{Title: "cached", Labels: []string{"keep"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(created.ID); err != nil {
		t.Fatal(err)
	}

	err = cache.UpdateIfMatch(created.ID, created.Revision, UpdateOpts{Labels: []string{"unsupported"}})
	var unsupported *ConditionalUpdateFieldUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("UpdateIfMatch = %v, want ConditionalUpdateFieldUnsupportedError", err)
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[created.ID]
	_, cached := cache.beads[created.ID]
	cache.mu.RUnlock()
	if dirty || !cached {
		t.Fatalf("typed field refusal changed cache state: dirty=%v cached=%v", dirty, cached)
	}
}

func TestNativeDoltStoreConditionalRevisionTokenDomain(t *testing.T) {
	t.Run("zero rejects every revision-fenced verb", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		created, err := store.Create(Bead{Title: "zero-token"})
		if err != nil {
			t.Fatal(err)
		}
		title := "must-not-apply"
		checks := []struct {
			name string
			run  func() error
		}{
			{name: "update", run: func() error { return store.UpdateIfMatch(created.ID, 0, UpdateOpts{Title: &title}) }},
			{name: "close", run: func() error { return store.CloseIfMatch(created.ID, 0) }},
			{name: "delete", run: func() error { return store.DeleteIfMatch(created.ID, 0) }},
			{name: "atomic close", run: func() error {
				_, err := store.CloseWithMetadataIfMatch(created.ID, 0, map[string]string{"k": "v"})
				return err
			}},
		}
		for _, check := range checks {
			t.Run(check.name, func(t *testing.T) {
				var pfe *PreconditionFailedError
				if err := check.run(); !errors.As(err, &pfe) || pfe.Expected != 0 {
					t.Fatalf("error = %#v, want zero-token PreconditionFailedError", err)
				}
			})
		}
		fresh, err := store.Get(created.ID)
		if err != nil || fresh.Title != "zero-token" || fresh.Status != "open" || fresh.Metadata["k"] != "" {
			t.Fatalf("zero-token calls mutated bead: %+v, err=%v", fresh, err)
		}
	})

	t.Run("negative is an opaque usable token", func(t *testing.T) {
		const id = "gc-negative"
		newStorage := func() (*nativeDoltStorageSpy, *bool) {
			invoked := false
			closed := false
			storage := &nativeDoltStorageSpy{}
			storage.getIssue = func(context.Context, string) (*beadslib.Issue, error) {
				status := beadslib.StatusOpen
				if closed {
					status = beadslib.StatusClosed
				}
				return &beadslib.Issue{ID: id, Title: "negative", Status: status, IssueType: beadslib.TypeTask, Priority: 2, RowVersion: -7}, nil
			}
			storage.updateIssueChecked = func(_ context.Context, _ string, _ map[string]interface{}, _ string, opts beadslib.UpdateIssueOptions) error {
				invoked = opts.ExpectedVersion != nil && *opts.ExpectedVersion == -7
				return nil
			}
			storage.updateIssue = func(context.Context, string, map[string]interface{}, string) error {
				invoked = true
				return nil
			}
			storage.closeIssueChecked = func(_ context.Context, _ string, _ string, opts beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
				invoked = opts.ExpectedVersion == nil || *opts.ExpectedVersion == -7
				closed = true
				return beadslib.CloseIssueResult{}, nil
			}
			storage.closeIssue = func(context.Context, string, string, string, string) error {
				invoked = true
				closed = true
				return nil
			}
			storage.deleteIssue = func(context.Context, string) error {
				invoked = true
				return nil
			}
			return storage, &invoked
		}
		title := "updated"
		verbs := []struct {
			name string
			run  func(*NativeDoltStore) error
		}{
			{name: "update", run: func(s *NativeDoltStore) error { return s.UpdateIfMatch(id, -7, UpdateOpts{Title: &title}) }},
			{name: "close", run: func(s *NativeDoltStore) error { return s.CloseIfMatch(id, -7) }},
			{name: "delete", run: func(s *NativeDoltStore) error { return s.DeleteIfMatch(id, -7) }},
			{name: "atomic close", run: func(s *NativeDoltStore) error {
				_, err := s.CloseWithMetadataIfMatch(id, -7, map[string]string{"k": "v"})
				return err
			}},
		}
		for _, verb := range verbs {
			t.Run(verb.name, func(t *testing.T) {
				storage, invoked := newStorage()
				if err := verb.run(newNativeDoltStoreForTest(storage)); err != nil {
					t.Fatalf("negative-token call: %v", err)
				}
				if !*invoked {
					t.Fatal("negative token was rejected before the storage mutation")
				}
			})
		}
	})
}

type nativeDoltPostCommitIndeterminateStorage struct {
	*nativeDoltMemStorage
	enabled       bool
	callbackCalls int
	afterCallback func()
}

type nativeDoltCheckedCloseIndeterminateStorage struct {
	*nativeDoltMemStorage
	calls      int
	afterClose func(string)
}

func (s *nativeDoltCheckedCloseIndeterminateStorage) CloseIssueChecked(
	ctx context.Context,
	id, actor string,
	opts beadslib.CloseIssueOptions,
) (beadslib.CloseIssueResult, error) {
	s.calls++
	result, err := s.nativeDoltMemStorage.CloseIssueChecked(ctx, id, actor, opts)
	if err != nil {
		return result, err
	}
	if s.afterClose != nil {
		s.afterClose(id)
	}
	return result, fmt.Errorf("checked close acknowledgement lost: %w: %w", serializationConflictError(), beadslib.ErrCommitIndeterminate)
}

func TestNativeDoltStoreCloseIfMatchIndeterminateCoordinatePolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		after     func(*nativeDoltCheckedCloseIndeterminateStorage, string)
		wantError bool
	}{
		{name: "exact coordinate confirms self win"},
		{name: "coordinate mismatch preserves ambiguity", wantError: true, after: func(s *nativeDoltCheckedCloseIndeterminateStorage, id string) {
			s.recordCloseProjection(id, "competitor", nativeConditionalCloseCoordinatePrefix+"competitor")
		}},
		{name: "coordinate absence preserves ambiguity", wantError: true, after: func(s *nativeDoltCheckedCloseIndeterminateStorage, id string) {
			s.clearCloseProjection(id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := &nativeDoltCheckedCloseIndeterminateStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
			if tc.after != nil {
				storage.afterClose = func(id string) { tc.after(storage, id) }
			}
			store := newNativeDoltStoreForTest(storage)
			created, err := store.Create(Bead{Title: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			err = store.CloseIfMatch(created.ID, created.Revision)
			if tc.wantError {
				if !errors.Is(err, beadslib.ErrCommitIndeterminate) {
					t.Fatalf("CloseIfMatch = %v, want ErrCommitIndeterminate", err)
				}
			} else if err != nil {
				t.Fatalf("CloseIfMatch = %v, want confirmed success", err)
			}
			if storage.calls != 1 {
				t.Fatalf("CloseIssueChecked calls = %d, want 1", storage.calls)
			}
		})
	}
}

func (s *nativeDoltPostCommitIndeterminateStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	if !s.enabled {
		return s.nativeDoltMemStorage.RunInTransaction(context.Background(), "", fn)
	}
	s.callbackCalls++
	if err := fn(nativeDoltTransactionForTest{storage: s.nativeDoltMemStorage}); err != nil {
		return err
	}
	if s.afterCallback != nil {
		s.afterCallback()
	}
	return fmt.Errorf("post-callback commit acknowledgement: %w", beadslib.ErrCommitIndeterminate)
}

func TestNativeDoltStoreCompositeIndeterminateResultsAreNotSelfConfirmed(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		storage := &nativeDoltPostCommitIndeterminateStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
		store := newNativeDoltStoreForTest(storage)
		created, err := store.Create(Bead{Title: "composite"})
		if err != nil {
			t.Fatal(err)
		}
		storage.enabled = true
		title := "landed"
		err = store.UpdateIfMatch(created.ID, created.Revision, UpdateOpts{Title: &title, Labels: []string{"landed"}})
		if !errors.Is(err, beadslib.ErrCommitIndeterminate) {
			t.Fatalf("UpdateIfMatch = %v, want ErrCommitIndeterminate", err)
		}
		if storage.callbackCalls != 1 {
			t.Fatalf("callback calls = %d, want 1", storage.callbackCalls)
		}
		landed, getErr := store.Get(created.ID)
		if getErr != nil || landed.Title != title || !slices.Contains(landed.Labels, "landed") {
			t.Fatalf("landed after-image = %+v, err=%v", landed, getErr)
		}
	})

	t.Run("delete", func(t *testing.T) {
		storage := &nativeDoltPostCommitIndeterminateStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
		store := newNativeDoltStoreForTest(storage)
		created, err := store.Create(Bead{Title: "delete"})
		if err != nil {
			t.Fatal(err)
		}
		storage.enabled = true
		err = store.DeleteIfMatch(created.ID, created.Revision)
		if !errors.Is(err, beadslib.ErrCommitIndeterminate) {
			t.Fatalf("DeleteIfMatch = %v, want ErrCommitIndeterminate", err)
		}
		if storage.callbackCalls != 1 {
			t.Fatalf("callback calls = %d, want 1", storage.callbackCalls)
		}
	})
}

type nativeDoltRollbackThenCompetitorDeleteStorage struct {
	*nativeDoltMemStorage
	targetID      string
	callbackCalls int
}

func (s *nativeDoltRollbackThenCompetitorDeleteStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	s.store.mu.Lock()
	seq, beads, deps := s.store.snapshot()
	s.store.mu.Unlock()
	closeSnapshot := s.snapshotCloseProjections()
	s.callbackCalls++
	if err := fn(nativeDoltTransactionForTest{storage: s.nativeDoltMemStorage}); err != nil {
		return err
	}
	s.store.restoreFrom(seq, beads, deps)
	s.restoreCloseProjections(closeSnapshot)
	if err := s.store.Delete(s.targetID); err != nil {
		return err
	}
	return fmt.Errorf("our delete rolled back; acknowledgement lost while competitor won: %w", beadslib.ErrCommitIndeterminate)
}

func TestNativeDoltStoreDeleteAbsenceDoesNotProveAuthorship(t *testing.T) {
	storage := &nativeDoltRollbackThenCompetitorDeleteStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "competitor-delete"})
	if err != nil {
		t.Fatal(err)
	}
	storage.targetID = created.ID
	err = store.DeleteIfMatch(created.ID, created.Revision)
	if !errors.Is(err, beadslib.ErrCommitIndeterminate) {
		t.Fatalf("DeleteIfMatch = %v, want ErrCommitIndeterminate despite absence", err)
	}
	if storage.callbackCalls != 1 {
		t.Fatalf("callback calls = %d, want 1", storage.callbackCalls)
	}
	if _, getErr := store.Get(created.ID); !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("competitor did not produce absent post-image: %v", getErr)
	}
}

func TestNativeDoltStoreConditionalDeletePreservesIncarnationlessSidecar(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	created, err := store.Create(Bead{Title: "sidecar"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocalString(created.ID, "lease", "next-incarnation"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteIfMatch(created.ID, created.Revision); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetLocalString(created.ID, "lease"); err != nil || got != "next-incarnation" {
		t.Fatalf("sidecar after conditional delete = %q, err=%v", got, err)
	}
}

func TestNativeDoltStoreAtomicConditionalCloseBlockerAndIndeterminatePolicy(t *testing.T) {
	t.Run("blocked close mutates neither metadata nor status", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		blocker, err := store.Create(Bead{Title: "blocker"})
		if err != nil {
			t.Fatal(err)
		}
		target, err := store.Create(Bead{Title: "blocked", Metadata: map[string]string{"keep": "yes"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		target, err = store.Get(target.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.CloseWithMetadataIfMatch(target.ID, target.Revision, map[string]string{"new": "value"})
		if !errors.Is(err, beadslib.ErrCloseBlocked) {
			t.Fatalf("CloseWithMetadataIfMatch = %v, want ErrCloseBlocked", err)
		}
		fresh, getErr := store.Get(target.ID)
		if getErr != nil || fresh.Status != "open" || fresh.Metadata["keep"] != "yes" || fresh.Metadata["new"] != "" {
			t.Fatalf("blocked close after-image = %+v, err=%v", fresh, getErr)
		}
	})

	t.Run("open child refuses and rolls back metadata", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		parent, err := store.Create(Bead{Title: "parent", Metadata: map[string]string{"keep": "yes"}})
		if err != nil {
			t.Fatal(err)
		}
		child, err := store.Create(Bead{Title: "open child"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(child.ID, parent.ID, "parent-child"); err != nil {
			t.Fatal(err)
		}
		parent, err = store.Get(parent.ID)
		if err != nil {
			t.Fatal(err)
		}

		closed, err := store.CloseWithMetadataIfMatch(parent.ID, parent.Revision, map[string]string{"new": "value"})
		if !errors.Is(err, issueops.ErrCloseOpenChildren) || closed.ID != "" {
			t.Fatalf("CloseWithMetadataIfMatch = (%+v, %v), want zero bead + ErrCloseOpenChildren", closed, err)
		}
		fresh, getErr := store.Get(parent.ID)
		if getErr != nil || fresh.Status != "open" || fresh.Metadata["keep"] != "yes" || fresh.Metadata["new"] != "" {
			t.Fatalf("open-child refusal after-image = %+v, err=%v", fresh, getErr)
		}
	})

	t.Run("exact durable coordinate confirms self win", func(t *testing.T) {
		storage := &nativeDoltPostCommitIndeterminateStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
		store := newNativeDoltStoreForTest(storage)
		created, err := store.Create(Bead{Title: "self-win"})
		if err != nil {
			t.Fatal(err)
		}
		storage.enabled = true
		closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"terminal": "yes"})
		if err != nil || closed.Status != "closed" || closed.Metadata["terminal"] != "yes" {
			t.Fatalf("CloseWithMetadataIfMatch = (%+v, %v), want confirmed close", closed, err)
		}
		if storage.callbackCalls != 1 {
			t.Fatalf("callback calls = %d, want 1", storage.callbackCalls)
		}
	})

	t.Run("coordinate mismatch preserves ambiguity", func(t *testing.T) {
		storage := &nativeDoltPostCommitIndeterminateStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
		store := newNativeDoltStoreForTest(storage)
		created, err := store.Create(Bead{Title: "mismatch"})
		if err != nil {
			t.Fatal(err)
		}
		storage.enabled = true
		storage.afterCallback = func() {
			storage.recordCloseProjection(created.ID, "competitor", nativeConditionalCloseCoordinatePrefix+"competitor")
		}
		closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, nil)
		if !errors.Is(err, beadslib.ErrCommitIndeterminate) || closed.ID != "" {
			t.Fatalf("CloseWithMetadataIfMatch = (%+v, %v), want zero + ErrCommitIndeterminate", closed, err)
		}
		if storage.callbackCalls != 1 {
			t.Fatalf("callback calls = %d, want 1", storage.callbackCalls)
		}
	})
}
