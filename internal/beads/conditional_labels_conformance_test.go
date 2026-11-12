package beads_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestConditionalUpdateLabelsConformance runs the label leg of the
// ConditionalWriter contract over every in-process implementation, raw and
// under the CachingStore the controller reads through. Each either guards
// labels with the row revision or refuses them; none may apply them unguarded.
// BdStore, which needs a bd binary, is pinned by TestBdStoreStillRefusesLabels.
func TestConditionalUpdateLabelsConformance(t *testing.T) {
	openFile := func(t *testing.T) beads.Store {
		s, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatalf("OpenFileStore: %v", err)
		}
		return s
	}
	cached := func(open func(t *testing.T) beads.Store) func(t *testing.T) beads.Store {
		return func(t *testing.T) beads.Store {
			c := beads.NewCachingStoreForTest(open(t), nil)
			if err := c.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			return c
		}
	}
	openMem := func(*testing.T) beads.Store { return beads.NewMemStore() }
	openSQLite := func(t *testing.T) beads.Store { return newSQLiteForConformance(t) }
	openNative := func(*testing.T) beads.Store { return beads.NewNativeDoltStoreForConformance() }

	for _, tc := range []struct {
		name    string
		open    func(t *testing.T) beads.Store
		guarded bool
	}{
		{"MemStore", openMem, true},
		{"FileStore", openFile, false},
		{"SQLiteStore", openSQLite, true},
		{"NativeDoltStore", openNative, true},
		{"CachingStore/MemStore", cached(openMem), true},
		{"CachingStore/FileStore", cached(openFile), false},
		{"CachingStore/SQLiteStore", cached(openSQLite), true},
		{"CachingStore/NativeDoltStore", cached(openNative), true},
	} {
		beadstest.RunConditionalLabelsConformance(t, tc.name, tc.open, tc.guarded)
	}
}

// TestConditionalLabelUpdateLosesToConcurrentWrite pins, on the SQLite
// revision layout, that a write from another handle on the same database
// between the read and the label CAS refuses the whole CAS, raw and through a
// CachingStore primed before that write. Native Dolt has the same test against
// real Dolt (TestNativeDoltLabelCASLosesToConcurrentWrite, integration).
func TestConditionalLabelUpdateLosesToConcurrentWrite(t *testing.T) {
	for _, viaCache := range []bool{false, true} {
		dir := t.TempDir()
		open := func() *beads.SQLiteStore {
			s, err := beads.OpenSQLiteStore(dir)
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			store := s.(*beads.SQLiteStore)
			t.Cleanup(func() { _ = store.CloseStore() })
			return store
		}
		var ours beads.Store = open()
		other := open()
		if viaCache {
			c := beads.NewCachingStoreForTest(ours, nil)
			if err := c.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			ours = c
		}
		created, err := ours.Create(beads.Bead{Title: "labels-race", Labels: []string{"keep"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		read, err := ours.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := other.SetMetadata(created.ID, "external", "1"); err != nil {
			t.Fatalf("external SetMetadata: %v", err)
		}
		writer, ok := beads.ConditionalWriterFor(ours)
		if !ok {
			t.Fatalf("%T is not a ConditionalWriter", ours)
		}
		err = writer.UpdateIfMatch(created.ID, read.Revision, beads.UpdateOpts{
			Labels:       []string{"late"},
			RemoveLabels: []string{"keep"},
		})
		if !beads.IsPreconditionFailed(err) {
			t.Fatalf("viaCache=%v: UpdateIfMatch after another handle's write = %v, want *PreconditionFailedError", viaCache, err)
		}
		for name, s := range map[string]beads.Store{"ours": ours, "other": other} {
			after, err := s.Get(created.ID)
			if err != nil {
				t.Fatalf("Get via %s: %v", name, err)
			}
			if !slices.Contains(after.Labels, "keep") || slices.Contains(after.Labels, "late") {
				t.Fatalf("viaCache=%v: labels via %s = %v after a refused CAS, want keep and no late", viaCache, name, after.Labels)
			}
		}
	}
}
