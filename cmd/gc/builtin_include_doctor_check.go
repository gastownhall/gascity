package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Builtin packs compose only through explicit city.toml includes: gc init
// writes them, this doctor check repairs them, and config load warns when
// they are missing. Nothing splices builtin packs into composition.

// missingRequiredBuiltinIncludes reports which required builtin packs are
// not reachable from the composed config's explicit includes and imports.
func missingRequiredBuiltinIncludes(fs fsys.FS, cfg *config.City, cityPath string) []string {
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

// builtinIncludeWarningCache dedups the missing-include warning to once per
// city per process, mirroring the builtin pack refresh warning behavior.
var builtinIncludeWarningCache sync.Map

// warnMissingRequiredBuiltinIncludes emits a once-per-city warning when the
// composed config does not reach a required builtin pack. The city still
// loads — it just runs without the builtin content it almost certainly
// wants — so this is a warning with a doctor-driven repair, not an error.
//
// Silent loaders (io.Discard) must not consume the once-per-city slot:
// commands often pre-load config quietly before the user-visible load, and
// the warning has to reach the visible one.
func warnMissingRequiredBuiltinIncludes(fs fsys.FS, cfg *config.City, tomlPath string, w io.Writer) {
	if w == nil || w == io.Discard || !usesOSFS(fs) {
		return
	}
	cityPath := filepath.Dir(tomlPath)
	missing := missingRequiredBuiltinIncludes(fs, cfg, cityPath)
	if len(missing) == 0 {
		return
	}
	key := normalizePathForCompare(cityPath)
	if _, alreadyWarned := builtinIncludeWarningCache.LoadOrStore(key, struct{}{}); alreadyWarned {
		return
	}
	fmt.Fprintf(w, "warning: city.toml does not include required builtin pack(s) %s; run \"gc doctor --fix\" to add the missing include(s)\n", strings.Join(missing, ", ")) //nolint:errcheck // best-effort warning emission
}

// retiredBuiltinIncludeEntries returns the workspace include entries that
// point at retired builtin system packs (e.g. .gc/system/packs/maintenance),
// which the binary no longer materializes.
func retiredBuiltinIncludeEntries(cityPath string, includes []string) []string {
	var stale []string
	for _, inc := range includes {
		if retiredBuiltinPackForInclude(cityPath, inc) != "" {
			stale = append(stale, inc)
		}
	}
	return stale
}

func retiredBuiltinPackForInclude(cityPath, include string) string {
	include = strings.TrimSpace(include)
	if include == "" {
		return ""
	}
	cleaned := filepath.ToSlash(filepath.Clean(include))
	abs := cleaned
	if !filepath.IsAbs(include) {
		abs = filepath.ToSlash(filepath.Clean(filepath.Join(cityPath, filepath.FromSlash(include))))
	}
	for _, name := range retiredBuiltinPackNames {
		canonical := builtinIncludePathForPack(name)
		if cleaned == canonical || strings.HasSuffix(abs, "/"+canonical) {
			return name
		}
	}
	return ""
}

type builtinIncludeDoctorCheck struct {
	cityPath string
}

func newBuiltinIncludeDoctorCheck(cityPath string) *builtinIncludeDoctorCheck {
	return &builtinIncludeDoctorCheck{cityPath: cityPath}
}

func (c *builtinIncludeDoctorCheck) Name() string { return "builtin-pack-includes" }

func (c *builtinIncludeDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
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
	stale := retiredBuiltinIncludeEntries(c.cityPath, manifest.Workspace.LegacyIncludes())

	var missing []string
	cfg, loadErr := loadCityConfigWithoutBuiltinPackRefresh(c.cityPath, io.Discard)
	if loadErr == nil {
		missing = missingRequiredBuiltinIncludes(fsys.OSFS{}, cfg, c.cityPath)
	}

	if len(stale) == 0 && len(missing) == 0 && loadErr == nil {
		r.Status = doctor.StatusOK
		r.Message = "required builtin pack includes present"
		return r
	}

	if loadErr != nil && len(stale) == 0 {
		// Config does not load and no stale builtin include explains it;
		// other doctor checks own general config errors.
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("cannot evaluate builtin includes: %v", loadErr)
		return r
	}

	r.Status = doctor.StatusError
	r.FixHint = `run "gc doctor --fix" to repair builtin pack includes in city.toml`
	var parts []string
	for _, inc := range stale {
		r.Details = append(r.Details, fmt.Sprintf("retired-builtin-include | %s | folded into the bundled core pack", inc))
	}
	if len(stale) > 0 {
		parts = append(parts, fmt.Sprintf("%d retired builtin include(s)", len(stale)))
	}
	for _, name := range missing {
		r.Details = append(r.Details, fmt.Sprintf("missing-builtin-include | %s | add %s to [workspace] includes", name, builtinIncludePathForPack(name)))
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d missing required builtin include(s)", len(missing)))
	}
	r.Message = strings.Join(parts, ", ")
	return r
}

func (c *builtinIncludeDoctorCheck) CanFix() bool { return true }

func (c *builtinIncludeDoctorCheck) Fix(_ *doctor.CheckContext) error {
	manifest, err := loadCityImportManifestFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		return fmt.Errorf("reading city.toml manifest: %w", err)
	}

	changed := false
	includes := manifest.Workspace.LegacyIncludes()
	kept := make([]string, 0, len(includes))
	for _, inc := range includes {
		if retiredBuiltinPackForInclude(c.cityPath, inc) != "" {
			changed = true
			continue
		}
		kept = append(kept, inc)
	}

	missing := requiredBuiltinPackNames(c.cityPath)
	if cfg, loadErr := loadCityConfigWithoutBuiltinPackRefresh(c.cityPath, io.Discard); loadErr == nil {
		missing = missingRequiredBuiltinIncludes(fsys.OSFS{}, cfg, c.cityPath)
	} else {
		// Config does not compose (possibly because of the stale includes
		// removed above). Conservatively ensure the canonical paths are
		// present rather than skipping the repair.
		var unlisted []string
		for _, name := range missing {
			listed := false
			for _, inc := range kept {
				if filepath.ToSlash(filepath.Clean(inc)) == builtinIncludePathForPack(name) {
					listed = true
					break
				}
			}
			if !listed {
				unlisted = append(unlisted, name)
			}
		}
		missing = unlisted
	}
	for _, name := range missing {
		kept = append(kept, builtinIncludePathForPack(name))
		changed = true
	}

	if !changed {
		return nil
	}
	manifest.Workspace.SetLegacyIncludes(kept)
	if err := writeCityImportManifestFS(fsys.OSFS{}, c.cityPath, manifest); err != nil {
		return fmt.Errorf("writing city.toml: %w", err)
	}
	return nil
}
