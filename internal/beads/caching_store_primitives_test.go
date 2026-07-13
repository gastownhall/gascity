package beads

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// newPrimitiveTestStore builds a CachingStore with pre-seeded six-map state so
// the primitive unit tests can assert exact post-state across ALL six maps —
// the contract is as much about which maps do NOT move as which do.
func newPrimitiveTestStore() *CachingStore {
	return &CachingStore{
		beads:       make(map[string]Bead),
		deps:        make(map[string][]Dep),
		dirty:       make(map[string]struct{}),
		beadSeq:     make(map[string]uint64),
		localBeadAt: make(map[string]time.Time),
		deletedSeq:  make(map[string]uint64),
	}
}

func TestEvictLockedRemovesAllSixMaps(t *testing.T) {
	c := newPrimitiveTestStore()
	id := "gc-1"
	c.beads[id] = Bead{ID: id}
	c.deps[id] = []Dep{{IssueID: id, DependsOnID: "gc-2", Type: "blocks"}}
	c.dirty[id] = struct{}{}
	c.beadSeq[id] = 7
	c.localBeadAt[id] = time.Now()
	c.deletedSeq[id] = 3
	// Unrelated row must survive.
	c.beads["gc-9"] = Bead{ID: "gc-9"}
	c.beadSeq["gc-9"] = 4

	c.evictLocked(id)

	assertAbsent(t, c, id)
	if _, ok := c.beads["gc-9"]; !ok {
		t.Fatal("evictLocked removed an unrelated row")
	}
	if c.beadSeq["gc-9"] != 4 {
		t.Fatal("evictLocked disturbed an unrelated beadSeq")
	}
}

func TestTombstoneLockedEvictsThenFences(t *testing.T) {
	c := newPrimitiveTestStore()
	id := "gc-1"
	c.beads[id] = Bead{ID: id}
	c.deps[id] = []Dep{{IssueID: id}}
	c.dirty[id] = struct{}{}
	c.beadSeq[id] = 7
	c.localBeadAt[id] = time.Now()
	c.deletedSeq[id] = 2

	c.tombstoneLocked(id, 42)

	if _, ok := c.beads[id]; ok {
		t.Fatal("tombstone left a live row")
	}
	if _, ok := c.deps[id]; ok {
		t.Fatal("tombstone left deps")
	}
	if _, ok := c.dirty[id]; ok {
		t.Fatal("tombstone left dirty")
	}
	if _, ok := c.beadSeq[id]; ok {
		t.Fatal("tombstone left beadSeq")
	}
	if _, ok := c.localBeadAt[id]; ok {
		t.Fatal("tombstone left localBeadAt")
	}
	if c.deletedSeq[id] != 42 {
		t.Fatalf("tombstone fence = %d, want 42", c.deletedSeq[id])
	}
}

func TestAbsorbFreshLockedDepsModes(t *testing.T) {
	now := time.Now()
	blocking := Bead{ID: "gc-1", Needs: []string{"gc-2"}}
	bare := Bead{ID: "gc-1"}
	cachedDeps := []Dep{{IssueID: "gc-1", DependsOnID: "gc-9", Type: "blocks"}}
	explicit := []Dep{{IssueID: "gc-1", DependsOnID: "gc-3", Type: "blocks"}}

	cases := []struct {
		name     string
		bead     Bead
		opts     absorbOpts
		wantDeps func(t *testing.T, deps []Dep, present bool)
	}{
		{
			name: "explicit",
			bead: bare,
			opts: absorbOpts{depsMode: depsExplicit, deps: explicit, seqMode: seqKeep, clearDirty: true},
			wantDeps: func(t *testing.T, deps []Dep, present bool) {
				if !present || len(deps) != 1 || deps[0].DependsOnID != "gc-3" {
					t.Fatalf("depsExplicit: got %v present=%v", deps, present)
				}
			},
		},
		{
			name: "fromFields carrying",
			bead: blocking,
			opts: absorbOpts{depsMode: depsFromFields, seqMode: seqKeep, clearDirty: true},
			wantDeps: func(t *testing.T, deps []Dep, present bool) {
				if !present || len(deps) != 1 || deps[0].DependsOnID != "gc-2" {
					t.Fatalf("depsFromFields carrying: got %v present=%v", deps, present)
				}
			},
		},
		{
			name: "fromFields bare writes nil unconditionally",
			bead: bare,
			opts: absorbOpts{depsMode: depsFromFields, seqMode: seqKeep, clearDirty: true},
			wantDeps: func(t *testing.T, deps []Dep, present bool) {
				if !present || deps != nil {
					t.Fatalf("depsFromFields bare: want present nil, got %v present=%v", deps, present)
				}
			},
		},
		{
			name: "fromFieldsIfCarried skips bare",
			bead: bare,
			opts: absorbOpts{depsMode: depsFromFieldsIfCarried, seqMode: seqKeep, clearDirty: true},
			wantDeps: func(t *testing.T, deps []Dep, present bool) {
				if !present || len(deps) != 1 || deps[0].DependsOnID != "gc-9" {
					t.Fatalf("depsFromFieldsIfCarried bare should keep cached: got %v present=%v", deps, present)
				}
			},
		},
		{
			name: "keepCached",
			bead: blocking,
			opts: absorbOpts{depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true},
			wantDeps: func(t *testing.T, deps []Dep, present bool) {
				if !present || len(deps) != 1 || deps[0].DependsOnID != "gc-9" {
					t.Fatalf("depsKeepCached: got %v present=%v", deps, present)
				}
			},
		},
		{
			name: "drop",
			bead: blocking,
			opts: absorbOpts{depsMode: depsDrop, seqMode: seqKeep, clearDirty: true},
			wantDeps: func(t *testing.T, deps []Dep, present bool) {
				if present {
					t.Fatalf("depsDrop: deps should be absent, got %v", deps)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newPrimitiveTestStore()
			c.deps["gc-1"] = cachedDeps
			c.dirty["gc-1"] = struct{}{}
			c.deletedSeq["gc-1"] = 5
			c.absorbFreshLocked("gc-1", tc.bead, now, tc.opts)

			if _, ok := c.beads["gc-1"]; !ok {
				t.Fatal("absorb did not install the row")
			}
			if _, ok := c.deletedSeq["gc-1"]; ok {
				t.Fatal("absorb must always clear the tombstone")
			}
			if _, ok := c.dirty["gc-1"]; ok {
				t.Fatal("clearDirty:true must clear the dirty mark")
			}
			deps, present := c.deps["gc-1"]
			tc.wantDeps(t, deps, present)
		})
	}
}

func TestAbsorbFreshLockedSeqModes(t *testing.T) {
	now := time.Now()

	t.Run("seqKeep touches neither fence", func(t *testing.T) {
		c := newPrimitiveTestStore()
		c.beadSeq["gc-1"] = 9
		c.localBeadAt["gc-1"] = now
		c.absorbFreshLocked("gc-1", Bead{ID: "gc-1"}, now, absorbOpts{depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true})
		if c.beadSeq["gc-1"] != 9 {
			t.Fatal("seqKeep cleared beadSeq")
		}
		if _, ok := c.localBeadAt["gc-1"]; !ok {
			t.Fatal("seqKeep cleared localBeadAt")
		}
	})

	t.Run("seqClearGuarded keeps recent local", func(t *testing.T) {
		c := newPrimitiveTestStore()
		c.beadSeq["gc-1"] = 9
		c.localBeadAt["gc-1"] = now // recent -> keep
		c.absorbFreshLocked("gc-1", Bead{ID: "gc-1"}, now, absorbOpts{depsMode: depsKeepCached, seqMode: seqClearGuarded, clearDirty: true})
		if c.beadSeq["gc-1"] != 9 {
			t.Fatal("seqClearGuarded cleared a recent-local fence")
		}
		if _, ok := c.localBeadAt["gc-1"]; !ok {
			t.Fatal("seqClearGuarded cleared a recent-local localBeadAt")
		}
	})

	t.Run("seqClearGuarded clears stale local", func(t *testing.T) {
		c := newPrimitiveTestStore()
		c.beadSeq["gc-1"] = 9
		c.localBeadAt["gc-1"] = now.Add(-10 * time.Second) // stale -> clear
		c.absorbFreshLocked("gc-1", Bead{ID: "gc-1"}, now, absorbOpts{depsMode: depsKeepCached, seqMode: seqClearGuarded, clearDirty: true})
		if _, ok := c.beadSeq["gc-1"]; ok {
			t.Fatal("seqClearGuarded left a stale beadSeq")
		}
		if _, ok := c.localBeadAt["gc-1"]; ok {
			t.Fatal("seqClearGuarded left a stale localBeadAt")
		}
	})

	t.Run("seqClearBeadSeqOnly clears beadSeq keeps localBeadAt", func(t *testing.T) {
		c := newPrimitiveTestStore()
		c.beadSeq["gc-1"] = 9
		c.localBeadAt["gc-1"] = now
		c.absorbFreshLocked("gc-1", Bead{ID: "gc-1"}, now, absorbOpts{depsMode: depsKeepCached, seqMode: seqClearBeadSeqOnly, clearDirty: true})
		if _, ok := c.beadSeq["gc-1"]; ok {
			t.Fatal("seqClearBeadSeqOnly left beadSeq")
		}
		if _, ok := c.localBeadAt["gc-1"]; !ok {
			t.Fatal("seqClearBeadSeqOnly cleared localBeadAt")
		}
	})
}

func TestAbsorbFreshLockedClearDirtyFalseKeepsMark(t *testing.T) {
	now := time.Now()
	c := newPrimitiveTestStore()
	c.dirty["gc-1"] = struct{}{}
	c.absorbFreshLocked("gc-1", Bead{ID: "gc-1"}, now, absorbOpts{depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: false})
	if _, ok := c.dirty["gc-1"]; !ok {
		t.Fatal("clearDirty:false must leave the dirty mark in place")
	}
	if _, ok := c.deletedSeq["gc-1"]; ok {
		t.Fatal("absorb must clear the tombstone even when clearDirty is false")
	}
}

// T7 — seqKeep divergence guard.
//
// Event paths run noteMutationLocked (beadSeq only, NO localBeadAt) immediately
// before absorbing. seqKeep is load-bearing there: a seqClearGuarded at an
// event site would find no recent localBeadAt and DELETE the beadSeq fence the
// event just installed, so the next in-flight snapshot (an older-startSeq List
// or reconcile) would clobber the event's row — the #2210/#2987 stale-read
// class. These tests pin that the real event sites keep the fence and that a
// stale snapshot is rejected by it.

// TestApplyEventSitesPreserveBeadSeqFence exercises each real event absorb site
// (EV1 created, EV2 updated, EV3 closed) and asserts the beadSeq fence set by
// the event's noteMutationLocked survives the absorb (seqKeep).
func TestApplyEventSitesPreserveBeadSeqFence(t *testing.T) {
	t.Parallel()

	t.Run("bead.created", func(t *testing.T) {
		t.Parallel()
		backing := NewMemStore()
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		cache.ApplyEvent("bead.created", json.RawMessage(`{"id":"gc-created","status":"open","issue_type":"task"}`))
		assertBeadSeqPresent(t, cache, "gc-created")
	})

	t.Run("bead.updated", func(t *testing.T) {
		t.Parallel()
		backing := NewMemStore()
		bead, err := backing.Create(Bead{Title: "before", Status: "open", Type: "task"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		after := "after"
		if err := backing.Update(bead.ID, UpdateOpts{Title: &after}); err != nil {
			t.Fatalf("Update backing: %v", err)
		}
		cache.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+bead.ID+`","title":"after"}`))
		assertBeadSeqPresent(t, cache, bead.ID)
	})

	t.Run("bead.closed", func(t *testing.T) {
		t.Parallel()
		backing := NewMemStore()
		bead, err := backing.Create(Bead{Title: "open", Status: "open", Type: "task"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if err := backing.Close(bead.ID); err != nil {
			t.Fatalf("Close backing: %v", err)
		}
		cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+bead.ID+`","status":"closed"}`))
		assertBeadSeqPresent(t, cache, bead.ID)
	})
}

// TestStaleSnapshotDoesNotClobberFencedEventRow drives the real List-refresh
// merge (refreshCachedBeads) with a startSeq captured BEFORE an event and a
// stale snapshot of the pre-event row. The beadSeq fence the event installed
// (> startSeq) must reject the stale row: the cached (event) row survives and
// the stale value is never absorbed.
func TestStaleSnapshotDoesNotClobberFencedEventRow(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "before-event", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.mu.RLock()
	staleStartSeq := cache.mutationSeq
	cache.mu.RUnlock()

	// The event advances the row past staleStartSeq and installs the beadSeq
	// fence via noteMutationLocked; the backing is updated first so the event
	// carries no field conflict against the cache.
	after := "after-event"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &after}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}
	cache.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+bead.ID+`","title":"after-event"}`))

	cache.mu.RLock()
	fenced := cache.beadSeq[bead.ID] > staleStartSeq
	cache.mu.RUnlock()
	if !fenced {
		t.Fatalf("precondition: event did not install a beadSeq fence past startSeq")
	}

	staleItem := Bead{ID: bead.ID, Title: "before-event", Status: "open", Type: "task"}
	refreshed := cache.refreshCachedBeads(ListQuery{Status: "open"}, staleStartSeq, []Bead{staleItem})

	cache.mu.RLock()
	cachedTitle := cache.beads[bead.ID].Title
	cache.mu.RUnlock()
	if cachedTitle != "after-event" {
		t.Fatalf("stale snapshot clobbered the fenced row: cached title = %q, want %q", cachedTitle, "after-event")
	}
	for _, b := range refreshed {
		if b.ID == bead.ID && b.Title == "before-event" {
			t.Fatal("stale snapshot value was served past the beadSeq fence")
		}
	}
}

// TestAbsorbSeqModeDivergenceAtEventPreState meta-verifies the seqKeep vs
// seqClearGuarded divergence against the exact pre-state an event site leaves:
// beadSeq set, localBeadAt absent (noteMutationLocked stamps only beadSeq).
// seqKeep preserves the fence; seqClearGuarded — the wrong choice at an event
// site — deletes it, which is precisely the silent stale-read regression T7
// exists to catch.
func TestAbsorbSeqModeDivergenceAtEventPreState(t *testing.T) {
	t.Parallel()
	now := time.Now()

	seqKeepStore := newPrimitiveTestStore()
	seqKeepStore.beadSeq["gc-1"] = 9 // noteMutationLocked-style: beadSeq only
	seqKeepStore.absorbFreshLocked("gc-1", Bead{ID: "gc-1"}, now, absorbOpts{depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true})
	if seqKeepStore.beadSeq["gc-1"] != 9 {
		t.Fatal("seqKeep must preserve the event-installed beadSeq fence")
	}

	seqClearStore := newPrimitiveTestStore()
	seqClearStore.beadSeq["gc-1"] = 9 // same event pre-state: no localBeadAt
	seqClearStore.absorbFreshLocked("gc-1", Bead{ID: "gc-1"}, now, absorbOpts{depsMode: depsKeepCached, seqMode: seqClearGuarded, clearDirty: true})
	if _, ok := seqClearStore.beadSeq["gc-1"]; ok {
		t.Fatal("expected seqClearGuarded to DELETE the fence at the event pre-state (this is why event sites MUST use seqKeep)")
	}
}

// T6 — ApplyEvent OC-3 ordering: absorb installs the row BEFORE the
// deps-overlay (updateEventDepsLocked → setEventDepsLocked →
// clearReadyProjectionLocked), so the overlay observes the newly absorbed row.
// If the order were inverted, clearReadyProjectionLocked would no-op on the
// still-absent row and the projected IsBlocked would survive.
func TestApplyEventAbsorbsBeforeDepsOverlay_OC3(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "blocker", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// A created event that carries a projected IsBlocked AND dependency fields.
	// EV1 absorbs the row (IsBlocked=true) then runs the deps overlay, which
	// clears the projection now that authoritative deps are known.
	payload, err := json.Marshal(map[string]any{
		"id":         "gc-blocked",
		"status":     "open",
		"issue_type": "task",
		"is_blocked": true,
		"needs":      []string{blocker.ID},
	})
	if err != nil {
		t.Fatalf("marshal created event: %v", err)
	}
	cache.ApplyEvent("bead.created", payload)

	got, err := cache.Get("gc-blocked")
	if err != nil {
		t.Fatalf("Get after created event: %v", err)
	}
	if got.IsBlocked != nil {
		t.Fatalf("OC-3 violated: projected IsBlocked = %v, want nil (overlay must clear the newly absorbed row)", *got.IsBlocked)
	}

	cache.mu.RLock()
	_, hasDeps := cache.deps["gc-blocked"]
	cache.mu.RUnlock()
	if !hasDeps {
		t.Fatal("expected the deps overlay to install authoritative deps for the absorbed row")
	}
}

func assertBeadSeqPresent(t *testing.T, c *CachingStore, id string) {
	t.Helper()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, ok := c.beadSeq[id]; !ok {
		t.Fatalf("event site did not preserve the beadSeq fence for %s (seqKeep regression)", id)
	}
}

func assertAbsent(t *testing.T, c *CachingStore, id string) {
	t.Helper()
	if _, ok := c.beads[id]; ok {
		t.Fatalf("%s still in beads", id)
	}
	if _, ok := c.deps[id]; ok {
		t.Fatalf("%s still in deps", id)
	}
	if _, ok := c.dirty[id]; ok {
		t.Fatalf("%s still in dirty", id)
	}
	if _, ok := c.beadSeq[id]; ok {
		t.Fatalf("%s still in beadSeq", id)
	}
	if _, ok := c.localBeadAt[id]; ok {
		t.Fatalf("%s still in localBeadAt", id)
	}
	if _, ok := c.deletedSeq[id]; ok {
		t.Fatalf("%s still in deletedSeq", id)
	}
}
