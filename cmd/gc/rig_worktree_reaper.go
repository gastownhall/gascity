package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// rigWorktreesDirName is the directory, directly under a rig's own path, that
// holds the agent-improvised checkouts reapDetachedRigWorktrees reclaims.
const rigWorktreesDirName = "worktrees"

// detachedHeadBranch is what git.CurrentBranch reports for a worktree that is
// checked out on a bare commit rather than a branch.
const detachedHeadBranch = "HEAD"

// reapDetachedRigWorktrees reclaims detached-HEAD git worktrees that sit
// directly under a rig's own worktrees directory (<rig path>/worktrees/<name>).
// These are the agent-improvised deploy-*, review-* and mayor-* checkouts: no
// formula creates them and no bead owns them, so reapClosedBeadWorktrees (which
// resolves an owning closed bead from the path) never sees them.
//
// Scope is the boundary against its sibling, and is deliberately narrow:
//   - only direct children of <rig path>/worktrees, never a nested entry and
//     never anything under the per-bead .gc/worktrees tree;
//   - only worktrees whose HEAD is detached. A branch-attached entry there is
//     the closed-bead reaper's jurisdiction, so it is not even reported here.
//
// Eligibility is unconditional: there is no owning bead to consult and no
// borrow-veto. Safety comes from gates that all fail closed toward keeping the
// worktree:
//  1. Named agent-home directories are never removed.
//  2. Freshness quarantine: younger than cfg.Daemon.AutoReapRigWorktreesMinAge
//     protects, and an indeterminate age (the ".git" pointer file cannot be
//     stat'd) protects. This is the primary guard, since nothing else proves
//     the worktree is finished; its knob is independent of the closed-bead one.
//  3. Liveness: no live process cwd and no active-session working directory may
//     sit at or beneath the worktree, and an indeterminate liveness scan
//     protects every candidate.
//  4. Git state: no uncommitted changes, no commit reachable from no branch,
//     tag or remote-tracking ref (a commit made on a detached HEAD is exactly
//     that, and removing the checkout would orphan it), and no stash. The
//     stash probe is repo-wide, so a stash anywhere in the rig repository
//     protects every candidate in that rig. A failed probe protects.
//
// suspendedRigs names rigs to skip entirely. When dryRun is true the reaper
// performs all discovery and classification, reports what it would reap as
// bead.worktree.reap_skipped events, and removes nothing. Events reuse the
// closed-bead reaper's types with an empty bead_id, because no bead is
// involved. skips carries skip-reporting history across sweeps exactly as for
// reapClosedBeadWorktrees; a nil tracker reports every skip.
func reapDetachedRigWorktrees(
	cfg *config.City,
	suspendedRigs map[string]bool,
	liveSessionDirs []string,
	dryRun bool,
	rec events.Recorder,
	skips *reapSkipTracker,
	stderr io.Writer,
) reapReport {
	report := reapReport{DryRun: dryRun}
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil || len(cfg.Rigs) == 0 {
		return report
	}

	skips.beginPass()
	defer skips.endPass()

	sessionHomes := make(map[string]bool, len(cfg.Agents))
	for i := range cfg.Agents {
		if name := cfg.Agents[i].BindingQualifiedName(); name != "" {
			sessionHomes[name] = true
		}
	}

	live := collectLiveWorktreeStateFn()
	if live.scanned && live.source != "" && live.source != liveScanSourceProc {
		fmt.Fprintf(stderr, "reapDetachedRigWorktrees: liveness scanned via %s (/proc unavailable)\n", live.source) //nolint:errcheck
	}
	minAge := cfg.Daemon.AutoReapRigWorktreesMinAge()

	for i := range cfg.Rigs {
		rigName := cfg.Rigs[i].Name
		if suspendedRigs[rigName] {
			continue
		}
		rigRoot := strings.TrimSpace(cfg.Rigs[i].Path)
		if rigRoot == "" {
			// No filesystem path: the owning repository cannot be resolved,
			// so nothing can be safely enumerated or removed.
			continue
		}
		worktreesDir := filepath.Join(rigRoot, rigWorktreesDirName)

		discovered, err := discoverWorktreeLiveness(rigRoot, live, liveSessionDirs)
		if err != nil {
			fmt.Fprintf(stderr, "reapDetachedRigWorktrees: listing worktrees for rig %s (%s): %v\n", rigName, rigRoot, err) //nolint:errcheck
			continue
		}

		for _, wl := range discovered {
			worktreePath := wl.Path

			// Direct children of <rig>/worktrees only.
			if !pathutil.SamePath(filepath.Dir(worktreePath), worktreesDir) {
				continue
			}
			if sessionHomes[filepath.Base(worktreePath)] {
				continue
			}

			// Detached only. Branch-attached entries belong to the
			// closed-bead reaper; a branch probe that errors cannot prove
			// the entry is detached, so it is left alone.
			branch, err := git.New(worktreePath).CurrentBranch()
			if err != nil {
				fmt.Fprintf(stderr, "reapDetachedRigWorktrees: reading HEAD of %s: %v\n", worktreePath, err) //nolint:errcheck
				continue
			}
			if branch != detachedHeadBranch {
				continue
			}

			reason := rigWorktreeProtectionReason(worktreePath, wl, live, minAge)
			if reason != "" {
				if skips.shouldSurface(worktreePath, reason) {
					fmt.Fprintf(stderr, "reapDetachedRigWorktrees: protecting %s (%s)\n", worktreePath, reason) //nolint:errcheck
					recordReapSkipped(rec, "", worktreePath, rigName, reason)
				}
				report.Protected = append(report.Protected, reapDecision{Path: worktreePath, Rig: rigName, Reason: reason})
				continue
			}

			if dryRun {
				const whatIf = "dry-run: would reap (detached HEAD, clean tree, no live process)"
				if skips.shouldSurface(worktreePath, whatIf) {
					fmt.Fprintf(stderr, "reapDetachedRigWorktrees: %s: %s\n", whatIf, worktreePath) //nolint:errcheck
					recordReapSkipped(rec, "", worktreePath, rigName, whatIf)
				}
				report.Reaped = append(report.Reaped, reapDecision{Path: worktreePath, Rig: rigName})
				continue
			}

			// git worktree remove must run from the owning repository, not
			// from inside the worktree being removed.
			if err := git.New(rigRoot).WorktreeRemove(worktreePath, false); err != nil {
				fmt.Fprintf(stderr, "reapDetachedRigWorktrees: removing %s: %v\n", worktreePath, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, "reapDetachedRigWorktrees: removed detached worktree %s\n", worktreePath) //nolint:errcheck
			if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{
				Path: worktreePath,
				Rig:  rigName,
			}); err == nil {
				rec.Record(events.Event{
					Type:    events.BeadWorktreeReaped,
					Actor:   "gc",
					Subject: filepath.Base(worktreePath),
					Payload: raw,
				})
			}
			report.Reaped = append(report.Reaped, reapDecision{Path: worktreePath, Rig: rigName})
		}
	}
	return report
}

// rigWorktreeProtectionReason returns why worktreePath must be kept, or "" when
// every gate clears. Gates run cheapest-first and the first to fire wins, so a
// protected tree costs no git probe once it is known to be young or live.
func rigWorktreeProtectionReason(worktreePath string, wl worktreeLiveness, live liveWorktreeState, minAge time.Duration) string {
	age, ok := computeWorktreeAge(worktreePath)
	switch {
	case !ok:
		return "worktree age indeterminate (failing closed)"
	case age < minAge:
		return fmt.Sprintf("worktree too young to reap (quarantine): min_age=%s", minAge)
	}

	if !live.scanned {
		return "liveness scan unavailable (failing closed, protecting all)"
	}
	if wl.Live {
		return "live: " + wl.Reason
	}

	// A probe error protects the tree: an errored probe proves nothing, and
	// reading it as a clean answer would fail open.
	wg := git.New(worktreePath)
	hasUncommitted := wg.HasUncommittedWork()
	hasUnreachable, unreachableErr := wg.HasUnreachableCommitsResult()
	if unreachableErr != nil {
		return fmt.Sprintf("git probe failed (failing closed): %v", unreachableErr)
	}
	hasStash, stashErr := wg.HasStashesResult()
	if stashErr != nil {
		return fmt.Sprintf("git probe failed (failing closed): %v", stashErr)
	}
	if hasUncommitted || hasUnreachable || hasStash {
		return fmt.Sprintf("unsafe git state: uncommitted=%v unreachable=%v stashes=%v", hasUncommitted, hasUnreachable, hasStash)
	}
	return ""
}
