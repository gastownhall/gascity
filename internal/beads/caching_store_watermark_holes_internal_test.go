package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file reproduce the watermark holes P1.10b closes: each
// drives one interleaving deterministically and requires that no admitted
// census whose CacheRev covers the writes disagrees with the backing. A census
// refused because a fenced row went dirty claims nothing, and a settling read
// must then admit one that agrees.

// admittedCensusAgrees reports whether an active census was admitted. When it
// was, the census must cover every write in revs and agree with truth on id:
// its presence, title, status, assignee, metadata key k and edges.
func admittedCensusAgrees(t *testing.T, cache *CachingStore, truth Store, id string, revs ...CacheRevision) bool {
	t.Helper()
	rows, observation, ok := cache.ObservedList(ListQuery{AllowScan: true})
	if !ok {
		return false
	}
	rev := observation.CacheRev()
	for _, w := range revs {
		if !coveredBy(w, rev) {
			t.Fatalf("clean census at %+v does not cover write %+v made before it", rev, w)
		}
	}
	want, err := truth.Get(id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("backing Get(%s): %v", id, err)
	}
	live := err == nil && want.Status != "closed"
	var got Bead
	found := false
	for _, row := range rows {
		if row.ID == id {
			got, found = row, true
		}
	}
	if found != live {
		t.Fatalf("clean census at %+v covering %+v has row %s present=%v, but the backing row is live=%v (%+v)", rev, revs, id, found, live, want)
	}
	if !live {
		return true
	}
	if got.Title != want.Title || got.Status != want.Status || got.Assignee != want.Assignee || got.Metadata["k"] != want.Metadata["k"] {
		t.Fatalf("clean census at %+v covering %+v shows %s as (title %q, status %q, assignee %q, k %q); the backing has (%q, %q, %q, %q)",
			rev, revs, id, got.Title, got.Status, got.Assignee, got.Metadata["k"], want.Title, want.Status, want.Assignee, want.Metadata["k"])
	}
	wantDeps, err := truth.DepList(id, "down")
	if err != nil {
		t.Fatalf("backing DepList(%s): %v", id, err)
	}
	cache.mu.RLock()
	gotDeps := depTargets(cache.deps[id])
	cache.mu.RUnlock()
	if !slices.Equal(gotDeps, depTargets(wantDeps)) {
		t.Fatalf("clean census at %+v covering %+v holds edges %v for %s; the backing has %v", rev, revs, gotDeps, id, depTargets(wantDeps))
	}
	return true
}

// assertSettledCensusAgrees requires the census to agree with truth on id, and
// after a settling read of id to be admitted.
func assertSettledCensusAgrees(t *testing.T, cache *CachingStore, truth Store, id string, revs ...CacheRevision) {
	t.Helper()
	admittedCensusAgrees(t, cache, truth, id, revs...)
	if _, err := cache.Get(id); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("settling Get(%s): %v", id, err)
	}
	if !admittedCensusAgrees(t, cache, truth, id, revs...) {
		t.Fatalf("census still refused after a settling read of %s", id)
	}
}

func depTargets(deps []Dep) []string {
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		out = append(out, d.DependsOnID)
	}
	slices.Sort(out)
	return out
}

// writeRaceStore runs beforeWrite once, right before the next backing write
// commits, and afterWrite once, right after the next backing write commits and
// before it returns to the cache.
type writeRaceStore struct {
	*casBackingStore
	beforeWrite func()
	afterWrite  func()
}

func (s *writeRaceStore) before() {
	if hook := s.beforeWrite; hook != nil {
		s.beforeWrite = nil
		hook()
	}
}

func (s *writeRaceStore) fire(err error) error {
	if hook := s.afterWrite; hook != nil && err == nil {
		s.afterWrite = nil
		hook()
	}
	return err
}

func (s *writeRaceStore) Update(id string, opts UpdateOpts) error {
	s.before()
	return s.fire(s.casBackingStore.Update(id, opts))
}

func (s *writeRaceStore) Close(id string) error {
	s.before()
	return s.fire(s.casBackingStore.Close(id))
}

func (s *writeRaceStore) Reopen(id string) error {
	s.before()
	return s.fire(s.casBackingStore.Reopen(id))
}

func (s *writeRaceStore) SetMetadata(id, key, value string) error {
	s.before()
	return s.fire(s.casBackingStore.SetMetadata(id, key, value))
}

func (s *writeRaceStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.before()
	return s.fire(s.casBackingStore.SetMetadataBatch(id, kvs))
}

func (s *writeRaceStore) DepAdd(issueID, dependsOnID, depType string) error {
	s.before()
	return s.fire(s.casBackingStore.DepAdd(issueID, dependsOnID, depType))
}

func (s *writeRaceStore) DepRemove(issueID, dependsOnID string) error {
	s.before()
	return s.fire(s.casBackingStore.DepRemove(issueID, dependsOnID))
}

func (s *writeRaceStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.before()
	n, err := s.casBackingStore.CloseAll(ids, metadata)
	return n, s.fire(err)
}

func (s *writeRaceStore) Tx(commitMsg string, fn func(Tx) error) error {
	s.before()
	return s.fire(s.casBackingStore.Tx(commitMsg, fn))
}

func (s *writeRaceStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	s.before()
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	return released, s.fire(err)
}

// TestCachingStoreUnconditionalWriteFencesRacingWrite races a second local
// write to the same row into each unconditional verb.
//
// In the commit window the racer commits and installs between the first
// write's backing commit and its return, so a startSeq taken after the commit
// already covers the racer; the first write's own change, laid over its
// refresh read (Update, Close, Reopen) or patched into the cached row when the
// refresh fails, would roll the racer back. Only a startSeq taken before the
// backing write fences it. (Update's and ReleaseIfCurrent's patch fallbacks
// already mark the row dirty, so they have no fallback case.)
//
// In the refresh window the racer commits and installs after the first write's
// refresh read, so installing that read would roll the racer back.
func TestCachingStoreUnconditionalWriteFencesRacingWrite(t *testing.T) {
	t.Parallel()

	a, b := "A", "B"
	inProgress := "in_progress"
	x, y := "x", "y"
	claim := func(t *testing.T, cache *CachingStore, id, _ string) {
		t.Helper()
		if err := cache.Update(id, UpdateOpts{Status: &inProgress, Assignee: &x}); err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	closeFirst := func(t *testing.T, cache *CachingStore, id, _ string) {
		t.Helper()
		if err := cache.Close(id); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	addEdge := func(t *testing.T, cache *CachingStore, id, other string) {
		t.Helper()
		if err := cache.DepAdd(id, other, "blocks"); err != nil {
			t.Fatalf("DepAdd: %v", err)
		}
	}
	type verb func(cache *CachingStore, id, other string) error
	update := func(title *string) verb {
		return func(cache *CachingStore, id, _ string) error { return cache.Update(id, UpdateOpts{Title: title}) }
	}
	setK := func(v string) verb {
		return func(cache *CachingStore, id, _ string) error { return cache.SetMetadata(id, "k", v) }
	}
	batchK := func(v string) verb {
		return func(cache *CachingStore, id, _ string) error {
			return cache.SetMetadataBatch(id, map[string]string{"k": v})
		}
	}
	closeVerb := func(cache *CachingStore, id, _ string) error { return cache.Close(id) }
	reopen := func(cache *CachingStore, id, _ string) error { return cache.Reopen(id) }
	closeAll := func(cache *CachingStore, id, _ string) error {
		_, err := cache.CloseAll([]string{id}, nil)
		return err
	}
	release := func(cache *CachingStore, id, _ string) error {
		released, err := cache.ReleaseIfCurrent(id, x)
		if err == nil && !released {
			err = errors.New("ReleaseIfCurrent released nothing")
		}
		return err
	}
	reclaim := func(cache *CachingStore, id, _ string) error {
		return cache.Update(id, UpdateOpts{Status: &inProgress, Assignee: &y})
	}
	txClose := func(cache *CachingStore, id, _ string) error {
		return cache.Tx("close", func(tx Tx) error { return tx.Close(id) })
	}
	txUpdate := func(cache *CachingStore, id, _ string) error {
		return cache.Tx("update", func(tx Tx) error { return tx.Update(id, UpdateOpts{Title: &a}) })
	}
	depAdd := func(cache *CachingStore, id, other string) error { return cache.DepAdd(id, other, "blocks") }
	depRemove := func(cache *CachingStore, id, other string) error { return cache.DepRemove(id, other) }

	for _, tc := range []struct {
		name        string
		refresh     bool // race in the refresh window instead of the commit window
		failRefresh bool // the first write's refresh read fails
		setup       func(t *testing.T, cache *CachingStore, id, other string)
		first       verb
		racer       verb
	}{
		{name: "update/overlay", first: update(&a), racer: update(&b)},
		{name: "close/overlay", first: closeVerb, racer: reopen},
		{name: "close/fallback", failRefresh: true, first: closeVerb, racer: reopen},
		{name: "reopen/overlay", setup: closeFirst, first: reopen, racer: closeAll},
		{name: "reopen/fallback", setup: closeFirst, failRefresh: true, first: reopen, racer: closeAll},
		{name: "set_metadata/fallback", failRefresh: true, first: setK("a"), racer: setK("b")},
		{name: "set_metadata_batch/fallback", failRefresh: true, first: batchK("a"), racer: batchK("b")},
		{name: "tx_close/fallback", failRefresh: true, first: txClose, racer: reopen},
		{name: "dep_add/fallback", failRefresh: true, first: depAdd, racer: depRemove},
		{name: "dep_remove/fallback", setup: addEdge, failRefresh: true, first: depRemove, racer: depAdd},

		{name: "update/refresh", refresh: true, first: update(&a), racer: update(&b)},
		{name: "close/refresh", refresh: true, first: closeVerb, racer: reopen},
		{name: "reopen/refresh", refresh: true, setup: closeFirst, first: reopen, racer: closeAll},
		{name: "set_metadata/refresh", refresh: true, first: setK("a"), racer: setK("b")},
		{name: "set_metadata_batch/refresh", refresh: true, first: batchK("a"), racer: batchK("b")},
		{name: "release_if_current/refresh", refresh: true, setup: claim, first: release, racer: reclaim},
		{name: "close_all/refresh", refresh: true, first: closeAll, racer: reopen},
		{name: "tx/refresh", refresh: true, first: txUpdate, racer: update(&b)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			other, err := cache.Create(Bead{Title: "other"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tc.setup != nil {
				tc.setup(t, cache, row.ID, other.ID)
			}

			var racerRev CacheRevision
			raced := false
			race := func() {
				raced = true
				if err := tc.racer(cache, row.ID, other.ID); err != nil {
					t.Errorf("racing write: %v", err)
				}
				racerRev = cache.WriteRev(row.ID)
				backing.failNextGet = tc.failRefresh
			}
			if tc.refresh {
				backing.onGetOnce = race
			} else {
				backing.afterWrite = race
			}
			if err := tc.first(cache, row.ID, other.ID); err != nil {
				t.Fatalf("first write: %v", err)
			}
			if !raced {
				t.Fatal("the racing write never ran; the interleaving is vacuous")
			}
			assertSettledCensusAgrees(t, cache, backing.Store, row.ID, racerRev, cache.WriteRev(row.ID))
		})
	}
}

// TestCachingStoreGraphApplyFencesRacingWrite races a local write into graph
// apply's refresh read of a row it created.
func TestCachingStoreGraphApplyFencesRacingWrite(t *testing.T) {
	t.Parallel()

	race := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, &storageGraphApplyRecordingStore{Store: race})
	applier, ok := cache.GraphApplyHandle()
	if !ok {
		t.Fatal("GraphApplyHandle unavailable")
	}
	title := "B"
	var createdID string
	var racerRev CacheRevision
	race.onGetOnce = func() {
		rows, err := race.Store.List(ListQuery{AllowScan: true})
		if err != nil || len(rows) != 1 {
			t.Errorf("backing rows = %d (%v), want the one created node", len(rows), err)
			return
		}
		createdID = rows[0].ID
		if err := cache.Update(createdID, UpdateOpts{Title: &title}); err != nil {
			t.Errorf("racing Update: %v", err)
		}
		racerRev = cache.WriteRev(createdID)
	}
	if _, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "g", Title: "g"}}}); err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if createdID == "" {
		t.Fatal("the racing write never ran; the interleaving is vacuous")
	}
	assertSettledCensusAgrees(t, cache, race.Store, createdID, racerRev, cache.WriteRev(createdID))
}

// TestCachingStoreDependencyWriteRefreshFailureKeepsStalenessMarks pins that a
// dependency write whose refresh failed patches its edge without clearing the
// row's dirty mark: the mark still stands for the rest of the row.
func TestCachingStoreDependencyWriteRefreshFailureKeepsStalenessMarks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		edged bool // the row starts with the edge the write removes
		write func(cache *CachingStore, id, other string) error
	}{
		{"dep_add", false, func(cache *CachingStore, id, other string) error { return cache.DepAdd(id, other, "blocks") }},
		{"dep_remove", true, func(cache *CachingStore, id, other string) error { return cache.DepRemove(id, other) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			other, err := cache.Create(Bead{Title: "other"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tc.edged {
				if err := cache.DepAdd(row.ID, other.ID, "blocks"); err != nil {
					t.Fatalf("DepAdd: %v", err)
				}
			}
			// A write around the cache, then its event conflicting with a
			// recent local write and failing verification, leaves the cached
			// row stale and dirty. The refresh clears the local write's
			// beadSeq, which routes the event to that verification.
			local, external := "local", "external"
			if err := cache.Update(row.ID, UpdateOpts{Title: &local}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			cache.mu.Lock()
			cache.markDirtyLocked(row.ID)
			cache.mu.Unlock()
			if _, err := cache.Get(row.ID); err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := backing.Update(row.ID, UpdateOpts{Title: &external}); err != nil {
				t.Fatalf("backing Update: %v", err)
			}
			backing.failNextGet = true
			cache.ApplyEvent("bead.updated", json.RawMessage(fmt.Sprintf(`{"id":%q,"title":%q}`, row.ID, external)))
			cache.mu.RLock()
			_, dirty := cache.dirty[row.ID]
			cache.mu.RUnlock()
			if !dirty {
				t.Fatal("the unverifiable event left the row clean; the setup is vacuous")
			}

			backing.failNextGet = true
			if err := tc.write(cache, row.ID, other.ID); err != nil {
				t.Fatalf("dependency write: %v", err)
			}
			assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
		})
	}
}

// TestCachingStoreApplyEventUncachedBranchFencesLocalWrites races a local
// write between an event's read phase and its install when the row is not
// cached at install time (F1): either the read phase found it uncached, or a
// conditional write evicted it after the read phase saw it, which leaves the
// event with nothing to merge onto but its raw payload.
func TestCachingStoreApplyEventUncachedBranchFencesLocalWrites(t *testing.T) {
	t.Parallel()

	title := "written"
	laggedUpdateIfMatch := func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
		pre, err := backing.Store.Get(id)
		if err != nil {
			t.Errorf("backing Get: %v", err)
			return
		}
		backing.staleNextGet = &pre
		if err := cache.UpdateIfMatch(id, pre.Revision, UpdateOpts{Title: &title}); err != nil {
			t.Errorf("UpdateIfMatch: %v", err)
		}
	}
	for _, tc := range []struct {
		name   string
		cached bool // the read phase finds the row cached
		write  func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string)
	}{
		{"cached_row_evicted_by_update_if_match", true, laggedUpdateIfMatch},
		{"update_refresh_failed", false, func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			backing.failNextGet = true
			if err := cache.Update(id, UpdateOpts{Title: &title}); err != nil {
				t.Errorf("Update: %v", err)
			}
		}},
		{"update_if_match_refetch_lagged", false, laggedUpdateIfMatch},
		{"delete", false, func(t *testing.T, _ *casBackingStore, cache *CachingStore, id string) {
			if err := cache.Delete(id); err != nil {
				t.Errorf("Delete: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			// Created around the cache, the event finds it uncached.
			create := backing.Create
			if tc.cached {
				create = cache.Create
			}
			row, err := create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			wrote := false
			cache.applyEventBeforeCommitForTest = func() {
				if wrote {
					return
				}
				wrote = true
				tc.write(t, backing, cache, row.ID)
			}
			cache.ApplyEvent("bead.updated", eventPayload(t, row))
			if !wrote {
				t.Fatal("the local write never ran; the interleaving is vacuous")
			}
			cache.applyEventBeforeCommitForTest = nil
			assertCensusAgreesAfterEvent(t, cache, backing.Store, row.ID)
		})
	}
}

// assertCensusAgreesAfterEvent is assertSettledCensusAgrees for a row a local
// delete may have removed, whose dirty mark only a reconcile clears.
func assertCensusAgreesAfterEvent(t *testing.T, cache *CachingStore, truth Store, id string) {
	t.Helper()
	writeRev := cache.WriteRev(id)
	admittedCensusAgrees(t, cache, truth, id, writeRev)
	if _, err := truth.Get(id); errors.Is(err, ErrNotFound) {
		return
	}
	assertSettledCensusAgrees(t, cache, truth, id, writeRev)
}

// TestCachingStoreApplyEventUncachedDirtyRowKeepsMarkWhenReadFails pins that an
// event for a dirty row the cache does not hold installs nothing when its
// backing read fails: its raw patch is not the backing read the mark waits on.
func TestCachingStoreApplyEventUncachedDirtyRowKeepsMarkWhenReadFails(t *testing.T) {
	t.Parallel()

	// No event's raw payload stands in for a failed read, bead.created's
	// included.
	for _, eventType := range []string{"bead.updated", "bead.created"} {
		t.Run(eventType, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			row, err := backing.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			stale := eventPayload(t, row)
			title := "written"
			backing.failNextGet = true
			if err := cache.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			backing.failNextGet = true
			cache.ApplyEvent(eventType, stale)
			if backing.failNextGet {
				t.Fatal("the event never read the backing; the interleaving is vacuous")
			}
			assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
		})
	}
}

func eventPayload(t *testing.T, b Bead) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return payload
}

// clearBeadSeqByScan ages id's local write to thirty seconds, past the
// five-second recency window but inside recentWriteVerifyWindow, and lets a
// reconcile clear its beadSeq fence: the state a watcher stall longer than
// five seconds leaves behind.
func clearBeadSeqByScan(t *testing.T, cache *CachingStore, id string) {
	t.Helper()
	ageLocalWriteBy(cache, id, 30*time.Second)
	cache.ReconcileNowForTest()
	cache.mu.RLock()
	_, fenced := cache.beadSeq[id]
	cache.mu.RUnlock()
	if fenced {
		t.Fatalf("reconcile left %s's beadSeq fence in place; the late-event state is vacuous", id)
	}
}

// TestCachingStoreLateEventVerifiedAgainstRecentWrite delivers an event
// snapshotted before a local write after a scan cleared the write's beadSeq
// fence (F3). A recent write keeps the conflict under backing verification,
// including an explicit empty edge set on a backing whose point read omits
// edges.
func TestCachingStoreLateEventVerifiedAgainstRecentWrite(t *testing.T) {
	t.Parallel()

	t.Run("fields", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache := newConditionalCacheForTest(t, backing)
		row, err := cache.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		stale := eventPayload(t, row)
		title := "written"
		if err := cache.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		clearBeadSeqByScan(t, cache, row.ID)
		reads := backing.getCalls
		cache.ApplyEvent("bead.updated", stale)
		if backing.getCalls == reads {
			t.Fatal("the late event was applied without a backing verification")
		}
		assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
	})

	t.Run("edges", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}
		cache := newConditionalCacheForTest(t, backing)
		row, err := cache.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		other, err := cache.Create(Bead{Title: "other"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// A class binding's event carries the full edge set, empty included.
		stale := json.RawMessage(fmt.Sprintf(`{"id":%q,"title":%q,"status":%q,"dependencies":[]}`, row.ID, row.Title, row.Status))
		if err := cache.DepAdd(row.ID, other.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd: %v", err)
		}
		clearBeadSeqByScan(t, cache, row.ID)
		cache.ApplyEvent("bead.updated", stale)
		assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
	})
}

// TestCachingStoreFullPrimeFloorFencesStraddlingInstalls runs a full Prime
// that replaces the maps between an install's start and its install (F4): the
// replace drops the deletion fence of a row deleted in between, so only the
// fence floor keeps the install from resurrecting it.
func TestCachingStoreFullPrimeFloorFencesStraddlingInstalls(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		arm  func(backing *casBackingStore, deleteAndPrime func())
		run  func(t *testing.T, cache *CachingStore, id string)
	}{
		{"unconditional_write", func(backing *casBackingStore, deleteAndPrime func()) {
			backing.onGetOnce = deleteAndPrime
		}, func(t *testing.T, cache *CachingStore, id string) {
			if err := cache.SetMetadata(id, "k", "v"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
		}},
		{"dirty_row_refetch", func(backing *casBackingStore, deleteAndPrime func()) {
			backing.onGetOnce = deleteAndPrime
		}, func(t *testing.T, cache *CachingStore, id string) {
			cache.mu.Lock()
			cache.markDirtyLocked(id)
			cache.mu.Unlock()
			if _, err := cache.Get(id); err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get: %v", err)
			}
		}},
		{"reconcile", func(backing *casBackingStore, deleteAndPrime func()) {
			backing.onListOnce = deleteAndPrime
		}, func(_ *testing.T, cache *CachingStore, _ string) {
			cache.ReconcileNowForTest()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			replaced := false
			tc.arm(backing, func() {
				if err := cache.Delete(row.ID); err != nil {
					t.Errorf("Delete: %v", err)
				}
				if err := cache.Prime(context.Background()); err != nil {
					t.Errorf("Prime: %v", err)
				}
				// Only the full replace drops the delete's tombstone.
				cache.mu.RLock()
				_, tombstoned := cache.deletedSeq[row.ID]
				cache.mu.RUnlock()
				replaced = !tombstoned
			})
			cache.mu.RLock()
			reconciledAt := cache.stats.LastReconcileAt
			cache.mu.RUnlock()
			tc.run(t, cache, row.ID)
			if !replaced {
				t.Fatal("the Prime did not replace the maps; the interleaving is vacuous")
			}
			admittedCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
			cache.mu.RLock()
			merged := !cache.stats.LastReconcileAt.Equal(reconciledAt)
			cache.mu.RUnlock()
			if tc.name == "reconcile" && merged {
				t.Fatal("a reconcile whose scan predates a full replace still merged")
			}
		})
	}
}

// cachedRowAgreesOrDirty requires id's cached row to be dirty or to agree with
// truth on metadata key k: a clean row is what a census or Get would serve.
func cachedRowAgreesOrDirty(t *testing.T, cache *CachingStore, truth Store, id string) {
	t.Helper()
	want, err := truth.Get(id)
	if err != nil {
		t.Fatalf("backing Get(%s): %v", id, err)
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if _, dirty := cache.dirty[id]; dirty {
		return
	}
	got, ok := cache.beads[id]
	if !ok {
		return
	}
	if got.Metadata["k"] != want.Metadata["k"] {
		t.Fatalf("clean cached row %s has k=%q; the backing has %q", id, got.Metadata["k"], want.Metadata["k"])
	}
}

// TestCachingStoreRefreshFailureFallbackKeepsRacedMark races a write into
// another's window before its commit, so the later-committing write is fenced
// and leaves the row dirty, and then makes a same-row write whose refresh
// fails. Its fallback patches only its own change into the stale row and must
// not clear the mark the fenced write left.
func TestCachingStoreRefreshFailureFallbackKeepsRacedMark(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		closed bool // the row starts closed
		write  func(cache *CachingStore, id string) error
	}{
		{"close", false, func(cache *CachingStore, id string) error { return cache.Close(id) }},
		{"reopen", true, func(cache *CachingStore, id string) error { return cache.Reopen(id) }},
		{"set_metadata", false, func(cache *CachingStore, id string) error { return cache.SetMetadata(id, "j", "x") }},
		{"set_metadata_batch", false, func(cache *CachingStore, id string) error {
			return cache.SetMetadataBatch(id, map[string]string{"j": "x"})
		}},
		{"tx_close", false, func(cache *CachingStore, id string) error {
			return cache.Tx("close", func(tx Tx) error { return tx.Close(id) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tc.closed {
				if err := cache.Close(row.ID); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
			ran := false
			backing.beforeWrite = func() {
				ran = true
				if err := cache.SetMetadata(row.ID, "k", "a"); err != nil {
					t.Errorf("earlier write: %v", err)
				}
			}
			if err := cache.SetMetadata(row.ID, "k", "b"); err != nil {
				t.Fatalf("fenced write: %v", err)
			}
			if !ran {
				t.Fatal("the earlier write never ran; the interleaving is vacuous")
			}
			fencedRev := cache.WriteRev(row.ID)

			backing.failNextGet = true
			if err := tc.write(cache, row.ID); err != nil {
				t.Fatalf("same-row write: %v", err)
			}
			if backing.failNextGet {
				t.Fatal("the same-row write never refreshed; the fallback is vacuous")
			}
			cachedRowAgreesOrDirty(t, cache, backing.Store, row.ID)
			assertSettledCensusAgrees(t, cache, backing.Store, row.ID, fencedRev, cache.WriteRev(row.ID))
		})
	}
}

// TestCachingStoreRacedWriteStillNotifies pins that a fenced write installs
// nothing but still emits its event: a lost bead.closed or bead.created is
// never emitted again.
func TestCachingStoreRacedWriteStillNotifies(t *testing.T) {
	t.Parallel()

	newCountingCache := func(t *testing.T, backing Store) (*CachingStore, func(eventType, id string) int) {
		t.Helper()
		var mu sync.Mutex
		counts := map[string]int{}
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
			mu.Lock()
			counts[eventType+" "+beadID]++
			mu.Unlock()
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return cache, func(eventType, id string) int {
			mu.Lock()
			defer mu.Unlock()
			return counts[eventType+" "+id]
		}
	}

	for _, tc := range []struct {
		name  string
		racer func(t *testing.T, cache *CachingStore, id string)
	}{
		{"same_row_write", func(t *testing.T, cache *CachingStore, id string) {
			if err := cache.SetMetadata(id, "k", "v"); err != nil {
				t.Errorf("SetMetadata: %v", err)
			}
		}},
		{"failing_cas", func(t *testing.T, cache *CachingStore, id string) {
			title := "lost"
			if err := cache.UpdateIfMatch(id, 1, UpdateOpts{Title: &title}); !IsPreconditionFailed(err) {
				t.Errorf("UpdateIfMatch at a stale revision = %v, want a precondition failure", err)
			}
		}},
	} {
		t.Run("close/"+tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
			cache, count := newCountingCache(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			backing.afterWrite = func() { tc.racer(t, cache, row.ID) }
			if err := cache.Close(row.ID); err != nil {
				t.Fatalf("Close: %v", err)
			}
			cache.mu.RLock()
			_, dirty := cache.dirty[row.ID]
			cache.mu.RUnlock()
			if !dirty {
				t.Fatal("the close was not fenced; the race is vacuous")
			}
			if got := count("bead.closed", row.ID); got != 1 {
				t.Fatalf("fenced Close emitted %d bead.closed events, want 1", got)
			}
		})
	}

	t.Run("graph_apply", func(t *testing.T) {
		t.Parallel()
		race := &casBackingStore{Store: NewMemStore()}
		cache, count := newCountingCache(t, &storageGraphApplyRecordingStore{Store: race})
		applier, ok := cache.GraphApplyHandle()
		if !ok {
			t.Fatal("GraphApplyHandle unavailable")
		}
		var createdID string
		race.onGetOnce = func() {
			rows, err := race.Store.List(ListQuery{AllowScan: true})
			if err != nil || len(rows) != 1 {
				t.Errorf("backing rows = %d (%v), want the one created node", len(rows), err)
				return
			}
			createdID = rows[0].ID
			if err := cache.SetMetadata(createdID, "k", "v"); err != nil {
				t.Errorf("racing SetMetadata: %v", err)
			}
		}
		if _, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "g", Title: "g"}}}); err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		if createdID == "" {
			t.Fatal("the racing write never ran; the interleaving is vacuous")
		}
		if got := count("bead.created", createdID); got != 1 {
			t.Fatalf("fenced graph apply emitted %d bead.created events, want 1", got)
		}
	})
}

// TestCachingStoreRacedStatusWriteClearsDependentVerdicts pins that a fenced
// Close or Reopen still drops its dependents' ready verdicts: the row's status
// may have changed even though the cache installed nothing.
func TestCachingStoreRacedStatusWriteClearsDependentVerdicts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		closed bool
		write  func(cache *CachingStore, id string) error
	}{
		{"close", false, func(cache *CachingStore, id string) error { return cache.Close(id) }},
		{"reopen", true, func(cache *CachingStore, id string) error { return cache.Reopen(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
			cache := newConditionalCacheForTest(t, backing)
			blocker, err := cache.Create(Bead{Title: "blocker"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			dependent, err := cache.Create(Bead{Title: "dependent"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := cache.DepAdd(dependent.ID, blocker.ID, "blocks"); err != nil {
				t.Fatalf("DepAdd: %v", err)
			}
			if tc.closed {
				if err := cache.Close(blocker.ID); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
			verdict := !tc.closed
			cache.mu.Lock()
			row := cache.beads[dependent.ID]
			row.IsBlocked = &verdict
			cache.beads[dependent.ID] = row
			cache.mu.Unlock()

			backing.afterWrite = func() {
				if err := cache.SetMetadata(blocker.ID, "k", "v"); err != nil {
					t.Errorf("racing SetMetadata: %v", err)
				}
			}
			if err := tc.write(cache, blocker.ID); err != nil {
				t.Fatalf("status write: %v", err)
			}
			cache.mu.RLock()
			_, dirty := cache.dirty[blocker.ID]
			stale := cache.beads[dependent.ID].IsBlocked
			cache.mu.RUnlock()
			if !dirty {
				t.Fatal("the status write was not fenced; the race is vacuous")
			}
			if stale != nil {
				t.Fatalf("dependent %s kept is_blocked=%v across its blocker's fenced status write", dependent.ID, *stale)
			}
		})
	}
}

// TestCachingStoreGetAnswersRowDroppedByFullReplace refreshes a dirty row
// whose read a full Prime replace overlaps and drops: the floor refuses the
// install, and Get must still answer the backing's row, not ErrNotFound.
func TestCachingStoreGetAnswersRowDroppedByFullReplace(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := cache.Create(Bead{Title: "other"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ageLocalWrite(cache, row.ID)
	// Closed around the cache, so the active full scan drops it while the
	// backing still has it.
	if err := backing.Close(row.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	cache.mu.Lock()
	cache.noteMutationLocked(row.ID)
	cache.markDirtyLocked(row.ID)
	cache.mu.Unlock()
	dropped := false
	backing.onGetOnce = func() {
		// A mutation after the Get's start puts it below the replace's floor.
		if err := cache.SetMetadata(other.ID, "k", "v"); err != nil {
			t.Errorf("SetMetadata: %v", err)
		}
		if err := cache.Prime(context.Background()); err != nil {
			t.Errorf("Prime: %v", err)
		}
		cache.mu.RLock()
		_, cached := cache.beads[row.ID]
		_, dirty := cache.dirty[row.ID]
		cache.mu.RUnlock()
		dropped = !cached && !dirty
	}
	got, err := cache.Get(row.ID)
	if !dropped {
		t.Fatal("the Prime did not drop the row; the interleaving is vacuous")
	}
	cache.mu.RLock()
	_, installed := cache.beads[row.ID]
	cache.mu.RUnlock()
	if installed {
		t.Fatal("the floor did not refuse the refresh; the interleaving is vacuous")
	}
	if err != nil || got.ID != row.ID || got.Status != "closed" {
		t.Fatalf("Get(%s) = (%+v, %v), want the backing's closed row", row.ID, got, err)
	}
}

// TestCachingStoreApplyEventNeverResurrectsTombstone delivers an event for a
// row this cache deleted, older than the event's read phase, whose install
// would be the raw payload: a transient read failure, or the cache's own
// delayed bead.created echo.
func TestCachingStoreApplyEventNeverResurrectsTombstone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		eventType string
		arm       func(backing *casBackingStore)
	}{
		{"updated_read_fails", "bead.updated", func(backing *casBackingStore) { backing.failNextGet = true }},
		{"created_echo", "bead.created", func(*casBackingStore) {}},
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
			if err := cache.Delete(row.ID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			tc.arm(backing)
			cache.ApplyEvent(tc.eventType, echo)
			if !admittedCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID)) {
				t.Fatal("census refused after an event for a deleted row; the tombstone should suffice")
			}
		})
	}
}

// TestCachingStoreApplyEventDirtyAbsentRowIgnoresNotFoundPatch pins that an
// event for a dirty row the cache does not hold installs nothing when its
// backing read reports the row missing: that is no backing row to install.
func TestCachingStoreApplyEventDirtyAbsentRowIgnoresNotFoundPatch(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := backing.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale := eventPayload(t, row)
	title := "written"
	backing.failNextGet = true
	if err := cache.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	backing.notFoundNextGet = true
	cache.ApplyEvent("bead.updated", stale)
	if backing.notFoundNextGet {
		t.Fatal("the event never read the backing; the interleaving is vacuous")
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
}

// TestCachingStoreRacedWriteLeavesNoMarkOnTombstone pins that a write fenced
// by a local delete of its row, and an event fenced the same way, leave no
// dirty mark: the tombstone already keeps readers off the row, and a mark
// would refuse the census until the next reconcile.
func TestCachingStoreRacedWriteLeavesNoMarkOnTombstone(t *testing.T) {
	t.Parallel()

	assertNoMark := func(t *testing.T, cache *CachingStore, truth Store, id string) {
		t.Helper()
		cache.mu.RLock()
		_, dirty := cache.dirty[id]
		_, tombstoned := cache.deletedSeq[id]
		cache.mu.RUnlock()
		if !tombstoned {
			t.Fatal("the row is not tombstoned; the delete is vacuous")
		}
		if dirty {
			t.Fatalf("tombstoned row %s carries a dirty mark", id)
		}
		if !admittedCensusAgrees(t, cache, truth, id, cache.WriteRev(id)) {
			t.Fatal("census refused")
		}
	}

	t.Run("write", func(t *testing.T) {
		t.Parallel()
		backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
		cache := newConditionalCacheForTest(t, backing)
		row, err := cache.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		backing.afterWrite = func() {
			if err := cache.Delete(row.ID); err != nil {
				t.Errorf("Delete: %v", err)
			}
		}
		if err := cache.SetMetadata(row.ID, "k", "v"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		assertNoMark(t, cache, backing.Store, row.ID)
	})

	t.Run("event", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache := newConditionalCacheForTest(t, backing)
		row, err := backing.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		cache.applyEventBeforeCommitForTest = func() {
			cache.applyEventBeforeCommitForTest = nil
			if err := cache.Delete(row.ID); err != nil {
				t.Errorf("Delete: %v", err)
			}
		}
		cache.ApplyEvent("bead.updated", eventPayload(t, row))
		assertNoMark(t, cache, backing.Store, row.ID)
	})
}

// TestCachingStoreLateEventUnverifiableMarksDirty pins that a late event whose
// backing verification fails leaves the row dirty rather than trusted.
func TestCachingStoreLateEventUnverifiableMarksDirty(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title, external := "written", "external"
	if err := cache.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	clearBeadSeqByScan(t, cache, row.ID)
	backing.failNextGet = true
	cache.ApplyEvent("bead.updated", json.RawMessage(fmt.Sprintf(`{"id":%q,"title":%q}`, row.ID, external)))
	if backing.failNextGet {
		t.Fatal("the event never verified against the backing; the path is vacuous")
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[row.ID]
	cache.mu.RUnlock()
	if !dirty {
		t.Fatal("an unverifiable late event left the row clean")
	}
}

// TestCachingStoreRecentWriteVerifyWindowIsSixtySeconds pins the F3 window at
// the contract's cache_lag_bound default: a conflicting late event is verified
// against the backing just inside it and applied unverified just past it.
func TestCachingStoreRecentWriteVerifyWindowIsSixtySeconds(t *testing.T) {
	t.Parallel()

	const bound = 60 * time.Second
	for _, tc := range []struct {
		name     string
		age      time.Duration
		verified bool
	}{
		{"inside", bound - time.Second, true},
		{"outside", bound + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			stale := eventPayload(t, row)
			title := "written"
			if err := cache.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			clearBeadSeqByScan(t, cache, row.ID)
			ageLocalWriteBy(cache, row.ID, tc.age)
			reads := backing.getCalls
			cache.ApplyEvent("bead.updated", stale)
			if verified := backing.getCalls > reads; verified != tc.verified {
				t.Fatalf("event against a write %v old: verified=%v, want %v", tc.age, verified, tc.verified)
			}
		})
	}
}

// TestCachingStoreLateEventRecheckedUnderLock lands a local write between a
// late event's read phase, which saw no conflict, and its install, with the
// write's beadSeq already cleared and its five-second window past: only the
// write stamp's window keeps the stale event off the row.
func TestCachingStoreLateEventRecheckedUnderLock(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale := eventPayload(t, row)
	title := "written"
	cache.applyEventBeforeCommitForTest = func() {
		cache.applyEventBeforeCommitForTest = nil
		if err := cache.Update(row.ID, UpdateOpts{Title: &title}); err != nil {
			t.Errorf("Update: %v", err)
			return
		}
		cache.mu.Lock()
		cache.localBeadAt[row.ID] = time.Now().Add(-time.Hour)
		cache.writeAt[row.ID] = time.Now().Add(-30 * time.Second)
		cache.markDirtyLocked(row.ID)
		cache.mu.Unlock()
		if _, err := cache.Get(row.ID); err != nil {
			t.Errorf("Get: %v", err)
		}
		cache.mu.RLock()
		_, fenced := cache.beadSeq[row.ID]
		cache.mu.RUnlock()
		if fenced {
			t.Error("the refresh left the write's beadSeq; the state is vacuous")
		}
	}
	cache.ApplyEvent("bead.updated", stale)
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
}

// note is one change notification a cache emitted.
type note struct {
	typ     string
	id      string
	payload json.RawMessage
}

// newNotingCache wires onChange the way cmd/gc does: every notification is
// recorded so a test can inspect it, or deliver it back to the same cache
// through ApplyEventSnapshot (cmd/gc/api_state.go feeds cache-reconcile
// events back that way). drain returns and forgets the recorded notes.
func newNotingCache(t *testing.T, backing Store) (*CachingStore, func() []note) {
	t.Helper()
	var mu sync.Mutex
	var notes []note
	cache := NewCachingStoreForTest(backing, func(typ, id string, payload json.RawMessage) {
		mu.Lock()
		notes = append(notes, note{typ: typ, id: id, payload: append(json.RawMessage(nil), payload...)})
		mu.Unlock()
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, func() []note {
		mu.Lock()
		defer mu.Unlock()
		out := notes
		notes = nil
		return out
	}
}

func isDirty(cache *CachingStore, id string) bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	_, dirty := cache.dirty[id]
	return dirty
}

func deliver(cache *CachingStore, notes []note) {
	for _, n := range notes {
		cache.ApplyEventSnapshot(n.typ, n.payload)
	}
}

// TestCachingStoreEventMergeKeepsRacedMark delivers events onto a row a fenced
// write left dirty: the winner's own bead.closed echo, and a late echo inside
// recentWriteVerifyWindow that the backing confirms. Merging either onto the
// stale cached row is not a backing read, so the mark must stand.
func TestCachingStoreEventMergeKeepsRacedMark(t *testing.T) {
	t.Parallel()

	t.Run("winner_echo", func(t *testing.T) {
		t.Parallel()
		backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
		cache, drain := newNotingCache(t, backing)
		row, err := cache.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		drain()
		inProgress := "in_progress"
		backing.beforeWrite = func() {
			if err := cache.Close(row.ID); err != nil {
				t.Errorf("winning Close: %v", err)
			}
		}
		if err := cache.Update(row.ID, UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("fenced Update: %v", err)
		}
		if !isDirty(cache, row.ID) {
			t.Fatal("the Update was not fenced; the race is vacuous")
		}
		loserRev := cache.WriteRev(row.ID)
		deliver(cache, drain())
		assertSettledCensusAgrees(t, cache, backing.Store, row.ID, loserRev)
	})

	t.Run("late_verified_echo", func(t *testing.T) {
		t.Parallel()
		backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
		cache, drain := newNotingCache(t, backing)
		row, err := cache.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		inProgress, x := "in_progress", "x"
		if err := cache.Update(row.ID, UpdateOpts{Status: &inProgress, Assignee: &x}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		drain()
		backing.beforeWrite = func() {
			if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
				t.Errorf("winning SetMetadata: %v", err)
			}
		}
		if released, err := cache.ReleaseIfCurrent(row.ID, x); err != nil || !released {
			t.Fatalf("fenced ReleaseIfCurrent = (%v, %v)", released, err)
		}
		if !isDirty(cache, row.ID) {
			t.Fatal("the release was not fenced; the race is vacuous")
		}
		loserRev := cache.WriteRev(row.ID)
		notes := drain()
		deliver(cache, notes)
		// The same echoes again after a watcher stall, inside the window.
		ageLocalWriteBy(cache, row.ID, 10*time.Second)
		deliver(cache, notes)
		assertSettledCensusAgrees(t, cache, backing.Store, row.ID, loserRev)
	})
}

// TestCachingStoreRacedCloseNotifiesClosedRow fences a Close whose refresh
// lags its own commit: the bead.closed it emits must carry status closed, as
// an unfenced Close's does, and its echo must not install the lagged open row.
func TestCachingStoreRacedCloseNotifiesClosedRow(t *testing.T) {
	t.Parallel()

	backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
	cache, drain := newNotingCache(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.beforeWrite = func() {
		if err := cache.SetMetadata(row.ID, "k", "v"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
		pre, err := backing.Store.Get(row.ID)
		if err != nil {
			t.Errorf("backing Get: %v", err)
		}
		backing.staleNextGet = &pre
		drain()
	}
	if err := cache.Close(row.ID); err != nil {
		t.Fatalf("fenced Close: %v", err)
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the Close was not fenced; the race is vacuous")
	}
	loserRev := cache.WriteRev(row.ID)
	notes := drain()
	if len(notes) != 1 || notes[0].typ != "bead.closed" {
		t.Fatalf("fenced Close emitted %+v, want one bead.closed", notes)
	}
	var payload Bead
	if err := json.Unmarshal(notes[0].payload, &payload); err != nil || payload.Status != "closed" {
		t.Fatalf("fenced Close's bead.closed carries status %q (%v), want closed", payload.Status, err)
	}
	deliver(cache, notes)
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, loserRev)
}

// TestCachingStoreGetRefreshFetchesOmittedEdges settles a fenced DepAdd with
// a Get on a backing whose point read omits edges: the refresh must take the
// edges from DepList, as the overlay does, or the clean row keeps the
// pre-write edge set.
func TestCachingStoreGetRefreshFetchesOmittedEdges(t *testing.T) {
	t.Parallel()

	backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := cache.Create(Bead{Title: "other"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.beforeWrite = func() {
		if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
	}
	if err := cache.DepAdd(row.ID, other.ID, "blocks"); err != nil {
		t.Fatalf("fenced DepAdd: %v", err)
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the DepAdd was not fenced; the race is vacuous")
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
}

// dropClosedRowByReconcile closes id through the cache and lets a reconcile
// past the recency window drop the closed row. Its write fence stays retained.
func dropClosedRowByReconcile(t *testing.T, cache *CachingStore, id string) {
	t.Helper()
	if err := cache.Close(id); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ageLocalWrite(cache, id)
	cache.ReconcileNowForTest()
	cache.mu.RLock()
	_, held := cache.beads[id]
	cache.mu.RUnlock()
	if held {
		t.Fatal("the reconcile kept the closed row; the state is vacuous")
	}
}

// TestCachingStoreReopenUncachedRefreshFailureMarksRow reopens a row a
// reconcile dropped, with a failing refresh: the reopened row is live in the
// backing but missing here, so the census must not cover the reopen.
func TestCachingStoreReopenUncachedRefreshFailureMarksRow(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropClosedRowByReconcile(t, cache, row.ID)
	backing.failNextGet = true
	if err := cache.Reopen(row.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if backing.failNextGet {
		t.Fatal("the Reopen never refreshed; the fallback is vacuous")
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
}

// TestCachingStoreApplyEventRefusesRawPatchForDroppedRow delivers a stale
// event for a closed row a reconcile dropped, with a failing backing read:
// its raw payload must not reinstall the row as open.
func TestCachingStoreApplyEventRefusesRawPatchForDroppedRow(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale := eventPayload(t, row)
	dropClosedRowByReconcile(t, cache, row.ID)
	closeRev := cache.WriteRev(row.ID)
	backing.failNextGet = true
	cache.ApplyEvent("bead.updated", stale)
	if backing.failNextGet {
		t.Fatal("the event never read the backing; the path is vacuous")
	}
	if !admittedCensusAgrees(t, cache, backing.Store, row.ID, closeRev) {
		t.Fatal("census refused; a refused raw patch needs no mark")
	}
}

// TestCachingStoreRacedMarkDrainsByReconcile pins the documented drain of a
// fenced write's mark by reconcile alone: the recency skip can keep it through
// a reconcile inside five seconds, and the first one after drains it.
func TestCachingStoreRacedMarkDrainsByReconcile(t *testing.T) {
	t.Parallel()

	backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.beforeWrite = func() {
		if err := cache.SetMetadata(row.ID, "k", "a"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
	}
	if err := cache.SetMetadata(row.ID, "k", "b"); err != nil {
		t.Fatalf("fenced SetMetadata: %v", err)
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the write was not fenced; the race is vacuous")
	}
	cache.ReconcileNowForTest()
	ageLocalWrite(cache, row.ID)
	cache.ReconcileNowForTest()
	if isDirty(cache, row.ID) {
		t.Fatal("a reconcile past the recency window did not drain the fenced write's mark")
	}
	if !admittedCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID)) {
		t.Fatal("census refused after the drain")
	}
}

// TestCachingStoreRacedWriteTakesItsOwnStamp pins that a fenced write still
// stamps its own WriteRev, above the write that fenced it: a consumer holding
// it must wait for a census that covers the fenced write itself.
func TestCachingStoreRacedWriteTakesItsOwnStamp(t *testing.T) {
	t.Parallel()

	backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var winnerRev CacheRevision
	backing.beforeWrite = func() {
		if err := cache.SetMetadata(row.ID, "k", "a"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
		winnerRev = cache.WriteRev(row.ID)
	}
	if err := cache.SetMetadata(row.ID, "k", "b"); err != nil {
		t.Fatalf("fenced SetMetadata: %v", err)
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the write was not fenced; the race is vacuous")
	}
	if loserRev := cache.WriteRev(row.ID); loserRev.Seq <= winnerRev.Seq {
		t.Fatalf("fenced write's WriteRev %+v does not exceed the fencing write's %+v", loserRev, winnerRev)
	}
}

// TestCachingStoreRacedFallbackLeavesCachedRowUntouched pins that a fenced
// write whose refresh failed builds its notification on a clone: "install
// nothing" includes the cached row's maps.
func TestCachingStoreRacedFallbackLeavesCachedRowUntouched(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(cache *CachingStore, id string) error
	}{
		{"set_metadata", func(cache *CachingStore, id string) error { return cache.SetMetadata(id, "k", "raced") }},
		{"set_metadata_batch", func(cache *CachingStore, id string) error {
			return cache.SetMetadataBatch(id, map[string]string{"k": "raced"})
		}},
		{"update", func(cache *CachingStore, id string) error {
			return cache.Update(id, UpdateOpts{Metadata: map[string]string{"k": "raced"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed", Metadata: map[string]string{"a": "1"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			backing.beforeWrite = func() {
				if err := cache.SetMetadata(row.ID, "w", "1"); err != nil {
					t.Errorf("winning SetMetadata: %v", err)
				}
				backing.failNextGet = true
			}
			if err := tc.write(cache, row.ID); err != nil {
				t.Fatalf("fenced write: %v", err)
			}
			if !isDirty(cache, row.ID) {
				t.Fatal("the write was not fenced; the race is vacuous")
			}
			cache.mu.RLock()
			got, present := cache.beads[row.ID].Metadata["k"]
			cache.mu.RUnlock()
			if present {
				t.Fatalf("a fenced write installed nothing, yet the cached row carries k=%q", got)
			}
		})
	}
}

// failingGetStore fails every Get while failGets is set, and runs beforeWrite
// once, right before the next Close, Reopen, ReleaseIfCurrent, SetMetadata,
// SetMetadataBatch or Update commits. The first failing Get closes failedGet.
// Safe for concurrent use.
type failingGetStore struct {
	*MemStore
	failGets    atomic.Bool
	beforeWrite atomic.Pointer[func()]
	failedGet   atomic.Pointer[chan struct{}]
}

func (s *failingGetStore) before() {
	if hook := s.beforeWrite.Swap(nil); hook != nil {
		(*hook)()
	}
}

func (s *failingGetStore) Get(id string) (Bead, error) {
	if s.failGets.Load() {
		if ch := s.failedGet.Swap(nil); ch != nil {
			close(*ch)
		}
		return Bead{}, errors.New("injected refresh failure")
	}
	return s.MemStore.Get(id)
}

func (s *failingGetStore) Close(id string) error { s.before(); return s.MemStore.Close(id) }

func (s *failingGetStore) SetMetadata(id, key, value string) error {
	s.before()
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *failingGetStore) Reopen(id string) error { s.before(); return s.MemStore.Reopen(id) }
func (s *failingGetStore) Update(id string, opts UpdateOpts) error {
	s.before()
	return s.MemStore.Update(id, opts)
}

func (s *failingGetStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.before()
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *failingGetStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	s.before()
	return s.MemStore.ReleaseIfCurrent(id, expectedAssignee)
}

// TestCachingStoreRacedFallbacksConcurrentWithWrites runs, under -race, each
// fenced write whose refresh fails against same-row writes whose refresh also
// fails: the fenced write's notification is marshaled after unlock while the
// concurrent fallbacks patch the row, so it must not share the row's maps.
func TestCachingStoreRacedFallbacksConcurrentWithWrites(t *testing.T) {
	t.Parallel()

	inProgress, x := "in_progress", "x"
	for _, tc := range []struct {
		name  string
		setup func(cache *CachingStore, id string) error
		write func(cache *CachingStore, id string) error
	}{
		{"close", nil, func(cache *CachingStore, id string) error { return cache.Close(id) }},
		{
			"reopen", func(cache *CachingStore, id string) error { return cache.Close(id) },
			func(cache *CachingStore, id string) error { return cache.Reopen(id) },
		},
		{"release_if_current", func(cache *CachingStore, id string) error {
			return cache.Update(id, UpdateOpts{Status: &inProgress, Assignee: &x})
		}, func(cache *CachingStore, id string) error {
			_, err := cache.ReleaseIfCurrent(id, x)
			return err
		}},
		{"set_metadata", nil, func(cache *CachingStore, id string) error { return cache.SetMetadata(id, "k", "v") }},
		{"set_metadata_batch", nil, func(cache *CachingStore, id string) error {
			return cache.SetMetadataBatch(id, map[string]string{"k": "v"})
		}},
		{"update", nil, func(cache *CachingStore, id string) error {
			return cache.Update(id, UpdateOpts{Metadata: map[string]string{"k": "v"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for range 10 {
				backing := &failingGetStore{MemStore: NewMemStore()}
				cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) {})
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatalf("Prime: %v", err)
				}
				row, err := cache.Create(Bead{Title: "seed", Metadata: map[string]string{"a": "1"}})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if tc.setup != nil {
					if err := tc.setup(cache, row.ID); err != nil {
						t.Fatalf("setup: %v", err)
					}
				}
				// A same-row write lands first so the write under test is
				// fenced; then every read fails, and the concurrent writer
				// starts once the fenced write's refresh has failed.
				failed := make(chan struct{})
				backing.failedGet.Store(&failed)
				hook := func() {
					if err := cache.SetMetadata(row.ID, "w", "1"); err != nil {
						t.Errorf("winning SetMetadata: %v", err)
					}
					backing.failGets.Store(true)
				}
				backing.beforeWrite.Store(&hook)
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					if err := tc.write(cache, row.ID); err != nil {
						t.Errorf("fenced write: %v", err)
					}
					// Release the writer even if no read failed, so a
					// vacuous setup fails instead of hanging.
					if ch := backing.failedGet.Swap(nil); ch != nil {
						t.Error("the fenced write's refresh never failed; the race is vacuous")
						close(*ch)
					}
				}()
				go func() {
					defer wg.Done()
					<-failed
					for k := range 20 {
						if err := cache.SetMetadata(row.ID, "x", strconv.Itoa(k)); err != nil {
							t.Errorf("concurrent SetMetadata: %v", err)
						}
					}
				}()
				wg.Wait()
			}
		})
	}
}

// TestCachingStoreRacedWriteNotifiesItsWrite fences each unconditional verb
// with a same-row write that commits first, with and without a failing
// refresh, and requires exactly one event for the row carrying the write.
func TestCachingStoreRacedWriteNotifiesItsWrite(t *testing.T) {
	t.Parallel()

	a := "A"
	inProgress, x := "in_progress", "x"
	type verb func(cache *CachingStore, id, other string) error
	field := func(name string, got func(Bead) string, want string) func(Bead) error {
		return func(b Bead) error {
			if got(b) != want {
				return fmt.Errorf("%s = %q, want %q", name, got(b), want)
			}
			return nil
		}
	}
	title := field("title", func(b Bead) string { return b.Title }, a)
	status := func(want string) func(Bead) error {
		return field("status", func(b Bead) string { return b.Status }, want)
	}
	k := field("metadata k", func(b Bead) string { return b.Metadata["k"] }, "v")
	edge := func(want bool) func(Bead) error {
		return func(b Bead) error {
			if got := len(b.Dependencies) > 0; got != want {
				return fmt.Errorf("edges = %v, want an edge: %v", b.Dependencies, want)
			}
			return nil
		}
	}
	closeRow := func(cache *CachingStore, id, _ string) error { return cache.Close(id) }
	for _, tc := range []struct {
		name     string
		fallback bool // also run with a failing refresh
		setup    verb
		write    verb
		typ      string
		check    func(Bead) error
	}{
		{"update", true, nil, func(cache *CachingStore, id, _ string) error {
			return cache.Update(id, UpdateOpts{Title: &a})
		}, "bead.updated", title},
		{"close", true, nil, closeRow, "bead.closed", status("closed")},
		{"reopen", true, closeRow, func(cache *CachingStore, id, _ string) error {
			return cache.Reopen(id)
		}, "bead.updated", status("open")},
		{"set_metadata", true, nil, func(cache *CachingStore, id, _ string) error {
			return cache.SetMetadata(id, "k", "v")
		}, "bead.updated", k},
		{"set_metadata_batch", true, nil, func(cache *CachingStore, id, _ string) error {
			return cache.SetMetadataBatch(id, map[string]string{"k": "v"})
		}, "bead.updated", k},
		{"release_if_current", true, func(cache *CachingStore, id, _ string) error {
			return cache.Update(id, UpdateOpts{Status: &inProgress, Assignee: &x})
		}, func(cache *CachingStore, id, _ string) error {
			released, err := cache.ReleaseIfCurrent(id, x)
			if err == nil && !released {
				err = errors.New("released nothing")
			}
			return err
		}, "bead.updated", func(b Bead) error {
			if b.Status != "open" || b.Assignee != "" {
				return fmt.Errorf("(status, assignee) = (%q, %q), want (open, \"\")", b.Status, b.Assignee)
			}
			return nil
		}},
		{"dep_add", false, nil, func(cache *CachingStore, id, other string) error {
			return cache.DepAdd(id, other, "blocks")
		}, "bead.updated", edge(true)},
		{"dep_remove", false, func(cache *CachingStore, id, other string) error {
			return cache.DepAdd(id, other, "blocks")
		}, func(cache *CachingStore, id, other string) error {
			return cache.DepRemove(id, other)
		}, "bead.updated", edge(false)},
		{"close_all", false, nil, func(cache *CachingStore, id, _ string) error {
			_, err := cache.CloseAll([]string{id}, nil)
			return err
		}, "bead.closed", status("closed")},
		{"tx_update", false, nil, func(cache *CachingStore, id, _ string) error {
			return cache.Tx("update", func(tx Tx) error { return tx.Update(id, UpdateOpts{Title: &a}) })
		}, "bead.updated", title},
		{"tx_close", true, nil, func(cache *CachingStore, id, _ string) error {
			return cache.Tx("close", func(tx Tx) error { return tx.Close(id) })
		}, "bead.closed", status("closed")},
	} {
		for _, fail := range []bool{false, true} {
			if fail && !tc.fallback {
				continue
			}
			name := tc.name + "/refreshed"
			if fail {
				name = tc.name + "/refresh_failed"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
				cache, drain := newNotingCache(t, backing)
				row, err := cache.Create(Bead{Title: "seed"})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				other, err := cache.Create(Bead{Title: "other"})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if tc.setup != nil {
					if err := tc.setup(cache, row.ID, other.ID); err != nil {
						t.Fatalf("setup: %v", err)
					}
				}
				backing.beforeWrite = func() {
					if err := cache.SetMetadata(row.ID, "w", "1"); err != nil {
						t.Errorf("winning SetMetadata: %v", err)
					}
					backing.failNextGet = fail
					drain()
				}
				if err := tc.write(cache, row.ID, other.ID); err != nil {
					t.Fatalf("fenced write: %v", err)
				}
				if !isDirty(cache, row.ID) {
					t.Fatal("the write was not fenced; the race is vacuous")
				}
				var mine []note
				for _, n := range drain() {
					if n.id == row.ID {
						mine = append(mine, n)
					}
				}
				if len(mine) != 1 || mine[0].typ != tc.typ {
					t.Fatalf("fenced write emitted %+v for %s, want one %s", mine, row.ID, tc.typ)
				}
				var payload Bead
				if err := json.Unmarshal(mine[0].payload, &payload); err != nil {
					t.Fatalf("payload: %v", err)
				}
				if err := tc.check(payload); err != nil {
					t.Fatalf("fenced write's %s payload: %v", tc.typ, err)
				}
			})
		}
	}
}

// graphRaceStore runs beforeApply once, right before its next graph apply
// commits.
type graphRaceStore struct {
	*storageGraphApplyRecordingStore
	beforeApply func()
}

func (s *graphRaceStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	if hook := s.beforeApply; hook != nil {
		s.beforeApply = nil
		hook()
	}
	return s.storageGraphApplyRecordingStore.ApplyGraphPlan(ctx, plan)
}

// TestCachingStoreMultiRowWriteMarksOnlyFencedRows runs a racer before a
// multi-row write's commit: the rows the racer fenced stay dirty, the others
// install clean, and the settled census agrees on all of them.
func TestCachingStoreMultiRowWriteMarksOnlyFencedRows(t *testing.T) {
	t.Parallel()

	a := "A"
	for _, tc := range []struct {
		name  string
		write func(cache *CachingStore, ids []string) error
	}{
		{"close_all", func(cache *CachingStore, ids []string) error {
			_, err := cache.CloseAll(ids, nil)
			return err
		}},
		{"tx", func(cache *CachingStore, ids []string) error {
			return cache.Tx("update", func(tx Tx) error {
				for _, id := range ids {
					if err := tx.Update(id, UpdateOpts{Title: &a}); err != nil {
						return err
					}
				}
				return nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
			cache := newConditionalCacheForTest(t, backing)
			fenced, err := cache.Create(Bead{Title: "fenced"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			clean, err := cache.Create(Bead{Title: "clean"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			backing.beforeWrite = func() {
				if err := cache.SetMetadata(fenced.ID, "k", "v"); err != nil {
					t.Errorf("racing SetMetadata: %v", err)
				}
			}
			if err := tc.write(cache, []string{fenced.ID, clean.ID}); err != nil {
				t.Fatalf("multi-row write: %v", err)
			}
			if !isDirty(cache, fenced.ID) {
				t.Fatalf("fenced row %s is clean after its install was refused", fenced.ID)
			}
			if isDirty(cache, clean.ID) {
				t.Fatalf("unfenced row %s is dirty", clean.ID)
			}
			for _, id := range []string{fenced.ID, clean.ID} {
				assertSettledCensusAgrees(t, cache, backing.Store, id, cache.WriteRev(id))
			}
		})
	}

	t.Run("graph_apply", func(t *testing.T) {
		t.Parallel()
		inner := &casBackingStore{Store: NewMemStore()}
		backing := &graphRaceStore{storageGraphApplyRecordingStore: &storageGraphApplyRecordingStore{Store: inner}}
		cache := newConditionalCacheForTest(t, backing)
		other, err := cache.Create(Bead{Title: "other"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		applier, ok := cache.GraphApplyHandle()
		if !ok {
			t.Fatal("GraphApplyHandle unavailable")
		}
		// A mutation and then a full replace before the commit put the
		// apply's start below the fence floor, which fences every row it
		// creates.
		backing.beforeApply = func() {
			if err := cache.SetMetadata(other.ID, "k", "v"); err != nil {
				t.Errorf("SetMetadata: %v", err)
			}
			if err := cache.Prime(context.Background()); err != nil {
				t.Errorf("Prime: %v", err)
			}
		}
		result, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "g", Title: "g"}}})
		if err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		id := result.IDs["g"]
		if !isDirty(cache, id) {
			t.Fatalf("graph row %s is clean after the floor refused its install", id)
		}
		assertSettledCensusAgrees(t, cache, inner.Store, id, cache.WriteRev(id))
	})
}

// TestCachingStoreVerifiedCloseRecheckedUnderLock lands a local Reopen between
// a bead.closed event's backing verification, which saw the row closed, and
// its install, leaving a cached row equal to the one the event verified
// against: the stale close must not install over the covered reopen.
func TestCachingStoreVerifiedCloseRecheckedUnderLock(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(row.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	closed, err := backing.Store.Get(row.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	reopened := false
	cache.applyEventBeforeCommitForTest = func() {
		cache.applyEventBeforeCommitForTest = nil
		reopened = true
		if err := cache.Reopen(row.ID); err != nil {
			t.Errorf("Reopen: %v", err)
		}
	}
	cache.ApplyEvent("bead.closed", eventPayload(t, closed))
	if !reopened {
		t.Fatal("the local Reopen never ran; the interleaving is vacuous")
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
}

// TestCachingStoreMultiRowRefreshFailureMarksRow pins that CloseAll and Tx
// mark a row dirty when its post-write refresh fails: the cached row lacks
// the committed write, so no census may cover it yet.
func TestCachingStoreMultiRowRefreshFailureMarksRow(t *testing.T) {
	t.Parallel()

	a := "A"
	for _, tc := range []struct {
		name  string
		write func(cache *CachingStore, id string) error
	}{
		{"close_all", func(cache *CachingStore, id string) error {
			_, err := cache.CloseAll([]string{id}, map[string]string{"k": "v"})
			return err
		}},
		{"tx_update", func(cache *CachingStore, id string) error {
			return cache.Tx("update", func(tx Tx) error { return tx.Update(id, UpdateOpts{Title: &a}) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			row, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			backing.failNextGet = true
			if err := tc.write(cache, row.ID); err != nil && tc.name == "tx_update" {
				t.Fatalf("write: %v", err)
			}
			if backing.failNextGet {
				t.Fatal("the write never refreshed; the fallback is vacuous")
			}
			if !isDirty(cache, row.ID) {
				t.Fatalf("row %s is clean after its refresh failed", row.ID)
			}
			assertSettledCensusAgrees(t, cache, backing.Store, row.ID, cache.WriteRev(row.ID))
		})
	}
}

// TestCachingStoreReconcileRecoveryHoldsUnreadRow pins that a reconcile whose
// recovery Get fails holds the cached row it merges back instead of absorbing
// it: the absorb would clear the mark a raced write left without any backing
// read, and a clean census would then show the row another process closed as
// live. (A fenced close leaves no row to hold: claimCloseLocked drops it.)
func TestCachingStoreReconcileRecoveryHoldsUnreadRow(t *testing.T) {
	t.Parallel()

	backing := &writeRaceStore{casBackingStore: &casBackingStore{Store: NewMemStore()}}
	cache := newConditionalCacheForTest(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.beforeWrite = func() {
		if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
	}
	if err := cache.SetMetadata(row.ID, "k", "2"); err != nil {
		t.Fatalf("fenced SetMetadata: %v", err)
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the write was not fenced; the interleaving is vacuous")
	}
	fencedRev := cache.WriteRev(row.ID)
	if err := backing.Store.Close(row.ID); err != nil {
		t.Fatalf("out-of-process Close: %v", err)
	}
	ageLocalWrite(cache, row.ID)
	backing.failNextGet = true
	cache.ReconcileNowForTest()
	if backing.failNextGet {
		t.Fatal("the reconcile never read the backing row; the recovery is vacuous")
	}
	if !isDirty(cache, row.ID) {
		t.Fatalf("row %s is clean after a reconcile that could not read it", row.ID)
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, fencedRev)
}

// TestCachingStoreStaleClosedSnapshotTakesBackingRow delivers a delayed rich
// bead.closed snapshot from an earlier close/reopen cycle. The backing row is
// closed again, so verification passes, but the snapshot is older than it: the
// cache must take the backing row, not merge the snapshot over the writes
// made since.
func TestCachingStoreStaleClosedSnapshotTakesBackingRow(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, drain := newNotingCache(t, backing)
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	drain()
	if err := cache.Close(row.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var firstClose note
	for _, n := range drain() {
		if n.typ == "bead.closed" {
			firstClose = n
		}
	}
	if firstClose.typ == "" {
		t.Fatal("no bead.closed notification")
	}
	if err := cache.Reopen(row.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if err := cache.SetMetadata(row.ID, "k", "2"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	w2 := cache.WriteRev(row.ID)
	if err := cache.Close(row.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	drain()
	cache.ApplyEventSnapshot("bead.closed", firstClose.payload)
	cache.mu.RLock()
	got := cache.beads[row.ID]
	cache.mu.RUnlock()
	if got.Metadata["k"] != "2" {
		t.Fatalf("cached k=%q after the stale close snapshot, want the backing's %q", got.Metadata["k"], "2")
	}
	// A Reopen whose refresh fails patches the cached row, so the census
	// shows whatever the snapshot left in it.
	backing.failNextGet = true
	if err := cache.Reopen(row.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	assertSettledCensusAgrees(t, cache, backing.Store, row.ID, w2)
}

// TestCachingStoreCreateFencesRacingWrite lands a local write between Create's
// refresh read and its install: the install must not roll the write back.
func TestCachingStoreCreateFencesRacingWrite(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, drain := newNotingCache(t, backing)
	var writerRev CacheRevision
	var id string
	backing.onGetOnce = func() {
		rows, err := backing.Store.List(ListQuery{AllowScan: true})
		if err != nil || len(rows) != 1 {
			t.Errorf("backing rows = %d (%v), want the one created row", len(rows), err)
			return
		}
		id = rows[0].ID
		if err := cache.SetMetadata(id, "k", "v"); err != nil {
			t.Errorf("racing SetMetadata: %v", err)
		}
		writerRev = cache.WriteRev(id)
	}
	if _, err := cache.Create(Bead{Title: "seed"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("the racing write never ran; the interleaving is vacuous")
	}
	created := false
	for _, n := range drain() {
		created = created || n.typ == "bead.created"
	}
	if !created {
		t.Fatal("a fenced Create must still notify bead.created")
	}
	assertSettledCensusAgrees(t, cache, backing.Store, id, writerRev, cache.WriteRev(id))
}

// depListFailStore fails the next DepList when failNextDepList is set.
type depListFailStore struct {
	*writeRaceStore
	failNextDepList bool
}

func (s *depListFailStore) DepList(id, direction string) ([]Dep, error) {
	if s.failNextDepList {
		s.failNextDepList = false
		return nil, errors.New("injected deplist failure")
	}
	return s.writeRaceStore.DepList(id, direction)
}

// TestCachingStoreGetRefreshDepListFailureKeepsMark settles a fenced DepAdd
// with a Get whose DepList fails, on a backing whose point read omits edges:
// installing the row would clear the mark over the pre-write edge set.
func TestCachingStoreGetRefreshDepListFailureKeepsMark(t *testing.T) {
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
	inner.beforeWrite = func() {
		if err := cache.SetMetadata(row.ID, "k", "1"); err != nil {
			t.Errorf("winning SetMetadata: %v", err)
		}
	}
	if err := cache.DepAdd(row.ID, other.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if !isDirty(cache, row.ID) {
		t.Fatal("the DepAdd was not fenced; the interleaving is vacuous")
	}
	rev := cache.WriteRev(row.ID)
	backing.failNextDepList = true
	got, err := cache.Get(row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if backing.failNextDepList {
		t.Fatal("the Get never listed edges; the failure is vacuous")
	}
	if got.Metadata["k"] != "1" {
		t.Fatalf("Get answered k=%q, want the backing row's %q", got.Metadata["k"], "1")
	}
	if !isDirty(cache, row.ID) {
		t.Fatalf("row %s is clean after a refresh that could not read its edges", row.ID)
	}
	assertSettledCensusAgrees(t, cache, inner.Store, row.ID, rev)
}

// TestCachingStoreGraphApplyRefreshFailureMarksRow pins that graph apply marks
// a row it created dirty when the row's refresh read fails.
func TestCachingStoreGraphApplyRefreshFailureMarksRow(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, &storageGraphApplyRecordingStore{Store: backing})
	applier, ok := cache.GraphApplyHandle()
	if !ok {
		t.Fatal("GraphApplyHandle unavailable")
	}
	backing.failNextGet = true
	result, err := applier.ApplyGraphPlan(context.Background(), &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "g", Title: "g"}}})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if backing.failNextGet {
		t.Fatal("graph apply never refreshed; the fallback is vacuous")
	}
	if len(result.IDs) != 1 {
		t.Fatalf("graph apply created %d rows, want 1", len(result.IDs))
	}
	id := result.IDs["g"]
	if !isDirty(cache, id) {
		t.Fatalf("row %s is clean after its refresh failed", id)
	}
	assertSettledCensusAgrees(t, cache, backing.Store, id, cache.WriteRev(id))
}

// TestCachingStoreUncachedWriteRefreshFailureMarksRow pins that SetMetadata
// and ReleaseIfCurrent mark a row the cache does not hold dirty when their
// refresh read fails: there is no cached row to patch the write into.
func TestCachingStoreUncachedWriteRefreshFailureMarksRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(cache *CachingStore, id string) error
	}{
		{"set_metadata", func(cache *CachingStore, id string) error {
			return cache.SetMetadata(id, "k", "v")
		}},
		{"release_if_current", func(cache *CachingStore, id string) error {
			released, err := cache.ReleaseIfCurrent(id, "a")
			if err == nil && !released {
				err = errors.New("not released")
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &failingGetStore{MemStore: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			// Created behind the cache, so it holds no row for it.
			row, err := backing.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("backing Create: %v", err)
			}
			inProgress, a := "in_progress", "a"
			if err := backing.MemStore.Update(row.ID, UpdateOpts{Status: &inProgress, Assignee: &a}); err != nil {
				t.Fatalf("backing Update: %v", err)
			}
			cache.mu.RLock()
			_, held := cache.beads[row.ID]
			cache.mu.RUnlock()
			if held {
				t.Fatal("the cache holds the row; the uncached branch is vacuous")
			}
			backing.failGets.Store(true)
			if err := tc.write(cache, row.ID); err != nil {
				t.Fatalf("write: %v", err)
			}
			backing.failGets.Store(false)
			if !isDirty(cache, row.ID) {
				t.Fatalf("row %s is clean after its refresh failed", row.ID)
			}
			assertSettledCensusAgrees(t, cache, backing.MemStore, row.ID, cache.WriteRev(row.ID))
		})
	}
}
