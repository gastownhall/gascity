package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// gc-beads-k8s exec provider and returns the city, the rig dir, and the
// provider string.
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

// The gc-beads-k8s branch ran bd init once and returned whatever it said, so
// a scope whose tables were left dirty hit beads' circular `bd dolt commit`
// advice there too. It must take the same recovery as the gc-beads-bd branch:
// commit the dirty tables, then run the init again.
func TestInitBeadsForDirWithExecutorK8sRecoversFromDirtyTables(t *testing.T) {
	cityDir, rigDir := k8sDirtyTablesCity(t)

	var commits int
	var gotCityPath, gotDatabase string
	stubCommitDirtyScopeTables(t, func(cityPath, database string) (bool, error) {
		commits++
		gotCityPath, gotDatabase = cityPath, database
		return true, nil
	})
	var calls int
	execute := func(_ string, _ []string, _ ...string) error {
		calls++
		if calls == 1 {
			return errors.New(bdDirtyTablesErrText)
		}
		return nil
	}

	if err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "frontend-db", execute); err != nil {
		t.Fatalf("initBeadsForDirWithExecutor() = %v, want nil after the recovery", err)
	}
	if calls != 2 {
		t.Fatalf("init attempts = %d, want 2 (initial + post-commit retry)", calls)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if gotCityPath != cityDir || gotDatabase != "frontend-db" {
		t.Fatalf("commit target = (%q, %q), want (%q, %q)", gotCityPath, gotDatabase, cityDir, "frontend-db")
	}
}

// A recovery that cannot commit (the real helper refuses an external or
// cluster Dolt) must not turn into a silent success or lose either cause: the
// caller sees the dirty-table refusal and the reason the commit did not run.
func TestInitBeadsForDirWithExecutorK8sSurfacesCommitFailure(t *testing.T) {
	cityDir, rigDir := k8sDirtyTablesCity(t)

	stubCommitDirtyScopeTables(t, func(_, _ string) (bool, error) {
		return false, errors.New("dolt endpoint is external; commit its working set at the endpoint")
	})
	var calls int
	execute := func(_ string, _ []string, _ ...string) error {
		calls++
		return errors.New(bdDirtyTablesErrText)
	}

	err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "frontend-db", execute)
	if err == nil {
		t.Fatal("initBeadsForDirWithExecutor() = nil, want the commit failure surfaced")
	}
	if !strings.Contains(err.Error(), "dolt endpoint is external") {
		t.Fatalf("error %q does not carry the commit failure", err)
	}
	if !isBdInitDirtyTablesError(err) {
		t.Fatalf("error %q lost the dirty-table refusal that started the recovery", err)
	}
	if calls != 1 {
		t.Fatalf("init attempts = %d, want 1 (no retry after a failed commit)", calls)
	}
}

// fakeDirtyOnceBd installs a bd that refuses its first `init` with beads'
// dirty-table text and succeeds afterwards, and returns a reader for the
// number of times it was invoked.
func fakeDirtyOnceBd(t *testing.T, dir string) func() int {
	t.Helper()
	countPath := filepath.Join(dir, "bd-invocations")
	script := "#!/bin/sh\n" +
		"echo x >> \"" + countPath + "\"\n" +
		"if [ \"$(wc -l < \"" + countPath + "\")\" -le 1 ]; then\n" +
		"  echo '" + bdDirtyTablesErrText + "' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exit 0\n"
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

// initDefaultRigBdStore is the init path for scopes the provider does not
// own, and it wrapped bd's refusal as `bd init: ...` with no recovery. It must
// commit the dirty tables and run bd init again.
func TestInitDefaultRigBdStoreRecoversFromDirtyTables(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	invocations := fakeDirtyOnceBd(t, city)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"r1"}`)

	var commits int
	var gotDatabase string
	stubCommitDirtyScopeTables(t, func(_, database string) (bool, error) {
		commits++
		gotDatabase = database
		return true, nil
	})

	// The init goes on to open the store; only the bd calls and the commit are
	// the subject, so a later failure is not asserted on, but it must not be
	// the dirty-table refusal.
	err := initDefaultRigBdStore(city, rig, "r1", "r1")
	if err != nil && isBdInitDirtyTablesError(err) {
		t.Fatalf("initDefaultRigBdStore() = %v, want the dirty-table refusal recovered", err)
	}
	if got := invocations(); got != 2 {
		t.Fatalf("bd init attempts = %d, want 2 (initial + post-commit retry)", got)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if gotDatabase != "r1" {
		t.Fatalf("commit database = %q, want %q", gotDatabase, "r1")
	}
}

func TestInitDefaultRigBdStoreSurfacesCommitFailure(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	invocations := fakeDirtyOnceBd(t, city)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"r1"}`)
	stubCommitDirtyScopeTables(t, func(_, _ string) (bool, error) {
		return false, errors.New("no managed Dolt port resolvable")
	})

	err := initDefaultRigBdStore(city, rig, "r1", "r1")
	if err == nil || !strings.Contains(err.Error(), "no managed Dolt port resolvable") {
		t.Fatalf("initDefaultRigBdStore() = %v, want the commit failure surfaced", err)
	}
	if got := invocations(); got != 1 {
		t.Fatalf("bd init attempts = %d, want 1 (no retry after a failed commit)", got)
	}
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

// A post-commit re-init that reports "already initialized" means the recovery
// worked, as the gc-beads-bd branch already treats it. The gc-beads-k8s branch
// must not turn a recovered scope into a hard init failure.
func TestInitBeadsForDirWithExecutorK8sTreatsAlreadyInitializedRecoveryAsSuccess(t *testing.T) {
	cityDir, rigDir := k8sDirtyTablesCity(t)

	var commits int
	stubCommitDirtyScopeTables(t, func(_, _ string) (bool, error) {
		commits++
		return true, nil
	})
	var calls int
	execute := func(_ string, _ []string, _ ...string) error {
		calls++
		if calls == 1 {
			return errors.New(bdDirtyTablesErrText)
		}
		return errors.New("bd init: already initialized")
	}

	if err := initBeadsForDirWithExecutor(cityDir, rigDir, "fe", "frontend-db", execute); err != nil {
		t.Fatalf("initBeadsForDirWithExecutor() = %v, want nil: an already-initialized answer after the recovery is success", err)
	}
	if calls != 2 {
		t.Fatalf("init attempts = %d, want 2 (initial + post-commit retry)", calls)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
}

// fakeDirtyThenAlreadyInitializedBd installs a bd that refuses its first `init`
// with beads' dirty-table text and answers "already initialized" to every call
// after it, and returns a reader for the number of times it was invoked.
func fakeDirtyThenAlreadyInitializedBd(t *testing.T, dir string) func() int {
	t.Helper()
	countPath := filepath.Join(dir, "bd-invocations")
	script := "#!/bin/sh\n" +
		"echo x >> \"" + countPath + "\"\n" +
		"if [ \"$(wc -l < \"" + countPath + "\")\" -le 1 ]; then\n" +
		"  echo '" + bdDirtyTablesErrText + "' >&2\n" +
		"else\n" +
		"  echo 'bd init: already initialized' >&2\n" +
		"fi\n" +
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

// initDefaultRigBdStore already treated an "already initialized" answer to the
// first bd init as success. The same answer to the post-commit retry means the
// recovery worked, so it must not come back as a `bd init: ...` failure.
func TestInitDefaultRigBdStoreTreatsAlreadyInitializedRecoveryAsSuccess(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	invocations := fakeDirtyThenAlreadyInitializedBd(t, city)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"r1"}`)

	var commits int
	stubCommitDirtyScopeTables(t, func(_, _ string) (bool, error) {
		commits++
		return true, nil
	})

	// The init goes on to open the store, so a later failure is not asserted
	// on; it must not be the already-initialized answer the retry gave.
	err := initDefaultRigBdStore(city, rig, "r1", "r1")
	if err != nil && strings.Contains(err.Error(), "already initialized") {
		t.Fatalf("initDefaultRigBdStore() = %v, want the already-initialized answer to the retry treated as success", err)
	}
	if got := invocations(); got != 2 {
		t.Fatalf("bd init attempts = %d, want 2 (initial + post-commit retry)", got)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
}
