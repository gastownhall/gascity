package beads

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// claimCapableBackingStore wraps a Store and adds a minimal two-argument
// Claim so CachingStore.Claim's write-through can be exercised without a
// live NativeDoltStore. It reproduces the one detail that makes the
// write-through risky: the real Claimer role's contract (see
// NativeDoltStore.Claim / issueops.ClaimResult) hands back a BARE ROW —
// Dependencies deliberately stripped — so this fake strips it too.
type claimCapableBackingStore struct {
	Store
	claimCalls int
	// failGetAfterClaim, when true, makes Get fail for every caller.
	// Claim's own internal reads go through c.Store.Get directly (bypassing
	// this flag) so a Claim RPC can still succeed inline while modeling a
	// separate, subsequent Get (CachingStore.Claim's post-claim refresh)
	// failing — e.g. a transient read blip right after a successful write.
	failGetAfterClaim bool
	// afterClaim runs once, right after the next claim commits and before it
	// returns to the cache.
	afterClaim func()
	// afterGet runs once, right after the next Get read its row and before it
	// returns. Claim's own reads bypass it, so it fires inside the cache's
	// post-claim refresh.
	afterGet func()
}

func (c *claimCapableBackingStore) Get(id string) (Bead, error) {
	if c.failGetAfterClaim {
		return Bead{}, errors.New("backing get unavailable")
	}
	b, err := c.Store.Get(id)
	if hook := c.afterGet; hook != nil {
		c.afterGet = nil
		hook()
	}
	return b, err
}

func (c *claimCapableBackingStore) Claim(id, assignee string) (Bead, bool, error) {
	c.claimCalls++
	current, err := c.Store.Get(id)
	if err != nil {
		return Bead{}, false, err
	}
	if current.Assignee != "" && current.Assignee != assignee {
		return Bead{}, false, nil
	}
	status := "in_progress"
	if err := c.Update(id, UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		return Bead{}, false, err
	}
	claimed, err := c.Store.Get(id)
	if err != nil {
		return Bead{}, false, err
	}
	claimed.Dependencies = nil
	claimed.Labels = nil
	if hook := c.afterClaim; hook != nil {
		c.afterClaim = nil
		hook()
	}
	return claimed, true, nil
}

// TestCachingStoreClaimWriteThroughKeepsCachedDependencies pins that a
// successful Claim leaves the cache's separately tracked dependency edges
// (c.deps[id]) alone: the claimed bead carries no Dependencies, so deriving
// edges from its fields (depsFromFields instead of depsKeepCached) would wipe
// every cached edge of a claimed bead.
func TestCachingStoreClaimWriteThroughKeepsCachedDependencies(t *testing.T) {
	t.Parallel()

	backing := &claimCapableBackingStore{Store: NewMemStore()}
	target, err := backing.Create(Bead{Title: "dep target"})
	if err != nil {
		t.Fatalf("Create dep target: %v", err)
	}
	work, err := backing.Create(Bead{Title: "claimable work"})
	if err != nil {
		t.Fatalf("Create work: %v", err)
	}
	if err := backing.DepAdd(work.ID, target.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.RLock()
	primedDeps := len(cache.deps[work.ID])
	cache.mu.RUnlock()
	if primedDeps != 1 {
		t.Fatalf("primed cache deps for %s = %d, want 1 (test setup is broken)", work.ID, primedDeps)
	}
	events = nil

	claimed, ok, err := cache.Claim(work.ID, "worker-1")
	if err != nil || !ok {
		t.Fatalf("Claim = (%v, %v, %v), want (bead, true, nil)", claimed, ok, err)
	}
	if backing.claimCalls != 1 {
		t.Fatalf("backing Claim calls = %d, want 1", backing.claimCalls)
	}
	if claimed.Assignee != "worker-1" {
		t.Fatalf("claimed bead assignee = %q, want worker-1", claimed.Assignee)
	}

	cache.mu.RLock()
	afterDeps := cloneDeps(cache.deps[work.ID])
	cache.mu.RUnlock()
	if len(afterDeps) != 1 || afterDeps[0].DependsOnID != target.ID {
		t.Fatalf("cache.deps[%s] after a successful claim = %+v, want the pre-claim dependency edge untouched (depsKeepCached)", work.ID, afterDeps)
	}
	if !stringSliceContains(events, "bead.updated:"+work.ID) {
		t.Fatalf("events = %v, want bead.updated for the claimed bead", events)
	}
}

// TestCachingStoreClaimWriteThroughKeepsCachedLabels pins that a successful
// Claim keeps the claimed bead's labels. The bare claim row strips Labels (see
// claimCapableBackingStore above) as it strips Dependencies, and installing
// it wholesale would drop every primed label, so CachingStore.Claim must
// install the refreshed full row, not the bare claim response.
func TestCachingStoreClaimWriteThroughKeepsCachedLabels(t *testing.T) {
	t.Parallel()

	backing := &claimCapableBackingStore{Store: NewMemStore()}
	work, err := backing.Create(Bead{Title: "claimable work", Labels: []string{"urgent", "backend"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.RLock()
	primedLabels := append([]string(nil), cache.beads[work.ID].Labels...)
	cache.mu.RUnlock()
	if len(primedLabels) != 2 {
		t.Fatalf("primed cache labels for %s = %v, want 2 labels (test setup is broken)", work.ID, primedLabels)
	}
	events = nil

	claimed, ok, err := cache.Claim(work.ID, "worker-1")
	if err != nil || !ok {
		t.Fatalf("Claim = (%v, %v, %v), want (bead, true, nil)", claimed, ok, err)
	}
	if claimed.Assignee != "worker-1" {
		t.Fatalf("claimed bead assignee = %q, want worker-1", claimed.Assignee)
	}

	cache.mu.RLock()
	afterLabels := append([]string(nil), cache.beads[work.ID].Labels...)
	cache.mu.RUnlock()
	if len(afterLabels) != 2 {
		t.Fatalf("cache.beads[%s].Labels after a successful claim = %v, want the pre-claim labels untouched", work.ID, afterLabels)
	}
	if !stringSliceContains(events, "bead.updated:"+work.ID) {
		t.Fatalf("events = %v, want bead.updated for the claimed bead", events)
	}
}

// TestCachingStoreClaimConflictAndUnsupportedNeverTouchTheCache pins the two
// early-return paths: a conflict (ok=false, nil error) and a backing store
// that lacks the capability entirely must both pass straight through without
// marking anything dirty or notifying — mirroring ReleaseIfCurrent's own
// unlocked early return.
func TestCachingStoreClaimConflictAndUnsupportedNeverTouchTheCache(t *testing.T) {
	t.Parallel()

	t.Run("conflict", func(t *testing.T) {
		t.Parallel()
		backing := &claimCapableBackingStore{Store: NewMemStore()}
		bead, err := backing.Create(Bead{Title: "task", Assignee: "holder"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		status := "in_progress"
		if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update: %v", err)
		}

		var events []string
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
			events = append(events, eventType+":"+beadID)
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		events = nil

		_, ok, err := cache.Claim(bead.ID, "someone-else")
		if err != nil {
			t.Fatalf("Claim error = %v, want nil on a conflict", err)
		}
		if ok {
			t.Fatal("Claim ok = true on a conflict")
		}
		if len(events) != 0 {
			t.Fatalf("events = %v, want none for a conflicted claim", events)
		}
	})

	t.Run("backing lacks Claim", func(t *testing.T) {
		t.Parallel()
		backing := NewMemStore() // no Claim(string,string) method
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}

		_, ok, err := cache.Claim("nonexistent", "worker-1")
		if ok {
			t.Fatal("Claim ok = true against a store with no Claim capability")
		}
		if !errors.Is(err, ErrClaimUnsupported) {
			t.Fatalf("err = %v, want ErrClaimUnsupported", err)
		}
	})
}

// TestCachingStoreClaimColdCacheMarksRowDirtyWhenRefreshFails pins the cold
// claim: when Claim's post-claim refresh (refreshBeadAfterWrite) fails and the
// id was never cached (nothing in c.beads to merge onto), CachingStore.Claim
// installs the bare, stripped claim row as a placeholder. Nothing confirmed
// that placeholder's labels, dependencies or comments, so it must be marked
// dirty, not clean: a clean mark would let a later cache read trust the empty
// label/dep state as settled fact instead of reading the backing row.
func TestCachingStoreClaimColdCacheMarksRowDirtyWhenRefreshFails(t *testing.T) {
	t.Parallel()

	backing := &claimCapableBackingStore{Store: NewMemStore()}
	work, err := backing.Create(Bead{Title: "claimable work", Labels: []string{"urgent"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// Evict work.ID so it is genuinely absent from cache.beads going into
	// Claim -- the real "cold claim" shape the branch under test exists for
	// -- rather than relying on Prime having raced the Create above.
	cache.mu.Lock()
	delete(cache.beads, work.ID)
	delete(cache.deps, work.ID)
	delete(cache.dirty, work.ID)
	cache.mu.Unlock()

	backing.failGetAfterClaim = true
	claimed, ok, err := cache.Claim(work.ID, "worker-1")
	if err != nil || !ok {
		t.Fatalf("Claim = (%v, %v, %v), want (bead, true, nil)", claimed, ok, err)
	}
	if claimed.Assignee != "worker-1" {
		t.Fatalf("claimed bead assignee = %q, want worker-1", claimed.Assignee)
	}

	cache.mu.RLock()
	_, isDirty := cache.dirty[work.ID]
	cache.mu.RUnlock()
	if !isDirty {
		t.Fatalf("cache.dirty[%s] after a cold claim whose post-claim refresh failed = false, want true (placeholder row must not be trusted as clean)", work.ID)
	}
}

// TestCachingStoreClaimWarmCacheRefreshFailureCarriesClaimedRevision pins that
// when the post-claim refresh fails on a cached row, the merged row Claim
// returns carries the claimed row's revision, not the pre-claim one, so a
// caller chaining a conditional write on it does not miss its own CAS.
func TestCachingStoreClaimWarmCacheRefreshFailureCarriesClaimedRevision(t *testing.T) {
	t.Parallel()

	backing := &claimCapableBackingStore{Store: NewMemStore()}
	work, err := backing.Create(Bead{
		Title:    "claimable work",
		Labels:   []string{"urgent"},
		Metadata: map[string]string{"phase": "queued"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// The claimed row carries metadata the cached row does not yet hold, so
	// only the refresh-failure merge's Metadata copy can surface it.
	if err := backing.SetMetadata(work.ID, "phase", "claimed"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	backing.failGetAfterClaim = true
	claimed, ok, err := cache.Claim(work.ID, "worker-1")
	if err != nil || !ok {
		t.Fatalf("Claim = (%v, %v, %v), want (bead, true, nil)", claimed, ok, err)
	}
	backing.failGetAfterClaim = false
	current, err := backing.Get(work.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if claimed.Revision != current.Revision {
		t.Fatalf("claimed revision = %d, want the post-claim revision %d", claimed.Revision, current.Revision)
	}
	if len(claimed.Labels) != 1 {
		t.Fatalf("claimed labels = %v, want the cached label kept", claimed.Labels)
	}
	if got := claimed.Metadata["phase"]; got != "claimed" {
		t.Fatalf("claimed metadata phase = %q, want the claimed row's value %q", got, "claimed")
	}
	claimed.Metadata["phase"] = "mutated-by-caller"
	cache.mu.RLock()
	cachedPhase := cache.beads[work.ID].Metadata["phase"]
	cache.mu.RUnlock()
	if cachedPhase != "claimed" {
		t.Fatalf("cached metadata phase = %q after mutating the returned row, want %q (returned map must not alias the cache)", cachedPhase, "claimed")
	}
}

// TestCachingStoreClaimRacedWriteInstallsNothing pins Claim's write fence: a
// local Delete or write of the claimed row that lands after the claim began
// stands. Claim installs neither its refreshed row nor its fallback over it,
// so a deleted row is not resurrected and a newer write is not overwritten
// with an older read, and it still notifies and returns the claim it
// committed: the acquisition row, not a refresh that already shows the newer
// write.
func TestCachingStoreClaimRacedWriteInstallsNothing(t *testing.T) {
	t.Parallel()

	deleteRow := func(t *testing.T, cache *CachingStore, id string) {
		if err := cache.Delete(id); err != nil {
			t.Errorf("racing Delete: %v", err)
		}
	}
	writeRow := func(t *testing.T, cache *CachingStore, id string) {
		if err := cache.SetMetadata(id, "k", "v"); err != nil {
			t.Errorf("racing SetMetadata: %v", err)
		}
	}
	closeRow := func(t *testing.T, cache *CachingStore, id string) {
		if err := cache.Close(id); err != nil {
			t.Errorf("racing Close: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		// duringRefresh fires the racer inside Claim's refresh, after it read
		// the row; otherwise the racer fires before that read, so the refresh
		// misses a deleted row and reads a closed one.
		duringRefresh bool
		racer         func(t *testing.T, cache *CachingStore, id string)
		deletes       bool
	}{
		{name: "delete_before_refresh", racer: deleteRow, deletes: true},
		{name: "delete_during_refresh", duringRefresh: true, racer: deleteRow, deletes: true},
		{name: "write_during_refresh", duringRefresh: true, racer: writeRow},
		{name: "close_before_refresh", racer: closeRow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &claimCapableBackingStore{Store: NewMemStore()}
			work, err := backing.Create(Bead{Title: "claimable work"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			var (
				mu     sync.Mutex
				events []string
				rows   []Bead
			)
			cache := NewCachingStoreForTest(backing, func(eventType, _ string, payload json.RawMessage) {
				var row Bead
				if err := json.Unmarshal(payload, &row); err != nil {
					t.Errorf("unmarshal %s payload: %v", eventType, err)
				}
				mu.Lock()
				events = append(events, eventType)
				rows = append(rows, row)
				mu.Unlock()
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			raced := -1
			race := func() {
				tc.racer(t, cache, work.ID)
				mu.Lock()
				raced = len(events)
				mu.Unlock()
			}
			if tc.duringRefresh {
				backing.afterGet = race
			} else {
				backing.afterClaim = race
			}
			claimed, ok, err := cache.Claim(work.ID, "worker-1")
			if err != nil || !ok {
				t.Fatalf("Claim = (%v, %v, %v), want (bead, true, nil)", claimed, ok, err)
			}
			if raced < 0 || backing.afterGet != nil || backing.afterClaim != nil {
				t.Fatal("the racer never ran; the race is vacuous")
			}
			if claimed.Assignee != "worker-1" || claimed.Status != "in_progress" {
				t.Fatalf("Claim returned (assignee %q, status %q), want (worker-1, in_progress)", claimed.Assignee, claimed.Status)
			}
			mu.Lock()
			after, afterRows := events[raced:], rows[raced:]
			mu.Unlock()
			if len(after) != 1 || after[0] != "bead.updated" || afterRows[0].ID != work.ID || afterRows[0].Assignee != "worker-1" || afterRows[0].Status != "in_progress" {
				t.Fatalf("events after the racer = %v (%+v), want exactly one bead.updated carrying the claim", after, afterRows)
			}

			if !tc.deletes {
				current, err := backing.Store.Get(work.ID)
				if err != nil {
					t.Fatalf("backing Get(%s): %v", work.ID, err)
				}
				if claimed.Revision == 0 || claimed.Revision >= current.Revision {
					t.Fatalf("Claim returned revision %d, want the claim's own, below the racing write's %d", claimed.Revision, current.Revision)
				}
				assertSettledCensusAgrees(t, cache, backing.Store, work.ID)
				return
			}
			cache.mu.RLock()
			_, cached := cache.beads[work.ID]
			_, tombstoned := cache.deletedSeq[work.ID]
			_, dirty := cache.dirty[work.ID]
			cache.mu.RUnlock()
			if cached || !tombstoned {
				t.Fatalf("Claim resurrected deleted row %s (cached=%v, tombstoned=%v)", work.ID, cached, tombstoned)
			}
			if dirty {
				t.Fatalf("tombstoned row %s carries a dirty mark", work.ID)
			}
			if _, err := cache.Get(work.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(%s) after the racing Delete = %v, want ErrNotFound", work.ID, err)
			}
			if !admittedCensusAgrees(t, cache, backing.Store, work.ID) {
				t.Fatal("census refused")
			}
		})
	}
}
