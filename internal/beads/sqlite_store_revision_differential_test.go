package beads_test

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestSQLiteStoreRevisionMovesWhereMemStoreDoes is a differential over every
// write verb a session row sees, the batch and Tx paths included: the SQLite
// engine must move a row's revision on exactly the steps the reference stores
// move it. A step SQLite misses lets a stale *IfMatch win once the binding is
// stamped; a step SQLite adds (a dependency edge or a refused write) is a
// spurious precondition failure no other store produces. Dependency edges are
// separate persistence, outside the REVISION CONTRACT on beads.ConditionalWriter,
// and the reference stores leave the row's revision alone for them.
func TestSQLiteStoreRevisionMovesWhereMemStoreDoes(t *testing.T) {
	type step struct {
		name  string
		moves bool // whether the reference stores move the revision
		run   func(t *testing.T, s beads.Store, id, other string) error
	}
	str := func(v string) *string { return &v }
	writer := func(t *testing.T, s beads.Store) beads.ConditionalWriter {
		t.Helper()
		w, ok := beads.ConditionalWriterFor(s)
		if !ok {
			t.Fatalf("%T does not implement ConditionalWriter", s)
		}
		return w
	}
	revOf := func(t *testing.T, s beads.Store, id string) int64 {
		t.Helper()
		b, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		return b.Revision
	}
	// claim runs the atomic Claim where the store has one. The reference
	// stores do not: a first claim is the same whole-row write as
	// Update(assign), and a same-owner reclaim writes nothing.
	claim := func(t *testing.T, s beads.Store, id string, reclaim bool) error {
		t.Helper()
		claimer, ok := s.(interface {
			Claim(id, assignee string) (beads.Bead, bool, error)
		})
		if !ok {
			if reclaim {
				return nil
			}
			return s.Update(id, beads.UpdateOpts{Status: str("in_progress"), Assignee: str("claimer")})
		}
		_, claimed, err := claimer.Claim(id, "claimer")
		if err == nil && !claimed {
			t.Fatalf("Claim(reclaim=%v) lost on a row nobody else holds", reclaim)
		}
		return err
	}
	steps := []step{
		{"Update(title)", true, func(_ *testing.T, s beads.Store, id, _ string) error {
			return s.Update(id, beads.UpdateOpts{Title: str("renamed")})
		}},
		{"Update(labels)", true, func(_ *testing.T, s beads.Store, id, _ string) error {
			return s.Update(id, beads.UpdateOpts{Labels: []string{"l1"}})
		}},
		{"SetMetadata", true, func(_ *testing.T, s beads.Store, id, _ string) error { return s.SetMetadata(id, "k", "v1") }},
		{"SetMetadataBatch", true, func(_ *testing.T, s beads.Store, id, _ string) error {
			return s.SetMetadataBatch(id, map[string]string{"a": "1", "b": "2"})
		}},
		{"UpdateIfMatch", true, func(t *testing.T, s beads.Store, id, _ string) error {
			return writer(t, s).UpdateIfMatch(id, revOf(t, s, id), beads.UpdateOpts{Metadata: map[string]string{"state": "awake"}})
		}},
		{"UpdateIfMatch(stale)", false, func(t *testing.T, s beads.Store, id, _ string) error {
			err := writer(t, s).UpdateIfMatch(id, revOf(t, s, id)+1000, beads.UpdateOpts{Title: str("stale")})
			if !beads.IsPreconditionFailed(err) {
				t.Fatalf("stale UpdateIfMatch = %v, want a precondition failure", err)
			}
			return nil
		}},
		{"CompareAndSetMetadataKey", true, func(t *testing.T, s beads.Store, id, _ string) error {
			if ok, err := writer(t, s).CompareAndSetMetadataKey(id, "lease", "", "me"); err != nil || !ok {
				t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want a swap", ok, err)
			}
			return nil
		}},
		{"CompareAndSetMetadataKey(mismatch)", false, func(t *testing.T, s beads.Store, id, _ string) error {
			if ok, err := writer(t, s).CompareAndSetMetadataKey(id, "lease", "someone-else", "me"); err != nil || ok {
				t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want a lost race", ok, err)
			}
			return nil
		}},
		{"Update(assign)", true, func(_ *testing.T, s beads.Store, id, _ string) error {
			return s.Update(id, beads.UpdateOpts{Status: str("in_progress"), Assignee: str("worker")})
		}},
		{"ReleaseIfCurrent(wrong)", false, func(t *testing.T, s beads.Store, id, _ string) error {
			released, err := s.(beads.ConditionalAssignmentReleaser).ReleaseIfCurrent(id, "nobody")
			if released {
				t.Fatal("ReleaseIfCurrent released for the wrong assignee")
			}
			return err
		}},
		{"ReleaseIfCurrent", true, func(_ *testing.T, s beads.Store, id, _ string) error {
			_, err := s.(beads.ConditionalAssignmentReleaser).ReleaseIfCurrent(id, "worker")
			return err
		}},
		{"Claim", true, func(t *testing.T, s beads.Store, id, _ string) error {
			return claim(t, s, id, false)
		}},
		{"Claim(same owner)", false, func(t *testing.T, s beads.Store, id, _ string) error {
			return claim(t, s, id, true)
		}},
		{"Tx(SetMetadataBatch)", true, func(_ *testing.T, s beads.Store, id, _ string) error {
			return s.Tx("differential", func(tx beads.Tx) error {
				return tx.SetMetadataBatch(id, map[string]string{"tx": "1"})
			})
		}},
		{"DepAdd", false, func(_ *testing.T, s beads.Store, id, other string) error { return s.DepAdd(id, other, "blocks") }},
		{"DepRemove", false, func(_ *testing.T, s beads.Store, id, other string) error { return s.DepRemove(id, other) }},
		{"Get+List", false, func(_ *testing.T, s beads.Store, id, _ string) error {
			if _, err := s.Get(id); err != nil {
				return err
			}
			_, err := s.List(beads.ListQuery{AllowScan: true})
			return err
		}},
		{"Close", true, func(_ *testing.T, s beads.Store, id, _ string) error { return s.Close(id) }},
		{"Reopen", true, func(_ *testing.T, s beads.Store, id, _ string) error { return s.Reopen(id) }},
		{"CloseAll", true, func(_ *testing.T, s beads.Store, id, _ string) error {
			_, err := s.CloseAll([]string{id}, map[string]string{"close_reason": "done"})
			return err
		}},
		{"CloseIfMatch", true, func(t *testing.T, s beads.Store, id, _ string) error {
			if err := s.Reopen(id); err != nil {
				return err
			}
			return writer(t, s).CloseIfMatch(id, revOf(t, s, id))
		}},
	}

	// moves runs the script on s and reports, per step, whether the row's
	// revision moved.
	moves := func(t *testing.T, s beads.Store) []bool {
		t.Helper()
		row, err := s.Create(beads.Bead{Title: "session-row"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		other, err := s.Create(beads.Bead{Title: "dep-target"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got := make([]bool, len(steps))
		for i, st := range steps {
			before := revOf(t, s, row.ID)
			if err := st.run(t, s, row.ID, other.ID); err != nil {
				t.Fatalf("%s: %v", st.name, err)
			}
			got[i] = revOf(t, s, row.ID) != before
		}
		return got
	}

	want := moves(t, beads.NewMemStore())
	for i, st := range steps {
		if want[i] != st.moves {
			t.Fatalf("MemStore %s: revision moved = %v, the contract says %v", st.name, want[i], st.moves)
		}
	}
	file, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	for name, got := range map[string][]bool{
		"FileStore":   moves(t, file),
		"SQLiteStore": moves(t, newSQLiteForConformance(t)),
	} {
		for i, st := range steps {
			if got[i] != want[i] {
				t.Errorf("%s %s: revision moved = %v, MemStore moved = %v", name, st.name, got[i], want[i])
			}
		}
	}
}
