package cityinit

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// InitConventionDirs returns the convention-discovered directories created by
// city init.
func InitConventionDirs() []string {
	return []string{
		"agents",
		"commands",
		"doctor",
		citylayout.FormulasRoot,
		citylayout.OrdersRoot,
		"template-fragments",
		"overlays",
		"assets",
	}
}

// ManagedScaffoldPaths returns the city-init-owned paths that can be restored
// or removed during rollback.
func ManagedScaffoldPaths() []string {
	seen := make(map[string]struct{}, len(InitConventionDirs())+5)
	paths := make([]string, 0, len(InitConventionDirs())+5)
	add := func(rel string) {
		if rel == "" {
			return
		}
		if _, ok := seen[rel]; ok {
			return
		}
		seen[rel] = struct{}{}
		paths = append(paths, rel)
	}
	add(citylayout.RuntimeRoot)
	add("hooks")
	add(citylayout.CityConfigFile)
	add("pack.toml")
	add(".gitignore")
	for _, rel := range InitConventionDirs() {
		add(rel)
	}
	return paths
}

// EnsureCityScaffoldFS creates the runtime scaffold required for a city.
func EnsureCityScaffoldFS(fs fsys.FS, cityPath string) error {
	for _, rel := range []string{
		citylayout.RuntimeRoot,
		citylayout.CacheRoot,
		citylayout.SystemRoot,
		filepath.Join(citylayout.RuntimeRoot, "runtime"),
	} {
		path := filepath.Join(cityPath, rel)
		if err := fs.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("creating city scaffold directory %q: %w", path, err)
		}
	}
	eventsPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	if _, err := fs.Stat(eventsPath); err == nil {
		return nil
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fmt.Errorf("checking city event log %q: %w", eventsPath, err)
	}
	if err := fs.WriteFile(eventsPath, nil, 0o644); err != nil {
		return fmt.Errorf("creating city event log %q: %w", eventsPath, err)
	}
	return nil
}

// CityAlreadyInitializedFS reports whether cityPath already has init output.
func CityAlreadyInitializedFS(fs fsys.FS, cityPath string) bool {
	if fi, err := fs.Stat(filepath.Join(cityPath, citylayout.CityConfigFile)); err == nil && !fi.IsDir() {
		return true
	}
	return CityHasScaffoldFS(fs, cityPath)
}

// CityHasScaffoldFS reports whether cityPath has the runtime scaffold.
func CityHasScaffoldFS(fs fsys.FS, cityPath string) bool {
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

// CityCanResumeInitFS reports whether cityPath has enough scaffold to resume
// startup checks after a previous init stopped during finalization.
func CityCanResumeInitFS(fs fsys.FS, cityPath string) bool {
	fi, err := fs.Stat(filepath.Join(cityPath, citylayout.CityConfigFile))
	if err != nil || fi.IsDir() {
		return false
	}
	return CityHasScaffoldFS(fs, cityPath)
}
