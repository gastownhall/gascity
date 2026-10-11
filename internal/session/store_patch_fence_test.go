package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// patchFenceBackend opens one store kind for the lifecycle-fenced patch tests.
// store is what the session front door wraps. backing is where a concurrent
// writer lands: the raw store underneath any cache, the way another process
// (a `gc session suspend` CLI) writes the row while the controller holds a
// cached view of it. fenced reports whether the store resolves a conditional
// writer, so the patch is revision-fenced rather than re-read-checked only.
// staleCache marks a cached front door whose re-read cannot see a write to the
// backing. The SQLiteStore rows are a split city's relocated session binding,
// stamped the way storage boot stamps the engine it opens.
type patchFenceBackend struct {
	name       string
	fenced     bool
	staleCache bool
	open       func(t *testing.T) (store, backing beads.Store)
}

func patchFenceBackends() []patchFenceBackend {
	stamp := func(t *testing.T, mode gate.Mode, raw beads.Store) beads.Store {
		t.Helper()
		result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
			ScopeRoot:         t.TempDir(),
			Provider:          "file",
			ConditionalWrites: mode,
			OpenFileStore:     func() (beads.Store, error) { return raw, nil },
		})
		if err != nil {
			t.Fatalf("OpenStoreAtForCity: %v", err)
		}
		return result.Store
	}
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
	stampSQLite := func(t *testing.T, mode gate.Mode) beads.Store {
		t.Helper()
		s := openSQLite(t)
		if err := beads.StampOpenedStore(s, "SQLiteStore", mode, nil, nil); err != nil {
			t.Fatalf("StampOpenedStore: %v", err)
		}
		return s
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
	return []patchFenceBackend{
		// Conditional writes off (the default rollout mode), and every store
		// without the capability: the re-read check alone guards the patch.
		{name: "MemStore/off", fenced: false, open: func(*testing.T) (beads.Store, beads.Store) {
			s := beads.NewMemStore()
			return s, s
		}},
		{name: "SQLiteStore/off", fenced: false, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := openSQLite(t)
			return s, s
		}},
		{name: "MemStore/auto", fenced: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := stamp(t, gate.Auto, beads.NewMemStore())
			return s, s
		}},
		{name: "FileStore/auto", fenced: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := stamp(t, gate.Auto, openFile(t))
			return s, s
		}},
		{name: "SQLiteStore/auto", fenced: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			s := stampSQLite(t, gate.Auto)
			return s, s
		}},
		// The concurrent write hits the backing, so the cache serves the
		// pre-write row to the re-read. Only the revision fence can see it.
		{name: "CachingStore/FileStore/auto", fenced: true, staleCache: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			backing := stamp(t, gate.Auto, openFile(t))
			return cached(t, backing), backing
		}},
		{name: "CachingStore/SQLiteStore/require", fenced: true, staleCache: true, open: func(t *testing.T) (beads.Store, beads.Store) {
			backing := stampSQLite(t, gate.Require)
			return cached(t, backing), backing
		}},
	}
}

// suspendPatch is what `gc session suspend` writes on a managed city.
var suspendPatch = map[string]string{
	"held_until":   "2099-01-01T00:00:00Z",
	"sleep_intent": "user-hold",
	"state":        string(StateSuspended),
}

func seedPatchFenceSession(t *testing.T, store beads.Store, name string) beads.Bead {
	t.Helper()
	created, err := store.Create(sessionBeadFixture(name, "open", map[string]string{
		"state":        string(StateActive),
		"session_name": name,
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func assertSuspendSurvived(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for key, want := range suspendPatch {
		if got.Metadata[key] != want {
			t.Fatalf("row %s = %q after the stale patch, want the suspend's %q (row %v)", key, got.Metadata[key], want, got.Metadata)
		}
	}
}

// TestApplyPatchIfLifecycleUnchangedRefusesOverAConcurrentSuspend is the
// regression for the stale status heal. The heal is decided from a snapshot
// read before `gc session suspend` wrote {state=suspended, user-hold,
// held_until}. Before the fix the heal wrote state=awake unconditionally,
// reverting the suspend's state and leaving the row awake+user-hold+held. The
// patch must now write nothing, on every store kind, so the suspend survives.
func TestApplyPatchIfLifecycleUnchangedRefusesOverAConcurrentSuspend(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			writer, _, err := beads.ResolveConditionalWriter(beads.SessionStore{Store: store})
			if err != nil || (writer != nil) != backend.fenced {
				t.Fatalf("ResolveConditionalWriter(%s) = (%v, %v), want fenced=%v", backend.name, writer, err, backend.fenced)
			}
			created := seedPatchFenceSession(t, store, "s-suspend")
			front := NewStore(beads.SessionStore{Store: store})
			snapshot, err := front.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			if err := backing.SetMetadataBatch(created.ID, suspendPatch); err != nil {
				t.Fatalf("suspend write: %v", err)
			}

			applied, err := front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
			if err != nil {
				t.Fatalf("ApplyPatchIfLifecycleUnchanged: %v", err)
			}
			if applied {
				t.Fatal("stale patch reported applied over a concurrent suspend")
			}
			assertSuspendSurvived(t, backing, created.ID)
		})
	}
}

// TestApplyPatchIfLifecycleUnchangedAppliesWhenOnlyOtherKeysMoved proves the
// check is scoped to lifecycle facts: a concurrent write to a key the
// lifecycle projection does not read (a nudge timestamp) must not starve the
// patch, and the patch must not clobber that key.
func TestApplyPatchIfLifecycleUnchangedAppliesWhenOnlyOtherKeysMoved(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPatchFenceSession(t, store, "s-unrelated")
			front := NewStore(beads.SessionStore{Store: store})
			snapshot, err := front.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := backing.SetMetadata(created.ID, "last_nudge_delivered_at", "2026-09-28T01:02:03Z"); err != nil {
				t.Fatalf("unrelated write: %v", err)
			}

			applied, err := front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
			if err != nil {
				t.Fatalf("ApplyPatchIfLifecycleUnchanged: %v", err)
			}
			if backend.staleCache {
				// The cache served a stale revision, so the fence refuses
				// once. That is conservative, never a lost update, and the
				// next attempt reads the refreshed row.
				if applied {
					t.Fatal("fenced patch on a stale cached revision reported applied")
				}
				snapshot, err = front.Get(created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				applied, err = front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
				if err != nil {
					t.Fatalf("ApplyPatchIfLifecycleUnchanged retry: %v", err)
				}
			}
			if !applied {
				t.Fatal("patch refused although no lifecycle fact moved")
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata["state"] != string(StateAwake) || got.Metadata["last_nudge_delivered_at"] != "2026-09-28T01:02:03Z" {
				t.Fatalf("row = state %q last_nudge_delivered_at %q, want awake with the unrelated write kept", got.Metadata["state"], got.Metadata["last_nudge_delivered_at"])
			}
		})
	}
}

// TestApplyPatchIfLifecycleUnchangedRefusesAClosedRow covers a close that
// lands after the snapshot: the patch must not stamp live-looking state onto
// the terminal row.
func TestApplyPatchIfLifecycleUnchangedRefusesAClosedRow(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPatchFenceSession(t, store, "s-closed")
			front := NewStore(beads.SessionStore{Store: store})
			snapshot, err := front.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := backing.SetMetadataBatch(created.ID, ClosePatch(closeTestNow, string(StateDrained))); err != nil {
				t.Fatalf("close patch: %v", err)
			}
			if err := backing.Close(created.ID); err != nil {
				t.Fatalf("close: %v", err)
			}

			applied, err := front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
			if err != nil {
				t.Fatalf("ApplyPatchIfLifecycleUnchanged: %v", err)
			}
			if applied {
				t.Fatal("stale patch reported applied over a concurrent close")
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" || got.Metadata["state"] != string(StateDrained) {
				t.Fatalf("row = status %q state %q, want closed/%s", got.Status, got.Metadata["state"], StateDrained)
			}
		})
	}
}

// afterGetStore runs afterGet once, right after the front door's re-read
// returns: the window between the lifecycle check and the write. It declares
// its inner store as the conditional-writes resolution target, so the fenced
// write goes straight to the stamped store.
type afterGetStore struct {
	beads.Store
	afterGet func(id string)
}

func (s *afterGetStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if s.afterGet != nil {
		fn := s.afterGet
		s.afterGet = nil
		fn(id)
	}
	return b, err
}

func (s *afterGetStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestApplyPatchIfLifecycleUnchangedFencesAWriteAfterTheReread proves the
// revision fence, not just the re-read: a suspend that lands after the re-read
// passed the lifecycle check must still win.
func TestApplyPatchIfLifecycleUnchangedFencesAWriteAfterTheReread(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		if !backend.fenced {
			continue // the documented residual window without conditional writes
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPatchFenceSession(t, store, "s-window")
			snapshot, err := NewStore(beads.SessionStore{Store: store}).Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			racing := &afterGetStore{Store: store, afterGet: func(id string) {
				if err := backing.SetMetadataBatch(id, suspendPatch); err != nil {
					t.Errorf("suspend write: %v", err)
				}
			}}
			front := NewStore(beads.SessionStore{Store: racing})

			applied, err := front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
			if err != nil {
				t.Fatalf("ApplyPatchIfLifecycleUnchanged: %v", err)
			}
			if applied {
				t.Fatal("patch reported applied although a suspend won the revision fence")
			}
			assertSuspendSurvived(t, backing, created.ID)
		})
	}
}

// TestApplyPatchIfLifecycleUnchangedRequireModeFailsClosed pins the rollout
// contract: under require, a store that cannot fence returns the refusal and
// writes nothing, rather than falling back to an unconditional write.
func TestApplyPatchIfLifecycleUnchangedRequireModeFailsClosed(t *testing.T) {
	mem := beads.NewMemStore()
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return mem, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	created := seedPatchFenceSession(t, result.Store, "s-require")
	front := NewStore(beads.SessionStore{Store: result.Store})
	snapshot, err := front.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	mem.DisableConditionalWrites = true

	applied, err := front.ApplyPatchIfLifecycleUnchanged(snapshot, MetadataPatch{"state": string(StateAwake)})
	if !beads.IsConditionalWritesRequired(err) || applied {
		t.Fatalf("ApplyPatchIfLifecycleUnchanged = (%v, %v), want the require-mode refusal", applied, err)
	}
	got, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != string(StateActive) {
		t.Fatalf("state = %q, want the row untouched", got.Metadata["state"])
	}
}
