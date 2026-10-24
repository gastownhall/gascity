package scripts_test

import (
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
)

// fixtureLifetimeSeconds bounds every fixture process these tests spawn.
// Nothing here may outlive its test: t.Cleanup is skipped entirely when the
// package trips its own -timeout and panics, so a fixture that "runs forever"
// orphans to init and is then indistinguishable from the production bug these
// tests cover — a stale one was mistaken for a live watchdog failure once
// (gas-px0). The bound is far longer than any assertion here waits, so it can
// never mask a real reap failure, and short enough to self-clear.
const fixtureLifetimeSeconds = 300

// reapFixture builds a repo-shaped environment for driving
// scripts/test-go-test-shard with a fake `go` whose run phase is scripted by
// the caller. It mirrors newGoTestShardFixture's shape (fake bin dir ahead of
// PATH, isolated HOME/TMPDIR) but lets each test decide what the product run
// does, which is the only axis these process-ownership tests vary.
type reapFixture struct {
	repoRoot string
	binDir   string
	homeDir  string
	tmpDir   string
	logPath  string
}

// output returns everything the runner has written so far. The runner's
// output goes to a real file rather than an os/exec pipe on purpose: a pipe
// keeps Cmd.Wait blocked until every descendant that inherited the write end
// closes it, which is precisely the orphaned-tree condition under test, so a
// regression would hang the suite instead of failing it.
func (f *reapFixture) output() string {
	if f.logPath == "" {
		return ""
	}
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		return ""
	}
	return string(data)
}

// newReapFixture writes a fake `go` that answers the metadata probes and the
// -list enumeration the shard runner makes, then executes runBody for the
// product run. runBody is plain /bin/sh, and must carry any paths it needs as
// literals: the runner invokes `go` through `env -i` with a fixed allowlist,
// so a test cannot reach the product run through the environment.
func newReapFixture(t *testing.T, runBody string) *reapFixture {
	t.Helper()

	root := repoRoot(t)
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	homeDir := filepath.Join(tmpDir, "home")
	for _, dir := range []string{binDir, homeDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	fakeGo := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  env)
    case "${2:-}" in
      GOPATH|GOCACHE|GOMODCACHE|GOTMPDIR|GOROOT) printf '%%s\n' %q ;;
      *) exit 99 ;;
    esac
    ;;
  list)
    printf '%%s\n' 'github.com/gastownhall/gascity'
    ;;
  test)
    for arg in "$@"; do
      if [ "$arg" = "-list" ]; then
        printf '%%s\n' TestAlpha TestBeta
        exit 0
      fi
    done
%s
    ;;
  *) exit 99 ;;
esac
`, tmpDir, runBody)

	writeExecutable(t, filepath.Join(binDir, "go"), fakeGo)

	return &reapFixture{repoRoot: root, binDir: binDir, homeDir: homeDir, tmpDir: tmpDir}
}

// start launches the shard runner in its own process group so a test can
// signal the runner alone, exactly as a dying parent chain would. The runner's
// output goes to a log file, read back through output.
func (f *reapFixture) start(t *testing.T, extraEnv ...string) *exec.Cmd {
	t.Helper()
	f.logPath = filepath.Join(f.tmpDir, "runner.log")
	logFile, err := os.Create(f.logPath)
	if err != nil {
		t.Fatalf("create runner log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	return f.startWithOutput(t, logFile, extraEnv...)
}

// startWithOutput is start with the runner's stdout and stderr both sent to
// out, for a test that must observe who still holds that descriptor. out is an
// *os.File so the runner inherits it directly: os/exec copies through a pipe of
// its own only for other writers.
func (f *reapFixture) startWithOutput(t *testing.T, out *os.File, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(filepath.Join(f.repoRoot, "scripts", "test-go-test-shard"), "./example", "1", "2")
	cmd.Dir = f.repoRoot
	cmd.Env = append([]string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.homeDir,
		"SHELL=/bin/sh",
		"TMPDIR=" + f.tmpDir,
		"GO_TEST_TIMEOUT=1m",
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
	}, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start test-go-test-shard: %v", err)
	}
	t.Cleanup(func() {
		// Never leave the runner or its tree behind, whatever the assertions did.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

// harnessBash returns a bash running script, for tests that drive a script
// directly rather than through a runner: scripts/lib/harness-reap.sh's
// functions, and tools/bazel/rules/go_test_run.sh (go_test_run_script_test.go).
func harnessBash(script string) *exec.Cmd {
	return exec.Command("bash", "-c", script)
}

// waitWithin reaps cmd, failing if it has not exited before within elapses so
// a regression in the runner cannot wedge the suite.
func waitWithin(t *testing.T, cmd *exec.Cmd, within time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("runner did not exit within %s", within)
		return nil
	}
}

// waitForPIDFile blocks until the scripted run body has recorded the PID of
// the descendant it spawned, so the test signals only after the tree exists.
func waitForPIDFile(t *testing.T, path string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant never recorded its pid at %s within %s", path, within)
	return 0
}

// processGone reports whether pid has left the process table, polling until
// within elapses. A zombie awaiting init's reap still answers to signal 0, so
// this tolerates a short settle window rather than sampling once.
func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// TestGoTestShardReapsTestTreeWhenTheRunnerIsTerminated is the gas-px0
// regression: kit-k72d measured a gc.test binary at PPID 1 burning ~1 core for
// 5h34m after its shard-runner chain died. The runner must own its process
// tree, so terminating the runner takes the whole tree with it instead of
// orphaning the test binary to init.
func TestGoTestShardReapsTestTreeWhenTheRunnerIsTerminated(t *testing.T) {
	pidDir := t.TempDir()
	pidFile := filepath.Join(pidDir, "descendant.pid")
	spawner := filepath.Join(pidDir, "spawn-descendant")

	// The descendant is a grandchild of the product run, which is what the
	// real shape is: shard runner -> go test -> gc.test.
	fixture := newReapFixture(t, fmt.Sprintf(`
    %q &
    sleep %d
`, spawner, fixtureLifetimeSeconds))
	writeExecutable(t, spawner, fmt.Sprintf(`#!/bin/sh
echo $$ > %q
exec sleep %d
`, pidFile, fixtureLifetimeSeconds))

	cmd := fixture.start(t)
	descendant := waitForPIDFile(t, pidFile, 30*time.Second)

	// Kill only the runner, the way a dead driver, a closed session, or a
	// killed parent chain does. Its descendants must not survive it.
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal runner: %v", err)
	}

	if !processGone(descendant, 20*time.Second) {
		t.Fatalf("descendant pid %d survived the runner's termination: the shard runner orphaned its test process tree", descendant)
	}
}

// TestGoTestShardWatchdogKillsARunThatDefeatsGoTimeout covers the other half
// of kit-k72d: the observed binary ran 5h34m despite -test.timeout=20m0s,
// because a wedged runtime defeats go's own timeout goroutine. An external
// watchdog owned by the runner must terminate the run regardless, escalating
// past signals the run ignores.
func TestGoTestShardWatchdogKillsARunThatDefeatsGoTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "wedged.pid")

	// Ignoring every catchable signal is how a wedged runtime behaves: go's
	// own timeout goroutine never gets to run, and a polite TERM is dropped.
	// Bounded rather than a bare `while :` loop: it must outlive the watchdog
	// under test by a wide margin to prove anything, but it must also die on
	// its own if nothing reaps it.
	fixture := newReapFixture(t, fmt.Sprintf(`
    trap '' TERM INT QUIT HUP
    echo $$ > %q
    end=$(( $(date +%%s) + %d ))
    while [ "$(date +%%s)" -lt "$end" ] ; do sleep 1 ; done
`, pidFile, fixtureLifetimeSeconds))

	start := time.Now()
	cmd := fixture.start(t,
		"GO_TEST_TIMEOUT=2s",
		"GO_TEST_WATCHDOG_GRACE=2s",
	)
	wedged := waitForPIDFile(t, pidFile, 30*time.Second)

	err := waitWithin(t, cmd, 90*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("runner exited 0; a watchdog kill must be a failure.\n%s", fixture.output())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("wait runner: %v\n%s", err, fixture.output())
	}
	if elapsed > 60*time.Second {
		t.Fatalf("watchdog took %s to fire with a 2s timeout and 2s grace", elapsed)
	}
	if out := fixture.output(); !strings.Contains(out, "watchdog") {
		t.Fatalf("runner gave no watchdog diagnostic; output:\n%s", out)
	}
	if !processGone(wedged, 20*time.Second) {
		t.Fatalf("wedged run pid %d survived the watchdog: it must escalate past ignored signals", wedged)
	}
}

// sweepHarness runs gc_harness_sweep_stale_orphans against a scripted process
// table. `kill` is a bash builtin and cannot be shadowed on PATH, so the sweep
// routes every kill through gc_harness_kill_pid and the test overrides that
// one function to record decisions instead of destroying real processes.
func sweepHarness(t *testing.T, lsofOutput, psOutput, minAge string) string {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	// Fixture tables go through files rather than being interpolated into the
	// fake tools: a multi-line table embedded with %q arrives as a literal
	// backslash-n, which silently collapses the whole table onto one line.
	psFixture := filepath.Join(dir, "ps.txt")
	lsofFixture := filepath.Join(dir, "lsof.txt")
	if err := os.WriteFile(psFixture, []byte(psOutput), 0o644); err != nil {
		t.Fatalf("write ps fixture: %v", err)
	}
	if err := os.WriteFile(lsofFixture, []byte(lsofOutput), 0o644); err != nil {
		t.Fatalf("write lsof fixture: %v", err)
	}

	// `ps -p <pid>` reports the socket holder's age; the population-1 scan
	// (`ps -u ...`) is fed separately so each case exercises one population.
	writeExecutable(t, filepath.Join(binDir, "ps"), fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "-p" ]; then
    printf '%%s\n' '99:59:59'
    exit 0
  fi
done
cat %q
`, psFixture))
	writeExecutable(t, filepath.Join(binDir, "lsof"), fmt.Sprintf(`#!/bin/sh
cat %q
`, lsofFixture))

	killLog := filepath.Join(dir, "kills")
	script := fmt.Sprintf(`
set -euo pipefail
source %q
gc_harness_kill_pid() { printf 'KILLED %%s\n' "$1" >> %q ; }
gc_harness_sweep_stale_orphans %s
`, filepath.Join(repoRoot(t), "scripts", "lib", "harness-reap.sh"), killLog, minAge)

	cmd := harnessBash(script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sweep failed: %v\n%s", err, out)
	}
	kills, readErr := os.ReadFile(killLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read kill log: %v", readErr)
	}
	return string(kills) + string(out)
}

// TestHarnessSweepReapsStrandedRunsButSparesLiveOnes pins the sweep's whole
// safety contract in one table: it must reap what earlier runs stranded, and
// must not touch a live sibling run or anything outside the test-socket
// namespace — above all a developer's own tmux server, per the tmux-safety
// rule in AGENTS.md.
func TestHarnessSweepReapsStrandedRunsButSparesLiveOnes(t *testing.T) {
	liveRun := t.TempDir()
	// A live run: the socket-parent dir exists and its creator (this test
	// process) is alive.
	liveDir := filepath.Join(liveRun, fmt.Sprintf("gct-%d-4242", os.Getpid()))
	if err := os.MkdirAll(filepath.Join(liveDir, "tmux", "tmux-501"), 0o755); err != nil {
		t.Fatalf("create live socket parent: %v", err)
	}

	strandedSocket := filepath.Join(liveRun, "gct-2147480000-99", "tmux", "tmux-501", "test-city")
	liveSocket := filepath.Join(liveDir, "tmux", "tmux-501", "test-city")

	lsof := strings.Join([]string{
		"p4001", "n" + strandedSocket, // run dir already gone -> stranded
		"p4002", "n" + liveSocket, // creator alive -> spare
		"p4003", "n/private/tmp/tmux-501/default", // the developer's own server
		"p4004", "n/var/run/some.sock", // unrelated socket
		"",
	}, "\n")

	got := sweepHarness(t, lsof, "", "60")

	if !strings.Contains(got, "KILLED 4001") {
		t.Errorf("sweep spared a stranded socket holder whose run dir is gone; log:\n%s", got)
	}
	for _, spared := range []string{"KILLED 4002", "KILLED 4003", "KILLED 4004"} {
		if strings.Contains(got, spared) {
			t.Errorf("sweep killed a process it must never touch (%s); log:\n%s", spared, got)
		}
	}
}

// TestHarnessSweepReapsOrphanedTestBinariesByAge covers the kit-k72d
// population: a .test binary reparented to init. Age is the guard that keeps a
// concurrently starting sibling run out of scope.
func TestHarnessSweepReapsOrphanedTestBinariesByAge(t *testing.T) {
	psTable := strings.Join([]string{
		"5001 1 05:34:12 gc.test",       // orphaned and old: the measured shape
		"5002 1 00:00:30 gc.test",       // orphaned but young: a run just starting
		"5003 9182 06:00:00 gc.test",    // old but still parented: a live run
		"5004 1 06:00:00 Google Chrome", // old orphan, not a test binary
		"",
	}, "\n")

	got := sweepHarness(t, "", psTable, "3600")

	if !strings.Contains(got, "KILLED 5001") {
		t.Errorf("sweep spared the orphaned test binary; log:\n%s", got)
	}
	for _, spared := range []string{"KILLED 5002", "KILLED 5003", "KILLED 5004"} {
		if strings.Contains(got, spared) {
			t.Errorf("sweep killed a process it must never touch (%s); log:\n%s", spared, got)
		}
	}
}

// TestHarnessDurationParsingDisarmsRatherThanGuesses pins that the watchdog
// budget is only computed from a duration the parser actually understands: an
// unrecognized spelling must fail, so the runner disarms instead of firing at
// a guessed budget.
func TestHarnessDurationParsingDisarmsRatherThanGuesses(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"20m", "1200"},
		{"90s", "90"},
		{"2h", "7200"},
		{"45", "45"},
		{"1m30s", ""},
		{"", ""},
		{"forever", ""},
	} {
		script := fmt.Sprintf(`source %q; gc_harness_duration_seconds %q || true`,
			filepath.Join(repoRoot(t), "scripts", "lib", "harness-reap.sh"), tc.input)
		out, err := exec.Command("bash", "-c", script).Output()
		if err != nil {
			t.Fatalf("parse %q: %v", tc.input, err)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("gc_harness_duration_seconds(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestGoTestShardLeavesNothingHoldingTheCallersPipe pins that a completed
// shard leaves no descendant holding the caller's stdout. CombinedOutput
// reads until every inheritor of the write end closes it, so a single leaked
// helper — the watchdog's own sleep(1) is the one that got this wrong —
// blocks the caller for the whole remaining watchdog budget even though the
// run itself finished in milliseconds. The armed budget here is far longer
// than the deadline, so a regression hangs rather than merely slows.
func TestGoTestShardLeavesNothingHoldingTheCallersPipe(t *testing.T) {
	fixture := newReapFixture(t, `    exit 0`)

	cmd := exec.Command(filepath.Join(fixture.repoRoot, "scripts", "test-go-test-shard"), "./example", "1", "2")
	cmd.Dir = fixture.repoRoot
	cmd.Env = []string{
		"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + fixture.homeDir,
		"SHELL=/bin/sh",
		"TMPDIR=" + fixture.tmpDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
		// A watchdog armed well past the test's own patience: whatever the
		// runner leaves behind would hold the pipe for this long.
		"GO_TEST_TIMEOUT=1h",
		"GO_TEST_WATCHDOG_GRACE=1h",
	}

	done := make(chan error, 1)
	go func() {
		_, err := cmd.CombinedOutput()
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shard runner failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("reading the shard runner's output blocked after the run finished: a descendant outlived the run holding the caller's pipe")
	}
}

// TestGoTestShardTerminationLeavesNothingHoldingTheCallersPipe is the
// signal-path twin of TestGoTestShardLeavesNothingHoldingTheCallersPipe
// (ga-880tzy). The watchdog lives in a process group of its own, so a runner
// whose signal trap tears down only the run's group leaves the watchdog and
// its sleep(1) behind as orphans: they hold the caller's stdout and the run's
// cwd until the deadline, and on waking signal the run's process-group id,
// which by then the kernel may have handed to an unrelated group. Reading the
// runner's stdout to EOF proves every process that inherited it is gone,
// watchdog included, without having to name any of them.
func TestGoTestShardTerminationLeavesNothingHoldingTheCallersPipe(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "run.pid")
	fixture := newReapFixture(t, fmt.Sprintf(`
    echo $$ > %q
    sleep %d
`, pidFile, fixtureLifetimeSeconds))

	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create runner stdout pipe: %v", err)
	}
	t.Cleanup(func() { _ = stdout.Close() })

	// The 60s budget outlasts this test's patience by a wide margin, so a
	// leaked watchdog is still holding the pipe when the test gives up. It is
	// also short enough that a leaked watchdog wakes long before the kernel
	// could plausibly recycle the run's dead process-group id; it then finds
	// no such group and exits without signaling anything.
	cmd := fixture.startWithOutput(t, stdoutWriter,
		"GO_TEST_TIMEOUT=30s",
		"GO_TEST_WATCHDOG_GRACE=30s",
	)
	// From here on the runner's tree holds the only write ends.
	_ = stdoutWriter.Close()
	waitForPIDFile(t, pidFile, 30*time.Second)

	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal runner: %v", err)
	}

	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, stdout)
		drained <- err
	}()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("read runner stdout: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a process of the terminated runner's tree still holds its stdout 20s later: the runner must take its watchdog down with its run")
	}
}

// TestHarnessSignalDuringLaunchStillTearsDownTheJustForkedJob pins the
// ga-96smfk.61 race. Bash runs a signal trap between any two commands, so a
// signal can land after a supervised run or its watchdog has been forked but
// before the runner has recorded its PID. A teardown that consults only the
// recorded PIDs then finds nothing to end: the just-forked job outlives the
// runner holding the caller's stdout. Under remote-exec load that window was
// wide enough to strand the watchdog, failing
// TestGoTestShardTerminationLeavesNothingHoldingTheCallersPipe. Here a DEBUG
// trap raises the signal at the first command boundary after the run's or the
// watchdog's fork inside gc_harness_run_supervised itself, so the race is
// exercised every run instead of once in a few hundred, through the launch
// the runners use rather than a copy of it.
func TestHarnessSignalDuringLaunchStillTearsDownTheJustForkedJob(t *testing.T) {
	for _, tc := range []struct {
		kind string
		// fork counts gc_harness_run_supervised's background forks up to the
		// one the signal follows: the run is the first, its watchdog the
		// second.
		fork int
	}{
		{kind: "run", fork: 1},
		{kind: "watchdog", fork: 2},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			script := fmt.Sprintf(`
set -euo pipefail
source %q
on_term() {
  printf 'launch window at signal: [%%s]\n' "$gc_harness_launching"
  gc_harness_terminate_supervised 5
  trap - TERM
  kill -TERM $$
}
trap on_term TERM
# Under functrace the DEBUG trap runs before every command, inside functions
# too. It raises the signal before the first command after the script's Nth
# background fork: $! has moved, but nothing has published it yet. A forked
# child starts with $! as it stood before its own fork, so it never sees a
# fork of its own and never raises.
forks=0
seen_pid=""
raise_after_fork() {
  (( forks < %[2]d )) || return 0
  local pid=""
  (( ${#!} == 0 )) || pid="$!"
  [[ "$pid" != "$seen_pid" ]] || return 0
  seen_pid="$pid"
  forks=$(( forks + 1 ))
  if (( forks == %[2]d )); then
    kill -TERM $$
  fi
}
set -o functrace
trap raise_after_fork DEBUG
gc_harness_run_supervised launch-window %[3]d 1 -- sleep %[3]d
`, filepath.Join(repoRoot(t), "scripts", "lib", "harness-reap.sh"), tc.fork, fixtureLifetimeSeconds)

			stdout, stdoutWriter, err := os.Pipe()
			if err != nil {
				t.Fatalf("create stdout pipe: %v", err)
			}
			t.Cleanup(func() { _ = stdout.Close() })

			cmd := harnessBash(script)
			cmd.Stdout = stdoutWriter
			cmd.Stderr = stdoutWriter
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start launch-window script: %v", err)
			}
			_ = stdoutWriter.Close()

			// EOF on stdout proves every process that inherited it is gone,
			// the just-forked job included. Left behind, that job is a sleep
			// outlasting this test, so the read below never returns on its own.
			type drain struct {
				output []byte
				err    error
			}
			drained := make(chan drain, 1)
			go func() {
				output, err := io.ReadAll(stdout)
				drained <- drain{output: output, err: err}
			}()
			var output []byte
			select {
			case got := <-drained:
				if got.err != nil {
					t.Fatalf("read script stdout: %v", got.err)
				}
				output = got.output
			case <-time.After(60 * time.Second):
				t.Fatalf("a %s job forked just before the signal still holds the runner's stdout: teardown must adopt a job whose PID was not yet published", tc.kind)
			}

			// A signal that missed the window would prove nothing about it.
			if want := fmt.Sprintf("launch window at signal: [%s]", tc.kind); !strings.Contains(string(output), want) {
				t.Fatalf("script output lacks %q: the signal did not land between the %s fork and the publish of its PID\n%s", want, tc.kind, output)
			}
			// Re-raising SIGTERM is the trap's last act, so dying of it proves
			// the teardown ran to its end rather than the script failing early.
			err = waitWithin(t, cmd, 10*time.Second)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("launch-window script exit = %v, want death by its trap's re-raised SIGTERM\n%s", err, output)
			}
			if status, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
				t.Fatalf("launch-window script exit = %v, want death by its trap's re-raised SIGTERM\n%s", err, output)
			}
		})
	}
}

// TestHarnessLaunchesUnderNounsetBeforeAnyBackgroundJob pins launch
// bookkeeping to bash 3.2, the oldest bash the runners support. Both runners
// that source harness-reap.sh run under `set -u`, and $! is unset until a
// script forks its first background job. Bash 3.2 aborts on any expansion of
// an unset $!, even a defaulted one such as ${!:-}, where later bashes
// substitute the default, so a read that passes on a modern bash can kill a
// macOS runner at its first launch. The script runs on bash 3.2 when the host
// has one (see findBash32); a host without one still runs the static scan,
// which rejects the defaulted forms that only bash 3.2 refuses.
func TestHarnessLaunchesUnderNounsetBeforeAnyBackgroundJob(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "lib", "harness-reap.sh")
	assertNoDefaultedLastJobPID(t,
		lib,
		filepath.Join(root, "scripts", "test-go-test-shard"),
		filepath.Join(root, "scripts", "test-local-parallel"),
	)

	bashPath := findBash32(t)
	if bashPath == "" {
		bashPath = "bash"
	}
	script := fmt.Sprintf(`
set -euo pipefail
source %q
gc_harness_launch_begin run
gc_harness_launch_publish
gc_harness_launch_begin watchdog
gc_harness_terminate_supervised 1
printf 'published run=[%%s] watchdog=[%%s] window=[%%s]\n' \
  "$gc_harness_supervised_pgid" "$gc_harness_watchdog_pgid" "$gc_harness_launching"
gc_harness_run_supervised nounset 30 1 -- true
echo launched
`, lib)
	out, err := shardTestCommand(bashPath, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("launch before any background job under %s: %v\n%s", bashPath, err, out)
	}
	if want := "published run=[] watchdog=[] window=[]\nlaunched\n"; string(out) != want {
		t.Fatalf("launch before any background job under %s printed:\n%s\nwant:\n%s", bashPath, out, want)
	}
}

// assertNoDefaultedLastJobPID fails for any non-comment line of the scripts at
// paths that expands $! through an operator: ${!:-}, ${!-}, ${!+x} and the
// rest. While $! is unset bash 3.2 aborts on every one of them under `set -u`,
// and later bashes accept them all, so no test on a modern bash notices.
// ${!name} indirection and ${!array[@]} keys are not $! and do not match.
func assertNoDefaultedLastJobPID(t *testing.T, paths ...string) {
	t.Helper()
	defaulted := regexp.MustCompile(`\$\{!:?[-+=?]`)
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for index, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if defaulted.MatchString(line) {
				t.Errorf("%s:%d expands $! through an operator, which aborts bash 3.2 under set -u until a background job exists; read it behind (( ${#!} == 0 )) instead:\n%s", path, index+1, line)
			}
		}
	}
}

// privateGotmpdirPattern is the shape of a runner's private GOTMPDIR:
// gc-gotmp-<creator PID>-<mktemp suffix>.
var privateGotmpdirPattern = regexp.MustCompile(`^gc-gotmp-[0-9]+-[A-Za-z0-9]+$`)

// assertPrivateGotmpdir checks that the product ran with a private GOTMPDIR
// directly under base, and that the runner removed it on the way out.
func assertPrivateGotmpdir(t *testing.T, got, base string) {
	t.Helper()
	if filepath.Dir(got) != base || !privateGotmpdirPattern.MatchString(filepath.Base(got)) {
		t.Fatalf("product GOTMPDIR = %q, want a private gc-gotmp-<pid>-<random> dir directly under %s", got, base)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("private GOTMPDIR %s survived the run (stat err=%v): go's WORK dirs leak with it", got, err)
	}
}

// TestGoTestShardRemovesItsGotmpdirWhenTheRunEnds covers the WORK-dir leak:
// go removes its go-build WORK dir on a normal exit or SIGINT but not on
// SIGTERM or SIGKILL, so every externally killed run left up to ~1 GB per
// go process in /var/tmp. The runner gives go a private GOTMPDIR and removes
// it whole, whether the run finishes or the runner is terminated.
func TestGoTestShardRemovesItsGotmpdirWhenTheRunEnds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		terminate bool
	}{
		{name: "run finishes", terminate: false},
		{name: "runner terminated", terminate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordDir := t.TempDir()
			recorded := filepath.Join(recordDir, "gotmpdir")
			pidFile := filepath.Join(recordDir, "run.pid")
			tail := "    exit 0"
			if tc.terminate {
				tail = fmt.Sprintf("    sleep %d", fixtureLifetimeSeconds)
			}
			// The body stands in for go: it builds into a WORK dir under its
			// GOTMPDIR, as a test binary compile does, then records the path.
			fixture := newReapFixture(t, fmt.Sprintf(`
    mkdir -p "$GOTMPDIR/go-build1/b001"
    : > "$GOTMPDIR/go-build1/b001/example.test"
    printf '%%s\n' "$GOTMPDIR" > %q
    echo $$ > %q
%s
`, recorded, pidFile, tail))

			cmd := fixture.start(t)
			if tc.terminate {
				waitForPIDFile(t, pidFile, 20*time.Second)
				if err := syscall.Kill(cmd.Process.Pid, syscall.SIGTERM); err != nil {
					t.Fatalf("signal runner: %v", err)
				}
			}
			_ = waitWithin(t, cmd, 30*time.Second)

			gotmpdir, err := os.ReadFile(recorded)
			if err != nil {
				t.Fatalf("the run never recorded its GOTMPDIR: %v\n%s", err, fixture.output())
			}
			assertPrivateGotmpdir(t, strings.TrimSpace(string(gotmpdir)), fixture.tmpDir)
		})
	}
}

// TestHarnessGotmpdirSweepRemovesOnlyAbandonedDirs covers the runs that die
// too hard to run their own traps (SIGKILL of the whole tree): a later run
// sweeps their private GOTMPDIRs, and must never take one that may still be
// in use.
func TestHarnessGotmpdirSweepRemovesOnlyAbandonedDirs(t *testing.T) {
	base := t.TempDir()

	// Above any kernel's pid_max, so it can never name a live creator.
	const deadPID = 2147480000

	old := time.Now().Add(-2 * time.Hour)
	makeDir := func(name string, recentFile bool) string {
		dir := filepath.Join(base, name)
		work := filepath.Join(dir, "go-build1", "b001")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatalf("create %s: %v", work, err)
		}
		binary := filepath.Join(work, "example.test")
		if err := os.WriteFile(binary, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", binary, err)
		}
		for _, p := range []string{binary, work, filepath.Dir(work), dir} {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatalf("age %s: %v", p, err)
			}
		}
		if recentFile {
			if err := os.Chtimes(binary, time.Now(), time.Now()); err != nil {
				t.Fatalf("touch %s: %v", binary, err)
			}
		}
		return dir
	}

	abandoned := makeDir(fmt.Sprintf("gc-gotmp-%d-aaaaaa", deadPID), false)
	live := makeDir(fmt.Sprintf("gc-gotmp-%d-bbbbbb", os.Getpid()), false)
	stillWriting := makeDir(fmt.Sprintf("gc-gotmp-%d-cccccc", deadPID), true)
	held := makeDir(fmt.Sprintf("gc-gotmp-%d-dddddd", deadPID), false)
	foreign := makeDir("go-build123456", false)

	// lsof reports a holder only for the held dir: a running test binary
	// whose runner died.
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "lsof"), `#!/bin/sh
for arg in "$@"; do last="$arg"; done
case "$last" in
  *-dddddd) echo 4242; exit 0 ;;
esac
exit 1
`)

	script := fmt.Sprintf(`
set -euo pipefail
source %q
gc_harness_sweep_stale_gotmpdirs %q 3600
`, filepath.Join(repoRoot(t), "scripts", "lib", "harness-reap.sh"), base)
	cmd := harnessBash(script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sweep failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("sweep kept %s, whose creator is gone and which nothing touched or holds; output:\n%s", abandoned, out)
	}
	for _, kept := range []string{live, stillWriting, held, foreign} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("sweep removed %s, which it must never touch: %v; output:\n%s", kept, err, out)
		}
	}
}

// TestHarnessGotmpdirIsOwnedByTheOutermostRunner pins the nesting rule: a
// runner whose GOTMPDIR is already a private dir shares it and never removes
// it, while a runner that creates one removes it.
func TestHarnessGotmpdirIsOwnedByTheOutermostRunner(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "gc-gotmp-1-outerX")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatalf("create outer dir: %v", err)
	}
	script := fmt.Sprintf(`
set -euo pipefail
source %q
gc_harness_own_gotmpdir %q 3600
printf 'shared=%%s owned=%%s\n' "$gc_harness_gotmpdir" "$gc_harness_gotmpdir_owned"
gc_harness_remove_gotmpdir
gc_harness_own_gotmpdir %q 3600
printf 'created=%%s owned=%%s\n' "$gc_harness_gotmpdir" "$gc_harness_gotmpdir_owned"
[ -d "$gc_harness_gotmpdir" ]
gc_harness_remove_gotmpdir
[ ! -e "$gc_harness_gotmpdir" ]
`, filepath.Join(repoRoot(t), "scripts", "lib", "harness-reap.sh"), outer, base)
	out, err := harnessBash(script).CombinedOutput()
	if err != nil {
		t.Fatalf("own/remove cycle failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "shared="+outer+" owned=0") {
		t.Errorf("a nested runner did not share the outer runner's dir; output:\n%s", out)
	}
	if _, err := os.Stat(outer); err != nil {
		t.Errorf("a nested runner removed the outer runner's dir: %v", err)
	}
	m := regexp.MustCompile(`created=(\S+) owned=1`).FindStringSubmatch(string(out))
	if m == nil || filepath.Dir(m[1]) != base || !privateGotmpdirPattern.MatchString(filepath.Base(m[1])) {
		t.Errorf("an outermost runner did not create its own private dir under %s; output:\n%s", base, out)
	}
}
