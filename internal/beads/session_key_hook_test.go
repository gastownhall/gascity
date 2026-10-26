package beads_test

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
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
	type write struct {
		id  string
		kvs map[string]string
	}
	var writes []write // the write hook's reports
	beads.SetSessionKeyHook(func(key string) { got = append(got, key) })
	defer beads.SetSessionKeyHook(nil)
	beads.SetSessionWriteHook(func(id string, kvs map[string]string) { writes = append(writes, write{id, maps.Clone(kvs)}) })
	defer beads.SetSessionWriteHook(nil)
	var session, work beads.Bead
	verb := func(name, key string, run func() error) {
		t.Helper()
		got, writes = nil, nil
		mustDo(t, run())
		if !slices.Contains(got, key) || slices.ContainsFunc(got, func(k string) bool { return strings.HasPrefix(k, "w_") }) {
			t.Errorf("%s reported %v, want %s and no work-bead key", name, got, key)
		}
		// The write hook sees each write once (creates included), an update
		// with its row's ID, and never a work bead's.
		var mine []write
		for _, w := range writes {
			for k := range w.kvs {
				if strings.HasPrefix(k, "w_") {
					t.Errorf("%s: write hook saw a work bead's write %v", name, w)
				}
			}
			if _, ok := w.kvs[key]; ok {
				mine = append(mine, w)
			}
		}
		create := strings.Contains(name, "Create")
		if len(mine) != 1 || (!create && mine[0].id == "") {
			t.Errorf("%s: write hook saw %v, want the write once, with its row", name, mine)
		}
	}
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := s.Create(b)
		mustDo(t, err)
		return created
	}
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
// still reaches it, once, with the row's ID.
func TestSessionWriteHookAloneSeesUpdates(t *testing.T) {
	for name, s := range map[string]beads.Store{
		"MemStore":        beads.NewAtomicCloseMemStore(),
		"NativeDoltStore": beads.NewNativeDoltStoreForConformance(),
	} {
		b, err := s.Create(beads.Bead{Title: "s", Type: "session"})
		mustDo(t, err)
		var ids []string
		var writes []map[string]string
		beads.SetSessionWriteHook(func(id string, kvs map[string]string) { ids, writes = append(ids, id), append(writes, maps.Clone(kvs)) })
		mustDo(t, s.SetMetadataBatch(b.ID, map[string]string{"k": "v"}))
		mustDo(t, s.Update(b.ID, beads.UpdateOpts{Metadata: map[string]string{"k2": "v"}}))
		beads.SetSessionWriteHook(nil)
		if len(writes) != 2 || writes[0]["k"] != "v" || writes[1]["k2"] != "v" || ids[0] != b.ID || ids[1] != b.ID {
			t.Errorf("%s: write hook alone saw %v for rows %v, want each update once for %s", name, writes, ids, b.ID)
		}
	}
}

// Kills a guarded batch that skips the session hooks, or that reports a create
// only once the batch passes its checks: on every backend that batches, a batch
// reports each session create's seed and each session update's patch to both
// hooks once, the write hook with the row's ID, and nothing for a work bead. A
// create is reported at the verb's entry, as Create reports one before its
// checks, so a batch the store then refuses has reported its creates too.
func TestGuardedBatchReportsSessionKeys(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) beads.Store{
		"MemStore": func(*testing.T) beads.Store { return beads.NewMemStore() },
		"FileStore": func(t *testing.T) beads.Store {
			s, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
			mustDo(t, err)
			return s
		},
		"SQLiteStore":     func(t *testing.T) beads.Store { return newSQLiteForConformance(t) },
		"NativeDoltStore": func(*testing.T) beads.Store { return beads.NewNativeDoltStoreForGuardedBatchConformance() },
	} {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			gb, ok := beads.GuardedBatcherFor(s)
			if !ok {
				t.Fatalf("GuardedBatcherFor(%T) = false, want a guarded batcher", s)
			}
			session, err := s.Create(beads.Bead{Title: "s", Type: "session"})
			mustDo(t, err)
			// Pinned ids share the namespace the store mints under.
			pin := session.ID[:strings.LastIndex(session.ID, "-")] + "-gbhook"
			var keys []string
			writes := map[string][]string{} // the write hook's reports, by row
			beads.SetSessionKeyHook(func(key string) { keys = append(keys, key) })
			defer beads.SetSessionKeyHook(nil)
			beads.SetSessionWriteHook(func(id string, kvs map[string]string) {
				writes[id] = append(writes[id], slices.Sorted(maps.Keys(kvs))...)
			})
			defer beads.SetSessionWriteHook(nil)
			create := func(name, typ, key string) beads.GuardedItem {
				return beads.GuardedItem{Create: &beads.Bead{ID: pin + name, Title: name, Type: typ, Metadata: map[string]string{key: "v"}}}
			}
			_, err = gb.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
				create("new", "session", "k_create"),
				create("work", "task", "w_create"),
				{Update: &beads.GuardedUpdate{ID: session.ID, Opts: beads.UpdateOpts{Metadata: map[string]string{"k_update": "v"}}}},
			}})
			mustDo(t, err)
			title := "t"
			_, err = gb.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
				create("refused", "session", "k_refused"),
				{Update: &beads.GuardedUpdate{ID: pin + "missing", Opts: beads.UpdateOpts{Title: &title}}},
			}})
			if !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("ApplyGuarded over a missing row = %v, want ErrNotFound", err)
			}
			slices.Sort(keys)
			if want := []string{"k_create", "k_refused", "k_update"}; !slices.Equal(keys, want) {
				t.Errorf("key hook saw %v, want %v", keys, want)
			}
			want := map[string][]string{pin + "new": {"k_create"}, session.ID: {"k_update"}, pin + "refused": {"k_refused"}}
			if !maps.EqualFunc(writes, want, slices.Equal[[]string]) {
				t.Errorf("write hook saw %v, want %v", writes, want)
			}
		})
	}
}
