package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/config"
)

// pruneLegacyConfiguredScripts removes symlink-only top-level scripts/
// directories left behind by the old ResolveScripts compatibility shim.
// Real user-authored files are preserved.
func pruneLegacyConfiguredScripts(cityPath string, cfg *config.City, handleErr func(scope string, err error)) {
	cityOrigins := legacyScriptSourceDirs(cfg.PackDirs)
	if err := pruneLegacyScripts(cityPath, cityOrigins); err != nil {
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
		rigOrigins = append(rigOrigins, legacyScriptSourceDirs(cfg.RigPackDirs[r.Name])...)
		if err := pruneLegacyScripts(rigPath, rigOrigins); err != nil {
			handleErr(fmt.Sprintf("rig %q", r.Name), err)
		}
	}
}

// pruneLegacyScripts removes a top-level scripts/ directory only when it
// contains compatibility-shim symlinks and no real files. The symlinks must
// resolve into one of the legacy script source directories derived from the
// current pack graph; foreign symlink-only trees are preserved as user-owned.
func pruneLegacyScripts(targetDir string, legacySourceDirs []string) error {
	symlinks, ok, err := legacyShimLinks(targetDir, legacySourceDirs)
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
// only when the tree is entirely composed of shim symlinks backed by the
// supplied pack script origins. Any real file or foreign symlink preserves the
// tree as user-owned.
func legacyShimLinks(targetDir string, legacySourceDirs []string) ([]string, bool, error) {
	scriptsDir := filepath.Join(targetDir, "scripts")
	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("stat %q: %w", scriptsDir, err)
	}

	var symlinks []string
	var sawReal bool
	err := filepath.WalkDir(scriptsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, lErr := os.Lstat(path)
		if lErr != nil {
			return lErr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if !symlinkMatchesLegacySource(path, legacySourceDirs) {
				sawReal = true
				return nil
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
	return symlinks, true, nil
}

func legacyScriptSourceDirs(packDirs []string) []string {
	if len(packDirs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(packDirs)*2)
	var dirs []string
	for _, packDir := range packDirs {
		for _, rel := range []string{"scripts", filepath.Join("assets", "scripts")} {
			dir := filepath.Join(packDir, rel)
			info, err := os.Stat(dir)
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

func symlinkMatchesLegacySource(linkPath string, legacySourceDirs []string) bool {
	if len(legacySourceDirs) == 0 {
		return false
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(linkPath), target)
	}
	target = filepath.Clean(target)
	for _, dir := range legacySourceDirs {
		dir = filepath.Clean(dir)
		if target == dir || strings.HasPrefix(target, dir+string(os.PathSeparator)) {
			return true
		}
	}
	return false
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
