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
		"GC_DOLT_COMPACT_BARE_GC",
		"GC_FAKE_DOLT_COMPACT_MODE",
		"GC_FAKE_DOLT_COUNT_FILE",
		"GC_FAKE_DOLT_STATE_FILE",
		"GC_FAKE_DOLT_HASH_STATE_FILE",
		"GC_PACK_STATE_DIR",
		"GC_CITY_RUNTIME_DIR",
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

func replaceCompactMarkerCreatedAt(t *testing.T, markerPath, createdAt string) {
	t.Helper()
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read compact marker: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "created_at=") {
			lines[i] = "created_at=" + createdAt
			replaced = true
			break
		}
	}
	if !replaced {
		t.Fatalf("compact marker missing created_at:\n%s", data)
	}
	if err := os.WriteFile(markerPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("rewrite compact marker: %v", err)
	}
}

func compactMarkerValue(t *testing.T, markerPath, key string) string {
	t.Helper()
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read compact marker: %v", err)
	}
	prefix := key + "="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("compact marker missing %s:\n%s", key, data)
	return ""
}

func rewriteLegacyPendingPushMarker(t *testing.T, markerPath, createdAt string) {
	t.Helper()
	if err := os.WriteFile(markerPath, []byte(
		"db=beads\n"+
			"reason=flatten and full GC succeeded but remote push failed\n"+
			"created_at="+createdAt+"\n",
	), 0o600); err != nil {
		t.Fatalf("rewrite legacy pending-push marker: %v", err)
	}
}

func writeBSDOnlyDate(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "date"), `#!/bin/sh
case "$*" in
  "-u +%Y-%m-%dT%H:%M:%SZ")
    printf '2026-05-15T00:00:00Z\n'
    ;;
  "+%s"|"-u +%s")
    printf '1778803200\n'
    ;;
  "-ju -f %Y-%m-%dT%H:%M:%SZ 2026-05-15T00:00:00Z +%s")
    printf '1778803200\n'
    ;;
  "-u -d "*)
    exit 1
    ;;
  *)
    printf 'unexpected fake date args: %s\n' "$*" >&2
    exit 64
    ;;
esac
`)
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
print_cells() {
  printf '+-------+\n'
  printf '| value |\n'
  printf '+-------+\n'
  for value in "$@"; do
    printf '| %%s |\n' "$value"
  done
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
  if [ "$mode" = "head_changes_once" ]; then
    calls_file="$state_file.head-calls"
    calls=0
    if [ -f "$calls_file" ]; then
      calls="$(cat "$calls_file")"
    fi
    calls=$((calls + 1))
    printf '%%s\n' "$calls" > "$calls_file"
    if [ "$calls" -eq 2 ]; then
      printf 'writercommit\n' > "$state_file"
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
      remote_success|remote_active_branch|remote_invalid_active_branch|remote_ahead|remote_ahead_reconciled|remote_fetch_failure|remote_fetch_failure_once|remote_push_failure|remote_advances_before_push|remote_gc_failure_once|remote_empty_head_push_failure|remote_ancestry_probe_failure|remote_writer_race_before_flatten|multiple_remotes_with_origin)
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
      remote_success|remote_active_branch|remote_invalid_active_branch|remote_ahead|remote_ahead_reconciled|remote_fetch_failure|remote_fetch_failure_once|remote_push_failure|remote_advances_before_push|remote_gc_failure_once|remote_empty_head_push_failure|remote_ancestry_probe_failure|remote_writer_race_before_flatten)
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
      remote_success|remote_active_branch|remote_invalid_active_branch|remote_ahead|remote_ahead_reconciled|remote_fetch_failure|remote_fetch_failure_once|remote_push_failure|remote_advances_before_push|remote_gc_failure_once|remote_empty_head_push_failure|remote_ancestry_probe_failure|remote_writer_race_before_flatten|multiple_remotes_with_origin)
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
    if [ "$mode" = "remote_fetch_failure_once" ]; then
      calls_file="$state_file.fetch-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$calls" -eq 1 ]; then
        printf 'fetch unavailable once\n' >&2
        exit 52
      fi
    fi
    exit 0
    ;;
  *"DOLT_FETCH('backup')"*)
    exit 0
    ;;
  *"SELECT active_branch()"*)
    if [ "$mode" = "remote_active_branch" ]; then
      print_cell gascity-3
    elif [ "$mode" = "remote_invalid_active_branch" ]; then
      print_cell --force
    else
      print_cell main
    fi
    exit 0
    ;;
  *"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/main'"*)
    if [ "$mode" = "remote_advances_before_push" ] || [ "$mode" = "remote_writer_race_before_flatten" ]; then
      calls_file="$state_file.remote-head-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$mode" = "remote_writer_race_before_flatten" ] && [ "$calls" -gt 1 ]; then
        print_cell writercommit
      elif [ "$calls" -gt 1 ]; then
        print_cell remotecommit
      else
        print_cell headcommit
      fi
    elif [ "$mode" = "remote_empty_head_push_failure" ]; then
      print_cell ""
    elif [ "$mode" = "remote_ahead" ] || [ "$mode" = "remote_ahead_reconciled" ] || [ "$mode" = "remote_ancestry_probe_failure" ]; then
      print_cell remotecommit
    else
      print_cell headcommit
    fi
    exit 0
    ;;
  *"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/gascity-3'"*)
    print_cell headcommit
    exit 0
    ;;
  *"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/trunk'"*)
    print_cell headcommit
    exit 0
    ;;
  *"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/backup/main'"*)
    print_cell headcommit
    exit 0
    ;;
  *"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = 'remotecommit'"*)
    if [ "$mode" = "remote_ancestry_probe_failure" ]; then
      printf 'ancestry probe unavailable\n' >&2
      exit 54
    fi
    if [ "$mode" = "remote_ahead_reconciled" ]; then
      print_cell 1
    else
      print_cell 0
    fi
    exit 0
    ;;
  *"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = 'headcommit'"*)
    if [ "$(current_head)" = "headcommit" ]; then
      print_cell 1
    else
      print_cell 0
    fi
    exit 0
    ;;
  *"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = 'writercommit'"*)
    print_cell 0
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
    if [ "$mode" = "writer_race_db_hash_empty_pre_probe" ] && [ "$(current_head)" = "compactcommit" ]; then
      calls_file="$state_file.compact-head-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$calls" -eq 3 ]; then
        print_cell ""
        exit 0
      fi
    fi
    if [ "$mode" = "head_probe_failure_during_preflight_verify" ]; then
      # The compact retry loop probes HEAD once before preflight and once
      # after collecting counts/hash; fail the second probe to prove that
      # verify-time probe errors are not reported as HEAD movement.
      calls_file="$state_file.head-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$calls" -eq 2 ]; then
        printf 'head probe unavailable during preflight verify\n' >&2
        exit 49
      fi
    fi
    # writer_race_before_flatten: a normal MVCC writer commits inside the
    # residual window between the final stable pre-flight HEAD check and the
    # flatten's DOLT_RESET. The pre-flight loop stabilizes at headcommit, so the
    # pre-reset HEAD probe (the 3rd HEAD probe taken while still at headcommit)
    # reports writercommit. State stays at headcommit so the flatten still
    # advances HEAD to compactcommit and verify still observes the gain+drift.
    # This advance lives in the HEAD-probe arm (not current_head) so the
    # "$(current_head)" gate-checks in other arms keep seeing the real state.
    if { [ "$mode" = "writer_race_before_flatten" ] || [ "$mode" = "remote_writer_race_before_flatten" ]; } && [ "$(current_head)" = "headcommit" ]; then
      calls_file="$state_file.prereset-head-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$calls" -ge 3 ]; then
        print_cell writercommit
        exit 0
      fi
    fi
    # writer_race_during_verify: a writer commits during/after the post-flatten
    # verify. The flatten advances HEAD to compactcommit; the 1st HEAD probe at
    # compactcommit is the flatten_head probe and the 2nd is the post-verify
    # probe, which reports writercommit so HEAD has moved past the flatten's own
    # commit. verify_counts still sees compactcommit (gain+drift) because it does
    # not probe HEAD and the "$(current_head)" gates read the real state.
    if { [ "$mode" = "writer_race_during_verify" ] || [ "$mode" = "writer_race_db_hash_during_verify" ] || [ "$mode" = "writer_race_with_mixed_same_count_hash_drift" ] || [ "$mode" = "row_count_decreases_with_writer_race" ]; } && [ "$(current_head)" = "compactcommit" ]; then
      calls_file="$state_file.postverify-head-calls"
      calls=0
      if [ -f "$calls_file" ]; then
        calls="$(cat "$calls_file")"
      fi
      calls=$((calls + 1))
      printf '%%s\n' "$calls" > "$calls_file"
      if [ "$calls" -ge 2 ]; then
        print_cell writercommit
        exit 0
      fi
    fi
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
    if [ "$mode" = "db_hash_empty" ]; then
      print_cell ""
      exit 0
    fi
    if [ "$mode" = "db_hash_empty_after_flatten" ] && [ "$(current_head)" = "compactcommit" ]; then
      print_cell ""
      exit 0
    fi
    if { [ "$mode" = "writer_race_after_postverify_before_db_hash" ] || [ "$mode" = "writer_race_db_hash_empty_pre_probe" ]; } && [ "$(current_head)" = "compactcommit" ]; then
      set_head writercommit
      set_hash hash-after-writer
      print_cell hash-after-writer
      exit 0
    fi
    # row_count_gain_with_stable_hashes models the narrow probe-ordering race
    # where the preflight row count is stale but the preflight value hashes
    # already match the post-flatten values.
    print_cell "$(current_hash)"
    exit 0
    ;;
  *"DOLT_HASHOF_TABLE('beads')"*)
    if [ "$mode" = "table_hash_empty" ]; then
      print_cell ""
      exit 0
    fi
    if [ "$mode" = "table_hash_empty_after_flatten" ] && [ "$(current_head)" = "compactcommit" ]; then
      print_cell ""
      exit 0
    fi
    if { [ "$mode" = "row_count_and_hash_diverges" ] || [ "$mode" = "same_table_replacement_with_row_gain" ] || [ "$mode" = "mixed_row_count_gain_and_same_count_hash_drift" ] || [ "$mode" = "writer_race_before_flatten" ] || [ "$mode" = "remote_writer_race_before_flatten" ] || [ "$mode" = "writer_race_during_verify" ] || [ "$mode" = "writer_race_with_mixed_same_count_hash_drift" ] || [ "$mode" = "row_count_decreases_with_writer_race" ] || [ "$mode" = "row_count_decreases_with_hash_change" ]; } && [ "$(current_head)" = "compactcommit" ]; then
      print_cell hash-beads-after-writer
      exit 0
    fi
    if [ "$mode" = "same_row_count_writer" ] && [ "$(current_head)" = "compactcommit" ]; then
      print_cell hash-beads-after-writer
      exit 0
    fi
    print_cell hash-beads-before
    exit 0
    ;;
  *"DOLT_HASHOF_TABLE('notes')"*)
    if { [ "$mode" = "mixed_row_count_gain_and_same_count_hash_drift" ] || [ "$mode" = "writer_race_with_mixed_same_count_hash_drift" ] || [ "$mode" = "same_count_hash_drift_then_probe_failure" ] || [ "$mode" = "probe_failure_then_same_count_hash_drift" ]; } && [ "$(current_head)" = "compactcommit" ]; then
      print_cell hash-notes-after-writer
      exit 0
    fi
    print_cell hash-notes-before
    exit 0
    ;;
  *"DOLT_HASHOF_TABLE('blocked_issues')"*)
    print_cell hash-blocked-issues
    exit 0
    ;;
  *"information_schema.tables"*)
    if [ "$mode" = "table_discovery_failure" ]; then
      printf 'information_schema unavailable\n' >&2
      exit 43
    fi
    if [ "$mode" = "post_flatten_table_list_failure" ] && [ "$(current_head)" = "compactcommit" ]; then
      printf 'information_schema unavailable after flatten\n' >&2
      exit 43
    fi
    if [ "$mode" = "invalid_table_name" ]; then
      print_cell 'bad/name'
      exit 0
    fi
    if [ "$mode" = "post_flatten_table_appears" ] && [ "$(current_head)" = "compactcommit" ]; then
      print_cells beads notes
      exit 0
    fi
    if [ "$mode" = "post_flatten_invalid_table_name" ] && [ "$(current_head)" = "compactcommit" ]; then
      print_cells beads 'bad/name'
      exit 0
    fi
    if [ "$mode" = "table_name_clobber" ]; then
      print_cell blocked_issues
      exit 0
    fi
    if [ "$mode" = "mixed_row_count_gain_and_same_count_hash_drift" ] || [ "$mode" = "writer_race_with_mixed_same_count_hash_drift" ]; then
      print_cells beads notes
      exit 0
    fi
    if [ "$mode" = "same_count_hash_drift_then_probe_failure" ]; then
      print_cells notes beads
      exit 0
    fi
    if [ "$mode" = "probe_failure_then_same_count_hash_drift" ]; then
      print_cells beads notes
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
  *"SELECT COUNT(*) FROM"*"notes"*)
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
    if { [ "$mode" = "row_count_failure_after_flatten" ] || [ "$mode" = "same_count_hash_drift_then_probe_failure" ] || [ "$mode" = "probe_failure_then_same_count_hash_drift" ]; } && [ "$calls" -gt 1 ]; then
      printf 'row count exploded after flatten\n' >&2
      exit 47
    fi
    if { [ "$mode" = "row_count_gain_with_stable_hashes" ] || [ "$mode" = "row_count_gain_with_db_hash_drift" ] || [ "$mode" = "row_count_and_hash_diverges" ] || [ "$mode" = "same_table_replacement_with_row_gain" ] || [ "$mode" = "mixed_row_count_gain_and_same_count_hash_drift" ] || [ "$mode" = "writer_race_before_flatten" ] || [ "$mode" = "remote_writer_race_before_flatten" ] || [ "$mode" = "writer_race_during_verify" ] || [ "$mode" = "writer_race_db_hash_during_verify" ] || [ "$mode" = "writer_race_with_mixed_same_count_hash_drift" ]; } && [ "$calls" -gt 1 ]; then
      print_cell 11
    elif { [ "$mode" = "row_count_decreases" ] || [ "$mode" = "row_count_decreases_with_writer_race" ] || [ "$mode" = "row_count_decreases_with_hash_change" ]; } && [ "$calls" -gt 1 ]; then
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
    if [ "$mode" = "row_count_gain_with_db_hash_drift" ] || [ "$mode" = "same_count_db_hash_drift" ] || [ "$mode" = "row_count_and_hash_diverges" ] || [ "$mode" = "same_table_replacement_with_row_gain" ] || [ "$mode" = "writer_race_db_hash_during_verify" ]; then
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
    rm -rf -- "${GC_DOLT_DATA_DIR:-}/$db/.dolt/noms/oldgen"
    exit 0
    ;;
  *"DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')"*)
    if [ "$mode" = "remote_push_failure" ] || [ "$mode" = "remote_empty_head_push_failure" ]; then
      printf 'push unavailable\n' >&2
      exit 53
    fi
    exit 0
    ;;
  *"DOLT_PUSH('--force', '--set-upstream', 'origin', 'gascity-3')"*)
    exit 0
    ;;
  *"DOLT_PUSH('--force', '--set-upstream', 'origin', 'gascity-3:trunk')"*)
    exit 0
    ;;
  *"DOLT_PUSH('--force', '--set-upstream', 'origin', 'main:gascity-3')"*)
    if [ "$mode" = "remote_push_failure" ] || [ "$mode" = "remote_empty_head_push_failure" ]; then
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

func TestCompactScriptPushesActiveBranchToRemote(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_active_branch", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin pushed compacted gascity-3") {
		t.Fatalf("output missing active-branch remote push success:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"SELECT active_branch()",
		"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/gascity-3'",
		"CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'gascity-3')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("compact must not hardcode main for non-main active branch:\n%s", log)
	}
}

func TestCompactScriptUsesRefspecEnvOverrideForRemoteBranch(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_active_branch",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_REFSPEC_BEADS=gascity-3:trunk",
	)
	if err != nil {
		t.Fatalf("compact failed with refspec override: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin pushed compacted trunk") {
		t.Fatalf("output missing refspec remote push success:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"SELECT active_branch()",
		"SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/trunk'",
		"CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'gascity-3:trunk')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %q:\n%s", want, log)
		}
	}
}

func TestCompactScriptRejectsRefspecEnvOverrideForDifferentActiveBranch(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_active_branch",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_REFSPEC_BEADS=main:trunk",
	)
	if err == nil {
		t.Fatalf("compact succeeded with mismatched refspec local branch:\n%s", out)
	}
	if !strings.Contains(out, "refspec override local branch=main does not match active branch=gascity-3") {
		t.Fatalf("output missing mismatched refspec error:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_PUSH"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("mismatched refspec must block compact before %s:\n%s", forbidden, log)
		}
	}
}

func TestCompactScriptRefspecOptionShapedOverrideFails(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_REFSPEC_BEADS=--force",
	)
	if err == nil {
		t.Fatalf("compact succeeded with option-shaped refspec override:\n%s", out)
	}
	if !strings.Contains(out, "invalid refspec override") {
		t.Fatalf("output missing invalid refspec override error:\n%s", out)
	}
}

func TestCompactScriptWarnsWhenActiveBranchFallbacksToMain(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_invalid_active_branch", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed after active-branch fallback: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WARN: active branch unresolved; falling back to main") {
		t.Fatalf("output missing active-branch fallback warning:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("fallback should push main after warning:\n%s", log)
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

func TestCompactScriptRecordsPendingPushWhenRemoteHeadChangesAfterCompaction(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_advances_before_push", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should keep local compaction successful when remote HEAD changes before push: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin HEAD changed before push") {
		t.Fatalf("output missing remote compare-and-push marker:\n%s", out)
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
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("remote drift after compaction should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(markerData), "remote=origin") ||
		!strings.Contains(string(markerData), "expected_remote_head=headcommit") ||
		!strings.Contains(string(markerData), "expected_remote_head_verified=1") ||
		!strings.Contains(string(markerData), "compacted_from_head=headcommit") {
		t.Fatalf("pending-push marker should preserve pre-drift remote contract:\n%s", markerData)
	}
}

func TestCompactScriptPushesWhenPreflightFetchFailsOnceThenRemoteHeadIsLocal(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_fetch_failure_once", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should self-heal when post-compaction fetch recovers to a local remote HEAD: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin fetch failed") ||
		!strings.Contains(out, "pushed compacted main") {
		t.Fatalf("output should show fetch recovery and push:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	if strings.Count(log, "CALL DOLT_FETCH('origin')") < 2 {
		t.Fatalf("compact should retry fetch before push:\n%s", log)
	}
	if !strings.Contains(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("recovered fetch should allow remote push:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("successful recovered fetch should not leave pending-push marker, stat err=%v", err)
	}
}

func TestCompactScriptCompactsFromLocalSourceOfTruthWhenRemoteAheadIsUnknown(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should proceed from local source of truth despite unknown remote HEAD: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote HEAD=remotecommit is not in local history") {
		t.Fatalf("output missing remote divergence notice:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if !strings.Contains(log, want) {
			t.Fatalf("remote divergence should not block local compaction; missing %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("remote-only commits must block force push:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("remote divergence should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(markerData), "expected_remote_head=remotecommit") ||
		!strings.Contains(string(markerData), "expected_remote_head_verified=0") ||
		!strings.Contains(string(markerData), "compacted_from_head=headcommit") {
		t.Fatalf("pending-push marker should preserve unsafe remote contract:\n%s", markerData)
	}
}

func TestCompactScriptFailsRetryWhenPendingPushRemoteHeadRemainsUnverified(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should leave unverified remote push pending: %v\n%s", err, firstOut)
	}

	secondOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("pending-push retry succeeded despite still-unverified remote HEAD:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "pending_push=present") ||
		!strings.Contains(secondOut, "manual reconciliation required") {
		t.Fatalf("retry should surface a terminal manual-reconciliation state:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("pending-push retry must not flatten again:\n%s", log)
	}
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("unverified remote retry must not force-push:\n%s", log)
	}
}

func TestCompactScriptKeepsRetryDeferredWhenPendingPushAncestryProbeFails(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should leave unverified remote push pending: %v\n%s", err, firstOut)
	}

	secondOut, err := fixture.run(t, "remote_ancestry_probe_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("pending-push retry should remain deferred when ancestry probe fails: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "pending_push=present") ||
		!strings.Contains(secondOut, "ancestry probe failed") {
		t.Fatalf("retry missing deferred ancestry-probe explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("pending-push retry must not flatten again:\n%s", log)
	}
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("failed ancestry probe must not force-push:\n%s", log)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(pendingPush); err != nil {
		t.Fatalf("deferred ancestry-probe retry should keep pending-push marker: %v", err)
	}
}

func TestCompactScriptRetriesPendingPushWhenRemoteHeadBecomesLocalLogAncestor(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should leave unverified remote push pending: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(pendingPush); err != nil {
		t.Fatalf("initial compact should write pending-push marker: %v", err)
	}

	secondOut, err := fixture.run(t, "remote_ahead_reconciled", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("pending-push retry should self-heal once remote HEAD is in local history: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "is now verified in local history") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("retry missing self-heal push explanation:\n%s", secondOut)
	}
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("successful self-healed retry should clear marker, stat err=%v", err)
	}
}

func TestCompactScriptRetriesPendingPushWithRefspecRemoteBranch(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_REFSPEC_BEADS=main:gascity-3",
	)
	if err != nil {
		t.Fatalf("initial compact should leave refspec remote push pending: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	marker, err := os.ReadFile(pendingPush)
	if err != nil {
		t.Fatalf("initial compact should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(marker), "local_branch=main") ||
		!strings.Contains(string(marker), "remote_branch=gascity-3") {
		t.Fatalf("pending-push marker should preserve refspec branches:\n%s", marker)
	}

	secondOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("pending-push retry should use marker refspec: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "pending_push=present") ||
		!strings.Contains(secondOut, "pushed compacted gascity-3") {
		t.Fatalf("retry missing refspec remote push explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if !strings.Contains(string(logData), "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main:gascity-3')") {
		t.Fatalf("pending-push retry should push stored refspec:\n%s", logData)
	}
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("successful refspec retry should clear marker, stat err=%v", err)
	}
}

func TestCompactScriptAbortsWhenHeadKeepsMovingAcrossPreflightRetries(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "head_changes_before_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite HEAD moving across every preflight retry:\n%s", out)
	}
	if !strings.Contains(out, "HEAD kept moving across 3 preflight attempts") {
		t.Fatalf("compact should explain the bounded preflight abort:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("continuously moving HEAD should abort before flatten; found %s:\n%s", forbidden, log)
		}
	}
}

func TestCompactScriptRetriesPreflightWhenHeadStabilizes(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "head_changes_once", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should retry and flatten once HEAD stabilizes: %v\n%s", err, out)
	}
	if got := strings.Count(out, "HEAD moved during preflight attempt=1/3"); got != 1 {
		t.Fatalf("compact should log exactly one preflight retry, got %d:\n%s", got, out)
	}
	if strings.Contains(out, "HEAD moved during preflight attempt=2/3") ||
		strings.Contains(out, "HEAD kept moving across") {
		t.Fatalf("compact should stop retrying once HEAD stabilizes:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	if got := strings.Count(log, "DOLT_RESET"); got != 1 {
		t.Fatalf("stabilized HEAD should flatten exactly once; DOLT_RESET count=%d:\n%s", got, log)
	}
	for _, want := range []string{"DOLT_COMMIT", "DOLT_GC"} {
		if !strings.Contains(log, want) {
			t.Fatalf("stabilized HEAD should complete compaction; missing %s:\n%s", want, log)
		}
	}
}

func TestCompactScriptFailsWhenPreflightHeadVerifyProbeFails(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "head_probe_failure_during_preflight_verify", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite preflight HEAD verify probe failure:\n%s", out)
	}
	if strings.Contains(out, "HEAD kept moving") {
		t.Fatalf("probe failure must not be reported as moving HEAD:\n%s", out)
	}
	if !strings.Contains(out, "head probe unavailable during preflight verify") {
		t.Fatalf("compact should surface the underlying HEAD probe failure:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") || strings.Contains(string(data), "DOLT_COMMIT") {
		t.Fatalf("failed HEAD verify probe should abort before flatten:\n%s", data)
	}
}

func TestCompactScriptCompactsFromLocalSourceOfTruthWhenRemoteFetchFails(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should proceed from local source of truth despite remote fetch failure: %v\n%s", err, out)
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
	for _, want := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if !strings.Contains(log, want) {
			t.Fatalf("fetch failure should not block local compaction; missing %s:\n%s", want, log)
		}
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("fetch failure before post-compaction push should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(markerData), "remote=origin") ||
		!strings.Contains(string(markerData), "expected_remote_head=\n") ||
		!strings.Contains(string(markerData), "expected_remote_head_verified=0") ||
		!strings.Contains(string(markerData), "compacted_from_head=headcommit") {
		t.Fatalf("pending-push marker should preserve unknown remote contract:\n%s", markerData)
	}
}

func TestCompactScriptRetriesPendingPushWhenRemoteHeadEqualsCompactedSource(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should leave fetch-failure push pending: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	firstMarker, err := os.ReadFile(pendingPush)
	if err != nil {
		t.Fatalf("initial fetch failure should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(firstMarker), "expected_remote_head=\n") {
		t.Fatalf("initial marker should record unknown expected remote head:\n%s", firstMarker)
	}
	if !strings.Contains(string(firstMarker), "compacted_from_head=headcommit") {
		t.Fatalf("initial marker should record compacted source head:\n%s", firstMarker)
	}

	secondOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("pending-push retry should self-heal when remote fetch recovers: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "pending_push=present") ||
		!strings.Contains(secondOut, "matches compacted source head") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("retry missing recovered-fetch self-heal explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = 'headcommit'") {
		t.Fatalf("compacted-source-head retry should not depend on post-flatten local log ancestry:\n%s", logData)
	}
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("successful recovered retry should clear marker, stat err=%v", err)
	}
}

func TestCompactScriptPreservesPendingPushCreatedAtAcrossUnresolvedRetries(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should leave unverified remote push pending: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	createdAt := compactMarkerValue(t, pendingPush, "created_at")

	time.Sleep(1100 * time.Millisecond)
	secondOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("second retry should remain manually deferred while remote HEAD is unverified:\n%s", secondOut)
	}
	if got := compactMarkerValue(t, pendingPush, "created_at"); got != createdAt {
		t.Fatalf("unresolved retry refreshed pending-push marker age: before=%s after=%s\n%s", createdAt, got, secondOut)
	}

	time.Sleep(1100 * time.Millisecond)
	thirdOut, err := fixture.run(t, "remote_ancestry_probe_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("ancestry-probe failure should keep retry deferred with marker intact: %v\n%s", err, thirdOut)
	}
	if got := compactMarkerValue(t, pendingPush, "created_at"); got != createdAt {
		t.Fatalf("deferred ancestry-probe retry refreshed pending-push marker age: before=%s after=%s\n%s", createdAt, got, thirdOut)
	}
}

func TestCompactScriptParsesPendingPushCreatedAtWithBSDDateFallback(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	writeBSDOnlyDate(t, fixture.binDir)
	firstOut, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should leave fetch-failure push pending: %v\n%s", err, firstOut)
	}

	secondOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("pending-push retry should parse marker age with BSD date fallback: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "matches compacted source head") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("retry missing compacted-source self-heal explanation:\n%s", secondOut)
	}
}

func TestCompactScriptRecordsUnverifiedPendingPushWhenRemoteHeadIsEmpty(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_empty_head_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should keep local compaction successful despite remote push failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin push failed") {
		t.Fatalf("output missing push failure:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("empty remote-head push failure should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(markerData), "remote=origin") ||
		!strings.Contains(string(markerData), "expected_remote_head=\n") ||
		!strings.Contains(string(markerData), "expected_remote_head_verified=0") ||
		!strings.Contains(string(markerData), "compacted_from_head=headcommit") {
		t.Fatalf("pending-push marker should preserve unverified empty remote contract:\n%s", markerData)
	}
}

func TestCompactScriptRecordsPendingPushWhenRemotePushFails(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should keep local compaction successful despite remote push failure: %v\n%s", err, out)
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
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("push failure should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(markerData), "remote=origin") ||
		!strings.Contains(string(markerData), "expected_remote_head=headcommit") ||
		!strings.Contains(string(markerData), "expected_remote_head_verified=1") ||
		!strings.Contains(string(markerData), "compacted_from_head=headcommit") {
		t.Fatalf("pending-push marker should preserve remote push contract:\n%s", markerData)
	}
}

func TestCompactScriptBlocksStalePendingPushRetryBeforeForcePush(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first compact should succeed locally despite remote push failure: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	replaceCompactMarkerCreatedAt(t, pendingPush, "1970-01-01T00:00:00Z")

	secondOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stale pending-push retry succeeded without manual review:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "pending_push marker is stale") ||
		!strings.Contains(secondOut, "manual review required") {
		t.Fatalf("retry missing stale-marker manual-review explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Count(string(logData), "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") != 1 {
		t.Fatalf("stale pending-push retry must not attempt another force push:\n%s", logData)
	}
	if _, err := os.Stat(pendingPush); err != nil {
		t.Fatalf("stale retry should keep pending-push marker: %v", err)
	}
}

func TestCompactScriptDryRunReportsStalePendingPushMarker(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first compact should succeed locally despite remote push failure: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	replaceCompactMarkerCreatedAt(t, pendingPush, "1970-01-01T00:00:00Z")

	dryRunOut, err := fixture.run(t, "remote_success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_DRY_RUN=1",
	)
	if err == nil {
		t.Fatalf("dry-run stale pending-push retry succeeded without manual review:\n%s", dryRunOut)
	}
	if !strings.Contains(dryRunOut, "pending_push marker is stale") ||
		!strings.Contains(dryRunOut, "manual review required") {
		t.Fatalf("dry-run missing stale-marker manual-review explanation:\n%s", dryRunOut)
	}
	if strings.Contains(dryRunOut, "would retry remote push") {
		t.Fatalf("dry-run should not claim it would retry a stale pending push:\n%s", dryRunOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Count(string(logData), "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") != 1 {
		t.Fatalf("dry-run stale pending-push retry must not attempt another force push:\n%s", logData)
	}
}

func TestCompactScriptRecoversLegacyPendingPushMarkerWhenRemoteHeadIsLocal(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first compact should succeed locally despite remote push failure: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	rewriteLegacyPendingPushMarker(t, pendingPush, "1970-01-01T00:00:00Z")

	secondOut, err := fixture.run(t, "remote_ahead_reconciled", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("legacy pending-push retry should recover from current remote state: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "legacy pending_push marker recovered") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("retry missing legacy-marker recovery explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("legacy pending-push retry must not flatten again:\n%s", log)
	}
	if strings.Count(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") < 2 {
		t.Fatalf("legacy pending-push retry should attempt the deferred remote push:\n%s", log)
	}
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("successful legacy retry should clear marker, stat err=%v", err)
	}
}

func TestCompactScriptLegacyPendingPushMarkerRequiresRemoteHeadInLocalHistory(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first compact should succeed locally despite remote push failure: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	rewriteLegacyPendingPushMarker(t, pendingPush, "1970-01-01T00:00:00Z")

	secondOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("legacy pending-push retry succeeded with unverified remote HEAD:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "legacy pending_push marker recovery requires remote HEAD") ||
		!strings.Contains(secondOut, "manual intervention required") {
		t.Fatalf("retry missing legacy-marker verification failure:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("legacy pending-push retry must not flatten again:\n%s", log)
	}
	if strings.Count(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") != 1 {
		t.Fatalf("unverified legacy pending-push retry must not attempt another force push:\n%s", log)
	}
	if _, err := os.Stat(pendingPush); err != nil {
		t.Fatalf("failed legacy retry should keep pending-push marker: %v", err)
	}
}

func TestCompactScriptFailsRetryWhenPendingPushRemoteHeadChangesAgain(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_advances_before_push", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should leave changed remote push pending: %v\n%s", err, firstOut)
	}

	secondOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("pending-push retry succeeded despite still-unverified changed remote HEAD:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "pending_push=present") ||
		!strings.Contains(secondOut, "manual reconciliation required") {
		t.Fatalf("retry should surface manual reconciliation for changed remote HEAD:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("pending-push retry must not flatten again:\n%s", log)
	}
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("unverified changed remote retry must not force-push:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	markerData, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("failed retry should keep pending-push marker: %v", err)
	}
	if !strings.Contains(string(markerData), "expected_remote_head=remotecommit") ||
		!strings.Contains(string(markerData), "expected_remote_head_verified=0") ||
		!strings.Contains(string(markerData), "compacted_from_head=headcommit") {
		t.Fatalf("failed retry should update marker to latest known remote head:\n%s", markerData)
	}
}

func TestCompactScriptRetriesPendingPushBeforeBelowThresholdSkip(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first compact should succeed locally despite remote push failure: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	marker, err := os.ReadFile(pendingPush)
	if err != nil {
		t.Fatalf("push failure should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(marker), "remote=origin") ||
		!strings.Contains(string(marker), "expected_remote_head=headcommit") ||
		!strings.Contains(string(marker), "expected_remote_head_verified=1") ||
		!strings.Contains(string(marker), "compacted_from_head=headcommit") {
		t.Fatalf("pending-push marker should preserve remote push contract:\n%s", marker)
	}

	secondOut, err := fixture.run(t, "below_threshold")
	if err != nil {
		t.Fatalf("below-threshold compact should retry pending remote push: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "pending_push=present") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("second compact missing pending-push retry explanation:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("below-threshold pending-push retry must not flatten again:\n%s", log)
	}
	if strings.Count(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") < 2 {
		t.Fatalf("pending-push retry should attempt the deferred remote push:\n%s", log)
	}
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("successful pending-push retry should clear marker, stat err=%v", err)
	}
}

func TestCompactScriptFailsBeforeFlattenWhenPendingPushMarkerCannotBeWritten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	pendingPushDir := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push")
	if err := os.MkdirAll(filepath.Dir(pendingPushDir), 0o755); err != nil {
		t.Fatalf("mkdir compact state dir: %v", err)
	}
	if err := os.WriteFile(pendingPushDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write marker-dir blocker: %v", err)
	}

	out, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite required pending-push marker write failure:\n%s", out)
	}
	if !strings.Contains(out, "unable to create marker directory") {
		t.Fatalf("output missing marker write failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC", "DOLT_PUSH"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("marker write failure must fail before local mutation; saw %s:\n%s", forbidden, log)
		}
	}
	secondOut, secondErr := fixture.run(t, "below_threshold")
	if secondErr != nil {
		t.Fatalf("below-threshold follow-up should not need hidden remote repair after preflight failure: %v\n%s", secondErr, secondOut)
	}
	if !strings.Contains(secondOut, "below_threshold") {
		t.Fatalf("follow-up run should make an explicit threshold decision:\n%s", secondOut)
	}
}

func TestCompactScriptFailsBeforeFlattenWhenPendingGCMarkerCannotBeWritten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	pendingGCDir := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc")
	if err := os.MkdirAll(filepath.Dir(pendingGCDir), 0o755); err != nil {
		t.Fatalf("mkdir compact state dir: %v", err)
	}
	if err := os.WriteFile(pendingGCDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write marker-dir blocker: %v", err)
	}

	out, err := fixture.run(t, "gc_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite required pending-GC marker write failure:\n%s", out)
	}
	if !strings.Contains(out, "unable to create marker directory") {
		t.Fatalf("output missing marker write failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_RESET") {
		t.Fatalf("pending-GC marker path failure must fail before flatten:\n%s", logData)
	}
}

func TestCompactScriptFailsBeforeFlattenWhenQuarantineMarkerCannotBeWritten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	quarantineDir := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine")
	if err := os.MkdirAll(filepath.Dir(quarantineDir), 0o755); err != nil {
		t.Fatalf("mkdir compact state dir: %v", err)
	}
	if err := os.WriteFile(quarantineDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write marker-dir blocker: %v", err)
	}

	out, err := fixture.run(t, "row_count_decreases", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite required quarantine marker write failure:\n%s", out)
	}
	if !strings.Contains(out, "unable to create marker directory") {
		t.Fatalf("output missing marker write failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_RESET") {
		t.Fatalf("quarantine marker path failure must fail before flatten:\n%s", logData)
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

func TestCompactScriptAllowsRowCountIncreaseWithStableValueHashes(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "row_count_gain_with_stable_hashes", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should allow row-count increase with stable value hashes: %v\n%s", err, out)
	}
	if !strings.Contains(out, "gained rows during flatten") ||
		!strings.Contains(out, "pending value-hash verification") ||
		!strings.Contains(out, "row-count increase passed value-hash verification") ||
		!strings.Contains(out, "full GC allowed") {
		t.Fatalf("output missing row-count gain verification notices:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if !strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("row-count increase with stable value hashes should still run full GC:\n%s", data)
	}
}

func TestCompactScriptQuarantinesSameTableRowGainWithValueHashDriftBeforeFullGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "same_table_replacement_with_row_gain", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite same-table row-count gain with value-hash drift:\n%s", out)
	}
	if !strings.Contains(out, "table=beads gained rows during flatten") ||
		!strings.Contains(out, "value hash changed with row-count increase") ||
		!strings.Contains(out, "post-flatten INTEGRITY check failed") {
		t.Fatalf("output missing same-table drift quarantine notices:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "DOLT_HASHOF_TABLE('beads')") {
		t.Fatalf("same-table row-count gain test should probe table value hash:\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("same-table row-count gain with value-hash drift must block full GC:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table value hash changed with row-count increase" {
		t.Fatalf("quarantine reason should identify gained-table hash drift, got %q", reason)
	}
}

func TestCompactScriptQuarantinesMixedRowGainAndSameCountHashDriftBeforeFullGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "mixed_row_count_gain_and_same_count_hash_drift", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite mixed row gain and same-count hash drift:\n%s", out)
	}
	if !strings.Contains(out, "table=beads gained rows during flatten") {
		t.Fatalf("output missing row-count gain evidence:\n%s", out)
	}
	if !strings.Contains(out, "table=notes value hash changed after flatten without row-count increase") {
		t.Fatalf("output missing same-count hash drift warning:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "DOLT_HASHOF_TABLE('beads')") || !strings.Contains(log, "DOLT_HASHOF_TABLE('notes')") {
		t.Fatalf("mixed drift test should probe table value hashes:\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("mixed row gain and same-count hash drift must block full GC:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table value hash changed with row-count increase" {
		t.Fatalf("quarantine reason should identify first table hash drift, got %q", reason)
	}
}

func TestCompactScriptQuarantinesMixedSignalsDespiteWriterRace(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_race_with_mixed_same_count_hash_drift", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite proven writer plus same-count hash drift:\n%s", out)
	}
	if !strings.Contains(out, "writer race detected") {
		t.Fatalf("output missing proven writer evidence:\n%s", out)
	}
	if !strings.Contains(out, "table=beads gained rows during flatten") ||
		!strings.Contains(out, "table=notes value hash changed after flatten without row-count increase") {
		t.Fatalf("output missing mixed integrity signals:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("mixed hard integrity signals must block full GC despite writer race:\n%s", log)
	}
	quarantine := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(quarantine); err != nil {
		t.Fatalf("mixed hard integrity signals should write quarantine marker: %v", err)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, err := os.Stat(pendingGC); !os.IsNotExist(err) {
		t.Fatalf("mixed hard integrity signals must not write pending-GC marker; stat=%v", err)
	}
}

// assertCompactWriterRaceDeferred encodes the shared expectations for a proven
// writer-race defer: the gain+drift quarantine is downgraded to a skip, so the
// run exits 0, logs the defer message, writes NO quarantine marker, and does not
// run DOLT_GC (GC is left for the next run after the writer settles).
func assertCompactWriterRaceDeferred(t *testing.T, fixture compactScriptFixture, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("writer-race defer must exit 0 (skip, not failure): %v\n%s", err, out)
	}
	if !strings.Contains(out, "writer race detected during flatten") ||
		!strings.Contains(out, "deferring, will retry next run") {
		t.Fatalf("output missing writer-race defer message:\n%s", out)
	}
	quarantine := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, statErr := os.Stat(quarantine); !os.IsNotExist(statErr) {
		t.Fatalf("writer-race defer must NOT write a quarantine marker; stat=%v", statErr)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if reason := compactMarkerValue(t, pendingGC, "reason"); reason != "writer race during flatten deferred full GC" {
		t.Fatalf("writer-race defer should record pending-GC retry marker, got reason %q", reason)
	}
	data, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("writer-race defer must skip GC this run:\n%s", string(data))
	}
}

// A normal MVCC writer that commits in the residual window between the final
// stable pre-flight HEAD check and the flatten's reset produces a post-flatten
// row-count gain + table value-hash drift that is indistinguishable, by value
// alone, from corruption. Because HEAD captured immediately before the reset
// differs from the stable pre-flight HEAD, the writer is proven and the run
// defers instead of writing the blocking quarantine marker.
func TestCompactScriptDefersWhenWriterCommitsBeforeFlatten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_race_before_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if !strings.Contains(out, "table=beads gained rows during flatten") ||
		!strings.Contains(out, "value hash changed with row-count increase") {
		t.Fatalf("output missing the ambiguous gain+drift signal that the gate downgrades:\n%s", out)
	}
	if !strings.Contains(out, "pre_reset_HEAD=writercommit") {
		t.Fatalf("defer message should report the pre-reset writer HEAD:\n%s", out)
	}
	assertCompactWriterRaceDeferred(t, fixture, out, err)
}

// A writer that commits during/after the post-flatten verify moves HEAD past
// the flatten's own commit. That difference proves a concurrent writer and the
// gain+drift quarantine is downgraded to a defer.
func TestCompactScriptDefersWhenWriterCommitsDuringVerify(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_race_during_verify", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if !strings.Contains(out, "table=beads gained rows during flatten") ||
		!strings.Contains(out, "value hash changed with row-count increase") {
		t.Fatalf("output missing the ambiguous gain+drift signal that the gate downgrades:\n%s", out)
	}
	if !strings.Contains(out, "post_verify_HEAD=writercommit") {
		t.Fatalf("defer message should report HEAD moving past the flatten commit:\n%s", out)
	}
	assertCompactWriterRaceDeferred(t, fixture, out, err)
}

// The whole-database value hash also drifts when a concurrent writer adds rows.
// When per-table checks pass but the database hash drifts with a row gain and a
// writer is proven (HEAD moved past the flatten commit), the database-hash
// gain+drift quarantine is likewise downgraded to a defer.
func TestCompactScriptDefersWhenWriterCommitsCausingDatabaseHashDrift(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_race_db_hash_during_verify", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if !strings.Contains(out, "database value hash drift with row-count increase") {
		t.Fatalf("output missing the database-hash writer-race defer:\n%s", out)
	}
	if !strings.Contains(out, "post_verify_HEAD=writercommit") {
		t.Fatalf("defer message should report HEAD moving past the flatten commit:\n%s", out)
	}
	assertCompactWriterRaceDeferred(t, fixture, out, err)
}

func TestCompactScriptDefersWhenWriterCommitsDuringDatabaseHash(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_race_after_postverify_before_db_hash", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if !strings.Contains(out, "database value hash drift") {
		t.Fatalf("output missing database-hash drift evidence:\n%s", out)
	}
	if !strings.Contains(out, "post_db_hash_HEAD=writercommit") {
		t.Fatalf("defer message should report HEAD moving across the database hash probe:\n%s", out)
	}
	assertCompactWriterRaceDeferred(t, fixture, out, err)
}

func TestCompactScriptDefersWhenDatabaseHashPreHeadProbeIsEmptyButPostProbeProvesWriter(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "writer_race_db_hash_empty_pre_probe", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if !strings.Contains(out, "database value hash drift") {
		t.Fatalf("output missing database-hash drift evidence:\n%s", out)
	}
	if !strings.Contains(out, "pre_db_hash_HEAD=<empty>") ||
		!strings.Contains(out, "post_db_hash_HEAD=writercommit") {
		t.Fatalf("defer message should report empty pre-probe HEAD and writer post-probe HEAD:\n%s", out)
	}
	assertCompactWriterRaceDeferred(t, fixture, out, err)
}

func TestCompactScriptRetriesPendingGCAfterWriterRaceDefer(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	oldgenFile := filepath.Join(fixture.dataDir, "beads", ".dolt", "noms", "oldgen", "archive")
	if err := os.MkdirAll(filepath.Dir(oldgenFile), 0o755); err != nil {
		t.Fatalf("mkdir oldgen fixture: %v", err)
	}
	if err := os.WriteFile(oldgenFile, []byte("orphaned oldgen data"), 0o644); err != nil {
		t.Fatalf("write oldgen fixture: %v", err)
	}

	firstOut, err := fixture.run(t, "writer_race_during_verify", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	assertCompactWriterRaceDeferred(t, fixture, firstOut, err)
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if compactedFrom := compactMarkerValue(t, pendingGC, "compacted_from_head"); compactedFrom != "headcommit" {
		t.Fatalf("pending-GC marker should preserve compaction source HEAD, got %q", compactedFrom)
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
	if strings.Count(log, "DOLT_GC") != 1 {
		t.Fatalf("writer-race defer should skip GC once, then run full GC on retry:\n%s", log)
	}
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("pending-GC retry must not flatten again:\n%s", log)
	}
	if _, err := os.Stat(oldgenFile); !os.IsNotExist(err) {
		t.Fatalf("successful pending-GC retry should reclaim oldgen fixture, stat err=%v", err)
	}
	if _, err := os.Stat(pendingGC); !os.IsNotExist(err) {
		t.Fatalf("successful pending-GC retry should clear marker, stat err=%v", err)
	}
}

func TestCompactScriptRetriesRemotePendingGCAfterBeforeFlattenWriterRace(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "remote_writer_race_before_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	assertCompactWriterRaceDeferred(t, fixture, firstOut, err)
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	marker, err := os.ReadFile(pendingGC)
	if err != nil {
		t.Fatalf("writer-race defer should write pending-GC marker: %v", err)
	}
	for _, want := range []string{
		"remote=origin",
		"expected_remote_head=headcommit",
		"expected_remote_head_verified=1",
		"compacted_from_head=writercommit",
	} {
		if !strings.Contains(string(marker), want) {
			t.Fatalf("pending-GC marker missing %q:\n%s", want, marker)
		}
	}

	secondOut, err := fixture.run(t, "remote_writer_race_before_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("pending-GC retry should accept remote writer HEAD as compacted source and push: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "pending_gc=present") ||
		!strings.Contains(secondOut, "HEAD=writercommit matches compacted source head") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("pending-GC retry missing remote writer-head success evidence:\n%s", secondOut)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Count(log, "DOLT_RESET") != 1 {
		t.Fatalf("pending-GC retry must not flatten again:\n%s", log)
	}
	if !strings.Contains(log, "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("pending-GC retry should push the remote-backed compaction:\n%s", log)
	}
	if _, err := os.Stat(pendingGC); !os.IsNotExist(err) {
		t.Fatalf("successful pending-GC retry should clear marker, stat err=%v", err)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("successful pending-GC retry should not leave pending-push marker, stat err=%v", err)
	}
}

func TestCompactScriptWriterRaceGateUsesFlagNotReasonText(t *testing.T) {
	sourcePath := filepath.Join(repoRoot(t), "commands", "compact", "run.sh")
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read compact script: %v", err)
	}
	source := string(data)
	if !strings.Contains(source, "verify_counts_saw_gain_hash_drift=1") {
		t.Fatalf("writer-race gate needs a dedicated gain+hash-drift flag")
	}
	if strings.Contains(source, `verify_counts_failure_reason" = "post-flatten table value hash changed with row-count increase"`) {
		t.Fatalf("writer-race gate must not depend on the human-readable failure reason")
	}
}

// Control: the same gain+drift signal with a STABLE HEAD (no writer proven) is a
// genuine anomaly and must still write the blocking quarantine marker and fail.
// This guards against the writer-race gate weakening real-corruption detection.
func TestCompactScriptStillQuarantinesGainAndHashDriftWithStableHead(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "same_table_replacement_with_row_gain", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stable-HEAD gain+drift must remain a blocking failure:\n%s", out)
	}
	if strings.Contains(out, "writer race detected") {
		t.Fatalf("stable HEAD must not be misclassified as a writer race:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten INTEGRITY check failed") {
		t.Fatalf("stable-HEAD gain+drift should escalate as an integrity failure:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table value hash changed with row-count increase" {
		t.Fatalf("stable-HEAD gain+drift must quarantine with the gain+drift reason, got %q", reason)
	}
	data, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("stable-HEAD gain+drift must block full GC:\n%s", string(data))
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
	if !strings.Contains(out, "row counts decreased; investigate before re-running") {
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

func TestCompactScriptReportsPostFlattenRowCountProbeFailureSeparately(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "row_count_failure_after_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite post-flatten row count probe failure:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten row count probe failed") {
		t.Fatalf("output missing post-flatten row-count probe failure:\n%s", out)
	}
	if strings.Contains(out, "row counts decreased; investigate before re-running") {
		t.Fatalf("probe failure must not be reported as a row-count decrease:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten row count probe failed" {
		t.Fatalf("quarantine reason should identify probe failure, got %q", reason)
	}
}

func TestCompactScriptQuarantinesPostFlattenTableListDriftBeforeFullGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "post_flatten_table_appears", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite a new table appearing after preflight:\n%s", out)
	}
	if !strings.Contains(out, "table=notes appeared after pre-flight snapshot") ||
		!strings.Contains(out, "post-flatten table list changed") {
		t.Fatalf("output missing post-flatten table-list drift quarantine:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("post-flatten table-list drift must block full GC:\n%s", logData)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table list changed" {
		t.Fatalf("quarantine reason should identify table-list drift, got %q", reason)
	}
}

func TestCompactScriptReportsPostFlattenTableListProbeFailureSeparately(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "post_flatten_table_list_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite post-flatten table-list probe failure:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten table list probe failed") ||
		!strings.Contains(out, "information_schema unavailable after flatten") {
		t.Fatalf("output missing post-flatten table-list probe failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("post-flatten table-list probe failure must block full GC:\n%s", logData)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table list probe failed" {
		t.Fatalf("quarantine reason should identify table-list probe failure, got %q", reason)
	}
}

func TestCompactScriptQuarantinesPostFlattenInvalidTableNameBeforeFullGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "post_flatten_invalid_table_name", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite an invalid table name after preflight:\n%s", out)
	}
	if !strings.Contains(out, "invalid table name after flatten table=bad/name") ||
		!strings.Contains(out, "post-flatten table list changed") {
		t.Fatalf("output missing post-flatten invalid-table-name quarantine:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("post-flatten invalid table name must block full GC:\n%s", logData)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table list changed" {
		t.Fatalf("quarantine reason should identify table-list drift, got %q", reason)
	}
}

func TestCompactScriptPreservesRowGainReasonForDatabaseHashDrift(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "row_count_gain_with_db_hash_drift", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite database value-hash drift after row-count gain:\n%s", out)
	}
	if !strings.Contains(out, "gained rows during flatten") ||
		!strings.Contains(out, "value hash changed with row-count increase") {
		t.Fatalf("output missing row-count-gain database hash drift reason:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("database hash drift after row-count gain must block full GC:\n%s", logData)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten value hash changed with row-count increase" {
		t.Fatalf("quarantine reason should identify DB hash drift after row-count gain, got %q", reason)
	}
}

func TestCompactScriptPreservesNoGainReasonForDatabaseHashDrift(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "same_count_db_hash_drift", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite database value-hash drift without row-count gain:\n%s", out)
	}
	if !strings.Contains(out, "value hash changed without row-count increase") {
		t.Fatalf("output missing no-gain database hash drift reason:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("database hash drift without row-count gain must block full GC:\n%s", logData)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten value hash changed without row-count increase" {
		t.Fatalf("quarantine reason should identify DB hash drift without row-count gain, got %q", reason)
	}
}

func TestCompactScriptPreservesPrimaryIntegrityReasonBeforeLaterProbeFailure(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "same_count_hash_drift_then_probe_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite same-count hash drift and later probe failure:\n%s", out)
	}
	if !strings.Contains(out, "table=notes value hash changed after flatten without row-count increase") {
		t.Fatalf("output missing primary hash-drift warning:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten row count failed for table=beads") {
		t.Fatalf("output missing later probe failure:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table value hash changed without row-count increase" {
		t.Fatalf("quarantine reason should preserve primary integrity failure, got %q", reason)
	}
}

func TestCompactScriptIntegrityReasonOutranksEarlierProbeFailure(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "probe_failure_then_same_count_hash_drift", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite probe failure and later hash drift:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten row count failed for table=beads") {
		t.Fatalf("output missing initial probe failure:\n%s", out)
	}
	if !strings.Contains(out, "table=notes value hash changed after flatten without row-count increase") {
		t.Fatalf("output missing later hash-drift warning:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table value hash changed without row-count increase" {
		t.Fatalf("quarantine reason should prefer integrity failure over earlier probe failure, got %q", reason)
	}
}

func TestCompactScriptQuarantinesSameRowCountWriterBeforeFullGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "same_row_count_writer", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite same-row-count value-hash drift:\n%s", out)
	}
	if !strings.Contains(out, "value hash changed after flatten") {
		t.Fatalf("output missing value-hash drift warning:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "DOLT_HASHOF_DB") {
		t.Fatalf("same-row-count writer test should probe database value hash:\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("same-row-count value-hash drift must block full GC:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("same-row-count value-hash drift should write quarantine marker: %v", err)
	}
}

func TestCompactScriptFailsOnEmptyPreflightValueHash(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "db_hash_empty", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite empty preflight value hash:\n%s", out)
	}
	if !strings.Contains(out, "pre-flatten value hash probe returned empty value") {
		t.Fatalf("output missing empty preflight hash failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_RESET") {
		t.Fatalf("empty preflight hash must fail before flatten:\n%s", logData)
	}
}

func TestCompactScriptFailsOnEmptyPreflightTableValueHash(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "table_hash_empty", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite empty preflight table value hash:\n%s", out)
	}
	if !strings.Contains(out, "pre-flight table value hash returned empty value") {
		t.Fatalf("output missing empty preflight table hash failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_RESET") {
		t.Fatalf("empty preflight table hash must fail before flatten:\n%s", logData)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("empty preflight table hash must not write quarantine marker")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat quarantine marker: %v", err)
	}
}

func TestCompactScriptQuarantinesEmptyPostflightValueHashBeforeFullGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "db_hash_empty_after_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite empty postflight value hash:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten value hash probe returned empty value") {
		t.Fatalf("output missing empty postflight hash failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "DOLT_RESET") {
		t.Fatalf("postflight hash test should flatten before detecting empty hash:\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("empty postflight hash must block full GC:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("empty postflight hash should write quarantine marker: %v", err)
	}
}

func TestCompactScriptQuarantinesEmptyPostflightTableValueHashBeforeFullGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "table_hash_empty_after_flatten", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded despite empty postflight table value hash:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten table value hash returned empty value") {
		t.Fatalf("output missing empty postflight table hash failure:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "DOLT_RESET") {
		t.Fatalf("postflight table hash test should flatten before detecting empty hash:\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("empty postflight table hash must block full GC:\n%s", log)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten table value hash probe failed" {
		t.Fatalf("quarantine reason should identify table hash probe failure, got %q", reason)
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
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	marker, err := os.ReadFile(pendingGC)
	if err != nil {
		t.Fatalf("GC failure should write pending-GC marker: %v", err)
	}
	if !strings.Contains(string(marker), "expected_remote_head_verified=0") ||
		!strings.Contains(string(marker), "compacted_from_head=headcommit") {
		t.Fatalf("pending-GC marker should preserve unverified empty remote contract:\n%s", marker)
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
	if _, err := os.Stat(pendingGC); !os.IsNotExist(err) {
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
		!strings.Contains(string(marker), "expected_remote_head=headcommit") ||
		!strings.Contains(string(marker), "expected_remote_head_verified=1") ||
		!strings.Contains(string(marker), "compacted_from_head=headcommit") {
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

func TestCompactScriptKeepsPendingGCWhenPendingPushHandoffCannotBeWritten(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "remote_gc_failure_once", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("first compact should fail after writing pending-GC marker:\n%s", firstOut)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, err := os.Stat(pendingGC); err != nil {
		t.Fatalf("GC failure should write pending-GC marker: %v", err)
	}
	pendingPushDir := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push")
	if err := os.RemoveAll(pendingPushDir); err != nil {
		t.Fatalf("remove pending-push dir before blocker install: %v", err)
	}
	if err := os.WriteFile(pendingPushDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write pending-push dir blocker: %v", err)
	}

	secondOut, err := fixture.run(t, "remote_ahead", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("pending-GC retry should fail when replacement pending-push marker cannot be written:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "unable to create marker directory") {
		t.Fatalf("retry missing pending-push marker write failure:\n%s", secondOut)
	}
	if _, err := os.Stat(pendingGC); err != nil {
		t.Fatalf("failed pending-push handoff should keep pending-GC marker: %v\n%s", err, secondOut)
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
	quarantine := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if !strings.Contains(secondOut, quarantine) ||
		!strings.Contains(secondOut, "reason=post-flatten row count decreased") {
		t.Fatalf("second compact missing quarantine marker details:\n%s", secondOut)
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

func TestCompactScriptAllowsExplicitLocalExternalEndpointWithoutManagedState(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	externalRoot := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "external-target")
	if err := os.MkdirAll(filepath.Join(externalRoot, "beads", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir external target db: %v", err)
	}

	out, err := fixture.run(t, "success",
		"GC_DOLT_MANAGED_LOCAL=0",
		"GC_DOLT_HOST=127.0.0.2",
		"GC_DOLT_DATA_DIR="+externalRoot,
		"GC_DOLT_STATE_FILE="+filepath.Join(externalRoot, "dolt-state.json"),
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_DRY_RUN=1",
	)
	if err != nil {
		t.Fatalf("dry-run compact against explicit local external endpoint failed:\n%s", out)
	}
	for _, unwanted := range []string{
		"managed local Dolt runtime is not applicable",
		"does not match managed runtime port",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("explicit local endpoint should not be treated as inactive managed runtime:\n%s", out)
		}
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run output missing explanation:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if !strings.Contains(string(logData), "db=beads query=") {
		t.Fatalf("explicit local endpoint did not query discovered database:\n%s", logData)
	}
}

func TestCompactScriptSkipsNonLocalExternalEndpoint(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success",
		"GC_DOLT_MANAGED_LOCAL=0",
		"GC_DOLT_HOST=external.example.internal",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_DRY_RUN=1",
	)
	if err != nil {
		t.Fatalf("non-local external endpoint skip should exit cleanly:\n%s", out)
	}
	if !strings.Contains(out, "GC_DOLT_HOST=external.example.internal is not a local Dolt compaction target") {
		t.Fatalf("output missing non-local external skip:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.TrimSpace(string(logData)) != "" {
		t.Fatalf("non-local external endpoint should not be queried:\n%s", logData)
	}
}

func TestCompactScriptSkipsNonLocalExternalEndpointWithoutPort(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	externalRoot := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "external-target")
	if err := os.MkdirAll(externalRoot, 0o755); err != nil {
		t.Fatalf("mkdir external target root: %v", err)
	}

	out, err := fixture.run(t, "success",
		"GC_DOLT_MANAGED_LOCAL=0",
		"GC_DOLT_HOST=external.example.internal",
		"GC_DOLT_PORT=",
		"GC_DOLT_DATA_DIR="+externalRoot,
		"GC_DOLT_STATE_FILE="+filepath.Join(externalRoot, "dolt-state.json"),
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_DRY_RUN=1",
	)
	if err != nil {
		t.Fatalf("non-local external endpoint without a port should skip cleanly:\n%s", out)
	}
	if !strings.Contains(out, "GC_DOLT_PORT is empty") {
		t.Fatalf("output missing empty-port external skip:\n%s", out)
	}
	if strings.Contains(out, "cannot resolve runtime port") {
		t.Fatalf("runtime port resolution should not run before external skip:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.TrimSpace(string(logData)) != "" {
		t.Fatalf("non-local external endpoint without a port should not be queried:\n%s", logData)
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

func TestCompactScriptOnlyDBsCanTargetUndiscoveredDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_ONLY_DBS=ga",
		"GC_DOLT_COMPACT_DRY_RUN=1",
	)
	if err != nil {
		t.Fatalf("explicit allowlisted compact failed:\n%s", out)
	}
	if !strings.Contains(out, "db=beads not in GC_DOLT_COMPACT_ONLY_DBS") ||
		!strings.Contains(out, "db=ga commits=") ||
		!strings.Contains(out, "dry-run") {
		t.Fatalf("output missing explicit allowlist target or discovered-db skip:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "db=beads query=") {
		t.Fatalf("non-allowlisted discovered database should not receive dolt queries:\n%s", log)
	}
	if !strings.Contains(log, "db=ga query=") {
		t.Fatalf("explicit allowlisted database was not queried:\n%s", log)
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

// Bare-GC mode (issue #2615): GC_DOLT_COMPACT_BARE_GC=1 must bypass the
// threshold + flatten path and run a single bare CALL DOLT_GC() per
// discovered database. The full DOLT_GC --full path is the wrong tool for
// the NBS journal range index — a working-set GC (bare DOLT_GC()) resets
// the index without rewriting history.

func TestCompactScriptBareGCBypassesThresholdAndSkipsFlatten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "below_threshold",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_BARE_GC=1")
	if err != nil {
		t.Fatalf("bare-gc compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "db=beads bare-gc duration=") {
		t.Fatalf("bare-gc output missing success line:\n%s", out)
	}
	if strings.Contains(out, "below_threshold=") {
		t.Fatalf("bare-gc must skip the threshold gate:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_PUSH", "DOLT_FETCH"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("bare-gc must not issue %s:\n%s", forbidden, log)
		}
	}
	if !strings.Contains(log, "CALL DOLT_GC()") {
		t.Fatalf("bare-gc must issue bare CALL DOLT_GC():\n%s", log)
	}
	if strings.Contains(log, "DOLT_GC('--full')") {
		t.Fatalf("bare-gc must NOT run --full:\n%s", log)
	}
}

func TestCompactScriptBareGCDryRunSkipsMutations(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_BARE_GC=1",
		"GC_DOLT_COMPACT_DRY_RUN=1")
	if err != nil {
		t.Fatalf("bare-gc dry-run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-run (would bare GC)") {
		t.Fatalf("bare-gc dry-run output missing explanation:\n%s", out)
	}
	// Bare-GC dry-run issues no dolt queries at all, so the fake dolt log
	// may not exist. Tolerate that and assert only on presence.
	if logData, err := os.ReadFile(fixture.doltLog); err == nil {
		if strings.Contains(string(logData), "DOLT_GC") {
			t.Fatalf("bare-gc dry-run must not issue DOLT_GC:\n%s", logData)
		}
	}
}

func TestCompactScriptBareGCHonorsOnlyDBsAllowlist(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "cache", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir cache db: %v", err)
	}
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_BARE_GC=1",
		"GC_DOLT_COMPACT_ONLY_DBS=beads")
	if err != nil {
		t.Fatalf("bare-gc allowlist compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "db=cache not in GC_DOLT_COMPACT_ONLY_DBS") {
		t.Fatalf("bare-gc output missing allowlist skip:\n%s", out)
	}
	if !strings.Contains(out, "db=beads bare-gc duration=") {
		t.Fatalf("bare-gc output missing success for allowlisted db:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "db=cache query=") {
		t.Fatalf("non-allowlisted database should not receive dolt queries:\n%s", log)
	}
	if !strings.Contains(log, "db=beads query=CALL DOLT_GC()") {
		t.Fatalf("allowlisted database was not bare-GC'd:\n%s", log)
	}
}

func TestCompactScriptBareGCRefusesQuarantinedDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	quarantineMarker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if err := os.MkdirAll(filepath.Dir(quarantineMarker), 0o755); err != nil {
		t.Fatalf("mkdir quarantine dir: %v", err)
	}
	if err := os.WriteFile(quarantineMarker, []byte("db=beads\nreason=test\ncreated_at=2026-05-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatalf("write quarantine marker: %v", err)
	}
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_BARE_GC=1")
	if err == nil {
		t.Fatalf("bare-gc must fail when quarantine marker exists:\n%s", out)
	}
	if !strings.Contains(out, "integrity quarantine marker exists") {
		t.Fatalf("bare-gc output missing quarantine explanation:\n%s", out)
	}
	if !strings.Contains(out, quarantineMarker) ||
		!strings.Contains(out, "reason=test") ||
		!strings.Contains(out, "created_at=2026-05-01T00:00:00Z") {
		t.Fatalf("bare-gc output missing quarantine marker details:\n%s", out)
	}
	// Quarantine refusal exits before any dolt query, so the fake dolt log
	// may not exist. Tolerate that and assert only on presence.
	if logData, err := os.ReadFile(fixture.doltLog); err == nil {
		if strings.Contains(string(logData), "DOLT_GC") {
			t.Fatalf("quarantined database must not be bare-GC'd:\n%s", logData)
		}
	}
}

func TestCompactScriptBareGCSurfacesDoltGCFailureStderr(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "gc_failure",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_BARE_GC=1")
	if err == nil {
		t.Fatalf("bare-gc must fail when DOLT_GC fails:\n%s", out)
	}
	if !strings.Contains(out, "gc exploded") {
		t.Fatalf("bare-gc output missing Dolt GC stderr:\n%s", out)
	}
	if !strings.Contains(out, "bare-gc failed rc=") {
		t.Fatalf("bare-gc output missing failure summary:\n%s", out)
	}
	if !strings.Contains(out, "1 database(s) failed bare GC") {
		t.Fatalf("bare-gc output missing per-run failure tally:\n%s", out)
	}
	// Bare GC must NOT write flatten-bookkeeping markers — those describe
	// flatten remediation state that bare GC never enters.
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, err := os.Stat(pendingGC); !os.IsNotExist(err) {
		t.Fatalf("bare-gc failure must not write pending-GC marker, stat err=%v", err)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(pendingPush); !os.IsNotExist(err) {
		t.Fatalf("bare-gc failure must not write pending-push marker, stat err=%v", err)
	}
	quarantine := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatalf("bare-gc failure must not write quarantine marker, stat err=%v", err)
	}
}

func TestCompactScriptBareGCRejectsInvalidValue(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_BARE_GC=bogus")
	if err == nil {
		t.Fatalf("bare-gc must reject invalid value:\n%s", out)
	}
	if !strings.Contains(out, "invalid GC_DOLT_COMPACT_BARE_GC=bogus") {
		t.Fatalf("bare-gc output missing invalid-value diagnostic:\n%s", out)
	}
	// Invalid env exits during validation, before any dolt query, so the
	// fake dolt log may not exist. Tolerate that and assert only on
	// presence.
	if logData, err := os.ReadFile(fixture.doltLog); err == nil {
		if strings.Contains(string(logData), "DOLT_GC") {
			t.Fatalf("invalid env value must exit before any DOLT_GC call:\n%s", logData)
		}
	}
}

func TestCompactScriptBareGCDisabledWhenEnvFalsy(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "below_threshold",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_BARE_GC=0")
	if err != nil {
		t.Fatalf("falsy bare-gc compact failed: %v\n%s", err, out)
	}
	// Falsy bare-gc must fall through to the normal threshold-gated path:
	// in below_threshold mode that means the standard "below_threshold" skip
	// line, NOT a bare-GC success.
	if !strings.Contains(out, "below_threshold=500") {
		t.Fatalf("falsy bare-gc must defer to the threshold path:\n%s", out)
	}
	if strings.Contains(out, "bare-gc") {
		t.Fatalf("falsy bare-gc must not execute the bare-GC path:\n%s", out)
	}
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("falsy bare-gc + below threshold must not call DOLT_GC:\n%s", logData)
	}
}

func TestPhantomDBScriptEscalatesAndPreservesAllDatabases(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)

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
	if !strings.Contains(out, "phantoms: 1") || !strings.Contains(out, "retired: 1") || !strings.Contains(out, "escalated: 2") {
		t.Fatalf("unexpected phantom summary:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "phantom", ".dolt")); err != nil {
		t.Fatalf("phantom source moved unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "orders.replaced-20260509T010203Z", ".dolt", "noms", "manifest")); err != nil {
		t.Fatalf("retired replacement moved unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "valid", ".dolt", "noms", "manifest")); err != nil {
		t.Fatalf("valid database should remain: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, ".quarantine", "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("quarantine directory non-empty: got %d entries: %v", len(matches), matches)
	}
	gcLogData, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	gcLog := string(gcLogData)
	if !strings.Contains(gcLog, "unservable database directories") {
		t.Fatalf("escalation should use neutral unservable wording:\n%s", gcLog)
	}
	if !strings.Contains(gcLog, "Phantoms missing noms/manifest: 1 phantom") {
		t.Fatalf("escalation should report phantom directories separately:\n%s", gcLog)
	}
	if !strings.Contains(gcLog, "Retired replacement directories: 1 orders.replaced-20260509T010203Z") {
		t.Fatalf("escalation should report retired replacements separately:\n%s", gcLog)
	}
	if !strings.Contains(gcLog, "--from controller") {
		t.Fatalf("phantom-db escalation mail must pass --from controller so it is not attributed to 'human':\n%s", gcLog)
	}
	if strings.Contains(gcLog, "phantom database(s)") {
		t.Fatalf("escalation should not label all unservables as phantoms:\n%s", gcLog)
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
	gcLogPath := writeDogFakeGC(t, binDir)
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
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "--from controller") {
		t.Fatalf("old-Dolt backup escalation mail must pass --from controller so it is not attributed to 'human':\n%s", gcLog)
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
	doltLogPath := writeBackupFakeDolt(t, binDir, "2.1.0", 0, "prod")
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

func TestBackupScriptSkipsConcurrentRunBeforeBackupSync(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed; skipping")
	}

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "prod", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)

	doltLogPath := filepath.Join(binDir, "dolt.log")
	startedFile := filepath.Join(binDir, "sync-started")
	releaseFile := filepath.Join(binDir, "sync-release")
	writeExecutable(t, filepath.Join(binDir, "dolt"), fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
printf 'dolt %%s\n' "$*" >> %s
if [ "${1:-}" = "version" ]; then
  printf 'dolt version 2.1.0\n'
  exit 0
fi
case "$*" in
  *"SHOW DATABASES"*)
    printf 'Database\nprod\n'
    exit 0
    ;;
esac
if [ "${1:-}" = "backup" ] && [ "$#" -eq 1 ]; then
  db="$(basename "$PWD")"
  printf '%%s-backup file:///backups/%%s\n' "$db" "$db"
  exit 0
fi
if [ "${1:-} ${2:-}" = "backup sync" ]; then
  : > %s
  while [ ! -f %s ]; do sleep 0.05; done
  exit 0
fi
exit 0
`, shellQuote(doltLogPath), shellQuote(startedFile), shellQuote(releaseFile)))

	firstDone := make(chan struct{})
	var firstOut string
	var firstErr error
	go func() {
		firstOut, firstErr = runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, "GC_BACKUP_DATABASES=prod")
		close(firstDone)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first backup run did not reach backup sync")
		}
		time.Sleep(25 * time.Millisecond)
	}

	secondDone := make(chan struct{})
	var secondOut string
	var secondErr error
	go func() {
		secondOut, secondErr = runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
			"GC_BACKUP_DATABASES=prod",
			"GC_DOLT_BACKUP_LOCK_WAIT_SECONDS=0",
		)
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		if err := os.WriteFile(releaseFile, []byte("ok\n"), 0o644); err != nil {
			t.Fatalf("release blocked backup runs: %v", err)
		}
		<-secondDone
		t.Fatalf("second backup run blocked instead of skipping while lock is held:\n%s", secondOut)
	}
	if secondErr != nil {
		t.Fatalf("second backup run failed: %v\n%s", secondErr, secondOut)
	}
	if !strings.Contains(secondOut, "already running") {
		t.Fatalf("second backup run should skip while lock is held:\n%s", secondOut)
	}

	if err := os.WriteFile(releaseFile, []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("release first backup run: %v", err)
	}
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first backup run did not finish after release")
	}
	if firstErr != nil {
		t.Fatalf("first backup run failed: %v\n%s", firstErr, firstOut)
	}

	doltLog, err := os.ReadFile(doltLogPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if got := strings.Count(string(doltLog), "backup sync prod-backup"); got != 1 {
		t.Fatalf("backup sync count = %d, want 1 while concurrent run skipped:\n%s", got, doltLog)
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
	doltLogPath := writeBackupFakeDolt(t, binDir, "2.1.0", 0, "prod", "performance_schema", "sys")

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
	_ = writeBackupFakeDolt(t, binDir, "2.1.0", 1)

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
	if !strings.Contains(string(gcLog), "--from controller") {
		t.Fatalf("backup failure mail must pass --from controller so it is not attributed to 'human':\n%s", gcLog)
	}
}

func TestDoctorScriptChecksBackupArtifactFreshnessPerDatabase(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	for _, db := range []string{"prod", "archive"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", db, err)
		}
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
case "$1" in
  backup)
    case "$(basename "$PWD")" in
      prod) printf 'prod-backup\n' ;;
      archive) printf 'archive-backup\n' ;;
    esac
    exit 0
    ;;
esac
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

// TestDoctorBackupOnlyChecksDBsWithBackupRemote asserts mol-dog-doctor's backup
// freshness scope mirrors mol-dog-backup.sh — only DBs with a configured
// "<db>-backup" remote are eligible. Cities with user DBs but no backup
// remotes (legitimate config) get no false stale-backup alarms.
//
// Companion to TestBackupScriptIgnoresDocumentedSystemSchemasForAutoDiscovery:
// backup.sh already filters by remote presence; doctor.sh must use the same
// gate so the two scripts agree on what "backup-eligible" means.
func TestDoctorBackupOnlyChecksDBsWithBackupRemote(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	for _, db := range []string{"prod", "archive"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", db, err)
		}
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  backup)
    if [ "$(basename "$PWD")" = "prod" ]; then
      printf 'prod-backup\n'
    fi
    exit 0
    ;;
esac
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
	if strings.Contains(string(gcLog), "archive backup missing") {
		t.Fatalf("doctor warned about archive (no <db>-backup remote configured); should be filtered out:\n%s", gcLog)
	}
	if !strings.Contains(string(gcLog), "prod backup missing") {
		t.Fatalf("doctor did not warn about prod (eligible: has prod-backup remote, no artifact); scope filter should not exclude it:\n%s", gcLog)
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
	for _, db := range []string{"prod", "prod_dev"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", db, err)
		}
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
case "$1" in
  backup)
    case "$(basename "$PWD")" in
      prod) printf 'prod-backup\n' ;;
      prod_dev) printf 'prod_dev-backup\n' ;;
    esac
    exit 0
    ;;
esac
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

// TestDoctorScriptAdvisoryMailCarriesExplicitSenderIdentity asserts the
// latency-WARN advisory mail path passes `--from controller` so the message
// is attributed to the controller-spawned exec order instead of falling
// back to "human" (the documented default when $GC_ALIAS / $GC_AGENT are
// unset, which is the case for controller-spawned exec orders).
func TestDoctorScriptAdvisoryMailCarriesExplicitSenderIdentity(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    exit 0
    ;;
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\n'
    exit 0
    ;;
esac
exit 0
`)

	// LATENCY_WARN_S=0 makes the latency check fire on every run because
	// PROBE_END - PROBE_START >= 0 always. That guarantees the advisory
	// mail path executes regardless of probe duration.
	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")
	if !strings.Contains(out, "server: ok") {
		t.Fatalf("doctor should report server ok when probe succeeds, output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "Dolt health advisory") {
		t.Fatalf("advisory mail did not fire; latency-WARN should have triggered, log:\n%s", gcLog)
	}
	if !strings.Contains(string(gcLog), "--from controller") {
		t.Fatalf("advisory mail must pass --from controller so it is not attributed to 'human', log:\n%s", gcLog)
	}
}

// TestDoctorScriptUnreachableEscalationCarriesExplicitSenderIdentity asserts
// the server-unreachable ESCALATION mail path also passes `--from controller`.
// Both advisory call sites share the same failure mode (defaulting to "human")
// when the controller-spawned script runs without $GC_ALIAS / $GC_AGENT set,
// so both need the regression check.
func TestDoctorScriptUnreachableEscalationCarriesExplicitSenderIdentity(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	// Fake dolt fails the active_branch() probe, which forces the script
	// down the unreachable-server ESCALATION branch.
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
exit 1
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir)
	if !strings.Contains(out, "server unreachable") {
		t.Fatalf("doctor should report server unreachable when probe fails, output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "ESCALATION: Dolt server unreachable") {
		t.Fatalf("unreachable escalation mail did not fire, log:\n%s", gcLog)
	}
	if !strings.Contains(string(gcLog), "--from controller") {
		t.Fatalf("unreachable escalation mail must pass --from controller so it is not attributed to 'human', log:\n%s", gcLog)
	}
}

func TestDoctorScriptUnreachableEscalationReportsMailFailure(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
if [ "$1" = "mail" ]; then
  echo 'invalid sender "controller"' >&2
  exit 1
fi
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
exit 1
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir)
	if !strings.Contains(out, "doctor: mail send failed: invalid sender \"controller\"") {
		t.Fatalf("doctor should surface mail failure, output:\n%s", out)
	}
	if !strings.Contains(out, "server unreachable on port 3307 (mail failed)") {
		t.Fatalf("doctor should not claim escalation success after mail failure, output:\n%s", out)
	}
	if strings.Contains(out, "server unreachable on port 3307 (escalated)") {
		t.Fatalf("doctor claimed escalation success after mail failure, output:\n%s", out)
	}
}

// A concurrent DELETE proven via HEAD movement produces a row-count decrease
// plus table-value-hash change. The new verify_counts_saw_decrease_hash_drift
// flag ensures these are NOT classified as same-count hash drift, so the
// writer-race defer path fires: exit 0, no quarantine, pending-GC marker written.
func TestCompactScriptDefersWhenWriterDeletesRows(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "row_count_decreases_with_writer_race", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("concurrent-DELETE defer must exit 0 (skip, not failure): %v\n%s", err, out)
	}
	if !strings.Contains(out, "row-count decrease is concurrent-writer DELETE, not corruption") {
		t.Fatalf("output missing concurrent-DELETE defer message:\n%s", out)
	}
	if !strings.Contains(out, "deferring, will retry next run") {
		t.Fatalf("output missing defer confirmation:\n%s", out)
	}
	quarantine := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, statErr := os.Stat(quarantine); !os.IsNotExist(statErr) {
		t.Fatalf("concurrent-DELETE defer must NOT write a quarantine marker; stat=%v", statErr)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if reason := compactMarkerValue(t, pendingGC, "reason"); reason != "writer race during flatten deferred full GC" {
		t.Fatalf("concurrent-DELETE defer should record pending-GC retry marker, got reason %q", reason)
	}
	data, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("concurrent-DELETE defer must skip GC this run:\n%s", string(data))
	}
}

// Control: a row-count decrease with a STABLE HEAD (no proven concurrent writer)
// must still quarantine. This guards the defer gate so it cannot fire on
// unexplained data loss.
func TestCompactScriptStillQuarantinesRowDecreaseWithStableHead(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	// row_count_decreases_with_hash_change: count drops + hash changes, HEAD stays
	// at compactcommit (no writer proven) — should quarantine, not defer.
	out, err := fixture.run(t, "row_count_decreases_with_hash_change", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stable-HEAD row-decrease must remain a blocking failure:\n%s", out)
	}
	if strings.Contains(out, "writer race detected") {
		t.Fatalf("stable HEAD must not be misclassified as a writer race:\n%s", out)
	}
	if !strings.Contains(out, "post-flatten INTEGRITY check failed") {
		t.Fatalf("stable-HEAD row-decrease should escalate as an integrity failure:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stable-HEAD row-decrease must write quarantine marker: %v", err)
	}
	data, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(data), "DOLT_GC") {
		t.Fatalf("stable-HEAD row-decrease must block full GC:\n%s", string(data))
	}
}
