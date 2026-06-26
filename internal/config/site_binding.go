package config

import (
	"bytes"
	"errors"
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
// workspace identity and rig-path declarations. Schema-2 root-city compose
// paths now promote rig.path to a hard error, but callers that intentionally
// need advisory-only diagnostics can still use this helper.
func DetectLegacySiteBindingSurfaces(cfg *City, source string) []string {
	if cfg == nil {
		return nil
	}

	warnings := legacyWorkspaceIdentitySurfaceWarnings(cfg, source)
	warnings = append(warnings, legacyRigPathSurfaceWarnings(cfg, source)...)
	return warnings
}

func legacyWorkspaceIdentitySurfaceWarnings(cfg *City, source string) []string {
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

	return warnings
}

func legacyRigPathSurfaceWarnings(cfg *City, source string) []string {
	if cfg == nil {
		return nil
	}

	var warnings []string
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
func LegacySiteBindingSurfaceErrors(cfg *City, source string, data ...[]byte) []string {
	if cfg == nil {
		return nil
	}

	locator := optionalConfigDiagnosticLocator(data)
	var errors []string
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
			sourceWithDiagnosticLine(source, locator.lineForRigPath(rigName)),
			rigName,
		))
	}

	return errors
}

// LegacySiteBindingSurfaceError aggregates unsupported pre-1.0 site-binding
// surfaces into one load-time error for schema=2 enforcement paths.
func LegacySiteBindingSurfaceError(cfg *City, source string, data ...[]byte) error {
	violations := LegacySiteBindingSurfaceErrors(cfg, source, data...)
	return configSurfaceError("pre-1.0 site-binding fields are no longer supported", violations)
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
// .gc/site.toml. Rigs without paths are left unbound and omitted. Existing
// bindings for rig names not represented by the current city config are
// preserved so non-doctor edits do not silently delete orphan bindings.
func PersistRigSiteBindings(fs fsys.FS, cityRoot string, rigs []Rig) error {
	return persistRigSiteBindings(fs, cityRoot, rigs, nil)
}

func persistRigSiteBindings(fs fsys.FS, cityRoot string, rigs []Rig, removedRigNames map[string]struct{}) error {
	existing, err := LoadSiteBinding(fs, cityRoot)
	if err != nil {
		return err
	}
	declaredNames := make(map[string]struct{}, len(rigs))
	binding := SiteBinding{
		WorkspaceName:   strings.TrimSpace(existing.WorkspaceName),
		WorkspacePrefix: strings.TrimSpace(existing.WorkspacePrefix),
		Rigs:            make([]RigSiteBinding, 0, len(rigs)+len(existing.Rigs)),
	}
	for _, rig := range rigs {
		name := strings.TrimSpace(rig.Name)
		path := strings.TrimSpace(rig.Path)
		if name == "" {
			continue
		}
		declaredNames[name] = struct{}{}
		if path == "" {
			continue
		}
		binding.Rigs = append(binding.Rigs, RigSiteBinding{Name: name, Path: path})
	}
	for _, rig := range existing.Rigs {
		name := strings.TrimSpace(rig.Name)
		path := strings.TrimSpace(rig.Path)
		if name == "" || path == "" {
			continue
		}
		if _, removed := removedRigNames[name]; removed {
			continue
		}
		if _, ok := declaredNames[name]; ok {
			continue
		}
		binding.Rigs = append(binding.Rigs, RigSiteBinding{Name: name, Path: path})
	}
	sort.Slice(binding.Rigs, func(i, j int) bool {
		if binding.Rigs[i].Name != binding.Rigs[j].Name {
			return binding.Rigs[i].Name < binding.Rigs[j].Name
		}
		return binding.Rigs[i].Path < binding.Rigs[j].Path
	})

	return persistSiteBinding(fs, cityRoot, binding)
}

// WriteCityAndRigSiteBindingsForEdit writes the checked-in city.toml form and
// the matching machine-local rig bindings as a recoverable pair. If the site
// binding write fails after city.toml is changed, the previous city.toml and
// .gc/site.toml contents are restored before returning the error.
func WriteCityAndRigSiteBindingsForEdit(fs fsys.FS, tomlPath string, cfg *City) error {
	return writeCityAndRigSiteBindingsForEdit(fs, tomlPath, cfg, nil)
}

// AppendRigAndWriteSiteBindingsForEdit appends a new [[rigs]] block to
// city.toml without re-serializing the whole file, preserving all existing
// comments. cfg must include newRig (with its path) so that the site binding
// (.gc/site.toml) is kept consistent. If the site binding write fails after
// city.toml is changed, both files are restored before the error is returned.
func AppendRigAndWriteSiteBindingsForEdit(fs fsys.FS, tomlPath string, cfg *City, newRig Rig) error {
	// cityRoot intentionally stays the symlink's directory even when city.toml
	// links elsewhere: that is where the city's .gc/ state lives, so the site
	// binding (.gc/site.toml) is kept next to the live city, not the target.
	cityRoot := filepath.Dir(tomlPath)
	writePath, err := ResolveCityAppendPath(fs, tomlPath)
	if err != nil {
		return err
	}

	// Serialize only the new rig block, stripping path (it belongs in site.toml).
	rigForCity := newRig
	rigForCity.Path = ""
	type rigBlock struct {
		Rigs []Rig `toml:"rigs"`
	}
	var rigBuf bytes.Buffer
	enc := toml.NewEncoder(&rigBuf)
	enc.Indent = ""
	if err := enc.Encode(rigBlock{Rigs: []Rig{rigForCity}}); err != nil {
		return fmt.Errorf("serializing new rig block: %w", err)
	}

	existing, err := fs.ReadFile(writePath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", writePath, err)
	}

	snapshot, err := snapshotCityAndSiteFiles(fs, writePath, SiteBindingPath(cityRoot))
	if err != nil {
		return err
	}

	content := make([]byte, len(existing))
	copy(content, existing)
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content = append(content, '\n')
	}
	content = append(content, '\n')
	content = append(content, rigBuf.Bytes()...)

	if err := fsys.WriteFileIfChangedAtomic(fs, writePath, content, 0o644); err != nil {
		return err
	}
	if err := persistRigSiteBindings(fs, cityRoot, cfg.Rigs, nil); err != nil {
		if restoreErr := snapshot.restore(fs); restoreErr != nil {
			return fmt.Errorf("writing .gc/site.toml failed and restoring city.toml/site binding failed: %w", errors.Join(err, restoreErr))
		}
		return fmt.Errorf("writing .gc/site.toml failed; restored city.toml and previous site binding, fix the site binding write error and retry: %w", err)
	}
	return nil
}

// WriteCityAndRigSiteBindingsForEditRemovingRigs writes city.toml and
// .gc/site.toml while removing bindings for rig names that were intentionally
// deleted from the city config.
func WriteCityAndRigSiteBindingsForEditRemovingRigs(fs fsys.FS, tomlPath string, cfg *City, removedRigNames ...string) error {
	return writeCityAndRigSiteBindingsForEdit(fs, tomlPath, cfg, rigNameSet(removedRigNames))
}

func writeCityAndRigSiteBindingsForEdit(fs fsys.FS, tomlPath string, cfg *City, removedRigNames map[string]struct{}) error {
	cityRoot := filepath.Dir(tomlPath)
	content, err := cfg.MarshalForWrite()
	if err != nil {
		return err
	}
	// cityRoot intentionally stays the symlink's directory even when
	// city.toml links elsewhere: that is where the city's .gc/ state lives.
	writePath, err := ResolveCityRewritePath(fs, tomlPath)
	if err != nil {
		return err
	}
	// Snapshot the .gc/site.toml target persistSiteBinding actually writes,
	// not the link itself: resolve-only (no key-loss guard, the rollback
	// rewrites snapshotted bytes), mirroring the resolved forward write so a
	// post-city.toml binding failure restores the target instead of renaming
	// over (or removing) a symlinked site binding.
	sitePath := SiteBindingPath(cityRoot)
	resolvedSitePath, err := fsys.ResolveSymlinks(fs, sitePath)
	if err != nil {
		return fmt.Errorf("resolving site binding %q for rewrite: %w", sitePath, err)
	}
	snapshot, err := snapshotCityAndSiteFiles(fs, writePath, resolvedSitePath)
	if err != nil {
		return err
	}
	if err := fsys.WriteFileIfChangedAtomic(fs, writePath, content, 0o644); err != nil {
		return err
	}
	if err := persistRigSiteBindings(fs, cityRoot, cfg.Rigs, removedRigNames); err != nil {
		if restoreErr := snapshot.restore(fs); restoreErr != nil {
			return fmt.Errorf("writing .gc/site.toml failed and restoring city.toml/site binding failed: %w", errors.Join(err, restoreErr))
		}
		return fmt.Errorf("writing .gc/site.toml failed; restored city.toml and previous site binding, fix the site binding write error and retry: %w", err)
	}
	return nil
}

// ResolveCityRewritePath returns the path an edit-rewrite of city.toml must
// write to. city.toml may be a symlink (e.g., into a checked-out repo):
// renaming a temp file over the link would replace the link itself, so the
// rewrite must follow the chain and write at the final target instead. It
// also refuses the rewrite when the current on-disk content would lose keys
// in the round-trip (see GuardCityRewriteKeyLoss).
//
// Every code path that rewrites an existing city.toml from a re-marshaled
// struct must resolve through this helper. Two classes are exempt because
// only half the protection applies: lossless byte-preserving writers that
// follow the link anyway (an os.WriteFile append or rewrite keeps existing
// bytes and truncates through the link; switching one to a temp-file +
// rename write revokes its exemption), and resolve-only restore or repair
// paths that write previously-snapshotted or stripped bytes back (they
// resolve via fsys.ResolveSymlinks but must skip the key-loss guard, which
// would falsely refuse whenever unrelated unknown keys are present).
func ResolveCityRewritePath(fs fsys.FS, tomlPath string) (string, error) {
	writePath, err := fsys.ResolveSymlinks(fs, tomlPath)
	if err != nil {
		return "", fmt.Errorf("resolving %s for rewrite: %w", tomlPath, err)
	}
	if err := GuardCityRewriteKeyLoss(fs, writePath); err != nil {
		return "", err
	}
	return writePath, nil
}

// ResolveCityAppendPath returns the path a byte-preserving append to city.toml
// must write to. Like ResolveCityRewritePath it follows symlinks so the append
// targets the real file instead of replacing the link, and it refuses content
// that no longer parses as City TOML (the caller loaded the config from this
// same path, so unparsable content means it changed underneath us). Unlike the
// full-rewrite path it does NOT refuse unknown keys: an append preserves the
// existing bytes verbatim and cannot drop forward-compatible keys, so the
// version-skew case (an older gc editing a newer gc's city.toml) stays lossless.
func ResolveCityAppendPath(fs fsys.FS, tomlPath string) (string, error) {
	writePath, err := fsys.ResolveSymlinks(fs, tomlPath)
	if err != nil {
		return "", fmt.Errorf("resolving %s for append: %w", tomlPath, err)
	}
	if err := guardCityAppendCorruption(fs, writePath); err != nil {
		return "", err
	}
	return writePath, nil
}

// GuardCityRewriteKeyLoss refuses to rewrite a city.toml whose current
// on-disk content contains keys this gc binary does not recognize: the
// struct round-trip would silently drop them (ga-lurp5d dropped
// agent_defaults.provider and beads.bd_compatibility in production this
// way). A missing file is fine — there is nothing to lose. An unparsable
// file is also refused: callers load the config from this same path before
// mutating it, so unparsable content here means the file changed underneath
// us and a rewrite would destroy whatever is there now.
func GuardCityRewriteKeyLoss(fs fsys.FS, path string) error {
	return GuardRewriteKeyLoss[City](fs, path)
}

// GuardRewriteKeyLoss refuses to rewrite a TOML config whose current on-disk
// content contains keys that struct T does not recognize. T must be the struct
// the caller marshals back (or a faithful superset of it) so that every key T
// fails to decode is genuinely one the rewrite would silently drop. It backs
// GuardCityRewriteKeyLoss for city.toml and the equivalent pack.toml rewrite
// guards (gc agent suspend, import-manifest rewrites), all of which round-trip
// a config through a reduced struct and would otherwise drop newer or manual
// keys at a checked-in target. A missing file is fine — there is nothing to
// lose. An unparsable file is refused: callers load from this same path before
// mutating it, so unparsable content means the file changed underneath us and
// a rewrite would destroy whatever is there now.
func GuardRewriteKeyLoss[T any](fs fsys.FS, path string) error {
	data, err := fs.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading %s before rewrite: %w", path, err)
	}
	var cfg T
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return fmt.Errorf("refusing to rewrite %s: current content does not parse, rewriting would destroy it: %w", path, err)
	}
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	keys := make([]string, 0, len(undecoded))
	for _, key := range undecoded {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	return fmt.Errorf("refusing to rewrite %s: it contains keys this gc binary does not recognize and the rewrite would silently drop them: %s (upgrade gc or remove the keys, then retry)", path, strings.Join(keys, ", "))
}

// guardCityAppendCorruption refuses to append to a city.toml whose current
// on-disk content no longer parses as City TOML. A missing file is fine — the
// caller handles fresh writes. An unparsable file is refused for the same
// reason as the rewrite guard: callers load the config from this same path
// before mutating it, so unparsable content means the file changed underneath
// us and appending would compound the corruption. Unlike guardCityRewriteKeyLoss
// it does not refuse unknown keys: a byte-level append preserves the existing
// content verbatim and cannot drop them.
func guardCityAppendCorruption(fs fsys.FS, path string) error {
	_, _, err := decodeCityForGuard(fs, path)
	return err
}

// decodeCityForGuard reads and TOML-decodes the current on-disk city.toml for
// the rewrite/append guards. ok is false when the file is missing (nothing to
// lose). It returns an error when the file exists but no longer parses as City
// TOML, because the caller loaded the config from this same path before
// mutating it and any edit would destroy content that changed underneath us.
func decodeCityForGuard(fs fsys.FS, path string) (md toml.MetaData, ok bool, err error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return toml.MetaData{}, false, nil
		}
		return toml.MetaData{}, false, fmt.Errorf("reading %s before edit: %w", path, err)
	}
	var cfg City
	md, err = toml.Decode(string(data), &cfg)
	if err != nil {
		return toml.MetaData{}, false, fmt.Errorf("refusing to edit %s: current content does not parse, the edit would destroy it: %w", path, err)
	}
	return md, true, nil
}

func rigNameSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		set[name] = struct{}{}
	}
	return set
}

type configFileRestoreSnapshot struct {
	files map[string]configFileSnapshot
}

type configFileSnapshot struct {
	data    []byte
	mode    os.FileMode
	existed bool
}

func snapshotCityAndSiteFiles(fs fsys.FS, paths ...string) (*configFileRestoreSnapshot, error) {
	snapshot := &configFileRestoreSnapshot{files: make(map[string]configFileSnapshot, len(paths))}
	for _, path := range paths {
		fileSnapshot, err := snapshotConfigFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshot.files[path] = fileSnapshot
	}
	return snapshot, nil
}

func snapshotConfigFile(fs fsys.FS, path string) (configFileSnapshot, error) {
	data, err := fs.ReadFile(path)
	switch {
	case err == nil:
		mode := os.FileMode(0o644)
		if info, statErr := fs.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
		return configFileSnapshot{data: data, mode: mode, existed: true}, nil
	case os.IsNotExist(err):
		return configFileSnapshot{}, nil
	default:
		return configFileSnapshot{}, fmt.Errorf("snapshotting %s: %w", path, err)
	}
}

func (s *configFileRestoreSnapshot) restore(fs fsys.FS) error {
	var restoreErr error
	paths := make([]string, 0, len(s.files))
	for path := range s.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		file := s.files[path]
		if !file.existed {
			if err := fs.Remove(path); err != nil && !os.IsNotExist(err) {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("removing %s: %w", path, err))
			}
			continue
		}
		if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("creating %s: %w", filepath.Dir(path), err))
			continue
		}
		if err := fsys.WriteFileAtomic(fs, path, file.data, file.mode); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring %s: %w", path, err))
		}
	}
	return restoreErr
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
	writePath, err := fsys.ResolveSymlinks(fs, path)
	if err != nil {
		return fmt.Errorf("resolving site binding %q for rewrite: %w", path, err)
	}
	if len(binding.Rigs) == 0 && binding.WorkspaceName == "" && binding.WorkspacePrefix == "" {
		if err := fs.Remove(writePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing site binding %q: %w", writePath, err)
		}
		return nil
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(binding); err != nil {
		return fmt.Errorf("marshaling site binding: %w", err)
	}
	if err := fs.MkdirAll(filepath.Dir(writePath), 0o755); err != nil {
		return fmt.Errorf("creating runtime dir %q: %w", filepath.Dir(writePath), err)
	}
	// Skip the write when on-disk content already matches. Keeps repeated
	// rig/suspend/resume/agent commands idempotent instead of churning
	// .gc/site.toml mtime (and breaking watcher debounce logic).
	if err := fsys.WriteFileIfChangedAtomic(fs, writePath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing site binding %q: %w", writePath, err)
	}
	return nil
}
