package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureBdRuntimeConfigValue_* are regression tests for #5286: Gas City
// writes issue_prefix/types.custom into the Dolt `config` table with a raw
// INSERT and never commits it. Against an external Dolt server, config is
// not registered in dolt_ignore, so the database is left with a permanently
// dirty working set that blocks the next beads schema migration touching
// config, with no in-band recovery in server mode.
//
// These tests exercise the real shell functions (extracted from the script,
// same technique as TestServerReachableReflectsDoltExit) against a fake
// `dolt` binary that logs the SQL text of every `sql -q` invocation, so the
// tests can assert on exactly which statements were issued without a live
// Dolt server.

func ensureBdRuntimeConfigTestScript(t *testing.T) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	src := string(scriptBytes)
	var b strings.Builder
	b.WriteString("connect_host() { printf '127.0.0.1'; }\n")
	for _, fn := range []string{
		"die",
		"valid_sql_name",
		"valid_custom_types_value",
		"validate_bd_runtime_config_value",
		"is_retryable_error",
		"sleep_ms",
		"server_sql",
		"server_sql_retry",
		"ensure_bd_runtime_config_value",
		"ensure_bd_runtime_issue_prefix",
	} {
		b.WriteString(extractShellFunction(t, src, fn))
		b.WriteString("\n")
	}
	return b.String()
}

// writeFakeSQLLoggingDolt writes a fake `dolt` that logs the query text of
// every `sql -q <query>` invocation to FAKE_DOLT_LOG (one line per call) and
// fails any query matching a DOLT_ADD/DOLT_COMMIT call when
// FAKE_DOLT_COMMIT_FAIL is set, printing FAKE_DOLT_COMMIT_ERROR to stderr.
// All other queries (the runtime config INSERT) always succeed, matching a
// healthy server for the part of this flow #5286 does not touch.
func writeFakeSQLLoggingDolt(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "dolt")
	body := `#!/bin/sh
log_file=${FAKE_DOLT_LOG:-/dev/null}
prev=""
query=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
  fi
  prev="$arg"
done
printf '%s\n' "$query" >> "$log_file"
case "$query" in
  *DOLT_ADD*|*DOLT_COMMIT*)
    if [ -n "${FAKE_DOLT_COMMIT_FAIL:-}" ]; then
      echo "${FAKE_DOLT_COMMIT_ERROR:-simulated commit failure}" >&2
      exit 1
    fi
    ;;
esac
exit 0
`
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

func TestEnsureBdRuntimeConfigValue_CommitsScopedToConfigTable(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	binDir := t.TempDir()
	writeFakeSQLLoggingDolt(t, binDir)
	logFile := filepath.Join(t.TempDir(), "dolt-calls.log")

	script := ensureBdRuntimeConfigTestScript(t) + "\nensure_bd_runtime_issue_prefix testdb tf\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOLT_PORT=42188",
		"DOLT_USER=root",
		"DOLT_PASSWORD=",
		"FAKE_DOLT_LOG="+logFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ensure_bd_runtime_issue_prefix failed: %v\noutput: %s", err, out)
	}

	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(logBytes)

	if !strings.Contains(log, "INSERT INTO config") {
		t.Fatalf("expected the runtime config INSERT to still happen; log:\n%s", log)
	}
	if !strings.Contains(log, "DOLT_ADD('config')") {
		t.Fatalf("expected a scoped CALL DOLT_ADD('config') after the write (#5286); log:\n%s", log)
	}
	if strings.Contains(log, "DOLT_ADD('.')") {
		t.Fatalf("commit must be scoped to the config table, not DOLT_ADD('.') -- an unscoped add would sweep any other dirty table into Gas City's commit, the exact hash-drift hazard #5286 warns against; log:\n%s", log)
	}
	if !strings.Contains(log, "DOLT_COMMIT") {
		t.Fatalf("expected a CALL DOLT_COMMIT after staging the config write (#5286); log:\n%s", log)
	}
}

func TestEnsureBdRuntimeConfigValue_FailsOpenOnCommitError(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	binDir := t.TempDir()
	writeFakeSQLLoggingDolt(t, binDir)
	logFile := filepath.Join(t.TempDir(), "dolt-calls.log")

	script := ensureBdRuntimeConfigTestScript(t) + "\nensure_bd_runtime_issue_prefix testdb tf\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOLT_PORT=42188",
		"DOLT_USER=root",
		"DOLT_PASSWORD=",
		"FAKE_DOLT_LOG="+logFile,
		"FAKE_DOLT_COMMIT_FAIL=1",
		"FAKE_DOLT_COMMIT_ERROR=simulated lock wait timeout",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a commit failure must not block provisioning (fail-open per #5286's proposed fix); err: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "simulated lock wait timeout") {
		t.Fatalf("a real commit failure must be reported, never swallowed (#5286); output:\n%s", out)
	}
}

func TestEnsureBdRuntimeConfigValue_TreatsNothingToCommitAsSuccess(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	binDir := t.TempDir()
	writeFakeSQLLoggingDolt(t, binDir)
	logFile := filepath.Join(t.TempDir(), "dolt-calls.log")

	script := ensureBdRuntimeConfigTestScript(t) + "\nensure_bd_runtime_issue_prefix testdb tf\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOLT_PORT=42188",
		"DOLT_USER=root",
		"DOLT_PASSWORD=",
		"FAKE_DOLT_LOG="+logFile,
		"FAKE_DOLT_COMMIT_FAIL=1",
		"FAKE_DOLT_COMMIT_ERROR=nothing to commit",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("an idempotent no-op commit must not fail provisioning; err: %v\noutput: %s", err, out)
	}
	if strings.Contains(string(out), "warning:") {
		t.Fatalf("an idempotent 'nothing to commit' re-run should not be reported as a warning; output:\n%s", out)
	}
}
