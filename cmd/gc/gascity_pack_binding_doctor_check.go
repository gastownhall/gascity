package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// gascityPackCanonicalBinding is the import key the public Gas City pack
// ecosystem is written against: the pack README installs it with
// `gc import add --name gc`, its role prompts call `gc gc claim`, its skills
// surface as `gc.<skill>`, and gc init has written [imports.gc] since v1.5.
// gc v1.4.x init bound the same pack as [imports.gascity].
const gascityPackCanonicalBinding = "gc"

// gascityPackBindingDoctorCheck flags a city-level import of the public Gas
// City pack bound under a key other than "gc" and, with --fix, renames that
// key to "gc". Only the city pack's own imports are considered: pack.toml
// [imports] plus the city.toml root [imports] overrides. Rig imports and
// [defaults.rig.imports] (where the gc-roles subpack lives) are never touched.
//
// The pack is identified by its source identity (repository + pack subpath,
// normalized across tree-URL, .git and //subpath spellings), never by the key
// name, so a third-party pack bound as "gascity" is left alone.
type gascityPackBindingDoctorCheck struct {
	cityPath string
}

func newGascityPackBindingDoctorCheck(cityPath string) *gascityPackBindingDoctorCheck {
	return &gascityPackBindingDoctorCheck{cityPath: cityPath}
}

func (c *gascityPackBindingDoctorCheck) Name() string { return "gascity-pack-binding" }

func (c *gascityPackBindingDoctorCheck) CanFix() bool { return true }

// WarmupEligible returns false; this check is not part of the gc start
// warm-up scan.
func (c *gascityPackBindingDoctorCheck) WarmupEligible() bool { return false }

// gascityPackBindingPlan is the analysis the check's Run and Fix share.
type gascityPackBindingPlan struct {
	// Rename is the non-gc key to rebind as gc ("" when none).
	Rename string
	// Remove lists non-gc keys that duplicate the gc import exactly and can
	// be dropped without changing what the city resolves.
	Remove []string
	// Blocked lists human-readable reasons a key cannot be repaired
	// automatically. A non-empty Blocked makes Fix refuse all changes.
	Blocked []string
	// Keys lists every non-gc key bound to the public Gas City pack.
	Keys []string
}

func (p gascityPackBindingPlan) empty() bool { return len(p.Keys) == 0 }

// isPublicGascityPackSource reports whether source addresses the public Gas
// City pack (gastownhall/gascity-packs, pack root "gascity") in any spelling
// a user may have authored: tree URL, .git and //subpath forms, https,
// http, SSH (git@github.com:... or ssh://), and any letter case in the
// GitHub host/owner/repo (GitHub treats those case-insensitively). Forks,
// other hosts, the gc-roles subpack ("gascity/roles"), and every other pack
// return false.
func isPublicGascityPackSource(source string) bool {
	name, repository, ok := builtinpacks.SourceLayout(canonicalGitHubSourceSpelling(source))
	return ok && repository == builtinpacks.PublicRepository && name == "gascity"
}

// canonicalGitHubSourceSpelling rewrites a GitHub source into the https form
// with a lowercase host/owner/repo so builtinpacks.SourceLayout (which
// compares the https spelling exactly) recognizes it. The pack subpath and
// ref keep their case. Non-GitHub sources are returned trimmed but otherwise
// unchanged.
func canonicalGitHubSourceSpelling(source string) string {
	s := strings.TrimSpace(source)
	lower := strings.ToLower(s)
	var rest string
	switch {
	case strings.HasPrefix(lower, "https://"):
		rest = s[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		rest = s[len("http://"):]
	case strings.HasPrefix(lower, "ssh://"):
		rest = s[len("ssh://"):]
		if strings.HasPrefix(strings.ToLower(rest), "git@") {
			rest = rest[len("git@"):]
		}
	case strings.HasPrefix(lower, "git@"):
		rest = s[len("git@"):]
		if i := strings.Index(rest, ":"); i >= 0 && !strings.Contains(rest[:i], "/") {
			rest = rest[:i] + "/" + rest[i+1:]
		}
	case strings.HasPrefix(lower, "github.com/"):
		rest = s
	default:
		return s
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 || !strings.EqualFold(parts[0], "github.com") {
		return s
	}
	out := "https://github.com/" + strings.ToLower(parts[1]) + "/" + strings.ToLower(parts[2])
	if len(parts) == 4 {
		out += "/" + parts[3]
	}
	return out
}

// gascityPackBindingImports returns the city pack's effective root imports:
// pack.toml [imports] overridden by city.toml root [imports], matching
// applyCityRootImportOverridesFS.
func gascityPackBindingImports(fs fsys.FS, cityPath string) (map[string]config.Import, error) {
	manifest, err := loadCityPackManifestFS(fs, cityPath)
	if err != nil {
		return nil, err
	}
	effective := copyImports(manifest.Imports)
	if err := applyCityRootImportOverridesFS(fs, cityPath, effective); err != nil {
		return nil, err
	}
	return effective, nil
}

func planGascityPackBinding(fs fsys.FS, cityPath string, effective map[string]config.Import) gascityPackBindingPlan {
	var plan gascityPackBindingPlan
	for name, imp := range effective {
		if name != gascityPackCanonicalBinding && isPublicGascityPackSource(imp.Source) {
			plan.Keys = append(plan.Keys, name)
		}
	}
	if len(plan.Keys) == 0 {
		return plan
	}
	sort.Strings(plan.Keys)

	for _, key := range plan.Keys {
		for _, ref := range gascityBindingReferences(fs, cityPath, key) {
			plan.Blocked = append(plan.Blocked, fmt.Sprintf("%s: %s looks like a %q-qualified name that the rename would leave dangling; if it refers to this import, change it to %q by hand and rerun, otherwise rename the import key by hand", key, ref, key+".", gascityPackCanonicalBinding+"."))
		}
	}

	gcImp, hasGC := effective[gascityPackCanonicalBinding]
	if hasGC && !isPublicGascityPackSource(gcImp.Source) {
		plan.Blocked = append(plan.Blocked, fmt.Sprintf("refusing to rebind: the %q import key is already bound to a different source %q", gascityPackCanonicalBinding, gcImp.Source))
		return plan
	}

	duplicatesOf := gcImp
	candidates := plan.Keys
	if !hasGC {
		// Prefer the v1.4.x init binding; otherwise the first key in order.
		plan.Rename = candidates[0]
		for _, key := range candidates {
			if key == "gascity" {
				plan.Rename = key
				break
			}
		}
		duplicatesOf = effective[plan.Rename]
	}
	for _, key := range candidates {
		if key == plan.Rename {
			continue
		}
		if sameImportAuthoring(effective[key], duplicatesOf) {
			plan.Remove = append(plan.Remove, key)
			continue
		}
		plan.Blocked = append(plan.Blocked, fmt.Sprintf("%s: imports the Gas City pack a second time with different settings (source %q, version %q) than the %q import; remove one of them by hand", key, effective[key].Source, effective[key].Version, gascityPackCanonicalBinding))
	}
	return plan
}

// sameImportAuthoring reports whether two imports are authored identically,
// so dropping one leaves the city resolving exactly the same pack revision
// and packs.lock (keyed by the source string) unchanged.
func sameImportAuthoring(a, b config.Import) bool {
	if strings.TrimSpace(a.Source) != strings.TrimSpace(b.Source) ||
		strings.TrimSpace(a.Version) != strings.TrimSpace(b.Version) ||
		a.Export != b.Export ||
		strings.TrimSpace(a.Shadow) != strings.TrimSpace(b.Shadow) {
		return false
	}
	if (a.Transitive == nil) != (b.Transitive == nil) {
		return false
	}
	return a.Transitive == nil || *a.Transitive == *b.Transitive
}

// gascityBindingReferences reports string values in the city manifests
// (pack.toml and city.toml; comments are ignored) shaped exactly like a name
// qualified by the binding — "<key>.<name>" or "<rig>/<key>.<name>", with no
// further dots or slashes — such as an agent, skill, or session name that a
// rename would leave dangling. Each entry names the file and TOML path.
func gascityBindingReferences(fs fsys.FS, cityPath, key string) []string {
	re := regexp.MustCompile(`^([A-Za-z0-9_-]+/)?` + regexp.QuoteMeta(key) + `\.[A-Za-z0-9_-]+$`)
	var refs []string
	for _, name := range []string{"pack.toml", "city.toml"} {
		data, err := fs.ReadFile(filepath.Join(cityPath, name))
		if err != nil {
			continue
		}
		var doc map[string]any
		if _, err := toml.Decode(string(data), &doc); err != nil {
			continue
		}
		walkTOMLStrings(doc, "", func(path, value string) {
			if re.MatchString(value) {
				refs = append(refs, fmt.Sprintf("%s %s = %q", name, path, value))
			}
		})
	}
	sort.Strings(refs)
	return refs
}

func walkTOMLStrings(v any, path string, visit func(path, value string)) {
	switch t := v.(type) {
	case string:
		visit(path, t)
	case map[string]any:
		for k, child := range t {
			next := k
			if path != "" {
				next = path + "." + k
			}
			walkTOMLStrings(child, next, visit)
		}
	case []map[string]any:
		for i, child := range t {
			walkTOMLStrings(child, fmt.Sprintf("%s[%d]", path, i), visit)
		}
	case []any:
		for i, child := range t {
			walkTOMLStrings(child, fmt.Sprintf("%s[%d]", path, i), visit)
		}
	}
}

func (c *gascityPackBindingDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	effective, err := gascityPackBindingImports(fsys.OSFS{}, c.cityPath)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("reading city imports: %v", err)
		return r
	}
	plan := planGascityPackBinding(fsys.OSFS{}, c.cityPath, effective)
	if plan.empty() {
		r.Status = doctor.StatusOK
		r.Message = "Gas City pack binding ok"
		return r
	}

	quoted := make([]string, 0, len(plan.Keys))
	for _, key := range plan.Keys {
		quoted = append(quoted, "[imports."+key+"]")
	}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("the public Gas City pack is imported as %s, but the pack ecosystem expects [imports.gc] (skill gc.mayor, command `gc gc claim`); moving the pack past 0.1.6 requires it", strings.Join(quoted, ", "))
	r.Details = []string{
		"the import key namespaces the pack's commands and skills: the pack documents skill `gc.mayor`, and newer role prompts run `gc gc claim`",
		"moving the Gas City pack past 0.1.6 requires the gc binding; packs.lock is keyed by source, so the rename needs no reinstall",
	}
	switch {
	case len(plan.Blocked) > 0:
		r.Details = append(r.Details, plan.Blocked...)
		r.FixHint = "resolve the conflicts above by hand, then rename the import key to gc in pack.toml"
	case plan.Rename != "":
		r.FixHint = fmt.Sprintf(`run "gc doctor --fix" to rename [imports.%s] to [imports.gc]`, plan.Rename)
		if len(plan.Remove) > 0 {
			r.FixHint += " and remove the duplicate " + strings.Join(plan.Remove, ", ") + " import(s)"
		}
	default:
		r.FixHint = fmt.Sprintf(`run "gc doctor --fix" to remove the duplicate %s import(s); [imports.gc] already imports the same pack`, strings.Join(plan.Remove, ", "))
	}
	return r
}

func (c *gascityPackBindingDoctorCheck) Fix(_ *doctor.CheckContext) error {
	return fixGascityPackBindingFS(fsys.OSFS{}, c.cityPath)
}

// fixGascityPackBindingFS applies the plan through the same manifest editing
// helpers `gc import add/remove` use. It refuses (changing nothing) when any
// key is blocked, and rolls both manifests back if a write fails.
func fixGascityPackBindingFS(fs fsys.FS, cityPath string) error {
	effective, err := gascityPackBindingImports(fs, cityPath)
	if err != nil {
		return fmt.Errorf("reading city imports: %w", err)
	}
	plan := planGascityPackBinding(fs, cityPath, effective)
	if plan.empty() {
		return nil
	}
	if len(plan.Blocked) > 0 {
		return fmt.Errorf("not rebinding the Gas City pack import: %s", strings.Join(plan.Blocked, "; "))
	}

	manifest, err := loadCityPackManifestFS(fs, cityPath)
	if err != nil {
		return err
	}
	var cfg *config.City
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	if _, err := fs.Stat(cityTomlPath); err == nil {
		cfg, err = loadCityImportManifestFS(fs, cityPath)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	apply := func(imports map[string]config.Import) bool {
		changed := false
		if plan.Rename != "" {
			if imp, ok := imports[plan.Rename]; ok {
				delete(imports, plan.Rename)
				imports[gascityPackCanonicalBinding] = imp
				changed = true
			}
		}
		for _, key := range plan.Remove {
			if _, ok := imports[key]; ok {
				delete(imports, key)
				changed = true
			}
		}
		return changed
	}
	packChanged := apply(manifest.Imports)
	cityChanged := cfg != nil && apply(cfg.Imports)
	if !packChanged && !cityChanged {
		return nil
	}

	packSnap, err := snapshotResolvedFile(fs, filepath.Join(cityPath, "pack.toml"))
	if err != nil {
		return fmt.Errorf("snapshotting pack.toml: %w", err)
	}
	citySnap, err := snapshotResolvedFile(fs, cityTomlPath)
	if err != nil {
		return fmt.Errorf("snapshotting city.toml: %w", err)
	}
	snapshots := []fileSnapshot{packSnap, citySnap}
	rollback := func(action string, cause error) error {
		if restoreErr := restoreSnapshots(fs, snapshots); restoreErr != nil {
			return fmt.Errorf("%s: %w (rollback failed: %w)", action, cause, restoreErr)
		}
		return fmt.Errorf("%s: %w", action, cause)
	}
	if packChanged {
		if err := writeCityPackManifest(fs, cityPath, manifest); err != nil {
			return rollback("writing pack.toml", err)
		}
	}
	if cityChanged {
		if err := writeCityImportManifestFS(fs, cityPath, cfg); err != nil {
			return rollback("writing city.toml", err)
		}
	}
	return nil
}
