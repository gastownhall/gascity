package tmuxtest

import (
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
// sibling run's sentinel exists. Each removal is described on diagnostics;
// callers that do not surface cleanup logs should pass io.Discard.
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

// killTmuxServerWait bounds how long killTmuxServersUnder waits for a
// killed server's process to actually exit before falling back to a direct
// SIGKILL. "tmux kill-server" returning success only means the server
// accepted the shutdown request -- closing panes and exiting happens
// asynchronously afterward (measured in the tens of milliseconds on a idle
// host), so this deadline is generous headroom for a loaded host, not a
// measured requirement.
const killTmuxServerWait = 2 * time.Second

// killTmuxServerPollInterval is the spacing between liveness checks while
// killTmuxServersUnder waits out killTmuxServerWait.
const killTmuxServerPollInterval = 20 * time.Millisecond

// killTmuxServersUnder issues "tmux -S <path> kill-server" for every Unix
// domain socket file found under dir, best-effort, and waits for the
// server process to actually exit before returning. A tmux server whose
// creator died before reaping it is still listening on that socket;
// os.RemoveAll only unlinks the socket file and never touches the server
// process itself, which is exactly what left it running and orphaned,
// reparented to init (ga-t33q83). The server's own PID is queried via
// "#{pid}" before the kill (the socket, and any session target on it, may
// already be gone by the time teardown finishes), then polled until it's
// confirmed dead or killTmuxServerWait elapses, with a direct SIGKILL
// fallback for a server that outlives the deadline. Each targeted
// socket/PID is explicit -- never a bare or default-socket kill-server, nor
// an untargeted signal. Diagnostics mirror the removal message above so a
// recurrence stays attributable from run logs.
func killTmuxServersUnder(dir string, diagnostics io.Writer) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return nil
		}
		pidOut, err := exec.Command("tmux", "-S", path, "display-message", "-p", "#{pid}").Output()
		if err != nil {
			return nil
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
		if err != nil || pid <= 0 {
			return nil
		}
		if _, err := exec.Command("tmux", "-S", path, "kill-server").CombinedOutput(); err != nil {
			return nil
		}
		deadline := time.Now().Add(killTmuxServerWait)
		for pidutil.Alive(pid) && time.Now().Before(deadline) {
			time.Sleep(killTmuxServerPollInterval)
		}
		if pidutil.Alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_, _ = fmt.Fprintf(diagnostics, "tmuxtest: killed orphaned tmux server at socket %s (pid %d)\n", path, pid)
		return nil
	})
}

// NewSocketParentDir sweeps orphaned sibling socket parent directories
// under root (see SweepOrphanPIDPrefixedDirs), then creates and returns a
// fresh one plus the *os.File holding its alive sentinel. The caller must
// keep the returned file referenced for as long as dir must stay protected
// from a concurrent sibling's sweep -- the runtime finalizes unreachable
// os.Files, which releases the flock. Sweep removal messages are written to
// diagnostics.
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
