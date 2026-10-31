package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
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

// customTypesHarness builds a shell program exposing
// ensure_bd_runtime_custom_types and its dependencies, followed by body.
func customTypesHarness(t *testing.T, body string) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
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
		"die", "is_remote", "connect_host", "sleep_ms", "server_sql",
		"is_retryable_error", "server_sql_retry", "valid_sql_name",
		"valid_custom_types_value", "validate_bd_runtime_config_value",
		"commit_bd_runtime_config", "ensure_bd_runtime_custom_types",
	} {
		harness.WriteString(extractShellFunction(t, script, fn))
		harness.WriteString("\n")
	}
	harness.WriteString(body)
	return harness.String()
}

// writeCustomTypesFakeDolt stubs `dolt ... sql -q <query>`, logging each query.
// FAKE_DOLT_TABLE_OUTPUT makes the custom_types INSERT fail with that text;
// FAKE_DOLT_REMOTE_OUTPUT is printed for the remote read-only missing-types
// probe.
func writeCustomTypesFakeDolt(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -u
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then query="$arg"; fi
  prev="$arg"
done
printf '%s\n' "$query" >> "$FAKE_DOLT_LOG"
case "$query" in
  *"INSERT IGNORE INTO custom_types"*)
    if [ -n "${FAKE_DOLT_TABLE_OUTPUT:-}" ]; then echo "$FAKE_DOLT_TABLE_OUTPUT" >&2; exit 1; fi ;;
  *gc-missing-custom-types*)
    printf '%s\n' "${FAKE_DOLT_REMOTE_OUTPUT:-}" ;;
esac
exit 0
`)
}

// runEnsureCustomTypes registers molecule,startup-health-episode through the
// fake-dolt harness and returns the output plus every query issued.
func runEnsureCustomTypes(t *testing.T, env ...string) (string, []string) {
	const types = "molecule,startup-health-episode"
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeCustomTypesFakeDolt(t, binDir)
	harnessPath := filepath.Join(dir, "harness.sh")
	writeExecutable(t, harnessPath, customTypesHarness(t,
		"ensure_bd_runtime_custom_types "+shellSingleQuote(configCommitTestDatabase)+" "+shellSingleQuote(types)+"\n"))
	logPath := filepath.Join(dir, "dolt.log")
	out := runShHarness(t, harnessPath, "ensure_bd_runtime_custom_types", append([]string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_DOLT_LOG=" + logPath,
	}, env...))
	var queries []string
	if data, err := os.ReadFile(logPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				queries = append(queries, line)
			}
		}
	}
	return string(out), queries
}

func queryContaining(queries []string, needle string) string {
	for _, q := range queries {
		if strings.Contains(q, needle) {
			return q
		}
	}
	return ""
}

// TestEnsureBdRuntimeCustomTypesMergesRowAndHealsTable pins the #6495 shape of
// the managed-server write: the config row is merged (an existing row keeps its
// value and only missing types are appended), the required types are INSERT
// IGNOREd into custom_types only when that table is already populated, and the
// commit stages exactly the two tables written.
func TestEnsureBdRuntimeCustomTypesMergesRowAndHealsTable(t *testing.T) {
	_, queries := runEnsureCustomTypes(t)

	row := queryContaining(queries, "INSERT INTO config")
	if row == "" {
		t.Fatalf("no config row write; queries:\n%s", strings.Join(queries, "\n"))
	}
	if strings.Contains(row, "VALUES(value)") {
		t.Fatalf("config row is overwritten, which drops operator types; query: %s", row)
	}
	for _, want := range []string{
		"ON DUPLICATE KEY UPDATE value = value",
		"FIND_IN_SET('startup-health-episode'",
		"CONCAT(value, ',startup-health-episode')",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("row merge missing %q; query: %s", want, row)
		}
	}
	table := queryContaining(queries, "INSERT IGNORE INTO custom_types")
	if table == "" {
		t.Fatalf("custom_types was never healed; queries:\n%s", strings.Join(queries, "\n"))
	}
	if !strings.Contains(table, "WHERE EXISTS (SELECT 1 FROM custom_types)") {
		t.Errorf("custom_types insert must be skipped for an empty table; query: %s", table)
	}
	if strings.Contains(table, "DELETE") {
		t.Errorf("custom_types heal must never delete rows; query: %s", table)
	}
	commit := queryContaining(queries, "DOLT_COMMIT")
	if !strings.Contains(commit, "DOLT_ADD('config', 'custom_types')") {
		t.Errorf("commit must stage config and custom_types, got: %s", commit)
	}
}

// TestEnsureBdRuntimeCustomTypesMissingTableStagesConfigOnly: a schema older
// than bd's custom_types migration has no table; that is left to bd's backfill
// silently, and the commit must not name a table that does not exist.
func TestEnsureBdRuntimeCustomTypesMissingTableStagesConfigOnly(t *testing.T) {
	out, queries := runEnsureCustomTypes(t,
		"FAKE_DOLT_TABLE_OUTPUT=table not found: custom_types")
	commit := queryContaining(queries, "DOLT_COMMIT")
	if !strings.Contains(commit, "DOLT_ADD('config')") {
		t.Errorf("commit must stage only config when custom_types was not written, got: %s", commit)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("a missing custom_types table is not an operator problem, got output: %q", out)
	}
}

// TestEnsureBdRuntimeCustomTypesTableFailureWarns: any other table failure is
// best-effort -- start continues -- but reported with the doctor hint.
func TestEnsureBdRuntimeCustomTypesTableFailureWarns(t *testing.T) {
	out, _ := runEnsureCustomTypes(t,
		"FAKE_DOLT_TABLE_OUTPUT=Error: permission denied")
	if !strings.Contains(out, "permission denied") || !strings.Contains(out, "gc doctor --fix") {
		t.Errorf("table heal failure must be surfaced with the doctor hint, got: %q", out)
	}
}

// TestEnsureBdRuntimeCustomTypesRemoteServerWarnsWithoutWriting: gc does not own
// the custom_types table on an external Dolt server. It must not write it, and
// must warn when required types are missing from it.
func TestEnsureBdRuntimeCustomTypesRemoteServerWarnsWithoutWriting(t *testing.T) {
	out, queries := runEnsureCustomTypes(t,
		"GC_DOLT_HOST=db.example.com",
		"FAKE_DOLT_REMOTE_OUTPUT=| gc-missing-custom-types:startup-health-episode |")
	if q := queryContaining(queries, "INSERT IGNORE INTO custom_types"); q != "" {
		t.Fatalf("custom_types written on an external server: %s", q)
	}
	if commit := queryContaining(queries, "DOLT_COMMIT"); strings.Contains(commit, "custom_types") {
		t.Errorf("commit staged custom_types on an external server: %s", commit)
	}
	if !strings.Contains(out, "startup-health-episode") || !strings.Contains(out, "gc doctor") {
		t.Errorf("want a warning naming the missing type and gc doctor, got: %q", out)
	}
}

// TestEnsureBdRuntimeCustomTypesRemoteServerCompleteIsQuiet: no warning when the
// external table already carries every required type.
func TestEnsureBdRuntimeCustomTypesRemoteServerCompleteIsQuiet(t *testing.T) {
	out, _ := runEnsureCustomTypes(t,
		"GC_DOLT_HOST=db.example.com",
		"FAKE_DOLT_REMOTE_OUTPUT=| gc-missing-custom-types: |")
	if strings.Contains(out, "warning") {
		t.Errorf("complete external table must not warn, got: %q", out)
	}
}

// TestEnsureBdRuntimeCustomTypesHealsUpgradedNativeStore is the end-to-end
// #6495 upgrade regression against a real Dolt server and the native store.
//
// A scope registered by gc v1.4.2 has the 13-type list in both the config row
// and the custom_types table. bd's validator reads the table whenever it is
// non-empty, so after the upgrade:
//
//   - creating a startup-health-episode bead through NativeDoltStore fails,
//     even though gc has already written the type into .beads/config.yaml:
//     YAML-only registration does NOT satisfy the native store (the library
//     never loads it), and must not be mistaken for a fix;
//   - after the shell's start-time ensure_bd_runtime_custom_types the create
//     succeeds, an operator type that lived in the row and table (ops-extra)
//     and one only the table carried (table-only) still validate, and the
//     write is committed rather than left dirty in the working set.
//
// A second database covers the empty-table shape: its JSON-array row is
// merged (not clobbered) and the empty table is left for bd's backfill.
func TestEnsureBdRuntimeCustomTypesHealsUpgradedNativeStore(t *testing.T) {
	clearInheritedBeadsEnv(t)
	v142 := strings.Join(v142CustomTypes, ",")
	setupQueries := append(seedDatabaseProjectIDQueries("gc-custom-types-upgrade-test"),
		"CALL DOLT_ADD('.')",
		"CALL DOLT_COMMIT('-m', 'test: seed identity', '--author', 'gascity-test <test@gascity.local>')")
	repoDir := filepath.Join(t.TempDir(), "fe")
	_, port, _, cleanupDolt := startPasswordedDoltServer(t, repoDir, setupQueries...)
	defer cleanupDolt()

	cityPath := t.TempDir()
	rigPath, err := writeManagedBdWaitTestCityScaffold(cityPath)
	if err != nil {
		t.Fatalf("writeManagedBdWaitTestCityScaffold: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, rigPath, "gc-custom-types-upgrade-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigPath, ".beads", "metadata.json"), contract.MetadataState{
		Database: "dolt", Backend: "dolt", DoltMode: "server", DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}
	// gc's start path writes every required type into config.yaml.
	if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, rigPath, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       strconv.Itoa(port),
		DoltUser:       "root",
		DoltMode:       "server",
	}); err != nil {
		t.Fatal(err)
	}
	if yaml, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml")); err != nil || !strings.Contains(string(yaml), "startup-health-episode") {
		t.Fatalf("config.yaml should already declare startup-health-episode (err=%v):\n%s", err, yaml)
	}

	nativeEnv, err := nativeDoltOpenEnvForScope(cityPath, nil, rigPath)
	if err != nil {
		t.Fatalf("nativeDoltOpenEnvForScope: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeStorageFixtureBootTimeout)
	defer cancel()

	// Seed the v1.4.2 registration: row and table both carry the 13-type list
	// plus an operator extra, as `bd config set` left them.
	storage, err := beads.OpenNativeStorage(ctx, rigPath, nativeEnv)
	if err != nil {
		t.Fatalf("OpenNativeStorage: %v", err)
	}
	for key, value := range map[string]string{"issue_prefix": "fe", "types.custom": v142 + ",ops-extra"} {
		if err := storage.SetConfig(ctx, key, value); err != nil {
			_ = storage.Close()
			t.Fatalf("SetConfig(%s): %v", key, err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("close seed storage: %v", err)
	}

	createType := func(typ string) error {
		t.Helper()
		store, err := beads.OpenNativeDoltStoreAt(ctx, rigPath, nativeEnv)
		if err != nil {
			t.Fatalf("OpenNativeDoltStoreAt: %v", err)
		}
		defer func() {
			if err := store.CloseStore(); err != nil {
				t.Errorf("close native store: %v", err)
			}
		}()
		_, err = store.Create(beads.Bead{Title: "type probe " + typ, Type: typ})
		return err
	}

	if err := createType("startup-health-episode"); err == nil || !strings.Contains(err.Error(), "invalid issue type") {
		t.Fatalf("pre-heal create error = %v, want invalid issue type (YAML-only registration must not satisfy the native store)", err)
	}

	dir := t.TempDir()
	harnessPath := filepath.Join(dir, "harness.sh")
	required := strings.Join(doctor.RequiredCustomTypes, ",")
	writeExecutable(t, harnessPath, customTypesHarness(t, `
q() { dolt --host 127.0.0.1 --port "$DOLT_PORT" --user "$DOLT_USER" --password "$DOLT_PASSWORD" --no-tls sql -r csv -q "$1" | tail -n 1 | tr -d '"'; }
server_sql "USE fe; INSERT IGNORE INTO custom_types (name) VALUES ('table-only'); CALL DOLT_ADD('custom_types'); CALL DOLT_COMMIT('-m', 'test: table-only extra', '--author', 'gascity-test <test@gascity.local>')" >/dev/null || exit 1
server_sql "CREATE DATABASE fresh; USE fresh; CREATE TABLE config (`+"\\`key\\`"+` VARCHAR(255) PRIMARY KEY, value TEXT NOT NULL); CREATE TABLE custom_types (name VARCHAR(64) PRIMARY KEY); INSERT INTO config VALUES ('types.custom', '[\"molecule\", \"ops-extra\"]'); CALL DOLT_ADD('.'); CALL DOLT_COMMIT('-m', 'test: fresh', '--author', 'gascity-test <test@gascity.local>')" >/dev/null || exit 1
ensure_bd_runtime_custom_types fe `+shellSingleQuote(required)+`
ensure_bd_runtime_custom_types fresh `+shellSingleQuote(required)+`
echo "FE_ROW=$(q "USE fe; SELECT value FROM config WHERE `+"\\`key\\`"+` = 'types.custom'")"
echo "FE_TABLE=$(q "USE fe; SELECT GROUP_CONCAT(name ORDER BY name) AS n FROM custom_types")"
echo "FE_DIRTY=$(q "USE fe; SELECT COUNT(*) AS c FROM dolt_status WHERE table_name IN ('config', 'custom_types')")"
echo "FRESH_ROW=$(q "USE fresh; SELECT value FROM config WHERE `+"\\`key\\`"+` = 'types.custom'")"
echo "FRESH_TABLE=$(q "USE fresh; SELECT COUNT(*) AS c FROM custom_types")"
`))
	out := runShHarness(t, harnessPath, "ensure_bd_runtime_custom_types (real dolt)", sanitizedBaseEnv(
		"GC_DOLT_PORT="+strconv.Itoa(port),
		"GC_DOLT_PASSWORD=secret",
	))
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.ToUpper(k) == k && !strings.Contains(k, " ") {
			got[k] = v
		}
	}
	setOf := func(csv string) map[string]bool {
		m := map[string]bool{}
		for _, v := range strings.Split(csv, ",") {
			m[strings.TrimSpace(v)] = true
		}
		return m
	}
	feRow, feTable := setOf(got["FE_ROW"]), setOf(got["FE_TABLE"])
	for _, typ := range append(append([]string{}, doctor.RequiredCustomTypes...), "ops-extra") {
		if !feRow[typ] {
			t.Errorf("fe config row %q lost or lacks %q", got["FE_ROW"], typ)
		}
	}
	for _, typ := range append(append([]string{}, doctor.RequiredCustomTypes...), "ops-extra", "table-only") {
		if !feTable[typ] {
			t.Errorf("fe custom_types %q lost or lacks %q", got["FE_TABLE"], typ)
		}
	}
	if got["FE_DIRTY"] != "0" {
		t.Errorf("fe config/custom_types left dirty in the working set (%q rows in dolt_status)\n%s", got["FE_DIRTY"], out)
	}
	freshRow := setOf(got["FRESH_ROW"])
	for _, typ := range append(append([]string{}, doctor.RequiredCustomTypes...), "ops-extra") {
		if !freshRow[typ] {
			t.Errorf("fresh JSON-array row merged to %q, lacks %q", got["FRESH_ROW"], typ)
		}
	}
	if got["FRESH_TABLE"] != "0" {
		t.Errorf("empty custom_types table was written (%q rows); it must be left to bd's backfill", got["FRESH_TABLE"])
	}

	for _, typ := range []string{"startup-health-episode", "ops-extra", "table-only"} {
		if err := createType(typ); err != nil {
			t.Errorf("post-heal create --type %s: %v", typ, err)
		}
	}
}
