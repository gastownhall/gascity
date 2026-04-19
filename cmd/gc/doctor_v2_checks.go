package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

func registerV2DeprecationChecks(d *doctor.Doctor) {
	d.Register(v2AgentFormatCheck{})
	d.Register(v2ImportFormatCheck{})
	d.Register(v2DefaultRigImportFormatCheck{})
	d.Register(v2RigPathSiteBindingCheck{})
	d.Register(v2ScriptsLayoutCheck{})
	d.Register(v2WorkspaceNameCheck{})
	d.Register(v2PromptTemplateSuffixCheck{})
}

type v2AgentFormatCheck struct{}

func (v2AgentFormatCheck) Name() string                     { return "v2-agent-format" }
func (v2AgentFormatCheck) CanFix() bool                     { return false }
func (v2AgentFormatCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2AgentFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	files := legacyAgentFiles(ctx.CityPath)
	if len(files) == 0 {
		return okCheck("v2-agent-format", "no legacy [[agent]] tables found")
	}
	return warnCheck("v2-agent-format",
		fmt.Sprintf("legacy [[agent]] tables found in %s", strings.Join(files, ", ")),
		v2MigrationHint(),
		files)
}

type v2ImportFormatCheck struct{}

func (v2ImportFormatCheck) Name() string                     { return "v2-import-format" }
func (v2ImportFormatCheck) CanFix() bool                     { return false }
func (v2ImportFormatCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2ImportFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cfg, ok := parseCityConfig(filepath.Join(ctx.CityPath, "city.toml"))
	if !ok || len(cfg.Workspace.Includes) == 0 {
		return okCheck("v2-import-format", "workspace.includes already migrated")
	}
	return warnCheck("v2-import-format",
		"workspace.includes is deprecated; migrate this city to [imports] before gc can load it from pack.toml and city.toml",
		v2MigrationHint(),
		cfg.Workspace.Includes)
}

type v2DefaultRigImportFormatCheck struct{}

func (v2DefaultRigImportFormatCheck) Name() string                     { return "v2-default-rig-import-format" }
func (v2DefaultRigImportFormatCheck) CanFix() bool                     { return false }
func (v2DefaultRigImportFormatCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2DefaultRigImportFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cfg, ok := parseCityConfig(filepath.Join(ctx.CityPath, "city.toml"))
	if !ok || len(cfg.Workspace.DefaultRigIncludes) == 0 {
		return okCheck("v2-default-rig-import-format", "workspace.default_rig_includes already migrated")
	}
	if len(cfg.Workspace.Includes) == 0 && len(legacyAgentFiles(ctx.CityPath)) == 0 {
		return okCheck("v2-default-rig-import-format", "workspace.default_rig_includes preserved for ordered rig defaults")
	}
	return warnCheck("v2-default-rig-import-format",
		"workspace.default_rig_includes is deprecated; migrate to [rig_defaults] imports = [...]",
		v2MigrationHint(),
		cfg.Workspace.DefaultRigIncludes)
}

type v2RigPathSiteBindingCheck struct{}

func (v2RigPathSiteBindingCheck) Name() string { return "v2-rig-path-site-binding" }

func (v2RigPathSiteBindingCheck) CanFix() bool { return true }

func (v2RigPathSiteBindingCheck) Fix(ctx *doctor.CheckContext) error {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(ctx.CityPath, "city.toml"))
	if err != nil {
		return err
	}
	// Snapshot the raw legacy paths before overlay so we can detect divergence
	// with the existing site binding. Without this, ApplySiteBindingsForEdit
	// would overwrite each legacy path with the site binding, silently discarding
	// the legacy value when they conflict.
	legacyByName := make(map[string]string, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		legacyByName[rig.Name] = strings.TrimSpace(rig.Path)
	}
	existing, err := config.LoadSiteBinding(fsys.OSFS{}, ctx.CityPath)
	if err != nil {
		return err
	}
	existingByName := make(map[string]string, len(existing.Rigs))
	for _, rig := range existing.Rigs {
		name := strings.TrimSpace(rig.Name)
		if name == "" {
			continue
		}
		existingByName[name] = strings.TrimSpace(rig.Path)
	}
	var conflicts []string
	for name, legacy := range legacyByName {
		site, ok := existingByName[name]
		if !ok || legacy == "" || site == "" {
			continue
		}
		// Normalize both sides so semantically equivalent path spellings
		// (relative vs absolute, redundant separators, trailing slashes,
		// symlinks, case-only differences on case-insensitive file
		// systems) don't trigger false conflicts that block migration.
		// The `resolveRigPaths` runtime follows the same convention:
		// relative paths are joined to the city root, then cleaned.
		if sameRigPath(ctx.CityPath, legacy, site) {
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("rig %q: city.toml=%q .gc/site.toml=%q", name, legacy, site))
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("refusing to migrate rig paths — city.toml and .gc/site.toml disagree; resolve manually and re-run `gc doctor --fix`:\n  %s",
			strings.Join(conflicts, "\n  "))
	}
	if _, err := config.ApplySiteBindingsForEdit(fsys.OSFS{}, ctx.CityPath, cfg); err != nil {
		return err
	}
	// Write city.toml FIRST, then .gc/site.toml. A crash between the two
	// writes leaves `.gc/site.toml` missing the new binding while city.toml
	// already has the path stripped — which the site-binding loader surfaces
	// via warnings (orphan legacy path) rather than silently resolving to an
	// empty effective path.
	content, err := cfg.MarshalForWrite()
	if err != nil {
		return err
	}
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	if err := fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, cityTomlPath, content, 0o644); err != nil {
		return err
	}
	if err := config.PersistRigSiteBindings(fsys.OSFS{}, ctx.CityPath, cfg.Rigs); err != nil {
		// Surface the half-migrated state explicitly: city.toml has
		// already been stripped of legacy paths but .gc/site.toml was
		// not updated, so declared rigs will load as unbound until the
		// user re-runs the migration.
		return fmt.Errorf("writing .gc/site.toml failed after city.toml was rewritten — rigs are now unbound; re-run `gc doctor --fix` to retry: %w", err)
	}
	return nil
}

// normalizeRigPath resolves a rig path for equality comparison: relative
// paths are joined to cityPath, then filepath.Clean is applied. This
// matches the runtime convention in config.resolveRigPaths.
func normalizeRigPath(cityPath, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cityPath, p)
	}
	return filepath.Clean(p)
}

// sameRigPath reports whether two rig paths refer to the same directory,
// accounting for the normalization differences that trip up naive string
// equality: relative vs absolute spellings, symlinks, and case-only
// differences on case-insensitive filesystems (macOS, Windows).
//
// When both normalized paths exist on disk, os.Stat + os.SameFile is the
// authoritative answer — it resolves symlinks and compares inodes, which
// covers every spelling difference including case on case-insensitive
// filesystems. Otherwise the function falls back to normalized string
// equality (which catches the common relative-vs-absolute case).
func sameRigPath(cityPath, a, b string) bool {
	na := normalizeRigPath(cityPath, a)
	nb := normalizeRigPath(cityPath, b)
	if na == nb {
		return true
	}
	aInfo, aErr := os.Stat(na)
	bInfo, bErr := os.Stat(nb)
	if aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo) {
		return true
	}
	return false
}

func (v2RigPathSiteBindingCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cfg, ok := parseCityConfig(filepath.Join(ctx.CityPath, "city.toml"))
	if !ok {
		return okCheck("v2-rig-path-site-binding", "rig path migration skipped until city.toml parses")
	}

	var legacy []string
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" {
			legacy = append(legacy, rig.Name)
		}
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, ctx.CityPath)
	if err != nil {
		return warnCheck("v2-rig-path-site-binding",
			fmt.Sprintf("failed to read .gc/site.toml: %v", err),
			"repair or remove the malformed .gc/site.toml file, then rerun gc doctor",
			nil)
	}
	declared := make(map[string]struct{}, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		declared[rig.Name] = struct{}{}
	}
	boundBySite := make(map[string]struct{}, len(binding.Rigs))
	var orphan []string
	for _, rig := range binding.Rigs {
		name := strings.TrimSpace(rig.Name)
		if name == "" {
			continue
		}
		if _, ok := declared[name]; ok {
			if strings.TrimSpace(rig.Path) != "" {
				boundBySite[name] = struct{}{}
			}
			continue
		}
		orphan = append(orphan, name)
	}
	// Unbound rigs are declared in city.toml but have no legacy path AND
	// no site binding — usually the aftermath of a half-migrated edit
	// (e.g., city.toml written, site.toml write failed). Surface this
	// state explicitly so operators don't silently run with rigs that
	// won't resolve to any store.
	var unbound []string
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" {
			continue
		}
		if _, ok := boundBySite[rig.Name]; ok {
			continue
		}
		unbound = append(unbound, rig.Name)
	}
	sort.Strings(legacy)
	sort.Strings(orphan)
	sort.Strings(unbound)

	// Multiple conditions may coexist (e.g., an unbound rig alongside an
	// orphan site binding whose name no longer matches). Combine all
	// non-empty categories into a single warning so operators see every
	// relevant name rather than just the first category the switch
	// would have picked.
	var messages []string
	var hints []string
	var details []string
	if len(legacy) > 0 {
		messages = append(messages, "rig paths still live in city.toml")
		hints = append(hints, "run `gc doctor --fix` to migrate rig paths into .gc/site.toml")
		details = append(details, legacy...)
	}
	if len(orphan) > 0 {
		messages = append(messages, ".gc/site.toml contains bindings for unknown rig names")
		hints = append(hints, "remove or rename the stale .gc/site.toml entries to match city.toml")
		details = append(details, orphan...)
	}
	if len(unbound) > 0 {
		messages = append(messages, "rigs are declared in city.toml but have no path binding in .gc/site.toml")
		hints = append(hints, "run `gc rig add <dir> --name <rig>` for each unbound rig, or restore the missing binding manually")
		details = append(details, unbound...)
	}
	if len(messages) == 0 {
		return okCheck("v2-rig-path-site-binding", "rig paths already managed in .gc/site.toml")
	}
	return warnCheck("v2-rig-path-site-binding",
		strings.Join(messages, "; "),
		strings.Join(hints, "; "),
		details)
}

type v2ScriptsLayoutCheck struct{}

func (v2ScriptsLayoutCheck) Name() string                     { return "v2-scripts-layout" }
func (v2ScriptsLayoutCheck) CanFix() bool                     { return false }
func (v2ScriptsLayoutCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2ScriptsLayoutCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	path := filepath.Join(ctx.CityPath, "scripts")
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return okCheck("v2-scripts-layout", "no top-level scripts/ directory found")
	}
	return warnCheck("v2-scripts-layout",
		"top-level scripts/ is deprecated; move scripts to commands/ or assets/",
		"move entrypoint scripts next to commands/doctor entries or under assets/",
		[]string{"scripts/"})
}

type v2WorkspaceNameCheck struct{}

func (v2WorkspaceNameCheck) Name() string                     { return "v2-workspace-name" }
func (v2WorkspaceNameCheck) CanFix() bool                     { return false }
func (v2WorkspaceNameCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2WorkspaceNameCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cfg, ok := parseCityConfig(filepath.Join(ctx.CityPath, "city.toml"))
	if !ok || strings.TrimSpace(cfg.Workspace.Name) == "" {
		return okCheck("v2-workspace-name", "workspace.name already absent")
	}
	return warnCheck("v2-workspace-name",
		"workspace.name will move to .gc/ in a future release",
		"review site-binding migration guidance before the hard cutover",
		[]string{cfg.Workspace.Name})
}

type v2PromptTemplateSuffixCheck struct{}

func (v2PromptTemplateSuffixCheck) Name() string                     { return "v2-prompt-template-suffix" }
func (v2PromptTemplateSuffixCheck) CanFix() bool                     { return false }
func (v2PromptTemplateSuffixCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2PromptTemplateSuffixCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	files := templatedMarkdownPrompts(ctx.CityPath)
	if len(files) == 0 {
		return okCheck("v2-prompt-template-suffix", "templated markdown prompts already use .template.md suffixes")
	}
	return warnCheck("v2-prompt-template-suffix",
		"templated markdown prompts should use .template.md",
		"rename each templated prompt file to *.template.md",
		files)
}

func okCheck(name, message string) *doctor.CheckResult {
	return &doctor.CheckResult{Name: name, Status: doctor.StatusOK, Message: message}
}

func warnCheck(name, message, hint string, details []string) *doctor.CheckResult {
	return &doctor.CheckResult{
		Name:    name,
		Status:  doctor.StatusWarning,
		Message: message,
		FixHint: hint,
		Details: details,
	}
}

func v2MigrationHint() string {
	return `run "gc doctor --fix" to rewrite safe mechanical cases, then rerun "gc doctor"`
}

func parseCityConfig(path string) (*config.City, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, false
	}
	return cfg, true
}

func legacyAgentFiles(cityPath string) []string {
	var files []string
	if cfg, ok := parseCityConfig(filepath.Join(cityPath, "city.toml")); ok && len(cfg.Agents) > 0 {
		files = append(files, "city.toml")
	}
	type rawPack struct {
		Agents []config.Agent `toml:"agent"`
	}
	packPath := filepath.Join(cityPath, "pack.toml")
	if data, err := os.ReadFile(packPath); err == nil {
		var pack rawPack
		if _, err := toml.Decode(string(data), &pack); err == nil && len(pack.Agents) > 0 {
			files = append(files, "pack.toml")
		}
	}
	return files
}

func templatedMarkdownPrompts(cityPath string) []string {
	candidates := make(map[string]bool)

	addPath := func(path string) {
		switch {
		case isCanonicalPromptTemplatePath(path):
			return
		case isLegacyPromptTemplatePath(path):
			candidates[path] = true
		case strings.HasSuffix(path, ".md"):
			candidates[path] = true
		}
	}

	if cfg, ok := parseCityConfig(filepath.Join(cityPath, "city.toml")); ok {
		for _, agent := range cfg.Agents {
			if agent.PromptTemplate != "" {
				addPath(resolvePromptPath(cityPath, agent.PromptTemplate))
			}
		}
	}

	type rawPack struct {
		Agents []config.Agent `toml:"agent"`
	}
	packPath := filepath.Join(cityPath, "pack.toml")
	if data, err := os.ReadFile(packPath); err == nil {
		var pack rawPack
		if _, err := toml.Decode(string(data), &pack); err == nil {
			for _, agent := range pack.Agents {
				if agent.PromptTemplate != "" {
					addPath(resolvePromptPath(cityPath, agent.PromptTemplate))
				}
			}
		}
	}

	for _, dir := range []string{filepath.Join(cityPath, "prompts"), filepath.Join(cityPath, "agents")} {
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if filepath.Base(path) == "prompt.md" ||
				filepath.Base(path) == "prompt.template.md" ||
				filepath.Base(path) == "prompt.md.tmpl" ||
				strings.HasPrefix(path, filepath.Join(cityPath, "prompts")+string(filepath.Separator)) {
				addPath(path)
			}
			return nil
		}); err != nil && !os.IsNotExist(err) {
			continue
		}
	}

	var files []string
	for path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "{{") {
			if rel, err := filepath.Rel(cityPath, path); err == nil {
				files = append(files, rel)
			} else {
				files = append(files, path)
			}
		}
	}
	sort.Strings(files)
	return files
}

func resolvePromptPath(cityPath, ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return filepath.Clean(filepath.Join(cityPath, ref))
}
