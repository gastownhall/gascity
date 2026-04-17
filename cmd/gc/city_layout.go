package main

import (
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
	// Touch events.jsonl so gc doctor doesn't warn and events are ready.
	eventsPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	if _, err := fs.Stat(eventsPath); err != nil {
		_ = fs.WriteFile(eventsPath, nil, 0o644)
	}
	return nil
}

func cityAlreadyInitializedFS(fs fsys.FS, cityPath string) bool {
	if fi, err := fs.Stat(filepath.Join(cityPath, citylayout.CityConfigFile)); err == nil && !fi.IsDir() {
		return true
	}
	return cityHasScaffoldFS(fs, cityPath)
}

func cityHasScaffoldFS(fs fsys.FS, cityPath string) bool {
	requiredDirs := []string{
		filepath.Join(cityPath, citylayout.RuntimeRoot),
		filepath.Join(cityPath, citylayout.RuntimeRoot, "cache"),
		filepath.Join(cityPath, citylayout.RuntimeRoot, "runtime"),
		filepath.Join(cityPath, citylayout.RuntimeRoot, "system"),
	}
	for _, dir := range requiredDirs {
		fi, err := fs.Stat(dir)
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	fi, err := fs.Stat(filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl"))
	return err == nil && !fi.IsDir()
}

func cityCanResumeInitFS(fs fsys.FS, cityPath string) bool {
	fi, err := fs.Stat(filepath.Join(cityPath, citylayout.CityConfigFile))
	if err != nil || fi.IsDir() {
		return false
	}
	return cityHasScaffoldFS(fs, cityPath)
}
