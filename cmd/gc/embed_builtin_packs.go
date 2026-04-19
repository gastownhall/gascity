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
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/orders"
)

// builtinPack pairs an embedded FS with the subdirectory name used under .gc/system/packs/.
type builtinPack struct {
	fs   fs.FS
	name string // e.g. "bd", "dolt"
}

const (
	legacyOrderConfigFile = "order.toml"
)

// builtinPacks lists all packs embedded in the gc binary. These are
// materialized to .gc/system/packs/ on every gc start and gc init.
var builtinPacks = []builtinPack{
	{fs: core.PackFS, name: "core"},
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
		if err := pruneLegacyEmbeddedOrders(bp.fs, dst); err != nil {
			return fmt.Errorf("pruning legacy %s order paths: %w", bp.name, err)
		}
	}
	return nil
}

// builtinPackIncludes returns the system pack paths that should be
// auto-included in config loading. These are appended as extraIncludes
// to LoadWithIncludes so they go through normal pack expansion
// (ExpandCityPacks) with dedup/fallback resolution.
//
// Core and maintenance are always included. Core ships the role prompts
// referenced by implicit agents and the overlay/per-provider hook files,
// so its content must reach PackOverlayDirs even when the user has never
// run `gc init` (and therefore has no implicit-import.toml written to
// $GC_HOME). When the beads provider is "bd" (the default), include bd
// and let its own pack includes pull in dolt transitively. Gastown is
// never auto-included — it requires an explicit workspace.includes entry.
func builtinPackIncludes(cityPath string) []string {
	systemRoot := filepath.Join(cityPath, citylayout.SystemPacksRoot)

	var includes []string
	if corePath := filepath.Join(systemRoot, "core"); packExists(corePath) {
		includes = append(includes, corePath)
	}
	if maintenancePath := filepath.Join(systemRoot, "maintenance"); packExists(maintenancePath) {
		includes = append(includes, maintenancePath)
	}

	// bd is gated on the beads provider. The managed exec wrapper path is
	// normalized back to "bd", so it only needs the bd pack. A direct
	// exec:gc-beads-bd override outside the managed wrapper still includes
	// dolt explicitly so config loading keeps the lifecycle helpers aligned.
	provider := strings.TrimSpace(configuredBeadsProviderValue(cityPath))
	normalizedProvider := normalizeRawBeadsProvider(cityPath, provider)
	usesDirectExecLifecycle := strings.HasPrefix(provider, "exec:") &&
		execProviderBase(provider) == "gc-beads-bd" &&
		normalizedProvider != "bd"
	if providerUsesBdStoreContract(normalizedProvider) {
		if bdPath := filepath.Join(systemRoot, "bd"); packExists(bdPath) {
			includes = append(includes, bdPath)
		}
	}
	if usesDirectExecLifecycle {
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
		if isExecutableScriptFilename(path) {
			perm = 0o755
		}
		return os.WriteFile(dst, data, perm)
	})
}

// isExecutableScriptFilename reports whether a materialized pack asset
// should be marked executable. Shell, Python, and bash interpreters all
// rely on shebang-based direct execution, so the file needs +x regardless
// of extension — gc invokes resolved run paths directly rather than
// wrapping them with an explicit interpreter command.
func isExecutableScriptFilename(name string) bool {
	for _, suffix := range []string{".sh", ".py", ".bash"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// pruneLegacyEmbeddedOrders removes deprecated order directory layouts when the
// embedded pack already provides the flat orders/<name>.toml form.
func pruneLegacyEmbeddedOrders(embedded fs.FS, dstDir string) error {
	entries, err := fs.ReadDir(embedded, "orders")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		orderName, ok := orders.TrimFlatOrderFilename(name)
		if !ok {
			continue
		}
		for _, legacyPath := range []string{
			filepath.Join(dstDir, "orders", orderName, legacyOrderConfigFile),
			filepath.Join(dstDir, "formulas", "orders", orderName, legacyOrderConfigFile),
		} {
			if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			pruneEmptyDirs(filepath.Dir(legacyPath), dstDir)
		}
	}
	return nil
}

func pruneEmptyDirs(dir, stop string) {
	stop = filepath.Clean(stop)
	for {
		cleanDir := filepath.Clean(dir)
		if cleanDir == stop || cleanDir == "." || cleanDir == string(filepath.Separator) {
			return
		}
		entries, err := os.ReadDir(cleanDir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(cleanDir); err != nil {
			return
		}
		dir = filepath.Dir(cleanDir)
	}
}

// MaterializeGastownPacks is a compatibility shim for callers that still
// reference it. With all packs now materialized by MaterializeBuiltinPacks,
// this is a no-op. It will be removed once all callers are updated.
func MaterializeGastownPacks(_ string) error {
	return nil
}
