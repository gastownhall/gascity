package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	legacyRigPathSiteBindingWarningFragment = "still declares path in city.toml; move it to .gc/site.toml"
	unknownRigSiteBindingWarningPrefix      = ".gc/site.toml declares a binding for unknown rig "
)

// IsNonFatalSiteBindingWarning reports whether warning is migration guidance
// that should stay non-fatal in strict mode.
func IsNonFatalSiteBindingWarning(warning string) bool {
	return strings.Contains(warning, legacyRigPathSiteBindingWarningFragment) ||
		strings.HasPrefix(warning, unknownRigSiteBindingWarningPrefix)
}

func legacyRigPathSiteBindingWarning(name string) string {
	return fmt.Sprintf("rig %q %s (run `gc doctor --fix`)", name, legacyRigPathSiteBindingWarningFragment)
}

func missingRigSiteBindingWarning(name string) string {
	return fmt.Sprintf(
		"rig %q is declared in city.toml but has no path binding in .gc/site.toml; run `gc rig add <dir> --name %s` to bind it",
		name,
		name,
	)
}

func unknownRigSiteBindingWarning(name string) string {
	return fmt.Sprintf("%s%q", unknownRigSiteBindingWarningPrefix, name)
}

// SiteBindingPath returns the machine-local site binding file for a city.
func SiteBindingPath(cityRoot string) string {
	return filepath.Join(cityRoot, citylayout.RuntimeRoot, "site.toml")
}

// SiteBinding stores machine-local rig bindings for a city.
type SiteBinding struct {
	Rigs []RigSiteBinding `toml:"rig,omitempty"`
}

// RigSiteBinding binds a declared rig name to a machine-local path.
type RigSiteBinding struct {
	Name string `toml:"name"`
	Path string `toml:"path,omitempty"`
}

// LoadSiteBinding reads .gc/site.toml. Missing files return an empty binding.
func LoadSiteBinding(fs fsys.FS, cityRoot string) (*SiteBinding, error) {
	path := SiteBindingPath(cityRoot)
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &SiteBinding{}, nil
		}
		return nil, fmt.Errorf("loading site binding %q: %w", path, err)
	}
	var binding SiteBinding
	if _, err := toml.Decode(string(data), &binding); err != nil {
		return nil, fmt.Errorf("parsing site binding %q: %w", path, err)
	}
	return &binding, nil
}

// ApplySiteBindings overlays .gc/site.toml onto cfg. Site bindings take
// precedence, but legacy city.toml rig paths still flow through as a
// compatibility fallback until users migrate them into .gc/site.toml.
func ApplySiteBindings(fs fsys.FS, cityRoot string, cfg *City) ([]string, error) {
	return applySiteBindings(fs, cityRoot, cfg, false)
}

// ApplySiteBindingsForEdit overlays .gc/site.toml for config-edit flows but
// retains raw city.toml paths as a fallback so edit commands can migrate them
// into .gc/site.toml on write.
func ApplySiteBindingsForEdit(fs fsys.FS, cityRoot string, cfg *City) ([]string, error) {
	return applySiteBindings(fs, cityRoot, cfg, true)
}

func applySiteBindings(fs fsys.FS, cityRoot string, cfg *City, keepLegacy bool) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	binding, err := LoadSiteBinding(fs, cityRoot)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string, len(binding.Rigs))
	for _, rig := range binding.Rigs {
		name := strings.TrimSpace(rig.Name)
		path := strings.TrimSpace(rig.Path)
		if name == "" || path == "" {
			continue
		}
		paths[name] = path
	}

	var warnings []string
	seen := make(map[string]struct{}, len(cfg.Rigs))
	for i := range cfg.Rigs {
		name := cfg.Rigs[i].Name
		seen[name] = struct{}{}
		legacyPath := strings.TrimSpace(cfg.Rigs[i].Path)
		if path, ok := paths[name]; ok {
			cfg.Rigs[i].Path = path
			continue
		}
		if keepLegacy || legacyPath != "" {
			cfg.Rigs[i].Path = legacyPath
			if legacyPath != "" && !keepLegacy {
				warnings = append(warnings, legacyRigPathSiteBindingWarning(name))
			}
			continue
		}
		cfg.Rigs[i].Path = ""
		if !keepLegacy {
			warnings = append(warnings, missingRigSiteBindingWarning(name))
		}
	}
	for name := range paths {
		if _, ok := seen[name]; ok {
			continue
		}
		warnings = append(warnings, unknownRigSiteBindingWarning(name))
	}
	sort.Strings(warnings)
	return warnings, nil
}

// PersistRigSiteBindings writes the current machine-local rig bindings to
// .gc/site.toml. Rigs without paths are left unbound and omitted.
func PersistRigSiteBindings(fs fsys.FS, cityRoot string, rigs []Rig) error {
	binding := SiteBinding{Rigs: make([]RigSiteBinding, 0, len(rigs))}
	for _, rig := range rigs {
		name := strings.TrimSpace(rig.Name)
		path := strings.TrimSpace(rig.Path)
		if name == "" || path == "" {
			continue
		}
		binding.Rigs = append(binding.Rigs, RigSiteBinding{Name: name, Path: path})
	}
	sort.Slice(binding.Rigs, func(i, j int) bool {
		return binding.Rigs[i].Name < binding.Rigs[j].Name
	})

	path := SiteBindingPath(cityRoot)
	if len(binding.Rigs) == 0 {
		if err := fs.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing site binding %q: %w", path, err)
		}
		return nil
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(binding); err != nil {
		return fmt.Errorf("marshaling site binding: %w", err)
	}
	if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating runtime dir %q: %w", filepath.Dir(path), err)
	}
	// Skip the write when on-disk content already matches. Keeps repeated
	// rig/suspend/resume/agent commands idempotent instead of churning
	// .gc/site.toml mtime (and breaking watcher debounce logic).
	if err := fsys.WriteFileIfChangedAtomic(fs, path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing site binding %q: %w", path, err)
	}
	return nil
}
