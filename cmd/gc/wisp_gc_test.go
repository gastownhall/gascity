package main

import (
	"fmt"
	"sort"
	"strings"
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
		makeGCBeadWithMetadata("wisp-1", now.Add(-2*time.Hour), "closed", "task", map[string]string{"gc.kind": "wisp"}),
		makeGCBead("mol-2", now.Add(-30*time.Minute), "closed", "molecule"),
		makeGCBead("mol-3", now.Add(-3*time.Hour), "closed", "molecule"),
	})

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 3 {
		t.Fatalf("purged = %d, want 3", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "wisp-1", "mol-3")
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

func TestWispGC_DeleteErrorIsSurfacedAndContinues(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBead("mol-2", now.Add(-2*time.Hour), "closed", "molecule"),
	})
	store.deleteErrors["mol-1"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err == nil {
		t.Fatal("expected delete error to be surfaced")
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v, want delete failure to be included", err)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-2")
}

func TestWispGC_PurgesExpiredMoleculeChildrenWithRoot(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		{
			ID:        "mol-1.2",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1.1",
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("mol-1.2", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1.1): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 root purge accounting", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "mol-1.1", "mol-1.2")
	for _, id := range []string{"mol-1", "mol-1.1", "mol-1.2"} {
		if _, err := store.Get(id); err == nil {
			t.Fatalf("Get(%s) succeeded after GC delete", id)
		}
	}
}

func TestWispGC_DoesNotDeleteExternalDependents(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		makeGCBead("external-1", now.Add(-2*time.Hour), "open", "task"),
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("external-1", "mol-1.1", "blocks"); err != nil {
		t.Fatalf("DepAdd(external-1->mol-1.1): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "mol-1.1")
	if _, err := store.Get("external-1"); err != nil {
		t.Fatalf("external dependent was deleted: %v", err)
	}
}

func TestWispGC_PurgesParentChildOwnedDependentsWithoutMetadata(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		{
			ID:        "mol-1.2",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("mol-1.2", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1.1): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "mol-1.1", "mol-1.2")
}

func TestWispGC_LeavesRootWhenChildDeleteFails(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	store.deleteErrors["mol-1.1"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err == nil {
		t.Fatal("expected child delete error")
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v, want delete failure to be included", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0 when child delete fails", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none", store.deletedIDs)
	}
	if _, err := store.Get("mol-1"); err != nil {
		t.Fatalf("root deleted after child failure: %v", err)
	}
	if _, err := store.Get("mol-1.1"); err != nil {
		t.Fatalf("child unexpectedly deleted after failure: %v", err)
	}
}

func TestWispGC_PartialChildDeleteRemainsRetryable(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		{
			ID:        "mol-1.1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1.1",
		},
		{
			ID:        "mol-1.2",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("mol-1.1.1", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1.1->mol-1.1): %v", err)
	}
	if err := store.DepAdd("mol-1.2", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1): %v", err)
	}
	store.deleteErrors["mol-1.2"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err == nil {
		t.Fatal("expected first pass child delete error")
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("first pass err = %v, want delete failure to be included", err)
	}
	if purged != 0 {
		t.Fatalf("first purged = %d, want 0", purged)
	}
	if _, err := store.Get("mol-1"); err != nil {
		t.Fatalf("root deleted after partial child failure: %v", err)
	}
	if _, err := store.Get("mol-1.2"); err != nil {
		t.Fatalf("failing child deleted unexpectedly: %v", err)
	}
	if _, err := store.Get("mol-1.1"); err == nil {
		t.Fatalf("expected an earlier child to be deleted before downstream failure")
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1.1.1", "mol-1.1")

	delete(store.deleteErrors, "mol-1.2")
	purged, err = wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC second pass: %v", err)
	}
	if purged != 1 {
		t.Fatalf("second purged = %d, want 1", purged)
	}
	for _, id := range []string{"mol-1", "mol-1.2"} {
		if _, err := store.Get(id); err == nil {
			t.Fatalf("Get(%s) succeeded after retry cleanup", id)
		}
	}
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

func TestWispGC_TrackingListErrorIsSurfacedAndMoleculePurgeContinues(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBeadWithLabels("track-old", now.Add(-3*time.Hour), "closed", "task", labelOrderTracking),
	})
	store.listErrors[gcQueryKey{Status: "closed", Label: labelOrderTracking}] = fmt.Errorf("tracking list failed")

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err == nil {
		t.Fatal("expected tracking list error to be surfaced")
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if !strings.Contains(err.Error(), "tracking list failed") {
		t.Fatalf("err = %v, want tracking list failure to be included", err)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1")
}

func TestWispGC_TrackingBeadsDoNotDeleteParentChildDescendants(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithLabels("track-old", now.Add(-3*time.Hour), "closed", "task", labelOrderTracking),
		{
			ID:        "track-child",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-3 * time.Hour),
			ParentID:  "track-old",
		},
	})
	if err := store.DepAdd("track-child", "track-old", "parent-child"); err != nil {
		t.Fatalf("DepAdd(track-child->track-old): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "track-old")
	if _, err := store.Get("track-child"); err != nil {
		t.Fatalf("tracking child was deleted: %v", err)
	}
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
	Status   string
	Type     string
	Label    string
	Metadata string
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
	if err := s.listErrors[gcQueryKey{Status: query.Status, Type: query.Type, Label: query.Label, Metadata: metadataQueryKey(query.Metadata)}]; err != nil {
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

func makeGCBeadWithMetadata(id string, createdAt time.Time, status, beadType string, metadata map[string]string) beads.Bead {
	bead := makeGCBead(id, createdAt, status, beadType)
	bead.Metadata = metadata
	return bead
}

func metadataQueryKey(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+metadata[key])
	}
	return strings.Join(parts, "\x00")
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
