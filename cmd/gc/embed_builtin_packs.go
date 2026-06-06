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
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	legacyOrderConfigFile = "order.toml"
)

// builtinPacks lists all packs embedded in the gc binary. These are
// materialized to .gc/system/packs/ on every gc start and gc init.
var builtinPacks = builtinpacks.All()

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
// atomic rename so readers never observe a truncated file. Executable scripts
// get 0755; everything else 0644.
//
// Operator edits are preserved only for non-required packs: a regular,
// correct-mode file in a non-required pack is left untouched even when its
// content differs from the embedded bytes (see gastownhall/gascity#2429).
// Required packs (core, maintenance, and the provider-dependent bd/dolt) are
// always refreshed and validated, so a stale or corrupt required pack on disk
// is repaired rather than silently accepted.
// Idempotent: safe to call on every gc start and gc init.
func MaterializeBuiltinPacks(cityPath string) error {
	required := requiredBuiltinPackSet(cityPath)
	for _, bp := range builtinPacks {
		dst := filepath.Join(cityPath, citylayout.SystemPacksRoot, bp.Name)
		_, isRequired := required[bp.Name]
		desired, err := materializeFS(bp.FS, dst, !isRequired)
		if err != nil {
			return fmt.Errorf("materializing %s pack: %w", bp.Name, err)
		}
		if err := pruneStaleGeneratedPackFiles(dst, desired); err != nil {
			return fmt.Errorf("pruning stale %s pack files: %w", bp.Name, err)
		}
		if err := pruneLegacyEmbeddedOrders(bp.FS, dst); err != nil {
			return fmt.Errorf("pruning legacy %s order paths: %w", bp.Name, err)
		}
	}
	if err := repairLegacyGcBeadsBdScript(cityPath); err != nil {
		return fmt.Errorf("repairing legacy gc-beads-bd script: %w", err)
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
		if !ok || !packContainsEmbeddedState(bp.FS, filepath.Join(systemRoot, name)) {
			missing = append(missing, name)
		}
	}
	return missing
}

func builtinPackByName(name string) (builtinpacks.Pack, bool) {
	for _, bp := range builtinPacks {
		if bp.Name == name {
			return bp, true
		}
	}
	return builtinpacks.Pack{}, false
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
			perm: builtinpacks.MaterializedFileMode(path),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

// requiredBuiltinPackSet returns the set of builtin pack names that must stay
// in lockstep with the embedded bytes for the city at cityPath. Required packs
// are refreshed and validated on every materialize; operator edits to them are
// not preserved. Derived from requiredBuiltinPackNames so the set tracks the
// provider-dependent membership (bd/dolt) exactly.
func requiredBuiltinPackSet(cityPath string) map[string]struct{} {
	names := requiredBuiltinPackNames(cityPath)
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
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
			Backend  string `toml:"backend"`
		} `toml:"beads"`
	}
	if _, err := toml.Decode(string(data), &peek); err != nil {
		return ""
	}
	return peek.Beads.Provider
}

func peekBeadsBackend(tomlPath string) string {
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	var peek struct {
		Beads struct {
			Backend string `toml:"backend"`
		} `toml:"beads"`
	}
	if _, err := toml.Decode(string(data), &peek); err != nil {
		return ""
	}
	return peek.Beads.Backend
}

// peekEventsProvider reads just the events.provider field from a city.toml
// without doing full config parsing. Returns "" if not set or on error.
//
// Used by gc event emit (called from bd hooks on every bead write) to avoid
// the full loadCityConfig path, which resolves [imports] and runs
// `git status --porcelain --ignored` against every cached pack-source repo
// — slow on hosts where a pack source is a large monorepo, and fan-out
// concurrent across a bd-write burst (see gastownhall/gascity#2099).
//
// Trade-off: include/import/pack-provided overrides of [events].provider are
// not honored on this hook fast path. Operators that need this path to bypass
// city.toml should use the GC_EVENTS env var.
func peekEventsProvider(tomlPath string) string {
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	var peek struct {
		Events struct {
			Provider string `toml:"provider"`
		} `toml:"events"`
	}
	if _, err := toml.Decode(string(data), &peek); err != nil {
		return ""
	}
	return peek.Events.Provider
}

// materializeFS walks an embed.FS, writes all files to dstDir, and returns the
// relative file paths that belong in the generated directory.
//
// When preserveOperatorEdits is true, existing regular files with the correct
// mode are preserved verbatim — content is NOT overwritten even when it differs
// from the embedded bytes. This protects operator-authored edits to non-required
// pack files (formula TOMLs, command scripts, etc.) from being silently reverted
// on every gc subcommand invocation (see gastownhall/gascity#2429). Operators
// who want to pick up a fresh embedded version after a binary upgrade must delete
// the on-disk file first.
//
// When preserveOperatorEdits is false (required packs), the preservation skip is
// disabled: every file is refreshed and validated against the embedded bytes, so
// a stale or corrupt required pack is repaired rather than silently accepted.
//
// The remaining repair semantics are independent of the flag: missing files are
// written (initial scaffolding), wrong-mode files are rewritten (e.g., script
// that lost its +x bit), and non-regular files (symlinks, etc.) are replaced
// with the embedded content.
func materializeFS(embedded fs.FS, dstDir string, preserveOperatorEdits bool) (map[string]struct{}, error) {
	desired := make(map[string]struct{})
	err := fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		dst := filepath.Join(dstDir, path)

		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		desired[filepath.ToSlash(path)] = struct{}{}

		perm := builtinpacks.MaterializedFileMode(path)

		// Preserve operator-authored content for non-required packs. Skip the
		// embedded write only when the existing on-disk entry is a regular file
		// with the correct mode — that's a file the operator might have edited.
		// Non-regular files (symlinks) and wrong-mode files still get repaired
		// below, matching the prior contract. Mode comparison uses
		// fsys.ComparableMode (perm + setuid/setgid/sticky) so it agrees with
		// the WriteFileIfContentOrModeChangedAtomic repair path below. Required
		// packs (preserveOperatorEdits == false) skip this branch entirely so
		// stale content is always refreshed.
		if preserveOperatorEdits {
			if info, statErr := os.Lstat(dst); statErr == nil {
				if info.Mode().IsRegular() && fsys.ComparableMode(info.Mode()) == fsys.ComparableMode(perm) {
					return nil
				}
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("stat %s: %w", dst, statErr)
			}
		}

		data, err := fs.ReadFile(embedded, path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}

		return fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, dst, data, perm)
	})
	if err != nil {
		return nil, err
	}
	return desired, nil
}

func repairLegacyGcBeadsBdScript(cityPath string) error {
	path := legacyGcBeadsBdScriptPath(cityPath)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !looksLikeGeneratedGcBeadsBdScript(data) {
		return nil
	}
	return fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, path, legacyGcBeadsBdShim(), 0o755)
}

func looksLikeGeneratedGcBeadsBdScript(data []byte) bool {
	text := string(data)
	return strings.Contains(text, "gc-beads-bd") && strings.Contains(text, "exec: beads provider")
}

func legacyGcBeadsBdShim() []byte {
	return []byte(`#!/bin/sh
set -eu

script_dir=$(dirname "$0")
city_root=$(cd "$script_dir/../.." && pwd)

exec "$city_root/.gc/system/packs/bd/assets/scripts/gc-beads-bd.sh" "$@"
`)
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
