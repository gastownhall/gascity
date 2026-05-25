package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/migrate"
)

func registerV2DeprecationChecks(d *doctor.Doctor) {
	for _, c := range v2DeprecationChecks() {
		d.Register(c)
	}
}

func v2DeprecationChecks() []doctor.Check {
	return []doctor.Check{
		v2FormulasDirCheck{},
		v2AgentFormatCheck{},
		v2ImportFormatCheck{},
		v2DefaultRigImportFormatCheck{},
		v2PackSourcesCheck{},
		v2RigPathSiteBindingCheck{},
		v2LegacyOrderLayoutCheck{},
		v2ScriptsLayoutCheck{},
		v2WorkspaceNameCheck{},
		v2PromptTemplateSuffixCheck{},
	}
}

type v2FormulasDirCheck struct{}

type formulasDirHit struct {
	Path       string
	Line       int
	Value      string
	Fixable    bool
	WithinCity bool
}

func (v2FormulasDirCheck) Name() string { return "v2-formulas-dir" }

func (v2FormulasDirCheck) CanFix() bool { return true }

func (v2FormulasDirCheck) Fix(ctx *doctor.CheckContext) error {
	hits, err := scanFormulasDirDeclarations(ctx.CityPath)
	if err != nil {
		return err
	}
	for _, hit := range hits {
		if !hit.Fixable || !hit.WithinCity {
			continue
		}
		if err := rewriteWithoutDefaultFormulasDir(hit.Path); err != nil {
			return fmt.Errorf("rewriting %s: %w", hit.Path, err)
		}
	}
	return nil
}

func (v2FormulasDirCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	hits, err := scanFormulasDirDeclarations(ctx.CityPath)
	if err != nil {
		return errorCheck("v2-formulas-dir",
			fmt.Sprintf("inspecting [formulas].dir declarations: %v", err),
			"fix the reported config file and rerun gc doctor",
			nil)
	}
	if len(hits) == 0 {
		return okCheck("v2-formulas-dir", "no deprecated [formulas].dir declarations found")
	}

	fixable := 0
	details := make([]string, 0, len(hits))
	for _, hit := range hits {
		action := "remove manually or migrate formulas into the well-known formulas/ directory"
		if hit.Fixable && hit.WithinCity {
			action = "gc doctor --fix can remove this redundant default"
			fixable++
		} else if hit.Fixable {
			action = "manual cleanup required; gc doctor --fix only changes files under the city"
		}
		details = append(details, fmt.Sprintf("%s: [formulas].dir=%q (%s)",
			doctorFormulasDirSource(hit.Path, hit.Line), hit.Value, action))
	}
	sort.Strings(details)

	hint := "remove custom [formulas].dir declarations or migrate formulas into the well-known formulas/ directory"
	if fixable == len(hits) {
		hint = "run `gc doctor --fix` to remove redundant [formulas].dir declarations"
	} else if fixable > 0 {
		hint = "run `gc doctor --fix` for redundant local defaults, then remove or migrate the remaining custom declarations manually"
	}
	return errorCheck("v2-formulas-dir",
		fmt.Sprintf("unsupported [formulas].dir declarations found in %d file(s)", len(hits)),
		hint,
		details)
}

func scanFormulasDirDeclarations(cityPath string) ([]formulasDirHit, error) {
	paths := formulasDirConfigPaths(cityPath)
	hits := make([]formulasDirHit, 0, len(paths))
	for _, path := range paths {
		hit, ok, err := readFormulasDirDeclaration(cityPath, path)
		if err != nil {
			return nil, err
		}
		if ok {
			hits = append(hits, hit)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Path == hits[j].Path {
			return hits[i].Line < hits[j].Line
		}
		return hits[i].Path < hits[j].Path
	})
	return hits, nil
}

func formulasDirConfigPaths(cityPath string) []string {
	seen := make(map[string]bool)
	var paths []string
	addPath := func(path string) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}

	addPath(filepath.Join(cityPath, "city.toml"))

	for _, ref := range cityConfigPackRefs(cityPath) {
		if dir, ok := localDoctorPackPath(cityPath, ref); ok {
			collectFormulasDirPackPaths(dir, seen, &paths)
		}
	}
	collectFormulasDirPackPaths(cityPath, seen, &paths)
	sort.Strings(paths)
	return paths
}

func cityConfigPackRefs(cityPath string) []string {
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil
	}
	var refs []string
	refs = append(refs, legacyPackSources(cfg.Workspace.LegacyIncludes(), cfg.Packs)...)
	refs = append(refs, legacyPackSources(cfg.Workspace.LegacyDefaultRigIncludes(), cfg.Packs)...)
	refs = append(refs, sortedDoctorImportSources(cfg.Imports)...)
	refs = append(refs, sortedDoctorImportSources(cfg.Defaults.Rig.Imports)...)
	for _, rig := range cfg.Rigs {
		refs = append(refs, legacyPackSources(rig.Includes, cfg.Packs)...)
		refs = append(refs, sortedDoctorImportSources(rig.Imports)...)
	}
	return refs
}

func legacyPackSources(refs []string, packs map[string]config.PackSource) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		source := ref
		if pack, ok := packs[ref]; ok && strings.TrimSpace(pack.Source) != "" {
			source = pack.Source
			if strings.TrimSpace(pack.Path) != "" && !doctorPackRefIsRemote(source) {
				source = filepath.Join(source, pack.Path)
			}
		}
		out = append(out, source)
	}
	return out
}

func doctorPackRefIsRemote(ref string) bool {
	ref = strings.TrimSpace(ref)
	return strings.Contains(ref, "://") || strings.HasPrefix(ref, "github.com/") || strings.HasPrefix(ref, "git@")
}

func collectFormulasDirPackPaths(packDir string, seen map[string]bool, paths *[]string) {
	packPath := filepath.Join(packDir, "pack.toml")
	packPath = filepath.Clean(packPath)
	if seen[packPath] {
		return
	}
	seen[packPath] = true
	*paths = append(*paths, packPath)

	cfg, ok := parseDoctorPackRefs(packPath)
	if !ok {
		return
	}
	for _, ref := range doctorPackRefSources(cfg, true) {
		nextDir, ok := localDoctorPackPathFromBase(packDir, ref)
		if !ok {
			continue
		}
		collectFormulasDirPackPaths(nextDir, seen, paths)
	}
}

type formulasDirFile struct {
	Formulas config.FormulasConfig `toml:"formulas"`
}

func readFormulasDirDeclaration(cityPath, path string) (formulasDirHit, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return formulasDirHit{}, false, nil
		}
		return formulasDirHit{}, false, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return formulasDirHit{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return formulasDirHit{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg formulasDirFile
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return formulasDirHit{}, false, nil
	}
	if !md.IsDefined("formulas", "dir") {
		return formulasDirHit{}, false, nil
	}
	value := strings.TrimSpace(cfg.Formulas.Dir)
	locator := config.NewDiagnosticLocator(data)
	line := locator.LineForKey("formulas", "dir")
	if line == 0 {
		line = locator.LineForTable("formulas")
	}
	return formulasDirHit{
		Path:       path,
		Line:       line,
		Value:      value,
		Fixable:    isDefaultFormulasDir(value),
		WithinCity: doctorPathWithinCity(cityPath, path),
	}, true, nil
}

func isDefaultFormulasDir(value string) bool {
	switch filepath.Clean(strings.TrimSpace(value)) {
	case ".", citylayout.FormulasRoot:
		return true
	default:
		return false
	}
}

func rewriteWithoutDefaultFormulasDir(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg formulasDirFile
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return err
	}
	if !md.IsDefined("formulas", "dir") || !isDefaultFormulasDir(cfg.Formulas.Dir) {
		return nil
	}
	rendered, changed := stripDefaultFormulasDirDeclaration(string(data))
	if !changed {
		return fmt.Errorf("could not locate [formulas].dir assignment")
	}
	return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, path, []byte(rendered), 0o644)
}

func stripDefaultFormulasDirDeclaration(source string) (string, bool) {
	hadTrailingNewline := strings.HasSuffix(source, "\n")
	lines := strings.Split(source, "\n")
	if hadTrailingNewline {
		lines = lines[:len(lines)-1]
	}

	remove := make(map[int]bool)
	currentTable := ""
	formulasTableLine := -1
	formulasDirLine := -1
	formulasHasOtherContent := false
	flushFormulas := func() {
		if formulasDirLine < 0 {
			return
		}
		remove[formulasDirLine] = true
		if !formulasHasOtherContent && formulasTableLine >= 0 {
			remove[formulasTableLine] = true
		}
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(stripTOMLInlineComment(line))
		if trimmed == "" {
			continue
		}
		if table, ok := parseDoctorTOMLTableHeader(trimmed); ok {
			if currentTable == "formulas" {
				flushFormulas()
			}
			currentTable = table
			if table == "formulas" {
				formulasTableLine = i
				formulasDirLine = -1
				formulasHasOtherContent = false
			}
			continue
		}
		key, _, ok := parseDoctorTOMLKeyValue(trimmed)
		if !ok {
			continue
		}
		switch {
		case currentTable == "formulas" && key == "dir":
			formulasDirLine = i
		case currentTable == "formulas":
			formulasHasOtherContent = true
		case key == "formulas.dir":
			remove[i] = true
		}
	}
	if currentTable == "formulas" {
		flushFormulas()
	}
	if len(remove) == 0 {
		return source, false
	}

	out := make([]string, 0, len(lines)-len(remove))
	for i, line := range lines {
		if remove[i] {
			continue
		}
		out = append(out, line)
	}
	rendered := strings.Join(out, "\n")
	if hadTrailingNewline {
		rendered += "\n"
	}
	return rendered, true
}

func parseDoctorTOMLTableHeader(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "[["), "]]")), true
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")), true
	}
	return "", false
}

func parseDoctorTOMLKeyValue(line string) (string, string, bool) {
	before, after, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(before), strings.TrimSpace(after), true
}

func doctorFormulasDirSource(path string, line int) string {
	if line <= 0 {
		return path
	}
	return fmt.Sprintf("%s:%d", path, line)
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
			"run `gc doctor --fix` to move each city.toml [[agent]] definition into agents/<name>/agent.toml; pack.toml [[agent]] enforcement remains deferred until doctor/remediation support exists",
			append(cityLegacy, packLegacy...))
	case cityHasLegacy:
		return errorCheck("v2-agent-format",
			"unsupported PackV1 [[agent]] tables found in city.toml",
			"run `gc doctor --fix` to move each city.toml [[agent]] definition into agents/<name>/agent.toml",
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
		"run `gc doctor --fix` to replace workspace.includes with [imports.<binding>] entries",
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
		"unsupported PackV1 workspace.default_rig_includes found; migrate to city.toml [defaults.rig.imports.<binding>]",
		`move each entry into city.toml [defaults.rig.imports.<binding>]`,
		doctorKeyDetails(cityTomlPath, "workspace", "default_rig_includes", "workspace.default_rig_includes", cfg.Workspace.LegacyDefaultRigIncludes()))
}

type v2PackSourcesCheck struct{}

func (v2PackSourcesCheck) Name() string { return "v2-pack-sources" }
func (v2PackSourcesCheck) CanFix() bool { return true }
func (v2PackSourcesCheck) Fix(ctx *doctor.CheckContext) error {
	return runV2PackMigration(ctx, v2MigrationWarnSink(ctx))
}

func (v2PackSourcesCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	cfg, ok := parseCityConfig(cityTomlPath)
	if !ok || len(cfg.Packs) == 0 {
		return okCheck("v2-pack-sources", "root [packs] entries already absent")
	}
	return errorCheck("v2-pack-sources",
		"unsupported PackV1 [packs] entries found in city.toml",
		"run `gc doctor --fix` to migrate entries referenced by workspace include lists; remove or rewrite any remaining [packs] entries manually as [imports]",
		doctorPackSourceDetails(cityTomlPath, cfg))
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
		name := strings.TrimSpace(rig.Name)
		if name == "" {
			continue
		}
		legacyByName[name] = strings.TrimSpace(rig.Path)
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
	var orphans []string
	for name, site := range existingByName {
		if _, ok := legacyByName[name]; ok {
			continue
		}
		orphans = append(orphans, fmt.Sprintf("rig %q: .gc/site.toml=%q", name, site))
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return fmt.Errorf("refusing to migrate rig paths because .gc/site.toml contains bindings for unknown rig names; remove or rename the stale entries and re-run `gc doctor --fix`:\n  %s",
			strings.Join(orphans, "\n  "))
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
	cityTomlPath := filepath.Join(ctx.CityPath, "city.toml")
	if err := config.WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityTomlPath, cfg); err != nil {
		return err
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

type v2LegacyOrderLayoutCheck struct{}

func (v2LegacyOrderLayoutCheck) Name() string { return "v2-legacy-order-layout" }

func (v2LegacyOrderLayoutCheck) CanFix() bool { return true }

func (v2LegacyOrderLayoutCheck) WarmupEligible() bool { return false }

func (v2LegacyOrderLayoutCheck) Fix(ctx *doctor.CheckContext) error {
	if ctx == nil || strings.TrimSpace(ctx.CityPath) == "" {
		return fmt.Errorf("city path is required")
	}
	return fixLegacyOrderLayouts(ctx.CityPath)
}

func (v2LegacyOrderLayoutCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	details := legacyOrderLayoutDetails(ctx.CityPath)
	if len(details) == 0 {
		return okCheck("v2-legacy-order-layout", "no PackV1 order subdirectory layouts found")
	}
	return errorCheck("v2-legacy-order-layout",
		"unsupported PackV1 order subdirectory layouts found",
		"run `gc doctor --fix` to migrate collision-free legacy order layouts, or rename each orders/<name>/order.toml or formulas/orders/<name>/order.toml file to the flat orders/<name>.toml layout manually",
		details)
}

type legacyOrderRoot struct {
	dir       string
	targetDir string
	hint      string
	fixable   bool
}

func legacyOrderLayoutDetails(cityPath string) []string {
	roots := legacyOrderLayoutRoots(cityPath)
	var details []string
	seen := make(map[string]bool)
	for _, root := range roots {
		for _, detail := range scanLegacyOrderRoot(root) {
			if seen[detail] {
				continue
			}
			seen[detail] = true
			details = append(details, detail)
		}
	}
	sort.Strings(details)
	return details
}

func legacyOrderLayoutRoots(cityPath string) []legacyOrderRoot {
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	cfg, cfgOK := parseCityConfig(cityTomlPath)
	formulasDir := ""
	if cfgOK {
		formulasDir = cfg.FormulasDir()
	}
	orderDir := citylayout.OrdersPath(cityPath)
	formulaOrderDir := filepath.Join(citylayout.ResolveFormulasDir(cityPath, formulasDir), "orders")
	formulaOrderDirFixable := doctorPathWithinCity(cityPath, formulaOrderDir) && doctorPathWithinCity(cityPath, orderDir)
	roots := []legacyOrderRoot{
		{dir: orderDir, targetDir: orderDir, hint: "rename to orders/%s.toml", fixable: doctorPathWithinCity(cityPath, orderDir)},
		{dir: formulaOrderDir, targetDir: orderDir, hint: legacyOrderRootHint(formulaOrderDirFixable, "move"), fixable: formulaOrderDirFixable},
	}
	if packDirs, err := config.LoadPackGraphDirsForDoctor(fsys.OSFS{}, cityTomlPath); err == nil {
		return appendLegacyOrderRootsForPackDirs(roots, cityPath, packDirs)
	}

	seenPacks := map[string]bool{absDoctorPathKey(cityPath): true}
	if cfgOK {
		for _, ref := range cfg.Workspace.LegacyIncludes() {
			if packDir, ok := localDoctorPackPath(cityPath, ref); ok {
				roots = appendLegacyOrderRootsForPackGraph(roots, cityPath, packDir, seenPacks)
			}
		}
		for _, imp := range cfg.Imports {
			if packDir, ok := localDoctorPackPath(cityPath, imp.Source); ok {
				roots = appendLegacyOrderRootsForPackGraph(roots, cityPath, packDir, seenPacks)
			}
		}
		for _, rig := range cfg.Rigs {
			for _, ref := range rig.Includes {
				if packDir, ok := localDoctorPackPath(cityPath, ref); ok {
					roots = appendLegacyOrderRootsForPackGraph(roots, cityPath, packDir, seenPacks)
				}
			}
			for _, source := range sortedDoctorImportSources(rig.Imports) {
				if packDir, ok := localDoctorPackPath(cityPath, source); ok {
					roots = appendLegacyOrderRootsForPackGraph(roots, cityPath, packDir, seenPacks)
				}
			}
		}
	}
	return appendLegacyOrderRootsForRootPackRefs(roots, cityPath, seenPacks)
}

func appendLegacyOrderRootsForPackDirs(roots []legacyOrderRoot, cityPath string, packDirs []string) []legacyOrderRoot {
	seenPacks := map[string]bool{absDoctorPathKey(cityPath): true}
	for _, packDir := range packDirs {
		key := absDoctorPathKey(packDir)
		if seenPacks[key] {
			continue
		}
		seenPacks[key] = true
		roots = append(roots, legacyOrderRootsForPack(cityPath, packDir)...)
	}
	return roots
}

func legacyOrderRootsForPack(cityPath, packDir string) []legacyOrderRoot {
	orderDir := filepath.Join(packDir, "orders")
	formulaOrderDir := filepath.Join(packDir, "formulas", "orders")
	orderDirFixable := doctorPathWithinCity(cityPath, orderDir)
	formulaOrderDirFixable := doctorPathWithinCity(cityPath, formulaOrderDir) && orderDirFixable
	return []legacyOrderRoot{
		{dir: orderDir, targetDir: orderDir, hint: legacyOrderRootHint(orderDirFixable, "rename"), fixable: orderDirFixable},
		{dir: formulaOrderDir, targetDir: orderDir, hint: legacyOrderRootHint(formulaOrderDirFixable, "move"), fixable: formulaOrderDirFixable},
	}
}

func legacyOrderRootHint(fixable bool, action string) string {
	if fixable {
		return action + " to orders/%s.toml"
	}
	return "manually " + action + " to orders/%s.toml; gc doctor --fix only changes files under the city"
}

func localDoctorPackPath(cityPath, ref string) (string, bool) {
	return localDoctorPackPathFromBase(cityPath, ref)
}

func localDoctorPackPathFromBase(baseDir, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "://") || strings.HasPrefix(ref, "github.com/") || strings.HasPrefix(ref, "git@") {
		return "", false
	}
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref), true
	}
	return filepath.Clean(filepath.Join(baseDir, ref)), true
}

type doctorPackRefsConfig struct {
	Pack struct {
		Includes []string `toml:"includes"`
	} `toml:"pack"`
	Imports  map[string]config.Import `toml:"imports"`
	Defaults config.PackDefaults      `toml:"defaults"`
}

func appendLegacyOrderRootsForRootPackRefs(roots []legacyOrderRoot, cityPath string, seenPacks map[string]bool) []legacyOrderRoot {
	return appendLegacyOrderRootsForPackRefs(roots, cityPath, cityPath, true, seenPacks)
}

func appendLegacyOrderRootsForPackGraph(roots []legacyOrderRoot, cityPath, packDir string, seenPacks map[string]bool) []legacyOrderRoot {
	key := absDoctorPathKey(packDir)
	if seenPacks[key] {
		return roots
	}
	seenPacks[key] = true
	roots = append(roots, legacyOrderRootsForPack(cityPath, packDir)...)
	return appendLegacyOrderRootsForPackRefs(roots, cityPath, packDir, false, seenPacks)
}

func appendLegacyOrderRootsForPackRefs(roots []legacyOrderRoot, cityPath, packDir string, includeDefaultRigImports bool, seenPacks map[string]bool) []legacyOrderRoot {
	cfg, ok := parseDoctorPackRefs(filepath.Join(packDir, "pack.toml"))
	if !ok {
		return roots
	}
	for _, ref := range doctorPackRefSources(cfg, includeDefaultRigImports) {
		nextDir, ok := localDoctorPackPathFromBase(packDir, ref)
		if !ok {
			continue
		}
		roots = appendLegacyOrderRootsForPackGraph(roots, cityPath, nextDir, seenPacks)
	}
	return roots
}

func parseDoctorPackRefs(path string) (*doctorPackRefsConfig, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cfg doctorPackRefsConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, false
	}
	return &cfg, true
}

func doctorPackRefSources(cfg *doctorPackRefsConfig, includeDefaultRigImports bool) []string {
	var refs []string
	refs = append(refs, cfg.Pack.Includes...)
	refs = append(refs, sortedDoctorImportSources(cfg.Imports)...)
	if includeDefaultRigImports {
		refs = append(refs, sortedDoctorImportSources(cfg.Defaults.Rig.Imports)...)
	}
	return refs
}

func sortedDoctorImportSources(imports map[string]config.Import) []string {
	if len(imports) == 0 {
		return nil
	}
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	sources := make([]string, 0, len(names))
	for _, name := range names {
		sources = append(sources, imports[name].Source)
	}
	return sources
}

func absDoctorPathKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func doctorPathWithinCity(cityPath, path string) bool {
	cityAbs := absDoctorPathKey(cityPath)
	pathAbs := absDoctorPathKey(path)
	if !cleanedPathWithin(cityAbs, pathAbs) {
		return false
	}
	cityReal, cityErr := filepath.EvalSymlinks(cityAbs)
	pathReal, pathErr := filepath.EvalSymlinks(pathAbs)
	if cityErr == nil && pathErr == nil {
		return cleanedPathWithin(filepath.Clean(cityReal), filepath.Clean(pathReal))
	}
	return true
}

func cleanedPathWithin(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel))
}

func scanLegacyOrderRoot(root legacyOrderRoot) []string {
	entries, err := os.ReadDir(root.dir)
	if err != nil {
		return nil
	}
	var details []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		source := filepath.Join(root.dir, name, "order.toml")
		if _, err := os.Stat(source); err != nil {
			continue
		}
		details = append(details, fmt.Sprintf("%s; "+root.hint, source, name))
	}
	return details
}

type legacyOrderMove struct {
	source string
	target string
}

func fixLegacyOrderLayouts(cityPath string) error {
	moves, err := planLegacyOrderMoves(legacyOrderLayoutRoots(cityPath))
	if err != nil {
		return err
	}
	var applied []legacyOrderMove
	for _, move := range moves {
		if err := os.MkdirAll(filepath.Dir(move.target), 0o755); err != nil {
			return rollbackLegacyOrderMoves(applied, fmt.Errorf("creating %s: %w", filepath.Dir(move.target), err))
		}
		if _, err := os.Stat(move.target); err == nil {
			return rollbackLegacyOrderMoves(applied, fmt.Errorf("target already exists: %s", move.target))
		} else if !os.IsNotExist(err) {
			return rollbackLegacyOrderMoves(applied, fmt.Errorf("checking target %s: %w", move.target, err))
		}
		if err := os.Rename(move.source, move.target); err != nil {
			return rollbackLegacyOrderMoves(applied, fmt.Errorf("moving %s to %s: %w", move.source, move.target, err))
		}
		applied = append(applied, move)
		_ = os.Remove(filepath.Dir(move.source))
	}
	return nil
}

func planLegacyOrderMoves(roots []legacyOrderRoot) ([]legacyOrderMove, error) {
	targetSources := make(map[string]string)
	var moves []legacyOrderMove
	var problems []string
	for _, root := range roots {
		if !root.fixable {
			continue
		}
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			problems = append(problems, fmt.Sprintf("reading %s: %v", root.dir, err))
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			source := filepath.Join(root.dir, entry.Name(), "order.toml")
			info, err := os.Stat(source)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				problems = append(problems, fmt.Sprintf("checking source %s: %v", source, err))
				continue
			}
			if info.IsDir() {
				problems = append(problems, fmt.Sprintf("source is a directory, not an order file: %s", source))
				continue
			}
			target := filepath.Join(root.targetDir, entry.Name()+".toml")
			if prior, ok := targetSources[target]; ok {
				problems = append(problems, fmt.Sprintf("multiple legacy order files would migrate to %s: %s and %s", target, prior, source))
				continue
			}
			targetSources[target] = source
			if _, err := os.Stat(target); err == nil {
				problems = append(problems, fmt.Sprintf("target already exists: %s", target))
				continue
			} else if !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("checking target %s: %v", target, err))
				continue
			}
			moves = append(moves, legacyOrderMove{source: source, target: target})
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("refusing to migrate legacy order layouts:\n  %s", strings.Join(problems, "\n  "))
	}
	return moves, nil
}

func rollbackLegacyOrderMoves(applied []legacyOrderMove, cause error) error {
	var restoreErr error
	for i := len(applied) - 1; i >= 0; i-- {
		move := applied[i]
		if err := os.MkdirAll(filepath.Dir(move.source), 0o755); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("recreating %s: %w", filepath.Dir(move.source), err))
			continue
		}
		if err := os.Rename(move.target, move.source); err != nil && !os.IsNotExist(err) {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring %s to %s: %w", move.target, move.source, err))
		}
	}
	if restoreErr != nil {
		return errors.Join(cause, restoreErr)
	}
	return cause
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
	path    string
	locator config.DiagnosticLocator
}

func newDoctorConfigLocator(path string) doctorConfigLocator {
	data, err := os.ReadFile(path)
	if err != nil {
		return doctorConfigLocator{path: path}
	}
	return doctorConfigLocator{path: path, locator: config.NewDiagnosticLocator(data)}
}

func (l doctorConfigLocator) source(line int) string {
	source := filepath.Base(l.path)
	if line <= 0 {
		return source
	}
	return fmt.Sprintf("%s:%d", source, line)
}

func (l doctorConfigLocator) lineForTable(table string) int {
	return l.locator.LineForTable(table)
}

func (l doctorConfigLocator) lineForKey(table, key string) int {
	return l.locator.LineForKey(table, key)
}

func (l doctorConfigLocator) lineForRigPath(rigName string) int {
	return l.locator.LineForRigPath(rigName)
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

func doctorPackSourceDetails(path string, cfg *config.City) []string {
	if cfg == nil || len(cfg.Packs) == 0 {
		return nil
	}
	locator := newDoctorConfigLocator(path)
	names := make([]string, 0, len(cfg.Packs))
	for name := range cfg.Packs {
		names = append(names, name)
	}
	sort.Strings(names)

	workspaceRefs := make(map[string]struct{})
	for _, include := range cfg.Workspace.LegacyIncludes() {
		workspaceRefs[include] = struct{}{}
	}
	for _, include := range cfg.Workspace.LegacyDefaultRigIncludes() {
		workspaceRefs[include] = struct{}{}
	}
	rigRefs := make(map[string]struct{})
	for _, rig := range cfg.Rigs {
		for _, include := range rig.Includes {
			rigRefs[include] = struct{}{}
		}
	}

	details := make([]string, 0, len(names))
	for _, name := range names {
		line := locator.locator.LineForTable("packs." + name)
		if line == 0 {
			line = locator.locator.LineForPacksTable()
		}
		remediation := "manual cleanup required"
		if _, ok := workspaceRefs[name]; ok {
			remediation = "gc doctor --fix can migrate this workspace/default-rig include reference"
			if _, rigReferenced := rigRefs[name]; rigReferenced {
				remediation = "manual cleanup required after gc doctor --fix because a rig include still references this pack"
			}
		}
		src := strings.TrimSpace(cfg.Packs[name].Source)
		if src == "" {
			src = "<empty source>"
		}
		details = append(details, fmt.Sprintf("%s: [packs.%s] source=%q (%s)",
			locator.source(line), name, src, remediation))
	}
	return details
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
