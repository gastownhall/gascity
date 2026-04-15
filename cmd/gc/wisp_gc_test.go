package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestWispGC_NilSafe(t *testing.T) {
	var wg wispGC
	if wg != nil {
		t.Error("nil wispGC should be nil")
	}
}

func TestWispGC_DisabledReturnsNil(t *testing.T) {
	wg := newWispGC(0, time.Hour)
	if wg != nil {
		t.Error("zero interval should return nil")
	}
	wg = newWispGC(time.Hour, 0)
	if wg != nil {
		t.Error("zero TTL should return nil")
	}
}

func TestWispGC_ShouldRunRespectsInterval(t *testing.T) {
	wg := newWispGC(5*time.Minute, time.Hour)
	now := time.Now()

	if !wg.shouldRun(now) {
		t.Error("should run on first call")
	}

	wg.(*memoryWispGC).lastRun = now

	if wg.shouldRun(now.Add(time.Minute)) {
		t.Error("should not run before interval elapsed")
	}

	if !wg.shouldRun(now.Add(6 * time.Minute)) {
		t.Error("should run after interval elapsed")
	}
}

func TestWispGC_PurgesExpiredMolecules(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBead("mol-2", now.Add(-30*time.Minute), "closed", "molecule"),
		makeGCBead("mol-3", now.Add(-3*time.Hour), "closed", "molecule"),
	})

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged = %d, want 2", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "mol-3")
}

func TestWispGC_NothingExpired(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-10*time.Minute), "closed", "molecule"),
	})

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none", store.deletedIDs)
	}
}

func TestWispGC_EmptyList(t *testing.T) {
	store := newGCStore(nil)
	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, time.Now())
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
}

func TestWispGC_DeleteErrorContinues(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBead("mol-2", now.Add(-2*time.Hour), "closed", "molecule"),
	})
	store.deleteErrors["mol-1"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-2")
}

func TestWispGC_PurgesExpiredTrackingBeads(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBeadWithLabels("track-old", now.Add(-3*time.Hour), "closed", "task", labelOrderTracking),
		makeGCBeadWithLabels("track-new", now.Add(-10*time.Minute), "closed", "task", labelOrderTracking),
		makeGCBeadWithLabels("track-open", now.Add(-5*time.Hour), "open", "task", labelOrderTracking),
	})

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged = %d, want 2", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "track-old")
}

func TestWispGC_TrackingListErrorIsBestEffort(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBeadWithLabels("track-old", now.Add(-3*time.Hour), "closed", "task", labelOrderTracking),
	})
	store.listErrors[gcQueryKey{Status: "closed", Label: labelOrderTracking}] = fmt.Errorf("tracking list failed")

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1")
}

func TestWispGC_ListErrorFailsRun(t *testing.T) {
	store := newGCStore(nil)
	store.listErrors[gcQueryKey{Status: "closed", Type: "molecule"}] = fmt.Errorf("molecule list failed")

	wg := newWispGC(5*time.Minute, time.Hour)
	_, err := wg.runGC(store, time.Now())
	if err == nil {
		t.Fatal("expected list error")
	}
}

type gcQueryKey struct {
	Status string
	Type   string
	Label  string
}

type gcTestStore struct {
	*beads.MemStore
	listErrors   map[gcQueryKey]error
	deleteErrors map[string]error
	deletedIDs   []string
}

func newGCStore(existing []beads.Bead) *gcTestStore {
	return &gcTestStore{
		MemStore:     beads.NewMemStoreFrom(0, existing, nil),
		listErrors:   map[gcQueryKey]error{},
		deleteErrors: map[string]error{},
	}
}

func (s *gcTestStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.listErrors[gcQueryKey{Status: query.Status, Type: query.Type, Label: query.Label}]; err != nil {
		return nil, err
	}
	return s.MemStore.List(query)
}

func (s *gcTestStore) Delete(id string) error {
	if err := s.deleteErrors[id]; err != nil {
		return err
	}
	if err := s.MemStore.Delete(id); err != nil {
		return err
	}
	s.deletedIDs = append(s.deletedIDs, id)
	return nil
}

//nolint:unparam // helper mirrors makeGCBeadWithLabels signature for readability
func makeGCBead(id string, createdAt time.Time, status, beadType string) beads.Bead {
	return makeGCBeadWithLabels(id, createdAt, status, beadType)
}

func makeGCBeadWithLabels(id string, createdAt time.Time, status, beadType string, labels ...string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Status:    status,
		Type:      beadType,
		CreatedAt: createdAt,
		Labels:    labels,
	}
}

func assertDeletedIDs(t *testing.T, deleted []string, want ...string) {
	t.Helper()
	if len(deleted) != len(want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
	seen := map[string]bool{}
	for _, id := range deleted {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("deleted = %v, want %v", deleted, want)
		}
	}
}

var _ beads.Store = (*gcTestStore)(nil)
