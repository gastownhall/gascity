package beadstest

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunFenceConformance exercises the ownership-fence bump contract that GC's
// native in-memory stores (MemStore, FileStore) must satisfy so guarded-release
// unit tests are non-vacuous. It mirrors the beads-side behavioral fence tests
// (internal/storage/dolt/fence_test.go): ClaimFence is a monotonic counter
// bumped ONLY on ownership transitions — a claim/unclaim (assignee change) or a
// reopen (closed→open) — and NEVER by content mutations or a close.
//
// It is exercised only against the native Mem/File stores. A bd-backed store
// leaves ClaimFence at 0 until the pinned bd emits claim_fence, so running this
// against BdStore/NativeDoltStore would be vacuous (every read returns 0).
//
// newStore must return a fresh, empty store for each call.
func RunFenceConformance(t *testing.T, newStore func() beads.Store) {
	t.Helper()

	fenceOf := func(t *testing.T, s beads.Store, id string) int64 {
		t.Helper()
		b, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		return b.ClaimFence
	}
	create := func(t *testing.T, s beads.Store) string {
		t.Helper()
		b, err := s.Create(beads.Bead{Title: "fence subject"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return b.ID
	}
	str := func(v string) *string { return &v }

	t.Run("CreateStartsFenceAtZero", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if got := fenceOf(t, s, id); got != 0 {
			t.Errorf("fresh bead ClaimFence = %d, want 0", got)
		}
	})

	t.Run("ClaimBumpsFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != 1 {
			t.Errorf("after claim ClaimFence = %d, want 1", got)
		}
	})

	t.Run("SameOwnerReclaimDoesNotBumpFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		f1 := fenceOf(t, s, id)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != f1 {
			t.Errorf("re-claim by the same owner bumped ClaimFence %d→%d; a no-op ownership write must not bump", f1, got)
		}
	})

	t.Run("UnclaimBumpsFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		f1 := fenceOf(t, s, id)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("")}); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != f1+1 {
			t.Errorf("after unclaim ClaimFence = %d, want %d", got, f1+1)
		}
	})

	t.Run("AssigneeChangeBumpsFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-a")}); err != nil {
			t.Fatal(err)
		}
		f1 := fenceOf(t, s, id)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-b")}); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != f1+1 {
			t.Errorf("after owner handoff ClaimFence = %d, want %d", got, f1+1)
		}
	})

	t.Run("PlainUpdateDoesNotBumpFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		f1 := fenceOf(t, s, id)
		if err := s.Update(id, beads.UpdateOpts{Title: str("renamed")}); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(id, beads.UpdateOpts{Metadata: map[string]string{"note": "x"}}); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != f1 {
			t.Errorf("content-only update bumped ClaimFence %d→%d; only ownership transitions bump", f1, got)
		}
	})

	t.Run("CloseDoesNotBumpFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		f1 := fenceOf(t, s, id)
		if err := s.Close(id); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != f1 {
			t.Errorf("close bumped ClaimFence %d→%d; close is not an ownership transition", f1, got)
		}
	})

	t.Run("ReopenBumpsFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(id); err != nil {
			t.Fatal(err)
		}
		f := fenceOf(t, s, id) // close did not bump
		if err := s.Reopen(id); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != f+1 {
			t.Errorf("after reopen ClaimFence = %d, want %d (closed→open starts a new ownership generation)", got, f+1)
		}
	})

	t.Run("InProgressToOpenKeepsFence", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1"), Status: str("in_progress")}); err != nil {
			t.Fatal(err)
		}
		f1 := fenceOf(t, s, id)
		// in_progress→open KEEPING the assignee is not a transition: the row stays
		// claimable only by the same owner, and the eventual release bumps at the
		// real ownership boundary.
		if err := s.Update(id, beads.UpdateOpts{Status: str("open")}); err != nil {
			t.Fatal(err)
		}
		if got := fenceOf(t, s, id); got != f1 {
			t.Errorf("in_progress→open (same owner) bumped ClaimFence %d→%d; only closed→open is a transition", f1, got)
		}
	})

	t.Run("FenceIsMonotonicAcrossTransitions", func(t *testing.T) {
		s := newStore()
		id := create(t, s)
		prev := fenceOf(t, s, id)
		ops := []beads.UpdateOpts{
			{Assignee: str("a")}, // claim
			{Assignee: str("b")}, // handoff
			{Assignee: str("")},  // unclaim
			{Assignee: str("c")}, // reclaim
		}
		for i, op := range ops {
			if err := s.Update(id, op); err != nil {
				t.Fatal(err)
			}
			cur := fenceOf(t, s, id)
			if cur <= prev {
				t.Errorf("op %d: ClaimFence did not advance (%d→%d); ownership transitions must be strictly monotonic", i, prev, cur)
			}
			prev = cur
		}
	})

	// The guarded-write path (UpdateIfMatch on ConditionalWriter) is the exact
	// entry point the fence exists to protect. It shares applyUpdateLocked with
	// Update, but a direct assertion keeps a future refactor of the CAS path from
	// silently dropping the bump.
	t.Run("ConditionalWriteBumpsFenceOnOwnershipChange", func(t *testing.T) {
		s := newStore()
		w, ok := beads.ConditionalWriterFor(s)
		if !ok {
			t.Skip("store has no ConditionalWriter")
		}
		id := create(t, s)
		b, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		f0 := b.ClaimFence
		if err := w.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatalf("UpdateIfMatch(claim): %v", err)
		}
		if got := fenceOf(t, s, id); got != f0+1 {
			t.Errorf("guarded-write claim ClaimFence = %d, want %d", got, f0+1)
		}
	})

	t.Run("ConditionalWriteDoesNotBumpFenceOnContentChange", func(t *testing.T) {
		s := newStore()
		w, ok := beads.ConditionalWriterFor(s)
		if !ok {
			t.Skip("store has no ConditionalWriter")
		}
		id := create(t, s)
		if err := s.Update(id, beads.UpdateOpts{Assignee: str("worker-1")}); err != nil {
			t.Fatal(err)
		}
		b, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		f1 := b.ClaimFence
		if err := w.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Title: str("renamed via CAS")}); err != nil {
			t.Fatalf("UpdateIfMatch(content): %v", err)
		}
		if got := fenceOf(t, s, id); got != f1 {
			t.Errorf("content-only guarded write bumped ClaimFence %d→%d", f1, got)
		}
	})
}
