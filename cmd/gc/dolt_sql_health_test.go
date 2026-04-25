package main

import (
	"errors"
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
		wantTable := "`" + db + "`.`__probe`"
		for _, q := range stmts {
			if !strings.Contains(q, wantTable) {
				t.Fatalf("probe stmt for %s missing %q: %s", db, wantTable, q)
			}
		}
		if !strings.Contains(joined, "REPLACE INTO "+wantTable+" VALUES (1)") {
			t.Fatalf("probe SQL for %s must write to %s: %s", db, wantTable, joined)
		}
	}
}

func TestManagedDoltQuoteIdentEscapesBackticks(t *testing.T) {
	cases := map[string]string{
		"gascity":            "`gascity`",
		"003":                "`003`",
		"with`backtick":      "`with``backtick`",
		"name with spaces":   "`name with spaces`",
		"":                   "``",
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
		{"all system", []string{"Database", "information_schema", "mysql", "dolt_cluster", "__gc_probe"}, ""},
		{"first user wins", []string{"Database", "__gc_probe", "dolt_cluster", "gascity", "be"}, "gascity"},
		{"case-insensitive system match", []string{"Database", "Information_Schema", "MySQL", "DOLT_CLUSTER", "__GC_PROBE", "gm"}, "gm"},
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

func TestManagedDoltSystemDatabasesIncludesLegacyProbe(t *testing.T) {
	if _, ok := managedDoltSystemDatabases[managedDoltProbeDatabase]; !ok {
		t.Fatalf("managedDoltSystemDatabases missing %q — probe could re-elect legacy database", managedDoltProbeDatabase)
	}
}

func assertNoManagedDoltProbeDrop(t *testing.T, label, text string) {
	t.Helper()
	dropProbeDatabase := regexp.MustCompile("(?i)\\bDROP\\s+DATABASE\\s+(IF\\s+EXISTS\\s+)?`?__gc_probe`?")
	dropProbeTable := regexp.MustCompile("(?i)\\bDROP\\s+TABLE\\s+(IF\\s+EXISTS\\s+)?(`?__gc_probe`?\\.)?`?__probe`?")
	if dropProbeDatabase.MatchString(text) {
		t.Fatalf("%s must not drop __gc_probe: %s", label, text)
	}
	if dropProbeTable.MatchString(text) {
		t.Fatalf("%s must keep __gc_probe.__probe stable: %s", label, text)
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

// assertManagedDoltProbeWrites is retained for the gc-beads-bd.sh fallback
// test (the bash branch still hits the legacy probe target until its own
// follow-up bead lands).
func assertManagedDoltProbeWrites(t *testing.T, label, text string) {
	t.Helper()
	if !strings.Contains(text, "REPLACE INTO __gc_probe.__probe VALUES (1)") {
		t.Fatalf("%s must write to __gc_probe.__probe: %s", label, text)
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
