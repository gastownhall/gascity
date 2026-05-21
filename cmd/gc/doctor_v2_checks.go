package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/migrate"
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

func (v2AgentFormatCheck) Name() string { return "v2-agent-format" }
func (v2AgentFormatCheck) CanFix() bool { return true }
func (v2AgentFormatCheck) Fix(ctx *doctor.CheckContext) error {
	return runV2PackMigration(ctx, v2MigrationWarnSink(ctx))
}

func (v2AgentFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cityLegacy, packLegacy := legacyAgentFiles(ctx.CityPath)
	cityHasLegacy := len(cityLegacy) > 0
	packHasLegacy := len(packLegacy) > 0
	switch {
	case !cityHasLegacy && !packHasLegacy:
		return okCheck("v2-agent-format", "no legacy [[agent]] tables found")
	case cityHasLegacy && packHasLegacy:
		return errorCheck("v2-agent-format",
			"unsupported PackV1 [[agent]] tables found in city.toml; pack.toml also still uses deferred legacy [[agent]] tables",
			"move each city.toml [[agent]] definition into agents/<name>/agent.toml; pack.toml [[agent]] enforcement remains deferred until doctor/remediation support exists",
			append(cityLegacy, packLegacy...))
	case cityHasLegacy:
		return errorCheck("v2-agent-format",
			"unsupported PackV1 [[agent]] tables found in city.toml",
			"move each city.toml [[agent]] definition into agents/<name>/agent.toml; gc doctor does not rewrite inline agents automatically in this wave",
			cityLegacy)
	default:
		return warnCheck("v2-agent-format",
			"legacy [[agent]] tables still present in pack.toml; enforcement is deferred until doctor/remediation support exists",
			"leave this as-is for now or migrate to agents/<name>/agent.toml ahead of the follow-on enforcement pass",
			packLegacy)
	}
}

type v2ImportFormatCheck struct{}

func (v2ImportFormatCheck) Name() string { return "v2-import-format" }
func (v2ImportFormatCheck) CanFix() bool { return true }
func (v2ImportFormatCheck) Fix(ctx *doctor.CheckContext) error {
	return runV2PackMigration(ctx, v2MigrationWarnSink(ctx))
}

func (v2ImportFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	cfg, ok := parseCityConfig(cityTomlPath)
	if !ok || len(cfg.Workspace.LegacyIncludes()) == 0 {
		return okCheck("v2-import-format", "workspace.includes already migrated")
	}
	return errorCheck("v2-import-format",
		"unsupported PackV1 workspace.includes found; migrate this city to [imports] before gc can load it",
		"replace workspace.includes with [imports.<binding>] entries; gc doctor does not rewrite import bindings automatically in this wave",
		doctorKeyDetails(cityTomlPath, "workspace", "includes", "workspace.includes", cfg.Workspace.LegacyIncludes()))
}

type v2DefaultRigImportFormatCheck struct{}

func (v2DefaultRigImportFormatCheck) Name() string { return "v2-default-rig-import-format" }
func (v2DefaultRigImportFormatCheck) CanFix() bool { return true }
func (v2DefaultRigImportFormatCheck) Fix(ctx *doctor.CheckContext) error {
	return runV2PackMigration(ctx, v2MigrationWarnSink(ctx))
}

func (v2DefaultRigImportFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	cfg, ok := parseCityConfig(cityTomlPath)
	if !ok || len(cfg.Workspace.LegacyDefaultRigIncludes()) == 0 {
		return okCheck("v2-default-rig-import-format", "workspace.default_rig_includes already migrated")
	}
	return errorCheck("v2-default-rig-import-format",
		"unsupported PackV1 workspace.default_rig_includes found; migrate to root pack.toml [defaults.rig.imports.<binding>]",
		`move each entry into root pack.toml [defaults.rig.imports.<binding>]`,
		doctorKeyDetails(cityTomlPath, "workspace", "default_rig_includes", "workspace.default_rig_includes", cfg.Workspace.LegacyDefaultRigIncludes()))
}

type v2RigPathSiteBindingCheck struct{}

func (v2RigPathSiteBindingCheck) Name() string { return "v2-rig-path-site-binding" }

func (v2RigPathSiteBindingCheck) CanFix() bool { return true }

func (v2RigPathSiteBindingCheck) Fix(ctx *doctor.CheckContext) error {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(ctx.CityPath, "city.toml"))
	if err != nil {
		return err
	}
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
	content, err := cfg.MarshalForWrite()
	if err != nil {
		return err
	}
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	if err := fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, cityTomlPath, content, 0o644); err != nil {
		return err
	}
	if err := config.PersistRigSiteBindings(fsys.OSFS{}, ctx.CityPath, cfg.Rigs); err != nil {
		return fmt.Errorf("writing .gc/site.toml failed after city.toml was rewritten — rigs are now unbound; re-run `gc doctor --fix` to retry: %w", err)
	}
	return nil
}

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
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	cfg, ok := parseCityConfig(cityTomlPath)
	if !ok {
		return okCheck("v2-rig-path-site-binding", "rig path migration skipped until city.toml parses")
	}
	locator := newDoctorConfigLocator(cityTomlPath)

	var legacy []string
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" {
			legacy = append(legacy, doctorRigPathDetail(locator, rig.Name))
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
	if len(legacy) > 0 {
		return errorCheck("v2-rig-path-site-binding",
			strings.Join(messages, "; "),
			strings.Join(hints, "; "),
			details)
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
	realFiles, sawSymlink, walkErr := inspectTopLevelScripts(path)
	if walkErr != nil {
		return warnCheck("v2-scripts-layout",
			fmt.Sprintf("inspecting top-level scripts/: %v", walkErr),
			"resolve filesystem errors and rerun gc doctor",
			[]string{"scripts/"})
	}
	if len(realFiles) == 0 {
		if sawSymlink {
			legacyShim, provenanceErr := legacyTopLevelScriptsShim(ctx.CityPath)
			if provenanceErr != nil {
				return warnCheck("v2-scripts-layout",
					fmt.Sprintf("inspecting top-level scripts/ provenance: %v", provenanceErr),
					"fix the config load error or inspect scripts/ manually before rerunning gc doctor",
					[]string{"scripts/"})
			}
			if legacyShim {
				return warnCheck("v2-scripts-layout",
					"top-level scripts/ only contains stale legacy symlinks",
					"delete scripts/ or rerun gc start/gc supervisor so runtime pruning can remove the old shim",
					[]string{"scripts/"})
			}
			return warnCheck("v2-scripts-layout",
				"top-level scripts/ only contains user-managed symlinks; runtime pruning will not remove them",
				"move scripts to commands/ or assets/, or remove the user-managed symlinks manually",
				[]string{"scripts/"})
		}
		return okCheck("v2-scripts-layout", "no legacy top-level scripts found")
	}
	return warnCheck("v2-scripts-layout",
		"top-level scripts/ contains legacy real files; move scripts to commands/ or assets/",
		"move entrypoint scripts next to commands/doctor entries or under assets/",
		realFiles)
}

// inspectTopLevelScripts returns relative paths (under "scripts/") of real
// files plus whether the tree contains any symlinks. Symlinks are treated as
// stale compatibility artifacts from the removed ResolveScripts shim, while
// real files indicate the deprecated user-authored top-level scripts layout.
func inspectTopLevelScripts(dir string) ([]string, bool, error) {
	var realFiles []string
	var sawSymlink bool
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, lErr := os.Lstat(path)
		if lErr != nil {
			return lErr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			sawSymlink = true
			return nil
		}
		rel, rErr := filepath.Rel(dir, path)
		if rErr != nil {
			return rErr
		}
		realFiles = append(realFiles, filepath.Join("scripts", rel))
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	sort.Strings(realFiles)
	return realFiles, sawSymlink, nil
}

func legacyTopLevelScriptsShim(cityPath string) (bool, error) {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return false, err
	}
	origins := legacyScriptOriginsForScope(cityPath, cfg.PackDirs)
	_, ok, err := legacyShimLinks(cityPath, origins, cityPath)
	return ok, err
}

type v2WorkspaceNameCheck struct{}

func (v2WorkspaceNameCheck) Name() string { return "v2-workspace-name" }
func (v2WorkspaceNameCheck) CanFix() bool { return true }
func (v2WorkspaceNameCheck) Fix(ctx *doctor.CheckContext) error {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(ctx.CityPath, "city.toml"))
	if err != nil {
		return err
	}
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, ctx.CityPath)
	if err != nil {
		return err
	}

	rawName := strings.TrimSpace(cfg.Workspace.Name)
	rawPrefix := strings.TrimSpace(cfg.Workspace.Prefix)
	siteName := strings.TrimSpace(binding.WorkspaceName)
	sitePrefix := strings.TrimSpace(binding.WorkspacePrefix)

	var conflicts []string
	if rawName != "" && siteName != "" && rawName != siteName {
		conflicts = append(conflicts, fmt.Sprintf("workspace.name=%q .gc/site.toml workspace_name=%q", rawName, siteName))
	}
	if rawPrefix != "" && sitePrefix != "" && rawPrefix != sitePrefix {
		conflicts = append(conflicts, fmt.Sprintf("workspace.prefix=%q .gc/site.toml workspace_prefix=%q", rawPrefix, sitePrefix))
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("refusing to migrate workspace identity — city.toml and .gc/site.toml disagree; resolve manually and re-run `gc doctor --fix`:\n  %s",
			strings.Join(conflicts, "\n  "))
	}

	name := siteName
	if name == "" {
		name = rawName
	}
	prefix := sitePrefix
	if prefix == "" {
		prefix = rawPrefix
	}

	// Write the site binding first. If the city.toml rewrite fails
	// afterwards, runtime identity remains stable and `gc doctor` will
	// continue warning about the still-present legacy fields rather than
	// silently losing the chosen name/prefix.
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, ctx.CityPath, name, prefix); err != nil {
		return err
	}
	cfg.Workspace.Name = ""
	cfg.Workspace.Prefix = ""
	content, err := cfg.MarshalForWrite()
	if err != nil {
		return err
	}
	return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, filepath.Join(ctx.CityPath, "city.toml"), content, 0o644)
}

func (v2WorkspaceNameCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	cfg, ok := parseCityConfig(cityTomlPath)
	if !ok {
		return okCheck("v2-workspace-name", "workspace identity migration skipped until city.toml parses")
	}
	rawName := strings.TrimSpace(cfg.Workspace.Name)
	rawPrefix := strings.TrimSpace(cfg.Workspace.Prefix)
	if rawName == "" && rawPrefix == "" {
		return okCheck("v2-workspace-name", "workspace identity already absent from city.toml")
	}
	var details []string
	locator := newDoctorConfigLocator(cityTomlPath)
	if rawName != "" {
		details = append(details, doctorKeyDetail(locator, "workspace", "name", "workspace.name="+rawName))
	}
	if rawPrefix != "" {
		details = append(details, doctorKeyDetail(locator, "workspace", "prefix", "workspace.prefix="+rawPrefix))
	}
	return errorCheck("v2-workspace-name",
		"workspace identity still lives in city.toml",
		"run `gc doctor --fix` to migrate workspace.name/workspace.prefix into .gc/site.toml",
		details)
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

func errorCheck(name, message, hint string, details []string) *doctor.CheckResult {
	return &doctor.CheckResult{
		Name:    name,
		Status:  doctor.StatusError,
		Message: message,
		FixHint: hint,
		Details: details,
	}
}

// runV2PackMigration applies the pack-shape migration (legacy [[agent]]
// tables, workspace.includes, default_rig_includes) for a doctor --fix run.
// It is safe to call from multiple checks: migrate.Apply is idempotent on a
// city that has already been migrated (it returns an empty change set).
//
// migrate.Apply can return warnings about behavior-affecting fields it had
// to drop (e.g. legacy [[agent]] entries with fallback = true — the
// fallback field has no v2 counterpart and shadowing must be reviewed by
// hand). doctor --fix must not silently swallow those, otherwise the next
// gc doctor run reports a green check and the manual follow-up is lost
// forever. The warnings are emitted to warnSink so Doctor.Run callers see
// them in the same captured output stream as the check results.
func runV2PackMigration(ctx *doctor.CheckContext, warnSink io.Writer) error {
	report, err := migrate.Apply(ctx.CityPath, migrate.Options{})
	if err != nil {
		return err
	}
	if warnSink == nil {
		warnSink = io.Discard
	}
	for _, w := range report.Warnings {
		fmt.Fprintf(warnSink, "      gc doctor --fix: %s\n", w) //nolint:errcheck // best-effort diagnostic
	}
	return nil
}

func v2MigrationWarnSink(ctx *doctor.CheckContext) io.Writer {
	if ctx != nil && ctx.Output != nil {
		return ctx.Output
	}
	return defaultV2MigrationWarnSink
}

// defaultV2MigrationWarnSink is the production warning sink for
// direct Fix calls outside Doctor.Run. Doctor.Run sets CheckContext.Output,
// and production doctor commands normally use that writer instead.
var defaultV2MigrationWarnSink io.Writer = os.Stderr

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

type doctorConfigLocator struct {
	path  string
	lines []string
}

func newDoctorConfigLocator(path string) doctorConfigLocator {
	data, err := os.ReadFile(path)
	if err != nil {
		return doctorConfigLocator{path: path}
	}
	return doctorConfigLocator{path: path, lines: strings.Split(string(data), "\n")}
}

func (l doctorConfigLocator) source(line int) string {
	source := filepath.Base(l.path)
	if line <= 0 {
		return source
	}
	return fmt.Sprintf("%s:%d", source, line)
}

func (l doctorConfigLocator) lineForTable(table string) int {
	for i, line := range l.lines {
		name, ok := parseDoctorTOMLTableHeader(line)
		if ok && name == table {
			return i + 1
		}
	}
	return 0
}

func (l doctorConfigLocator) lineForKey(table, key string) int {
	var currentTable string
	for i, line := range l.lines {
		trimmed := trimDoctorTOMLLine(line)
		if trimmed == "" {
			continue
		}
		if name, ok := parseDoctorTOMLTableHeader(trimmed); ok {
			currentTable = name
			continue
		}
		if currentTable != table {
			continue
		}
		gotKey, _, ok := strings.Cut(trimmed, "=")
		if ok && strings.TrimSpace(gotKey) == key {
			return i + 1
		}
	}
	return 0
}

func (l doctorConfigLocator) lineForRigPath(rigName string) int {
	var inRig bool
	var currentRigName string
	var currentPathLine int

	flushRig := func() int {
		if !inRig || currentPathLine == 0 {
			return 0
		}
		if rigName == "" || currentRigName == rigName {
			return currentPathLine
		}
		return 0
	}

	for i, line := range l.lines {
		trimmed := trimDoctorTOMLLine(line)
		if trimmed == "" {
			continue
		}
		if name, ok := parseDoctorTOMLTableHeader(trimmed); ok {
			if line := flushRig(); line > 0 {
				return line
			}
			inRig = name == "rigs"
			currentRigName = ""
			currentPathLine = 0
			continue
		}
		if !inRig {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			currentRigName = unquoteDoctorTOMLValue(value)
		case "path":
			currentPathLine = i + 1
		}
	}
	return flushRig()
}

func doctorTableDetail(locator doctorConfigLocator, table, label string) string {
	return fmt.Sprintf("%s: %s", locator.source(locator.lineForTable(table)), label)
}

func doctorKeyDetail(locator doctorConfigLocator, table, key, detail string) string {
	return fmt.Sprintf("%s: %s", locator.source(locator.lineForKey(table, key)), detail)
}

func doctorKeyDetails(path, table, key, label string, values []string) []string {
	locator := newDoctorConfigLocator(path)
	if len(values) == 0 {
		return []string{doctorKeyDetail(locator, table, key, label)}
	}
	details := make([]string, 0, len(values))
	for _, value := range values {
		details = append(details, doctorKeyDetail(locator, table, key, fmt.Sprintf("%s includes %q", label, value)))
	}
	return details
}

func doctorRigPathDetail(locator doctorConfigLocator, rigName string) string {
	if strings.TrimSpace(rigName) == "" {
		rigName = "<unnamed>"
	}
	return fmt.Sprintf("%s: rig %q path", locator.source(locator.lineForRigPath(rigName)), rigName)
}

func parseDoctorTOMLTableHeader(line string) (string, bool) {
	trimmed := trimDoctorTOMLLine(line)
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "[["), "]]")), true
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")), true
	}
	return "", false
}

func trimDoctorTOMLLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	if before, _, ok := strings.Cut(trimmed, "#"); ok {
		trimmed = strings.TrimSpace(before)
	}
	return trimmed
}

func unquoteDoctorTOMLValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}

func legacyAgentFiles(cityPath string) (cityLegacy, packLegacy []string) {
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	if cfg, ok := parseCityConfig(cityTomlPath); ok && len(cfg.Agents) > 0 {
		cityLegacy = append(cityLegacy, doctorTableDetail(newDoctorConfigLocator(cityTomlPath), "agent", "city.toml [[agent]]"))
	}
	type rawPack struct {
		Agents []config.Agent `toml:"agent"`
	}
	packPath := filepath.Join(cityPath, "pack.toml")
	if data, err := os.ReadFile(packPath); err == nil {
		var pack rawPack
		if _, err := toml.Decode(string(data), &pack); err == nil && len(pack.Agents) > 0 {
			packLegacy = append(packLegacy, doctorTableDetail(newDoctorConfigLocator(packPath), "agent", "pack.toml [[agent]]"))
		}
	}
	return cityLegacy, packLegacy
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
