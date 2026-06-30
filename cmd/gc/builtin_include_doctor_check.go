package main

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// Builtin packs compose only through explicit [imports.<name>] entries with
// bundled sources: gc init writes them, this doctor check repairs them, and
// config load warns when they are missing. The retired
// workspace.includes = [".gc/system/packs/<name>"] model migrates here.

// missingRequiredBuiltinImports reports which required builtin packs are
// not reachable from the composed config's explicit imports/includes.
func missingRequiredBuiltinImports(fs fsys.FS, cfg *config.City, cityPath string) []string {
	if cfg == nil {
		return nil
	}
	reachable := config.ReachablePackNames(cfg, fs, cityPath)
	var missing []string
	for _, name := range requiredBuiltinPackNames(cityPath) {
		if !reachable[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// builtinImportWarningCache dedups the missing-import warning to once per
// city per process.
var builtinImportWarningCache sync.Map

// warnMissingRequiredBuiltinImports emits a once-per-city warning when the
// composed config does not reach a required builtin pack. The city still
// loads — it just runs without the builtin content it almost certainly
// wants — so this is a warning with a doctor-driven repair, not an error.
//
// Silent loaders (io.Discard) must not consume the once-per-city slot:
// commands often pre-load config quietly before the user-visible load, and
// the warning has to reach the visible one.
func warnMissingRequiredBuiltinImports(fs fsys.FS, cfg *config.City, tomlPath string, w io.Writer) {
	if w == nil || w == io.Discard || !usesOSFS(fs) {
		return
	}
	cityPath := filepath.Dir(tomlPath)
	missing := missingRequiredBuiltinImports(fs, cfg, cityPath)
	if len(missing) == 0 {
		return
	}
	key := normalizePathForCompare(cityPath)
	if _, alreadyWarned := builtinImportWarningCache.LoadOrStore(key, struct{}{}); alreadyWarned {
		return
	}
	fmt.Fprintf(w, "warning: this city does not import required builtin pack(s) %s; run \"gc doctor --fix\" to add the missing import(s)\n", strings.Join(missing, ", ")) //nolint:errcheck // best-effort warning emission
}

func legacySystemPacksInclude(cityPath, include string) bool {
	include = strings.TrimSpace(include)
	if include == "" {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(include))
	if strings.HasPrefix(cleaned, citylayout.SystemPacksRoot+"/") {
		return true
	}
	abs := cleaned
	if !filepath.IsAbs(include) {
		abs = filepath.ToSlash(filepath.Clean(filepath.Join(cityPath, filepath.FromSlash(include))))
	}
	return strings.Contains(abs, "/"+citylayout.SystemPacksRoot+"/")
}

// legacySystemPacksPackName extracts the pack name a legacy
// .gc/system/packs reference points at. Returns "" when the reference
// does not target the retired tree or nests below a pack directory
// (nested paths have no unambiguous import equivalent).
func legacySystemPacksPackName(cityPath, ref string) string {
	if !legacySystemPacksInclude(cityPath, ref) {
		return ""
	}
	trimmed := strings.TrimSpace(ref)
	cleaned := filepath.ToSlash(filepath.Clean(trimmed))
	if !filepath.IsAbs(trimmed) {
		cleaned = filepath.ToSlash(filepath.Clean(filepath.Join(cityPath, filepath.FromSlash(trimmed))))
	}
	idx := strings.Index(cleaned, citylayout.SystemPacksRoot+"/")
	if idx < 0 {
		return ""
	}
	name := cleaned[idx+len(citylayout.SystemPacksRoot)+1:]
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}

// legacyRefKind distinguishes include-list entries from import sources in
// a legacy system-packs reference: includes migrate by strip-or-convert,
// import sources migrate by rewriting the source in place.
type legacyRefKind int

const (
	legacyRefInclude legacyRefKind = iota
	legacyRefImportSource
)

// legacySystemPacksRef locates one config-manifest reference that still
// composes through the retired .gc/system/packs tree.
type legacySystemPacksRef struct {
	// File is the manifest holding the reference: "city.toml",
	// "pack.toml", or a fragment include entry as authored in city.toml.
	File string
	// Surface names the config surface holding the reference, e.g.
	// "workspace.includes" or "rigs[demo].imports.core.source".
	Surface string
	// Value is the include entry or import source as authored.
	Value string
	// Kind reports whether Value is an include entry or an import source.
	Kind legacyRefKind
}

// legacySystemPacksScan is the result of scanning every manifest surface
// that can compose through the retired .gc/system/packs tree.
type legacySystemPacksScan struct {
	Refs []legacySystemPacksRef
	// Uninspectable lists config fragments referenced from city.toml (and
	// pack.toml when present but unreadable) whose legacy references
	// cannot be determined: missing, unreadable, unparseable, or remote
	// include entries. The scan is inconclusive for those files, so
	// destructive callers must fail closed.
	Uninspectable []string
}

// importSourcePeek decodes just the source field of an import entry.
type importSourcePeek struct {
	Source string `toml:"source"`
}

// legacyComposeSurfacePeek decodes only the manifest surfaces that can
// compose through the retired .gc/system/packs tree, without full config
// parsing. Fragments merge workspace includes / default-rig includes
// additively and concatenate [[rigs]] (internal/config/compose.go), so
// every route the root manifest can host, a fragment can host too. The
// [pack] includes and [imports]/[defaults.rig.imports] tables cover the
// same surfaces in pack.toml.
type legacyComposeSurfacePeek struct {
	Include []string                    `toml:"include"`
	Imports map[string]importSourcePeek `toml:"imports"`
	Pack    struct {
		Includes []string `toml:"includes"`
	} `toml:"pack"`
	Workspace struct {
		Includes           []string `toml:"includes"`
		DefaultRigIncludes []string `toml:"default_rig_includes"`
	} `toml:"workspace"`
	Defaults struct {
		Rig struct {
			Imports map[string]importSourcePeek `toml:"imports"`
		} `toml:"rig"`
	} `toml:"defaults"`
	Rigs []struct {
		Name     string                      `toml:"name"`
		Includes []string                    `toml:"includes"`
		Imports  map[string]importSourcePeek `toml:"imports"`
	} `toml:"rigs"`
}

// decodeLegacyComposeSurfaces reads one manifest or fragment file.
// exists is false when the file is absent; ok is false when the file
// exists but cannot be read or parsed.
func decodeLegacyComposeSurfaces(path string) (peek *legacyComposeSurfacePeek, exists, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, true
		}
		return nil, true, false
	}
	var decoded legacyComposeSurfacePeek
	if _, err := toml.Decode(string(data), &decoded); err != nil {
		return nil, true, false
	}
	return &decoded, true, true
}

// resolveCityManifestPath resolves a root include entry the way the
// config loader resolves root-declared includes: "//" prefixes resolve
// against the city root, absolute paths are kept, and everything else is
// city-root-relative (internal/config/compose.go resolveConfigPath with
// declDir == cityRoot).
func resolveCityManifestPath(cityPath, entry string) string {
	if strings.HasPrefix(entry, "//") {
		return filepath.Join(cityPath, strings.TrimPrefix(entry, "//"))
	}
	if filepath.IsAbs(entry) {
		return entry
	}
	return filepath.Join(cityPath, entry)
}

// collectLegacySystemPacksRefs scans the city's root manifests (city.toml
// and pack.toml) plus every local config fragment referenced from
// city.toml for references that still compose through the retired
// .gc/system/packs tree. ok is false when city.toml exists but cannot be
// read or parsed — destructive callers must fail closed. Fragments and a
// pack.toml that exist but cannot be inspected (unreadable, unparseable,
// or remote include entries the peek cannot fetch) are reported in
// Uninspectable instead of failing the whole scan, so the doctor can
// surface them without masking the readable surfaces.
func collectLegacySystemPacksRefs(cityPath string) (scan legacySystemPacksScan, ok bool) {
	root, _, rootOK := decodeLegacyComposeSurfaces(filepath.Join(cityPath, "city.toml"))
	if !rootOK {
		return legacySystemPacksScan{}, false
	}
	if root != nil {
		scan.appendSurfaceRefs(cityPath, "city.toml", root)
		for _, inc := range root.Include {
			frag, _, fragOK := decodeLegacyComposeSurfaces(resolveCityManifestPath(cityPath, inc))
			if !fragOK || frag == nil {
				scan.Uninspectable = append(scan.Uninspectable, inc)
				continue
			}
			scan.appendSurfaceRefs(cityPath, inc, frag)
		}
	}
	pack, packExists, packOK := decodeLegacyComposeSurfaces(filepath.Join(cityPath, "pack.toml"))
	switch {
	case !packOK && packExists:
		scan.Uninspectable = append(scan.Uninspectable, "pack.toml")
	case pack != nil:
		scan.appendSurfaceRefs(cityPath, "pack.toml", pack)
	}
	return scan, true
}

func (s *legacySystemPacksScan) appendSurfaceRefs(cityPath, file string, peek *legacyComposeSurfacePeek) {
	add := func(surface, value string, kind legacyRefKind) {
		if legacySystemPacksInclude(cityPath, value) {
			s.Refs = append(s.Refs, legacySystemPacksRef{File: file, Surface: surface, Value: value, Kind: kind})
		}
	}
	for _, inc := range peek.Pack.Includes {
		add("pack.includes", inc, legacyRefInclude)
	}
	for _, inc := range peek.Workspace.Includes {
		add("workspace.includes", inc, legacyRefInclude)
	}
	for _, inc := range peek.Workspace.DefaultRigIncludes {
		add("workspace.default_rig_includes", inc, legacyRefInclude)
	}
	for _, name := range sortedPeekImportNames(peek.Imports) {
		add(fmt.Sprintf("imports.%s.source", name), peek.Imports[name].Source, legacyRefImportSource)
	}
	for _, name := range sortedPeekImportNames(peek.Defaults.Rig.Imports) {
		add(fmt.Sprintf("defaults.rig.imports.%s.source", name), peek.Defaults.Rig.Imports[name].Source, legacyRefImportSource)
	}
	for i, rig := range peek.Rigs {
		label := strings.TrimSpace(rig.Name)
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		for _, inc := range rig.Includes {
			add(fmt.Sprintf("rigs[%s].includes", label), inc, legacyRefInclude)
		}
		for _, name := range sortedPeekImportNames(rig.Imports) {
			add(fmt.Sprintf("rigs[%s].imports.%s.source", label, name), rig.Imports[name].Source, legacyRefImportSource)
		}
	}
}

func sortedPeekImportNames(imports map[string]importSourcePeek) []string {
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type builtinImportDoctorCheck struct {
	cityPath string
}

func newBuiltinImportDoctorCheck(cityPath string) *builtinImportDoctorCheck {
	return &builtinImportDoctorCheck{cityPath: cityPath}
}

func (c *builtinImportDoctorCheck) Name() string { return "builtin-pack-imports" }

func (c *builtinImportDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	if _, err := os.Stat(filepath.Join(c.cityPath, "city.toml")); err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("reading city.toml: %v", err)
		return r
	}

	if _, err := loadCityImportManifestFS(fsys.OSFS{}, c.cityPath); err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("reading city.toml manifest: %v", err)
		return r
	}
	scan, scanOK := collectLegacySystemPacksRefs(c.cityPath)
	if !scanOK {
		r.Status = doctor.StatusError
		r.Message = "reading city.toml manifest: cannot inspect legacy .gc/system/packs references"
		return r
	}
	refs := doctorOwnedLegacyRefs(c.cityPath, scan.Refs)

	var missing []string
	cfg, loadErr := loadCityConfigWithoutBuiltinPackRefresh(c.cityPath, io.Discard)
	if loadErr == nil {
		missing = missingRequiredBuiltinImports(fsys.OSFS{}, cfg, c.cityPath)
	}

	if len(refs) == 0 && len(missing) == 0 && loadErr == nil {
		if uninspectable := c.uninspectableWithRetiredTree(scan); len(uninspectable) > 0 {
			r.Status = doctor.StatusWarning
			r.Message = fmt.Sprintf("%d config fragment(s) cannot be inspected for legacy %s references", len(uninspectable), citylayout.SystemPacksRoot)
			for _, name := range uninspectable {
				r.Details = append(r.Details, fmt.Sprintf("uninspectable-config-fragment | %s | the retired %s tree is preserved as a precaution; if this fragment does not reference it, remove the tree by hand", name, citylayout.SystemPacksRoot))
			}
			return r
		}
		r.Status = doctor.StatusOK
		r.Message = "required builtin pack imports present"
		return r
	}

	if loadErr != nil && len(refs) == 0 {
		// Config does not load and no legacy system-packs reference explains
		// it; other doctor checks own general config errors.
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("cannot evaluate builtin imports: %v", loadErr)
		return r
	}

	r.Status = doctor.StatusError
	r.FixHint = `run "gc doctor --fix" to migrate builtin pack composition to [imports]`
	var parts []string
	auto, manual := splitLegacyRefsByFixability(c.cityPath, refs)
	for _, ref := range auto {
		if ref.Surface == "workspace.includes" {
			if _, _, ok := bundledImportForLegacySystemPacksRef(c.cityPath, ref.Value); ok {
				r.Details = append(r.Details, fmt.Sprintf("legacy-system-packs-include | %s | builtin packs compose via [imports] now; migrates to the pinned bundled import on --fix", ref.Value))
			} else {
				r.Details = append(r.Details, fmt.Sprintf("legacy-system-packs-include | %s | removed on --fix; the maintenance pack was folded into the bundled core pack", ref.Value))
			}
			continue
		}
		r.Details = append(r.Details, fmt.Sprintf("legacy-system-packs-ref | city.toml %s | %s | migrates to the pinned bundled import on --fix", ref.Surface, ref.Value))
	}
	if len(auto) > 0 {
		parts = append(parts, fmt.Sprintf("%d legacy %s reference(s)", len(auto), citylayout.SystemPacksRoot))
	}
	for _, ref := range manual {
		r.Details = append(r.Details, fmt.Sprintf("legacy-system-packs-manual | %s %s | %s | %s", ref.File, ref.Surface, ref.Value, manualLegacyRefInstruction(c.cityPath, ref)))
	}
	if len(manual) > 0 {
		parts = append(parts, fmt.Sprintf("%d legacy %s reference(s) needing manual migration", len(manual), citylayout.SystemPacksRoot))
	}
	for _, name := range missing {
		r.Details = append(r.Details, fmt.Sprintf("missing-builtin-import | %s | add [imports.%s] with the bundled source", name, name))
	}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d missing required builtin import(s)", len(missing)))
	}
	r.Message = strings.Join(parts, ", ")
	return r
}

// doctorOwnedLegacyRefs filters out import-source references the
// packv2-import-state check owns: wave-1 public-pack sources
// (gastown/maintenance under the retired tree) are detected and rewritten
// there, and double-reporting them here with different guidance would
// conflict. The import-state check reads pack.toml and root city.toml
// declared imports, including top-level overrides, rig imports, and
// default-rig imports. Fragment import-source refs stay here because no
// other check names them. Include-list references to those packs stay here
// too — the import-state check only reads declared imports.
func doctorOwnedLegacyRefs(cityPath string, refs []legacySystemPacksRef) []legacySystemPacksRef {
	owned := make([]legacySystemPacksRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Kind == legacyRefImportSource && importStateOwnsWave1Ref(ref) {
			if _, wave1 := legacyPublicPackForSource(cityPath, ref.Value); wave1 {
				continue
			}
		}
		owned = append(owned, ref)
	}
	return owned
}

func importStateOwnsWave1Ref(ref legacySystemPacksRef) bool {
	if ref.File == "pack.toml" {
		return true
	}
	return ref.File == "city.toml"
}

// splitLegacyRefsByFixability mirrors what Fix can rewrite: city.toml
// workspace includes whose pack has a canonical bundled import (stripped
// after the replacement pack.toml import lands) or that reference the
// folded-into-core maintenance pack (removed), and city.toml references
// whose pack has a canonical bundled import (converted/rewritten in
// place). Everything else — fragments, pack.toml, fragment-authored
// wave-1 public-pack import sources (Fix never rewrites those; the
// packv2-import-state check owns root-declared wave-1 semantics,
// including the maintenance removal), and non-builtin packs under the
// retired tree on every surface including workspace.includes — needs the
// manual edit named in the detail line.
func splitLegacyRefsByFixability(cityPath string, refs []legacySystemPacksRef) (auto, manual []legacySystemPacksRef) {
	for _, ref := range refs {
		switch {
		case ref.File != "city.toml":
			manual = append(manual, ref)
		case ref.Surface == "workspace.includes":
			if _, _, ok := bundledImportForLegacySystemPacksRef(cityPath, ref.Value); ok {
				auto = append(auto, ref)
				continue
			}
			if _, wave1 := legacyPublicPackForSource(cityPath, ref.Value); wave1 {
				auto = append(auto, ref)
				continue
			}
			manual = append(manual, ref)
		default:
			if ref.Kind == legacyRefImportSource {
				if _, wave1 := legacyPublicPackForSource(cityPath, ref.Value); wave1 {
					manual = append(manual, ref)
					continue
				}
			}
			if _, _, ok := bundledImportForLegacySystemPacksRef(cityPath, ref.Value); ok {
				auto = append(auto, ref)
			} else {
				manual = append(manual, ref)
			}
		}
	}
	return auto, manual
}

// manualLegacyRefInstruction returns the per-reference action for a
// legacy reference Fix cannot rewrite. User-authored fragments are never
// rewritten automatically: a decode/re-marshal round trip would drop
// comments and any content outside the config schema.
func manualLegacyRefInstruction(cityPath string, ref legacySystemPacksRef) string {
	if pack, wave1 := legacyPublicPackForSource(cityPath, ref.Value); wave1 {
		if pack == "maintenance" {
			return "remove this entry; the maintenance pack was folded into the bundled core pack"
		}
		return fmt.Sprintf("replace with the public gascity-packs source for %q", pack)
	}
	if name := legacySystemPacksPackName(cityPath, ref.Value); name != "" {
		if _, ok := builtinpacks.CanonicalImportSource(name); ok {
			return fmt.Sprintf("replace with a pinned bundled import for %q (builtin packs compose via [imports]; \"gc doctor --fix\" ensures the required imports first, making this edit safe)", name)
		}
	}
	return fmt.Sprintf("references a non-builtin pack under the retired %s tree; move the pack out of the tree and update this reference by hand", citylayout.SystemPacksRoot)
}

// uninspectableWithRetiredTree reports the scan's uninspectable fragments
// only while the retired tree still exists — once the tree is gone there
// is nothing the fragments could compose through, so they stop being a
// migration concern.
func (c *builtinImportDoctorCheck) uninspectableWithRetiredTree(scan legacySystemPacksScan) []string {
	if len(scan.Uninspectable) == 0 {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(c.cityPath, citylayout.SystemPacksRoot)); err != nil {
		return nil
	}
	return scan.Uninspectable
}

func (c *builtinImportDoctorCheck) CanFix() bool { return true }

func (c *builtinImportDoctorCheck) Fix(_ *doctor.CheckContext) error {
	manifest, err := loadCityImportManifestFS(fsys.OSFS{}, c.cityPath)
	if err != nil {
		return fmt.Errorf("reading city.toml manifest: %w", err)
	}

	// 1. Ensure the required builtin imports — plus a pinned import for
	// every canonical pack whose legacy workspace include step 2 strips,
	// so stripping never narrows the composed pack set — exist in the
	// pack.toml manifest, the canonical home for city-level imports and
	// the only surface the lockfile collection reads, before stripping
	// any legacy route from city.toml. missingAfterIncludeStrip masks the
	// legacy routes, so it computes the same as-if-migrated answer on the
	// pre-strip state; writing the imports first means a crash between
	// the two manifest writes leaves the benign dual-route state (a
	// bundled import plus a legacy include of the same pack composes
	// correctly) instead of a city that composes through neither route.
	// Legacy cities without a pack.toml get a minimal one, matching the
	// migrate tool's behavior.
	missing := c.missingAfterIncludeStrip()
	imports, order := builtinImportsForNames(missing)
	for _, inc := range manifest.Workspace.LegacyIncludes() {
		imp, name, ok := bundledImportForLegacySystemPacksRef(c.cityPath, inc)
		if !ok {
			continue
		}
		if _, exists := imports[name]; exists {
			continue
		}
		imports[name] = imp
		order = append(order, name)
	}
	if len(order) > 0 {
		packManifest, err := loadCityPackManifestFS(fsys.OSFS{}, c.cityPath)
		if err != nil {
			return fmt.Errorf("reading pack.toml manifest: %w", err)
		}
		if strings.TrimSpace(packManifest.Pack.Name) == "" {
			packManifest.Pack.Name = filepath.Base(c.cityPath)
		}
		if packManifest.Pack.Schema == 0 {
			packManifest.Pack.Schema = 2
		}
		if packManifest.Imports == nil {
			packManifest.Imports = make(map[string]config.Import, len(order))
		}
		// Only write pack.toml when a landing actually changed it: the
		// write re-encodes the manifest, so a no-op rewrite would drop
		// user comments for nothing (e.g. when every stripped include
		// dedups against an existing canonical import).
		before := maps.Clone(packManifest.Imports)
		for _, name := range order {
			packManifest.Imports, err = ensureBundledImportBinding(packManifest.Imports, name, imports[name])
			if err != nil {
				return fmt.Errorf("ensuring bundled import %q in pack.toml: %w", name, err)
			}
		}
		if !maps.Equal(before, packManifest.Imports) {
			if err := writeCityPackManifest(fsys.OSFS{}, c.cityPath, packManifest); err != nil {
				return fmt.Errorf("writing pack.toml: %w", err)
			}
		}
	}

	// 2. Migrate city.toml's own legacy .gc/system/packs references:
	// strip legacy workspace includes whose replacement import landed in
	// step 1 (and the folded-into-core maintenance include), and rewrite
	// builtin-named rig includes, rig import sources, default-rig
	// includes, and city import sources to pinned bundled imports.
	// References this fix does not own — config fragments, pack.toml,
	// wave-1 public-pack import sources, and non-builtin packs under the
	// retired tree (workspace includes included) — are kept in place and
	// reported by Run with per-reference instructions.
	changed, err := migrateLegacySystemPacksManifest(c.cityPath, manifest)
	if err != nil {
		return err
	}
	if changed {
		if err := writeCityImportManifestFS(fsys.OSFS{}, c.cityPath, manifest); err != nil {
			return fmt.Errorf("writing city.toml: %w", err)
		}
	}

	// 3. Refresh the lockfile + caches so the new imports resolve offline.
	// Only when steps 1-2 actually landed or rewrote imports. When the sole
	// detected condition is one this Fix does not own — a manual-migration
	// reference or an uninspectable fragment — steps 1-2 are no-ops (order is
	// empty and nothing migrated), so resyncing every declared import here
	// would turn an advisory warning into a hard --fix failure whenever an
	// unrelated import is momentarily unresolvable (e.g. offline, or a sibling
	// superseded pin the packv2-import-state check repairs later in the run).
	if len(order) == 0 && !changed {
		return nil
	}
	allImports, err := collectAllImportsFS(c.cityPath)
	if err != nil {
		return fmt.Errorf("reading declared imports: %w", err)
	}
	lock, err := syncImports(c.cityPath, allImports, packman.InstallResolveIfNeeded)
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

// missingAfterIncludeStrip recomputes the missing required builtin packs
// as if every legacy .gc/system/packs route were already migrated:
// reachability through the preserved retired tree must not mask a missing
// import, because the migration (automatic, or the manual edits Run
// instructs) removes those routes and the imports must already be in
// place by the time the last legacy reference is removed. Falls back to
// "everything required and not literally imported" when the config still
// does not compose.
func (c *builtinImportDoctorCheck) missingAfterIncludeStrip() []string {
	if cfg, loadErr := loadCityConfigWithoutBuiltinPackRefresh(c.cityPath, io.Discard); loadErr == nil {
		return missingRequiredBuiltinImports(fsys.OSFS{}, maskLegacySystemPacksRoutes(cfg, c.cityPath), c.cityPath)
	}
	declared, err := collectAllImportsFS(c.cityPath)
	if err != nil {
		declared = nil
	}
	var missing []string
	for _, name := range requiredBuiltinPackNames(c.cityPath) {
		if _, ok := declared["pack:"+name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// migrateLegacySystemPacksManifest rewrites the legacy .gc/system/packs
// references in the loaded city.toml manifest that have an unambiguous
// modern form: legacy workspace includes whose pack has a canonical
// bundled import are stripped (Fix lands the replacement pack.toml import
// first), the wave-1 maintenance workspace include is removed (its
// content was folded into the bundled core pack, so removal IS the
// migration), builtin-named rig includes and default-rig includes become
// pinned bundled imports on the same scope, and builtin-named city / rig
// / default-rig import sources are rewritten to the canonical bundled
// source at the canonical pin. A converted include never narrows the
// composed pack set: when its natural binding is occupied by a different
// import, the bundled import lands under a fresh unique binding, and only
// an existing import of the same canonical source makes the include a
// duplicate route that needs no new import. Wave-1 public-pack import
// sources (gastown/maintenance) are left to the packv2-import-state
// check, and non-builtin references — including workspace includes of
// packs under the retired tree with no canonical bundled import — are
// kept in place for the manual migration Run instructs, so composition
// and the prune gate keep seeing them. Reports whether the manifest
// changed.
func migrateLegacySystemPacksManifest(cityPath string, manifest *config.City) (bool, error) {
	changed := rewriteLegacyBundledImportSources(cityPath, manifest.Imports)

	// Rewrite builtin-named legacy import sources before converting any
	// include: a converted include dedups against existing bindings by
	// canonical source, so an import that holds the same pack in its
	// legacy spelling must reach the canonical one first.

	if rewriteLegacyBundledImportSources(cityPath, manifest.Defaults.Rig.Imports) {
		changed = true
	}

	includes := manifest.Workspace.LegacyIncludes()
	kept := make([]string, 0, len(includes))
	for _, inc := range includes {
		if !legacySystemPacksInclude(cityPath, inc) {
			kept = append(kept, inc)
			continue
		}
		if _, _, ok := bundledImportForLegacySystemPacksRef(cityPath, inc); ok {
			changed = true
			continue
		}
		if _, wave1 := legacyPublicPackForSource(cityPath, inc); wave1 {
			changed = true
			continue
		}
		kept = append(kept, inc)
	}
	if len(kept) != len(includes) {
		manifest.Workspace.SetLegacyIncludes(kept)
	}

	defaultIncludes := manifest.Workspace.LegacyDefaultRigIncludes()
	keptDefaults := make([]string, 0, len(defaultIncludes))
	for _, inc := range defaultIncludes {
		imp, name, ok := bundledImportForLegacySystemPacksRef(cityPath, inc)
		if !ok {
			keptDefaults = append(keptDefaults, inc)
			continue
		}
		imports, err := ensureBundledImportBinding(manifest.Defaults.Rig.Imports, name, imp)
		if err != nil {
			return false, fmt.Errorf("converting legacy default-rig include %q: %w", inc, err)
		}
		manifest.Defaults.Rig.Imports = imports
		changed = true
	}
	if len(keptDefaults) != len(defaultIncludes) {
		manifest.Workspace.SetLegacyDefaultRigIncludes(keptDefaults)
	}

	for i := range manifest.Rigs {
		rig := &manifest.Rigs[i]
		if rewriteLegacyBundledImportSources(cityPath, rig.Imports) {
			changed = true
		}
		label := strings.TrimSpace(rig.Name)
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		keptRigIncludes := make([]string, 0, len(rig.Includes))
		for _, inc := range rig.Includes {
			imp, name, ok := bundledImportForLegacySystemPacksRef(cityPath, inc)
			if !ok {
				keptRigIncludes = append(keptRigIncludes, inc)
				continue
			}
			imports, err := ensureBundledImportBinding(rig.Imports, name, imp)
			if err != nil {
				return false, fmt.Errorf("converting legacy include %q in rig %s: %w", inc, label, err)
			}
			rig.Imports = imports
			changed = true
		}
		if len(keptRigIncludes) != len(rig.Includes) {
			rig.Includes = keptRigIncludes
		}
	}
	return changed, nil
}

// ensureBundledImportBinding lands a converted legacy include's bundled
// import in a scope's import map without narrowing the composed pack set.
// An existing binding stands in for the converted import — making the
// include a duplicate composition route that needs no new import — only
// when it imports the same canonical source with the default option
// semantics composition's own reuse policy requires
// (config.Import.HasDefaultOptionSemantics): an exported, non-transitive,
// or shadow-silenced binding composes something narrower or different, so
// the bundled import still lands beside it. The version policy is
// deliberately looser than composition's versionless-only reuse: an
// explicit user pin on the canonical source is respected as the duplicate
// route (packs.lock keys entries by source, so the same source must never
// land at a second pin), and a versionless binding gains the canonical
// pin in place — the same healing rewriteLegacyBundledImportSources
// applies to legacy-spelled sources — because a conversion must never
// leave a bundled source unpinned. Otherwise the import lands on its
// natural binding, or on a fresh unique one when a different import
// occupies it, mirroring the uniquification policy normal composition
// applies to colliding legacy includes (config.AddOrderedLegacyImports).
//
// One corner is refused instead of landed: a same-source binding with
// non-default option semantics pinned at a different version. The
// non-default options mean it cannot stand in for the converted import,
// but landing the bundled pin beside it would put one source at two pins
// — a state packs.lock cannot hold (mergeConstraints rejects it on every
// later sync) — so the conversion surfaces the conflict before mutating
// anything. A pinned same-source binding with DEFAULT semantics still
// wins over the conflict: nothing new lands, so no second pin is created
// by this conversion.
func ensureBundledImportBinding(imports map[string]config.Import, name string, imp config.Import) (map[string]config.Import, error) {
	pinInPlace := ""
	conflict := ""
	for binding, existing := range imports {
		if existing.Source != imp.Source {
			continue
		}
		if !existing.HasDefaultOptionSemantics() {
			if v := strings.TrimSpace(existing.Version); v != "" && v != strings.TrimSpace(imp.Version) {
				if conflict == "" || binding < conflict {
					conflict = binding
				}
			}
			continue
		}
		if strings.TrimSpace(existing.Version) != "" {
			return imports, nil
		}
		if pinInPlace == "" || binding < pinInPlace {
			pinInPlace = binding
		}
	}
	if conflict != "" {
		return imports, fmt.Errorf("import %q pins %s at %q with non-default options, and the bundled %q import must land the same source at %q; packs.lock cannot hold one source at two pins — align import %q with the bundled pin or remove the legacy reference by hand, then re-run \"gc doctor --fix\"", conflict, imp.Source, imports[conflict].Version, name, imp.Version, conflict)
	}
	if pinInPlace != "" {
		existing := imports[pinInPlace]
		existing.Version = imp.Version
		imports[pinInPlace] = existing
		return imports, nil
	}
	if imports == nil {
		imports = make(map[string]config.Import, 1)
	}
	imports[config.UniqueLegacyImportBinding(imports, name)] = imp
	return imports, nil
}

// bundledImportForLegacySystemPacksRef maps a legacy reference under the
// retired tree to the named pack's canonical bundled import at the
// canonical pin. ok is false for non-legacy references, nested paths,
// and packs without a canonical bundled source.
func bundledImportForLegacySystemPacksRef(cityPath, ref string) (config.Import, string, bool) {
	name := legacySystemPacksPackName(cityPath, ref)
	if name == "" {
		return config.Import{}, "", false
	}
	source, ok := builtinpacks.CanonicalImportSource(name)
	if !ok {
		return config.Import{}, "", false
	}
	return config.Import{Source: source, Version: bundledSourcePinnedVersion(source)}, name, true
}

// rewriteLegacyBundledImportSources rewrites import entries whose source
// points under the retired tree at a builtin pack to the canonical
// bundled source and pin, preserving the binding. Wave-1 public-pack
// sources are skipped — the packv2-import-state check owns their
// migration (including the maintenance removal semantics).
func rewriteLegacyBundledImportSources(cityPath string, imports map[string]config.Import) bool {
	changed := false
	for binding, imp := range imports {
		if _, wave1 := legacyPublicPackForSource(cityPath, imp.Source); wave1 {
			continue
		}
		rewritten, _, ok := bundledImportForLegacySystemPacksRef(cityPath, imp.Source)
		if !ok {
			continue
		}
		// Migrate only the source location and pin. The existing import's
		// load-bearing composition options (export, transitive, shadow) are
		// preserved so "gc doctor --fix" never silently changes composition
		// behavior while relocating a legacy source to its bundled equivalent.
		imp.Source = rewritten.Source
		imp.Version = rewritten.Version
		imports[binding] = imp
		changed = true
	}
	return changed
}

// maskLegacySystemPacksRoutes returns a copy of cfg with every
// composition route through the retired .gc/system/packs tree removed, so
// reachability reflects the post-migration config. Only the surfaces
// ReachablePackNames consults are masked; cfg itself is not modified.
func maskLegacySystemPacksRoutes(cfg *config.City, cityPath string) *config.City {
	masked := *cfg
	masked.Workspace.SetLegacyIncludes(withoutLegacySystemPacksRefs(cityPath, cfg.Workspace.LegacyIncludes()))
	masked.Workspace.SetLegacyDefaultRigIncludes(withoutLegacySystemPacksRefs(cityPath, cfg.Workspace.LegacyDefaultRigIncludes()))
	masked.Imports = withoutLegacySystemPacksImports(cityPath, cfg.Imports)
	masked.DefaultRigImports = withoutLegacySystemPacksImports(cityPath, cfg.DefaultRigImports)
	if len(cfg.Rigs) > 0 {
		masked.Rigs = append([]config.Rig(nil), cfg.Rigs...)
		for i := range masked.Rigs {
			masked.Rigs[i].Includes = withoutLegacySystemPacksRefs(cityPath, masked.Rigs[i].Includes)
			masked.Rigs[i].Imports = withoutLegacySystemPacksImports(cityPath, masked.Rigs[i].Imports)
		}
	}
	return &masked
}

func withoutLegacySystemPacksRefs(cityPath string, refs []string) []string {
	kept := make([]string, 0, len(refs))
	for _, ref := range refs {
		if legacySystemPacksInclude(cityPath, ref) {
			continue
		}
		kept = append(kept, ref)
	}
	return kept
}

func withoutLegacySystemPacksImports(cityPath string, imports map[string]config.Import) map[string]config.Import {
	if len(imports) == 0 {
		return imports
	}
	kept := make(map[string]config.Import, len(imports))
	for binding, imp := range imports {
		if legacySystemPacksInclude(cityPath, imp.Source) {
			continue
		}
		kept[binding] = imp
	}
	return kept
}
