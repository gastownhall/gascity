package beads

import (
	"errors"
	"testing"
	"time"
)

// mc-03lk4: an event carries no revision, so an unverified bead.updated
// merged onto a cached row pairs its fields with the cached revision. One
// older than the scan that installed that revision rolls the row back while
// keeping the current revision, and a decide-then-CAS at it overwrites the
// newer write at the store.

// staleEventBackings are the in-process stores with a revision CAS. The fix
// verifies a conflicting event against each one's point read.
var staleEventBackings = []struct {
	name string
	open func(t *testing.T) Store
}{
	{"mem", func(*testing.T) Store { return NewMemStore() }},
	{"sqlite", func(t *testing.T) Store {
		s, err := OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		t.Cleanup(func() { _ = s.(*SQLiteStore).CloseStore() })
		return s
	}},
	{"native-dolt", func(*testing.T) Store { return newNativeDoltStoreForTest(newNativeDoltMemStorage()) }},
}

// staleEventAfterRescan writes held_until=old then =new around the cache, as
// another process would, lets a rescan install the newer row, and then
// delivers the older write's event. It returns the cache, the row id and the
// newer backing row.
func staleEventAfterRescan(t *testing.T, backing Store) (*CachingStore, string, Bead) {
	t.Helper()
	row, err := backing.Create(Bead{Title: "held"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := newConditionalCacheForTest(t, backing)
	older := writeHeldUntil(t, backing, row.ID, "old")
	newer := writeHeldUntil(t, backing, row.ID, "new")
	cache.ReconcileNowForTest()
	cache.mu.RLock()
	installed := cache.beads[row.ID]
	_, mutated := cache.beadSeq[row.ID]
	cache.mu.RUnlock()
	if installed.Metadata["held_until"] != "new" || installed.Revision != newer.Revision || mutated {
		t.Fatalf("the rescan left %v at revision %d (mutated=%v), want the clean newer row at %d; the path is vacuous",
			installed.Metadata, installed.Revision, mutated, newer.Revision)
	}
	payload, err := EncodeBeadEventPayload(older)
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	cache.ApplyEvent("bead.updated", payload)
	return cache, row.ID, newer
}

func writeHeldUntil(t *testing.T, backing Store, id, value string) Bead {
	t.Helper()
	return writeMetadata(t, backing, id, "held_until", value)
}

// writeMetadata sets key on id around the cache, as another process would,
// and returns the backing row after it.
func writeMetadata(t *testing.T, backing Store, id, key, value string) Bead {
	t.Helper()
	if err := backing.SetMetadata(id, key, value); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	b, err := backing.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return b
}

// An event reordered after a rescan neither regresses the cached row's fields
// nor pairs them with its revision, and the row, whose backing read equals the
// cached row, goes dirty for a later read to settle. Kills the base code, and
// (by the dirty mark only: the install-time drop alone keeps the fields and
// revision) a clean row's update left unverified at read time.
func TestCachingStoreStaleEventAfterRescanKeepsRowAndRevision(t *testing.T) {
	t.Parallel()
	for _, b := range staleEventBackings {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			cache, id, newer := staleEventAfterRescan(t, b.open(t))
			cache.mu.RLock()
			got := cache.beads[id]
			_, dirty := cache.dirty[id]
			cache.mu.RUnlock()
			if got.Metadata["held_until"] != "new" || got.Revision != newer.Revision {
				t.Fatalf("cached row = held_until %q at revision %d, want %q at %d",
					got.Metadata["held_until"], got.Revision, "new", newer.Revision)
			}
			if !dirty {
				t.Fatal("a stale event whose backing read equals the cached row left it clean")
			}
		})
	}
}

// A decide-then-CAS after the stale event either decides on the fresh row or
// is refused at the store; the newer write stands. Kills the same mutation as
// above, observed where it does harm: the heal of the expired hold lands.
func TestCachingStoreCASAfterStaleEventRefusesOrDecidesFresh(t *testing.T) {
	t.Parallel()
	for _, b := range staleEventBackings {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			backing := b.open(t)
			cache, id, _ := staleEventAfterRescan(t, backing)
			read, err := cache.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			// The decision: clear an expired ("old") hold.
			if read.Metadata["held_until"] == "old" {
				err := cache.UpdateIfMatch(id, read.Revision, UpdateOpts{Metadata: map[string]string{"held_until": "cleared"}})
				var pfe *PreconditionFailedError
				if !errors.As(err, &pfe) {
					t.Fatalf("a CAS decided on the stale row returned %v, want a precondition failure", err)
				}
			}
			stored, err := backing.Get(id)
			if err != nil {
				t.Fatalf("backing Get: %v", err)
			}
			if stored.Metadata["held_until"] != "new" {
				t.Fatalf("backing held_until = %q, want the newer write's %q", stored.Metadata["held_until"], "new")
			}
		})
	}
}

// A verified event whose row was reinstalled with equal fields at a newer
// revision between the verify and the install (an ABA) is dropped: merged,
// it would pair its fields with a revision whose row differs. Kills: the
// revision dropped from changedSinceVerify.
func TestCachingStoreVerifiedEventDroppedAfterSameFieldsNewRevision(t *testing.T) {
	t.Parallel()
	backing := NewMemStore()
	row, err := backing.Create(Bead{Title: "held", Metadata: map[string]string{"held_until": "x"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := newConditionalCacheForTest(t, backing)
	payload, err := EncodeBeadEventPayload(writeHeldUntil(t, backing, row.ID, "y"))
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	var reverted Bead
	cache.applyEventBeforeCommitForTest = func() {
		cache.applyEventBeforeCommitForTest = nil
		reverted = writeHeldUntil(t, backing, row.ID, "x")
		cache.mu.Lock()
		cache.markDirtyLocked(row.ID)
		cache.mu.Unlock()
		if _, err := cache.Get(row.ID); err != nil {
			t.Errorf("Get: %v", err)
		}
	}
	cache.ApplyEvent("bead.updated", payload)
	if reverted.Revision == 0 {
		t.Fatal("the event never reached its install; the path is vacuous")
	}
	cache.mu.RLock()
	got := cache.beads[row.ID]
	cache.mu.RUnlock()
	if got.Metadata["held_until"] != "x" || got.Revision != reverted.Revision {
		t.Fatalf("cached row = held_until %q at revision %d, want %q at %d",
			got.Metadata["held_until"], got.Revision, "x", reverted.Revision)
	}
}

// An event that matched the cached row when read, and conflicts only with a
// newer row a rescan installed before its install, is dropped unverified.
// Kills: the install-time check confined to locally mutated rows.
func TestCachingStoreEventConflictingOnlyAtInstallDropped(t *testing.T) {
	t.Parallel()
	backing := NewMemStore()
	row, err := backing.Create(Bead{Title: "held"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := newConditionalCacheForTest(t, backing)
	payload, err := EncodeBeadEventPayload(writeHeldUntil(t, backing, row.ID, "y"))
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	cache.ReconcileNowForTest()
	var newer Bead
	cache.applyEventBeforeCommitForTest = func() {
		cache.applyEventBeforeCommitForTest = nil
		newer = writeHeldUntil(t, backing, row.ID, "z")
		cache.ReconcileNowForTest()
	}
	cache.ApplyEvent("bead.updated", payload)
	if newer.Revision == 0 {
		t.Fatal("the event never reached its install; the path is vacuous")
	}
	cache.mu.RLock()
	got := cache.beads[row.ID]
	cache.mu.RUnlock()
	if got.Metadata["held_until"] != "z" || got.Revision != newer.Revision {
		t.Fatalf("cached row = held_until %q at revision %d, want %q at %d",
			got.Metadata["held_until"], got.Revision, "z", newer.Revision)
	}
}

// rowFences reads id's cached row, dirty mark and beadSeq stamp.
func rowFences(cache *CachingStore, id string) (row Bead, dirty bool, seq uint64) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	_, dirty = cache.dirty[id]
	return cloneBead(cache.beads[id]), dirty, cache.beadSeq[id]
}

// A field-conflicting bead.created or bead.deleted on a clean row keeps its
// unverified path: a created on a held row reads nothing and changes nothing,
// and a deleted tombstones the row. Kills (M7): the verification widened to
// every event type, which reads the backing for both and drops the delete.
func TestCachingStoreFieldConflictingCreatedAndDeletedKeepTheirPaths(t *testing.T) {
	t.Parallel()
	t.Run("created", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		row, err := backing.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		cache := newConditionalCacheForTest(t, backing)
		stale := eventPayload(t, row)
		title := "renamed"
		if err := backing.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		cache.ReconcileNowForTest()
		reads := backing.getCalls
		cache.ApplyEvent("bead.created", stale)
		got, dirty, _ := rowFences(cache, row.ID)
		if backing.getCalls != reads || dirty || got.Title != title {
			t.Fatalf("after a stale bead.created: backing reads %d, dirty %v, title %q; want 0, clean, %q",
				backing.getCalls-reads, dirty, got.Title, title)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		row, err := backing.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		cache := newConditionalCacheForTest(t, backing)
		if err := backing.Delete(row.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		row.Title = "renamed before the delete"
		reads := backing.getCalls
		cache.ApplyEvent("bead.deleted", eventPayload(t, row))
		cache.mu.RLock()
		_, tombstoned := cache.deletedSeq[row.ID]
		cache.mu.RUnlock()
		if !tombstoned || backing.getCalls != reads {
			t.Fatalf("after a field-conflicting bead.deleted: tombstoned %v, backing reads %d; want true, 0",
				tombstoned, backing.getCalls-reads)
		}
	})
}

// snapshotListStore lists a fixed snapshot while one is set: a scan whose
// listing predates a write it merges after.
type snapshotListStore struct {
	*MemStore
	snapshot []Bead
}

func (s *snapshotListStore) List(query ListQuery) ([]Bead, error) {
	if s.snapshot == nil {
		return s.MemStore.List(query)
	}
	rows := make([]Bead, 0, len(s.snapshot))
	for _, b := range s.snapshot {
		rows = append(rows, cloneBead(b))
	}
	return ApplyListQuery(rows, query), nil
}

// A scan that merges a listing older than a verified event between its check
// and its install leaves the older row; the event is settled against it, so
// the row goes dirty and the next read returns the event's state. Kills: a
// bare drop there, which leaves the older row clean until the next scan.
func TestCachingStoreVerifiedEventSettledAgainstAScanInsideItsWindow(t *testing.T) {
	t.Parallel()
	backing := &snapshotListStore{MemStore: NewMemStore()}
	row, err := backing.Create(Bead{Title: "s0"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := newConditionalCacheForTest(t, backing)
	write := func(title string) Bead {
		if err := backing.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		b, err := backing.Get(row.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return b
	}
	older := write("s1")
	payload, err := EncodeBeadEventPayload(write("s2"))
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	cache.applyEventBeforeCommitForTest = func() {
		cache.applyEventBeforeCommitForTest = nil
		backing.snapshot = []Bead{older}
		cache.ReconcileNowForTest()
		backing.snapshot = nil
	}
	cache.ApplyEvent("bead.updated", payload)
	if mid, _, _ := rowFences(cache, row.ID); mid.Title != "s1" {
		t.Fatalf("the scan left title %q, want the older listing's s1; the race is vacuous", mid.Title)
	}
	if _, dirty, _ := rowFences(cache, row.ID); !dirty {
		t.Fatal("the verified event lost to an older scan left the row clean")
	}
	got, err := cache.Get(row.ID)
	if err != nil || got.Title != "s2" {
		t.Fatalf("Get = %q, %v; want the event's s2", got.Title, err)
	}
}

// Superseded events on the shared SQLite shape: a gc.outcome write's event
// delivered after the close that followed it installs the closed backing row,
// stamped, and the row stays clean, so the census keeps serving. Kills: an
// unconfirmed event that always dirties the row.
func TestCachingStoreSupersededEventInstallsTheBackingRow(t *testing.T) {
	t.Parallel()
	engine := staleEventBackings[1].open(t)
	row, err := engine.Create(Bead{Title: "step"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := newConditionalCacheForTest(t, engine)
	_, _, before := rowFences(cache, row.ID)
	outcome, err := EncodeBeadEventPayload(writeMetadata(t, engine, row.ID, "gc.outcome", "pass"))
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	if err := engine.Close(row.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := engine.Get(row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	cache.ApplyEvent("bead.updated", outcome)
	got, dirty, seq := rowFences(cache, row.ID)
	if dirty || got.Status != "closed" || got.Revision != closed.Revision || got.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("after a superseded event: dirty %v, status %q, revision %d, gc.outcome %q; want clean, closed, %d, pass",
			dirty, got.Status, got.Revision, got.Metadata["gc.outcome"], closed.Revision)
	}
	if seq <= before {
		t.Fatalf("the install left beadSeq %d, not stamped past %d", seq, before)
	}
	if _, ok := cache.CachedList(ListQuery{Status: "open"}); !ok {
		t.Fatal("the census declined after a superseded event")
	}
}

// blockingSubprocessStore is a backing whose point read forks a process and,
// while block is set, does not answer until it is closed.
type blockingSubprocessStore struct {
	*MemStore
	block chan struct{}
}

func (s *blockingSubprocessStore) readsBySubprocess() bool { return true }

func (s *blockingSubprocessStore) Get(id string) (Bead, error) {
	if s.block != nil {
		<-s.block
	}
	return s.MemStore.Get(id)
}

// A clean row whose event check fails, or on a subprocess backing outlasts
// its deadline, is marked dirty and stamped, so no older scan clears the
// mark. Kills (M6): the stamp dropped from that path, and a check that waits
// on a stalled bd.
func TestCachingStoreUncheckableEventDirtiesAndStamps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (Store, func(*CachingStore), func())
	}{
		{"error", func(*testing.T) (Store, func(*CachingStore), func()) {
			backing := &casBackingStore{Store: NewMemStore()}
			return backing, func(*CachingStore) { backing.failNextGet = true }, func() {}
		}},
		{"deadline", func(*testing.T) (Store, func(*CachingStore), func()) {
			backing := &blockingSubprocessStore{MemStore: NewMemStore()}
			block := make(chan struct{})
			arm := func(cache *CachingStore) {
				backing.block = block
				cache.eventCheckAfter = func(time.Duration) <-chan time.Time {
					fired := make(chan time.Time)
					close(fired)
					return fired
				}
			}
			return backing, arm, func() { close(block) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing, arm, release := tc.setup(t)
			defer release()
			row, err := backing.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			cache := newConditionalCacheForTest(t, backing)
			_, _, before := rowFences(cache, row.ID)
			arm(cache)
			row.Title = "renamed elsewhere"
			cache.ApplyEvent("bead.updated", eventPayload(t, row))
			got, dirty, seq := rowFences(cache, row.ID)
			if !dirty || seq <= before || got.Title != "seed" {
				t.Fatalf("after an uncheckable event: dirty %v, beadSeq %d (was %d), title %q; want dirty, stamped, seed",
					dirty, seq, before, got.Title)
			}
		})
	}
}

// An event identical to the cached row reads nothing and marks nothing: the
// bus's duplicates cost no backing read. Kills: a verification gated on
// anything wider than a conflict.
func TestCachingStoreIdenticalDuplicateEventReadsNothing(t *testing.T) {
	t.Parallel()
	backing := &casBackingStore{Store: NewMemStore()}
	row, err := backing.Create(Bead{Title: "seed", Metadata: map[string]string{"held_until": "x"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := newConditionalCacheForTest(t, backing)
	fresh, err := backing.Get(row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	payload, err := EncodeBeadEventPayload(fresh)
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	reads := backing.getCalls
	for range 3 {
		cache.ApplyEvent("bead.updated", payload)
	}
	if _, dirty, _ := rowFences(cache, row.ID); dirty || backing.getCalls != reads {
		t.Fatalf("duplicates: dirty %v, backing reads %d; want clean, 0", dirty, backing.getCalls-reads)
	}
}
