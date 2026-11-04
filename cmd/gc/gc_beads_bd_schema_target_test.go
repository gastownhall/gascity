package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// schemaTargetFixture is a bash harness around wait_for_bd_runtime_schema with
// two stubs on PATH:
//
//   - dolt answers the three queries the wait makes. The config table is always
//     present (the state the old wait mistook for "ready"), and the
//     schema_migrations cursor is read from a sequence file, one value per read,
//     repeating the last value once the sequence is exhausted.
//   - bd answers `migrate schema --db <path>` the way the real binary does for a
//     throwaway database ("Schema migrated to v<target>"), or fails when
//     probeFails is set, and logs every invocation so a test can prove what the
//     wait ran.
type schemaTargetFixture struct {
	binDir string
	bdLog  string
	script string
}

// schemaTargetVersion is the schema version the stub bd reports it migrates to.
const schemaTargetVersion = 66

func newSchemaTargetFixture(t *testing.T, probeFails bool, cursors ...int) *schemaTargetFixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	src := readGCBeadsBdScript(t)
	binDir := t.TempDir()
	f := &schemaTargetFixture{
		binDir: binDir,
		bdLog:  filepath.Join(binDir, "bd-invocations"),
	}

	seqFile := filepath.Join(binDir, "cursor-sequence")
	var seq strings.Builder
	for _, c := range cursors {
		fmt.Fprintf(&seq, "%d\n", c)
	}
	if err := os.WriteFile(seqFile, []byte(seq.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	readCountFile := filepath.Join(binDir, "cursor-reads")

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
case "$query" in
  *"FROM config"*)
    exit 0
    ;;
  *"information_schema.tables"*"schema_migrations"*)
    printf 'cnt\n1\n'
    exit 0
    ;;
  *"schema_migrations"*)
    n=$(cat %q 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" > %q
    v=$(sed -n "${n}p" %q)
    [ -n "$v" ] || v=$(tail -n 1 %q)
    printf 'cur\n%%s\n' "$v"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, readCountFile, readCountFile, seqFile, seqFile)
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(fakeDolt), 0o755); err != nil {
		t.Fatal(err)
	}

	bdBody := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "migrate schema")
    if [ %t = true ]; then
      echo "bd: cannot create throwaway database" >&2
      exit 1
    fi
    echo "Schema migrated to v%d"
    exit 0
    ;;
esac
exit 0
`, f.bdLog, probeFails, schemaTargetVersion)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdBody), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	f.script = "connect_host() { printf '127.0.0.1'; }\n" +
		"BD_BIN=" + filepath.Join(binDir, "bd") + "\n" +
		strings.Join([]string{
			extractShellFunction(t, src, "valid_sql_name"),
			extractShellFunction(t, src, "server_sql"),
			extractShellFunction(t, src, "bd_runtime_schema_ready"),
			extractShellFunction(t, src, "bd_runtime_schema_cursor"),
			extractShellFunction(t, src, "sleep_ms"),
			extractShellFunction(t, src, "wait_for_bd_runtime_schema"),
		}, "\n") + "\n"
	return f
}

// run executes the wait against database hq and returns its stderr and exit
// status.
func (f *schemaTargetFixture) run(t *testing.T) (stderr string, exit int) {
	t.Helper()
	_, stderr, err := runGCBeadsBdCommand(t, append(os.Environ(),
		"PATH="+f.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOLT_PORT=42188",
		"DOLT_USER=root",
		"DOLT_PASSWORD=",
	), "bash", "-c", f.script+"wait_for_bd_runtime_schema hq\n")
	return stderr, exitCodeOf(t, err)
}

// TestWaitForBdRuntimeSchemaIsNotReadyWhileCursorBehindTarget pins the bug this
// bead fixes: the config table is created by migration 28, so a database whose
// migration stalled at v60 of 66 has a config table and the old wait returned
// success on it. op_init then proceeded against a half-migrated schema.
//
// "Ready" is the cursor reaching the target, and a wait that gives up short of
// it must fail loudly — naming the database, both cursors, and the explicit
// consent step (`bd migrate schema`, beads#5920) the operator has to take. It
// must not run that step itself.
func TestWaitForBdRuntimeSchemaIsNotReadyWhileCursorBehindTarget(t *testing.T) {
	f := newSchemaTargetFixture(t, false, 60)
	stderr, exit := f.run(t)
	if exit == 0 {
		t.Fatalf("wait_for_bd_runtime_schema succeeded with the config table present and the cursor stalled at 60 of 66; a stalled migration must not read as ready")
	}
	for _, want := range []string{"hq", "60", "66", "bd migrate schema"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stalled-migration failure message lacks %q; stderr:\n%s", want, stderr)
		}
	}
}

// TestWaitForBdRuntimeSchemaNeverCompletesTheMigrationItself pins the #5920
// consent model on the wait: the only `bd migrate schema` it may run is the
// target probe against a throwaway --db. A `migrate schema` without --db would
// run against the live database, which is the operator's decision to make.
func TestWaitForBdRuntimeSchemaNeverCompletesTheMigrationItself(t *testing.T) {
	f := newSchemaTargetFixture(t, false, 60)
	f.run(t)

	raw, err := os.ReadFile(f.bdLog)
	if err != nil {
		t.Fatalf("bd was never asked for the target schema version (%v); the wait cannot know what ready means without it", err)
	}
	probes := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.HasPrefix(line, "migrate schema") {
			continue
		}
		probes++
		if !strings.Contains(line, "--db ") {
			t.Errorf("wait ran %q against the live database; only a throwaway --db probe is allowed", line)
		}
	}
	if probes == 0 {
		t.Fatalf("wait never ran the throwaway `bd migrate schema --db` probe; bd calls: %s", raw)
	}
}

// TestWaitForBdRuntimeSchemaSucceedsWhenCursorReachesTarget is the positive
// control: the config table exists from migration 28 on, so success has to wait
// out the migrations after it. The cursor advances 64 -> 65 -> 66 and the wait
// returns only once it is there.
func TestWaitForBdRuntimeSchemaSucceedsWhenCursorReachesTarget(t *testing.T) {
	f := newSchemaTargetFixture(t, false, 64, 65, 66)
	stderr, exit := f.run(t)
	if exit != 0 {
		t.Fatalf("wait_for_bd_runtime_schema failed (exit %d) though the cursor advanced to the target 66; stderr:\n%s", exit, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(f.binDir, "cursor-reads"))
	if err != nil {
		t.Fatalf("cursor was never read: %v", err)
	}
	if reads := strings.TrimSpace(string(raw)); reads != "3" {
		t.Errorf("cursor read %s time(s); the wait returned before observing the cursor reach the target (want 3 reads: 64, 65, 66)", reads)
	}
}

// TestWaitForBdRuntimeSchemaPassesAheadOfTargetCursorThrough pins the other
// direction: a database ahead of this bd (a newer bd migrated it) is schema
// skew, which is the open path's concern, not a stalled migration. The wait
// must not turn it into a failure.
func TestWaitForBdRuntimeSchemaPassesAheadOfTargetCursorThrough(t *testing.T) {
	f := newSchemaTargetFixture(t, false, 70)
	stderr, exit := f.run(t)
	if exit != 0 {
		t.Fatalf("wait_for_bd_runtime_schema failed (exit %d) for a cursor (70) ahead of the target (66); stderr:\n%s", exit, stderr)
	}
}

// TestWaitForBdRuntimeSchemaFailsLoudlyWhenTargetUnknown pins "don't swallow
// errors": if the target cannot be learned there is nothing to compare the
// cursor to, and reading the config table as ready is exactly the old bug.
func TestWaitForBdRuntimeSchemaFailsLoudlyWhenTargetUnknown(t *testing.T) {
	f := newSchemaTargetFixture(t, true, 66)
	stderr, exit := f.run(t)
	if exit == 0 {
		t.Fatalf("wait_for_bd_runtime_schema succeeded without knowing the target schema version")
	}
	if !strings.Contains(stderr, "target") {
		t.Errorf("failure message does not say the target schema version is unknown; stderr:\n%s", stderr)
	}
}
