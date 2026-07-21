package tmuxtest

import (
	"fmt"
	"os"
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
	if end < len(suffix) && suffix[end] != '-' {
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
// the host /tmp (ga-djbcqt). PID liveness is only a fallback for dirs
// without a sentinel -- e.g. ones leaked before this sweep existed. Dirs
// younger than socketParentSweepMinAge are never touched, covering the
// window before a sibling run's sentinel exists.
func SweepOrphanPIDPrefixedDirs(root, prefix string) {
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
			// No sentinel at all: either leaked before this sweep existed,
			// or crashed between MkdirTemp and HoldAliveSentinel. Fall back
			// to PID liveness.
			if pidutil.Alive(pid) {
				continue
			}
			reason = "legacy: pid dead, no sentinel"
		}
		// Name each removal so a recurrence of ga-djbcqt is attributable
		// from run logs instead of gate-log forensics.
		fmt.Fprintf(os.Stderr, "tmuxtest: removing orphaned socket parent %s (%s)\n", path, reason)
		_ = os.RemoveAll(path)
	}
}

// NewSocketParentDir sweeps orphaned sibling socket parent directories
// under root (see SweepOrphanPIDPrefixedDirs), then creates and returns a
// fresh one plus the *os.File holding its alive sentinel. The caller must
// keep the returned file referenced for as long as dir must stay protected
// from a concurrent sibling's sweep -- the runtime finalizes unreachable
// os.Files, which releases the flock.
func NewSocketParentDir(root string) (dir string, sentinel *os.File, err error) {
	SweepOrphanPIDPrefixedDirs(root, SocketParentDirPrefix)
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
