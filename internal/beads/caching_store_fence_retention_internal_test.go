package beads

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The tests in this file reproduce the watermark holes P1.10c closes. A row's
// write fences used to go when the row left the cache, and a raw bead.created
// patch could install an uncached row, so an in-flight write or a late event
// could reinstall a row deleted or closed since, or roll a newer write back,
// under a covering census. On a backing whose rows omit edges, installs whose
// DepList read failed or was skipped cleared a raced write's mark.

// isRetained reports whether the cache is retaining id's write fences.
func isRetained(cache *CachingStore, id string) bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	_, ok := cache.retainedAt[id]
	return ok
}

// TestCachingStoreReconcileEvictionKeepsInFlightFence races a second write and
// a reconcile into a write's refresh read. The reconcile evicts the closed row;
// the first write, whose read predates the second, must still be fenced rather
// than install and roll the second back.
func TestCachingStoreReconcileEvictionKeepsInFlightFence(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cache.Close(row.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var second CacheRevision
	backing.onGetOnce = func() {
		if err := cache.SetMetadata(row.ID, "k", "2"); err != nil {
			t.Errorf("second SetMetadata: %v", err)
		}
		second = cache.WriteRev(row.ID)
		cache.ReconcileNowForTest()
		cache.mu.RLock()
		_, held := cache.beads[row.ID]
		cache.mu.RUnlock()
		if held {
			t.Error("the reconcile kept the closed row; the race is vacuous")
		}
	}
	if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
		t.Fatalf("first SetMetadata: %v", err)
	}
	// Reopen with a failing refresh patches the cached row, so a census sees
	// whatever the first write left there.
	backing.failNextGet = true
	if err := cache.Reopen(row.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, second, cache.WriteRev(row.ID))
}

// TestCachingStoreReconcileSweepKeepsInFlightTombstone races a Delete and a
// reconcile into a write's refresh read: the orphan sweep must keep the
// tombstone, so the write does not reinstall the deleted row.
func TestCachingStoreReconcileSweepKeepsInFlightTombstone(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var deleted CacheRevision
	backing.onGetOnce = func() {
		if err := cache.Delete(row.ID); err != nil {
			t.Errorf("Delete: %v", err)
		}
		deleted = cache.WriteRev(row.ID)
		cache.ReconcileNowForTest()
		if !isRetained(cache, row.ID) {
			t.Error("the reconcile's sweep did not visit the tombstone; the race is vacuous")
		}
	}
	if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, deleted)
}

// TestCachingStoreCreatedEchoAfterCloseDrop delivers the cache's own delayed
// bead.created echo for a row it closed, after a reconcile dropped the closed
// row, when the event's backing read fails or the backing hides closed rows
// from Get. The raw patch must not reinstall the row.
func TestCachingStoreCreatedEchoAfterCloseDrop(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		arm  func(backing *casBackingStore)
	}{
		{"failed_read", func(backing *casBackingStore) { backing.failNextGet = true }},
		{"hide_closed", func(backing *casBackingStore) { backing.hideClosedFromGet = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			echo := eventPayload(t, row)
			dropClosedRowByReconcile(t, cache, row.ID)
			closed := cache.WriteRev(row.ID)
			tc.arm(backing)
			cache.ApplyEventSnapshot("bead.created", echo)
			if backing.failNextGet {
				t.Fatal("the event made no backing read; the failure is vacuous")
			}
			assertSettledCensusAgrees(t, cache, backing.Store, row.ID, closed)
		})
	}
}

// TestCachingStoreCreatedEchoAfterDeleteAndReconcile delivers the delayed
// bead.created echo of a row the cache deleted, after a reconcile swept the
// tombstone's row. The backing read is a plain miss; the raw patch must not
// reinstall the row.
func TestCachingStoreCreatedEchoAfterDeleteAndReconcile(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	echo := eventPayload(t, row)
	if err := cache.Delete(row.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deleted := cache.WriteRev(row.ID)
	cache.ReconcileNowForTest()
	cache.ApplyEventSnapshot("bead.created", echo)
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, deleted)
}

// TestCachingStoreRetainedWriteStampVerifiesLateEvent drops a written row from
// the cache, brings it back with a backing read, and delivers an event
// snapshotted before the write. The write's retained stamp must keep the
// conflicting event under backing verification, so it does not roll the write
// back under a covering census.
func TestCachingStoreRetainedWriteStampVerifiesLateEvent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		drop func(t *testing.T, cache *CachingStore)
	}{
		{"reconcile_evicts_closed", func(_ *testing.T, cache *CachingStore) {
			cache.ReconcileNowForTest()
		}},
		{"prime_drops_closed", func(t *testing.T, cache *CachingStore) {
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mem := NewMemStore()
			cache := newConditionalCacheForTest(t, &casBackingStore{Store: mem})
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := cache.SetMetadata(row.ID, "k", "0"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			before, err := mem.Get(row.ID)
			if err != nil {
				t.Fatalf("backing Get: %v", err)
			}
			stale := eventPayload(t, before)
			if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			written := cache.WriteRev(row.ID)
			// Closed out of band and past the recency window, but well within
			// recentWriteVerifyWindow, the row leaves the cache.
			if err := mem.Close(row.ID); err != nil {
				t.Fatalf("backing Close: %v", err)
			}
			ageLocalWriteBy(cache, row.ID, 10*time.Second)
			tc.drop(t, cache)
			cache.mu.RLock()
			_, held := cache.beads[row.ID]
			cache.mu.RUnlock()
			if held {
				t.Fatal("the row stayed cached; the drop is vacuous")
			}
			// Reopened out of band, a reconcile reads it back clean.
			if err := mem.Reopen(row.ID); err != nil {
				t.Fatalf("backing Reopen: %v", err)
			}
			cache.ReconcileNowForTest()
			cache.mu.RLock()
			_, held = cache.beads[row.ID]
			_, mutated := cache.beadSeq[row.ID]
			cache.mu.RUnlock()
			if !held || mutated {
				t.Fatalf("after the reread held=%v beadSeq=%v, want a clean unfenced row", held, mutated)
			}
			cache.ApplyEvent("bead.updated", stale)
			assertSettledCensusAgrees(t, cache, mem, row.ID, written)
		})
	}
}

// TestCachingStorePrunedFenceRefusesSpanningWrite lets a write's refresh read
// span a second write, a reconcile that evicts the closed row, and a reconcile
// that prunes the retained fences past the window. No per-row fence is left,
// so the fence floor must refuse the first write's install.
func TestCachingStorePrunedFenceRefusesSpanningWrite(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cache.Close(row.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var second CacheRevision
	backing.onGetOnce = func() {
		if err := cache.SetMetadata(row.ID, "k", "2"); err != nil {
			t.Errorf("second SetMetadata: %v", err)
		}
		second = cache.WriteRev(row.ID)
		cache.ReconcileNowForTest()
		ageLocalWrite(cache, row.ID)
		cache.ReconcileNowForTest()
		cache.mu.RLock()
		_, fenced := cache.writeSeq[row.ID]
		cache.mu.RUnlock()
		if fenced {
			t.Error("the reconcile did not prune the retained fence; the race is vacuous")
		}
	}
	if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
		t.Fatalf("first SetMetadata: %v", err)
	}
	backing.failNextGet = true
	if err := cache.Reopen(row.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, second, cache.WriteRev(row.ID))
}

// TestPruneRetainedFencesLocked pins the prune decision with an injected
// clock: a retention is pruned only once both it and the id's last write are
// older than recentWriteVerifyWindow, a pruned fence raises the fence floor,
// and a row back in the cache keeps its fences.
func TestPruneRetainedFencesLocked(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-2 * time.Hour)
	for _, tc := range []struct {
		name       string
		retainedAt time.Time
		writeAt    time.Time
		held       bool
		wantPruned bool
	}{
		{"at_the_window", now.Add(-recentWriteVerifyWindow), old, false, false},
		{"past_the_window", now.Add(-recentWriteVerifyWindow - time.Nanosecond), old, false, true},
		{"rewritten_within_the_window", old, now.Add(-time.Second), false, false},
		{"row_back_in_the_cache", old, old, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newPrimitiveTestStore()
			id := "gc-1"
			c.writeSeq = map[string]uint64{id: 7}
			c.writeAt = map[string]time.Time{id: tc.writeAt}
			c.deletedSeq[id] = 9
			c.retainedAt = map[string]time.Time{id: tc.retainedAt}
			c.fenceFloor = 3
			if tc.held {
				c.beads[id] = Bead{ID: id}
			}

			c.pruneRetainedFencesLocked(now)

			_, wrote := c.writeSeq[id]
			_, stamped := c.writeAt[id]
			_, deleted := c.deletedSeq[id]
			_, retained := c.retainedAt[id]
			if pruned := !wrote && !stamped && !deleted; pruned != tc.wantPruned || (wrote != stamped || wrote != deleted) {
				t.Fatalf("writeSeq=%v writeAt=%v deletedSeq=%v, want pruned=%v", wrote, stamped, deleted, tc.wantPruned)
			}
			wantFloor := uint64(3)
			if tc.wantPruned {
				wantFloor = 9
			}
			if c.fenceFloor != wantFloor {
				t.Fatalf("fence floor %d, want %d", c.fenceFloor, wantFloor)
			}
			if wantRetained := !tc.wantPruned && !tc.held; retained != wantRetained {
				t.Fatalf("retained=%v, want %v", retained, wantRetained)
			}
			if tc.wantPruned && (!c.writeFencedLocked(id, 8) || c.writeFencedLocked(id, 9)) {
				t.Fatal("the floor must refuse a start below the pruned fence and only those")
			}
		})
	}
}

// TestCachingStoreRetainedFencesStayBounded deletes many rows and lets a
// reconcile retain their fences: one window later, a prune leaves no fence,
// stamp or retention behind.
func TestCachingStoreRetainedFencesStayBounded(t *testing.T) {
	t.Parallel()

	const rows = 32
	cache := newConditionalCacheForTest(t, NewMemStore())
	for i := 0; i < rows; i++ {
		b, err := cache.Create(Bead{Title: "row"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := cache.Delete(b.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	cache.ReconcileNowForTest()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.retainedAt) != rows || len(cache.writeSeq) != rows || len(cache.deletedSeq) != rows {
		t.Fatalf("retained %d, writeSeq %d, deletedSeq %d after the reconcile, want %d each",
			len(cache.retainedAt), len(cache.writeSeq), len(cache.deletedSeq), rows)
	}
	var since time.Time
	for _, at := range cache.retainedAt {
		if at.After(since) {
			since = at
		}
	}
	cache.pruneRetainedFencesLocked(since.Add(recentWriteVerifyWindow))
	if len(cache.retainedAt) != rows {
		t.Fatalf("%d of %d retentions left at the window, want none pruned yet", len(cache.retainedAt), rows)
	}
	cache.pruneRetainedFencesLocked(since.Add(recentWriteVerifyWindow + time.Nanosecond))
	if n := len(cache.retainedAt) + len(cache.writeSeq) + len(cache.writeAt) + len(cache.deletedSeq); n != 0 {
		t.Fatalf("%d fence entries left past the window, want none", n)
	}
	if cache.fenceFloor != cache.mutationSeq {
		t.Fatalf("fence floor %d after pruning every fence, want the last one at %d", cache.fenceFloor, cache.mutationSeq)
	}
}

// listStripStore strips edges from List rows too, like a backing whose rows
// omit them, without declaring completeness. A List leaves out the row named
// hide, so a list refreshes it through a point read instead.
type listStripStore struct {
	*depListFailStore
	hide string
}

func (s *listStripStore) List(q ListQuery) ([]Bead, error) {
	rows, err := s.depListFailStore.List(q)
	kept := rows[:0]
	for _, row := range rows {
		if row.ID == s.hide {
			continue
		}
		row.Dependencies, row.Needs = nil, nil
		kept = append(kept, row)
	}
	return kept, err
}

// newEdgeOmittingCache returns a cache over a backing whose point reads and
// lists omit edges, and the hooks a test arms on it.
func newEdgeOmittingCache(t *testing.T) (*CachingStore, *listStripStore, *writeRaceStore) {
	t.Helper()
	inner := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}}
	backing := &listStripStore{depListFailStore: &depListFailStore{writeRaceStore: inner}}
	return newConditionalCacheForTest(t, backing), backing, inner
}

// racedDepAdd adds an edge from id to other while a SetMetadata to id races
// into the write, which leaves id dirty over its pre-write edge set. It
// returns the DepAdd's write revision.
func racedDepAdd(t *testing.T, cache *CachingStore, inner *writeRaceStore, id, other string) CacheRevision {
	t.Helper()
	inner.beforeWrite = func() {
		if err := cache.SetMetadata(id, "k", "1"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
	}
	if err := cache.DepAdd(id, other, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if !isDirty(cache, id) {
		t.Fatal("the DepAdd was not fenced; the race is vacuous")
	}
	return cache.WriteRev(id)
}

// TestCachingStoreLiveListKeepsRacedDepMark settles a fenced DepAdd with a
// Live or Parent list, on a backing whose rows omit edges, through each of the
// list's installs: the listed row, and the point read of a cached row the list
// left out. A list reads no DepList, so no install may clear the mark over the
// pre-write edge set.
func TestCachingStoreLiveListKeepsRacedDepMark(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// hide leaves the row out of the List, so the list refreshes it
		// through a point read: the parent or live-missing refresh.
		hide   bool
		parent bool
	}{
		{"listed", false, false},
		{"parent_missing", true, true},
		{"live_missing", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache, backing, inner := newEdgeOmittingCache(t)
			parent, err := cache.Create(Bead{Title: "parent"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			seed := Bead{Title: "seed"}
			if tc.parent {
				seed.ParentID = parent.ID
			}
			row, err := cache.Create(seed)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			other, err := cache.Create(Bead{Title: "other"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			rev := racedDepAdd(t, cache, inner, row.ID, other.ID)
			ageLocalWrite(cache, row.ID)
			query := ListQuery{AllowScan: true, Live: true}
			if tc.parent {
				query = ListQuery{ParentID: parent.ID}
			}
			if tc.hide {
				backing.hide = row.ID
			}
			getsBefore := inner.getCalls
			if _, err := cache.List(query); err != nil {
				t.Fatalf("List: %v", err)
			}
			if tc.hide && inner.getCalls == getsBefore {
				t.Fatal("the list made no point read of the hidden row; the refresh is vacuous")
			}
			assertSettledCensusAgrees(t, cache, inner.Store, row.ID, rev)
		})
	}
}

// TestCachingStoreReopenDepListFailureKeepsMark reopens a row a raced DepAdd
// left dirty, with the refresh's DepList failing on a backing whose point read
// omits edges. The refresh failed, so Reopen must leave the mark.
func TestCachingStoreReopenDepListFailureKeepsMark(t *testing.T) {
	t.Parallel()

	inner := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}}
	backing := &depListFailStore{writeRaceStore: inner}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := cache.Create(Bead{Title: "other"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cache.Close(row.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	inner.beforeWrite = func() {
		if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
	}
	if err := cache.DepAdd(row.ID, other.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the DepAdd was not fenced; the race is vacuous")
	}
	backing.failNextDepList = true
	if err := cache.Reopen(row.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if backing.failNextDepList {
		t.Fatal("Reopen read no DepList; the failure is vacuous")
	}
	assertSettledCensusAgrees(t, cache, inner.Store, row.ID, cache.WriteRev(row.ID))
}

// TestCachingStoreDepAddRefreshDepListFailure fails DepAdd's own refresh
// DepList on a backing whose point read omits edges: the field-derived edge
// set is empty, so the refresh must count as failed rather than install it.
func TestCachingStoreDepAddRefreshDepListFailure(t *testing.T) {
	t.Parallel()

	inner := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}}
	backing := &depListFailStore{writeRaceStore: inner}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := cache.Create(Bead{Title: "other"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.failNextDepList = true
	if err := cache.DepAdd(row.ID, other.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if backing.failNextDepList {
		t.Fatal("DepAdd read no DepList; the failure is vacuous")
	}
	assertSettledCensusAgrees(t, cache, inner.Store, row.ID, cache.WriteRev(row.ID))
}

// TestCachingStoreEdgeOmittingInstallKeepsRacedDepMark settles a fenced
// DepAdd, on a backing whose rows omit edges, through an install that reads no
// edges: a successful CAS's refetch, and a reconcile or full Prime whose
// dependency read failed. Each installs the cached or field-derived edge set,
// which predates the DepAdd, so it must leave the mark rather than let a
// census cover the DepAdd while showing the row without its edge.
func TestCachingStoreEdgeOmittingInstallKeepsRacedDepMark(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		settle func(t *testing.T, cache *CachingStore, backing *listStripStore, id string)
	}{
		{"conditional_install", func(t *testing.T, cache *CachingStore, _ *listStripStore, id string) {
			swapped, err := cache.CompareAndSetMetadataKey(id, "c", "", "v")
			if err != nil || !swapped {
				t.Fatalf("CompareAndSetMetadataKey = %v, %v; want a successful swap", swapped, err)
			}
		}},
		{"reconcile_dep_read_failed", func(t *testing.T, cache *CachingStore, backing *listStripStore, _ string) {
			backing.failNextDepList = true
			cache.ReconcileNowForTest()
			if backing.failNextDepList {
				t.Fatal("the reconcile read no DepList; the failure is vacuous")
			}
		}},
		{"prime_dep_read_failed", func(t *testing.T, cache *CachingStore, backing *listStripStore, _ string) {
			backing.failNextDepList = true
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if backing.failNextDepList {
				t.Fatal("the Prime read no DepList; the failure is vacuous")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache, backing, inner := newEdgeOmittingCache(t)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			other, err := cache.Create(Bead{Title: "other"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			rev := racedDepAdd(t, cache, inner, row.ID, other.ID)
			ageLocalWrite(cache, row.ID)
			tc.settle(t, cache, backing, row.ID)
			cache.mu.RLock()
			_, held := cache.beads[row.ID]
			cache.mu.RUnlock()
			if !held {
				t.Fatal("the settling path installed no row; the install is vacuous")
			}
			assertSettledCensusAgrees(t, cache, inner.Store, row.ID, rev, cache.WriteRev(row.ID))
		})
	}
}

// TestCachingStoreEventTombstoneRestartsRetention delivers a fresh bead.deleted
// for an id whose fences have been retained for more than the window, while an
// unrelated write is in flight. The new tombstone must restart the retention:
// pruning it at once would raise the fence floor over the in-flight write and
// refuse its install.
func TestCachingStoreEventTombstoneRestartsRetention(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	gone, err := cache.Create(Bead{Title: "gone"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row, err := cache.Create(Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropClosedRowByReconcile(t, cache, gone.ID)
	ageLocalWriteBy(cache, gone.ID, 2*recentWriteVerifyWindow)
	if !isRetained(cache, gone.ID) {
		t.Fatal("the dropped row's fences are not retained; the state is vacuous")
	}
	backing.onGetOnce = func() {
		if err := backing.Delete(gone.ID); err != nil {
			t.Errorf("backing Delete: %v", err)
		}
		cache.ApplyEvent("bead.deleted", eventPayload(t, gone))
		cache.mu.RLock()
		_, tombstoned := cache.deletedSeq[gone.ID]
		cache.mu.RUnlock()
		if !tombstoned {
			t.Error("the event did not tombstone the id; the floor push is vacuous")
		}
		cache.ReconcileNowForTest()
	}
	if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if isDirty(cache, row.ID) {
		t.Fatal("the unrelated write was refused as raced")
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
}

// TestCachingStoreReopenUncachedDepListFailureNotifies reopens a row a
// reconcile dropped, with the refresh's DepList failing on a backing whose
// point read omits edges. The refresh failed, so the row stays marked, but the
// backing read found it, so the reopen must still notify.
func TestCachingStoreReopenUncachedDepListFailureNotifies(t *testing.T) {
	t.Parallel()

	inner := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}}
	backing := &depListFailStore{writeRaceStore: inner}
	cache, drain := newNotingCache(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropClosedRowByReconcile(t, cache, row.ID)
	drain()
	backing.failNextDepList = true
	if err := cache.Reopen(row.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if backing.failNextDepList {
		t.Fatal("Reopen read no DepList; the failure is vacuous")
	}
	if !hasNote(drain(), "bead.updated", row.ID) {
		t.Fatal("the reopen of an uncached row emitted no bead.updated")
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the reopened row is not marked")
	}
	assertSettledCensusAgrees(t, cache, inner.Store, row.ID, cache.WriteRev(row.ID))
}

// TestCachingStoreGraphApplyDepListFailureNotifies fails graph apply's refresh
// DepList on a backing whose point read omits edges. The row exists, so it
// must notify bead.created and stay marked, not be reported missing.
func TestCachingStoreGraphApplyDepListFailureNotifies(t *testing.T) {
	t.Parallel()

	inner := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}}
	backing := &depListFailStore{writeRaceStore: inner}
	cache, drain := newNotingCache(t, &storageGraphApplyRecordingStore{Store: backing})
	applier, ok := cache.GraphApplyHandle()
	if !ok {
		t.Fatal("GraphApplyHandle unavailable")
	}
	backing.failNextDepList = true
	result, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "g", Title: "g"}}})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if backing.failNextDepList {
		t.Fatal("graph apply read no DepList; the failure is vacuous")
	}
	id := result.IDs["g"]
	if !hasNote(drain(), "bead.created", id) {
		t.Fatalf("graph apply emitted no bead.created for %s", id)
	}
	if problem := cache.Stats().LastProblem; strings.Contains(problem, ErrNotFound.Error()) {
		t.Fatalf("graph apply reported an existing row missing: %s", problem)
	}
	if !isDirty(cache, id) {
		t.Fatalf("row %s is clean after its edges went unread", id)
	}
	assertSettledCensusAgrees(t, cache, inner.Store, id, cache.WriteRev(id))
}

// hasNote reports whether notes hold a typ notification for id.
func hasNote(notes []note, typ, id string) bool {
	for _, n := range notes {
		if n.typ == typ && n.id == id {
			return true
		}
	}
	return false
}
