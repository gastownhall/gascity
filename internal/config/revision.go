package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Revision computes a deterministic bundle hash from all resolved config
// source files. This serves as a revision identifier — if the revision
// changes, the effective config may have changed and a reload is warranted.
//
// The hash covers the content of all source files listed in Provenance,
// plus pack directory contents for any rigs with packs (including
// plural pack lists and city-level packs).
func Revision(fs fsys.FS, prov *Provenance, cfg *City, cityRoot string) string {
	h := sha256.New()

	// Hash all config source files in stable order.
	sources := make([]string, len(prov.Sources))
	copy(sources, prov.Sources)
	sort.Strings(sources)
	for _, path := range sources {
		data, err := fs.ReadFile(path)
		if err != nil {
			continue
		}
		h.Write([]byte(path)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
		h.Write(data)         //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	}

	// Hash rig pack directory contents (v1 Includes-based refs).
	rigs := cfg.Rigs
	for _, r := range rigs {
		for _, ref := range r.Includes {
			topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
			topoHash := PackContentHashRecursive(fs, topoDir)
			h.Write([]byte("pack:" + r.Name + ":" + ref)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                            //nolint:errcheck // hash.Write never errors
			h.Write([]byte(topoHash))                     //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                            //nolint:errcheck // hash.Write never errors
		}
	}

	// Hash city-level pack directory contents (v1 Includes-based refs).
	for _, ref := range cfg.Workspace.Includes {
		topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
		topoHash := PackContentHashRecursive(fs, topoDir)
		h.Write([]byte("city-pack:" + ref)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                  //nolint:errcheck // hash.Write never errors
		h.Write([]byte(topoHash))           //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                  //nolint:errcheck // hash.Write never errors
	}

	// Hash v2-resolved pack directories (populated by ExpandPacks from
	// [imports.X] and [rigs.imports.X]). Without this, editing a file in
	// an imported pack does not change the revision, so the reconciler
	// never notices. Regression guard: gastownhall/gascity#779.
	for _, dir := range cfg.PackDirs {
		topoHash := PackContentHashRecursive(fs, dir)
		h.Write([]byte("city-packdir:" + dir)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                     //nolint:errcheck // hash.Write never errors
		h.Write([]byte(topoHash))              //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                     //nolint:errcheck // hash.Write never errors
	}
	rigPackDirNames := make([]string, 0, len(cfg.RigPackDirs))
	for name := range cfg.RigPackDirs {
		rigPackDirNames = append(rigPackDirNames, name)
	}
	sort.Strings(rigPackDirNames)
	for _, rigName := range rigPackDirNames {
		for _, dir := range cfg.RigPackDirs[rigName] {
			topoHash := PackContentHashRecursive(fs, dir)
			h.Write([]byte("rig-packdir:" + rigName + ":" + dir)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                                    //nolint:errcheck // hash.Write never errors
			h.Write([]byte(topoHash))                             //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                                    //nolint:errcheck // hash.Write never errors
		}
	}

	// Hash convention-discovered city-pack trees so adding or editing
	// agents/commands/doctor content changes the effective revision too.
	for _, dir := range existingConventionDiscoveryDirsFS(fs, cityRoot) {
		topoHash := PackContentHashRecursive(fs, dir)
		h.Write([]byte("city-discovery:" + dir)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                       //nolint:errcheck // hash.Write never errors
		h.Write([]byte(topoHash))                //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                       //nolint:errcheck // hash.Write never errors
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

// WatchDirs returns the set of directories that should be watched for
// config changes. This includes the directory of each source file,
// rig pack directories, and city-level pack directories.
// Returns deduplicated, sorted paths.
func WatchDirs(prov *Provenance, cfg *City, cityRoot string) []string {
	seen := make(map[string]bool)
	var dirs []string

	addDir := func(dir string) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}

	// Config source file directories.
	if prov != nil {
		for _, src := range prov.Sources {
			addDir(filepath.Dir(src))
		}
	}

	// Rig pack directories (v1 Includes-based refs).
	for _, r := range cfg.Rigs {
		for _, ref := range r.Includes {
			topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
			addDir(topoDir)
		}
	}

	// City-level pack directories (v1 Includes-based refs).
	for _, ref := range cfg.Workspace.Includes {
		topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
		addDir(topoDir)
	}

	// v2-resolved pack directories (populated by ExpandPacks from
	// [imports.X] and [rigs.imports.X]). Without these, cities composing
	// packs via v2 imports get zero fsnotify coverage for the imported
	// pack tree — hot-reload silently breaks.
	// Regression guard: gastownhall/gascity#779.
	for _, dir := range cfg.PackDirs {
		addDir(dir)
	}
	for _, rigDirs := range cfg.RigPackDirs {
		for _, dir := range rigDirs {
			addDir(dir)
		}
	}

	// Convention-discovered city-pack trees are loaded directly from the city
	// root, so watch them too when they already exist.
	for _, dir := range existingConventionDiscoveryDirsOS(cityRoot) {
		addDir(dir)
	}

	sort.Strings(dirs)
	return dirs
}

var conventionDiscoveryDirNames = []string{"agents", "commands", "doctor"}

func existingConventionDiscoveryDirsFS(fs fsys.FS, cityRoot string) []string {
	var dirs []string
	for _, name := range conventionDiscoveryDirNames {
		dir := filepath.Join(cityRoot, name)
		if info, err := fs.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func existingConventionDiscoveryDirsOS(cityRoot string) []string {
	var dirs []string
	for _, name := range conventionDiscoveryDirNames {
		dir := filepath.Join(cityRoot, name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
