package beads

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestSQLiteLiveOpenLeavesOutRowsDeferredIntoTheFuture pins a SQLite store's
// answer to a Live "open": its own raw open, as every backing gives it
// (ListQuery.Live). A SQLite store records no parking status, so a deferral is
// its defer_until alone, and a row deferred into the future is parked, not
// open, as bd's raw open leaves out bd's deferred status. A storage migration
// carries every bead bd parks into a class binding in that form, and the Live
// "open" readers (the seat-work gates, the controller's wake demand, the
// orphan and route sweeps) count each row the answer holds as live work. Every
// other read keeps the row: a non-Live "open", an "in_progress", and a read of
// every status but closed.
func TestSQLiteLiveOpenLeavesOutRowsDeferredIntoTheFuture(t *testing.T) {
	store := newReadySQLiteStore(t)
	future := time.Now().Add(24 * time.Hour).UTC()
	past := time.Now().Add(-24 * time.Hour).UTC()
	base := time.Unix(1_700_000_000, 0).UTC()
	mk := func(i int, title, status string, until *time.Time) string {
		t.Helper()
		created, err := store.Create(Bead{Title: title, Type: "task", Status: status, DeferUntil: until, CreatedAt: base.Add(time.Duration(i) * time.Second)})
		if err != nil {
			t.Fatalf("Create(%q): %v", title, err)
		}
		return created.ID
	}
	// Created first, so a LIMIT pushed into SQL ahead of the deferral filter
	// would take it alone and answer nothing.
	deferred := mk(0, "deferred into the future", "open", &future)
	open := mk(1, "open", "open", nil)
	due := mk(2, "deferral already due", "open", &past)
	claimed := mk(3, "claimed while deferred", "in_progress", &future)

	for _, tc := range []struct {
		name  string
		query ListQuery
		want  []string
	}{
		{"live open", ListQuery{Status: "open", Live: true, AllowScan: true, Sort: SortCreatedAsc}, []string{open, due}},
		{"bounded live open", ListQuery{Status: "open", Live: true, AllowScan: true, Sort: SortCreatedAsc, Limit: 1}, []string{open}},
		{"cached open", ListQuery{Status: "open", AllowScan: true, Sort: SortCreatedAsc}, []string{deferred, open, due}},
		{"live in progress", ListQuery{Status: "in_progress", Live: true, AllowScan: true, Sort: SortCreatedAsc}, []string{claimed}},
		{"live every status but closed", ListQuery{Live: true, AllowScan: true, Sort: SortCreatedAsc}, []string{deferred, open, due, claimed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := store.List(tc.query)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if got := readyBeadIDs(rows); !slices.Equal(got, tc.want) {
				t.Fatalf("List = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCachingStoreOverSQLiteLiveOpenSkipsDeferredRows: a CachingStore over a
// SQLite store, as the controller holds a class binding, must not pay for the
// rows a Live "open" leaves out by design. The cache holds a deferred row as
// "open", so its stale-row sweep would take it for a row another process
// changed and read it back on every call, as it did for a native store's
// parked rows (TestCachingStoreOverNativeLiveOpenSkipsParkedRows). A row that
// left the page for another reason is still read back.
func TestCachingStoreOverSQLiteLiveOpenSkipsDeferredRows(t *testing.T) {
	sqlite := newReadySQLiteStore(t)
	future := time.Now().Add(24 * time.Hour).UTC()
	open, err := sqlite.Create(Bead{Title: "open", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	// One deferred row: a stale read of one id is a Get (PrefetchExact batches
	// only two or more), so the counter sees it.
	deferred, err := sqlite.Create(Bead{Title: "deferred", Type: "task", DeferUntil: &future})
	if err != nil {
		t.Fatal(err)
	}
	backing := &overlayCountingStore{Store: sqlite}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	live := func(t *testing.T) []string {
		t.Helper()
		items, err := cache.List(ListQuery{Status: "open", TierMode: TierBoth, Live: true, AllowScan: true})
		if err != nil {
			t.Fatalf("Live List(open): %v", err)
		}
		return sortedBeadIDs(items)
	}
	before, _, _ := backing.counts()
	for call := 1; call <= 3; call++ {
		if got := live(t); !slices.Equal(got, []string{open.ID}) {
			t.Fatalf("call %d: Live List(open) = %v, want only %s", call, got, open.ID)
		}
	}
	if gets, _, _ := backing.counts(); gets != before {
		t.Fatalf("%d backing Gets over 3 calls, want 0: the sweep re-read the deferred row", gets-before)
	}
	cached, err := cache.List(ListQuery{Status: "open", TierMode: TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedBeadIDs(cached), sortedBeadIDs([]Bead{open, deferred}); !slices.Equal(got, want) {
		t.Fatalf("cached List(open) = %v, want gc's open %v", got, want)
	}

	// Control: a row another process closed leaves the page and is read back.
	if err := sqlite.Close(open.ID); err != nil {
		t.Fatal(err)
	}
	if got := live(t); len(got) != 0 {
		t.Fatalf("Live List(open) = %v after the close, want none", got)
	}
	if gets, _, _ := backing.counts(); gets != before+1 {
		t.Fatalf("%d backing Gets, want 1 for the row another process closed", gets-before)
	}
}

// TestSQLiteClaimRefusesAnOpenRowDeferredIntoTheFuture: a claim takes only a
// row in the store's raw open, as bd claims only a bead in its raw open. An
// open row deferred into the future is the row a Live "open" leaves out, so a
// claim of it is a conflict that changes nothing, whoever it is assigned to.
// Taken, it would be an in_progress row every hook read drops as deferred
// while the controller counts it as its claimant's work. A deferral already
// due claims as an open row does, and a claimant's reclaim of a row it holds
// in_progress stays a no-op success.
func TestSQLiteClaimRefusesAnOpenRowDeferredIntoTheFuture(t *testing.T) {
	store := newReadySQLiteStore(t)
	future := time.Now().Add(24 * time.Hour).UTC()
	past := time.Now().Add(-24 * time.Hour).UTC()
	mk := func(title, status, assignee string, until *time.Time) Bead {
		t.Helper()
		created, err := store.Create(Bead{Title: title, Type: "task", Status: status, Assignee: assignee, DeferUntil: until})
		if err != nil {
			t.Fatalf("Create(%q): %v", title, err)
		}
		stored, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get(%q): %v", title, err)
		}
		return stored
	}
	for _, before := range []Bead{
		mk("deferred", "open", "", &future),
		mk("deferred, assigned to the claimant", "open", "worker", &future),
	} {
		t.Run(before.Title, func(t *testing.T) {
			got, ok, err := store.Claim(before.ID, "worker")
			if err != nil || ok {
				t.Fatalf("Claim = (%+v, %v, %v), want a conflict", got, ok, err)
			}
			after, err := store.Get(before.ID)
			if err != nil {
				t.Fatalf("Get after Claim: %v", err)
			}
			if after.Status != "open" || after.Assignee != before.Assignee || after.Revision != before.Revision || after.ClaimFence != before.ClaimFence {
				t.Fatalf("the refused Claim wrote the row: before=%+v after=%+v", before, after)
			}
			if after.DeferUntil == nil || !after.DeferUntil.Equal(*before.DeferUntil) {
				t.Fatalf("the refused Claim moved the deferral: before=%v after=%v", before.DeferUntil, after.DeferUntil)
			}
		})
	}

	due := mk("deferral already due", "open", "", &past)
	if got, ok, err := store.Claim(due.ID, "worker"); err != nil || !ok || got.Status != "in_progress" || got.Assignee != "worker" {
		t.Fatalf("Claim of a due deferral = (%+v, %v, %v), want it claimed for worker", got, ok, err)
	}

	held := mk("held while deferred", "in_progress", "worker", &future)
	got, ok, err := store.Claim(held.ID, "worker")
	if err != nil || !ok || got.Revision != held.Revision || got.ClaimFence != held.ClaimFence {
		t.Fatalf("the holder's reclaim = (%+v, %v, %v), want a no-op success at revision %d", got, ok, err, held.Revision)
	}
}
