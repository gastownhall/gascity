package scripts_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// signal the runner alone, exactly as a dying parent chain would.
func (f *reapFixture) start(t *testing.T, extraEnv ...string) *exec.Cmd {
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

	f.logPath = filepath.Join(f.tmpDir, "runner.log")
	logFile, err := os.Create(f.logPath)
	if err != nil {
		t.Fatalf("create runner log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	cmd.Stdout = logFile
	cmd.Stderr = logFile

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

	cmd := exec.Command("bash", "-c", script)
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
