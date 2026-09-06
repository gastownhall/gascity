package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestManagedDoltReadOnlyProbeStatementsForReturnsNothingForEmptyDB(t *testing.T) {
	for _, db := range []string{"", " ", "\t"} {
		if got := managedDoltReadOnlyProbeStatementsFor(db); got != nil {
			t.Fatalf("managedDoltReadOnlyProbeStatementsFor(%q) = %v, want nil", db, got)
		}
		if got := managedDoltReadOnlyProbeSQLFor(db); got != "" {
			t.Fatalf("managedDoltReadOnlyProbeSQLFor(%q) = %q, want \"\"", db, got)
		}
	}
}

func TestManagedDoltReadOnlyProbeNeverTargetsLegacyDatabase(t *testing.T) {
	for _, db := range []string{"gascity", "gm", "be", "user_db", "003", "name-with-hyphen"} {
		stmts := managedDoltReadOnlyProbeStatementsFor(db)
		joined := managedDoltReadOnlyProbeSQLFor(db)
		for _, q := range append(append([]string{}, stmts...), joined) {
			assertNoManagedDoltProbeLegacyTarget(t, "probe stmts for "+db, q)
			assertNoManagedDoltProbeDrop(t, "probe stmts for "+db, q)
		}
		wantTable := "`" + db + "`.`" + managedDoltProbeTable + "`"
		wantIgnoreTable := "`" + db + "`.`dolt_ignore`"
		wantUse := "USE `" + db + "`"
		for _, q := range stmts {
			if !strings.Contains(q, wantTable) && !strings.Contains(q, wantIgnoreTable) && q != wantUse {
				t.Fatalf("probe stmt for %s missing %q: %s", db, wantTable, q)
			}
			if strings.Contains(q, "`.`__probe`") {
				t.Fatalf("probe stmt for %s uses generic probe table: %s", db, q)
			}
		}
		if !strings.Contains(joined, "REPLACE INTO "+wantTable+" VALUES (1)") {
			t.Fatalf("probe SQL for %s must write to %s: %s", db, wantTable, joined)
		}
	}
}

// TestManagedDoltReadOnlyProbeRegistersIgnoreRuleLast pins the fix for the daa
// 2026-08-04 incident class: an unregistered probe table gets first-committed by
// the compaction flatten's `DOLT_COMMIT -Am`, which drifts the database hash and
// quarantines GC for the database. The rule registration must stay LAST so a
// read-only server still fails on CREATE/REPLACE, which is what the read-only
// classification keys on.
func TestManagedDoltReadOnlyProbeRegistersIgnoreRuleLast(t *testing.T) {
	for _, db := range []string{"gascity", "003", "name-with-hyphen", "with`backtick"} {
		stmts := managedDoltReadOnlyProbeStatementsFor(db)
		if len(stmts) != 4 {
			t.Fatalf("probe stmts for %s = %v, want 4 statements", db, stmts)
		}
		wantIgnore := "INSERT IGNORE INTO " + managedDoltQuoteIdent(db) + ".`dolt_ignore` (pattern, ignored) VALUES ('" + managedDoltProbeTable + "', 1)"
		if stmts[3] != wantIgnore {
			t.Fatalf("probe stmts for %s last statement = %q, want %q", db, stmts[3], wantIgnore)
		}
		// dolt_ignore is a session-root-backed system table: without a current
		// database on the session, a qualified write to it fails with "no root
		// value found in session" on both probe paths (dolt CLI over --host and
		// the direct driver connect with no default schema). USE must come first
		// and must be the only statement before the CREATE — it is read-only, so
		// a read-only server still fails first on the CREATE/REPLACE the
		// classification keys on.
		if stmts[0] != "USE "+managedDoltQuoteIdent(db) {
			t.Fatalf("probe stmts for %s must select the database first: %q", db, stmts[0])
		}
		// Prefix pins alone would accept a CREATE/REPLACE aimed at dolt_ignore,
		// which the relaxed legacy-target loop above also lets through; pin the
		// quoted probe-table target the same way the ignore rule is pinned.
		wantTarget := managedDoltQuoteIdent(db) + "." + managedDoltQuoteIdent(managedDoltProbeTable)
		if !strings.HasPrefix(stmts[1], "CREATE TABLE IF NOT EXISTS ") || !strings.Contains(stmts[1], wantTarget) {
			t.Fatalf("probe stmts for %s must create %s after USE: %q", db, wantTarget, stmts[1])
		}
		if !strings.HasPrefix(stmts[2], "REPLACE INTO ") || !strings.Contains(stmts[2], wantTarget) {
			t.Fatalf("probe stmts for %s must write the probe row into %s third: %q", db, wantTarget, stmts[2])
		}
		if strings.Contains(stmts[3], "REPLACE INTO") {
			t.Fatalf("probe stmts for %s must not REPLACE the ignore rule (an operator ignored=0 override has to survive): %q", db, stmts[3])
		}
		joined := managedDoltReadOnlyProbeSQLFor(db)
		if !strings.HasSuffix(joined, wantIgnore+";") {
			t.Fatalf("probe SQL for %s = %q, want it to end with %q", db, joined, wantIgnore+";")
		}
	}
}

func TestManagedDoltQuoteIdentEscapesBackticks(t *testing.T) {
	cases := map[string]string{
		"gascity":          "`gascity`",
		"003":              "`003`",
		"with`backtick":    "`with``backtick`",
		"name with spaces": "`name with spaces`",
		"":                 "``",
	}
	for in, want := range cases {
		if got := managedDoltQuoteIdent(in); got != want {
			t.Fatalf("managedDoltQuoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManagedDoltFirstUserDatabaseSkipsSystemDatabases(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"all system", []string{"Database", "information_schema", "mysql", "dolt", "dolt_cluster", "performance_schema", "sys", "__gc_probe"}, ""},
		{"first user wins", []string{"Database", "__gc_probe", "dolt_cluster", "performance_schema", "sys", "gascity", "be"}, "gascity"},
		{"case-insensitive system match", []string{"Database", "Information_Schema", "MySQL", "DOLT_CLUSTER", "PERFORMANCE_SCHEMA", "SYS", "__GC_PROBE", "gm"}, "gm"},
		{"empty", []string{}, ""},
		{"only header", []string{"Database"}, ""},
		{"whitespace + blanks ignored", []string{"Database", "", "  ", "gascity"}, "gascity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltFirstUserDatabase(tc.lines); got != tc.want {
				t.Fatalf("managedDoltFirstUserDatabase(%v) = %q, want %q", tc.lines, got, tc.want)
			}
		})
	}
}

func TestManagedDoltFirstUserDatabaseFromCSVHandlesEscapedNames(t *testing.T) {
	got, err := managedDoltFirstUserDatabaseFromCSV("Database\ninformation_schema\n\"tenant,one\"\n")
	if err != nil {
		t.Fatalf("managedDoltFirstUserDatabaseFromCSV() error = %v", err)
	}
	if got != "tenant,one" {
		t.Fatalf("managedDoltFirstUserDatabaseFromCSV() = %q, want tenant,one", got)
	}

	got, err = managedDoltFirstUserDatabaseFromCSV("Database\n\"tenant\"\"two\"\n")
	if err != nil {
		t.Fatalf("managedDoltFirstUserDatabaseFromCSV() quote error = %v", err)
	}
	if got != "tenant\"two" {
		t.Fatalf("managedDoltFirstUserDatabaseFromCSV() = %q, want tenant\"two", got)
	}
}

func TestManagedDoltReadOnlyStateNoUserDatabaseIsUnknown(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ninformation_schema\nmysql\ndolt\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*"__gc_read_only_probe"*)
    echo "unexpected write probe without a user database" >&2
    exit 2
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	state, err := managedDoltReadOnlyState("127.0.0.1", "3311", "root")
	if err == nil {
		t.Fatal("managedDoltReadOnlyState() error = nil, want no-user-database diagnostic")
	}
	if state != "unknown" {
		t.Fatalf("managedDoltReadOnlyState() state = %q, want unknown", state)
	}
	if !strings.Contains(err.Error(), "no user database") {
		t.Fatalf("managedDoltReadOnlyState() error = %v, want no user database", err)
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if strings.Contains(string(invocation), "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("managedDoltReadOnlyState() ran write probe without user database:\n%s", invocation)
	}
}

func TestManagedDoltHealthCheckNoUserDatabaseIsUnknown(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -r csv -q SELECT COUNT(*) AS cnt FROM information_schema.SCHEMATA"*)
    exit 0
    ;;
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ninformation_schema\nmysql\ndolt\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
    exit 0
    ;;
  *"sql -r csv -q SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"*)
    printf 'cnt\n0\n'
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*)
    echo "unexpected write probe without a user database" >&2
    exit 2
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	report, err := managedDoltHealthCheck("127.0.0.1", "3311", "root", true)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v", err)
	}
	if !report.QueryReady || report.ReadOnly != "unknown" || report.ConnectionCount != "0" {
		t.Fatalf("managedDoltHealthCheck() = %+v, want query-ready unknown with connection count", report)
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if strings.Contains(string(invocation), "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("managedDoltHealthCheck() ran write probe without user database:\n%s", invocation)
	}
}

func TestManagedDoltResetProbeDropsUserProbeTables(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ngascity\ninformation_schema\nwith-hyphen\n__gc_probe\n'
    exit 0
    ;;
  *"DROP DATABASE IF EXISTS __gc_probe"*)
    exit 0
    ;;
  *"DROP TABLE IF EXISTS"*"__gc_read_only_probe"*)
    exit 0
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 2
    ;;
esac
`)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := managedDoltResetProbe("127.0.0.1", "3311", "root"); err != nil {
		t.Fatalf("managedDoltResetProbe() error = %v", err)
	}
	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	text := string(invocation)
	for _, want := range []string{
		"DROP DATABASE IF EXISTS __gc_probe",
		"DROP TABLE IF EXISTS `gascity`.`" + managedDoltProbeTable + "`",
		"DROP TABLE IF EXISTS `with-hyphen`.`" + managedDoltProbeTable + "`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("managedDoltResetProbe() invocation = %s, want %q", text, want)
		}
	}
	if strings.Contains(text, "information_schema`.`"+managedDoltProbeTable) || strings.Contains(text, "__gc_probe`.`"+managedDoltProbeTable) {
		t.Fatalf("managedDoltResetProbe() dropped probe table in system database:\n%s", text)
	}
}

func TestManagedDoltSystemDatabasesIncludesManagedAndDoltSystemDatabases(t *testing.T) {
	for _, name := range []string{
		"information_schema",
		"mysql",
		"dolt",
		"dolt_cluster",
		"performance_schema",
		"sys",
		managedDoltProbeDatabase,
	} {
		if _, ok := managedDoltSystemDatabases[name]; !ok {
			t.Fatalf("managedDoltSystemDatabases missing %q", name)
		}
	}
}

func assertNoManagedDoltProbeDrop(t *testing.T, label, text string) {
	t.Helper()
	dropProbeDatabase := regexp.MustCompile("(?i)\\bDROP\\s+DATABASE\\s+(IF\\s+EXISTS\\s+)?`?__gc_probe`?")
	dropGenericProbeTable := regexp.MustCompile("(?i)\\bDROP\\s+TABLE\\s+(IF\\s+EXISTS\\s+)?(`?__gc_probe`?\\.)?`?__probe`?")
	dropManagedProbeTable := regexp.MustCompile("(?i)\\bDROP\\s+TABLE\\s+(IF\\s+EXISTS\\s+)?(`?__gc_probe`?\\.)?`?" + regexp.QuoteMeta(managedDoltProbeTable) + "`?")
	if dropProbeDatabase.MatchString(text) {
		t.Fatalf("%s must not drop __gc_probe: %s", label, text)
	}
	if dropGenericProbeTable.MatchString(text) {
		t.Fatalf("%s must not drop generic __probe tables: %s", label, text)
	}
	if dropManagedProbeTable.MatchString(text) {
		t.Fatalf("%s must not drop %s from normal probe paths: %s", label, managedDoltProbeTable, text)
	}
}

// assertNoManagedDoltProbeLegacyTarget enforces that gc CLI probe SQL never
// CREATEs or writes to the legacy `__gc_probe` database — that's what made
// it dolt's stats backing store and accumulated 596k buckets in production.
func assertNoManagedDoltProbeLegacyTarget(t *testing.T, label, text string) {
	t.Helper()
	createLegacy := regexp.MustCompile("(?i)\\bCREATE\\s+(DATABASE|TABLE)\\s+(IF\\s+NOT\\s+EXISTS\\s+)?`?__gc_probe`?")
	writeLegacy := regexp.MustCompile("(?i)\\b(REPLACE|INSERT)\\s+INTO\\s+`?__gc_probe`?")
	if createLegacy.MatchString(text) {
		t.Fatalf("%s must not create __gc_probe: %s", label, text)
	}
	if writeLegacy.MatchString(text) {
		t.Fatalf("%s must not write to __gc_probe: %s", label, text)
	}
}

func TestManagedDoltHealthCheckWithPasswordUsesDirectHelpers(t *testing.T) {
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$INVOCATION_FILE\"\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_DOLT_PASSWORD", "secret")

	oldQuery := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	defer func() {
		managedDoltQueryProbeDirectFn = oldQuery
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
	}()

	calledQuery := false
	calledReadOnly := false
	calledConnCount := false
	managedDoltQueryProbeDirectFn = func(host, port, user string) error {
		calledQuery = true
		if host != "0.0.0.0" || port != "3311" || user != "root" {
			t.Fatalf("query direct args = %q %q %q", host, port, user)
		}
		return nil
	}
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		calledReadOnly = true
		return "false", nil
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) {
		calledConnCount = true
		return "7", nil
	}

	report, err := managedDoltHealthCheck("0.0.0.0", "3311", "root", true)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v", err)
	}
	if !calledQuery || !calledReadOnly || !calledConnCount {
		t.Fatalf("direct helper calls = query:%v readOnly:%v connCount:%v", calledQuery, calledReadOnly, calledConnCount)
	}
	if !report.QueryReady || report.ReadOnly != "false" || report.ConnectionCount != "7" {
		t.Fatalf("managedDoltHealthCheck() = %+v", report)
	}
	if invocation, err := os.ReadFile(invocationFile); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt argv should not be used when GC_DOLT_PASSWORD is set: %s", string(invocation))
	}
}

func TestManagedDoltHealthCheckWithPasswordPropagatesReadOnlyProbeErrors(t *testing.T) {
	t.Setenv("GC_DOLT_PASSWORD", "secret")

	oldQuery := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	defer func() {
		managedDoltQueryProbeDirectFn = oldQuery
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
	}()

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error {
		return nil
	}
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		return "unknown", errors.New("read-only probe failed")
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) {
		t.Fatal("connection count should not run after read-only probe failure")
		return "", nil
	}

	_, err := managedDoltHealthCheck("127.0.0.1", "3311", "root", true)
	if err == nil {
		t.Fatal("managedDoltHealthCheck() error = nil, want read-only probe failure")
	}
	if !strings.Contains(err.Error(), "read-only probe failed") {
		t.Fatalf("managedDoltHealthCheck() error = %v, want read-only probe failure", err)
	}
}

func TestRunManagedDoltSQLTimesOut(t *testing.T) {
	binDir := t.TempDir()
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldTimeout := managedDoltSQLCommandTimeout
	managedDoltSQLCommandTimeout = 50 * time.Millisecond
	defer func() { managedDoltSQLCommandTimeout = oldTimeout }()

	_, err := runManagedDoltSQL("127.0.0.1", "3311", "root", "-q", "SELECT 1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("runManagedDoltSQL() error = %v, want timeout", err)
	}
}

func TestRunManagedDoltSQLIncludesConfiguredPasswordFlag(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	fakeDolt := filepath.Join(binDir, "dolt")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", argsFile)
	if err := os.WriteFile(fakeDolt, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_DOLT_PASSWORD", "secret")

	if _, err := runManagedDoltSQL("127.0.0.1", "3311", "root", "-q", "SELECT 1"); err != nil {
		t.Fatalf("runManagedDoltSQL() error = %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--password\nsecret\n") {
		t.Fatalf("dolt args missing configured password flag:\n%s", data)
	}
}
