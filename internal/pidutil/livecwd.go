package pidutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// LiveState captures the working directories of every live process observed
// on this host, plus whether the enumeration itself succeeded. It backs
// internal/session's working-directory collision guard (ga-ighomh.1) and the
// bead-worktree reaper's liveness gate.
type LiveState struct {
	// CWDs is the set of canonicalized (symlink-resolved, absolute) working
	// directories of live processes. Deduplicated.
	CWDs []string
	// PIDCWDs maps each live process's PID to its canonicalized working
	// directory, so a caller can attribute a live cwd to a specific PID.
	PIDCWDs map[int]string
	// Scanned reports whether the process table was enumerated at all. False
	// means liveness is indeterminate and the caller must fail closed.
	Scanned bool
}

// procRootDir is the procfs mount LiveCWDs walks.
const procRootDir = "/proc"

// lsofScanTimeout bounds the lsof enumeration used where there is no procfs.
// A timeout leaves the scan indeterminate, so the caller fails closed.
const lsofScanTimeout = 20 * time.Second

// LiveCWDs enumerates the working directory of every process on the host:
// from procfs where the host has one, and from lsof where it does not (macOS).
// A host with neither, or a scan that fails outright, returns Scanned=false so
// the caller fails closed.
func LiveCWDs() LiveState {
	return liveCWDsFrom(procRootDir, lsofLiveCWDs)
}

// liveCWDsFrom is LiveCWDs over the procfs mounted at procRoot, with fallback
// as the enumeration used when procRoot does not exist. A procfs that exists
// but cannot be listed is a real scan failure, not "this platform has none",
// so the fallback does not paper over it.
//
// Per-process readlink failures are skipped, not fatal: a process may exit
// mid-walk, and a process owned by another user may have a cwd this process
// cannot resolve. The fleet runs every agent as the same user, so agent
// working directories are always visible here.
func liveCWDsFrom(procRoot string, fallback func() LiveState) LiveState {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fallback()
		}
		return LiveState{}
	}
	state := newLiveState()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue // not a PID directory
		}
		link, err := os.Readlink(filepath.Join(procRoot, entry.Name(), "cwd"))
		if err != nil || link == "" {
			continue
		}
		// A cwd whose inode has been unlinked carries a trailing " (deleted)"
		// marker. The directory is gone, so it can never match a live path on
		// disk — drop it rather than canonicalize a bogus path.
		if strings.HasSuffix(link, " (deleted)") {
			continue
		}
		state.add(pid, pathutil.NormalizePathForCompare(link))
	}
	state.Scanned = true
	return state.LiveState
}

// lsofErrAnnotation matches the per-process errors lsof reports inside the n
// field itself, as an absolute-looking string
// ("/proc/1/cwd (readlink: Permission denied)"). Counting one would make an
// unreadable scan look usable.
var lsofErrAnnotation = regexp.MustCompile(`\s\((?:readlink|stat|lstat|opendir|getcwd)[^)]*: [^)]*\)$`)

// lsofLiveCWDs enumerates process working directories with lsof, for hosts
// that have no procfs.
func lsofLiveCWDs() LiveState {
	ctx, cancel := context.WithTimeout(context.Background(), lsofScanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-F0pn")
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = fmt.Errorf("lsof: %w", ctxErr)
	}
	return ParseLsofCWDs(out, err)
}

// ParseLsofCWDs parses `lsof -a -d cwd -F0pn` output, with the error the run
// returned, into a LiveState.
//
// The 0 in -F0 makes lsof NUL-terminate each field; without it a path
// containing a newline would split across two lines and be silently
// truncated into a different directory. A "p<pid>" field names the process
// the following "n<path>" fields belong to.
//
// Two rules fail closed or open on purpose:
//
//   - No readable record at all means the enumeration FAILED (lsof absent, or
//     every record an error annotation), not that the host is idle: Scanned is
//     false.
//   - Records alongside an ordinary non-zero exit are a PARTIAL scan and count.
//     lsof cannot read other users' descriptors unprivileged; /proc has the
//     identical blind spot. A deadline is the exception: truncation at an
//     arbitrary point is not that bounded blind spot, so it is not Scanned.
func ParseLsofCWDs(out []byte, runErr error) LiveState {
	if errors.Is(runErr, context.DeadlineExceeded) {
		return LiveState{}
	}
	state := newLiveState()
	pid := 0
	// Fields are NUL-terminated; records are newline-separated, so the first
	// field after a record boundary carries a leading newline to trim.
	for _, field := range strings.Split(string(out), "\x00") {
		field = strings.Trim(field, "\r\n")
		if p, ok := strings.CutPrefix(field, "p"); ok {
			pid, _ = strconv.Atoi(p)
			continue
		}
		path, ok := strings.CutPrefix(field, "n")
		if !ok || !strings.HasPrefix(path, "/") || lsofErrAnnotation.MatchString(path) {
			continue
		}
		state.add(pid, pathutil.NormalizePathForCompare(path))
	}
	if len(state.CWDs) == 0 {
		return LiveState{}
	}
	state.Scanned = true
	return state.LiveState
}

// liveStateBuilder accumulates a LiveState, keeping CWDs deduplicated in
// first-seen order.
type liveStateBuilder struct {
	LiveState
	seen map[string]struct{}
}

func newLiveState() *liveStateBuilder {
	return &liveStateBuilder{
		LiveState: LiveState{PIDCWDs: make(map[int]string)},
		seen:      make(map[string]struct{}),
	}
}

// add records canon as the cwd of pid (pid <= 0 records the directory only).
func (b *liveStateBuilder) add(pid int, canon string) {
	if canon == "" {
		return
	}
	if pid > 0 {
		b.PIDCWDs[pid] = canon
	}
	if _, ok := b.seen[canon]; ok {
		return
	}
	b.seen[canon] = struct{}{}
	b.CWDs = append(b.CWDs, canon)
}
