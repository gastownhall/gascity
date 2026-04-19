package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
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
			topoDir, ok := revisionPackDir(ref, cityRoot, cityRoot)
			if !ok {
				continue
			}
			topoHash := PackContentHashRecursive(fs, topoDir)
			h.Write([]byte("pack:" + r.Name + ":" + ref)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                            //nolint:errcheck // hash.Write never errors
			h.Write([]byte(topoHash))                     //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                            //nolint:errcheck // hash.Write never errors
		}
	}

	// Hash city-level pack directory contents (v1 Includes-based refs).
	for _, ref := range cfg.Workspace.Includes {
		topoDir, ok := revisionPackDir(ref, cityRoot, cityRoot)
		if !ok {
			continue
		}
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
		if strings.TrimSpace(dir) == "" {
			continue
		}
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
			if strings.TrimSpace(dir) == "" {
				continue
			}
			topoHash := PackContentHashRecursive(fs, dir)
			h.Write([]byte("rig-packdir:" + rigName + ":" + dir)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                                    //nolint:errcheck // hash.Write never errors
			h.Write([]byte(topoHash))                             //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                                    //nolint:errcheck // hash.Write never errors
		}
	}

	// Hash convention-discovered city-pack trees so adding or editing
	// convention content changes the effective revision too.
	for _, dir := range existingConventionDiscoveryDirsFS(fs, cityRoot) {
		topoHash := PackContentHashRecursive(fs, dir)
		h.Write([]byte("city-discovery:" + dir)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                       //nolint:errcheck // hash.Write never errors
		h.Write([]byte(topoHash))                //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                       //nolint:errcheck // hash.Write never errors
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

// WatchTarget describes a filesystem path that should be watched for config
// changes and how much of its subtree participates in discovery.
type WatchTarget struct {
	Path                string
	Recursive           bool
	DiscoverConventions bool
}

// WatchTargets returns the set of paths that should be watched for config
// changes. Config source directories are shallow; city roots discover
// convention subdirectories; pack roots and convention roots are recursive.
// Returns deduplicated targets sorted by path.
func WatchTargets(prov *Provenance, cfg *City, cityRoot string) []WatchTarget {
	seen := make(map[string]int)
	var targets []WatchTarget

	addTarget := func(path string, recursive, discoverConventions bool) {
		if path == "" {
			return
		}
		if idx, ok := seen[path]; ok {
			targets[idx].Recursive = targets[idx].Recursive || recursive
			targets[idx].DiscoverConventions = targets[idx].DiscoverConventions || discoverConventions
			return
		}
		seen[path] = len(targets)
		targets = append(targets, WatchTarget{
			Path:                path,
			Recursive:           recursive,
			DiscoverConventions: discoverConventions,
		})
	}

	// Config source file directories.
	if prov != nil {
		for _, src := range prov.Sources {
			dir := filepath.Dir(src)
			addTarget(dir, false, pathutil.SamePath(dir, cityRoot))
		}
	}

	// Rig pack directories (v1 Includes-based refs).
	for _, r := range cfg.Rigs {
		for _, ref := range r.Includes {
			topoDir, ok := revisionPackDir(ref, cityRoot, cityRoot)
			if !ok {
				continue
			}
			addTarget(topoDir, true, false)
		}
	}

	// City-level pack directories (v1 Includes-based refs).
	for _, ref := range cfg.Workspace.Includes {
		topoDir, ok := revisionPackDir(ref, cityRoot, cityRoot)
		if !ok {
			continue
		}
		addTarget(topoDir, true, false)
	}

	// v2-resolved pack directories populated by ExpandPacks from [imports.X]
	// and [rigs.imports.X]. Regression guard: gastownhall/gascity#779.
	for _, dir := range cfg.PackDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		addTarget(dir, true, false)
	}
	rigPackDirNames := make([]string, 0, len(cfg.RigPackDirs))
	for name := range cfg.RigPackDirs {
		rigPackDirNames = append(rigPackDirNames, name)
	}
	sort.Strings(rigPackDirNames)
	for _, rigName := range rigPackDirNames {
		for _, dir := range cfg.RigPackDirs[rigName] {
			if strings.TrimSpace(dir) == "" {
				continue
			}
			addTarget(dir, true, false)
		}
	}

	// Convention-discovered city-pack trees are loaded directly from the city
	// root, so watch them too when they already exist.
	for _, dir := range existingConventionDiscoveryDirsOS(cityRoot) {
		addTarget(dir, true, false)
	}

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Path < targets[j].Path
	})
	return targets
}

func revisionPackDir(ref, declDir, cityRoot string) (string, bool) {
	if strings.TrimSpace(ref) == "" {
		return "", false
	}
	dir, err := resolvePackRef(ref, declDir, cityRoot)
	if err != nil || strings.TrimSpace(dir) == "" {
		return "", false
	}
	return dir, true
}

// WatchDirs returns the deduplicated paths from WatchTargets. It is retained
// for callers that only need the legacy path list.
func WatchDirs(prov *Provenance, cfg *City, cityRoot string) []string {
	targets := WatchTargets(prov, cfg, cityRoot)
	dirs := make([]string, len(targets))
	for i, target := range targets {
		dirs[i] = target.Path
	}
	return dirs
}

var conventionDiscoveryDirNames = []string{"agents", "commands", "doctor", "formulas", "orders", "template-fragments", "skills", "mcp"}

// ConventionDiscoveryDirNames returns the fixed top-level directory names
// whose contents participate in convention-based pack discovery.
func ConventionDiscoveryDirNames() []string {
	return append([]string(nil), conventionDiscoveryDirNames...)
}

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
