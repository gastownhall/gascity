package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// shell runs the harness (empty means bash). Pins exercise the script's
	// shell functions under both bash and /bin/sh because production hosts
	// run dash.
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

// TestRunWithTimeoutCapturedOutputDoesNotWaitForWatchdogSleep holds the two
// captured-output contracts of run_with_timeout (#4462, #8392): the command
// substitution yields the command's output without waiting for the
// watchdog's sleep, and the call stops that sleep instead of leaving it to
// run out the timeout holding the substitution pipe.
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

	// #4462: the substitution must complete as soon as the timed
	// command does, before the release pipe is written.
	if !waitCommand() {
		t.Fatal("run_with_timeout command substitution stayed blocked on the watchdog sleep before the release pipe was written")
	}
	// #8392: the call must also stop the sleep rather than leave it
	// to run out the timeout; dead, it can no longer print
	// "finished", so the signal pipe drains to EOF.
	if err := readerSeesEOF(signalR, signalReader, testutil.ExecRaceTimeout); err != nil {
		t.Fatalf("watchdog sleep outlived the captured-output call: %v", err)
	}
	releaseControls()

	if runErr != nil {
		t.Errorf("run_with_timeout harness failed: %v\nstderr: %s", runErr, stderr.String())
	}
	if got, want := stdout.String(), "result=command-output\n"; got != want {
		t.Errorf("harness output = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Errorf("harness stderr = %q, want empty", got)
	}
}

// forEachPinShell runs a shell-function pin under bash and /bin/sh: the
// script's production hosts include dash, so its functions must hold under
// both. A shell that is not installed is skipped.
func forEachPinShell(t *testing.T, pin func(t *testing.T, shell string)) {
	t.Helper()
	for _, shell := range []string{"bash", "sh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("%s not available; skipping shell-function test", shell)
			}
			pin(t, shell)
		})
	}
}

// runWithTimeoutFn extracts run_with_timeout from the bd provider script.
func runWithTimeoutFn(t *testing.T) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return extractShellFunction(t, string(scriptBytes), "run_with_timeout")
}

// pinHarness prefixes a pin scenario with the shebang, set -eu (the script
// runs set -e) and the extracted function.
func pinHarness(fn, scenario string) string {
	return "#!/usr/bin/env bash\nset -eu\n" + fn + "\n" + scenario
}

// pipeSeesEOF reads r once and reports whether that read hit EOF: every
// writer the harness handed down must be gone.
func pipeSeesEOF(r *os.File, timeout time.Duration) error {
	if err := r.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set EOF probe deadline: %w", err)
	}
	var one [1]byte
	if _, err := r.Read(one[:]); err != io.EOF {
		return fmt.Errorf("read returned %w, want EOF", err)
	}
	return nil
}

// readerSeesEOF drains br (which may hold buffered bytes in front of r, so a
// raw r.Read would miss them) and reports whether it ends at EOF without
// further data arriving within timeout.
func readerSeesEOF(r *os.File, br *bufio.Reader, timeout time.Duration) error {
	if err := r.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set EOF probe deadline: %w", err)
	}
	for {
		if _, err := br.ReadByte(); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("read returned %w, want EOF", err)
		}
	}
}

// TestRunWithTimeoutStopsWatchdogSleepWhenCommandFinishes pins the #8392
// contract for a command that finishes while the watchdog's sleep is still
// running: the call must stop that sleep before returning. The watchdog
// redirects only fds 0-2, so the sleep inherits every other caller
// descriptor (op_start holds its start lock on fd 9 across timed calls) and
// a sleep left behind keeps them for the rest of the timeout. The fake
// sleep announces its own pid on the signal pipe and blocks until released;
// after the caller exits the pid must be gone and the signal pipe must see
// EOF rather than the sleep's "finished".
func TestRunWithTimeoutStopsWatchdogSleepWhenCommandFinishes(t *testing.T) {
	forEachPinShell(t, func(t *testing.T, shell string) {
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
printf 'started %s\n' "$$" >&"$SLEEP_SIGNAL_FD"
printf 'go\n' >&"$COMMAND_GATE_WRITE_FD"
IFS= read -r release <&"$SLEEP_RELEASE_FD"
[ "$release" = "release" ] || exit 65
printf 'finished\n' >&"$SLEEP_SIGNAL_FD"
`)

		// The gate makes the command wait for the sleep's "go", so the sleep
		// is provably running when the command finishes.
		harness := pinHarness(runWithTimeoutFn(t), `
run_with_timeout 120 sh -c 'IFS= read -r gate <&"$COMMAND_GATE_READ_FD"; [ "$gate" = go ]'
`)
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
		doneConsumed := false
		defer func() {
			// On a red run the orphaned sleep blocks on the release pipe;
			// releasing the controls ends it.
			releaseControls()
			if doneConsumed {
				return
			}
			select {
			case <-done:
			case <-time.After(testutil.ExecRaceTimeout):
				_ = cmd.Process.Kill()
				<-done
			}
		}()

		signalReader := bufio.NewReader(signalR)
		readToken := func(name string) (string, error) {
			if err := signalR.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
				return "", fmt.Errorf("set %s deadline: %w", name, err)
			}
			var token string
			if _, err := fmt.Fscan(signalReader, &token); err != nil {
				return "", fmt.Errorf("read %s: %w", name, err)
			}
			return token, nil
		}
		if token, err := readToken("watchdog sleep started signal"); err != nil {
			t.Fatal(err)
		} else if token != "started" {
			t.Fatalf("watchdog sleep started signal = %q, want %q", token, "started")
		}
		pidToken, err := readToken("watchdog sleep pid")
		if err != nil {
			t.Fatal(err)
		}
		sleepPid, err := strconv.Atoi(pidToken)
		if err != nil {
			t.Fatalf("parse watchdog sleep pid %q: %v", pidToken, err)
		}

		var runErr error
		timer := time.NewTimer(testutil.ExecRaceTimeout)
		defer timer.Stop()
		select {
		case runErr = <-done:
			doneConsumed = true
		case <-timer.C:
			releaseControls()
			t.Fatal("run_with_timeout harness did not finish after its command completed")
		}
		if runErr != nil {
			t.Fatalf("run_with_timeout harness failed: %v\nstdout: %s\nstderr: %s", runErr, stdout.String(), stderr.String())
		}
		if pidAlive(sleepPid) {
			releaseControls()
			t.Fatalf("watchdog sleep (pid %d) is still running after run_with_timeout returned; it would hold every inherited descriptor for the rest of the timeout", sleepPid)
		}
		releaseControls()
		if err := readerSeesEOF(signalR, signalReader, testutil.ExecRaceTimeout); err != nil {
			t.Fatalf("watchdog sleep outlived the call through the signal pipe: %v", err)
		}
	})
}

// TestRunWithTimeoutLeavesNothingAfterInstantCommands pins the #8392 race
// window at its narrowest: a TERM that lands between the watchdog's
// `sleep &` and the pid assignment must not leak the sleep either. Two
// hundred back-to-back calls of an instant command under set -eu; the
// caller holds the write end of a pipe on fd 3, and once it exits its whole
// process group must be gone and the pipe must see EOF. The timeout is 2 s
// so a red run's leftovers expire on their own.
func TestRunWithTimeoutLeavesNothingAfterInstantCommands(t *testing.T) {
	forEachPinShell(t, func(t *testing.T, shell string) {
		pipeR, pipeW, err := os.Pipe()
		if err != nil {
			t.Fatalf("create fd-3 pipe: %v", err)
		}
		t.Cleanup(func() {
			_ = pipeR.Close()
			_ = pipeW.Close()
		})

		harness := pinHarness(runWithTimeoutFn(t), `
i=0
while [ "$i" -lt 200 ]; do
    run_with_timeout 2 true
    i=$((i + 1))
done
`)
		env := journalRecoveryEnv{binDir: t.TempDir(), shell: shell}
		cmd := env.command(harness)
		cmd.ExtraFiles = []*os.File{pipeW}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start run_with_timeout harness: %v", err)
		}
		_ = pipeW.Close()
		pgid := cmd.Process.Pid

		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		timer := time.NewTimer(testutil.ExecRaceTimeout + 6*time.Second)
		defer timer.Stop()
		var runErr error
		select {
		case runErr = <-done:
		case <-timer.C:
			_ = cmd.Process.Kill()
			runErr = <-done
			t.Fatalf("run_with_timeout harness did not finish 200 instant calls in time: %v", runErr)
		}
		if runErr != nil {
			t.Fatalf("run_with_timeout harness failed: %v", runErr)
		}
		if err := waitForProcessGroupExit(pgid, time.Second); err != nil {
			t.Fatalf("processes started by the run_with_timeout calls outlived the caller: %v", err)
		}
		if err := pipeSeesEOF(pipeR, time.Second); err != nil {
			t.Fatalf("fd 3 write end handed down to the calls was not released: %v", err)
		}
	})
}

// runTimeoutKillPin runs a harness that must kill a blocking 5 s command
// via the watchdog, print rc=143, take at least minKill (the timeout or
// fallback duration) and well under the command's runtime, and leave
// nothing behind in the harness's process group.
func runTimeoutKillPin(t *testing.T, shell, harness string, minKill time.Duration) {
	t.Helper()
	env := journalRecoveryEnv{binDir: t.TempDir(), shell: shell}
	cmd := env.command(harness)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// stderr stays free: dash reports its TERM'd background child with a
	// "Terminated" notice there, which is the caller shell's job status,
	// not the call's contract.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run_with_timeout harness failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if got, want := stdout.String(), "rc=143\n"; got != want {
		t.Fatalf("harness output = %q, want %q", got, want)
	}
	if elapsed < minKill {
		t.Fatalf("run_with_timeout returned 143 after only %s; the kill fired before %s", elapsed, minKill)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("run_with_timeout returned after %s; the 5s command ran to its end instead of being killed", elapsed)
	}
	if err := waitForProcessGroupExit(cmd.Process.Pid, 2*time.Second); err != nil {
		t.Fatalf("processes started by the call outlived it: %v", err)
	}
}

// TestRunWithTimeoutKillsCommandAfterTimeout guards the timeout path: a
// command that outlives the timeout is TERMed, the call returns its 143 no
// earlier than the timeout, and nothing the call started survives it.
func TestRunWithTimeoutKillsCommandAfterTimeout(t *testing.T) {
	forEachPinShell(t, func(t *testing.T, shell string) {
		harness := pinHarness(runWithTimeoutFn(t), `
rc=0
run_with_timeout 1 sleep 5 || rc=$?
printf 'rc=%s\n' "$rc"
`)
		runTimeoutKillPin(t, shell, harness, time.Second)
	})
}

// TestRunWithTimeoutRejectedDurationFallsBackToOneSecond guards the 1 s
// fallback for a timeout duration sleep rejects: the call still kills the
// command and returns 143 no earlier than one second, also under the
// script's set -e.
func TestRunWithTimeoutRejectedDurationFallsBackToOneSecond(t *testing.T) {
	forEachPinShell(t, func(t *testing.T, shell string) {
		harness := pinHarness(runWithTimeoutFn(t), `
rc=0
run_with_timeout abc sleep 5 || rc=$?
printf 'rc=%s\n' "$rc"
`)
		runTimeoutKillPin(t, shell, harness, time.Second)
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
