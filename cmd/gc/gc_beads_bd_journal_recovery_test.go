package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// These tests exercise the journal-corruption auto-recovery helpers from
// examples/bd/assets/scripts/gc-beads-bd.sh (#3176) against a stub `dolt`
// binary. The contract under test:
//   - only databases whose offline probe confirms journal corruption are
//     touched;
//   - restore only runs from a non-empty local file:// backup registered in
//     the database's repo_state.json — bd's canonical `default` backup (what
//     mol-dog-backup registers via `bd backup init` since #6751) or the
//     legacy <db>-backup name, `default` taking precedence when both exist;
//   - the restore runs in a staging directory and replaces the corrupt store
//     only once it has succeeded; the store is then preserved (moved aside,
//     never deleted);
//   - the swap only renames, so a kill at any step leaves the store or the
//     finished copy for the next start, which moves the copy in before the
//     server starts; recovery refuses directories a rename cannot reach;
//   - fail-closed: when the selected backup is unusable, or the restore
//     fails, runs out of time or is killed, the store is still in the data
//     dir and the function fails so the server cannot start silently missing
//     a database;
//   - a restore is recorded in the dolt log, which outlives the stderr the
//     provider's caller discards on success, and the restored copy has the
//     backup it came from registered again.

// journalRecoveryHarness extracts the recovery functions into a script that
// runs attempt_journal_corruption_recovery. maskJQ hides jq via a `command`
// override so the awk fallback is what runs regardless of the host toolchain.
func journalRecoveryHarness(t *testing.T, maskJQ bool) string {
	t.Helper()
	return journalRecoveryScript(t, maskJQ, "attempt_journal_corruption_recovery")
}

// journalNextStartHarness runs what op_start runs after a recovery stopped
// part way: finish_interrupted_journal_recovery before the server starts,
// then, when stillCorrupt says the server fails on the store again, the
// recovery.
func journalNextStartHarness(t *testing.T, stillCorrupt bool) string {
	t.Helper()
	if stillCorrupt {
		return journalRecoveryScript(t, false, "finish_interrupted_journal_recovery && attempt_journal_corruption_recovery")
	}
	return journalRecoveryScript(t, false, "finish_interrupted_journal_recovery")
}

// journalRecoveryScript extracts the recovery functions into a runnable
// script whose last line is entry.
func journalRecoveryScript(t *testing.T, maskJQ bool, entry string) string {
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
	if maskJQ {
		// The `command` override masks jq so the awk fallback is what
		// gets exercised regardless of the host toolchain.
		harness.WriteString("command() { case \"$*\" in '-v jq') return 1 ;; esac; builtin command \"$@\"; }\n")
	}
	for _, fn := range []string{
		"run_with_timeout",
		"journal_corruption_signature",
		"database_journal_corrupt",
		"backup_registry_entries",
		"backup_registry_has_backup",
		"backup_registry_backup_url",
		"backup_registry_summary",
		"backup_remote_url_for_recovery",
		"backup_restore_refusal",
		"journal_recovery_note",
		"reregister_restored_backup",
		"journal_restore_limit",
		"restore_backup_within",
		"restore_swap_refusal",
		"finish_interrupted_journal_recovery",
		"attempt_journal_corruption_recovery",
	} {
		harness.WriteString(extractShellFunction(t, script, fn))
		harness.WriteString("\n")
	}
	harness.WriteString(entry + "\n")
	return harness.String()
}

// fakeBlockShell defines block_at for the fakes: when FAKE_BLOCK_AT names the
// step a fake has reached, it reports `started` on fd FAKE_STARTED_FD and
// then blocks reading fd FAKE_BLOCK_FD, each when set, so a test can stop or
// kill the recovery right there.
const fakeBlockShell = `block_at() {
  [ "${FAKE_BLOCK_AT:-}" = "$1" ] || return 0
  if [ -n "${FAKE_STARTED_FD:-}" ]; then
    printf 'started\n' >&"$FAKE_STARTED_FD"
  fi
  if [ -n "${FAKE_BLOCK_FD:-}" ]; then
    IFS= read -r release <&"$FAKE_BLOCK_FD" || true
  fi
}
`

// writeJournalFakeDolt stubs `dolt status` (corrupt when the database dir
// contains a .corrupt marker), `dolt backup restore` and `dolt backup add`.
// The restore records its URL argument in FAKE_RESTORE_CALLS and creates the
// database in the current directory, as dolt does. With FAKE_RESTORE_EXIT it
// leaves a partial-marker there and fails. Otherwise it writes a
// restored-marker, blocks there when FAKE_BLOCK_AT is `restore`, and copies
// FAKE_RESTORE_REPO_STATE in as the restored repo_state.json when set. The add
// blocks first when FAKE_BLOCK_AT is `add`, then records
// `<database dir name> <name> <url>` in FAKE_BACKUP_ADD_CALLS and honors
// FAKE_BACKUP_ADD_EXIT.
func writeJournalFakeDolt(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -u
`+fakeBlockShell+`case "${1:-}" in
  status)
    if [ -f "$PWD/.corrupt" ]; then
      echo "failed to load database: possible data loss detected in journal file at offset 961963: corrupted journal" >&2
      exit 1
    fi
    exit 0
    ;;
  backup)
    case "${2:-}" in
      restore)
        printf '%s\n' "${3:-}" >> "${FAKE_RESTORE_CALLS:-/dev/null}"
        db="$4"
        mkdir -p "$PWD/$db/.dolt"
        if [ "${FAKE_RESTORE_EXIT:-0}" != "0" ]; then
          echo partial > "$PWD/$db/partial-marker"
          echo "restore failed" >&2
          exit "$FAKE_RESTORE_EXIT"
        fi
        echo restored > "$PWD/$db/restored-marker"
        block_at restore
        if [ -n "${FAKE_RESTORE_REPO_STATE:-}" ]; then
          cp "$FAKE_RESTORE_REPO_STATE" "$PWD/$db/.dolt/repo_state.json"
        fi
        exit 0
        ;;
      add)
        block_at add
        printf '%s %s %s\n' "$(basename "$PWD")" "${3:-}" "${4:-}" >> "${FAKE_BACKUP_ADD_CALLS:-/dev/null}"
        if [ "${FAKE_BACKUP_ADD_EXIT:-0}" != "0" ]; then
          echo "backup add failed" >&2
          exit "$FAKE_BACKUP_ADD_EXIT"
        fi
        exit 0
        ;;
    esac
    ;;
esac
exit 0
`)
}

// writeJournalFakeMoves puts mv and ln shims in front of the real commands.
// mv blocks first when FAKE_BLOCK_AT is `mv <source>`, and fails, as a
// refused rename does, when its source matches one of the colon-separated
// globs in FAKE_MV_FAIL_FROM. ln fails, as a hard link across filesystems
// does, when its source matches one of the globs in FAKE_LN_FAIL_FROM.
// Anything else runs the real command.
func writeJournalFakeMoves(t *testing.T, binDir string) {
	t.Helper()
	realMv, err := exec.LookPath("mv")
	if err != nil {
		t.Fatalf("find mv: %v", err)
	}
	realLn, err := exec.LookPath("ln")
	if err != nil {
		t.Fatalf("find ln: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "mv"), `#!/usr/bin/env bash
set -u
`+fakeBlockShell+`block_at "mv $1"
set -f
IFS=:
for glob in ${FAKE_MV_FAIL_FROM:-}; do
  case "$1" in
    $glob)
      printf "mv: cannot move '%s' to '%s': Permission denied\n" "$1" "$2" >&2
      exit 1
      ;;
  esac
done
exec `+shellQuotePOSIXPath(realMv)+` "$@"
`)
	writeExecutable(t, filepath.Join(binDir, "ln"), `#!/usr/bin/env bash
set -u
set -f
IFS=:
for glob in ${FAKE_LN_FAIL_FROM:-}; do
  case "$1" in
    $glob)
      printf "ln: failed to create hard link '%s' => '%s': Invalid cross-device link\n" "$2" "$1" >&2
      exit 1
      ;;
  esac
done
exec `+shellQuotePOSIXPath(realLn)+` "$@"
`)
}

// writeJournalFakeBd stubs `bd` so tests can assert recovery never invokes
// it: auto-recovery is gc-owned (#3176) and must restore through dolt alone.
func writeJournalFakeBd(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -u
printf 'bd %s\n' "$*" >> "${FAKE_BD_LOG:-/dev/null}"
exit 0
`)
}

type journalRecoveryEnv struct {
	cityDir   string
	dataDir   string
	stateDir  string
	logFile   string
	backupDir string
	binDir    string
	// backupAddLog collects the fake dolt's `backup add` calls.
	backupAddLog string
	// shell runs the harness; empty means bash.
	shell string
}

func setupJournalRecoveryEnv(t *testing.T) journalRecoveryEnv {
	t.Helper()
	cityDir := t.TempDir()
	env := journalRecoveryEnv{
		cityDir:      cityDir,
		dataDir:      filepath.Join(cityDir, "dolt-data"),
		stateDir:     filepath.Join(cityDir, "pack-state"),
		logFile:      filepath.Join(cityDir, "dolt.log"),
		backupDir:    filepath.Join(cityDir, "backups", "prod"),
		binDir:       filepath.Join(cityDir, "bin"),
		backupAddLog: filepath.Join(cityDir, "backup-add-calls.log"),
	}
	for _, dir := range []string{env.dataDir, env.stateDir, env.backupDir, env.binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeJournalFakeDolt(t, env.binDir)
	writeJournalFakeBd(t, env.binDir)
	writeJournalFakeMoves(t, env.binDir)
	return env
}

func (e journalRecoveryEnv) addDatabase(t *testing.T, name string, corrupt bool, repoState string) {
	t.Helper()
	dbDir := filepath.Join(e.dataDir, name)
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dbDir, err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "original-marker"), []byte(name), 0o644); err != nil {
		t.Fatalf("write original marker: %v", err)
	}
	if corrupt {
		if err := os.WriteFile(filepath.Join(dbDir, ".corrupt"), []byte("1"), 0o644); err != nil {
			t.Fatalf("write corrupt marker: %v", err)
		}
	}
	if repoState != "" {
		if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "repo_state.json"), []byte(repoState), 0o644); err != nil {
			t.Fatalf("write repo_state.json: %v", err)
		}
	}
}

// command runs harness in the env. The restore time limit is unset unless
// extraEnv sets it, so recovery uses its default whatever the host's
// environment holds; a later extraEnv entry overrides an earlier one.
func (e journalRecoveryEnv) command(harness string, extraEnv ...string) *exec.Cmd {
	shell := e.shell
	if shell == "" {
		shell = "bash"
	}
	cmd := exec.Command(shell, "-c", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DATA_DIR="+e.dataDir,
		"PACK_STATE_DIR="+e.stateDir,
		"LOG_FILE="+e.logFile,
		"FAKE_BACKUP_ADD_CALLS="+e.backupAddLog,
		"GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd
}

func (e journalRecoveryEnv) run(t *testing.T, harness string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := e.command(harness, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readOptional returns the contents of a file a fake writes only when it is
// called, and "" when it was never created.
func readOptional(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// assertRestoreRecorded asserts what a successful restore of prod from the
// named backup leaves behind for operators: the RESTORED notice on stderr;
// the same notice and the preserved-store line in the dolt log, the durable
// record, since the provider's caller discards stderr when an op succeeds;
// exactly one re-registration of that backup on the restored database,
// which `dolt backup restore` leaves with no backup registered; and nothing
// left in the recovery's own directories.
func (e journalRecoveryEnv) assertRestoreRecorded(t *testing.T, out, backupName, url string) {
	t.Helper()
	notice := "RESTORED 'prod' from backup '" + backupName + "' at " + url
	if !strings.Contains(out, notice) {
		t.Fatalf("restore notice must name the selected backup and its URL (%q):\n%s", notice, out)
	}
	doltLog := readOptional(t, e.logFile)
	for _, want := range []string{
		"gc-beads-bd: " + notice,
		"gc-beads-bd: preserved corrupt store at " + filepath.Join(e.stateDir, "corrupt-aside", "prod."),
		"gc-beads-bd: re-registered backup '" + backupName + "' at " + url + " on the restored 'prod'",
	} {
		if !strings.Contains(doltLog, want) {
			t.Errorf("dolt log missing %q, so the restore leaves no durable record:\n%s", want, doltLog)
		}
	}
	if got, want := strings.TrimSpace(readOptional(t, e.backupAddLog)), "prod "+backupName+" "+url; got != want {
		t.Errorf("backup add calls = %q, want exactly %q", got, want)
	}
	e.assertNoRecoveryLeftovers(t)
}

// assertNoRecoveryLeftovers asserts that recovery left nothing in
// restore-staging/, which only ever holds a partial copy or one already
// moved on, nor a restored copy waiting in restore-ready/, nor its
// filesystem probe in the data dir.
func (e journalRecoveryEnv) assertNoRecoveryLeftovers(t *testing.T) {
	t.Helper()
	for _, dir := range []string{"restore-staging", "restore-ready"} {
		entries, err := os.ReadDir(filepath.Join(e.stateDir, dir))
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			t.Errorf("recovery left %v in %s", names, dir)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, ".gc-restore-fs-probe")); !os.IsNotExist(err) {
		t.Errorf("recovery left its filesystem probe in the data dir (stat err: %v)", err)
	}
}

func TestRunWithTimeoutCapturedOutputDoesNotWaitForWatchdogSleep(t *testing.T) {
	forEachRunWithTimeoutShell(t, func(t *testing.T, shell string) {
		runWithTimeout := runWithTimeoutFunction(t)
		signalR, signalW := newRunWithTimeoutPipe(t, "sleep-signal")
		releaseR, releaseW := newRunWithTimeoutPipe(t, "sleep-release")
		commandGateR, commandGateW := newRunWithTimeoutPipe(t, "command-gate")

		binDir := t.TempDir()
		writeExecutable(t, filepath.Join(binDir, "sleep"), `#!/bin/sh
set -u
[ "$#" -eq 1 ] && [ "$1" = "120" ] || exit 64
: "${SLEEP_SIGNAL_FD:?}"
: "${SLEEP_RELEASE_FD:?}"
: "${COMMAND_GATE_WRITE_FD:?}"
printf 'started\n' >&"$SLEEP_SIGNAL_FD"
printf 'go\n' >&"$COMMAND_GATE_WRITE_FD"
IFS= read -r release <&"$SLEEP_RELEASE_FD"
[ "$release" = "release" ] || exit 65
printf 'finished\n' >&"$SLEEP_SIGNAL_FD"
`)

		harness := "set -eu\n" + runWithTimeout + `
result=$(run_with_timeout 120 sh -c 'IFS= read -r gate <&"$COMMAND_GATE_READ_FD"; [ "$gate" = go ]; printf command-output')
printf 'result=%s\n' "$result"
`
		env := journalRecoveryEnv{binDir: binDir, shell: shell}
		cmd := env.command(harness,
			"SLEEP_SIGNAL_FD=3",
			"SLEEP_RELEASE_FD=4",
			"COMMAND_GATE_WRITE_FD=5",
			"COMMAND_GATE_READ_FD=6",
		)
		cmd.ExtraFiles = []*os.File{signalW, releaseR, commandGateW, commandGateR}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start run_with_timeout harness: %v", err)
		}
		_ = signalW.Close()
		_ = releaseR.Close()
		_ = commandGateR.Close()

		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		controlsReleased := false
		releaseControls := func() {
			if controlsReleased {
				return
			}
			controlsReleased = true
			_, _ = fmt.Fprintln(commandGateW, "go")
			_ = commandGateW.Close()
			_, _ = fmt.Fprintln(releaseW, "release")
			_ = releaseW.Close()
		}
		signalReader := bufio.NewReader(signalR)
		if err := signalR.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
			t.Fatalf("set sleep-signal deadline: %v", err)
		}
		var runErr error
		doneConsumed := false
		waitCommand := func() bool {
			if doneConsumed {
				return true
			}
			timer := time.NewTimer(testutil.ExecRaceTimeout)
			defer timer.Stop()
			select {
			case runErr = <-done:
				doneConsumed = true
				return true
			case <-timer.C:
				return false
			}
		}
		defer func() {
			releaseControls()
			if waitCommand() {
				return
			}
			_ = cmd.Process.Kill()
			runErr = <-done
			doneConsumed = true
		}()

		var started string
		if _, err := fmt.Fscan(signalReader, &started); err != nil || started != "started" {
			t.Fatalf("read watchdog sleep started signal = %q (%v), want started", started, err)
		}

		completedBeforeRelease := waitCommand()
		releaseControls()
		if !completedBeforeRelease && !waitCommand() {
			t.Fatal("run_with_timeout harness did not finish after releasing the watchdog sleep")
		}
		if runErr != nil {
			t.Errorf("run_with_timeout harness failed: %v\nstderr: %s", runErr, stderr.String())
		}
		if got, want := stdout.String(), "result=command-output\n"; got != want {
			t.Errorf("harness output = %q, want %q", got, want)
		}
		if got := stderr.String(); got != "" {
			t.Errorf("harness stderr = %q, want empty", got)
		}
		if !completedBeforeRelease {
			t.Fatal("run_with_timeout command substitution stayed blocked after the timed command exited because the watchdog sleep retained the substitution output pipe")
		}
		rest, err := io.ReadAll(signalReader)
		if err != nil {
			t.Fatalf("sleep-signal pipe still held open after the harness exited: %v", err)
		}
		if got := strings.TrimSpace(string(rest)); got != "" {
			t.Fatalf("watchdog sleep outlived run_with_timeout: sleep-signal pipe read %q after the harness exited, want EOF", got)
		}
	})
}

// runWithTimeoutShells are the shells gc-beads-bd.sh must work under: bash,
// and the system sh (dash on Debian, bash in POSIX mode on macOS).
var runWithTimeoutShells = []string{"bash", "sh"}

// forEachRunWithTimeoutShell runs fn as one subtest per shell in
// runWithTimeoutShells, skipping shells that are not installed.
func forEachRunWithTimeoutShell(t *testing.T, fn func(t *testing.T, shell string)) {
	t.Helper()
	for _, shell := range runWithTimeoutShells {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("%s not available; skipping shell-function test", shell)
			}
			fn(t, shell)
		})
	}
}

// runWithTimeoutFunction returns run_with_timeout as written in gc-beads-bd.sh.
func runWithTimeoutFunction(t *testing.T) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return extractShellFunction(t, string(scriptBytes), "run_with_timeout")
}

// newRunWithTimeoutPipe returns a pipe whose ends are closed at test cleanup.
func newRunWithTimeoutPipe(t *testing.T, name string) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create %s pipe: %v", name, err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

func TestRunWithTimeoutStopsWatchdogSleepWhenCommandFinishes(t *testing.T) {
	forEachRunWithTimeoutShell(t, func(t *testing.T, shell string) {
		runWithTimeout := runWithTimeoutFunction(t)
		signalR, signalW := newRunWithTimeoutPipe(t, "sleep-signal")
		releaseR, releaseW := newRunWithTimeoutPipe(t, "sleep-release")
		commandGateR, commandGateW := newRunWithTimeoutPipe(t, "command-gate")

		binDir := t.TempDir()
		writeExecutable(t, filepath.Join(binDir, "sleep"), `#!/bin/sh
set -u
[ "$#" -eq 1 ] && [ "$1" = "120" ] || exit 64
printf 'started %s\n' "$$" >&"$SLEEP_SIGNAL_FD"
printf 'go\n' >&"$COMMAND_GATE_WRITE_FD"
IFS= read -r release <&"$SLEEP_RELEASE_FD"
[ "$release" = "release" ] || exit 65
printf 'finished\n' >&"$SLEEP_SIGNAL_FD"
`)

		harness := "set -eu\n" + runWithTimeout + `
status=0
run_with_timeout 120 sh -c 'IFS= read -r gate <&"$COMMAND_GATE_READ_FD"; [ "$gate" = go ]' || status=$?
printf 'rc=%s\n' "$status"
`
		env := journalRecoveryEnv{binDir: binDir, shell: shell}
		cmd := env.command(harness,
			"SLEEP_SIGNAL_FD=3",
			"SLEEP_RELEASE_FD=4",
			"COMMAND_GATE_WRITE_FD=5",
			"COMMAND_GATE_READ_FD=6",
		)
		cmd.ExtraFiles = []*os.File{signalW, releaseR, commandGateW, commandGateR}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start run_with_timeout harness: %v", err)
		}
		_ = signalW.Close()
		_ = releaseR.Close()
		_ = commandGateR.Close()
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		// Opening the gate and releasing the fake sleep lets a stray command
		// or sleep finish, so a failing run leaves nothing behind.
		defer func() {
			_, _ = fmt.Fprintln(commandGateW, "go")
			_ = commandGateW.Close()
			_, _ = fmt.Fprintln(releaseW, "release")
			_ = releaseW.Close()
		}()

		if err := signalR.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
			t.Fatalf("set sleep-signal deadline: %v", err)
		}
		signalReader := bufio.NewReader(signalR)
		var started string
		var sleepPID int
		if _, err := fmt.Fscan(signalReader, &started, &sleepPID); err != nil || started != "started" {
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("read watchdog sleep started signal = %q %d (%v), want started <pid>", started, sleepPID, err)
		}

		timer := time.NewTimer(testutil.ExecRaceTimeout)
		defer timer.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run_with_timeout harness failed: %v\nstderr: %s", err, stderr.String())
			}
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-done
			t.Fatal("run_with_timeout harness did not return after the timed command finished")
		}
		if got, want := stdout.String(), "rc=0\n"; got != want {
			t.Errorf("harness output = %q, want %q", got, want)
		}
		if pidAlive(sleepPID) {
			t.Fatalf("watchdog sleep (pid %d) still runs after run_with_timeout returned; it holds every caller descriptor above 2 until the timeout", sleepPID)
		}

		_, _ = fmt.Fprintln(releaseW, "release")
		_ = releaseW.Close()
		rest, err := io.ReadAll(signalReader)
		if err != nil {
			t.Fatalf("sleep-signal pipe still held open after the harness exited: %v", err)
		}
		if got := strings.TrimSpace(string(rest)); got != "" {
			t.Fatalf("sleep-signal pipe read %q after the harness exited, want EOF", got)
		}
	})
}

// expectPipeEOF fails unless r reports EOF within timeout, which proves that
// every process holding the pipe's write end has exited.
func expectPipeEOF(t *testing.T, r *os.File, timeout time.Duration, what string) {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set %s deadline: %v", what, err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("%s: write end still held open after %s (read %q): %v", what, timeout, data, err)
	}
	if len(data) != 0 {
		t.Fatalf("%s: read %q, want EOF", what, data)
	}
}

// runWithTimeoutInGroup runs harness under shell as the leader of a new
// process group, with extraFiles as fds 3 and up, and returns its stdout and
// wall time. It fails the test when the harness exits non-zero or outlives
// budget. stderr is not checked: dash reports a TERM-killed job there.
func runWithTimeoutInGroup(t *testing.T, shell, harness string, budget time.Duration, extraFiles ...*os.File) (string, time.Duration, int) {
	t.Helper()
	env := journalRecoveryEnv{binDir: t.TempDir(), shell: shell}
	cmd := env.command(harness)
	cmd.ExtraFiles = extraFiles
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start run_with_timeout harness: %v", err)
	}
	for _, f := range extraFiles {
		_ = f.Close()
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("run_with_timeout harness failed after %s: %v\nstderr: %s", elapsed, err, stderr.String())
		}
		return stdout.String(), elapsed, cmd.Process.Pid
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("run_with_timeout harness still running after %s", budget)
		return "", 0, 0
	}
}

// expectProcessGroupGone fails when any process of group pgid still runs:
// after the harness exits, a member can only be something a call left behind.
func expectProcessGroupGone(t *testing.T, pgid int) {
	t.Helper()
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group %d still has members after the harness exited (kill -0 = %v), want ESRCH", pgid, err)
	}
}

func TestRunWithTimeoutLeavesNothingAfterInstantCommands(t *testing.T) {
	forEachRunWithTimeoutShell(t, func(t *testing.T, shell string) {
		heldR, heldW := newRunWithTimeoutPipe(t, "held-descriptor")
		harness := "set -eu\n" + runWithTimeoutFunction(t) + `
i=0
while [ "$i" -lt 200 ]; do
	run_with_timeout 2 true
	i=$((i + 1))
done
printf 'calls=%s\n' "$i"
`
		out, _, pgid := runWithTimeoutInGroup(t, shell, harness, testutil.ExecRaceTimeout+3*2*time.Second, heldW)
		if want := "calls=200\n"; out != want {
			t.Errorf("harness output = %q, want %q", out, want)
		}
		expectProcessGroupGone(t, pgid)
		expectPipeEOF(t, heldR, time.Second, "caller fd 3")
	})
}

func TestRunWithTimeoutKillsCommandAfterTimeout(t *testing.T) {
	forEachRunWithTimeoutShell(t, func(t *testing.T, shell string) {
		harness := "set -eu\n" + runWithTimeoutFunction(t) + `
status=0
run_with_timeout 1 sleep 5 || status=$?
printf 'rc=%s\n' "$status"
`
		out, elapsed, pgid := runWithTimeoutInGroup(t, shell, harness, testutil.ExecRaceTimeout)
		if want := "rc=143\n"; out != want {
			t.Errorf("harness output = %q, want %q", out, want)
		}
		if elapsed < time.Second || elapsed >= 4*time.Second {
			t.Errorf("run_with_timeout 1 sleep 5 took %s, want at least 1s and well under 5s", elapsed)
		}
		expectProcessGroupGone(t, pgid)
	})
}

func TestRunWithTimeoutRejectedDurationFallsBackToOneSecond(t *testing.T) {
	forEachRunWithTimeoutShell(t, func(t *testing.T, shell string) {
		harness := "set -eu\n" + runWithTimeoutFunction(t) + `
status=0
run_with_timeout abc sleep 5 || status=$?
printf 'rc=%s\n' "$status"
`
		out, elapsed, pgid := runWithTimeoutInGroup(t, shell, harness, testutil.ExecRaceTimeout)
		if want := "rc=143\n"; out != want {
			t.Errorf("harness output = %q, want %q", out, want)
		}
		if elapsed < time.Second || elapsed >= 4*time.Second {
			t.Errorf("run_with_timeout abc sleep 5 took %s, want at least the 1s fallback and well under 5s", elapsed)
		}
		expectProcessGroupGone(t, pgid)
	})
}

func prodRepoState(backupDir string) string {
	return `{"head":"refs/heads/main","backups":{"prod-backup":{"name":"prod-backup","url":"file://` + backupDir + `","fetch_specs":[],"params":{}}}}`
}

// bdDefaultRepoState returns the repo_state.json shape bd records for
// `bd backup init`: the destination is registered under the name `default`
// (bd v1.3.1, cmd/bd/backup_dolt.go), which is the only name mol-dog-backup
// has left behind since #6751 rewrote ensure_backup_destination.
func bdDefaultRepoState(backupDir string) string {
	return `{"head":"refs/heads/main","backups":{"default":{"name":"default","url":"file://` + backupDir + `","fetch_specs":[],"params":{}}}}`
}

// bothBackupsRepoState registers both backup names, the legacy entry first
// in the JSON text so a reader that scans for the last occurrence of a name
// cannot pick the default entry by accident of position.
func bothBackupsRepoState(defaultDir, legacyDir string) string {
	return `{"head":"refs/heads/main","backups":{"prod-backup":{"name":"prod-backup","url":"file://` + legacyDir + `","fetch_specs":[],"params":{}},"default":{"name":"default","url":"file://` + defaultDir + `","fetch_specs":[],"params":{}}}}`
}

// TestJournalRecoveryPrefersBdDefaultOverLegacyBackup pins the precedence
// when both backup names are registered: recovery restores exactly once,
// from the `default` entry (the one mol-dog-backup still syncs), its log
// line names that backup and its URL so operators can see which source a
// restored database came from, and that backup is the one registered again.
func TestJournalRecoveryPrefersBdDefaultOverLegacyBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	for _, tc := range []struct {
		name   string
		maskJQ bool
	}{
		{name: "awk fallback", maskJQ: true},
		{name: "jq", maskJQ: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.maskJQ {
				if _, err := exec.LookPath("jq"); err != nil {
					t.Skip("jq not available; skipping jq-path test")
				}
			}
			env := setupJournalRecoveryEnv(t)
			defaultDir := env.backupDir
			legacyDir := filepath.Join(env.cityDir, "backups", "legacy")
			if err := os.MkdirAll(legacyDir, 0o755); err != nil {
				t.Fatalf("mkdir legacy backup dir: %v", err)
			}
			for _, dir := range []string{defaultDir, legacyDir} {
				if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("backup"), 0o644); err != nil {
					t.Fatalf("seed backup content in %s: %v", dir, err)
				}
			}
			restoreCalls := filepath.Join(env.cityDir, "restore-calls.log")
			env.addDatabase(t, "prod", true, bothBackupsRepoState(defaultDir, legacyDir))

			out, err := env.run(t, journalRecoveryHarness(t, tc.maskJQ), "FAKE_RESTORE_CALLS="+restoreCalls)
			if err != nil {
				t.Fatalf("recovery failed: %v\n%s", err, out)
			}
			wantURL := "file://" + defaultDir
			env.assertRestoreRecorded(t, out, "default", wantURL)
			calls, err := os.ReadFile(restoreCalls)
			if err != nil {
				t.Fatalf("read restore calls: %v", err)
			}
			lines := strings.Fields(strings.TrimSpace(string(calls)))
			if len(lines) != 1 || lines[0] != wantURL {
				t.Fatalf("restore calls = %v, want exactly one restore from %s", lines, wantURL)
			}
		})
	}
}

// TestJournalRecoveryRestoresFromBdDefaultBackup pins that auto-recovery
// restores a journal-corrupt database from bd's canonical `default` backup
// registration — the shape every gc-managed city has after #6751 — records
// the restore in the dolt log, registers `default` on the restored copy
// again, and never invokes bd while doing so. Both the jq and the awk
// (no-jq) reader paths are exercised.
func TestJournalRecoveryRestoresFromBdDefaultBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	for _, tc := range []struct {
		name   string
		maskJQ bool
	}{
		{name: "awk fallback", maskJQ: true},
		{name: "jq", maskJQ: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.maskJQ {
				if _, err := exec.LookPath("jq"); err != nil {
					t.Skip("jq not available; skipping jq-path test")
				}
			}
			env := setupJournalRecoveryEnv(t)
			if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
				t.Fatalf("seed backup content: %v", err)
			}
			bdLog := filepath.Join(env.cityDir, "bd-calls.log")
			env.addDatabase(t, "prod", true, bdDefaultRepoState(env.backupDir))

			out, err := env.run(t, journalRecoveryHarness(t, tc.maskJQ), "FAKE_BD_LOG="+bdLog)
			if err != nil {
				t.Fatalf("recovery failed: %v\n%s", err, out)
			}
			env.assertRestoreRecorded(t, out, "default", "file://"+env.backupDir)
			if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "restored-marker")); err != nil {
				t.Fatalf("prod was not restored from the default backup: %v\n%s", err, out)
			}
			asides, err := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "prod.*"))
			if err != nil || len(asides) != 1 {
				t.Fatalf("corrupt store must be preserved aside, got %v (err %v)\n%s", asides, err, out)
			}
			if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
				t.Fatalf("aside copy missing original content: %v", err)
			}
			if bdCalls, readErr := os.ReadFile(bdLog); readErr == nil && len(bdCalls) > 0 {
				t.Fatalf("recovery must not invoke bd, bd calls:\n%s", bdCalls)
			} else if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read bd log: %v", readErr)
			}
		})
	}
}

// TestJournalRecoveryRestoresFromLegacyStringBackup pins the other legacy
// shape: pre-#6751 writers registered <db>-backup as a plain string URL, and
// a corrupt database whose only registered backup is that entry must still
// restore from it, with the notice naming the entry and its URL. The object
// form of the same entry is pinned by
// TestJournalRecoveryRestoresCorruptDBFromLocalBackup.
func TestJournalRecoveryRestoresFromLegacyStringBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	for _, tc := range []struct {
		name   string
		maskJQ bool
	}{
		{name: "awk fallback", maskJQ: true},
		{name: "jq", maskJQ: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.maskJQ {
				if _, err := exec.LookPath("jq"); err != nil {
					t.Skip("jq not available; skipping jq-path test")
				}
			}
			env := setupJournalRecoveryEnv(t)
			if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
				t.Fatalf("seed backup content: %v", err)
			}
			restoreCalls := filepath.Join(env.cityDir, "restore-calls.log")
			repoState := `{"head":"refs/heads/main","backups":{"prod-backup":"file://` + env.backupDir + `"}}`
			env.addDatabase(t, "prod", true, repoState)

			out, err := env.run(t, journalRecoveryHarness(t, tc.maskJQ), "FAKE_RESTORE_CALLS="+restoreCalls)
			if err != nil {
				t.Fatalf("recovery failed: %v\n%s", err, out)
			}
			wantURL := "file://" + env.backupDir
			env.assertRestoreRecorded(t, out, "prod-backup", wantURL)
			if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "restored-marker")); err != nil {
				t.Fatalf("prod was not restored from the legacy backup: %v\n%s", err, out)
			}
			calls, err := os.ReadFile(restoreCalls)
			if err != nil {
				t.Fatalf("read restore calls: %v", err)
			}
			if lines := strings.Fields(strings.TrimSpace(string(calls))); len(lines) != 1 || lines[0] != wantURL {
				t.Fatalf("restore calls = %v, want exactly one restore from %s", lines, wantURL)
			}
		})
	}
}

func TestJournalRecoveryRestoresCorruptDBFromLocalBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, prodRepoState(env.backupDir))
	env.addDatabase(t, "healthy", false, "")

	out, err := env.run(t, journalRecoveryHarness(t, false))
	if err != nil {
		t.Fatalf("recovery failed: %v\n%s", err, out)
	}
	env.assertRestoreRecorded(t, out, "prod-backup", "file://"+env.backupDir)
	if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "restored-marker")); err != nil {
		t.Fatalf("prod was not restored from backup: %v\n%s", err, out)
	}
	asides, err := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "prod.*"))
	if err != nil || len(asides) != 1 {
		t.Fatalf("corrupt store must be preserved aside, got %v (err %v)\n%s", asides, err, out)
	}
	if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
		t.Fatalf("aside copy missing original content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "healthy", "original-marker")); err != nil {
		t.Fatalf("healthy database must not be touched: %v", err)
	}
	if dirs, _ := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "healthy.*")); len(dirs) != 0 {
		t.Fatalf("healthy database must not be moved aside: %v", dirs)
	}
}

// TestJournalRecoveryFailsClosedWithoutUsableBackup pins the refusal path on
// both reader paths: the store stays in place, no restore or backup add runs,
// and the refusal says why, lists every registered backup, and prints a
// manual command that names the selected URL only when that URL could work
// (a remote backup an operator may choose to trust). The refusal is also in
// the dolt log, like every other recovery step.
func TestJournalRecoveryFailsClosedWithoutUsableBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	// legacyDir holds a usable legacy backup (seeded by seedLegacy);
	// env.backupDir stays empty unless a case seeds it and serves as the
	// default backup's target.
	legacyDirFor := func(env journalRecoveryEnv) string {
		return filepath.Join(env.cityDir, "backups", "legacy")
	}
	seedBackup := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir backup dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("backup"), 0o644); err != nil {
			t.Fatalf("seed backup content in %s: %v", dir, err)
		}
	}
	seedLegacy := func(t *testing.T, env journalRecoveryEnv) { seedBackup(t, legacyDirFor(env)) }
	tests := []struct {
		name      string
		repoState func(env journalRecoveryEnv) string
		setup     func(t *testing.T, env journalRecoveryEnv)
		// reason is a substring of the refusal's first line.
		reason string
		// listing is the exact registered-backups listing.
		listing func(env journalRecoveryEnv) string
		// manualURL is the URL the manual restore command names.
		manualURL string
	}{
		{
			name:      "no backup remote recorded",
			repoState: func(journalRecoveryEnv) string { return `{"head":"refs/heads/main"}` },
			reason:    "neither backup default nor prod-backup is registered",
			listing:   func(journalRecoveryEnv) string { return "none registered" },
			manualURL: "<url>",
		},
		{
			name:      "backup remote dir empty",
			repoState: func(env journalRecoveryEnv) string { return prodRepoState(env.backupDir) },
			reason:    "selected backup 'prod-backup' (url: file://",
			listing: func(env journalRecoveryEnv) string {
				return "prod-backup=file://" + env.backupDir
			},
			manualURL: "<url>",
		},
		{
			name: "non-file backup remote",
			repoState: func(journalRecoveryEnv) string {
				return `{"backups":{"prod-backup":{"url":"aws://bucket/prod"}}}`
			},
			reason:    "it is not a local file:// backup",
			listing:   func(journalRecoveryEnv) string { return "prod-backup=aws://bucket/prod" },
			manualURL: "aws://bucket/prod",
		},
		{
			name: "default backup is a non-file remote",
			repoState: func(journalRecoveryEnv) string {
				return `{"backups":{"default":{"name":"default","url":"aws://bucket/prod","fetch_specs":[],"params":{}}}}`
			},
			reason:    "selected backup 'default' (url: aws://bucket/prod) cannot be restored automatically: it is not a local file:// backup",
			listing:   func(journalRecoveryEnv) string { return "default=aws://bucket/prod" },
			manualURL: "aws://bucket/prod",
		},
		{
			name:      "default backup dir empty",
			repoState: func(env journalRecoveryEnv) string { return bdDefaultRepoState(env.backupDir) },
			reason:    "is empty",
			listing:   func(env journalRecoveryEnv) string { return "default=file://" + env.backupDir },
			manualURL: "<url>",
		},
		{
			name: "default backup dir missing",
			repoState: func(env journalRecoveryEnv) string {
				return bdDefaultRepoState(filepath.Join(env.cityDir, "backups", "gone"))
			},
			reason: "does not exist",
			listing: func(env journalRecoveryEnv) string {
				return "default=file://" + filepath.Join(env.cityDir, "backups", "gone")
			},
			manualURL: "<url>",
		},
		{
			name: "default entry without a url",
			repoState: func(journalRecoveryEnv) string {
				return `{"backups":{"default":{"name":"default","fetch_specs":[],"params":{}}}}`
			},
			reason:    "selected backup 'default' (url: none) cannot be restored automatically: it has no URL",
			listing:   func(journalRecoveryEnv) string { return "default=" },
			manualURL: "<url>",
		},
		{
			// The default entry is selected first and is unusable; recovery
			// must refuse rather than fall through to the usable legacy
			// entry — the legacy backup stopped syncing when it was renamed
			// and would restore older data without anyone choosing it.
			name: "default unusable even with usable legacy entry",
			repoState: func(env journalRecoveryEnv) string {
				return bothBackupsRepoState(env.backupDir, legacyDirFor(env))
			},
			setup:  seedLegacy,
			reason: "selected backup 'default'",
			listing: func(env journalRecoveryEnv) string {
				return "prod-backup=file://" + legacyDirFor(env) + ", default=file://" + env.backupDir
			},
			manualURL: "<url>",
		},
		{
			// A backup directory two databases sync to holds whichever
			// synced last, so restoring it could bring back the other
			// database's data under this one's name.
			name:      "default backup dir shared with another database",
			repoState: func(env journalRecoveryEnv) string { return bdDefaultRepoState(env.backupDir) },
			setup: func(t *testing.T, env journalRecoveryEnv) {
				seedBackup(t, env.backupDir)
				env.addDatabase(t, "other", false, bdDefaultRepoState(env.backupDir))
			},
			reason:    "database other registers the same directory",
			listing:   func(env journalRecoveryEnv) string { return "default=file://" + env.backupDir },
			manualURL: "<url>",
		},
	}
	for _, parser := range []struct {
		name   string
		maskJQ bool
	}{
		{name: "awk fallback", maskJQ: true},
		{name: "jq", maskJQ: false},
	} {
		t.Run(parser.name, func(t *testing.T) {
			if !parser.maskJQ {
				if _, err := exec.LookPath("jq"); err != nil {
					t.Skip("jq not available; skipping jq-path test")
				}
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					env := setupJournalRecoveryEnv(t)
					restoreCalls := filepath.Join(env.cityDir, "restore-calls.log")
					if tc.setup != nil {
						tc.setup(t, env)
					}
					env.addDatabase(t, "prod", true, tc.repoState(env))

					out, err := env.run(t, journalRecoveryHarness(t, parser.maskJQ), "FAKE_RESTORE_CALLS="+restoreCalls)
					if err == nil {
						t.Fatalf("recovery must fail closed without a usable backup\n%s", out)
					}
					if !strings.Contains(out, "NOT auto-recovering 'prod': ") || !strings.Contains(out, tc.reason) {
						t.Fatalf("refusal must say why (%q):\n%s", tc.reason, out)
					}
					wantListing := "candidate backups checked: default, prod-backup; registered backups: " + tc.listing(env) + "\n"
					if !strings.Contains(out, wantListing) {
						t.Fatalf("refusal must list every registered backup (%q):\n%s", wantListing, out)
					}
					wantManual := "\"dolt backup restore '" + tc.manualURL + "' prod\""
					if !strings.Contains(out, wantManual) {
						t.Fatalf("refusal must print the manual restore command %s:\n%s", wantManual, out)
					}
					if doltLog := readOptional(t, env.logFile); !strings.Contains(doltLog, "gc-beads-bd: NOT auto-recovering 'prod': ") {
						t.Fatalf("dolt log missing the refusal:\n%s", doltLog)
					}
					if _, statErr := os.Stat(filepath.Join(env.dataDir, "prod", "original-marker")); statErr != nil {
						t.Fatalf("prod store must stay in place when not recoverable: %v", statErr)
					}
					if calls := strings.TrimSpace(readOptional(t, restoreCalls)); calls != "" {
						t.Fatalf("no restore may run when the selected backup is unusable, restore calls:\n%s", calls)
					}
					if calls := strings.TrimSpace(readOptional(t, env.backupAddLog)); calls != "" {
						t.Fatalf("no backup may be registered when nothing was restored, backup add calls:\n%s", calls)
					}
				})
			}
		})
	}
}

// TestJournalRecoveryKeepsRestoreWhenBackupReRegistrationFails pins that a
// restore stands when registering its backup on the restored copy fails:
// recovery still succeeds, and the failure is reported on stderr and in the
// dolt log together with the command that registers the backup by hand.
func TestJournalRecoveryKeepsRestoreWhenBackupReRegistrationFails(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, bdDefaultRepoState(env.backupDir))

	out, err := env.run(t, journalRecoveryHarness(t, false), "FAKE_BACKUP_ADD_EXIT=1")
	if err != nil {
		t.Fatalf("a failed re-registration must not undo the restore: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "restored-marker")); err != nil {
		t.Fatalf("prod was not restored: %v\n%s", err, out)
	}
	want := "WARNING: restored 'prod' has no backup registered and is not backed up until it has one: with the server down, run \"dolt backup add default 'file://" + env.backupDir + "'\" in " + filepath.Join(env.dataDir, "prod")
	if !strings.Contains(out, want) {
		t.Fatalf("output missing the re-registration warning %q:\n%s", want, out)
	}
	if doltLog := readOptional(t, env.logFile); !strings.Contains(doltLog, want) {
		t.Fatalf("dolt log missing the re-registration warning %q:\n%s", want, doltLog)
	}
}

// TestJournalRecoverySkipsReRegistrationWhenRestoreKeptTheBackup pins that a
// restored copy which still registers its backup is left alone: adding the
// name again would fail and report a missing backup that is not missing.
func TestJournalRecoverySkipsReRegistrationWhenRestoreKeptTheBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, bdDefaultRepoState(env.backupDir))
	keptState := filepath.Join(env.cityDir, "kept-repo-state.json")
	if err := os.WriteFile(keptState, []byte(bdDefaultRepoState(env.backupDir)), 0o644); err != nil {
		t.Fatalf("write kept repo_state: %v", err)
	}

	out, err := env.run(t, journalRecoveryHarness(t, false), "FAKE_RESTORE_REPO_STATE="+keptState)
	if err != nil {
		t.Fatalf("recovery failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "restored 'prod' still registers backup 'default'") {
		t.Fatalf("output must say the restored copy kept its backup:\n%s", out)
	}
	if calls := strings.TrimSpace(readOptional(t, env.backupAddLog)); calls != "" {
		t.Fatalf("backup add must not run when the restored copy kept its backup, calls:\n%s", calls)
	}
}

// TestJournalRecoveryLeavesStoreInPlaceWhenRestoreFails pins the failed
// restore: it wrote only to staging, so the corrupt store is still in the
// data dir, untouched, the partial copy is removed, and recovery fails so
// the server cannot start without the database.
func TestJournalRecoveryLeavesStoreInPlaceWhenRestoreFails(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, prodRepoState(env.backupDir))

	out, err := env.run(t, journalRecoveryHarness(t, false), "FAKE_RESTORE_EXIT=1")
	if err == nil {
		t.Fatalf("recovery must fail when restore fails\n%s", out)
	}
	want := "backup restore failed for 'prod' (see " + env.logFile + "); " + filepath.Join(env.dataDir, "prod") + " is untouched"
	if !strings.Contains(out, want) {
		t.Fatalf("output missing the restore failure notice %q:\n%s", want, out)
	}
	env.assertStoreUntouched(t, out)
	env.assertNoRecoveryLeftovers(t)
}

// assertStoreUntouched asserts that prod's corrupt store is still in the data
// dir and was never moved aside, and that no restored or partial copy took
// its place.
func (e journalRecoveryEnv) assertStoreUntouched(t *testing.T, out string) {
	t.Helper()
	prod := filepath.Join(e.dataDir, "prod")
	if _, err := os.Stat(filepath.Join(prod, "original-marker")); err != nil {
		t.Fatalf("prod's corrupt store must still be in the data dir: %v\n%s", err, out)
	}
	for _, marker := range []string{"restored-marker", "partial-marker"} {
		if _, err := os.Stat(filepath.Join(prod, marker)); !os.IsNotExist(err) {
			t.Fatalf("the data dir holds restore output (%s, stat err: %v)\n%s", marker, err, out)
		}
	}
	if asides, _ := filepath.Glob(filepath.Join(e.stateDir, "corrupt-aside", "prod.*")); len(asides) != 0 {
		t.Fatalf("the store must not be moved aside before its restore succeeds: %v\n%s", asides, out)
	}
}

// TestJournalRecoveryStopsARestoreThatOutlastsItsLimit pins the time limit on
// an automatic restore: a restore still running when
// GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS runs out is stopped and its staged
// copy removed, the corrupt store stays in place, nothing is registered, and
// recovery fails telling the operator to run `gc dolt start`, which lifts the
// limit.
func TestJournalRecoveryStopsARestoreThatOutlastsItsLimit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, bdDefaultRepoState(env.backupDir))
	blockR, blockW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create restore block pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = blockR.Close()
		_ = blockW.Close()
	})
	// A safety net, not the trigger: if the limit does not stop the blocked
	// restore, releasing it ends the run and the assertions below fail.
	release := time.AfterFunc(testutil.ExecRaceTimeout, func() { _ = blockW.Close() })
	defer release.Stop()

	cmd := env.command(journalRecoveryHarness(t, false),
		"GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS=1",
		"FAKE_BLOCK_AT=restore",
		"FAKE_BLOCK_FD=3",
	)
	cmd.ExtraFiles = []*os.File{blockR}
	outBytes, err := cmd.CombinedOutput()
	out := string(outBytes)
	if err == nil {
		t.Fatalf("recovery must fail when its restore runs out of time\n%s", out)
	}
	want := "backup restore of 'prod' did not finish within 1s and was stopped; " + filepath.Join(env.dataDir, "prod") +
		" is untouched. Run \"gc dolt start\", which lets the restore run to completion, then \"gc start\""
	if !strings.Contains(out, want) {
		t.Fatalf("output missing the time-limit notice %q:\n%s", want, out)
	}
	if doltLog := readOptional(t, env.logFile); !strings.Contains(doltLog, want) {
		t.Fatalf("dolt log missing the time-limit notice %q:\n%s", want, doltLog)
	}
	env.assertStoreUntouched(t, out)
	env.assertNoRecoveryLeftovers(t)
	if calls := strings.TrimSpace(readOptional(t, env.backupAddLog)); calls != "" {
		t.Fatalf("no backup may be registered when the restore was stopped, backup add calls:\n%s", calls)
	}
}

// killRecoveryAt runs harness in its own process group, as
// prepareProviderOpCommand runs the provider's start operation, waits until
// the fake at step (FAKE_BLOCK_AT) reports that it started, and SIGKILLs the
// whole group there, as gc does once the operation's 120s are up. It returns
// what the recovery printed before the kill.
func (e journalRecoveryEnv) killRecoveryAt(t *testing.T, harness, step string) string {
	t.Helper()
	startedR, startedW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create started pipe: %v", err)
	}
	blockR, blockW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create block pipe: %v", err)
	}
	t.Cleanup(func() {
		for _, f := range []*os.File{startedR, startedW, blockR, blockW} {
			_ = f.Close()
		}
	})

	cmd := e.command(harness, "FAKE_BLOCK_AT="+step, "FAKE_STARTED_FD=3", "FAKE_BLOCK_FD=4")
	cmd.ExtraFiles = []*os.File{startedW, blockR}
	// Its own process group, killed whole, as prepareProviderOpCommand
	// arranges for the provider's start operation.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start recovery: %v", err)
	}
	_ = startedW.Close()
	_ = blockR.Close()
	killGroup := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := startedR.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
		_ = killGroup()
		_ = cmd.Wait()
		t.Fatalf("set started deadline: %v", err)
	}
	if line, err := bufio.NewReader(startedR).ReadString('\n'); err != nil || line != "started\n" {
		_ = killGroup()
		_ = cmd.Wait()
		t.Fatalf("the recovery never reached %q (%q, %v):\n%s", step, line, err, out.String())
	}
	if err := killGroup(); err != nil {
		t.Fatalf("SIGKILL the recovery's process group: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("the killed recovery exited cleanly:\n%s", out.String())
	}
	return out.String()
}

// TestJournalRecoveryConvergesWhenKilledPartWay SIGKILLs a recovery at each
// step where gc's kill of the provider's start operation can land, then runs
// what the next start runs: finish_interrupted_journal_recovery before the
// server starts and, when the store is still in the data dir and corrupt so
// that the server fails again, the recovery. Wherever the kill landed, the
// next start leaves prod restored in the data dir and the corrupt store kept
// aside; a start killed between the two renames leaves prod missing, and the
// next start moves the finished copy in rather than discarding it.
func TestJournalRecoveryConvergesWhenKilledPartWay(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	staged := func(env journalRecoveryEnv) string { return filepath.Join(env.stateDir, "restore-staging", "prod") }
	ready := func(env journalRecoveryEnv) string { return filepath.Join(env.stateDir, "restore-ready", "prod") }
	removedStaging := func(env journalRecoveryEnv) string {
		return "removed " + filepath.Join(env.stateDir, "restore-staging") + ", left by an interrupted restore"
	}
	for _, tc := range []struct {
		name string
		// step is where the kill lands (FAKE_BLOCK_AT).
		step func(env journalRecoveryEnv) string
		// left is where the killed recovery left the restored copy;
		// storeMoved is whether it had moved the corrupt store aside, and
		// registered whether it had registered the backup on the copy.
		left                   func(env journalRecoveryEnv) string
		storeMoved, registered bool
		// settled is what the next start says it did with what was left.
		settled func(env journalRecoveryEnv) string
	}{
		{
			name:    "during the restore",
			step:    func(journalRecoveryEnv) string { return "restore" },
			left:    staged,
			settled: removedStaging,
		},
		{
			name:    "while the backup is registered on the restored copy",
			step:    func(journalRecoveryEnv) string { return "add" },
			left:    staged,
			settled: removedStaging,
		},
		{
			name:       "before the corrupt store is moved aside",
			step:       func(env journalRecoveryEnv) string { return "mv " + filepath.Join(env.dataDir, "prod") },
			left:       ready,
			registered: true,
			settled: func(env journalRecoveryEnv) string {
				return "removed " + ready(env) + ", left by an interrupted recovery: 'prod' is in " + env.dataDir
			},
		},
		{
			name:       "between moving the corrupt store aside and moving the copy in",
			step:       func(env journalRecoveryEnv) string { return "mv " + ready(env) },
			left:       ready,
			storeMoved: true,
			registered: true,
			settled: func(env journalRecoveryEnv) string {
				return "RESTORED 'prod' by moving in " + ready(env) + ", which an interrupted recovery restored from its backup"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
				t.Fatalf("seed backup content: %v", err)
			}
			env.addDatabase(t, "prod", true, bdDefaultRepoState(env.backupDir))

			out := env.killRecoveryAt(t, journalRecoveryHarness(t, false), tc.step(env))
			if _, err := os.Stat(filepath.Join(tc.left(env), "restored-marker")); err != nil {
				t.Fatalf("the killed recovery should have left its restored copy at %s: %v\n%s", tc.left(env), err, out)
			}
			if tc.storeMoved {
				if _, err := os.Stat(filepath.Join(env.dataDir, "prod")); !os.IsNotExist(err) {
					t.Fatalf("the kill should land with prod missing from the data dir (stat err: %v)\n%s", err, out)
				}
			} else {
				env.assertStoreUntouched(t, out)
			}
			wantAdds := ""
			if tc.registered {
				wantAdds = "prod default file://" + env.backupDir
			}
			if got := strings.TrimSpace(readOptional(t, env.backupAddLog)); got != wantAdds {
				t.Fatalf("backup add calls before the kill = %q, want %q", got, wantAdds)
			}
			if doltLog := readOptional(t, env.logFile); !strings.Contains(doltLog, "an automatic restore may take 45s, and \"gc dolt start\" lets it run to completion") {
				t.Fatalf("dolt log must say how to let the restore finish before it starts:\n%s", doltLog)
			}
			if err := os.Remove(env.backupAddLog); err != nil && !os.IsNotExist(err) {
				t.Fatalf("reset backup add calls: %v", err)
			}

			again, err := env.run(t, journalNextStartHarness(t, !tc.storeMoved))
			if err != nil {
				t.Fatalf("the next start failed: %v\n%s", err, again)
			}
			if want := tc.settled(env); !strings.Contains(again, want) {
				t.Fatalf("the next start must say what it did with the interrupted recovery (%q):\n%s", want, again)
			}
			if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "restored-marker")); err != nil {
				t.Fatalf("prod must be restored in the data dir after the next start: %v\n%s", err, again)
			}
			if tc.storeMoved {
				// The copy moves in as the killed recovery left it, with its
				// backup registered: nothing is restored or registered again.
				if calls := strings.TrimSpace(readOptional(t, env.backupAddLog)); calls != "" {
					t.Fatalf("the next start must not register the backup again, backup add calls:\n%s", calls)
				}
				if doltLog := readOptional(t, env.logFile); !strings.Contains(doltLog, "gc-beads-bd: "+tc.settled(env)) {
					t.Fatalf("dolt log missing %q:\n%s", tc.settled(env), doltLog)
				}
				env.assertNoRecoveryLeftovers(t)
			} else {
				env.assertRestoreRecorded(t, again, "default", "file://"+env.backupDir)
			}
			asides, err := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "prod.*"))
			if err != nil || len(asides) != 1 {
				t.Fatalf("the corrupt store must be kept aside exactly once, got %v (err %v)\n%s", asides, err, again)
			}
			if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
				t.Fatalf("the store kept aside lost its content: %v", err)
			}
		})
	}
}

// TestFinishInterruptedJournalRecoveryIsQuietWithNothingLeft pins the usual
// case of the step op_start runs before every server start: with no
// recovery left part way, including after a recovery that refused before it
// restored anything and left only empty directories, it succeeds and says
// and writes nothing.
func TestFinishInterruptedJournalRecoveryIsQuietWithNothingLeft(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	for _, tc := range []struct {
		name string
		dirs []string
	}{
		{name: "nothing at all"},
		{name: "empty directories of a refused recovery", dirs: []string{"restore-staging", "restore-ready", "corrupt-aside"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			env.addDatabase(t, "prod", false, "")
			for _, dir := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(env.stateDir, dir), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			out, err := env.run(t, journalNextStartHarness(t, false))
			if err != nil || out != "" {
				t.Fatalf("finish_interrupted_journal_recovery = (%q, %v), want quiet success", out, err)
			}
			if doltLog := readOptional(t, env.logFile); doltLog != "" {
				t.Fatalf("finish_interrupted_journal_recovery wrote to the dolt log:\n%s", doltLog)
			}
			if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "original-marker")); err != nil {
				t.Fatalf("prod must stay in place: %v", err)
			}
		})
	}
}

// TestJournalRecoveryPutsTheStoreBackWhenTheSwapFails pins the swap's
// rollback. When the restored copy cannot be moved into the data dir after
// the corrupt store was moved aside, the store is moved back and the copy
// removed. When the store cannot be moved back either, both stay where they
// are and the CRITICAL note names them; the next start then fails closed,
// before the server starts, while the copy cannot be moved in, and moves it
// in once it can.
func TestJournalRecoveryPutsTheStoreBackWhenTheSwapFails(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	setup := func(t *testing.T) journalRecoveryEnv {
		t.Helper()
		env := setupJournalRecoveryEnv(t)
		if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
			t.Fatalf("seed backup content: %v", err)
		}
		env.addDatabase(t, "prod", true, bdDefaultRepoState(env.backupDir))
		return env
	}

	t.Run("the corrupt store is moved back", func(t *testing.T) {
		env := setup(t)
		out, err := env.run(t, journalRecoveryHarness(t, false),
			"FAKE_MV_FAIL_FROM="+filepath.Join(env.stateDir, "restore-ready", "prod"))
		if err == nil {
			t.Fatalf("recovery must fail when the restored copy cannot be moved in\n%s", out)
		}
		want := "could not move the restored 'prod' into " + env.dataDir + "; the corrupt store is back in place"
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
		env.assertStoreUntouched(t, out)
		env.assertNoRecoveryLeftovers(t)
	})

	t.Run("neither can be moved", func(t *testing.T) {
		env := setup(t)
		ready := filepath.Join(env.stateDir, "restore-ready", "prod")
		failBoth := "FAKE_MV_FAIL_FROM=" + ready + ":" + filepath.Join(env.stateDir, "corrupt-aside", "prod.*")
		out, err := env.run(t, journalRecoveryHarness(t, false), failBoth)
		if err == nil {
			t.Fatalf("recovery must fail when neither the copy nor the store can be moved\n%s", out)
		}
		asides, err := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "prod.*"))
		if err != nil || len(asides) != 1 {
			t.Fatalf("the corrupt store must still be aside, got %v (err %v)\n%s", asides, err, out)
		}
		want := "CRITICAL: the restored 'prod' could not be moved into " + env.dataDir +
			" AND the corrupt store could not be moved back; original data is at " + asides[0] + ", the restored copy at " + ready
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
		if doltLog := readOptional(t, env.logFile); !strings.Contains(doltLog, want) {
			t.Fatalf("dolt log missing %q:\n%s", want, doltLog)
		}
		assertCopiesKept := func(t *testing.T, out string) {
			t.Helper()
			if _, err := os.Stat(filepath.Join(env.dataDir, "prod")); !os.IsNotExist(err) {
				t.Fatalf("nothing may take prod's place in the data dir (stat err: %v)\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
				t.Fatalf("the corrupt store must be kept at %s: %v\n%s", asides[0], err, out)
			}
			if _, err := os.Stat(filepath.Join(ready, "restored-marker")); err != nil {
				t.Fatalf("the restored copy must be kept at %s: %v\n%s", ready, err, out)
			}
		}
		assertCopiesKept(t, out)

		finish := journalNextStartHarness(t, false)
		again, err := env.run(t, finish, failBoth)
		if err == nil {
			t.Fatalf("the next start must fail closed while prod cannot be moved in\n%s", again)
		}
		want = "CRITICAL: 'prod' is missing from " + env.dataDir + ", and the copy an interrupted recovery restored could not be moved in from " + ready
		if !strings.Contains(again, want) {
			t.Fatalf("output missing %q:\n%s", want, again)
		}
		assertCopiesKept(t, again)

		again, err = env.run(t, finish)
		if err != nil {
			t.Fatalf("the next start must move the copy in once it can: %v\n%s", err, again)
		}
		if want := "RESTORED 'prod' by moving in " + ready; !strings.Contains(again, want) {
			t.Fatalf("output missing %q:\n%s", want, again)
		}
		if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "restored-marker")); err != nil {
			t.Fatalf("prod must be restored in the data dir: %v\n%s", err, again)
		}
		if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
			t.Fatalf("the corrupt store must still be kept at %s: %v", asides[0], err)
		}
		env.assertNoRecoveryLeftovers(t)
	})
}

// TestJournalRecoveryRefusesDirectoriesOnAnotherFilesystem pins that
// recovery moves databases only by renaming them. When restore-staging/,
// restore-ready/ or corrupt-aside/ cannot be hard-linked into the data dir,
// as across filesystems, where mv copies and a start killed during the copy
// leaves a partial database, recovery refuses before it restores anything:
// the store stays in place, and the manual restore command names the
// selected backup, which is usable.
func TestJournalRecoveryRefusesDirectoriesOnAnotherFilesystem(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	for _, dir := range []string{"restore-staging", "restore-ready", "corrupt-aside"} {
		t.Run(dir, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
				t.Fatalf("seed backup content: %v", err)
			}
			env.addDatabase(t, "prod", true, bdDefaultRepoState(env.backupDir))
			restoreCalls := filepath.Join(env.cityDir, "restore-calls.log")

			out, err := env.run(t, journalRecoveryHarness(t, false),
				"FAKE_RESTORE_CALLS="+restoreCalls,
				"FAKE_LN_FAIL_FROM="+filepath.Join(env.stateDir, dir, "*"))
			if err == nil {
				t.Fatalf("recovery must refuse when %s is on another filesystem\n%s", dir, out)
			}
			want := "NOT auto-recovering 'prod': " + filepath.Join(env.stateDir, dir) + " is not on the filesystem of " + env.dataDir +
				" (ln: failed to create hard link"
			if !strings.Contains(out, want) {
				t.Fatalf("refusal must say which directory a rename cannot reach (%q):\n%s", want, out)
			}
			wantManual := "manual recovery required: move " + filepath.Join(env.dataDir, "prod") + " aside, then run \"dolt backup restore 'file://" +
				env.backupDir + "' prod\" from " + env.dataDir + "\n"
			if !strings.Contains(out, wantManual) {
				t.Fatalf("refusal must name the usable backup in the manual command (%q):\n%s", wantManual, out)
			}
			env.assertStoreUntouched(t, out)
			if calls := strings.TrimSpace(readOptional(t, restoreCalls)); calls != "" {
				t.Fatalf("no restore may run once recovery refused, restore calls:\n%s", calls)
			}
			if calls := strings.TrimSpace(readOptional(t, env.backupAddLog)); calls != "" {
				t.Fatalf("no backup may be registered once recovery refused, backup add calls:\n%s", calls)
			}
			env.assertNoRecoveryLeftovers(t)
		})
	}
}

// TestGcBeadsBdStartFinishesAnInterruptedJournalRecovery runs the provider's
// start over what a recovery killed between its two renames leaves: prod
// missing from the data dir, its restored copy in restore-ready/ and the
// corrupt store under corrupt-aside/. Start moves the copy in before it
// starts the server. While the copy cannot be moved in, start fails without
// starting the server, which would otherwise serve the city without prod and
// let init create it as an empty bead store.
func TestGcBeadsBdStartFinishesAnInterruptedJournalRecovery(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not available; gc-beads-bd start requires it")
	}
	for _, tc := range []struct {
		name     string
		failMove bool
	}{
		{name: "moves the restored copy in"},
		{name: "fails closed while the copy cannot be moved in", failMove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			materializeBuiltinPacksForTest(t, cityPath)
			script := gcBeadsBdScriptPath(cityPath)
			binDir := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
			fakeGC := writeFakeManagedConfigWriterGC(t, binDir, invocationFile)
			writeFakeManagedConfigWriterDolt(t, binDir)
			writeJournalFakeMoves(t, binDir)

			// The layout writeFakeManagedConfigWriterGC reports.
			packDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt-from-gc")
			dataDir := filepath.Join(cityPath, ".beads", "dolt")
			ready := filepath.Join(packDir, "restore-ready", "prod")
			aside := filepath.Join(packDir, "corrupt-aside", "prod.20261027T000000Z")
			for _, dir := range []string{filepath.Join(ready, ".dolt"), filepath.Join(aside, ".dolt"), dataDir} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(ready, "restored-marker"), []byte("restored"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(aside, "original-marker"), []byte("prod"), 0o644); err != nil {
				t.Fatal(err)
			}

			env := sanitizedBaseEnv(
				"GC_CITY_PATH="+cityPath,
				"GC_BIN="+fakeGC,
				"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
			)
			if tc.failMove {
				env = append(env, "FAKE_MV_FAIL_FROM="+ready)
			}
			// The fake gc's start-managed starts no process, so nothing is
			// left to stop.
			stdout, stderr, err := runGCBeadsBdCommand(t, env, script, "start")
			out := stdout + stderr
			started := strings.Contains(readOptional(t, invocationFile), "gc dolt-state start-managed")
			if _, statErr := os.Stat(filepath.Join(aside, "original-marker")); statErr != nil {
				t.Fatalf("the corrupt store must stay aside: %v\n%s", statErr, out)
			}

			if tc.failMove {
				if err == nil {
					t.Fatalf("start must fail while prod is missing from the data dir\n%s", out)
				}
				if started {
					t.Fatalf("start started the server without prod:\n%s", out)
				}
				for _, want := range []string{
					"CRITICAL: 'prod' is missing from " + dataDir,
					"a database is missing from " + dataDir + " after an interrupted journal-corruption recovery",
				} {
					if !strings.Contains(out, want) {
						t.Fatalf("start output missing %q:\n%s", want, out)
					}
				}
				if _, statErr := os.Stat(filepath.Join(ready, "restored-marker")); statErr != nil {
					t.Fatalf("the restored copy must be kept at %s: %v", ready, statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
			}
			if !started {
				t.Fatalf("start did not start the server:\n%s", out)
			}
			if _, statErr := os.Stat(filepath.Join(dataDir, "prod", "restored-marker")); statErr != nil {
				t.Fatalf("start must move the restored copy into the data dir: %v\n%s", statErr, out)
			}
			if _, statErr := os.Stat(ready); !os.IsNotExist(statErr) {
				t.Fatalf("the restored copy must leave restore-ready/ (stat err: %v)", statErr)
			}
			want := "gc-beads-bd: RESTORED 'prod' by moving in " + ready
			if doltLog := readOptional(t, filepath.Join(packDir, "dolt.log")); !strings.Contains(doltLog, want) {
				t.Fatalf("dolt log missing %q:\n%s", want, doltLog)
			}
		})
	}
}

// TestJournalRestoreLimitFitsTheProviderStartOperation pins how long an
// automatic restore may run. gc gives the provider's start operation that
// recovery runs in 120s, and before the restored copy is swapped in come the
// server start attempt that reported the corruption
// (load_start_managed_from_gc), the corrupt database's offline probe, the
// restore and the backup's registration on the restored copy. With each at
// the limit the script sets, the default must end before gc's kill, so a
// restore that runs out of time fails start with its message and one that
// finishes is swapped in. The server start after the swap does not count: a
// kill there leaves the restored database in place
// (TestJournalRecoveryConvergesWhenKilledPartWay). 0 means no limit, which
// `gc dolt start` sets, and a value that is not a whole number of seconds is
// reported and replaced by the default.
func TestJournalRestoreLimitFitsTheProviderStartOperation(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	script, err := os.ReadFile(filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	harness := "#!/usr/bin/env bash\nset -u\n" +
		extractShellFunction(t, string(script), "journal_recovery_note") + "\n" +
		extractShellFunction(t, string(script), "journal_restore_limit") + "\n" +
		"journal_restore_limit\n"
	for _, tc := range []struct {
		name, setting, want string
		warns               bool
	}{
		{name: "default", want: "45"},
		{name: "no limit", setting: "0", want: "0"},
		{name: "operator limit", setting: "600", want: "600"},
		{name: "unit suffix", setting: "45s", want: "45", warns: true},
		{name: "negative", setting: "-1", want: "45", warns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			var extra []string
			if tc.setting != "" {
				extra = append(extra, "GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS="+tc.setting)
			}
			cmd := env.command(harness, extra...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("journal_restore_limit failed: %v\n%s", err, stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got != tc.want {
				t.Fatalf("limit = %q, want %q", got, tc.want)
			}
			warning := "ignoring GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS='" + tc.setting + "', which is not a whole number of seconds; using 45"
			if got := strings.Contains(stderr.String(), warning); got != tc.warns {
				t.Fatalf("warning %q reported = %v, want %v:\n%s", warning, got, tc.warns, stderr.String())
			}
			if tc.name != "default" {
				return
			}
			limit, err := strconv.Atoi(tc.want)
			if err != nil {
				t.Fatalf("parse default limit %q: %v", tc.want, err)
			}
			restore := time.Duration(limit) * time.Second
			startAttempt := scriptTimeLimit(t, string(script), `dolt-state start-managed [^\n]*--timeout-ms (\d+)`, time.Millisecond)
			probe := scriptTimeLimit(t, string(script), `run_with_timeout (\d+) dolt status`, time.Second)
			reregister := scriptTimeLimit(t, string(script), `run_with_timeout (\d+) dolt backup add`, time.Second)
			if budget := providerOpTimeout("start"); startAttempt+probe+restore+reregister >= budget {
				t.Fatalf("a %s server start attempt, a %s probe, a %s restore and a %s registration do not end before gc kills the %s start operation",
					startAttempt, probe, restore, reregister, budget)
			}
		})
	}
}

// scriptTimeLimit returns the time limit the script's one match of pattern
// sets: its first group, a whole number of unit.
func scriptTimeLimit(t *testing.T, script, pattern string, unit time.Duration) time.Duration {
	t.Helper()
	matches := regexp.MustCompile(pattern).FindAllStringSubmatch(script, -1)
	if len(matches) != 1 {
		t.Fatalf("script has %d matches of %q, want exactly 1", len(matches), pattern)
	}
	n, err := strconv.Atoi(matches[0][1])
	if err != nil {
		t.Fatalf("parse the limit %q matched by %q: %v", matches[0][1], pattern, err)
	}
	return time.Duration(n) * unit
}

func TestJournalRecoveryReportsWhenNoCorruptionConfirmed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	env.addDatabase(t, "healthy", false, "")

	out, err := env.run(t, journalRecoveryHarness(t, false))
	if err == nil {
		t.Fatalf("recovery must fail when nothing is corrupt (caller dies with original error)\n%s", out)
	}
	if !strings.Contains(out, "no corrupt database confirmed") {
		t.Fatalf("output missing probe-found-nothing notice:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(env.dataDir, "healthy", "original-marker")); statErr != nil {
		t.Fatalf("healthy database must not be touched: %v", statErr)
	}
}

// runBackupRemoteURLShapeTests exercises backup_remote_url_for_recovery and
// backup_registry_summary against the repo_state.json shapes they must read,
// asserting the `name<TAB>url` selection and the name=url listing. maskJQ
// forces the awk fallback by hiding jq from `command -v`; otherwise the host
// jq path is exercised. Both readers must agree on every shape.
func runBackupRemoteURLShapeTests(t *testing.T, maskJQ bool) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	var fns strings.Builder
	for _, fn := range []string{
		"backup_registry_entries",
		"backup_registry_has_backup",
		"backup_registry_backup_url",
		"backup_registry_summary",
		"backup_remote_url_for_recovery",
	} {
		fns.WriteString(extractShellFunction(t, string(scriptBytes), fn))
		fns.WriteString("\n")
	}

	tests := []struct {
		name      string
		repoState string
		want      string
		summary   string
	}{
		{
			name:      "legacy object form",
			repoState: `{"backups":{"prod-backup":{"name":"prod-backup","url":"file:///backups/prod","fetch_specs":[]}}}`,
			want:      "prod-backup\tfile:///backups/prod",
			summary:   "prod-backup=file:///backups/prod",
		},
		{
			name:      "legacy string form",
			repoState: `{"backups":{"prod-backup":"file:///backups/prod"}}`,
			want:      "prod-backup\tfile:///backups/prod",
			summary:   "prod-backup=file:///backups/prod",
		},
		{
			name:      "bd default object form",
			repoState: `{"backups":{"default":{"name":"default","url":"file:///backups/prod","fetch_specs":[],"params":{}}}}`,
			want:      "default\tfile:///backups/prod",
			summary:   "default=file:///backups/prod",
		},
		{
			name:      "default wins with legacy first in text",
			repoState: `{"backups":{"prod-backup":{"url":"file:///backups/legacy"},"default":{"url":"file:///backups/dflt"}}}`,
			want:      "default\tfile:///backups/dflt",
			summary:   "prod-backup=file:///backups/legacy, default=file:///backups/dflt",
		},
		{
			name:      "default wins with default first in text",
			repoState: `{"backups":{"default":{"url":"file:///backups/dflt"},"prod-backup":{"url":"file:///backups/legacy"}}}`,
			want:      "default\tfile:///backups/dflt",
			summary:   "default=file:///backups/dflt, prod-backup=file:///backups/legacy",
		},
		{
			name:      "remote named default is not the backup",
			repoState: `{"head":"refs/heads/main","remotes":{"default":{"name":"default","url":"aws://dolt-remote/prod","fetch_specs":[],"params":{}}},"backups":{"prod-backup":{"name":"prod-backup","url":"file:///backups/prod","fetch_specs":[],"params":{}}}}`,
			want:      "prod-backup\tfile:///backups/prod",
			summary:   "prod-backup=file:///backups/prod",
		},
		{
			// Dolt writes "branches" after "backups", and a tracked branch
			// is an object keyed by the branch name.
			name:      "tracked branch named default after the backups",
			repoState: `{"head":"refs/heads/main","remotes":{},"backups":{"prod-backup":{"name":"prod-backup","url":"file:///backups/prod","fetch_specs":[],"params":{}}},"branches":{"default":{"head":"refs/heads/default","remote":"origin","merge":"refs/heads/default"}}}`,
			want:      "prod-backup\tfile:///backups/prod",
			summary:   "prod-backup=file:///backups/prod",
		},
		{
			name:      "tracked branch named default after a legacy string entry",
			repoState: `{"head":"refs/heads/main","backups":{"prod-backup":"file:///backups/prod"},"branches":{"default":{"head":"refs/heads/default","remote":"origin"}}}`,
			want:      "prod-backup\tfile:///backups/prod",
			summary:   "prod-backup=file:///backups/prod",
		},
		{
			name:      "key named default nested inside a backup",
			repoState: `{"backups":{"prod-backup":{"name":"default","url":"file:///backups/prod","params":{"default":{"url":"file:///backups/other"}}}}}`,
			want:      "prod-backup\tfile:///backups/prod",
			summary:   "prod-backup=file:///backups/prod",
		},
		{
			// Selection is by presence: a default entry without a URL is
			// still selected, and the caller refuses it.
			name:      "default entry without a url",
			repoState: `{"backups":{"default":{"name":"default","fetch_specs":[],"params":{}},"prod-backup":{"url":"file:///backups/prod"}}}`,
			want:      "default\t",
			summary:   "default=, prod-backup=file:///backups/prod",
		},
		{
			name: "pretty-printed as dolt writes it",
			repoState: `{
  "head": "refs/heads/main",
  "remotes": {},
  "backups": {
    "default": {
      "name": "default",
      "url": "file:///backups/prod",
      "fetch_specs": [
        "refs/heads/*:refs/remotes/default/*"
      ],
      "params": {}
    }
  },
  "branches": {}
}
`,
			want:    "default\tfile:///backups/prod",
			summary: "default=file:///backups/prod",
		},
		{
			name:      "escaped slashes in the url",
			repoState: `{"backups":{"default":{"url":"file:\/\/\/backups\/prod"}}}`,
			want:      "default\tfile:///backups/prod",
			summary:   "default=file:///backups/prod",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "repo_state.json"), []byte(tc.repoState), 0o644); err != nil {
				t.Fatal(err)
			}
			harness := "#!/usr/bin/env bash\nset -u\n"
			if maskJQ {
				// The `command` override masks jq so the awk fallback is what
				// gets exercised regardless of the host toolchain.
				harness += "command() { case \"$*\" in '-v jq') return 1 ;; esac; builtin command \"$@\"; }\n"
			}
			harness += fns.String() + "\nbackup_remote_url_for_recovery prod \"$1\"\nbackup_registry_summary \"$1\"\n"
			out, err := exec.Command("bash", "-c", harness, "harness", dbDir).CombinedOutput()
			if err != nil {
				t.Fatalf("backup_remote_url_for_recovery failed: %v\n%s", err, out)
			}
			selection, summary, _ := strings.Cut(string(out), "\n")
			if selection != tc.want {
				t.Fatalf("selection = %q, want %q", selection, tc.want)
			}
			if summary != tc.summary {
				t.Fatalf("summary = %q, want %q", summary, tc.summary)
			}
		})
	}
}

func TestBackupRemoteURLForRecoveryParsesBothShapesWithoutJQ(t *testing.T) {
	runBackupRemoteURLShapeTests(t, true)
}

// The jq path must handle the legacy string form too: `.url` on a string
// errors in jq, so the expression uses `.url?` — this test pins that (#3176
// review finding).
func TestBackupRemoteURLForRecoveryParsesBothShapesWithJQ(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; skipping jq-path test")
	}
	runBackupRemoteURLShapeTests(t, false)
}
