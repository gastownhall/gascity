package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
)

const worktreeStaleFileName = ".worktree-stale"

// agentWorktreeGitProbe is the subset of git.Git used by
// cleanupClosedBeadAgentHomeWorktrees. Defined as an interface so tests can
// inject a fake without standing up real git worktrees.
type agentWorktreeGitProbe interface {
	IsRepo() bool
	CurrentBranch() (string, error)
	HasUncommittedWork() bool
	CheckoutDetach(ref string) error
	DefaultBranch() (string, error)
}

// newAgentWorktreeGitProbe is the factory for the git probe. Tests may
// replace this var to inject a fake implementation.
var newAgentWorktreeGitProbe = func(workDir string) agentWorktreeGitProbe {
	return git.New(workDir)
}

// cleanupClosedBeadAgentHomeWorktrees scans the named-session (agent home)
// worktrees for each rig and cleans up stale .worktree-stale markers:
//
//   - Case A: worktree is already detached (CurrentBranch == "HEAD") →
//     remove the marker unconditionally (no rebase can be needed).
//   - Case B: worktree is on a branch whose bead ID is confirmed closed →
//     reset to detached origin/main and remove the marker, provided the
//     working tree has no uncommitted changes.
//
// Per-bead worktrees (directories whose names do not match a session home)
// are skipped — the bead_worktree_reaper handles those.
// Returns the number of worktrees cleaned.
func cleanupClosedBeadAgentHomeWorktrees(
	cityPath string,
	cfg *config.City,
	rigBeadStores map[string]beads.Store,
	stderr io.Writer,
) int {
	if stderr == nil {
		stderr = io.Discard
	}
	if cfg == nil || len(rigBeadStores) == 0 {
		return 0
	}

	sessionHomes := make(map[string]bool, len(cfg.Agents))
	for i := range cfg.Agents {
		if name := cfg.Agents[i].BindingQualifiedName(); name != "" {
			sessionHomes[name] = true
		}
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")
	cleaned := 0

	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigWorktreeDir := filepath.Join(wtRoot, rigName)
		entries, err := os.ReadDir(rigWorktreeDir)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "cleanupClosedBeadAgentHomeWorktrees: reading %s: %v\n", rigWorktreeDir, err) //nolint:errcheck
			}
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !sessionHomes[name] {
				continue
			}

			worktreePath := filepath.Join(rigWorktreeDir, name)
			stalePath := filepath.Join(worktreePath, worktreeStaleFileName)
			if _, err := os.Stat(stalePath); err != nil {
				continue
			}

			wg := newAgentWorktreeGitProbe(worktreePath)
			if !wg.IsRepo() {
				continue
			}

			branch, err := wg.CurrentBranch()
			if err != nil {
				continue
			}

			// Case A: already detached — the stale marker is false, remove it.
			if branch == "HEAD" {
				if removeErr := os.Remove(stalePath); removeErr == nil {
					fmt.Fprintf(stderr, "cleanupClosedBeadAgentHomeWorktrees: removed false .worktree-stale from %s (already detached)\n", worktreePath) //nolint:errcheck
					cleaned++
				}
				continue
			}

			// Case B: on a named branch — check whether its bead is closed.
			beadID := beadIDFromBranch(cfg, branch)
			if beadID == "" {
				continue
			}
			bead, err := store.Get(beadID)
			if err != nil || bead.Status != "closed" {
				continue
			}

			// Safety: never reset a worktree that has uncommitted work.
			if wg.HasUncommittedWork() {
				fmt.Fprintf(stderr, "cleanupClosedBeadAgentHomeWorktrees: skipping %s: bead %s closed but has uncommitted work\n", worktreePath, beadID) //nolint:errcheck
				continue
			}

			defaultBranch, err := wg.DefaultBranch()
			if err != nil || strings.TrimSpace(defaultBranch) == "" {
				defaultBranch = "main"
			}
			resetRef := "origin/" + defaultBranch
			if err := wg.CheckoutDetach(resetRef); err != nil {
				fmt.Fprintf(stderr, "cleanupClosedBeadAgentHomeWorktrees: resetting %s to %s: %v\n", worktreePath, resetRef, err) //nolint:errcheck
				continue
			}
			if removeErr := os.Remove(stalePath); removeErr != nil && !os.IsNotExist(removeErr) {
				fmt.Fprintf(stderr, "cleanupClosedBeadAgentHomeWorktrees: removing stale marker from %s: %v\n", worktreePath, removeErr) //nolint:errcheck
			}
			fmt.Fprintf(stderr, "cleanupClosedBeadAgentHomeWorktrees: reset %s to %s (bead %s closed)\n", worktreePath, resetRef, beadID) //nolint:errcheck
			cleaned++
		}
	}
	return cleaned
}

// beadIDFromBranch extracts a bead ID from a branch name of the form
// "<agent>/<beadID-slug>" or bare "<beadID>". Returns "" when the branch
// contains no valid configured bead ID.
func beadIDFromBranch(cfg *config.City, branch string) string {
	if branch == "" || branch == "HEAD" {
		return ""
	}
	// Strip optional leading agent-name segment (e.g. "builder/ga-abc123" → "ga-abc123").
	suffix := branch
	for i := 0; i < len(branch); i++ {
		if branch[i] == '/' {
			suffix = branch[i+1:]
			break
		}
	}
	return extractBeadIDFromWorktreeName(cfg, suffix)
}
