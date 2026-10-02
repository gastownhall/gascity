package beads

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedSQLiteGraphIDs writes one minimal bead per id in a single transaction on
// the store's write handle. It bypasses Create on purpose: the point is to
// reproduce an on-disk id population (including ids another process minted),
// not to exercise the allocator while seeding it.
func seedSQLiteGraphIDs(t *testing.T, s *SQLiteStore, ids []string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	now := time.Now()
	for _, id := range ids {
		if err := s.upsertBeadTx(ctx, tx, Bead{ID: id, Title: "seed", Status: "open", Type: "task", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

// wrappedGraphIDs returns count ids the pre-fix int64 allocator produced after
// it overflowed past math.MaxInt64: gcg--9223372036854775808, then upward.
func wrappedGraphIDs(count int) []string {
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		ids = append(ids, fmt.Sprintf("%s-%d", sqliteGraphPrefix, int64(math.MinInt64)+int64(i)))
	}
	return ids
}

// nearMaxGraphIDs are the outliers a deployed graph holds: an id at
// math.MaxInt64-1 (and at MaxInt64, which the old allocator minted just
// before it wrapped).
func nearMaxGraphIDs() []string {
	return []string{
		fmt.Sprintf("%s-%d", sqliteGraphPrefix, int64(math.MaxInt64)-1),
		fmt.Sprintf("%s-%d", sqliteGraphPrefix, int64(math.MaxInt64)),
	}
}

// TestSQLiteStoreSequenceIgnoresNearMaxAndWrappedIDs is the maintainer-city
// allocator state: gcg-9223372036854775806 plus a dense block of wrapped
// "gcg--9223…" ids. The old recovery reseeded next to math.MaxInt64 (reading
// "-N" as N), so the next Add wrapped to math.MinInt64 and every fresh process
// walked the whole wrapped block inside its write transaction. Recovery must
// resume after the largest allocatable id instead, with no walk and no reseed.
func TestSQLiteStoreSequenceIgnoresNearMaxAndWrappedIDs(t *testing.T) {
	dir := t.TempDir()
	seeder := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	ids := []string{"gcg-1", "gcg-2", "gcg-48227"}
	ids = append(ids, nearMaxGraphIDs()...)
	ids = append(ids, wrappedGraphIDs(500)...)
	seedSQLiteGraphIDs(t, seeder, ids)

	var reseeds atomic.Int32
	prev := observeSQLiteSequenceReseed
	observeSQLiteSequenceReseed = func() { reseeds.Add(1) }
	t.Cleanup(func() { observeSQLiteSequenceReseed = prev })

	fresh := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if got := fresh.seq.Load(); got != 48227 {
		t.Fatalf("recovered sequence = %d, want 48227 (the largest allocatable id)", got)
	}
	created, err := fresh.Create(Bead{Title: "next"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "gcg-48228" {
		t.Fatalf("Create minted %q, want gcg-48228", created.ID)
	}
	if got := reseeds.Load(); got != 0 {
		t.Fatalf("reseed scans = %d, want 0", got)
	}
}

// TestSQLiteStoreSequenceNeverOverflows: the allocator reaches its ceiling as
// an explicit error, never by wrapping into negative ids, and nothing near
// math.MaxInt64 — a pinned id, a census floor — can lift it past the ceiling.
func TestSQLiteStoreSequenceNeverOverflows(t *testing.T) {
	s := newSQLiteGraphApplyStore(t, t.TempDir(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if _, err := s.Create(Bead{ID: fmt.Sprintf("gcg-%d", int64(math.MaxInt64)-1), Title: "pinned near max"}); err != nil {
		t.Fatalf("Create pinned: %v", err)
	}
	s.AdvanceSequenceFloor(math.MaxInt64)
	if got := s.seq.Load(); got != 0 {
		t.Fatalf("sequence = %d after near-max pinned id and floor, want 0", got)
	}

	s.seq.Store(sqliteSequenceCeiling - 1)
	last, err := s.Create(Bead{Title: "last"})
	if err != nil {
		t.Fatalf("Create at ceiling: %v", err)
	}
	if want := fmt.Sprintf("gcg-%d", sqliteSequenceCeiling); last.ID != want {
		t.Fatalf("Create minted %q, want %q", last.ID, want)
	}
	over, err := s.Create(Bead{Title: "over"})
	if err == nil {
		t.Fatalf("Create past the ceiling minted %q, want an exhaustion error", over.ID)
	}
	if !strings.Contains(err.Error(), "sequence exhausted") {
		t.Fatalf("Create past the ceiling: %v, want sequence exhausted", err)
	}
}

// TestSQLiteStoreSequenceIgnoresOutOfRangeSuffix: a pinned id whose numeric
// tail does not fit in int64 used to clamp the allocator to MaxInt64 (Atoi's
// range error was discarded), which is one way a store's sequence reaches the
// wrap. The allocator can never mint such an id, so it must not move.
func TestSQLiteStoreSequenceIgnoresOutOfRangeSuffix(t *testing.T) {
	s := newSQLiteGraphApplyStore(t, t.TempDir(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if _, err := s.Create(Bead{ID: "gcg-123456789012345678901234", Title: "huge"}); err != nil {
		t.Fatalf("Create pinned: %v", err)
	}
	if got := s.seq.Load(); got != 0 {
		t.Fatalf("sequence = %d after out-of-range pinned id, want 0", got)
	}
	created, err := s.Create(Bead{Title: "next"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "gcg-1" {
		t.Fatalf("Create minted %q, want gcg-1", created.ID)
	}
}

// TestSQLiteStoreStaleFloorProbesBeforeFullReseed covers the common stale-floor
// case (another process minted a few ids since this one opened): a graph apply
// must resolve it with point lookups, not with a whole-table reseed per node.
func TestSQLiteStoreStaleFloorProbesBeforeFullReseed(t *testing.T) {
	dir := t.TempDir()
	cli := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	controller := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	for i := 0; i < 40; i++ {
		if _, err := controller.Create(Bead{Title: "controller"}); err != nil {
			t.Fatalf("controller Create: %v", err)
		}
	}
	var reseeds atomic.Int32
	prev := observeSQLiteSequenceReseed
	observeSQLiteSequenceReseed = func() { reseeds.Add(1) }
	t.Cleanup(func() { observeSQLiteSequenceReseed = prev })

	plan := &GraphApplyPlan{}
	for i := 0; i < 20; i++ {
		plan.Nodes = append(plan.Nodes, GraphApplyNode{Key: strconv.Itoa(i), Title: "step"})
	}
	result, err := cli.ApplyGraphPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if got := reseeds.Load(); got != 0 {
		t.Fatalf("full reseed scans = %d, want 0 for a 40-id stale floor", got)
	}
	seen := map[string]bool{}
	for _, id := range result.IDs {
		if seen[id] {
			t.Fatalf("duplicate id %s in %v", id, result.IDs)
		}
		seen[id] = true
		if n, _ := strconv.Atoi(strings.TrimPrefix(id, "gcg-")); n <= 40 {
			t.Fatalf("minted %s inside the controller's range", id)
		}
	}
}

// TestSQLiteStoreWriteTxTakesWriteLockAtBegin is the SQLITE_BUSY_SNAPSHOT (517)
// mechanism in isolation. A deferred transaction reads first, pinning a WAL
// snapshot; if another connection commits before the transaction's first
// write, the read->write upgrade fails with 517, which busy_timeout cannot wait
// out. The write handle must open transactions with BEGIN IMMEDIATE so the
// competing writer is the one that waits.
//
// The competing writer is a raw connection with busy_timeout 0, so the outcome
// is decided by lock state alone, not by how long anything waits: it is refused
// at once if our transaction holds the write lock, and commits at once if not.
func TestSQLiteStoreWriteTxTakesWriteLockAtBegin(t *testing.T) {
	dir := t.TempDir()
	cli := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	existing, err := cli.Create(Bead{Title: "existing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: cli.path, RawQuery: "_pragma=busy_timeout(0)"}).String())
	if err != nil {
		t.Fatalf("open competing connection: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	ctx := context.Background()
	tx, err := cli.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := cli.idExistsTx(ctx, tx, existing.ID); err != nil {
		t.Fatalf("read in tx: %v", err)
	}

	_, otherErr := other.ExecContext(ctx, `INSERT INTO kv(key,value) VALUES('competing-writer','1')`)
	writeErr := cli.clearClaimFenceTx(ctx, tx, existing.ID)
	if otherErr == nil {
		t.Fatalf("a competing writer committed inside our write transaction (begin is not IMMEDIATE); our first write then got: %v", writeErr)
	}
	if !isSQLiteBusy(otherErr) {
		t.Fatalf("competing writer: %v, want SQLITE_BUSY", otherErr)
	}
	if writeErr != nil {
		t.Fatalf("first write in tx: %v", writeErr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := other.ExecContext(ctx, `INSERT INTO kv(key,value) VALUES('competing-writer','1')`); err != nil {
		t.Fatalf("competing writer after our commit: %v", err)
	}
}

// TestSQLiteStoreGraphCookUnderConcurrentWriter reproduces the maintainer-city
// cook failure end to end: a large graph store holding an id at
// math.MaxInt64-1 and a block of ids from the overflowed old allocator,
// a fresh CLI process cooking a multi-node workflow, and a controller
// committing continuously. Before the fix the cook reseeded next to
// math.MaxInt64, wrapped, and walked the occupied block (with a full-table
// reseed per node) inside a deferred transaction, and failed with
// "clearing claim fence ...: database is locked (517)" on every retry.
func TestSQLiteStoreGraphCookUnderConcurrentWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds a large store")
	}
	prevSleep := sqliteBusySleep
	sqliteBusySleep = func(time.Duration) {}
	t.Cleanup(func() { sqliteBusySleep = prevSleep })

	dir := t.TempDir()
	controller := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	ids := make([]string, 0, 25000)
	for i := 1; i <= 20000; i++ {
		ids = append(ids, "gcg-"+strconv.Itoa(i))
	}
	ids = append(ids, nearMaxGraphIDs()...)
	ids = append(ids, wrappedGraphIDs(2000)...)
	seedSQLiteGraphIDs(t, controller, ids)
	hot, err := controller.Create(Bead{Title: "controller-hot"})
	if err != nil {
		t.Fatalf("controller Create: %v", err)
	}

	cli := newSQLiteGraphApplyStore(t, dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))

	stop := make(chan struct{})
	firstCommit := make(chan struct{})
	var wg sync.WaitGroup
	var controllerWrites atomic.Int64
	wg.Add(1)
	// Paced far above the ~1 commit/s measured on the live city, but not a
	// tight loop: a writer that re-takes the lock the instant it commits just
	// measures SQLite's (unfair) busy-handler polling, not this store.
	pace := time.NewTicker(2 * time.Millisecond)
	defer pace.Stop()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-pace.C:
			}
			if err := controller.SetMetadata(hot.ID, "tick", strconv.Itoa(i)); err == nil {
				if controllerWrites.Add(1) == 1 {
					close(firstCommit)
				}
			}
		}
	}()
	select {
	case <-firstCommit:
	case <-time.After(30 * time.Second):
		close(stop)
		wg.Wait()
		t.Fatal("controller never committed")
	}

	plan := &GraphApplyPlan{}
	for i := 0; i < 30; i++ {
		node := GraphApplyNode{Key: strconv.Itoa(i), Title: "step"}
		if i > 0 {
			node.ParentKey = "0"
		}
		plan.Nodes = append(plan.Nodes, node)
	}
	result, applyErr := cli.ApplyGraphPlan(context.Background(), plan)
	close(stop)
	wg.Wait()
	if applyErr != nil {
		t.Fatalf("ApplyGraphPlan under a concurrent writer: %v (controller commits: %d)", applyErr, controllerWrites.Load())
	}
	if len(result.IDs) != len(plan.Nodes) {
		t.Fatalf("result ids = %d, want %d", len(result.IDs), len(plan.Nodes))
	}
	for _, id := range result.IDs {
		n, err := strconv.ParseInt(strings.TrimPrefix(id, "gcg-"), 10, 64)
		if err != nil || n <= 20000 || n > 20000+int64(len(plan.Nodes))+1 {
			t.Fatalf("cook minted %s, want the ids right after gcg-20000 (and the controller's one)", id)
		}
	}
}

// TestSQLiteListMetadataQueriesDriveOffMetadataIndex pins the plan behind
// `gc workflow delete-source` and every ListByMetadata caller. Without stats
// SQLite preferred idx_beads_tier_status (tier='main') and walked every
// main-tier row of the graph to test id IN (metadata subquery); on the 1.4 GB
// maintainer-city graph a dry-run delete-source took 189 s.
func TestSQLiteListMetadataQueriesDriveOffMetadataIndex(t *testing.T) {
	s := newSQLiteGraphApplyStore(t, t.TempDir(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	source := map[string]string{"gc.source_bead_id": "mc-5495l"}
	cases := map[string]ListQuery{
		"live":             {Metadata: source},
		"include closed":   {Metadata: source, IncludeClosed: true},
		"status":           {Metadata: source, Status: "open"},
		"type":             {Metadata: source, Type: "task"},
		"assignee":         {Metadata: source, Assignee: "worker"},
		"list by metadata": {Metadata: source, Limit: 10, Sort: SortCreatedDesc},
		"two keys":         {Metadata: map[string]string{"gc.source_bead_id": "mc-5495l", "gc.kind": "workflow"}},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			sqlText, args := sqliteListSQL(q, "b.id")
			rows, err := s.readDB.Query("EXPLAIN QUERY PLAN "+sqlText, args...)
			if err != nil {
				t.Fatalf("explain %s: %v", sqlText, err)
			}
			defer rows.Close() //nolint:errcheck
			var plan []string
			for rows.Next() {
				var id, parent, notUsed int
				var detail string
				if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
					t.Fatalf("scan plan: %v", err)
				}
				plan = append(plan, detail)
			}
			joined := strings.Join(plan, " | ")
			if !strings.Contains(joined, "SEARCH b USING INDEX sqlite_autoindex_beads_1 (id=?)") ||
				!strings.Contains(joined, "idx_metadata_key_value") {
				t.Fatalf("metadata list does not drive off the metadata index:\n  sql: %s\n  plan: %s", sqlText, joined)
			}
		})
	}

	// The residual filters must still filter.
	keep, err := s.Create(Bead{Title: "keep", Type: "task", Assignee: "worker", Metadata: source})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(Bead{Title: "other type", Type: "bug", Assignee: "worker", Metadata: source}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed, err := s.Create(Bead{Title: "closed", Type: "task", Assignee: "worker", Metadata: source})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := s.List(ListQuery{Metadata: source, Type: "task", Assignee: "worker"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != keep.ID {
		t.Fatalf("List = %v, want only %s", got, keep.ID)
	}
}

func TestSQLiteStoreWriterDSNBeginsImmediate(t *testing.T) {
	const path = "/city/.gc/store/graph/beads.sqlite"
	writer, err := url.Parse(sqliteStoreWriterDSN(path))
	if err != nil {
		t.Fatalf("parse writer DSN: %v", err)
	}
	if got := writer.Query().Get("_txlock"); got != "immediate" {
		t.Fatalf("writer DSN _txlock = %q, want immediate", got)
	}
	if got := writer.Query().Get("mode"); got != "" {
		t.Fatalf("writer DSN mode = %q, want read-write default", got)
	}
	if !slices.Equal(dsnPragmas(t, writer.String()), dsnPragmas(t, sqliteStoreDSN(path, false))) {
		t.Fatalf("writer DSN pragmas diverge from the shared DSN")
	}
	for _, dsn := range []string{sqliteStoreDSN(path, false), sqliteStoreDSN(path, true), sqliteStorePrivateRecoveryDSN(path)} {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse %s: %v", dsn, err)
		}
		if got := parsed.Query().Get("_txlock"); got != "" {
			t.Fatalf("shared/read DSN %s carries _txlock=%s; the read pool must stay deferred", dsn, got)
		}
	}
}
