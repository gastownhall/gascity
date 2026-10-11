package beads_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Kills a backend write verb that skips the session-key hook: on every
// backend, each verb reports the keys it writes to a session bead (typed, or
// created untyped with the gc:session label), and none to another bead.
func TestStoreWriteVerbsReportSessionKeys(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) beads.Store{
		"MemStore": func(*testing.T) beads.Store { return beads.NewAtomicCloseMemStore() },
		"SQLiteStore": func(t *testing.T) beads.Store {
			s, err := beads.OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"NativeDoltStore": func(*testing.T) beads.Store { return beads.NewNativeDoltStoreForConformance() },
	} {
		t.Run(name, func(t *testing.T) { exerciseSessionKeyHook(t, open(t)) })
	}
}

// exerciseSessionKeyHook runs every write verb s has, each on a session bead
// and on a work bead, and checks each reported the session bead's key, and
// only that key, as it ran.
func exerciseSessionKeyHook(t *testing.T, s beads.Store) {
	t.Helper()
	var got []string
	var writes []map[string]string // the write hook's patches: updates only
	beads.SetSessionKeyHook(func(key string) { got = append(got, key) })
	defer beads.SetSessionKeyHook(nil)
	beads.SetSessionWriteHook(func(kvs map[string]string) { writes = append(writes, maps.Clone(kvs)) })
	defer beads.SetSessionWriteHook(nil)
	verb := func(name, key string, run func() error) {
		t.Helper()
		got, writes = nil, nil
		mustDo(t, run())
		if !slices.Contains(got, key) || slices.ContainsFunc(got, func(k string) bool { return strings.HasPrefix(k, "w_") }) {
			t.Errorf("%s reported %v, want %s and no work-bead key", name, got, key)
		}
		seen := slices.ContainsFunc(writes, func(kvs map[string]string) bool { _, ok := kvs[key]; return ok })
		work := slices.ContainsFunc(writes, func(kvs map[string]string) bool {
			for k := range kvs {
				if strings.HasPrefix(k, "w_") {
					return true
				}
			}
			return false
		})
		if create := strings.Contains(name, "Create"); seen == create || work {
			t.Errorf("%s: write hook saw %v; want the session update's patch (none for a create) and no work bead's", name, writes)
		}
	}
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := s.Create(b)
		mustDo(t, err)
		return created
	}
	var session, work beads.Bead
	verb("Create", "k_create", func() error {
		session = mk(beads.Bead{Title: "s", Type: "session", Metadata: map[string]string{"k_create": "v"}})
		work = mk(beads.Bead{Title: "w", Type: "task", Metadata: map[string]string{"w_create": "v"}})
		return nil
	})
	verb("Create untyped with the session label", "k_label", func() error {
		mk(beads.Bead{Title: "l", Labels: []string{"gc:session"}, Metadata: map[string]string{"k_label": "v"}})
		return nil
	})
	both := func(name, key string, run func(id, key string) error) {
		verb(name, key, func() error {
			if err := run(session.ID, key); err != nil {
				return err
			}
			return run(work.ID, "w_"+key)
		})
	}
	both("SetMetadata", "k_set", func(id, k string) error { return s.SetMetadata(id, k, "v") })
	both("SetMetadataBatch", "k_batch", func(id, k string) error { return s.SetMetadataBatch(id, map[string]string{k: "v"}) })
	both("Update", "k_update", func(id, k string) error { return s.Update(id, beads.UpdateOpts{Metadata: map[string]string{k: "v"}}) })
	if cw, ok := beads.ConditionalWriterFor(s); ok {
		both("UpdateIfMatch", "k_ifmatch", func(id, k string) error {
			b, err := s.Get(id)
			if err != nil {
				return err
			}
			return unlessUnsupported(cw.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Metadata: map[string]string{k: "v"}}))
		})
		both("CompareAndSetMetadataKey", "k_cas", func(id, k string) error {
			_, err := cw.CompareAndSetMetadataKey(id, k, "", "v")
			return unlessUnsupported(err)
		})
	}
	if closer, ok := beads.AtomicConditionalCloserFor(s); ok {
		row := mk(beads.Bead{Title: "c", Type: "session"})
		verb("CloseWithMetadataIfMatch", "k_atomic_close", func() error {
			_, err := closer.CloseWithMetadataIfMatch(row.ID, row.Revision, map[string]string{"k_atomic_close": "v"})
			return err
		})
	}
	verb("Tx Create", "k_tx_create", func() error {
		return s.Tx("hook", func(tx beads.Tx) error {
			_, err := tx.Create(beads.Bead{Title: "t", Type: "session", Metadata: map[string]string{"k_tx_create": "v"}})
			return err
		})
	})
	verb("Tx Update", "k_tx_update", func() error {
		return s.Tx("hook", func(tx beads.Tx) error {
			return tx.Update(session.ID, beads.UpdateOpts{Metadata: map[string]string{"k_tx_update": "v"}})
		})
	})
	verb("Tx SetMetadataBatch", "k_tx_batch", func() error {
		return s.Tx("hook", func(tx beads.Tx) error { return tx.SetMetadataBatch(session.ID, map[string]string{"k_tx_batch": "v"}) })
	})
	other := mk(beads.Bead{Title: "o", Type: "session"})
	verb("CloseAll", "k_closeall", func() error {
		_, err := s.CloseAll([]string{other.ID, work.ID}, map[string]string{"k_closeall": "v"})
		return err
	})
}

// unlessUnsupported forgives a backend (a bd without CAS) that refuses a
// conditional write after reporting it: the hook fires at the verb's entry.
func unlessUnsupported(err error) error {
	if errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		return nil
	}
	return err
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Kills a store that reads a row to learn its type only for the key hook: with
// the write hook alone installed, every backend's update of a session bead
// still reaches it. The native conformance double reports each update twice:
// once from the store verb (by ID) and once from the MemStore it stores in.
func TestSessionWriteHookAloneSeesUpdates(t *testing.T) {
	for name, tc := range map[string]struct {
		s       beads.Store
		perEach int
	}{
		"MemStore":        {beads.NewAtomicCloseMemStore(), 1},
		"NativeDoltStore": {beads.NewNativeDoltStoreForConformance(), 2},
	} {
		var writes []map[string]string
		beads.SetSessionWriteHook(func(kvs map[string]string) { writes = append(writes, maps.Clone(kvs)) })
		b, err := tc.s.Create(beads.Bead{Title: "s", Type: "session"})
		mustDo(t, err)
		mustDo(t, tc.s.SetMetadataBatch(b.ID, map[string]string{"k": "v"}))
		mustDo(t, tc.s.Update(b.ID, beads.UpdateOpts{Metadata: map[string]string{"k2": "v"}}))
		beads.SetSessionWriteHook(nil)
		if len(writes) != 2*tc.perEach || writes[0]["k"] != "v" || writes[len(writes)-1]["k2"] != "v" {
			t.Errorf("%s: write hook alone saw %v, want each update %d time(s)", name, writes, tc.perEach)
		}
	}
}
