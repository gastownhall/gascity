package beads

import (
	"context"
	"database/sql"
	"net/url"
	"slices"
	"testing"
)

func dsnPragmas(t *testing.T, dsn string) []string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing DSN %q: %v", dsn, err)
	}
	return parsed.Query()["_pragma"]
}

// The read-path pragmas touch only process memory, so every mode carries them.
var sqliteReadPathPragmas = []string{
	"cache_size(-64000)",
	"mmap_size(268435456)",
	"temp_store(MEMORY)",
}

func TestSQLiteStoreDSNCarriesTuningPragmas(t *testing.T) {
	cases := []struct {
		name            string
		dsn             string
		wantMode        string
		wantSynchronous bool
	}{
		{name: "read-write", dsn: sqliteStoreDSN("/city/.gc/store/graph/beads.sqlite", false), wantSynchronous: true},
		{name: "read-only", dsn: sqliteStoreDSN("/city/.gc/store/graph/beads.sqlite", true), wantMode: "ro"},
		{name: "private recovery", dsn: sqliteStorePrivateRecoveryDSN("/snapshot/.beads/beads.sqlite"), wantMode: "rw", wantSynchronous: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pragmas := dsnPragmas(t, tc.dsn)
			want := append([]string{"busy_timeout(5000)", "foreign_keys(1)"}, sqliteReadPathPragmas...)
			if tc.wantSynchronous {
				want = append(want, "synchronous(NORMAL)")
			}
			for _, pragma := range want {
				if !slices.Contains(pragmas, pragma) {
					t.Errorf("DSN %s is missing _pragma=%s (got %v)", tc.dsn, pragma, pragmas)
				}
			}
			// synchronous is a write knob; a mode=ro connection cannot write or
			// checkpoint, so the read-only DSN must not carry it.
			if !tc.wantSynchronous && slices.Contains(pragmas, "synchronous(NORMAL)") {
				t.Errorf("read-only DSN %s relaxes synchronous (got %v)", tc.dsn, pragmas)
			}
			parsed, err := url.Parse(tc.dsn)
			if err != nil {
				t.Fatalf("parsing DSN %q: %v", tc.dsn, err)
			}
			if got := parsed.Query().Get("mode"); got != tc.wantMode {
				t.Errorf("DSN mode = %q, want %q", got, tc.wantMode)
			}
		})
	}
}

func sqlitePragmaValue(t *testing.T, conn *sql.Conn, pragma string) string {
	t.Helper()
	var value string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&value); err != nil {
		t.Fatalf("reading PRAGMA %s: %v", pragma, err)
	}
	return value
}

// checkoutAllConns holds every connection the pool will hand out at once, so
// the assertions below cover each one rather than whichever the pool reuses.
// That is the whole reason the pragmas ride the DSN instead of a post-open Exec.
func checkoutAllConns(t *testing.T, db *sql.DB, n int) []*sql.Conn {
	t.Helper()
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("checking out connection %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return conns
}

func assertTuningPragmas(t *testing.T, conns []*sql.Conn, wantSynchronous string) {
	t.Helper()
	for i, conn := range conns {
		for pragma, want := range map[string]string{
			"cache_size":   "-64000",
			"mmap_size":    "268435456",
			"temp_store":   "2", // MEMORY
			"synchronous":  wantSynchronous,
			"busy_timeout": "5000",
			"foreign_keys": "1",
		} {
			if got := sqlitePragmaValue(t, conn, pragma); got != want {
				t.Errorf("connection %d: PRAGMA %s = %s, want %s", i, pragma, got, want)
			}
		}
	}
}

func TestSQLiteStoreConnectionsApplyTuningPragmas(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	// The write connection is created by applySchema on a brand-new database,
	// so this also guards the creation pragmas against drifting back to FULL
	// and silently overriding the DSN for the process's only writer.
	t.Run("write connection", func(t *testing.T) {
		assertTuningPragmas(t, checkoutAllConns(t, store.db, 1), "1")
	})
	t.Run("read pool", func(t *testing.T) {
		assertTuningPragmas(t, checkoutAllConns(t, store.readDB, 8), "1")
	})
}

func TestSQLiteStoreReadOnlyConnectionsKeepFullSynchronous(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("OpenSQLiteStore writer: %v", err)
	}
	if err := opened.(*SQLiteStore).CloseStore(); err != nil {
		t.Fatalf("CloseStore writer: %v", err)
	}

	opened, err = OpenSQLiteStore(dir, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("OpenSQLiteStore read-only: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	if _, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true}); err != nil {
		t.Fatalf("List through tuned read-only store: %v", err)
	}

	// The read-path pragmas still apply; only synchronous stays at the SQLite
	// default, because nothing on a mode=ro connection can write. Checking out
	// the whole pool has to come last: it leaves no connection for List.
	assertTuningPragmas(t, checkoutAllConns(t, store.readDB, 8), "2")
}
