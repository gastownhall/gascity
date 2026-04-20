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
	if err := pruneLegacyScripts(cityPath); err != nil {
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
		if err := pruneLegacyScripts(rigPath); err != nil {
			handleErr(fmt.Sprintf("rig %q", r.Name), err)
		}
	}
}

// pruneLegacyScripts removes a top-level scripts/ directory only when it
// contains compatibility-shim symlinks and no real files.
func pruneLegacyScripts(targetDir string) error {
	scriptsDir := filepath.Join(targetDir, "scripts")
	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %q: %w", scriptsDir, err)
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
			symlinks = append(symlinks, path)
			return nil
		}
		sawReal = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking %q: %w", scriptsDir, err)
	}
	if sawReal || len(symlinks) == 0 {
		return nil
	}

	for _, path := range symlinks {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy script symlink %q: %w", path, err)
		}
	}
	return removeEmptyDirsInclusive(scriptsDir)
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
