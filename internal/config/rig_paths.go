package config

import (
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// HydrateRigPaths fills missing rig paths from the machine-local registry for
// the given city. This keeps Phase A runtime callers working while city.toml
// stops persisting rig.path.
func HydrateRigPaths(cfg *City, cityPath string) error {
	if cfg == nil || len(cfg.Rigs) == 0 || cityPath == "" {
		return nil
	}
	regPath, ok := defaultRegistryPath()
	if !ok {
		return nil
	}
	return hydrateRigPathsWithRegistry(cfg, cityPath, supervisor.NewRegistry(regPath))
}

func hydrateRigPathsWithRegistry(cfg *City, cityPath string, reg *supervisor.Registry) error {
	if cfg == nil || reg == nil {
		return nil
	}
	cityCanonical := canonicalRigComparePath(cityPath)
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Path != "" {
			continue
		}
		entry, ok := reg.LookupRigByName(cfg.Rigs[i].Name)
		if !ok || entry.Path == "" {
			continue
		}
		if entry.DefaultCity != "" && cityCanonical != "" && canonicalRigComparePath(entry.DefaultCity) != cityCanonical {
			continue
		}
		cfg.Rigs[i].Path = entry.Path
		cfg.Rigs[i].PathMachineLocal = true
	}
	return nil
}

func defaultRegistryPath() (path string, ok bool) {
	defer func() {
		if recover() != nil {
			path = ""
			ok = false
		}
	}()
	path = supervisor.RegistryPath()
	if path == "" {
		return "", false
	}
	return path, true
}

func canonicalRigComparePath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	if info, err := os.Stat(abs); err == nil && info != nil {
		return abs
	}
	return filepath.Clean(abs)
}
