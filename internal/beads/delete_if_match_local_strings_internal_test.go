package beads

import "testing"

// TestDeleteIfMatchRemovesLocalStrings pins DeleteIfMatch to Delete's scrub:
// a fenced delete must drop the bead's clone-local strings, as the plain
// Delete and NativeDoltStore.DeleteIfMatch already do. Otherwise a caller that
// moves from Delete to DeleteIfMatch (the closed-session purge) leaks a
// sidecar entry per deleted bead, and a reused id inherits the old values.
func TestDeleteIfMatchRemovesLocalStrings(t *testing.T) {
	t.Run("MemStore", func(t *testing.T) {
		m := NewMemStore()
		b, err := m.Create(Bead{Title: "fenced delete"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := m.SetLocalString(b.ID, "last_woke_at", "2026-10-04T00:00:00Z"); err != nil {
			t.Fatalf("SetLocalString: %v", err)
		}
		got, err := m.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := m.DeleteIfMatch(b.ID, got.Revision); err != nil {
			t.Fatalf("DeleteIfMatch: %v", err)
		}
		m.mu.Lock()
		left := m.localStrings[b.ID]
		m.mu.Unlock()
		if len(left) != 0 {
			t.Fatalf("local strings after DeleteIfMatch = %v, want none", left)
		}
	})
	t.Run("SQLiteStore", func(t *testing.T) {
		opened, err := OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		s := opened.(*SQLiteStore)
		t.Cleanup(func() { _ = s.CloseStore() })
		b, err := s.Create(Bead{Title: "fenced delete"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.SetLocalString(b.ID, "last_woke_at", "2026-10-04T00:00:00Z"); err != nil {
			t.Fatalf("SetLocalString: %v", err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := s.DeleteIfMatch(b.ID, got.Revision); err != nil {
			t.Fatalf("DeleteIfMatch: %v", err)
		}
		left, err := s.localStrings.Get(b.ID, "last_woke_at")
		if err != nil {
			t.Fatalf("sidecar Get: %v", err)
		}
		if left != "" {
			t.Fatalf("local string after DeleteIfMatch = %q, want it removed", left)
		}
	})
}
