package config

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/steveyegge/gascity/internal/fsys"
)

// Revision computes a deterministic bundle hash from all resolved config
// source files. This serves as a revision identifier — if the revision
// changes, the effective config may have changed and a reload is warranted.
//
// The hash covers the content of all source files listed in Provenance,
// plus topology directory contents for any rigs with topologies (including
// plural topology lists and city-level topologies).
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

	// Hash rig topology directory contents (all topology sources).
	rigs := cfg.Rigs
	for _, r := range rigs {
		for _, ref := range EffectiveRigTopologies(r) {
			topoDir, _ := resolveTopologyRef(ref, cityRoot, cityRoot)
			topoHash := TopologyContentHashRecursive(fs, topoDir)
			h.Write([]byte("topo:" + r.Name + ":" + ref)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                            //nolint:errcheck // hash.Write never errors
			h.Write([]byte(topoHash))                     //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})                            //nolint:errcheck // hash.Write never errors
		}
	}

	// Hash city-level topology directory contents.
	for _, ref := range EffectiveCityTopologies(cfg.Workspace) {
		topoDir, _ := resolveTopologyRef(ref, cityRoot, cityRoot)
		topoHash := TopologyContentHashRecursive(fs, topoDir)
		h.Write([]byte("city-topo:" + ref)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                  //nolint:errcheck // hash.Write never errors
		h.Write([]byte(topoHash))           //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})                  //nolint:errcheck // hash.Write never errors
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

// WatchDirs returns the set of directories that should be watched for
// config changes. This includes the directory of each source file,
// rig topology directories, and city-level topology directories.
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
	for _, src := range prov.Sources {
		addDir(filepath.Dir(src))
	}

	// Rig topology directories (all topology sources).
	for _, r := range cfg.Rigs {
		for _, ref := range EffectiveRigTopologies(r) {
			topoDir, _ := resolveTopologyRef(ref, cityRoot, cityRoot)
			addDir(topoDir)
		}
	}

	// City-level topology directories.
	for _, ref := range EffectiveCityTopologies(cfg.Workspace) {
		topoDir, _ := resolveTopologyRef(ref, cityRoot, cityRoot)
		addDir(topoDir)
	}

	sort.Strings(dirs)
	return dirs
}
