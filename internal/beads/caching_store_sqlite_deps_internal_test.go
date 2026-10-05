package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// depsFixture is one blocker and a step it blocks, in a backing under a primed
// cache whose notifications are recorded and can be fed back the way the
// controller's bead-event watcher feeds back its own (cache-reconcile) rows:
// as authoritative snapshots.
type depsFixture struct {
	engine  Store
	cache   *CachingStore
	pending []cacheWriteNotification
	blocker Bead
	step    Bead
}

// depsBackings are the two shapes that matter: the SQLite engine, whose rows
// carry their edges, and a MemStore, whose rows omit them (as the DoltLite
// read store's do) and so exercises the cache's own hardening.
var depsBackings = []struct {
	name string
	open func(t *testing.T) Store
}{
	{"sqlite", func(t *testing.T) Store {
		opened, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreIDPrefix("gcg"))
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		t.Cleanup(func() { _ = opened.(*SQLiteStore).CloseStore() })
		return opened
	}},
	{"rows-omit-edges", func(*testing.T) Store { return NewMemStore() }},
}

func newDepsFixture(t *testing.T, engine Store) *depsFixture {
	t.Helper()
	f := &depsFixture{engine: engine}
	var err error
	if f.blocker, err = engine.Create(Bead{Title: "blocker", Type: "task"}); err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	if f.step, err = engine.Create(Bead{Title: "step", Type: "task", Assignee: "worker-1"}); err != nil {
		t.Fatalf("Create step: %v", err)
	}
	if err := engine.DepAdd(f.step.ID, f.blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	f.cache = NewCachingStore(engine, func(_ ChangeSource, eventType, beadID, _, _, _ string, _ *[]string, payload json.RawMessage) {
		f.pending = append(f.pending, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := f.cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return f
}

// feedBack applies every recorded notification as the watcher applies a
// cache-reconcile row, and returns how many there were.
func (f *depsFixture) feedBack() int {
	notes := f.pending
	f.pending = nil
	for _, note := range notes {
		f.cache.ApplyEventSnapshot(note.eventType, note.payload)
	}
	return len(notes)
}

// assertStepBlocked checks the cache against the backing: the step's edge set
// matches, and cached readiness withholds the open step as the backing does.
func (f *depsFixture) assertStepBlocked(t *testing.T, when string) {
	t.Helper()
	want, err := f.engine.DepList(f.step.ID, "down")
	if err != nil {
		t.Fatalf("backing DepList: %v", err)
	}
	got, err := f.cache.cachedDepListOnly(f.step.ID, "down")
	if err != nil {
		t.Fatalf("%s: cached DepList: %v", when, err)
	}
	if !depSetEqual(want, got) {
		t.Fatalf("%s: cached edges %+v, backing edges %+v", when, got, want)
	}
	ready, err := f.cache.cachedReadyOnly(ReadyQuery{})
	if err != nil {
		t.Fatalf("%s: cached Ready: %v", when, err)
	}
	for _, b := range ready {
		if b.ID == f.step.ID {
			t.Fatalf("%s: cached Ready offers %s, which its blocker holds back", when, f.step.ID)
		}
	}
}

// Every write verb through the cache keeps the written row's edges, and so
// does feeding the cache's own notifications back. Kills: a write-through
// absorb that sources edges from a row that carries none (Get over the SQLite
// engine returned none; a MemStore's never does), a create that keeps "cached"
// edges a new row cannot have, a reopen after a CloseAll that had dropped
// them, and an authoritative snapshot without a dependencies key read as "no
// deps".
func TestCacheWriteVerbsKeepDependencyEdges(t *testing.T) {
	t.Parallel()
	for _, backing := range depsBackings {
		t.Run(backing.name, func(t *testing.T) {
			t.Parallel()
			f := newDepsFixture(t, backing.open(t))
			f.assertStepBlocked(t, "after prime")
			title := "renamed"
			inProgress := "in_progress"
			verbs := []struct {
				name  string
				write func() error
			}{
				{"Update", func() error { return f.cache.Update(f.step.ID, UpdateOpts{Title: &title}) }},
				{"SetMetadata", func() error { return f.cache.SetMetadata(f.step.ID, "k", "v") }},
				{"SetMetadataBatch", func() error { return f.cache.SetMetadataBatch(f.step.ID, map[string]string{"a": "1"}) }},
				{"Close+Reopen", func() error {
					if err := f.cache.Close(f.step.ID); err != nil {
						return err
					}
					return f.cache.Reopen(f.step.ID)
				}},
				{"CloseAll+Reopen", func() error {
					if _, err := f.cache.CloseAll([]string{f.step.ID}, nil); err != nil {
						return err
					}
					return f.cache.Reopen(f.step.ID)
				}},
				{"Tx", func() error {
					return f.cache.Tx("tx", func(tx Tx) error { return tx.SetMetadataBatch(f.step.ID, map[string]string{"tx": "1"}) })
				}},
				{"ReleaseIfCurrent", func() error {
					if err := f.cache.Update(f.step.ID, UpdateOpts{Status: &inProgress}); err != nil {
						return err
					}
					_, err := f.cache.ReleaseIfCurrent(f.step.ID, "worker-1")
					return err
				}},
				// A new blocked step created through the cache: its edges come
				// from the created row, and the checks below follow it.
				{"Create with Needs", func() error {
					created, err := f.cache.Create(Bead{Title: "new step", Type: "task", Needs: []string{f.blocker.ID}})
					if err == nil {
						f.step = created
					}
					return err
				}},
			}
			for _, verb := range verbs {
				if err := verb.write(); err != nil {
					t.Fatalf("%s: %v", verb.name, err)
				}
				f.assertStepBlocked(t, verb.name+" through the cache")
				f.feedBack()
				f.assertStepBlocked(t, verb.name+" after its events came back")
			}
		})
	}
}

// A converged cache must go quiet. Kills the re-emit loop: a re-scan emits a
// row whose snapshot omits its edges, the fed-back snapshot wipes the cached
// edges, and the next re-scan sees them "change" again, forever.
func TestCacheReScansGoQuietWithEventsFedBack(t *testing.T) {
	t.Parallel()
	for _, backing := range depsBackings {
		t.Run(backing.name, func(t *testing.T) {
			t.Parallel()
			f := newDepsFixture(t, backing.open(t))
			if err := f.cache.SetMetadata(f.step.ID, "k", "v"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			f.feedBack()
			var perPass []int
			for pass := 0; pass < 4; pass++ {
				f.cache.runReconciliation()
				perPass = append(perPass, f.feedBack())
			}
			for pass, n := range perPass[1:] {
				if n != 0 {
					t.Fatalf("re-scan %d emitted %d rows (per pass: %v); a converged cache must emit nothing", pass+2, n, perPass)
				}
			}
			f.assertStepBlocked(t, "after the re-scans")
		})
	}
}

// The SQLite engine's rows carry their edges from the deps table, the
// convention bd and the native Dolt store follow. Kills: Get or List answering
// from the create-time JSON, which omits edges added later and keeps edges
// removed since.
func TestSQLiteRowsCarryTheirDependencyEdges(t *testing.T) {
	t.Parallel()
	engine := depsBackings[0].open(t).(*SQLiteStore)
	f := newDepsFixture(t, engine)
	other, err := engine.Create(Bead{Title: "other", Type: "task", Needs: []string{f.blocker.ID}})
	if err != nil {
		t.Fatalf("Create with needs: %v", err)
	}
	if err := engine.DepRemove(other.ID, f.blocker.ID); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}
	want := map[string]int{f.blocker.ID: 0, f.step.ID: 1, other.ID: 0}
	for id, n := range want {
		got, err := engine.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if len(depsFromBeadFields(got)) != n {
			t.Fatalf("Get(%s) edges = %+v (needs %v), want %d", id, got.Dependencies, got.Needs, n)
		}
	}
	rows, err := engine.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, row := range rows {
		if len(depsFromBeadFields(row)) != want[row.ID] {
			t.Fatalf("List row %s edges = %+v (needs %v), want %d", row.ID, row.Dependencies, row.Needs, want[row.ID])
		}
	}
	ready, err := engine.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, row := range ready {
		if len(depsFromBeadFields(row)) != want[row.ID] {
			t.Fatalf("Ready row %s edges = %+v (needs %v), want %d", row.ID, row.Dependencies, row.Needs, want[row.ID])
		}
	}
}

// depListCountingSQLite counts per-row DepList calls; the embedded engine
// still declares complete-edge rows.
type depListCountingSQLite struct {
	*SQLiteStore
	depLists int
}

func (s *depListCountingSQLite) DepList(id, direction string) ([]Dep, error) {
	s.depLists++
	return s.SQLiteStore.DepList(id, direction)
}

// Kills: the SQLite engine not declaring complete-edge rows, which sends a
// prime and every re-scan back to one DepList per active row (no declaration)
// or leaves the cache's edge set incomplete (declared false).
func TestSQLiteCachePrimeReadsNoPerRowDepList(t *testing.T) {
	t.Parallel()
	counting := &depListCountingSQLite{SQLiteStore: depsBackings[0].open(t).(*SQLiteStore)}
	f := newDepsFixture(t, counting)
	f.cache.runReconciliation()
	if counting.depLists != 0 {
		t.Fatalf("prime and one re-scan issued %d per-row DepList calls, want 0", counting.depLists)
	}
	if !f.cache.depsComplete {
		t.Fatal("the cache over SQLite holds incomplete edges after prime; the engine's rows must declare theirs complete")
	}
	f.assertStepBlocked(t, "after prime and a re-scan")
}

// A backing that declares complete rows answers "no edges" with a row that
// carries none. Kills: an absorb that keeps a cached edge the store no longer
// has because the refreshed row carried no dependency fields.
func TestCacheAbsorbDropsAnEdgeRemovedBehindIt(t *testing.T) {
	t.Parallel()
	f := newDepsFixture(t, depsBackings[0].open(t))
	if err := f.engine.DepRemove(f.step.ID, f.blocker.ID); err != nil {
		t.Fatalf("DepRemove behind the cache: %v", err)
	}
	if err := f.cache.SetMetadata(f.step.ID, "k", "v"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	got, err := f.cache.cachedDepListOnly(f.step.ID, "down")
	if err != nil || len(got) != 0 {
		t.Fatalf("cached edges after the refresh = (%+v, %v), want none", got, err)
	}
}

// Kills: hydrating only the first chunk of ids, which leaves every row past
// the first 500 of a List edgeless.
func TestSQLiteListHydratesEdgesPastOneChunk(t *testing.T) {
	t.Parallel()
	engine := depsBackings[0].open(t).(*SQLiteStore)
	root, err := engine.Create(Bead{Title: "root", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const n = 2*sqliteDepsQueryChunk + 7
	err = engine.Tx("seed", func(tx Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Create(Bead{Title: "step", Type: "task", Needs: []string{root.ID}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	rows, err := engine.List(ListQuery{AllowScan: true, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i, row := range rows {
		if row.ID == root.ID {
			continue
		}
		if len(row.Dependencies) != 1 {
			t.Fatalf("row %d (%s) carries %d edges, want 1", i, row.ID, len(row.Dependencies))
		}
	}
}

// Rows the engine returns from inside a write carry their edges too. Kills: an
// in-transaction read without hydration, whose row a fenced close hands to the
// cache.
func TestSQLiteInTransactionReadsCarryEdges(t *testing.T) {
	t.Parallel()
	engine := depsBackings[0].open(t).(*SQLiteStore)
	f := newDepsFixture(t, engine)
	claimed, won, err := engine.Claim(f.step.ID, "worker-1")
	if err != nil || !won {
		t.Fatalf("Claim = (%v, %v)", won, err)
	}
	if len(claimed.Dependencies) != 1 {
		t.Fatalf("claimed row carries %+v, want its edge", claimed.Dependencies)
	}
	current, err := engine.Get(f.step.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	closed, err := engine.CloseWithMetadataIfMatch(f.step.ID, current.Revision, map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if len(closed.Dependencies) != 1 {
		t.Fatalf("closed row carries %+v, want its edge", closed.Dependencies)
	}
}

// A dirty row's refresh on Get installs the backing's current edges. Kills
// (M32): the dirty-refresh absorb keeping stale cached edges over the row's.
func TestCacheDirtyRefreshTakesTheRowsEdges(t *testing.T) {
	t.Parallel()
	f := newDepsFixture(t, depsBackings[0].open(t))
	other, err := f.engine.Create(Bead{Title: "second blocker", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.engine.DepAdd(f.step.ID, other.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd behind the cache: %v", err)
	}
	f.cache.mu.Lock()
	f.cache.markDirtyLocked(f.step.ID)
	f.cache.mu.Unlock()
	if _, err := f.cache.Get(f.step.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := f.cache.cachedDepListOnly(f.step.ID, "down")
	if err != nil || len(got) != 2 {
		t.Fatalf("cached edges after the dirty refresh = (%+v, %v), want both", got, err)
	}
}

// Kills: the edge read wrapping a canceled context's error, which hides the
// cause a caller classifies timeouts by.
func TestSQLiteEdgeReadReportsTheContextError(t *testing.T) {
	t.Parallel()
	engine := depsBackings[0].open(t).(*SQLiteStore)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sqliteDepsFor(ctx, engine.readDB, []string{"gcg-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("edge read with a canceled context = %v, want context.Canceled itself", err)
	}
}

// getFailsOnceStore fails the next Get of one id, as a transient backing error
// right after a write would.
type getFailsOnceStore struct {
	*MemStore
	failID string
}

func (s *getFailsOnceStore) Get(id string) (Bead, error) {
	if id == s.failID {
		s.failID = ""
		return Bead{}, errors.New("transient read failure")
	}
	return s.MemStore.Get(id)
}

// When the refresh after an Update fails, the cache patches its own row; that
// row's struct is not a backing read, so its (absent) dependency fields say
// nothing. Kills: the fallback sourcing edges from the patched struct, which
// wipes the row's edges until a later refresh.
func TestCacheUpdateRefreshFailureKeepsEdges(t *testing.T) {
	t.Parallel()
	backing := &getFailsOnceStore{MemStore: NewMemStore()}
	f := newDepsFixture(t, backing)
	backing.failID = f.step.ID
	title := "renamed"
	if err := f.cache.Update(f.step.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	f.cache.mu.RLock()
	deps := cloneDeps(f.cache.deps[f.step.ID])
	f.cache.mu.RUnlock()
	if len(deps) != 1 || deps[0].DependsOnID != f.blocker.ID {
		t.Fatalf("cached edges after a failed refresh = %+v, want the edge kept", deps)
	}
}
