package beads

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// counterMemStore is a MemStore that also implements Counter, so the
// differential can assert Count parity in the cap+1 regime where the overlay
// declines and Count delegates to the backing Counter (matching pre-change
// behavior for a Counter-capable backing such as the production BdStore).
type counterMemStore struct {
	*MemStore
}

func (s counterMemStore) Count(_ context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range rows {
		if slices.Contains(excludeTypes, b.Type) {
			continue
		}
		n++
	}
	return n, nil
}

// Ready honors IsBlocked via cachedBeadReady, matching the production SQL ready
// reader. Plain MemStore.Ready ignores IsBlocked, which would make the clean
// cache twin diverge from backing.Ready in the cap+1 fallback regime; a
// faithful backing keeps the twin a valid oracle across every dirty regime.
func (s counterMemStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	all, err := s.MemStore.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		return nil, err
	}
	statusByID := make(map[string]string, len(all))
	for _, b := range all {
		statusByID[b.ID] = b.Status
	}
	now := time.Now().UTC()
	var result []Bead
	for _, b := range all {
		if !IsReadyCandidateForTier(b, now, q.TierMode) {
			continue
		}
		if q.Assignee != "" && b.Assignee != q.Assignee {
			continue
		}
		deps, derr := s.MemStore.DepList(b.ID, "down")
		if derr != nil {
			return nil, derr
		}
		if !cachedBeadReady(b, statusByID, deps) {
			continue
		}
		result = append(result, cloneBead(b))
	}
	sortBeadsReadyOrder(result)
	if q.Limit > 0 && len(result) > q.Limit {
		result = result[:q.Limit]
	}
	return result, nil
}

// overlayCountingStore wraps a Store and records backing round-trips so the
// dirty-overlay perf assertions can prove that one dirty bead costs one
// backing.Get rather than a full backing.List/backing.Ready scan. getHook, if
// set, runs before each Get with no cache lock held so tests can inject
// mid-overlay mutations (the fence/race suite).
type overlayCountingStore struct {
	Store
	mu       sync.Mutex
	gets     int
	lists    int
	readies  int
	depLists int
	getHook  func(id string)
}

func (s *overlayCountingStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.gets++
	hook := s.getHook
	s.mu.Unlock()
	// Fetch first so the overlay receives this (possibly soon-to-be-stale)
	// snapshot, then run the hook to inject a concurrent mutation that lands
	// while the overlay holds no lock — exercising the beadSeq/deletedSeq fence.
	b, err := s.Store.Get(id)
	if hook != nil {
		hook(id)
	}
	return b, err
}

func (s *overlayCountingStore) List(query ListQuery) ([]Bead, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.List(query)
}

func (s *overlayCountingStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	s.mu.Lock()
	s.readies++
	s.mu.Unlock()
	return s.Store.Ready(query...)
}

func (s *overlayCountingStore) DepList(id, direction string) ([]Dep, error) {
	s.mu.Lock()
	s.depLists++
	s.mu.Unlock()
	return s.Store.DepList(id, direction)
}

func (s *overlayCountingStore) counts() (gets, lists, readies, depLists int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.lists, s.readies, s.depLists
}

func (s *overlayCountingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets, s.lists, s.readies, s.depLists = 0, 0, 0, 0
}

func (s *overlayCountingStore) setGetHook(hook func(id string)) {
	s.mu.Lock()
	s.getHook = hook
	s.mu.Unlock()
}

func markDirtyForTest(c *CachingStore, ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		c.markDirtyLocked(id)
	}
}

func beadIDSet(beads []Bead) map[string]Bead {
	m := make(map[string]Bead, len(beads))
	for _, b := range beads {
		m[b.ID] = b
	}
	return m
}

func sortedIDs(beads []Bead) []string {
	ids := make([]string, 0, len(beads))
	for _, b := range beads {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

// assertBeadsEquivalent compares two read results as multisets keyed by ID,
// checking the observable fields the read paths surface. Order-sensitive
// checks are covered separately (TestOverlayPreservesSortOrder).
func assertBeadsEquivalent(t *testing.T, ctx string, got, want []Bead) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len(got)=%d want=%d\n got=%v\nwant=%v", ctx, len(got), len(want), sortedIDs(got), sortedIDs(want))
	}
	gotByID := beadIDSet(got)
	for _, w := range want {
		g, ok := gotByID[w.ID]
		if !ok {
			t.Fatalf("%s: missing bead %q; got=%v want=%v", ctx, w.ID, sortedIDs(got), sortedIDs(want))
		}
		if g.Title != w.Title || g.Status != w.Status || g.Assignee != w.Assignee || g.Type != w.Type {
			t.Fatalf("%s: bead %q mismatch\n got=%+v\nwant=%+v", ctx, w.ID, g, w)
		}
	}
}

// TestOverlayReadEquivalenceDifferential is the headline read-equivalence test.
// For each seeded iteration it primes a store, drives it into a mixed dirty
// state (rows changed in backing, rows deleted from backing, and IDs never
// cached), then asserts every overlay-served read (List/Ready/Get/Count) is
// identical to a clean-primed twin store over the same backing — which the
// existing corpus proves equals the pre-change backing-served result. The
// twin is the ground truth: with no concurrent writers the dirty overlay must
// return exactly what a clean cache would (invariant I2).
func TestOverlayReadEquivalenceDifferential(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 5, 50, 500} {
		for _, k := range []int{0, 1, 2, dirtyOverlayMaxGets, dirtyOverlayMaxGets + 1} {
			seed := int64(n*1000 + k)
			t.Run(fmt.Sprintf("n%d_k%d", n, k), func(t *testing.T) {
				runOverlayDifferential(t, seed, n, k)
			})
		}
	}
}

func runOverlayDifferential(t *testing.T, seed int64, n, k int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	backing := counterMemStore{MemStore: NewMemStore()}

	statuses := []string{"open", "in_progress"}
	labels := []string{"alpha", "beta", "gamma"}
	assignees := []string{"", "ann", "bob"}

	var ids []string
	for i := 0; i < n; i++ {
		b := Bead{
			Title:    fmt.Sprintf("bead-%d", i),
			Status:   statuses[rng.Intn(len(statuses))],
			Assignee: assignees[rng.Intn(len(assignees))],
			Labels:   []string{labels[rng.Intn(len(labels))]},
			Metadata: map[string]string{"grp": fmt.Sprintf("g%d", rng.Intn(3))},
		}
		// Some beads carry a blocking dependency on an earlier bead via Needs,
		// so the fetched bead carries its dependency fields — the production
		// BdStore contract the overlay's depsFromFields absorb relies on.
		if i > 0 && rng.Intn(3) == 0 {
			b.Needs = []string{ids[rng.Intn(len(ids))]}
		}
		if rng.Intn(5) == 0 {
			blocked := rng.Intn(2) == 0
			b.IsBlocked = &blocked
		}
		created, err := backing.Create(b)
		if err != nil {
			t.Fatalf("seed=%d create: %v", seed, err)
		}
		ids = append(ids, created.ID)
	}

	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("seed=%d prime: %v", seed, err)
	}

	// Drive a mixed dirty state over K ids: mutate-in-backing, delete, or a
	// brand-new never-cached id. Every mutated id is also marked dirty so the
	// overlay is responsible for reconverging it (untouched-but-stale rows are
	// out of scope for a per-bead overlay).
	var dirtyIDs []string
	for i := 0; i < k; i++ {
		switch {
		case len(ids) > 0 && rng.Intn(3) == 0:
			id := ids[rng.Intn(len(ids))]
			newTitle := fmt.Sprintf("mutated-%d", i)
			newAssignee := assignees[rng.Intn(len(assignees))]
			_ = backing.Update(id, UpdateOpts{Title: &newTitle, Assignee: &newAssignee})
			dirtyIDs = append(dirtyIDs, id)
		case len(ids) > 0 && rng.Intn(2) == 0:
			id := ids[rng.Intn(len(ids))]
			_ = backing.Delete(id)
			dirtyIDs = append(dirtyIDs, id)
		default:
			created, err := backing.Create(Bead{Title: fmt.Sprintf("fresh-%d", i), Status: "open"})
			if err != nil {
				t.Fatalf("seed=%d create fresh: %v", seed, err)
			}
			dirtyIDs = append(dirtyIDs, created.ID)
		}
	}
	markDirtyForTest(store, dirtyIDs...)

	// Ground-truth twin: a clean cache primed on the now-current backing.
	twin := NewCachingStoreForTest(backing, nil)
	if err := twin.Prime(context.Background()); err != nil {
		t.Fatalf("seed=%d twin prime: %v", seed, err)
	}

	queries := []ListQuery{
		{AllowScan: true, Sort: SortCreatedAsc},
		{Status: "open", Sort: SortCreatedAsc},
		{Status: "in_progress", Sort: SortCreatedDesc},
		{Label: "alpha", Sort: SortCreatedAsc},
		{Assignee: "ann", Sort: SortCreatedAsc},
		{Metadata: map[string]string{"grp": "g1"}, Sort: SortCreatedAsc},
		{AllowScan: true, Limit: 3, Sort: SortCreatedAsc},
	}
	for i, q := range queries {
		gotList, gotErr := store.List(q)
		wantList, wantErr := twin.List(q)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("seed=%d q%d List err got=%v want=%v", seed, i, gotErr, wantErr)
		}
		assertBeadsEquivalent(t, fmt.Sprintf("seed=%d q%d List", seed, i), gotList, wantList)

		gotCount, gErr := store.Count(context.Background(), q)
		wantCount, wErr := twin.Count(context.Background(), q)
		if (gErr == nil) != (wErr == nil) {
			t.Fatalf("seed=%d q%d Count err got=%v want=%v", seed, i, gErr, wErr)
		}
		if gErr == nil && gotCount != wantCount {
			t.Fatalf("seed=%d q%d Count got=%d want=%d", seed, i, gotCount, wantCount)
		}
	}

	gotReady, err := store.Ready()
	if err != nil {
		t.Fatalf("seed=%d Ready: %v", seed, err)
	}
	// The clean twin uses the same cachedBeadReady code the overlay serves from,
	// and the faithful backing.Ready honors IsBlocked too, so the twin is a valid
	// ground truth in every dirty regime (overlay-served and cap+1 fallback).
	wantReady, err := twin.Ready()
	if err != nil {
		t.Fatalf("seed=%d twin Ready: %v", seed, err)
	}
	assertBeadsEquivalent(t, fmt.Sprintf("seed=%d Ready", seed), gotReady, wantReady)

	// Per-ID Get equivalence, including deleted (ErrNotFound) and fresh ids.
	allIDs := append(append([]string{}, ids...), dirtyIDs...)
	for _, id := range allIDs {
		gotBead, gotErr := store.Get(id)
		wantBead, wantErr := twin.Get(id)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("seed=%d Get(%s) err got=%v want=%v", seed, id, gotErr, wantErr)
		}
		if gotErr == nil && (gotBead.Title != wantBead.Title || gotBead.Status != wantBead.Status) {
			t.Fatalf("seed=%d Get(%s) got=%+v want=%+v", seed, id, gotBead, wantBead)
		}
	}
}

// TestOverlayPreservesSortOrder proves the overlay-served result keeps the
// exact sort+limit order of a clean cache for a deterministic sort.
func TestOverlayPreservesSortOrder(t *testing.T) {
	t.Parallel()
	backing := NewMemStore()
	var ids []string
	for i := 0; i < 12; i++ {
		created, err := backing.Create(Bead{Title: fmt.Sprintf("b%02d", i), Status: "open"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, created.ID)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	newTitle := "zzz-moved"
	if err := backing.Update(ids[0], UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("update: %v", err)
	}
	markDirtyForTest(store, ids[0])

	twin := NewCachingStoreForTest(backing, nil)
	if err := twin.Prime(context.Background()); err != nil {
		t.Fatalf("twin prime: %v", err)
	}

	for _, sort := range []SortOrder{SortCreatedAsc, SortCreatedDesc} {
		q := ListQuery{AllowScan: true, Sort: sort}
		got, err := store.List(q)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want, err := twin.List(q)
		if err != nil {
			t.Fatalf("twin List: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("sort=%s len got=%d want=%d", sort, len(got), len(want))
		}
		for i := range got {
			if got[i].ID != want[i].ID {
				t.Fatalf("sort=%s position %d got=%s want=%s", sort, i, got[i].ID, want[i].ID)
			}
		}
	}
}

// TestOverlayPerfRoundTripAccounting is the perf assertion: one dirty bead
// costs one backing.Get and zero backing.List/backing.Ready; a clean cache
// costs nothing; and the cap+1 case degrades to exactly today's single
// backing.List with no Gets.
func TestOverlayPerfRoundTripAccounting(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	var ids []string
	for i := 0; i < 3000; i++ {
		created, err := backing.Create(Bead{Title: fmt.Sprintf("b%d", i), Status: "open"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, created.ID)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Clean cache: overlay adds zero backing cost.
	backing.reset()
	if _, err := store.List(ListQuery{Status: "open"}); err != nil {
		t.Fatalf("clean List: %v", err)
	}
	if g, l, r, _ := backing.counts(); g != 0 || l != 0 || r != 0 {
		t.Fatalf("clean List backing calls: gets=%d lists=%d readies=%d, want 0/0/0", g, l, r)
	}

	// One dirty bead: exactly one backing.Get, zero backing.List.
	newTitle := "changed"
	if err := backing.Update(ids[0], UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("update: %v", err)
	}
	markDirtyForTest(store, ids[0])
	backing.reset()
	rows, err := store.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("dirty List: %v", err)
	}
	if g, l, _, _ := backing.counts(); g != 1 || l != 0 {
		t.Fatalf("1 dirty List backing calls: gets=%d lists=%d, want 1/0", g, l)
	}
	if len(rows) != 3000 {
		t.Fatalf("dirty List len=%d want 3000", len(rows))
	}
	// Second read: mark cleared, zero backing cost.
	backing.reset()
	if _, err := store.List(ListQuery{Status: "open"}); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if g, l, _, _ := backing.counts(); g != 0 || l != 0 {
		t.Fatalf("cleared List backing calls: gets=%d lists=%d, want 0/0", g, l)
	}

	// cap dirty beads: exactly cap Gets, zero List.
	for i := 0; i < dirtyOverlayMaxGets; i++ {
		markDirtyForTest(store, ids[i])
	}
	backing.reset()
	if _, err := store.List(ListQuery{Status: "open"}); err != nil {
		t.Fatalf("cap List: %v", err)
	}
	if g, l, _, _ := backing.counts(); g != dirtyOverlayMaxGets || l != 0 {
		t.Fatalf("cap List backing calls: gets=%d lists=%d, want %d/0", g, l, dirtyOverlayMaxGets)
	}

	// cap+1 dirty beads: fall back to exactly one backing.List, zero Gets.
	for i := 0; i < dirtyOverlayMaxGets+1; i++ {
		markDirtyForTest(store, ids[i])
	}
	backing.reset()
	if _, err := store.List(ListQuery{Status: "open"}); err != nil {
		t.Fatalf("cap+1 List: %v", err)
	}
	if g, l, _, _ := backing.counts(); g != 0 || l != 1 {
		t.Fatalf("cap+1 List backing calls: gets=%d lists=%d, want 0/1", g, l)
	}
}

// TestOverlayReadyPerfRoundTrip proves one dirty bead routes Ready through a
// single backing.Get, not a full backing.Ready scan.
func TestOverlayReadyPerfRoundTrip(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	var ids []string
	for i := 0; i < 200; i++ {
		created, err := backing.Create(Bead{Title: fmt.Sprintf("b%d", i), Status: "open"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, created.ID)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	newTitle := "changed"
	if err := backing.Update(ids[0], UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("update: %v", err)
	}
	markDirtyForTest(store, ids[0])
	backing.reset()
	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if g, _, r, _ := backing.counts(); g != 1 || r != 0 {
		t.Fatalf("1 dirty Ready backing calls: gets=%d readies=%d, want 1/0", g, r)
	}
}

// TestOverlayNotFoundSuppressed proves a dirty bead deleted from the backing is
// suppressed (omitted, matching what backing.List would return) and that each
// read pays exactly one bounded Get for it — never a full List.
func TestOverlayNotFoundSuppressed(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	keep, err := backing.Create(Bead{Title: "keep", Status: "open"})
	if err != nil {
		t.Fatalf("create keep: %v", err)
	}
	gone, err := backing.Create(Bead{Title: "gone", Status: "open"})
	if err != nil {
		t.Fatalf("create gone: %v", err)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if err := backing.Delete(gone.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	markDirtyForTest(store, gone.ID)

	backing.reset()
	rows, err := store.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != keep.ID {
		t.Fatalf("List = %v, want only %s", sortedIDs(rows), keep.ID)
	}
	if g, l, _, _ := backing.counts(); g != 1 || l != 0 {
		t.Fatalf("suppressed List backing calls: gets=%d lists=%d, want 1/0", g, l)
	}
	// The ErrNotFound mark is deliberately left set (convergence stays with the
	// reconciler), so a second read pays one bounded Get again, never a List.
	backing.reset()
	if _, err := store.List(ListQuery{Status: "open"}); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if g, l, _, _ := backing.counts(); g != 1 || l != 0 {
		t.Fatalf("second suppressed List backing calls: gets=%d lists=%d, want 1/0", g, l)
	}
}

// TestOverlayFenceMidOverlayLocalWrite proves a local write that lands after
// the overlay snapshot is never clobbered by the fetched row (invariant I3):
// the read reflects the newer local state or falls back, never the pre-update
// fetched row.
func TestOverlayFenceMidOverlayLocalWrite(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "orig", Status: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Backing carries a stale "fetched" value; the overlay Get returns it.
	staleTitle := "stale-fetch"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &staleTitle}); err != nil {
		t.Fatalf("update backing: %v", err)
	}
	markDirtyForTest(store, bead.ID)

	// When the overlay releases the lock to Get, a local write-through lands a
	// newer value and re-marks the row dirty, bumping beadSeq past the snapshot.
	// A plain atomic guard (not sync.Once, which is not reentrant) ensures the
	// nested refresh-Get inside store.Update does not recurse into the mutation.
	var fired atomic.Bool
	backing.setGetHook(func(id string) {
		if fired.Swap(true) {
			return
		}
		newTitle := "local-newer"
		if err := store.Update(id, UpdateOpts{Title: &newTitle}); err != nil {
			t.Errorf("mid-overlay local write: %v", err)
		}
	})

	rows, err := store.List(ListQuery{Status: "open"})
	backing.setGetHook(nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List len=%d want 1", len(rows))
	}
	if rows[0].Title == "stale-fetch" {
		t.Fatalf("overlay served the fenced-out fetched row %q (I3 violation)", rows[0].Title)
	}
	// Authoritative read must reflect the local write-through value.
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "local-newer" {
		t.Fatalf("Get title=%q want local-newer", got.Title)
	}
}

// TestOverlayMidOverlayDelete proves a mid-overlay delete+tombstone is honored:
// the row is omitted and the deletedSeq fence prevents resurrection (I3).
func TestOverlayMidOverlayDelete(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	keep, err := backing.Create(Bead{Title: "keep", Status: "open"})
	if err != nil {
		t.Fatalf("create keep: %v", err)
	}
	victim, err := backing.Create(Bead{Title: "victim", Status: "open"})
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	newTitle := "victim-changed"
	if err := backing.Update(victim.ID, UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("update: %v", err)
	}
	markDirtyForTest(store, victim.ID)

	var fired atomic.Bool
	backing.setGetHook(func(id string) {
		if id != victim.ID || fired.Swap(true) {
			return
		}
		if err := store.Delete(victim.ID); err != nil {
			t.Errorf("mid-overlay delete: %v", err)
		}
	})
	rows, err := store.List(ListQuery{Status: "open"})
	backing.setGetHook(nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := sortedIDs(rows); len(ids) != 1 || ids[0] != keep.ID {
		t.Fatalf("List = %v, want only %s (deleted row must not resurrect)", ids, keep.ID)
	}
	if _, err := store.Get(victim.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(victim) = %v, want ErrNotFound", err)
	}
}

// TestOverlayConcurrentHammer runs reads against writes under -race and checks
// no data race fires and reads stay internally consistent (I1/I7).
func TestOverlayConcurrentHammer(t *testing.T) {
	t.Parallel()
	backing := NewMemStore()
	var ids []string
	for i := 0; i < 40; i++ {
		created, err := backing.Create(Bead{Title: fmt.Sprintf("b%d", i), Status: "open"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, created.ID)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	deadline := time.Now().Add(750 * time.Millisecond)

	reader := func() {
		defer wg.Done()
		for !stop.Load() {
			_, _ = store.List(ListQuery{Status: "open"})
			_, _ = store.Ready()
			_, _ = store.Count(context.Background(), ListQuery{Status: "open"})
			if len(ids) > 0 {
				_, _ = store.Get(ids[0])
			}
		}
	}
	writer := func(seed int64) {
		defer wg.Done()
		rng := rand.New(rand.NewSource(seed))
		for !stop.Load() {
			id := ids[rng.Intn(len(ids))]
			title := fmt.Sprintf("w%d", rng.Intn(1000))
			_ = store.Update(id, UpdateOpts{Title: &title})
			markDirtyForTest(store, id)
		}
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go reader()
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go writer(int64(i + 1))
	}
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()

	// Read-your-writes probe (I1): after a settled write, the read paths must
	// reflect it, never a known-stale row.
	final := "final-value"
	if err := store.Update(ids[0], UpdateOpts{Title: &final}); err != nil {
		t.Fatalf("final update: %v", err)
	}
	got, err := store.Get(ids[0])
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if got.Title != final {
		t.Fatalf("final Get title=%q want %q", got.Title, final)
	}
}
