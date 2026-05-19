package config

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
)

type revisionSnapshot struct {
	dirHashes      map[string]string
	fileContents   map[string][]byte
	fileKnown      map[string]bool
	conventionDirs []string
}

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
		data, ok := prov.sourceContents[path]
		if !ok {
			var err error
			data, err = fs.ReadFile(path)
			if err != nil {
				continue
			}
		}
		h.Write([]byte(path)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
		h.Write(data)         //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	}

	// Hash rig pack directory contents (all pack sources).
	rigs := cfg.Rigs
	for _, r := range rigs {
		for _, ref := range r.Includes {
			topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
			writeRevisionDirHash(h, prov, "pack:"+r.Name+":"+ref, fs, topoDir)
		}
	}

	// Hash city-level pack directory contents.
	for _, ref := range cfg.Workspace.Includes {
		topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
		writeRevisionDirHash(h, prov, "city-pack:"+ref, fs, topoDir)
	}

	// Remote PackV2 imports resolve through packs.lock, so lockfile changes
	// can change the effective config even when city.toml/pack.toml stay
	// untouched.
	if tracksPackV2Imports(cfg) {
		lockPath := filepath.Join(cityRoot, "packs.lock")
		if data, known, exists := revisionSnapshotFile(prov, lockPath); known {
			if exists {
				writeRevisionBytes(h, lockPath, data)
			}
		} else if data, err := fs.ReadFile(lockPath); err == nil {
			h.Write([]byte(lockPath)) //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})        //nolint:errcheck // hash.Write never errors
			h.Write(data)             //nolint:errcheck // hash.Write never errors
			h.Write([]byte{0})        //nolint:errcheck // hash.Write never errors
		}
	}

	// Hash v2-resolved pack directories (populated by ExpandPacks from
	// [imports.X] and [rigs.imports.X]). Without this, editing a file in
	// an imported pack does not change the revision, so the reconciler
	// never notices. Regression guard: gastownhall/gascity#779.
	for _, dir := range cfg.PackDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		writeRevisionDirHash(h, prov, "city-packdir:"+dir, fs, dir)
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
			writeRevisionDirHash(h, prov, "rig-packdir:"+rigName+":"+dir, fs, dir)
		}
	}
	// Hash convention-discovered city-pack trees so adding or editing
	// agents/commands/doctor content changes the effective revision too.
	for _, dir := range revisionConventionDirs(prov, fs, cityRoot) {
		writeRevisionDirHash(h, prov, "city-discovery:"+dir, fs, dir)
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

func (p *Provenance) captureRevisionSnapshot(fs fsys.FS, cfg *City, cityRoot string) {
	if p == nil || cfg == nil {
		return
	}
	p.recordMissingSourceContents(fs)
	snap := &revisionSnapshot{
		dirHashes:    make(map[string]string),
		fileContents: make(map[string][]byte),
		fileKnown:    make(map[string]bool),
	}
	recordDir := func(label, dir string) {
		snap.dirHashes[label] = PackContentHashRecursive(fs, dir)
	}

	for _, r := range cfg.Rigs {
		for _, ref := range r.Includes {
			topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
			recordDir("pack:"+r.Name+":"+ref, topoDir)
		}
	}
	for _, ref := range cfg.Workspace.Includes {
		topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
		recordDir("city-pack:"+ref, topoDir)
	}
	if tracksPackV2Imports(cfg) {
		lockPath := filepath.Join(cityRoot, "packs.lock")
		snap.fileKnown[lockPath] = true
		if data, err := fs.ReadFile(lockPath); err == nil {
			snap.fileContents[lockPath] = cloneBytes(data)
		}
	}
	for _, dir := range cfg.PackDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		recordDir("city-packdir:"+dir, dir)
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
			recordDir("rig-packdir:"+rigName+":"+dir, dir)
		}
	}
	snap.conventionDirs = existingConventionDiscoveryDirsFS(fs, cityRoot)
	for _, dir := range snap.conventionDirs {
		recordDir("city-discovery:"+dir, dir)
	}
	p.revisionSnapshot = snap
}

func (p *Provenance) recordMissingSourceContents(fs fsys.FS) {
	if p == nil {
		return
	}
	for _, path := range p.Sources {
		if _, ok := p.sourceContents[path]; ok {
			continue
		}
		data, err := fs.ReadFile(path)
		if err != nil {
			continue
		}
		p.recordSource(path, data)
	}
}

func writeRevisionDirHash(h hash.Hash, prov *Provenance, label string, fs fsys.FS, dir string) {
	topoHash, ok := revisionSnapshotDirHash(prov, label)
	if !ok {
		topoHash = PackContentHashRecursive(fs, dir)
	}
	writeRevisionBytes(h, label, []byte(topoHash))
}

func writeRevisionBytes(h hash.Hash, label string, data []byte) {
	h.Write([]byte(label)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})     //nolint:errcheck // hash.Write never errors
	h.Write(data)          //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})     //nolint:errcheck // hash.Write never errors
}

func revisionSnapshotDirHash(prov *Provenance, label string) (string, bool) {
	if prov == nil || prov.revisionSnapshot == nil {
		return "", false
	}
	v, ok := prov.revisionSnapshot.dirHashes[label]
	return v, ok
}

func revisionSnapshotFile(prov *Provenance, path string) ([]byte, bool, bool) {
	if prov == nil || prov.revisionSnapshot == nil || !prov.revisionSnapshot.fileKnown[path] {
		return nil, false, false
	}
	data, exists := prov.revisionSnapshot.fileContents[path]
	return data, true, exists
}

func revisionConventionDirs(prov *Provenance, fs fsys.FS, cityRoot string) []string {
	if prov == nil || prov.revisionSnapshot == nil {
		return existingConventionDiscoveryDirsFS(fs, cityRoot)
	}
	return append([]string(nil), prov.revisionSnapshot.conventionDirs...)
}

func cloneBytes(data []byte) []byte {
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp
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

	if prov != nil {
		for _, src := range prov.Sources {
			dir := filepath.Dir(src)
			addTarget(dir, false, pathutil.SamePath(dir, cityRoot))
		}
	}

	for _, r := range cfg.Rigs {
		for _, ref := range r.Includes {
			topoDir, ok := revisionPackDir(ref, cityRoot, cityRoot)
			if !ok {
				continue
			}
			addTarget(topoDir, true, false)
		}
	}

	for _, ref := range cfg.Workspace.Includes {
		topoDir, ok := revisionPackDir(ref, cityRoot, cityRoot)
		if !ok {
			continue
		}
		addTarget(topoDir, true, false)
	}

	for _, dir := range cfg.PackDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		addTarget(dir, true, false)
	}
	rigNames := make([]string, 0, len(cfg.RigPackDirs))
	for rigName := range cfg.RigPackDirs {
		rigNames = append(rigNames, rigName)
	}
	sort.Strings(rigNames)
	for _, rigName := range rigNames {
		for _, dir := range cfg.RigPackDirs[rigName] {
			if strings.TrimSpace(dir) == "" {
				continue
			}
			addTarget(dir, true, false)
		}
	}

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

// WatchDirs returns the deduplicated paths from WatchTargets.
func WatchDirs(prov *Provenance, cfg *City, cityRoot string) []string {
	targets := WatchTargets(prov, cfg, cityRoot)
	dirs := make([]string, len(targets))
	for i, target := range targets {
		dirs[i] = target.Path
	}
	return dirs
}

func tracksPackV2Imports(cfg *City) bool {
	if cfg == nil {
		return false
	}
	if len(cfg.Imports) > 0 || len(cfg.PackDirs) > 0 || len(cfg.RigPackDirs) > 0 {
		return true
	}
	for _, rig := range cfg.Rigs {
		if len(rig.Imports) > 0 {
			return true
		}
	}
	return false
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
