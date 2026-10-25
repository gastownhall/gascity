package main

import (
	"errors"
	"strings"
	"testing"
)

// The data-read probe exists because dolt-health and beads-health were green 60s
// before a total outage (ga-vrq5pu): every probe touched only the catalog or a
// table the probe itself had just written, so a dolt-ignored table whose prolly
// tree had lost chunks (wisps, events, ...) was never read. These tests pin the
// probe to a row read that has to fetch a leaf node of a pre-existing
// dolt-ignored table.

const (
	testEmptyChunkErr  = "empty chunk returned from ChunkStore"
	testMissingChunkEr = "chunk 4fd8ukqm3ln2t9odk0i2in3t2gnvi3db required but not found"
)

// dataReadOps builds a managedDoltDataReadOps over a fixed table list and a
// per-database read function; batches records every read the probe issued.
func dataReadOps(tables []managedDoltTableRef, read func(db string, stmts []string) error, batches *[][]string) managedDoltDataReadOps {
	return managedDoltDataReadOps{
		listTables: func() ([]managedDoltTableRef, error) { return tables, nil },
		readBatch: func(db string, stmts []string) error {
			if batches != nil {
				*batches = append(*batches, append([]string{db}, stmts...))
			}
			return read(db, stmts)
		},
	}
}

func TestManagedDoltIgnoredTableDiscoverySQLCoversEveryIgnoredPattern(t *testing.T) {
	sql := managedDoltIgnoredTableDiscoverySQL()
	for _, name := range []string{
		"wisps", "local_metadata", "leases", "events", "bd_events_journal",
		"bd_events_seq", "repo_mtimes", "ignored_schema_migrations",
	} {
		if !strings.Contains(sql, "'"+name+"'") {
			t.Errorf("discovery SQL does not name ignored table %q: %s", name, sql)
		}
	}
	// wisp_% is a LIKE pattern, so its underscore must be escaped to stay literal.
	if !strings.Contains(sql, `wisp\\_%`) {
		t.Errorf("discovery SQL does not cover the wisp_%% family with an escaped underscore: %s", sql)
	}
	if !strings.Contains(strings.ToLower(sql), "information_schema.tables") {
		t.Errorf("discovery must read the catalog, not guess table names: %s", sql)
	}
	// The probe's own table is written by the read-only probe; reading it proves nothing.
	if strings.Contains(sql, managedDoltProbeTable) {
		t.Errorf("discovery must not target the probe's own table %q: %s", managedDoltProbeTable, sql)
	}
}

func TestManagedDoltLeafReadStatementsForForceALeafFetch(t *testing.T) {
	stmts := managedDoltLeafReadStatementsFor("my-db", []string{"wisps", "wisp_labels"})
	if len(stmts) != 2 {
		t.Fatalf("statements = %v, want one per table", stmts)
	}
	for i, table := range []string{"wisps", "wisp_labels"} {
		stmt := stmts[i]
		upper := strings.ToUpper(stmt)
		if !strings.HasPrefix(upper, "SELECT * FROM ") {
			t.Errorf("stmt %q must read row data (SELECT *), not an aggregate", stmt)
		}
		if !strings.Contains(upper, "LIMIT 1") {
			t.Errorf("stmt %q must be bounded to one row", stmt)
		}
		if !strings.Contains(stmt, "`my-db`.`"+table+"`") {
			t.Errorf("stmt %q must qualify and quote db.table", stmt)
		}
		// COUNT(*) is answerable from the root's cardinality without fetching a
		// leaf, which is exactly the blind spot being closed.
		for _, banned := range []string{"COUNT(", "INFORMATION_SCHEMA", "CREATE ", "REPLACE ", "INSERT ", "DROP "} {
			if strings.Contains(upper, banned) {
				t.Errorf("stmt %q contains %q: not a leaf-fetching read of an existing table", stmt, banned)
			}
		}
	}
}

func TestManagedDoltDataReadProbeHealthyWhenReadsSucceed(t *testing.T) {
	var batches [][]string
	ops := dataReadOps([]managedDoltTableRef{
		{DB: "gascity", Table: "wisps"}, {DB: "gascity", Table: "events"}, {DB: "gm", Table: "wisps"},
	}, func(string, []string) error { return nil }, &batches)

	got := managedDoltDataReadProbeWith(ops)
	if got != (managedDoltDataReadResult{}) {
		t.Fatalf("probe = %+v, want healthy", got)
	}
	// One batch per database keeps the 30s patrol to a bounded number of
	// round trips no matter how many ignored tables a database carries.
	if len(batches) != 2 {
		t.Fatalf("batches = %v, want one per database", batches)
	}
	if batches[0][0] != "gascity" || len(batches[0]) != 3 {
		t.Fatalf("gascity batch = %v, want 2 statements", batches[0])
	}
}

func TestManagedDoltDataReadProbeNoIgnoredTablesIsHealthy(t *testing.T) {
	var batches [][]string
	got := managedDoltDataReadProbeWith(dataReadOps(nil, func(string, []string) error { return nil }, &batches))
	if got != (managedDoltDataReadResult{}) {
		t.Fatalf("probe = %+v, want healthy when no ignored table exists", got)
	}
	if len(batches) != 0 {
		t.Fatalf("issued reads %v with nothing to read", batches)
	}
}

func TestManagedDoltDataReadProbeAbsentTableIsHealthy(t *testing.T) {
	for _, msg := range []string{
		"table not found: wisps",
		"Error 1146 (HY000): table not found: wisps",
		"Table 'gascity.wisps' doesn't exist",
	} {
		ops := dataReadOps([]managedDoltTableRef{{DB: "gascity", Table: "wisps"}},
			func(string, []string) error { return errors.New(msg) }, nil)
		if got := managedDoltDataReadProbeWith(ops); got != (managedDoltDataReadResult{}) {
			t.Errorf("absent-table error %q: probe = %+v, want healthy", msg, got)
		}
	}
}

func TestManagedDoltDataReadProbePropagatesChunkErrorText(t *testing.T) {
	for _, msg := range []string{testEmptyChunkErr, testMissingChunkEr} {
		ops := dataReadOps([]managedDoltTableRef{{DB: "gascity", Table: "wisps"}},
			func(string, []string) error { return errors.New("Error 1105: " + msg) }, nil)
		got := managedDoltDataReadProbeWith(ops)
		if !strings.Contains(got.Err, msg) {
			t.Errorf("Err = %q, want the underlying text %q preserved", got.Err, msg)
		}
		if !strings.Contains(got.Err, "gascity") {
			t.Errorf("Err = %q, want the failing database named", got.Err)
		}
		if got.Warning != "" {
			t.Errorf("Warning = %q, a chunk error is a failure, not a warning", got.Warning)
		}
	}
}

func TestManagedDoltDataReadProbeChunkErrorBeatsEarlierInconclusiveError(t *testing.T) {
	ops := dataReadOps([]managedDoltTableRef{
		{DB: "aaa", Table: "wisps"}, {DB: "zzz", Table: "wisps"},
	}, func(db string, _ []string) error {
		if db == "aaa" {
			return errors.New("connection reset by peer")
		}
		return errors.New(testEmptyChunkErr)
	}, nil)
	got := managedDoltDataReadProbeWith(ops)
	if !strings.Contains(got.Err, testEmptyChunkErr) {
		t.Fatalf("Err = %q, want the later database's chunk error not masked by an earlier transient one", got.Err)
	}
}

func TestManagedDoltDataReadProbeOtherErrorsAreWarningsNotFailures(t *testing.T) {
	ops := dataReadOps([]managedDoltTableRef{{DB: "gascity", Table: "wisps"}},
		func(string, []string) error { return errors.New("timed out after 5s") }, nil)
	got := managedDoltDataReadProbeWith(ops)
	if got.Err != "" {
		t.Fatalf("Err = %q: an inconclusive read must not look like data loss (it would trigger a restart)", got.Err)
	}
	if !strings.Contains(got.Warning, "timed out after 5s") {
		t.Fatalf("Warning = %q, want the underlying error surfaced, not swallowed", got.Warning)
	}
}

func TestManagedDoltDataReadProbeDiscoveryFailureIsWarning(t *testing.T) {
	ops := managedDoltDataReadOps{
		listTables: func() ([]managedDoltTableRef, error) { return nil, errors.New("catalog unavailable") },
		readBatch:  func(string, []string) error { t.Fatal("read attempted after failed discovery"); return nil },
	}
	got := managedDoltDataReadProbeWith(ops)
	if got.Err != "" || !strings.Contains(got.Warning, "catalog unavailable") {
		t.Fatalf("probe = %+v, want a warning carrying the discovery error", got)
	}
}

func TestManagedDoltDataReadProbeSkipsSystemDatabases(t *testing.T) {
	var batches [][]string
	ops := dataReadOps([]managedDoltTableRef{
		{DB: "mysql", Table: "events"}, {DB: "information_schema", Table: "events"}, {DB: "gm", Table: "events"},
	}, func(string, []string) error { return nil }, &batches)
	managedDoltDataReadProbeWith(ops)
	if len(batches) != 1 || batches[0][0] != "gm" {
		t.Fatalf("batches = %v, want only the user database", batches)
	}
}

// A chunk-level data loss must not make managedDoltHealthCheck return an error:
// dolt_recover_managed.go treats a health-check error as a failed recovery and
// terminates the server, which would turn a diagnosable corruption into an
// outage. It is reported through the report instead.
func TestManagedDoltHealthCheckReportsDataReadErrorWithoutFailingTheCheck(t *testing.T) {
	old := managedDoltDataReadProbeFn
	defer func() { managedDoltDataReadProbeFn = old }()
	managedDoltDataReadProbeFn = func(_, _, _ string) managedDoltDataReadResult {
		return managedDoltDataReadResult{Err: "gascity: " + testEmptyChunkErr}
	}
	stubManagedDoltHealthyServer(t)

	report, err := managedDoltHealthCheck("127.0.0.1", "3311", "root", false)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v, want nil (a returned error makes recovery kill the server)", err)
	}
	if !report.QueryReady {
		t.Fatalf("QueryReady = false; the server answers queries, only data is unreadable")
	}
	if !strings.Contains(report.DataReadError, testEmptyChunkErr) {
		t.Fatalf("DataReadError = %q, want the chunk error text", report.DataReadError)
	}
}

func TestManagedDoltHealthCheckReportsDataReadWarning(t *testing.T) {
	old := managedDoltDataReadProbeFn
	defer func() { managedDoltDataReadProbeFn = old }()
	managedDoltDataReadProbeFn = func(_, _, _ string) managedDoltDataReadResult {
		return managedDoltDataReadResult{Warning: "gascity: timed out after 5s"}
	}
	stubManagedDoltHealthyServer(t)

	report, err := managedDoltHealthCheck("127.0.0.1", "3311", "root", false)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v", err)
	}
	if report.DataReadError != "" || !strings.Contains(report.DataReadWarning, "timed out") {
		t.Fatalf("report = %+v, want only DataReadWarning set", report)
	}
}

func TestManagedDoltHealthCheckFieldsEmitDataReadFindingsOnlyWhenPresent(t *testing.T) {
	clean := managedDoltHealthCheckFields(managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false", ConnectionCount: "3"})
	for _, line := range clean {
		if strings.HasPrefix(line, "data_read") {
			t.Fatalf("clean report emitted %q; existing consumers parse exact field sets", line)
		}
	}
	got := managedDoltHealthCheckFields(managedDoltSQLHealthReport{
		QueryReady: true, ReadOnly: "false", ConnectionCount: "3",
		DataReadError:   "gascity: " + testEmptyChunkErr + "\nsecond line",
		DataReadWarning: "gm: timed out",
	})
	joined := strings.Join(got, "\n")
	// Field values are single-line so the tab/newline-separated protocol cannot be
	// forged or truncated by a multi-line driver message.
	if !strings.Contains(joined, "data_read_error\tgascity: "+testEmptyChunkErr+" second line") {
		t.Errorf("fields = %q, want a single-line data_read_error", got)
	}
	if !strings.Contains(joined, "data_read_warning\tgm: timed out") {
		t.Errorf("fields = %q, want data_read_warning", got)
	}
}

// stubManagedDoltHealthyServer makes the catalog, read-only and connection-count
// probes of managedDoltHealthCheck succeed without a server.
func stubManagedDoltHealthyServer(t *testing.T) {
	t.Helper()
	t.Setenv("GC_DOLT_PASSWORD", "stub")
	oldQuery, oldReadOnly, oldConn := managedDoltQueryProbeDirectFn, managedDoltReadOnlyStateDirectFn, managedDoltConnectionCountDirectFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn, managedDoltReadOnlyStateDirectFn, managedDoltConnectionCountDirectFn = oldQuery, oldReadOnly, oldConn
	})
	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) { return "false", nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "1", nil }
}
