package session

import (
	"context"
	"errors"
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
		if err := beads.StampOpenedStore(s, "SQLiteStore", mode, false, nil, nil); err != nil {
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

// TestUpdateMetadataFencedRedecidesOverAConcurrentSuspend proves the fenced
// write re-reads and re-decides when a suspend lands between its read and its
// write, on stores that can fence; legacy stores keep the unfenced write.
func TestUpdateMetadataFencedRedecidesOverAConcurrentSuspend(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPatchFenceSession(t, store, "s-fenced")
			front := NewStore(beads.SessionStore{Store: store})
			decisions := 0

			written, err := front.UpdateMetadataFenced(created.ID, 3, func(current Info, _ PersistedResponse) MetadataPatch {
				decisions++
				if decisions == 1 {
					if err := backing.SetMetadataBatch(created.ID, suspendPatch); err != nil {
						t.Fatalf("suspend write: %v", err)
					}
				}
				if current.MetadataState == string(StateSuspended) {
					return nil
				}
				return MetadataPatch{"state": string(StateAwake)}
			})
			if err != nil {
				t.Fatalf("UpdateMetadataFenced: %v", err)
			}
			if !backend.fenced {
				if !written {
					t.Fatal("legacy store: patch not written")
				}
				return
			}
			if written {
				t.Fatal("fenced store: stale patch written over the concurrent suspend")
			}
			assertSuspendSurvived(t, backing, created.ID)
		})
	}
}

// checkedLegacyStores opens one store shape that resolves no conditional
// writer: an unstamped MemStore, one stamped off, and one stamped auto over an
// incapable MemStore — the default topology's session store (#8313), where the
// pinned bd cannot fence a write.
func checkedLegacyStores(t *testing.T) map[string]func() *beads.MemStore {
	t.Helper()
	return map[string]func() *beads.MemStore{
		"an unstamped store": beads.NewMemStore,
		"a store stamped off": func() *beads.MemStore {
			mem := beads.NewMemStore()
			if err := beads.StampOpenedStore(mem, "MemStore", gate.Off, false, nil, nil); err != nil {
				t.Fatal(err)
			}
			return mem
		},
		"a store stamped auto with no conditional writer": func() *beads.MemStore {
			mem := beads.NewMemStore()
			mem.DisableConditionalWrites = true
			if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, false, nil, nil); err != nil {
				t.Fatal(err)
			}
			return mem
		},
	}
}

func checkedSessionRow(t *testing.T, store beads.Store, meta map[string]string) beads.Bead {
	t.Helper()
	b := beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive)}}
	for k, v := range meta {
		b.Metadata[k] = v
	}
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// TestUpdateMetadataChecked (AC-1 for ga-6eefx8, #8313): the checked write is
// the legacy fence's one admission. On a store with no conditional writer it
// writes the patch unfenced, but only when its caller answers that it is not a
// CAS-only writer — a missing answer, or a CAS-only caller, refuses wrapping
// ErrConditionalWriteUnsupported and nothing lands. A require store refuses
// by its typed error without consulting the caller's answer; a capable store
// takes the revision CAS without consulting it either. The answer is asked
// before the deciding read — it can be slow — so a write made inside it is
// visible to decide. A lost CAS re-decides up to its attempts, writes nothing
// past them, and leaves no open mint.
func TestUpdateMetadataChecked(t *testing.T) {
	for name, open := range checkedLegacyStores(t) {
		t.Run(name, func(t *testing.T) {
			mem := open()
			front := NewStore(beads.SessionStore{Store: mem})
			b := checkedSessionRow(t, mem, nil)

			// A caller that is not CAS-only gets the legacy write, unfenced.
			written, err := front.UpdateMetadataChecked(b.ID, 1, func(Info, PersistedResponse) MetadataPatch {
				return MetadataPatch{"state": string(StateAwake)}
			}, func() bool { return false })
			if err != nil || !written {
				t.Fatalf("the legacy write = %v, %v; want written, nil", written, err)
			}
			got, err := mem.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["state"] != string(StateAwake) {
				t.Fatalf("state = %q, want the patch landed unfenced", got.Metadata["state"])
			}

			// A missing answer is not the legacy answer: the store refuses,
			// and nothing lands.
			written, err = front.UpdateMetadataChecked(b.ID, 1, func(Info, PersistedResponse) MetadataPatch {
				return MetadataPatch{"state": "a2"}
			}, nil)
			if written || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
				t.Fatalf("without the caller's answer = %v, %v; want refused wrapping ErrConditionalWriteUnsupported", written, err)
			}

			// A CAS-only caller is refused the legacy write the same way.
			written, err = front.UpdateMetadataChecked(b.ID, 1, func(Info, PersistedResponse) MetadataPatch {
				return MetadataPatch{"state": "a3"}
			}, func() bool { return true })
			if written || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
				t.Fatalf("a CAS-only caller = %v, %v; want refused wrapping ErrConditionalWriteUnsupported", written, err)
			}
			if got, _ := mem.Get(b.ID); got.Metadata["state"] != string(StateAwake) {
				t.Fatalf("state = %q after the refusals; want the refused patches unwritten", got.Metadata["state"])
			}

			// The answer is asked before the deciding read: a write made
			// inside it is the row decide reads.
			asked := 0
			written, err = front.UpdateMetadataChecked(b.ID, 1, func(info Info, _ PersistedResponse) MetadataPatch {
				if info.MetadataState != string(StateSuspended) {
					t.Fatalf("decide read state = %q; want the write the answer made", info.MetadataState)
				}
				return MetadataPatch{"state": string(StateActive)}
			}, func() bool {
				asked++
				if err := mem.SetMetadataBatch(b.ID, map[string]string{"state": string(StateSuspended)}); err != nil {
					t.Fatal(err)
				}
				return false
			})
			if err != nil || !written || asked != 1 {
				t.Fatalf("the answered write = %v, %v (answer asked %d); want written, nil, asked once", written, err, asked)
			}

			// An empty patch writes nothing and reports so.
			written, err = front.UpdateMetadataChecked(b.ID, 1, func(Info, PersistedResponse) MetadataPatch {
				return nil
			}, func() bool { return false })
			if written || err != nil {
				t.Fatalf("an empty patch = %v, %v; want not written, nil", written, err)
			}
		})
	}

	// A require store refuses without consulting the caller's answer.
	mem := beads.NewMemStore()
	mem.DisableConditionalWrites = true
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	b := checkedSessionRow(t, mem, nil)
	consulted := false
	written, err := NewStore(beads.SessionStore{Store: mem}).UpdateMetadataChecked(b.ID, 1, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"state": string(StateAwake)}
	}, func() bool { consulted = true; return false })
	if written || !beads.IsConditionalWritesRequired(err) {
		t.Fatalf("a require store = %v, %v; want refused by the typed require error", written, err)
	}
	if consulted {
		t.Fatal("the caller's answer was consulted under require")
	}

	// A capable store takes the revision CAS without consulting the answer.
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	capable := checkedSessionRow(t, stamped, nil)
	rec := &rowWriteRecorder{Store: stamped}
	consulted = false
	written, err = NewStore(beads.SessionStore{Store: rec}).UpdateMetadataChecked(capable.ID, 1, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"state": string(StateAwake)}
	}, func() bool { consulted = true; return true })
	if err != nil || !written {
		t.Fatalf("a capable store = %v, %v; want written, nil", written, err)
	}
	if consulted {
		t.Fatal("the mode answer was consulted on a capable store")
	}
	if len(rec.writes) != 1 || rec.writes[0] != "UpdateIfMatch" {
		t.Fatalf("writes = %q, want exactly one revision CAS", rec.writes)
	}
	if got, _ := stamped.Get(capable.ID); got.Metadata["state"] != string(StateAwake) {
		t.Fatalf("state = %q, want the CAS patch landed", got.Metadata["state"])
	}

	// A row that is not a session row is not written.
	foreign, err := beads.NewMemStore().Create(beads.Bead{Title: "f", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if written, err := NewStore(beads.SessionStore{Store: beads.NewMemStore()}).UpdateMetadataChecked(foreign.ID, 1,
		func(Info, PersistedResponse) MetadataPatch { return MetadataPatch{"state": string(StateAwake)} },
		func() bool { return false }); written || err == nil {
		t.Fatalf("a non-session row = %v, %v; want refused", written, err)
	}

	// A CAS that always loses re-decides up to its attempts, writes nothing,
	// and leaves no open mint.
	stamped = stampedMemStore(t, gate.Auto, beads.NewMemStore())
	contended := checkedSessionRow(t, stamped, map[string]string{"wait_hold": "true"})
	before := OpenDispositionMints(contended.ID)
	loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
	decisions := 0
	written, err = NewStore(beads.SessionStore{Store: loser}).UpdateMetadataChecked(contended.ID, 2, func(info Info, _ PersistedResponse) MetadataPatch {
		decisions++
		to, err := DispositionOfInfo(info).Apply(ActorController, DispEvent{Kind: EvWaitEnd}, State(info.MetadataState), dispT0)
		if err != nil {
			t.Fatal(err)
		}
		return to.Changed().Into(info.ID, nil)
	}, nil)
	if written || err != nil {
		t.Fatalf("a CAS that always loses = %v, %v; want not written, nil", written, err)
	}
	if decisions != 2 {
		t.Fatalf("decisions = %d, want one per attempt", decisions)
	}
	if n := OpenDispositionMints(contended.ID) - before; n != 0 {
		t.Errorf("the lost attempts left %d open mints", n)
	}
}
