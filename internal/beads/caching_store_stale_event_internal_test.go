package beads

import (
	"errors"
	"testing"
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
	if err := backing.SetMetadata(id, "held_until", value); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	b, err := backing.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return b
}

// An event reordered after a rescan neither regresses the cached row's fields
// nor pairs them with its revision. Kills: a clean row's conflicting
// bead.updated merged without the backing verification (staleRisk dropped).
func TestCachingStoreStaleEventAfterRescanKeepsRowAndRevision(t *testing.T) {
	t.Parallel()
	for _, b := range staleEventBackings {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			cache, id, newer := staleEventAfterRescan(t, b.open(t))
			cache.mu.RLock()
			got := cache.beads[id]
			cache.mu.RUnlock()
			if got.Metadata["held_until"] != "new" || got.Revision != newer.Revision {
				t.Fatalf("cached row = held_until %q at revision %d, want %q at %d",
					got.Metadata["held_until"], got.Revision, "new", newer.Revision)
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
