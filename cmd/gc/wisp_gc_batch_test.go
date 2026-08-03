package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// batchGCStore is a gcTestStore that also advertises beads.BatchDeleter and
// counts DepRemove, so a test can assert the wisp GC deletes a closure with one
// batched delete call instead of an O(subprocess-per-edge) teardown.
type batchGCStore struct {
	*gcTestStore
	batchCalls [][]string
	depRemoves int
}

//nolint:unparam // error return satisfies beads.BatchDeleter; the test spy never fails.
func (s *batchGCStore) DeleteBatch(ids []string) error {
	s.batchCalls = append(s.batchCalls, append([]string(nil), ids...))
	for _, id := range ids {
		_ = s.Delete(id)
	}
	return nil
}

var _ beads.BatchDeleter = (*batchGCStore)(nil)

func (s *batchGCStore) DepRemove(issueID, dependsOnID string) error {
	s.depRemoves++
	return s.gcTestStore.DepRemove(issueID, dependsOnID)
}

func TestWispGCClosureUsesBatchedDelete(t *testing.T) {
	now := time.Now()
	base := newGCStore([]beads.Bead{
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
	if err := base.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := base.DepAdd("mol-1.2", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1.1): %v", err)
	}
	store := &batchGCStore{gcTestStore: base}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 root purge accounting", purged)
	}

	// The whole closure is torn down with a single batched delete call, and no
	// per-edge DepRemove is issued — ON DELETE CASCADE removes the edges.
	if len(store.batchCalls) != 1 {
		t.Fatalf("batch calls = %v, want exactly one batched call", store.batchCalls)
	}
	if got := len(store.batchCalls[0]); got != 3 {
		t.Fatalf("batched delete removed %d ids, want 3 (mol-1, mol-1.1, mol-1.2)", got)
	}
	if store.depRemoves != 0 {
		t.Fatalf("DepRemove called %d times; want 0 (batched delete handles edges)", store.depRemoves)
	}
	assertDeletedIDs(t, base.deletedIDs, "mol-1", "mol-1.1", "mol-1.2")
}

// The production controller rewraps the store in beadPolicyStore, whose embedded
// beads.Store does not promote optional capabilities. Without the explicit
// DeleteBatch forward, the wisp-GC delete path would type-assert the wrapper,
// miss BatchDeleter, and silently fall back to per-bead deletion. This pins that
// the batched path stays reachable through the policy wrapper.
func TestDeleteWorkflowBeadsBatchReachesBatchDeleterThroughPolicyWrapper(t *testing.T) {
	now := time.Now()
	base := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now, "closed", "molecule"),
		makeGCBead("mol-1.1", now, "closed", "task"),
	})
	batchStore := &batchGCStore{gcTestStore: base}

	wrapped := wrapStoreWithBeadPolicies(batchStore, nil)
	if _, ok := wrapped.(beads.BatchDeleter); !ok {
		t.Fatalf("policy-wrapped store does not expose beads.BatchDeleter")
	}

	if err := deleteWorkflowBeadsBatch(wrapped, []string{"mol-1", "mol-1.1"}); err != nil {
		t.Fatalf("deleteWorkflowBeadsBatch through policy wrapper: %v", err)
	}

	if len(batchStore.batchCalls) != 1 || len(batchStore.batchCalls[0]) != 2 {
		t.Fatalf("batch calls = %v, want one batched call of 2 ids through the wrapper", batchStore.batchCalls)
	}
	if batchStore.depRemoves != 0 {
		t.Fatalf("DepRemove called %d times; want 0 (batched path, not per-bead fallback)", batchStore.depRemoves)
	}
	assertDeletedIDs(t, base.deletedIDs, "mol-1", "mol-1.1")
}

// When the policy-wrapped backing store does not implement BatchDeleter, the
// wrapper's DeleteBatch reports ErrBatchDeleteUnsupported and the caller falls
// through to per-bead deletion — the beads are still removed.
func TestDeleteWorkflowBeadsBatchFallsBackThroughPolicyWrapperWithoutBatchDeleter(t *testing.T) {
	now := time.Now()
	base := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now, "closed", "molecule"),
		makeGCBead("mol-1.1", now, "closed", "task"),
	})

	// base (plain gcTestStore over MemStore) does not implement beads.BatchDeleter.
	wrapped := wrapStoreWithBeadPolicies(base, nil)

	if err := deleteWorkflowBeadsBatch(wrapped, []string{"mol-1", "mol-1.1"}); err != nil {
		t.Fatalf("deleteWorkflowBeadsBatch fallback through policy wrapper: %v", err)
	}
	assertDeletedIDs(t, base.deletedIDs, "mol-1", "mol-1.1")
}
