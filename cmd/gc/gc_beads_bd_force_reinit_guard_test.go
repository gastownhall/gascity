package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Exit codes of bd_runtime_store_holds_bd_tables. Named here so the table below
// reads as the three-way answer it is rather than as bare integers.
const (
	bdTablesPresent = 0
	bdTablesAbsent  = 1
	bdTablesUnknown = 2
)

// TestStoreHoldsBdTablesDistinguishesEmptyFromUndetermined pins the guard that
// decides whether op_init may answer a negative bd_runtime_schema_ready probe
// with a DESTRUCTIVE `bd init --force`.
//
// The probe swallows every error, so "the bd schema is absent" and "the server
// hiccuped mid-query" arrive at the call site as the same false. server_reachable
// narrows that only to the case where the whole server is down: it runs earlier,
// on a different connection, and without a database context, so a blip during
// the schema probe itself still reads as a missing schema. Forcing a reinit
// there re-runs bd's migrations over a working set that already holds
// uncommitted rows, beads refuses to migrate a dirty table
// (gastownhall/beads#4566), and city init dies with a bare "bd init failed"
// naming neither the database nor the reason.
//
// The three-way answer is the point. "Absent" authorizes the reinit and is the
// ordinary fresh-init path, "present" forbids it, and "undetermined" is neither.
// Folding undetermined into present would refuse to initialize a fresh city
// whenever the count query is unavailable; folding it into absent would restore
// the destructive guess this guard exists to stop.
func TestStoreHoldsBdTablesDistinguishesEmptyFromUndetermined(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	src := readGCBeadsBdScript(t)
	validSQLName := extractShellFunction(t, src, "valid_sql_name")
	tableCount := extractShellFunction(t, src, "bd_runtime_bd_table_count")
	schemaCursor := extractShellFunction(t, src, "bd_runtime_schema_cursor")
	holdsTables := extractShellFunction(t, src, "bd_runtime_store_holds_bd_tables")

	cases := []struct {
		name     string
		stdout   string
		exitCode int
		want     int
		why      string
	}{
		{
			name:     "empty_database_authorizes_reinit",
			stdout:   "cnt\n0\n",
			exitCode: 0,
			want:     bdTablesAbsent,
			why:      "no bd tables is the fresh store this branch serves, so reinit only creates schema and must not be delayed",
		},
		{
			name:     "populated_database_forbids_reinit",
			stdout:   "cnt\n3\n",
			exitCode: 0,
			want:     bdTablesPresent,
			why:      "bd tables exist, so the negative schema probe contradicts the database and a forced reinit would migrate over live rows",
		},
		{
			name:     "unanswered_query_is_undetermined",
			stdout:   "",
			exitCode: 1,
			want:     bdTablesUnknown,
			why:      "a query that did not answer is evidence of nothing, and must read as neither an empty nor a populated store",
		},
		{
			name:     "empty_output_with_success_is_undetermined",
			stdout:   "",
			exitCode: 0,
			want:     bdTablesUnknown,
			why:      "a server or stub that exits 0 without printing a count has still told us nothing",
		},
		{
			name:     "unparseable_count_is_undetermined",
			stdout:   "cnt\nnot-a-number\n",
			exitCode: 0,
			want:     bdTablesUnknown,
			why:      "a count that is not a number says nothing about what the reinit would destroy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			writeFakeCountDolt(t, binDir, tc.stdout, tc.exitCode)

			// connect_host is overridden so the test exercises only the
			// safety decision, not host resolution.
			script := "connect_host() { printf '127.0.0.1'; }\n" +
				validSQLName + "\n" +
				tableCount + "\n" +
				schemaCursor + "\n" +
				holdsTables + "\n" +
				"bd_runtime_store_holds_bd_tables hq\n"

			got := exitCodeOf(t, runGCBeadsBdSnippet(t, script, binDir))
			if got != tc.want {
				t.Fatalf("bd_runtime_store_holds_bd_tables = %d, want %d (fake dolt stdout=%q exit=%d): %s",
					got, tc.want, tc.stdout, tc.exitCode, tc.why)
			}
		})
	}
}

// TestBdRuntimeBdTableCountRejectsUnsafeDatabaseNames keeps the count query's
// database argument on the same allowlist the rest of the script interpolates
// under. The name reaches a SQL string directly, so one that valid_sql_name
// would reject must never get that far.
func TestBdRuntimeBdTableCountRejectsUnsafeDatabaseNames(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	src := readGCBeadsBdScript(t)
	validSQLName := extractShellFunction(t, src, "valid_sql_name")
	tableCount := extractShellFunction(t, src, "bd_runtime_bd_table_count")

	cases := []struct {
		name string
		db   string
	}{
		{"empty", ""},
		{"statement_separator", "hq; DROP DATABASE hq"},
		{"single_quote", "hq'"},
		{"backtick", "hq`"},
		{"whitespace", "hq hq"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			// A clean count and exit 0: were the name guard missing, the call
			// would succeed and this test would catch it.
			writeFakeCountDolt(t, binDir, "cnt\n0\n", 0)

			script := "connect_host() { printf '127.0.0.1'; }\n" +
				validSQLName + "\n" +
				tableCount + "\n" +
				"bd_runtime_bd_table_count \"$1\"\n"

			if err := runGCBeadsBdSnippet(t, script, binDir, tc.db); err == nil {
				t.Fatalf("bd_runtime_bd_table_count accepted unsafe database name %q", tc.db)
			}
		})
	}
}

// readGCBeadsBdScript returns the provider script's source.
func readGCBeadsBdScript(t *testing.T) string {
	t.Helper()
	scriptPath := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return string(scriptBytes)
}

// runGCBeadsBdSnippet runs extracted shell functions with binDir first on PATH,
// passing args as $1, $2, … and returning the snippet's exit status as an error.
func runGCBeadsBdSnippet(t *testing.T, script, binDir string, args ...string) error {
	t.Helper()
	_, _, err := runGCBeadsBdCommand(t, append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOLT_PORT=42188",
		"DOLT_USER=root",
		"DOLT_PASSWORD=",
	), "bash", append([]string{"-c", script, "bash"}, args...)...)
	return err
}

// exitCodeOf turns runGCBeadsBdSnippet's error back into the shell exit status,
// failing the test on an error that carries no status (the snippet never ran).
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("running shell snippet: %v", err)
	return -1
}

// writeFakeCountDolt installs a dolt stub that prints the given CSV on stdout
// and exits with the given code, standing in for the count query's server. The
// payload goes through a file so no shell quoting of the CSV is needed.
func writeFakeCountDolt(t *testing.T, dir, stdout string, exitCode int) {
	t.Helper()
	payload := filepath.Join(dir, "count.csv")
	if err := os.WriteFile(payload, []byte(stdout), 0o600); err != nil {
		t.Fatalf("write fake dolt payload: %v", err)
	}
	body := "#!/bin/sh\ncat '" + payload + "'\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// TestGcBeadsBdInitRefusesForcedReinitWhenDatabaseHoldsBdTables drives op_init
// itself rather than the helper underneath it. The table above pins what
// bd_runtime_store_holds_bd_tables answers; this pins what op_init does with a
// "present", which is where the answer either prevents a destructive reinit or
// does nothing at all.
//
// The scenario is the one from the failing job: a database holding bd tables
// whose schema probe keeps coming back negative. op_init used to answer that
// with `bd init --force`, beads then refused to migrate the dirty tables the
// reinit had to touch (gastownhall/beads#4566), and city init died naming
// neither the database nor the cause. Three things are asserted, and the last
// is the one carrying the data-safety property: init stops, it says which
// database and why, and bd init never ran. A refusal message on its own would
// not have saved the store.
//
// Neither existing force-reinit test covers this branch. Their fake dolt logs
// the count query and answers nothing on stdout, so both land on "undetermined"
// and proceed to the reinit, which leaves "present" wired to the refusal by
// nothing but inspection.
func TestGcBeadsBdInitRefusesForcedReinitWhenDatabaseHoldsBdTables(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// bd init exits 0 rather than failing, so a regression surfaces as this
	// test's own assertion rather than as an unrelated downstream error.
	initMarker := filepath.Join(t.TempDir(), "bd-init-ran")
	fakeBd := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" = "init" ]; then
  printf '%%s\n' "$@" > %q
fi
exit 0
`, initMarker)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(fakeBd), 0o755); err != nil {
		t.Fatal(err)
	}

	// The count says four bd tables are present; the schema probe never
	// succeeds. That pair is the contradiction the guard exists to notice.
	fakeDolt := `#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  *information_schema.tables*)
    printf 'cnt\n4\n'
    exit 0
    ;;
  *"FROM config"*)
    echo "table not found: config" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(fakeDolt), 0o755); err != nil {
		t.Fatal(err)
	}

	// sleep_ms shells out to sleep, so stubbing it spends the retry budget at
	// no wall-clock cost.
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runGCBeadsBdCommand(t, sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...), script, "init", cityPath, "gc", "hq")
	// The refusal is written to stderr and the progress lines to stdout; the
	// assertions below are all substring checks, so reading them as one body
	// keeps this independent of how the two streams interleave.
	out := stdout + stderr
	if err == nil {
		t.Fatalf("init should refuse to force-reinitialize a database holding bd tables, but it succeeded:\n%s", out)
	}

	got := out
	for _, want := range []string{
		"holds bd tables but its bd schema stayed unreadable across retries",
		"refusing to force-reinitialize",
		"'hq'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("refusal is not self-describing, missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "missing bd schema; re-initializing") {
		t.Fatalf("init fell through to the destructive reinit instead of refusing:\n%s", got)
	}
	if _, statErr := os.Stat(initMarker); statErr == nil {
		argv, _ := os.ReadFile(initMarker)
		t.Fatalf("bd init ran despite the refusal, so the guard reported without preventing:\nargv:\n%s\noutput:\n%s", argv, got)
	}
}

// TestStoreHoldsBdTablesConsidersMigrationCursor extends the guard above to
// the bug that let a mid-migration database slip through as "empty": the
// four bd tables genuinely are absent while a concurrent `bd init` is still
// running its own migrations, but schema_migrations already carries a
// non-zero cursor. bd_runtime_store_holds_bd_tables must read that cursor
// and answer "present" (0) rather than "absent" (1), because op_init treats
// 1 as license to force a reinit immediately, without even trying
// wait_for_bd_runtime_schema first — exactly the fast path that raced the
// concurrent initializer in the failing job (a random vNN database force-
// reinits and beads then refuses to migrate the dirty tables).
//
// writeFakeCountDolt cannot express this: it returns one canned response no
// matter which query is sent, so a case needing the table count and the
// migration cursor to disagree needs a fake that dispatches on the SQL
// text. writeFakeSchemaCursorDolt below is that sibling; it does not
// replace writeFakeCountDolt, and the existing cases above stay independent
// of query order exactly as before.
func TestStoreHoldsBdTablesConsidersMigrationCursor(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	src := readGCBeadsBdScript(t)
	validSQLName := extractShellFunction(t, src, "valid_sql_name")
	tableCount := extractShellFunction(t, src, "bd_runtime_bd_table_count")
	schemaCursor := extractShellFunction(t, src, "bd_runtime_schema_cursor")
	holdsTables := extractShellFunction(t, src, "bd_runtime_store_holds_bd_tables")

	cases := []struct {
		name           string
		tableCountCSV  string
		tableCountExit int
		migExistsCSV   string
		migExistsExit  int
		cursorCSV      string
		cursorExit     int
		want           int
		why            string
	}{
		{
			name:           "genuinely_fresh_store_still_authorizes_reinit",
			tableCountCSV:  "cnt\n0\n",
			tableCountExit: 0,
			migExistsCSV:   "cnt\n0\n",
			migExistsExit:  0,
			cursorCSV:      "cur\n0\n",
			cursorExit:     0,
			want:           bdTablesAbsent,
			why:            "no bd tables and no migration history at all is the ordinary fresh-init path and must not gain latency or a different answer from the new cursor check",
		},
		{
			name:           "mid_migration_store_counts_as_present",
			tableCountCSV:  "cnt\n0\n",
			tableCountExit: 0,
			migExistsCSV:   "cnt\n1\n",
			migExistsExit:  0,
			cursorCSV:      "cur\n6\n",
			cursorExit:     0,
			want:           bdTablesPresent,
			why:            "a concurrent initializer's migration has advanced the cursor before creating the four bd tables; reading this as empty is the exact bug (ga-e2z1zb) that force-reinits a mid-migration database",
		},
		{
			name:           "populated_store_is_present_regardless_of_cursor",
			tableCountCSV:  "cnt\n3\n",
			tableCountExit: 0,
			migExistsCSV:   "cnt\n1\n",
			migExistsExit:  0,
			cursorCSV:      "cur\n0\n",
			cursorExit:     0,
			want:           bdTablesPresent,
			why:            "the four-table count alone already proves the store is populated; the cursor is irrelevant here and must not flip this to absent",
		},
		{
			name:           "table_count_query_failure_is_undetermined",
			tableCountCSV:  "",
			tableCountExit: 1,
			migExistsCSV:   "cnt\n0\n",
			migExistsExit:  0,
			cursorCSV:      "cur\n0\n",
			cursorExit:     0,
			want:           bdTablesUnknown,
			why:            "the pre-existing undetermined path must survive the cursor-aware rewrite unchanged",
		},
		{
			name:           "cursor_value_query_failure_is_undetermined_not_absent",
			tableCountCSV:  "cnt\n0\n",
			tableCountExit: 0,
			migExistsCSV:   "cnt\n1\n",
			migExistsExit:  0,
			cursorCSV:      "",
			cursorExit:     1,
			want:           bdTablesUnknown,
			why:            "a cursor query that does not answer is evidence of nothing and must not be read as cursor=0, which would silently re-authorize the destructive reinit",
		},
		{
			name:           "migration_table_existence_query_failure_is_undetermined_not_absent",
			tableCountCSV:  "cnt\n0\n",
			tableCountExit: 0,
			migExistsCSV:   "",
			migExistsExit:  1,
			cursorCSV:      "cur\n0\n",
			cursorExit:     0,
			want:           bdTablesUnknown,
			why:            "the existence probe is as much a part of reading the cursor as the value query, and its failure must not be silently treated as cursor=0 either",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			writeFakeSchemaCursorDolt(t, binDir,
				tc.tableCountCSV, tc.tableCountExit,
				tc.migExistsCSV, tc.migExistsExit,
				tc.cursorCSV, tc.cursorExit)

			script := "connect_host() { printf '127.0.0.1'; }\n" +
				validSQLName + "\n" +
				tableCount + "\n" +
				schemaCursor + "\n" +
				holdsTables + "\n" +
				"bd_runtime_store_holds_bd_tables hq\n"

			got := exitCodeOf(t, runGCBeadsBdSnippet(t, script, binDir))
			if got != tc.want {
				t.Fatalf("bd_runtime_store_holds_bd_tables = %d, want %d (table count=%q/%d, migrations exist=%q/%d, cursor=%q/%d): %s",
					got, tc.want,
					tc.tableCountCSV, tc.tableCountExit,
					tc.migExistsCSV, tc.migExistsExit,
					tc.cursorCSV, tc.cursorExit,
					tc.why)
			}
		})
	}
}

// writeFakeSchemaCursorDolt installs a dolt stub that answers three distinct
// queries differently by dispatching on the SQL text sent via -q: the
// four-table count query (same shape writeFakeCountDolt answers), the
// schema_migrations existence probe, and the schema_migrations cursor value
// query. writeFakeCountDolt answers every query identically and so cannot
// drive a case where the count and the cursor need to disagree; this is the
// sibling the cursor-awareness tests need instead.
func writeFakeSchemaCursorDolt(t *testing.T, dir string,
	tableCountCSV string, tableCountExit int,
	migExistsCSV string, migExistsExit int,
	cursorCSV string, cursorExit int,
) {
	t.Helper()

	tableCountFile := filepath.Join(dir, "table-count.csv")
	migExistsFile := filepath.Join(dir, "migrations-exist.csv")
	cursorFile := filepath.Join(dir, "cursor.csv")
	if err := os.WriteFile(tableCountFile, []byte(tableCountCSV), 0o600); err != nil {
		t.Fatalf("write fake dolt payload: %v", err)
	}
	if err := os.WriteFile(migExistsFile, []byte(migExistsCSV), 0o600); err != nil {
		t.Fatalf("write fake dolt payload: %v", err)
	}
	if err := os.WriteFile(cursorFile, []byte(cursorCSV), 0o600); err != nil {
		t.Fatalf("write fake dolt payload: %v", err)
	}

	body := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  *"'issues'"*)
    cat %q
    exit %d
    ;;
  *"information_schema.tables"*"schema_migrations"*)
    cat %q
    exit %d
    ;;
  *"schema_migrations"*)
    cat %q
    exit %d
    ;;
  *)
    exit 0
    ;;
esac
`, tableCountFile, tableCountExit, migExistsFile, migExistsExit, cursorFile, cursorExit)
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// TestForceReinitGuardWaitOutlastsCursorAdvancing pins the other half of the
// same bug: even once bd_runtime_store_holds_bd_tables correctly answers
// "present" for a mid-migration store, op_init only avoids the destructive
// reinit if wait_for_bd_runtime_schema actually waits for that migration to
// finish. The pre-fix loop gives up after a fixed 8 attempts with no regard
// for whether the concurrent initializer is still making progress; a
// migration slower than 8 short backoff steps still loses the race. This
// drives a fake schema_migrations cursor that advances on every call and
// never repeats, with schema_ready only turning true once the cursor has
// moved well past that old fixed budget — proving the wait outlives it.
func TestForceReinitGuardWaitOutlastsCursorAdvancing(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	src := readGCBeadsBdScript(t)
	validSQLName := extractShellFunction(t, src, "valid_sql_name")
	serverSQL := extractShellFunction(t, src, "server_sql")
	schemaReady := extractShellFunction(t, src, "bd_runtime_schema_ready")
	tableCount := extractShellFunction(t, src, "bd_runtime_bd_table_count")
	schemaCursor := extractShellFunction(t, src, "bd_runtime_schema_cursor")
	sleepMs := extractShellFunction(t, src, "sleep_ms")
	waitForSchema := extractShellFunction(t, src, "wait_for_bd_runtime_schema")

	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "cursor-counter")

	const readyAtCursor = 12 // past the old fixed 8-attempt budget
	fakeDolt := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
counter_file=%q
case "$query" in
  *"FROM config"*)
    n=$(cat "$counter_file" 2>/dev/null || echo 0)
    [ "$n" -ge %d ]
    exit $?
    ;;
  *"information_schema.tables"*"schema_migrations"*)
    printf 'cnt\n1\n'
    exit 0
    ;;
  *"schema_migrations"*)
    n=$(cat "$counter_file" 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" > "$counter_file"
    printf 'cur\n%%d\n' "$n"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, counterFile, readyAtCursor)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(fakeDolt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := "connect_host() { printf '127.0.0.1'; }\n" +
		validSQLName + "\n" +
		serverSQL + "\n" +
		schemaReady + "\n" +
		tableCount + "\n" +
		schemaCursor + "\n" +
		sleepMs + "\n" +
		waitForSchema + "\n" +
		"wait_for_bd_runtime_schema hq\n"

	if err := runGCBeadsBdSnippet(t, script, binDir); err != nil {
		counterContents, _ := os.ReadFile(counterFile)
		t.Fatalf("wait_for_bd_runtime_schema gave up while the migration cursor was still advancing (last cursor seen: %s): %v", counterContents, err)
	}
}

// TestForceReinitGuardWaitGivesUpOnStalledCursor is the other side of the
// same behavior: when the cursor stops moving — a genuinely stuck or
// crashed concurrent initializer, not just a slow one — the wait must still
// give up rather than hang, and must return failure so op_init's caller can
// die instead of silently proceeding.
func TestForceReinitGuardWaitGivesUpOnStalledCursor(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	src := readGCBeadsBdScript(t)
	validSQLName := extractShellFunction(t, src, "valid_sql_name")
	serverSQL := extractShellFunction(t, src, "server_sql")
	schemaReady := extractShellFunction(t, src, "bd_runtime_schema_ready")
	tableCount := extractShellFunction(t, src, "bd_runtime_bd_table_count")
	schemaCursor := extractShellFunction(t, src, "bd_runtime_schema_cursor")
	sleepMs := extractShellFunction(t, src, "sleep_ms")
	waitForSchema := extractShellFunction(t, src, "wait_for_bd_runtime_schema")

	binDir := t.TempDir()
	callCountFile := filepath.Join(binDir, "cursor-call-count")

	fakeDolt := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
call_count_file=%q
case "$query" in
  *"FROM config"*)
    exit 1
    ;;
  *"information_schema.tables"*"schema_migrations"*)
    printf 'cnt\n1\n'
    exit 0
    ;;
  *"schema_migrations"*)
    n=$(cat "$call_count_file" 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" > "$call_count_file"
    printf 'cur\n6\n'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, callCountFile)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(fakeDolt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := "connect_host() { printf '127.0.0.1'; }\n" +
		validSQLName + "\n" +
		serverSQL + "\n" +
		schemaReady + "\n" +
		tableCount + "\n" +
		schemaCursor + "\n" +
		sleepMs + "\n" +
		waitForSchema + "\n" +
		"wait_for_bd_runtime_schema hq\n"

	err := runGCBeadsBdSnippet(t, script, binDir)
	if err == nil {
		t.Fatalf("wait_for_bd_runtime_schema succeeded against a cursor that never moved from 6; a stalled migration must not read as ready")
	}

	raw, readErr := os.ReadFile(callCountFile)
	if readErr != nil {
		t.Fatalf("cursor was never queried at all (%v); wait_for_bd_runtime_schema must retry a stalled cursor before giving up, not fail on the first check", readErr)
	}
	calls, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("unreadable call count %q: %v", raw, convErr)
	}
	if calls < 2 {
		t.Fatalf("cursor was read only %d time(s); wait_for_bd_runtime_schema must retry a stalled cursor at least once before giving up", calls)
	}
	if calls > 100 {
		t.Fatalf("cursor was read %d times; wait_for_bd_runtime_schema must give up on a stalled cursor within a bounded number of attempts, not spin indefinitely", calls)
	}
}

// TestGcBeadsBdInitRefusesForcedReinitWhenMigrationCursorIsAdvancing drives
// op_init through the exact scenario in the failing job: a target database
// with zero of the four bd tables (so the pre-fix guard read it as
// genuinely empty) but a schema_migrations cursor already at a non-zero
// value, left behind by a concurrent initializer's own bd init that has not
// finished. op_init's fast path for "absent" skips wait_for_bd_runtime_schema
// entirely and forces a reinit immediately —
// TestGcBeadsBdInitRefusesForcedReinitWhenDatabaseHoldsBdTables above only
// covers the count>0 branch of the guard, never this one, which is why the
// bug shipped.
//
// The die message is asserted to differ from the count>0 case's message: a
// store with a stalled migration does not "hold bd tables" in the sense
// that message describes, and conflating the two would mislead whoever
// reads the failure while triaging it.
func TestGcBeadsBdInitRefusesForcedReinitWhenMigrationCursorIsAdvancing(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// bd init exits 0 rather than failing, so a regression surfaces as this
	// test's own assertion rather than as an unrelated downstream error.
	initMarker := filepath.Join(t.TempDir(), "bd-init-ran")
	fakeBd := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" = "init" ]; then
  printf '%%s\n' "$@" > %q
fi
exit 0
`, initMarker)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(fakeBd), 0o755); err != nil {
		t.Fatal(err)
	}

	// Zero of the four bd tables, but schema_migrations already exists with
	// a stalled non-zero cursor: a concurrent initializer's migration that
	// has made progress but not finished, or has crashed partway. The
	// schema readiness probe never succeeds either, matching a store that
	// is not yet queryable.
	fakeDolt := `#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  *"'issues'"*)
    printf 'cnt\n0\n'
    exit 0
    ;;
  *"information_schema.tables"*"schema_migrations"*)
    printf 'cnt\n1\n'
    exit 0
    ;;
  *"schema_migrations"*)
    printf 'cur\n6\n'
    exit 0
    ;;
  *"FROM config"*)
    echo "table not found: config" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(fakeDolt), 0o755); err != nil {
		t.Fatal(err)
	}

	// sleep_ms shells out to sleep, so stubbing it spends the retry budget
	// at no wall-clock cost.
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runGCBeadsBdCommand(t, sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...), script, "init", cityPath, "gc", "hq")
	out := stdout + stderr
	if err == nil {
		t.Fatalf("init should refuse to force-reinitialize a database with an advancing migration cursor, but it succeeded:\n%s", out)
	}

	for _, want := range []string{
		"refusing to force-reinitialize",
		"'hq'",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("refusal is not self-describing, missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "concurrent") && !strings.Contains(out, "migration") {
		t.Fatalf("refusal does not name a concurrent/in-progress migration, so it is indistinguishable from the unrelated count>0 refusal while triaging:\n%s", out)
	}
	if strings.Contains(out, "holds bd tables but its bd schema stayed unreadable across retries") {
		t.Fatalf("refusal reused the count>0 message for a store with zero bd tables, which misdescribes what is actually there:\n%s", out)
	}
	if strings.Contains(out, "missing bd schema; re-initializing") {
		t.Fatalf("init fell through to the destructive reinit instead of refusing:\n%s", out)
	}
	if _, statErr := os.Stat(initMarker); statErr == nil {
		argv, _ := os.ReadFile(initMarker)
		t.Fatalf("bd init ran despite the refusal, so the guard reported without preventing:\nargv:\n%s\noutput:\n%s", argv, out)
	}
}

// TestGcBeadsBdScriptDocumentsSchemaSettleTimeoutOverride pins the exit
// contract's requirement that wait_for_bd_runtime_schema's wall-clock hard
// cap has an env override, and that the override is documented in the
// script's own header block the way every other GC_DOLT_*_TIMEOUT_MS
// variable already is (GC_DOLT_LOCK_RELEASE_TIMEOUT_MS,
// GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS). Deliberately does not drive
// the cap with real wall-clock timing — that would make the suite slow and
// flaky for no behavioral benefit over the stall/advance tests above; this
// only checks the override exists and is wired in.
func TestGcBeadsBdScriptDocumentsSchemaSettleTimeoutOverride(t *testing.T) {
	src := readGCBeadsBdScript(t)

	headerEnd := strings.Index(src, "\nset -e")
	if headerEnd == -1 {
		t.Fatalf("could not locate end of script header comment block")
	}
	header := src[:headerEnd]
	if !strings.Contains(header, "GC_DOLT_SCHEMA_SETTLE_TIMEOUT_MS") {
		t.Fatalf("script header does not document GC_DOLT_SCHEMA_SETTLE_TIMEOUT_MS, unlike every other GC_DOLT_*_TIMEOUT_MS override:\n%s", header)
	}

	waitForSchema := extractShellFunction(t, src, "wait_for_bd_runtime_schema")
	if !strings.Contains(waitForSchema, "GC_DOLT_SCHEMA_SETTLE_TIMEOUT_MS") {
		t.Fatalf("wait_for_bd_runtime_schema does not read GC_DOLT_SCHEMA_SETTLE_TIMEOUT_MS, so the documented override in the header has no effect:\n%s", waitForSchema)
	}
	if !strings.Contains(waitForSchema, "120000") {
		t.Fatalf("wait_for_bd_runtime_schema does not default the hard cap to >=120s (120000ms), the minimum the exit contract requires:\n%s", waitForSchema)
	}
}
