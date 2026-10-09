package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// closeBackend opens one real store kind for the session close tests. store
// is what the session front door wraps. backing is where a concurrent writer
// lands: the raw store underneath any cache, so a cached front door holds a
// stale revision, as a controller does when another process writes the row.
type closeBackend struct {
	name   string
	atomic bool
	open   func(t *testing.T) (store, backing beads.Store)
}

func sessionCloseBackends() []closeBackend {
	openSQLite := func(t *testing.T) beads.Store {
		t.Helper()
		opened, err := beads.OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		store := opened.(*beads.SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })
		return store
	}
	openFile := func(t *testing.T) beads.Store {
		t.Helper()
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatalf("OpenFileStore: %v", err)
		}
		return store
	}
	cached := func(t *testing.T, backing beads.Store) beads.Store {
		t.Helper()
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return cache
	}
	return []closeBackend{
		{name: "AtomicCloseMemStore", atomic: true, open: func(*testing.T) (beads.Store, beads.Store) {
			s := beads.NewAtomicCloseMemStore()
			return s, s
		}},
		{name: "FileStore", atomic: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := openFile(t)
			return s, s
		}},
		{name: "SQLiteStore", atomic: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := openSQLite(t)
			return s, s
		}},
		{name: "CachingStore/FileStore", atomic: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			backing := openFile(t)
			return cached(t, backing), backing
		}},
		{name: "CachingStore/SQLiteStore", atomic: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			backing := openSQLite(t)
			return cached(t, backing), backing
		}},
		// Plain MemStore is the stand-in for every store without the capability
		// (BdStore, exec, a legacy sqlite layout): the documented two-write
		// fallback.
		{name: "MemStore", atomic: false, open: func(*testing.T) (beads.Store, beads.Store) {
			s := beads.NewMemStore()
			return s, s
		}},
	}
}

// TestCloseAcrossBackendsKeepsTerminalMetadataWithTheClose runs the stale
// awake writer against every in-tree store kind. On a store that provides the
// atomic close, the wake wins the fence and changes the facts the close was
// decided on, so the close writes nothing and returns
// ErrSessionCloseSuperseded; the next pass
// decides again and closes through the fenced single write only. On a store
// without the capability, the front door must take the two-write fallback,
// and nothing more is promised there: that residual race is documented on
// Store.Close.
func TestCloseAcrossBackendsKeepsTerminalMetadataWithTheClose(t *testing.T) {
	for _, backend := range sessionCloseBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			if _, ok := beads.AtomicConditionalCloserFor(beads.SessionStore{Store: store}); ok != backend.atomic {
				t.Fatalf("AtomicConditionalCloserFor(%s) = %v, want %v", backend.name, ok, backend.atomic)
			}
			created := seedOpenSession(t, store, "s-"+backend.name)
			tracing := &closeInterferenceStore{Store: store}
			if backend.atomic {
				tracing.interfere = staleAwakeOnce(backing)
			}
			front := NewStore(beads.SessionStore{Store: tracing})

			closed, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
			if backend.atomic {
				if !errors.Is(err, ErrSessionCloseSuperseded) || closed {
					t.Fatalf("Close = (%v, %v), want (false, ErrSessionCloseSuperseded) after a wake won the fence", closed, err)
				}
				woken, getErr := backing.Get(created.ID)
				if getErr != nil {
					t.Fatalf("Get: %v", getErr)
				}
				if woken.Status != "open" || woken.Metadata["state"] != string(StateAwake) {
					t.Fatalf("row = status %q state %q, want the wake's open/awake row", woken.Status, woken.Metadata["state"])
				}
				if tracing.atomicCalls != 1 || tracing.plainCloseCalls != 0 {
					t.Fatalf("atomic/plain close calls = %d/%d, want 1/0 (one lost fence, no retry, no split write)", tracing.atomicCalls, tracing.plainCloseCalls)
				}
				// The next pass decides on the woken row and closes it.
				closed, err = front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
			}
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !closed {
				t.Fatal("Close reported not-closed for an open session")
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" || got.Metadata["state"] != string(StateDrained) ||
				got.Metadata["close_reason"] != CanonicalCloseReason(string(StateDrained)) {
				t.Fatalf("terminal row = status %q state %q close_reason %q, want closed/%s with the ClosePatch reason",
					got.Status, got.Metadata["state"], got.Metadata["close_reason"], StateDrained)
			}
			if backend.atomic {
				if tracing.atomicCalls != 2 || tracing.plainCloseCalls != 0 {
					t.Fatalf("atomic/plain close calls = %d/%d, want 2/0 (the refused attempt, then the next pass, no split write)", tracing.atomicCalls, tracing.plainCloseCalls)
				}
			} else if tracing.atomicCalls != 1 || tracing.plainCloseCalls != 1 {
				// The tracing wrapper itself carries the method, so it is
				// asked once, answers unsupported (the backing lacks the
				// capability), and the front door falls back.
				t.Fatalf("atomic/plain close calls = %d/%d, want 1/1 (unsupported, then the fallback)", tracing.atomicCalls, tracing.plainCloseCalls)
			}

			again, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
			if err != nil || again {
				t.Fatalf("second Close = (%v, %v), want (false, nil) on a closed session", again, err)
			}
		})
	}
}
