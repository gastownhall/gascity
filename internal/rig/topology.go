package rig

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// SnapshotTopologyFiles captures every canonical topology file a rig add may
// mutate so a later failure can roll the whole add back atomically: the city
// and per-rig .beads metadata/config mirrors, dolt-server.port mirrors, the
// site binding, city.toml, and packs.lock. packs.lock is written by the
// deferred bundled-rig-import commit after the city config write, so it must be
// covered by the rollback snapshot to keep rig add atomic across the lockfile.
func SnapshotTopologyFiles(fs fsys.FS, cityPath string, cfg *config.City) ([]FileSnapshot, error) {
	snapshots := make([]FileSnapshot, 0, len(cfg.Rigs)*3+6)
	cityToml, err := SnapshotResolvedFile(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, cityToml)
	packsLock, err := SnapshotOptionalFile(fs, filepath.Join(cityPath, "packs.lock"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, packsLock)
	siteToml, err := SnapshotResolvedFile(fs, config.SiteBindingPath(cityPath))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, siteToml)
	citySnapshots, err := snapshotCanonicalFiles(fs, cityPath)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, citySnapshots...)
	cityPort, err := SnapshotResolvedFile(fs, filepath.Join(cityPath, ".beads", "dolt-server.port"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, cityPort)
	seen := map[string]struct{}{}
	for _, rig := range cfg.Rigs {
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		rigPath = filepath.Clean(rigPath)
		if _, ok := seen[rigPath]; ok {
			continue
		}
		seen[rigPath] = struct{}{}
		rigSnapshots, err := snapshotCanonicalFiles(fs, rigPath)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, rigSnapshots...)
		rigPort, err := SnapshotResolvedFile(fs, filepath.Join(rigPath, ".beads", "dolt-server.port"))
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, rigPort)
	}
	return snapshots, nil
}

// snapshotCanonicalFiles snapshots the .beads metadata.json and config.yaml
// mirrors under scopeRoot for rollback.
func snapshotCanonicalFiles(fs fsys.FS, scopeRoot string) ([]FileSnapshot, error) {
	paths := []string{
		filepath.Join(scopeRoot, ".beads", "metadata.json"),
		filepath.Join(scopeRoot, ".beads", "config.yaml"),
	}
	snapshots := make([]FileSnapshot, 0, len(paths))
	for _, path := range paths {
		snap, err := SnapshotResolvedFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}
