package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// Builtin packs compose only through explicit [imports.<name>] entries with
// bundled sources: gc init writes them, this doctor check repairs them, and
// config load warns when they are missing. The retired
// workspace.includes = [".gc/system/packs/<name>"] model migrates here.

// missingRequiredBuiltinImports reports which required builtin packs are
// not reachable from the composed config's explicit imports/includes.
func missingRequiredBuiltinImports(fs fsys.FS, cfg *config.City, cityPath string) []string {
	if cfg == nil {
		return nil
	}
	reachable := config.ReachablePackNames(cfg, fs, cityPath)
	var missing []string
	for _, name := range requiredBuiltinPackNames(cityPath) {
		if !reachable[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// builtinImportWarningCache dedups the missing-import warning to once per
// city per process.
var builtinImportWarningCache sync.Map

// warnMissingRequiredBuiltinImports emits a once-per-city warning when the
// composed config does not reach a required builtin pack. The city still
// loads — it just runs without the builtin content it almost certainly
// wants — so this is a warning with a doctor-driven repair, not an error.
//
// Silent loaders (io.Discard) must not consume the once-per-city slot:
// commands often pre-load config quietly before the user-visible load, and
// the warning has to reach the visible one.
func warnMissingRequiredBuiltinImports(fs fsys.FS, cfg *config.City, tomlPath string, w io.Writer) {
	if w == nil || w == io.Discard || !usesOSFS(fs) {
		return
	}
	cityPath := filepath.Dir(tomlPath)
	missing := missingRequiredBuiltinImports(fs, cfg, cityPath)
	if len(missing) == 0 {
		return
	}
	key := normalizePathForCompare(cityPath)
	if _, alreadyWarned := builtinImportWarningCache.LoadOrStore(key, struct{}{}); alreadyWarned {
		return
	}
	fmt.Fprintf(w, "warning: this city does not import required builtin pack(s) %s; run \"gc doctor --fix\" to add the missing import(s)\n", strings.Join(missing, ", ")) //nolint:errcheck // best-effort warning emission
}

// legacySystemPacksIncludeEntries returns the workspace include entries that
// point at the retired per-city .gc/system/packs tree (any pack name —
// builtin packs compose via imports now).
func legacySystemPacksIncludeEntries(cityPath string, includes []string) []string {
	var stale []string
	for _, inc := range includes {
		if legacySystemPacksInclude(cityPath, inc) {
			stale = append(stale, inc)
		}
	}
	return stale
}

func legacySystemPacksInclude(cityPath, include string) bool {
	include = strings.TrimSpace(include)
	if include == "" {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(include))
	if strings.HasPrefix(cleaned, citylayout.SystemPacksRoot+"/") {
		return true
	}
	abs := cleaned
	if !filepath.IsAbs(include) {
		abs = filepath.ToSlash(filepath.Clean(filepath.Join(cityPath, filepath.FromSlash(include))))
	}
	return strings.Contains(abs, "/"+citylayout.SystemPacksRoot+"/")
}

type builtinImportDoctorCheck struct {
	cityPath string
}

func newBuiltinImportDoctorCheck(cityPath string) *builtinImportDoctorCheck {
	return &builtinImportDoctorCheck{cityPath: cityPath}
}

func (c *builtinImportDoctorCheck) Name() string { return "builtin-pack-imports" }

func (c *builtinImportDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	if _, err := os.Stat(filepath.Join(c.cityPath, "city.toml")); err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("reading city.toml: %v", err)
		return r
	}

	manifest, err := loadCityImportManifestFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("reading city.toml manifest: %v", err)
		return r
	}
	stale := legacySystemPacksIncludeEntries(c.cityPath, manifest.Workspace.LegacyIncludes())

	var missing []string
	cfg, loadErr := loadCityConfigWithoutBuiltinPackRefresh(c.cityPath, io.Discard)
	if loadErr == nil {
		missing = missingRequiredBuiltinImports(fsys.OSFS{}, cfg, c.cityPath)
	}

	if len(stale) == 0 && len(missing) == 0 && loadErr == nil {
		r.Status = doctor.StatusOK
		r.Message = "required builtin pack imports present"
		return r
	}

	if loadErr != nil && len(stale) == 0 {
		// Config does not load and no legacy system-packs include explains
		// it; other doctor checks own general config errors.
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("cannot evaluate builtin imports: %v", loadErr)
		return r
	}

	r.Status = doctor.StatusError
	r.FixHint = `run "gc doctor --fix" to migrate builtin pack composition to [imports]`
	var parts []string
	for _, inc := range stale {
		r.Details = append(r.Details, fmt.Sprintf("legacy-system-packs-include | %s | builtin packs compose via [imports] now", inc))
	}
	if len(stale) > 0 {
		parts = append(parts, fmt.Sprintf("%d legacy .gc/system/packs include(s)", len(stale)))
	}
	for _, name := range missing {
		r.Details = append(r.Details, fmt.Sprintf("missing-builtin-import | %s | add [imports.%s] with the bundled source", name, name))
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d missing required builtin import(s)", len(missing)))
	}
	r.Message = strings.Join(parts, ", ")
	return r
}

func (c *builtinImportDoctorCheck) CanFix() bool { return true }

func (c *builtinImportDoctorCheck) Fix(_ *doctor.CheckContext) error {
	// 1. Strip legacy .gc/system/packs includes from city.toml.
	manifest, err := loadCityImportManifestFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		return fmt.Errorf("reading city.toml manifest: %w", err)
	}
	cityChanged := false
	includes := manifest.Workspace.LegacyIncludes()
	kept := make([]string, 0, len(includes))
	for _, inc := range includes {
		if legacySystemPacksInclude(c.cityPath, inc) {
			cityChanged = true
			continue
		}
		kept = append(kept, inc)
	}
	if cityChanged {
		manifest.Workspace.SetLegacyIncludes(kept)
		if err := writeCityImportManifestFS(fsys.OSFS{}, c.cityPath, manifest); err != nil {
			return fmt.Errorf("writing city.toml: %w", err)
		}
	}

	// 2. Ensure the required builtin imports exist in the pack.toml
	// manifest — the canonical home for city-level imports and the only
	// surface the lockfile collection reads. Legacy cities without a
	// pack.toml get a minimal one, matching the migrate tool's behavior.
	missing := c.missingAfterIncludeStrip()
	if len(missing) > 0 {
		imports, order := builtinImportsForNames(missing)
		packManifest, err := loadCityPackManifestFS(fsys.OSFS{}, c.cityPath)
		if err != nil {
			return fmt.Errorf("reading pack.toml manifest: %w", err)
		}
		if strings.TrimSpace(packManifest.Pack.Name) == "" {
			packManifest.Pack.Name = filepath.Base(c.cityPath)
		}
		if packManifest.Pack.Schema == 0 {
			packManifest.Pack.Schema = 2
		}
		if packManifest.Imports == nil {
			packManifest.Imports = make(map[string]config.Import, len(order))
		}
		for _, name := range order {
			if _, exists := packManifest.Imports[name]; !exists {
				packManifest.Imports[name] = imports[name]
			}
		}
		if err := writeCityPackManifest(fsys.OSFS{}, c.cityPath, packManifest); err != nil {
			return fmt.Errorf("writing pack.toml: %w", err)
		}
	}

	// 3. Refresh the lockfile + caches so the new imports resolve offline.
	allImports, err := collectAllImportsFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		return fmt.Errorf("reading declared imports: %w", err)
	}
	lock, err := syncImports(c.cityPath, allImports, packman.InstallResolveIfNeeded)
	if err != nil {
		return err
	}
	if err := writeImportLockfile(fsys.OSFS{}, c.cityPath, lock); err != nil {
		return err
	}
	if _, err := installLockedImports(c.cityPath); err != nil {
		return err
	}
	return nil
}

// missingAfterIncludeStrip recomputes the missing required builtin packs
// after the legacy include strip, falling back to "everything required and
// not literally imported" when the config still does not compose.
func (c *builtinImportDoctorCheck) missingAfterIncludeStrip() []string {
	if cfg, loadErr := loadCityConfigWithoutBuiltinPackRefresh(c.cityPath, io.Discard); loadErr == nil {
		return missingRequiredBuiltinImports(fsys.OSFS{}, cfg, c.cityPath)
	}
	declared, err := collectAllImportsFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		declared = nil
	}
	var missing []string
	for _, name := range requiredBuiltinPackNames(c.cityPath) {
		if _, ok := declared[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}
