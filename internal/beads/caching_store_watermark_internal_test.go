package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// coveredBy reports whether a clean census at r reflects a write at w.
func coveredBy(w, r CacheRevision) bool {
	return w.Epoch == r.Epoch && w.Seq <= r.Seq
}

// watermarkWrite is one write verb driven through the cache by the watermark
// tests. reflected reports whether an active census reflects the write.
type watermarkWrite struct {
	name      string
	write     func(t *testing.T, cache *CachingStore, id string)
	reflected func(rows []Bead, id string) bool
}

func watermarkWrites() []watermarkWrite {
	title := "written"
	rowWith := func(pred func(Bead) bool) func([]Bead, string) bool {
		return func(rows []Bead, id string) bool {
			for _, row := range rows {
				if row.ID == id {
					return pred(row)
				}
			}
			return false
		}
	}
	absent := func(rows []Bead, id string) bool {
		for _, row := range rows {
			if row.ID == id {
				return false
			}
		}
		return true
	}
	titled := rowWith(func(b Bead) bool { return b.Title == title })
	swapped := rowWith(func(b Bead) bool { return b.Metadata["k"] == "v" })
	revision := func(t *testing.T, cache *CachingStore, id string) int64 {
		t.Helper()
		got, err := cache.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got.Revision
	}
	return []watermarkWrite{
		{"update", func(t *testing.T, cache *CachingStore, id string) {
			if err := cache.Update(id, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("Update: %v", err)
			}
		}, titled},
		{"set_metadata_batch", func(t *testing.T, cache *CachingStore, id string) {
			if err := cache.SetMetadataBatch(id, map[string]string{"k": "v"}); err != nil {
				t.Fatalf("SetMetadataBatch: %v", err)
			}
		}, swapped},
		{"update_if_match", func(t *testing.T, cache *CachingStore, id string) {
			if err := cache.UpdateIfMatch(id, revision(t, cache, id), UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("UpdateIfMatch: %v", err)
			}
		}, titled},
		{"compare_and_set_metadata_key", func(t *testing.T, cache *CachingStore, id string) {
			if ok, err := cache.CompareAndSetMetadataKey(id, "k", "", "v"); !ok || err != nil {
				t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", ok, err)
			}
		}, swapped},
		{"close_if_match", func(t *testing.T, cache *CachingStore, id string) {
			if err := cache.CloseIfMatch(id, revision(t, cache, id)); err != nil {
				t.Fatalf("CloseIfMatch: %v", err)
			}
		}, absent},
		{"delete_if_match", func(t *testing.T, cache *CachingStore, id string) {
			if err := cache.DeleteIfMatch(id, revision(t, cache, id)); err != nil {
				t.Fatalf("DeleteIfMatch: %v", err)
			}
		}, absent},
	}
}

// mustCleanCensus takes an active census that must be admitted.
func mustCleanCensus(t *testing.T, cache *CachingStore) ([]Bead, CacheRevision) {
	t.Helper()
	rows, observation, ok := cache.ObservedList(ListQuery{AllowScan: true})
	if !ok {
		t.Fatal("ObservedList refused; the cache is dirty")
	}
	return rows, observation.CacheRev()
}

// censusRow returns id's row from rows.
func censusRow(t *testing.T, rows []Bead, id string) Bead {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("census %+v has no row %s", rows, id)
	return Bead{}
}

// ageLocalWrite moves id's local-write stamp past the recency window, the
// state of a write older than five seconds.
func ageLocalWrite(cache *CachingStore, id string) {
	cache.mu.Lock()
	cache.localBeadAt[id] = time.Now().Add(-time.Hour)
	cache.mu.Unlock()
}

// TestCachingStoreWriteRevBoundedByFirstReflectingCleanCensus pins the
// ledger clearing contract: the first clean census taken after a write
// returns reflects it and carries a CacheRev covering the write's WriteRev,
// while a census pinned before the write does not cover it. The census needs
// no backing read in between: conditional writes refetch eagerly.
func TestCachingStoreWriteRevBoundedByFirstReflectingCleanCensus(t *testing.T) {
	t.Parallel()

	for _, tc := range watermarkWrites() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "seed"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			_, before := mustCleanCensus(t, cache)

			tc.write(t, cache, b.ID)
			writeRev := cache.WriteRev(b.ID)
			reads := backing.getCalls

			rows, after := mustCleanCensus(t, cache)
			if backing.getCalls != reads {
				t.Fatalf("census performed %d backing reads, want none", backing.getCalls-reads)
			}
			if !tc.reflected(rows, b.ID) {
				t.Fatalf("clean census %+v does not reflect the write to %s", rows, b.ID)
			}
			if !coveredBy(writeRev, after) {
				t.Fatalf("WriteRev %+v not covered by CacheRev %+v of a census reflecting the write", writeRev, after)
			}
			if coveredBy(writeRev, before) {
				t.Fatalf("WriteRev %+v covered by pre-write CacheRev %+v; the pinned pass would clear an unseen write", writeRev, before)
			}
		})
	}
}

// TestCachingStoreCensusRefusedWithoutRevisionWhileDirty pins that a dirty
// cache yields no rows and no revision, so a pass keeps its previous CacheRev.
// The lagged legs also prove the eager refetch never installs a row that does
// not reflect the committed write, including one still at expectedRevision
// whose fields already match: the census stays refused until an ordinary read
// observes the commit.
func TestCachingStoreCensusRefusedWithoutRevisionWhileDirty(t *testing.T) {
	t.Parallel()

	lagged := func(opts func(pre Bead) UpdateOpts) func(*testing.T, *casBackingStore, *CachingStore, string) {
		return func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			got, err := cache.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			pre := cloneBead(got)
			backing.staleNextGet = &pre
			if err := cache.UpdateIfMatch(id, got.Revision, opts(got)); err != nil {
				t.Fatalf("UpdateIfMatch: %v", err)
			}
		}
	}
	cases := []struct {
		name  string
		dirty func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string)
	}{
		{"ambiguous_conditional_failure", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			backing.errOverride = errors.New("bd: connection reset mid-write")
			if _, err := cache.CompareAndSetMetadataKey(id, "k", "", "v"); err == nil {
				t.Fatal("CompareAndSetMetadataKey: want the injected ambiguous error")
			}
			backing.errOverride = nil
			// The ambiguous write did land.
			if err := backing.SetMetadataBatch(id, map[string]string{"k": "v"}); err != nil {
				t.Fatalf("backing SetMetadataBatch: %v", err)
			}
		}},
		{"lagged_conditional_refetch", lagged(func(Bead) UpdateOpts {
			return UpdateOpts{Metadata: map[string]string{"k": "v"}}
		})},
		// The write re-states a field the backing row already has, so only
		// the unmoved revision exposes the pre-write read; the cached row was
		// at an older revision, so the write cannot be proven idempotent.
		{"lagged_refetch_at_expected_revision_over_stale_row", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			if err := backing.SetMetadataBatch(id, map[string]string{"k": "v"}); err != nil {
				t.Fatalf("out-of-band SetMetadataBatch: %v", err)
			}
			pre, err := backing.Store.Get(id)
			if err != nil {
				t.Fatalf("backing Get: %v", err)
			}
			backing.staleNextGet = &pre
			title := pre.Title
			if err := cache.UpdateIfMatch(id, pre.Revision, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("UpdateIfMatch: %v", err)
			}
		}},
		{"lagged_refetch_at_expected_revision_over_dirty_row", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			if err := cache.SetMetadataBatch(id, map[string]string{"k": "v"}); err != nil {
				t.Fatalf("SetMetadataBatch: %v", err)
			}
			backing.errOverride = errors.New("bd: connection reset mid-write")
			if _, err := cache.CompareAndSetMetadataKey(id, "x", "", "y"); err == nil {
				t.Fatal("CompareAndSetMetadataKey: want the injected ambiguous error")
			}
			backing.errOverride = nil
			pre, err := backing.Store.Get(id)
			if err != nil {
				t.Fatalf("backing Get: %v", err)
			}
			backing.staleNextGet = &pre
			title := pre.Title
			if err := cache.UpdateIfMatch(id, pre.Revision, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("UpdateIfMatch: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "dirty"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			tc.dirty(t, backing, cache, b.ID)
			writeRev := cache.WriteRev(b.ID)
			rows, observation, ok := cache.ObservedList(ListQuery{AllowScan: true})
			if ok || rows != nil || observation.CacheRev() != (CacheRevision{}) {
				t.Fatalf("ObservedList on a dirty cache = %v rows, CacheRev %+v, ok=%v; want refused with no revision",
					len(rows), observation.CacheRev(), ok)
			}

			if _, err := cache.Get(b.ID); err != nil {
				t.Fatalf("Get: %v", err)
			}
			rows, after := mustCleanCensus(t, cache)
			if !coveredBy(writeRev, after) {
				t.Fatalf("WriteRev %+v not covered by CacheRev %+v after refetch", writeRev, after)
			}
			fresh, err := backing.Store.Get(b.ID)
			if err != nil {
				t.Fatalf("backing Get: %v", err)
			}
			if row := censusRow(t, rows, b.ID); row.Revision != fresh.Revision || row.Metadata["k"] != fresh.Metadata["k"] {
				t.Fatalf("census after refetch = %+v, want the committed row %+v", row, fresh)
			}
		})
	}
}

// TestCachingStoreConditionalWriteEagerRefetchClearsDirty pins that a
// successful conditional write leaves the cache clean and holding the
// committed row with no later read, at the same single backing Get the
// post-write notification already paid for.
func TestCachingStoreConditionalWriteEagerRefetchClearsDirty(t *testing.T) {
	t.Parallel()

	for _, tc := range watermarkWrites() {
		if tc.name != "update_if_match" && tc.name != "compare_and_set_metadata_key" && tc.name != "close_if_match" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "eager"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			// The caller's revision read is cache-served, so the only backing
			// Get is the post-write refetch.
			reads := backing.getCalls
			tc.write(t, cache, b.ID)
			if got := backing.getCalls - reads; got != 1 {
				t.Fatalf("backing Get calls = %d, want the single post-write refetch", got)
			}

			cache.mu.RLock()
			dirty := len(cache.dirty)
			cached, inBeads := cache.beads[b.ID]
			cache.mu.RUnlock()
			if dirty != 0 {
				t.Fatalf("dirty set has %d rows after a conditional write; want the eager refetch to clear it", dirty)
			}
			fresh, err := backing.Store.Get(b.ID)
			if err != nil {
				t.Fatalf("backing Get: %v", err)
			}
			if !inBeads || cached.Revision != fresh.Revision || cached.Status != fresh.Status || cached.Title != fresh.Title {
				t.Fatalf("cached row = %+v (present=%v), want the committed backing row %+v", cached, inBeads, fresh)
			}
		})
	}
}

// TestCachingStoreUpdateReflected pins the eager refetch's install predicate:
// every written field must already be on the refetched row, and a cleared
// metadata key may read back as empty or absent.
func TestCachingStoreUpdateReflected(t *testing.T) {
	t.Parallel()

	str := func(s string) *string { return &s }
	priority, other := 2, 3
	row := Bead{
		Title: "t", Status: "in_progress", Type: "task", Priority: &priority,
		Description: "d", Assignee: "a", Metadata: map[string]string{"k": "v"},
	}
	unprioritized := cloneBead(row)
	unprioritized.Priority = nil
	cases := []struct {
		name string
		row  Bead
		opts UpdateOpts
		want bool
	}{
		{"all_fields_present", row, UpdateOpts{
			Title: str("t"), Status: str("in_progress"), Type: str("task"), Priority: &priority,
			Description: str("d"), Assignee: str("a"), Metadata: map[string]string{"k": "v"},
		}, true},
		{"cleared_key_absent", row, UpdateOpts{Metadata: map[string]string{"gone": ""}}, true},
		{"lagged_title", row, UpdateOpts{Title: str("x")}, false},
		{"lagged_status", row, UpdateOpts{Status: str("open")}, false},
		{"lagged_type", row, UpdateOpts{Type: str("bug")}, false},
		{"lagged_priority", row, UpdateOpts{Priority: &other}, false},
		{"priority_missing", unprioritized, UpdateOpts{Priority: &priority}, false},
		{"lagged_description", row, UpdateOpts{Description: str("x")}, false},
		{"lagged_assignee", row, UpdateOpts{Assignee: str("b")}, false},
		{"lagged_metadata", row, UpdateOpts{Metadata: map[string]string{"k": "w"}}, false},
		{"cleared_key_still_set", row, UpdateOpts{Metadata: map[string]string{"k": ""}}, false},
	}
	for _, tc := range cases {
		if got := updateReflected(tc.row, tc.opts); got != tc.want {
			t.Errorf("%s: updateReflected = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCachingStoreRevisionMoved pins the revision half of the install
// predicate: a row still at expectedRevision is a pre-write read, and zero on
// either side carries no usable token.
func TestCachingStoreRevisionMoved(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		revision int64
		expected int64
		want     bool
	}{
		{"moved", 8, 7, true},
		{"unmoved", 7, 7, false},
		{"no_expected_token", 7, 0, true},
		{"no_row_token", 0, 7, true},
	}
	for _, tc := range cases {
		if got := revisionMoved(Bead{Revision: tc.revision}, tc.expected); got != tc.want {
			t.Errorf("%s: revisionMoved = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCachingStoreConditionalRefetchFencedByNewerMutation races a newer
// mutation of the same row into the eager refetch's backing read. The fence
// must discard the older refetch: it may neither overwrite the newer row, nor
// clear a dirty mark the newer mutation set, nor resurrect a deleted row.
func TestCachingStoreConditionalRefetchFencedByNewerMutation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		newer func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string)
		check func(t *testing.T, cache *CachingStore, id string)
	}{
		{"newer_write_installed", func(t *testing.T, _ *casBackingStore, cache *CachingStore, id string) {
			if err := cache.SetMetadataBatch(id, map[string]string{"n": "2"}); err != nil {
				t.Fatalf("SetMetadataBatch: %v", err)
			}
		}, func(t *testing.T, cache *CachingStore, id string) {
			rows, rev := mustCleanCensus(t, cache)
			if row := censusRow(t, rows, id); row.Metadata["n"] != "2" {
				t.Fatalf("census row %+v; the stale refetch overwrote the newer write", row)
			}
			if w := cache.WriteRev(id); !coveredBy(w, rev) {
				t.Fatalf("WriteRev %+v not covered by CacheRev %+v", w, rev)
			}
		}},
		{"newer_mutation_left_dirty", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			backing.errOverride = errors.New("bd: connection reset mid-write")
			if _, err := cache.CompareAndSetMetadataKey(id, "n", "", "2"); err == nil {
				t.Fatal("CompareAndSetMetadataKey: want the injected ambiguous error")
			}
			backing.errOverride = nil
		}, func(t *testing.T, cache *CachingStore, id string) {
			if _, _, ok := cache.ObservedList(ListQuery{AllowScan: true}); ok {
				t.Fatal("census admitted; the stale refetch cleared the newer mutation's dirty mark")
			}
			cache.mu.RLock()
			_, dirty := cache.dirty[id]
			cache.mu.RUnlock()
			if !dirty {
				t.Fatalf("row %s not dirty after an ambiguous newer write", id)
			}
		}},
		{"newer_delete", func(t *testing.T, _ *casBackingStore, cache *CachingStore, id string) {
			if err := cache.Delete(id); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		}, func(t *testing.T, cache *CachingStore, id string) {
			if rows, _ := mustCleanCensus(t, cache); len(rows) != 0 {
				t.Fatalf("census = %+v; the stale refetch resurrected deleted %s", rows, id)
			}
			if _, err := cache.Get(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after delete = %v, want ErrNotFound", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "fenced"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			backing.onGetOnce = func() { tc.newer(t, backing, cache, b.ID) }
			title := "older"
			if err := cache.UpdateIfMatch(b.ID, got.Revision, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("UpdateIfMatch: %v", err)
			}
			if backing.onGetOnce != nil {
				t.Fatal("the conditional write never refetched; the race was not exercised")
			}
			tc.check(t, cache, b.ID)
		})
	}
}

// TestCachingStoreConditionalInstallKeepsWriteFence pins that the eager
// install keeps the write's own beadSeq fence, as every write path must
// (gastownhall/gascity#2210). Each case feeds the cache an older view of the
// row after a fenced write installed it; none may roll the row back, and a
// census that covers the newest write must show it.
func TestCachingStoreConditionalInstallKeepsWriteFence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		run  func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string)
		want string
	}{
		{"same_row_cas_race", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			// B's CAS runs entirely inside A's refetch, after A read n=1.
			backing.onGetOnce = func() {
				if ok, err := cache.CompareAndSetMetadataKey(id, "n", "1", "2"); !ok || err != nil {
					t.Errorf("inner CompareAndSetMetadataKey = (%v, %v)", ok, err)
				}
			}
			if ok, err := cache.CompareAndSetMetadataKey(id, "n", "", "1"); !ok || err != nil {
				t.Fatalf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
			}
		}, "2"},
		{"dirty_get_started_before_write", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			backing.errOverride = errors.New("bd: connection reset mid-write")
			if _, err := cache.CompareAndSetMetadataKey(id, "x", "", "y"); err == nil {
				t.Fatal("CompareAndSetMetadataKey: want the injected ambiguous error")
			}
			backing.errOverride = nil
			// The Get reads the pre-write row; the fenced write lands before
			// the Get relocks.
			backing.onGetOnce = func() {
				if ok, err := cache.CompareAndSetMetadataKey(id, "n", "", "1"); !ok || err != nil {
					t.Errorf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
				}
			}
			if _, err := cache.Get(id); err != nil {
				t.Fatalf("Get: %v", err)
			}
		}, "1"},
		{"reconcile_scan_older_than_recency_window", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			// The scan collects the pre-write row; the fenced write lands and
			// ages past the recency window before the merge.
			backing.onListOnce = func() {
				if ok, err := cache.CompareAndSetMetadataKey(id, "n", "", "1"); !ok || err != nil {
					t.Errorf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
				}
				ageLocalWrite(cache, id)
			}
			cache.runReconciliation()
		}, "1"},
		{"late_echo_event", func(t *testing.T, _ *casBackingStore, cache *CachingStore, id string) {
			if err := cache.SetMetadataBatch(id, map[string]string{"n": "0"}); err != nil {
				t.Fatalf("SetMetadataBatch: %v", err)
			}
			if ok, err := cache.CompareAndSetMetadataKey(id, "n", "0", "1"); !ok || err != nil {
				t.Fatalf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
			}
			ageLocalWrite(cache, id)
			// The hook echo of the n=0 write arrives after the fenced write.
			cache.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+id+`","metadata":{"n":"0"}}`))
		}, "1"},
		{"refetch_after_newer_write_was_refetched", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			got, err := cache.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			// Inside A's refetch: B's write-through refresh fails, leaving the
			// row dirty, and C's Get refetches it, clearing B's beadSeq. Only
			// B's write revision still fences A's older read.
			backing.onGetOnce = func() {
				backing.failNextGet = true
				if err := cache.SetMetadataBatch(id, map[string]string{"n": "2"}); err != nil {
					t.Errorf("SetMetadataBatch: %v", err)
				}
				if _, err := cache.Get(id); err != nil {
					t.Errorf("Get: %v", err)
				}
			}
			title := "older"
			if err := cache.UpdateIfMatch(id, got.Revision, UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("UpdateIfMatch: %v", err)
			}
		}, "2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "fence"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			tc.run(t, backing, cache, b.ID)
			if t.Failed() {
				return
			}
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata["n"] != tc.want {
				t.Fatalf("Get n=%q, want %q; an older view rolled the row back", got.Metadata["n"], tc.want)
			}
			rows, rev := mustCleanCensus(t, cache)
			if row := censusRow(t, rows, b.ID); row.Metadata["n"] != tc.want {
				t.Fatalf("census row n=%q, want %q", row.Metadata["n"], tc.want)
			}
			if w := cache.WriteRev(b.ID); !coveredBy(w, rev) {
				t.Fatalf("WriteRev %+v not covered by CacheRev %+v", w, rev)
			}
		})
	}
}

// watermarkWriteRecord is one successful write a concurrent writer published.
type watermarkWriteRecord struct {
	n   int
	rev CacheRevision
}

// watermarkWriteLog collects published writes for census readers.
type watermarkWriteLog struct {
	mu      sync.Mutex
	records map[string][]watermarkWriteRecord
}

func (l *watermarkWriteLog) add(id string, r watermarkWriteRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records[id] = append(l.records[id], r)
}

// floor returns, per id, the highest written n among the writes rev covers.
func (l *watermarkWriteLog) floor(rev CacheRevision) map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.records))
	for id, records := range l.records {
		for _, r := range records {
			if coveredBy(r.rev, rev) && r.n > out[id] {
				out[id] = r.n
			}
		}
	}
	return out
}

// runWatermarkStress drives writers against census readers. Each admitted
// census must show, for every row, at least the highest write its CacheRev
// covers, and one reader's CacheRev never moves backwards. Once the writers
// stop, a census must be admitted that covers every write.
func runWatermarkStress(t *testing.T, cache *CachingStore, ids []string, writers int, write func(w int) (string, int, bool)) map[string]int {
	t.Helper()
	log := &watermarkWriteLog{records: map[string][]watermarkWriteRecord{}}
	var writersWG, readersWG sync.WaitGroup
	done := make(chan struct{})
	for w := range writers {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for {
				id, n, more := write(w)
				if !more {
					return
				}
				log.add(id, watermarkWriteRecord{n: n, rev: cache.WriteRev(id)})
			}
		}()
	}
	var admitted atomic.Int64
	for range 3 {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			var last CacheRevision
			for {
				select {
				case <-done:
					return
				default:
				}
				rows, observation, ok := cache.ObservedList(ListQuery{AllowScan: true})
				if !ok {
					continue
				}
				admitted.Add(1)
				rev := observation.CacheRev()
				if rev.Epoch != last.Epoch && last.Epoch != 0 || rev.Seq < last.Seq {
					t.Errorf("CacheRev moved backwards: %+v after %+v", rev, last)
					return
				}
				last = rev
				floor := log.floor(rev)
				for _, row := range rows {
					if got, _ := strconv.Atoi(row.Metadata["n"]); got < floor[row.ID] {
						t.Errorf("census at %+v shows %s n=%d, but it covers a write of n=%d", rev, row.ID, got, floor[row.ID])
						return
					}
				}
			}
		}()
	}
	writersWG.Wait()
	close(done)
	readersWG.Wait()
	if t.Failed() {
		return nil
	}

	// A write whose refetch raced a newer write declines to install and
	// leaves the row dirty until its next read; settle every row first.
	for _, id := range ids {
		if _, err := cache.Get(id); err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
	}
	rows, rev := mustCleanCensus(t, cache)
	if len(rows) != len(ids) {
		t.Fatalf("final census has %d rows, want %d", len(rows), len(ids))
	}
	final := make(map[string]int, len(rows))
	for _, row := range rows {
		final[row.ID], _ = strconv.Atoi(row.Metadata["n"])
		for _, r := range log.records[row.ID] {
			if !coveredBy(r.rev, rev) {
				t.Fatalf("final census %+v does not cover %s write %+v", rev, row.ID, r)
			}
		}
	}
	t.Logf("admitted %d concurrent censuses", admitted.Load())
	return final
}

// TestCachingStoreWatermarkConcurrentWritersAndCensus runs one conditional
// writer per row against census readers.
func TestCachingStoreWatermarkConcurrentWritersAndCensus(t *testing.T) {
	t.Parallel()

	const (
		writers = 4
		rounds  = 40
	)
	cache := newConditionalCacheForTest(t, NewMemStore())
	ids := make([]string, writers)
	for i := range ids {
		b, err := cache.Create(Bead{Title: "w" + strconv.Itoa(i)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[i] = b.ID
	}
	next := make([]int, writers)
	final := runWatermarkStress(t, cache, ids, writers, func(w int) (string, int, bool) {
		id := ids[w]
		next[w]++
		n := next[w]
		if n > rounds {
			return "", 0, false
		}
		if n%2 == 0 {
			ok, err := cache.CompareAndSetMetadataKey(id, "n", strconv.Itoa(n-1), strconv.Itoa(n))
			if err != nil || !ok {
				t.Errorf("CompareAndSetMetadataKey(%s, %d) = (%v, %v)", id, n, ok, err)
				return "", 0, false
			}
			return id, n, true
		}
		got, err := cache.Get(id)
		if err != nil {
			t.Errorf("Get(%s): %v", id, err)
			return "", 0, false
		}
		if err := cache.UpdateIfMatch(id, got.Revision, UpdateOpts{Metadata: map[string]string{"n": strconv.Itoa(n)}}); err != nil {
			t.Errorf("UpdateIfMatch(%s, %d): %v", id, n, err)
			return "", 0, false
		}
		return id, n, true
	})
	for _, id := range ids {
		if final != nil && final[id] != rounds {
			t.Fatalf("final census %s n=%d, want %d", id, final[id], rounds)
		}
	}
}

// TestCachingStoreWatermarkConcurrentWritersOnOneRow races several CAS writers
// on one row against census readers: every lost CAS retries from a fresh read,
// and no install may roll the row back past a covered write.
func TestCachingStoreWatermarkConcurrentWritersOnOneRow(t *testing.T) {
	t.Parallel()

	const (
		writers = 4
		rounds  = 25
	)
	cache := newConditionalCacheForTest(t, NewMemStore())
	b, err := cache.Create(Bead{Title: "contended"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wins := make([]int, writers)
	final := runWatermarkStress(t, cache, []string{b.ID}, writers, func(w int) (string, int, bool) {
		if wins[w] == rounds {
			return "", 0, false
		}
		for {
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Errorf("Get: %v", err)
				return "", 0, false
			}
			current := got.Metadata["n"]
			n, _ := strconv.Atoi(current)
			ok, err := cache.CompareAndSetMetadataKey(b.ID, "n", current, strconv.Itoa(n+1))
			if err != nil {
				t.Errorf("CompareAndSetMetadataKey: %v", err)
				return "", 0, false
			}
			if ok {
				wins[w]++
				return b.ID, n + 1, true
			}
		}
	})
	if final != nil && final[b.ID] != writers*rounds {
		t.Fatalf("final n=%d, want %d", final[b.ID], writers*rounds)
	}
}

// TestCachingStoreAtomicCloserInstallsReturnedRow pins that the atomic closer
// installs the committed row it returns, clean and without a backing read,
// unless a mutation raced the backing write, in which case the row is not
// attributable and stays a miss.
func TestCachingStoreAtomicCloserInstallsReturnedRow(t *testing.T) {
	t.Parallel()

	t.Run("installed", func(t *testing.T) {
		t.Parallel()
		backing := &atomicConditionalCloseBacking{Store: newNativeDoltStoreForTest(newNativeDoltMemStorage())}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "terminal"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		backing.getCalls = 0
		if _, err := closeWithCacheHandle(cache, b.ID, got.Revision, map[string]string{"state": "drained"}); err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
		rows, rev := mustCleanCensus(t, cache)
		if len(rows) != 0 {
			t.Fatalf("active census = %+v after the terminal close", rows)
		}
		if w := cache.WriteRev(b.ID); !coveredBy(w, rev) {
			t.Fatalf("WriteRev %+v not covered by CacheRev %+v", w, rev)
		}
		if backing.getCalls != 0 {
			t.Fatalf("backing Get calls = %d, want 0", backing.getCalls)
		}
	})

	t.Run("reconcile_absorbed_external_change_not_installed", func(t *testing.T) {
		t.Parallel()
		backing := &atomicConditionalCloseBacking{Store: newNativeDoltStoreForTest(newNativeDoltMemStorage())}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "reopened externally"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		ageLocalWrite(cache, b.ID)
		// An external reopen that reconcile absorbs before the eviction bumps
		// no seq and clears the beadSeq fence.
		backing.afterClose = func() {
			if err := backing.Reopen(b.ID); err != nil {
				t.Errorf("external Reopen: %v", err)
			}
			cache.runReconciliation()
		}
		if _, err := closeWithCacheHandle(cache, b.ID, got.Revision, nil); err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
		current, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if current.Status != "open" {
			t.Fatalf("Get status = %q, want the external reopen; the stale closed row was installed", current.Status)
		}
	})

	for _, mangle := range []struct {
		name string
		fn   func(*Bead)
	}{
		{"returned_row_not_closed", func(b *Bead) { b.Status = "open" }},
		{"returned_row_other_id", func(b *Bead) { b.ID = "" }},
	} {
		t.Run(mangle.name, func(t *testing.T) {
			t.Parallel()
			backing := &atomicConditionalCloseBacking{Store: newNativeDoltStoreForTest(newNativeDoltMemStorage())}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "mangled"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			backing.mangleReturned = mangle.fn
			if _, err := closeWithCacheHandle(cache, b.ID, got.Revision, nil); err != nil {
				t.Fatalf("CloseWithMetadataIfMatch: %v", err)
			}
			cache.mu.RLock()
			_, dirty := cache.dirty[b.ID]
			cache.mu.RUnlock()
			if !dirty {
				t.Fatal("a returned row that is not the committed close was installed")
			}
		})
	}

	t.Run("raced_delete_not_installed", func(t *testing.T) {
		t.Parallel()
		backing := &atomicConditionalCloseBacking{Store: newNativeDoltStoreForTest(newNativeDoltMemStorage())}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "deleted"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		// The delete leaves no row to compare revisions against; only its
		// tombstone says the returned row is stale.
		backing.afterClose = func() {
			if err := cache.Delete(b.ID); err != nil {
				t.Errorf("Delete: %v", err)
			}
		}
		if _, err := closeWithCacheHandle(cache, b.ID, got.Revision, nil); err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
		if row, err := cache.Get(b.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after the raced delete = (%+v, %v), want ErrNotFound; the closed row was resurrected", row, err)
		}
	})

	t.Run("raced_mutation_not_installed", func(t *testing.T) {
		t.Parallel()
		backing := &atomicConditionalCloseBacking{Store: newNativeDoltStoreForTest(newNativeDoltMemStorage())}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "reopened"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		backing.afterClose = func() {
			if err := cache.Reopen(b.ID); err != nil {
				t.Errorf("Reopen: %v", err)
			}
		}
		if _, err := closeWithCacheHandle(cache, b.ID, got.Revision, nil); err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
		current, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if current.Status != "open" {
			t.Fatalf("Get status = %q, want the raced reopen; the stale closed row was installed", current.Status)
		}
	})
}

// TestCachingStoreWriteRevCoversShortCircuitedWrites pins WriteRev on rows this
// cache never wrote, where a matching write short-circuits without a backing
// call: the revision is the current sequence, and a clean census covering it
// reflects the (already present) write.
func TestCachingStoreWriteRevCoversShortCircuitedWrites(t *testing.T) {
	t.Parallel()

	title := "seeded"
	cases := []struct {
		name  string
		write func(cache *CachingStore, id string) error
		check func(row Bead, present bool) bool
	}{
		{"update", func(cache *CachingStore, id string) error {
			return cache.Update(id, UpdateOpts{Title: &title})
		}, func(row Bead, present bool) bool { return present && row.Title == title }},
		{"set_metadata", func(cache *CachingStore, id string) error {
			return cache.SetMetadata(id, "k", "v")
		}, func(row Bead, present bool) bool { return present && row.Metadata["k"] == "v" }},
		{"set_metadata_batch", func(cache *CachingStore, id string) error {
			return cache.SetMetadataBatch(id, map[string]string{"k": "v"})
		}, func(row Bead, present bool) bool { return present && row.Metadata["k"] == "v" }},
		{"close", func(cache *CachingStore, id string) error {
			return cache.Close(id)
		}, func(_ Bead, present bool) bool { return !present }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			seeded, err := backing.Create(Bead{Title: title, Metadata: map[string]string{"k": "v"}})
			if err != nil {
				t.Fatalf("seed Create: %v", err)
			}
			other, err := backing.Create(Bead{Title: "other"})
			if err != nil {
				t.Fatalf("seed Create: %v", err)
			}
			cache := newConditionalCacheForTest(t, backing)
			if tc.name == "close" {
				// The cache learns of the close from the backing, not a write.
				if err := backing.Close(seeded.ID); err != nil {
					t.Fatalf("backing Close: %v", err)
				}
				cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+seeded.ID+`","status":"closed"}`))
			}
			// An unrelated write moves the sequence past the seeded row.
			if err := cache.SetMetadata(other.ID, "x", "y"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}

			calls := backing.getCalls
			if err := tc.write(cache, seeded.ID); err != nil {
				t.Fatalf("write: %v", err)
			}
			if backing.getCalls != calls {
				t.Fatal("the write reached the backing; the short-circuit was not exercised")
			}
			w := cache.WriteRev(seeded.ID)
			if w.Seq != cacheMutationSeq(cache) {
				t.Fatalf("WriteRev %+v for a never-written row, want the current sequence %d", w, cacheMutationSeq(cache))
			}
			rows, rev := mustCleanCensus(t, cache)
			if !coveredBy(w, rev) {
				t.Fatalf("WriteRev %+v not covered by CacheRev %+v", w, rev)
			}
			var row Bead
			present := false
			for _, r := range rows {
				if r.ID == seeded.ID {
					row, present = r, true
				}
			}
			if !tc.check(row, present) {
				t.Fatalf("census row %+v (present=%v) does not reflect the short-circuited write", row, present)
			}
		})
	}
}

// TestCachingStoreRevisionEpochPerInstance pins that each CachingStore issues
// revisions under its own nonzero epoch, so revisions from a rebuilt cache are
// never comparable with the old one's.
func TestCachingStoreRevisionEpochPerInstance(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	b, err := backing.Create(Bead{Title: "shared"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := newConditionalCacheForTest(t, backing)
	second := newConditionalCacheForTest(t, backing)
	_, firstRev := mustCleanCensus(t, first)
	_, secondRev := mustCleanCensus(t, second)
	if firstRev.Epoch == 0 || secondRev.Epoch == 0 || firstRev.Epoch == secondRev.Epoch {
		t.Fatalf("epochs %d and %d, want distinct and nonzero", firstRev.Epoch, secondRev.Epoch)
	}
	if w := first.WriteRev(b.ID); w.Epoch != firstRev.Epoch {
		t.Fatalf("WriteRev epoch %d, want the instance's %d", w.Epoch, firstRev.Epoch)
	}
	if coveredBy(first.WriteRev(b.ID), secondRev) {
		t.Fatal("a revision from one instance is covered by another instance's census")
	}
}

// TestCachingStorePrimeDropsWriteRevisionsOfDroppedRows pins that a wholesale
// prime keeps the write revision of every row it keeps and drops the rest.
func TestCachingStorePrimeDropsWriteRevisionsOfDroppedRows(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	cache := newConditionalCacheForTest(t, backing)
	kept, err := cache.Create(Bead{Title: "kept"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropped, err := cache.Create(Bead{Title: "dropped"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(dropped.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	ageLocalWrite(cache, kept.ID)
	ageLocalWrite(cache, dropped.ID)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.RLock()
	_, keptRev := cache.writeSeq[kept.ID]
	_, droppedRev := cache.writeSeq[dropped.ID]
	_, droppedRow := cache.beads[dropped.ID]
	cache.mu.RUnlock()
	if droppedRow {
		t.Fatal("prime kept the out-of-band closed row; the prune was not exercised")
	}
	if !keptRev || droppedRev {
		t.Fatalf("writeSeq kept=%v dropped=%v after prime, want true and false", keptRev, droppedRev)
	}
}

// TestCachingStoreEvictDropsWriteRevision pins that a row leaving the cache
// through evictLocked takes its write revision along, so the map cannot leak
// ids the cache no longer holds. WriteRev then falls back to the current
// sequence.
func TestCachingStoreEvictDropsWriteRevision(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		evict func(t *testing.T, backing *MemStore, cache *CachingStore, id string)
	}{
		{"delete_tombstone", func(t *testing.T, _ *MemStore, cache *CachingStore, id string) {
			if err := cache.Delete(id); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		}},
		{"live_list_drops_missing_row", func(t *testing.T, backing *MemStore, cache *CachingStore, id string) {
			if err := backing.Delete(id); err != nil {
				t.Fatalf("backing Delete: %v", err)
			}
			ageLocalWrite(cache, id)
			if _, err := cache.List(ListQuery{Live: true, AllowScan: true}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := NewMemStore()
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "evicted"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			tc.evict(t, backing, cache, b.ID)
			cache.mu.RLock()
			_, cached := cache.beads[b.ID]
			_, recorded := cache.writeSeq[b.ID]
			cache.mu.RUnlock()
			if cached {
				t.Fatal("row still cached; the eviction was not exercised")
			}
			if recorded {
				t.Fatalf("writeSeq still holds evicted %s", b.ID)
			}
			if w := cache.WriteRev(b.ID); w.Seq != cacheMutationSeq(cache) {
				t.Fatalf("WriteRev %+v after eviction, want the current sequence", w)
			}
		})
	}
}

// TestCachingStoreConditionalInstallRequiresServingCache pins that the eager
// install never populates a cache that is not serving reads.
func TestCachingStoreConditionalInstallRequiresServingCache(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	b, err := backing.Create(Bead{Title: "unprimed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	title := "written"
	if err := cache.UpdateIfMatch(b.ID, b.Revision, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch: %v", err)
	}
	cache.mu.RLock()
	_, installed := cache.beads[b.ID]
	cache.mu.RUnlock()
	if installed {
		t.Fatal("the eager refetch installed a row into an unprimed cache")
	}
}

// TestCachingStoreConditionalInstallCarriesDependencies pins that the eager
// install keeps the row's cached dependencies, which no conditional verb
// changes, when the backing's Get does not return them.
func TestCachingStoreConditionalInstallCarriesDependencies(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true}
	blocker, err := backing.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blocked, err := backing.Create(Bead{Title: "blocked"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cache := newConditionalCacheForTest(t, backing)
	readyIDs := func() map[string]bool {
		t.Helper()
		ready, err := cache.Ready()
		if err != nil {
			t.Fatalf("Ready: %v", err)
		}
		out := map[string]bool{}
		for _, b := range ready {
			out[b.ID] = true
		}
		return out
	}
	if readyIDs()[blocked.ID] {
		t.Fatal("blocked bead ready before the write; the fixture is wrong")
	}

	if ok, err := cache.CompareAndSetMetadataKey(blocked.ID, "k", "", "v"); !ok || err != nil {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[blocked.ID]
	cache.mu.RUnlock()
	if dirty {
		t.Fatal("the eager refetch did not install; the install was not exercised")
	}
	if readyIDs()[blocked.ID] {
		t.Fatal("blocked bead ready after a fenced metadata write; the install dropped its dependencies")
	}
}

// TestCachingStoreWatermarkNeverFabricatesRevisions pins honest reporting for
// backings whose conditional writes cannot commit: a missing capability, a
// backing that declares the verbs but refuses them (DoltliteReadStore's
// shape), and a gate refusal. None may advance WriteRev or dirty the cache.
func TestCachingStoreWatermarkNeverFabricatesRevisions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		backing func() Store
		inject  error
	}{
		{"capability_absent", func() Store { return conditionalFreeStore{NewMemStore()} }, nil},
		{"verbs_refuse_unsupported", func() Store { return &casBackingStore{Store: NewMemStore()} }, ErrConditionalWriteUnsupported},
		{"gate_refusal", func() Store { return &casBackingStore{Store: NewMemStore()} }, &GateRefusalError{Verb: "update", Code: "close-authority"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := tc.backing()
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "written"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			createRev := cache.WriteRev(b.ID)
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			_, before := mustCleanCensus(t, cache)
			if cas, ok := backing.(*casBackingStore); ok {
				cas.errOverride = tc.inject
			}

			title := "x"
			if err := cache.UpdateIfMatch(b.ID, got.Revision, UpdateOpts{Title: &title}); err == nil {
				t.Fatal("UpdateIfMatch succeeded on a refusing backing")
			}
			if err := cache.CloseIfMatch(b.ID, got.Revision); err == nil {
				t.Fatal("CloseIfMatch succeeded on a refusing backing")
			}
			if err := cache.DeleteIfMatch(b.ID, got.Revision); err == nil {
				t.Fatal("DeleteIfMatch succeeded on a refusing backing")
			}
			if ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "v"); ok || err == nil {
				t.Fatalf("CompareAndSetMetadataKey = (%v, %v) on a refusing backing", ok, err)
			}
			if _, err := closeWithCacheHandle(cache, b.ID, got.Revision, nil); err == nil {
				t.Fatal("CloseWithMetadataIfMatch succeeded on a refusing backing")
			}

			if w := cache.WriteRev(b.ID); w != createRev {
				t.Fatalf("WriteRev after refused writes = %+v, want the Create's %+v", w, createRev)
			}
			_, after := mustCleanCensus(t, cache)
			if after != before {
				t.Fatalf("CacheRev after refused writes = %+v, want unchanged %+v", after, before)
			}
		})
	}
}

// conditionalFreeStore hides every optional capability of its backing, the
// shape of the exec: provider and other stores without conditional writes.
type conditionalFreeStore struct{ Store }

// clearWriteFence leaves id with a committed fenced write (n=1) whose beadSeq
// fence a later refetch cleared, so only its write revision still says the
// row is newer than any read begun before the write. lagged picks how the row
// was dirty again for that refetch: the write's own refetch served a lagged
// pre-write row, or an unverifiable event re-dirtied the installed row
// without a seq bump.
func clearWriteFence(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, lagged bool) {
	t.Helper()
	if lagged {
		pre, err := backing.Store.Get(id)
		if err != nil {
			t.Fatalf("backing Get: %v", err)
		}
		backing.staleNextGet = &pre
	}
	if ok, err := cache.CompareAndSetMetadataKey(id, "n", "", "1"); !ok || err != nil {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
	}
	ageLocalWrite(cache, id)
	if !lagged {
		cache.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+id+`","metadata":{"n":"9"}}`))
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[id]
	cache.mu.RUnlock()
	if !dirty {
		t.Fatalf("row %s not dirty; the refetch below would not clear beadSeq", id)
	}
	if _, err := cache.Get(id); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cache.mu.RLock()
	_, beadSeq := cache.beadSeq[id]
	cache.mu.RUnlock()
	if beadSeq {
		t.Fatalf("row %s kept its beadSeq; only writeSeq was meant to fence it", id)
	}
	ageLocalWrite(cache, id)
}

// TestCachingStoreSnapshotPathsFenceOnWriteSeq pins that every path installing
// a row read before a local write fences on the write's revision, not only its
// beadSeq, which a newer refetch may already have cleared. Each reader starts
// before the write and reads the pre-write row; installing it would leave a
// clean census that covers the write while showing the old value.
func TestCachingStoreSnapshotPathsFenceOnWriteSeq(t *testing.T) {
	t.Parallel()

	dirtyFirst := func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
		backing.errOverride = errors.New("bd: connection reset mid-write")
		if _, err := cache.CompareAndSetMetadataKey(id, "x", "", "y"); err == nil {
			t.Fatal("CompareAndSetMetadataKey: want the injected ambiguous error")
		}
		backing.errOverride = nil
	}
	paths := []struct {
		name string
		read func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, race func())
	}{
		{"get_dirty_refetch", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, race func()) {
			dirtyFirst(t, backing, cache, id)
			backing.onGetOnce = race
			if _, err := cache.Get(id); err != nil {
				t.Fatalf("Get: %v", err)
			}
		}},
		{"list_dirty_overlay", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, race func()) {
			dirtyFirst(t, backing, cache, id)
			backing.onGetOnce = race
			if _, err := cache.List(ListQuery{AllowScan: true}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
		{"reconcile_merge", func(_ *testing.T, backing *casBackingStore, cache *CachingStore, _ string, race func()) {
			backing.onListOnce = race
			cache.runReconciliation()
		}},
		{"live_list_refresh", func(t *testing.T, backing *casBackingStore, cache *CachingStore, _ string, race func()) {
			backing.onListOnce = race
			if _, err := cache.List(ListQuery{Live: true, AllowScan: true}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
		{"parent_list_refetch", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, race func()) {
			parent := reparentOutOfBand(t, backing, cache, id)
			backing.onGetOnce = race
			if _, err := cache.List(ListQuery{ParentID: parent}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
		{"parent_list_missing_row", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, race func()) {
			parent := reparentOutOfBand(t, backing, cache, id)
			backing.onGetOnce = func() {
				race()
				backing.notFoundNextGet = true
			}
			if _, err := cache.List(ListQuery{ParentID: parent}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
		{"live_list_missing_row_gone", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, race func()) {
			status := "in_progress"
			if err := backing.Update(id, UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("out-of-band Update: %v", err)
			}
			backing.onGetOnce = func() {
				race()
				backing.notFoundNextGet = true
			}
			if _, err := cache.List(ListQuery{Live: true, Status: "open"}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
		{"live_list_missing_row_refetch", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string, race func()) {
			// Out of band the row leaves the live query, so the refresh
			// point-reads the cached row it no longer lists.
			status := "in_progress"
			if err := backing.Update(id, UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("out-of-band Update: %v", err)
			}
			backing.onGetOnce = race
			if _, err := cache.List(ListQuery{Live: true, Status: "open"}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
	}
	for _, path := range paths {
		for _, lagged := range []bool{false, true} {
			name := path.name + "/event_redirty"
			if lagged {
				name = path.name + "/lagged_refetch"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				backing := &casBackingStore{Store: NewMemStore()}
				cache := newConditionalCacheForTest(t, backing)
				b, err := cache.Create(Bead{Title: "fenced"})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				raced := false
				path.read(t, backing, cache, b.ID, func() {
					raced = true
					clearWriteFence(t, backing, cache, b.ID, lagged)
				})
				if !raced {
					t.Fatal("the reader never reached the backing; the race was not exercised")
				}
				rows, rev := mustCleanCensus(t, cache)
				if row := censusRow(t, rows, b.ID); row.Metadata["n"] != "1" {
					t.Fatalf("census row n=%q, want 1; the pre-write read was installed", row.Metadata["n"])
				}
				if w := cache.WriteRev(b.ID); !coveredBy(w, rev) {
					t.Fatalf("WriteRev %+v not covered by CacheRev %+v", w, rev)
				}
			})
		}
	}
}

// reparentOutOfBand files id under a parent through the cache, then moves it
// to another parent behind the cache's back, so a List of the first parent
// point-reads the cached child it no longer returns. It returns the first
// parent's id.
func reparentOutOfBand(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) string {
	t.Helper()
	parent, err := cache.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	other, err := cache.Create(Bead{Title: "other parent"})
	if err != nil {
		t.Fatalf("Create other parent: %v", err)
	}
	if err := cache.Update(id, UpdateOpts{ParentID: &parent.ID}); err != nil {
		t.Fatalf("Update parent: %v", err)
	}
	if err := backing.Update(id, UpdateOpts{ParentID: &other.ID}); err != nil {
		t.Fatalf("out-of-band reparent: %v", err)
	}
	return parent.ID
}

// TestCachingStorePrimeSkipsRowsWrittenAfterSnapshot pins that Prime and
// PrimeActive, merging a snapshot taken before a local write, leave the
// written id to that write instead of installing the snapshot row and clearing
// the write's beadSeq fence.
func TestCachingStorePrimeSkipsRowsWrittenAfterSnapshot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		prime func(*CachingStore) error
	}{
		{"prime", func(c *CachingStore) error { return c.Prime(context.Background()) }},
		{"prime_active", func(c *CachingStore) error { return c.PrimeActive() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "evicted"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			// Mid-snapshot, a fenced write lands and its lagged refetch leaves
			// the row evicted and dirty.
			backing.onListOnce = func() {
				pre, err := backing.Store.Get(b.ID)
				if err != nil {
					t.Errorf("backing Get: %v", err)
					return
				}
				backing.staleNextGet = &pre
				if ok, err := cache.CompareAndSetMetadataKey(b.ID, "n", "", "1"); !ok || err != nil {
					t.Errorf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
				}
			}
			if err := tc.prime(cache); err != nil {
				t.Fatalf("prime: %v", err)
			}
			cache.mu.RLock()
			_, installed := cache.beads[b.ID]
			_, fenced := cache.beadSeq[b.ID]
			cache.mu.RUnlock()
			if installed || !fenced {
				t.Fatalf("after prime: snapshot row installed=%v beadSeq kept=%v, want false and true", installed, fenced)
			}
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata["n"] != "1" {
				t.Fatalf("Get n=%q, want the committed 1", got.Metadata["n"])
			}
		})
	}
}

// depsBacking is a MemStore whose Get carries dependency fields, like a bd
// point read, with a one-shot hook before each read.
type depsBacking struct {
	*MemStore
	beforeGetOnce func()
}

func (s *depsBacking) Get(id string) (Bead, error) {
	if hook := s.beforeGetOnce; hook != nil {
		s.beforeGetOnce = nil
		hook()
	}
	b, err := s.MemStore.Get(id)
	if err != nil {
		return b, err
	}
	deps, err := s.DepList(id, "down")
	if err != nil {
		return Bead{}, err
	}
	b.Dependencies = deps
	return b, nil
}

// newBlockerFixture seeds an open blocker and a second bead over a backing
// whose Get carries dependencies; blocked makes the second bead depend on it.
func newBlockerFixture(t *testing.T, blocked bool) (*depsBacking, *CachingStore, Bead, Bead) {
	t.Helper()
	backing := &depsBacking{MemStore: NewMemStore()}
	blocker, err := backing.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := backing.Create(Bead{Title: "dependent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if blocked {
		if err := backing.DepAdd(b.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd: %v", err)
		}
	}
	return backing, newConditionalCacheForTest(t, backing), blocker, b
}

// assertInstalledAndBlocked checks that id's fenced write installed a clean
// row and that readiness still sees id as blocked.
func assertInstalledAndBlocked(t *testing.T, cache *CachingStore, id string) {
	t.Helper()
	cache.mu.RLock()
	_, dirty := cache.dirty[id]
	cache.mu.RUnlock()
	if dirty {
		t.Fatal("the eager refetch did not install; the install was not exercised")
	}
	ready, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, b := range ready {
		if b.ID == id {
			t.Fatalf("blocked bead %s served as ready after the fenced write", id)
		}
	}
}

// TestCachingStoreConditionalInstallPrefersRowDependencies pins that a
// refetched row carrying dependency fields installs those, not the evicted
// row's: another writer may have added a blocking edge right after the commit,
// and its event can be dropped by the recency rule.
func TestCachingStoreConditionalInstallPrefersRowDependencies(t *testing.T) {
	t.Parallel()

	backing, cache, blocker, b := newBlockerFixture(t, false)
	backing.beforeGetOnce = func() {
		if err := backing.DepAdd(b.ID, blocker.ID, "blocks"); err != nil {
			t.Errorf("DepAdd: %v", err)
		}
	}
	if ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "v"); !ok || err != nil {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
	}
	assertInstalledAndBlocked(t, cache, b.ID)
}

// TestCachingStoreConditionalInstallTakesRowDependenciesWhenNoneCached pins
// that a row the cache no longer held at eviction installs the dependencies
// its refetch carries.
func TestCachingStoreConditionalInstallTakesRowDependenciesWhenNoneCached(t *testing.T) {
	t.Parallel()

	_, cache, _, b := newBlockerFixture(t, true)
	// A lost CAS evicts the row and its cached dependencies.
	if ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "stale", "v"); ok || err != nil {
		t.Fatalf("losing CompareAndSetMetadataKey = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "v"); !ok || err != nil {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
	}
	assertInstalledAndBlocked(t, cache, b.ID)
}

// TestCachingStoreIdempotentConditionalWriteInstalls pins the unmoved-revision
// rule on a backend that does not re-stamp no-op writes: the refetch installs
// when the evicted row was clean at expectedRevision, and stays a miss when
// the cached row was dirty or at another revision.
func TestCachingStoreIdempotentConditionalWriteInstalls(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		prepare   func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string)
		wantClean bool
	}{
		{"clean_at_expected", func(*testing.T, *casBackingStore, *CachingStore, string) {}, true},
		{"cached_at_other_revision", func(t *testing.T, backing *casBackingStore, _ *CachingStore, id string) {
			if err := backing.SetMetadataBatch(id, map[string]string{"o": "1"}); err != nil {
				t.Fatalf("out-of-band SetMetadataBatch: %v", err)
			}
		}, false},
		{"cached_dirty", func(t *testing.T, backing *casBackingStore, cache *CachingStore, id string) {
			backing.errOverride = errors.New("bd: connection reset mid-write")
			if _, err := cache.CompareAndSetMetadataKey(id, "x", "", "y"); err == nil {
				t.Fatal("CompareAndSetMetadataKey: want the injected ambiguous error")
			}
			backing.errOverride = nil
		}, false},
	}
	verbs := []struct {
		name  string
		setup func(t *testing.T, cache *CachingStore, id string)
		write func(cache *CachingStore, id string, revision int64) error
	}{
		{"update_if_match", func(*testing.T, *CachingStore, string) {}, func(cache *CachingStore, id string, revision int64) error {
			title := "idempotent"
			return cache.UpdateIfMatch(id, revision, UpdateOpts{Title: &title})
		}},
		{"close_if_match", func(t *testing.T, cache *CachingStore, id string) {
			got, err := cache.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := cache.CloseIfMatch(id, got.Revision); err != nil {
				t.Fatalf("CloseIfMatch: %v", err)
			}
		}, func(cache *CachingStore, id string, revision int64) error {
			return cache.CloseIfMatch(id, revision)
		}},
	}
	for _, verb := range verbs {
		for _, tc := range cases {
			t.Run(verb.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				backing := &casBackingStore{Store: NewMemStore(), noopKeepsRevision: true}
				cache := newConditionalCacheForTest(t, backing)
				b, err := cache.Create(Bead{Title: "idempotent"})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				verb.setup(t, cache, b.ID)
				tc.prepare(t, backing, cache, b.ID)
				current, err := backing.Store.Get(b.ID)
				if err != nil {
					t.Fatalf("backing Get: %v", err)
				}
				casCalls := backing.casCalls
				if err := verb.write(cache, b.ID, current.Revision); err != nil {
					t.Fatalf("write: %v", err)
				}
				if backing.casCalls == casCalls {
					t.Fatal("the fenced write never reached the backing")
				}
				if after, _ := backing.Store.Get(b.ID); after.Revision != current.Revision {
					t.Fatal("the backing re-stamped the no-op write; the fixture is wrong")
				}
				cache.mu.RLock()
				_, dirty := cache.dirty[b.ID]
				cache.mu.RUnlock()
				if dirty == tc.wantClean {
					t.Fatalf("dirty=%v after an idempotent write, want clean=%v", dirty, tc.wantClean)
				}
			})
		}
	}
}

// TestCachingStoreCloseIfMatchRejectsLaggedRowAtExpectedRevision pins the
// revision half of CloseIfMatch's install predicate: a refetch still at
// expectedRevision is a pre-write read unless the evicted row was clean there,
// even when it already shows the closed status.
func TestCachingStoreCloseIfMatchRejectsLaggedRowAtExpectedRevision(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	b, err := cache.Create(Bead{Title: "reclosed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Closed out of band: the cache still holds the open row.
	if err := backing.Close(b.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	pre, err := backing.Store.Get(b.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	backing.staleNextGet = &pre
	if err := cache.CloseIfMatch(b.ID, pre.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[b.ID]
	cache.mu.RUnlock()
	if !dirty {
		t.Fatal("a lagged refetch at expectedRevision was installed")
	}
}

// TestCachingStoreConditionalEvictionPriorCoversEveryFence pins that the
// eviction's prior reflects each fence an earlier mutation can leave: a
// tombstone, a beadSeq, and a write revision whose beadSeq was cleared.
func TestCachingStoreConditionalEvictionPriorCoversEveryFence(t *testing.T) {
	t.Parallel()

	const fence = uint64(1 << 20)
	cases := []struct {
		name string
		set  func(c *CachingStore, id string)
	}{
		{"bead_seq", func(c *CachingStore, id string) { c.beadSeq[id] = fence }},
		{"deleted_seq", func(c *CachingStore, id string) { c.deletedSeq[id] = fence }},
		{"write_seq", func(c *CachingStore, id string) { c.writeSeq[id] = fence }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := newConditionalCacheForTest(t, NewMemStore())
			b, err := cache.Create(Bead{Title: "prior"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			cache.mu.Lock()
			delete(cache.beadSeq, b.ID)
			delete(cache.writeSeq, b.ID)
			tc.set(cache, b.ID)
			cache.mu.Unlock()
			if ev := cache.evictForConditionalWrite(b.ID); ev.prior != fence {
				t.Fatalf("prior = %d, want the %s fence %d", ev.prior, tc.name, fence)
			}
		})
	}
}

// TestCachingStoreSuppressedChurnFencesOnWriteSeq pins that the overlay's
// suppressed-row churn check treats a local write after the snapshot as churn
// even once its beadSeq is gone, so the serve never omits a row a newer local
// write re-established.
func TestCachingStoreSuppressedChurnFencesOnWriteSeq(t *testing.T) {
	t.Parallel()

	cache := newConditionalCacheForTest(t, NewMemStore())
	b, err := cache.Create(Bead{Title: "suppressed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	startSeq := cache.mutationSeq
	delete(cache.beadSeq, b.ID)
	cache.writeSeq[b.ID] = startSeq + 1
	cache.dirty[b.ID] = struct{}{}
	suppressed := map[string]struct{}{b.ID: {}}
	if !cache.retrySuppressedChurnLocked(suppressed, startSeq) {
		t.Fatal("a suppressed row written after the snapshot was not treated as churn")
	}
	if _, still := suppressed[b.ID]; still {
		t.Fatal("the churned row stayed suppressed")
	}
}

// TestCachingStoreReconcileAfterConditionalWriteEmitsNoCreate pins that a
// reconcile after a successful fenced write finds the row installed, so it
// does not announce the written bead as newly created.
func TestCachingStoreReconcileAfterConditionalWriteEmitsNoCreate(t *testing.T) {
	t.Parallel()

	var notes []cacheWriteNotification
	backing := NewMemStore()
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "reconciled"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "v"); !ok || err != nil {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v)", ok, err)
	}
	notes = nil
	cache.runReconciliation()
	for _, n := range notes {
		if n.beadID == b.ID && n.eventType == "bead.created" {
			t.Fatalf("reconcile announced %s as created after a fenced write", b.ID)
		}
	}
}
