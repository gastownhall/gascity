package beads

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSQLiteStoreTxCommitsEveryWriteAtomically(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := s.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	seed, err := store.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	if err := store.Tx("atomic success", func(tx Tx) error {
		created, err := tx.Create(Bead{Title: "created in tx"})
		if err != nil {
			return err
		}
		if err := tx.Update(seed.ID, UpdateOpts{Title: ptrTo("updated in tx")}); err != nil {
			return err
		}
		if err := tx.SetMetadataBatch(seed.ID, map[string]string{"phase": "committed"}); err != nil {
			return err
		}
		return tx.Close(created.ID)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if !store.AtomicTx() || !StoreSupportsAtomicTx(store) {
		t.Fatal("SQLite store did not report atomic transactions")
	}
	seed, err = store.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get seed: %v", err)
	}
	if seed.Title != "updated in tx" || seed.Metadata["phase"] != "committed" {
		t.Fatalf("committed seed = %+v, want transaction writes", seed)
	}
	rows, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("committed row count = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.Title == "created in tx" && row.Status == "closed" {
			return
		}
	}
	t.Fatalf("committed rows = %+v, want created closed row", rows)
}

func TestSQLiteStoreTxRollsBackEveryWriteOnCallbackError(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := s.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	seed, err := store.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	wantErr := errors.New("rollback please")
	err = store.Tx("atomic rollback", func(tx Tx) error {
		created, err := tx.Create(Bead{Title: "must disappear"})
		if err != nil {
			return err
		}
		if err := tx.Update(seed.ID, UpdateOpts{Title: ptrTo("must roll back")}); err != nil {
			return err
		}
		if err := tx.SetMetadataBatch(seed.ID, map[string]string{"phase": "must roll back"}); err != nil {
			return err
		}
		if err := tx.Close(created.ID); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Tx error = %v, want %v", err, wantErr)
	}
	seed, err = store.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get seed: %v", err)
	}
	if seed.Title != "seed" || len(seed.Metadata) != 0 || seed.Status != "open" {
		t.Fatalf("rolled-back seed = %+v", seed)
	}
	rows, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows after rollback = %+v, want only seed", rows)
	}
}

func TestSQLiteStoreReadsAndWritesLegacyThreeColumnDepsSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, sqliteStoreFilename)
	createSQLiteSchemaFixture(t, dir, false, true, nil)
	schemaBefore := sqliteSchemaFingerprint(t, dbPath)
	opened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore legacy: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	a, err := store.Create(Bead{Title: "a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := store.Create(Bead{Title: "b"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if err := store.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}
	if err := store.DepAdd(a.ID, b.ID, "waits-for"); err != nil {
		t.Fatalf("DepAdd replacement type: %v", err)
	}
	deps, err := store.DepList(a.ID, "down")
	if err != nil || len(deps) != 1 || deps[0].Type != "waits-for" {
		t.Fatalf("legacy deps after replacement = %+v, %v", deps, err)
	}
	if err := store.UpdateIfMatch(a.ID, 0, UpdateOpts{Title: ptrTo("conditional")}); !errors.Is(err, ErrConditionalWriteUnsupported) {
		t.Fatalf("conditional write on pre-revision Graph = %v, want ErrConditionalWriteUnsupported", err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	// The compatibility path must not rewrite the historic three-column key:
	// an older binary can reopen this database unchanged.
	check, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen legacy schema: %v", err)
	}
	defer check.Close() //nolint:errcheck
	legacy, err := sqliteDepsUsesLegacyPrimaryKey(context.Background(), check)
	if err != nil || !legacy {
		t.Fatalf("legacy deps key after writes = %v, %v", legacy, err)
	}
	if got := sqliteSchemaFingerprint(t, dbPath); got != schemaBefore {
		t.Fatalf("opening and writing legacy Graph rewrote its schema:\n--- before ---\n%s\n--- after ---\n%s", schemaBefore, got)
	}
	// This is the exact old binary's dependency write shape. It must continue
	// to execute after this candidate has modified the same database.
	if _, err := check.Exec(`
		INSERT INTO deps(issue_id, depends_on_id, dep_type) VALUES(?,?,?)
		ON CONFLICT(issue_id, depends_on_id, dep_type) DO NOTHING`, a.ID, b.ID, "tracks"); err != nil {
		t.Fatalf("old-binary dependency write after candidate = %v", err)
	}
	if err := check.Close(); err != nil {
		t.Fatalf("close old-binary connection: %v", err)
	}

	// Finish the full rollback interoperability sequence: old fixture -> this
	// candidate -> old binary mutation -> this candidate reopening the same
	// database. The final candidate must both read the old write and restore the
	// one-edge-per-pair behavior without changing the legacy schema.
	reopened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore candidate after old-binary write: %v", err)
	}
	candidate := reopened.(*SQLiteStore)
	t.Cleanup(func() { _ = candidate.CloseStore() })
	deps, err = candidate.DepList(a.ID, "down")
	if err != nil {
		t.Fatalf("candidate DepList after old-binary write: %v", err)
	}
	seenTypes := make(map[string]bool, len(deps))
	for _, dep := range deps {
		seenTypes[dep.Type] = true
	}
	if !seenTypes["waits-for"] || !seenTypes["tracks"] {
		t.Fatalf("candidate deps after old-binary write = %+v, want waits-for and tracks", deps)
	}
	if err := candidate.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("candidate dependency replacement after old-binary write: %v", err)
	}
	deps, err = candidate.DepList(a.ID, "down")
	if err != nil || len(deps) != 1 || deps[0].Type != "blocks" {
		t.Fatalf("candidate deps after replacement = %+v, %v; want one blocks edge", deps, err)
	}
	if got := sqliteSchemaFingerprint(t, dbPath); got != schemaBefore {
		t.Fatalf("candidate reopen after old-binary write rewrote legacy schema:\n--- before ---\n%s\n--- after ---\n%s", schemaBefore, got)
	}
}

func sqliteSchemaFingerprint(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open schema fingerprint: %v", err)
	}
	defer db.Close() //nolint:errcheck
	rows, err := db.Query(`
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		t.Fatalf("query schema fingerprint: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out strings.Builder
	for rows.Next() {
		var typ, name, table, ddl string
		if err := rows.Scan(&typ, &name, &table, &ddl); err != nil {
			t.Fatalf("scan schema fingerprint: %v", err)
		}
		fmt.Fprintf(&out, "%s|%s|%s|%s\n", typ, name, table, ddl) //nolint:errcheck
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema fingerprint: %v", err)
	}
	return out.String()
}

func TestSQLiteStoreRetentionSweeperIsDisabledByDefault(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	if !store.disableRetentionSweeper {
		t.Fatal("default SQLite store enabled terminal-record deletion")
	}
}

func TestSQLiteStoreCreatesAndGets(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			c.CloseStore() //nolint:errcheck
		}
	}()

	b := Bead{Title: "hello world", Type: "task"}
	created, err := s.Create(b)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created bead has empty ID")
	}
	if created.Status != "open" {
		t.Fatalf("expected status=open, got %q", created.Status)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "hello world" {
		t.Fatalf("expected title %q, got %q", "hello world", got.Title)
	}
}

func TestSQLiteStorePersistsLocalStringsOutsideDatabase(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore (first): %v", err)
	}
	store := opened.(*SQLiteStore)
	created, err := store.Create(Bead{Title: "local state"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetLocalString(created.ID, "last_woke_at", "2026-07-26T00:00:00Z"); err != nil {
		t.Fatalf("SetLocalString: %v", err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore (first): %v", err)
	}

	sidecarPath := filepath.Join(dir, ".beads", "local-strings.json")
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("local-string sidecar %q: %v", sidecarPath, err)
	}

	reopened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore (second): %v", err)
	}
	reopenedStore := reopened.(*SQLiteStore)
	defer func() { _ = reopenedStore.CloseStore() }()
	got, err := reopenedStore.GetLocalString(created.ID, "last_woke_at")
	if err != nil {
		t.Fatalf("GetLocalString after reopen: %v", err)
	}
	if got != "2026-07-26T00:00:00Z" {
		t.Fatalf("GetLocalString after reopen = %q, want persisted value", got)
	}
	durable, err := reopenedStore.Get(created.ID)
	if err != nil {
		t.Fatalf("Get durable bead: %v", err)
	}
	if _, leaked := durable.Metadata["last_woke_at"]; leaked {
		t.Fatal("clone-local string leaked into durable bead metadata")
	}
}

func TestSQLiteStoreReadOnlyRejectsLocalStringWrites(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore (rw): %v", err)
	}
	store := opened.(*SQLiteStore)
	created, err := store.Create(Bead{Title: "migration source"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore (rw): %v", err)
	}

	readOnly, err := OpenSQLiteStore(dir, WithSQLiteStoreReadOnly())
	if err != nil {
		t.Fatalf("OpenSQLiteStore (ro): %v", err)
	}
	readOnlyStore := readOnly.(*SQLiteStore)
	defer func() { _ = readOnlyStore.CloseStore() }()
	if err := readOnlyStore.SetLocalString(created.ID, "k", "v"); err == nil {
		t.Fatal("SetLocalString on read-only store succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", "local-strings.json")); !os.IsNotExist(err) {
		t.Fatalf("read-only SetLocalString created a sidecar: %v", err)
	}
}

func TestSQLiteStoreReleaseIfCurrent(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			c.CloseStore() //nolint:errcheck
		}
	}()

	created, err := s.Create(Bead{Title: "work", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := s.Update(created.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}

	releaser := s.(ConditionalAssignmentReleaser)
	released, err := releaser.ReleaseIfCurrent(created.ID, "worker-2")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent wrong assignee: %v", err)
	}
	if released {
		t.Fatal("ReleaseIfCurrent released a bead with the wrong assignee")
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after skipped release: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("skipped release mutated bead: %+v", got)
	}

	released, err = releaser.ReleaseIfCurrent("missing", "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent missing id: %v", err)
	}
	if released {
		t.Fatal("ReleaseIfCurrent released missing bead")
	}

	openBead, err := s.Create(Bead{Title: "open work", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create open bead: %v", err)
	}
	released, err = releaser.ReleaseIfCurrent(openBead.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent wrong status: %v", err)
	}
	if released {
		t.Fatal("ReleaseIfCurrent released non-in-progress bead")
	}

	released, err = releaser.ReleaseIfCurrent(created.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent matching assignee: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent did not release matching in-progress assignment")
	}
	got, err = s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after release: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("released bead = %+v, want open and unassigned", got)
	}
}

func TestSQLiteStoreReady(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			c.CloseStore() //nolint:errcheck
		}
	}()

	// Create an unblocked bead.
	free, err := s.Create(Bead{Title: "free task", Type: "task"})
	if err != nil {
		t.Fatalf("create free: %v", err)
	}

	// Create a blocker and a blocked bead (dependency wired via DepAdd).
	blocker, err := s.Create(Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := s.Create(Bead{Title: "blocked task", Type: "task"})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}
	if err := s.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("dep add: %v", err)
	}

	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("ready: %v", err)
	}

	readyIDs := make(map[string]bool)
	for _, b := range ready {
		readyIDs[b.ID] = true
	}
	if !readyIDs[free.ID] {
		t.Errorf("free bead %q should be ready", free.ID)
	}
	if !readyIDs[blocker.ID] {
		t.Errorf("blocker %q should be ready", blocker.ID)
	}
	if readyIDs[blocked.ID] {
		t.Errorf("blocked bead %q should NOT be ready", blocked.ID)
	}
}

func TestSQLiteStoreReadyHonorsTierMode(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			c.CloseStore() //nolint:errcheck
		}
	}()

	history, err := s.Create(Bead{Title: "history", Type: "task"})
	if err != nil {
		t.Fatalf("create history: %v", err)
	}
	noHistory, err := s.Create(Bead{Title: "no history", Type: "task", NoHistory: true})
	if err != nil {
		t.Fatalf("create no history: %v", err)
	}
	ephemeral, err := s.Create(Bead{Title: "ephemeral", Type: "task", Ephemeral: true})
	if err != nil {
		t.Fatalf("create ephemeral: %v", err)
	}

	defaultReady, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready(default): %v", err)
	}
	if sqliteReadyIDSet(defaultReady)[ephemeral.ID] {
		t.Fatalf("Ready(default) included ephemeral row %q: %+v", ephemeral.ID, defaultReady)
	}
	if !sqliteReadyIDSet(defaultReady)[history.ID] || !sqliteReadyIDSet(defaultReady)[noHistory.ID] {
		t.Fatalf("Ready(default) = %+v, want history and no-history rows", defaultReady)
	}

	wisps, err := s.Ready(ReadyQuery{TierMode: TierWisps})
	if err != nil {
		t.Fatalf("Ready(TierWisps): %v", err)
	}
	wispIDs := sqliteReadyIDSet(wisps)
	if wispIDs[history.ID] {
		t.Fatalf("Ready(TierWisps) included history row %q: %+v", history.ID, wisps)
	}
	if !wispIDs[noHistory.ID] || !wispIDs[ephemeral.ID] {
		t.Fatalf("Ready(TierWisps) = %+v, want no-history and ephemeral rows", wisps)
	}

	both, err := s.Ready(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("Ready(TierBoth): %v", err)
	}
	bothIDs := sqliteReadyIDSet(both)
	for _, id := range []string{history.ID, noHistory.ID, ephemeral.ID} {
		if !bothIDs[id] {
			t.Fatalf("Ready(TierBoth) = %+v, missing %s", both, id)
		}
	}
}

func TestSQLiteStoreCloseStore(t *testing.T) {
	// settleBelow yields until the goroutine count drops to at most target
	// (CloseStore joins the sweeper synchronously; only database/sql's
	// internal closer goroutines need a beat), bounded so a real leak still
	// fails. No fixed sleep — the fixed_sleep census ratchet is hard.
	settleBelow := func(target int) int {
		n := runtime.NumGoroutine()
		for i := 0; i < 200_000 && n > target; i++ {
			runtime.Gosched()
			if i%1000 == 0 {
				runtime.GC()
			}
			n = runtime.NumGoroutine()
		}
		return n
	}

	settleBelow(0)
	base := runtime.NumGoroutine()

	s, err := OpenSQLiteStore(t.TempDir(),
		WithSQLiteStoreRetention(4*time.Hour, 30*time.Second))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	closer, ok := s.(interface{ CloseStore() error })
	if !ok {
		t.Fatal("SQLiteStore does not implement CloseStore() error")
	}
	if err := closer.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	// Idempotent second call must not error.
	if err := closer.CloseStore(); err != nil {
		t.Fatalf("second CloseStore: %v", err)
	}

	residual := settleBelow(base+5) - base
	if residual > 5 {
		t.Fatalf("CloseStore leaked goroutines: residual=%d after open+close (want <=5)", residual)
	}
}

func TestSQLiteStoreCloseStoreRetriesOnlyFailedResources(t *testing.T) {
	tests := []struct {
		name       string
		failRead   bool
		failWrite  bool
		wantFirst  []string
		wantSecond []string
	}{
		{
			name:       "read database",
			failRead:   true,
			wantFirst:  []string{"stop", "read", "write"},
			wantSecond: []string{"stop", "read", "write", "read"},
		},
		{
			name:       "write database",
			failWrite:  true,
			wantFirst:  []string{"stop", "read", "write"},
			wantSecond: []string{"stop", "read", "write", "write"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closeErr := errors.New("database close failed")
			var calls []string
			readFails := tt.failRead
			writeFails := tt.failWrite
			retentionDone := make(chan struct{})
			store := &SQLiteStore{
				retentionStop: func() {
					calls = append(calls, "stop")
					close(retentionDone)
				},
				retentionDone: retentionDone,
				closeReadDB: func() error {
					calls = append(calls, "read")
					if readFails {
						readFails = false
						return closeErr
					}
					return nil
				},
				closeWriteDB: func() error {
					calls = append(calls, "write")
					if writeFails {
						writeFails = false
						return closeErr
					}
					return nil
				},
			}

			if err := store.CloseStore(); !errors.Is(err, closeErr) {
				t.Fatalf("first CloseStore() error = %v, want close error", err)
			}
			if got := fmt.Sprint(calls); got != fmt.Sprint(tt.wantFirst) {
				t.Fatalf("first close calls = %s, want %s", got, tt.wantFirst)
			}
			if tt.failRead && store.closeReadDB == nil {
				t.Fatal("failed read database close was discarded")
			}
			if tt.failWrite && store.closeWriteDB == nil {
				t.Fatal("failed write database close was discarded")
			}

			if err := store.CloseStore(); err != nil {
				t.Fatalf("retry CloseStore(): %v", err)
			}
			if got := fmt.Sprint(calls); got != fmt.Sprint(tt.wantSecond) {
				t.Fatalf("retry close calls = %s, want %s", got, tt.wantSecond)
			}
			if store.closeReadDB != nil || store.closeWriteDB != nil {
				t.Fatal("successful close resources were retained")
			}
			if err := store.CloseStore(); err != nil {
				t.Fatalf("idempotent CloseStore(): %v", err)
			}
			if got := fmt.Sprint(calls); got != fmt.Sprint(tt.wantSecond) {
				t.Fatalf("idempotent close calls = %s, want %s", got, tt.wantSecond)
			}
		})
	}
}

func TestSQLiteStoreCloseStoreSerializesConcurrentCallers(t *testing.T) {
	var calls []string
	retentionDone := make(chan struct{})
	store := &SQLiteStore{
		retentionStop: func() {
			calls = append(calls, "stop")
			close(retentionDone)
		},
		retentionDone: retentionDone,
		closeReadDB: func() error {
			calls = append(calls, "read")
			return nil
		},
		closeWriteDB: func() error {
			calls = append(calls, "write")
			return nil
		},
	}

	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			errs <- store.CloseStore()
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("CloseStore() error = %v, want nil", err)
		}
	}
	if got, want := fmt.Sprint(calls), "[stop read write]"; got != want {
		t.Fatalf("concurrent close calls = %s, want %s", got, want)
	}
}

func TestSQLiteStoreListAppliesResidualAssigneeFilterBeforeLimit(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	if _, err := store.Create(Bead{
		ID:        "gc-before",
		Title:     "unmatched first row",
		Assignee:  "other-worker",
		CreatedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatalf("Create unmatched bead: %v", err)
	}
	if _, err := store.Create(Bead{
		ID:        "gc-match",
		Title:     "matching second row",
		Assignee:  "target-worker",
		CreatedAt: time.Unix(2, 0).UTC(),
	}); err != nil {
		t.Fatalf("Create matching bead: %v", err)
	}

	rows, err := store.List(ListQuery{
		Assignees: []string{"target-worker"},
		Limit:     1,
		Sort:      SortCreatedAsc,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "gc-match" {
		t.Fatalf("List residual-assignee rows = %#v, want only gc-match", rows)
	}
}

// TestSQLiteStoreNoLeakOnDiscard is the goroutine-leak regression test for a
// discarded store. Opening N stores with the retention sweeper enabled and
// calling CloseStore on each must keep the goroutine count at ~baseline.
// Without CloseStore the count would grow by >=1 goroutine per store per tick.
func TestSQLiteStoreNoLeakOnDiscard(t *testing.T) {
	const n = 25

	// settleBelow yields until the goroutine count drops to at most target
	// (CloseStore joins the sweeper synchronously; only database/sql's
	// internal closer goroutines need a beat), bounded so a real leak still
	// fails. No fixed sleep — the fixed_sleep census ratchet is hard.
	settleBelow := func(target int) int {
		n := runtime.NumGoroutine()
		for i := 0; i < 200_000 && n > target; i++ {
			runtime.Gosched()
			if i%1000 == 0 {
				runtime.GC()
			}
			n = runtime.NumGoroutine()
		}
		return n
	}

	settleBelow(0)
	base := runtime.NumGoroutine()

	for i := 0; i < n; i++ {
		s, err := OpenSQLiteStore(t.TempDir(),
			WithSQLiteStoreRetention(4*time.Hour, 30*time.Second))
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		closer, ok := s.(interface{ CloseStore() error })
		if !ok {
			t.Fatalf("SQLiteStore does not implement CloseStore() error")
		}
		if err := closer.CloseStore(); err != nil {
			t.Fatalf("CloseStore %d: %v", i, err)
		}
	}

	residual := settleBelow(base+5) - base
	t.Logf("goroutines: base=%d after=%d residual=%d (opened+closed %d stores)",
		base, base+residual, residual, n)

	if residual > 5 {
		t.Fatalf("SQLiteStore CloseStore did not release resources: residual goroutines=%d after %d open+close cycles (want <=5)", residual, n)
	}
}

func sqliteReadyIDSet(rows []Bead) map[string]bool {
	ids := make(map[string]bool, len(rows))
	for _, row := range rows {
		ids[row.ID] = true
	}
	return ids
}

func TestIsSQLiteBusy(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("some other error"), false},
		{errors.New("database is locked (5) (SQLITE_BUSY)"), true},
		{errors.New("SQLITE_BUSY (5)"), true},
		{errors.New("database is locked"), true},
		{fmt.Errorf("sqlite update: begin tx: %w", errors.New("database is locked (5) (SQLITE_BUSY)")), true},
	}
	for _, tc := range cases {
		if got := isSQLiteBusy(tc.err); got != tc.want {
			t.Errorf("isSQLiteBusy(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRetryOnBusy(t *testing.T) {
	t.Run("succeeds_immediately", func(t *testing.T) {
		calls := 0
		err := retryOnBusy(func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 1 {
			t.Fatalf("expected 1 call, got %d", calls)
		}
	})

	t.Run("retries_on_busy_then_succeeds", func(t *testing.T) {
		calls := 0
		busyErr := errors.New("database is locked (5) (SQLITE_BUSY)")
		err := retryOnBusy(func() error {
			calls++
			if calls < 3 {
				return busyErr
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 3 {
			t.Fatalf("expected 3 calls, got %d", calls)
		}
	})

	t.Run("exhausts_retries_and_returns_busy_error", func(t *testing.T) {
		calls := 0
		busyErr := errors.New("database is locked (5) (SQLITE_BUSY)")
		err := retryOnBusy(func() error {
			calls++
			return busyErr
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if calls != 1+sqliteBusyRetryAttempts {
			t.Fatalf("expected %d calls, got %d", 1+sqliteBusyRetryAttempts, calls)
		}
	})

	t.Run("does_not_retry_non_busy_error", func(t *testing.T) {
		calls := 0
		err := retryOnBusy(func() error {
			calls++
			return errors.New("something else went wrong")
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if calls != 1 {
			t.Fatalf("expected 1 call, got %d", calls)
		}
	})
}

func closeSQLiteTestStore(t *testing.T, s Store) {
	t.Helper()
	if c, ok := s.(interface{ CloseStore() error }); ok {
		c.CloseStore() //nolint:errcheck
	}
}

func TestSQLiteStoreReadOnlyReadsWithoutMutating(t *testing.T) {
	dir := t.TempDir()
	// Seed a source db through the normal read-write path.
	rw, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	seeded, err := rw.Create(Bead{ID: "gcg-42", Title: "src", Type: "session", Status: "open"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	closeSQLiteTestStore(t, rw)

	ro, err := OpenSQLiteStore(dir, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	defer closeSQLiteTestStore(t, ro)

	got, err := ro.Get(seeded.ID)
	if err != nil {
		t.Fatalf("ro get: %v", err)
	}
	if got.Title != "src" {
		t.Fatalf("ro get title = %q, want src", got.Title)
	}
	rows, err := ro.List(ListQuery{IncludeClosed: true, TierMode: TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("ro list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ro list = %d rows, want 1", len(rows))
	}

	// Writes must be rejected by the driver, never mutate the source.
	if _, err := ro.Create(Bead{Title: "nope", Type: "task"}); err == nil {
		t.Fatal("read-only Create unexpectedly succeeded")
	}
}

func fileSHA256(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestSQLiteStoreReadOnlyLeavesSourceByteIdenticalWithLiveWAL reproduces the
// exact scenario the query_only(1) form failed: a read-only open over a source
// db that carries a POPULATED, un-checkpointed -wal (a stopped writer's crash
// state), as the SOLE connection. A mode=ro open must read the WAL-resident
// rows AND leave both the main db file and the -wal byte-identical across
// open/read/close — a query_only connection instead auto-checkpoints on close,
// rewriting the main db and deleting the -wal (mutating the migration source).
func TestSQLiteStoreReadOnlyLeavesSourceByteIdenticalWithLiveWAL(t *testing.T) {
	// Seed a checkpointed row through the store, then a second row via a raw
	// writer with autocheckpoint disabled so it stays WAL-resident.
	seed := t.TempDir()
	rw, err := OpenSQLiteStore(seed, WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := rw.Create(Bead{ID: "gcg-1", Type: "session", Title: "checkpointed", Status: "open"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	closeSQLiteTestStore(t, rw)

	raw, err := sql.Open("sqlite", filepath.Join(seed, "beads.sqlite")+"?_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatalf("open raw writer: %v", err)
	}
	raw.SetMaxOpenConns(1)
	beadJSON := `{"id":"gcg-2","issue_type":"session","status":"open","title":"wal-resident"}`
	if _, err := raw.Exec(
		"INSERT INTO beads(id,tier,title,status,issue_type,created_at,updated_at,bead_json) VALUES('gcg-2','main','wal-resident','open','session',1,1,?)",
		beadJSON); err != nil {
		t.Fatalf("wal-resident insert: %v", err)
	}
	// Copy main + -wal out from under the still-open writer so the copy carries a
	// live, un-checkpointed WAL but has NO holding connection (the stopped-writer
	// crash state), then release the writer.
	src := t.TempDir()
	for _, suffix := range []string{"", "-wal"} {
		b, rerr := os.ReadFile(filepath.Join(seed, "beads.sqlite"+suffix))
		if rerr != nil {
			t.Fatalf("read seed%s: %v", suffix, rerr)
		}
		if werr := os.WriteFile(filepath.Join(src, "beads.sqlite"+suffix), b, 0o644); werr != nil {
			t.Fatalf("write src%s: %v", suffix, werr)
		}
	}
	_ = raw.Close()

	mainPath := filepath.Join(src, "beads.sqlite")
	walPath := mainPath + "-wal"
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("precondition: source has no live -wal: %v", err)
	}
	mainBefore, walBefore := fileSHA256(t, mainPath), fileSHA256(t, walPath)

	ro, err := OpenSQLiteStore(src, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	rows, err := ro.List(ListQuery{IncludeClosed: true, TierMode: TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("ro list: %v", err)
	}
	closeSQLiteTestStore(t, ro)

	// The read must see BOTH the checkpointed and the WAL-resident row.
	if len(rows) != 2 {
		t.Fatalf("ro list = %d rows, want 2 (incl the WAL-resident row)", len(rows))
	}
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("read-only open DELETED the source -wal (checkpoint-on-close): %v", err)
	}
	if got := fileSHA256(t, mainPath); got != mainBefore {
		t.Fatalf("read-only open MUTATED the source main db (checkpoint-on-close): %s != %s", got, mainBefore)
	}
	if got := fileSHA256(t, walPath); got != walBefore {
		t.Fatalf("read-only open MUTATED the source -wal: %s != %s", got, walBefore)
	}
}

// TestSQLiteStoreReadOnlyReadsLegacySchemaWithoutRevision reproduces the
// combined infra store written before optimistic-concurrency revisions landed.
// Migration opens this source read-only, so it must project a zero revision
// without altering the legacy table or checkpointing its live WAL.
func TestSQLiteStoreReadOnlyReadsLegacySchemaWithoutRevision(t *testing.T) {
	seed := t.TempDir()
	seedPath := filepath.Join(seed, sqliteStoreFilename)
	createSQLiteSchemaFixture(t, seed, false, true, nil)
	raw, err := sql.Open("sqlite", seedPath+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatalf("open legacy writer: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = raw.Close()
		t.Fatalf("checkpoint legacy schema: %v", err)
	}
	beadJSON := `{"id":"gcg-42","title":"legacy","status":"open","issue_type":"task"}`
	if _, err := raw.Exec(
		`INSERT INTO beads(
			id,tier,title,status,issue_type,created_at,updated_at,bead_json
		) VALUES('gcg-42','main','legacy','open','task',1,1,?)`,
		beadJSON,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("insert legacy bead: %v", err)
	}

	// Copy while the writer is open so the fixture retains the uncheckpointed
	// row in its WAL, matching a stopped combined infra store.
	src := t.TempDir()
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(seedPath + suffix)
		if err != nil {
			_ = raw.Close()
			t.Fatalf("read legacy source%s: %v", suffix, err)
		}
		if err := os.WriteFile(filepath.Join(src, sqliteStoreFilename+suffix), b, 0o644); err != nil {
			_ = raw.Close()
			t.Fatalf("write legacy fixture%s: %v", suffix, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy writer: %v", err)
	}

	mainPath := filepath.Join(src, sqliteStoreFilename)
	walPath := mainPath + "-wal"
	mainBefore, walBefore := fileSHA256(t, mainPath), fileSHA256(t, walPath)
	opened, err := OpenSQLiteStore(src, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("open legacy source read-only: %v", err)
	}
	ro := opened.(*SQLiteStore)
	rows, err := ro.List(ListQuery{IncludeClosed: true, TierMode: TierBoth, AllowScan: true})
	if err != nil {
		_ = ro.CloseStore()
		t.Fatalf("list legacy source: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "gcg-42" || rows[0].Revision != 0 {
		_ = ro.CloseStore()
		t.Fatalf("legacy list = %#v, want gcg-42 at revision zero", rows)
	}
	got, err := ro.Get("gcg-42")
	if err != nil {
		_ = ro.CloseStore()
		t.Fatalf("get legacy bead: %v", err)
	}
	if got.Revision != 0 {
		_ = ro.CloseStore()
		t.Fatalf("legacy get revision = %d, want 0", got.Revision)
	}
	ready, err := ro.Ready()
	if err != nil {
		_ = ro.CloseStore()
		t.Fatalf("ready legacy beads: %v", err)
	}
	if len(ready) != 1 || ready[0].Revision != 0 {
		_ = ro.CloseStore()
		t.Fatalf("legacy ready = %#v, want one bead at revision zero", ready)
	}
	if err := ro.CloseStore(); err != nil {
		t.Fatalf("close legacy source: %v", err)
	}
	if got := fileSHA256(t, mainPath); got != mainBefore {
		t.Fatalf("legacy read-only open mutated source main db: %s != %s", got, mainBefore)
	}
	if got := fileSHA256(t, walPath); got != walBefore {
		t.Fatalf("legacy read-only open mutated source WAL: %s != %s", got, walBefore)
	}
}

func TestSQLiteStoreReadOnlyMissingFileErrors(t *testing.T) {
	// A read-only open never creates the file or its parent directory.
	dir := filepath.Join(t.TempDir(), "absent")
	if _, err := OpenSQLiteStore(dir, WithSQLiteStoreReadOnly()); err == nil {
		t.Fatal("expected error opening a nonexistent read-only store")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("read-only open created the directory: stat err = %v", statErr)
	}
}
