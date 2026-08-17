package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	rows, err := s.List(query)
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
	all, err := s.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
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
		deps, derr := s.DepList(b.ID, "down")
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
	mu      sync.Mutex
	gets    int
	lists   int
	readies int
	getHook func(id string)
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

func (s *overlayCountingStore) counts() (gets, lists, readies int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.lists, s.readies
}

func (s *overlayCountingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets, s.lists, s.readies = 0, 0, 0
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
// checking the full observable field set the read paths surface — not just
// Title/Status/Assignee/Type but also Labels, Metadata, and the IsBlocked
// ready-projection — so a divergence in any cached field (the #2987 regression
// clobbered deps, which surface through IsBlocked/DepList) fails the assertion.
// Order-sensitive checks are covered separately (TestOverlayPreservesSortOrder).
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
			t.Fatalf("%s: bead %q core-field mismatch\n got=%+v\nwant=%+v", ctx, w.ID, g, w)
		}
		if !slices.Equal(g.Labels, w.Labels) {
			t.Fatalf("%s: bead %q labels mismatch got=%v want=%v", ctx, w.ID, g.Labels, w.Labels)
		}
		if !maps.Equal(g.Metadata, w.Metadata) {
			t.Fatalf("%s: bead %q metadata mismatch got=%v want=%v", ctx, w.ID, g.Metadata, w.Metadata)
		}
		if !boolPtrEqual(g.IsBlocked, w.IsBlocked) {
			t.Fatalf("%s: bead %q IsBlocked mismatch got=%v want=%v", ctx, w.ID, ptrStr(g.IsBlocked), ptrStr(w.IsBlocked))
		}
	}
}

func ptrStr(b *bool) string {
	if b == nil {
		return "nil"
	}
	return fmt.Sprintf("%t", *b)
}

// assertDepsEquivalent compares two dependency rows as ID-keyed sets, ignoring
// order, so a cache that clobbered a blocked bead's deps to nil (the #2987
// regression) diverges from the ground-truth twin.
func assertDepsEquivalent(t *testing.T, ctx string, got, want []Dep) {
	t.Helper()
	norm := func(deps []Dep) []string {
		out := make([]string, 0, len(deps))
		for _, d := range deps {
			out = append(out, fmt.Sprintf("%s->%s(%s)", d.IssueID, d.DependsOnID, d.Type))
		}
		sort.Strings(out)
		return out
	}
	g, w := norm(got), norm(want)
	if !slices.Equal(g, w) {
		t.Fatalf("%s: deps mismatch\n got=%v\nwant=%v", ctx, g, w)
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
	if g, l, r := backing.counts(); g != 0 || l != 0 || r != 0 {
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
	if g, l, _ := backing.counts(); g != 1 || l != 0 {
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
	if g, l, _ := backing.counts(); g != 0 || l != 0 {
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
	if g, l, _ := backing.counts(); g != dirtyOverlayMaxGets || l != 0 {
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
	if g, l, _ := backing.counts(); g != 0 || l != 1 {
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
	if g, _, r := backing.counts(); g != 1 || r != 0 {
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
	if g, l, _ := backing.counts(); g != 1 || l != 0 {
		t.Fatalf("suppressed List backing calls: gets=%d lists=%d, want 1/0", g, l)
	}
	// The ErrNotFound mark is deliberately left set (convergence stays with the
	// reconciler), so a second read pays one bounded Get again, never a List.
	backing.reset()
	if _, err := store.List(ListQuery{Status: "open"}); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if g, l, _ := backing.counts(); g != 1 || l != 0 {
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

// depStrippingStore mirrors the fork's flagship native DoltLite read store
// (internal/beads/doltlite_read_store.go): its Get and List return beads with
// NO Dependencies/Needs fields and no denormalized IsBlocked projection — those
// live in separate dependency tables the row snapshot does not carry — while
// DepList, dependencySnapshotForCache, and a blocking-aware Ready serve the
// authoritative deps. A cache that absorbs a dirty row from this backing with
// depsFromFields would clobber the cached deps to nil, serving a blocked bead as
// ready and making DepList return empty (gastownhall/gascity#2987 class). It is
// the shim that reproduces the exact R1 failure the overlay rework must close.
type depStrippingStore struct {
	*MemStore
}

func stripDepFields(b Bead) Bead {
	b.Needs = nil
	b.Dependencies = nil
	b.IsBlocked = nil
	return b
}

func (s depStrippingStore) Get(id string) (Bead, error) {
	b, err := s.MemStore.Get(id)
	if err != nil {
		return Bead{}, err
	}
	return stripDepFields(b), nil
}

func (s depStrippingStore) List(query ListQuery) ([]Bead, error) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i] = stripDepFields(rows[i])
	}
	return rows, nil
}

func (s depStrippingStore) Count(_ context.Context, query ListQuery, excludeTypes ...string) (int, error) {
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

// Ready computes blocking via cachedBeadReady (IsBlocked is never carried by a
// DoltLite snapshot, so readiness falls to the dependency tables) and returns
// dep-stripped rows. Using the same readiness predicate the cache serves from —
// rather than MemStore.Ready, which treats a missing blocker as still blocking —
// keeps the clean twin a valid oracle across every dirty regime, including the
// cap+1 fallback where store.Ready delegates to backing.Ready.
func (s depStrippingStore) Ready(query ...ReadyQuery) ([]Bead, error) {
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
		deps, derr := s.DepList(b.ID, "down")
		if derr != nil {
			return nil, derr
		}
		if !cachedBeadReady(b, statusByID, deps) {
			continue
		}
		result = append(result, stripDepFields(cloneBead(b)))
	}
	sortBeadsReadyOrder(result)
	if q.Limit > 0 && len(result) > q.Limit {
		result = result[:q.Limit]
	}
	return result, nil
}

// dependencySnapshotForCache mirrors DoltliteReadStore: Prime (and thus the
// clean twin) sources complete deps here even though Get/List strip them.
func (s depStrippingStore) dependencySnapshotForCache(ids []string) (map[string][]Dep, bool, error) {
	deps, err := s.DepListBatch(ids)
	if err != nil {
		return deps, false, err
	}
	return deps, true, nil
}

func readyIDs(t *testing.T, store *CachingStore) []string {
	t.Helper()
	rows, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	return sortedIDs(rows)
}

// TestOverlayDeplessBackingBlockedBeadNotServedReady is the deterministic proof
// that the overlay closes the R1 (#2987-class) regression on a backing whose
// Get carries no dependency fields: a blocked, dirty bead must never be served
// as ready and its DepList must never wrongly return empty after the overlay
// refresh. This test FAILS on the pre-rework overlay (depsFromFields clobbers
// c.deps[blocked] to nil) and passes once the overlay sources deps from an
// explicit backing.DepList for dep-less rows.
func TestOverlayDeplessBackingBlockedBeadNotServedReady(t *testing.T) {
	t.Parallel()
	backing := depStrippingStore{MemStore: NewMemStore()}
	blocker, err := backing.Create(Bead{Title: "blocker", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := backing.Create(Bead{Title: "blocked", Status: "open", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}

	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// The clean cache must already exclude the blocked bead from Ready.
	if ready := readyIDs(t, store); slices.Contains(ready, blocked.ID) {
		t.Fatalf("clean cache served blocked bead as ready: %v", ready)
	}

	// Mutate blocked in backing (title only — its deps are unchanged) and mark it
	// dirty so the overlay refreshes it via backing.Get, which carries no dep
	// fields. The overlay must NOT clobber blocked's cached deps to nil.
	newTitle := "blocked-v2"
	if err := backing.Update(blocked.ID, UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("update: %v", err)
	}
	markDirtyForTest(store, blocked.ID)

	if ready := readyIDs(t, store); slices.Contains(ready, blocked.ID) {
		t.Fatalf("R1 regression: dirty-overlay served blocked bead as ready (deps clobbered to nil): %v", ready)
	}
	deps, err := store.DepList(blocked.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) == 0 {
		t.Fatalf("R1 regression: DepList(blocked) returned empty after dirty overlay")
	}
	assertDepsEquivalent(t, "blocked deps after overlay", deps, []Dep{{IssueID: blocked.ID, DependsOnID: blocker.ID, Type: "blocks"}})
	got, err := store.Get(blocked.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != newTitle {
		t.Fatalf("overlay did not surface refreshed title: got %q want %q", got.Title, newTitle)
	}
}

// TestOverlayDeplessBackingReadEquivalenceDifferential is the R1 differential
// over a dep-less backing.Get shape (depStrippingStore). It complements the
// dep-carrying TestOverlayReadEquivalenceDifferential so T1 exercises BOTH
// backing shapes. Every overlay-served read (List/Ready/Count/DepList) must
// equal a clean-primed twin over the same backing, in every dirty regime — the
// blocked-bead deps clobber shows up as a Ready/DepList divergence here.
func TestOverlayDeplessBackingReadEquivalenceDifferential(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 5, 50, 500} {
		for _, k := range []int{0, 1, 2, dirtyOverlayMaxGets, dirtyOverlayMaxGets + 1} {
			seed := int64(n*1000 + k)
			t.Run(fmt.Sprintf("n%d_k%d", n, k), func(t *testing.T) {
				runDeplessOverlayDifferential(t, seed, n, k)
			})
		}
	}
}

func runDeplessOverlayDifferential(t *testing.T, seed int64, n, k int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	backing := depStrippingStore{MemStore: NewMemStore()}

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
		// Roughly half the beads carry a blocking dependency on an earlier bead.
		// Because the backing strips dep fields on Get, the overlay can only keep
		// these blocked beads out of Ready by sourcing deps from backing.DepList.
		if i > 0 && rng.Intn(2) == 0 {
			b.Needs = []string{ids[rng.Intn(len(ids))]}
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

	// Drive a mixed dirty state. Bias the first dirty pick toward a bead that
	// carries a dependency so a blocked+dirty row is exercised whenever one
	// exists; the rest are random mutate/delete/fresh like the dep-carrying twin.
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

	twin := NewCachingStoreForTest(backing, nil)
	if err := twin.Prime(context.Background()); err != nil {
		t.Fatalf("seed=%d twin prime: %v", seed, err)
	}

	queries := []ListQuery{
		{AllowScan: true, Sort: SortCreatedAsc},
		{Status: "open", Sort: SortCreatedAsc},
		{Label: "alpha", Sort: SortCreatedAsc},
		{Assignee: "ann", Sort: SortCreatedAsc},
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

	// Ready is where the deps clobber surfaces: a blocked bead must stay out.
	gotReady, err := store.Ready()
	if err != nil {
		t.Fatalf("seed=%d Ready: %v", seed, err)
	}
	wantReady, err := twin.Ready()
	if err != nil {
		t.Fatalf("seed=%d twin Ready: %v", seed, err)
	}
	assertBeadsEquivalent(t, fmt.Sprintf("seed=%d Ready", seed), gotReady, wantReady)

	// DepList equivalence after the overlay ran: a clobbered row would report an
	// empty dep set where the twin still sees the blocking dependency.
	allIDs := append(append([]string{}, ids...), dirtyIDs...)
	for _, id := range allIDs {
		gotDeps, gotErr := store.DepList(id, "down")
		wantDeps, wantErr := twin.DepList(id, "down")
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("seed=%d DepList(%s) err got=%v want=%v", seed, id, gotErr, wantErr)
		}
		if gotErr == nil {
			assertDepsEquivalent(t, fmt.Sprintf("seed=%d DepList(%s)", seed, id), gotDeps, wantDeps)
		}
	}
}

// TestOverlayDeterministicPassTwoRetry proves the bounded retry deterministically
// absorbs a new dirty mark that lands on a DIFFERENT id mid-overlay: pass 1
// fetches A, a mid-fetch write dirties B, and pass 2 absorbs B — with no
// fallback backing.List. This is the deterministic companion to the probabilistic
// concurrent hammer.
func TestOverlayDeterministicPassTwoRetry(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	a, err := backing.Create(Bead{Title: "a", Status: "open"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := backing.Create(Bead{Title: "b", Status: "open"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	ta, tb := "a-v2", "b-v2"
	if err := backing.Update(a.ID, UpdateOpts{Title: &ta}); err != nil {
		t.Fatalf("update a: %v", err)
	}
	if err := backing.Update(b.ID, UpdateOpts{Title: &tb}); err != nil {
		t.Fatalf("update b: %v", err)
	}
	markDirtyForTest(store, a.ID)

	var fired atomic.Bool
	backing.setGetHook(func(id string) {
		if id != a.ID || fired.Swap(true) {
			return
		}
		markDirtyForTest(store, b.ID)
	})
	backing.reset()
	rows, err := store.List(ListQuery{Status: "open"})
	backing.setGetHook(nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := beadIDSet(rows)
	if got[a.ID].Title != ta || got[b.ID].Title != tb {
		t.Fatalf("pass-2 did not absorb both refreshed rows: a=%q b=%q", got[a.ID].Title, got[b.ID].Title)
	}
	if g, l, _ := backing.counts(); l != 0 || g != 2 {
		t.Fatalf("deterministic pass-2: want gets=2 lists=0, got gets=%d lists=%d", g, l)
	}
}

// TestOverlayChurnEveryPassFallsBack proves that when every pass introduces a
// fresh dirty mark on a new id, the bounded 2-pass overlay stops chasing churn
// and falls back to a single backing.List — never looping unboundedly.
func TestOverlayChurnEveryPassFallsBack(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	a, err := backing.Create(Bead{Title: "a", Status: "open"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	var extras []string
	for i := 0; i < 5; i++ {
		e, err := backing.Create(Bead{Title: fmt.Sprintf("e%d", i), Status: "open"})
		if err != nil {
			t.Fatalf("create extra: %v", err)
		}
		extras = append(extras, e.ID)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	ta := "a-v2"
	if err := backing.Update(a.ID, UpdateOpts{Title: &ta}); err != nil {
		t.Fatalf("update a: %v", err)
	}
	markDirtyForTest(store, a.ID)

	var idx atomic.Int32
	backing.setGetHook(func(_ string) {
		i := int(idx.Add(1)) - 1
		if i < len(extras) {
			markDirtyForTest(store, extras[i])
		}
	})
	backing.reset()
	rows, err := store.List(ListQuery{Status: "open"})
	backing.setGetHook(nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, l, _ := backing.counts(); l == 0 {
		t.Fatalf("expected fallback backing.List after churn on every pass, got lists=0")
	}
	if len(rows) != 6 {
		t.Fatalf("fallback List len=%d want 6", len(rows))
	}
}

// TestOverlaySuppressedResurrectionFence proves the symmetric fence for
// ErrNotFound-suppressed ids: if a concurrent event-apply resurrects a suppressed
// row as a live, clean bead between its fetch and the overlay's re-lock, the
// overlay must re-fetch it rather than omit it — otherwise the serve returns the
// cache MINUS a now-live row (a torn read). This test FAILS on the pre-rework
// overlay, which unconditionally omits every suppressed id.
func TestOverlaySuppressedResurrectionFence(t *testing.T) {
	t.Parallel()
	backing := &overlayCountingStore{Store: NewMemStore()}
	keep, err := backing.Create(Bead{Title: "keep", Status: "open"})
	if err != nil {
		t.Fatalf("create keep: %v", err)
	}
	ghost, err := backing.Create(Bead{Title: "ghost", Status: "open"})
	if err != nil {
		t.Fatalf("create ghost: %v", err)
	}
	store := NewCachingStoreForTest(backing, nil)
	if err := store.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Delete ghost from the backing and mark it dirty so the overlay Gets
	// ErrNotFound and suppresses it. The mid-fetch hook then races the re-lock by
	// recreating ghost in the backing AND applying a bead.updated event that
	// re-installs it as a live, non-dirty cache row (bumping its mutation seq
	// past the overlay snapshot) — the exact concurrent event-apply the symmetric
	// fence must catch.
	if err := backing.Delete(ghost.ID); err != nil {
		t.Fatalf("delete ghost: %v", err)
	}
	markDirtyForTest(store, ghost.ID)

	mem := backing.Store.(*MemStore)
	var fired atomic.Bool
	backing.setGetHook(func(id string) {
		if id != ghost.ID || fired.Swap(true) {
			return
		}
		mem.mu.Lock()
		mem.beads = append(mem.beads, Bead{ID: ghost.ID, Title: "ghost-reborn", Status: "open", Type: "task", CreatedAt: time.Now()})
		mem.mu.Unlock()
		store.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+ghost.ID+`","title":"ghost-reborn"}`))
	})
	rows, err := store.List(ListQuery{Status: "open"})
	backing.setGetHook(nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := sortedIDs(rows)
	if !slices.Contains(ids, ghost.ID) {
		t.Fatalf("suppressed-fence: resurrected row omitted from result (torn read): got %v, want it to include %s", ids, ghost.ID)
	}
	if !slices.Contains(ids, keep.ID) {
		t.Fatalf("List dropped the untouched keep row: %v", ids)
	}
}
