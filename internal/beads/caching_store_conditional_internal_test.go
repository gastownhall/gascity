package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// casBackingStore wraps a Store for the conditional-write cache tests. It
// counts Get and conditional-write calls (the anti-vacuity probes), can fail
// or lag the next Get (forcing the refresh-failure and visibility-lag paths),
// and can hide closed beads from Get (the CloseIfMatch tolerance carve-out).
// Because interface embedding does not promote optional methods, the four
// ConditionalWriter verbs are defined explicitly, delegating to the wrapped
// store; errOverride, when set, replaces the delegated result so error-class
// handling can be exercised without a faulty real backend.
type casBackingStore struct {
	Store
	getCalls          int
	casCalls          int
	failNextGet       bool
	staleNextGet      *Bead
	hideClosedFromGet bool
	errOverride       error
	// onListOnce fires once after the wrapped List collects its rows and
	// before they return to the cache — the window in which a concurrent
	// scan's merge-back races writes that landed mid-scan.
	onListOnce func()
}

func (s *casBackingStore) List(query ListQuery) ([]Bead, error) {
	items, err := s.Store.List(query)
	if hook := s.onListOnce; hook != nil {
		s.onListOnce = nil
		hook()
	}
	return items, err
}

func (s *casBackingStore) Get(id string) (Bead, error) {
	s.getCalls++
	if s.failNextGet {
		s.failNextGet = false
		return Bead{}, errors.New("injected refresh failure")
	}
	if s.staleNextGet != nil {
		stale := cloneBead(*s.staleNextGet)
		s.staleNextGet = nil
		return stale, nil
	}
	b, err := s.Store.Get(id)
	if err == nil && s.hideClosedFromGet && b.Status == "closed" {
		return Bead{}, ErrNotFound
	}
	return b, err
}

func (s *casBackingStore) delegate() (ConditionalWriter, bool) {
	return ConditionalWriterFor(s.Store)
}

func (s *casBackingStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	s.casCalls++
	if s.errOverride != nil {
		return s.errOverride
	}
	w, ok := s.delegate()
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	return w.UpdateIfMatch(id, expectedRevision, opts)
}

func (s *casBackingStore) CloseIfMatch(id string, expectedRevision int64) error {
	s.casCalls++
	if s.errOverride != nil {
		return s.errOverride
	}
	w, ok := s.delegate()
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	return w.CloseIfMatch(id, expectedRevision)
}

func (s *casBackingStore) DeleteIfMatch(id string, expectedRevision int64) error {
	s.casCalls++
	if s.errOverride != nil {
		return s.errOverride
	}
	w, ok := s.delegate()
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	return w.DeleteIfMatch(id, expectedRevision)
}

func (s *casBackingStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	s.casCalls++
	if s.errOverride != nil {
		return false, s.errOverride
	}
	w, ok := s.delegate()
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	return w.CompareAndSetMetadataKey(id, key, expected, next)
}

// assertConditionalEvicted checks the exact evict composition: entry and deps
// gone, dirty set (so the next Get re-reads the backing and re-primes), and —
// critically — deletedSeq NOT stamped, which would short-circuit Get to
// ErrNotFound without ever consulting the backing.
func assertConditionalEvicted(t *testing.T, c *CachingStore, id string) {
	t.Helper()
	c.mu.RLock()
	_, inBeads := c.beads[id]
	_, dirty := c.dirty[id]
	_, deleted := c.deletedSeq[id]
	c.mu.RUnlock()
	if inBeads {
		t.Fatalf("bead %s still cached after evict", id)
	}
	if !dirty {
		t.Fatalf("bead %s not marked dirty after evict (next Get would miss the backing re-read)", id)
	}
	if deleted {
		t.Fatalf("bead %s has deletedSeq stamped by evict — Get would fabricate ErrNotFound for a live bead", id)
	}
}

func newConditionalCacheForTest(t *testing.T, backing Store) *CachingStore {
	t.Helper()
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache
}

// TestCachingStoreCASRetryLoopConverges is the merge gate of DESIGN §8.5: a
// consumer retry loop over Get→conditional-write must converge through the
// cache in both failure shapes, instead of livelocking on a stale cached
// revision. Anti-vacuity: both legs prove the pre-evict reads were
// cache-served, so "the next Get hits the backing" is a real transition.
func TestCachingStoreCASRetryLoopConverges(t *testing.T) {
	t.Parallel()

	t.Run("refresh_failure_evicts_and_retry_converges", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "cas-converge"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		pre := backing.getCalls
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if backing.getCalls != pre {
			t.Fatalf("pre-evict Get consulted the backing (%d -> %d calls); the cache is not primed and the test is vacuous",
				pre, backing.getCalls)
		}

		backing.failNextGet = true
		title := "fenced"
		if err := cache.UpdateIfMatch(b.ID, got.Revision, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("UpdateIfMatch at current revision: %v (the CAS succeeded; only the refresh was injected to fail)", err)
		}
		assertConditionalEvicted(t, cache, b.ID)

		pre = backing.getCalls
		fresh, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after evict: %v", err)
		}
		if backing.getCalls == pre {
			t.Fatal("post-evict Get did not consult the backing")
		}
		if fresh.Revision <= got.Revision {
			t.Fatalf("post-evict Get returned revision %d, want > %d (the post-write revision)", fresh.Revision, got.Revision)
		}
		if fresh.Title != title {
			t.Fatalf("post-evict Get returned title %q, want %q", fresh.Title, title)
		}

		retry := "fenced-retry"
		if err := cache.UpdateIfMatch(b.ID, fresh.Revision, UpdateOpts{Title: &retry}); err != nil {
			t.Fatalf("retry with the refreshed revision must converge: %v", err)
		}
	})

	t.Run("stale_cache_precondition_surfaces_evicts_and_retry_converges", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		backing := &casBackingStore{Store: mem}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "cas-stale"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		// Out-of-band mutation directly against the inner store: the cache
		// keeps serving the now-stale revision.
		if err := mem.SetMetadata(b.ID, "k", "out-of-band"); err != nil {
			t.Fatalf("out-of-band SetMetadata: %v", err)
		}
		live, err := mem.Get(b.ID)
		if err != nil {
			t.Fatalf("backing Get: %v", err)
		}

		pre := backing.getCalls
		stale, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if backing.getCalls != pre {
			t.Fatal("pre-evict Get consulted the backing; the staleness setup is vacuous")
		}
		if stale.Revision >= live.Revision {
			t.Fatalf("cached revision %d is not stale against the backing's %d", stale.Revision, live.Revision)
		}

		title := "stale-write"
		err = cache.UpdateIfMatch(b.ID, stale.Revision, UpdateOpts{Title: &title})
		var pfe *PreconditionFailedError
		if !errors.As(err, &pfe) {
			t.Fatalf("stale fenced write: got %v, want *PreconditionFailedError", err)
		}
		if pfe.Expected != stale.Revision {
			t.Fatalf("PreconditionFailedError.Expected = %d, want %d (forwarded untouched)", pfe.Expected, stale.Revision)
		}
		if pfe.Current != live.Revision {
			t.Fatalf("PreconditionFailedError.Current = %d, want %d (forwarded untouched)", pfe.Current, live.Revision)
		}
		assertConditionalEvicted(t, cache, b.ID)

		pre = backing.getCalls
		fresh, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after precondition evict: %v", err)
		}
		if backing.getCalls == pre {
			t.Fatal("post-evict Get did not consult the backing")
		}
		if fresh.Revision != live.Revision {
			t.Fatalf("post-evict Get returned revision %d, want the live %d", fresh.Revision, live.Revision)
		}

		if err := cache.UpdateIfMatch(b.ID, fresh.Revision, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("retry with the refreshed revision must converge: %v", err)
		}
	})
}

func TestCachingStoreConditionalWriteSuccessRefreshesCache(t *testing.T) {
	t.Parallel()

	t.Run("update_if_match", func(t *testing.T) {
		t.Parallel()
		var notes []cacheWriteNotification
		backing := NewMemStore()
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		b, err := cache.Create(Bead{Title: "upd"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		notes = nil
		title := "applied"
		if err := cache.UpdateIfMatch(b.ID, got.Revision, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("UpdateIfMatch: %v", err)
		}
		cached, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after fenced update: %v", err)
		}
		fresh, err := backing.Get(b.ID)
		if err != nil {
			t.Fatalf("backing Get: %v", err)
		}
		if cached.Title != title {
			t.Fatalf("cached title = %q, want %q", cached.Title, title)
		}
		if cached.Revision != fresh.Revision {
			t.Fatalf("cached revision = %d, backing = %d (refresh must adopt the post-write revision)", cached.Revision, fresh.Revision)
		}
		if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != b.ID {
			t.Fatalf("notifications = %+v, want exactly one bead.updated for %s", notes, b.ID)
		}
	})

	t.Run("close_if_match", func(t *testing.T) {
		t.Parallel()
		var notes []cacheWriteNotification
		backing := NewMemStore()
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		b, err := cache.Create(Bead{Title: "cls"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		notes = nil
		if err := cache.CloseIfMatch(b.ID, got.Revision); err != nil {
			t.Fatalf("CloseIfMatch: %v", err)
		}
		cached, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after fenced close: %v", err)
		}
		fresh, err := backing.Get(b.ID)
		if err != nil {
			t.Fatalf("backing Get: %v", err)
		}
		if cached.Status != "closed" {
			t.Fatalf("cached status = %q, want closed", cached.Status)
		}
		if cached.Revision != fresh.Revision {
			t.Fatalf("cached revision = %d, backing = %d", cached.Revision, fresh.Revision)
		}
		if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != b.ID {
			t.Fatalf("notifications = %+v, want exactly one bead.closed for %s", notes, b.ID)
		}
	})

	t.Run("compare_and_set_metadata_key", func(t *testing.T) {
		t.Parallel()
		var notes []cacheWriteNotification
		backing := NewMemStore()
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		b, err := cache.Create(Bead{Title: "cas"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		notes = nil
		ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "v")
		if err != nil || !ok {
			t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", ok, err)
		}
		cached, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after swap: %v", err)
		}
		fresh, err := backing.Get(b.ID)
		if err != nil {
			t.Fatalf("backing Get: %v", err)
		}
		if cached.Metadata["k"] != "v" {
			t.Fatalf("cached metadata k = %q, want v", cached.Metadata["k"])
		}
		if cached.Revision != fresh.Revision {
			t.Fatalf("cached revision = %d, backing = %d", cached.Revision, fresh.Revision)
		}
		if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != b.ID {
			t.Fatalf("notifications = %+v, want exactly one bead.updated for %s", notes, b.ID)
		}
	})
}

// TestCachingStoreConditionalWriteWritesThroughOnLaggedRefresh pins the
// write-through rule: when the post-write refresh serves a lagged (pre-write)
// row, the cache must still reflect exactly what the fenced verb proved
// committed — the caller's opts, the closed status, or the swapped key. The
// lagged revision is accepted (it self-heals: a fenced write against it
// precondition-fails and evicts); a lagged field value would not self-heal
// for plain readers.
// TestCachingStoreConditionalWriteEvictsOnLaggedRefresh pins the
// no-fabrication contract: a fenced write's post-write refresh cannot be
// attributed to our commit (the backing may serve a LAGGED pre-write row, or
// a LATER one), so the cache installs NOTHING — the entry is evicted and the
// next read consults the backing, which by then serves the committed state.
// The change notification fires with the verbatim refresh; consumers re-read
// by id rather than trusting event payloads for point-in-time state.
func TestCachingStoreConditionalWriteEvictsOnLaggedRefresh(t *testing.T) {
	t.Parallel()

	t.Run("update_opts", func(t *testing.T) {
		t.Parallel()
		var notes []cacheWriteNotification
		backing := &casBackingStore{Store: NewMemStore()}
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
			notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		b, err := cache.Create(Bead{Title: "pre-write"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		snapshot := cloneBead(got)
		backing.staleNextGet = &snapshot
		notes = nil
		title := "written"
		if err := cache.UpdateIfMatch(b.ID, got.Revision, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("UpdateIfMatch: %v", err)
		}

		// Evicted, never adopted: no cached row survives the fenced write.
		cache.mu.RLock()
		_, inBeads := cache.beads[b.ID]
		_, dirty := cache.dirty[b.ID]
		cache.mu.RUnlock()
		if inBeads {
			t.Fatal("fenced update adopted a row into the cache; the lagged refresh makes any adoption a fabrication")
		}
		if !dirty {
			t.Fatal("fenced update did not mark the entry dirty for backing re-read")
		}

		// The next read consults the backing and reports the committed state.
		cached, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after fenced update: %v", err)
		}
		if cached.Title != title {
			t.Fatalf("post-write read = %q, want the backing's committed %q", cached.Title, title)
		}
		if len(notes) != 1 || notes[0].eventType != "bead.updated" {
			t.Fatalf("notifications = %+v, want exactly one bead.updated", notes)
		}
	})

	t.Run("close_status", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "close-lag"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		snapshot := cloneBead(got)
		backing.staleNextGet = &snapshot
		if err := cache.CloseIfMatch(b.ID, got.Revision); err != nil {
			t.Fatalf("CloseIfMatch: %v", err)
		}
		cached, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after fenced close: %v", err)
		}
		if cached.Status != "closed" {
			t.Fatalf("post-close read = %q, want the backing's committed closed status", cached.Status)
		}
	})

	t.Run("swapped_metadata_key", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache := newConditionalCacheForTest(t, backing)
		b, err := cache.Create(Bead{Title: "cas-lag"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		snapshot := cloneBead(got)
		backing.staleNextGet = &snapshot
		ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "v")
		if !ok || err != nil {
			t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", ok, err)
		}
		cached, err := cache.Get(b.ID)
		if err != nil {
			t.Fatalf("Get after fenced swap: %v", err)
		}
		if cached.Metadata["k"] != "v" {
			t.Fatalf("post-swap read k = %q, want the backing's committed %q", cached.Metadata["k"], "v")
		}
	})
}

func TestCachingStoreCompareAndSetSuccessRefreshFailureEvicts(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	b, err := cache.Create(Bead{Title: "cas-refresh-fail"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing.failNextGet = true
	ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "won")
	if err != nil || !ok {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil) — only the refresh was injected to fail", ok, err)
	}
	assertConditionalEvicted(t, cache, b.ID)

	// Convergence: the next Get reaches the backing and shows this process
	// its own win.
	fresh, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after evict: %v", err)
	}
	if fresh.Metadata["k"] != "won" {
		t.Fatalf("re-read metadata k = %q, want the swapped %q", fresh.Metadata["k"], "won")
	}
}

func TestCachingStoreCloseIfMatchClearsDependentReadyProjection(t *testing.T) {
	t.Parallel()

	blockedProjection := true
	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "blocker", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	blocked, err := backing.Create(Bead{
		Title:     "blocked",
		Status:    "open",
		Type:      "task",
		Needs:     []string{blocker.ID},
		IsBlocked: &blockedProjection,
	})
	if err != nil {
		t.Fatalf("Create blocked: %v", err)
	}

	cache := newConditionalCacheForTest(t, backing)
	ready, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable before the fenced close")
	}
	readyByID := make(map[string]bool, len(ready))
	for _, bead := range ready {
		readyByID[bead.ID] = true
	}
	if !readyByID[blocker.ID] || readyByID[blocked.ID] {
		t.Fatalf("CachedReady before fenced close = %v, want blocker ready and dependent blocked", readyByID)
	}

	got, err := cache.Get(blocker.ID)
	if err != nil {
		t.Fatalf("Get blocker: %v", err)
	}
	if err := cache.CloseIfMatch(blocker.ID, got.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}

	// The fenced close evicts the blocker (dirty), so the cached view is
	// legitimately unavailable until a read re-primes the entry.
	if _, err := cache.Get(blocker.ID); err != nil {
		t.Fatalf("re-prime blocker after fenced close: %v", err)
	}
	ready, ok = cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable after the fenced close and re-prime")
	}
	readyByID = make(map[string]bool, len(ready))
	for _, bead := range ready {
		readyByID[bead.ID] = true
	}
	if !readyByID[blocked.ID] {
		t.Fatalf("CachedReady after fenced close = %v, want the dependent unblocked (its projected IsBlocked must be cleared)",
			readyByID)
	}
}

func TestCachingStoreDeleteIfMatchMirrorsDeleteScrub(t *testing.T) {
	t.Parallel()

	var notes []cacheWriteNotification
	backing := &casBackingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "del"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	notes = nil
	if err := cache.DeleteIfMatch(b.ID, got.Revision); err != nil {
		t.Fatalf("DeleteIfMatch: %v", err)
	}

	cache.mu.RLock()
	_, inBeads := cache.beads[b.ID]
	_, dirty := cache.dirty[b.ID]
	_, deleted := cache.deletedSeq[b.ID]
	cache.mu.RUnlock()
	if inBeads || dirty {
		t.Fatalf("scrub incomplete: inBeads=%v dirty=%v, want both false", inBeads, dirty)
	}
	if !deleted {
		t.Fatal("deletedSeq not stamped after DeleteIfMatch success — this is the one place it is correct")
	}

	// Get must return ErrNotFound WITHOUT consulting the backing.
	pre := backing.getCalls
	if _, err := cache.Get(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after fenced delete = %v, want ErrNotFound", err)
	}
	if backing.getCalls != pre {
		t.Fatal("Get after fenced delete consulted the backing; deletedSeq should short-circuit")
	}

	if len(notes) != 1 || notes[0].eventType != "bead.deleted" || notes[0].beadID != b.ID {
		t.Fatalf("notifications = %+v, want exactly one bead.deleted for %s", notes, b.ID)
	}
}

func TestCachingStoreCompareAndSetLoserEvictsAndConverges(t *testing.T) {
	t.Parallel()

	var notes []cacheWriteNotification
	mem := NewMemStore()
	backing := &casBackingStore{Store: mem}
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "cas-loser"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A cross-process winner lands out-of-band; the cache still serves the
	// pre-winner value that fed this process its losing `expected`.
	if err := mem.SetMetadata(b.ID, "k", "winner"); err != nil {
		t.Fatalf("out-of-band SetMetadata: %v", err)
	}

	notes = nil
	ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "", "mine")
	if err != nil {
		t.Fatalf("losing CAS returned error: %v, want (false, nil)", err)
	}
	if ok {
		t.Fatal("losing CAS returned true")
	}
	assertConditionalEvicted(t, cache, b.ID)
	if len(notes) != 0 {
		t.Fatalf("losing CAS fired notifications: %+v, want none (no write committed)", notes)
	}

	// Convergence: the re-read now reaches the backing and the retry wins.
	fresh, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after loser evict: %v", err)
	}
	if fresh.Metadata["k"] != "winner" {
		t.Fatalf("re-read metadata k = %q, want the winner's value", fresh.Metadata["k"])
	}
	ok, err = cache.CompareAndSetMetadataKey(b.ID, "k", "winner", "mine")
	if err != nil || !ok {
		t.Fatalf("retry CAS from the winner's value = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestCachingStoreCloseIfMatchToleratesBackingHidingClosedBeads(t *testing.T) {
	t.Parallel()

	var notes []cacheWriteNotification
	backing := &casBackingStore{Store: NewMemStore(), hideClosedFromGet: true}
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "hidden-close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	problemsBefore := cache.Stats().ProblemCount
	notes = nil
	if err := cache.CloseIfMatch(b.ID, got.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}

	if got := cache.Stats().ProblemCount; got != problemsBefore {
		t.Fatalf("ProblemCount %d -> %d; post-close ErrNotFound is tolerated, not a refresh failure", problemsBefore, got)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none on the tolerated-evict leg", notes)
	}
	assertConditionalEvicted(t, cache, b.ID)

	// The next read reports what the backing itself would: not found.
	if _, err := cache.Get(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after tolerated close = %v, want ErrNotFound (the backing hides closed beads)", err)
	}
}

func TestCachingStoreConditionalPreconditionEvictsPerVerb(t *testing.T) {
	t.Parallel()

	verbs := []struct {
		name string
		call func(c *CachingStore, id string, rev int64) error
	}{
		{"update", func(c *CachingStore, id string, rev int64) error {
			title := "x"
			return c.UpdateIfMatch(id, rev, UpdateOpts{Title: &title})
		}},
		{"close", func(c *CachingStore, id string, rev int64) error {
			return c.CloseIfMatch(id, rev)
		}},
		{"delete", func(c *CachingStore, id string, rev int64) error {
			return c.DeleteIfMatch(id, rev)
		}},
	}
	for _, verb := range verbs {
		t.Run(verb.name, func(t *testing.T) {
			t.Parallel()
			mem := NewMemStore()
			cache := newConditionalCacheForTest(t, &casBackingStore{Store: mem})
			b, err := cache.Create(Bead{Title: "stale-" + verb.name})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := mem.SetMetadata(b.ID, "k", "moved"); err != nil {
				t.Fatalf("out-of-band SetMetadata: %v", err)
			}

			err = verb.call(cache, b.ID, got.Revision)
			if !IsPreconditionFailed(err) {
				t.Fatalf("%s with stale revision: got %v, want precondition failure", verb.name, err)
			}
			assertConditionalEvicted(t, cache, b.ID)
		})
	}
}

func TestCachingStoreConditionalWriteErrorClassCacheActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		inject      error
		wantDirty   bool
		wantEvicted bool
	}{
		// Gate refusal proves the write did not commit and nothing about this
		// entry's freshness: the cache keeps serving.
		{"gate_refusal", &GateRefusalError{Verb: "update", Code: "close-authority"}, false, false},
		// CAS exhaustion proves the backing revision kept moving under
		// repeated re-reads: the cached row cannot be trusted either — evict
		// (dirty routes the next read through the backing).
		{"cas_retries_exhausted", &CASRetriesExhaustedError{Key: "k", Attempts: 4}, true, true},
		// A disabled/incapable backing likewise proves no commit.
		{"unsupported", ErrConditionalWriteUnsupported, false, false},
		// Anything else may have committed (ambiguous transport failure):
		// dirty forces the next Get through the backing without dropping the
		// entry from cached listings.
		{"ambiguous_transport", errors.New("bd: connection reset mid-write"), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache := newConditionalCacheForTest(t, backing)
			b, err := cache.Create(Bead{Title: "err-" + tc.name})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := cache.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			backing.errOverride = tc.inject
			title := "x"
			err = cache.UpdateIfMatch(b.ID, got.Revision, UpdateOpts{Title: &title})
			if !errors.Is(err, tc.inject) {
				t.Fatalf("UpdateIfMatch error = %v, want the injected %v forwarded untouched", err, tc.inject)
			}

			cache.mu.RLock()
			_, inBeads := cache.beads[b.ID]
			_, dirty := cache.dirty[b.ID]
			_, deleted := cache.deletedSeq[b.ID]
			cache.mu.RUnlock()
			if inBeads == tc.wantEvicted {
				t.Fatalf("%s: entry cached=%v, want evicted=%v", tc.name, inBeads, tc.wantEvicted)
			}
			if dirty != tc.wantDirty {
				t.Fatalf("dirty = %v, want %v", dirty, tc.wantDirty)
			}
			if deleted {
				t.Fatal("deletedSeq stamped on an error path")
			}

			// The (false, err) CAS shape routes through the same handler.
			backing.errOverride = tc.inject
			ok, casErr := cache.CompareAndSetMetadataKey(b.ID, "k", "", "v")
			if ok {
				t.Fatal("CAS returned true on an injected error")
			}
			if !errors.Is(casErr, tc.inject) {
				t.Fatalf("CAS error = %v, want the injected %v forwarded untouched", casErr, tc.inject)
			}
		})
	}
}

// TestCachingStoreAmbiguousConditionalFailureDirtySurvivesConcurrentScan pins
// the seq protection on the ambiguous-error dirty mark: a scan that started
// before the ambiguous failure must not merge its pre-write rows back over the
// mark. Without noteLocalMutationLocked beside the dirty-set, the List
// merge-back installs the stale row and deletes the flag — leaving a
// may-have-committed write invisible to every subsequent cache-served Get.
func TestCachingStoreAmbiguousConditionalFailureDirtySurvivesConcurrentScan(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	backing := &casBackingStore{Store: mem}
	cache := newConditionalCacheForTest(t, backing)
	b, err := cache.Create(Bead{Title: "pre-write"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mid-scan (rows already collected, merge-back still pending): the write
	// commits out-of-band at the backing, while the fenced write through the
	// cache reports an ambiguous transport failure.
	backing.onListOnce = func() {
		title := "committed"
		if err := mem.Update(b.ID, UpdateOpts{Title: &title}); err != nil {
			t.Errorf("out-of-band Update: %v", err)
		}
		backing.errOverride = errors.New("ambiguous transport failure")
		if err := cache.UpdateIfMatch(b.ID, 1, UpdateOpts{Title: &title}); err == nil {
			t.Error("UpdateIfMatch: want the injected ambiguous error")
		}
		backing.errOverride = nil
	}
	if _, err := cache.List(ListQuery{Live: true, AllowScan: true}); err != nil {
		t.Fatalf("List: %v", err)
	}

	// The dirty mark must have survived the merge-back: the next Get consults
	// the backing and observes the committed write.
	pre := backing.getCalls
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if backing.getCalls == pre {
		t.Fatal("Get was cache-served; the concurrent scan merge-back erased the ambiguous-dirty mark")
	}
	if got.Title != "committed" {
		t.Fatalf("Get title = %q, want the committed %q", got.Title, "committed")
	}
}

func TestCachingStoreCompareAndSetForwardsWithoutCachedPreCheck(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache := newConditionalCacheForTest(t, backing)
	b, err := cache.Create(Bead{Title: "no-precheck"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cache.SetMetadata(b.ID, "k", "target"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	// The cached value already equals `next`: a fabricated "already matches"
	// short-circuit would return (true, nil) without a backing call. The real
	// fence must be evaluated by the backing — current "target" != expected
	// "old" — and lose.
	pre := backing.casCalls
	ok, err := cache.CompareAndSetMetadataKey(b.ID, "k", "old", "target")
	if err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if ok {
		t.Fatal("CAS returned true; a cached-value short-circuit fabricated a success without a backing write")
	}
	if backing.casCalls != pre+1 {
		t.Fatalf("backing CAS calls %d -> %d, want exactly one forwarded call (no cached pre-check)", pre, backing.casCalls)
	}
}

// TestCachingStoreConditionalWritesStampDelegatesToBacking pins §6.3's
// delegation rule: the cache is a wrapper, not a second store, so it carries
// no stamp of its own — stamp writes, stamp reads, and the degrade latch all
// forward to the backing store, and the seam resolves the CachingStore using
// the backing's mode while returning the CACHING store as the writer (so the
// forward-and-evict cache rules stay in the loop).
func TestCachingStoreConditionalWritesStampDelegatesToBacking(t *testing.T) {
	mem := NewMemStore()
	cache := newConditionalCacheForTest(t, mem)

	cache.stampConditionalWritesMode(gate.Require, false)
	if mode, defaulted := mem.conditionalWritesMode(); mode != gate.Require || defaulted {
		t.Fatalf("backing stamp after caching stamp = (%q, %v), want (require, false)", mode, defaulted)
	}
	if mode, _ := cache.conditionalWritesMode(); mode != gate.Require {
		t.Fatalf("caching stamp read = %q, want the backing's require", mode)
	}

	w, diag, err := ResolveConditionalWriter(cache)
	if err != nil || diag != nil {
		t.Fatalf("require∧capable over cache = diag %v err %v, want nil/nil", diag, err)
	}
	if got, ok := w.(*CachingStore); !ok || got != cache {
		t.Fatalf("writer = %T, want the CachingStore itself (cache rules must stay in the write path)", w)
	}

	// The degrade latch is ONE latch shared with the backing store.
	if !mem.noteConditionalDegradeOnce() {
		t.Fatal("backing first degrade note = false, want true")
	}
	if cache.noteConditionalDegradeOnce() {
		t.Fatal("caching degrade note = true after backing noted, want the shared latch to report false")
	}
}

// TestCachingStoreConditionalCapabilityDelegatesToBacking drives the seam's
// prober through the cache: an incapable backing degrades the cache resolve
// (auto) and refuses it (require); a carrier-less wrapped backing resolves as
// unset→legacy.
func TestCachingStoreConditionalCapabilityDelegatesToBacking(t *testing.T) {
	t.Run("backing instance toggle degrades the cache resolve", func(t *testing.T) {
		mem := NewMemStore()
		mem.DisableConditionalWrites = true
		cache := newConditionalCacheForTest(t, mem)
		cache.stampConditionalWritesMode(gate.Auto, false)

		w, diag, err := ResolveConditionalWriter(cache)
		if w != nil || err != nil || diag == nil {
			t.Fatalf("auto over cache w/ disabled backing = (%v, %v, %v), want (nil, diag, nil)", w, diag, err)
		}
		if diag.Store != "CachingStore" {
			t.Fatalf("diag.Store = %q, want CachingStore (the resolved store, not the backing)", diag.Store)
		}
	})
	t.Run("require over an incapable backing refuses closed", func(t *testing.T) {
		mem := NewMemStore()
		mem.DisableConditionalWrites = true
		cache := newConditionalCacheForTest(t, mem)
		cache.stampConditionalWritesMode(gate.Require, false)

		w, diag, err := ResolveConditionalWriter(cache)
		if w != nil || diag == nil || !IsConditionalWritesRequired(err) {
			t.Fatalf("require over cache = (%v, %v, %v), want (nil, diag, typed refusal)", w, diag, err)
		}
	})
	t.Run("carrier-less wrapped backing resolves unset legacy", func(t *testing.T) {
		backing := &casBackingStore{Store: NewMemStore()}
		cache := newConditionalCacheForTest(t, backing)
		// Stamping forwards to a backing that cannot carry it: the miss is
		// REPORTED (red-team F2), never silently believed.
		if cache.stampConditionalWritesMode(gate.Require, false) {
			t.Fatal("stamp into a carrier-less backing reported landed=true")
		}
		w, diag, err := ResolveConditionalWriter(cache)
		if w != nil || diag != nil || err != nil {
			t.Fatalf("cache over carrier-less backing = (%v, %v, %v), want unset legacy (nil, nil, nil)", w, diag, err)
		}
	})
	t.Run("stamped backing with CAS verbs but no prober is vacuously capable", func(t *testing.T) {
		mem := NewMemStore()
		backing := &casOnlyStore{Store: mem, ConditionalWriter: mem}
		cache := newConditionalCacheForTest(t, backing)
		if !cache.stampConditionalWritesMode(gate.Auto, false) {
			t.Fatal("stamp into a carrier backing reported landed=false")
		}
		w, diag, err := ResolveConditionalWriter(cache)
		if err != nil || diag != nil {
			t.Fatalf("auto over cache w/ CAS-verbs-no-prober backing = diag %v err %v, want nil/nil (vacuously capable)", diag, err)
		}
		if got, ok := w.(*CachingStore); !ok || got != cache {
			t.Fatalf("writer = %T, want the CachingStore itself", w)
		}
	})
	t.Run("stamped backing without CAS verbs degrades with the backing reason", func(t *testing.T) {
		// The future cache-over-NativeDoltStore shape: the backing carries a
		// stamp but implements neither the prober nor ConditionalWriter.
		backing := &stampedNoCASStore{Store: NewMemStore()}
		cache := newConditionalCacheForTest(t, backing)
		cache.stampConditionalWritesMode(gate.Auto, false)
		w, diag, err := ResolveConditionalWriter(cache)
		if w != nil || err != nil || diag == nil {
			t.Fatalf("auto over cache w/ CAS-less backing = (%v, %v, %v), want (nil, diag, nil)", w, diag, err)
		}
		if !strings.Contains(diag.PreflightReason, "backing store does not implement conditional writes") {
			t.Fatalf("PreflightReason = %q, want the backing-incapable reason", diag.PreflightReason)
		}
	})
}

// TestCachingStoreConditionalFollowsBackingResolveTarget pins the production
// sandwich CachingStore → target-declaring wrapper → stamped store (the
// controller wraps the factory store in a policy layer BEFORE caching): the
// cache's carrier, prober, and verb forwarding must follow the wrapper's
// declared resolution target or the stamp is hidden and require silently
// collapses to legacy.
func TestCachingStoreConditionalFollowsBackingResolveTarget(t *testing.T) {
	mem := NewMemStore()
	mem.stampConditionalWritesMode(gate.Require, false)
	wrapped := &resolveTargetWrapper{Store: mem, target: mem}
	cache := newConditionalCacheForTest(t, wrapped)

	if mode, _ := cache.conditionalWritesMode(); mode != gate.Require {
		t.Fatalf("cache mode through wrapped backing = %q, want require", mode)
	}
	writer, diag, err := ResolveConditionalWriter(cache)
	if err != nil || diag != nil {
		t.Fatalf("resolve = diag %v err %v, want the cache writer", diag, err)
	}
	if got, ok := writer.(*CachingStore); !ok || got != cache {
		t.Fatalf("writer = %T, want the CachingStore itself", writer)
	}

	// The verbs reach the stamped store through the wrapper too.
	created, err := cache.Create(Bead{Title: "sandwich"})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := cache.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	fenced := "fenced"
	if err := writer.UpdateIfMatch(created.ID, fresh.Revision, UpdateOpts{Title: &fenced}); err != nil {
		t.Fatalf("UpdateIfMatch through the sandwich: %v", err)
	}
}
