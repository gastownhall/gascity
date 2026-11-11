package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
)

// worktreeDriftGitProbe is the subset of git.Git used by
// patrolCommitClassWorktreeDrift. Defined as an interface so tests can
// inject a fake without standing up real git worktrees.
type worktreeDriftGitProbe interface {
	IsRepo() bool
	CurrentBranch() (string, error)
	HasUncommittedWork() bool
	DefaultBranch() (string, error)
	AheadBehindRef(ref string) (ahead, behind int, err error)
	InProgressOperation() bool
}

// newWorktreeDriftGitProbe is the factory for the git probe. Tests may
// replace this var to inject a fake implementation.
var newWorktreeDriftGitProbe = func(workDir string) worktreeDriftGitProbe {
	return git.New(workDir)
}

// worktreeDriftBeadType identifies the single reusable tracking bead each
// commit-class agent gets for drift-observation bookkeeping. Exactly one
// bead per configured agent identity is ever created — it is reused
// indefinitely across observe/resolve cycles, never one-per-incident, so
// this cannot grow unbounded with fleet uptime.
const worktreeDriftBeadType = "worktree_drift"

const (
	worktreeDriftMetaIdentity        = "identity"
	worktreeDriftMetaFirstObservedAt = "first_observed_at"
	worktreeDriftMetaStallEventFired = "stall_event_fired"
)

// patrolCommitClassWorktreeDrift is an independent, read-only health-patrol
// check for commit-class agents' persistent worktrees. Unlike the
// pre_start --freshen-commit check (which only ever runs when a new tmux
// session starts), this sweep runs on the controller's own tick regardless
// of whether any session has run recently — closing the gap where a
// zero-active-session pool (e.g. builder, min_active_sessions=0) can drift
// unnoticed indefinitely. See ga-6prf1p.
//
// For each commit-class agent this computes ahead/behind vs. the resolved
// default branch and detached-HEAD state, tracks how long drift has
// persisted using a single reusable Type:"worktree_drift" bead per agent
// identity (agent.QualifiedName()), and publishes a WorktreeDriftStalled
// event once per observation window when the configured threshold is
// exceeded. The tracking bead lives in store (the city-level store — this
// is fleet-wide housekeeping, not scoped to any one rig's work items) so
// dedup state survives controller restarts: the system converges because
// work persists.
//
// Strictly read-only: never fetches, rebases, or checks out. AheadBehindRef
// compares two already-resolved local refs (HEAD and origin/<default>), so
// this performs no network I/O at all — the "don't hammer origin" rate-limit
// requirement is satisfied by construction, not by added throttling.
//
// Skips (rather than flags) any worktree with an in-progress git operation
// (InProgressOperation) or uncommitted work: both indicate either a
// concurrently-starting session's own rebase/reset (TOCTOU — treat
// ambiguity as "skip this interval," not "act") or a human actively
// debugging (grace window). Ambiguous state must not be treated as a
// stall, and any in-flight observation window is left untouched rather
// than cleared, so a transient conflict cannot reset the clock.
//
// Returns the number of newly-fired stall events.
func patrolCommitClassWorktreeDrift(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	threshold time.Duration,
	rec events.Recorder,
	now time.Time,
	stderr io.Writer,
) int {
	if stderr == nil {
		stderr = io.Discard
	}
	if cfg == nil || store == nil || threshold <= 0 {
		return 0
	}
	if rec == nil {
		rec = events.Discard
	}

	fired := 0
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if !agent.IsCommitClass() {
			continue
		}
		identity := agent.QualifiedName()
		if identity == "" {
			continue
		}

		worktreePath := filepath.Join(cityPath, ".gc", "worktrees", agent.Dir, agent.BindingQualifiedName())
		if _, err := os.Stat(worktreePath); err != nil {
			// No worktree materialized yet — nothing to observe.
			continue
		}

		wg := newWorktreeDriftGitProbe(worktreePath)
		if !wg.IsRepo() {
			continue
		}

		// TOCTOU guard + grace window: an in-progress operation or
		// uncommitted work means this read could observe a transient,
		// about-to-change state (a concurrently-starting session's own
		// rebase, or a human mid-debug). Skip the interval entirely
		// rather than act on ambiguous state.
		if wg.InProgressOperation() || wg.HasUncommittedWork() {
			continue
		}

		branch, err := wg.CurrentBranch()
		if err != nil {
			continue
		}
		detached := branch == "HEAD"

		defaultBranch, err := wg.DefaultBranch()
		if err != nil || strings.TrimSpace(defaultBranch) == "" {
			defaultBranch = "main"
		}
		ahead, behind, err := wg.AheadBehindRef("origin/" + defaultBranch)
		if err != nil {
			// origin/<default> isn't locally resolvable (e.g. never
			// fetched) — nothing to compare read-only, skip.
			continue
		}

		if !detached && behind == 0 {
			clearWorktreeDriftObservation(store, identity, stderr)
			continue
		}

		b, err := getOrCreateWorktreeDriftBead(store, identity)
		if err != nil {
			fmt.Fprintf(stderr, "patrolCommitClassWorktreeDrift: tracking bead for %s: %v\n", identity, err) //nolint:errcheck
			continue
		}

		firstObservedAt := parseMetadataTime(b.Metadata[worktreeDriftMetaFirstObservedAt])
		if firstObservedAt.IsZero() {
			firstObservedAt = now
			if err := store.SetMetadata(b.ID, worktreeDriftMetaFirstObservedAt, now.Format(time.RFC3339)); err != nil {
				fmt.Fprintf(stderr, "patrolCommitClassWorktreeDrift: recording first observation for %s: %v\n", identity, err) //nolint:errcheck
				continue
			}
		}

		if b.Metadata[worktreeDriftMetaStallEventFired] == "true" {
			continue
		}

		elapsed := now.Sub(firstObservedAt)
		if elapsed < threshold {
			continue
		}

		rec.Record(events.Event{
			Type:    events.WorktreeDriftStalled,
			Actor:   "gc",
			Subject: identity,
			Message: fmt.Sprintf("commit-class worktree %s has been drifted for %s with no session to correct it", worktreePath, elapsed.Round(time.Second)),
			Payload: events.WorktreeDriftStalledPayloadJSON(identity, worktreePath, detached, branch, ahead, behind, firstObservedAt.Format(time.RFC3339), int(elapsed.Seconds())),
		})
		if err := store.SetMetadata(b.ID, worktreeDriftMetaStallEventFired, "true"); err != nil {
			fmt.Fprintf(stderr, "patrolCommitClassWorktreeDrift: marking stall fired for %s: %v\n", identity, err) //nolint:errcheck
			continue
		}
		fired++
	}
	return fired
}

// getOrCreateWorktreeDriftBead returns the single reusable worktree_drift
// tracking bead for identity, creating it if this is the first time drift
// has ever been observed for that agent.
func getOrCreateWorktreeDriftBead(store beads.Store, identity string) (beads.Bead, error) {
	existing, err := store.List(beads.ListQuery{
		Type:     worktreeDriftBeadType,
		Metadata: map[string]string{worktreeDriftMetaIdentity: identity},
	})
	if err != nil {
		return beads.Bead{}, err
	}
	if len(existing) > 0 {
		return existing[0], nil
	}
	return store.Create(beads.Bead{
		Title: fmt.Sprintf("worktree drift tracking: %s", identity),
		Type:  worktreeDriftBeadType,
		Metadata: map[string]string{
			worktreeDriftMetaIdentity: identity,
		},
	})
}

// clearWorktreeDriftObservation resets the drift-tracking bead's
// observation window when a previously-drifted worktree is found no longer
// drifted. A no-op when no tracking bead exists yet, since a worktree that
// has never drifted has nothing to clear.
func clearWorktreeDriftObservation(store beads.Store, identity string, stderr io.Writer) {
	existing, err := store.List(beads.ListQuery{
		Type:     worktreeDriftBeadType,
		Metadata: map[string]string{worktreeDriftMetaIdentity: identity},
	})
	if err != nil {
		fmt.Fprintf(stderr, "patrolCommitClassWorktreeDrift: listing tracking bead for %s: %v\n", identity, err) //nolint:errcheck
		return
	}
	if len(existing) == 0 {
		return
	}
	b := existing[0]
	if b.Metadata[worktreeDriftMetaFirstObservedAt] == "" && b.Metadata[worktreeDriftMetaStallEventFired] == "" {
		return // already clear
	}
	if err := store.SetMetadata(b.ID, worktreeDriftMetaFirstObservedAt, ""); err != nil {
		fmt.Fprintf(stderr, "patrolCommitClassWorktreeDrift: clearing observation for %s: %v\n", identity, err) //nolint:errcheck
		return
	}
	if err := store.SetMetadata(b.ID, worktreeDriftMetaStallEventFired, ""); err != nil {
		fmt.Fprintf(stderr, "patrolCommitClassWorktreeDrift: clearing stall flag for %s: %v\n", identity, err) //nolint:errcheck
	}
}

// parseMetadataTime parses an RFC3339 timestamp stored in bead metadata,
// returning the zero time for an empty or malformed value.
func parseMetadataTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
