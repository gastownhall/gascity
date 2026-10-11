package beads

import (
	"fmt"
	"testing"
	"time"
)

func seekQuery(sort SortOrder, at time.Time, id string) ListQuery {
	return ListQuery{
		AllowScan: true,
		Sort:      sort,
		SeekAfter: &SeekBoundary{CreatedAt: at, ID: id},
	}
}

func TestSeekAfterDescKeepsStrictlyOlderRows(t *testing.T) {
	b := time.Date(2026, 7, 11, 12, 0, 0, 500000000, time.UTC)
	q := seekQuery(SortCreatedDesc, b, "gc-50")

	for _, tc := range []struct {
		name string
		bead Bead
		want bool
	}{
		{"older created_at", Bead{ID: "gc-99", Status: "open", CreatedAt: b.Add(-time.Second)}, true},
		{"newer created_at", Bead{ID: "gc-1", Status: "open", CreatedAt: b.Add(time.Second)}, false},
		{"boundary row itself", Bead{ID: "gc-50", Status: "open", CreatedAt: b}, false},
		{"tie, smaller id (after in DESC id order)", Bead{ID: "gc-49", Status: "open", CreatedAt: b}, true},
		{"tie, larger id (before boundary in DESC)", Bead{ID: "gc-51", Status: "open", CreatedAt: b}, false},
		{"sub-second newer", Bead{ID: "gc-2", Status: "open", CreatedAt: b.Add(time.Millisecond)}, false},
		{"sub-second older", Bead{ID: "gc-98", Status: "open", CreatedAt: b.Add(-time.Millisecond)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := q.Matches(tc.bead); got != tc.want {
				t.Fatalf("Matches(%s) = %v, want %v", tc.bead.ID, got, tc.want)
			}
		})
	}
}

func TestSeekAfterAscKeepsStrictlyNewerRows(t *testing.T) {
	b := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	q := seekQuery(SortCreatedAsc, b, "gc-50")

	for _, tc := range []struct {
		name string
		bead Bead
		want bool
	}{
		{"newer created_at", Bead{ID: "gc-1", Status: "open", CreatedAt: b.Add(time.Second)}, true},
		{"older created_at", Bead{ID: "gc-99", Status: "open", CreatedAt: b.Add(-time.Second)}, false},
		{"boundary row itself", Bead{ID: "gc-50", Status: "open", CreatedAt: b}, false},
		{"tie, larger id (after in ASC id order)", Bead{ID: "gc-51", Status: "open", CreatedAt: b}, true},
		{"tie, smaller id", Bead{ID: "gc-49", Status: "open", CreatedAt: b}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := q.Matches(tc.bead); got != tc.want {
				t.Fatalf("Matches(%s) = %v, want %v", tc.bead.ID, got, tc.want)
			}
		})
	}
}

func TestSeekAfterComposesWithOtherFilters(t *testing.T) {
	b := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	q := seekQuery(SortCreatedDesc, b, "gc-50")
	q.Type = "task"

	older := b.Add(-time.Minute)
	if q.Matches(Bead{ID: "gc-99", Status: "open", Type: "epic", CreatedAt: older}) {
		t.Fatal("type filter must still apply alongside the seek")
	}
	if !q.Matches(Bead{ID: "gc-99", Status: "open", Type: "task", CreatedAt: older}) {
		t.Fatal("matching type + after-boundary row must pass")
	}
}

func TestSeekAfterRequiresExplicitSort(t *testing.T) {
	q := seekQuery(SortDefault, time.Now(), "gc-1")
	if err := q.Validate(); err == nil {
		t.Fatal("SeekAfter with SortDefault must fail Validate — a seek without a total order is meaningless")
	}
	if err := seekQuery(SortCreatedDesc, time.Now(), "gc-1").Validate(); err != nil {
		t.Fatalf("SeekAfter with explicit sort should validate: %v", err)
	}
}

func TestSeekAfterCountsAsFilter(t *testing.T) {
	q := ListQuery{SeekAfter: &SeekBoundary{CreatedAt: time.Now(), ID: "gc-1"}, Sort: SortCreatedDesc}
	if !q.HasFilter() {
		t.Fatal("SeekAfter must count as a filter so seek-only queries are not rejected as scans")
	}
}

// TestSeekAfterWalkMemStoreNoSkipNoDup is the core correctness property: a
// keyset walk sees every pre-walk row exactly once even when new rows are
// inserted between pages (the scenario where offset cursors skip or
// duplicate).
func TestSeekAfterWalkMemStoreNoSkipNoDup(t *testing.T) {
	runSeekWalk(t, NewMemStore())
}

// TestSeekAfterWalkCachingStoreNoSkipNoDup runs the same property through a
// CachingStore (the production read path for open beads): the cache scan must
// apply the boundary via Matches before its sort+truncate.
func TestSeekAfterWalkCachingStoreNoSkipNoDup(t *testing.T) {
	runSeekWalk(t, NewCachingStoreForTest(NewMemStore(), nil))
}

func runSeekWalk(t *testing.T, store Store) {
	t.Helper()
	var preWalk []string
	for i := 0; i < 25; i++ {
		b, err := store.Create(Bead{Title: "t", Status: "open"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		preWalk = append(preWalk, b.ID)
	}

	seen := map[string]int{}
	var boundary *SeekBoundary
	pages := 0
	for {
		q := ListQuery{AllowScan: true, Sort: SortCreatedDesc, Limit: 7, SeekAfter: boundary}
		rows, err := store.List(q)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seen[r.ID]++
		}
		last := rows[len(rows)-1]
		boundary = &SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		pages++
		if pages > 20 {
			t.Fatal("walk did not terminate")
		}
		// Insert new rows mid-walk: with created-DESC order they sort before
		// the boundary and must NOT appear in subsequent pages, and must not
		// displace any pre-walk row.
		if _, err := store.Create(Bead{Title: "mid-walk", Status: "open"}); err != nil {
			t.Fatalf("mid-walk create: %v", err)
		}
	}

	for _, id := range preWalk {
		if seen[id] != 1 {
			t.Errorf("pre-walk bead %s seen %d times, want exactly 1", id, seen[id])
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("bead %s duplicated across pages (%d times)", id, n)
		}
	}
}

// TestSeekGatesForceClientSideLimit pins the store-safety gates: every
// backend whose native query layer cannot express the compound
// (created_at, id) boundary must fetch unbounded and filter Go-side.
// Applying a native limit first silently drops page rows.
func TestSeekGatesForceClientSideLimit(t *testing.T) {
	seek := &SeekBoundary{CreatedAt: time.Now(), ID: "gc-1"}

	base := ListQuery{Sort: SortCreatedDesc, Limit: 10, TierMode: TierBoth}
	if bdListRequiresClientLimit(base, base, false) {
		t.Fatal("baseline TierBoth query should allow bd-side limit (test setup wrong)")
	}
	seeked := base
	seeked.SeekAfter = seek
	if !bdListRequiresClientLimit(seeked, seeked, false) {
		t.Fatal("bdListRequiresClientLimit must force client-side limit when SeekAfter is set")
	}

	wisps := ListQuery{Sort: SortCreatedDesc, Limit: 10}
	if !canApplyWispsServerLimit(wisps) {
		t.Fatal("baseline wisps query should allow the server limit (test setup wrong)")
	}
	wisps.SeekAfter = seek
	if canApplyWispsServerLimit(wisps) {
		t.Fatal("canApplyWispsServerLimit must reject seeked queries — bd query cannot express the boundary")
	}
}

// TestSeekAfterWalkWithTiedCreatedAt: whole-second created_at ties are the
// production norm on bd/doltlite (timestamps truncate to seconds). The id
// tie-break must keep the walk exact when a page cuts mid-tie.
func TestSeekAfterWalkWithTiedCreatedAt(t *testing.T) {
	ts := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	var seeded []Bead
	for i := 0; i < 17; i++ {
		// Three distinct seconds, heavy ties inside each.
		seeded = append(seeded, Bead{
			ID:        fmt.Sprintf("gc-%02d", i),
			Title:     "t",
			Status:    "open",
			CreatedAt: ts.Add(time.Duration(i%3) * time.Second),
		})
	}
	store := NewMemStoreFrom(100, seeded, nil)

	seen := map[string]int{}
	var boundary *SeekBoundary
	pages := 0
	for {
		rows, err := store.List(ListQuery{AllowScan: true, Sort: SortCreatedDesc, Limit: 4, SeekAfter: boundary})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seen[r.ID]++
		}
		last := rows[len(rows)-1]
		boundary = &SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		if pages++; pages > 10 {
			t.Fatal("walk did not terminate")
		}
	}
	if len(seen) != len(seeded) {
		t.Fatalf("walk saw %d distinct rows, want %d", len(seen), len(seeded))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s seen %d times, want 1 (tie-break skip/dup)", id, n)
		}
	}
}
