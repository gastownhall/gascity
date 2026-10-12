package beadstest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ReadyParityHarness supplies what the ready-parity suite cannot reach through
// the beads.Store interface.
type ReadyParityHarness struct {
	// Open returns a fresh, empty backing store.
	Open func(t *testing.T) beads.Store
	// Rescan runs one full reconcile pass of cache against its backing: the
	// pass the cache's background loop runs on its cadence.
	Rescan func(cache *beads.CachingStore)
}

// ReadyParityOptions controls governed opt-outs of the ready-parity suite.
type ReadyParityOptions struct {
	// SkipCachedReadyParity requests skipping the CachedReadyParity subtest. It
	// is honored only when a valid, unexpired entry for that subtest exists in
	// the skip ledger; otherwise the subtest fails loudly.
	SkipCachedReadyParity bool
}

// readyParityFarFuture defers a row past any date the suite will run on.
var readyParityFarFuture = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)

// readyParitySubtest is the ledger key of the whole parity contract.
const readyParitySubtest = "CachedReadyParity"

// readyParityQueries are the ready shapes compared: both tier modes a census
// reads, the wisp tier, an assignee filter, and Limit prefixes, which only
// agree when both sides sort before they cut.
var readyParityQueries = []beads.ReadyQuery{
	{},
	{TierMode: beads.TierBoth},
	{TierMode: beads.TierWisps},
	{Assignee: "worker"},
	{Limit: 3},
	{TierMode: beads.TierBoth, Limit: 4},
}

// RunReadyParityConformance checks that a primed CachingStore over the store
// answers Ready with the same rows in the same order as the store itself, on a
// fixed corpus and after seeded random mutations made through the cache and
// behind it, each followed by a re-scan. That equality is what lets a
// controller serve a store's ready demand from the cache: a converged cache
// must not offer work the store holds back (a missing, foreign, or
// closed-with-work_outcome=blocked blocker), and a bounded read must cut the
// same canonical (priority, created_at, id) prefix on both paths.
func RunReadyParityConformance(t *testing.T, name string, h ReadyParityHarness) {
	RunReadyParityConformanceWithOptions(t, name, h, ReadyParityOptions{})
}

// RunReadyParityConformanceWithOptions runs the ready-parity suite with
// ledger-governed opt-outs.
func RunReadyParityConformanceWithOptions(t *testing.T, name string, h ReadyParityHarness, opts ReadyParityOptions) {
	t.Helper()
	t.Run(name+"/"+readyParitySubtest, func(t *testing.T) {
		if opts.SkipCachedReadyParity {
			requireLedgeredSkip(t, readyParitySubtest)
		}
		t.Run("Fixtures", func(t *testing.T) {
			backing := h.Open(t)
			seedReadyParityCorpus(t, backing)
			cache := primeReadyParityCache(t, backing)
			assertReadyParity(t, "primed", cache, backing)
			h.Rescan(cache)
			assertReadyParity(t, "re-scanned", cache, backing)
		})
		t.Run("Randomized", func(t *testing.T) {
			for _, seed := range []uint64{1, 2, 3} {
				t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
					runReadyParityRandomized(t, h, seed, 200)
				})
			}
		})
	})
}

// seedReadyParityCorpus writes one row per ready-relevant distinction directly
// to the backing store, before any cache exists.
func seedReadyParityCorpus(t *testing.T, s beads.Store) {
	t.Helper()
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	pri := func(p int) *int { return &p }
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		if b.CreatedAt.IsZero() {
			b.CreatedAt = t0
		}
		created, err := s.Create(b)
		if err != nil {
			t.Fatalf("Create(%q): %v", b.Title, err)
		}
		return created
	}
	dep := func(issue beads.Bead, target, depType string) {
		t.Helper()
		if err := s.DepAdd(issue.ID, target, depType); err != nil {
			t.Fatalf("DepAdd(%s -> %s, %s): %v", issue.ID, target, depType, err)
		}
	}

	// Order: priority first, created_at next, id last; nil priority sorts as 2.
	mk(beads.Bead{Title: "p0 late", Priority: pri(0), CreatedAt: t1})
	mk(beads.Bead{Title: "p1 early", Priority: pri(1)})
	mk(beads.Bead{Title: "nil priority tie"})
	mk(beads.Bead{Title: "p2 tie", Priority: pri(2)})
	mk(beads.Bead{Title: "p2 tie again", Priority: pri(2)})
	mk(beads.Bead{Title: "p3", Priority: pri(3), CreatedAt: t1})

	openBlocker := mk(beads.Bead{Title: "open blocker", Priority: pri(4)})
	dep(mk(beads.Bead{Title: "blocked by open"}), openBlocker.ID, "blocks")
	dep(mk(beads.Bead{Title: "waits for open"}), openBlocker.ID, "waits-for")
	dep(mk(beads.Bead{Title: "conditionally blocked by open"}), openBlocker.ID, "conditional-blocks")
	dep(mk(beads.Bead{Title: "child of open parent"}), openBlocker.ID, "parent-child")

	// A store may drop the edges onto a deleted row, so the edge onto a
	// missing blocker is added after the delete.
	gone := mk(beads.Bead{Title: "deleted blocker"})
	if err := s.Delete(gone.ID); err != nil {
		t.Fatalf("Delete(%s): %v", gone.ID, err)
	}
	dep(mk(beads.Bead{Title: "blocked by missing"}), gone.ID, "blocks")
	dep(mk(beads.Bead{Title: "blocked by foreign"}), "zzforeign-1", "blocks")

	closedBlocker := mk(beads.Bead{Title: "closed blocker"})
	dep(mk(beads.Bead{Title: "released by close"}), closedBlocker.ID, "blocks")
	if err := s.Close(closedBlocker.ID); err != nil {
		t.Fatalf("Close(%s): %v", closedBlocker.ID, err)
	}
	failedBlocker := mk(beads.Bead{Title: "closed blocked-outcome blocker"})
	dep(mk(beads.Bead{Title: "blocked by blocked outcome"}), failedBlocker.ID, "blocks")
	if _, err := s.CloseAll([]string{failedBlocker.ID}, map[string]string{
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	}); err != nil {
		t.Fatalf("CloseAll(%s): %v", failedBlocker.ID, err)
	}

	future := readyParityFarFuture
	mk(beads.Bead{Title: "deferred", DeferUntil: &future})
	mk(beads.Bead{Title: "session-labeled", Labels: []string{"gc:session"}})
	mk(beads.Bead{Title: "message", Type: "message"})
	mk(beads.Bead{Title: "bug", Type: "bug"})
	mk(beads.Bead{Title: "in progress", Status: "in_progress"})
	mk(beads.Bead{Title: "assigned", Assignee: "worker"})
	mk(beads.Bead{Title: "ephemeral", Ephemeral: true})
	mk(beads.Bead{Title: "no history", NoHistory: true, Priority: pri(1), CreatedAt: t1})
}

// runReadyParityRandomized applies steps seeded mutations, alternating at
// random between writes through the cache and writes behind it, and compares
// after a re-scan at every checkpoint.
func runReadyParityRandomized(t *testing.T, h ReadyParityHarness, seed uint64, steps int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	backing := h.Open(t)
	seedReadyParityCorpus(t, backing)
	cache := primeReadyParityCache(t, backing)

	instants := []time.Time{
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		time.Date(2026, 1, 2, 3, 5, 5, 0, time.UTC),
		time.Date(2026, 1, 2, 3, 6, 5, 0, time.UTC),
	}
	priorities := []*int{nil, intPtr(0), intPtr(1), intPtr(2), intPtr(3)}
	types := []string{"task", "task", "task", "bug", "message"}
	depTypes := []string{"blocks", "blocks", "waits-for", "conditional-blocks", "parent-child"}
	var deleted []string

	for step := 1; step <= steps; step++ {
		w := backing
		where := "behind the cache"
		if rng.IntN(2) == 0 {
			w, where = cache, "through the cache"
		}
		all, err := backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			t.Fatalf("step %d: List: %v", step, err)
		}
		var open, closed []beads.Bead
		for _, b := range all {
			if b.Status == "closed" {
				closed = append(closed, b)
			} else {
				open = append(open, b)
			}
		}
		pick := func(from []beads.Bead) beads.Bead { return from[rng.IntN(len(from))] }
		fail := func(op string, err error) {
			t.Helper()
			t.Fatalf("seed %d step %d (%s): %s: %v", seed, step, where, op, err)
		}

		// One mutation per step; a no-op choice returns without skipping the
		// step's checkpoint.
		func() {
			switch op := rng.IntN(8); {
			case op == 0 || len(open) < 2:
				b := beads.Bead{
					Title:     fmt.Sprintf("random %d", step),
					Type:      types[rng.IntN(len(types))],
					Priority:  priorities[rng.IntN(len(priorities))],
					CreatedAt: instants[rng.IntN(len(instants))],
					Ephemeral: rng.IntN(8) == 0,
				}
				if rng.IntN(5) == 0 {
					b.Assignee = "worker"
				}
				if _, err := w.Create(b); err != nil {
					fail("Create", err)
				}
			case op <= 2:
				issue := pick(open)
				var target string
				switch r := rng.IntN(6); {
				case r == 0:
					target = fmt.Sprintf("zzforeign-%d", rng.IntN(3))
				case r == 1 && len(deleted) > 0:
					target = deleted[rng.IntN(len(deleted))]
				case r == 2 && len(closed) > 0:
					target = pick(closed).ID
				default:
					target = pick(open).ID
				}
				if target == issue.ID {
					return
				}
				if err := w.DepAdd(issue.ID, target, depTypes[rng.IntN(len(depTypes))]); err != nil {
					fail("DepAdd", err)
				}
			case op == 3:
				issue := pick(open)
				deps, err := backing.DepList(issue.ID, "down")
				if err != nil {
					fail("DepList", err)
				}
				if len(deps) == 0 {
					return
				}
				if err := w.DepRemove(issue.ID, deps[rng.IntN(len(deps))].DependsOnID); err != nil {
					fail("DepRemove", err)
				}
			case op == 4:
				var meta map[string]string
				if rng.IntN(2) == 0 {
					meta = map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}
				}
				if _, err := w.CloseAll([]string{pick(open).ID}, meta); err != nil {
					fail("CloseAll", err)
				}
			case op == 5 && len(closed) > 0:
				if err := w.Reopen(pick(closed).ID); err != nil {
					fail("Reopen", err)
				}
			case op == 6:
				p := rng.IntN(5)
				if err := w.Update(pick(open).ID, beads.UpdateOpts{Priority: &p}); err != nil {
					fail("Update", err)
				}
			default:
				victim := pick(open)
				if err := w.Delete(victim.ID); err != nil {
					fail("Delete", err)
				}
				deleted = append(deleted, victim.ID)
			}
		}()

		if step%25 == 0 {
			h.Rescan(cache)
			assertReadyParity(t, fmt.Sprintf("seed %d after step %d", seed, step), cache, backing)
		}
	}
}

func primeReadyParityCache(t *testing.T, backing beads.Store) *beads.CachingStore {
	t.Helper()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache
}

// assertReadyParity compares the cache's served ready projection with the
// backing store's own Ready for every query shape: same ids, same order.
func assertReadyParity(t *testing.T, when string, cache *beads.CachingStore, backing beads.Store) {
	t.Helper()
	want, err := backing.Ready()
	if err != nil {
		t.Fatalf("%s: backing Ready: %v", when, err)
	}
	got, err := cache.Ready()
	if err != nil {
		t.Fatalf("%s: cache Ready: %v", when, err)
	}
	compareReadyIDs(t, when, "Ready()", got, want)
	for _, q := range readyParityQueries {
		want, err := backing.Ready(q)
		if err != nil {
			t.Fatalf("%s: backing Ready(%+v): %v", when, q, err)
		}
		got, err := cache.ReadyContext(context.Background(), q)
		if err != nil {
			t.Fatalf("%s: cache ReadyContext(%+v) did not serve from the cache: %v", when, q, err)
		}
		compareReadyIDs(t, when, fmt.Sprintf("Ready(%+v)", q), got, want)
	}
}

func compareReadyIDs(t *testing.T, when, shape string, got, want []beads.Bead) {
	t.Helper()
	ids := func(rows []beads.Bead) []string {
		out := make([]string, len(rows))
		for i, b := range rows {
			out[i] = b.ID + " " + b.Title
		}
		return out
	}
	if g, w := ids(got), ids(want); !slices.Equal(g, w) {
		t.Fatalf("%s: %s differs between the cache and the store\ncache: %q\nstore: %q", when, shape, g, w)
	}
}

func intPtr(v int) *int { return &v }
