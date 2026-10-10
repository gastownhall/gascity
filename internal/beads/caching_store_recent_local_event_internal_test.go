package beads

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
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

// sameSecondStore stamps every row it returns with one updated_at, as a
// Dolt-family backing (DATETIME, whole seconds) does for writes within one
// second, and counts its point reads.
type sameSecondStore struct {
	*casBackingStore
	stamp time.Time
}

func (s *sameSecondStore) Get(id string) (Bead, error) {
	b, err := s.casBackingStore.Get(id)
	b.UpdatedAt = s.stamp
	return b, err
}

func (s *sameSecondStore) List(q ListQuery) ([]Bead, error) {
	rows, err := s.casBackingStore.List(q)
	for i := range rows {
		rows[i].UpdatedAt = s.stamp
	}
	return rows, err
}

// ownEchoFixture is a cache over a sameSecondStore at a frozen clock that has
// written one row twice, state=creating then state=awake, recording what it
// emitted for the row.
type ownEchoFixture struct {
	backing *sameSecondStore
	cache   *CachingStore
	id      string
	echoes  []json.RawMessage // in emission order
	clock   atomic.Int64      // the cache's clock, in UnixNano
}

func newOwnEchoFixture(t *testing.T) *ownEchoFixture {
	t.Helper()
	f := &ownEchoFixture{backing: &sameSecondStore{
		casBackingStore: &casBackingStore{Store: NewMemStore()},
		stamp:           time.Now().Truncate(time.Second),
	}}
	row, err := f.backing.Create(Bead{Title: "seat", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.id = row.ID
	var mu sync.Mutex
	f.clock.Store(time.Now().UnixNano())
	f.cache = NewCachingStoreForTest(f.backing, func(_, id string, payload json.RawMessage) {
		if id == f.id {
			mu.Lock()
			defer mu.Unlock()
			f.echoes = append(f.echoes, payload)
		}
	}, WithClock(func() time.Time { return time.Unix(0, f.clock.Load()) }))
	if err := f.cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return f
}

// writeTwice writes state=creating then, gap later on the cache's clock,
// state=awake.
func (f *ownEchoFixture) writeTwice(t *testing.T, gap time.Duration) {
	t.Helper()
	for i, state := range []string{"creating", "awake"} {
		if i > 0 {
			f.clock.Add(int64(gap))
		}
		if err := f.cache.SetMetadata(f.id, "state", state); err != nil {
			t.Fatalf("SetMetadata %s: %v", state, err)
		}
	}
	if len(f.echoes) != 2 {
		t.Fatalf("emitted %d snapshots of the row, want 2", len(f.echoes))
	}
}

// reapplied returns echo with fn applied to its decoded fields.
func reapplied(t *testing.T, echo json.RawMessage, fn func(map[string]any)) json.RawMessage {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(echo, &fields); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	fn(fields)
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode echo: %v", err)
	}
	return out
}

// On a backing that stamps whole seconds, the cache's own echo of the first of
// two writes in one second, fed back after the second, drops without a backing
// read and leaves the row clean and the census serving; anything else that
// conflicts inside the window is checked. Kills: own echoes told by updated_at
// (they tie here and dirty the row), a tie read as older (drops the operator's
// same-second write), and each identity condition dropped: the stamp, the
// complete-snapshot requirement, the delete exclusion and the beadSeq fence.
func TestCachingStoreOwnEchoInsideWindowReadsNothing(t *testing.T) {
	t.Parallel()
	t.Run("own echo", func(t *testing.T) {
		t.Parallel()
		f := newOwnEchoFixture(t)
		f.writeTwice(t, 0)
		reads := f.backing.getCalls
		for _, echo := range f.echoes {
			f.cache.ApplyEventSnapshot("bead.updated", echo)
		}
		got, dirty, _ := rowFences(f.cache, f.id)
		if got.Metadata["state"] != "awake" || dirty || f.backing.getCalls != reads {
			t.Fatalf("after the own echoes: state %q, dirty %v, %d reads; want awake, clean, none",
				got.Metadata["state"], dirty, f.backing.getCalls-reads)
		}
		if _, ok := f.cache.CachedList(ListQuery{Status: "open"}); !ok {
			t.Fatal("the census declined after the cache's own echoes")
		}
	})
	t.Run("operator write in the same second", func(t *testing.T) {
		t.Parallel()
		f := newOwnEchoFixture(t)
		f.writeTwice(t, 0)
		external := writeMetadata(t, f.backing, f.id, "state", "suspended")
		f.cache.ApplyEventSnapshot("bead.updated", eventPayload(t, external))
		got, _, _ := rowFences(f.cache, f.id)
		if got.Metadata["state"] != "suspended" {
			t.Fatalf("a same-second operator write left cached state %q, want suspended", got.Metadata["state"])
		}
	})
	for _, tc := range []struct {
		name      string
		eventType string
		event     func(t *testing.T, echo json.RawMessage) json.RawMessage
	}{
		{"restamped", "bead.updated", func(t *testing.T, echo json.RawMessage) json.RawMessage {
			return reapplied(t, echo, func(m map[string]any) {
				m["updated_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			})
		}},
		{"partial", "bead.updated", func(t *testing.T, echo json.RawMessage) json.RawMessage {
			return reapplied(t, echo, func(m map[string]any) { delete(m, "title") })
		}},
		{"deleted", "bead.deleted", func(_ *testing.T, echo json.RawMessage) json.RawMessage { return echo }},
	} {
		t.Run("checked: "+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newOwnEchoFixture(t)
			f.writeTwice(t, 0)
			reads := f.backing.getCalls
			f.cache.ApplyEventSnapshot(tc.eventType, tc.event(t, f.echoes[0]))
			got, _, _ := rowFences(f.cache, f.id)
			if f.backing.getCalls == reads || got.Metadata["state"] != "awake" {
				t.Fatalf("after the event: %d reads, cached state %q; want checked, awake",
					f.backing.getCalls-reads, got.Metadata["state"])
			}
		})
	}
	// An emission older than the window is no echo the window vouches for,
	// even while a later write keeps the row inside it: here the first
	// emission is 5.5s old, the second write 4.5s.
	t.Run("checked: emitted before the window", func(t *testing.T) {
		t.Parallel()
		f := newOwnEchoFixture(t)
		f.writeTwice(t, time.Second)
		f.clock.Add(int64(4500 * time.Millisecond))
		reads := f.backing.getCalls
		f.cache.ApplyEventSnapshot("bead.updated", f.echoes[0])
		got, _, _ := rowFences(f.cache, f.id)
		if f.backing.getCalls == reads || got.Metadata["state"] != "awake" {
			t.Fatalf("after the echo: %d reads, cached state %q; want checked, awake",
				f.backing.getCalls-reads, got.Metadata["state"])
		}
	})
	// A dirty-row Get clears beadSeq and keeps the window's stamp; there an
	// echo is checked, as main's in-window verification checked it.
	t.Run("checked: no beadSeq", func(t *testing.T) {
		t.Parallel()
		f := newOwnEchoFixture(t)
		if err := f.cache.SetMetadata(f.id, "state", "creating"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		f.backing.failNextGet = true // the Update's refresh fails: the row goes dirty
		if err := f.cache.Update(f.id, UpdateOpts{Metadata: map[string]string{"state": "awake"}}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, err := f.cache.Get(f.id); err != nil {
			t.Fatalf("Get: %v", err)
		}
		f.cache.mu.RLock()
		_, mutated := f.cache.beadSeq[f.id]
		f.cache.mu.RUnlock()
		if mutated {
			t.Fatal("the dirty-row Get left beadSeq set; the case is vacuous")
		}
		reads := f.backing.getCalls
		f.cache.ApplyEventSnapshot("bead.updated", f.echoes[0])
		got, _, _ := rowFences(f.cache, f.id)
		if f.backing.getCalls == reads || got.Metadata["state"] != "awake" {
			t.Fatalf("after the echo: %d reads, cached state %q; want checked, awake",
				f.backing.getCalls-reads, got.Metadata["state"])
		}
	})
}
