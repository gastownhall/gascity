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
