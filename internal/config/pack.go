package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	iofs "io/fs"
	"log"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/pricing"
)

// packFile is the expected filename inside a pack directory.
const packFile = "pack.toml"

// currentPackSchema is the supported pack schema version.
const currentPackSchema = 2

type deferredRigPatches struct {
	rigName            string
	agentStart         int
	agentEnd           int
	expectedAgentCount int
	expectedAgentNames []string
	overrides          []AgentOverride
}

// PackConfig is the TOML structure of a pack.toml file. Agent
// definitions are discovered from agents/<name>/agent.toml; the inline agent
// list remains schema-visible for migration compatibility with legacy packs.
type PackConfig struct {
	Pack           PackMeta          `toml:"pack" jsonschema:"required"`
	Imports        map[string]Import `toml:"imports,omitempty"`
	AgentDefaults  AgentDefaults     `toml:"agent_defaults,omitempty"`
	AgentsDefaults AgentDefaults     `toml:"agents,omitempty" jsonschema:"-"`
	Defaults       PackDefaults      `toml:"defaults,omitempty" jsonschema:"-"`
	// Agents holds legacy inline agent templates accepted by the current
	// loader. New packs should define agents under
	// agents/<name>/agent.toml instead.
	Agents        []Agent                     `toml:"agent,omitempty"`
	NamedSessions []NamedSession              `toml:"named_session,omitempty"`
	Services      []Service                   `toml:"service,omitempty"`
	Webhooks      []Webhook                   `toml:"webhook,omitempty"`
	Providers     map[string]ProviderSpec     `toml:"providers,omitempty"`
	Upstreams     map[string]UpstreamSpec     `toml:"upstreams,omitempty"`
	Runtimes      map[string]PackRuntimeEntry `toml:"runtimes,omitempty"`
	Formulas      FormulasConfig              `toml:"formulas,omitempty" jsonschema:"-"`
	Patches       PackPatches                 `toml:"patches,omitempty"`
	Doctor        []PackDoctorEntry           `toml:"doctor,omitempty"`
	Commands      []PackCommandEntry          `toml:"commands,omitempty"`
	Global        PackGlobal                  `toml:"global,omitempty"`
	Pricing       []pricing.ModelPricing      `toml:"pricing,omitempty"`
}

// PackPatches holds the patch operations valid in pack.toml. City
// configuration may patch agents, rigs, and providers; packs may only patch
// agents visible within that pack load.
type PackPatches struct {
	Agents []AgentPatch `toml:"agent,omitempty"`
}

// IsEmpty reports whether the pack declares no supported patch entries.
func (p *PackPatches) IsEmpty() bool {
	return p == nil || len(p.Agents) == 0
}

// PackDefaults holds [defaults] entries used to seed generated rig
// configuration.
type PackDefaults struct {
	Rig PackRigDefaults `toml:"rig,omitempty"`
}

// PackRigDefaults holds the [defaults.rig] block — defaults applied
// to rigs created from this pack.
type PackRigDefaults struct {
	Imports map[string]Import `toml:"imports,omitempty"`
}

// ExpandPacks resolves pack references on all rigs. For each rig
// with pack fields set (V1 includes or V2 [rigs.imports.X]), it loads
// the pack directories, stamps agents with dir = rig.Name and
// BindingName from imports, resolves paths relative to the pack
// directory, and appends the agents to the city config.
//
// Overrides from the rig are applied to the stamped agents (after all
// packs for the rig are expanded). All expansion happens before
// validation — downstream sees a flat City struct.
// ExpandPacks applies those rig overrides inline. It does not coordinate
// ordering with city-level ApplyPatches; use LoadWithIncludes for full
// city composition where city-level patches run before rig overrides.
//
// rigFormulaDirs is populated with per-rig pack formula directories
// (Layer 3). cityRoot is the city directory (parent of city.toml), used
// for path resolution.
func ExpandPacks(cfg *City, fs fsys.FS, cityRoot string, rigFormulaDirs map[string][]string) error {
	return expandPacks(cfg, fs, cityRoot, rigFormulaDirs, LoadOptions{})
}

func expandPacks(cfg *City, fs fsys.FS, cityRoot string, rigFormulaDirs map[string][]string, opts LoadOptions) error {
	var expanded []Agent
	// City-scoped agents and named sessions encountered through a rig-scope
	// include/import are hoisted to city scope (deduped) rather than dropped,
	// so a city-scoped agent that lives in a rig-included pack (e.g. a routing
	// coordinator in a pack only ever rig-included) still registers. Collected
	// here across all rigs and merged into cfg once below.
	var hoistedAgents []Agent
	var hoistedNamedSessions []NamedSession
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		cache := &packLoadCache{results: make(map[string]*packLoadResult)}
		topoRefs := rig.Includes
		if len(topoRefs) == 0 && len(rig.Imports) == 0 {
			// When a rig has only a path (no explicit includes/imports), treat
			// the path directory itself as an implicit include if it contains a
			// pack.toml. This supports the schema-2 convention where a rig root
			// can carry a pack.toml with agents/ directories.
			if p := strings.TrimSpace(rig.Path); p != "" {
				packPath := p
				if !filepath.IsAbs(packPath) {
					packPath = filepath.Join(cityRoot, packPath)
				}
				if _, sErr := fs.Stat(filepath.Join(packPath, packFile)); sErr == nil {
					topoRefs = []string{packPath}
				}
			}
			if len(topoRefs) == 0 {
				continue
			}
		}

		var rigAgents []Agent
		var rigNamedSessions []NamedSession
		var rigTopoDirs []string
		var rigPackGraphOnlyDirs []string
		var rigImportPackDirs []string
		var rigGlobals []ResolvedPackGlobal
		for _, ref := range topoRefs {
			topoDir, err := resolvePackRef(ref, cityRoot, cityRoot)
			if err != nil {
				return fmt.Errorf("rig %q pack %q: %w", rig.Name, ref, err)
			}
			topoPath := filepath.Join(topoDir, packFile)

			// Skip remote packs whose subpath was deleted upstream.
			if isRemoteRef(ref) {
				if _, sErr := fs.Stat(topoPath); sErr != nil {
					log.Printf("rig %q pack %q: not found, skipping: %v", rig.Name, ref, sErr)
					continue
				}
			}

			agents, namedSessions, providers, upstreams, services, topoDirs, reqs, globals, err := loadPackWithCacheOptions(fs, topoPath, topoDir, cityRoot, rig.Name, nil, cache, opts)
			if err != nil {
				return fmt.Errorf("rig %q pack %q: %w", rig.Name, ref, err)
			}
			cfg.LoadWarnings = appendUnique(cfg.LoadWarnings, cachedPackWarnings(cache, topoDir)...)
			if len(services) > 0 {
				return fmt.Errorf("rig %q pack %q: [[service]] is only allowed in city-scoped packs", rig.Name, ref)
			}
			rigGlobals = append(rigGlobals, globals...)
			packName := tcPackName(fs, topoPath)
			cfg.PackCommands = appendDiscoveredCommands(
				cfg.PackCommands,
				stampDefaultBinding(cachedPackCommands(cache, topoDir), packName)...,
			)
			cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, cachedPackDoctors(cache, topoDir)...)
			// Runtime selection is city-wide, so rig pack runtimes
			// register into the same namespace as city-level ones.
			if err := mergeCityRuntimes(cfg, cachedPackRuntimes(cache, topoDir)); err != nil {
				return fmt.Errorf("rig %q pack %q: %w", rig.Name, ref, err)
			}
			skills := cachedPackSkills(cache, topoDir)
			if packName == "" && len(skills) > 0 {
				return fmt.Errorf("rig %q pack %q: discovered skills require [pack].name for binding", rig.Name, ref)
			}
			if cfg.RigPackSkills == nil {
				cfg.RigPackSkills = make(map[string][]DiscoveredSkillCatalog)
			}
			cfg.RigPackSkills[rig.Name] = appendDiscoveredSkills(
				cfg.RigPackSkills[rig.Name],
				stampSkillBinding(skills, packName)...,
			)

			// Validate rig-scoped requirements.
			for _, req := range reqs {
				if req.Scope != "rig" {
					continue
				}
				found := false
				for _, a := range agents {
					if a.Name == req.Agent {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("rig %q: pack requires rig agent %q — include a pack that provides it", rig.Name, req.Agent)
				}
			}

			// Accumulate pack dirs for this rig.
			rigTopoDirs = appendUnique(rigTopoDirs, topoDirs...)
			rigPackGraphOnlyDirs = appendUniqueLastWins(rigPackGraphOnlyDirs, topoDirs...)

			// Keep only rig-scoped and unscoped agents for rig expansion;
			// hoist city-scoped ones to city scope instead of dropping them.
			hoistedAgents = append(hoistedAgents, hoistCityScopedAgents(agents)...)
			hoistedNamedSessions = append(hoistedNamedSessions, hoistCityScopedNamedSessions(namedSessions)...)
			agents = filterAgentsByScope(agents, false)
			namedSessions = filterNamedSessionsByScope(namedSessions, false)

			// Record rig pack formula dirs (Layer 3) — derive from topoDirs.
			if rigFormulaDirs != nil {
				for _, td := range topoDirs {
					fd := filepath.Join(td, "formulas")
					if _, sErr := fs.Stat(fd); sErr == nil {
						rigFormulaDirs[rig.Name] = append(rigFormulaDirs[rig.Name], fd)
					}
				}
			}

			rigAgents = append(rigAgents, agents...)
			rigNamedSessions = append(rigNamedSessions, namedSessions...)

			// Merge pack providers into city (additive, no overwrite).
			if len(providers) > 0 {
				if cfg.Providers == nil {
					cfg.Providers = make(map[string]ProviderSpec)
				}
				for name, spec := range providers {
					if _, exists := cfg.Providers[name]; !exists {
						cfg.Providers[name] = spec
					}
				}
			}

			// Merge pack upstreams into city (additive, no overwrite).
			if len(upstreams) > 0 {
				if cfg.Upstreams == nil {
					cfg.Upstreams = make(map[string]UpstreamSpec)
				}
				for name, spec := range upstreams {
					if _, exists := cfg.Upstreams[name]; !exists {
						cfg.Upstreams[name] = spec
					}
				}
			}
		}

		// Process rig-level [imports.X] entries (V2).
		if len(rig.Imports) > 0 {
			importNames := make([]string, 0, len(rig.Imports))
			for name := range rig.Imports {
				importNames = append(importNames, name)
			}
			sort.Strings(importNames)

			for _, bindingName := range importNames {
				imp := rig.Imports[bindingName]
				if !isOSFileSystem(fs) && builtinpacks.IsSource(imp.Source) {
					continue
				}

				impDir, err := resolveImportPackRef(imp.Source, imp.Version, cityRoot, cityRoot)
				if err != nil {
					return fmt.Errorf("rig %q import %q: %w", rig.Name, bindingName, err)
				}

				impPath := filepath.Join(impDir, packFile)
				agents, namedSessions, providers, upstreams, services, topoDirs, reqs, globals, err := loadPackWithCacheOptions(
					fs, impPath, impDir, cityRoot, rig.Name, nil, cache, opts)
				if err != nil {
					return fmt.Errorf("rig %q import %q: %w", rig.Name, bindingName, err)
				}
				warnings := cachedPackWarnings(cache, impDir)
				commands := cachedPackCommands(cache, impDir)
				doctors := cachedPackDoctors(cache, impDir)
				runtimes := cachedPackRuntimes(cache, impDir)
				skills := cachedPackSkills(cache, impDir)
				if !imp.ImportIsTransitive() {
					warnings = cachedPackLocalWarnings(cache, impDir)
					absImpDir, _ := filepath.Abs(impDir)
					var direct []Agent
					for _, a := range agents {
						absSrc, _ := filepath.Abs(a.SourceDir)
						if absSrc == absImpDir {
							direct = append(direct, a)
						}
					}
					agents = direct
					namedSessions = filterNamedSessionsBySourceDir(namedSessions, impDir)
					services = filterServicesBySourceDir(services, impDir)
					commands = filterCommandsByPackDir(commands, impDir)
					doctors = filterDoctorsByPackDir(doctors, impDir)
					runtimes = filterRuntimesByPackDir(runtimes, impDir)
					providers = cachedPackLocalProviders(cache, impDir)
					upstreams = cachedPackLocalUpstreams(cache, impDir)
					topoDirs = cachedPackLocalTopoDirs(cache, impDir)
					reqs = cachedPackLocalRequires(cache, impDir)
					globals = cachedPackLocalGlobals(cache, impDir)
					skills = filterSkillsByPackDir(skills, impDir)
				}
				cfg.LoadWarnings = appendUnique(cfg.LoadWarnings, warnings...)
				if len(services) > 0 {
					return fmt.Errorf("rig %q import %q: [[service]] is only allowed in city-scoped packs", rig.Name, bindingName)
				}
				rigGlobals = append(rigGlobals, globals...)
				if cfg.RigPackSkills == nil {
					cfg.RigPackSkills = make(map[string][]DiscoveredSkillCatalog)
				}
				cfg.RigPackSkills[rig.Name] = appendDiscoveredSkills(
					cfg.RigPackSkills[rig.Name],
					stampImportedSkillBinding(skills, bindingName, imp.Export)...,
				)
				mcpTopoDirs := topoDirs

				if !imp.ImportIsTransitive() {
					mcpTopoDirs = filterPackDirsByRoot(topoDirs, impDir)
				}
				if cfg.RigImportMCPBindings == nil {
					cfg.RigImportMCPBindings = make(map[string]map[string]string)
				}
				cfg.RigImportMCPBindings[rig.Name] = stampMCPDirBindings(cfg.RigImportMCPBindings[rig.Name], mcpTopoDirs, bindingName)

				// Stamp binding name on agents and named sessions.
				// At the rig level, ALL agents from an import get the rig's
				// binding — nested bindings are overridden.
				for i := range agents {
					agents[i].BindingName = bindingName
				}
				for i := range namedSessions {
					namedSessions[i].BindingName = bindingName
				}
				for i := range commands {
					if commands[i].BindingName == "" {
						commands[i].BindingName = bindingName
					} else if imp.Export {
						commands[i].BindingName = bindingName
					}
				}
				for i := range doctors {
					if doctors[i].BindingName == "" {
						doctors[i].BindingName = bindingName
					} else if imp.Export {
						doctors[i].BindingName = bindingName
					}
				}

				// Re-qualify depends_on with binding name now that it's stamped.
				for i := range agents {
					if agents[i].BindingName == "" || len(agents[i].DependsOn) == 0 {
						continue
					}
					for j, dep := range agents[i].DependsOn {
						// If dep was already rewritten with dir prefix but
						// doesn't have the binding, inject it.
						_, depName := ParseQualifiedName(dep)
						if !strings.Contains(depName, ".") {
							// Bare name after dir prefix: inject binding.
							binding := agents[i].BindingName
							if agents[i].Dir != "" {
								agents[i].DependsOn[j] = agents[i].Dir + "/" + binding + "." + depName
							} else {
								agents[i].DependsOn[j] = binding + "." + depName
							}
						}
					}
				}

				// Read pack name for provenance.
				impData, readErr := fs.ReadFile(impPath)
				if readErr != nil {
					return fmt.Errorf("rig %q import %q: reading %s: %w", rig.Name, bindingName, impPath, readErr)
				}
				packName, err := decodePackName(impData)
				if err != nil {
					return fmt.Errorf("rig %q import %q: parsing %s: %w", rig.Name, bindingName, impPath, err)
				}
				for i := range agents {
					if agents[i].PackName == "" {
						agents[i].PackName = packName
					}
				}
				for i := range commands {
					if commands[i].PackName == "" {
						commands[i].PackName = packName
					}
				}

				// Validate rig-scoped requirements.
				for _, req := range reqs {
					if req.Scope != "rig" {
						continue
					}
					found := false
					for _, a := range agents {
						if a.Name == req.Agent {
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("rig %q: import %q requires rig agent %q — not found", rig.Name, bindingName, req.Agent)
					}
				}

				rigTopoDirs = appendUnique(rigTopoDirs, topoDirs...)
				rigImportPackDirs = prependUniqueBlock(rigImportPackDirs, mcpTopoDirs...)

				// Hoist city-scoped agents/sessions to city scope instead of
				// dropping them at the rig-import boundary.
				hoistedAgents = append(hoistedAgents, hoistCityScopedAgents(agents)...)
				hoistedNamedSessions = append(hoistedNamedSessions, hoistCityScopedNamedSessions(namedSessions)...)
				agents = filterAgentsByScope(agents, false)
				namedSessions = filterNamedSessionsByScope(namedSessions, false)

				if rigFormulaDirs != nil {
					for _, td := range topoDirs {
						fd := filepath.Join(td, "formulas")
						if _, sErr := fs.Stat(fd); sErr == nil {
							rigFormulaDirs[rig.Name] = append(rigFormulaDirs[rig.Name], fd)
						}
					}
				}

				rigAgents = append(rigAgents, agents...)
				rigNamedSessions = append(rigNamedSessions, namedSessions...)
				cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, doctors...)
				// Runtime selection is city-wide, so rig-imported
				// runtime packs register into the same namespace.
				if err := mergeCityRuntimes(cfg, runtimes); err != nil {
					return fmt.Errorf("rig %q import %q: %w", rig.Name, bindingName, err)
				}

				if len(providers) > 0 {
					if cfg.Providers == nil {
						cfg.Providers = make(map[string]ProviderSpec)
					}
					for name, spec := range providers {
						if _, exists := cfg.Providers[name]; !exists {
							cfg.Providers[name] = spec
						}
					}
				}

				if len(upstreams) > 0 {
					if cfg.Upstreams == nil {
						cfg.Upstreams = make(map[string]UpstreamSpec)
					}
					for name, spec := range upstreams {
						if _, exists := cfg.Upstreams[name]; !exists {
							cfg.Upstreams[name] = spec
						}
					}
				}
			}
		}

		// Store per-rig pack dirs.
		if cfg.RigPackDirs == nil {
			cfg.RigPackDirs = make(map[string][]string)
		}
		if len(rigTopoDirs) > 0 {
			cfg.RigPackDirs[rig.Name] = rigTopoDirs
		}
		if len(rigPackGraphOnlyDirs) > 0 {
			if cfg.RigPackGraphOnlyDirs == nil {
				cfg.RigPackGraphOnlyDirs = make(map[string][]string)
			}
			cfg.RigPackGraphOnlyDirs[rig.Name] = rigPackGraphOnlyDirs
		}
		if len(rigImportPackDirs) > 0 {
			if cfg.RigImportPackDirs == nil {
				cfg.RigImportPackDirs = make(map[string][]string)
			}
			cfg.RigImportPackDirs[rig.Name] = rigImportPackDirs
		}

		// Collect overlay/ dirs from rig pack dirs.
		var rigOverlayDirs []string
		for _, dir := range rigTopoDirs {
			od := filepath.Join(dir, "overlay")
			if info, sErr := fs.Stat(od); sErr == nil && info.IsDir() {
				rigOverlayDirs = appendUnique(rigOverlayDirs, od)
			}
		}
		if len(rigOverlayDirs) > 0 {
			if cfg.RigOverlayDirs == nil {
				cfg.RigOverlayDirs = make(map[string][]string)
			}
			cfg.RigOverlayDirs[rig.Name] = rigOverlayDirs
		}

		// Check for duplicate agent names across packs for this rig.
		if err := checkPackAgentCollisions(rigAgents, rig.Name); err != nil {
			return err
		}

		// Apply or defer per-rig overrides/patches after all packs for this rig.
		// V2 accepts both "overrides" (V1) and "patches" (V2) TOML keys.
		allOverrides := append([]AgentOverride(nil), rig.Overrides...)
		allOverrides = append(allOverrides, rig.RigPatches...)
		if opts.deferRigPatches {
			if opts.deferredRigPatches == nil {
				return fmt.Errorf("rig %q: deferred rig patches requested without destination", rig.Name)
			}
			if len(allOverrides) > 0 {
				start := len(cfg.Agents) + len(expanded)
				*opts.deferredRigPatches = append(*opts.deferredRigPatches, deferredRigPatches{
					rigName:            rig.Name,
					agentStart:         start,
					agentEnd:           start + len(rigAgents),
					expectedAgentNames: qualifiedAgentNames(rigAgents),
					overrides:          allOverrides,
				})
			}
		} else if err := applyOverrides(rigAgents, allOverrides, rig.Name); err != nil {
			return fmt.Errorf("rig %q: %w", rig.Name, err)
		}

		// Store rig-level pack globals.
		if len(rigGlobals) > 0 {
			if cfg.RigPackGlobals == nil {
				cfg.RigPackGlobals = make(map[string][]ResolvedPackGlobal)
			}
			cfg.RigPackGlobals[rig.Name] = rigGlobals
		}

		expanded = append(expanded, rigAgents...)
		cfg.NamedSessions = append(cfg.NamedSessions, rigNamedSessions...)
	}
	cfg.Agents = append(cfg.Agents, expanded...)
	// Merge hoisted city-scoped agents/sessions (from rig includes/imports)
	// into the city set, deduped by qualified name. The same pack is commonly
	// included by several rigs; without dedup the same city-scoped agent would
	// be registered once per rig and collide (see duplicate_agent_error.go).
	// Any name already present at city scope (city-scope expansion, a city-root
	// agents/<name>/, or an earlier hoist) wins; the hoisted copy is skipped.
	cfg.Agents = mergeHoistedCityAgents(cfg.Agents, hoistedAgents)
	cfg.NamedSessions = mergeHoistedCityNamedSessions(cfg.NamedSessions, hoistedNamedSessions)
	if opts.deferRigPatches && opts.deferredRigPatches != nil {
		for i := range *opts.deferredRigPatches {
			(*opts.deferredRigPatches)[i].expectedAgentCount = len(cfg.Agents)
		}
	}
	return nil
}

// ExpandCityPacks loads all city-level packs from workspace.includes (V1)
// and city-level [imports.X] (V2). City pack agents are stamped with
// dir="" (city-scoped) and prepended to the agent list. Returns
// (formulaDirs, packRequirements, shadowWarnings, error). cityRoot is
// the city directory.
func ExpandCityPacks(cfg *City, fs fsys.FS, cityRoot string) ([]string, []PackRequirement, []string, error) {
	return expandCityPacks(cfg, fs, cityRoot, LoadOptions{})
}

func expandCityPacks(cfg *City, fs fsys.FS, cityRoot string, opts LoadOptions) ([]string, []PackRequirement, []string, error) {
	topos := cfg.Workspace.LegacyIncludes()
	hasImports := len(cfg.Imports) > 0
	if len(topos) == 0 && !hasImports {
		return nil, nil, nil, nil
	}

	var allAgents []Agent
	var allRigAgentsFromCityImports []Agent
	var allNamedSessions []NamedSession
	var allRigNamedSessionsFromCityImports []NamedSession
	var formulaDirs []string
	var allPackDirs []string
	var packGraphOnlyDirs []string
	var explicitImportPackDirs []string
	var implicitImportPackDirs []string
	var bootstrapImportPackDirs []string
	var allRequires []PackRequirement
	var allGlobals []ResolvedPackGlobal
	var packWarnings []string
	// Shared cache across all pack loads to deduplicate diamond DAGs.
	cache := &packLoadCache{results: make(map[string]*packLoadResult)}

	for _, ref := range topos {
		topoDir, err := resolvePackRef(ref, cityRoot, cityRoot)
		if err != nil {
			// Pack directory may have been removed upstream (e.g. renamed/deleted
			// in the remote repo). Skip gracefully so the rest of the city loads.
			if errors.Is(err, iofs.ErrNotExist) {
				log.Printf("city pack %q: not found, skipping: %v", ref, err)
				continue
			}
			return nil, nil, nil, fmt.Errorf("city pack %q: %w", ref, err)
		}
		topoPath := filepath.Join(topoDir, packFile)

		// For remote includes, skip gracefully if the subpath was
		// deleted upstream (the git fetch succeeded but the path no
		// longer exists in the repo).
		if isRemoteRef(ref) {
			if _, sErr := fs.Stat(topoPath); sErr != nil {
				log.Printf("city pack %q: not found, skipping: %v", ref, sErr)
				continue
			}
		}

		agents, namedSessions, providers, upstreams, services, topoDirs, reqs, globals, err := loadPackWithCacheOptions(fs, topoPath, topoDir, cityRoot, "", nil, cache, opts)
		if err != nil {
			// pack.toml may be missing if the pack was removed upstream after
			// the repo was fetched. Skip gracefully.
			if errors.Is(err, iofs.ErrNotExist) {
				log.Printf("city pack %q: not found, skipping: %v", ref, err)
				continue
			}
			return nil, nil, nil, fmt.Errorf("city pack %q: %w", ref, err)
		}
		packWarnings = appendUnique(packWarnings, cachedPackWarnings(cache, topoDir)...)
		allRequires = append(allRequires, reqs...)
		allGlobals = append(allGlobals, globals...)
		cfg.Services = append(cfg.Services, services...)
		cfg.Webhooks = append(cfg.Webhooks, cachedPackWebhooks(cache, topoDir)...)
		packName := tcPackName(fs, topoPath)
		if packName == "" && len(cachedPackCommands(cache, topoDir)) > 0 {
			return nil, nil, nil, fmt.Errorf("city pack %q: discovered commands require [pack].name for CLI binding", ref)
		}
		cfg.PackCommands = appendDiscoveredCommands(cfg.PackCommands, stampDefaultBinding(cachedPackCommands(cache, topoDir), packName)...)
		cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, cachedPackDoctors(cache, topoDir)...)
		skills := cachedPackSkills(cache, topoDir)
		if packName == "" && len(skills) > 0 {
			return nil, nil, nil, fmt.Errorf("city pack %q: discovered skills require [pack].name for shared binding", ref)
		}
		cfg.PackSkills = appendDiscoveredSkills(cfg.PackSkills, stampSkillBinding(skills, packName)...)

		// Accumulate pack dirs (deduped).
		allPackDirs = appendUnique(allPackDirs, topoDirs...)
		packGraphOnlyDirs = appendUniqueLastWins(packGraphOnlyDirs, topoDirs...)

		// Keep only city-scoped and unscoped agents for city expansion.
		allRigAgentsFromCityImports = append(allRigAgentsFromCityImports,
			expandCityImportedAgentsForRigs(agents, cfg.Rigs, "")...)
		allRigNamedSessionsFromCityImports = append(allRigNamedSessionsFromCityImports,
			expandCityImportedNamedSessionsForRigs(namedSessions, cfg.Rigs, "")...)
		agents = filterAgentsByScope(agents, true)
		namedSessions = filterNamedSessionsByScope(namedSessions, true)

		allAgents = append(allAgents, agents...)
		allNamedSessions = append(allNamedSessions, namedSessions...)

		// Derive formula dirs from pack dirs.
		for _, td := range topoDirs {
			fd := filepath.Join(td, "formulas")
			if _, sErr := fs.Stat(fd); sErr == nil {
				formulaDirs = append(formulaDirs, fd)
			}
		}

		// Register pack-declared runtimes city-wide (collisions error).
		if err := mergeCityRuntimes(cfg, cachedPackRuntimes(cache, topoDir)); err != nil {
			return nil, nil, nil, fmt.Errorf("city pack %q: %w", ref, err)
		}

		// Merge pack providers (additive, first wins).
		if len(providers) > 0 {
			if cfg.Providers == nil {
				cfg.Providers = make(map[string]ProviderSpec)
			}
			for name, spec := range providers {
				if _, exists := cfg.Providers[name]; !exists {
					cfg.Providers[name] = spec
				}
			}
		}

		// Merge pack upstreams (additive, first wins; city.toml upstreams win).
		if len(upstreams) > 0 {
			if cfg.Upstreams == nil {
				cfg.Upstreams = make(map[string]UpstreamSpec)
			}
			for name, spec := range upstreams {
				if _, exists := cfg.Upstreams[name]; !exists {
					cfg.Upstreams[name] = spec
				}
			}
		}
	}

	// Process city-level [imports.X] entries (V2). These produce agents
	// with qualified names (bindingName.agentName). Processed after V1
	// includes so imports can coexist during migration.
	if hasImports {
		importNames := make([]string, 0, len(cfg.Imports))
		for name := range cfg.Imports {
			importNames = append(importNames, name)
		}
		sort.Strings(importNames)

		for _, bindingName := range importNames {
			imp := cfg.Imports[bindingName]
			if cfg.ImplicitImportBindings != nil && cfg.ImplicitImportBindings[bindingName] {
				continue
			}
			// Bundled builtin sources resolve from the user-global cache
			// on the real filesystem; hermetic non-OS loads (test fakes)
			// skip them.
			if !isOSFileSystem(fs) && builtinpacks.IsSource(imp.Source) {
				continue
			}

			// Unlike V1 includes (which skip gracefully for missing remote
			// subpaths), V2 imports are always fatal on missing source.
			// A typo in [imports.X].source should not be silently ignored.
			impDir, err := resolveImportPackRef(imp.Source, imp.Version, cityRoot, cityRoot)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("city import %q: %w", bindingName, err)
			}

			impPath := filepath.Join(impDir, packFile)
			agents, namedSessions, providers, upstreams, services, topoDirs, reqs, globals, err := loadPackWithCacheOptions(
				fs, impPath, impDir, cityRoot, "", nil, cache, opts)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("city import %q: %w", bindingName, err)
			}
			warnings := cachedPackWarnings(cache, impDir)
			if !imp.ImportIsTransitive() {
				warnings = cachedPackLocalWarnings(cache, impDir)
			}
			packWarnings = appendUnique(packWarnings, warnings...)
			commands := cachedPackCommands(cache, impDir)
			doctors := cachedPackDoctors(cache, impDir)
			runtimes := cachedPackRuntimes(cache, impDir)
			skills := cachedPackSkills(cache, impDir)
			webhooks := cachedPackWebhooks(cache, impDir)
			mcpTopoDirs := topoDirs

			// by this import. Nested pack dependencies reached through
			// either [imports] or legacy [pack].includes stay hidden from
			// the consumer.
			if !imp.ImportIsTransitive() {
				absImpDir, _ := filepath.Abs(impDir)
				var direct []Agent
				for _, a := range agents {
					absSrc, _ := filepath.Abs(a.SourceDir)
					if absSrc == absImpDir {
						direct = append(direct, a)
					}
				}
				agents = direct
				namedSessions = filterNamedSessionsBySourceDir(namedSessions, impDir)
				services = filterServicesBySourceDir(services, impDir)
				commands = filterCommandsByPackDir(commands, impDir)
				doctors = filterDoctorsByPackDir(doctors, impDir)
				runtimes = filterRuntimesByPackDir(runtimes, impDir)
				providers = cachedPackLocalProviders(cache, impDir)
				upstreams = cachedPackLocalUpstreams(cache, impDir)
				topoDirs = cachedPackLocalTopoDirs(cache, impDir)
				reqs = cachedPackLocalRequires(cache, impDir)
				globals = cachedPackLocalGlobals(cache, impDir)
				skills = filterSkillsByPackDir(skills, impDir)
				webhooks = filterWebhooksBySourceDir(webhooks, impDir)
				mcpTopoDirs = filterPackDirsByRoot(topoDirs, impDir)
			}

			// Stamp binding name on all agents and named sessions.
			// At the city level, ALL agents from an import get the city's
			// binding — any nested bindings are overridden because the city
			// is the root of composition and its binding is the user-visible one.
			for i := range agents {
				agents[i].BindingName = bindingName
			}
			for i := range namedSessions {
				namedSessions[i].BindingName = bindingName
			}

			// Re-qualify depends_on with binding name.
			for i := range agents {
				if agents[i].BindingName == "" || len(agents[i].DependsOn) == 0 {
					continue
				}
				for j, dep := range agents[i].DependsOn {
					_, depName := ParseQualifiedName(dep)
					if !strings.Contains(depName, ".") {
						binding := agents[i].BindingName
						if agents[i].Dir != "" {
							agents[i].DependsOn[j] = agents[i].Dir + "/" + binding + "." + depName
						} else {
							agents[i].DependsOn[j] = binding + "." + depName
						}
					}
				}
			}
			for i := range commands {
				if commands[i].BindingName == "" {
					commands[i].BindingName = bindingName
				} else if imp.Export {
					commands[i].BindingName = bindingName
				}
			}
			for i := range doctors {
				if doctors[i].BindingName == "" {
					doctors[i].BindingName = bindingName
				} else if imp.Export {
					doctors[i].BindingName = bindingName
				}
			}

			// Read imported pack name for provenance.
			impData, readErr := fs.ReadFile(impPath)
			if readErr != nil {
				return nil, nil, nil, fmt.Errorf("city import %q: reading %s: %w", bindingName, impPath, readErr)
			}
			packName, err := decodePackName(impData)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("city import %q: parsing %s: %w", bindingName, impPath, err)
			}
			for i := range agents {
				if agents[i].PackName == "" {
					agents[i].PackName = packName
				}
			}
			for i := range commands {
				if commands[i].PackName == "" {
					commands[i].PackName = packName
				}
			}
			for i := range doctors {
				if doctors[i].PackName == "" {
					doctors[i].PackName = packName
				}
			}

			allRigAgentsFromCityImports = append(allRigAgentsFromCityImports,
				expandCityImportedAgentsForRigs(agents, cfg.Rigs, bindingName)...)
			allRigNamedSessionsFromCityImports = append(allRigNamedSessionsFromCityImports,
				expandCityImportedNamedSessionsForRigs(namedSessions, cfg.Rigs, bindingName)...)

			allRequires = append(allRequires, reqs...)
			allGlobals = append(allGlobals, globals...)
			cfg.Services = append(cfg.Services, services...)
			cfg.Webhooks = append(cfg.Webhooks, webhooks...)
			cfg.PackCommands = appendDiscoveredCommands(cfg.PackCommands, commands...)
			cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, doctors...)
			// Register pack-declared runtimes city-wide (collisions error).
			if err := mergeCityRuntimes(cfg, runtimes); err != nil {
				return nil, nil, nil, fmt.Errorf("city import %q: %w", bindingName, err)
			}
			// Bootstrap-managed implicit imports own their skill
			// materialization through the compat path; explicit user
			// imports (including [imports.core]) contribute skills like
			// any other pack.
			if cfg.BootstrapImportBindings == nil || !cfg.BootstrapImportBindings[bindingName] {
				cfg.PackSkills = appendDiscoveredSkills(cfg.PackSkills, stampImportedSkillBinding(skills, bindingName, imp.Export)...)
			}
			allPackDirs = appendUnique(allPackDirs, topoDirs...)
			switch {
			case cfg.BootstrapImportBindings != nil && cfg.BootstrapImportBindings[bindingName]:
				bootstrapImportPackDirs = prependUniqueBlock(bootstrapImportPackDirs, mcpTopoDirs...)
				cfg.BootstrapImportMCPBindings = stampMCPDirBindings(cfg.BootstrapImportMCPBindings, mcpTopoDirs, bindingName)
			case cfg.ImplicitImportBindings != nil && cfg.ImplicitImportBindings[bindingName]:
				implicitImportPackDirs = prependUniqueBlock(implicitImportPackDirs, mcpTopoDirs...)
				cfg.ImplicitImportMCPBindings = stampMCPDirBindings(cfg.ImplicitImportMCPBindings, mcpTopoDirs, bindingName)
			default:
				explicitImportPackDirs = prependUniqueBlock(explicitImportPackDirs, mcpTopoDirs...)
				cfg.ExplicitImportMCPBindings = stampMCPDirBindings(cfg.ExplicitImportMCPBindings, mcpTopoDirs, bindingName)
			}

			// Filter by scope for city expansion.
			agents = filterAgentsByScope(agents, true)
			namedSessions = filterNamedSessionsByScope(namedSessions, true)

			allAgents = append(allAgents, agents...)
			allNamedSessions = append(allNamedSessions, namedSessions...)

			// Derive formula dirs.
			for _, td := range topoDirs {
				fd := filepath.Join(td, "formulas")
				if _, sErr := fs.Stat(fd); sErr == nil {
					formulaDirs = append(formulaDirs, fd)
				}
			}

			// Merge providers (additive, first wins).
			if len(providers) > 0 {
				if cfg.Providers == nil {
					cfg.Providers = make(map[string]ProviderSpec)
				}
				for name, spec := range providers {
					if _, exists := cfg.Providers[name]; !exists {
						cfg.Providers[name] = spec
					}
				}
			}

			// Merge upstreams (additive, first wins; city.toml upstreams win).
			if len(upstreams) > 0 {
				if cfg.Upstreams == nil {
					cfg.Upstreams = make(map[string]UpstreamSpec)
				}
				for name, spec := range upstreams {
					if _, exists := cfg.Upstreams[name]; !exists {
						cfg.Upstreams[name] = spec
					}
				}
			}
		}
	}

	allAgents = append(allAgents, allRigAgentsFromCityImports...)
	allNamedSessions = append(allNamedSessions, allRigNamedSessionsFromCityImports...)

	// Store city pack dirs.
	cfg.PackDirs = appendUnique(cfg.PackDirs, allPackDirs...)
	cfg.PackGraphOnlyDirs = appendUniqueLastWins(cfg.PackGraphOnlyDirs, packGraphOnlyDirs...)
	cfg.ExplicitImportPackDirs = appendUniqueLastWins(cfg.ExplicitImportPackDirs, explicitImportPackDirs...)
	cfg.ImplicitImportPackDirs = appendUniqueLastWins(cfg.ImplicitImportPackDirs, implicitImportPackDirs...)
	cfg.BootstrapImportPackDirs = appendUniqueLastWins(cfg.BootstrapImportPackDirs, bootstrapImportPackDirs...)

	// Collect overlay/ dirs from pack dirs.
	for _, dir := range allPackDirs {
		od := filepath.Join(dir, "overlay")
		if info, err := fs.Stat(od); err == nil && info.IsDir() {
			cfg.PackOverlayDirs = appendUnique(cfg.PackOverlayDirs, od)
		}
	}

	// Check for duplicate agent names across city packs.
	if err := checkPackAgentCollisions(allAgents, ""); err != nil {
		return nil, nil, nil, err
	}

	// City pack agents go at the front (before user-defined agents).
	cfg.Agents = append(allAgents, cfg.Agents...)
	cfg.NamedSessions = append(allNamedSessions, cfg.NamedSessions...)

	// Detect shadow conflicts: city-local agents masking imported agents.
	// A city agent (BindingName == "") with the same bare Name as an
	// imported agent (BindingName != "") shadows it. Warn unless the
	// import has shadow = "silent".
	var shadowWarnings []string
	if hasImports {
		// Build set of imported agent bare names → binding name.
		importedNames := make(map[string]string) // bare name → binding
		for _, a := range cfg.Agents {
			if a.BindingName != "" && a.Dir == "" {
				importedNames[a.Name] = a.BindingName
			}
		}
		// Check city-local agents against imported names.
		for _, a := range cfg.Agents {
			if a.BindingName == "" && a.Dir == "" && !a.Implicit {
				if binding, ok := importedNames[a.Name]; ok {
					// Check if this import has shadow = "silent".
					if imp, impOk := cfg.Imports[binding]; impOk && imp.Shadow == "silent" {
						continue
					}
					shadowWarnings = append(shadowWarnings,
						fmt.Sprintf("city agent %q shadows agent of the same name from import %q (set shadow = \"silent\" on [imports.%s] to suppress)", a.Name, binding, binding))
				}
			}
		}
	}

	// Store city-level pack globals.
	cfg.PackGlobals = append(cfg.PackGlobals, allGlobals...)
	shadowWarnings = appendUnique(shadowWarnings, packWarnings...)

	return formulaDirs, allRequires, shadowWarnings, nil
}

// resolveImportPackRef resolves a V2 import's pack directory.
// declaredVersion is the import's declared version constraint; it gates the
// no-lock bundled fallback so a declared non-canonical pin never silently
// composes the binary's embedded content.
func resolveImportPackRef(ref, declaredVersion, declDir, cityRoot string) (string, error) {
	if isGitHubTreeURL(ref) {
		_, subpath, _ := parseGitHubTreeURL(ref)
		cacheDir, err := resolveInstalledRemoteImport(ref, declaredVersion, cityRoot)
		if err != nil {
			return "", err
		}
		if subpath != "" {
			return filepath.Join(cacheDir, subpath), nil
		}
		return cacheDir, nil
	}
	if isRemoteInclude(ref) {
		_, subpath, _ := parseRemoteInclude(ref)
		cacheDir, err := resolveInstalledRemoteImport(ref, declaredVersion, cityRoot)
		if err != nil {
			return "", err
		}
		if subpath != "" {
			return filepath.Join(cacheDir, subpath), nil
		}
		return cacheDir, nil
	}
	return resolvePackRef(ref, declDir, cityRoot)
}

// ComputeFormulaLayers builds the FormulaLayers from the resolved formula
// directories. Each layer slice is ordered lowest→highest priority.
//
// Parameters:
//   - cityTopoFormulas: formula dirs from city packs (Layer 1), nil if none
//   - cityLocalFormulas: formula dir from city [formulas] section (Layer 2), "" if none
//   - rigTopoFormulas: map[rigName][]formulaDirs from rig packs (Layer 3)
//   - rigs: rig configs (for rig-local FormulasDir, Layer 4)
//   - cityRoot: city directory for resolving relative paths
func ComputeFormulaLayers(cityTopoFormulas []string, cityLocalFormulas string, rigTopoFormulas map[string][]string, rigs []Rig, cityRoot string) FormulaLayers {
	fl := FormulaLayers{
		Rigs: make(map[string][]string),
	}

	// City layers (apply to city-scoped agents and as base for all rigs).
	var cityLayers []string
	cityLayers = append(cityLayers, cityTopoFormulas...)
	if cityLocalFormulas != "" {
		cityLayers = append(cityLayers, cityLocalFormulas)
	}
	fl.City = cityLayers

	// Per-rig layers: city layers + rig pack + rig local.
	for _, r := range rigs {
		layers := make([]string, len(cityLayers))
		copy(layers, cityLayers)
		if fds, ok := rigTopoFormulas[r.Name]; ok {
			layers = append(layers, fds...)
		}
		if r.FormulasDir != "" {
			rigLocalDir := resolveConfigPath(r.FormulasDir, cityRoot, cityRoot)
			layers = append(layers, rigLocalDir)
		}
		if len(layers) > 0 {
			fl.Rigs[r.Name] = layers
		}
	}

	return fl
}

// checkPackAgentCollisions detects duplicate agent names within
// pack-expanded agents and returns an error with provenance (which
// pack directories defined the conflicting agents). rigName is used
// for the error message context; pass "" for city-scoped agents.
func checkPackAgentCollisions(agents []Agent, rigName string) error {
	// Map agent qualified name → list of source directories that defined it.
	// Uses QualifiedName so agents with different bindings (e.g.,
	// "gs.mayor" and "maint.mayor") don't collide.
	sources := make(map[string][]string)
	for _, a := range agents {
		src := a.SourceDir
		if src == "" {
			continue // inline agents have no SourceDir
		}
		qn := a.QualifiedName()
		existing := sources[qn]
		if !slices.Contains(existing, src) {
			sources[qn] = append(existing, src)
		}
	}
	for name, dirs := range sources {
		if len(dirs) < 2 {
			continue
		}
		scope := "city"
		if rigName != "" {
			scope = fmt.Sprintf("rig %q", rigName)
		}
		return fmt.Errorf("%s: packs define duplicate agent %q:\n  - %s\nrename one agent in its pack.toml, or use separate rigs",
			scope, name, strings.Join(dirs, "\n  - "))
	}
	return nil
}

// loadPack loads a pack.toml, validates metadata, and returns the
// agent list with dir stamped and paths adjusted, and the ordered pack
// directories.
//
// The topoDirs return is the ordered list: included pack dirs first
// (depth-first), then this pack's dir. Consumers derive resource paths
// from these dirs (e.g., formulas/, prompts/shared/).
//
// The seen set tracks visited pack directories for cycle detection.
// Pass nil for the initial call; it will be initialized automatically.
// Includes are processed recursively: included agents come first (base
// layer), then the parent's own agents (override layer).
// packLoadCache caches results from loadPack to avoid loading the same
// pack directory twice in a diamond-shaped DAG (A→B→D, A→C→D). The
// cache is keyed by absolute directory path.
type packLoadCache struct {
	results map[string]*packLoadResult
}

type packLoadResult struct {
	agents         []Agent
	namedSessions  []NamedSession
	providers      map[string]ProviderSpec
	localProviders map[string]ProviderSpec
	upstreams      map[string]UpstreamSpec
	localUpstreams map[string]UpstreamSpec
	services       []Service
	webhooks       []Webhook
	topoDirs       []string
	localTopoDirs  []string
	requires       []PackRequirement
	localRequires  []PackRequirement
	globals        []ResolvedPackGlobal
	localGlobals   []ResolvedPackGlobal
	commands       []DiscoveredCommand
	doctors        []DiscoveredDoctor
	runtimes       []DiscoveredRuntime
	skills         []DiscoveredSkillCatalog
	localWarnings  []string
	warnings       []string
}

func parsePackConfigWithMeta(data []byte, source string) (PackConfig, []string, error) {
	cfg, _, warnings, err := parsePackConfigWithMetadata(data, source)
	return cfg, warnings, err
}

func parsePackConfigWithMetadata(data []byte, source string) (PackConfig, toml.MetaData, []string, error) {
	var cfg PackConfig
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return PackConfig{}, md, nil, err
	}
	normalizePackAgentDefaultsAlias(&cfg, md)
	warnings := agentDefaultsCompatibilityWarnings(md, source)
	warnings = append(warnings, CheckUndecodedKeys(md, source)...)
	return cfg, md, warnings, nil
}

func normalizePackAgentDefaultsAlias(cfg *PackConfig, meta toml.MetaData) {
	FoldAgentDefaultsAlias(&cfg.AgentDefaults, cfg.AgentsDefaults, meta)
	cfg.AgentsDefaults = AgentDefaults{}
}

//nolint:unparam // compatibility wrapper keeps the recursion-set argument at the public helper boundary.
func loadPack(fs fsys.FS, topoPath, topoDir, cityRoot, rigName string, seen map[string]bool) ([]Agent, []NamedSession, map[string]ProviderSpec, map[string]UpstreamSpec, []Service, []string, []PackRequirement, []ResolvedPackGlobal, error) {
	return loadPackWithCache(fs, topoPath, topoDir, cityRoot, rigName, seen, nil)
}

func loadPackWithCache(fs fsys.FS, topoPath, topoDir, cityRoot, rigName string, seen map[string]bool, cache *packLoadCache) ([]Agent, []NamedSession, map[string]ProviderSpec, map[string]UpstreamSpec, []Service, []string, []PackRequirement, []ResolvedPackGlobal, error) {
	return loadPackWithCacheOptions(fs, topoPath, topoDir, cityRoot, rigName, seen, cache, LoadOptions{})
}

// LintPackLoad is the pack graph state needed by CLI linting.
type LintPackLoad struct {
	Path          string
	Name          string
	Agents        []Agent
	NamedSessions []NamedSession
	Providers     map[string]ProviderSpec
	Upstreams     map[string]UpstreamSpec
	PackDirs      []string
	Warnings      []string
}

// LoadPackForLint loads a standalone pack directory using the same parser,
// include/import expansion, and path adjustment as normal pack loading.
func LoadPackForLint(fs fsys.FS, packDir string) (*LintPackLoad, error) {
	if strings.TrimSpace(packDir) == "" {
		return nil, fmt.Errorf("pack directory is required")
	}
	absDir, err := filepath.Abs(packDir)
	if err != nil {
		return nil, fmt.Errorf("resolving pack directory %q: %w", packDir, err)
	}
	topoPath := filepath.Join(absDir, packFile)
	cache := &packLoadCache{results: make(map[string]*packLoadResult)}
	agents, namedSessions, providers, upstreams, _, topoDirs, _, _, err := loadPackWithCacheOptions(
		fs, topoPath, absDir, absDir, "", nil, cache, LoadOptions{})
	if err != nil {
		return nil, err
	}
	data, err := fs.ReadFile(topoPath)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", packFile, err)
	}
	packName, err := decodePackName(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", packFile, err)
	}
	return &LintPackLoad{
		Path:          absDir,
		Name:          packName,
		Agents:        agents,
		NamedSessions: namedSessions,
		Providers:     providers,
		Upstreams:     upstreams,
		PackDirs:      topoDirs,
		Warnings:      cachedPackWarnings(cache, absDir),
	}, nil
}

func loadPackWithCacheOptions(fs fsys.FS, topoPath, topoDir, cityRoot, rigName string, seen map[string]bool, cache *packLoadCache, opts LoadOptions) ([]Agent, []NamedSession, map[string]ProviderSpec, map[string]UpstreamSpec, []Service, []string, []PackRequirement, []ResolvedPackGlobal, error) {
	var agents []Agent
	var namedSessions []NamedSession
	var providers map[string]ProviderSpec
	var upstreams map[string]UpstreamSpec
	var services []Service
	var topoDirs []string
	var requirements []PackRequirement
	var globals []ResolvedPackGlobal
	err := withRepoCacheReadLockForPath(topoDir, func() error {
		var loadErr error
		agents, namedSessions, providers, upstreams, services, topoDirs, requirements, globals, loadErr = loadPackWithCacheOptionsLocked(
			fs, topoPath, topoDir, cityRoot, rigName, seen, cache, opts)
		return loadErr
	})
	return agents, namedSessions, providers, upstreams, services, topoDirs, requirements, globals, err
}

func loadPackWithCacheOptionsLocked(fs fsys.FS, topoPath, topoDir, cityRoot, rigName string, seen map[string]bool, cache *packLoadCache, opts LoadOptions) ([]Agent, []NamedSession, map[string]ProviderSpec, map[string]UpstreamSpec, []Service, []string, []PackRequirement, []ResolvedPackGlobal, error) {
	// Initialize seen set on first call.
	if seen == nil {
		seen = make(map[string]bool)
	}
	if cache == nil {
		cache = &packLoadCache{results: make(map[string]*packLoadResult)}
	}

	// Cycle detection: resolve to absolute path for reliable comparison.
	// seen is a recursion-stack set (not global-visited): entries are added
	// on entry and removed on return. This allows diamond-shaped DAGs
	// (A→B→D, A→C→D) while still catching true cycles (A→B→A).
	absTopoDir, err := filepath.Abs(topoDir)
	if err != nil {
		absTopoDir = topoDir
	}
	if seen[absTopoDir] {
		return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("cycle detected: pack %q already visited", topoDir)
	}

	// Dedup: if we've already loaded this exact directory, return a copy
	// of the cached result so the caller can stamp different bindings
	// without mutating the cached canonical copy. This supports both
	// diamond DAGs (same binding, deduped by downstream collision checks)
	// and intentional multi-binding (same pack imported as both "foo"
	// and "bar").
	if cached, ok := cache.results[absTopoDir]; ok {
		cloned := clonePackLoadResult(cached)
		return cloned.agents, cloned.namedSessions, cloned.providers, cloned.upstreams, cloned.services, cloned.topoDirs, cloned.requires, cloned.globals, nil
	}

	seen[absTopoDir] = true
	defer func() { delete(seen, absTopoDir) }()

	data, err := fs.ReadFile(topoPath)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("loading %s: %w", packFile, err)
	}

	tc, md, packWarnings, err := parsePackConfigWithMetadata(data, topoPath)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("parsing %s: %w", packFile, err)
	}
	if err := validatePackAuthoringSurface(md, topoPath); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("parsing %s: %w", packFile, err)
	}
	if fatalWarnings := fatalUndecodedWarnings(md, topoPath); len(fatalWarnings) > 0 {
		return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("parsing %s: %s", packFile, strings.Join(fatalWarnings, "; "))
	}

	if err := validatePackMeta(&tc.Pack); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}

	// Process includes: accumulate base-layer agents, providers,
	// pack dirs, requirements, and globals from included packs.
	var includedAgents []Agent
	var includedNamedSessions []NamedSession
	var includedServices []Service
	var includedWebhooks []Webhook
	var includedTopoDirs []string
	var allRequires []PackRequirement
	var includedGlobals []ResolvedPackGlobal
	var includedCommands []DiscoveredCommand
	var includedDoctors []DiscoveredDoctor
	var includedRuntimes []DiscoveredRuntime
	var includedSkills []DiscoveredSkillCatalog
	var inheritedWarnings []string
	includedProviders := make(map[string]ProviderSpec)
	includedUpstreams := make(map[string]UpstreamSpec)

	for _, inc := range tc.Pack.Includes {
		incTopoDir, err := resolvePackRef(inc, topoDir, cityRoot)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("include %q: %w", inc, err)
		}

		incTopoPath := filepath.Join(incTopoDir, packFile)
		incAgents, incNamedSessions, incProviders, incUpstreams, incServices, incTopoDirs, incReqs, incGlobals, err := loadPackWithCacheOptions(
			fs, incTopoPath, incTopoDir, cityRoot, rigName, seen, cache, opts)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("include %q: %w", inc, err)
		}
		inheritedWarnings = appendUnique(inheritedWarnings, cachedPackWarnings(cache, incTopoDir)...)

		includedAgents = append(includedAgents, incAgents...)
		includedNamedSessions = append(includedNamedSessions, incNamedSessions...)
		includedServices = append(includedServices, incServices...)
		includedWebhooks = append(includedWebhooks, cachedPackWebhooks(cache, incTopoDir)...)
		includedTopoDirs = append(includedTopoDirs, incTopoDirs...)
		allRequires = append(allRequires, incReqs...)
		includedGlobals = append(includedGlobals, incGlobals...)
		includedCommands = append(includedCommands, cachedPackCommands(cache, incTopoDir)...)
		includedDoctors = append(includedDoctors, cachedPackDoctors(cache, incTopoDir)...)
		includedRuntimes = append(includedRuntimes, cachedPackRuntimes(cache, incTopoDir)...)
		includedSkills = append(includedSkills, cachedPackSkills(cache, incTopoDir)...)

		// Merge providers: included first, no overwrite.
		for name, spec := range incProviders {
			if _, exists := includedProviders[name]; !exists {
				includedProviders[name] = spec
			}
		}
		// Merge upstreams: included first, no overwrite (mirrors providers).
		for name, spec := range incUpstreams {
			if _, exists := includedUpstreams[name]; !exists {
				includedUpstreams[name] = spec
			}
		}
	}
	applyInheritedPackAgentDefaults(includedAgents, tc.AgentDefaults)

	// Process V2 [imports.X] entries. These are named bindings that
	// produce agents with qualified names (bindingName.agentName).
	// Resolution mechanics are described at the resolveImportPackRef call
	// site below. Process in sorted order for deterministic output.
	importNames := make([]string, 0, len(tc.Imports))
	for name := range tc.Imports {
		importNames = append(importNames, name)
	}
	sort.Strings(importNames)

	for _, bindingName := range importNames {
		imp := tc.Imports[bindingName]

		// Resolve the import source through the V2-aware resolver: local
		// paths resolve directly, packs.lock authoritatively resolves
		// remote sources, and a bundled source at its canonical pin
		// self-heals from the binary's embedded content when the lock is
		// absent or lacks the entry — matching city- and rig-scope imports.
		impDir, err := resolveImportPackRef(imp.Source, imp.Version, topoDir, cityRoot)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: %w", bindingName, err)
		}

		impPath := filepath.Join(impDir, packFile)
		impAgents, impNamedSessions, impProviders, impUpstreams, impServices, impTopoDirs, impReqs, impGlobals, err := loadPackWithCacheOptions(
			fs, impPath, impDir, cityRoot, rigName, seen, cache, opts)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: %w", bindingName, err)
		}
		warnings := cachedPackWarnings(cache, impDir)
		if !imp.ImportIsTransitive() {
			warnings = cachedPackLocalWarnings(cache, impDir)
		}
		inheritedWarnings = appendUnique(inheritedWarnings, warnings...)
		impCommands := cachedPackCommands(cache, impDir)
		impDoctors := cachedPackDoctors(cache, impDir)
		impRuntimes := cachedPackRuntimes(cache, impDir)
		impSkills := cachedPackSkills(cache, impDir)
		impWebhooks := cachedPackWebhooks(cache, impDir)

		// When transitive = false, strip agents that came from the
		// imported pack's nested dependencies. We keep only agents
		// whose SourceDir matches the import's own directory, which
		// suppresses both nested [imports] and legacy [pack].includes.
		if !imp.ImportIsTransitive() {
			absImpDir, _ := filepath.Abs(impDir)
			var direct []Agent
			for _, a := range impAgents {
				absSrc, _ := filepath.Abs(a.SourceDir)
				if absSrc == absImpDir {
					direct = append(direct, a)
				}
			}
			impAgents = direct
			impNamedSessions = filterNamedSessionsBySourceDir(impNamedSessions, impDir)
			impServices = filterServicesBySourceDir(impServices, impDir)
			impCommands = filterCommandsByPackDir(impCommands, impDir)
			impDoctors = filterDoctorsByPackDir(impDoctors, impDir)
			impRuntimes = filterRuntimesByPackDir(impRuntimes, impDir)
			impProviders = cachedPackLocalProviders(cache, impDir)
			impUpstreams = cachedPackLocalUpstreams(cache, impDir)
			impTopoDirs = cachedPackLocalTopoDirs(cache, impDir)
			impReqs = cachedPackLocalRequires(cache, impDir)
			impGlobals = cachedPackLocalGlobals(cache, impDir)
			impSkills = filterSkillsByPackDir(impSkills, impDir)
			impWebhooks = filterWebhooksBySourceDir(impWebhooks, impDir)
		}

		// Stamp binding name on all agents and named sessions from this import.
		for i := range impAgents {
			if impAgents[i].BindingName == "" {
				impAgents[i].BindingName = bindingName
			} else if imp.Export {
				impAgents[i].BindingName = bindingName
			}
		}
		for i := range impNamedSessions {
			if impNamedSessions[i].BindingName == "" {
				impNamedSessions[i].BindingName = bindingName
			} else if imp.Export {
				impNamedSessions[i].BindingName = bindingName
			}
		}
		for i := range impCommands {
			if impCommands[i].BindingName == "" {
				impCommands[i].BindingName = bindingName
			} else if imp.Export {
				impCommands[i].BindingName = bindingName
			}
		}
		for i := range impDoctors {
			if impDoctors[i].BindingName == "" {
				impDoctors[i].BindingName = bindingName
			} else if imp.Export {
				impDoctors[i].BindingName = bindingName
			}
		}
		impSkills = stampImportedSkillBinding(impSkills, bindingName, imp.Export)

		// Read the imported pack name for provenance tracking.
		impData, readErr := fs.ReadFile(impPath)
		if readErr != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: reading %s: %w", bindingName, impPath, readErr)
		}
		packName, err := decodePackName(impData)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: parsing %s: %w", bindingName, impPath, err)
		}
		for i := range impAgents {
			if impAgents[i].PackName == "" {
				impAgents[i].PackName = packName
			}
		}
		for i := range impCommands {
			if impCommands[i].PackName == "" {
				impCommands[i].PackName = packName
			}
		}
		for i := range impDoctors {
			if impDoctors[i].PackName == "" {
				impDoctors[i].PackName = packName
			}
		}
		for i := range impSkills {
			if impSkills[i].PackName == "" {
				impSkills[i].PackName = packName
			}
		}

		includedAgents = append(includedAgents, impAgents...)
		includedNamedSessions = append(includedNamedSessions, impNamedSessions...)
		includedServices = append(includedServices, impServices...)
		includedWebhooks = append(includedWebhooks, impWebhooks...)
		includedTopoDirs = append(includedTopoDirs, impTopoDirs...)
		allRequires = append(allRequires, impReqs...)
		includedGlobals = append(includedGlobals, impGlobals...)
		includedCommands = append(includedCommands, impCommands...)
		includedDoctors = append(includedDoctors, impDoctors...)
		includedRuntimes = append(includedRuntimes, impRuntimes...)
		includedSkills = append(includedSkills, impSkills...)

		for name, spec := range impProviders {
			if _, exists := includedProviders[name]; !exists {
				includedProviders[name] = spec
			}
		}
		for name, spec := range impUpstreams {
			if _, exists := includedUpstreams[name]; !exists {
				includedUpstreams[name] = spec
			}
		}
	}

	// Collect this pack's own requirements.
	allRequires = append(allRequires, tc.Pack.Requires...)

	// Stamp layoutV1Inline on this pack's [[agent]] blocks BEFORE v2
	// discovery appends to tc.Agents. Discovery stamps layoutV2Convention
	// itself; the field is preserved through the merge below. (ga-9ogb)
	for i := range tc.Agents {
		tc.Agents[i].layout = layoutV1Inline
	}

	// V2 convention-based agent discovery: scan agents/ directory.
	// Convention-discovered agents are appended AFTER TOML-declared agents
	// so [[agent]] tables take precedence when both exist.
	discovered, dErr := DiscoverPackAgents(fs, topoDir, tc.Pack.Name, agentNameSet(tc.Agents))
	if dErr != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, dErr
	}
	tc.Agents = append(tc.Agents, discovered...)
	applyInheritedPackAgentDefaults(tc.Agents, tc.AgentDefaults)

	commands, err := DiscoverPackCommands(fs, topoDir, tc.Pack.Name)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	commands = append(commands, legacyPackCommands(tc.Commands, topoDir, tc.Pack.Name)...)
	doctors, err := DiscoverPackDoctors(fs, topoDir, tc.Pack.Name)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	legacyDoctors, err := legacyPackDoctors(fs, tc.Doctor, topoDir, tc.Pack.Name)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	doctors = append(doctors, legacyDoctors...)
	localRuntimes, err := packLocalRuntimes(&tc, topoDir)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	skills, err := DiscoverPackSkills(fs, topoDir, tc.Pack.Name)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}

	// V2 convention-based order discovery: top-level orders/ flat files are the
	// standard layout. Deprecated subdirectory order paths are a filesystem
	// layout cutover, not a city.toml PackV1 authoring surface, so they are
	// rejected for all pack schemas.
	if !opts.allowLegacyOrderLayouts {
		if _, err := orders.ScanRoots(fs, []orders.ScanRoot{{
			Dir:          filepath.Join(topoDir, "orders"),
			FormulaLayer: filepath.Join(topoDir, "formulas"),
		}}, nil); err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, err
		}
	}

	// Stamp parent agents: set dir = rigName (unless already set), adjust paths.
	agents := make([]Agent, len(tc.Agents))
	copy(agents, tc.Agents)
	for i := range agents {
		if agents[i].Dir == "" {
			agents[i].Dir = rigName
		}
		// Track where this agent's config was defined.
		agents[i].SourceDir = topoDir
		// Stamp source provenance (ga-tpfc). expandCityPacks may
		// later override sourcePack → sourceAutoImport for bindings
		// that came from [defaults.rig.imports].
		agents[i].source = sourcePack
		// Resolve prompt_template paths relative to pack directory.
		if agents[i].PromptTemplate != "" {
			agents[i].PromptTemplate = adjustFragmentPath(
				agents[i].PromptTemplate, topoDir, cityRoot)
		}
		// Leave session_setup_script as-authored and resolve it at runtime
		// against SourceDir so pack-local script paths do not collapse back
		// into city-root-relative strings.
		// Resolve overlay_dir paths relative to pack directory.
		if agents[i].OverlayDir != "" {
			agents[i].OverlayDir = adjustFragmentPath(
				agents[i].OverlayDir, topoDir, cityRoot)
		}
	}
	namedSessions := make([]NamedSession, len(tc.NamedSessions))
	copy(namedSessions, tc.NamedSessions)
	for i := range namedSessions {
		if namedSessions[i].Dir == "" {
			namedSessions[i].Dir = rigName
		}
		namedSessions[i].SourceDir = topoDir
	}

	services := make([]Service, len(tc.Services))
	copy(services, tc.Services)
	for i := range services {
		services[i].SourceDir = topoDir
		if services[i].PublishMode == "direct" {
			return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("service %q: packs may not set publish_mode=direct", services[i].Name)
		}
	}

	// Stamp this pack's own webhooks with pack provenance. The default-closed
	// public pack-guard runs once post-composition in LoadWithIncludes; here we
	// only record where each webhook came from.
	webhooks := stampWebhookSource(tc.Webhooks, topoDir)

	// Merge: included agents first (base), then parent agents (override).
	includedAgents = append(includedAgents, agents...)
	includedNamedSessions = append(includedNamedSessions, namedSessions...)
	includedServices = append(includedServices, services...)
	includedWebhooks = append(includedWebhooks, webhooks...)
	includedCommands = append(includedCommands, commands...)
	includedDoctors = append(includedDoctors, doctors...)
	includedRuntimes = append(includedRuntimes, localRuntimes...)
	includedSkills = append(includedSkills, skills...)

	// Apply pack-level patches to the merged agent list.
	if !tc.Patches.IsEmpty() {
		adjustPackPatchPaths(&tc.Patches, topoDir, cityRoot)
		if err := applyPackAgentPatches(includedAgents, tc.Patches.Agents); err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, err
		}
	}

	// Qualify depends_on entries AFTER patches so that patch-supplied
	// bare names are also qualified. Pack agents have Dir = rigName,
	// making their QualifiedName "rig/name" (V1) or "rig/binding.name"
	// (V2). DependsOn entries are written as bare names in pack TOML.
	// Rewrite them to include the rig prefix and, for V2 agents, the
	// binding name of the depending agent (sibling deps share binding).
	for i := range includedAgents {
		if len(includedAgents[i].DependsOn) == 0 {
			continue
		}
		qualified := make([]string, len(includedAgents[i].DependsOn))
		for j, dep := range includedAgents[i].DependsOn {
			if strings.Contains(dep, "/") || strings.Contains(dep, ".") {
				// Already qualified — leave as-is.
				qualified[j] = dep
				continue
			}
			// Bare dep name: qualify with the same prefix as this agent.
			// For V2 agents, prepend binding so "db" becomes "gs.db"
			// (matching sibling agents from the same import).
			if includedAgents[i].BindingName != "" {
				dep = includedAgents[i].BindingName + "." + dep
			}
			if includedAgents[i].Dir != "" {
				dep = includedAgents[i].Dir + "/" + dep
			}
			qualified[j] = dep
		}
		includedAgents[i].DependsOn = qualified
	}

	// Merge providers: parent wins over included.
	mergedProviders := includedProviders
	for name, spec := range tc.Providers {
		mergedProviders[name] = spec
	}

	// Merge upstreams: parent wins over included (mirrors providers).
	mergedUpstreams := includedUpstreams
	for name, spec := range tc.Upstreams {
		mergedUpstreams[name] = spec
	}

	// Build pack dirs: included pack dirs first (lower priority),
	// then this pack's dir (higher priority).
	var topoDirs []string
	topoDirs = append(topoDirs, includedTopoDirs...)
	topoDirs = append(topoDirs, topoDir)

	// Collect globals: included globals first, then this pack's own.
	var localGlobals []ResolvedPackGlobal
	if len(tc.Global.SessionLive) > 0 {
		localGlobals = append(localGlobals, ResolvedPackGlobal{
			SessionLive: resolveConfigDirInCommands(tc.Global.SessionLive, topoDir),
			PackName:    tc.Pack.Name,
		})
	}
	var allGlobals []ResolvedPackGlobal
	allGlobals = append(allGlobals, includedGlobals...)
	allGlobals = append(allGlobals, localGlobals...)

	// Cache result for diamond-DAG dedup.
	cache.results[absTopoDir] = clonePackLoadResult(&packLoadResult{
		agents:         includedAgents,
		namedSessions:  includedNamedSessions,
		providers:      mergedProviders,
		localProviders: tc.Providers,
		upstreams:      mergedUpstreams,
		localUpstreams: tc.Upstreams,
		services:       includedServices,
		webhooks:       includedWebhooks,
		topoDirs:       topoDirs,
		localTopoDirs:  []string{topoDir},
		requires:       allRequires,
		localRequires:  append([]PackRequirement(nil), tc.Pack.Requires...),
		globals:        allGlobals,
		localGlobals:   localGlobals,
		commands:       includedCommands,
		doctors:        includedDoctors,
		runtimes:       includedRuntimes,
		skills:         includedSkills,
		localWarnings:  append([]string(nil), packWarnings...),
		warnings:       appendUnique(append([]string(nil), inheritedWarnings...), packWarnings...),
	})

	return includedAgents, includedNamedSessions, mergedProviders, mergedUpstreams, includedServices, topoDirs, allRequires, allGlobals, nil
}

func clonePackLoadResult(in *packLoadResult) *packLoadResult {
	if in == nil {
		return nil
	}
	return &packLoadResult{
		agents:         deepCopyAgents(in.agents),
		namedSessions:  deepCopyNamedSessions(in.namedSessions),
		providers:      deepCopyProviderSpecs(in.providers),
		localProviders: deepCopyProviderSpecs(in.localProviders),
		upstreams:      deepCopyUpstreamSpecs(in.upstreams),
		localUpstreams: deepCopyUpstreamSpecs(in.localUpstreams),
		services:       deepCopyServices(in.services),
		webhooks:       deepCopyWebhooks(in.webhooks),
		topoDirs:       append([]string(nil), in.topoDirs...),
		localTopoDirs:  append([]string(nil), in.localTopoDirs...),
		requires:       append([]PackRequirement(nil), in.requires...),
		localRequires:  append([]PackRequirement(nil), in.localRequires...),
		globals:        deepCopyResolvedPackGlobals(in.globals),
		localGlobals:   deepCopyResolvedPackGlobals(in.localGlobals),
		commands:       deepCopyCommands(in.commands),
		doctors:        deepCopyDoctors(in.doctors),
		runtimes:       append([]DiscoveredRuntime(nil), in.runtimes...),
		skills:         deepCopySkills(in.skills),
		localWarnings:  append([]string(nil), in.localWarnings...),
		warnings:       append([]string(nil), in.warnings...),
	}
}

func deepCopyAgents(in []Agent) []Agent {
	out := make([]Agent, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Args = append([]string(nil), in[i].Args...)
		out[i].PreStart = append([]string(nil), in[i].PreStart...)
		out[i].ProcessNames = append([]string(nil), in[i].ProcessNames...)
		out[i].Env = deepCopyStringMap(in[i].Env)
		out[i].OptionDefaults = deepCopyStringMap(in[i].OptionDefaults)
		out[i].NamepoolNames = append([]string(nil), in[i].NamepoolNames...)
		out[i].InstallAgentHooks = append([]string(nil), in[i].InstallAgentHooks...)
		out[i].SessionSetup = append([]string(nil), in[i].SessionSetup...)
		out[i].SessionLive = append([]string(nil), in[i].SessionLive...)
		out[i].InjectFragments = append([]string(nil), in[i].InjectFragments...)
		out[i].AppendFragments = append([]string(nil), in[i].AppendFragments...)
		out[i].DependsOn = append([]string(nil), in[i].DependsOn...)
		out[i].MaxActiveSessions = copyIntPtr(in[i].MaxActiveSessions)
		out[i].MinActiveSessions = copyIntPtr(in[i].MinActiveSessions)
		out[i].ReadyDelayMs = copyIntPtr(in[i].ReadyDelayMs)
		out[i].EmitsPermissionWarning = copyBoolPtr(in[i].EmitsPermissionWarning)
		out[i].HooksInstalled = copyBoolPtr(in[i].HooksInstalled)
		out[i].InjectAssignedSkills = copyBoolPtr(in[i].InjectAssignedSkills)
		out[i].DefaultSlingFormula = copyStringPtr(in[i].DefaultSlingFormula)
		out[i].InheritedDefaultSlingFormula = copyStringPtr(in[i].InheritedDefaultSlingFormula)
		out[i].InheritedAppendFragments = append([]string(nil), in[i].InheritedAppendFragments...)
		out[i].Attach = copyBoolPtr(in[i].Attach)
	}
	return out
}

func deepCopyNamedSessions(in []NamedSession) []NamedSession {
	out := make([]NamedSession, len(in))
	copy(out, in)
	return out
}

func deepCopyServices(in []Service) []Service {
	out := make([]Service, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Process.Command = append([]string(nil), in[i].Process.Command...)
	}
	return out
}

func deepCopyProviderSpecs(in map[string]ProviderSpec) map[string]ProviderSpec {
	if in == nil {
		return nil
	}
	out := make(map[string]ProviderSpec, len(in))
	for name, spec := range in {
		out[name] = deepCopyProviderSpec(spec)
	}
	return out
}

func deepCopyUpstreamSpecs(in map[string]UpstreamSpec) map[string]UpstreamSpec {
	if in == nil {
		return nil
	}
	out := make(map[string]UpstreamSpec, len(in))
	for name, spec := range in {
		spec.Env = deepCopyStringMap(spec.Env)
		out[name] = spec
	}
	return out
}

func deepCopyProviderSpec(in ProviderSpec) ProviderSpec {
	out := in
	out.Args = append([]string(nil), in.Args...)
	out.ArgsAppend = append([]string(nil), in.ArgsAppend...)
	out.ProcessNames = append([]string(nil), in.ProcessNames...)
	out.Env = deepCopyStringMap(in.Env)
	out.PermissionModes = deepCopyStringMap(in.PermissionModes)
	out.OptionDefaults = deepCopyStringMap(in.OptionDefaults)
	out.OptionsSchema = deepCopyProviderOptions(in.OptionsSchema)
	out.PrintArgs = append([]string(nil), in.PrintArgs...)
	if in.ACPArgs != nil {
		out.ACPArgs = make([]string, len(in.ACPArgs))
		copy(out.ACPArgs, in.ACPArgs)
	}
	out.Base = copyStringPtr(in.Base)
	out.EmitsPermissionWarning = copyBoolPtr(in.EmitsPermissionWarning)
	out.SupportsACP = copyBoolPtr(in.SupportsACP)
	out.SupportsHooks = copyBoolPtr(in.SupportsHooks)
	return out
}

func deepCopyProviderOptions(in []ProviderOption) []ProviderOption {
	out := make([]ProviderOption, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Choices = deepCopyOptionChoices(in[i].Choices)
	}
	return out
}

func deepCopyOptionChoices(in []OptionChoice) []OptionChoice {
	out := make([]OptionChoice, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].FlagArgs = append([]string(nil), in[i].FlagArgs...)
		out[i].FlagAliases = cloneStringSlices(in[i].FlagAliases)
	}
	return out
}

func deepCopyResolvedPackGlobals(in []ResolvedPackGlobal) []ResolvedPackGlobal {
	out := make([]ResolvedPackGlobal, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].SessionLive = append([]string(nil), in[i].SessionLive...)
	}
	return out
}

func deepCopyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyIntPtr(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func copyBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func copyStringPtr(in *string) *string {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func applyInheritedPackAgentDefaults(agents []Agent, defaults AgentDefaults) {
	for i := range agents {
		if agents[i].BindingName != "" {
			continue
		}
		// Includes compose from the inside out: once an included agent has
		// inherited a scalar default, outer packs do not replace it.
		if defaults.Provider != "" && agents[i].Provider == "" && agents[i].InheritedProvider == "" {
			agents[i].InheritedProvider = defaults.Provider
		}
		if defaults.DefaultSlingFormula != "" && agents[i].DefaultSlingFormula == nil && agents[i].InheritedDefaultSlingFormula == nil {
			agents[i].InheritedDefaultSlingFormula = copyStringPtr(&defaults.DefaultSlingFormula)
		}
		if len(defaults.AppendFragments) > 0 {
			agents[i].InheritedAppendFragments = appendUnique(agents[i].InheritedAppendFragments, defaults.AppendFragments...)
		}
	}
}

func cachedPackCommands(cache *packLoadCache, topoDir string) []DiscoveredCommand {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	out := deepCopyCommands(result.commands)
	return out
}

func cachedPackWarnings(cache *packLoadCache, topoDir string) []string {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return append([]string(nil), result.warnings...)
}

func cachedPackLocalWarnings(cache *packLoadCache, topoDir string) []string {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return append([]string(nil), result.localWarnings...)
}

func cachedPackLocalProviders(cache *packLoadCache, topoDir string) map[string]ProviderSpec {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return deepCopyProviderSpecs(result.localProviders)
}

func cachedPackLocalUpstreams(cache *packLoadCache, topoDir string) map[string]UpstreamSpec {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return deepCopyUpstreamSpecs(result.localUpstreams)
}

func cachedPackLocalTopoDirs(cache *packLoadCache, topoDir string) []string {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return append([]string(nil), result.localTopoDirs...)
}

func cachedPackLocalRequires(cache *packLoadCache, topoDir string) []PackRequirement {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return append([]PackRequirement(nil), result.localRequires...)
}

func cachedPackLocalGlobals(cache *packLoadCache, topoDir string) []ResolvedPackGlobal {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return deepCopyResolvedPackGlobals(result.localGlobals)
}

func filterNamedSessionsBySourceDir(namedSessions []NamedSession, sourceDir string) []NamedSession {
	if len(namedSessions) == 0 {
		return nil
	}
	absWant, _ := filepath.Abs(sourceDir)
	var out []NamedSession
	for _, named := range namedSessions {
		absDir, _ := filepath.Abs(named.SourceDir)
		if absDir == absWant {
			out = append(out, named)
		}
	}
	return out
}

func cachedPackDoctors(cache *packLoadCache, topoDir string) []DiscoveredDoctor {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	out := deepCopyDoctors(result.doctors)
	return out
}

// isOSFileSystem reports whether fs is the real operating-system
// filesystem. Bundled builtin pack content only exists there (embedded in
// the binary, served via the user-global cache), so non-OS loads skip
// bundled imports.
func isOSFileSystem(fs fsys.FS) bool {
	switch fs.(type) {
	case fsys.OSFS, *fsys.OSFS:
		return true
	default:
		return false
	}
}

func cachedPackSkills(cache *packLoadCache, topoDir string) []DiscoveredSkillCatalog {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return deepCopySkills(result.skills)
}

func filterCommandsByPackDir(commands []DiscoveredCommand, packDir string) []DiscoveredCommand {
	absPackDir, _ := filepath.Abs(packDir)
	var out []DiscoveredCommand
	for _, cmd := range commands {
		absDir, _ := filepath.Abs(cmd.PackDir)
		if absDir == absPackDir {
			out = append(out, cmd)
		}
	}
	return out
}

func filterServicesBySourceDir(services []Service, sourceDir string) []Service {
	absSource, _ := filepath.Abs(sourceDir)
	var out []Service
	for _, service := range services {
		absDir, _ := filepath.Abs(service.SourceDir)
		if absDir == absSource || strings.HasPrefix(absDir, absSource+string(filepath.Separator)) {
			out = append(out, service)
		}
	}
	return out
}

func filterDoctorsByPackDir(doctors []DiscoveredDoctor, packDir string) []DiscoveredDoctor {
	absPackDir, _ := filepath.Abs(packDir)
	var out []DiscoveredDoctor
	for _, check := range doctors {
		absDir, _ := filepath.Abs(check.PackDir)
		if absDir == absPackDir {
			out = append(out, check)
		}
	}
	return out
}

func filterSkillsByPackDir(skills []DiscoveredSkillCatalog, packDir string) []DiscoveredSkillCatalog {
	absPackDir, _ := filepath.Abs(packDir)
	var out []DiscoveredSkillCatalog
	for _, skill := range skills {
		absDir, _ := filepath.Abs(skill.PackDir)
		if absDir == absPackDir {
			out = append(out, skill)
		}
	}
	return out
}

func filterPackDirsByRoot(packDirs []string, rootDir string) []string {
	absRoot, _ := filepath.Abs(rootDir)
	var out []string
	for _, dir := range packDirs {
		absDir, _ := filepath.Abs(dir)
		if absDir == absRoot {
			out = append(out, dir)
		}
	}
	return out
}

func stampMCPDirBindings(dst map[string]string, packDirs []string, binding string) map[string]string {
	if len(packDirs) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, len(packDirs))
	}
	for _, dir := range packDirs {
		if _, exists := dst[dir]; exists {
			continue
		}
		dst[dir] = binding
	}
	return dst
}

func agentNameSet(agents []Agent) map[string]bool {
	names := make(map[string]bool, len(agents))
	for _, a := range agents {
		names[a.Name] = true
	}
	return names
}

func appendDiscoveredCommands(dst []DiscoveredCommand, src ...DiscoveredCommand) []DiscoveredCommand {
	for _, cmd := range src {
		duplicate := false
		for _, existing := range dst {
			if slices.Equal(existing.Command, cmd.Command) &&
				existing.BindingName == cmd.BindingName &&
				existing.RunScript == cmd.RunScript {
				duplicate = true
				break
			}
		}
		if !duplicate {
			dst = append(dst, cmd)
		}
	}
	return dst
}

func appendDiscoveredDoctors(dst []DiscoveredDoctor, src ...DiscoveredDoctor) []DiscoveredDoctor {
	for _, check := range src {
		duplicateIdx := -1
		for i, existing := range dst {
			if existing.Name == check.Name &&
				existing.BindingName == check.BindingName &&
				existing.RunScript == check.RunScript {
				duplicateIdx = i
				break
			}
		}
		if duplicateIdx < 0 {
			dst = append(dst, check)
			continue
		}
		// Duplicate detected (same Name + BindingName + RunScript). Merge
		// complementary metadata so a richer source doesn't lose out to an
		// earlier-appended sparse one. Specifically: a convention-discovered
		// entry that lacks an explicit `fix` manifest still wins on Name
		// dedup against a legacy [[doctor]] TOML entry for the same check
		// that declares `fix = "..."`. Without this merge, CanFix would
		// spuriously return false on the winning entry.
		if dst[duplicateIdx].FixScript == "" && check.FixScript != "" {
			dst[duplicateIdx].FixScript = check.FixScript
		}
	}
	return dst
}

func appendDiscoveredSkills(dst []DiscoveredSkillCatalog, src ...DiscoveredSkillCatalog) []DiscoveredSkillCatalog {
	for _, skill := range src {
		duplicate := false
		for _, existing := range dst {
			if existing.SourceDir == skill.SourceDir && existing.BindingName == skill.BindingName {
				duplicate = true
				break
			}
		}
		if !duplicate {
			dst = append(dst, skill)
		}
	}
	return dst
}

func stampDefaultBinding(commands []DiscoveredCommand, defaultBinding string) []DiscoveredCommand {
	out := deepCopyCommands(commands)
	for i := range out {
		if out[i].BindingName == "" {
			out[i].BindingName = defaultBinding
		}
	}
	return out
}

func stampSkillBinding(skills []DiscoveredSkillCatalog, bindingName string) []DiscoveredSkillCatalog {
	out := deepCopySkills(skills)
	for i := range out {
		if out[i].BindingName == "" {
			out[i].BindingName = bindingName
		}
	}
	return out
}

func stampImportedSkillBinding(skills []DiscoveredSkillCatalog, bindingName string, export bool) []DiscoveredSkillCatalog {
	out := deepCopySkills(skills)
	for i := range out {
		if out[i].BindingName == "" || export {
			out[i].BindingName = bindingName
		}
	}
	return out
}

func deepCopyCommands(in []DiscoveredCommand) []DiscoveredCommand {
	out := make([]DiscoveredCommand, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].Command != nil {
			out[i].Command = append([]string{}, in[i].Command...)
		}
	}
	return out
}

func deepCopyDoctors(in []DiscoveredDoctor) []DiscoveredDoctor {
	out := make([]DiscoveredDoctor, len(in))
	copy(out, in)
	return out
}

func deepCopySkills(in []DiscoveredSkillCatalog) []DiscoveredSkillCatalog {
	out := make([]DiscoveredSkillCatalog, len(in))
	copy(out, in)
	return out
}

func tcPackName(fs fsys.FS, topoPath string) string {
	data, err := fs.ReadFile(topoPath)
	if err != nil {
		return ""
	}
	var meta struct {
		Pack struct {
			Name string `toml:"name"`
		} `toml:"pack"`
	}
	if _, err := toml.Decode(string(data), &meta); err != nil {
		return ""
	}
	return meta.Pack.Name
}

func legacyPackCommands(entries []PackCommandEntry, packDir, packName string) []DiscoveredCommand {
	out := make([]DiscoveredCommand, 0, len(entries))
	for _, entry := range entries {
		runScript := entry.Script
		if runScript != "" && !filepath.IsAbs(runScript) && !strings.Contains(runScript, "{{") {
			runScript = filepath.Join(packDir, runScript)
		}

		helpFile := ""
		if entry.LongDescription != "" {
			helpFile = entry.LongDescription
			if !filepath.IsAbs(helpFile) {
				helpFile = filepath.Join(packDir, helpFile)
			}
		}

		out = append(out, DiscoveredCommand{
			Name:        entry.Name,
			Command:     []string{entry.Name},
			Description: entry.Description,
			RunScript:   runScript,
			HelpFile:    helpFile,
			SourceDir:   packDir,
			PackDir:     packDir,
			PackName:    packName,
		})
	}
	return out
}

func legacyPackDoctors(fs fsys.FS, entries []PackDoctorEntry, packDir, packName string) ([]DiscoveredDoctor, error) {
	out := make([]DiscoveredDoctor, 0, len(entries))
	for _, entry := range entries {
		runScript := entry.Script
		if runScript != "" && !filepath.IsAbs(runScript) {
			runScript = filepath.Join(packDir, runScript)
		}

		fixScript := entry.Fix
		if fixScript != "" {
			resolved, err := resolveContainedDoctorFixPath(packDir, packDir, fixScript)
			if err != nil {
				return nil, fmt.Errorf("doctor %s fix: %w", entry.Name, err)
			}
			if _, err := fs.Stat(resolved); err != nil {
				return nil, fmt.Errorf("doctor %s fix %q: %w", entry.Name, fixScript, err)
			}
			fixScript = resolved
		}

		out = append(out, DiscoveredDoctor{
			Name:        entry.Name,
			Description: entry.Description,
			RunScript:   runScript,
			FixScript:   fixScript,
			SourceDir:   packDir,
			PackDir:     packDir,
			PackName:    packName,
			Warmup:      entry.Warmup,
		})
	}
	return out, nil
}

// applyPackGlobals appends [global].session_live commands from packs
// to matching agents. City-level globals affect ALL agents. Rig-level
// globals affect only agents in that rig.
func applyPackGlobals(cfg *City) {
	applied := make([]map[string]bool, len(cfg.Agents))
	apply := func(agentIndex int, g ResolvedPackGlobal) {
		if g.PackName != "" {
			if applied[agentIndex] == nil {
				applied[agentIndex] = make(map[string]bool)
			}
			if applied[agentIndex][g.PackName] {
				return
			}
			applied[agentIndex][g.PackName] = true
		}
		cfg.Agents[agentIndex].SessionLive = append(
			cfg.Agents[agentIndex].SessionLive, g.SessionLive...)
	}

	// City-level globals → all agents.
	for _, g := range cfg.PackGlobals {
		for i := range cfg.Agents {
			apply(i, g)
		}
	}
	// Rig-level globals → only that rig's agents.
	for rigName, globals := range cfg.RigPackGlobals {
		for _, g := range globals {
			for i := range cfg.Agents {
				if cfg.Agents[i].Dir == rigName {
					apply(i, g)
				}
			}
		}
	}
}

// resolveConfigDirInCommands replaces {{.ConfigDir}} in each command with
// the concrete pack directory path. Other template variables ({{.Session}},
// {{.Agent}}, etc.) are left as-is for per-agent expansion later.
func resolveConfigDirInCommands(cmds []string, configDir string) []string {
	result := make([]string, len(cmds))
	for i, cmd := range cmds {
		result[i] = strings.ReplaceAll(cmd, "{{.ConfigDir}}", configDir)
	}
	return result
}

// adjustPackPatchPaths resolves file-path fields in patches relative to
// the pack directory. session_setup_script is resolved all the way to an
// absolute path because patches do not retain independent source provenance
// after application; prompt_template and overlay_dir keep the existing
// city-root-relative representation used elsewhere in composition.
func adjustPackPatchPaths(patches *PackPatches, topoDir, cityRoot string) {
	for i := range patches.Agents {
		p := &patches.Agents[i]
		if p.SessionSetupScript != nil && *p.SessionSetupScript != "" {
			v := resolveConfigPath(*p.SessionSetupScript, topoDir, cityRoot)
			p.SessionSetupScript = &v
		}
		if p.PromptTemplate != nil && *p.PromptTemplate != "" {
			v := adjustFragmentPath(*p.PromptTemplate, topoDir, cityRoot)
			p.PromptTemplate = &v
		}
		if p.OverlayDir != nil && *p.OverlayDir != "" {
			v := adjustFragmentPath(*p.OverlayDir, topoDir, cityRoot)
			p.OverlayDir = &v
		}
	}
}

// applyPackAgentPatches applies agent patches to a merged agent slice.
// When a patch has Dir == "", it matches by Name alone — this is the
// normal case for pack authors who don't know which rig will use their
// pack (agents are rig-stamped during recursive loadPack before patches
// run). When Dir is set, both Dir and Name must match.
// Returns an error if a patch targets a nonexistent agent.
func applyPackAgentPatches(agents []Agent, patches []AgentPatch) error {
	for i, p := range patches {
		target := qualifiedNameFromPatch(p.Dir, p.Name)
		found := false
		for j := range agents {
			if p.Dir == "" {
				// Name-only match: pack patches don't know the rig name.
				if agents[j].Name == p.Name {
					applyAgentPatchFields(&agents[j], &patches[i])
					found = true
					break
				}
			} else {
				if agents[j].Dir == p.Dir && agents[j].Name == p.Name {
					applyAgentPatchFields(&agents[j], &patches[i])
					found = true
					break
				}
			}
		}
		if !found {
			return fmt.Errorf("patches.agent[%d]: agent %q not found in pack", i, target)
		}
	}
	return nil
}

// validatePackMeta checks the [pack] header for required fields
// and schema compatibility.
func validatePackMeta(meta *PackMeta) error {
	if meta.Name == "" {
		return fmt.Errorf("[pack] name is required")
	}
	if meta.Schema == 0 {
		return fmt.Errorf("[pack] schema is required")
	}
	if meta.Schema > currentPackSchema {
		return fmt.Errorf("[pack] schema %d not supported (max %d)", meta.Schema, currentPackSchema)
	}
	for i, req := range meta.Requires {
		if req.Agent == "" {
			return fmt.Errorf("[pack] requires[%d]: agent is required", i)
		}
		if req.Scope != "city" && req.Scope != "rig" {
			return fmt.Errorf("[pack] requires[%d]: scope must be \"city\" or \"rig\", got %q", i, req.Scope)
		}
	}
	return nil
}

// appendUnique appends items to dst, skipping any already present.
func appendUnique(dst []string, items ...string) []string {
	seen := setFromSlice(dst)
	for _, item := range items {
		if !seen[item] {
			dst = append(dst, item)
			seen[item] = true
		}
	}
	return dst
}

// appendUniqueLastWins appends items to dst while keeping only the
// highest-precedence occurrence of each path. Re-seeing an item moves it to the
// end of the slice.
func appendUniqueLastWins(dst []string, items ...string) []string {
	for _, item := range items {
		filtered := dst[:0]
		for _, existing := range dst {
			if existing == item {
				continue
			}
			filtered = append(filtered, existing)
		}
		dst = filtered
		dst = append(dst, item)
	}
	return dst
}

// prependUniqueBlock prepends one precedence block ahead of dst, keeping the
// first insertion of any shared path. This lets earlier processed root bindings
// retain ownership of shared dependency dirs while still placing later sibling
// roots at lower precedence under later-wins merges.
func prependUniqueBlock(dst []string, items ...string) []string {
	if len(items) == 0 {
		return dst
	}
	out := make([]string, 0, len(items)+len(dst))
	added := setFromSlice(dst)
	for _, item := range items {
		if added[item] {
			continue
		}
		out = append(out, item)
		added[item] = true
	}
	out = append(out, dst...)
	return out
}

// setFromSlice builds a set from a string slice.
func setFromSlice(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// filterAgentsByScope filters agents based on their scope and the expansion
// context. If cityExpansion is true, keeps city-scoped and unscoped agents.
// If false, keeps rig-scoped and unscoped agents.
func filterAgentsByScope(agents []Agent, cityExpansion bool) []Agent {
	var result []Agent
	for _, a := range agents {
		switch a.Scope {
		case "city":
			if cityExpansion {
				result = append(result, a)
			}
		case "rig":
			if !cityExpansion {
				result = append(result, a)
			}
		default: // "" — unscoped, include in both contexts
			result = append(result, a)
		}
	}
	return result
}

func filterNamedSessionsByScope(sessions []NamedSession, cityExpansion bool) []NamedSession {
	var result []NamedSession
	for _, s := range sessions {
		switch s.Scope {
		case "city":
			if cityExpansion {
				result = append(result, s)
			}
		case "rig":
			if !cityExpansion {
				result = append(result, s)
			}
		default:
			result = append(result, s)
		}
	}
	return result
}

func expandCityImportedAgentsForRigs(agents []Agent, rigs []Rig, bindingName string) []Agent {
	if len(agents) == 0 || len(rigs) == 0 {
		return nil
	}
	var expanded []Agent
	for _, rig := range rigs {
		rigName := strings.TrimSpace(rig.Name)
		if rigName == "" || rigDeclaresImportBinding(rig, bindingName) {
			continue
		}
		for _, a := range agents {
			if a.Scope == "city" {
				continue
			}
			a.Dir = rigName
			// Clone DependsOn before qualifying in place: the range copy shares
			// the original slice's backing array, so an in-place rewrite would
			// poison the city-scoped copies filtered afterward and lock the
			// first rig's prefix onto every later rig.
			a.DependsOn = append([]string(nil), a.DependsOn...)
			qualifyAgentDependsOnInPlace(&a)
			expanded = append(expanded, a)
		}
	}
	return expanded
}

func qualifyAgentDependsOnInPlace(a *Agent) {
	if a == nil || len(a.DependsOn) == 0 {
		return
	}
	for i, dep := range a.DependsOn {
		dep = strings.TrimSpace(dep)
		if dep == "" || strings.Contains(dep, "/") {
			continue
		}
		binding := strings.TrimSpace(a.BindingName)
		if !strings.Contains(dep, ".") {
			if binding != "" {
				dep = binding + "." + dep
			}
		} else if binding != "" && !strings.HasPrefix(dep, binding+".") {
			continue
		}
		if a.Dir != "" {
			dep = a.Dir + "/" + dep
		}
		a.DependsOn[i] = dep
	}
}

func expandCityImportedNamedSessionsForRigs(sessions []NamedSession, rigs []Rig, bindingName string) []NamedSession {
	if len(sessions) == 0 || len(rigs) == 0 {
		return nil
	}
	var expanded []NamedSession
	for _, rig := range rigs {
		rigName := strings.TrimSpace(rig.Name)
		if rigName == "" || rigDeclaresImportBinding(rig, bindingName) {
			continue
		}
		for _, s := range sessions {
			if s.Scope == "city" {
				continue
			}
			s.Dir = rigName
			expanded = append(expanded, s)
		}
	}
	return expanded
}

func rigDeclaresImportBinding(rig Rig, bindingName string) bool {
	bindingName = strings.TrimSpace(bindingName)
	if bindingName == "" || len(rig.Imports) == 0 {
		return false
	}
	_, ok := rig.Imports[bindingName]
	return ok
}

// hoistCityScopedAgents returns copies of the city-scoped agents in the
// given slice, restamped for city scope (Dir cleared — it was stamped to the
// rig name during pack load). Used at rig include/import boundaries so a
// city-scoped agent that lives in a rig-included pack is hoisted to city
// scope instead of being silently dropped. BindingName is preserved so a
// city-scoped agent imported under a binding keeps its qualified identity.
func hoistCityScopedAgents(agents []Agent) []Agent {
	var hoisted []Agent
	for _, a := range agents {
		if a.Scope != "city" {
			continue
		}
		a.Dir = ""
		hoisted = append(hoisted, a)
	}
	return hoisted
}

// hoistCityScopedNamedSessions mirrors hoistCityScopedAgents for named
// sessions.
func hoistCityScopedNamedSessions(sessions []NamedSession) []NamedSession {
	var hoisted []NamedSession
	for _, s := range sessions {
		if s.Scope != "city" {
			continue
		}
		s.Dir = ""
		hoisted = append(hoisted, s)
	}
	return hoisted
}

// mergeHoistedCityAgents appends hoisted city-scoped agents to the city
// agent set, skipping any whose qualified name is already present (from
// city-scope expansion, a city-root agent, or an earlier hoist of the same
// agent via another rig). First occurrence wins, so an existing city-scope
// or city-root definition is preferred over a hoisted one. Identical
// definitions reached through multiple rigs register exactly once.
func mergeHoistedCityAgents(agents, hoisted []Agent) []Agent {
	if len(hoisted) == 0 {
		return agents
	}
	seenQN := make(map[string]bool, len(agents))
	seenDirName := make(map[[2]string]bool, len(agents))
	for i := range agents {
		seenQN[agents[i].QualifiedName()] = true
		seenDirName[[2]string{agents[i].Dir, agents[i].Name}] = true
	}
	for _, a := range hoisted {
		qn := a.QualifiedName()
		dn := [2]string{a.Dir, a.Name}
		if seenQN[qn] || seenDirName[dn] {
			continue
		}
		seenQN[qn] = true
		seenDirName[dn] = true
		agents = append(agents, a)
	}
	return agents
}

// mergeHoistedCityNamedSessions mirrors mergeHoistedCityAgents for named
// sessions.
func mergeHoistedCityNamedSessions(sessions, hoisted []NamedSession) []NamedSession {
	if len(hoisted) == 0 {
		return sessions
	}
	seenQN := make(map[string]bool, len(sessions))
	seenTpl := make(map[[2]string]bool, len(sessions))
	for i := range sessions {
		seenQN[sessions[i].QualifiedName()] = true
		seenTpl[[2]string{sessions[i].Dir, sessions[i].Template}] = true
	}
	for _, s := range hoisted {
		qn := s.QualifiedName()
		tpl := [2]string{s.Dir, s.Template}
		if seenQN[qn] || seenTpl[tpl] {
			continue
		}
		seenQN[qn] = true
		seenTpl[tpl] = true
		sessions = append(sessions, s)
	}
	return sessions
}

func applyDeferredRigPatches(cfg *City, deferred []deferredRigPatches) error {
	for _, d := range deferred {
		if d.agentStart < 0 || d.agentEnd < d.agentStart || d.agentEnd > len(cfg.Agents) {
			return fmt.Errorf("rig %q: deferred agent range [%d:%d] outside merged agents", d.rigName, d.agentStart, d.agentEnd)
		}
		if len(cfg.Agents) != d.expectedAgentCount {
			return fmt.Errorf("rig %q: merged agent count changed before deferred rig patches: got %d, want %d", d.rigName, len(cfg.Agents), d.expectedAgentCount)
		}
		if len(d.expectedAgentNames) != d.agentEnd-d.agentStart {
			return fmt.Errorf("rig %q: deferred agent range [%d:%d] has %d identity snapshots", d.rigName, d.agentStart, d.agentEnd, len(d.expectedAgentNames))
		}
		for i, want := range d.expectedAgentNames {
			got := cfg.Agents[d.agentStart+i].QualifiedName()
			if got != want {
				return fmt.Errorf("rig %q: agent at deferred range index %d changed before deferred rig patches: got %q, want %q", d.rigName, d.agentStart+i, got, want)
			}
		}
		if err := applyOverrides(cfg.Agents[d.agentStart:d.agentEnd], d.overrides, d.rigName); err != nil {
			return fmt.Errorf("rig %q: %w", d.rigName, err)
		}
	}
	return nil
}

func qualifiedAgentNames(agents []Agent) []string {
	names := make([]string, 0, len(agents))
	for i := range agents {
		names = append(names, agents[i].QualifiedName())
	}
	return names
}

// applyOverrides applies per-rig overrides to pack-stamped agents.
// Each override targets an agent by name within the pack.
func applyOverrides(agents []Agent, overrides []AgentOverride, _ string) error {
	for i, ov := range overrides {
		if ov.Agent == "" {
			return fmt.Errorf("overrides[%d]: agent name is required", i)
		}
		found := false
		for j := range agents {
			if agents[j].Name == ov.Agent {
				applyAgentOverride(&agents[j], &ov)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("overrides[%d]: agent %q not found in pack", i, ov.Agent)
		}
	}
	return nil
}

// applyAgentOverride applies a single override to an agent.
func applyAgentOverride(a *Agent, ov *AgentOverride) {
	if ov.Dir != nil {
		a.Dir = *ov.Dir
	}
	if ov.WorkDir != nil {
		a.WorkDir = *ov.WorkDir
	}
	if ov.TmuxAlias != nil {
		a.TmuxAlias = *ov.TmuxAlias
	}
	if ov.Scope != nil {
		a.Scope = *ov.Scope
	}
	if ov.Suspended != nil {
		a.Suspended = *ov.Suspended
	}
	if len(ov.PreStart) > 0 {
		a.PreStart = append([]string(nil), ov.PreStart...)
	}
	if len(ov.PreStartAppend) > 0 {
		a.PreStart = append(a.PreStart, ov.PreStartAppend...)
	}
	if ov.PromptTemplate != nil {
		a.PromptTemplate = *ov.PromptTemplate
	}
	if ov.Session != nil {
		a.Session = *ov.Session
	}
	if ov.Provider != nil {
		a.Provider = *ov.Provider
	}
	if ov.Upstream != nil {
		a.Upstream = *ov.Upstream
	}
	if ov.Args != nil {
		a.Args = append([]string(nil), (*ov.Args)...)
	}
	if ov.StartCommand != nil {
		a.StartCommand = *ov.StartCommand
	}
	if ov.Lifecycle != nil {
		a.Lifecycle = *ov.Lifecycle
	}
	if ov.Nudge != nil {
		a.Nudge = *ov.Nudge
	}
	if ov.IdleTimeout != nil {
		a.IdleTimeout = *ov.IdleTimeout
	}
	if ov.MaxSessionAge != nil {
		a.MaxSessionAge = *ov.MaxSessionAge
	}
	if ov.MaxSessionAgeJitter != nil {
		a.MaxSessionAgeJitter = *ov.MaxSessionAgeJitter
	}
	if ov.SleepAfterIdle != nil {
		a.SleepAfterIdle = NormalizeSleepAfterIdle(*ov.SleepAfterIdle)
		a.SleepAfterIdleSource = "rig_override"
	}
	if len(ov.InstallAgentHooks) > 0 {
		a.InstallAgentHooks = append([]string(nil), ov.InstallAgentHooks...)
	}
	if len(ov.InstallAgentHooksAppend) > 0 {
		a.InstallAgentHooks = append(a.InstallAgentHooks, ov.InstallAgentHooksAppend...)
	}
	if ov.HooksInstalled != nil {
		a.HooksInstalled = ov.HooksInstalled
	}
	if ov.InjectAssignedSkills != nil {
		a.InjectAssignedSkills = ov.InjectAssignedSkills
	}
	if len(ov.SessionSetup) > 0 {
		a.SessionSetup = append([]string(nil), ov.SessionSetup...)
	}
	if len(ov.SessionSetupAppend) > 0 {
		a.SessionSetup = append(a.SessionSetup, ov.SessionSetupAppend...)
	}
	if ov.SessionSetupScript != nil {
		a.SessionSetupScript = *ov.SessionSetupScript
	}
	if len(ov.SessionLive) > 0 {
		a.SessionLive = append([]string(nil), ov.SessionLive...)
	}
	if len(ov.SessionLiveAppend) > 0 {
		a.SessionLive = append(a.SessionLive, ov.SessionLiveAppend...)
	}
	if ov.OverlayDir != nil {
		a.OverlayDir = *ov.OverlayDir
	}
	if ov.DefaultSlingFormula != nil {
		a.DefaultSlingFormula = ov.DefaultSlingFormula
	}
	if ov.Attach != nil {
		a.Attach = ov.Attach
	}
	if len(ov.DependsOn) > 0 {
		a.DependsOn = append([]string(nil), ov.DependsOn...)
	}
	if ov.ResumeCommand != nil {
		a.ResumeCommand = *ov.ResumeCommand
	}
	if ov.WakeMode != nil {
		a.WakeMode = *ov.WakeMode
	}
	if ov.MouseMode != nil {
		a.MouseMode = *ov.MouseMode
	}
	if ov.InjectFragments != nil {
		a.InjectFragments = append([]string(nil), (*ov.InjectFragments)...)
	}
	if len(ov.AppendFragments) > 0 {
		a.AppendFragments = append([]string(nil), ov.AppendFragments...)
	}
	if len(ov.InjectFragmentsAppend) > 0 {
		a.InjectFragments = append(a.InjectFragments, ov.InjectFragmentsAppend...)
	}
	if ov.MaxActiveSessions != nil {
		a.MaxActiveSessions = ov.MaxActiveSessions
	}
	if ov.MinActiveSessions != nil {
		a.MinActiveSessions = ov.MinActiveSessions
	}
	if ov.ScaleCheck != nil {
		a.ScaleCheck = *ov.ScaleCheck
	}
	// Env: additive merge.
	if len(ov.Env) > 0 {
		if a.Env == nil {
			a.Env = make(map[string]string, len(ov.Env))
		}
		for k, v := range ov.Env {
			a.Env[k] = v
		}
	}
	for _, k := range ov.EnvRemove {
		delete(a.Env, k)
	}
	// OptionDefaults: additive merge (override keys win).
	if len(ov.OptionDefaults) > 0 {
		if a.OptionDefaults == nil {
			a.OptionDefaults = make(map[string]string, len(ov.OptionDefaults))
		}
		for k, v := range ov.OptionDefaults {
			a.OptionDefaults[k] = v
		}
	}
	// Pool: sub-field patching.
	if ov.Pool != nil {
		applyPoolOverride(a, ov.Pool)
	}
}

// PackContentHash computes a SHA-256 hash of all files in a pack
// directory. The hash is deterministic (sorted filenames). Returns empty
// string if the directory cannot be read.
func PackContentHash(fs fsys.FS, topoDir string) string {
	entries, err := fs.ReadDir(topoDir)
	if err != nil {
		return ""
	}

	// Collect all file paths (non-recursive for now).
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		paths = append(paths, e.Name())
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, name := range paths {
		data, err := fs.ReadFile(filepath.Join(topoDir, name))
		if err != nil {
			continue
		}
		h.Write([]byte(name)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
		h.Write(data)         //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// PackContentHashRecursive computes a SHA-256 hash of all files in a
// pack directory, recursively descending into subdirectories. File
// paths are sorted for determinism and include the relative path from
// topoDir.
// packContentHashCache memoizes PackContentHashRecursive across calls. The
// revision-snapshot capture (revision.go) hashes the full content of every pack
// tree referenced by the city and every rig on every reconcile tick; with many
// rigs sharing the same packs this re-reads and re-SHA256s the same trees many
// times per tick and again every patrol, even though packs almost never change
// between ticks. This was the dominant supervisor CPU cost once the dolt
// connection churn was eliminated (gastownhall/gascity#1978 follow-up).
//
// The cache keys the content hash by absolute pack dir plus a cheap stat
// fingerprint (per-file size+mtime, no content reads). An unchanged tree is
// content-hashed once and reused — both for repeats within a single tick and
// across ticks. Invalidation follows standard build-cache semantics: any file
// add/remove, size change, or mtime bump (every normal edit and git checkout)
// changes the fingerprint and forces a re-hash. The only blind spot is an edit
// that preserves both size and mtime, which pack tooling does not do.
var packContentHashCache sync.Map // absDir(string) -> packContentHashEntry

type packContentHashEntry struct {
	fingerprint uint64
	hash        string
}

// ResetPackContentHashCache clears the memoized pack content hashes. Tests that
// mutate a pack tree in place under a path a previous test already hashed call
// this to avoid cross-test cache bleed.
func ResetPackContentHashCache() {
	packContentHashCache.Range(func(k, _ any) bool {
		packContentHashCache.Delete(k)
		return true
	})
}

// PackContentHashRecursive returns a stable content hash of every file under
// topoDir (ignoring runtime dirs). Results are memoized per directory and gated
// by a cheap stat fingerprint, so an unchanged tree is hashed once and reused
// across calls and reconcile ticks; see packContentHashCache.
func PackContentHashRecursive(fs fsys.FS, topoDir string) string {
	var paths []string
	collectFiles(fs, topoDir, "", &paths)
	sort.Strings(paths)

	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}

	// Cheap stat fingerprint (no content reads) gates the full content hash.
	fp := fnv.New64a()
	for _, relPath := range paths {
		fmt.Fprintf(fp, "%s\x00", relPath) //nolint:errcheck // hash.Write never errors
		if info, statErr := fs.Stat(filepath.Join(topoDir, relPath)); statErr == nil {
			fmt.Fprintf(fp, "%d\x00%d\x00", info.Size(), info.ModTime().UnixNano()) //nolint:errcheck
		}
	}
	fpSum := fp.Sum64()
	if v, ok := packContentHashCache.Load(absDir); ok {
		if entry := v.(packContentHashEntry); entry.fingerprint == fpSum {
			return entry.hash
		}
	}

	h := sha256.New()
	for _, relPath := range paths {
		data, err := fs.ReadFile(filepath.Join(topoDir, relPath))
		if err != nil {
			continue
		}
		h.Write([]byte(relPath)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})       //nolint:errcheck // hash.Write never errors
		h.Write(data)            //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})       //nolint:errcheck // hash.Write never errors
	}
	result := fmt.Sprintf("%x", h.Sum(nil))
	packContentHashCache.Store(absDir, packContentHashEntry{fingerprint: fpSum, hash: result})
	return result
}

// collectFiles recursively collects file paths relative to base.
func collectFiles(fs fsys.FS, base, prefix string, out *[]string) {
	dir := base
	if prefix != "" {
		dir = filepath.Join(base, prefix)
	}
	if prefix != "" && isIgnoredPackRuntimePath(prefix) {
		return
	}
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		rel := e.Name()
		if prefix != "" {
			rel = prefix + "/" + e.Name()
		}
		if isIgnoredPackRuntimePath(rel) {
			continue
		}
		if e.IsDir() {
			collectFiles(fs, base, rel, out)
		} else {
			*out = append(*out, rel)
		}
	}
}

func isIgnoredPackRuntimePath(path string) bool {
	parts := strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case ".beads", ".cache", ".gc", ".git", "state", "tmp":
		return true
	}
	// Language-ecosystem dependency dirs are skipped at ANY depth. Pack
	// hashing previously walked into node_modules for packs anchored at
	// monorepo roots, opening tens of thousands of files into the
	// supervisor every dirty reload (gastownhall/gascity#2954). Matches
	// the existing __pycache__ precedent for Python ecosystems.
	for _, part := range parts {
		if part == "__pycache__" || part == "node_modules" {
			return true
		}
	}
	return false
}

// resolveNamedPacks translates named pack references to cache paths.
// If a reference in workspace.includes or rig.includes matches a key
// in cfg.Packs, it is rewritten to the local cache directory path.
// Local path references pass through unchanged.
// Called after merge + patches, before expansion.
func resolveNamedPacks(cfg *City, cityRoot string) {
	if len(cfg.Packs) == 0 {
		return
	}
	// City includes.
	includes := cfg.Workspace.LegacyIncludes()
	for i, ref := range includes {
		if src, ok := cfg.Packs[ref]; ok {
			includes[i] = PackCachePath(cityRoot, ref, src)
		}
	}
	cfg.Workspace.SetLegacyIncludes(includes)
	// Rig includes.
	for i := range cfg.Rigs {
		for j, ref := range cfg.Rigs[i].Includes {
			if src, ok := cfg.Packs[ref]; ok {
				cfg.Rigs[i].Includes[j] = PackCachePath(cityRoot, ref, src)
			}
		}
	}
}

// PackDefinesAgent checks whether a pack (recursively through includes)
// defines a rig-scoped agent with the given name. Returns false on error
// (fail-open: caller should add the default polecat).
func PackDefinesAgent(fs fsys.FS, packRef, cityRoot, agentName string) bool {
	topoDir, err := resolvePackRef(packRef, cityRoot, cityRoot)
	if err != nil {
		return false
	}
	topoPath := filepath.Join(topoDir, packFile)

	agents, _, _, _, _, _, _, _, err := loadPack(fs, topoPath, topoDir, cityRoot, "", nil)
	if err != nil {
		return false
	}

	// Filter to rig-scoped agents only.
	rigAgents := filterAgentsByScope(agents, false)
	for _, a := range rigAgents {
		if a.Name == agentName {
			return true
		}
	}
	return false
}

func decodePackName(data []byte) (string, error) {
	var meta struct {
		Pack struct {
			Name string `toml:"name"`
		} `toml:"pack"`
	}
	if _, err := toml.Decode(string(data), &meta); err != nil {
		return "", err
	}
	return meta.Pack.Name, nil
}

// HasPackRigs reports whether any rig in the config uses a pack.
// Rigs with only a path are included because expandPacks auto-discovers
// their root pack.toml (if present) as an implicit include.
func HasPackRigs(rigs []Rig) bool {
	for _, r := range rigs {
		if len(r.Includes) > 0 || len(r.Imports) > 0 {
			return true
		}
		if strings.TrimSpace(r.Path) != "" {
			return true
		}
	}
	return false
}

// PackSummary returns a string summarizing pack usage per rig
// (for provenance/config show output). Only includes rigs with packs.
func PackSummary(cfg *City, fs fsys.FS, cityRoot string) map[string]string {
	result := make(map[string]string)
	for _, r := range cfg.Rigs {
		topoRefs := r.Includes
		if len(topoRefs) == 0 {
			continue
		}
		var summaries []string
		for _, ref := range topoRefs {
			summaries = append(summaries, packSummaryOne(fs, ref, cityRoot))
		}
		result[r.Name] = strings.Join(summaries, "; ")
	}
	return result
}

// packSummaryOne builds a summary string for a single pack reference.
func packSummaryOne(fs fsys.FS, ref, cityRoot string) string {
	topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
	topoPath := filepath.Join(topoDir, packFile)
	data, err := fs.ReadFile(topoPath)
	if err != nil {
		return ref + " (unreadable)"
	}
	var tc PackConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		return ref + " (parse error)"
	}
	hash := PackContentHashRecursive(fs, topoDir)
	short := hash
	if len(short) > 12 {
		short = short[:12]
	}
	var parts []string
	parts = append(parts, tc.Pack.Name)
	if tc.Pack.Version != "" {
		parts = append(parts, tc.Pack.Version)
	}
	parts = append(parts, "("+short+")")
	return strings.Join(parts, " ")
}

// PackDoctorInfo pairs a doctor entry with its resolved context.
type PackDoctorInfo struct {
	// PackName is the pack's [pack] name.
	PackName string
	// Entry is the parsed [[doctor]] entry.
	Entry PackDoctorEntry
	// TopoDir is the absolute pack directory (for resolving script paths).
	TopoDir string
}

// LoadPackDoctorEntries reads pack.toml files from each pack
// directory, extracts [[doctor]] entries, and returns them with resolved
// context. Directories are deduplicated by absolute path. Errors in
// individual packs are silently skipped (the pack may have been
// validated elsewhere; doctor should be best-effort).
func LoadPackDoctorEntries(fs fsys.FS, topoDirs []string) []PackDoctorInfo {
	seen := make(map[string]bool)
	var result []PackDoctorInfo

	for _, dir := range topoDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			absDir = dir
		}
		if seen[absDir] {
			continue
		}
		seen[absDir] = true

		topoPath := filepath.Join(dir, packFile)
		data, err := fs.ReadFile(topoPath)
		if err != nil {
			continue
		}

		var tc PackConfig
		if _, err := toml.Decode(string(data), &tc); err != nil {
			continue
		}

		for _, entry := range tc.Doctor {
			result = append(result, PackDoctorInfo{
				PackName: tc.Pack.Name,
				Entry:    entry,
				TopoDir:  dir,
			})
		}
	}

	return result
}

// PackCommandInfo pairs a command entry with its resolved context.
type PackCommandInfo struct {
	// PackName is the pack's [pack] name.
	PackName string
	// Entry is the parsed [[commands]] entry.
	Entry PackCommandEntry
	// PackDir is the absolute pack directory (for resolving script paths).
	PackDir string
}

// LoadPackCommandEntries reads pack.toml files from each pack directory,
// extracts [[commands]] entries, and returns them with resolved context.
// Directories are deduplicated by absolute path. Errors in individual
// packs are silently skipped (best-effort, same as LoadPackDoctorEntries).
func LoadPackCommandEntries(fs fsys.FS, packDirs []string) []PackCommandInfo {
	seen := make(map[string]bool)
	var result []PackCommandInfo

	for _, dir := range packDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			absDir = dir
		}
		if seen[absDir] {
			continue
		}
		seen[absDir] = true

		tomlPath := filepath.Join(dir, packFile)
		data, err := fs.ReadFile(tomlPath)
		if err != nil {
			continue
		}

		var tc PackConfig
		if _, err := toml.Decode(string(data), &tc); err != nil {
			continue
		}

		for _, entry := range tc.Commands {
			result = append(result, PackCommandInfo{
				PackName: tc.Pack.Name,
				Entry:    entry,
				PackDir:  dir,
			})
		}
	}

	return result
}
