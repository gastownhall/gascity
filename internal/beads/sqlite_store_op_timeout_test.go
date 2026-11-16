package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// sqliteTestOpBound is the injected operation bound the real-I/O tests use:
// long enough that the statements the tests expect to succeed finish well
// inside it, short enough that a test waiting for expiry stays quick.
const sqliteTestOpBound = 100 * time.Millisecond

// openSQLiteStoreWithOpTimeout opens a store in a fresh directory whose
// operation bound is d.
func openSQLiteStoreWithOpTimeout(t *testing.T, d time.Duration) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreOpTimeout(d))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// holdSQLiteWriterConn takes the store's only writer connection with a
// test-held transaction and returns the function that gives it back. An
// operation that needs the writer then waits in database/sql's pool, a
// Go-level wait that does not depend on SQLite's busy handler.
func holdSQLiteWriterConn(t *testing.T, store *SQLiteStore) (release func()) {
	t.Helper()
	held, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("holding the writer connection: %v", err)
	}
	release = sync.OnceFunc(func() { _ = held.Rollback() })
	t.Cleanup(release)
	return release
}

// holdSQLiteReaderConns takes every connection of the read pool and returns
// the function that gives them back.
func holdSQLiteReaderConns(t *testing.T, store *SQLiteStore) (release func()) {
	t.Helper()
	var conns []interface{ Close() error }
	for i := 0; i < sqliteReadPoolSize; i++ {
		conn, err := store.readDB.Conn(context.Background())
		if err != nil {
			t.Fatalf("holding read connection %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	release = sync.OnceFunc(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	t.Cleanup(release)
	return release
}

// dumpSQLiteRows renders every row of the beads and deps tables, so a test can
// prove an operation that gave up wrote nothing by comparing two dumps.
func dumpSQLiteRows(t *testing.T, store *SQLiteStore) string {
	t.Helper()
	var out strings.Builder
	for _, q := range []string{`SELECT * FROM beads ORDER BY id`, `SELECT * FROM deps ORDER BY issue_id, depends_on_id`} {
		rows, err := store.readDB.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns of %s: %v", q, err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scanning %s: %v", q, err)
			}
			fmt.Fprintf(&out, "%v\n", vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("reading %s: %v", q, err)
		}
		_ = rows.Close()
		out.WriteString("--\n")
	}
	return out.String()
}

// requireStoreTimeout fails unless err carries both the store's own timeout
// sentinel and the context error it came from.
func requireStoreTimeout(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: err = nil, want ErrStoreTimeout wrapping context.DeadlineExceeded", what)
	}
	if !errors.Is(err, ErrStoreTimeout) {
		t.Errorf("%s: err = %v, want errors.Is(err, ErrStoreTimeout)", what, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%s: err = %v, want errors.Is(err, context.DeadlineExceeded)", what, err)
	}
}

// slowDeleteTrigger installs a trigger that makes deleting id run a statement
// far longer than any bound a test sets, so only an interrupt ends it: a long
// statement the writer-connection wait cannot model.
func slowDeleteTrigger(t *testing.T, store *SQLiteStore, id string) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE VIEW sqlite_test_slow AS
			WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 4000000000)
			SELECT count(*) AS n FROM c`,
		`CREATE TRIGGER sqlite_test_slow_delete AFTER DELETE ON beads WHEN old.id = '` + id + `'
			BEGIN SELECT n FROM sqlite_test_slow; END`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("installing the slow-delete trigger: %v", err)
		}
	}
}

// TestSQLiteStoreWritesGiveUpAtTheOperationBound is AT-S1.1: with the writer
// connection held, every write operation returns ErrStoreTimeout wrapping
// context.DeadlineExceeded after the bound, and nothing was written.
func TestSQLiteStoreWritesGiveUpAtTheOperationBound(t *testing.T) {
	store := openSQLiteStoreWithOpTimeout(t, sqliteTestOpBound)
	for _, b := range []Bead{
		{ID: "gc-a", Title: "a"},
		{ID: "gc-b", Title: "b", Status: "in_progress", Assignee: "w1"},
		{ID: "gc-c", Title: "c"},
		{ID: "gc-d", Title: "d"},
	} {
		if _, err := store.CreateWithForeignID(b); err != nil {
			t.Fatalf("seeding %s: %v", b.ID, err)
		}
	}
	if err := store.DepAdd("gc-a", "gc-b", "blocks"); err != nil {
		t.Fatalf("seeding a dependency: %v", err)
	}
	a, err := store.Get("gc-a")
	if err != nil {
		t.Fatalf("Get gc-a: %v", err)
	}
	c, err := store.Get("gc-c")
	if err != nil {
		t.Fatalf("Get gc-c: %v", err)
	}
	before := dumpSQLiteRows(t, store)

	release := holdSQLiteWriterConn(t, store)
	ops := []struct {
		name string
		run  func() error
	}{
		{"Create", func() error { _, err := store.Create(Bead{Title: "new"}); return err }},
		{"CreateWithForeignIDBatch", func() error { return store.CreateWithForeignIDBatch([]Bead{{ID: "gc-n1", Title: "n1"}}) }},
		{"Update", func() error { return store.Update("gc-a", UpdateOpts{Title: ptrTo("renamed")}) }},
		{"Close", func() error { return store.Close("gc-a") }},
		{"ReleaseIfCurrent", func() error { _, err := store.ReleaseIfCurrent("gc-b", "w1"); return err }},
		{"Claim", func() error { _, _, err := store.Claim("gc-a", "w2"); return err }},
		{"CompareAndSetMetadataKey", func() error { _, err := store.CompareAndSetMetadataKey("gc-a", "k", "", "v"); return err }},
		{"UpdateIfMatch", func() error {
			return store.UpdateIfMatch("gc-a", a.Revision, UpdateOpts{Title: ptrTo("fenced")})
		}},
		{"CloseIfMatch", func() error { return store.CloseIfMatch("gc-a", a.Revision) }},
		{"Delete", func() error { return store.Delete("gc-d") }},
		{"DeleteIfMatch", func() error { return store.DeleteIfMatch("gc-c", c.Revision) }},
		{"DeleteBatch", func() error { return store.DeleteBatch([]string{"gc-d"}) }},
		{"DepAdd", func() error { return store.DepAdd("gc-c", "gc-d", "blocks") }},
		{"DepAddWithMetadata", func() error { return store.DepAddWithMetadata("gc-c", "gc-d", "blocks", `{"k":1}`) }},
		{"DepAddBatch", func() error {
			return store.DepAddBatch([]DepWithPayload{{Dep: Dep{IssueID: "gc-c", DependsOnID: "gc-d", Type: "blocks"}}})
		}},
		{"DepRemove", func() error { return store.DepRemove("gc-a", "gc-b") }},
		{"DepRemoveBatch", func() error { return store.DepRemoveBatch([]DepPair{{IssueID: "gc-a", DependsOnID: "gc-b"}}) }},
		{"ApplyGuarded", func() error {
			_, err := store.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
				{Create: &Bead{ID: "gc-g1", Title: "g1"}},
			}})
			return err
		}},
		{"ApplyGraphPlan", func() error {
			_, err := store.ApplyGraphPlan(context.Background(), &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "n", Title: "graph node"}}})
			return err
		}},
		{"Tx", func() error {
			return store.Tx("bounded", func(tx Tx) error {
				_, err := tx.Create(Bead{Title: "in tx"})
				return err
			})
		}},
	}
	for _, op := range ops {
		requireStoreTimeout(t, op.name, op.run())
	}
	release()
	if after := dumpSQLiteRows(t, store); after != before {
		t.Fatalf("an operation that gave up wrote rows:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestSQLiteStoreReadsGiveUpAtTheOperationBound is AT-S1.2 and the read half
// of AT-S1.10: with every read connection held, each read returns the wrapped
// error and a nil collection, never a partial one.
func TestSQLiteStoreReadsGiveUpAtTheOperationBound(t *testing.T) {
	store := openSQLiteStoreWithOpTimeout(t, sqliteTestOpBound)
	if _, err := store.CreateWithForeignID(Bead{ID: "gc-a", Title: "a"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	holdSQLiteReaderConns(t, store)

	_, err := store.Get("gc-a")
	requireStoreTimeout(t, "Get", err)

	list, err := store.List(ListQuery{AllowScan: true})
	requireStoreTimeout(t, "List", err)
	if list != nil {
		t.Errorf("List returned %d rows beside its error, want nil", len(list))
	}

	deps, err := store.DepList("gc-a", "down")
	requireStoreTimeout(t, "DepList", err)
	if deps != nil {
		t.Errorf("DepList returned %v beside its error, want a nil slice", deps)
	}

	batch, err := store.DepListBatch([]string{"gc-a"})
	requireStoreTimeout(t, "DepListBatch", err)
	if batch != nil {
		t.Errorf("DepListBatch returned %v beside its error, want nil", batch)
	}

	ready, err := store.Ready()
	requireStoreTimeout(t, "Ready", err)
	if ready != nil {
		t.Errorf("Ready returned %d rows beside its error, want nil", len(ready))
	}

	readyCtx, err := store.ReadyContext(context.Background())
	requireStoreTimeout(t, "ReadyContext", err)
	if readyCtx != nil {
		t.Errorf("ReadyContext returned %d rows beside its error, want nil", len(readyCtx))
	}
}

// TestSQLiteStoreListOverManyBeadsGivesUpWithNoBeads is the List half of
// AT-S1.10: a List over more than one hydration chunk (500 ids) that cannot
// finish returns the error and no beads, never a list some of whose beads
// lack their deps.
func TestSQLiteStoreListOverManyBeadsGivesUpWithNoBeads(t *testing.T) {
	store := openSQLiteStoreWithOpTimeout(t, sqliteTestOpBound)
	rows := make([]Bead, 0, 600)
	for i := 0; i < 600; i++ {
		rows = append(rows, Bead{ID: fmt.Sprintf("gc-m%04d", i), Title: "many"})
	}
	if err := store.CreateWithForeignIDBatch(rows); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if got, err := store.List(ListQuery{AllowScan: true}); err != nil || len(got) != 600 {
		t.Fatalf("List before the hold = %d rows, %v; want 600, nil", len(got), err)
	}
	holdSQLiteReaderConns(t, store)
	got, err := store.List(ListQuery{AllowScan: true})
	requireStoreTimeout(t, "List", err)
	if got != nil {
		t.Fatalf("List returned %d beads beside its error, want none", len(got))
	}
}

// TestSQLiteStoreCallerCancelPassesThroughUnmarked is AT-S1.3: a caller's own
// cancel or deadline is the caller's, never the store's timeout.
func TestSQLiteStoreCallerCancelPassesThroughUnmarked(t *testing.T) {
	store := openSQLiteStoreForTest(t, t.TempDir())
	plan := &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "n", Title: "graph node"}}}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ApplyGraphPlan(canceled, plan); !errors.Is(err, context.Canceled) || errors.Is(err, ErrStoreTimeout) {
		t.Errorf("ApplyGraphPlan(canceled ctx) = %v, want context.Canceled and not ErrStoreTimeout", err)
	}
	if _, err := store.ReadyContext(canceled); err != context.Canceled {
		t.Errorf("ReadyContext(canceled ctx) = %v, want context.Canceled unchanged", err)
	}

	// The caller's deadline fires while the operation waits for the writer, long
	// before the store's own (default) bound: the caller's error comes back.
	release := holdSQLiteWriterConn(t, store)
	defer release()
	short, stop := context.WithTimeout(context.Background(), sqliteTestOpBound)
	defer stop()
	_, err := store.ApplyGraphPlan(short, plan)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStoreTimeout) {
		t.Errorf("ApplyGraphPlan(caller deadline) = %v, want context.DeadlineExceeded and not ErrStoreTimeout", err)
	}
}

// TestSQLiteOpContextHelpers pins the two helpers that carry the bound: the op
// ctx treats a nil parent as Background and applies the store's bound, and the
// mapping keys on ctx state, checking the op ctx before the parent.
func TestSQLiteOpContextHelpers(t *testing.T) {
	t.Run("nil parent is background", func(t *testing.T) {
		store := &SQLiteStore{opTimeout: time.Hour}
		var nilParent context.Context
		ctx, cancel := store.opContext(nilParent)
		defer cancel()
		if ctx == nil {
			t.Fatal("opContext(nil) returned a nil context")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("opContext(nil) has no deadline, want the store's bound")
		}
		cancel()
		if ctx.Err() == nil {
			t.Fatal("cancel did not cancel the op context")
		}
	})

	t.Run("a parent deadline sooner than the bound wins", func(t *testing.T) {
		store := &SQLiteStore{opTimeout: time.Hour}
		parent, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		ctx, cancel := store.opContext(parent)
		defer cancel()
		got, _ := ctx.Deadline()
		want, _ := parent.Deadline()
		if !got.Equal(want) {
			t.Errorf("op deadline = %v, want the parent's %v", got, want)
		}
	})

	busy := errors.New("original failure")
	live := context.Background()
	t.Run("mapStoreTimeout", func(t *testing.T) {
		expired := func() (context.Context, context.Context) {
			opCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			t.Cleanup(cancel)
			<-opCtx.Done()
			return opCtx, live
		}
		canceledParent := func() (context.Context, context.Context) {
			parent, cancel := context.WithCancel(context.Background())
			opCtx, stop := context.WithTimeout(parent, time.Hour)
			t.Cleanup(stop)
			cancel()
			return opCtx, parent
		}
		deadlinedParent := func() (context.Context, context.Context) {
			parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			t.Cleanup(cancel)
			opCtx, stop := context.WithTimeout(parent, time.Hour)
			t.Cleanup(stop)
			<-opCtx.Done()
			return opCtx, parent
		}

		if got := mapStoreTimeout(live, live, nil); got != nil {
			t.Errorf("nil error mapped to %v", got)
		}
		if got := mapStoreTimeout(live, live, busy); got != busy {
			t.Errorf("live op ctx mapped %v to %v, want it unchanged", busy, got)
		}
		opCtx, parent := expired()
		if got := mapStoreTimeout(opCtx, parent, nil); got != nil {
			t.Errorf("nil error under an expired op ctx mapped to %v", got)
		}
		got := mapStoreTimeout(opCtx, parent, busy)
		requireStoreTimeout(t, "expired op ctx", got)
		if !errors.Is(got, busy) {
			t.Errorf("mapped error %v lost the original %v", got, busy)
		}
		opCtx, parent = canceledParent()
		if got := mapStoreTimeout(opCtx, parent, busy); got != busy {
			t.Errorf("caller cancel mapped %v to %v, want it unchanged (the op ctx is done too, but the parent ended it)", busy, got)
		}
		opCtx, parent = deadlinedParent()
		if got := mapStoreTimeout(opCtx, parent, busy); got != busy || errors.Is(got, ErrStoreTimeout) {
			t.Errorf("caller deadline mapped %v to %v, want it unchanged and never the sentinel", busy, got)
		}
	})
}

// TestSQLiteStoreOpTimeoutDefaults is AT-S1.11: the bound defaults to 60 s, a
// non-positive option keeps the default, and a bare struct literal gets it.
func TestSQLiteStoreOpTimeoutDefaults(t *testing.T) {
	if sqliteDefaultOpTimeout != 60*time.Second {
		t.Fatalf("sqliteDefaultOpTimeout = %v, want 60s", sqliteDefaultOpTimeout)
	}
	// The retry ladder plus busy_timeout is about 40 s; the bound must sit above
	// it so ordinary contention still ends as ErrSQLiteBusyExhausted.
	if sqliteDefaultOpTimeout <= 40*time.Second {
		t.Errorf("sqliteDefaultOpTimeout %v is not above the retry ladder (about 40s)", sqliteDefaultOpTimeout)
	}
	var o SQLiteStoreOptions
	WithSQLiteStoreOpTimeout(5 * time.Second)(&o)
	if o.opTimeout != 5*time.Second {
		t.Fatalf("WithSQLiteStoreOpTimeout(5s) set %v", o.opTimeout)
	}
	for _, d := range []time.Duration{0, -time.Second} {
		var o SQLiteStoreOptions
		WithSQLiteStoreOpTimeout(d)(&o)
		if o.opTimeout != 0 {
			t.Errorf("WithSQLiteStoreOpTimeout(%v) set %v, want the default left in place", d, o.opTimeout)
		}
	}

	withinDefault := func(what string, s *SQLiteStore) {
		t.Helper()
		ctx, cancel := s.opContext(context.Background())
		defer cancel()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("%s: op ctx has no deadline", what)
		}
		if left := time.Until(deadline); left < sqliteDefaultOpTimeout-5*time.Second || left > sqliteDefaultOpTimeout {
			t.Errorf("%s: op ctx expires in %v, want about %v", what, left, sqliteDefaultOpTimeout)
		}
	}
	withinDefault("bare literal", &SQLiteStore{})
	opened, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreOpTimeout(-1))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	withinDefault("non-positive option", store)

	bounded := openSQLiteStoreWithOpTimeout(t, 3*time.Second)
	ctx, cancel := bounded.opContext(context.Background())
	defer cancel()
	if deadline, _ := ctx.Deadline(); time.Until(deadline) > 3*time.Second {
		t.Errorf("a 3s bound produced an op ctx expiring in %v", time.Until(deadline))
	}
}

// TestSQLiteStoreTxCallbackPastTheBoundFailsItsNextStoreCall is AT-S1.4: the
// op ctx rides sqliteStoreTx.ctx, so once the bound passes the callback's next
// store call fails, and Tx returns an error matching the sentinel, the ctx
// error and the callback's own error, with nothing persisted.
func TestSQLiteStoreTxCallbackPastTheBoundFailsItsNextStoreCall(t *testing.T) {
	store := openSQLiteStoreWithOpTimeout(t, sqliteTestOpBound)
	var inner error
	err := store.Tx("slow callback", func(tx Tx) error {
		if _, err := tx.Create(Bead{ID: "gc-tx1", Title: "before the bound"}); err != nil {
			return err
		}
		<-tx.(*sqliteStoreTx).ctx.Done() // the callback blocks past the bound
		_, inner = tx.Create(Bead{ID: "gc-tx2", Title: "after the bound"})
		return fmt.Errorf("callback: %w", inner)
	})
	if inner == nil {
		t.Fatal("a store call made through tx after the bound succeeded")
	}
	requireStoreTimeout(t, "Tx", err)
	if !errors.Is(err, inner) {
		t.Errorf("Tx error %v does not wrap the callback's own error %v", err, inner)
	}
	for _, id := range []string{"gc-tx1", "gc-tx2"} {
		if _, err := store.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound: Tx persisted a write past its bound", id, err)
		}
	}
}

// TestSQLiteRetryOnBusyStopsWithItsContext is AT-S1.5: an expired ctx ends the
// ladder early and the last error comes back unmarked, since the ladder did
// not run out; a live ctx keeps today's behavior.
func TestSQLiteRetryOnBusyStopsWithItsContext(t *testing.T) {
	busyErr := errors.New("database is locked (5) (SQLITE_BUSY)")

	t.Run("the sleep seam ends with the ctx", func(t *testing.T) {
		prev := sqliteBusySleep
		sqliteBusySleep = func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		}
		t.Cleanup(func() { sqliteBusySleep = prev })
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := retryOnBusy(ctx, func() error {
			calls++
			cancel() // expires while the ladder is about to back off
			return busyErr
		})
		if calls != 1 {
			t.Errorf("fn ran %d times after the ctx ended, want 1", calls)
		}
		if err != busyErr {
			t.Errorf("early stop returned %v, want the last error %v unchanged", err, busyErr)
		}
		if errors.Is(err, ErrSQLiteBusyExhausted) {
			t.Errorf("early stop returned %v marked ErrSQLiteBusyExhausted, but the ladder did not run out", err)
		}
	})

	t.Run("a live ctx still runs the whole ladder", func(t *testing.T) {
		prev := sqliteBusySleep
		sqliteBusySleep = func(context.Context, time.Duration) error { return nil }
		t.Cleanup(func() { sqliteBusySleep = prev })
		calls := 0
		err := retryOnBusy(context.Background(), func() error {
			calls++
			return busyErr
		})
		if calls != 1+sqliteBusyRetryAttempts {
			t.Errorf("fn ran %d times, want %d", calls, 1+sqliteBusyRetryAttempts)
		}
		if !errors.Is(err, ErrSQLiteBusyExhausted) {
			t.Errorf("exhausted ladder returned %v, want ErrSQLiteBusyExhausted", err)
		}
	})

	t.Run("the real seam waits on the timer and the ctx", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			if err := sqliteBusySleep(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("sleep past the ctx = %v, want context.DeadlineExceeded", err)
			}
			if elapsed := time.Since(start); elapsed != time.Second {
				t.Errorf("sleep past the ctx ended after %v, want when the ctx did (1s)", elapsed)
			}
			start = time.Now()
			if err := sqliteBusySleep(context.Background(), 2*time.Second); err != nil {
				t.Errorf("a sleep inside its ctx = %v, want nil", err)
			}
			if elapsed := time.Since(start); elapsed != 2*time.Second {
				t.Errorf("a sleep inside its ctx lasted %v, want 2s", elapsed)
			}
		})
	})

	t.Run("a short ctx ends the real ladder early", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			start := time.Now()
			calls := 0
			err := retryOnBusy(ctx, func() error {
				calls++
				return busyErr
			})
			if elapsed := time.Since(start); elapsed > 120*time.Millisecond {
				t.Errorf("the ladder ran %v past a 120ms ctx", elapsed)
			}
			if calls >= 1+sqliteBusyRetryAttempts {
				t.Errorf("fn ran %d times, want the ladder cut short", calls)
			}
			if errors.Is(err, ErrSQLiteBusyExhausted) || !errors.Is(err, busyErr) {
				t.Errorf("early stop returned %v, want the unmarked last error", err)
			}
		})
	})
}

// TestSQLiteStoreCloseStoreStopsAPurgeAfterOneDelete is AT-S1.6: CloseStore
// during a purge of many expired ids returns after at most the delete in
// flight, and the ids the purge had not reached stay.
func TestSQLiteStoreCloseStoreStopsAPurgeAfterOneDelete(t *testing.T) {
	dir := t.TempDir()
	seeded, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	seed := seeded.(*SQLiteStore)
	const total = 10
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gc-p%02d", i)
		ids = append(ids, id)
		if _, err := seed.CreateWithForeignID(Bead{ID: id, Title: "expired"}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
		if _, err := seed.db.Exec(`UPDATE beads SET status='closed', updated_at=? WHERE id=?`, old+int64(i), id); err != nil {
			t.Fatalf("expiring %s: %v", id, err)
		}
	}
	slow := ids[2]
	slowDeleteTrigger(t, seed, slow)
	if err := seed.CloseStore(); err != nil {
		t.Fatalf("closing the seeding store: %v", err)
	}

	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreRetention(time.Hour, 10*time.Millisecond))
	if err != nil {
		t.Fatalf("reopening with the sweeper: %v", err)
	}
	store := opened.(*SQLiteStore)
	gone := func(id string) bool {
		_, err := store.Get(id)
		return errors.Is(err, ErrNotFound)
	}
	// The sweeper deletes the two ids before the slow one, then sits in the
	// slow delete; spin (no fixed sleep) until it has got there.
	deadline := time.Now().Add(30 * time.Second)
	for !(gone(ids[0]) && gone(ids[1])) {
		if time.Now().After(deadline) {
			t.Fatal("the sweeper never reached the slow delete")
		}
		runtime.Gosched()
	}

	closed := make(chan error, 1)
	go func() { closed <- store.CloseStore() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CloseStore did not return: it is waiting for the whole purge, not one delete")
	}

	reopened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("reopening to inspect: %v", err)
	}
	check := reopened.(*SQLiteStore)
	t.Cleanup(func() { _ = check.CloseStore() })
	for _, id := range ids[2:] {
		if _, err := check.Get(id); err != nil {
			t.Errorf("Get(%s) = %v, want it kept: the purge was stopped before it", id, err)
		}
	}
}

// TestSQLiteStoreDeleteBatchBoundReportsTheCommittedChunks is AT-S1.7: when the
// bound fires after k chunks the error is a *BatchDeleteError holding exactly
// those chunks, it matches the sentinel through Unwrap, and the rest stay.
func TestSQLiteStoreDeleteBatchBoundReportsTheCommittedChunks(t *testing.T) {
	store := openSQLiteStoreWithOpTimeout(t, 500*time.Millisecond)
	const total = 600
	ids := make([]string, 0, total)
	rows := make([]Bead, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gc-b%04d", i)
		if i == 300 {
			id = "gc-slow"
		}
		ids = append(ids, id)
		rows = append(rows, Bead{ID: id, Title: "batch"})
	}
	store.opTimeout = time.Minute // seeding is not under test
	if err := store.CreateWithForeignIDBatch(rows); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	slowDeleteTrigger(t, store, "gc-slow")
	store.opTimeout = 500 * time.Millisecond

	err := store.DeleteBatch(ids)
	var batchErr *BatchDeleteError
	if !errors.As(err, &batchErr) {
		t.Fatalf("DeleteBatch = %v, want a *BatchDeleteError", err)
	}
	requireStoreTimeout(t, "DeleteBatch", err)
	if want := ids[:sqliteDeleteBatchChunk]; !reflect.DeepEqual(batchErr.Committed, want) {
		t.Fatalf("Committed holds %d ids, want exactly the first chunk of %d", len(batchErr.Committed), len(want))
	}
	for _, id := range ids[:sqliteDeleteBatchChunk] {
		if _, err := store.Get(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%s) = %v, want it deleted", id, err)
		}
	}
	for _, id := range ids[sqliteDeleteBatchChunk:] {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("Get(%s) = %v, want it kept", id, err)
		}
	}
}

// TestSQLiteStoreDeleteSidecarFailureIsNotTheSentinel is AT-S1.8: a failure of
// the local-strings cleanup that follows a committed delete is reported as
// itself, never as the store's timeout, and the row is gone.
func TestSQLiteStoreDeleteSidecarFailureIsNotTheSentinel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete func(s *SQLiteStore, id string, rev int64) error
	}{
		{"Delete", func(s *SQLiteStore, id string, _ int64) error { return s.Delete(id) }},
		{"DeleteIfMatch", func(s *SQLiteStore, id string, rev int64) error { return s.DeleteIfMatch(id, rev) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openSQLiteStoreForTest(t, t.TempDir())
			made, err := store.Create(Bead{Title: "has local strings"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			created, err := store.Get(made.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := store.SetLocalString(created.ID, "k", "v"); err != nil {
				t.Fatalf("SetLocalString: %v", err)
			}
			// A sidecar whose parent is a regular file can be neither read nor written.
			blocker := filepath.Join(t.TempDir(), "blocker")
			if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
				t.Fatalf("writing the blocker file: %v", err)
			}
			store.localStrings = newLocalSidecar(filepath.Join(blocker, "local-strings.json"))

			err = tc.delete(store, created.ID, created.Revision)
			if err == nil || !strings.Contains(err.Error(), "cleaning up local strings") {
				t.Fatalf("delete = %v, want the local-strings cleanup error", err)
			}
			if errors.Is(err, ErrStoreTimeout) {
				t.Errorf("delete = %v matches ErrStoreTimeout; a post-commit step never returns the sentinel", err)
			}
			if _, err := store.Get(created.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get after the delete = %v, want ErrNotFound: the delete committed", err)
			}
		})
	}
}
