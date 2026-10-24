package dolttest

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
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// OwnerEnv carries the identity ("<pid>.<starttime>") of the armed test
// binary that owns a process. ArmOwnerReaper exports it into the test
// binary's environment, so every process the run spawns inherits it,
// including the detached ones that outlive their parent by design: bd's
// `db-proxy-child` (Setsid) and the `dolt sql-server` in its session. The
// GC_ prefix keeps it in gc's session passthrough environment, so processes
// started inside agent sessions carry it too.
const OwnerEnv = "GC_TEST_PROCESS_OWNER"

// ownerWatchdogEnv switches a re-executed test binary into watchdog mode. Its
// value is the owner token the watchdog reaps.
const ownerWatchdogEnv = "DOLTTEST_OWNER_WATCHDOG"

// ownerReapGrace is how long a reap waits after SIGTERM before it SIGKILLs.
// SIGTERM lets bd's proxy stop its dolt child and lets dolt flush.
const ownerReapGrace = 2 * time.Second

// ownerReapRounds bounds the re-scans after a reap: a proxy can spawn a
// marked dolt child between the scan and its own death.
const ownerReapRounds = 3

// ownerWatchdogPipe is the write end of the pipe whose EOF tells the watchdog
// that the test binary is gone. It must stay reachable for the life of the
// process: the runtime closes unreachable os.Files from a finalizer, and that
// close would fire the watchdog while the run is still alive.
var ownerWatchdogPipe *os.File

// ownedProcess is one process carrying an OwnerEnv marker. startTime pins
// its identity against pid reuse during the reap.
type ownedProcess struct {
	pid       int
	startTime string
	owner     string
}

// ArmOwnerReaper ties every process this test binary spawns to its lifetime.
// Call it first in TestMain, before anything else spawns processes.
//
// t.Cleanup and TestMain defers never run when `go test -timeout` panics or
// a gate kills the binary, and the processes bd detaches on purpose survive a
// process-group kill. ArmOwnerReaper closes that gap in three parts:
//
//  1. It reaps processes left by prior runs whose owner is no longer alive.
//  2. It sets OwnerEnv to this binary's identity for every descendant.
//  3. It starts a watchdog: this binary re-executed in its own session,
//     holding the read end of a pipe. The kernel closes the write end when
//     this process exits for any reason, SIGKILL included. On EOF the
//     watchdog reaps every process that carries this binary's token, then
//     removes the directories registered with RemoveOnOwnerExit.
//
// In the watchdog re-exec this call runs the watchdog and exits the process
// instead of returning. The reaper is a no-op where /proc is unavailable.
func ArmOwnerReaper() error {
	if token := os.Getenv(ownerWatchdogEnv); token != "" {
		os.Exit(runOwnerWatchdog(token, os.Stdin))
	}
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		return nil
	}
	token, err := ownerToken(os.Getpid())
	if err != nil {
		return fmt.Errorf("dolttest: reading own process identity: %w", err)
	}
	for _, p := range sweepStaleOwned() {
		fmt.Fprintf(os.Stderr, "dolttest: reaped pid %d left by dead test binary %s\n", p.pid, p.owner) //nolint:errcheck
	}
	if err := os.Setenv(OwnerEnv, token); err != nil {
		return fmt.Errorf("dolttest: exporting %s: %w", OwnerEnv, err)
	}
	return startOwnerWatchdog(token)
}

func startOwnerWatchdog(token string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("dolttest: locating test binary for owner watchdog: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("dolttest: creating owner watchdog pipe: %w", err)
	}
	cmd := exec.Command(exe)
	// The watchdog does not carry the marker: it is not part of the run it
	// reaps. Its stdout and stderr stay on /dev/null because the run's output
	// pipes close with the run, and a write to a closed pipe would kill the
	// watchdog with SIGPIPE before it reaps.
	cmd.Env = append(envWithout(os.Environ(), OwnerEnv), ownerWatchdogEnv+"="+token)
	cmd.Stdin = r
	// Setsid keeps the watchdog out of the test binary's process group and
	// session, so the group kill that takes the run down spares it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return fmt.Errorf("dolttest: starting owner watchdog: %w", err)
	}
	_ = r.Close()
	ownerWatchdogPipe = w
	// The watchdog exits only after this process does, so nothing here will
	// ever wait on it.
	return cmd.Process.Release()
}

// RemoveOnOwnerExit registers dir for removal by the owner watchdog once this
// test binary is gone and its processes are reaped. It is for run-scoped
// directories a killed run would otherwise strand, such as the per-run
// directory holding the built gc and bd binaries. dir must be absolute. It is
// a no-op when ArmOwnerReaper has not started a watchdog.
func RemoveOnOwnerExit(dir string) error {
	if ownerWatchdogPipe == nil {
		return nil
	}
	if !filepath.IsAbs(dir) || strings.ContainsRune(dir, '\n') {
		return fmt.Errorf("dolttest: owner-exit removal needs an absolute single-line path, got %q", dir)
	}
	if _, err := ownerWatchdogPipe.WriteString(filepath.Clean(dir) + "\n"); err != nil {
		return fmt.Errorf("dolttest: registering %s for owner-exit removal: %w", dir, err)
	}
	return nil
}

// runOwnerWatchdog reads directory registrations from parent until EOF, then
// reaps every process that carries token and removes the registered
// directories. Processes go first so none still writes into a directory as
// it is removed. It returns the watchdog's exit code.
func runOwnerWatchdog(token string, parent io.Reader) int {
	var dirs []string
	lines := bufio.NewScanner(parent)
	for lines.Scan() {
		if dir := lines.Text(); filepath.IsAbs(dir) {
			dirs = append(dirs, dir)
		}
	}
	reapOwned(token)
	code := 0
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			code = 1
		}
	}
	return code
}

// reapOwned terminates every live process whose OwnerEnv is token: SIGTERM,
// then SIGKILL for whatever is still the same process after ownerReapGrace.
func reapOwned(token string) {
	reapOwnedMatching(func(owner string) bool { return owner == token })
}

// sweepStaleOwned reaps marked processes whose owning test binary is dead:
// the ones a killed run left behind when its watchdog could not run either.
// Processes owned by a live run, including concurrent ones, are spared, and
// so are processes whose marker names no process at all. It returns the
// processes it signaled.
func sweepStaleOwned() []ownedProcess {
	return reapOwnedMatching(func(owner string) bool {
		pid, startTime, ok := parseOwnerToken(owner)
		return ok && !pidutil.AliveWithStartTime(pid, startTime)
	})
}

func reapOwnedMatching(match func(owner string) bool) []ownedProcess {
	var reaped []ownedProcess
	for round := 0; round < ownerReapRounds; round++ {
		var targets []ownedProcess
		for _, p := range scanOwnedProcesses() {
			if match(p.owner) {
				targets = append(targets, p)
			}
		}
		if len(targets) == 0 {
			break
		}
		terminateOwned(targets)
		reaped = append(reaped, targets...)
	}
	return reaped
}

func terminateOwned(targets []ownedProcess) {
	for _, p := range targets {
		_ = syscall.Kill(p.pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(ownerReapGrace)
	for _, p := range targets {
		if waitProcessExit(p.pid, p.startTime, time.Until(deadline)) {
			continue
		}
		// The identity check keeps a recycled pid from receiving SIGKILL.
		if pidutil.AliveWithStartTime(p.pid, p.startTime) {
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
		}
	}
}

// scanOwnedProcesses returns every readable process other than this one that
// carries an OwnerEnv marker. tmux servers are left out: the tmux safety rule
// confines tmux cleanup to socket-scoped sweeps (tmuxtest), and a tmux server
// exits on its own once its marked pane processes are gone.
func scanOwnedProcesses() []ownedProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []ownedProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		owner := procEnvValue(pid, OwnerEnv)
		if owner == "" || procIsTmux(pid) {
			continue
		}
		startTime, err := pidutil.StartTime(pid)
		if err != nil {
			continue
		}
		out = append(out, ownedProcess{pid: pid, startTime: startTime, owner: owner})
	}
	return out
}

// procEnvValue returns key's value in pid's initial environment, or "" when
// it is absent or unreadable (another user's process, or one that exited).
func procEnvValue(pid int, key string) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	prefix := []byte(key + "=")
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if bytes.HasPrefix(entry, prefix) {
			return string(entry[len(prefix):])
		}
	}
	return ""
}

func procIsTmux(pid int) bool {
	argv, err := pidutil.Cmdline(pid)
	return err == nil && len(argv) > 0 && filepath.Base(argv[0]) == "tmux"
}

// ownerToken returns the OwnerEnv value identifying pid.
func ownerToken(pid int) (string, error) {
	startTime, err := pidutil.StartTime(pid)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(pid) + "." + startTime, nil
}

// parseOwnerToken splits an OwnerEnv value into the owner's pid and start
// time.
func parseOwnerToken(token string) (pid int, startTime string, ok bool) {
	pidPart, startTime, ok := strings.Cut(token, ".")
	if !ok || startTime == "" {
		return 0, "", false
	}
	pid, err := strconv.Atoi(pidPart)
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	return pid, startTime, true
}

// envWithout returns env without any entry for key.
func envWithout(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}
