// Package citylayout centralizes city-root discovery and path resolution
// for visible content roots.
package citylayout

import (
	"os"
	"path/filepath"
)

// Canonical city layout roots.
const (
	// CityConfigFile is the canonical marker file for a city root.
	CityConfigFile = "city.toml"

	RuntimeRoot = ".gc"

	PromptsRoot  = "prompts"
	FormulasRoot = "formulas"
	OrdersRoot   = "orders"

	HooksRoot      = "hooks"
	ClaudeHookFile = "hooks/claude.json"

	ScriptsRoot = "scripts"

	SystemRoot      = ".gc/system"
	SystemPacksRoot = ".gc/system/packs"

	CacheRoot         = ".gc/cache"
	CachePacksRoot    = ".gc/cache/packs"
	CacheIncludesRoot = ".gc/cache/includes"
)

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

// HasRuntimeRoot reports whether dir contains the .gc/ runtime root.
func HasRuntimeRoot(dir string) bool {
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

// FormulasPath returns the absolute path to the formulas directory.
func FormulasPath(cityRoot string) string { return filepath.Join(cityRoot, FormulasRoot) }

// OrdersPath returns the absolute path to the orders directory.
func OrdersPath(cityRoot string) string { return filepath.Join(cityRoot, OrdersRoot) }

// ScriptsPath returns the absolute path to the scripts directory.
func ScriptsPath(cityRoot string) string { return filepath.Join(cityRoot, ScriptsRoot) }

// ClaudeHookFilePath returns the absolute path to the Claude hook file.
func ClaudeHookFilePath(cityRoot string) string { return filepath.Join(cityRoot, ClaudeHookFile) }

// ResolveFormulasDir resolves the city-local formulas directory. If configured
// is empty or ".", returns the default formulas path. If absolute, returns as-is.
// Otherwise joins with cityRoot.
func ResolveFormulasDir(cityRoot, configured string) string {
	normalized := filepath.Clean(configured)
	if normalized == "." || normalized == "" {
		return filepath.Join(cityRoot, FormulasRoot)
	}
	if filepath.IsAbs(configured) {
		return configured
	}
	return filepath.Join(cityRoot, configured)
}
