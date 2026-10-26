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

// skipUnlessProcNetTCPReadable skips a test that drives runtime.sh's
// listener probe against the kernel's socket tables when they are not
// readable (no procfs): there the probe answers via lsof instead, so the
// kernel-table assertions cannot hold.
func skipUnlessProcNetTCPReadable(t *testing.T) {
	t.Helper()
	if _, err := os.ReadFile("/proc/net/tcp"); err != nil {
		t.Skip("/proc/net/tcp unreadable; the listener probe's kernel-table path is Linux-only")
	}
}

// runRuntimeScriptShell sources runtime.sh in a fresh /bin/sh and then runs
// body, returning the shell's exit code and combined output. binDir is
// prepended to PATH so test shims win while the host tools the probe itself
// needs (find, awk, sort, ps) stay visible; cityPath provides GC_CITY_PATH.
func runRuntimeScriptShell(t *testing.T, cityPath, binDir, body string, extraEnv ...string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; `+body)
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "PATH", "LSOF_MARKER"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
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
	t.Fatalf("running runtime.sh shell: %v\noutput:\n%s", err, out)
	return 0, ""
}

// writeLsofRecordingShim installs an lsof shim into binDir that records each
// invocation at $LSOF_MARKER and prints PID 1 — running as far as
// pid_is_running's ps fallback is concerned, and never the holder a
// kernel-table probe is expected to find.
func writeLsofRecordingShim(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "lsof"), `#!/bin/sh
touch "${LSOF_MARKER:?}"
printf '1\n'
`)
}

// assertListenerPIDAnsweredFromProc holds the AC-1 contract of the
// gascity#7553 fix: with readable kernel socket tables,
// managed_runtime_listener_pid must print the listening process's PID from
// those tables and must never execute lsof, whose unfiltered /proc walk
// parks it in uninterruptible sleep when any FUSE mount on the host stalls
// (leaving dolt orders to time out with unkillable leftover processes).
func assertListenerPIDAnsweredFromProc(t *testing.T) {
	t.Helper()
	skipUnlessProcNetTCPReadable(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	want := strconv.Itoa(os.Getpid())

	bin := t.TempDir()
	marker := filepath.Join(bin, "lsof-ran")
	writeLsofRecordingShim(t, bin)

	code, out := runRuntimeScriptShell(t, t.TempDir(), bin,
		"managed_runtime_listener_pid "+strconv.Itoa(port),
		"GC_DOLT_PORT=4406",
		"LSOF_MARKER="+marker,
	)
	if code != 0 {
		t.Fatalf("managed_runtime_listener_pid exited %d\n%s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("lsof ran; with readable kernel socket tables the listener probe must answer without it (gascity#7553)\noutput:\n%s", out)
	}
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("managed_runtime_listener_pid = %q, want %q (the process holding the listener)\noutput:\n%s", got, want, out)
	}
}

// TestManagedRuntimeListenerPIDReadsProcNotLsof is AC-1 of the gascity#7553
// fix: the listener probe answers from the kernel socket tables, not lsof.
func TestManagedRuntimeListenerPIDReadsProcNotLsof(t *testing.T) {
	assertListenerPIDAnsweredFromProc(t)
}

// TestManagedRuntimeListenerPIDDoesNotHangOnStalledMount is the recorded
// repro name for gascity#7553 and holds the same contract as
// TestManagedRuntimeListenerPIDReadsProcNotLsof: an lsof-backed probe hangs
// on a stalled FUSE mount, so the holder lookup must never reach lsof while
// the kernel socket tables are readable.
func TestManagedRuntimeListenerPIDDoesNotHangOnStalledMount(t *testing.T) {
	assertListenerPIDAnsweredFromProc(t)
}

// TestRuntimeScriptPortResolutionFindsHolderWithoutLsof is AC-2 of the
// gascity#7553 fix: sourcing runtime.sh with managed state recording the
// test process as the live holder resolves GC_DOLT_PORT from the kernel
// socket tables without running lsof.
func TestRuntimeScriptPortResolutionFindsHolderWithoutLsof(t *testing.T) {
	skipUnlessProcNetTCPReadable(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	cityPath := t.TempDir()
	bin := t.TempDir()
	marker := filepath.Join(bin, "lsof-ran")
	writeLsofRecordingShim(t, bin)
	writeManagedRuntimeStateForScript(t, cityPath, port)

	code, out := runRuntimeScriptShell(t, cityPath, bin,
		`printf '%s\n' "$GC_DOLT_PORT"`,
		"LSOF_MARKER="+marker,
	)
	if code != 0 {
		t.Fatalf("runtime.sh exited %d; managed state names this process as the live holder, so the port must resolve\n%s", code, out)
	}
	if got, want := strings.TrimSpace(out), strconv.Itoa(port); got != want {
		t.Fatalf("GC_DOLT_PORT = %q, want %q\n%s", got, want, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("lsof ran during port resolution; the holder must come from the kernel socket tables (gascity#7553)\n%s", out)
	}
}

// TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcNet is AC-3 of the
// gascity#7553 fix: when no kernel socket table is readable (no procfs),
// managed_runtime_listener_pid keeps answering with lsof, exactly as before:
// the same argv, and the first holder lsof reports that is still running.
func TestManagedRuntimeListenerPIDFallsBackToLsofWithoutProcNet(t *testing.T) {
	dead := reapedChildPID(t)
	if out, err := exec.Command("ps", "-p", strconv.Itoa(dead), "-o", "pid=").Output(); err == nil && len(strings.Fields(string(out))) > 0 {
		t.Skipf("PID %d was reused before the shim could report it as a dead holder", dead)
	}

	bin := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "lsof-argv")
	writeExecutable(t, filepath.Join(bin, "lsof"), fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nprintf '%d\\n1\\n'\n", shellQuote(argvLog), dead))

	code, out := runRuntimeScriptShell(t, t.TempDir(), bin,
		"managed_runtime_listener_pid 4407",
		"GC_DOLT_PORT=4406",
		"GC_DOLT_PROC_ROOT="+t.TempDir(),
	)
	if code != 0 {
		t.Fatalf("managed_runtime_listener_pid exited %d\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != "1" {
		t.Fatalf("managed_runtime_listener_pid = %q, want 1 (the first running lsof holder, after the reaped %d)\n%s", got, dead, out)
	}
	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("lsof shim never ran: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), "-nP -t -iTCP:4407 -sTCP:LISTEN"; got != want {
		t.Fatalf("lsof argv = %q, want %q", got, want)
	}
}

// listenerProbeTCPFixture and listenerProbeTCP6Fixture mirror the kernel's
// /proc/net/tcp and /proc/net/tcp6 tables: a 127.0.0.1:3307 listener (0CEB,
// inode 4242001), a port-22 listener (inode 17), an ESTABLISHED and a
// TIME_WAIT row on 3307 (inodes 4242002 and 0), a truncated row, an IPv6
// [::]:3307 listener (inode 4242003), and a row whose port field is not hex
// (ZZZZ, inode 4242004). Only the two 3307 listeners may contribute holders.
const listenerProbeTCPFixture = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0CEB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242001 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 17 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0CEB 0100007F:C7FD 01 00000000:00000000 00:00000000 00000000  1000        0 4242002 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:0CEB 0100007F:C7FE 06 00000000:00000000 03:00000F9E 00000000     0        0 0 3 0000000000000000
   4: 0100007F:0CEB 00000000:0000 0A
`

const listenerProbeTCP6Fixture = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0CEB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242003 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:ZZZZ 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242004 1 0000000000000000 100 0 0 10 0
`

// writeProcFixture builds the GC_DOLT_PROC_ROOT shape the listener probe
// reads: kernel socket tables under net/, plus per-PID fd directories whose
// entries are dangling symlinks named socket:[inode] — the same text the
// kernel renders for real descriptors, which find's -lname matches without
// ever statting the (nonexistent) target. holders maps "<pid>/fd/<n>" to the
// socket inode that entry claims.
func writeProcFixture(t *testing.T, tcp, tcp6 string, holders map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatalf("mkdir net: %v", err)
	}
	for name, body := range map[string]string{"tcp": tcp, "tcp6": tcp6} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(root, "net", name), []byte(body), 0o644); err != nil {
			t.Fatalf("write net/%s: %v", name, err)
		}
	}
	for fd, inode := range holders {
		dir := filepath.Join(root, filepath.Dir(fd))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.Symlink("socket:["+inode+"]", filepath.Join(root, fd)); err != nil {
			t.Fatalf("symlink %s -> socket:[%s]: %v", fd, inode, err)
		}
	}
	return root
}

// TestManagedRuntimeListenerPIDsFixture pins the table-reading rules against
// a fixed kernel-table fixture: both the IPv4 and the IPv6 listener on 3307
// are found, a holder with two descriptors on the same socket is listed
// once, and ESTABLISHED sockets, other ports, zero inodes, truncated rows,
// and non-hex port fields never contribute a holder.
func TestManagedRuntimeListenerPIDsFixture(t *testing.T) {
	root := writeProcFixture(t, listenerProbeTCPFixture, listenerProbeTCP6Fixture, map[string]string{
		"101/fd/3": "4242001",
		"101/fd/4": "4242001", // second fd on the same socket: 101 still listed once
		"202/fd/5": "4242003", // the IPv6 listener's holder
		"303/fd/6": "4242002", // ESTABLISHED on 3307: not a listener
		"404/fd/7": "17",      // the port-22 listener
		"505/fd/8": "4242004", // the non-hex ZZZZ-port row
	})

	code, out := runRuntimeScriptShell(t, t.TempDir(), t.TempDir(),
		"managed_runtime_listener_pids 3307",
		"GC_DOLT_PORT=4406",
		"GC_DOLT_PROC_ROOT="+root,
	)
	if code != 0 {
		t.Fatalf("managed_runtime_listener_pids exited %d\n%s", code, out)
	}
	if got, want := strings.TrimSpace(out), "101\n202"; got != want {
		t.Fatalf("managed_runtime_listener_pids = %q, want %q (the IPv4 and IPv6 holders, ascending, deduplicated)\n%s", got, want, out)
	}
}

// reapedChildPID returns the PID of a child that has already exited and been
// waited for — gone from the process table the way pid_is_running checks it.
// The PID can be reused the moment it is freed; callers that need it to stay
// dead must verify (and skip on reuse).
func reapedChildPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run reaped child: %v", err)
	}
	return cmd.ProcessState.Pid()
}

// TestManagedRuntimeListenerPIDSkipsHoldersThatAreNotRunning pins the
// running filter: the singular probe answers only with a PID that is alive,
// so a holder that has since exited is never mistaken for the server — and
// when every holder is dead there is no answer at all.
func TestManagedRuntimeListenerPIDSkipsHoldersThatAreNotRunning(t *testing.T) {
	live := os.Getpid()
	dead := reapedChildPID(t)
	if out, err := exec.Command("ps", "-p", strconv.Itoa(dead), "-o", "pid=").Output(); err == nil && len(strings.Fields(string(out))) > 0 {
		t.Skipf("PID %d was reused before the fixture could use it as a dead holder", dead)
	}

	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:0CEB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242001 1 0000000000000000 100 0 0 10 0\n" +
		"   1: 0100007F:0CEC 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242005 1 0000000000000000 100 0 0 10 0\n"
	root := writeProcFixture(t, tcp, "", map[string]string{
		fmt.Sprintf("%d/fd/3", dead): "4242001",
		fmt.Sprintf("%d/fd/3", live): "4242001",
		fmt.Sprintf("%d/fd/4", dead): "4242005",
	})

	// Port 3307 is held by both the reaped child and this process: only the
	// live holder answers.
	code, out := runRuntimeScriptShell(t, t.TempDir(), t.TempDir(),
		"managed_runtime_listener_pid 3307",
		"GC_DOLT_PORT=4406",
		"GC_DOLT_PROC_ROOT="+root,
	)
	if code != 0 {
		t.Fatalf("managed_runtime_listener_pid exited %d\n%s", code, out)
	}
	if got, want := strings.TrimSpace(out), strconv.Itoa(live); got != want {
		t.Fatalf("managed_runtime_listener_pid = %q, want %q (the only live holder)\n%s", got, want, out)
	}

	// Port 3308 is held only by the reaped child: no running holder, no
	// answer.
	code, out = runRuntimeScriptShell(t, t.TempDir(), t.TempDir(),
		"managed_runtime_listener_pid 3308",
		"GC_DOLT_PORT=4406",
		"GC_DOLT_PROC_ROOT="+root,
	)
	if code != 0 {
		t.Fatalf("managed_runtime_listener_pid exited %d\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != "" {
		t.Fatalf("managed_runtime_listener_pid = %q, want no output (the only holder is not running)\n%s", got, out)
	}
}

// TestManagedRuntimeListenerPIDRejectsNonNumericPorts pins the input guard:
// an empty or non-numeric port answers nothing and never spawns lsof, whose
// own complaint about such arguments would be noise on every caller's path.
func TestManagedRuntimeListenerPIDRejectsNonNumericPorts(t *testing.T) {
	for _, port := range []string{"", "abc", "0x0CEB"} {
		bin := t.TempDir()
		marker := filepath.Join(bin, "lsof-ran")
		writeLsofRecordingShim(t, bin)

		code, out := runRuntimeScriptShell(t, t.TempDir(), bin,
			`managed_runtime_listener_pids "$PROBE_PORT"; managed_runtime_listener_pid "$PROBE_PORT"`,
			"GC_DOLT_PORT=4406",
			"PROBE_PORT="+port,
			"LSOF_MARKER="+marker,
		)
		if code != 0 {
			t.Fatalf("port %q: exited %d\n%s", port, code, out)
		}
		if got := strings.TrimSpace(out); got != "" {
			t.Fatalf("port %q: answered %q, want no output\n%s", port, got, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("port %q: lsof ran; a non-numeric port must be rejected before any tool is invoked\n%s", port, out)
		}
	}
}

// TestManagedRuntimeListenerPIDFindsIPv6Holder extends AC-1 to the tcp6
// table: a listener bound to [::1] must be found through /proc/net/tcp6 just
// like an IPv4 one, without lsof.
func TestManagedRuntimeListenerPIDFindsIPv6Holder(t *testing.T) {
	skipUnlessProcNetTCPReadable(t)

	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback listener available: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	bin := t.TempDir()
	marker := filepath.Join(bin, "lsof-ran")
	writeLsofRecordingShim(t, bin)

	code, out := runRuntimeScriptShell(t, t.TempDir(), bin,
		"managed_runtime_listener_pid "+strconv.Itoa(port),
		"GC_DOLT_PORT=4406",
		"LSOF_MARKER="+marker,
	)
	if code != 0 {
		t.Fatalf("managed_runtime_listener_pid exited %d\n%s", code, out)
	}
	if got, want := strings.TrimSpace(out), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("managed_runtime_listener_pid = %q, want %q (the [::1] holder, via net/tcp6)\n%s", got, want, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("lsof ran; the IPv6 holder must come from net/tcp6 (gascity#7553)\n%s", out)
	}
}

// TestRuntimeScriptPortResolutionRejectsForeignHolder covers the mismatch
// branch of managed_runtime_port: managed state names a running PID that is
// not the socket holder (here PID 1 while this test holds the listener), so
// the state's port must not be trusted and resolution dies with the
// structured exit-78 error — with lsof never invoked.
func TestRuntimeScriptPortResolutionRejectsForeignHolder(t *testing.T) {
	skipUnlessProcNetTCPReadable(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	cityPath := t.TempDir()
	bin := t.TempDir()
	marker := filepath.Join(bin, "lsof-ran")
	writeLsofRecordingShim(t, bin)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, 1)

	stateFile := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	code, out := runRuntimeScriptShell(t, cityPath, bin,
		`printf '%s\n' "$GC_DOLT_PORT"`,
		"LSOF_MARKER="+marker,
	)
	if code != 78 {
		t.Fatalf("runtime.sh exited %d, want 78 (the state PID is not the holder)\n%s", code, out)
	}
	if got, want := out, expectedPortResolveErrorWithProvider(stateFile, cityPath, "present but not running"); got != want {
		t.Fatalf("runtime.sh output = %q, want %q", got, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("lsof ran while rejecting a foreign holder; the kernel tables already answer (gascity#7553)\n%s", out)
	}
}

// TestRuntimeScriptPortResolutionWithoutListenerRow covers the definitive
// no-listener branch: the state names this process on a port whose listener
// has been closed, so the kernel tables answer "no LISTEN row" and the
// decision falls to the TCP reachability probe — lsof is not consulted for a
// second opinion. A reachable port (nc succeeds) is accepted as the state
// port; an unreachable one dies with the structured exit-78 error.
func TestRuntimeScriptPortResolutionWithoutListenerRow(t *testing.T) {
	skipUnlessProcNetTCPReadable(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	t.Run("reachable port accepted", func(t *testing.T) {
		cityPath := t.TempDir()
		bin := t.TempDir()
		marker := filepath.Join(bin, "lsof-ran")
		writeLsofRecordingShim(t, bin)
		writeExecutable(t, filepath.Join(bin, "nc"), "#!/bin/sh\nexit 0\n")
		writeManagedRuntimeStateForScript(t, cityPath, port)

		code, out := runRuntimeScriptShell(t, cityPath, bin,
			`printf '%s\n' "$GC_DOLT_PORT"`,
			"LSOF_MARKER="+marker,
		)
		if code != 0 {
			t.Fatalf("runtime.sh exited %d; nc says the port is reachable, so the state port must be accepted\n%s", code, out)
		}
		if got, want := strings.TrimSpace(out), strconv.Itoa(port); got != want {
			t.Fatalf("GC_DOLT_PORT = %q, want %q\n%s", got, want, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("lsof ran after a definitive no-listener answer from the kernel tables\n%s", out)
		}
	})

	t.Run("unreachable port rejected", func(t *testing.T) {
		cityPath := t.TempDir()
		bin := t.TempDir()
		marker := filepath.Join(bin, "lsof-ran")
		writeLsofRecordingShim(t, bin)
		writeExecutable(t, filepath.Join(bin, "nc"), "#!/bin/sh\nexit 1\n")
		writeManagedRuntimeStateForScript(t, cityPath, port)

		stateFile := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
		code, out := runRuntimeScriptShell(t, cityPath, bin,
			`printf '%s\n' "$GC_DOLT_PORT"`,
			"LSOF_MARKER="+marker,
		)
		if code != 78 {
			t.Fatalf("runtime.sh exited %d, want 78 (no holder and no listener)\n%s", code, out)
		}
		if got, want := out, expectedPortResolveErrorWithProvider(stateFile, cityPath, "present but not running"); got != want {
			t.Fatalf("runtime.sh output = %q, want %q", got, want)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("lsof ran after a definitive no-listener answer from the kernel tables\n%s", out)
		}
	})
}

// TestHealthScriptServerPIDComesFromProcNotLsof runs the health command end
// to end against a real listener held by this process: server.pid in the
// JSON report must name this process, found through the kernel socket
// tables, with lsof never invoked anywhere on the health path.
func TestHealthScriptServerPIDComesFromProcNotLsof(t *testing.T) {
	skipUnlessProcNetTCPReadable(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	root := repoRoot(t)
	cityPath := t.TempDir()
	bin := t.TempDir()
	marker := filepath.Join(bin, "lsof-ran")
	writeLsofRecordingShim(t, bin)
	for _, name := range []string{"gc", "nc", "dolt"} {
		writeExecutable(t, filepath.Join(bin, name), "#!/bin/sh\nexit 1\n")
	}
	env := append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH", "LSOF_MARKER"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+strconv.Itoa(port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LSOF_MARKER="+marker,
	)
	out, err := newHealthScriptCmd(root, env, "--json").Output()
	if err != nil {
		t.Fatalf("health run.sh --json failed: %v\n%s", err, out)
	}
	var report struct {
		Server struct {
			PID int `json:"pid"`
		} `json:"server"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse health JSON: %v\n%s", err, out)
	}
	if report.Server.PID != os.Getpid() {
		t.Fatalf("server.pid = %d, want %d (this process holds the listener)\n%s", report.Server.PID, os.Getpid(), out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("lsof ran during the health probe; the kernel tables already answer (gascity#7553)\n%s", out)
	}
}
