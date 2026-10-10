package beads

import (
	"context"
	"testing"
	"time"
)

// mc-xlphf: an out-of-process write to a row this cache wrote less than five
// seconds earlier (recentLocalMutation) was dropped at read time, unverified
// and leaving the row clean, so the cache served the controller's own older
// write until the next scan (64s and 93s on a split city, where an operator's
// `gc session suspend` landed ~1s after the controller's state=awake). These
// tests write through the cache, then write around it as another process
// would, then deliver that write's event, with the cache's clock frozen so the
// local write stays inside the window.

// recentLocalWriteThenExternal writes state=awake through a cache over backing
// at a frozen clock, then state=suspended around it, and returns the cache, the
// row id and the external write's event payload. It fails when the local write
// left no beadSeq fence or fell outside the recency window: that is the state
// the dropping branch needs, so without it the test is vacuous.
func recentLocalWriteThenExternal(t *testing.T, backing Store) (*CachingStore, string, Bead) {
	t.Helper()
	row, err := backing.Create(Bead{Title: "seat"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	frozen := time.Now()
	cache := NewCachingStoreForTest(backing, nil, WithClock(func() time.Time { return frozen }))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.SetMetadata(row.ID, "state", "awake"); err != nil {
		t.Fatalf("SetMetadata through the cache: %v", err)
	}
	cache.mu.RLock()
	_, mutated := cache.beadSeq[row.ID]
	recent := recentLocalMutation(cache.localBeadAt[row.ID], cache.clockNow())
	cache.mu.RUnlock()
	if !mutated || !recent {
		t.Fatalf("after the local write: beadSeq set %v, recent %v; want both", mutated, recent)
	}
	external := writeMetadata(t, backing, row.ID, "state", "suspended")
	return cache, row.ID, external
}

// An external write inside the window that the backing confirms applies: the
// cached row reads it at once, clean. Kills the base code (the read-phase
// drop on a recently local, locally mutated row).
func TestCachingStoreExternalWriteAfterRecentLocalWriteApplies(t *testing.T) {
	t.Parallel()
	for _, b := range staleEventBackings {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			cache, id, external := recentLocalWriteThenExternal(t, b.open(t))
			cache.ApplyEvent("bead.updated", eventPayload(t, external))
			got, dirty, _ := rowFences(cache, id)
			if got.Metadata["state"] != "suspended" || dirty {
				t.Fatalf("after the external write's event: cached state %q, dirty %v; want suspended, clean",
					got.Metadata["state"], dirty)
			}
		})
	}
}

// An external write inside the window whose check cannot confirm it never
// leaves the controller's older row clean: the row goes dirty and stamped, and
// the next Get reads the backing. The three shapes are a failed check, a check
// past its deadline on a subprocess backing (bd over hosted Postgres: every
// check), and a backing read that lags the event (it still returns the cached
// row). Kills the base code, and a recently local mismatch dropped without
// settling (lagging).
func TestCachingStoreUnconfirmedExternalWriteAfterRecentLocalWriteDirtiesTheRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// open returns the backing and arms the shape once the setup has
		// written through it.
		open func(t *testing.T) (Store, func(*CachingStore, string), func())
	}{
		{"error", func(*testing.T) (Store, func(*CachingStore, string), func()) {
			backing := &casBackingStore{Store: NewMemStore()}
			return backing, func(*CachingStore, string) { backing.failNextGet = true }, func() {}
		}},
		{"deadline", func(*testing.T) (Store, func(*CachingStore, string), func()) {
			backing := &blockingSubprocessStore{MemStore: NewMemStore()}
			block := make(chan struct{})
			arm := func(cache *CachingStore, _ string) {
				backing.block = block
				cache.eventCheckAfter = func(time.Duration) <-chan time.Time {
					fired := make(chan time.Time)
					close(fired)
					return fired
				}
			}
			return backing, arm, func() { close(block) }
		}},
		{"lagging", func(*testing.T) (Store, func(*CachingStore, string), func()) {
			backing := &casBackingStore{Store: NewMemStore()}
			arm := func(cache *CachingStore, id string) {
				cache.mu.RLock()
				lag := cloneBead(cache.beads[id])
				cache.mu.RUnlock()
				backing.staleNextGet = &lag
			}
			return backing, arm, func() {}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing, arm, release := tc.open(t)
			cache, id, external := recentLocalWriteThenExternal(t, backing)
			_, _, before := rowFences(cache, id)
			arm(cache, id)
			cache.ApplyEvent("bead.updated", eventPayload(t, external))
			got, dirty, seq := rowFences(cache, id)
			if !dirty || got.Metadata["state"] != "awake" {
				t.Fatalf("after an unconfirmed external write: dirty %v, cached state %q; want dirty, awake",
					dirty, got.Metadata["state"])
			}
			if tc.name != "lagging" && seq <= before {
				t.Fatalf("the dirty mark left beadSeq %d, not stamped past %d", seq, before)
			}
			release()
			read, err := cache.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if read.Metadata["state"] != "suspended" {
				t.Fatalf("the next Get read state %q, want the external write's suspended", read.Metadata["state"])
			}
		})
	}
}

// A stale event inside the window, the echo of an earlier local write that a
// later one superseded, does not roll the cached row back. Pins the guard the
// read-phase drop stood for: verification keeps it.
func TestCachingStoreStaleEchoAfterRecentLocalWriteKeepsTheRow(t *testing.T) {
	t.Parallel()
	for _, b := range staleEventBackings {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			backing := b.open(t)
			row, err := backing.Create(Bead{Title: "seat"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			frozen := time.Now()
			cache := NewCachingStoreForTest(backing, nil, WithClock(func() time.Time { return frozen }))
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if err := cache.SetMetadata(row.ID, "state", "creating"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			echo, err := backing.Get(row.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := cache.SetMetadata(row.ID, "state", "awake"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			cache.ApplyEvent("bead.updated", eventPayload(t, echo))
			got, _, _ := rowFences(cache, row.ID)
			if got.Metadata["state"] != "awake" {
				t.Fatalf("a stale echo rolled the cached state back to %q, want awake", got.Metadata["state"])
			}
			read, err := cache.Get(row.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if read.Metadata["state"] != "awake" {
				t.Fatalf("Get read state %q after the stale echo, want awake", read.Metadata["state"])
			}
		})
	}
}

// Inside the window, an event older than the cached row by updated_at, the echo
// of a superseded local write, drops without a backing read and leaves the row
// clean; one tied with the row's stamp or carrying none is checked, and, its
// read equalling the cached row, marks it dirty as any unconfirmed field update
// does (mc-03lk4). None rolls the row back. Kills: the
// echo verified anyway (a bd check outlasts its deadline and dirties the row on
// every reordered echo), and a tie or a missing stamp read as older.
func TestCachingStoreSupersededEchoInsideWindowReadsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		stamp     func(row Bead) time.Time
		wantReads bool
	}{
		{"older", func(row Bead) time.Time { return row.UpdatedAt.Add(-time.Millisecond) }, false},
		{"tied", func(row Bead) time.Time { return row.UpdatedAt }, true},
		{"unstamped", func(Bead) time.Time { return time.Time{} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			row, err := backing.Create(Bead{Title: "seat"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			frozen := time.Now()
			cache := NewCachingStoreForTest(backing, nil, WithClock(func() time.Time { return frozen }))
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if err := cache.SetMetadata(row.ID, "state", "creating"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			echo, err := backing.Store.Get(row.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := cache.SetMetadata(row.ID, "state", "awake"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			cached, _, _ := rowFences(cache, row.ID)
			if cached.UpdatedAt.IsZero() {
				t.Fatal("the cached row carries no updated_at; the stamps compare nothing")
			}
			// The echo's stamp is set relative to the cached row's, which
			// the backing stamped from its own clock.
			echo.UpdatedAt = tc.stamp(cached)
			reads := backing.getCalls
			cache.ApplyEvent("bead.updated", eventPayload(t, echo))
			got, dirty, _ := rowFences(cache, row.ID)
			if got.Metadata["state"] != "awake" || dirty != tc.wantReads {
				t.Fatalf("after the echo: cached state %q, dirty %v; want awake, dirty %v",
					got.Metadata["state"], dirty, tc.wantReads)
			}
			if read := backing.getCalls > reads; read != tc.wantReads {
				t.Fatalf("the echo read the backing: %v, want %v", read, tc.wantReads)
			}
		})
	}
}
