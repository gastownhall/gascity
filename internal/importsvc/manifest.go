// Package importsvc holds the shared orchestration for adding and removing
// pack imports. It is the single code path behind both the `gc import add` /
// `gc import remove` CLI commands and the supervisor HTTP handlers, so it
// operates on an injected [fsys.FS] plus a city path and carries no cobra,
// io.Writer, or working-directory coupling. Callers map the typed errors it
// returns to whatever surface they speak (exit codes or HTTP status).
//
// KNOWN DUPLICATION (follow-up to converge): the manifest/scope helpers below
// — loadCityPackManifest, writeCityPackManifest, loadImportScope,
// CollectAllImports and their support funcs — were lifted verbatim from the
// unimportable package-main copies in cmd/gc (cmd_import.go still keeps
// loadCityPackManifestFS/collectAllImportsFS/loadImportScopeFS, shared with the
// other gc import subcommands). The two copies must stay byte-equivalent in
// behavior; a divergence in pack.toml round-trip rules would silently desync
// the CLI and the HTTP path. The intended end state is for cmd/gc to delegate
// these reads to importsvc too; until then, treat any edit here as needing the
// mirror edit in cmd_import.go (and vice versa).
package importsvc

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/pricing"
)

// Seam variables wrap the packman entry points the orchestration drives. They
// are package vars so importsvc's own tests can stub the network- and
// git-touching steps; the CLI injects its own stubbable copies through [Deps]
// so the existing command tests keep working unchanged.
var (
	syncLock          = packman.SyncLock
	writeLockfile     = packman.WriteLockfile
	resolveVersion    = packman.ResolveVersion
	defaultConstraint = packman.DefaultConstraint
	resolveHeadCommit = defaultHeadCommit
)

const cityPackSchema = 1

type cityPackManifest struct {
	Pack                  config.PackMeta                `toml:"pack"`
	Imports               map[string]config.Import       `toml:"imports,omitempty"`
	AgentDefaults         config.AgentDefaults           `toml:"agent_defaults,omitempty"`
	AgentsDefaults        config.AgentDefaults           `toml:"agents,omitempty" jsonschema:"-"`
	Defaults              cityPackDefaults               `toml:"defaults,omitempty"`
	DefaultRigImportOrder []string                       `toml:"-"`
	Agents                []config.Agent                 `toml:"agent,omitempty"`
	NamedSessions         []config.NamedSession          `toml:"named_session,omitempty"`
	Services              []config.Service               `toml:"service,omitempty"`
	Providers             map[string]config.ProviderSpec `toml:"providers,omitempty"`
	Upstreams             map[string]config.UpstreamSpec `toml:"upstreams,omitempty"`
	Formulas              config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches               config.Patches                 `toml:"patches,omitempty"`
	Doctor                []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands              []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global                config.PackGlobal              `toml:"global,omitempty"`
	Pricing               []pricing.ModelPricing         `toml:"pricing,omitempty"`
}

type cityPackDefaults struct {
	Rig cityPackRigDefaults `toml:"rig,omitempty"`
}

type cityPackRigDefaults struct {
	Imports map[string]config.Import `toml:"imports,omitempty"`
}

type cityPackManifestBody struct {
	Pack          config.PackMeta                `toml:"pack"`
	Imports       map[string]config.Import       `toml:"imports,omitempty"`
	AgentDefaults config.AgentDefaults           `toml:"agent_defaults,omitempty"`
	Agents        []config.Agent                 `toml:"agent,omitempty"`
	NamedSessions []config.NamedSession          `toml:"named_session,omitempty"`
	Services      []config.Service               `toml:"service,omitempty"`
	Providers     map[string]config.ProviderSpec `toml:"providers,omitempty"`
	Upstreams     map[string]config.UpstreamSpec `toml:"upstreams,omitempty"`
	Formulas      config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches       config.Patches                 `toml:"patches,omitempty"`
	Doctor        []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands      []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global        config.PackGlobal              `toml:"global,omitempty"`
	Pricing       []pricing.ModelPricing         `toml:"pricing,omitempty"`
}

// importScopeState captures the imports table being edited (root pack.toml or a
// rig in city.toml), the synthetic-key prefix used to address it in the merged
// import graph, and a save closure that writes the mutated table back.
type importScopeState struct {
	imports      map[string]config.Import
	syntheticTag string
	save         func() error
}

func (s *importScopeState) syntheticKey(name string) string {
	return s.syntheticTag + name
}

func (s *importScopeState) isRootPackScope() bool {
	return s != nil && s.syntheticTag == "pack:"
}

// loadImportScope loads the writable import table for the requested scope. When
// rig is empty the root pack.toml [imports] table is edited; otherwise the
// named rig's [rigs.imports] table inside city.toml is edited.
func loadImportScope(fs fsys.FS, cityPath, rig string) (*importScopeState, error) {
	targetRig := strings.TrimSpace(rig)
	if targetRig == "" {
		manifest, err := loadCityPackManifest(fs, cityPath)
		if err != nil {
			return nil, err
		}
		if manifest.Imports == nil {
			manifest.Imports = make(map[string]config.Import)
		}
		return &importScopeState{
			imports:      manifest.Imports,
			syntheticTag: "pack:",
			save: func() error {
				return writeCityPackManifest(fs, cityPath, manifest)
			},
		}, nil
	}

	if _, err := fs.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("rig-scoped imports require a city directory: %s", cityPath)
		}
		return nil, err
	}

	cfg, err := loadCityImportManifest(fs, cityPath)
	if err != nil {
		return nil, err
	}
	rigIndex, rigName, err := findImportRigIndex(cityPath, cfg.Rigs, targetRig)
	if err != nil {
		return nil, err
	}
	if cfg.Rigs[rigIndex].Imports == nil {
		cfg.Rigs[rigIndex].Imports = make(map[string]config.Import)
	}
	return &importScopeState{
		imports:      cfg.Rigs[rigIndex].Imports,
		syntheticTag: "rig:" + rigName + ":",
		save: func() error {
			return writeCityImportManifest(fs, cityPath, cfg)
		},
	}, nil
}

// CollectAllImports returns the full effective import graph keyed by synthetic
// scope tags ("pack:<name>", "default-rig:<binding>", "rig:<rig>:<name>"). It
// is exported for callers (e.g. a future list/GET handler) that need the same
// merged view the add/remove sync uses.
func CollectAllImports(fs fsys.FS, cityPath string) (map[string]config.Import, error) {
	all := make(map[string]config.Import)

	packManifest, err := loadCityPackManifest(fs, cityPath)
	if err != nil {
		return nil, err
	}
	rootImports := copyImports(packManifest.Imports)
	if err := applyCityRootImportOverrides(fs, cityPath, rootImports); err != nil {
		return nil, err
	}
	for name, imp := range rootImports {
		all["pack:"+name] = imp
	}
	defaults, err := config.LoadRootPackDefaultRigImports(fs, cityPath)
	if err != nil {
		return nil, err
	}
	for _, bound := range defaults {
		all["default-rig:"+bound.Binding] = bound.Import
	}

	if _, err := fs.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			return all, nil
		}
		return nil, err
	}

	cfg, err := loadCityImportManifest(fs, cityPath)
	if err != nil {
		return nil, err
	}
	for _, rig := range cfg.Rigs {
		for name, imp := range rig.Imports {
			all["rig:"+rig.Name+":"+name] = imp
		}
	}
	return all, nil
}

func copyImports(imports map[string]config.Import) map[string]config.Import {
	out := make(map[string]config.Import, len(imports))
	for name, imp := range imports {
		out[name] = imp
	}
	return out
}

func applyCityRootImportOverrides(fs fsys.FS, cityPath string, imports map[string]config.Import) error {
	overrides, err := loadCityRootImports(fs, cityPath)
	if err != nil {
		return err
	}
	for name, imp := range overrides {
		imports[name] = imp
	}
	return nil
}

// loadCityRootImports returns the root-level [imports] entries from city.toml,
// or nil when no city.toml exists.
func loadCityRootImports(fs fsys.FS, cityPath string) (map[string]config.Import, error) {
	if _, err := fs.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cfg, err := loadCityImportManifest(fs, cityPath)
	if err != nil {
		return nil, err
	}
	return cfg.Imports, nil
}

// cityRootImportExists reports whether city.toml's root [imports] table defines
// name. City entries own the effective root import wholesale, so the
// add/remove write paths must consult this before mutating pack.toml.
func cityRootImportExists(fs fsys.FS, cityPath, name string) (bool, error) {
	overrides, err := loadCityRootImports(fs, cityPath)
	if err != nil {
		return false, err
	}
	_, ok := overrides[name]
	return ok, nil
}

func loadCityImportManifest(fs fsys.FS, cityPath string) (*config.City, error) {
	return loadCityConfigForEdit(fs, filepath.Join(cityPath, "city.toml"))
}

func writeCityImportManifest(fs fsys.FS, cityPath string, cfg *config.City) error {
	if cfg == nil {
		cfg = &config.City{}
	}
	return writeCityConfigForEdit(fs, filepath.Join(cityPath, "city.toml"), cfg)
}

// loadCityConfigForEdit loads the raw city config WITHOUT pack/include
// expansion, preserving include directives, pack references, and patches for a
// faithful round-trip on rewrite.
func loadCityConfigForEdit(fs fsys.FS, tomlPath string) (*config.City, error) {
	cfg, err := config.Load(fs, tomlPath)
	if err != nil {
		return nil, err
	}
	if _, err := config.ApplySiteBindingsForEdit(fs, filepath.Dir(tomlPath), cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func writeCityConfigForEdit(fs fsys.FS, tomlPath string, cfg *config.City) error {
	return config.WriteCityAndRigSiteBindingsForEdit(fs, tomlPath, cfg)
}

func findImportRigIndex(cityPath string, rigs []config.Rig, target string) (int, string, error) {
	for i, rig := range rigs {
		if rig.Name == target {
			return i, rig.Name, nil
		}
	}

	resolvedRigs := append([]config.Rig(nil), rigs...)
	resolveRigPaths(cityPath, resolvedRigs)

	targetPath := target
	if !filepath.IsAbs(targetPath) {
		abs, err := filepath.Abs(filepath.Join(cityPath, targetPath))
		if err == nil {
			targetPath = abs
		}
	}
	for i, rig := range resolvedRigs {
		if pathutil.SamePath(rig.Path, targetPath) {
			return i, rigs[i].Name, nil
		}
	}

	return -1, "", fmt.Errorf("rig %q not found", target)
}

// resolveRigPaths resolves relative rig paths to absolute (relative to
// cityPath), mutating rigs in place.
func resolveRigPaths(cityPath string, rigs []config.Rig) {
	for i := range rigs {
		if strings.TrimSpace(rigs[i].Path) == "" {
			continue
		}
		if !filepath.IsAbs(rigs[i].Path) {
			rigs[i].Path = filepath.Join(cityPath, rigs[i].Path)
		}
	}
}

func loadCityPackManifest(fs fsys.FS, cityPath string) (*cityPackManifest, error) {
	path := filepath.Join(cityPath, "pack.toml")
	data, err := fs.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		manifest := &cityPackManifest{
			Pack: config.PackMeta{
				Name:   defaultCityPackName(fs, cityPath),
				Schema: cityPackSchema,
			},
			Imports: make(map[string]config.Import),
		}
		return manifest, nil
	}

	var manifest cityPackManifest
	md, err := toml.Decode(string(data), &manifest)
	if err != nil {
		return nil, fmt.Errorf("parsing pack.toml: %w", err)
	}
	// Fold the legacy [agents] alias into [agent_defaults] before any rewrite:
	// the manifest body emits only [agent_defaults], so without this the
	// import-manifest rewrite would silently drop an [agents] table even though
	// the key-loss guard recognizes it. Mirrors parse-time normalization.
	config.FoldAgentDefaultsAlias(&manifest.AgentDefaults, manifest.AgentsDefaults, md)
	manifest.AgentsDefaults = config.AgentDefaults{}
	if manifest.Pack.Name == "" {
		manifest.Pack.Name = defaultCityPackName(fs, cityPath)
	}
	if manifest.Pack.Schema == 0 {
		manifest.Pack.Schema = cityPackSchema
	}
	if manifest.Imports == nil {
		manifest.Imports = make(map[string]config.Import)
	}
	if len(manifest.Defaults.Rig.Imports) > 0 {
		ordered, err := config.LoadRootPackDefaultRigImports(fs, cityPath)
		if err != nil {
			return nil, err
		}
		manifest.DefaultRigImportOrder = make([]string, 0, len(ordered))
		for _, bound := range ordered {
			manifest.DefaultRigImportOrder = append(manifest.DefaultRigImportOrder, bound.Binding)
		}
	}
	return &manifest, nil
}

func writeCityPackManifest(fs fsys.FS, cityPath string, manifest *cityPackManifest) error {
	if manifest == nil {
		manifest = &cityPackManifest{}
	}
	if manifest.Pack.Name == "" {
		manifest.Pack.Name = defaultCityPackName(fs, cityPath)
	}
	if manifest.Pack.Schema == 0 {
		manifest.Pack.Schema = cityPackSchema
	}
	if manifest.Imports == nil {
		manifest.Imports = make(map[string]config.Import)
	}

	var buf bytes.Buffer
	body := cityPackManifestBody{
		Pack:          manifest.Pack,
		Imports:       manifest.Imports,
		AgentDefaults: manifest.AgentDefaults,
		Agents:        manifest.Agents,
		NamedSessions: manifest.NamedSessions,
		Services:      manifest.Services,
		Providers:     manifest.Providers,
		Upstreams:     manifest.Upstreams,
		Formulas:      manifest.Formulas,
		Patches:       manifest.Patches,
		Doctor:        manifest.Doctor,
		Commands:      manifest.Commands,
		Global:        manifest.Global,
		Pricing:       manifest.Pricing,
	}
	if err := toml.NewEncoder(&buf).Encode(body); err != nil {
		return fmt.Errorf("encoding pack.toml: %w", err)
	}
	if err := writeOrderedDefaultRigImports(&buf, manifest); err != nil {
		return err
	}
	// Resolve before the rename: pack.toml may be a symlink into a checked-out
	// repo, and renaming over the unresolved path would replace the link with a
	// regular file and strand the stale manifest in the checked-in target.
	writePath, err := fsys.ResolveSymlinks(fs, filepath.Join(cityPath, "pack.toml"))
	if err != nil {
		return err
	}
	// Refuse the rewrite when the on-disk pack.toml carries keys this binary
	// does not recognize: the manifest round-trip would silently drop newer or
	// manual keys at the checked-in target.
	if err := config.GuardRewriteKeyLoss[cityPackManifest](fs, writePath); err != nil {
		return err
	}
	return fsys.WriteFileAtomic(fs, writePath, buf.Bytes(), 0o644)
}

func writeOrderedDefaultRigImports(buf *bytes.Buffer, manifest *cityPackManifest) error {
	if manifest == nil || len(manifest.Defaults.Rig.Imports) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(manifest.Defaults.Rig.Imports))
	names := make([]string, 0, len(manifest.Defaults.Rig.Imports))
	for _, name := range manifest.DefaultRigImportOrder {
		if _, ok := manifest.Defaults.Rig.Imports[name]; ok && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	var remaining []string
	for name := range manifest.Defaults.Rig.Imports {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	names = append(names, remaining...)

	for _, name := range names {
		imp := manifest.Defaults.Rig.Imports[name]
		fmt.Fprintf(buf, "\n[defaults.rig.imports.%s]\n", strconv.Quote(name)) //nolint:errcheck
		if err := toml.NewEncoder(buf).Encode(imp); err != nil {
			return fmt.Errorf("encoding defaults.rig.imports.%s: %w", name, err)
		}
	}
	return nil
}

func defaultCityPackName(fs fsys.FS, cityPath string) string {
	cfg, err := config.Load(fs, filepath.Join(cityPath, "city.toml"))
	if err == nil {
		return config.EffectiveCityName(cfg, filepath.Base(cityPath))
	}
	return filepath.Base(cityPath)
}
