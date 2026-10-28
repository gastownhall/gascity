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
//   - the corrupt store is preserved (moved aside, never deleted) before any
//     restore;
//   - restore only runs from a non-empty local file:// <db>-backup remote;
//   - fail-closed: when no usable backup exists or the restore fails, the
//     store ends up back in the data dir and the function fails so the server
//     cannot start silently missing a database.

func journalRecoveryHarness(t *testing.T) string {
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
	for _, fn := range []string{
		"run_with_timeout",
		"journal_corruption_signature",
		"database_journal_corrupt",
		"backup_remote_url_for_recovery",
		"backup_restore_source_usable",
		"attempt_journal_corruption_recovery",
	} {
		harness.WriteString(extractShellFunction(t, script, fn))
		harness.WriteString("\n")
	}
	harness.WriteString("attempt_journal_corruption_recovery\n")
	return harness.String()
}

// writeJournalFakeDolt stubs `dolt status` (corrupt when the database dir
// contains a .corrupt marker) and `dolt backup restore` (recreates the
// database with a restored-marker, honoring FAKE_RESTORE_EXIT).
func writeJournalFakeDolt(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -u
case "${1:-}" in
  status)
    if [ -f "$PWD/.corrupt" ]; then
      echo "failed to load database: possible data loss detected in journal file at offset 961963: corrupted journal" >&2
      exit 1
    fi
    exit 0
    ;;
  backup)
    if [ "${2:-}" = "restore" ]; then
      if [ "${FAKE_RESTORE_EXIT:-0}" != "0" ]; then
        echo "restore failed" >&2
        exit "$FAKE_RESTORE_EXIT"
      fi
      db="$4"
      mkdir -p "$PWD/$db/.dolt"
      echo restored > "$PWD/$db/restored-marker"
      exit 0
    fi
    ;;
esac
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
	// shell runs the harness; empty means bash.
	shell string
}

func setupJournalRecoveryEnv(t *testing.T) journalRecoveryEnv {
	t.Helper()
	cityDir := t.TempDir()
	env := journalRecoveryEnv{
		cityDir:   cityDir,
		dataDir:   filepath.Join(cityDir, "dolt-data"),
		stateDir:  filepath.Join(cityDir, "pack-state"),
		logFile:   filepath.Join(cityDir, "dolt.log"),
		backupDir: filepath.Join(cityDir, "backups", "prod"),
		binDir:    filepath.Join(cityDir, "bin"),
	}
	for _, dir := range []string{env.dataDir, env.stateDir, env.backupDir, env.binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeJournalFakeDolt(t, env.binDir)
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

func TestRunWithTimeoutCapturedOutputDoesNotWaitForWatchdogSleep(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	runWithTimeout := extractShellFunction(t, string(scriptBytes), "run_with_timeout")

	newPipe := func(name string) (*os.File, *os.File) {
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
	signalR, signalW := newPipe("sleep-signal")
	releaseR, releaseW := newPipe("sleep-release")
	commandGateR, commandGateW := newPipe("command-gate")

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

	harness := "#!/usr/bin/env bash\nset -u\n" + runWithTimeout + `
result=$(run_with_timeout 120 sh -c 'IFS= read -r gate <&"$COMMAND_GATE_READ_FD"; [ "$gate" = go ]; printf command-output')
printf 'result=%s\n' "$result"
`
	env := journalRecoveryEnv{binDir: binDir}
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
	readSignal := func(name, want string) error {
		if err := signalR.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
			return fmt.Errorf("set %s deadline: %w", name, err)
		}
		var got string
		if _, err := fmt.Fscan(signalReader, &got); err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("%s = %q, want %q", name, got, want)
		}
		return nil
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

	if err := readSignal("watchdog sleep started signal", "started"); err != nil {
		t.Fatal(err)
	}

	completedBeforeRelease := waitCommand()
	releaseControls()
	if err := readSignal("watchdog sleep finished signal", "finished"); err != nil {
		t.Fatal(err)
	}
	blockedByWatchdogPipe := !completedBeforeRelease
	if blockedByWatchdogPipe {
		if !waitCommand() {
			t.Fatal("run_with_timeout harness did not finish after releasing the watchdog sleep")
		}
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
	if blockedByWatchdogPipe {
		t.Fatal("run_with_timeout command substitution stayed blocked after the watchdog sleep released the timed command because the sleep retained the substitution output pipe")
	}
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

func prodRepoState(backupDir string) string {
	return `{"head":"refs/heads/main","backups":{"prod-backup":{"name":"prod-backup","url":"file://` + backupDir + `","fetch_specs":[],"params":{}}}}`
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

	out, err := env.run(t, journalRecoveryHarness(t))
	if err != nil {
		t.Fatalf("recovery failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "RESTORED 'prod'") {
		t.Fatalf("output missing loud restore notice:\n%s", out)
	}
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

func TestJournalRecoveryFailsClosedWithoutUsableBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	tests := []struct {
		name      string
		repoState func(env journalRecoveryEnv) string
		seed      bool
	}{
		{
			name:      "no backup remote recorded",
			repoState: func(journalRecoveryEnv) string { return `{"head":"refs/heads/main"}` },
		},
		{
			name:      "backup remote dir empty",
			repoState: func(env journalRecoveryEnv) string { return prodRepoState(env.backupDir) },
		},
		{
			name: "non-file backup remote",
			repoState: func(journalRecoveryEnv) string {
				return `{"backups":{"prod-backup":{"url":"aws://bucket/prod"}}}`
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			env.addDatabase(t, "prod", true, tc.repoState(env))

			out, err := env.run(t, journalRecoveryHarness(t))
			if err == nil {
				t.Fatalf("recovery must fail closed without a usable backup\n%s", out)
			}
			if !strings.Contains(out, "NOT auto-recovering 'prod'") {
				t.Fatalf("output missing loud refusal:\n%s", out)
			}
			if _, statErr := os.Stat(filepath.Join(env.dataDir, "prod", "original-marker")); statErr != nil {
				t.Fatalf("prod store must stay in place when not recoverable: %v", statErr)
			}
		})
	}
}

func TestJournalRecoveryMovesStoreBackWhenRestoreFails(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, prodRepoState(env.backupDir))

	out, err := env.run(t, journalRecoveryHarness(t), "FAKE_RESTORE_EXIT=1")
	if err == nil {
		t.Fatalf("recovery must fail when restore fails\n%s", out)
	}
	if !strings.Contains(out, "backup restore failed for 'prod'") {
		t.Fatalf("output missing restore failure notice:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(env.dataDir, "prod", "original-marker")); statErr != nil {
		t.Fatalf("prod store must be moved back after failed restore (fail closed): %v\n%s", statErr, out)
	}
}

func TestJournalRecoveryReportsWhenNoCorruptionConfirmed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	env.addDatabase(t, "healthy", false, "")

	out, err := env.run(t, journalRecoveryHarness(t))
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

// runBackupRemoteURLShapeTests exercises backup_remote_url_for_recovery
// against both repo_state.json backup shapes. maskJQ forces the sed fallback
// by hiding jq from `command -v`; otherwise the host jq path is exercised.
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
	fn := extractShellFunction(t, string(scriptBytes), "backup_remote_url_for_recovery")

	tests := []struct {
		name      string
		repoState string
		want      string
	}{
		{
			name:      "object form",
			repoState: `{"backups":{"prod-backup":{"name":"prod-backup","url":"file:///backups/prod","fetch_specs":[]}}}`,
			want:      "file:///backups/prod",
		},
		{
			name:      "legacy string form",
			repoState: `{"backups":{"prod-backup":"file:///backups/prod"}}`,
			want:      "file:///backups/prod",
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
				// The `command` override masks jq so the sed fallback is what
				// gets exercised regardless of the host toolchain.
				harness += "command() { case \"$*\" in '-v jq') return 1 ;; esac; builtin command \"$@\"; }\n"
			}
			harness += fn + "\nbackup_remote_url_for_recovery prod \"$1\"\n"
			out, err := exec.Command("bash", "-c", harness, "harness", dbDir).CombinedOutput()
			if err != nil {
				t.Fatalf("backup_remote_url_for_recovery failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
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
