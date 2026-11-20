package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// databaseHealthFakeDolt answers the managed-Dolt health probes. HEAD_MODE,
// WRITE_MODE and READ_MODE pick how the city database responds; the error
// texts are what Dolt 2.4 prints for each outage shape.
const databaseHealthFakeDolt = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVOCATION_FILE"
case "$*" in
  *"information_schema.SCHEMATA"*)
    exit 0
    ;;
  *"information_schema.PROCESSLIST"*)
    printf 'cnt\n3\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\naaa_first\nhq\ninformation_schema\nmysql\n'
    exit 0
    ;;
  *"DOLT_HASHOF"*)
    case "$HEAD_MODE" in
      ok) printf 'head\nlm41v2vo0p5s281qk22bmmqdun6coqlh\n' ;;
      missing) echo "error on line 1 for query USE: Error 1049 (HY000): database not found: hq" >&2; exit 1 ;;
      ghost) echo "Error 1105 (HY000): cannot resolve default branch head for database 'hq': 'ghost'" >&2; exit 1 ;;
      garbage) printf 'head\nNULL\n' ;;
      short) printf 'head\nabc\n' ;;
    esac
    exit 0
    ;;
  *"WHERE k = 1"*)
    case "$READ_MODE" in
      ok) printf 'cnt\n1\n' ;;
      empty) printf 'cnt\n0\n' ;;
      fail) echo "Error 1105 (HY000): no root value found in session" >&2; exit 1 ;;
    esac
    exit 0
    ;;
  *"CREATE TABLE IF NOT EXISTS"*)
    case "$WRITE_MODE" in
      ok) exit 0 ;;
      readonly) echo "database server is set to read only mode" >&2; exit 1 ;;
      fail) echo "Error 1105 (HY000): no root value found in session" >&2; exit 1 ;;
    esac
    ;;
esac
echo "unexpected command: $*" >&2
exit 2
`

func setupDatabaseHealthFakeDolt(t *testing.T, headMode, writeMode, readMode string) string {
	t.Helper()
	binDir := t.TempDir()
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation.txt")
	writeFakeDoltSQLBinary(t, binDir, invocationFile, databaseHealthFakeDolt)
	t.Setenv("INVOCATION_FILE", invocationFile)
	t.Setenv("HEAD_MODE", headMode)
	t.Setenv("WRITE_MODE", writeMode)
	t.Setenv("READ_MODE", readMode)
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return invocationFile
}

func readInvocations(t *testing.T, invocationFile string) string {
	t.Helper()
	data, err := os.ReadFile(invocationFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	return string(data)
}

func TestManagedDoltHealthCheckForProvesCityDatabase(t *testing.T) {
	bt := "`"
	cityProbe := bt + "hq" + bt + "." + bt + managedDoltProbeTable + bt
	tests := []struct {
		name         string
		head         string
		write        string
		read         string
		wantReady    bool
		wantReadOnly string
		wantErrParts []string
		wantWrite    bool
		wantReadBack bool
	}{
		{
			name: "healthy", head: "ok", write: "ok", read: "ok",
			wantReady: true, wantReadOnly: "false", wantWrite: true, wantReadBack: true,
		},
		{
			name: "missing from catalog", head: "missing", write: "ok", read: "ok",
			wantReadOnly: "unknown",
			wantErrParts: []string{`database "hq" default branch head unresolvable`, "database not found: hq"},
		},
		{
			name: "default branch head unresolvable", head: "ghost", write: "ok", read: "ok",
			wantReadOnly: "unknown",
			wantErrParts: []string{`database "hq" default branch head unresolvable`, "cannot resolve default branch head"},
		},
		{
			name: "head is not a commit hash", head: "garbage", write: "ok", read: "ok",
			wantReadOnly: "unknown",
			wantErrParts: []string{"unexpected head"},
		},
		{
			name: "sample write fails", head: "ok", write: "fail", read: "ok",
			wantReadOnly: "unknown", wantWrite: true,
			wantErrParts: []string{`sample write to database "hq" failed`, "no root value found in session"},
		},
		{
			name: "sample read fails", head: "ok", write: "ok", read: "fail",
			wantReadOnly: "false", wantWrite: true, wantReadBack: true,
			wantErrParts: []string{`sample read from database "hq" failed`, "no root value found in session"},
		},
		{
			name: "sample row missing after write", head: "ok", write: "ok", read: "empty",
			wantReadOnly: "false", wantWrite: true, wantReadBack: true,
			wantErrParts: []string{"probe row count = 0 after write, want 1"},
		},
		{
			// Read-only is classified on its own; the caller decides whether a
			// read-only server is fatal (local managed) or tolerated (remote).
			name: "read-only server", head: "ok", write: "readonly", read: "ok",
			wantReady: true, wantReadOnly: "true", wantWrite: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invocationFile := setupDatabaseHealthFakeDolt(t, tt.head, tt.write, tt.read)

			report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", true, managedDoltHealthTarget{Database: "hq"})
			if err != nil {
				t.Fatalf("managedDoltHealthCheckFor() error = %v, want database failures reported in-band", err)
			}
			if !report.QueryReady || report.Database != "hq" || report.ConnectionCount != "3" {
				t.Fatalf("report = %+v, want query-ready hq report with connection count", report)
			}
			if report.DatabaseReady != tt.wantReady {
				t.Fatalf("DatabaseReady = %v (error %q), want %v", report.DatabaseReady, report.DatabaseError, tt.wantReady)
			}
			if report.ReadOnly != tt.wantReadOnly {
				t.Fatalf("ReadOnly = %q, want %q", report.ReadOnly, tt.wantReadOnly)
			}
			if tt.wantReady && report.DatabaseError != "" {
				t.Fatalf("DatabaseError = %q, want empty for a ready database", report.DatabaseError)
			}
			for _, part := range tt.wantErrParts {
				if !strings.Contains(report.DatabaseError, part) {
					t.Fatalf("DatabaseError = %q, want it to contain %q", report.DatabaseError, part)
				}
			}

			text := readInvocations(t, invocationFile)
			if got := strings.Contains(text, "CREATE TABLE IF NOT EXISTS "+cityProbe); got != tt.wantWrite {
				t.Fatalf("sample write to hq ran = %v, want %v:\n%s", got, tt.wantWrite, text)
			}
			if got := strings.Contains(text, "WHERE k = 1"); got != tt.wantReadBack {
				t.Fatalf("sample read-back ran = %v, want %v:\n%s", got, tt.wantReadBack, text)
			}
			// The write probe must land in the city database, not whichever
			// user database SHOW DATABASES happens to list first.
			if strings.Contains(text, "SHOW DATABASES") || strings.Contains(text, "aaa_first") {
				t.Fatalf("health check discovered a probe database instead of using hq:\n%s", text)
			}
		})
	}
}

func TestManagedDoltHealthCheckForRequiresRepositoryInDataDir(t *testing.T) {
	invocationFile := setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "rigdb", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory without .dolt is not a repository.
	if err := os.MkdirAll(filepath.Join(dataDir, "hq"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", true, managedDoltHealthTarget{Database: "hq", DataDir: dataDir})
	if err != nil {
		t.Fatalf("managedDoltHealthCheckFor() error = %v", err)
	}
	if report.DatabaseReady {
		t.Fatalf("DatabaseReady = true, want false when hq has no repository under %s", dataDir)
	}
	want := `database "hq" absent from data dir ` + dataDir
	if report.DatabaseError != want {
		t.Fatalf("DatabaseError = %q, want %q", report.DatabaseError, want)
	}
	if text := readInvocations(t, invocationFile); strings.Contains(text, "DOLT_HASHOF") || strings.Contains(text, "CREATE TABLE") {
		t.Fatalf("health check queried a database missing on disk:\n%s", text)
	}
}

func TestManagedDoltHealthCheckForAcceptsRepositoryDifferingOnlyInCase(t *testing.T) {
	setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "HQ", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", true, managedDoltHealthTarget{Database: "hq", DataDir: dataDir})
	if err != nil {
		t.Fatalf("managedDoltHealthCheckFor() error = %v", err)
	}
	if !report.DatabaseReady {
		t.Fatalf("DatabaseReady = false (%q), want true for data dir entry HQ", report.DatabaseError)
	}
}

func TestManagedDoltHealthCheckForReportsMissingDataDir(t *testing.T) {
	setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
	dataDir := filepath.Join(t.TempDir(), "gone")

	report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", true, managedDoltHealthTarget{Database: "hq", DataDir: dataDir})
	if err != nil {
		t.Fatalf("managedDoltHealthCheckFor() error = %v", err)
	}
	if report.DatabaseReady || !strings.Contains(report.DatabaseError, "reading data dir "+dataDir) {
		t.Fatalf("report = %+v, want data dir read failure", report)
	}
}

func TestManagedDoltHealthCheckForWithoutDatabaseKeepsServerOnlyContract(t *testing.T) {
	setupDatabaseHealthFakeDolt(t, "missing", "ok", "ok")

	report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", true, managedDoltHealthTarget{})
	if err != nil {
		t.Fatalf("managedDoltHealthCheckFor() error = %v", err)
	}
	if report.Database != "" || report.DatabaseReady || report.DatabaseError != "" || report.ReadOnly != "false" {
		t.Fatalf("report = %+v, want a server-only report", report)
	}
	fields := managedDoltHealthCheckFields(report)
	if len(fields) != 3 {
		t.Fatalf("fields = %q, want only the server fields", fields)
	}
}

func TestManagedDoltHealthCheckFieldsReportDatabaseOnOneLine(t *testing.T) {
	got := managedDoltHealthCheckFields(managedDoltSQLHealthReport{
		QueryReady:      true,
		ReadOnly:        "unknown",
		ConnectionCount: "4",
		Database:        "hq",
		DatabaseError:   "database \"hq\" default branch head unresolvable:\nError 1105\t(HY000)",
	})
	want := []string{
		"query_ready\ttrue",
		"read_only\tunknown",
		"connection_count\t4",
		"database\thq",
		"database_ready\tfalse",
		"database_error\tdatabase \"hq\" default branch head unresolvable: Error 1105 (HY000)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("fields =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	ready := managedDoltHealthCheckFields(managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false", Database: "hq", DatabaseReady: true})
	if ready[len(ready)-1] != "database_ready\ttrue" {
		t.Fatalf("ready fields = %q, want trailing database_ready true", ready)
	}
}

func TestManagedDoltHealthCheckForWithPasswordProbesNamedDatabase(t *testing.T) {
	t.Setenv("GC_DOLT_PASSWORD", "secret")
	oldQuery := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldHead := managedDoltDatabaseHeadDirectFn
	oldReadBack := managedDoltProbeReadBackDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	defer func() {
		managedDoltQueryProbeDirectFn = oldQuery
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltDatabaseHeadDirectFn = oldHead
		managedDoltProbeReadBackDirectFn = oldReadBack
		managedDoltConnectionCountDirectFn = oldConnCount
	}()

	var calls []string
	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltDatabaseHeadDirectFn = func(_, _, _, database string) (string, error) {
		calls = append(calls, "head:"+database)
		return "lm41v2vo0p5s281qk22bmmqdun6coqlh", nil
	}
	managedDoltReadOnlyStateDirectFn = func(_, _, _, database string) (string, error) {
		calls = append(calls, "write:"+database)
		return "false", nil
	}
	managedDoltProbeReadBackDirectFn = func(_, _, _, database string) error {
		calls = append(calls, "read:"+database)
		return nil
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "2", nil }

	report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", true, managedDoltHealthTarget{Database: "hq"})
	if err != nil {
		t.Fatalf("managedDoltHealthCheckFor() error = %v", err)
	}
	if !report.DatabaseReady || report.ReadOnly != "false" || report.ConnectionCount != "2" {
		t.Fatalf("report = %+v, want ready writable hq", report)
	}
	if got, want := strings.Join(calls, ","), "head:hq,write:hq,read:hq"; got != want {
		t.Fatalf("direct probe calls = %q, want %q", got, want)
	}
}

func TestDoltStateHealthCheckCmdReportsUnavailableDatabaseInBand(t *testing.T) {
	setupDatabaseHealthFakeDolt(t, "missing", "ok", "ok")

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only", "--database", "hq"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s; want exit 0 with the failure in-band", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["query_ready"] != "true" || got["database"] != "hq" || got["database_ready"] != "false" {
		t.Fatalf("output = %q, want query-ready hq with database_ready false", stdout.String())
	}
	if !strings.Contains(got["database_error"], "database not found: hq") {
		t.Fatalf("database_error = %q, want the Dolt catalog error", got["database_error"])
	}
}

// TestDoltStateHealthCheckCmdDatabaseNotInCatalog covers AC-1: a server
// that answers queries while the named database is gone from its catalog
// keeps exit 0 (the server layer is up), reports query_ready true,
// database_ready false, and a one-line database_error naming the database,
// and never issues the read-only write probe, whose target may be the
// broken database.
func TestDoltStateHealthCheckCmdDatabaseNotInCatalog(t *testing.T) {
	invocationFile := setupDatabaseHealthFakeDolt(t, "missing", "ok", "ok")

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only", "--database", "hq"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s; want exit 0 with the failure in-band", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["query_ready"] != "true" || got["database_ready"] != "false" {
		t.Fatalf("output = %q, want query_ready true and database_ready false", stdout.String())
	}
	if !strings.Contains(got["database_error"], "hq") {
		t.Fatalf("database_error = %q, want it to name hq", got["database_error"])
	}
	if strings.Contains(got["database_error"], "\n") {
		t.Fatalf("database_error = %q, want a single line", got["database_error"])
	}
	if text := readInvocations(t, invocationFile); strings.Contains(text, "CREATE TABLE IF NOT EXISTS") || strings.Contains(text, "REPLACE INTO") {
		t.Fatalf("health check issued a write probe against a database missing from the catalog:\n%s", text)
	}
}

// TestDoltStateHealthCheckCmdDatabaseHeadUnresolvable covers AC-2 on the
// dolt-CLI path: a database that is in the catalog but whose
// DOLT_HASHOF('HEAD') fails (errno 1105) or returns a value that is not a
// commit hash reports database_ready false with a head-resolution error.
func TestDoltStateHealthCheckCmdDatabaseHeadUnresolvable(t *testing.T) {
	tests := []struct {
		name     string
		head     string
		wantPart string
	}{
		{name: "errno 1105", head: "ghost", wantPart: "cannot resolve default branch head"},
		{name: "head is not a hash", head: "garbage", wantPart: "unexpected head"},
		{name: "head is too short", head: "short", wantPart: "unexpected head"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupDatabaseHealthFakeDolt(t, tt.head, "ok", "ok")

			var stdout, stderr bytes.Buffer
			code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--database", "hq"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run() = %d, stderr = %s; want exit 0 with the failure in-band", code, stderr.String())
			}
			got := parseDoltStateOutput(t, stdout.String())
			if got["query_ready"] != "true" || got["database_ready"] != "false" {
				t.Fatalf("output = %q, want query_ready true and database_ready false", stdout.String())
			}
			if !strings.Contains(got["database_error"], `database "hq" default branch head unresolvable`) || !strings.Contains(got["database_error"], tt.wantPart) {
				t.Fatalf("database_error = %q, want a head-resolution error containing %q", got["database_error"], tt.wantPart)
			}
		})
	}
}

// TestManagedDoltHealthCheckForPasswordPathReportsHeadFailure covers AC-2 on
// the direct-driver path (GC_DOLT_PASSWORD set): a head that does not
// resolve is reported in-band, the write probe never runs against the
// broken database, and no dolt binary is invoked.
func TestManagedDoltHealthCheckForPasswordPathReportsHeadFailure(t *testing.T) {
	t.Setenv("GC_DOLT_PASSWORD", "secret")
	oldQuery := managedDoltQueryProbeDirectFn
	oldHead := managedDoltDatabaseHeadDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldReadBack := managedDoltProbeReadBackDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	defer func() {
		managedDoltQueryProbeDirectFn = oldQuery
		managedDoltDatabaseHeadDirectFn = oldHead
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltProbeReadBackDirectFn = oldReadBack
		managedDoltConnectionCountDirectFn = oldConnCount
	}()

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltDatabaseHeadDirectFn = func(_, _, _, database string) (string, error) {
		if database != "hq" {
			t.Errorf("head probe database = %q, want hq", database)
		}
		return "", errors.New("Error 1105 (HY000): no root value found in session")
	}
	probeRan := false
	managedDoltReadOnlyStateDirectFn = func(_, _, _, _ string) (string, error) {
		probeRan = true
		return "false", nil
	}
	managedDoltProbeReadBackDirectFn = func(_, _, _, _ string) error {
		probeRan = true
		return nil
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "2", nil }
	// No dolt binary anywhere on PATH: taking the dolt-CLI path would fail
	// the probe and surface as an error instead of the in-band verdict.
	t.Setenv("PATH", t.TempDir())

	report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", true, managedDoltHealthTarget{Database: "hq"})
	if err != nil {
		t.Fatalf("managedDoltHealthCheckFor() error = %v, want the failure in-band", err)
	}
	if report.DatabaseReady {
		t.Fatalf("DatabaseReady = true, want false")
	}
	if !strings.Contains(report.DatabaseError, "Error 1105") || !strings.Contains(report.DatabaseError, "no root value found in session") {
		t.Fatalf("DatabaseError = %q, want the direct head failure", report.DatabaseError)
	}
	if probeRan {
		t.Fatalf("write probe ran against a database whose head does not resolve")
	}
	if report.ReadOnly != "unknown" {
		t.Fatalf("ReadOnly = %q, want unknown when the database check failed", report.ReadOnly)
	}
}

// TestDoltStateHealthCheckCmdDatabaseMissingOnDisk covers AC-3: the on-disk
// check counts a repository directory whose name differs from the database
// only by the '-'→'_' mapping Dolt applies when exposing a directory as a
// database (the rule managed_backing_store_exists compares by), follows
// symlinks to the repository, and counts a missing data dir as missing —
// even though the SQL checks would succeed.
func TestDoltStateHealthCheckCmdDatabaseMissingOnDisk(t *testing.T) {
	runCheck := func(t *testing.T, database, dataDir string) map[string]string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only", "--database", database, "--data-dir", dataDir}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
		return parseDoltStateOutput(t, stdout.String())
	}
	t.Run("only another database on disk", func(t *testing.T) {
		setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
		dataDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dataDir, "other_db", ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := runCheck(t, "gas_city", dataDir)
		if got["database_ready"] != "false" || !strings.Contains(got["database_error"], "gas_city") || !strings.Contains(got["database_error"], "data dir") {
			t.Fatalf("output maps = %v, want database_ready false with an on-disk error naming gas_city", got)
		}
	})
	t.Run("dash normalized repository matches", func(t *testing.T) {
		setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
		dataDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dataDir, "gas-city", ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := runCheck(t, "gas_city", dataDir); got["database_ready"] != "true" {
			t.Fatalf("output maps = %v, want database_ready true for repository dir gas-city", got)
		}
	})
	t.Run("data dir absent", func(t *testing.T) {
		setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
		dataDir := filepath.Join(t.TempDir(), "gone")
		got := runCheck(t, "gas_city", dataDir)
		if got["database_ready"] != "false" || !strings.Contains(got["database_error"], "data dir") {
			t.Fatalf("output maps = %v, want database_ready false for an absent data dir", got)
		}
	})
	t.Run("symlinked repository matches", func(t *testing.T) {
		setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
		dataDir := t.TempDir()
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(repo, filepath.Join(dataDir, "gas-city")); err != nil {
			t.Fatal(err)
		}
		if got := runCheck(t, "gas_city", dataDir); got["database_ready"] != "true" {
			t.Fatalf("output maps = %v, want database_ready true through a symlinked repository", got)
		}
	})
}

// TestDoltStateHealthCheckCmdDatabaseReady covers AC-4: with a healthy
// database the report keeps today's query_ready, read_only and
// connection_count lines and adds the database verdict.
func TestDoltStateHealthCheckCmdDatabaseReady(t *testing.T) {
	setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only", "--database", "hq"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	want := "query_ready\ttrue\nread_only\tfalse\nconnection_count\t3\ndatabase\thq\ndatabase_ready\ttrue\n"
	if stdout.String() != want {
		t.Fatalf("output = %q, want %q", stdout.String(), want)
	}
}

// TestDoltStateHealthCheckCmdWithoutDatabaseUnchanged covers AC-4: without
// --database the output is byte-identical to the server-only report.
func TestDoltStateHealthCheckCmdWithoutDatabaseUnchanged(t *testing.T) {
	setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	want := "query_ready\ttrue\nread_only\tfalse\nconnection_count\t3\n"
	if stdout.String() != want {
		t.Fatalf("output = %q, want the server-only report %q", stdout.String(), want)
	}
}

func TestDoltStateHealthCheckCmdChecksDataDir(t *testing.T) {
	setupDatabaseHealthFakeDolt(t, "ok", "ok", "ok")
	dataDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only", "--database", "hq", "--data-dir", dataDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	got := parseDoltStateOutput(t, stdout.String())
	if got["database_ready"] != "false" || !strings.Contains(got["database_error"], "absent from data dir") {
		t.Fatalf("output = %q, want on-disk absence reported", stdout.String())
	}

	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"dolt-state", "health-check", "--host", "127.0.0.1", "--port", "3311", "--user", "root", "--check-read-only", "--database", "hq", "--data-dir", dataDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := parseDoltStateOutput(t, stdout.String()); got["database_ready"] != "true" {
		t.Fatalf("output = %q, want database_ready true once hq is on disk", stdout.String())
	}
}
