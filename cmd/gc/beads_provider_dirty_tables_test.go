package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// bdDirtyTablesErrText is the refusal beads emits (verbatim, modulo the table
// list) when pending schema migrations would alter tables that already carry
// uncommitted working-set changes. Reproduced here so the classifier is tested
// against the real upstream text rather than a paraphrase of it.
const bdDirtyTablesErrText = "bd init: pending schema migrations alter pre-existing dirty tables: " +
	"config, dependencies; run 'bd dolt commit' to commit the working set at the current schema, " +
	"then re-run the migration (gastownhall/beads#4566)"

func TestIsBdInitDirtyTablesError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "verbatim beads refusal", err: errors.New(bdDirtyTablesErrText), want: true},
		{
			name: "ignored-migration variant",
			err: errors.New("pending ignored schema migrations alter pre-existing dirty tables: " +
				"issue_snapshots"),
			want: true,
		},
		{
			name: "wrapped by an exec provider",
			err:  fmt.Errorf("gc-beads-bd init: %w", errors.New(bdDirtyTablesErrText)),
			want: true,
		},
		{
			name: "unrelated migration failure",
			err:  errors.New("bd init: Unknown column 'agent_state' in 'issues'"),
			want: false,
		},
		{name: "already initialized", err: errors.New("bd init: already initialized"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBdInitDirtyTablesError(tt.err); got != tt.want {
				t.Fatalf("isBdInitDirtyTablesError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// stubCommitDirtyScopeTables swaps the managed-Dolt commit seam for the test's
// own and restores it afterwards.
func stubCommitDirtyScopeTables(t *testing.T, fn func(cityPath, database string) (bool, error)) {
	t.Helper()
	prev := commitDirtyScopeTables
	commitDirtyScopeTables = fn
	t.Cleanup(func() { commitDirtyScopeTables = prev })
}

// The dirty set observed in the field shrank 9 -> 1 -> 0: bd init makes partial
// progress each pass and re-dirties tables, so a single commit does not clear
// it. Recovery must keep committing until init stops refusing.
func TestRecoverBdInitFromDirtyTablesLoopsUntilClean(t *testing.T) {
	var commits int
	stubCommitDirtyScopeTables(t, func(_, database string) (bool, error) {
		commits++
		if database != "gsp" {
			t.Fatalf("commit database = %q, want %q", database, "gsp")
		}
		return true, nil
	})

	var reinits int
	err := recoverBdInitFromDirtyTables("/city", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		reinits++
		if reinits < 2 {
			return errors.New(bdDirtyTablesErrText)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("recoverBdInitFromDirtyTables() = %v, want nil", err)
	}
	if commits != 2 {
		t.Fatalf("commit rounds = %d, want 2", commits)
	}
	if reinits != 2 {
		t.Fatalf("re-init attempts = %d, want 2", reinits)
	}
}

// A commit that reports nothing left to commit cannot make further progress:
// looping again would spin against an unchanged database.
func TestRecoverBdInitFromDirtyTablesStopsWhenNothingLeftToCommit(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	var commits, reinits int
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) {
		commits++
		return false, nil
	})

	err := recoverBdInitFromDirtyTables("/city", "gsp", initErr, func() error {
		reinits++
		return nil
	})
	if !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want the original init error", err)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if reinits != 0 {
		t.Fatalf("re-init attempts = %d, want 0", reinits)
	}
}

// A non-dirty-table failure on the retry is the real outcome and must surface
// unchanged rather than being retried as if it were still the deadlock.
func TestRecoverBdInitFromDirtyTablesSurfacesUnrelatedRetryFailure(t *testing.T) {
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) { return true, nil })

	other := errors.New("bd init: connection refused")
	err := recoverBdInitFromDirtyTables("/city", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		return other
	})
	if !errors.Is(err, other) {
		t.Fatalf("error = %v, want %v", err, other)
	}
}

// When committing itself fails, the operator needs both the refusal and the
// reason the documented remedy could not be applied.
func TestRecoverBdInitFromDirtyTablesSurfacesCommitFailure(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	commitErr := errors.New("connect to managed Dolt: dial tcp: connection refused")
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) { return false, commitErr })

	err := recoverBdInitFromDirtyTables("/city", "gsp", initErr, func() error {
		t.Fatal("re-init must not run after a failed commit")
		return nil
	})
	if !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want it to wrap the init refusal", err)
	}
	if !errors.Is(err, commitErr) {
		t.Fatalf("error = %v, want it to wrap the commit failure", err)
	}
}

// A database that never stops refusing must not spin forever.
func TestRecoverBdInitFromDirtyTablesBoundsRounds(t *testing.T) {
	var commits int
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) {
		commits++
		return true, nil
	})

	err := recoverBdInitFromDirtyTables("/city", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		return errors.New(bdDirtyTablesErrText)
	})
	if err == nil {
		t.Fatal("recoverBdInitFromDirtyTables() = nil, want an error")
	}
	if commits != maxBdInitDirtyTableRounds {
		t.Fatalf("commit rounds = %d, want %d", commits, maxBdInitDirtyTableRounds)
	}
}

// An empty database name leaves nothing to commit against, so recovery must
// decline rather than guess at a target.
func TestRecoverBdInitFromDirtyTablesWithoutDatabase(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) {
		t.Fatal("commit must not run without a database name")
		return false, nil
	})

	if err := recoverBdInitFromDirtyTables("/city", "  ", initErr, func() error {
		t.Fatal("re-init must not run without a database name")
		return nil
	}); !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want the original init error", err)
	}
}

// End-to-end through the entry point `gc rig add` actually calls: a rig whose
// fresh database comes back dirty must be recovered in-process instead of
// handing the operator beads' circular "run 'bd dolt commit'" advice.
func TestInitBeadsForDirWithExecutorRecoversFromDirtyTables(t *testing.T) {
	cityDir := t.TempDir()
	cityConfig := `[workspace]
name = "demo"

[beads]
provider = "bd"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	// This recovery belongs to an existing direct-server city. Without the
	// persisted city marker, a fresh scope correctly takes the new
	// provider-owned proxied-local path and never reaches legacy recovery.
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityDir, "rigs", "gascity-packs")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var commits int
	var gotCityPath, gotDatabase string
	stubCommitDirtyScopeTables(t, func(cityPath, database string) (bool, error) {
		commits++
		gotCityPath, gotDatabase = cityPath, database
		return true, nil
	})

	// Fail the first init the way beads does, then stop with a sentinel so the
	// assertion stays on the recovery and never reaches the real store
	// finalization.
	stopAfterRecovery := errors.New("stop after recovery")
	var calls int
	execute := func(_ string, _ []string, _ ...string) error {
		calls++
		if calls == 1 {
			return errors.New(bdDirtyTablesErrText)
		}
		return stopAfterRecovery
	}

	err := initBeadsForDirWithExecutor(cityDir, rigDir, "gsp", "gsp", execute)
	if !errors.Is(err, stopAfterRecovery) {
		t.Fatalf("initBeadsForDirWithExecutor error = %v, want %v", err, stopAfterRecovery)
	}
	if calls != 2 {
		t.Fatalf("bd init attempts = %d, want 2 (initial + post-commit retry)", calls)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if gotCityPath != cityDir {
		t.Fatalf("commit cityPath = %q, want %q", gotCityPath, cityDir)
	}
	if gotDatabase != "gsp" {
		t.Fatalf("commit database = %q, want %q", gotDatabase, "gsp")
	}
}

// The commit must carry an explicit --author: Dolt aborts a commit with an
// empty committer identity, and a fresh host is not guaranteed to have a
// global one configured. `USE` has to land before the `CALL` so the procedure
// runs against the scope database rather than whatever the session defaults to.
func TestDirtyScopeTablesCommitSQL(t *testing.T) {
	got := dirtyScopeTablesCommitSQL("gsp")

	wantAuthor := "'--author', 'gascity-builder <builder@gascity.local>'"
	if !strings.Contains(got, wantAuthor) {
		t.Fatalf("commit SQL = %q, want it to contain %q", got, wantAuthor)
	}
	if !strings.Contains(got, "DOLT_COMMIT('-A'") {
		t.Fatalf("commit SQL = %q, want it to stage every table with -A", got)
	}
	if !strings.Contains(got, dirtyScopeTablesCommitMessage) {
		t.Fatalf("commit SQL = %q, want it to contain message %q", got, dirtyScopeTablesCommitMessage)
	}

	use := strings.Index(got, "USE "+managedDoltQuoteIdent("gsp")+";")
	call := strings.Index(got, "CALL DOLT_COMMIT")
	if use != 0 {
		t.Fatalf("commit SQL = %q, want it to start with the USE statement", got)
	}
	if call < use {
		t.Fatalf("commit SQL = %q, want USE before CALL", got)
	}
}

// The identifier is quoted, and neither fixed literal contains a quote
// character, so the statement cannot be broken by the database name.
func TestDirtyScopeTablesCommitSQLQuotesDatabase(t *testing.T) {
	got := dirtyScopeTablesCommitSQL("we`ird")
	if !strings.HasPrefix(got, "USE `we``ird`;") {
		t.Fatalf("commit SQL = %q, want the database identifier escaped", got)
	}
}

// TestManagedDoltDirtyTablesWithPasswordUseDirectConnection pins that a
// configured managed password sends the dirty-table count and commit through
// the pooled driver connection, not the dolt CLI, where a `default` dolt
// profile's password would outrank DOLT_CLI_PASSWORD (see
// runManagedDoltSQLContext).
func TestManagedDoltDirtyTablesWithPasswordUseDirectConnection(t *testing.T) {
	doltInvocations := installFailingDoltRecorder(t)
	t.Setenv("GC_DOLT_PASSWORD", "secret")
	prevCount, prevCommit := managedDoltDirtyTableCountDirectFn, managedDoltCommitDirtyTablesDirectFn
	t.Cleanup(func() {
		managedDoltDirtyTableCountDirectFn, managedDoltCommitDirtyTablesDirectFn = prevCount, prevCommit
	})
	var calls []string
	managedDoltDirtyTableCountDirectFn = func(port, database string) (int, error) {
		calls = append(calls, fmt.Sprintf("count %q %q", port, database))
		return 3, nil
	}
	managedDoltCommitDirtyTablesDirectFn = func(port, database string) error {
		calls = append(calls, fmt.Sprintf("commit %q %q", port, database))
		return nil
	}

	if n, err := managedDoltDirtyTableCount("3311", "gsp"); err != nil || n != 3 {
		t.Fatalf("managedDoltDirtyTableCount() = %d, %v; want 3 from the direct count", n, err)
	}
	if err := managedDoltCommitDirtyTables("3311", "gsp"); err != nil {
		t.Fatalf("managedDoltCommitDirtyTables() error = %v", err)
	}
	want := []string{`count "3311" "gsp"`, `commit "3311" "gsp"`}
	if !slices.Equal(calls, want) {
		t.Fatalf("direct calls = %q, want %q", calls, want)
	}
	if invocations := doltInvocations(); invocations != "" {
		t.Fatalf("dolt CLI ran with GC_DOLT_PASSWORD set: %s", invocations)
	}
}

// TestManagedDoltDirtyTablesWithPasswordAgainstRealServer counts and commits a
// dirty working set on a real passworded server while a failing dolt is first
// on PATH, so both can only have gone over the driver connection.
func TestManagedDoltDirtyTablesWithPasswordAgainstRealServer(t *testing.T) {
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	doltInvocations := installFailingDoltRecorder(t)
	_, port, _, cleanup := startPasswordedDoltServer(t, "",
		"CREATE DATABASE `gsp`; USE `gsp`; CREATE TABLE dirty (k INT PRIMARY KEY); INSERT INTO dirty VALUES (1);")
	defer cleanup()
	portText := strconv.Itoa(port)

	if n, err := managedDoltDirtyTableCount(portText, "gsp"); err != nil || n == 0 {
		t.Fatalf("managedDoltDirtyTableCount() before the commit = %d, %v; want dirty tables", n, err)
	}
	if err := managedDoltCommitDirtyTables(portText, "gsp"); err != nil {
		t.Fatalf("managedDoltCommitDirtyTables() error = %v", err)
	}
	if n, err := managedDoltDirtyTableCount(portText, "gsp"); err != nil || n != 0 {
		t.Fatalf("managedDoltDirtyTableCount() after the commit = %d, %v; want 0", n, err)
	}
	db, err := managedDoltOpenDatabase("", portText, "root", "gsp")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase() error = %v", err)
	}
	var committer, message string
	if err := db.QueryRow("SELECT committer, message FROM dolt_log LIMIT 1").Scan(&committer, &message); err != nil {
		t.Fatalf("reading the recovery commit from dolt_log: %v", err)
	}
	if committer != "gascity-builder" || message != dirtyScopeTablesCommitMessage {
		t.Fatalf("recovery commit = %q by %q, want %q by gascity-builder", message, committer, dirtyScopeTablesCommitMessage)
	}
	// commitDirtyScopeTablesViaManagedDolt reads "nothing to commit" as an
	// already-clean database, so the driver error must still carry dolt's text.
	err = managedDoltCommitDirtyTables(portText, "gsp")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
		t.Fatalf("managedDoltCommitDirtyTables() on a clean working set error = %v, want dolt's nothing-to-commit refusal", err)
	}
	if invocations := doltInvocations(); invocations != "" {
		t.Fatalf("dolt CLI ran with GC_DOLT_PASSWORD set: %s", invocations)
	}
}

func TestParseSmokeCount(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    int
		wantErr bool
	}{
		{name: "header and value", out: "cnt\n9\n", want: 9},
		{name: "zero rows dirty", out: "cnt\n0\n", want: 0},
		{name: "trailing blank lines", out: "cnt\n3\n\n\n", want: 3},
		{name: "empty output", out: "", wantErr: true},
		{name: "whitespace only", out: "   \n\t\n", wantErr: true},
		{name: "trailing non-numeric warning", out: "cnt\n3\nWarning: connection reset\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSmokeCount(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSmokeCount(%q) = %d, want an error", tt.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSmokeCount(%q) = %v, want %d", tt.out, err, tt.want)
			}
			if got != tt.want {
				t.Fatalf("parseSmokeCount(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}

// k8sDirtyTablesCity lays out a city whose scope init goes through the
// gc-beads-k8s exec provider and returns the city dir and the rig dir (a rig
// whose Dolt endpoint differs from the city's).
func k8sDirtyTablesCity(t *testing.T) (cityDir, rigDir string) {
	t.Helper()
	clearGCEnv(t)
	cityDir = t.TempDir()
	rigDir = filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[dolt]
host = "city-db.example.com"
port = 3307

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig-db.example.com"
dolt_port = "4407"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "gc-beads-k8s")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	return cityDir, rigDir
}

// The recovery's count query names the database it counts. Whether Dolt
// resolves that qualifier or reads the session's own database decides if the
// count is right: a session-scoped read of a clean session database would
// report 0 for a dirty target and the recovery would stop as "nothing to
// commit". The connection here defaults to a different database than the one
// being counted, so only a database-qualified read sees the target's tables.
func TestManagedDoltDirtyTableCountIsDatabaseQualified(t *testing.T) {
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	_, port, _, cleanup := startPasswordedDoltServer(t, "",
		"CREATE DATABASE `other`; CREATE DATABASE `gsp`; "+
			"USE `gsp`; CREATE TABLE dirty (k INT PRIMARY KEY); INSERT INTO dirty VALUES (1);")
	defer cleanup()
	portText := strconv.Itoa(port)

	db, err := managedDoltOpenDatabase("", portText, "root", "other")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase(other) error = %v", err)
	}
	var viaOtherSession int
	if err := db.QueryRow(managedDoltDirtyTableCountSQL("gsp")).Scan(&viaOtherSession); err != nil {
		t.Fatalf("counting gsp from a session on `other`: %v", err)
	}
	if viaOtherSession == 0 {
		t.Fatalf("%q from a session on `other` = 0, want gsp's dirty tables: the count is session-scoped, not database-qualified",
			managedDoltDirtyTableCountSQL("gsp"))
	}
	var viaOwnSession int
	if err := db.QueryRow("SELECT COUNT(*) FROM dolt_status").Scan(&viaOwnSession); err != nil {
		t.Fatalf("counting the session's own database: %v", err)
	}
	if viaOwnSession != 0 {
		t.Fatalf("session database `other` reports %d dirty tables, want 0 (the control must be clean)", viaOwnSession)
	}
}

// failOnDirtyTablesCommit is the tripwire on the commit seam. The two init
// paths below do not run a Dolt gc manages, so reaching the commit at all is
// the defect: gc would be committing a working set on a Dolt it does not own.
func failOnDirtyTablesCommit(t *testing.T) {
	t.Helper()
	stubCommitDirtyScopeTables(t, func(cityPath, database string) (bool, error) {
		t.Errorf("commitDirtyScopeTables(%q, %q) was reached: gc commits a working set only on a Dolt it manages", cityPath, database)
		return false, errors.New("the commit seam must not be reached on this init path")
	})
}

// k8sInitRecorder is an exec-provider executor that records how often init ran
// and the environment of the last run, and answers with err.
type k8sInitRecorder struct {
	calls int
	env   []string
	err   error
}

func (r *k8sInitRecorder) execute(_ string, env []string, _ ...string) error {
	r.calls++
	r.env = env
	return r.err
}

// assertDirtyTablesRemedy checks the parts of the remedy text common to both
// init paths. remedy is the text that follows the original refusal.
func assertDirtyTablesRemedy(t *testing.T, remedy, wantDatabase string) {
	t.Helper()
	for _, want := range []string{
		wantDatabase,
		"gc commits a working set only on a Dolt it manages",
		"CALL DOLT_COMMIT('-A', '-m'",
		"bd dolt commit",
		"same refusal",
	} {
		if !strings.Contains(remedy, want) {
			t.Errorf("remedy %q does not contain %q", remedy, want)
		}
	}
}

// The gc-beads-k8s init branch is a Dolt gc does not manage, so a dirty-table
// refusal there cannot be recovered by committing. It must try init once and
// return bd's refusal, intact and first, with the remedy and the rig's own
// endpoint after it.
func TestInitBeadsForDirWithExecutorK8sReturnsRemedyForDirtyTables(t *testing.T) {
	cityDir, rigDir := k8sDirtyTablesCity(t)
	failOnDirtyTablesCommit(t)
	refusal := errors.New(bdDirtyTablesErrText)
	rec := &k8sInitRecorder{err: refusal}

	err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "frontend-db", rec.execute)
	if err == nil {
		t.Fatal("initBeadsForDirWithExecutor() = nil, want bd's refusal returned")
	}
	if rec.calls != 1 {
		t.Errorf("init attempts = %d, want 1 (no recovery, no retry)", rec.calls)
	}
	if !errors.Is(err, refusal) {
		t.Errorf("error %q does not wrap bd's refusal with %%w", err)
	}
	if !isBdInitDirtyTablesError(err) {
		t.Errorf("error %q is no longer a dirty-table refusal", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, bdDirtyTablesErrText) {
		t.Fatalf("error %q does not start with bd's refusal %q", msg, bdDirtyTablesErrText)
	}
	remedy := strings.TrimPrefix(msg, bdDirtyTablesErrText)
	assertDirtyTablesRemedy(t, remedy, `"frontend-db"`)
	if !strings.Contains(remedy, "rig-db.example.com:4407") {
		t.Errorf("remedy %q does not name the rig's Dolt endpoint rig-db.example.com:4407", remedy)
	}
	if strings.Contains(msg, "city-db.example.com") {
		t.Errorf("error %q names the city's Dolt endpoint, not the rig's", msg)
	}
	if strings.Contains(msg, "gc-beads-k8s") {
		t.Errorf("error %q names the provider", msg)
	}
	// The endpoint in the message is the one the init really ran against.
	if got := envListValue(rec.env, "GC_DOLT_HOST"); got != "rig-db.example.com" {
		t.Errorf("init env GC_DOLT_HOST = %q, want rig-db.example.com", got)
	}
	if got := envListValue(rec.env, "GC_DOLT_PORT"); got != "4407" {
		t.Errorf("init env GC_DOLT_PORT = %q, want 4407", got)
	}
}

// An empty database name must never reach the message as an empty quoted name.
func TestInitBeadsForDirWithExecutorK8sRemedyWithoutDatabase(t *testing.T) {
	cityDir, rigDir := k8sDirtyTablesCity(t)
	failOnDirtyTablesCommit(t)
	rec := &k8sInitRecorder{err: errors.New(bdDirtyTablesErrText)}

	err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "", rec.execute)
	if err == nil || !isBdInitDirtyTablesError(err) {
		t.Fatalf("initBeadsForDirWithExecutor() = %v, want a dirty-table refusal", err)
	}
	remedy := strings.TrimPrefix(err.Error(), bdDirtyTablesErrText)
	assertDirtyTablesRemedy(t, remedy, "the scope's database")
	if strings.Contains(remedy, `""`) {
		t.Errorf("remedy %q carries an empty quoted database name", remedy)
	}
}

// The init env carries the Dolt credentials to the provider; none of them may
// reach the error, which is printed and logged.
func TestInitBeadsForDirWithExecutorK8sRemedyOmitsCredentials(t *testing.T) {
	cityDir, rigDir := k8sDirtyTablesCity(t)
	failOnDirtyTablesCommit(t)
	secrets := map[string]string{
		"GC_DOLT_PASSWORD":              "sentinel-gc-dolt-password",
		"BEADS_DOLT_PASSWORD":           "sentinel-beads-dolt-password",
		"BEADS_CREDENTIALS_FILE":        "/sentinel/credentials-file",
		"BEADS_DOLT_CREDENTIAL_COMMAND": "sentinel-credential-command",
		registryCredentialProviderEnv:   "sentinel-credential-provider",
	}
	for key, value := range secrets {
		t.Setenv(key, value)
	}
	rec := &k8sInitRecorder{err: errors.New(bdDirtyTablesErrText)}

	err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "frontend-db", rec.execute)
	if err == nil || !isBdInitDirtyTablesError(err) {
		t.Fatalf("initBeadsForDirWithExecutor() = %v, want a dirty-table refusal", err)
	}
	if !strings.Contains(err.Error(), "CALL DOLT_COMMIT") {
		t.Fatalf("error %q carries no remedy, so the credential check below proves nothing", err)
	}
	carried := 0
	for key, value := range secrets {
		if !slices.ContainsFunc(rec.env, func(kv string) bool { return strings.Contains(kv, value) }) {
			t.Logf("init env does not carry %s", key)
			continue
		}
		carried++
		if strings.Contains(err.Error(), value) {
			t.Errorf("error %q carries the %s value", err, key)
		}
	}
	if carried == 0 {
		t.Fatal("the init env carried none of the credential sentinels, so the check above proves nothing")
	}
}

// What the init returns when nothing was refused is unchanged: success is nil,
// and a failure that is not the dirty-table refusal is the very same error.
func TestInitBeadsForDirWithExecutorK8sLeavesOtherOutcomesAlone(t *testing.T) {
	cityDir, rigDir := k8sDirtyTablesCity(t)
	failOnDirtyTablesCommit(t)

	ok := &k8sInitRecorder{}
	if err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "frontend-db", ok.execute); err != nil {
		t.Errorf("initBeadsForDirWithExecutor() = %v, want nil", err)
	}
	if ok.calls != 1 {
		t.Errorf("init attempts = %d, want 1", ok.calls)
	}

	other := errors.New("bd init: Unknown column 'agent_state' in 'issues'")
	failing := &k8sInitRecorder{err: other}
	if err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "frontend-db", failing.execute); err != other { //nolint:errorlint // identity is the assertion
		t.Errorf("initBeadsForDirWithExecutor() = %v, want the same error value, unchanged", err)
	}
	if failing.calls != 1 {
		t.Errorf("init attempts = %d, want 1", failing.calls)
	}
}

// fakeRefusingBd installs a bd that refuses every `init` with beads'
// dirty-table text (or with failure, when given), and returns a reader for the
// number of times it was invoked.
func fakeRefusingBd(t *testing.T, dir, failure string) func() int {
	t.Helper()
	countPath := filepath.Join(dir, "bd-invocations")
	script := "#!/bin/sh\n" +
		"echo x >> \"" + countPath + "\"\n" +
		"echo '" + failure + "' >&2\n" +
		"exit 1\n"
	fake := filepath.Join(dir, "bd")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", fake)
	return func() int {
		data, err := os.ReadFile(countPath)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "\n")
	}
}

// initDefaultRigBdStore runs bd against a Dolt server bd itself resolved, and
// gc passes it no endpoint. gc cannot say which server holds the dirty tables,
// so it must not commit: it runs init once and wraps bd's refusal as
// `bd init: ...`, with the remedy and the scope's .beads/ directory after it.
func TestInitDefaultRigBdStoreReturnsRemedyForDirtyTables(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	invocations := fakeRefusingBd(t, city, bdDirtyTablesErrText)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"r1"}`)
	failOnDirtyTablesCommit(t)

	err := initDefaultRigBdStore(city, rig, "r1", "r1")
	if err == nil {
		t.Fatal("initDefaultRigBdStore() = nil, want bd's refusal returned")
	}
	if got := invocations(); got != 1 {
		t.Errorf("bd init attempts = %d, want 1 (no recovery, no retry)", got)
	}
	if !isBdInitDirtyTablesError(err) {
		t.Errorf("error %q is no longer a dirty-table refusal", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "bd init: ") {
		t.Errorf("error %q is not wrapped as `bd init: ...`", msg)
	}
	const refusalEnd = "(gastownhall/beads#4566)"
	cut := strings.Index(msg, refusalEnd)
	if cut < 0 {
		t.Fatalf("error %q does not carry bd's refusal first", msg)
	}
	remedy := msg[cut+len(refusalEnd):]
	assertDirtyTablesRemedy(t, remedy, `"r1"`)
	for _, want := range []string{
		filepath.Join(rig, ".beads"),
		"does not know which Dolt server bd resolved",
	} {
		if !strings.Contains(remedy, want) {
			t.Errorf("remedy %q does not contain %q", remedy, want)
		}
	}
	if m := regexp.MustCompile(`:\d{2,5}\b`).FindString(remedy); m != "" {
		t.Errorf("remedy %q names a host and port (%q); gc passed bd no endpoint", remedy, m)
	}
}

// Any other bd init failure keeps the `bd init: ...` wrap and gains no remedy.
func TestInitDefaultRigBdStoreLeavesOtherFailuresAlone(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	invocations := fakeRefusingBd(t, city, "Error: Unknown column agent_state in issues")
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"r1"}`)
	failOnDirtyTablesCommit(t)

	err := initDefaultRigBdStore(city, rig, "r1", "r1")
	if err == nil {
		t.Fatal("initDefaultRigBdStore() = nil, want the init failure returned")
	}
	if got := invocations(); got != 1 {
		t.Errorf("bd init attempts = %d, want 1", got)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "bd init: ") || !strings.Contains(msg, "Unknown column agent_state") {
		t.Errorf("error %q is not the `bd init: ...` wrap of bd's failure", msg)
	}
	if strings.Contains(msg, "DOLT_COMMIT") || isBdInitDirtyTablesError(err) {
		t.Errorf("error %q carries a dirty-table remedy for a failure that is not one", msg)
	}
}

// The endpoint phrase names only the host and port the init env carries, and
// falls back to the provider's default when it carries neither.
func TestDirtyTablesEndpointText(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{name: "host and port", env: []string{"GC_DOLT_HOST=rig-db.example.com", "GC_DOLT_PORT=4407"}, want: "the Dolt at rig-db.example.com:4407"},
		{name: "host only", env: []string{"GC_DOLT_HOST=rig-db.example.com"}, want: "the Dolt at rig-db.example.com"},
		{name: "port only", env: []string{"GC_DOLT_PORT=4407"}, want: "the Dolt on port 4407"},
		{name: "neither", env: []string{"PATH=/usr/bin"}, want: "the provider's default Dolt"},
		{name: "empty values", env: []string{"GC_DOLT_HOST=", "GC_DOLT_PORT="}, want: "the provider's default Dolt"},
		{name: "last entry wins", env: []string{"GC_DOLT_HOST=old", "GC_DOLT_HOST=rig-db.example.com", "GC_DOLT_PORT=4407"}, want: "the Dolt at rig-db.example.com:4407"},
		{name: "credentials are not read", env: []string{"GC_DOLT_PASSWORD=hunter2", "BEADS_DOLT_PASSWORD=hunter2"}, want: "the provider's default Dolt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dirtyTablesEndpointText(tt.env); got != tt.want {
				t.Fatalf("dirtyTablesEndpointText(%v) = %q, want %q", tt.env, got, tt.want)
			}
		})
	}
}
