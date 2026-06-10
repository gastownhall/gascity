package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

type importStateDoctorCheck struct {
	cityPath string
}

const importStateSyncFixHint = `run "gc doctor --fix" or "gc import install"`

var resolveWave1PublicPackImports = defaultWave1PublicPackImports

type wave1PublicPackImportTarget struct {
	Binding string
	Import  config.Import
	Remove  bool
}

type legacyPublicPackRewrite struct {
	From string
	To   string
}

func newImportStateDoctorCheck(cityPath string) *importStateDoctorCheck {
	return &importStateDoctorCheck{cityPath: cityPath}
}

func (c *importStateDoctorCheck) Name() string { return "packv2-import-state" }

func (c *importStateDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	imports, err := collectAllImportsFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("reading declared imports: %v", err)
		return r
	}
	if details := durableRegistryImportDetails(imports); len(details) > 0 {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("%d durable import(s) use command-time registry selectors", len(details))
		r.FixHint = "replace registry: sources with concrete pack sources"
		r.Details = details
		return r
	}
	if details := legacyPublicPackImportDetails(c.cityPath, imports); len(details) > 0 {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("%d legacy public built-in pack import(s)", len(details))
		r.FixHint = `run "gc doctor --fix" to rewrite legacy gastown imports and remove legacy maintenance imports`
		r.Details = details
		return r
	}
	report, err := checkInstalledImports(c.cityPath, imports)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("checking import state: %v", err)
		r.FixHint = importStateSyncFixHint
		return r
	}
	if !report.HasIssues() {
		r.Status = doctor.StatusOK
		r.Message = fmt.Sprintf("%d remote import(s) installed", report.CheckedSources)
		return r
	}

	r.Status = doctor.StatusError
	r.Message = fmt.Sprintf("%d import state issue(s)", len(report.Issues))
	r.FixHint = importStateSyncFixHint
	for _, issue := range report.Issues {
		r.Details = append(r.Details, formatImportStateDoctorDetail(issue))
	}
	return r
}

func durableRegistryImportDetails(imports map[string]config.Import) []string {
	var names []string
	for name, imp := range imports {
		if strings.HasPrefix(strings.TrimSpace(imp.Source), "registry:") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	details := make([]string, 0, len(names))
	for _, name := range names {
		details = append(details, fmt.Sprintf("registry-selector-source | %s | %s | registry selectors are command-time inputs only; pack.toml must store the concrete pack source", name, imports[name].Source))
	}
	return details
}

func (c *importStateDoctorCheck) CanFix() bool { return true }

func (c *importStateDoctorCheck) Fix(_ *doctor.CheckContext) error {
	imports, err := collectAllImportsFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		return fmt.Errorf("reading declared imports: %w", err)
	}
	if details := durableRegistryImportDetails(imports); len(details) > 0 {
		return fmt.Errorf("durable registry selectors require manual replacement with concrete pack sources")
	}
	if details := legacyPublicPackImportDetails(c.cityPath, imports); len(details) > 0 {
		targets, err := resolveWave1PublicPackImports(legacyPublicPackNames(imports, c.cityPath))
		if err != nil {
			return err
		}
		if _, err := rewriteLegacyPublicPackImportsFS(fsys.OSFS{}, c.cityPath, targets); err != nil {
			return err
		}
		imports, err = collectAllImportsFS(fsys.OSFS{}, c.cityPath)
		if err != nil {
			return fmt.Errorf("reading migrated imports: %w", err)
		}
	}
	lock, err := syncImports(c.cityPath, imports, packman.InstallResolveIfNeeded)
	if err != nil {
		return err
	}
	if err := writeImportLockfile(fsys.OSFS{}, c.cityPath, lock); err != nil {
		return err
	}
	if _, err := installLockedImports(c.cityPath); err != nil {
		return err
	}
	return nil
}

func defaultWave1PublicPackImports(packNames []string) (map[string]wave1PublicPackImportTarget, error) {
	targets := make(map[string]wave1PublicPackImportTarget, len(packNames))
	for _, packName := range packNames {
		switch packName {
		case "gastown":
			targets[packName] = wave1PublicPackImportTarget{
				Binding: "gastown",
				Import: config.Import{
					Source:  config.PublicGastownPackSource,
					Version: config.PublicGastownPackVersion,
				},
			}
		case "maintenance":
			targets[packName] = wave1PublicPackImportTarget{
				Binding: "maintenance",
				Remove:  true,
			}
		default:
			return nil, fmt.Errorf("unsupported wave 1 public pack migration target %q", packName)
		}
	}
	return targets, nil
}

func formatImportStateDoctorDetail(issue packman.CheckIssue) string {
	parts := []string{issue.Code}
	if issue.ImportName != "" {
		parts = append(parts, issue.ImportName)
	}
	if issue.Source != "" {
		parts = append(parts, issue.Source)
	}
	if issue.Commit != "" {
		parts = append(parts, "commit="+issue.Commit)
	}
	if issue.Path != "" {
		parts = append(parts, "path="+issue.Path)
	}
	if issue.Message != "" {
		parts = append(parts, issue.Message)
	}
	return strings.Join(parts, " | ")
}

func legacyPublicPackImportDetails(cityPath string, imports map[string]config.Import) []string {
	var names []string
	for name, imp := range imports {
		if _, ok := legacyPublicPackForSource(cityPath, imp.Source); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	details := make([]string, 0, len(names))
	for _, name := range names {
		pack, _ := legacyPublicPackForSource(cityPath, imports[name].Source)
		action := "should use the public gascity-packs source"
		if pack == "maintenance" {
			action = "should be removed; the maintenance pack was folded into the bundled core pack"
		}
		details = append(details, fmt.Sprintf("legacy-public-pack-source | %s | %s | legacy %s import %s", name, imports[name].Source, pack, action))
	}
	return details
}

func legacyPublicPackNames(imports map[string]config.Import, cityPath string) []string {
	seen := make(map[string]bool)
	for _, imp := range imports {
		if pack, ok := legacyPublicPackForSource(cityPath, imp.Source); ok {
			seen[pack] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func legacyPublicPackForSource(cityPath, source string) (string, bool) {
	source = strings.TrimSpace(source)
	if isRemoteImportSource(source) {
		return "", false
	}
	source = strings.TrimSpace(filepath.Clean(source))
	if source == "." || source == "" {
		return "", false
	}
	source = filepath.ToSlash(source)
	for _, pack := range []string{"gastown", "maintenance"} {
		if source == ".gc/system/packs/"+pack || source == "examples/gastown/packs/"+pack {
			return pack, true
		}
	}
	path := source
	if !filepath.IsAbs(path) {
		path = filepath.Join(cityPath, filepath.FromSlash(source))
	}
	path = filepath.ToSlash(filepath.Clean(path))
	for _, pack := range []string{"gastown", "maintenance"} {
		for _, suffix := range []string{"/.gc/system/packs/" + pack, "/examples/gastown/packs/" + pack} {
			if strings.HasSuffix(path, suffix) {
				return pack, true
			}
		}
	}
	return "", false
}

func rewriteLegacyPublicPackImportsFS(fs fsys.FS, cityPath string, targets map[string]wave1PublicPackImportTarget) (bool, error) {
	for packName, target := range targets {
		if strings.TrimSpace(packName) == "" || strings.TrimSpace(target.Binding) == "" {
			return false, fmt.Errorf("wave 1 public pack migration target for %q is incomplete", packName)
		}
		if !target.Remove && strings.TrimSpace(target.Import.Source) == "" {
			return false, fmt.Errorf("wave 1 public pack migration target for %q is missing source", packName)
		}
	}
	changed := false

	manifest, err := loadCityPackManifestFS(fs, cityPath)
	if err != nil {
		return false, err
	}
	packChanged, _, err := rewriteLegacyPublicPackImportMap(cityPath, manifest.Imports, targets)
	if err != nil {
		return false, fmt.Errorf("pack.toml imports: %w", err)
	}
	defaultRigChanged, defaultRigRewrites, err := rewriteLegacyPublicPackImportMap(cityPath, manifest.Defaults.Rig.Imports, targets)
	if err != nil {
		return false, fmt.Errorf("pack.toml default rig imports: %w", err)
	}

	var cfg *config.City
	cityChanged := false
	if _, err := fs.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			if packChanged || defaultRigChanged {
				if defaultRigChanged {
					manifest.DefaultRigImportOrder = replaceImportOrderWithTargets(manifest.DefaultRigImportOrder, defaultRigRewrites)
				}
				if err := writeCityPackManifest(fs, cityPath, manifest); err != nil {
					return false, err
				}
				changed = true
			}
			return changed, nil
		}
		return false, err
	}
	cfg, err = loadCityImportManifestFS(fs, cityPath)
	if err != nil {
		return false, err
	}
	for i := range cfg.Rigs {
		rigChanged, _, err := rewriteLegacyPublicPackImportMap(cityPath, cfg.Rigs[i].Imports, targets)
		if err != nil {
			return false, fmt.Errorf("city.toml rig %q imports: %w", cfg.Rigs[i].Name, err)
		}
		cityChanged = cityChanged || rigChanged
	}
	cityDefaultRigChanged, _, err := rewriteLegacyPublicPackImportMap(cityPath, cfg.Defaults.Rig.Imports, targets)
	if err != nil {
		return false, fmt.Errorf("city.toml default rig imports: %w", err)
	}
	cityChanged = cityChanged || cityDefaultRigChanged
	if packChanged || defaultRigChanged {
		if defaultRigChanged {
			manifest.DefaultRigImportOrder = replaceImportOrderWithTargets(manifest.DefaultRigImportOrder, defaultRigRewrites)
		}
		if err := writeCityPackManifest(fs, cityPath, manifest); err != nil {
			return false, err
		}
		changed = true
	}
	if cityChanged {
		if err := writeCityImportManifestFS(fs, cityPath, cfg); err != nil {
			return false, err
		}
		changed = true
	}

	return changed, nil
}

func rewriteLegacyPublicPackImportMap(cityPath string, imports map[string]config.Import, targets map[string]wave1PublicPackImportTarget) (bool, []legacyPublicPackRewrite, error) {
	if len(imports) == 0 {
		return false, nil, nil
	}
	legacy := make(map[string]string)
	for name, imp := range imports {
		if packName, ok := legacyPublicPackForSource(cityPath, imp.Source); ok {
			legacy[name] = packName
		}
	}
	if len(legacy) == 0 {
		return false, nil, nil
	}
	var legacyNames []string
	for name := range legacy {
		legacyNames = append(legacyNames, name)
	}
	sort.Strings(legacyNames)

	var rewrites []legacyPublicPackRewrite
	targetBindings := make(map[string]wave1PublicPackImportTarget)
	for _, name := range legacyNames {
		packName := legacy[name]
		target, ok := targets[packName]
		if !ok {
			return false, nil, fmt.Errorf("missing migration target for legacy %q import %q", packName, name)
		}
		to := ""
		if !target.Remove {
			targetBindings[target.Binding] = target
			to = target.Binding
		}
		rewrites = append(rewrites, legacyPublicPackRewrite{From: name, To: to})
	}
	for binding, target := range targetBindings {
		if existing, ok := imports[binding]; ok && !sameImport(existing, target.Import) {
			if _, legacyTarget := legacyPublicPackForSource(cityPath, existing.Source); !legacyTarget {
				return false, nil, fmt.Errorf("refusing to overwrite existing %q import with source %q", binding, existing.Source)
			}
		}
	}
	for _, name := range legacyNames {
		delete(imports, name)
	}
	for binding, target := range targetBindings {
		imports[binding] = target.Import
	}
	return true, rewrites, nil
}

func sameImport(a, b config.Import) bool {
	return strings.TrimSpace(a.Source) == strings.TrimSpace(b.Source) &&
		strings.TrimSpace(a.Version) == strings.TrimSpace(b.Version)
}

func replaceImportOrderWithTargets(order []string, rewrites []legacyPublicPackRewrite) []string {
	targetByLegacy := make(map[string]string, len(rewrites))
	for _, rewrite := range rewrites {
		targetByLegacy[rewrite.From] = rewrite.To
	}
	out := make([]string, 0, len(order)+1)
	seenTarget := make(map[string]bool)
	for _, name := range order {
		if target, ok := targetByLegacy[name]; ok {
			if target != "" && !seenTarget[target] {
				out = append(out, target)
				seenTarget[target] = true
			}
			continue
		}
		seenTarget[name] = true
		out = append(out, name)
	}
	for _, rewrite := range rewrites {
		if rewrite.To != "" && !seenTarget[rewrite.To] {
			out = append(out, rewrite.To)
			seenTarget[rewrite.To] = true
		}
	}
	return out
}
