package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests exercise ensure_bd_runtime_config_value from
// examples/bd/assets/scripts/gc-beads-bd.sh against a stub `dolt` binary.
//
// Gas City writes issue_prefix and types.custom straight into the beads Dolt
// `config` table because beads refuses `bd config set issue_prefix` (it is
// owned by `bd init`/`bd rename-prefix`). That raw INSERT used to leave the
// row sitting in the Dolt working set forever: `config` is not registered in
// dolt_ignore, so every city/rig database Gas City provisioned stayed
// permanently dirty.
//
// That matters for two reasons the script already documents elsewhere:
//
//   - A beads schema migration refuses to alter a table that has pre-existing
//     uncommitted changes (beads DirtyTablesError). Migration 0030 already
//     issues `DELETE FROM config`, so the next migration touching `config`
//     bricks every affected database at once. Its documented recovery,
//     `bd dolt commit`, cannot run against an external Dolt server
//     (gastownhall/beads#4566 fixed that deadlock for embedded mode only),
//     leaving no in-band way out.
//   - A table that lives only in the working set gets swept into an unrelated
//     `DOLT_COMMIT -Am` later, drifting the database hash and quarantining GC
//     for that database -- the exact hazard the read-only probe table is
//     registered in dolt_ignore to avoid (see the comment above
//     managed_dolt_read_only_probe in the script).
//
// The contract under test: the config write is committed before the function
// returns, the commit is scoped to `config` alone so it cannot sweep unrelated
// dirty tables into Gas City's commit, and an already-clean working set is not
// treated as a failure.

// configCommitHarness builds a shell program exposing
// ensure_bd_runtime_config_value and its transitive dependencies, then invokes
// it for one config key/value. The invocation is baked in rather than passed as
// argv so the shared runShHarness runner can execute it, keeping this file off
// the untagged subprocess census.
func configCommitHarness(t *testing.T, key, value string) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script := string(scriptBytes)

	var harness strings.Builder
	harness.WriteString("#!/usr/bin/env bash\nset -u\n")
	harness.WriteString("DOLT_HOST=\"${GC_DOLT_HOST:-127.0.0.1}\"\n")
	harness.WriteString("DOLT_USER=\"${GC_DOLT_USER:-root}\"\n")
	harness.WriteString("DOLT_PASSWORD=\"${GC_DOLT_PASSWORD:-}\"\n")
	harness.WriteString("DOLT_PORT=\"${GC_DOLT_PORT:-3307}\"\n")
	for _, fn := range []string{
		"die",
		"is_remote",
		"connect_host",
		"sleep_ms",
		"server_sql",
		"is_retryable_error",
		"server_sql_retry",
		"valid_sql_name",
		"valid_custom_types_value",
		"validate_bd_runtime_config_value",
		"ensure_bd_runtime_config_value",
	} {
		harness.WriteString(extractShellFunction(t, script, fn))
		harness.WriteString("\n")
	}
	// The commit helper is extracted optionally so that when it is absent the
	// tests fail on the behavior they assert (no commit was issued) rather
	// than on a missing-function harness error.
	harness.WriteString(optionalShellFunction(script, "commit_bd_runtime_config"))
	harness.WriteString("\n")
	harness.WriteString("ensure_bd_runtime_config_value " +
		shellSingleQuote(configCommitTestDatabase) + " " +
		shellSingleQuote(key) + " " + shellSingleQuote(value) + "\n")
	return harness.String()
}

// optionalShellFunction returns the named shell function's source, or an empty
// string when the script does not define it.
func optionalShellFunction(script, name string) string {
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)\s*\{.*?\n\}`)
	loc := pattern.FindStringIndex(script)
	if loc == nil {
		return ""
	}
	return script[loc[0]:loc[1]]
}

// writeConfigCommitFakeDolt stubs `dolt ... sql -q <query>` by appending each
// query to FAKE_DOLT_LOG. When FAKE_DOLT_COMMIT_OUTPUT is set, any query
// containing DOLT_COMMIT fails with that text, modeling Dolt's refusal to
// create an empty commit.
func writeConfigCommitFakeDolt(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -u
log_file=${FAKE_DOLT_LOG:-/dev/null}
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then query="$arg"; fi
  prev="$arg"
done
printf '%s\n' "$query" >> "$log_file"
if [ -n "${FAKE_DOLT_COMMIT_OUTPUT:-}" ]; then
  case "$query" in
    *DOLT_COMMIT*) echo "$FAKE_DOLT_COMMIT_OUTPUT" >&2; exit 1 ;;
  esac
fi
exit 0
`)
}

// configCommitTestDatabase is the Dolt database name the harness writes to.
const configCommitTestDatabase = "rigdb"

// runEnsureConfigValue runs the harness for one config key/value write and
// returns the combined output plus every SQL query the stub dolt received.
// It routes through the shared runShHarness runner, which fails the test if the
// harness itself exits non-zero.
func runEnsureConfigValue(t *testing.T, key, value string, env ...string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeConfigCommitFakeDolt(t, binDir)

	harnessPath := filepath.Join(dir, "harness.sh")
	writeExecutable(t, harnessPath, configCommitHarness(t, key, value))

	logPath := filepath.Join(dir, "dolt.log")
	harnessEnv := append([]string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_DOLT_LOG=" + logPath,
	}, env...)
	out := runShHarness(t, harnessPath, "ensure_bd_runtime_config_value", harnessEnv)

	var queries []string
	if data, readErr := os.ReadFile(logPath); readErr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				queries = append(queries, line)
			}
		}
	}
	return string(out), queries
}

// TestEnsureBdRuntimeConfigValueCommitsTheWrite is the regression guard for
// Gas City leaving issue_prefix permanently uncommitted in every provisioned
// database.
func TestEnsureBdRuntimeConfigValueCommitsTheWrite(t *testing.T) {
	_, queries := runEnsureConfigValue(t, "issue_prefix", "sctforg")

	joined := strings.Join(queries, "\n")
	if !strings.Contains(joined, "INSERT INTO config") {
		t.Fatalf("expected the config INSERT to still be issued; queries:\n%s", joined)
	}
	if !strings.Contains(joined, "DOLT_COMMIT") {
		t.Fatalf("config write was never committed, leaving the working set dirty; queries:\n%s", joined)
	}
	if !strings.Contains(joined, "DOLT_ADD") {
		t.Fatalf("expected the config table to be staged before commit; queries:\n%s", joined)
	}
}

// TestEnsureBdRuntimeConfigValueCommitScopedToConfigTable pins the commit to
// the config table. A blanket DOLT_ADD('.') / DOLT_COMMIT('-Am') would sweep
// unrelated dirty tables into Gas City's commit and drift the database hash.
func TestEnsureBdRuntimeConfigValueCommitScopedToConfigTable(t *testing.T) {
	_, queries := runEnsureConfigValue(t, "issue_prefix", "sctforg")

	var commitQuery string
	for _, q := range queries {
		if strings.Contains(q, "DOLT_COMMIT") {
			commitQuery = q
			break
		}
	}
	if commitQuery == "" {
		t.Fatal("no DOLT_COMMIT query was issued")
	}
	if !strings.Contains(commitQuery, "DOLT_ADD('config')") {
		t.Errorf("commit must stage only the config table, got: %s", commitQuery)
	}
	for _, forbidden := range []string{"DOLT_ADD('.')", "DOLT_ADD('-A')", "'-Am'", "'-A'"} {
		if strings.Contains(commitQuery, forbidden) {
			t.Errorf("commit must not stage every table (%s), got: %s", forbidden, commitQuery)
		}
	}
}

// TestEnsureBdRuntimeConfigValueCommitsCustomTypes covers the second caller,
// ensure_bd_runtime_custom_types, which funnels through the same helper.
func TestEnsureBdRuntimeConfigValueCommitsCustomTypes(t *testing.T) {
	_, queries := runEnsureConfigValue(t, "types.custom", "molecule,convoy,session")
	joined := strings.Join(queries, "\n")
	if !strings.Contains(joined, "DOLT_COMMIT") {
		t.Fatalf("types.custom write was never committed; queries:\n%s", joined)
	}
}

// TestEnsureBdRuntimeConfigValueToleratesNothingToCommit covers the idempotent
// re-run: the value is already present and committed, so Dolt refuses an empty
// commit. That must not fail provisioning.
func TestEnsureBdRuntimeConfigValueToleratesNothingToCommit(t *testing.T) {
	_, queries := runEnsureConfigValue(t, "issue_prefix", "sctforg",
		"FAKE_DOLT_COMMIT_OUTPUT=Error: nothing to commit")
	if len(queries) == 0 {
		t.Fatal("expected the stub dolt to receive queries")
	}
}

// TestEnsureBdRuntimeConfigValueReportsCommitFailure guards against silently
// swallowing a genuine commit error: provisioning stays fail-open (the value
// is already written) but the operator must be told the working set is dirty.
func TestEnsureBdRuntimeConfigValueReportsCommitFailure(t *testing.T) {
	out, _ := runEnsureConfigValue(t, "issue_prefix", "sctforg",
		"FAKE_DOLT_COMMIT_OUTPUT=Error: connection refused")
	if !strings.Contains(out, "connection refused") {
		t.Errorf("commit failure must be surfaced to the operator, got: %q", out)
	}
}
