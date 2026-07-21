package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/sling"
)

// reapClosedBeadWorktrees scans per-bead git worktrees under
// cityPath/.gc/worktrees/<rig>/ and removes any that are associated with a
// closed bead and pass all safety gates (no uncommitted work, no unpushed
// commits, no stashes). Named session home directories are never removed.
// Returns the number of worktrees successfully removed.
func reapClosedBeadWorktrees(
	cityPath string,
	cfg *config.City,
	rigBeadStores map[string]beads.Store,
	rec events.Recorder,
	stderr io.Writer,
) int {
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil || len(rigBeadStores) == 0 {
		return 0
	}

	// Build a guard set of session home names so agent template directories
	// are never touched.
	sessionHomes := make(map[string]bool, len(cfg.Agents))
	for i := range cfg.Agents {
		if name := cfg.Agents[i].BindingQualifiedName(); name != "" {
			sessionHomes[name] = true
		}
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")
	reaped := 0

	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigRoot := rigRootForName(rigName, cfg.Rigs)
		rigWorktreeDir := filepath.Join(wtRoot, rigName)
		entries, err := os.ReadDir(rigWorktreeDir)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: reading %s: %v\n", rigWorktreeDir, err) //nolint:errcheck
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()

			// Session home guard: never touch agent template directories.
			if sessionHomes[name] {
				continue
			}

			// Extract a bead ID candidate from the directory name.
			beadID := extractBeadIDFromWorktreeName(cfg, name)
			if beadID == "" {
				continue
			}

			// Confirm the bead exists and is closed in this rig's store.
			bead, err := store.Get(beadID)
			if err != nil || bead.Status != "closed" {
				// ErrNotFound, transient error, or bead not yet closed — skip.
				continue
			}

			worktreePath := filepath.Join(rigWorktreeDir, name)

			// Scope gate: only act on paths strictly under the worktree root.
			if !isStrictlyUnderDir(wtRoot, worktreePath) {
				continue
			}

			// Safety checks: run from the worktree directory so git status
			// and stash list apply to the worktree's branch. A probe error
			// is treated the same as an unsafe result (fail closed) — an
			// unreadable probe gives no evidence the tree is safe to reap.
			wg := newGitProbe(worktreePath)
			hasUncommitted := wg.HasUncommittedWork()
			hasUnpushed, unpushedErr := wg.HasUnpushedCommitsResult()
			hasStashes, stashesErr := wg.HasStashesResult()

			if hasUncommitted || hasUnpushed || hasStashes || unpushedErr != nil || stashesErr != nil {
				reason := fmt.Sprintf(
					"uncommitted=%v unpushed=%v (err=%v) stashes=%v (err=%v)",
					hasUncommitted, hasUnpushed, unpushedErr, hasStashes, stashesErr,
				)
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: skipping %s (bead %s closed but unsafe: %s)\n",
					worktreePath, beadID, reason,
				)
				if raw, err := json.Marshal(events.BeadWorktreeReapSkippedPayload{
					BeadID: beadID,
					Path:   worktreePath,
					Rig:    rigName,
					Reason: reason,
				}); err == nil {
					rec.Record(events.Event{
						Type:    events.BeadWorktreeReapSkipped,
						Actor:   "gc",
						Subject: beadID,
						Payload: raw,
					})
				}
				continue
			}

			// Capture branch before removal — the worktree dir will be gone after.
			branch, _ := wg.CurrentBranch()

			if rigRoot == "" {
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: skipping %s (bead %s closed but rig %q path unresolved)\n",
					worktreePath, beadID, rigName,
				)
				continue
			}

			// Remove the worktree. git worktree remove must be run from the
			// rig's own repo root, not from the city root — a rig is an
			// external repository with its own .git, distinct from the
			// city's (mirrors session_worktree_prune.go's rig-root pattern).
			if err := newGitProbe(rigRoot).WorktreeRemove(worktreePath, false); err != nil {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: removing %s: %v\n", worktreePath, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, //nolint:errcheck
				"reapClosedBeadWorktrees: removed worktree %s for closed bead %s\n",
				worktreePath, beadID,
			)
			if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{
				BeadID: beadID,
				Path:   worktreePath,
				Rig:    rigName,
				Branch: branch,
			}); err == nil {
				rec.Record(events.Event{
					Type:    events.BeadWorktreeReaped,
					Actor:   "gc",
					Subject: beadID,
					Payload: raw,
				})
			}
			reaped++
		}
	}
	return reaped
}

// extractBeadIDFromWorktreeName scans dash-separated segment windows in name,
// growing from each start position, for one that LooksLikeConfiguredBeadID.
// Returns the first match, or "" if none. A configured rig prefix may itself
// be hyphenated (e.g. rig "agent-diagnostics"), so a fixed two-segment window
// is not enough — the window must grow to cover multi-segment prefixes.
// Handles names like "builder-ga-34q3ss-pr2738" → "ga-34q3ss",
// "agent-diagnostics-h1" → "agent-diagnostics-h1" (hyphenated rig prefix),
// and bare "ga-06kfi6" → "ga-06kfi6".
func extractBeadIDFromWorktreeName(cfg *config.City, name string) string {
	if name == "" || cfg == nil {
		return ""
	}
	parts := strings.Split(name, "-")
	for i := 0; i < len(parts); i++ {
		for j := i + 1; j < len(parts); j++ {
			candidate := strings.Join(parts[i:j+1], "-")
			if sling.LooksLikeConfiguredBeadID(cfg, candidate) {
				return candidate
			}
		}
	}
	return ""
}

// isStrictlyUnderDir reports whether path is strictly contained within dir
// (i.e., it is not dir itself and has dir as a prefix component).
func isStrictlyUnderDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}
