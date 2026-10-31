package beads

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// carryNoteRecorder records every cache notification as "type id" pairs.
type carryNoteRecorder struct {
	mu    sync.Mutex
	notes []string
}

func (r *carryNoteRecorder) onChange(eventType, beadID string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, eventType+" "+beadID)
}

func (r *carryNoteRecorder) count(eventType, beadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	note := eventType + " " + beadID
	n := 0
	for _, x := range r.notes {
		if x == note {
			n++
		}
	}
	return n
}

// reset drops the notes recorded so far.
func (r *carryNoteRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = nil
}

// all returns the notes recorded so far, in order.
func (r *carryNoteRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notes...)
}

// carryHeldOpen reports whether cs holds id and its status when it does.
func carryHeldOpen(cs *CachingStore, id string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	b, ok := cs.beads[id]
	return b.Status, ok
}

// hideFromListStore wraps a Store and omits one id from every List while its
// hide flag is set: a listing that drops an alive row.
type hideFromListStore struct {
	Store
	hideID string
	hide   bool
}

func (s *hideFromListStore) List(query ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	if !s.hide {
		return rows, nil
	}
	kept := make([]Bead, 0, len(rows))
	for _, b := range rows {
		if b.ID != s.hideID {
			kept = append(kept, b)
		}
	}
	return kept, nil
}

// TestCachingStorePrimeAnnouncesExternalCloseOnce pins that a row the cache
// holds open and another process closes behind it is announced closed exactly
// once when a full Prime sees the close, in the controller's startup order
// (PrimeActive, then the async full Prime: cmd/gc/api_state.go
// primeThenStartReconciler) and on a second full Prime over a live cache, as
// when a reconcile pass sees it instead. The Prime's active-only scan omits
// the row, the replace carries it, and the settle (a live list that includes
// closed rows, then two reconcile passes, as the close-notification
// conformance suite settles) announces the close once.
func TestCachingStorePrimeAnnouncesExternalCloseOnce(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		first func(*CachingStore) error // fills the cache with the open row
		then  func(*CachingStore) error // sees the cache after the close behind it
	}{
		{
			"PrimeActiveThenPrime", func(cs *CachingStore) error { return cs.PrimeActive() },
			func(cs *CachingStore) error { return cs.Prime(ctx) },
		},
		{
			"PrimeThenPrime", func(cs *CachingStore) error { return cs.Prime(ctx) },
			func(cs *CachingStore) error { return cs.Prime(ctx) },
		},
		{
			"PrimeActiveThenReconcile_control", func(cs *CachingStore) error { return cs.PrimeActive() },
			func(cs *CachingStore) error { cs.ReconcileNowForTest(); return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemStore()
			seed, err := mem.Create(Bead{Title: "closed behind the cache"})
			if err != nil {
				t.Fatalf("Create seed: %v", err)
			}
			bystander, err := mem.Create(Bead{Title: "stays open"})
			if err != nil {
				t.Fatalf("Create bystander: %v", err)
			}
			rec := &carryNoteRecorder{}
			cs := NewCachingStoreForTest(mem, rec.onChange)
			if err := tc.first(cs); err != nil {
				t.Fatalf("first: %v", err)
			}
			if status, held := carryHeldOpen(cs, seed.ID); !held || status != "open" {
				t.Fatalf("cache holds %s = (%q, %v) before the close, want open; the case is vacuous", seed.ID, status, held)
			}

			// Another process closes the bead in the shared store.
			if err := mem.Close(seed.ID); err != nil {
				t.Fatalf("backing Close: %v", err)
			}
			if err := tc.then(cs); err != nil {
				t.Fatalf("then: %v", err)
			}
			if _, err := cs.List(ListQuery{AllowScan: true, IncludeClosed: true, Live: true}); err != nil {
				t.Fatalf("live List: %v", err)
			}
			cs.ReconcileNowForTest()
			cs.ReconcileNowForTest()

			if got, err := cs.Get(seed.ID); err != nil || got.Status != "closed" {
				t.Errorf("Get(%s) = (%q, %v), want closed", seed.ID, got.Status, err)
			}
			if got := rec.count("bead.closed", bystander.ID); got != 0 {
				t.Errorf("bead.closed for open bystander %s = %d, want 0 (notifications: %v)", bystander.ID, got, rec.all())
			}
			if got := rec.count("bead.closed", seed.ID); got != 1 {
				t.Errorf("bead.closed for %s = %d, want exactly 1 (closed behind the cache, then seen by %s; notifications: %v)", seed.ID, got, tc.name, rec.all())
			}
		})
	}
}

// TestCachingStorePrimeReplaceCarriesScanMissingOpenRows pins what a full
// Prime's replace does with a cached open row the active-only scan does not
// return: the row is carried, not dropped, and the next reconcile's per-id
// re-verify (recoverMissingFromList) decides its fate — a close or delete
// behind the cache is announced exactly once, and an alive row one listing
// merely omitted stays cached open with no spurious events.
func TestCachingStorePrimeReplaceCarriesScanMissingOpenRows(t *testing.T) {
	// A row deleted behind the cache is absent from the scan like a closed
	// one, and the reconcile after the Prime announces the same synthetic
	// bead.closed it would without the Prime.
	t.Run("deleted_behind_cache", func(t *testing.T) {
		mem := NewMemStore()
		seed, err := mem.Create(Bead{Title: "deleted behind the cache"})
		if err != nil {
			t.Fatalf("Create seed: %v", err)
		}
		bystander, err := mem.Create(Bead{Title: "stays"})
		if err != nil {
			t.Fatalf("Create bystander: %v", err)
		}
		rec := &carryNoteRecorder{}
		cs := NewCachingStoreForTest(mem, rec.onChange)
		if err := cs.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if err := mem.Delete(seed.ID); err != nil {
			t.Fatalf("backing Delete: %v", err)
		}
		if err := cs.Prime(context.Background()); err != nil {
			t.Fatalf("second Prime: %v", err)
		}
		cs.ReconcileNowForTest()
		if got := rec.count("bead.closed", seed.ID); got != 1 {
			t.Errorf("bead.closed for %s = %d, want exactly 1 (synthetic, from the reconcile's re-verify)", seed.ID, got)
		}
		if got := rec.count("bead.closed", bystander.ID); got != 0 {
			t.Errorf("bead.closed for bystander %s = %d, want 0", bystander.ID, got)
		}
	})

	// A row this cache wrote longer ago than the 5s local-write recency
	// window is carried like any other scan-missing open row: its close
	// behind the cache is announced by the next reconcile pass.
	for _, tc := range []struct {
		name     string
		liveList bool
	}{
		{"reconcile", false},
		{"live_list_then_reconcile", true},
	} {
		t.Run("aged_local_write/"+tc.name, func(t *testing.T) {
			mem := NewMemStore()
			rec := &carryNoteRecorder{}
			cs := NewCachingStoreForTest(mem, rec.onChange)
			seed, err := cs.Create(Bead{Title: "written here, closed elsewhere"})
			if err != nil {
				t.Fatalf("Create seed: %v", err)
			}
			bystander, err := cs.Create(Bead{Title: "stays open"})
			if err != nil {
				t.Fatalf("Create bystander: %v", err)
			}
			if err := cs.SetMetadata(seed.ID, "k", "v"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			ageLocalWriteBy(cs, seed.ID, 10*time.Second)
			ageLocalWriteBy(cs, bystander.ID, 10*time.Second)
			if err := mem.Close(seed.ID); err != nil {
				t.Fatalf("backing Close: %v", err)
			}
			if err := cs.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			// Count from the settle onward, past the Create announcements.
			rec.reset()
			if tc.liveList {
				if _, err := cs.List(ListQuery{AllowScan: true, IncludeClosed: true, Live: true}); err != nil {
					t.Fatalf("live List: %v", err)
				}
			}
			cs.ReconcileNowForTest()
			cs.ReconcileNowForTest()
			if got := rec.count("bead.closed", seed.ID); got != 1 {
				t.Errorf("bead.closed for %s = %d, want exactly 1 (aged local write carried by the Prime, close announced by the reconcile)", seed.ID, got)
			}
			if got := rec.count("bead.created", seed.ID); got != 0 {
				t.Errorf("bead.created for %s = %d, want 0 (a carried row is not a transition)", seed.ID, got)
			}
			if got := rec.count("bead.closed", bystander.ID); got != 0 {
				t.Errorf("bead.closed for open bystander %s = %d, want 0", bystander.ID, got)
			}
			if got, err := cs.Get(seed.ID); err != nil || got.Status != "closed" {
				t.Errorf("Get(%s) = (%q, %v), want closed", seed.ID, got.Status, err)
			}
		})
	}

	// An alive row one listing merely omits is not a close: the Prime
	// carries it, and a later reconcile that lists it again absorbs it back
	// (or recovery merges it back while it stays omitted) with no
	// bead.created and no bead.closed.
	for _, tc := range []struct {
		name                 string
		hideThroughReconcile bool
	}{
		{"listed_again", false},
		{"still_omitted", true},
	} {
		t.Run("alive_missing_from_scan/"+tc.name, func(t *testing.T) {
			mem := NewMemStore()
			seed, err := mem.Create(Bead{Title: "omitted by one listing"})
			if err != nil {
				t.Fatalf("Create seed: %v", err)
			}
			bystander, err := mem.Create(Bead{Title: "listed"})
			if err != nil {
				t.Fatalf("Create bystander: %v", err)
			}
			backing := &hideFromListStore{Store: mem, hideID: seed.ID}
			rec := &carryNoteRecorder{}
			cs := NewCachingStoreForTest(backing, rec.onChange)
			if err := cs.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			backing.hide = true
			if err := cs.Prime(context.Background()); err != nil {
				t.Fatalf("second Prime: %v", err)
			}
			if status, held := carryHeldOpen(cs, seed.ID); !held || status != "open" {
				t.Fatalf("after the omitting Prime, cache holds %s = (%q, %v), want carried open", seed.ID, status, held)
			}
			rec.reset()
			backing.hide = tc.hideThroughReconcile
			cs.ReconcileNowForTest()
			cs.ReconcileNowForTest()
			if status, held := carryHeldOpen(cs, seed.ID); !held || status != "open" {
				t.Errorf("after the reconciles, cache holds %s = (%q, %v), want held open", seed.ID, status, held)
			}
			for _, note := range []string{"bead.created", "bead.closed"} {
				if got := rec.count(note, seed.ID); got != 0 {
					t.Errorf("%s for %s = %d, want 0 (an omitted alive row is not a transition)", note, seed.ID, got)
				}
			}
			if got := rec.count("bead.closed", bystander.ID); got != 0 {
				t.Errorf("bead.closed for bystander %s = %d, want 0", bystander.ID, got)
			}
		})
	}
}

// TestCachingStorePrimeCarriedRowReadsBeforeReconcile pins what reads see
// between a full Prime that carried a row closed behind the cache and the
// reconcile that re-verifies it: the carried row is marked dirty, so Get
// reads the backing, the strict cache-only handles decline, the overlay
// readers refresh the row before they answer, and the Prime asks for the
// verifying reconcile rather than leaving it a full cadence away.
func TestCachingStorePrimeCarriedRowReadsBeforeReconcile(t *testing.T) {
	ctx := context.Background()
	// primeOverClose fills the cache with an open seed and an open bystander
	// as the controller's startup does, closes the seed behind the cache, and
	// runs the full Prime that carries it.
	primeOverClose := func(t *testing.T) (*CachingStore, *carryNoteRecorder, Bead, Bead) {
		t.Helper()
		mem := NewMemStore()
		seed, err := mem.Create(Bead{Title: "closed behind the cache"})
		if err != nil {
			t.Fatalf("Create seed: %v", err)
		}
		bystander, err := mem.Create(Bead{Title: "stays open"})
		if err != nil {
			t.Fatalf("Create bystander: %v", err)
		}
		rec := &carryNoteRecorder{}
		cs := NewCachingStoreForTest(mem, rec.onChange)
		if err := cs.PrimeActive(); err != nil {
			t.Fatalf("PrimeActive: %v", err)
		}
		if err := mem.Close(seed.ID); err != nil {
			t.Fatalf("backing Close: %v", err)
		}
		if err := cs.Prime(ctx); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if status, held := carryHeldOpen(cs, seed.ID); !held || status != "open" {
			t.Fatalf("after the Prime, cache holds %s = (%q, %v), want carried open; the case is vacuous", seed.ID, status, held)
		}
		return cs, rec, seed, bystander
	}
	// offers reports whether rows holds id.
	offers := func(rows []Bead, id string) bool {
		for _, b := range rows {
			if b.ID == id {
				return true
			}
		}
		return false
	}

	t.Run("get_reads_the_backing", func(t *testing.T) {
		cs, rec, seed, _ := primeOverClose(t)
		if got, err := cs.Get(seed.ID); err != nil || got.Status != "closed" {
			t.Errorf("Get(%s) right after the Prime = (%q, %v), want closed", seed.ID, got.Status, err)
		}
		cs.ReconcileNowForTest()
		if got := rec.count("bead.closed", seed.ID); got != 1 {
			t.Errorf("bead.closed for %s = %d after the Get and a reconcile, want exactly 1", seed.ID, got)
		}
	})

	t.Run("strict_cache_reads_decline", func(t *testing.T) {
		cs, _, seed, _ := primeOverClose(t)
		if rows, ok := cs.CachedReady(); ok && offers(rows, seed.ID) {
			t.Errorf("CachedReady right after the Prime offers %s, closed behind the cache", seed.ID)
		}
		handles := cs.Handles()
		rows, err := handles.Cached.Ready(ReadyQuery{})
		switch {
		case errors.Is(err, ErrCacheUnavailable):
		case err != nil:
			t.Errorf("Cached.Ready right after the Prime: %v, want rows or ErrCacheUnavailable", err)
		case offers(rows, seed.ID):
			t.Errorf("Cached.Ready right after the Prime offers %s, closed behind the cache", seed.ID)
		}
		rows, err = handles.Cached.List(ListQuery{AllowScan: true})
		switch {
		case errors.Is(err, ErrCacheUnavailable):
		case err != nil:
			t.Errorf("Cached.List right after the Prime: %v, want rows or ErrCacheUnavailable", err)
		case offers(rows, seed.ID):
			t.Errorf("Cached.List right after the Prime lists %s open, closed behind the cache", seed.ID)
		}
	})

	t.Run("overlay_ready_refreshes_the_row", func(t *testing.T) {
		cs, rec, seed, bystander := primeOverClose(t)
		rows, err := cs.ReadyPreferCache(ReadyQuery{})
		if err != nil {
			t.Fatalf("ReadyPreferCache: %v", err)
		}
		if offers(rows, seed.ID) || !offers(rows, bystander.ID) {
			t.Errorf("ReadyPreferCache right after the Prime = %v, want %s ready and %s not", beadIDsOf(rows), bystander.ID, seed.ID)
		}
		cs.ReconcileNowForTest()
		if got := rec.count("bead.closed", seed.ID); got != 1 {
			t.Errorf("bead.closed for %s = %d after the read and a reconcile, want exactly 1", seed.ID, got)
		}
	})

	t.Run("verifying_reconcile_is_due", func(t *testing.T) {
		cs, rec, seed, bystander := primeOverClose(t)
		cs.ReconcileIfDueForTest()
		if got := rec.count("bead.closed", seed.ID); got != 1 {
			t.Errorf("bead.closed for %s = %d after the reconcile due right after the Prime, want exactly 1", seed.ID, got)
		}
		if got := rec.count("bead.closed", bystander.ID); got != 0 {
			t.Errorf("bead.closed for open bystander %s = %d, want 0", bystander.ID, got)
		}
	})

	// A Prime that carries nothing asks for no reconcile: the next one stays
	// a cadence away.
	t.Run("nothing_carried_requests_nothing", func(t *testing.T) {
		mem := NewMemStore()
		if _, err := mem.Create(Bead{Title: "stays open"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		cs := NewCachingStoreForTest(mem, nil)
		if err := cs.PrimeActive(); err != nil {
			t.Fatalf("PrimeActive: %v", err)
		}
		if err := cs.Prime(ctx); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if got := cs.Stats().ReconcileRequests; got != 0 {
			t.Errorf("ReconcileRequests after a Prime that carried nothing = %d, want 0", got)
		}
		if delay := cs.nextReconcileDelay(cs.clockNow()); delay <= 0 {
			t.Errorf("next reconcile after a Prime that carried nothing is due in %v, want a cadence away", delay)
		}
	})
}
