// Package citylayout centralizes city-root discovery and compatibility-aware
// path resolution for the visible content roots introduced by layout v2.
package citylayout

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Canonical city layout roots and compatibility aliases.
const (
	// CityConfigFile is the canonical marker file for a city root.
	CityConfigFile = "city.toml"

	RuntimeRoot = ".gc"

	PromptsRoot        = "prompts"
	LegacyPromptsRoot  = ".gc/prompts"
	FormulasRoot       = "formulas"
	LegacyFormulasRoot = ".gc/formulas"

	AutomationsRoot       = "automations"
	LegacyAutomationsRoot = ".gc/formulas/automations"

	HooksRoot            = "hooks"
	ClaudeHookFile       = "hooks/claude.json"
	LegacyClaudeHookFile = ".gc/settings.json"

	ScriptsRoot       = "scripts"
	LegacyScriptsRoot = ".gc/scripts"

	SystemRoot         = ".gc/system"
	SystemPromptsRoot  = ".gc/system/prompts"
	SystemFormulasRoot = ".gc/system/formulas"
	SystemPacksRoot    = ".gc/system/packs"
	SystemBinRoot      = ".gc/system/bin"

	LegacySystemFormulasRoot = ".gc/system-formulas"
	LegacyPacksRoot          = ".gc/packs"
	LegacySystemBinRoot      = ".gc/bin"
	LegacyBeadsBdScript      = ".gc/bin/gc-beads-bd"

	CacheRoot         = ".gc/cache"
	CachePacksRoot    = ".gc/cache/packs"
	CacheIncludesRoot = ".gc/cache/includes"
)

// ManagedAsset identifies one of the city-owned content roots that supports
// canonical/legacy compatibility resolution.
type ManagedAsset int

// Managed asset kinds supported by the compatibility resolver.
const (
	// AssetUnknown means the path does not target a managed city-owned root.
	AssetUnknown ManagedAsset = iota
	AssetPrompt
	AssetFormula
	AssetAutomation
	AssetClaudeHook
	AssetScript
)

// ManagedRef reports the canonical and legacy relative paths for a managed
// city-owned asset, plus which variant was selected.
type ManagedRef struct {
	Asset      ManagedAsset
	Canonical  string
	Legacy     string
	UsedLegacy bool
	Shadowed   bool
}

// HasCityConfig reports whether dir contains the canonical city marker file.
func HasCityConfig(dir string) bool {
	if dir == "" {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, CityConfigFile)); err == nil && !fi.IsDir() {
		return true
	}
	return false
}

// HasLegacyRuntimeRoot reports whether dir contains the legacy .gc/ marker.
func HasLegacyRuntimeRoot(dir string) bool {
	if dir == "" {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, RuntimeRoot)); err == nil && fi.IsDir() {
		return true
	}
	return false
}

// RuntimePath joins rel under the city runtime root.
func RuntimePath(cityRoot string, rel ...string) string {
	parts := append([]string{cityRoot, RuntimeRoot}, rel...)
	return filepath.Join(parts...)
}

// SystemPath joins rel under the city system root.
func SystemPath(cityRoot string, rel ...string) string {
	parts := append([]string{cityRoot, SystemRoot}, rel...)
	return filepath.Join(parts...)
}

// CachePath joins rel under the city cache root.
func CachePath(cityRoot string, rel ...string) string {
	parts := append([]string{cityRoot, CacheRoot}, rel...)
	return filepath.Join(parts...)
}

// CanonicalRef rewrites a managed legacy ref to its visible canonical form.
func CanonicalRef(ref string) string {
	m, ok := managedRoots(ref)
	if !ok {
		return ref
	}
	if strings.HasPrefix(ref, m.legacy) {
		return m.canonical + strings.TrimPrefix(ref, m.legacy)
	}
	if ref == m.legacy {
		return m.canonical
	}
	return ref
}

// ResolveCityOwnedPath resolves a managed ref into its canonical and legacy
// counterparts and notes whether the legacy copy was selected or shadowed.
func ResolveCityOwnedPath(fs fsys.FS, cityRoot, ref string) ManagedRef {
	m, ok := managedRoots(ref)
	if !ok {
		return ManagedRef{
			Asset:     AssetUnknown,
			Canonical: ref,
			Legacy:    ref,
		}
	}

	result := ManagedRef{
		Asset:     m.asset,
		Canonical: m.canonical + strings.TrimPrefix(ref, m.activePrefix(ref)),
		Legacy:    m.legacy + strings.TrimPrefix(ref, m.activePrefix(ref)),
	}

	if ref == m.legacy {
		result.Canonical = m.canonical
		result.Legacy = m.legacy
	}

	canonicalExists := pathExists(fs, filepath.Join(cityRoot, result.Canonical))
	legacyExists := pathExists(fs, filepath.Join(cityRoot, result.Legacy))

	switch {
	case canonicalExists:
		result.Shadowed = legacyExists && result.Legacy != result.Canonical
	case legacyExists:
		result.UsedLegacy = true
	default:
		result.UsedLegacy = strings.HasPrefix(ref, m.legacy)
	}

	return result
}

// ResolveReadPath resolves ref to the on-disk path that should be read.
func ResolveReadPath(fs fsys.FS, cityRoot, ref string) string {
	resolved := ResolveCityOwnedPath(fs, cityRoot, ref)
	if resolved.Asset == AssetUnknown {
		return filepath.Join(cityRoot, ref)
	}
	if pathExists(fs, filepath.Join(cityRoot, resolved.Canonical)) {
		return filepath.Join(cityRoot, resolved.Canonical)
	}
	if pathExists(fs, filepath.Join(cityRoot, resolved.Legacy)) {
		return filepath.Join(cityRoot, resolved.Legacy)
	}
	return filepath.Join(cityRoot, resolved.Canonical)
}

// ResolveScriptsDir resolves the city-level helper script source directory.
func ResolveScriptsDir(fs fsys.FS, cityRoot string) string {
	canonical := filepath.Join(cityRoot, ScriptsRoot)
	if pathExists(fs, canonical) {
		return canonical
	}
	legacy := filepath.Join(cityRoot, LegacyScriptsRoot)
	if pathExists(fs, legacy) {
		return legacy
	}
	return canonical
}

// ResolveClaudeHookPath resolves the city-level Claude hook source file.
func ResolveClaudeHookPath(fs fsys.FS, cityRoot string) ManagedRef {
	return ResolveCityOwnedPath(fs, cityRoot, ClaudeHookFile)
}

// ResolveCityFormulasDir resolves the city-local formulas directory, honoring
// the visible root first and falling back to the legacy root during migration.
func ResolveCityFormulasDir(fs fsys.FS, cityRoot, configured string) string {
	normalized := filepath.Clean(configured)
	if normalized == "." || normalized == "" {
		normalized = FormulasRoot
	}
	switch normalized {
	case FormulasRoot:
		canonical := filepath.Join(cityRoot, FormulasRoot)
		if pathExists(fs, canonical) {
			return canonical
		}
		legacy := filepath.Join(cityRoot, LegacyFormulasRoot)
		if pathExists(fs, legacy) {
			return legacy
		}
		return canonical
	case LegacyFormulasRoot:
		canonical := filepath.Join(cityRoot, FormulasRoot)
		if pathExists(fs, canonical) {
			return canonical
		}
		legacy := filepath.Join(cityRoot, LegacyFormulasRoot)
		if pathExists(fs, legacy) {
			return legacy
		}
		return canonical
	default:
		if filepath.IsAbs(configured) {
			return configured
		}
		return filepath.Join(cityRoot, configured)
	}
}

// ResolveCityAutomationRoots returns the ordered city-local automation roots
// for compatibility scanning: legacy first, canonical second.
func ResolveCityAutomationRoots(fs fsys.FS, cityRoot string) []string {
	var roots []string
	legacy := filepath.Join(cityRoot, LegacyAutomationsRoot)
	canonical := filepath.Join(cityRoot, AutomationsRoot)

	if pathExists(fs, legacy) {
		roots = append(roots, legacy)
	}
	if pathExists(fs, canonical) {
		roots = append(roots, canonical)
	}
	if len(roots) == 0 {
		roots = append(roots, canonical)
	}
	return roots
}

func pathExists(fs fsys.FS, path string) bool {
	if _, err := fs.Stat(path); err == nil {
		return true
	}
	return false
}

type managedRoot struct {
	asset     ManagedAsset
	canonical string
	legacy    string
}

func (m managedRoot) activePrefix(ref string) string {
	switch {
	case ref == m.canonical, strings.HasPrefix(ref, m.canonical+"/"):
		return m.canonical
	case ref == m.legacy, strings.HasPrefix(ref, m.legacy+"/"):
		return m.legacy
	default:
		return m.canonical
	}
}

func managedRoots(ref string) (managedRoot, bool) {
	for _, candidate := range []managedRoot{
		{asset: AssetClaudeHook, canonical: ClaudeHookFile, legacy: LegacyClaudeHookFile},
		{asset: AssetAutomation, canonical: AutomationsRoot, legacy: LegacyAutomationsRoot},
		{asset: AssetPrompt, canonical: PromptsRoot, legacy: LegacyPromptsRoot},
		{asset: AssetFormula, canonical: FormulasRoot, legacy: LegacyFormulasRoot},
		{asset: AssetScript, canonical: ScriptsRoot, legacy: LegacyScriptsRoot},
	} {
		if ref == candidate.canonical || ref == candidate.legacy {
			return candidate, true
		}
		if strings.HasPrefix(ref, candidate.canonical+"/") || strings.HasPrefix(ref, candidate.legacy+"/") {
			return candidate, true
		}
	}
	return managedRoot{}, false
}
