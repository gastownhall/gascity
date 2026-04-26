// Package bd_test validates the gc-beads-bd lifecycle script's
// set_issue_prefix_in_dolt_config helper — the fix for issue #1232.
//
// The bug: bd 1.0.3+ refuses `bd config set issue_prefix` with
// "issue_prefix is reserved for setup", but `bd create` reads the
// config table at runtime and fails with "database not initialized:
// issue_prefix config is missing" when the row is absent. The previous
// lifecycle script swallowed bd's rejection with `2>/dev/null || true`,
// leaving the row missing and breaking every downstream `bd create`.
//
// These tests stub `run_bd_pinned`, `server_sql`, and
// `server_sql_retry` to drive the helper through every branch without
// requiring a real Dolt server.
package bd_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath returns the absolute path of gc-beads-bd.sh from this test's
// position in the source tree.
func scriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	p := filepath.Join(dir, "assets", "scripts", "gc-beads-bd.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("script not found at %s: %v", p, err)
	}
	return p
}

// runHelper sources the script and invokes set_issue_prefix_in_dolt_config
// with the given args, after the supplied bash setup has installed any
// stubs. Returns combined output and the command's exit error (nil on 0).
func runHelper(t *testing.T, setup, dir, db, prefix string) (string, error) {
	t.Helper()
	body := `set -u
# Required globals the script's other helpers reference.
export DOLT_PORT="${DOLT_PORT:-3306}"
export DOLT_USER="${DOLT_USER:-root}"
export DOLT_PASSWORD="${DOLT_PASSWORD:-}"
# Avoid touching real Dolt for connect_host's gc-helper probe path.
unset GC_BIN GC_HELPER_BIN

# Source under a guard that disables the entrypoint dispatch at the
# bottom of the script. We only want the function definitions.
__BD_SCRIPT_TEST_SOURCE=1
. "$1"

# The script enables 'set -e' globally; relax it here so a failing
# helper invocation in the test harness does not kill the shell before
# we print the EXIT marker.
set +e
` + setup + `
set_issue_prefix_in_dolt_config "$2" "$3" "$4"
echo "EXIT=$?"
`
	cmd := exec.Command("bash", "-c", body, "bash", scriptPath(t), dir, db, prefix)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestHelperDefined confirms the helper is reachable via sourcing —
// catches refactors that accidentally drop the function definition.
func TestHelperDefined(t *testing.T) {
	body := `__BD_SCRIPT_TEST_SOURCE=1; . "$1"; declare -F set_issue_prefix_in_dolt_config && echo OK`
	cmd := exec.Command("bash", "-c", body, "bash", scriptPath(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("source script: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Errorf("set_issue_prefix_in_dolt_config not defined after sourcing\n%s", out)
	}
}

// TestBdCLIPathSucceeds: when bd accepts the call, the helper returns 0
// without ever invoking SQL fallback. Guards against a regression that
// always takes the fallback path even on bd versions that work.
func TestBdCLIPathSucceeds(t *testing.T) {
	setup := `
sql_called=0
run_bd_pinned() { return 0; }
server_sql() { sql_called=1; return 0; }
server_sql_retry() { sql_called=1; return 0; }
`
	out, err := runHelper(t, setup, "/tmp/dir", "mydb", "myprefix")
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "EXIT=0") {
		t.Errorf("expected EXIT=0\n%s", out)
	}
	// The script doesn't return sql_called to us, but if SQL had run with
	// our stubbed server_sql, the verify branch would print to stdout —
	// no extra output here means SQL wasn't reached.
}

// TestBdCLIRejected_SQLFallbackSucceeds: the bd 1.0.3 failure mode.
// Helper must take the fallback, write via SQL, verify, and return 0.
func TestBdCLIRejected_SQLFallbackSucceeds(t *testing.T) {
	setup := `
run_bd_pinned() {
    # Mimic bd 1.0.3's exact rejection text.
    echo "Error: issue_prefix cannot be set via 'bd config set'." >&2
    return 1
}
sql_history_file=$(mktemp)
export sql_history_file
server_sql_retry() {
    printf '%s\n' "$1" >> "$sql_history_file"
    return 0
}
server_sql() {
    # Verify SELECT — return the prefix that was inserted.
    if [[ "$1" == *SELECT* ]]; then
        echo "myprefix"
        return 0
    fi
    return 0
}
`
	out, err := runHelper(t, setup, "/tmp/dir", "mydb", "myprefix")
	if err != nil {
		t.Fatalf("helper unexpectedly failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "EXIT=0") {
		t.Fatalf("expected EXIT=0 (fallback should succeed)\n%s", out)
	}
}

// TestBdCLIRejected_SQLFallbackAlsoFails_ReturnsError: when SQL also
// fails, the helper must surface an error rather than masking it.
// This is the failure mode the original `2>/dev/null || true` was
// hiding.
func TestBdCLIRejected_SQLFallbackAlsoFails_ReturnsError(t *testing.T) {
	setup := `
run_bd_pinned() { return 1; }
server_sql_retry() { echo "could not connect to dolt server" >&2; return 1; }
server_sql() { return 1; }
`
	out, _ := runHelper(t, setup, "/tmp/dir", "mydb", "myprefix")
	if !strings.Contains(out, "EXIT=1") {
		t.Errorf("expected EXIT=1 in output\n%s", out)
	}
	if !strings.Contains(out, "failed to write issue_prefix") {
		t.Errorf("expected error message mentioning issue_prefix\n%s", out)
	}
}

// TestVerifyMismatchReturnsError: SQL claims success but verification
// SELECT returns the wrong value (or empty). The helper must catch this
// — otherwise the row would silently still be missing and bd create
// would still fail.
func TestVerifyMismatchReturnsError(t *testing.T) {
	setup := `
run_bd_pinned() { return 1; }
server_sql_retry() { return 0; }
server_sql() {
    if [[ "$1" == *SELECT* ]]; then
        # Verify returns nothing — simulate write that didn't commit.
        echo ""
        return 0
    fi
    return 0
}
`
	out, _ := runHelper(t, setup, "/tmp/dir", "mydb", "myprefix")
	if !strings.Contains(out, "EXIT=1") {
		t.Errorf("expected EXIT=1 when verify fails\n%s", out)
	}
	if !strings.Contains(out, "verification failed") {
		t.Errorf("expected verification failure message\n%s", out)
	}
}

// TestRejectsInvalidPrefix guards against SQL injection in the prefix.
// valid_sql_name must reject anything outside [a-zA-Z0-9_-].
func TestRejectsInvalidPrefix(t *testing.T) {
	setup := `
# Stubs would be reached only if validation passed.
run_bd_pinned() { echo "should not be called" >&2; return 0; }
server_sql_retry() { echo "should not be called" >&2; return 0; }
server_sql() { echo "should not be called" >&2; return 0; }
`
	cases := []string{
		"prefix; DROP TABLE config;--",
		"my prefix",      // space
		"prefix'value",   // quote
		"prefix\"value",  // double-quote
		"prefix\\backsl", // backslash
	}
	for _, prefix := range cases {
		t.Run(prefix, func(t *testing.T) {
			out, _ := runHelper(t, setup, "/tmp/dir", "mydb", prefix)
			if !strings.Contains(out, "invalid beads prefix") {
				t.Errorf("invalid prefix %q must be rejected with validation error\n%s", prefix, out)
			}
		})
	}
}

// TestRejectsInvalidDatabase mirrors TestRejectsInvalidPrefix for the
// dolt_database argument.
func TestRejectsInvalidDatabase(t *testing.T) {
	setup := `
run_bd_pinned() { return 0; }
server_sql_retry() { return 0; }
server_sql() { echo "x"; return 0; }
`
	out, _ := runHelper(t, setup, "/tmp/dir", "bad name; DROP", "myprefix")
	if !strings.Contains(out, "invalid dolt database name") {
		t.Errorf("expected validation error for bad db name\n%s", out)
	}
}

// TestRejectsMissingArgs covers the up-front contract check.
func TestRejectsMissingArgs(t *testing.T) {
	setup := `
run_bd_pinned() { return 0; }
server_sql_retry() { return 0; }
server_sql() { return 0; }
`
	out, _ := runHelper(t, setup, "", "db", "prefix")
	if !strings.Contains(out, "missing arguments") {
		t.Errorf("expected missing-args error for empty dir\n%s", out)
	}
}

// TestSQLContainsCorrectInsertAndCommit confirms the SQL fallback
// emits the INSERT...ON DUPLICATE KEY UPDATE pattern plus a
// DOLT_COMMIT — the exact SQL the issue's manual repair recommends.
func TestSQLContainsCorrectInsertAndCommit(t *testing.T) {
	historyFile := filepath.Join(t.TempDir(), "sql.log")
	setup := `
run_bd_pinned() { return 1; }
server_sql_retry() {
    printf '%s\n' "$1" >> "` + historyFile + `"
    return 0
}
server_sql() {
    if [[ "$1" == *SELECT* ]]; then
        echo "myprefix"; return 0
    fi
    return 0
}
`
	out, err := runHelper(t, setup, "/tmp/dir", "mydb", "myprefix")
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	logged, readErr := os.ReadFile(historyFile)
	if readErr != nil {
		t.Fatalf("reading sql history: %v", readErr)
	}
	logStr := string(logged)
	for _, want := range []string{
		"USE `mydb`",
		"INSERT INTO config",
		"`key`",
		"'issue_prefix'",
		"'myprefix'",
		"ON DUPLICATE KEY UPDATE",
		"DOLT_COMMIT",
	} {
		if !strings.Contains(logStr, want) {
			t.Errorf("SQL log missing %q:\n%s", want, logStr)
		}
	}
}
