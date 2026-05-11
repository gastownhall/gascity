package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

func runDogScriptCommand(t *testing.T, scriptName, binDir, cityPath, dataDir string, extraEnv ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("bash", filepath.Join(root, "assets", "scripts", scriptName))
	cmd.Env = append(filteredEnv(
		"PATH",
		"GC_CITY_PATH",
		"GC_PACK_DIR",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_PORT",
		"GC_DOLT_HOST",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_BACKUP_DATABASES",
		"GC_BACKUP_OFFSITE_PATH",
		"GC_BACKUP_ARTIFACT_DIR",
		"GC_PHANTOM_DATA_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runDogScript(t *testing.T, scriptName, binDir, cityPath, dataDir string, extraEnv ...string) string {
	t.Helper()
	out, err := runDogScriptCommand(t, scriptName, binDir, cityPath, dataDir, extraEnv...)
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", scriptName, err, out)
	}
	return out
}

func writeDogFakeGC(t *testing.T, binDir string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "gc.log")
	writeExecutable(t, filepath.Join(binDir, "gc"), fmt.Sprintf(`#!/bin/sh
printf 'gc %s\n' "$*" >> %s
exit 0
`, "%s", shellQuote(logPath)))
	return logPath
}

func TestDogExecScriptsAreBashSyntaxValid(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	root := repoRoot(t)
	for _, scriptName := range []string{
		"mol-dog-backup.sh",
		"mol-dog-doctor.sh",
		"mol-dog-phantom-db.sh",
	} {
		t.Run(scriptName, func(t *testing.T) {
			cmd := exec.Command("bash", "-n", filepath.Join(root, "assets", "scripts", scriptName))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bash -n failed: %v\n%s", err, out)
			}
		})
	}
	commandScripts, err := filepath.Glob(filepath.Join(root, "commands", "*", "run.sh"))
	if err != nil {
		t.Fatalf("glob command scripts: %v", err)
	}
	for _, scriptPath := range commandScripts {
		name := strings.TrimPrefix(scriptPath, root+string(os.PathSeparator))
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n", scriptPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bash -n failed: %v\n%s", err, out)
			}
		})
	}
}

type compactScriptFixture struct {
	root          string
	cityPath      string
	dataDir       string
	binDir        string
	doltLog       string
	stateFile     string
	hashStateFile string
	port          int
}

func newCompactScriptFixture(t *testing.T) compactScriptFixture {
	t.Helper()
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	t.Cleanup(cleanup)

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(filepath.Join(dataDir, "beads", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir dolt db: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir city beads dir: %v", err)
	}
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, os.Getpid())

	binDir := t.TempDir()
	writeCompactFakeGC(t, binDir)
	doltLog := writeCompactFakeDolt(t, binDir)
	stateFile := filepath.Join(binDir, "head-state")
	if err := os.WriteFile(stateFile, []byte("headcommit\n"), 0o644); err != nil {
		t.Fatalf("write fake dolt state: %v", err)
	}
	hashStateFile := filepath.Join(binDir, "hash-state")
	if err := os.WriteFile(hashStateFile, []byte("hash-before\n"), 0o644); err != nil {
		t.Fatalf("write fake dolt hash state: %v", err)
	}
	return compactScriptFixture{
		root:          root,
		cityPath:      cityPath,
		dataDir:       dataDir,
		binDir:        binDir,
		doltLog:       doltLog,
		stateFile:     stateFile,
		hashStateFile: hashStateFile,
		port:          port,
	}
}

func (f compactScriptFixture) run(t *testing.T, mode string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(f.root, "commands", "compact", "run.sh"))
	cmd.Env = append(filteredEnv(
		"PATH",
		"GC_CITY_PATH",
		"GC_PACK_DIR",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_PORT",
		"GC_DOLT_HOST",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_MANAGED_LOCAL",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS",
		"GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS",
		"GC_DOLT_COMPACT_DRY_RUN",
		"GC_DOLT_COMPACT_ONLY_DBS",
		"GC_DOLT_COMPACT_REMOTE",
		"GC_FAKE_DOLT_COMPACT_MODE",
		"GC_FAKE_DOLT_COUNT_FILE",
		"GC_FAKE_DOLT_STATE_FILE",
		"GC_FAKE_DOLT_HASH_STATE_FILE",
	),
		"PATH="+f.binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+f.cityPath,
		"GC_PACK_DIR="+f.root,
		"GC_DOLT_DATA_DIR="+f.dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", f.port),
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_MANAGED_LOCAL=1",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=5",
		"GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS=5",
		"GC_FAKE_DOLT_COMPACT_MODE="+mode,
		"GC_FAKE_DOLT_COUNT_FILE="+filepath.Join(f.binDir, "row-count-calls"),
		"GC_FAKE_DOLT_STATE_FILE="+f.stateFile,
		"GC_FAKE_DOLT_HASH_STATE_FILE="+f.hashStateFile,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runCompactScriptCommand(t *testing.T, mode string) (string, string, error) {
	t.Helper()
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	return out, fixture.doltLog, err
}

func writeCompactFakeGC(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
if [ "${1:-}" = "rig" ] && [ "${2:-}" = "list" ]; then
  printf '{"rigs":[]}\n'
  exit 0
fi
exit 0
`)
}

func writeCompactFakeDolt(t *testing.T, binDir string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "dolt.log")
	writeExecutable(t, filepath.Join(binDir, "dolt"), fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
log=%s
mode="${GC_FAKE_DOLT_COMPACT_MODE:-success}"
count_file="${GC_FAKE_DOLT_COUNT_FILE:-}"
state_file="${GC_FAKE_DOLT_STATE_FILE:-}"
hash_state_file="${GC_FAKE_DOLT_HASH_STATE_FILE:-}"
query=""
db=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --use-db)
      db="$2"
      shift 2
      ;;
    -q)
      query="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
printf 'db=%%s query=%%s\n' "$db" "$query" >> "$log"
print_cell() {
  printf '+-------+\n'
  printf '| value |\n'
  printf '+-------+\n'
  printf '| %%s |\n' "$1"
  printf '+-------+\n'
}
current_head() {
  if [ "$mode" = "head_changes_before_flatten" ]; then
    calls_file="$state_file.head-calls"
    calls=0
    if [ -f "$calls_file" ]; then
      calls="$(cat "$calls_file")"
    fi
    calls=$((calls + 1))
    printf '%%s\n' "$calls" > "$calls_file"
    if [ $((calls %% 2)) -eq 0 ]; then
      printf 'writercommit\n'
      return 0
    fi
  fi
  if [ -n "$state_file" ] && [ -f "$state_file" ]; then
    sed -n '1p' "$state_file"
  else
    printf 'headcommit\n'
  fi
}
set_head() {
  [ -n "$state_file" ] || return 0
  printf '%%s\n' "$1" > "$state_file"
}
current_hash() {
  if [ -n "$hash_state_file" ] && [ -f "$hash_state_file" ]; then
    sed -n '1p' "$hash_state_file"
  else
    printf 'hash-before\n'
  fi
}
set_hash() {
  [ -n "$hash_state_file" ] || return 0
  printf '%%s\n' "$1" > "$hash_state_file"
}
case "$query" in
  *"SELECT COUNT(*) FROM dolt_remotes WHERE name = 'origin'"*)
    case "$mode" in
      remote_success|remote_ahead|remote_fetch_failure|remote_push_failure|remote_advances_before_push|remote_gc_failure_once|multiple_remotes_with_origin)
        print_cell 1
        ;;
      *)
        print_cell 0
        ;;
    esac
    exit 0
    ;;
  *"SELECT COUNT(*) FROM dolt_remotes WHERE name = 'backup'"*)
    case "$mode" in
      explicit_backup_remote)
        print_cell 1
        ;;
      *)
        print_cell 0
        ;;
    esac
    exit 0
    ;;
  *"SELECT COUNT(*) FROM dolt_remotes"*)
    case "$mode" in
      remote_success|remote_ahead|remote_fetch_failure|remote_push_failure|remote_advances_before_push|remote_gc_failure_once)
        print_cell 1
        ;;
      multiple_remotes_with_origin|multiple_remotes_no_origin)
        print_cell 2
        ;;
      explicit_backup_remote)
        print_cell 1
        ;;
      *)
        print_cell 0
        ;;
    esac
    exit 0
    ;;
  *"SELECT name FROM dolt_remotes ORDER BY name LIMIT 1"*)
    case "$mode" in
      remote_success|remote_ahead|remote_fetch_failure|remote_push_failure|remote_advances_before_push|remote_gc_failure_once|multiple_remotes_with_origin)
        print_cell origin
        ;;
      explicit_backup_remote)
        print_cell backup
        ;;
      *)
        print_cell ""
        ;;
    esac
    exit 0
    ;;
  *"DOLT_FETCH('origin')"*)
    if [ "$mode" = "remote_fetch_failure" ]; then
      printf 'fetch unavailable\n' >&2
      exit 52
    fi
    exit 0
    ;;
  *"DOLT_FETCH('backup')"*)
    exit 0
    ;;
  *"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/main'"*)
    if [ "$mode" = "remote_advances_before_push" ]; then
      calls_file="$state_file.remote-head-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$calls" -gt 1 ]; then
        print_cell remotecommit
      else
        print_cell headcommit
      fi
    elif [ "$mode" = "remote_ahead" ]; then
      print_cell remotecommit
    else
      print_cell headcommit
    fi
    exit 0
    ;;
  *"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/backup/main'"*)
    print_cell headcommit
    exit 0
    ;;
  *"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = 'remotecommit'"*)
    print_cell 0
    exit 0
    ;;
  *"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = 'headcommit'"*)
    print_cell 1
    exit 0
    ;;
  *"SELECT COUNT(*) FROM (SELECT 1 FROM dolt_log"*)
    if [ "$mode" = "commit_count_failure" ]; then
      printf 'dolt_log unavailable\n' >&2
      exit 42
    fi
    if [ "$mode" = "below_threshold" ]; then
      print_cell 499
    else
      print_cell 600
    fi
    exit 0
    ;;
  *"SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1"*)
    print_cell "$(current_head)"
    exit 0
    ;;
  *"SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1"*)
    if [ "$mode" = "root_commit_failure" ]; then
      printf 'root commit exploded\n' >&2
      exit 46
    fi
    print_cell rootcommit
    exit 0
    ;;
  *"DOLT_HASHOF_DB()"*)
    if [ "$mode" = "db_hash_failure" ]; then
      printf 'db hash exploded\n' >&2
      exit 48
    fi
    print_cell "$(current_hash)"
    exit 0
    ;;
  *"information_schema.tables"*)
    if [ "$mode" = "table_discovery_failure" ]; then
      printf 'information_schema unavailable\n' >&2
      exit 43
    fi
    if [ "$mode" = "invalid_table_name" ]; then
      print_cell 'bad/name'
      exit 0
    fi
    if [ "$mode" = "table_name_clobber" ]; then
      print_cell blocked_issues
      exit 0
    fi
    print_cell beads
    exit 0
    ;;
  *"SELECT COUNT(*) FROM"*"blocked_issues"*)
    if [ "$db" = "blocked_issues" ]; then
      printf 'database not found: blocked_issues\n' >&2
      exit 1049
    fi
    print_cell 10
    exit 0
    ;;
  *"SELECT COUNT(*) FROM"*"beads"*)
    if [ "$mode" = "row_count_failure" ]; then
      printf 'row count exploded\n' >&2
      exit 47
    fi
    calls=0
    if [ -n "$count_file" ] && [ -f "$count_file" ]; then
      calls="$(cat "$count_file")"
    fi
    calls=$((calls + 1))
    if [ -n "$count_file" ]; then
      printf '%%s\n' "$calls" > "$count_file"
    fi
    if [ "$mode" = "row_count_diverges" ] && [ "$calls" -gt 1 ]; then
      print_cell 11
    elif [ "$mode" = "row_count_decreases" ] && [ "$calls" -gt 1 ]; then
      print_cell 9
    else
      print_cell 10
    fi
    exit 0
    ;;
  *"DOLT_RESET"*)
    if [[ "$query" == *"--hard"* ]]; then
      set_head headcommit
      exit 0
    fi
    if [ "$mode" = "flatten_failure" ]; then
      printf 'reset exploded\n' >&2
      exit 44
    fi
    if [ "$mode" = "commit_failure_after_reset" ]; then
      set_head rootcommit
      printf 'commit rejected after reset\n' >&2
      exit 44
    fi
    if [ "$mode" = "commit_failure_after_external_head_advance" ]; then
      set_head writercommit
      printf 'commit rejected after external writer advanced HEAD\n' >&2
      exit 44
    fi
    set_head compactcommit
    if [ "$mode" = "same_row_count_writer" ]; then
      set_hash hash-after-writer
    fi
    exit 0
    ;;
  *"DOLT_GC"*)
    if [ "$mode" = "remote_gc_failure_once" ]; then
      calls_file="$state_file.gc-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$calls" -eq 1 ]; then
        printf 'gc exploded once\n' >&2
        exit 45
      fi
    fi
    if [ "$mode" = "gc_failure" ]; then
      printf 'gc exploded\n' >&2
      exit 45
    fi
    exit 0
    ;;
  *"DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')"*)
    if [ "$mode" = "remote_push_failure" ]; then
      printf 'push unavailable\n' >&2
      exit 53
    fi
    exit 0
    ;;
  *"DOLT_PUSH('--force', '--set-upstream', 'backup', 'main')"*)
    exit 0
    ;;
esac
printf 'unexpected query: %%s\n' "$query" >&2
exit 64
`, shellQuote(logPath)))
	return logPath
}

func TestCompactScriptSkipsBelowThresholdWithoutFlattening(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "below_threshold=500") {
		t.Fatalf("output missing below-threshold skip:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("below-threshold compact must not flatten:\n%s", data)
	}
}

func TestCompactScriptDefaultThresholdIs2000(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "below_threshold=2000") {
		t.Fatalf("output missing default 2000 threshold:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("default-threshold compact must not flatten a 600-commit db:\n%s", data)
	}
}

func TestCompactScriptFlattensAndVerifies(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "commits=600->600") || !strings.Contains(out, "— ok") {
		t.Fatalf("output missing success summary:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %s:\n%s", want, log)
		}
	}
}

func TestCompactScriptRefetchesAndForcePushesRemote(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin") {
		t.Fatalf("output missing remote-awareness marker:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"CALL DOLT_FETCH('origin')",
		"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/main'",
		"CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %q:\n%s", want, log)
		}
	}
	if strings.Count(log, "CALL DOLT_FETCH('origin')") < 2 {
		t.Fatalf("compact should re-fetch immediately before remote push:\n%s", log)
	}
}

func TestCompactScriptPrefersOriginWhenMultipleRemotesExist(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "multiple_remotes_with_origin", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed with origin available among multiple remotes: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin") {
		t.Fatalf("output missing origin remote selection:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if !strings.Contains(string(data), "DOLT_FETCH('origin')") {
		t.Fatalf("compact did not fetch origin among multiple remotes:\n%s", data)
	}
}

func TestCompactScriptFailsWhenMultipleRemotesLackOrigin(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "multiple_remotes_no_origin", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite ambiguous remotes:\n%s", out)
	}
	if !strings.Contains(out, "multiple remotes found without origin") {
		t.Fatalf("output missing ambiguous remote failure:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	for _, forbidden := range []string{"DOLT_FETCH", "DOLT_RESET", "DOLT_PUSH"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("ambiguous remotes must block compaction before %s:\n%s", forbidden, data)
		}
	}
}

func TestCompactScriptUsesExplicitRemote(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "explicit_backup_remote", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_REMOTE=backup")
	if err != nil {
		t.Fatalf("compact failed with explicit remote: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=backup") {
		t.Fatalf("output missing explicit remote selection:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	for _, want := range []string{
		"SELECT COUNT(*) FROM dolt_remotes WHERE name = 'backup'",
		"CALL DOLT_FETCH('backup')",
		"CALL DOLT_PUSH('--force', '--set-upstream', 'backup', 'main')",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("dolt log missing %q:\n%s", want, data)
		}
	}
}

func TestCompactScriptAbortsPushWhenRemoteHeadChangesAfterCompaction(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_advances_before_push", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite remote HEAD changing before push:\n%s", out)
	}
	if !strings.Contains(out, "remote=origin HEAD changed before push") {
		t.Fatalf("output missing remote compare-and-push failure:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if !strings.Contains(log, want) {
			t.Fatalf("remote compare failure should happen after local compaction %s:\n%s", want, log)
		}
	}
	if strings.Count(log, "CALL DOLT_FETCH('origin')") < 2 {
		t.Fatalf("compact should re-fetch before deciding whether to push:\n%s", log)
	}
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("remote HEAD drift must block push:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("remote drift after compaction should write pending-push marker: %v", err)
	}
}

func TestCompactScriptFailsBeforeFlattenWhenRemoteAheadIsUnknown(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite unknown remote HEAD:\n%s", out)
	}
	if !strings.Contains(out, "remote HEAD=remotecommit is not in local history") {
		t.Fatalf("output missing remote divergence notice:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC", "DOLT_PUSH"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("remote divergence must block local compaction before %s:\n%s", forbidden, log)
		}
	}
}

func TestCompactScriptFailsWhenHeadChangesBeforeFlatten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "head_changes_before_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite live-server moving HEAD:\n%s", out)
	}
	if !strings.Contains(out, "HEAD changed before flatten") {
		t.Fatalf("output missing moving-HEAD failure:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("moving HEAD must block local compaction before %s:\n%s", forbidden, log)
		}
	}
}

func TestCompactScriptFailsBeforeFlattenWhenRemoteFetchFails(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite remote fetch failure:\n%s", out)
	}
	if !strings.Contains(out, "remote=origin fetch failed") {
		t.Fatalf("output missing fetch failure:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	if strings.Contains(log, "dolt_remote_branches") {
		t.Fatalf("fetch failure must skip remote-head comparison:\n%s", log)
	}
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC", "DOLT_PUSH"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("fetch failure must block local compaction before %s:\n%s", forbidden, log)
		}
	}
}

func TestCompactScriptTreatsRemotePushFailureAsFatal(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite remote push failure:\n%s", out)
	}
	if !strings.Contains(out, "remote=origin push failed") {
		t.Fatalf("output missing push failure:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC", "DOLT_PUSH"} {
		if !strings.Contains(log, want) {
			t.Fatalf("push failure test missing %s:\n%s", want, log)
		}
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("push failure should write pending-push marker: %v", err)
	}
}

func TestCompactScriptFailsOnTableDiscoveryProbeFailure(t *testing.T) {
	out, doltLog, err := runCompactScriptCommand(t, "table_discovery_failure")
	if err == nil {
		t.Fatalf("compact succeeded despite table discovery failure:\n%s", out)
	}
	if !strings.Contains(out, "table list probe failed") {
		t.Fatalf("output missing table discovery failure:\n%s", out)
	}
	if !strings.Contains(out, "information_schema unavailable") {
		t.Fatalf("output missing table discovery stderr:\n%s", out)
	}
	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("table discovery failure must not flatten:\n%s", data)
	}
}

func TestCompactScriptFailsOnCommitCountProbeFailure(t *testing.T) {
	out, doltLog, err := runCompactScriptCommand(t, "commit_count_failure")
	if err == nil {
		t.Fatalf("compact succeeded despite commit count failure:\n%s", out)
	}
	if !strings.Contains(out, "commit count probe failed") {
		t.Fatalf("output missing commit count failure:\n%s", out)
	}
	if !strings.Contains(out, "dolt_log unavailable") {
		t.Fatalf("output missing commit count stderr:\n%s", out)
	}
	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("commit count failure must not flatten:\n%s", data)
	}
}

func TestCompactScriptFailsOnRowCountIncreaseBeforeGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "row_count_diverges", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite row-count increase:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten INTEGRITY check failed") {
		t.Fatalf("output missing integrity failure:\n%s", out)
	}
	if !strings.Contains(out, "row counts diverged; investigate before re-running") {
		t.Fatalf("integrity failure missing investigation guidance:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("row-count increase must not run full GC:\n%s", data)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("row-count increase should write quarantine marker: %v", err)
	}
}

func TestCompactScriptFailsOnRowCountDecreaseBeforeGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "row_count_decreases", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite row-count decrease:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten INTEGRITY check failed") {
		t.Fatalf("output missing integrity failure:\n%s", out)
	}
	if !strings.Contains(out, "row counts diverged; investigate before re-running") {
		t.Fatalf("integrity failure missing investigation guidance:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("row-count decrease must not run full GC:\n%s", data)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("row-count decrease should write quarantine marker: %v", err)
	}
}

func TestCompactScriptFailsOnSameRowCountWriterBeforeGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "same_row_count_writer", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite same-row-count live writer:\n%s", out)
	}
	if !strings.Contains(out, "value hash changed after flatten") {
		t.Fatalf("output missing value-hash integrity failure:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("same-row-count writer must not run full GC:\n%s", data)
	}
	if !strings.Contains(out, "leaving post-flatten HEAD=compactcommit in place") {
		t.Fatalf("integrity failure should preserve possible writer data for manual repair:\n%s", out)
	}
	state, err := os.ReadFile(fixture.stateFile)
	if err != nil {
		t.Fatalf("read fake dolt state: %v", err)
	}
	if strings.TrimSpace(string(state)) != "compactcommit" {
		t.Fatalf("integrity failure should not roll back possible writer data, state=%q", state)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_RESET('--hard', 'headcommit')") {
		t.Fatalf("integrity failure must not hard-reset over possible writer data:\n%s", logData)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("same-row-count writer should write quarantine marker: %v", err)
	}
}

func TestCompactScriptSurfacesRootCommitProbeFailureStderr(t *testing.T) {
	out, doltLog, err := runCompactScriptCommand(t, "root_commit_failure")
	if err == nil {
		t.Fatalf("compact succeeded despite root commit failure:\n%s", out)
	}
	if !strings.Contains(out, "root commit probe failed") || !strings.Contains(out, "root commit exploded") {
		t.Fatalf("output missing root commit failure stderr:\n%s", out)
	}
	if strings.Contains(out, "root commit probe failed — skip") {
		t.Fatalf("root commit hard failure must not be logged as a skip:\n%s", out)
	}
	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("root commit failure must not flatten:\n%s", data)
	}
}

func TestCompactScriptSurfacesRowCountProbeFailureStderr(t *testing.T) {
	out, doltLog, err := runCompactScriptCommand(t, "row_count_failure")
	if err == nil {
		t.Fatalf("compact succeeded despite row count failure:\n%s", out)
	}
	if !strings.Contains(out, "row count probe failed") || !strings.Contains(out, "row count exploded") {
		t.Fatalf("output missing row count failure stderr:\n%s", out)
	}
	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("row count failure must not flatten:\n%s", data)
	}
}

func TestCompactScriptFailsOnInvalidTableNameBeforeRowCount(t *testing.T) {
	out, doltLog, err := runCompactScriptCommand(t, "invalid_table_name")
	if err == nil {
		t.Fatalf("compact succeeded despite invalid table name:\n%s", out)
	}
	if !strings.Contains(out, "invalid table name from information_schema") {
		t.Fatalf("output missing invalid table name failure:\n%s", out)
	}
	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "SELECT COUNT(*) FROM `bad/name`") {
		t.Fatalf("invalid table name reached row-count SQL:\n%s", data)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("invalid table name must not flatten:\n%s", data)
	}
}

func TestCompactScriptRestoresHeadWhenFlattenCommitFails(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "commit_failure_after_reset", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite reset-success commit failure:\n%s", out)
	}
	if !strings.Contains(out, "commit rejected after reset") {
		t.Fatalf("output missing commit failure stderr:\n%s", out)
	}
	if !strings.Contains(out, "restored pre-flatten HEAD=headcommit") {
		t.Fatalf("output missing restore confirmation:\n%s", out)
	}
	state, err := os.ReadFile(fixture.stateFile)
	if err != nil {
		t.Fatalf("read fake dolt state: %v", err)
	}
	if strings.TrimSpace(string(state)) != "headcommit" {
		t.Fatalf("HEAD not restored, state=%q", state)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "DOLT_RESET('--hard', 'headcommit')") {
		t.Fatalf("flatten failure did not restore original HEAD:\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("flatten failure must not run full GC:\n%s", log)
	}
}

func TestCompactScriptRefusesToRestoreOverExternalHeadAdvance(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "commit_failure_after_external_head_advance", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite reset-success commit failure after external writer:\n%s", out)
	}
	if !strings.Contains(out, "commit rejected after external writer advanced HEAD") {
		t.Fatalf("output missing commit failure stderr:\n%s", out)
	}
	if !strings.Contains(out, "manual repair required") {
		t.Fatalf("output missing manual repair warning:\n%s", out)
	}
	state, err := os.ReadFile(fixture.stateFile)
	if err != nil {
		t.Fatalf("read fake dolt state: %v", err)
	}
	if strings.TrimSpace(string(state)) != "writercommit" {
		t.Fatalf("external writer HEAD was overwritten, state=%q", state)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "DOLT_RESET('--hard', 'headcommit')") {
		t.Fatalf("flatten failure must not hard-reset over external writer HEAD:\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("flatten failure must not run full GC:\n%s", log)
	}
}

func TestCompactScriptSurfacesFlattenFailureStderr(t *testing.T) {
	out, _, err := runCompactScriptCommand(t, "flatten_failure")
	if err == nil {
		t.Fatalf("compact succeeded despite flatten failure:\n%s", out)
	}
	if !strings.Contains(out, "reset exploded") {
		t.Fatalf("output missing Dolt reset/commit stderr:\n%s", out)
	}
}

func TestCompactScriptSurfacesGCFailureStderr(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "gc_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite DOLT_GC failure:\n%s", out)
	}
	if !strings.Contains(out, "gc exploded") {
		t.Fatalf("output missing Dolt GC stderr:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("GC failure should write pending-GC marker: %v", err)
	}
}

func TestCompactScriptRetriesFullGCForBelowThresholdPendingMarker(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "gc_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("first compact succeeded despite DOLT_GC failure:\n%s", firstOut)
	}
	secondOut, err := fixture.run(t, "below_threshold")
	if err != nil {
		t.Fatalf("second compact should retry pending-GC path:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "pending_gc=present") {
		t.Fatalf("second compact missing pending-GC retry explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_GC") < 2 {
		t.Fatalf("expected initial full GC and below-threshold retry:\n%s", log)
	}
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("below-threshold retry must not flatten again:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("successful pending-GC retry should clear marker, stat err=%v", err)
	}
}

func TestCompactScriptRetriesPendingGCThenPushesRemote(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "remote_gc_failure_once", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("first compact succeeded despite one-shot DOLT_GC failure:\n%s", firstOut)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	marker, err := os.ReadFile(pendingGC)
	if err != nil {
		t.Fatalf("GC failure should write pending-GC marker: %v", err)
	}
	if !strings.Contains(string(marker), "remote=origin") ||
		!strings.Contains(string(marker), "expected_remote_head=headcommit") {
		t.Fatalf("pending-GC marker should preserve remote push contract:\n%s", marker)
	}

	secondOut, err := fixture.run(t, "remote_gc_failure_once", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("second compact should retry pending-GC path and push remote:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "pending_gc=present") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("second compact missing pending-GC remote push explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_GC") < 2 {
		t.Fatalf("expected initial full GC and pending-GC retry:\n%s", log)
	}
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("pending-GC retry must not flatten again:\n%s", log)
	}
	if !strings.Contains(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("pending-GC retry should push remote-backed compaction:\n%s", log)
	}
	if _, err := os.Stat(pendingGC); !os.IsNotExist(err) {
		t.Fatalf("successful pending-GC retry should clear marker, stat err=%v", err)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("successful pending-GC retry should not leave pending-push marker, stat err=%v", err)
	}
}

func TestCompactScriptSkipsHealthyBelowThresholdOldgenWithoutPendingMarker(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	oldgen := filepath.Join(fixture.dataDir, "beads", ".dolt", "noms", "oldgen")
	if err := os.MkdirAll(oldgen, 0o755); err != nil {
		t.Fatalf("mkdir oldgen: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldgen, "archive"), []byte("healthy"), 0o644); err != nil {
		t.Fatalf("write oldgen archive marker: %v", err)
	}

	out, err := fixture.run(t, "below_threshold")
	if err != nil {
		t.Fatalf("healthy below-threshold oldgen should skip:\n%s", out)
	}
	if !strings.Contains(out, "oldgen_archives=present pending_gc=absent") {
		t.Fatalf("output missing healthy oldgen skip explanation:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("healthy below-threshold oldgen must not run full GC:\n%s", logData)
	}
}

func TestCompactScriptQuarantineBlocksSecondCycleAfterRowCountDecrease(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "row_count_decreases", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("first compact succeeded despite row-count decrease:\n%s", firstOut)
	}
	secondOut, err := fixture.run(t, "below_threshold")
	if err == nil {
		t.Fatalf("second compact succeeded despite quarantine:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "integrity quarantine marker exists") {
		t.Fatalf("second compact missing quarantine explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("quarantined database must not run full GC:\n%s", logData)
	}
}

func TestCompactScriptDryRunSkipsMutations(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_DRY_RUN=1")
	if err != nil {
		t.Fatalf("dry-run compact failed:\n%s", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run output missing explanation:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("dry-run must not issue %s:\n%s", forbidden, log)
		}
	}
}

func TestCompactScriptOnlyDBsAllowlistFiltersDatabases(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "cache", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir cache db: %v", err)
	}
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_ONLY_DBS=beads")
	if err != nil {
		t.Fatalf("allowlisted compact failed:\n%s", out)
	}
	if !strings.Contains(out, "db=cache not in GC_DOLT_COMPACT_ONLY_DBS") {
		t.Fatalf("output missing allowlist skip:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "db=cache query=") {
		t.Fatalf("non-allowlisted database should not receive dolt queries:\n%s", log)
	}
	if !strings.Contains(log, "db=beads query=") {
		t.Fatalf("allowlisted database was not queried:\n%s", log)
	}
}

func TestCompactScriptTableNameDoesNotClobberDatabaseName(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "table_name_clobber", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed when table name looked like a database: %v\n%s", err, out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "db=blocked_issues query=") {
		t.Fatalf("table validation clobbered current database name:\n%s", log)
	}
	if !strings.Contains(log, "db=beads query=SELECT COUNT(*) FROM `blocked_issues`") {
		t.Fatalf("blocked_issues table should be counted in the beads database:\n%s", log)
	}
}

func TestPhantomDBScriptQuarantinesPhantomsAndRetiredReplacements(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)

	for _, path := range []string{
		filepath.Join(dataDir, "valid", ".dolt", "noms"),
		filepath.Join(dataDir, "phantom", ".dolt"),
		filepath.Join(dataDir, "orders.replaced-20260509T010203Z", ".dolt", "noms"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	writeTestFile(t, filepath.Join(dataDir, "valid", ".dolt", "noms", "manifest"), "ok")
	writeTestFile(t, filepath.Join(dataDir, "orders.replaced-20260509T010203Z", ".dolt", "noms", "manifest"), "ok")

	out := runDogScript(t, "mol-dog-phantom-db.sh", binDir, cityPath, dataDir)
	if !strings.Contains(out, "phantoms: 1") || !strings.Contains(out, "retired: 1") || !strings.Contains(out, "quarantined: 2") {
		t.Fatalf("unexpected phantom summary:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "phantom")); !os.IsNotExist(err) {
		t.Fatalf("phantom source should be moved, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "orders.replaced-20260509T010203Z")); !os.IsNotExist(err) {
		t.Fatalf("retired replacement source should be moved, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "valid", ".dolt", "noms", "manifest")); err != nil {
		t.Fatalf("valid database should remain: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, ".quarantine", "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("quarantined entries = %d, want 2: %v", len(matches), matches)
	}
}

func writeBackupFakeDolt(t *testing.T, binDir, version string, syncExit int, sqlDatabases ...string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "dolt.log")
	dbCSV := "Database\n" + strings.Join(sqlDatabases, "\n") + "\n"
	writeExecutable(t, filepath.Join(binDir, "dolt"), fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
printf 'dolt %%s\n' "$*" >> %s
if [ "${1:-}" = "version" ]; then
  printf 'dolt version %%s\n' %s
  exit 0
fi
case "$*" in
  *"SHOW DATABASES"*)
    printf %%s %s
    exit 0
    ;;
esac
if [ "${1:-}" = "backup" ] && [ "$#" -eq 1 ]; then
  db="$(basename "$PWD")"
  printf '%%s-backup file:///backups/%%s\n' "$db" "$db"
  exit 0
fi
if [ "${1:-}" = "remote" ]; then
  printf 'remote should not be used\n' >&2
  exit 64
fi
if [ "${1:-} ${2:-}" = "backup sync" ]; then
  exit %d
fi
exit 0
`, shellQuote(logPath), shellQuote(version), shellQuote(dbCSV), syncExit))
	return logPath
}

func writeBackupFakeRsync(t *testing.T, binDir string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "rsync.log")
	writeExecutable(t, filepath.Join(binDir, "rsync"), fmt.Sprintf(`#!/bin/sh
printf 'rsync %s\n' "$*" >> %s
exit 0
`, "%s", shellQuote(logPath)))
	return logPath
}

func writeBSDLikeGrep(t *testing.T, binDir string) {
	t.Helper()
	realGrep, err := exec.LookPath("grep")
	if err != nil {
		t.Fatalf("find grep: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "grep"), fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
bre_alternation='\|'
if [ "$#" -ge 2 ] && { [ "$1" = "-vi" ] || [ "$1" = "-i" ]; } && [[ "$2" == *"$bre_alternation"* ]]; then
  if [ "$1" = "-vi" ]; then
    shift 2
    cat "$@"
    exit 0
  fi
  exit 1
fi
exec %s "$@"
`, shellQuote(realGrep)))
}

func TestBackupScriptSkipsOldDoltBeforeSync(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "prod", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)
	doltLogPath := writeBackupFakeDolt(t, binDir, "1.86.1", 0)

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, "GC_BACKUP_DATABASES=prod")
	if err == nil {
		t.Fatalf("old Dolt preflight succeeded; want failure\n%s", out)
	}
	if !strings.Contains(out, "dolt-too-old") {
		t.Fatalf("output missing dolt-too-old skip:\n%s", out)
	}
	doltLog, err := os.ReadFile(doltLogPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(doltLog), "backup sync") {
		t.Fatalf("old dolt must not reach backup sync:\n%s", doltLog)
	}
}

func TestBackupOrderTimeoutCoversScriptBudget(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "orders", "mol-dog-backup.toml"))
	if err != nil {
		t.Fatalf("read backup order: %v", err)
	}
	order, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parse backup order: %v", err)
	}

	const intendedDBs = 10
	required := 30*time.Second + intendedDBs*120*time.Second + 300*time.Second
	if got := order.TimeoutOrDefault(); got < required {
		t.Fatalf("backup order timeout = %s, want at least %s for SQL probe + %d DB syncs + offsite rsync", got, required, intendedDBs)
	}
}

func TestBackupScriptDiscoversNamedBackupsAndSyncsArtifactsOffsite(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	offsiteDir := filepath.Join(cityPath, "offsite")
	for _, path := range []string{
		filepath.Join(dataDir, "prod", ".dolt"),
		artifactDir,
		offsiteDir,
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)
	doltLogPath := writeBackupFakeDolt(t, binDir, "1.86.2", 0, "prod")
	rsyncLogPath := writeBackupFakeRsync(t, binDir)

	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, "GC_BACKUP_OFFSITE_PATH="+offsiteDir)
	if !strings.Contains(out, "synced: 1/1") || !strings.Contains(out, "offsite: ok") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	doltLog, err := os.ReadFile(doltLogPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	for _, want := range []string{"SHOW DATABASES", "backup", "backup sync prod-backup"} {
		if !strings.Contains(string(doltLog), want) {
			t.Fatalf("dolt log missing %q:\n%s", want, doltLog)
		}
	}
	if strings.Contains(string(doltLog), "remote") {
		t.Fatalf("backup discovery should not use dolt remote:\n%s", doltLog)
	}
	rsyncLog, err := os.ReadFile(rsyncLogPath)
	if err != nil {
		t.Fatalf("read rsync log: %v", err)
	}
	if !strings.Contains(string(rsyncLog), artifactDir+"/") {
		t.Fatalf("rsync should use backup artifact dir, log:\n%s", rsyncLog)
	}
	if strings.Contains(string(rsyncLog), dataDir+"/") {
		t.Fatalf("rsync must not use live data dir, log:\n%s", rsyncLog)
	}
}

func TestBackupScriptIgnoresDocumentedSystemSchemasForAutoDiscoveryWithBSDGrep(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	for _, db := range []string{"prod", "performance_schema", "sys"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", db, err)
		}
	}
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)
	writeBSDLikeGrep(t, binDir)
	doltLogPath := writeBackupFakeDolt(t, binDir, "1.86.2", 0, "prod", "performance_schema", "sys")

	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir)
	if !strings.Contains(out, "synced: 1/1") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	doltLog, err := os.ReadFile(doltLogPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	for _, systemDB := range []string{"performance_schema", "sys"} {
		if strings.Contains(string(doltLog), "backup sync "+systemDB+"-backup") {
			t.Fatalf("backup auto-discovery should ignore %s, log:\n%s", systemDB, doltLog)
		}
	}
}

func TestBackupScriptCountsFailedDatabasesByDatabase(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "prod", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	_ = writeBackupFakeDolt(t, binDir, "1.86.2", 1)

	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, "GC_BACKUP_DATABASES=prod")
	if !strings.Contains(out, "synced: 0/1") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "Backup dog: 1/1 databases failed to sync") {
		t.Fatalf("failure mail should count databases, log:\n%s", gcLog)
	}
}

func TestDoctorScriptChecksBackupArtifactFreshnessPerDatabase(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	freshBackup := filepath.Join(artifactDir, "prod.backup")
	writeTestFile(t, freshBackup, "backup")
	fresh := time.Now()
	if err := os.Chtimes(freshBackup, fresh, fresh); err != nil {
		t.Fatalf("chtimes fresh backup: %v", err)
	}
	staleBackup := filepath.Join(artifactDir, "archive.backup")
	writeTestFile(t, staleBackup, "backup")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleBackup, old, old); err != nil {
		t.Fatalf("chtimes stale backup: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\narchive\n'
    exit 0
    ;;
esac
exit 0
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_BACKUP_STALE_S=1")
	if !strings.Contains(out, "server: ok") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "archive backup is") {
		t.Fatalf("doctor did not report stale archive backup artifact, log:\n%s", gcLog)
	}
	if strings.Contains(string(gcLog), "prod backup is") {
		t.Fatalf("fresh prod backup should not be reported stale, log:\n%s", gcLog)
	}
}

func TestDoctorScriptIgnoresDocumentedSystemSchemasForBackupFreshness(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	freshBackup := filepath.Join(artifactDir, "prod.backup")
	writeTestFile(t, freshBackup, "backup")
	fresh := time.Now()
	if err := os.Chtimes(freshBackup, fresh, fresh); err != nil {
		t.Fatalf("chtimes fresh backup: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\nperformance_schema\nsys\n'
    exit 0
    ;;
esac
exit 0
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_BACKUP_STALE_S=1")
	if !strings.Contains(out, "server: ok") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	for _, systemDB := range []string{"performance_schema", "sys"} {
		if strings.Contains(string(gcLog), systemDB) {
			t.Fatalf("doctor should ignore %s for backup freshness, log:\n%s", systemDB, gcLog)
		}
	}
}

func TestDoctorScriptDetectsDoctestOrphansWithBSDGrep(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeBSDLikeGrep(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\ndoctest_leftover\ndoctortest_leftover\n'
    exit 0
    ;;
esac
exit 0
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_BACKUP_STALE_S=1")
	if !strings.Contains(out, "orphans: 2") {
		t.Fatalf("doctor should report doctest/doctortest orphan databases, output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "Orphan DBs: 2") {
		t.Fatalf("doctor advisory should report orphan count, log:\n%s", gcLog)
	}
}

func TestDoctorScriptDoesNotCreditSharedPrefixBackupToDatabase(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	freshSiblingBackup := filepath.Join(artifactDir, "prod_dev.backup")
	writeTestFile(t, freshSiblingBackup, "backup")
	fresh := time.Now()
	if err := os.Chtimes(freshSiblingBackup, fresh, fresh); err != nil {
		t.Fatalf("chtimes fresh sibling backup: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\nprod_dev\n'
    exit 0
    ;;
esac
exit 0
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_BACKUP_STALE_S=1")
	if !strings.Contains(out, "server: ok") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "prod backup missing") {
		t.Fatalf("doctor should not credit prod_dev backup to prod, log:\n%s", gcLog)
	}
	if strings.Contains(string(gcLog), "prod_dev backup") {
		t.Fatalf("fresh prod_dev backup should not be reported stale, log:\n%s", gcLog)
	}
}
