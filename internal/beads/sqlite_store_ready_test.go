package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func newReadySQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// seedReadyCorpus populates a store with beads that exercise every ReadyQuery
// filter: assignee, tier, dependency gating, and non-actionable types.
func seedReadyCorpus(t *testing.T, store *SQLiteStore) {
	t.Helper()
	mk := func(b Bead) Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create(%q): %v", b.Title, err)
		}
		return created
	}
	for i := 0; i < 4; i++ {
		mk(Bead{Title: fmt.Sprintf("unassigned-%d", i), Type: "task", Status: "open"})
	}
	for i := 0; i < 3; i++ {
		mk(Bead{Title: fmt.Sprintf("alice-%d", i), Type: "task", Status: "open", Assignee: "alice"})
	}
	mk(Bead{Title: "bob", Type: "task", Status: "open", Assignee: "bob"})
	mk(Bead{Title: "not actionable", Type: "message", Status: "open"})
	mk(Bead{Title: "already closed", Type: "task", Status: "closed"})

	blocker := mk(Bead{Title: "blocker", Type: "task", Status: "open", Assignee: "alice"})
	blocked := mk(Bead{Title: "blocked", Type: "task", Status: "open", Assignee: "alice"})
	if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
}

func readyBeadIDs(rows []Bead) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

func sameReadyIDs(a, b []Bead) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func TestSQLiteStoreReadyContextMatchesReady(t *testing.T) {
	store := newReadySQLiteStore(t)
	seedReadyCorpus(t, store)

	cases := []struct {
		name  string
		query []ReadyQuery
	}{
		{name: "default", query: nil},
		{name: "empty query", query: []ReadyQuery{{}}},
		{name: "assignee alice", query: []ReadyQuery{{Assignee: "alice"}}},
		{name: "assignee bob", query: []ReadyQuery{{Assignee: "bob"}}},
		{name: "assignee nobody", query: []ReadyQuery{{Assignee: "nobody"}}},
		{name: "limit 2", query: []ReadyQuery{{Limit: 2}}},
		{name: "limit 1 assignee alice", query: []ReadyQuery{{Limit: 1, Assignee: "alice"}}},
		{name: "tier both", query: []ReadyQuery{{TierMode: TierBoth}}},
		{name: "tier wisps", query: []ReadyQuery{{TierMode: TierWisps}}},
		{name: "tier wisps limit 1", query: []ReadyQuery{{TierMode: TierWisps, Limit: 1}}},
	}

	baseline, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(baseline) < 3 {
		t.Fatalf("Ready returned %d beads, want a corpus large enough to distinguish filters", len(baseline))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := store.Ready(tc.query...)
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			got, err := store.ReadyContext(context.Background(), tc.query...)
			if err != nil {
				t.Fatalf("ReadyContext: %v", err)
			}
			if !sameReadyIDs(got, want) {
				t.Fatalf("ReadyContext = %v, want the same beads Ready returned: %v", readyBeadIDs(got), readyBeadIDs(want))
			}
		})
	}

	// A ReadyContext that drops its query arguments would pass every
	// comparison above only if every filter were a no-op. Prove at least one
	// filtered query is strictly narrower than the unfiltered read.
	narrow, err := store.ReadyContext(context.Background(), ReadyQuery{Assignee: "bob"})
	if err != nil {
		t.Fatalf("ReadyContext(assignee bob): %v", err)
	}
	if len(narrow) != 1 {
		t.Fatalf("ReadyContext(assignee bob) = %v, want exactly the one bead assigned to bob", readyBeadIDs(narrow))
	}
	if len(narrow) >= len(baseline) {
		t.Fatalf("filtered ReadyContext returned %d beads and the unfiltered read returned %d; the filter was ignored", len(narrow), len(baseline))
	}
	limited, err := store.ReadyContext(context.Background(), ReadyQuery{Limit: 2})
	if err != nil {
		t.Fatalf("ReadyContext(limit 2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("ReadyContext(limit 2) returned %d beads, want 2", len(limited))
	}
}

func TestSQLiteStoreReadyContextExcludesBlockedBeads(t *testing.T) {
	store := newReadySQLiteStore(t)
	blocker, err := store.Create(Bead{Title: "blocker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	blocked, err := store.Create(Bead{Title: "blocked", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create blocked: %v", err)
	}
	if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	rows, err := store.ReadyContext(context.Background())
	if err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
	if ids := readyBeadIDs(rows); len(ids) != 1 || ids[0] != blocker.ID {
		t.Fatalf("ReadyContext = %v, want only the unblocked bead %q", ids, blocker.ID)
	}

	if err := store.Close(blocker.ID); err != nil {
		t.Fatalf("Close blocker: %v", err)
	}
	rows, err = store.ReadyContext(context.Background())
	if err != nil {
		t.Fatalf("ReadyContext after the blocker closed: %v", err)
	}
	if ids := readyBeadIDs(rows); len(ids) != 1 || ids[0] != blocked.ID {
		t.Fatalf("ReadyContext = %v, want the released bead %q", ids, blocked.ID)
	}
}

func TestSQLiteStoreReadyContextCanceledContext(t *testing.T) {
	store := newReadySQLiteStore(t)
	seedReadyCorpus(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err := store.ReadyContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadyContext error = %v, want context.Canceled", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ReadyContext returned %d beads on a canceled context, want none", len(rows))
	}
}

func TestSQLiteStoreReadyContextExpiredDeadline(t *testing.T) {
	store := newReadySQLiteStore(t)
	seedReadyCorpus(t, store)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	rows, err := store.ReadyContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadyContext error = %v, want context.DeadlineExceeded", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ReadyContext returned %d beads on an expired deadline, want none", len(rows))
	}
}

// TestSQLiteStoreReadyContextStopsMidScan cancels once the read is already in
// flight, which the entry-only ctx.Err() check cannot catch.
func TestSQLiteStoreReadyContextStopsMidScan(t *testing.T) {
	store := newReadySQLiteStore(t)
	for i := 0; i < 64; i++ {
		if _, err := store.Create(Bead{Title: fmt.Sprintf("ready-%02d", i), Type: "task", Status: "open"}); err != nil {
			t.Fatalf("Create bead %d: %v", i, err)
		}
	}

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnErrCheckContext{Context: base, cancel: cancel, cancelAt: 4}

	rows, err := store.ReadyContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadyContext error = %v, want context.Canceled (rows = %d)", err, len(rows))
	}
	if len(rows) != 0 {
		t.Fatalf("ReadyContext returned %d beads after cancellation, want none", len(rows))
	}
	if checks := ctx.checks.Load(); checks < ctx.cancelAt {
		t.Fatalf("context checks = %d, want at least %d; the scan never consulted the context", checks, ctx.cancelAt)
	}
}

func TestSQLiteStoreReadyContextBackgroundSkipsErrChecks(t *testing.T) {
	store := newReadySQLiteStore(t)
	seedReadyCorpus(t, store)

	ctx := &countingErrContext{Context: context.Background()}
	rows, err := store.ReadyContext(ctx)
	if err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("ReadyContext returned no beads for an uncancellable context")
	}
	// One check is the entry guard; the per-row decode loop must skip a
	// context that can never be canceled.
	if checks := ctx.checks.Load(); checks > 1 {
		t.Fatalf("uncancellable context checks = %d, want at most the single entry guard", checks)
	}
}

// TestSQLiteStoreReadyContextCancelsWhileWaitingForAConnection saturates the
// read pool so the ready query cannot start. Only a context that actually
// reaches the database/sql call can abandon that wait; a ReadyContext that
// checks its context once at entry and then issues a context-blind query
// blocks here until the pool frees up.
func TestSQLiteStoreReadyContextCancelsWhileWaitingForAConnection(t *testing.T) {
	store := newReadySQLiteStore(t)
	seedReadyCorpus(t, store)

	store.readDB.SetMaxOpenConns(1)
	held, err := store.readDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer held.Close() //nolint:errcheck

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &observedErrContext{Context: base, checked: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := store.ReadyContext(ctx)
		done <- err
	}()

	select {
	case <-ctx.checked: // the entry guard ran and saw a live context
	case <-time.After(beadsHangBudget):
		t.Fatal("ReadyContext never checked its context")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadyContext error = %v, want context.Canceled", err)
		}
	case <-time.After(beadsHangBudget):
		t.Fatal("ReadyContext kept waiting for a read connection after cancellation")
	}
}

func TestSQLiteStoreSatisfiesContextReadyReader(t *testing.T) {
	store := newReadySQLiteStore(t)
	if _, ok := Store(store).(ContextReadyReader); !ok {
		t.Fatal("SQLiteStore does not satisfy ContextReadyReader; the beads adapter will veto context-ready reads")
	}
}

// TestSQLiteStoreReadyOrderIsCanonical pins SQLite's ready order to the
// canonical (priority, created_at, id) order a CachingStore serves (#3208):
// a nil priority sorts as 2, equal priority and created_at fall back to the
// id, and Limit cuts that order's prefix after the post-decode filters.
func TestSQLiteStoreReadyOrderIsCanonical(t *testing.T) {
	store := newReadySQLiteStore(t)
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	pri := func(p int) *int { return &p }
	future := t0.Add(1000 * 24 * time.Hour)
	for _, b := range []Bead{
		// Inserted so neither creation order nor id order is the answer.
		{ID: "gc-tie-z", Title: "p2 tie z", Priority: pri(2), CreatedAt: t0},
		{ID: "gc-late", Title: "p0 late", Priority: pri(0), CreatedAt: t0.Add(time.Hour)},
		{ID: "gc-tie-m", Title: "nil tie m", CreatedAt: t0},
		{ID: "gc-early", Title: "p2 early", Priority: pri(2), CreatedAt: t0.Add(-time.Hour)},
		{ID: "gc-p1", Title: "p1", Priority: pri(1), CreatedAt: t0.Add(2 * time.Hour)},
		{ID: "gc-tie-a", Title: "p2 tie a", Priority: pri(2), CreatedAt: t0},
		{ID: "gc-p3", Title: "p3", Priority: pri(3), CreatedAt: t0.Add(-2 * time.Hour)},
		// Survives SQL but not the post-decode filter; a source-side LIMIT
		// would spend a slot on it.
		{ID: "gc-deferred", Title: "p0 deferred", Priority: pri(0), CreatedAt: t0.Add(-3 * time.Hour), DeferUntil: &future},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatalf("Create(%s): %v", b.ID, err)
		}
	}
	want := []string{"gc-late", "gc-p1", "gc-early", "gc-tie-a", "gc-tie-m", "gc-tie-z", "gc-p3"}

	for _, tc := range []struct {
		name  string
		query ReadyQuery
		want  []string
	}{
		{name: "default", want: want},
		{name: "tier both", query: ReadyQuery{TierMode: TierBoth}, want: want},
		{name: "tier wisps", query: ReadyQuery{TierMode: TierWisps}, want: nil},
		{name: "limit 1", query: ReadyQuery{Limit: 1}, want: want[:1]},
		{name: "limit 4", query: ReadyQuery{Limit: 4}, want: want[:4]},
		{name: "tier both limit 5", query: ReadyQuery{TierMode: TierBoth, Limit: 5}, want: want[:5]},
		{name: "limit past the end", query: ReadyQuery{Limit: 50}, want: want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := store.Ready(tc.query)
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			if got := readyBeadIDs(rows); !slices.Equal(got, tc.want) {
				t.Fatalf("Ready(%+v) = %v, want %v", tc.query, got, tc.want)
			}
			rows, err = store.ReadyContext(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("ReadyContext: %v", err)
			}
			if got := readyBeadIDs(rows); !slices.Equal(got, tc.want) {
				t.Fatalf("ReadyContext(%+v) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestSQLiteEnrichReadyProjectionEqualsReadySQL proves the is_blocked column
// the store hands a CachingStore is exactly the blocking predicate its own
// Ready negates: every non-closed row gets a verdict, and a ready candidate is
// in Ready iff its verdict is false. The corpus spans more than one projection
// batch and every blocker shape the two readers could disagree on.
func TestSQLiteEnrichReadyProjectionEqualsReadySQL(t *testing.T) {
	store := newReadySQLiteStore(t)
	mk := func(b Bead) Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create(%q): %v", b.Title, err)
		}
		return created
	}
	dep := func(issue Bead, target, depType string) {
		t.Helper()
		if err := store.DepAdd(issue.ID, target, depType); err != nil {
			t.Fatalf("DepAdd(%s -> %s): %v", issue.ID, target, err)
		}
	}
	for i := 0; i < sqliteReadyProjectionBatch; i++ {
		mk(Bead{Title: fmt.Sprintf("filler-%03d", i)})
	}

	openBlocker := mk(Bead{Title: "open blocker"})
	// Delete drops the edges onto a row, so the edge onto a missing blocker is
	// added after the delete.
	gone := mk(Bead{Title: "deleted blocker"})
	if err := store.Delete(gone.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	closedBlocker := mk(Bead{Title: "closed blocker"})
	failedBlocker := mk(Bead{Title: "closed blocked-outcome blocker"})
	wantBlocked := map[string]bool{}
	blockedBy := func(title, target, depType string, blocked bool) {
		t.Helper()
		b := mk(Bead{Title: title})
		dep(b, target, depType)
		wantBlocked[b.ID] = blocked
	}
	blockedBy("blocked by open", openBlocker.ID, "blocks", true)
	blockedBy("waits for open", openBlocker.ID, "waits-for", true)
	blockedBy("conditionally blocked by open", openBlocker.ID, "conditional-blocks", true)
	blockedBy("child of open parent", openBlocker.ID, "parent-child", false)
	blockedBy("blocked by missing", gone.ID, "blocks", true)
	blockedBy("blocked by foreign", "zzforeign-1", "blocks", true)
	blockedBy("released by close", closedBlocker.ID, "blocks", false)
	blockedBy("blocked by blocked outcome", failedBlocker.ID, "blocks", true)
	inProgress := mk(Bead{Title: "in progress, blocked by foreign", Status: "in_progress"})
	dep(inProgress, "zzforeign-2", "blocks")
	wantBlocked[inProgress.ID] = true

	if err := store.Close(closedBlocker.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.CloseAll([]string{failedBlocker.ID}, map[string]string{
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	}); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}

	rows, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	enriched, err := store.enrichReadyProjectionForCache(rows)
	if err != nil {
		t.Fatalf("enrichReadyProjectionForCache: %v", err)
	}
	ready, err := store.Ready(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	inReady := make(map[string]bool, len(ready))
	for _, b := range ready {
		inReady[b.ID] = true
	}

	now := time.Now()
	for _, b := range enriched {
		if b.IsBlocked == nil {
			t.Fatalf("row %s (%q) got no verdict", b.ID, b.Title)
		}
		if want, pinned := wantBlocked[b.ID]; pinned && *b.IsBlocked != want {
			t.Errorf("row %s (%q): IsBlocked = %v, want %v", b.ID, b.Title, *b.IsBlocked, want)
		}
		if IsReadyCandidateForTier(b, now, TierBoth) && inReady[b.ID] == *b.IsBlocked {
			t.Errorf("row %s (%q): IsBlocked = %v but in Ready = %v", b.ID, b.Title, *b.IsBlocked, inReady[b.ID])
		}
	}
	if len(enriched) != len(rows) || len(enriched) <= sqliteReadyProjectionBatch {
		t.Fatalf("enriched %d of %d rows; want every row, across more than one batch of %d",
			len(enriched), len(rows), sqliteReadyProjectionBatch)
	}
}
