package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
)

// gitCommonDirLookupTimeout caps the subprocess git call used by
// rigFromGitWorktree so a hung git invocation cannot stall every
// `gc bd` command. Resolution falls back to the city scope on timeout.
const gitCommonDirLookupTimeout = 2 * time.Second

func rigForDir(cfg *config.City, cityPath, dir string) (config.Rig, bool) {
	rig, ok, _ := resolveRigForDir(cfg, cityPath, dir)
	return rig, ok
}

func resolveRigForDir(cfg *config.City, cityPath, dir string) (config.Rig, bool, error) {
	dir = normalizePathForCompare(dir)
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		rigPath := normalizePathForCompare(resolveStoreScopeRoot(cityPath, rig.Path))
		if pathWithinScope(dir, rigPath) {
			return rig, true, nil
		}
	}
	if rig, ok, err := rigFromRedirectedBeadsDir(cfg, cityPath, dir); ok || err != nil {
		return rig, ok, err
	}
	return rigFromGitWorktree(cfg, cityPath, dir)
}

func rigFromRedirectedBeadsDir(cfg *config.City, cityPath, dir string) (config.Rig, bool, error) {
	// Redirect resolution is meaningful only when cwd lies inside cityPath.
	// When tests or commands run with a cwd outside the declared city tree
	// (e.g., a polecat worktree under a different gc city), walking up the
	// cwd chain would pick up unrelated .beads/redirect files and either
	// mis-route the command or hard-error against the test's fake cfg.
	cityScope := normalizePathForCompare(cityPath)
	if !pathWithinScope(normalizePathForCompare(dir), cityScope) {
		return config.Rig{}, false, nil
	}
	for current := dir; current != "" && current != filepath.Dir(current); current = filepath.Dir(current) {
		if !pathWithinScope(normalizePathForCompare(current), cityScope) {
			break
		}
		redirectPath := filepath.Join(current, ".beads", "redirect")
		redirectTarget, err := os.ReadFile(redirectPath)
		if err != nil {
			continue
		}
		targetBeadsDir := normalizePathForCompare(strings.TrimSpace(string(redirectTarget)))
		if targetBeadsDir == "" {
			continue
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			rigBeadsDir := normalizePathForCompare(filepath.Join(resolveStoreScopeRoot(cityPath, rig.Path), ".beads"))
			if targetBeadsDir == rigBeadsDir {
				return rig, true, nil
			}
		}
		return config.Rig{}, false, fmt.Errorf("cwd redirect %s points outside declared city rigs", redirectPath)
	}
	return config.Rig{}, false, nil
}

func pathWithinScope(path, scopeRoot string) bool {
	if scopeRoot == "" {
		return false
	}
	if path == scopeRoot {
		return true
	}
	return len(path) > len(scopeRoot) && strings.HasPrefix(path, scopeRoot) && path[len(scopeRoot)] == '/'
}

// gitCommonDirForDir is the seam used to ask git for the worktree's main
// repository directory. Tests override this to avoid spawning a real git
// subprocess while still exercising rigFromGitWorktree's matching logic.
var gitCommonDirForDir = func(dir string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommonDirLookupTimeout)
	defer cancel()
	common, err := git.New(dir).CommonDirCtx(ctx)
	if err != nil {
		return "", false
	}
	return common, true
}

// rigFromGitWorktree resolves the rig by asking git for the main repository
// directory of the worktree containing dir, then matching that against
// configured rig.Path values. This is the safety net for callers (typically
// polecat worktrees) whose .beads/redirect was never created or has been
// removed: filesystem path heuristics alone cannot map an unrelated worktree
// location back to its rig, but git itself always knows.
//
// Returns false when dir is not inside any git repository, when git is
// unavailable, or when the resolved repository root does not match any
// configured rig.Path. The city's own checkout (when cityPath happens to
// be a git repo) is intentionally not treated as a rig — callers that want
// the HQ scope already fall through to bdCityScopeTarget.
func rigFromGitWorktree(cfg *config.City, cityPath, dir string) (config.Rig, bool, error) {
	commonDir, ok := gitCommonDirForDir(dir)
	if !ok {
		return config.Rig{}, false, nil
	}
	commonDir = normalizePathForCompare(commonDir)
	// git rev-parse --git-common-dir returns "<repo>/.git" for the standard
	// layout. The repository root is the parent directory; fall back to
	// commonDir itself for bare repos or unusual layouts where the basename
	// is not ".git".
	repoRoot := commonDir
	if filepath.Base(commonDir) == ".git" {
		repoRoot = filepath.Dir(commonDir)
	}
	repoRoot = normalizePathForCompare(repoRoot)

	cityRoot := normalizePathForCompare(cityPath)
	if repoRoot == cityRoot {
		// Skip the city checkout itself so HQ-rooted callers still fall
		// through to the city scope instead of being misidentified as a
		// rig with the same path.
		return config.Rig{}, false, nil
	}

	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		rigPath := normalizePathForCompare(resolveStoreScopeRoot(cityPath, rig.Path))
		if repoRoot == rigPath {
			return rig, true, nil
		}
	}
	return config.Rig{}, false, nil
}
