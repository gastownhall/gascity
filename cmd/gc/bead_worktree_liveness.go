package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A per-bead worktree is protected from reaping when a process is actively
// working inside it, even though its bead is closed and its tree is momentarily
// git-clean. This is the "closed-bead != end-of-use" guard: bead status and
// git-cleanliness say nothing about whether an agent is mid-stage in the tree
// right now (a source anchor closes at plan-delivery while later review stages
// keep committing in the same tree; a tree is transiently clean between a push
// and the next stage). Deleting such a tree destroys live work — the founding
// incident behind gastownhall/gascity#4492.
//
// The design is modeled on the dolt_cleanup fail-closed precedent
// (dolt_cleanup_discovery.go): the /proc/<pid>/cwd signal is authoritative, and
// when it cannot be gathered the caller must protect every worktree rather than
// risk deleting live work.

// liveWorktreeState captures the working directories of every live process the
// reaper could observe on this host, plus whether the enumeration itself
// succeeded.
type liveWorktreeState struct {
	// cwds is the set of canonicalized (symlink-resolved, absolute) working
	// directories of live processes. Deduplicated.
	cwds []string
	// scanned reports whether the process table was enumerated at all. False
	// means liveness is indeterminate — no enumeration mechanism was available,
	// or every one of them failed — and the reaper must fail closed by
	// protecting every candidate worktree.
	scanned bool
	// source names the mechanism that produced this scan (liveScanSourceProc,
	// liveScanSourceLsof), empty when scanned is false. Recorded so the choice
	// of mechanism is observable rather than inferred from the host: a fallback
	// that silently substitutes itself is hard to debug when the gate later
	// behaves unexpectedly.
	source string
}

// collectLiveWorktreeStateFn is the seam the reaper calls to gather live
// process cwds. Indirected through a package-level var so tests can inject a
// deterministic set (including the fail-closed scanned=false case) without
// standing up real processes.
var collectLiveWorktreeStateFn = collectLiveWorktreeState

// collectLiveWorktreeState walks /proc/<pid>/cwd for every process on the host
// and records their canonical working directories. On a host without /proc it
// falls back to a portable process-table enumeration
// (bead_worktree_liveness_fallback.go); when no mechanism succeeds it returns
// scanned=false so the caller fails closed and reaps nothing.
//
// The fallback matters because /proc is Linux-only, and returning
// scanned=false for its absence does not merely make the reaper cautious on
// other platforms — it disables the feature outright and permanently, while the
// operator sees only "liveness scan unavailable". Darwin binaries are a
// published release target, and CI runs on Linux, so nothing here fails on the
// platform where the gate never worked.
//
// The check is at runtime rather than behind a build tag deliberately: /proc can
// also be absent on Linux (a container without it mounted), and the same
// fallback covers that case.
//
// Per-process readlink failures are skipped, not fatal: a process may exit
// mid-walk, and a process owned by another user may have a cwd this process
// cannot resolve. The fleet runs every agent as the same user, so agent
// worktree cwds are always visible here; the active-session-directory
// cross-check plus the git-clean and closed-bead gates back-stop any process
// this scan cannot see. This matches the dolt reaper's posture: the /proc
// signal protects, it never authorizes a deletion the other gates would refuse.
func collectLiveWorktreeState() liveWorktreeState {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return collectLiveWorktreeStateFallback()
	}
	seen := make(map[string]struct{})
	var cwds []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue // not a PID directory
		}
		link, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil || link == "" {
			continue
		}
		// A cwd whose inode has been unlinked carries a trailing " (deleted)"
		// marker. The directory is gone, so it can never match a live worktree
		// path on disk — drop it rather than canonicalize a bogus path. (The
		// rare live directory literally named "... (deleted)" would be dropped
		// too; that only ever loses protection for a pathological path the
		// fleet never creates, and the git-clean gate still applies.)
		if strings.HasSuffix(link, " (deleted)") {
			continue
		}
		canon := pathutil.NormalizePathForCompare(link)
		if canon == "" {
			continue
		}
		if _, ok := seen[canon]; ok {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}
	return liveWorktreeState{cwds: cwds, scanned: true, source: liveScanSourceProc}
}

// worktreeIsLive reports whether any live signal sits at or beneath
// worktreePath: a live process cwd, or a recorded active-session working
// directory. "At or beneath" means the worktree is protected when a process is
// running in it OR in any subdirectory of it (an agent whose cwd is a nested
// test/build subdir of its assigned tree still counts). It returns the matching
// path as a human-readable reason for dry-run/operator output.
//
// The caller is responsible for the fail-closed case: worktreeIsLive assumes
// the live set was successfully gathered. When liveWorktreeState.scanned is
// false the caller must protect unconditionally and never reach this function
// for a reap decision.
func worktreeIsLive(worktreePath string, live liveWorktreeState, sessionDirs []string) (bool, string) {
	wt := pathutil.NormalizePathForCompare(worktreePath)
	if wt == "" {
		return false, ""
	}
	for _, cwd := range live.cwds {
		if pathAtOrUnder(wt, cwd) {
			return true, "live process cwd " + cwd
		}
	}
	for _, dir := range sessionDirs {
		d := pathutil.NormalizePathForCompare(dir)
		if d == "" {
			continue
		}
		if pathAtOrUnder(wt, d) {
			return true, "active session dir " + d
		}
	}
	return false, ""
}

// pathAtOrUnder reports whether candidate equals root or is lexically contained
// beneath it. Both arguments must already be normalized (symlink-resolved,
// absolute, cleaned) — collectLiveWorktreeState normalizes cwds once at
// gather-time and worktreeIsLive normalizes the worktree once, so this avoids
// re-resolving symlinks on every pair in what can be a large process × worktree
// cross-product each tick.
func pathAtOrUnder(root, candidate string) bool {
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

// liveSessionWorktreeDirs collects the recorded working directories of every
// open (non-closed) session in the snapshot: the canonical worker_dir first
// (via WorkerDirFromInfo), plus the raw work_dir and gc.work_dir mirrors so a
// session whose canonical dir is momentarily unstamped still contributes a
// protecting path. The result is the "active session set" the reaper
// cross-checks against — a belt-and-suspenders signal alongside the
// authoritative /proc cwd scan, since session metadata is stamped at
// create/dispatch and is not continuously refreshed. Deduplicated; empty
// entries dropped.
func liveSessionWorktreeDirs(snapshot *sessionBeadSnapshot) []string {
	if snapshot == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var dirs []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		dirs = append(dirs, p)
	}
	for _, info := range snapshot.OpenInfos() {
		add(sessionpkg.WorkerDirFromInfo(info))
		add(info.WorkDir)
		add(info.WorkDirCanonical)
		add(info.WorkerDir)
	}
	return dirs
}
