package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// Builtin packs are never materialized into the city: they compose through
// explicit [imports.<name>] entries (written by gc init, repaired by the
// builtin-pack-imports doctor check) whose bundled sources resolve from the
// user-global repo cache. The running binary pre-seeds that cache with its
// embedded content at each pack's canonical pin; a bundled source pinned at
// any other commit is an ordinary remote import and is fetched for real.
// The retired .gc/system/packs tree is pruned on sight.

var builtinRuntimeReadyCache sync.Map

type builtinRuntimeState struct {
	mu          sync.Mutex
	ready       bool
	lastWarning string
}

// ensureBuiltinPacksForConfigLoad is the shared config-load boundary for
// builtin pack readiness. It hydrates the user-global repo cache for the
// bundled sources pinned in packs.lock plus the always-required builtin
// sources (core, and bd for bd-provider cities), regenerates the stable
// gc-beads-bd shim, and prunes the retired .gc/system/packs tree. Every
// production loader routes through it so any gc command self-heals a cold
// cache after a binary upgrade or cache eviction. It injects nothing into
// config composition.
func ensureBuiltinPacksForConfigLoad(fs fsys.FS, tomlPath string, warningWriter io.Writer) error {
	if !usesOSFS(fs) {
		return nil
	}
	return EnsureBuiltinRuntimeAssets(filepath.Dir(tomlPath), warningWriter)
}

// EnsureBuiltinRuntimeAssets performs the per-city builtin readiness work
// outside config loading (init finalization, supervisor startup and city
// registration, beads bootstrap). Failures degrade to a once-per-city
// warning when the required caches are already usable.
func EnsureBuiltinRuntimeAssets(cityPath string, warningWriter io.Writer) error {
	key := normalizePathForCompare(cityPath)
	stateAny, _ := builtinRuntimeReadyCache.LoadOrStore(key, &builtinRuntimeState{})
	state := stateAny.(*builtinRuntimeState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, err := config.GlobalRepoCacheRoot(); err != nil {
		// No user-global cache root (hermetic test environment without
		// GC_HOME). There is nothing to heal: bundled-source resolution
		// surfaces its own error if config references a bundled import.
		pruneRetiredSystemPacks(cityPath, warningWriter)
		return nil
	}
	if state.ready && requiredBuiltinSourcesUsable(cityPath) && lockedBundledImportsUsable(cityPath) {
		return nil
	}
	state.ready = false

	var problems []error
	if err := ensureBundledLockedRemoteImportsCached(cityPath); err != nil {
		problems = append(problems, err)
	}
	if err := ensureRequiredBuiltinSourcesCached(cityPath); err != nil {
		problems = append(problems, err)
	}
	if err := ensureGcBeadsBdShim(cityPath); err != nil {
		problems = append(problems, fmt.Errorf("writing gc-beads-bd shim: %w", err))
	}
	pruneRetiredSystemPacks(cityPath, warningWriter)

	if len(problems) > 0 {
		if !requiredBuiltinSourcesUsable(cityPath) {
			state.lastWarning = ""
			return fmt.Errorf("preparing builtin pack caches: %w", problems[0])
		}
		const warningKey = "builtin-cache-refresh-incomplete"
		if state.lastWarning != warningKey {
			emitBuiltinRuntimeWarning(warningWriter, fmt.Errorf("builtin pack cache refresh incomplete; using existing caches: %w", problems[0]))
			state.lastWarning = warningKey
		}
		return nil
	}
	state.ready = true
	state.lastWarning = ""
	return nil
}

// requiredBuiltinSources returns the bundled sources every city with this
// configuration needs, keyed by pack name.
//
// Core is always required: it ships the role prompts referenced by
// implicit agents, the gc-* skills, mechanical housekeeping orders, and
// the per-provider hook overlays. When the beads provider is "bd" (the
// default), bd is required and its own pack imports pull in dolt
// transitively. Gastown and gascity are never required — they need an
// explicit import.
func requiredBuiltinSources(cityPath string) map[string]string {
	sources := make(map[string]string)
	for _, name := range requiredBuiltinPackNames(cityPath) {
		source, ok := builtinpacks.Source(name)
		if !ok {
			continue
		}
		sources[name] = source
	}
	return sources
}

func requiredBuiltinPackNames(cityPath string) []string {
	required := []string{"core"}

	if cityUsesBdStoreContract(cityPath) {
		required = append(required, "bd")
	}
	provider := strings.TrimSpace(configuredBeadsProviderValue(cityPath))
	usesDirectExecLifecycle := strings.HasPrefix(provider, "exec:") &&
		execProviderBase(provider) == "gc-beads-bd" &&
		normalizeRawBeadsProvider(cityPath, provider) != "bd"
	if usesDirectExecLifecycle {
		required = append(required, "dolt")
	}
	return required
}

// bundledPackImportCommit is the commit tag bundled-source caches and lock
// entries pin (config.BundledPackImportVersion without the "sha:" prefix).
func bundledPackImportCommit() string {
	return strings.TrimPrefix(config.BundledPackImportVersion, "sha:")
}

// bundledSourcePinnedVersion returns the canonical pinned version for a
// bundled source (public registry pins for gastown/gascity so they never
// conflict with the pins gc init templates write).
func bundledSourcePinnedVersion(source string) string {
	return config.BundledSourcePinnedVersion(source)
}

// requiredBuiltinImports returns the [imports.<name>] entries gc init
// writes (and the builtin-pack-imports doctor check repairs) for this
// city's providers, plus the deterministic name order.
func requiredBuiltinImports(cityPath string) (map[string]config.Import, []string) {
	return builtinImportsForNames(requiredBuiltinPackNames(cityPath))
}

// builtinImportsForInit resolves the beads provider the same way
// command-time store selection does — GC_BEADS env first, then the
// about-to-be-written city.toml provider — so init writes exactly the
// imports the builtin-pack-imports doctor check will later enforce.
func builtinImportsForInit(cityProvider string) (map[string]config.Import, []string) {
	provider := strings.TrimSpace(os.Getenv("GC_BEADS"))
	if provider == "" {
		provider = strings.TrimSpace(cityProvider)
	}
	if provider == "" {
		provider = "bd" // matches the rawBeadsProvider default
	}
	names := []string{"core"}
	if providerUsesBdStoreContract(provider) {
		names = append(names, "bd")
	}
	return builtinImportsForNames(names)
}

func builtinImportsForNames(names []string) (map[string]config.Import, []string) {
	imports := make(map[string]config.Import, len(names))
	ordered := make([]string, 0, len(names))
	for _, name := range names {
		source, ok := builtinpacks.Source(name)
		if !ok {
			continue
		}
		imports[name] = config.Import{
			Source:  source,
			Version: config.BundledSourcePinnedVersion(source),
		}
		ordered = append(ordered, name)
	}
	return imports, ordered
}

// ensureRequiredBuiltinSourcesCached hydrates the user-global cache for the
// required bundled sources at the canonical pin, independent of packs.lock,
// so the stable shim target and pre-migration cities always have the
// current binary's content available.
func ensureRequiredBuiltinSourcesCached(cityPath string) error {
	commit := bundledPackImportCommit()
	for name, source := range requiredBuiltinSources(cityPath) {
		cachePath, err := packman.RepoCachePath(source, commit)
		if err != nil {
			return fmt.Errorf("resolving cache path for bundled %s pack: %w", name, err)
		}
		if builtinpacks.ValidateSyntheticRepo(cachePath, commit) == nil {
			continue
		}
		if _, err := packman.EnsureRepoInCache(source, commit); err != nil {
			return fmt.Errorf("caching bundled %s pack: %w", name, err)
		}
	}
	return nil
}

func requiredBuiltinSourcesUsable(cityPath string) bool {
	commit := bundledPackImportCommit()
	for _, source := range requiredBuiltinSources(cityPath) {
		cachePath, err := packman.RepoCachePath(source, commit)
		if err != nil {
			return false
		}
		if builtinpacks.ValidateSyntheticRepo(cachePath, commit) != nil {
			return false
		}
	}
	return true
}

// bundledGcBeadsBdScriptTarget returns the cache-resolved bd lifecycle
// script the stable shim execs.
func bundledGcBeadsBdScriptTarget() (string, error) {
	source, ok := builtinpacks.Source("bd")
	if !ok {
		return "", fmt.Errorf("bundled bd pack is not registered")
	}
	cachePath, err := packman.RepoCachePath(source, bundledPackImportCommit())
	if err != nil {
		return "", err
	}
	pack, _ := builtinpacks.ByName("bd")
	return filepath.Join(cachePath, filepath.FromSlash(pack.Subpath), "assets", "scripts", "gc-beads-bd.sh"), nil
}

// ensureGcBeadsBdShim writes the stable per-city gc-beads-bd entrypoint
// (.gc/scripts/gc-beads-bd.sh) execing the cache-resolved bundled script.
// The shim path is what sessions and provider pins reference; this
// boundary rewrites its target whenever the binary (and therefore the
// cache location) changes. Cities on non-bd providers skip it.
func ensureGcBeadsBdShim(cityPath string) error {
	if !cityUsesBdStoreContract(cityPath) {
		return nil
	}
	target, err := bundledGcBeadsBdScriptTarget()
	if err != nil {
		return err
	}
	shim := fmt.Sprintf(`#!/bin/sh
# Generated by gc — do not edit. Stable entrypoint for the bundled bd
# lifecycle script; the target tracks the running binary's pack cache.
exec %q "$@"
`, target)
	path := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, path, []byte(shim), 0o755)
}

// pruneRetiredSystemPacks removes the retired .gc/system/packs tree.
// Builtin packs are no longer materialized per city; everything resolves
// from the user-global cache. Removal failures only warn — a leftover
// tree is inert.
//
// The prune is gated on the legacy reference migration: while the city's
// manifests still compose through the retired tree on ANY route —
// workspace includes, default-rig includes, rig includes, or city / rig /
// default-rig import sources, whether declared in city.toml, pack.toml,
// or a local config fragment — deleting the tree would either silently
// strip those packs from composition (dangling V1 workspace includes skip
// with only a log line) or hard-fail config load citywide (rig includes
// and local-path import sources do not skip). Until the migration has
// happened the tree is preserved and a once-per-city warning points at
// the doctor repair. The gate fails closed: an unreadable or unparseable
// city.toml, and any referenced fragment that cannot be inspected
// (unreadable, unparseable, or remote), preserve the inert tree rather
// than risk deleting composition state the gate cannot see.
func pruneRetiredSystemPacks(cityPath string, warningWriter io.Writer) {
	root := filepath.Join(cityPath, citylayout.SystemPacksRoot)
	if _, err := os.Lstat(root); err != nil {
		return
	}
	scan, ok := collectLegacySystemPacksRefs(cityPath)
	if !ok || len(scan.Uninspectable) > 0 {
		return
	}
	if len(scan.Refs) > 0 {
		warnLegacySystemPacksPreserved(cityPath, warningWriter)
		return
	}
	if err := os.RemoveAll(root); err != nil {
		emitBuiltinRuntimeWarning(warningWriter, fmt.Errorf("pruning retired %s: %w", citylayout.SystemPacksRoot, err))
	}
}

// legacyIncludePruneWarningCache dedups the preserved-legacy-tree warning
// to once per city per process.
var legacyIncludePruneWarningCache sync.Map

// warnLegacySystemPacksPreserved emits the once-per-city migration pointer
// for a legacy tree preserved by the prune gate. Silent loaders
// (io.Discard) must not consume the once-per-city slot: commands often
// pre-load config quietly before the user-visible load, and the warning
// has to reach the visible one.
func warnLegacySystemPacksPreserved(cityPath string, w io.Writer) {
	if w == nil || w == io.Discard {
		return
	}
	key := normalizePathForCompare(cityPath)
	if _, alreadyWarned := legacyIncludePruneWarningCache.LoadOrStore(key, struct{}{}); alreadyWarned {
		return
	}
	fmt.Fprintf(w, "warning: city config still composes builtin packs through legacy %s references; run \"gc doctor --fix\" to migrate to [imports] (the retired tree is preserved until then)\n", citylayout.SystemPacksRoot) //nolint:errcheck // best-effort warning emission
}

func emitBuiltinRuntimeWarning(w io.Writer, err error) {
	if w == nil || err == nil {
		return
	}
	fmt.Fprintf(w, "warning: %v\n", err) //nolint:errcheck // best-effort warning emission
}

func usesOSFS(fs fsys.FS) bool {
	switch fs.(type) {
	case fsys.OSFS, *fsys.OSFS:
		return true
	default:
		return false
	}
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
