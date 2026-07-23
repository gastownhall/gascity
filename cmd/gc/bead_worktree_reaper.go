package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/sling"
)

// beadWorktreeGitProbe is the subset of git.Git used while deciding whether
// a closed bead's worktree can be removed. Keeping the probes behind an
// interface lets tests verify that any inconclusive result fails closed.
type beadWorktreeGitProbe interface {
	IsRepo() bool
	CurrentBranch() (string, error)
	HasUncommittedWork() bool
	HasUnpushedCommitsResult() (bool, error)
	HasStashesResult() (bool, error)
	WorktreeRemove(path string, force bool) error
}

var newBeadWorktreeGitProbe = func(workDir string) beadWorktreeGitProbe {
	return git.New(workDir)
}

// reapClosedBeadWorktrees scans per-bead git worktrees under
// both the legacy cityPath/.gc/worktrees/<rig>/ layout and the canonical
// cityPath/.gc/worktrees/<rig>/artifacts/worktrees/ layout. It removes any
// worktrees associated with a closed bead that pass all safety gates (valid
// git repository, no uncommitted work, no unpushed commits, and no stashes).
// Named session home directories are never removed. Returns the number of
// worktrees successfully removed.
func reapClosedBeadWorktrees(
	cityPath string,
	cfg *config.City,
	cityBeadStore beads.Store,
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
	resolvedWtRoot, err := resolveWorktreeRoot(cityPath, wtRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: resolving worktree root %s: %v\n", wtRoot, err) //nolint:errcheck
		}
		return 0
	}
	reaped := 0

	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigWorktreeDir := filepath.Join(wtRoot, rigName)
		scanDirs := []string{
			rigWorktreeDir,
			filepath.Join(rigWorktreeDir, "artifacts", "worktrees"),
		}
		for _, scanDir := range scanDirs {
			resolvedScanDir, err := resolveWorktreeScanDir(resolvedWtRoot, scanDir)
			if err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(stderr, "reapClosedBeadWorktrees: resolving scan directory %s: %v\n", scanDir, err) //nolint:errcheck
				}
				continue
			}
			entries, err := os.ReadDir(resolvedScanDir)
			if err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(stderr, "reapClosedBeadWorktrees: reading %s: %v\n", resolvedScanDir, err) //nolint:errcheck
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

				// Routed work can live in the HQ store even when its executor is
				// rig-scoped. Require exactly one authoritative row across the
				// HQ and owning-rig stores before treating the bead as closed.
				closed, lookupReason := closedBeadForWorktreeReap(cityBeadStore, store, beadID)
				if !closed {
					if lookupReason != "" {
						recordBeadWorktreeReapSkip(rec, stderr, beadID, rigName, filepath.Join(resolvedScanDir, name), lookupReason)
					}
					continue
				}

				worktreePath := filepath.Join(resolvedScanDir, name)

				// Scope gate: only act on paths strictly under the scanned
				// worktree directory and the city-wide worktree root.
				if !isStrictlyUnderDir(resolvedScanDir, worktreePath) || !isStrictlyUnderDir(resolvedWtRoot, worktreePath) {
					continue
				}

				// Safety checks run from the candidate worktree. Every probe
				// must positively succeed before removal.
				wg := newBeadWorktreeGitProbe(worktreePath)
				if !wg.IsRepo() {
					recordBeadWorktreeReapSkip(rec, stderr, beadID, rigName, worktreePath, "not a git repository")
					continue
				}
				hasUncommitted := wg.HasUncommittedWork()
				hasUnpushed, err := wg.HasUnpushedCommitsResult()
				if err != nil {
					recordBeadWorktreeReapSkip(rec, stderr, beadID, rigName, worktreePath, "unpushed probe failed: "+err.Error())
					continue
				}
				hasStashes, err := wg.HasStashesResult()
				if err != nil {
					recordBeadWorktreeReapSkip(rec, stderr, beadID, rigName, worktreePath, "stash probe failed: "+err.Error())
					continue
				}

				if hasUncommitted || hasUnpushed || hasStashes {
					reason := fmt.Sprintf("uncommitted=%v unpushed=%v stashes=%v", hasUncommitted, hasUnpushed, hasStashes)
					recordBeadWorktreeReapSkip(rec, stderr, beadID, rigName, worktreePath, reason)
					continue
				}

				// Capture branch before removal — the worktree dir will be gone
				// after. An inconclusive branch probe also holds the gate shut.
				branch, err := wg.CurrentBranch()
				if err != nil {
					recordBeadWorktreeReapSkip(rec, stderr, beadID, rigName, worktreePath, "branch probe failed: "+err.Error())
					continue
				}

				// Remove from the configured rig repository, not from the city
				// root or the worktree being removed.
				rigRepo := configuredRigRepoPath(cityPath, cfg, rigName)
				if rigRepo == "" {
					recordBeadWorktreeReapSkip(rec, stderr, beadID, rigName, worktreePath, "configured rig repository unresolved")
					continue
				}
				if err := newBeadWorktreeGitProbe(rigRepo).WorktreeRemove(worktreePath, false); err != nil {
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
	}
	return reaped
}

func recordBeadWorktreeReapSkip(
	rec events.Recorder,
	stderr io.Writer,
	beadID, rigName, worktreePath, reason string,
) {
	fmt.Fprintf(stderr, //nolint:errcheck
		"reapClosedBeadWorktrees: skipping %s (bead %s: %s)\n",
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
}

func configuredRigRepoPath(cityPath string, cfg *config.City, rigName string) string {
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName && strings.TrimSpace(cfg.Rigs[i].Path) != "" {
			return resolveStoreScopeRoot(cityPath, cfg.Rigs[i].Path)
		}
	}
	return ""
}

func resolveWorktreeRoot(cityPath, wtRoot string) (string, error) {
	resolvedCity, err := filepath.EvalSymlinks(cityPath)
	if err != nil {
		return "", fmt.Errorf("resolving city path: %w", err)
	}
	resolvedWtRoot, err := filepath.EvalSymlinks(wtRoot)
	if err != nil {
		return "", err
	}
	resolvedCity = filepath.Clean(resolvedCity)
	resolvedWtRoot = filepath.Clean(resolvedWtRoot)
	if !isStrictlyUnderDir(resolvedCity, resolvedWtRoot) {
		return "", fmt.Errorf("resolved worktree root %s escapes city %s", resolvedWtRoot, resolvedCity)
	}
	return resolvedWtRoot, nil
}

func resolveWorktreeScanDir(resolvedWtRoot, scanDir string) (string, error) {
	resolvedScanDir, err := filepath.EvalSymlinks(scanDir)
	if err != nil {
		return "", err
	}
	resolvedScanDir = filepath.Clean(resolvedScanDir)
	if !isStrictlyUnderDir(resolvedWtRoot, resolvedScanDir) {
		return "", fmt.Errorf("resolved scan directory %s escapes worktree root %s", resolvedScanDir, resolvedWtRoot)
	}
	return resolvedScanDir, nil
}

// closedBeadForWorktreeReap returns true only when exactly one authoritative
// bead row exists across the HQ and owning-rig stores and that row is closed.
// Duplicate rows are ambiguous even when they currently agree, so cleanup
// fails closed until the routing/store inconsistency is resolved.
func closedBeadForWorktreeReap(cityStore, rigStore beads.Store, beadID string) (bool, string) {
	stores := []struct {
		name  string
		store beads.Store
	}{
		{name: "HQ", store: cityStore},
	}
	if rigStore != cityStore {
		stores = append(stores, struct {
			name  string
			store beads.Store
		}{name: "rig", store: rigStore})
	}

	var found []beads.Bead
	for _, candidate := range stores {
		if candidate.store == nil {
			continue
		}
		bead, err := candidate.store.Get(beadID)
		switch {
		case err == nil:
			found = append(found, bead)
		case errors.Is(err, beads.ErrNotFound):
			continue
		default:
			return false, fmt.Sprintf("%s store probe failed: %v", candidate.name, err)
		}
	}
	if len(found) > 1 {
		return false, "duplicate bead rows found in HQ and rig stores"
	}
	if len(found) == 0 || found[0].Status != "closed" {
		return false, ""
	}
	return true, ""
}

// extractBeadIDFromWorktreeName scans dash-separated substrings in name for
// one that LooksLikeConfiguredBeadID. For each starting segment it keeps the
// longest valid candidate, so configured hyphenated prefixes are not
// truncated to a shorter prefix match. Returns the first positional match, or
// "" if none. Handles names like "builder-ga-34q3ss-pr2738" → "ga-34q3ss",
// "builder-agent-diagnostics-hnn" → "agent-diagnostics-hnn", and bare
// "ga-06kfi6" → "ga-06kfi6".
func extractBeadIDFromWorktreeName(cfg *config.City, name string) string {
	if name == "" || cfg == nil {
		return ""
	}
	parts := strings.Split(name, "-")
	for start := 0; start+1 < len(parts); start++ {
		longest := ""
		for end := start + 2; end <= len(parts); end++ {
			candidate := strings.Join(parts[start:end], "-")
			if sling.LooksLikeConfiguredBeadID(cfg, candidate) && len(candidate) > len(longest) {
				longest = candidate
			}
		}
		if longest != "" {
			return longest
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
