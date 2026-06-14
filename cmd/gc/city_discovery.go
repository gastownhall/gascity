package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
)

type cityDiscoveryOptions struct {
	ceilingDirs          []string
	ignoredLegacyRuntime []string
}

// findCity walks dir upward looking for a directory containing city.toml.
// Implicit discovery is bounded so it does not accidentally resolve unrelated
// ancestors such as $HOME or the supervisor's global ~/.gc runtime root.
func findCity(dir string) (string, error) {
	return findCityWithOptions(dir, implicitCityDiscoveryOptions())
}

func findCityWithOptions(dir string, opts cityDiscoveryOptions) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	var legacy string
	for {
		if citylayout.HasCityConfig(dir) {
			return dir, nil
		}
		if legacy == "" && !isCityDiscoveryCeiling(dir, opts.ceilingDirs) && citylayout.HasRuntimeRoot(dir) && !isIgnoredLegacyRuntimeRoot(dir, opts.ignoredLegacyRuntime) {
			legacy = dir
		}
		if isCityDiscoveryCeiling(dir, opts.ceilingDirs) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if legacy != "" {
		return legacy, nil
	}
	return "", fmt.Errorf("not in a city directory (no city.toml or .gc/ found)")
}

func implicitCityDiscoveryOptions() cityDiscoveryOptions {
	return cityDiscoveryOptions{
		ceilingDirs:          implicitCityDiscoveryCeilings(),
		ignoredLegacyRuntime: implicitIgnoredLegacyRuntimeRoots(),
	}
}

func implicitCityDiscoveryCeilings() []string {
	var paths []string
	if raw := strings.TrimSpace(os.Getenv("GC_CEILING_DIRECTORIES")); raw != "" {
		paths = append(paths, strings.Split(raw, string(os.PathListSeparator))...)
	}
	home, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(home) != "" {
		paths = append(paths, home)
	}
	if tmp := strings.TrimSpace(os.TempDir()); tmp != "" {
		paths = append(paths, tmp)
	}
	return normalizeDiscoveryPaths(paths)
}

func implicitIgnoredLegacyRuntimeRoots() []string {
	var ignored []string
	if runtimeRoot := configuredSupervisorRuntimeRoot(); runtimeRoot != "" {
		ignored = append(ignored, runtimeRoot)
	}
	// Also ignore .gc/ under every ancestor of os.TempDir(). When a gc city
	// is running, its runtime state may live at TMPDIR/.gc/ (e.g. /tmp/.gc/).
	// Test processes receive a prefixed TMPDIR like /tmp/gct.../; walking up
	// every ancestor ensures /tmp/.gc/ is ignored when the live city uses
	// /tmp as its runtime root.
	for dir := normalizeDiscoveryPath(os.TempDir()); dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		ignored = append(ignored, filepath.Join(dir, citylayout.RuntimeRoot))
	}
	return ignored
}

func configuredSupervisorRuntimeRoot() string {
	if gcHome := strings.TrimSpace(os.Getenv("GC_HOME")); gcHome != "" {
		return normalizeDiscoveryPath(gcHome)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(normalizeDiscoveryPath(home), citylayout.RuntimeRoot)
}

func isCityDiscoveryCeiling(dir string, ceilings []string) bool {
	dir = normalizeDiscoveryPath(dir)
	for _, ceiling := range ceilings {
		if dir == ceiling {
			return true
		}
	}
	return false
}

func isIgnoredLegacyRuntimeRoot(dir string, ignored []string) bool {
	runtimeRoot := filepath.Join(normalizeDiscoveryPath(dir), citylayout.RuntimeRoot)
	for _, candidate := range ignored {
		if runtimeRoot == candidate {
			return true
		}
	}
	return false
}

func normalizeDiscoveryPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = normalizeDiscoveryPath(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

func normalizeDiscoveryPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	return filepath.Clean(path)
}
