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
		for _, q := range stmts {
			if !strings.Contains(q, wantTable) {
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
		{"all system", []string{"Database", "information_schema", "mysql", "dolt_cluster", "performance_schema", "sys", "__gc_probe"}, ""},
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
    printf 'Database\ninformation_schema\nmysql\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
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
  *"sql -q SELECT active_branch()"*)
    exit 0
    ;;
  *"sql -r csv -q SHOW DATABASES"*)
    printf 'Database\ninformation_schema\nmysql\ndolt_cluster\nperformance_schema\nsys\n__gc_probe\n'
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
