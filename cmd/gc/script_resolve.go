package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// pruneLegacyConfiguredScripts removes symlink-only top-level scripts/
// directories left behind by the old ResolveScripts compatibility shim.
// Real user-authored files are preserved.
func pruneLegacyConfiguredScripts(cityPath string, cfg *config.City, handleErr func(scope string, err error)) {
	cityOrigins := legacyScriptOriginsForScope(cityPath, cfg.PackDirs)
	if err := pruneLegacyScripts(cityPath, cityOrigins, cityPath); err != nil {
		handleErr("city", err)
	}
	for _, r := range cfg.Rigs {
		rigPath := strings.TrimSpace(r.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		rigOrigins := append([]string{}, cityOrigins...)
		rigOrigins = append(rigOrigins, legacyScriptOriginsForScope(rigPath, cfg.RigPackDirs[r.Name])...)
		if err := pruneLegacyScripts(rigPath, rigOrigins, rigPath, cityPath); err != nil {
			handleErr(fmt.Sprintf("rig %q", r.Name), err)
		}
	}
}

// pruneLegacyScripts removes a top-level scripts/ directory only when it
// exactly matches the absolute symlink tree the old ResolveScripts shim would
// have generated from the legacy origins still represented in the current
// tree. Real files, foreign symlinks, or user-managed symlink layouts that
// merely point into pack script directories are preserved as user-owned.
func pruneLegacyScripts(targetDir string, legacySourceDirs []string, fallbackRoots ...string) error {
	symlinks, ok, err := legacyShimLinks(targetDir, legacySourceDirs, fallbackRoots...)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	for _, path := range symlinks {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy script symlink %q: %w", path, err)
		}
	}
	return removeEmptyDirsInclusive(filepath.Join(targetDir, "scripts"))
}

// legacyShimLinks returns the legacy top-level scripts/ symlinks for targetDir
// only when the tree is entirely composed of the exact absolute winner links
// that the old ResolveScripts compatibility shim would have emitted for the
// legacy origins still backing the observed tree. Any real file, relative
// symlink, foreign symlink, or user-managed relayout preserves the tree as
// user-owned.
func legacyShimLinks(targetDir string, legacySourceDirs []string, fallbackRoots ...string) ([]string, bool, error) {
	return legacyShimLinksFS(targetDir, legacySourceDirs, fsys.OSFS{}, fallbackRoots...)
}

func legacyShimLinksFS(targetDir string, legacySourceDirs []string, sourceFS fsys.FS, fallbackRoots ...string) ([]string, bool, error) {
	scriptsDir := filepath.Join(targetDir, "scripts")
	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("stat %q: %w", scriptsDir, err)
	}
	winners, err := legacyShimWinnersFS(sourceFS, legacySourceDirs)
	if err != nil {
		return nil, false, err
	}

	var symlinks []string
	var sawReal bool
	usedOrigins := make(map[string]struct{}, len(legacySourceDirs))
	fallbackRoots = normalizedFallbackRoots(targetDir, fallbackRoots)
	err = filepath.WalkDir(scriptsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, lErr := os.Lstat(path)
		if lErr != nil {
			return lErr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			rel, rErr := filepath.Rel(scriptsDir, path)
			if rErr != nil {
				return rErr
			}
			target, matchedWinner := legacyWinnerTarget(path, rel, winners)
			matchesLegacy := matchedWinner ||
				(len(winners) == 0 && symlinkMatchesLegacyShape(path, scriptsDir, rel, fallbackRoots))
			if !matchesLegacy {
				sawReal = true
				return nil
			}
			if matchedWinner {
				origin := legacySourceDirForTarget(target, legacySourceDirs)
				if origin == "" {
					sawReal = true
					return nil
				}
				usedOrigins[origin] = struct{}{}
			}
			symlinks = append(symlinks, path)
			return nil
		}
		sawReal = true
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("walking %q: %w", scriptsDir, err)
	}
	if sawReal || len(symlinks) == 0 {
		return nil, false, nil
	}
	if len(winners) == 0 {
		return symlinks, true, nil
	}

	scopedOrigins := filterLegacyOrigins(legacySourceDirs, usedOrigins)
	scopedWinners, err := legacyShimWinnersFS(sourceFS, scopedOrigins)
	if err != nil {
		return nil, false, err
	}
	if len(symlinks) != len(scopedWinners) {
		return nil, false, nil
	}
	return symlinks, true, nil
}

func legacyShimWinnersFS(fs fsys.FS, legacySourceDirs []string) (map[string]string, error) {
	winners := make(map[string]string)
	for _, layerDir := range legacySourceDirs {
		if err := walkFilesFS(fs, layerDir, func(path string) error {
			rel, err := filepath.Rel(layerDir, path)
			if err != nil {
				return nil
			}
			winners[rel] = filepath.Clean(path)
			return nil
		}); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("walking legacy script origin %q: %w", layerDir, err)
		}
	}
	return winners, nil
}

func walkFilesFS(fs fsys.FS, dir string, visit func(path string) error) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			if err := walkFilesFS(fs, path, visit); err != nil {
				return err
			}
			continue
		}
		if err := visit(path); err != nil {
			return err
		}
	}
	return nil
}

func normalizedFallbackRoots(targetDir string, fallbackRoots []string) []string {
	if len(fallbackRoots) == 0 {
		return []string{filepath.Clean(targetDir)}
	}
	seen := make(map[string]struct{}, len(fallbackRoots))
	var cleaned []string
	for _, root := range fallbackRoots {
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		cleaned = append(cleaned, root)
	}
	return cleaned
}

func legacyScriptSourceDirs(packDirs []string) []string {
	return legacyScriptSourceDirsFS(fsys.OSFS{}, packDirs)
}

func legacyScriptOriginsForScope(scopeRoot string, packDirs []string) []string {
	dirs := legacyScriptSourceDirs(packDirs)
	if len(dirs) > 0 {
		return dirs
	}
	return legacyLocalScriptOrigins(scopeRoot)
}

func legacyScriptSourceDirsFS(fs fsys.FS, packDirs []string) []string {
	if len(packDirs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(packDirs)*2)
	var dirs []string
	for _, packDir := range packDirs {
		for _, rel := range []string{"scripts", filepath.Join("assets", "scripts")} {
			dir := filepath.Join(packDir, rel)
			info, err := fs.Stat(dir)
			if err != nil || !info.IsDir() {
				continue
			}
			dir = filepath.Clean(dir)
			if _, ok := seen[dir]; ok {
				continue
			}
			seen[dir] = struct{}{}
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func legacyLocalScriptOrigins(scopeRoot string) []string {
	return legacyLocalScriptOriginsFS(fsys.OSFS{}, scopeRoot)
}

func legacyLocalScriptOriginsFS(fs fsys.FS, scopeRoot string) []string {
	dir := filepath.Join(scopeRoot, "assets", "scripts")
	info, err := fs.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	return []string{filepath.Clean(dir)}
}

func legacyWinnerTarget(linkPath, rel string, winners map[string]string) (string, bool) {
	if len(winners) == 0 {
		return "", false
	}
	wantTarget, ok := winners[rel]
	if !ok {
		return "", false
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		return "", false
	}
	target = filepath.Clean(target)
	return target, target == wantTarget
}

func legacySourceDirForTarget(target string, legacySourceDirs []string) string {
	target = filepath.Clean(target)
	var best string
	for _, dir := range legacySourceDirs {
		dir = filepath.Clean(dir)
		if target != dir && !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
			continue
		}
		if len(dir) > len(best) {
			best = dir
		}
	}
	return best
}

func filterLegacyOrigins(legacySourceDirs []string, usedOrigins map[string]struct{}) []string {
	if len(usedOrigins) == 0 {
		return nil
	}
	origins := make([]string, 0, len(usedOrigins))
	for _, dir := range legacySourceDirs {
		dir = filepath.Clean(dir)
		if _, ok := usedOrigins[dir]; ok {
			origins = append(origins, dir)
		}
	}
	return origins
}

func symlinkMatchesLegacyShape(linkPath, scriptsDir, rel string, fallbackRoots []string) bool {
	target, err := os.Readlink(linkPath)
	if err != nil || !filepath.IsAbs(target) {
		return false
	}
	target = filepath.Clean(target)
	scriptsDir = filepath.Clean(scriptsDir)
	rel = filepath.Clean(rel)
	suffix := string(os.PathSeparator) + rel
	if !strings.HasSuffix(target, suffix) {
		return false
	}
	origin := filepath.Clean(strings.TrimSuffix(target, suffix))
	if origin == scriptsDir || strings.HasPrefix(origin, scriptsDir+string(os.PathSeparator)) {
		return false
	}
	var underAllowedRoot bool
	for _, root := range fallbackRoots {
		if origin == root || strings.HasPrefix(origin, root+string(os.PathSeparator)) {
			underAllowedRoot = true
			break
		}
	}
	if !underAllowedRoot {
		return false
	}
	return filepath.Base(origin) == "scripts"
}

// removeEmptyDirsInclusive removes empty directories bottom-up, including root.
func removeEmptyDirsInclusive(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("walking empty-dir cleanup for %q: %w", root, err)
	}

	// Process deepest first.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !os.IsNotExist(err) && !isDirectoryNotEmpty(err) {
			return fmt.Errorf("removing empty dir %q: %w", dirs[i], err)
		}
	}
	return nil
}

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}
