package beads

import (
	"context"
	"encoding/json"
	"errors"
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
}

func (c *claimCapableBackingStore) Get(id string) (Bead, error) {
	if c.failGetAfterClaim {
		return Bead{}, errors.New("backing get unavailable")
	}
	return c.Store.Get(id)
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
	return claimed, true, nil
}

// TestCachingStoreClaimWriteThroughKeepsCachedDependencies is the write-through
// correctness test for S5b-4: a successful Claim must not clobber the cache's
// separately-tracked dependency edges (c.deps[id]) to empty, because the
// claimed bead it absorbs deliberately carries no Dependencies. Using
// depsFromFields here (instead of depsKeepCached) would silently wipe every
// cached dependency edge on the claimed bead's first successful claim.
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

// TestCachingStoreClaimWriteThroughKeepsCachedLabels is the item-7 regression
// test: Claim's bare-row contract strips Labels (see claimCapableBackingStore
// above) the same way it strips Dependencies, but unlike dependencies there
// was no depsKeepCached-equivalent guard protecting Labels — a successful
// Claim used to absorb the bare row wholesale via absorbFreshLocked, wiping
// any Labels the cache had primed for that bead. CachingStore.Claim must now
// refresh from backing (mirroring ReleaseIfCurrent) so the richer, label-
// bearing row — not the bare claim response — is what lands in the cache.
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

// TestCachingStoreClaimColdCacheMarksRowDirtyWhenRefreshFails is the S5b-7
// review FINAL item 7 sub-bullet 3 regression test: when Claim's post-claim
// refresh (refreshBeadAfterWrite) fails AND the id was never cached before
// (a true cold claim -- nothing in c.beads to merge onto), CachingStore.Claim
// installs the bare, stripped claim row as a placeholder. That placeholder
// has no labels/dependencies/comments confirmed from the backing store (the
// refresh that would have confirmed them failed), so it must be marked
// dirty, not clean: a clean mark would let a later cache read trust the
// empty label/dep state as settled fact instead of bypassing to the backing
// store for the real row.
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
