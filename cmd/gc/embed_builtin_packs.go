package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/examples/dolt"
	"github.com/gastownhall/gascity/examples/gastown/packs/gastown"
	"github.com/gastownhall/gascity/examples/gastown/packs/maintenance"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// builtinPack pairs an embedded FS with the subdirectory name used under .gc/system/packs/.
type builtinPack struct {
	fs   fs.FS
	name string // e.g. "bd", "dolt"
}

// builtinPacks lists all packs embedded in the gc binary. These are
// materialized to .gc/system/packs/ on every gc start and gc init.
var builtinPacks = []builtinPack{
	{fs: bd.PackFS, name: "bd"},
	{fs: dolt.PackFS, name: "dolt"},
	{fs: maintenance.PackFS, name: "maintenance"},
	{fs: gastown.PackFS, name: "gastown"},
}

// MaterializeBuiltinPacks writes all embedded pack files to
// .gc/system/packs/{name}/ in the city directory. Files are always
// overwritten to stay in sync with the gc binary version. Shell scripts
// get 0755; everything else 0644.
// Idempotent: safe to call on every gc start and gc init.
func MaterializeBuiltinPacks(cityPath string) error {
	for _, bp := range builtinPacks {
		dst := filepath.Join(cityPath, citylayout.SystemPacksRoot, bp.name)
		if err := materializeFS(bp.fs, ".", dst); err != nil {
			return fmt.Errorf("materializing %s pack: %w", bp.name, err)
		}
	}
	return nil
}

// builtinPackIncludes returns the system pack paths that should be
// auto-included in config loading. These are appended as extraIncludes
// to LoadWithIncludes so they go through normal pack expansion
// (ExpandCityPacks) with dedup/fallback resolution.
//
// Maintenance is always included. bd/dolt are included when the beads
// provider is "bd" (the default). Gastown is never auto-included — it
// requires an explicit workspace.includes entry.
func builtinPackIncludes(cityPath string) []string {
	systemRoot := filepath.Join(cityPath, citylayout.SystemPacksRoot)

	// Maintenance is always auto-included.
	var includes []string
	if maintenancePath := filepath.Join(systemRoot, "maintenance"); packExists(maintenancePath) {
		includes = append(includes, maintenancePath)
	}

	// bd/dolt are gated on the beads provider.
	provider := os.Getenv("GC_BEADS")
	if provider == "" {
		// Peek at city.toml for the provider setting without full config load.
		provider = peekBeadsProvider(filepath.Join(cityPath, "city.toml"))
	}
	if provider == "" || provider == "bd" {
		if bdPath := filepath.Join(systemRoot, "bd"); packExists(bdPath) {
			includes = append(includes, bdPath)
		}
		if doltPath := filepath.Join(systemRoot, "dolt"); packExists(doltPath) {
			includes = append(includes, doltPath)
		}
	}

	return includes
}

// packExists checks if a pack.toml exists in the given directory.
func packExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pack.toml"))
	return err == nil
}

// peekBeadsProvider reads just the beads.provider field from a city.toml
// without doing full config parsing. Returns "" if not set or on error.
func peekBeadsProvider(tomlPath string) string {
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	var peek struct {
		Beads struct {
			Provider string `toml:"provider"`
		} `toml:"beads"`
	}
	if _, err := toml.Decode(string(data), &peek); err != nil {
		return ""
	}
	return peek.Beads.Provider
}

// materializeFS walks an embed.FS rooted at root and writes all files to dstDir.
func materializeFS(embedded fs.FS, root, dstDir string) error {
	return fs.WalkDir(embedded, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Compute the relative path from root.
		rel := path
		if root != "." {
			rel = strings.TrimPrefix(path, root+"/")
			if rel == root {
				return nil
			}
		}

		dst := filepath.Join(dstDir, rel)

		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}

		data, err := fs.ReadFile(embedded, path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}

		perm := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			perm = 0o755
		}
		return os.WriteFile(dst, data, perm)
	})
}

// MaterializeGastownPacks is a compatibility shim for callers that still
// reference it. With all packs now materialized by MaterializeBuiltinPacks,
// this is a no-op. It will be removed once all callers are updated.
func MaterializeGastownPacks(_ string) error {
	return nil
}
