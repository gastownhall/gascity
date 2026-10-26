// Package dolt_test validates that runtime.sh's run_bounded helper
// honors its own documented contract (SIGTERM, brief grace period,
// then SIGKILL) on every fallback path — including the python3
// fallback used when neither timeout nor gtimeout is on PATH, which
// previously escalated straight to SIGKILL. See gascity#4823: the
// mismatch let a bounded `dolt backup sync` be killed without any
// chance to run its own signal handler, leaking unreferenced backup
// archives (dolt has no prune verb).
package dolt_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runRunBoundedUnderPython3Fallback sources runtime.sh with a PATH
// that exposes only python3 (no timeout/gtimeout), so run_bounded is
// forced onto the fallback branch under test, then invokes
// `run_bounded 1 python3 childScript` against the given child script.
//
// The child is itself a python3 script (not a shell script wrapping
// a blocking `sleep`) because a shell blocked in wait() on a
// foreground child defers pending trap handlers until that child
// returns — an artifact of shell signal delivery, not of run_bounded.
// A single process installing its own Python signal handler mirrors
// how a real target like `dolt` receives and reacts to signals.
func runRunBoundedUnderPython3Fallback(t *testing.T, childScript string, extraEnv ...string) (int, string) {
	t.Helper()

	python3Path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed; cannot exercise run_bounded's python3 fallback")
	}
	bin := t.TempDir()
	if err := os.Symlink(python3Path, filepath.Join(bin, "python3")); err != nil {
		t.Fatalf("symlink python3: %v", err)
	}
	hostSh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("LookPath(sh): %v", err)
	}
	if err := os.Symlink(hostSh, filepath.Join(bin, "sh")); err != nil {
		t.Fatalf("symlink sh: %v", err)
	}

	root := repoRoot(t)
	cityPath := t.TempDir()
	cmd := exec.Command("sh", "-c",
		`. "$GC_PACK_DIR/assets/scripts/runtime.sh"; run_bounded 1 python3 `+shellQuote(childScript))
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_PORT=4406",
		"PATH="+bin,
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("running run_bounded: %v\noutput:\n%s", err, out)
	return 0, ""
}

// TestRunBoundedPython3FallbackSendsSigtermBeforeKill is the
// regression guard for gascity#4823: on timeout, the python3 fallback
// must give the child a chance to catch SIGTERM and exit gracefully,
// not jump straight to SIGKILL. The child here installs a SIGTERM
// handler and writes a marker file from it; under the pre-fix
// `subprocess.run(..., timeout=...)` implementation (bare
// `process.kill()` on expiry) the handler never runs and the marker
// is never written.
// The 1s bound below is production input to run_bounded, not this test's
// own hang-detector timer (TESTING.md: a timeout the test feeds INTO the
// code under test is never migrated to a hang budget), so the blanket
// >=10s "Test deadline rule" for timers racing an exec start does not
// apply to it — enlarging it would hide the very race gascity#4823 exists
// to catch. run_bounded starts its timer at Popen, so python3 interpreter
// startup is counted against the bound, and a starved host can starve the
// child before signal.signal ever runs. The READY marker, written
// immediately after the handler is installed and before the child
// sleeps, is what keeps the assertion honest instead: an attempt where
// READY never appears proved nothing about the SIGTERM contract (the
// child never reached the handler within the bound) and is retried
// rather than treated as a regression.
func TestRunBoundedPython3FallbackSendsSigtermBeforeKill(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "sigterm-received")
	ready := filepath.Join(dir, "ready")
	child := filepath.Join(dir, "child.py")
	writeExecutable(t, child, `import os
import signal
import sys
import time


def handler(signum, frame):
    with open(os.environ["MARKER_FILE"], "w") as f:
        f.write("caught\n")
    sys.exit(0)


signal.signal(signal.SIGTERM, handler)
with open(os.environ["READY_MARKER"], "w") as f:
    f.write("ready\n")
time.sleep(10)
`)

	const maxAttempts = 3
	var exitCode int
	var out string
	reachedReady := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_ = os.Remove(marker)
		_ = os.Remove(ready)
		exitCode, out = runRunBoundedUnderPython3Fallback(t, child, "MARKER_FILE="+marker, "READY_MARKER="+ready)
		if _, err := os.Stat(ready); err == nil {
			reachedReady = true
			break
		}
	}
	if !reachedReady {
		t.Fatalf("child never reached its SIGTERM handler installation within the bound after %d attempts (readiness marker never appeared); likely host load starved python3 interpreter startup, not a SIGTERM regression\noutput:\n%s", maxAttempts, out)
	}

	if exitCode != 124 {
		t.Fatalf("run_bounded exit code = %d, want 124 (timeout)\noutput:\n%s", exitCode, out)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("child was ready and still never received SIGTERM (marker file missing): %v\noutput:\n%s", err, out)
	}
	if string(got) != "caught\n" {
		t.Fatalf("marker file contents = %q, want %q", got, "caught\n")
	}
}

// TestRunBoundedPython3FallbackEscalatesToSigkillAfterGrace confirms
// the other half of the contract still holds: a child that ignores
// SIGTERM is killed shortly after the grace period, not left running
// forever.
func TestRunBoundedPython3FallbackEscalatesToSigkillAfterGrace(t *testing.T) {
	dir := t.TempDir()
	doneMarker := filepath.Join(dir, "still-running")
	child := filepath.Join(dir, "child.py")
	writeExecutable(t, child, `import os
import signal
import time

signal.signal(signal.SIGTERM, signal.SIG_IGN)
with open(os.environ["DONE_MARKER"], "w") as f:
    f.write("started\n")
time.sleep(30)
`)

	start := time.Now()
	exitCode, out := runRunBoundedUnderPython3Fallback(t, child, "DONE_MARKER="+doneMarker)
	elapsed := time.Since(start)

	if exitCode != 124 {
		t.Fatalf("run_bounded exit code = %d, want 124 (timeout)\noutput:\n%s", exitCode, out)
	}
	if _, err := os.ReadFile(doneMarker); err != nil {
		t.Fatalf("child never started: %v\noutput:\n%s", err, out)
	}
	// 1s timeout + up to 2s grace; allow generous scheduling slack
	// while still proving the child didn't run the full 30s sleep.
	if elapsed > 10*time.Second {
		t.Fatalf("run_bounded took %s to return; expected escalation to SIGKILL well under 10s", elapsed)
	}
}

// TestRunBoundedPython3FallbackPassesThroughOutputAndExitCode covers the
// non-timeout path: the rewrite swapped buffered capture for inherited
// fds, and on stock macOS this fallback is run_bounded's only
// implementation, so a child that exits normally must still have its
// output and exit status reach the caller.
func TestRunBoundedPython3FallbackPassesThroughOutputAndExitCode(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child.py")
	writeExecutable(t, child, `import sys

sys.stdout.write("to-stdout\n")
sys.stderr.write("to-stderr\n")
sys.exit(3)
`)

	exitCode, out := runRunBoundedUnderPython3Fallback(t, child)

	if exitCode != 3 {
		t.Fatalf("run_bounded exit code = %d, want 3 (child's own status)\noutput:\n%s", exitCode, out)
	}
	if !strings.Contains(out, "to-stdout") || !strings.Contains(out, "to-stderr") {
		t.Fatalf("child output not passed through; got:\n%s", out)
	}
}

// skipWithoutProcNet skips a test that needs the real kernel's
// /proc/net/tcp listener table (absent on macOS).
func skipWithoutProcNet(t *testing.T) {
	t.Helper()
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		t.Skipf("/proc/net/tcp unreadable (%v); the procfs listener probe does not apply", err)
	}
	_ = f.Close()
}

// listenLocal opens a TCP listener on 127.0.0.1 for the test process and
// returns its port.
func listenLocal(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().(*net.TCPAddr).Port
}

// writeRecordingLsof installs a PATH lsof shim in binDir that appends
// its argv to the returned marker file on every invocation and prints
// output. The marker is absent until the shim runs.
func writeRecordingLsof(t *testing.T, binDir, output string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "lsof-invocations")
	writeExecutable(t, filepath.Join(binDir, "lsof"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+shellQuote(marker)+"\nprintf '%s' "+shellQuote(output)+"\n")
	return marker
}

// assertLsofNeverRan fails when the recording lsof shim was executed.
func assertLsofNeverRan(t *testing.T, marker string) {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err == nil {
		t.Fatalf("lsof shim ran, want the procfs probe to answer without lsof; invocations:\n%s", data)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading lsof marker: %v", err)
	}
}

// runtimeShellEnv returns the environment for sourcing runtime.sh from a
// fresh city with binDir ahead of the host PATH, so find, awk, sort and ps
// stay visible to the probe.
func runtimeShellEnv(t *testing.T, cityPath, binDir string, extra ...string) []string {
	t.Helper()
	env := append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+repoRoot(t),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return append(env, extra...)
}

// runRuntimeShell sources runtime.sh in sh, then runs script, and returns
// its stdout.
func runRuntimeShell(t *testing.T, env []string, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; `+script)
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v\nstdout:\n%s\nstderr:\n%s", script, err, out, stderr.String())
	}
	return string(out)
}

// reapedPID returns the PID of a child process that has already exited
// and been reaped, so pid_is_running reports it as not running.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running true: %v", err)
	}
	return cmd.ProcessState.Pid()
}

// TestManagedRuntimeListenerPIDReadsProcNotLsof is the regression guard
// for gascity#7553: on a stalled FUSE mount lsof blocks in uninterruptible
// sleep before it filters sockets, so on Linux the listener holder must
// come from /proc/net/tcp and fd link text without ever running lsof.
func TestManagedRuntimeListenerPIDReadsProcNotLsof(t *testing.T) {
	testManagedRuntimeListenerPIDReadsProc(t)
}

// TestManagedRuntimeListenerPIDDoesNotHangOnStalledMount is the triage
// name for the same guard.
func TestManagedRuntimeListenerPIDDoesNotHangOnStalledMount(t *testing.T) {
	testManagedRuntimeListenerPIDReadsProc(t)
}

func testManagedRuntimeListenerPIDReadsProc(t *testing.T) {
	skipWithoutProcNet(t)
	port := listenLocal(t)
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "1\n")

	env := runtimeShellEnv(t, t.TempDir(), binDir, "GC_DOLT_PORT=4406")
	got := strings.TrimSpace(runRuntimeShell(t, env, "managed_runtime_listener_pid "+strconv.Itoa(port)))
	if want := strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("managed_runtime_listener_pid %d = %q, want the listening test process %s", port, got, want)
	}
	assertLsofNeverRan(t, marker)
}

func TestManagedRuntimeListenerPIDFindsIPv6Holder(t *testing.T) {
	skipWithoutProcNet(t)
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "1\n")

	env := runtimeShellEnv(t, t.TempDir(), binDir, "GC_DOLT_PORT=4406")
	got := strings.TrimSpace(runRuntimeShell(t, env, "managed_runtime_listener_pid "+strconv.Itoa(port)))
	if want := strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("managed_runtime_listener_pid %d = %q, want the listening test process %s", port, got, want)
	}
	assertLsofNeverRan(t, marker)
}

func TestManagedRuntimeListenerPIDIgnoresBadPort(t *testing.T) {
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "1\n")
	env := runtimeShellEnv(t, t.TempDir(), binDir, "GC_DOLT_PORT=4406", "GC_DOLT_PROC_ROOT="+t.TempDir())
	for _, port := range []string{"''", "abc", "33x7"} {
		if got := runRuntimeShell(t, env, "managed_runtime_listener_pid "+port); got != "" {
			t.Fatalf("managed_runtime_listener_pid %s = %q, want nothing", port, got)
		}
	}
	assertLsofNeverRan(t, marker)
}

// TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcNet covers hosts
// without procfs listener tables (macOS): the holder comes from lsof as
// before, skipping holders that are no longer running.
func TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcNet(t *testing.T) {
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, fmt.Sprintf("%d\n1\n", reapedPID(t)))

	env := runtimeShellEnv(t, t.TempDir(), binDir, "GC_DOLT_PORT=4406", "GC_DOLT_PROC_ROOT="+t.TempDir())
	if got := strings.TrimSpace(runRuntimeShell(t, env, "managed_runtime_listener_pid 4407")); got != "1" {
		t.Fatalf("managed_runtime_listener_pid 4407 = %q, want the first running lsof holder 1", got)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("lsof shim never ran: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), "-nP -t -iTCP:4407 -sTCP:LISTEN"; got != want {
		t.Fatalf("lsof argv = %q, want %q", got, want)
	}
}

// procNetTCPFixture mirrors internal/pidutil/listener_test.go: listeners on
// 127.0.0.1:3307 (inode 4242001) and 0.0.0.0:22 (inode 17), an ESTABLISHED
// row on 3307 (inode 4242002), a TIME_WAIT row with inode 0, and a
// truncated row.
const procNetTCPFixture = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0CEB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242001 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 17 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0CEB 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 4242002 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:0CEB 0100007F:D432 06 00000000:00000000 03:00000F9E 00000000     0        0 0 3 0000000000000000
   4: 0100007F:0CEB 00000000:0000 0A
`

// procNetTCP6Fixture holds a [::]:3307 listener (inode 4242003) and a row
// whose port is not hex (inode 4242004).
const procNetTCP6Fixture = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0CEB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242003 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:ZZZZ 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242004 1 0000000000000000 100 0 0 10 0
`

// writeProcFixture builds a fake procfs root with the fixture listener
// tables and, for each pid, fd entries that are dangling symlinks to
// socket:[inode], the link text the kernel prints for a socket fd.
func writeProcFixture(t *testing.T, fds map[int][]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(procNetTCPFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp6"), []byte(procNetTCP6Fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	for pid, inodes := range fds {
		fdDir := filepath.Join(root, strconv.Itoa(pid), "fd")
		if err := os.MkdirAll(fdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i, inode := range inodes {
			if err := os.Symlink("socket:["+inode+"]", filepath.Join(fdDir, strconv.Itoa(i+3))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestManagedRuntimeListenerPIDsReadsFixtureTables(t *testing.T) {
	procRoot := writeProcFixture(t, map[int][]string{
		700: {"4242001", "4242001"},
		500: {"4242003"},
		800: {"4242002"},
		900: {"17"},
		950: {"4242004"},
	})
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "800\n900\n")

	env := runtimeShellEnv(t, t.TempDir(), binDir, "GC_DOLT_PORT=4406", "GC_DOLT_PROC_ROOT="+procRoot)
	if got, want := runRuntimeShell(t, env, "managed_runtime_listener_pids 3307"), "500\n700\n"; got != want {
		t.Fatalf("managed_runtime_listener_pids 3307 = %q, want %q (LISTEN holders only, sorted, once each)", got, want)
	}
	if got := runRuntimeShell(t, env, "managed_runtime_listener_pids 3308"); got != "" {
		t.Fatalf("managed_runtime_listener_pids 3308 = %q, want nothing for a port with no LISTEN row", got)
	}
	assertLsofNeverRan(t, marker)
}

func TestManagedRuntimeListenerPIDSkipsHoldersNotRunning(t *testing.T) {
	dead := reapedPID(t)
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "1\n")

	live := writeProcFixture(t, map[int][]string{dead: {"4242001"}, os.Getpid(): {"4242003"}})
	env := runtimeShellEnv(t, t.TempDir(), binDir, "GC_DOLT_PORT=4406", "GC_DOLT_PROC_ROOT="+live)
	if got, want := strings.TrimSpace(runRuntimeShell(t, env, "managed_runtime_listener_pid 3307")), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("managed_runtime_listener_pid 3307 = %q, want the running holder %s", got, want)
	}

	deadOnly := writeProcFixture(t, map[int][]string{dead: {"4242001"}})
	env = runtimeShellEnv(t, t.TempDir(), binDir, "GC_DOLT_PORT=4406", "GC_DOLT_PROC_ROOT="+deadOnly)
	if got := runRuntimeShell(t, env, "managed_runtime_listener_pid 3307"); got != "" {
		t.Fatalf("managed_runtime_listener_pid 3307 = %q, want nothing when every holder has exited", got)
	}
	assertLsofNeverRan(t, marker)
}

// TestRuntimeScriptPortResolutionFindsHolderWithoutLsof covers source-time
// port resolution: live managed state whose PID holds the port resolves
// GC_DOLT_PORT from the procfs probe, never running lsof.
func TestRuntimeScriptPortResolutionFindsHolderWithoutLsof(t *testing.T) {
	skipWithoutProcNet(t)
	port := listenLocal(t)
	cityPath := t.TempDir()
	writeManagedRuntimeStateForScript(t, cityPath, port)
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "1\n")

	got := strings.TrimSpace(runRuntimeShell(t, runtimeShellEnv(t, cityPath, binDir), `printf '%s\n' "$GC_DOLT_PORT"`))
	if want := strconv.Itoa(port); got != want {
		t.Fatalf("GC_DOLT_PORT = %q, want managed port %s", got, want)
	}
	assertLsofNeverRan(t, marker)
}

func TestRuntimeScriptPortResolutionRejectsForeignHolder(t *testing.T) {
	skipWithoutProcNet(t)
	port := listenLocal(t)
	cityPath := t.TempDir()
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, 1)
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "1\n")

	cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
	cmd.Env = runtimeShellEnv(t, cityPath, binDir)
	out, err := cmd.CombinedOutput()
	assertRuntimePortExit78(t, err, out, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), cityPath)
	assertLsofNeverRan(t, marker)
}

func TestRuntimeScriptPortResolutionWithoutListenRowUsesReachability(t *testing.T) {
	skipWithoutProcNet(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, tt := range []struct {
		name      string
		reachable bool
	}{
		{name: "reachable port accepted", reachable: true},
		{name: "unreachable port rejected", reachable: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeManagedRuntimeStateForScript(t, cityPath, port)
			binDir := t.TempDir()
			marker := writeRecordingLsof(t, binDir, "1\n")
			ncExit := "1"
			if tt.reachable {
				ncExit = "0"
			}
			writeExecutable(t, filepath.Join(binDir, "nc"), "#!/bin/sh\nexit "+ncExit+"\n")

			cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
			cmd.Env = runtimeShellEnv(t, cityPath, binDir)
			out, err := cmd.CombinedOutput()
			if tt.reachable {
				if err != nil {
					t.Fatalf("runtime.sh failed: %v\n%s", err, out)
				}
				if got, want := strings.TrimSpace(string(out)), strconv.Itoa(port); got != want {
					t.Fatalf("GC_DOLT_PORT = %q, want %s", got, want)
				}
			} else {
				assertRuntimePortExit78(t, err, out, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), cityPath)
			}
			assertLsofNeverRan(t, marker)
		})
	}
}

func TestHealthScriptReportsServerPIDWithoutLsof(t *testing.T) {
	skipWithoutProcNet(t)
	port := listenLocal(t)
	binDir := t.TempDir()
	marker := writeRecordingLsof(t, binDir, "1\n")
	writeExecutable(t, filepath.Join(binDir, "dolt"), "#!/bin/sh\nexit 0\n")

	env := append(runtimeShellEnv(t, t.TempDir(), binDir),
		"GC_DOLT_PORT="+strconv.Itoa(port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
	)
	out, err := newHealthScriptCmd(repoRoot(t), env, "--json").Output()
	if err != nil {
		t.Fatalf("health --json: %v\n%s", err, out)
	}
	var report struct {
		Server struct {
			PID int `json:"pid"`
		} `json:"server"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parsing health JSON: %v\n%s", err, out)
	}
	if report.Server.PID != os.Getpid() {
		t.Fatalf("server.pid = %d, want the listening test process %d\n%s", report.Server.PID, os.Getpid(), out)
	}
	assertLsofNeverRan(t, marker)
}
