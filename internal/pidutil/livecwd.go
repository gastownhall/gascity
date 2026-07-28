package pidutil

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// LiveState captures the working directories of every live process observed
// on this host, plus whether the enumeration itself succeeded. This is the
// primitive internal/session's working-directory collision guard shares with
// cmd/gc's worktree reaper (ga-ighomh.1 acceptance criterion 5), so both
// callers fail closed the same way when /proc cannot be scanned.
type LiveState struct {
	// CWDs is the set of canonicalized (symlink-resolved, absolute) working
	// directories of live processes. Deduplicated.
	CWDs []string
	// PIDCWDs maps each live process's PID to its canonicalized working
	// directory, gathered by the same /proc walk as CWDs. Unlike CWDs (which
	// only answers "is some process live at this path"), PIDCWDs lets a
	// caller attribute a live cwd to a *specific* PID — e.g. cross-referenced
	// against a known session's own PID — rather than blaming an arbitrary
	// known session for a coincidental cwd match elsewhere on the host
	// (ga-9x4z1g.1 FR3).
	PIDCWDs map[int]string
	// Scanned reports whether the process table was enumerated at all. False
	// means liveness is indeterminate — the host has no /proc, or the
	// top-level walk failed — and the caller must fail closed.
	Scanned bool
}

// LiveCWDs walks /proc/<pid>/cwd for every process on the host and records
// their canonical working directories. On a host without /proc (or when the
// top-level /proc walk fails outright) it returns Scanned=false so the
// caller fails closed.
//
// Per-process readlink failures are skipped, not fatal: a process may exit
// mid-walk, and a process owned by another user may have a cwd this process
// cannot resolve. The fleet runs every agent as the same user, so agent
// working directories are always visible here.
func LiveCWDs() LiveState {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return LiveState{Scanned: false}
	}
	seen := make(map[string]struct{})
	var cwds []string
	pidCWDs := make(map[int]string)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a PID directory
		}
		link, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil || link == "" {
			continue
		}
		// A cwd whose inode has been unlinked carries a trailing " (deleted)"
		// marker. The directory is gone, so it can never match a live path on
		// disk — drop it rather than canonicalize a bogus path.
		if strings.HasSuffix(link, " (deleted)") {
			continue
		}
		canon := pathutil.NormalizePathForCompare(link)
		if canon == "" {
			continue
		}
		pidCWDs[pid] = canon
		if _, ok := seen[canon]; ok {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}
	return LiveState{CWDs: cwds, PIDCWDs: pidCWDs, Scanned: true}
}

// PathAtOrUnder reports whether candidate equals root or is lexically
// contained beneath it. Both arguments must already be normalized
// (symlink-resolved, absolute, cleaned) — LiveCWDs normalizes cwds once at
// gather-time, so callers normalize the root once rather than re-resolving
// symlinks on every pair in what can be a large comparison set.
func PathAtOrUnder(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	if root == candidate {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
