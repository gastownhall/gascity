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
	legacyWorkspaceIdentityWarningFragment  = "workspace identity fields are deprecated in v2; move them to .gc/site.toml"
	legacyRigPathSurfaceWarningFragment     = "rig.path is deprecated in v2; move it to .gc/site.toml"
)

// IsNonFatalSiteBindingWarning reports whether warning is migration guidance
// that should stay non-fatal in strict mode.
func IsNonFatalSiteBindingWarning(warning string) bool {
	return strings.Contains(warning, legacyRigPathSiteBindingWarningFragment) ||
		strings.Contains(warning, legacyWorkspaceIdentityWarningFragment) ||
		strings.Contains(warning, legacyRigPathSurfaceWarningFragment) ||
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

// DetectLegacySiteBindingSurfaces returns migration warnings for pre-1.0
// workspace identity and rig-path declarations that still appear in city
// fragments. Wave 2 defers hard-errors for fragments until the remediation
// story is real, but we still want the loader to surface what needs work.
func DetectLegacySiteBindingSurfaces(cfg *City, source string) []string {
	if cfg == nil {
		return nil
	}

	var warnings []string
	var workspaceFields []string
	if strings.TrimSpace(cfg.Workspace.Name) != "" {
		workspaceFields = append(workspaceFields, "workspace.name")
	}
	if strings.TrimSpace(cfg.Workspace.Prefix) != "" {
		workspaceFields = append(workspaceFields, "workspace.prefix")
	}
	if len(workspaceFields) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: %s (%s); move them to .gc/site.toml (run `gc doctor --fix` if this is the root city.toml; fragments must be updated by hand)",
			source,
			legacyWorkspaceIdentityWarningFragment,
			strings.Join(workspaceFields, ", "),
		))
	}

	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		rigName := strings.TrimSpace(rig.Name)
		if rigName == "" {
			rigName = "<unnamed>"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s: %s for rig %q; move it to .gc/site.toml (run `gc doctor --fix` if this is the root city.toml; otherwise add the binding manually and remove rig.path from the fragment)",
			source,
			legacyRigPathSurfaceWarningFragment,
			rigName,
		))
	}

	return warnings
}

// LegacySiteBindingSurfaceErrors returns hard-error diagnostics for pre-1.0
// workspace identity and rig-path declarations that should now live in
// .gc/site.toml instead of city config.
func LegacySiteBindingSurfaceErrors(cfg *City, source string) []string {
	if cfg == nil {
		return nil
	}

	var errors []string
	var workspaceFields []string
	if strings.TrimSpace(cfg.Workspace.Name) != "" {
		workspaceFields = append(workspaceFields, "workspace.name")
	}
	if strings.TrimSpace(cfg.Workspace.Prefix) != "" {
		workspaceFields = append(workspaceFields, "workspace.prefix")
	}
	if len(workspaceFields) > 0 {
		errors = append(errors, fmt.Sprintf(
			"%s: unsupported pre-1.0 workspace identity fields (%s); move them to .gc/site.toml (run `gc doctor --fix` if this is the root city.toml; fragments must be updated by hand)",
			source,
			strings.Join(workspaceFields, ", "),
		))
	}

	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		rigName := strings.TrimSpace(rig.Name)
		if rigName == "" {
			rigName = "<unnamed>"
		}
		errors = append(errors, fmt.Sprintf(
			"%s: unsupported pre-1.0 rig.path for rig %q; move it to .gc/site.toml (run `gc doctor --fix` if this is the root city.toml; otherwise add the binding manually and remove rig.path from the fragment)",
			source,
			rigName,
		))
	}

	return errors
}

// LegacySiteBindingSurfaceError aggregates unsupported pre-1.0 site-binding
// surfaces into one load-time error for schema=2 enforcement paths.
func LegacySiteBindingSurfaceError(cfg *City, source string) error {
	violations := LegacySiteBindingSurfaceErrors(cfg, source)
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("pre-1.0 site-binding fields are no longer supported:\n  - %s",
		strings.Join(violations, "\n  - "))
}

// SiteBindingPath returns the machine-local site binding file for a city.
func SiteBindingPath(cityRoot string) string {
	return filepath.Join(cityRoot, citylayout.RuntimeRoot, "site.toml")
}

// SiteBinding stores machine-local rig bindings for a city.
type SiteBinding struct {
	WorkspaceName   string           `toml:"workspace_name,omitempty"`
	WorkspacePrefix string           `toml:"workspace_prefix,omitempty"`
	Rigs            []RigSiteBinding `toml:"rig,omitempty"`
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
	applyWorkspaceIdentityBinding(cityRoot, binding, cfg)
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

// ResolveWorkspaceIdentity applies workspace identity from site binding when
// present, otherwise falls back to declared config and finally directory
// basename. Callers that need the effective city identity without mutating raw
// workspace fields should use this helper.
func ResolveWorkspaceIdentity(fs fsys.FS, cityRoot string, cfg *City) error {
	if cfg == nil {
		return nil
	}
	binding, err := LoadSiteBinding(fs, cityRoot)
	if err != nil {
		return err
	}
	applyWorkspaceIdentityBinding(cityRoot, binding, cfg)
	return nil
}

func applyWorkspaceIdentityBinding(cityRoot string, binding *SiteBinding, cfg *City) {
	if cfg == nil {
		return
	}
	name := strings.TrimSpace(filepath.Base(filepath.Clean(cityRoot)))
	if raw := strings.TrimSpace(cfg.Workspace.Name); raw != "" {
		name = raw
	}
	if binding != nil {
		if site := strings.TrimSpace(binding.WorkspaceName); site != "" {
			name = site
		}
	}
	cfg.ResolvedWorkspaceName = name

	prefix := strings.TrimSpace(cfg.Workspace.Prefix)
	if binding != nil {
		if site := strings.TrimSpace(binding.WorkspacePrefix); site != "" {
			prefix = site
		}
	}
	cfg.ResolvedWorkspacePrefix = prefix
}

// PersistRigSiteBindings writes the current machine-local rig bindings to
// .gc/site.toml. Rigs without paths are left unbound and omitted.
func PersistRigSiteBindings(fs fsys.FS, cityRoot string, rigs []Rig) error {
	existing, err := LoadSiteBinding(fs, cityRoot)
	if err != nil {
		return err
	}
	binding := SiteBinding{
		WorkspaceName:   strings.TrimSpace(existing.WorkspaceName),
		WorkspacePrefix: strings.TrimSpace(existing.WorkspacePrefix),
		Rigs:            make([]RigSiteBinding, 0, len(rigs)),
	}
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

	return persistSiteBinding(fs, cityRoot, binding)
}

// PersistWorkspaceSiteBinding writes machine-local workspace identity to
// .gc/site.toml while preserving any existing rig bindings.
func PersistWorkspaceSiteBinding(fs fsys.FS, cityRoot, name, prefix string) error {
	existing, err := LoadSiteBinding(fs, cityRoot)
	if err != nil {
		return err
	}
	binding := SiteBinding{
		WorkspaceName:   strings.TrimSpace(name),
		WorkspacePrefix: strings.TrimSpace(prefix),
		Rigs:            append([]RigSiteBinding(nil), existing.Rigs...),
	}
	return persistSiteBinding(fs, cityRoot, binding)
}

func persistSiteBinding(fs fsys.FS, cityRoot string, binding SiteBinding) error {
	path := SiteBindingPath(cityRoot)
	if len(binding.Rigs) == 0 && binding.WorkspaceName == "" && binding.WorkspacePrefix == "" {
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
