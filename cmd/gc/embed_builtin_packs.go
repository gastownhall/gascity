package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/examples/dolt"
	"github.com/gastownhall/gascity/examples/gastown/packs/gastown"
	"github.com/gastownhall/gascity/examples/gastown/packs/maintenance"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
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

var builtinPackRefreshCache sync.Map

type builtinPackRefreshState struct {
	mu          sync.Mutex
	ready       bool
	lastWarning string
}

type builtinPackRefreshResult struct {
	ready   bool
	warning error
	fatal   error
}

type builtinPackFile struct {
	data []byte
	perm os.FileMode
}

// MaterializeBuiltinPacks writes all embedded pack files to
// .gc/system/packs/{name}/ in the city directory. Files whose content and mode
// already match are left in place; changed content or mode is repaired with an
// atomic rename so readers never observe a truncated file. Shell scripts get
// 0755; everything else 0644.
// Idempotent: safe to call on every gc start and gc init.
func MaterializeBuiltinPacks(cityPath string) error {
	for _, bp := range builtinPacks {
		dst := filepath.Join(cityPath, citylayout.SystemPacksRoot, bp.name)
		desired, err := materializeFS(bp.fs, ".", dst)
		if err != nil {
			return fmt.Errorf("materializing %s pack: %w", bp.name, err)
		}
		if err := pruneStaleGeneratedPackFiles(dst, desired); err != nil {
			return fmt.Errorf("pruning stale %s pack files: %w", bp.name, err)
		}
		if err := pruneLegacyEmbeddedOrders(bp.fs, dst); err != nil {
			return fmt.Errorf("pruning legacy %s order paths: %w", bp.name, err)
		}
	}
	return nil
}

func builtinPackIncludesForConfigLoad(fs fsys.FS, tomlPath string, warningWriter io.Writer) ([]string, error) {
	if !usesOSFS(fs) {
		return nil, nil
	}
	cityPath := filepath.Dir(tomlPath)
	if err := ensureBuiltinPacksReadyForConfigLoad(cityPath, warningWriter); err != nil {
		return nil, err
	}
	return builtinPackIncludes(cityPath), nil
}

func usesOSFS(fs fsys.FS) bool {
	switch fs.(type) {
	case fsys.OSFS, *fsys.OSFS:
		return true
	default:
		return false
	}
}

func ensureBuiltinPacksReadyForConfigLoad(cityPath string, warningWriter io.Writer) error {
	key := normalizePathForCompare(cityPath)
	stateAny, _ := builtinPackRefreshCache.LoadOrStore(key, &builtinPackRefreshState{})
	state := stateAny.(*builtinPackRefreshState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ready {
		if len(unusableRequiredBuiltinPackNames(cityPath)) == 0 {
			return nil
		}
		state.ready = false
	}
	result := materializeBuiltinPacksForConfigLoad(cityPath)
	if result.fatal != nil {
		state.lastWarning = ""
		return result.fatal
	}
	if result.warning != nil {
		const warningKey = "builtin-pack-refresh-incomplete"
		if state.lastWarning != warningKey {
			emitBuiltinPackRefreshWarning(warningWriter, result.warning)
			state.lastWarning = warningKey
		}
		return nil
	}
	if result.ready {
		state.ready = true
		state.lastWarning = ""
	}
	return nil
}

func materializeBuiltinPacksForConfigLoad(cityPath string) builtinPackRefreshResult {
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		if missing := unusableRequiredBuiltinPackNames(cityPath); len(missing) > 0 {
			return builtinPackRefreshResult{
				fatal: fmt.Errorf("materializing builtin packs: required builtin packs remain unusable (%s): %w", strings.Join(missing, ", "), err),
			}
		}
		return builtinPackRefreshResult{
			warning: fmt.Errorf("builtin pack refresh incomplete; using existing materialized packs: %w", err),
		}
	}
	return builtinPackRefreshResult{ready: true}
}

func unusableRequiredBuiltinPackNames(cityPath string) []string {
	systemRoot := filepath.Join(cityPath, citylayout.SystemPacksRoot)
	var missing []string
	for _, name := range requiredBuiltinPackNames(cityPath) {
		bp, ok := builtinPackByName(name)
		if !ok || !packContainsEmbeddedState(bp.fs, filepath.Join(systemRoot, name)) {
			missing = append(missing, name)
		}
	}
	return missing
}

func builtinPackByName(name string) (builtinPack, bool) {
	for _, bp := range builtinPacks {
		if bp.name == name {
			return bp, true
		}
	}
	return builtinPack{}, false
}

func packContainsEmbeddedState(embedded fs.FS, dstDir string) bool {
	manifest, err := embeddedPackManifest(embedded)
	if err != nil {
		return false
	}
	return packContainsEmbeddedManifest(manifest, dstDir)
}

func packContainsEmbeddedManifest(manifest map[string]builtinPackFile, dstDir string) bool {
	fi, err := os.Stat(dstDir)
	if err != nil || !fi.IsDir() {
		return false
	}
	for rel, want := range manifest {
		dstPath := filepath.Join(dstDir, filepath.FromSlash(rel))
		info, err := os.Lstat(dstPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != want.perm {
			return false
		}
		got, err := os.ReadFile(dstPath)
		if err != nil || !bytes.Equal(got, want.data) {
			return false
		}
	}
	return true
}

func embeddedPackManifest(embedded fs.FS) (map[string]builtinPackFile, error) {
	manifest := make(map[string]builtinPackFile)
	err := fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(embedded, path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}
		manifest[filepath.ToSlash(path)] = builtinPackFile{
			data: data,
			perm: builtinPackFileMode(path),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

func requiredBuiltinPackNames(cityPath string) []string {
	required := []string{"core", "maintenance"}

	provider := strings.TrimSpace(configuredBeadsProviderValue(cityPath))
	normalizedProvider := normalizeRawBeadsProvider(cityPath, provider)
	if providerUsesBdStoreContract(normalizedProvider) {
		required = append(required, "bd")
	}
	usesDirectExecLifecycle := strings.HasPrefix(provider, "exec:") &&
		execProviderBase(provider) == "gc-beads-bd" &&
		normalizedProvider != "bd"
	if usesDirectExecLifecycle {
		required = append(required, "dolt")
	}
	return required
}

func emitBuiltinPackRefreshWarning(w io.Writer, err error) {
	if w == nil || err == nil {
		return
	}
	fmt.Fprintf(w, "warning: %v\n", err) //nolint:errcheck // best-effort warning emission
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
	for _, name := range requiredBuiltinPackNames(cityPath) {
		packPath := filepath.Join(systemRoot, name)
		if packExists(packPath) {
			includes = append(includes, packPath)
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

// materializeFS walks an embed.FS rooted at root, writes all files to dstDir,
// and returns the relative file paths that belong in the generated directory.
func materializeFS(embedded fs.FS, root, dstDir string) (map[string]struct{}, error) {
	desired := make(map[string]struct{})
	err := fs.WalkDir(embedded, root, func(path string, d fs.DirEntry, err error) error {
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
		desired[filepath.ToSlash(rel)] = struct{}{}

		data, err := fs.ReadFile(embedded, path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}

		perm := builtinPackFileMode(path)
		return fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, dst, data, perm)
	})
	if err != nil {
		return nil, err
	}
	return desired, nil
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

func builtinPackFileMode(name string) os.FileMode {
	if isExecutableScriptFilename(name) {
		return 0o755
	}
	return 0o644
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

// pruneStaleGeneratedPackFiles treats the current binary's embedded pack tree
// as the source of truth for generated files. Concurrent older/newer binaries
// can briefly prune each other's obsolete generated-only files, but the next
// successful materialization self-heals the directory to the active binary.
func pruneStaleGeneratedPackFiles(dstDir string, desired map[string]struct{}) error {
	if _, err := os.Stat(dstDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	dirsToPrune := make(map[string]struct{})
	if err := filepath.WalkDir(dstDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dstDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := desired[rel]; ok {
			return nil
		}
		// Ignore in-flight atomic temp files so concurrent refreshes do not
		// delete each other's rename targets mid-write.
		if isGeneratedPackAtomicTempRel(rel, func(path string) bool {
			_, ok := desired[path]
			return ok
		}) {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		dirsToPrune[filepath.Dir(path)] = struct{}{}
		return nil
	}); err != nil {
		return err
	}

	pruneDirs := make([]string, 0, len(dirsToPrune))
	for dir := range dirsToPrune {
		pruneDirs = append(pruneDirs, dir)
	}
	sort.Slice(pruneDirs, func(i, j int) bool {
		left := filepath.Clean(pruneDirs[i])
		right := filepath.Clean(pruneDirs[j])
		leftDepth := strings.Count(left, string(filepath.Separator))
		rightDepth := strings.Count(right, string(filepath.Separator))
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return left > right
	})
	for _, dir := range pruneDirs {
		pruneEmptyDirs(dir, dstDir)
	}
	return nil
}

func isGeneratedPackAtomicTempRel(rel string, hasDesired func(string) bool) bool {
	idx := strings.LastIndex(rel, ".tmp.")
	return idx > 0 && hasDesired(rel[:idx])
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
