package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
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
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("ReadyContext never checked its context")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadyContext error = %v, want context.Canceled", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("ReadyContext kept waiting for a read connection after cancellation")
	}
}

func TestSQLiteStoreSatisfiesContextReadyReader(t *testing.T) {
	store := newReadySQLiteStore(t)
	if _, ok := Store(store).(ContextReadyReader); !ok {
		t.Fatal("SQLiteStore does not satisfy ContextReadyReader; the beads adapter will veto context-ready reads")
	}
}
