package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

func ensureCityScaffold(cityPath string) error {
	return ensureCityScaffoldFS(fsys.OSFS{}, cityPath)
}

func ensureCityScaffoldFS(fs fsys.FS, cityPath string) error {
	for _, rel := range []string{
		citylayout.RuntimeRoot,
		citylayout.CacheRoot,
		citylayout.SystemRoot,
		filepath.Join(citylayout.RuntimeRoot, "runtime"),
	} {
		if err := fs.MkdirAll(filepath.Join(cityPath, rel), 0o755); err != nil {
			return err
		}
	}
	return migrateLegacySystemArtifactsFS(fs, cityPath)
}

func cityAlreadyInitializedFS(fs fsys.FS, cityPath string) bool {
	if fi, err := fs.Stat(filepath.Join(cityPath, citylayout.CityConfigFile)); err == nil && !fi.IsDir() {
		return true
	}
	if fi, err := fs.Stat(filepath.Join(cityPath, citylayout.RuntimeRoot)); err == nil && fi.IsDir() {
		return true
	}
	return false
}

func normalizeInitFromLegacyContent(cityPath string) error {
	steps := [][2]string{
		{citylayout.LegacyPromptsRoot, citylayout.PromptsRoot},
		{citylayout.LegacyAutomationsRoot, citylayout.AutomationsRoot},
		{citylayout.LegacyFormulasRoot, citylayout.FormulasRoot},
		{citylayout.LegacyClaudeHookFile, citylayout.ClaudeHookFile},
		{citylayout.LegacyScriptsRoot, citylayout.ScriptsRoot},
	}
	for _, step := range steps {
		if err := migrateLegacyContent(filepath.Join(cityPath, step[0]), filepath.Join(cityPath, step[1])); err != nil {
			return fmt.Errorf("%s -> %s: %w", step[0], step[1], err)
		}
	}
	return nil
}

func migrateLegacySystemArtifactsFS(fs fsys.FS, cityPath string) error {
	for _, move := range [][2]string{
		{citylayout.LegacySystemFormulasRoot, citylayout.SystemFormulasRoot},
		{citylayout.LegacyBeadsBdScript, filepath.Join(citylayout.SystemBinRoot, "gc-beads-bd")},
	} {
		if err := renameIfMissingFS(fs, filepath.Join(cityPath, move[0]), filepath.Join(cityPath, move[1])); err != nil {
			return err
		}
	}

	legacyPacksRoot := filepath.Join(cityPath, citylayout.LegacyPacksRoot)
	if _, err := fs.Stat(legacyPacksRoot); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	entries, err := fs.ReadDir(legacyPacksRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		var target string
		switch name {
		case "_inc":
			target = filepath.Join(cityPath, citylayout.CacheIncludesRoot)
		case "bd", "dolt":
			target = filepath.Join(cityPath, citylayout.SystemPacksRoot, name)
		default:
			target = filepath.Join(cityPath, citylayout.CachePacksRoot, name)
		}
		if err := renameIfMissingFS(fs, filepath.Join(legacyPacksRoot, name), target); err != nil {
			return err
		}
	}
	return pruneEmptyDirsFS(fs, legacyPacksRoot, filepath.Join(cityPath, citylayout.RuntimeRoot))
}

func renameIfMissingFS(fs fsys.FS, legacyPath, canonicalPath string) error {
	if _, err := fs.Stat(legacyPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if _, err := fs.Stat(canonicalPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := fs.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		return err
	}
	return fs.Rename(legacyPath, canonicalPath)
}

func migrateLegacyContent(legacyPath, canonicalPath string) error {
	legacyInfo, legacyErr := os.Stat(legacyPath)
	if legacyErr != nil {
		if os.IsNotExist(legacyErr) {
			return nil
		}
		return legacyErr
	}
	canonicalInfo, canonicalErr := os.Stat(canonicalPath)
	if canonicalErr != nil && !os.IsNotExist(canonicalErr) {
		return canonicalErr
	}
	if os.IsNotExist(canonicalErr) {
		if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
			return err
		}
		return os.Rename(legacyPath, canonicalPath)
	}
	if legacyInfo.IsDir() != canonicalInfo.IsDir() {
		return fmt.Errorf("conflicting types")
	}
	if !legacyInfo.IsDir() {
		legacyData, err := os.ReadFile(legacyPath)
		if err != nil {
			return err
		}
		canonicalData, err := os.ReadFile(canonicalPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(legacyData, canonicalData) {
			return fmt.Errorf("conflicting contents")
		}
		return os.Remove(legacyPath)
	}

	entries, err := os.ReadDir(legacyPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := migrateLegacyContent(
			filepath.Join(legacyPath, entry.Name()),
			filepath.Join(canonicalPath, entry.Name()),
		); err != nil {
			return err
		}
	}
	return pruneEmptyDirs(legacyPath, filepath.Dir(filepath.Dir(legacyPath)))
}

func pruneEmptyDirs(path, stop string) error {
	for {
		if path == stop || path == filepath.Dir(stop) {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil
		}
		if len(entries) > 0 {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func pruneEmptyDirsFS(fs fsys.FS, path, stop string) error {
	for {
		if path == stop || path == filepath.Dir(stop) {
			return nil
		}
		entries, err := fs.ReadDir(path)
		if err != nil {
			return nil
		}
		if len(entries) > 0 {
			return nil
		}
		if err := fs.Remove(path); err != nil {
			return nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}
