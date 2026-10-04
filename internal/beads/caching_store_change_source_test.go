package beads

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// sourceRecorder collects each change notification as "source type id".
type sourceRecorder struct {
	mu  sync.Mutex
	got []string
}

func (r *sourceRecorder) onChange(source ChangeSource, eventType, beadID, _, _, _ string, _ *[]string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, source.String()+" "+eventType+" "+beadID)
}

func (r *sourceRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

// TestCachingStoreNotificationsCarryTheirSource pins that a consumer can tell
// a close this process wrote from one the cache inferred from a read: only the
// inferred kinds may be stale, so only they need re-validating before a durable
// effect (mc-zndi7.43/.55/.56).
func TestCachingStoreNotificationsCarryTheirSource(t *testing.T) {
	t.Parallel()

	rec := &sourceRecorder{}
	backing := NewMemStore()
	cache := NewCachingStore(backing, rec.onChange)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	local, err := cache.Create(Bead{Title: "local"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cache.Close(local.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertEvents(t, rec.take(), "local bead.created "+local.ID, "local bead.closed "+local.ID)

	scanned, err := cache.Create(Bead{Title: "closed out of process, seen by a scan"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	refreshed, err := cache.Create(Bead{Title: "closed out of process, seen by RefreshRow"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec.take()
	if err := backing.Close(scanned.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	if err := backing.Close(refreshed.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}

	if _, err := cache.RefreshRow(refreshed.ID); err != nil {
		t.Fatalf("RefreshRow: %v", err)
	}
	assertEvents(t, rec.take(), "refresh bead.closed "+refreshed.ID)

	gone, err := cache.Create(Bead{Title: "deleted out of process, seen by RefreshRow"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec.take()
	if err := backing.Delete(gone.ID); err != nil {
		t.Fatalf("backing Delete: %v", err)
	}
	if _, err := cache.RefreshRow(gone.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RefreshRow of a deleted row: err = %v, want ErrNotFound", err)
	}
	assertEvents(t, rec.take(), "refresh bead.closed "+gone.ID)

	cache.ReconcileForTest()
	assertEvents(t, rec.take(), "scan bead.closed "+scanned.ID)
}

func TestChangeSourceInferred(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source ChangeSource
		want   bool
	}{
		{ChangeLocal, false},
		{ChangeScan, true},
		{ChangeRefresh, true},
		{ChangeSource(0), true}, // unknown fails toward re-validation
	} {
		if got := tc.source.Inferred(); got != tc.want {
			t.Errorf("%s.Inferred() = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestCachingStoreLocalWritePathsAreTaggedLocal pins the source on the write
// paths that notify in bulk or through a conditional handle: each is a write
// this process committed, so each must be local, or the controller would
// re-read (and, for a gone row, drop) a real completion.
func TestCachingStoreLocalWritePathsAreTaggedLocal(t *testing.T) {
	t.Parallel()

	newCache := func(t *testing.T, backing Store) (*CachingStore, *sourceRecorder) {
		t.Helper()
		rec := &sourceRecorder{}
		cache := NewCachingStore(backing, rec.onChange)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return cache, rec
	}
	create := func(t *testing.T, cache *CachingStore, rec *sourceRecorder) string {
		t.Helper()
		b, err := cache.Create(Bead{Title: "row"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		rec.take()
		return b.ID
	}

	t.Run("CloseAll", func(t *testing.T) {
		t.Parallel()
		cache, rec := newCache(t, NewMemStore())
		id := create(t, cache, rec)
		if _, err := cache.CloseAll([]string{id}, nil); err != nil {
			t.Fatalf("CloseAll: %v", err)
		}
		assertEvents(t, rec.take(), "local bead.closed "+id)
	})
	t.Run("Tx", func(t *testing.T) {
		t.Parallel()
		cache, rec := newCache(t, NewMemStore())
		id := create(t, cache, rec)
		if err := cache.Tx("close", func(tx Tx) error { return tx.Close(id) }); err != nil {
			t.Fatalf("Tx: %v", err)
		}
		assertEvents(t, rec.take(), "local bead.closed "+id)
	})
	t.Run("graph apply", func(t *testing.T) {
		t.Parallel()
		cache, rec := newCache(t, &storageGraphApplyRecordingStore{Store: NewMemStore()})
		applier, ok := cache.GraphApplyHandle()
		if !ok {
			t.Fatal("GraphApplyHandle unavailable")
		}
		result, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "g", Title: "g"}}})
		if err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		assertEvents(t, rec.take(), "local bead.created "+result.IDs["g"])
	})
	t.Run("CloseIfMatch", func(t *testing.T) {
		t.Parallel()
		backing := NewMemStore()
		cache, rec := newCache(t, backing)
		id := create(t, cache, rec)
		row, _ := backing.Get(id)
		if err := cache.CloseIfMatch(id, row.Revision); err != nil {
			t.Fatalf("CloseIfMatch: %v", err)
		}
		assertEvents(t, rec.take(), "local bead.closed "+id)
	})
	t.Run("CloseWithMetadataIfMatch", func(t *testing.T) {
		t.Parallel()
		backing := NewAtomicCloseMemStore()
		cache, rec := newCache(t, backing)
		id := create(t, cache, rec)
		row, _ := backing.Get(id)
		closer, ok := cache.AtomicConditionalCloserHandle()
		if !ok {
			t.Fatal("AtomicConditionalCloserHandle unavailable")
		}
		if _, err := closer.CloseWithMetadataIfMatch(id, row.Revision, map[string]string{"close_reason": "r"}); err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
		assertEvents(t, rec.take(), "local bead.closed "+id)
	})
}

// TestCachingStoreUpdateOfAGoneRowIsARefresh is mc-zndi7.60: Update's write
// succeeded but its refetch found the row deleted. The bead.closed it emits
// is read from the row's absence, not a close this process made, so it is
// tagged refresh and re-validated (a deleted step is not a completed one).
func TestCachingStoreUpdateOfAGoneRowIsARefresh(t *testing.T) {
	t.Parallel()

	rec := &sourceRecorder{}
	backing := &deleteAfterUpdateStore{Store: NewMemStore()}
	b, err := backing.Create(Bead{Title: "step"})
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCachingStore(backing, rec.onChange)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	title := "renamed"
	if err := cache.Update(b.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	assertEvents(t, rec.take(), "refresh bead.closed "+b.ID)
}
