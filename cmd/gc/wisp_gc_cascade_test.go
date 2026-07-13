package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// cascadeGCStore is a gcTestStore that also advertises beads.CascadeDeleter and
// counts DepRemove, so a test can assert the wisp GC deletes a closure with one
// batched cascade call instead of an O(subprocess-per-edge) teardown.
type cascadeGCStore struct {
	*gcTestStore
	cascadeCalls [][]string
	depRemoves   int
}

//nolint:unparam // error return satisfies beads.CascadeDeleter; the test spy never fails.
func (s *cascadeGCStore) DeleteCascade(ids []string) error {
	s.cascadeCalls = append(s.cascadeCalls, append([]string(nil), ids...))
	for _, id := range ids {
		_ = s.Delete(id)
	}
	return nil
}

var _ beads.CascadeDeleter = (*cascadeGCStore)(nil)

func (s *cascadeGCStore) DepRemove(issueID, dependsOnID string) error {
	s.depRemoves++
	return s.gcTestStore.DepRemove(issueID, dependsOnID)
}

func TestWispGCClosureUsesBatchedCascadeDelete(t *testing.T) {
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
	store := &cascadeGCStore{gcTestStore: base}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 root purge accounting", purged)
	}

	// The whole closure is torn down with a single batched cascade call, and no
	// per-edge DepRemove is issued — ON DELETE CASCADE removes the edges.
	if len(store.cascadeCalls) != 1 {
		t.Fatalf("cascade calls = %v, want exactly one batched call", store.cascadeCalls)
	}
	if got := len(store.cascadeCalls[0]); got != 3 {
		t.Fatalf("batched cascade deleted %d ids, want 3 (mol-1, mol-1.1, mol-1.2)", got)
	}
	if store.depRemoves != 0 {
		t.Fatalf("DepRemove called %d times; want 0 (batched cascade handles edges)", store.depRemoves)
	}
	assertDeletedIDs(t, base.deletedIDs, "mol-1", "mol-1.1", "mol-1.2")
}
