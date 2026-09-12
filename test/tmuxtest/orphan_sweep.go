package tmuxtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// SocketParentDirPrefix is the shared prefix for the tmux Unix-socket parent
// directories created by cmd/gc, internal/runtime/tmux, and test/integration
// TestMains. All three use the same root ("/tmp", for macOS socket-path
// length reasons -- see each call site) and prefix so a sweep triggered by
// any one of them reaps orphans left by any of the others.
const SocketParentDirPrefix = "gct-"

// socketParentAliveSentinelName is a lock file inside each socket parent
// dir. The creating process holds an exclusive flock on it for its
// lifetime; SweepOrphanPIDPrefixedDirs probes the lock instead of trusting
// PID visibility, which lies across PID namespaces (ga-djbcqt: bwrap
// --unshare-pid sandboxes see every host PID as dead while sharing the host
// /tmp). Ported from cmd/gc's identical test-temp-root sentinel mechanism
// (cmd/gc/test_orphan_sweep_test.go) so all three tmux socket parent
// creation sites share one policy instead of cmd/gc's copy being
// reimplemented per package -- package main cannot be imported, so this is
// the shared home.
const socketParentAliveSentinelName = ".gc-test-alive.lock"

// socketParentSweepMinAge is the minimum age before a PID-prefixed dir
// becomes a sweep candidate. It closes the window where a sibling run has
// created its dir but not yet acquired the alive sentinel.
const socketParentSweepMinAge = time.Hour

// PIDPrefixedTempPattern returns the os.MkdirTemp pattern for this
// process's own socket parent dir: "<prefix><pid>-*".
func PIDPrefixedTempPattern(prefix string) string {
	return prefix + strconv.Itoa(os.Getpid()) + "-*"
}

// HoldAliveSentinel creates <dir>/.gc-test-alive.lock and takes an
// exclusive flock on it. The caller must keep the returned file referenced
// for as long as dir must stay protected from SweepOrphanPIDPrefixedDirs:
// the runtime finalizes unreachable os.Files, which closes the descriptor
// and releases the lock.
func HoldAliveSentinel(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, socketParentAliveSentinelName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening alive sentinel in %q: %w", dir, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking alive sentinel in %q: %w", dir, err)
	}
	return f, nil
}

// aliveSentinelHeld probes <dir>'s alive sentinel. exists reports whether
// the sentinel file is present; held reports whether some process still
// holds its flock. Probe failures are reported as held so the sweep stays
// conservative.
func aliveSentinelHeld(dir string) (exists, held bool) {
	f, err := os.OpenFile(filepath.Join(dir, socketParentAliveSentinelName), os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false
		}
		return true, true
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true, false
}

// pidFromPrefixedDirName parses the owner PID out of a socket-parent dir name
// of the form "<prefix><PID>-<random>" -- the shape NewSocketParentDir creates
// via os.MkdirTemp(root, "<prefix><PID>-*"). The "-" separator after the PID is
// required: a bare all-digit "<prefix><digits>" name is a legacy directory left
// by the pre-sweep harness (os.MkdirTemp(root, prefix)), whose trailing digits
// are a random suffix, not an owner PID. Parsing that random number as a PID
// could reap a still-live legacy sibling once it aged past the sweep guard, so
// such names are rejected here and left for a dedicated opt-in cleanup path.
func pidFromPrefixedDirName(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	end := 0
	for end < len(suffix) && suffix[end] >= '0' && suffix[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	if end >= len(suffix) || suffix[end] != '-' {
		return 0, false
	}
	pid, err := strconv.Atoi(suffix[:end])
	if err != nil {
		return 0, false
	}
	return pid, true
}

// SweepOrphanPIDPrefixedDirs removes <root>/<prefix><PID>-<random> dirs
// whose creator is gone. Best-effort; ignores errors. Ported from cmd/gc's
// sweepOrphanPIDPrefixedDirs (test_orphan_sweep_test.go) so cmd/gc,
// internal/runtime/tmux, and test/integration share one policy for their
// tmux socket parent dirs instead of each reimplementing it.
//
// Liveness is decided by the alive sentinel flock when present: flock state
// is visible across PID namespaces, whereas raw PID liveness reports every
// host PID as dead from inside a bwrap --unshare-pid sandbox that shares
// the host /tmp (ga-djbcqt). PID liveness is only a fallback for a
// "<prefix><PID>-<random>" dir that crashed between MkdirTemp and
// HoldAliveSentinel; legacy pre-sweep names with no "-" after the PID are
// rejected by pidFromPrefixedDirName and never swept here. Dirs younger than
// socketParentSweepMinAge are never touched, covering the window before a
// sibling run's sentinel exists.
//
// This signals processes, not just files: before an eligible dir is removed,
// any tmux server bound to a socket under it is killed through that explicit
// socket, with a bounded wait and an exact-PID SIGKILL fallback for a server
// that outlives it. Each removal and each kill outcome is described on
// diagnostics; callers that do not surface cleanup logs should pass
// io.Discard.
func SweepOrphanPIDPrefixedDirs(root, prefix string, diagnostics io.Writer) {
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	self := os.Getpid()
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, ok := pidFromPrefixedDirName(e.Name(), prefix)
		if !ok || pid <= 0 || pid == self {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < socketParentSweepMinAge {
			continue
		}
		path := filepath.Join(root, e.Name())
		exists, held := aliveSentinelHeld(path)
		var reason string
		switch {
		case held:
			// Creator (possibly in another PID namespace) is still alive.
			continue
		case exists:
			// Sentinel present but unlocked: the creator is gone. Remove.
			reason = "free sentinel"
		default:
			// A "<prefix><PID>-<random>" dir with no sentinel: its creator
			// crashed between MkdirTemp and HoldAliveSentinel. Fall back to
			// PID liveness. (Legacy no-"-" names are rejected by
			// pidFromPrefixedDirName and never reach here.)
			if pidutil.Alive(pid) {
				continue
			}
			reason = "pid dead, no sentinel"
		}
		// Name each removal so a recurrence of ga-djbcqt is attributable
		// from run logs instead of gate-log forensics.
		_, _ = fmt.Fprintf(diagnostics, "tmuxtest: removing orphaned socket parent %s (%s)\n", path, reason)
		killTmuxServersUnder(path, diagnostics)
		_ = os.RemoveAll(path)
	}
}

// killTmuxServerWait bounds how long the reaper waits for a killed server's
// process to actually exit before falling back to a direct SIGKILL. "tmux
// kill-server" returning success only means the server accepted the shutdown
// request -- closing panes and exiting happens asynchronously afterward
// (measured in the tens of milliseconds on an idle host), so this deadline is
// generous headroom for a loaded host, not a measured requirement. It bounds
// the wait for an exit, not the client call that requested it; see
// tmuxGuardCommandTimeout for the latter.
const killTmuxServerWait = 2 * time.Second

// killTmuxServerPollInterval is the spacing between liveness checks while
// the reaper waits out killTmuxServerWait.
const killTmuxServerPollInterval = 20 * time.Millisecond

// errNoTmuxServerAtSocket reports that nothing answered the PID query at a
// socket -- a stale socket file left by an already-dead server, which is the
// common case and worth no diagnostics. It is distinct from a query that
// timed out, which means a peer is holding the socket and did not answer.
var errNoTmuxServerAtSocket = errors.New("no tmux server answering at socket")

// isTmuxArgv reports whether argv belongs to a tmux process, matching on the
// basename of argv[0] the way cmd/gc's leak guard attributes a PID before
// signaling it (cmd/gc/tmux_leak_guard_test.go, readTmuxArgv).
func isTmuxArgv(argv []string) bool {
	return len(argv) > 0 && filepath.Base(argv[0]) == "tmux"
}

// tmuxServerIdentityAtSocket asks the tmux server bound to socketPath for its
// own PID and captures that PID's start-time identity token in the same step.
// Capturing the token here, before anything signals the process, is what lets
// the caller re-attribute the PID later: a bare liveness check would report a
// recycled PID's unrelated new owner as the original target.
func tmuxServerIdentityAtSocket(socketPath string) (pid int, startTime string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, "tmux", "-S", socketPath, "display-message", "-p", "#{pid}").Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, "", fmt.Errorf("querying tmux server pid at %s: %w", socketPath, ctxErr)
	}
	if runErr != nil {
		return 0, "", errNoTmuxServerAtSocket
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil || pid <= 0 {
		return 0, "", fmt.Errorf("tmux server at %s reported an unusable pid %q", socketPath, strings.TrimSpace(string(out)))
	}
	startTime, _ = pidutil.StartTime(pid)
	return pid, startTime, nil
}

// tmuxServerReaper holds the process-touching operations the sweep performs
// per socket. They are struct fields rather than direct calls so the
// kill-fails-but-process-lives contract can be unit-tested without a real
// tmux server -- the same seam internal/runtime/proctable/kill_unix.go uses
// to test KillByPID's confirmed-dead-before-return guarantee.
type tmuxServerReaper struct {
	// serverIdentity resolves the PID serving socketPath plus that PID's
	// start-time identity token.
	serverIdentity func(socketPath string) (pid int, startTime string, err error)
	// killServer asks the server bound to socketPath to shut down.
	killServer func(socketPath string) error
	// sameProcess reports whether pid is still the process startTime named.
	sameProcess func(pid int, startTime string) bool
	// isTmuxProcess reports whether pid is still a live tmux process.
	isTmuxProcess func(pid int) bool
	// signal delivers sig to pid.
	signal func(pid int, sig syscall.Signal) error
	// exitWait and pollInterval bound the wait for a graceful exit.
	exitWait     time.Duration
	pollInterval time.Duration
}

// liveTmuxServerReaper is the reaper wired to the real host.
var liveTmuxServerReaper = tmuxServerReaper{
	serverIdentity: tmuxServerIdentityAtSocket,
	killServer:     killTmuxServerAtSocket,
	sameProcess:    pidutil.AliveWithStartTime,
	isTmuxProcess:  func(pid int) bool { return pidutil.AliveWithCmdline(pid, isTmuxArgv) },
	signal:         syscall.Kill,
	exitWait:       killTmuxServerWait,
	pollInterval:   killTmuxServerPollInterval,
}

// killTmuxServersUnder reaps the tmux server behind every Unix domain socket
// file found under dir, best-effort, and waits for each server process to
// actually exit before returning. A tmux server whose creator died before
// reaping it is still listening on that socket; os.RemoveAll only unlinks the
// socket file and never touches the server process itself, which is exactly
// what left it running and orphaned, reparented to init (ga-t33q83).
func killTmuxServersUnder(dir string, diagnostics io.Writer) {
	liveTmuxServerReaper.killServersUnder(dir, diagnostics)
}

// killServersUnder walks dir and reaps the server behind each socket file.
func (r tmuxServerReaper) killServersUnder(dir string, diagnostics io.Writer) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return nil
		}
		r.reapServerAtSocket(path, diagnostics)
		return nil
	})
}

// reapServerAtSocket kills the tmux server bound to socketPath and does not
// return until that server is gone or has resisted every escalation.
//
// The server's own PID is queried before the kill, since the socket -- and
// any session target on it -- may already be gone by the time teardown
// finishes. Both client calls are bounded (tmuxGuardCommandTimeout): this
// runs at TestMain startup for three packages, against the least healthy
// server population by construction, so an unbounded call to a
// wedged-but-accepting peer would hang whole suites with no output.
//
// A failed or timed-out kill-server never ends the reap: the process is
// polled and SIGKILLed anyway, because the caller unlinks the socket either
// way and a survivor would be left running, socketless, and undiscoverable.
// Every outcome, including failure, is named on diagnostics so such a
// survivor stays attributable from run logs.
//
// Escalation to SIGKILL re-attributes the target first -- same PID, same
// start time (pidutil.AliveWithStartTime), still a tmux process
// (pidutil.AliveWithCmdline), never this process. The PID was reported by an
// IPC peer discovered under a shared socket root and may be up to
// killTmuxServerWait stale by the time the signal lands, so bare liveness
// would be enough to SIGKILL a recycled PID's innocent new owner
// (internal/runtime/proctable/kill_unix.go guards its own kill the same way).
//
// An unavailable start-time token does not veto the SIGKILL. pidutil.StartTime
// reads /proc, so it fails for every PID on darwin; refusing to signal without
// a token would delete the escalation entirely on macOS hosts, where an
// orphaned server is just as able to outlive kill-server. The token is one of
// two independent gates, and the other -- pidutil.AliveWithCmdline, which must
// still see argv[0] basename "tmux" -- keeps narrowing the target on Linux
// when the token is missing.
func (r tmuxServerReaper) reapServerAtSocket(socketPath string, diagnostics io.Writer) {
	pid, startTime, err := r.serverIdentity(socketPath)
	if err != nil {
		if !errors.Is(err, errNoTmuxServerAtSocket) {
			_, _ = fmt.Fprintf(diagnostics, "tmuxtest: leaving socket %s unreaped: %v\n", socketPath, err)
		}
		return
	}
	if pid == os.Getpid() {
		_, _ = fmt.Fprintf(diagnostics, "tmuxtest: socket %s reported this process (pid %d) as its tmux server; not signaling\n", socketPath, pid)
		return
	}
	if killErr := r.killServer(socketPath); killErr != nil {
		_, _ = fmt.Fprintf(diagnostics, "tmuxtest: kill-server failed for orphaned tmux server at socket %s (pid %d): %v\n", socketPath, pid, killErr)
	}
	deadline := time.Now().Add(r.exitWait)
	for r.sameProcess(pid, startTime) && time.Now().Before(deadline) {
		time.Sleep(r.pollInterval)
	}
	if !r.sameProcess(pid, startTime) {
		_, _ = fmt.Fprintf(diagnostics, "tmuxtest: killed orphaned tmux server at socket %s (pid %d)\n", socketPath, pid)
		return
	}
	if !r.isTmuxProcess(pid) {
		_, _ = fmt.Fprintf(diagnostics, "tmuxtest: orphaned tmux server at socket %s (pid %d) outlived kill-server and no longer looks like tmux; not signaling\n", socketPath, pid)
		return
	}
	if sigErr := r.signal(pid, syscall.SIGKILL); sigErr != nil {
		_, _ = fmt.Fprintf(diagnostics, "tmuxtest: SIGKILL failed for orphaned tmux server at socket %s (pid %d): %v\n", socketPath, pid, sigErr)
		return
	}
	_, _ = fmt.Fprintf(diagnostics, "tmuxtest: SIGKILLed orphaned tmux server at socket %s (pid %d) after it outlived kill-server by %s\n", socketPath, pid, r.exitWait)
}

// NewSocketParentDir sweeps orphaned sibling socket parent directories
// under root (see SweepOrphanPIDPrefixedDirs), then creates and returns a
// fresh one plus the *os.File holding its alive sentinel. The caller must
// keep the returned file referenced for as long as dir must stay protected
// from a concurrent sibling's sweep -- the runtime finalizes unreachable
// os.Files, which releases the flock. The sweep signals processes as well as
// removing directories: any tmux server bound to a socket under an eligible
// sibling dir is killed through that explicit socket, with a bounded wait and
// an exact-PID SIGKILL fallback. Sweep removal and kill messages are written
// to diagnostics.
func NewSocketParentDir(root string, diagnostics io.Writer) (dir string, sentinel *os.File, err error) {
	SweepOrphanPIDPrefixedDirs(root, SocketParentDirPrefix, diagnostics)
	dir, err = os.MkdirTemp(root, PIDPrefixedTempPattern(SocketParentDirPrefix))
	if err != nil {
		return "", nil, err
	}
	sentinel, err = HoldAliveSentinel(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, sentinel, nil
}
