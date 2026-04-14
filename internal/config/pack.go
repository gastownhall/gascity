package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	iofs "io/fs"
	"log"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

// packFile is the expected filename inside a pack directory.
const packFile = "pack.toml"

// currentPackSchema is the supported pack schema version.
const currentPackSchema = 2

// packConfig is the TOML structure of a pack.toml file.
// It has a [pack] metadata header and agent definitions.
type packConfig struct {
	Pack          PackMeta                `toml:"pack"`
	Imports       map[string]Import       `toml:"imports,omitempty"`
	AgentDefaults AgentDefaults           `toml:"agent_defaults,omitempty"`
	Agents        []Agent                 `toml:"agent"`
	NamedSessions []NamedSession          `toml:"named_session,omitempty"`
	Services      []Service               `toml:"service,omitempty"`
	Providers     map[string]ProviderSpec `toml:"providers,omitempty"`
	Formulas      FormulasConfig          `toml:"formulas,omitempty"`
	Patches       Patches                 `toml:"patches,omitempty"`
	Doctor        []PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands      []PackCommandEntry      `toml:"commands,omitempty"`
	Global        PackGlobal              `toml:"global,omitempty"`
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
//
// rigFormulaDirs is populated with per-rig pack formula directories
// (Layer 3). cityRoot is the city directory (parent of city.toml), used
// for path resolution.
func ExpandPacks(cfg *City, fs fsys.FS, cityRoot string, rigFormulaDirs map[string][]string) error {
	var expanded []Agent
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		cache := &packLoadCache{results: make(map[string]*packLoadResult)}
		topoRefs := rig.Includes
		if len(topoRefs) == 0 && len(rig.Imports) == 0 {
			continue
		}

		var rigAgents []Agent
		var rigNamedSessions []NamedSession
		var rigTopoDirs []string
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

			agents, namedSessions, providers, services, topoDirs, reqs, globals, err := loadPackWithCache(fs, topoPath, topoDir, cityRoot, rig.Name, nil, cache)
			if err != nil {
				return fmt.Errorf("rig %q pack %q: %w", rig.Name, ref, err)
			}
			if len(services) > 0 {
				return fmt.Errorf("rig %q pack %q: [[service]] is only allowed in city-scoped packs", rig.Name, ref)
			}
			rigGlobals = append(rigGlobals, globals...)
			cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, cachedPackDoctors(cache, topoDir)...)

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

			// Keep only rig-scoped and unscoped agents for rig expansion.
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

				impDir, err := resolvePackRef(imp.Source, cityRoot, cityRoot)
				if err != nil {
					return fmt.Errorf("rig %q import %q: %w", rig.Name, bindingName, err)
				}

				impPath := filepath.Join(impDir, packFile)
				agents, namedSessions, providers, services, topoDirs, reqs, globals, err := loadPackWithCache(
					fs, impPath, impDir, cityRoot, rig.Name, nil, cache)
				if err != nil {
					return fmt.Errorf("rig %q import %q: %w", rig.Name, bindingName, err)
				}
				if len(services) > 0 {
					return fmt.Errorf("rig %q import %q: [[service]] is only allowed in city-scoped packs", rig.Name, bindingName)
				}
				rigGlobals = append(rigGlobals, globals...)
				cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, cachedPackDoctors(cache, impDir)...)

				// Stamp binding name on agents and named sessions.
				// At the rig level, ALL agents from an import get the rig's
				// binding — nested bindings are overridden.
				for i := range agents {
					agents[i].BindingName = bindingName
				}
				for i := range namedSessions {
					namedSessions[i].BindingName = bindingName
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
			}
		}

		// Store per-rig pack dirs.
		if cfg.RigPackDirs == nil {
			cfg.RigPackDirs = make(map[string][]string)
		}
		if len(rigTopoDirs) > 0 {
			cfg.RigPackDirs[rig.Name] = rigTopoDirs
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

		// Collect scripts/ dirs from rig pack dirs.
		var rigScriptDirs []string
		for _, dir := range rigTopoDirs {
			sd := filepath.Join(dir, "scripts")
			if info, sErr := fs.Stat(sd); sErr == nil && info.IsDir() {
				rigScriptDirs = appendUnique(rigScriptDirs, sd)
			}
		}
		if len(rigScriptDirs) > 0 {
			if cfg.RigScriptDirs == nil {
				cfg.RigScriptDirs = make(map[string][]string)
			}
			cfg.RigScriptDirs[rig.Name] = rigScriptDirs
		}

		// Resolve fallback agents before collision detection.
		rigAgents = resolveFallbackAgents(rigAgents)

		// Check for duplicate agent names across packs for this rig.
		if err := checkPackAgentCollisions(rigAgents, rig.Name); err != nil {
			return err
		}

		// Apply per-rig overrides/patches after all packs for this rig.
		// V2 accepts both "overrides" (V1) and "patches" (V2) TOML keys.
		allOverrides := rig.Overrides
		allOverrides = append(allOverrides, rig.RigPatches...)
		if err := applyOverrides(rigAgents, allOverrides, rig.Name); err != nil {
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
	return nil
}

// ExpandCityPacks loads all city-level packs from workspace.includes (V1)
// and city-level [imports.X] (V2). City pack agents are stamped with
// dir="" (city-scoped) and prepended to the agent list. Returns
// (formulaDirs, packRequirements, shadowWarnings, error). cityRoot is
// the city directory.
func ExpandCityPacks(cfg *City, fs fsys.FS, cityRoot string) ([]string, []PackRequirement, []string, error) {
	topos := cfg.Workspace.Includes
	hasImports := len(cfg.Imports) > 0
	if len(topos) == 0 && !hasImports {
		return nil, nil, nil, nil
	}

	var allAgents []Agent
	var allNamedSessions []NamedSession
	var formulaDirs []string
	var allPackDirs []string
	var allRequires []PackRequirement
	var allGlobals []ResolvedPackGlobal
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

		agents, namedSessions, providers, services, topoDirs, reqs, globals, err := loadPackWithCache(fs, topoPath, topoDir, cityRoot, "", nil, cache)
		if err != nil {
			// pack.toml may be missing if the pack was removed upstream after
			// the repo was fetched. Skip gracefully.
			if errors.Is(err, iofs.ErrNotExist) {
				log.Printf("city pack %q: not found, skipping: %v", ref, err)
				continue
			}
			return nil, nil, nil, fmt.Errorf("city pack %q: %w", ref, err)
		}
		allRequires = append(allRequires, reqs...)
		allGlobals = append(allGlobals, globals...)
		cfg.Services = append(cfg.Services, services...)
		packName := tcPackName(fs, topoPath)
		if packName == "" && len(cachedPackCommands(cache, topoDir)) > 0 {
			return nil, nil, nil, fmt.Errorf("city pack %q: discovered commands require [pack].name for CLI binding", ref)
		}
		cfg.PackCommands = appendDiscoveredCommands(cfg.PackCommands, stampDefaultBinding(cachedPackCommands(cache, topoDir), packName)...)
		cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, cachedPackDoctors(cache, topoDir)...)

		// Accumulate pack dirs (deduped).
		allPackDirs = appendUnique(allPackDirs, topoDirs...)

		// Keep only city-scoped and unscoped agents for city expansion.
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

			// Unlike V1 includes (which skip gracefully for missing remote
			// subpaths), V2 imports are always fatal on missing source.
			// A typo in [imports.X].source should not be silently ignored.
			impDir, err := resolvePackRef(imp.Source, cityRoot, cityRoot)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("city import %q: %w", bindingName, err)
			}

			impPath := filepath.Join(impDir, packFile)
			agents, namedSessions, providers, services, topoDirs, reqs, globals, err := loadPackWithCache(
				fs, impPath, impDir, cityRoot, "", nil, cache)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("city import %q: %w", bindingName, err)
			}
			commands := cachedPackCommands(cache, impDir)
			doctors := cachedPackDoctors(cache, impDir)

			// When transitive = false, keep only agents directly defined
			// by this import (not its own transitive dependencies).
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
				commands = filterCommandsBySourceDir(commands, impDir)
				doctors = filterDoctorsBySourceDir(doctors, impDir)
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

			allRequires = append(allRequires, reqs...)
			allGlobals = append(allGlobals, globals...)
			cfg.Services = append(cfg.Services, services...)
			cfg.PackCommands = appendDiscoveredCommands(cfg.PackCommands, commands...)
			cfg.PackDoctors = appendDiscoveredDoctors(cfg.PackDoctors, doctors...)
			allPackDirs = appendUnique(allPackDirs, topoDirs...)

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
		}
	}

	// Store city pack dirs.
	cfg.PackDirs = appendUnique(cfg.PackDirs, allPackDirs...)

	// Collect overlay/ dirs from pack dirs.
	for _, dir := range allPackDirs {
		od := filepath.Join(dir, "overlay")
		if info, err := fs.Stat(od); err == nil && info.IsDir() {
			cfg.PackOverlayDirs = appendUnique(cfg.PackOverlayDirs, od)
		}
	}

	// Collect scripts/ dirs from pack dirs.
	for _, dir := range allPackDirs {
		sd := filepath.Join(dir, "scripts")
		if info, err := fs.Stat(sd); err == nil && info.IsDir() {
			cfg.PackScriptDirs = appendUnique(cfg.PackScriptDirs, sd)
		}
	}

	// Resolve fallback agents before collision detection.
	allAgents = resolveFallbackAgents(allAgents)

	// Check for duplicate agent names across city packs.
	if err := checkPackAgentCollisions(allAgents, ""); err != nil {
		return nil, nil, nil, err
	}

	// City pack agents go at the front (before user-defined agents).
	// Run fallback dedup again on the combined set so system pack
	// fallback agents yield to inline city-level agents.
	cfg.Agents = resolveFallbackAgents(append(allAgents, cfg.Agents...))
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

	return formulaDirs, allRequires, shadowWarnings, nil
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

// ComputeScriptLayers builds the ScriptLayers from the resolved script
// directories. Each layer slice is ordered lowest→highest priority.
// City pack scripts form the base; rig pack scripts layer on top.
func ComputeScriptLayers(cityPackScripts []string, rigPackScripts map[string][]string, rigs []Rig) ScriptLayers {
	sl := ScriptLayers{
		Rigs: make(map[string][]string),
	}
	sl.City = append([]string{}, cityPackScripts...)

	for _, r := range rigs {
		layers := make([]string, len(cityPackScripts))
		copy(layers, cityPackScripts)
		if sds, ok := rigPackScripts[r.Name]; ok {
			layers = append(layers, sds...)
		}
		if len(layers) > 0 {
			sl.Rigs[r.Name] = layers
		}
	}

	return sl
}

// resolveFallbackAgents resolves fallback agent collisions. When agents
// from different SourceDirs share a name:
//   - One fallback + one non-fallback: non-fallback wins, fallback removed
//   - Both fallback: first loaded wins (depth-first include order)
//   - Neither fallback: left for checkPackAgentCollisions to error
//
// Agents from the same SourceDir are never in conflict (they're duplicates
// within one pack, handled elsewhere). Order is preserved.
func resolveFallbackAgents(agents []Agent) []Agent {
	// Build per-name groups from distinct SourceDirs.
	type entry struct {
		idx      int
		fallback bool
		srcDir   string
	}
	groups := make(map[string][]entry)
	for i, a := range agents {
		// Use QualifiedName so agents with different bindings
		// (e.g., "gs.mayor" and "maint.mayor") don't collide.
		groups[a.QualifiedName()] = append(groups[a.QualifiedName()], entry{i, a.Fallback, a.SourceDir})
	}

	// Determine which indices to remove.
	remove := make(map[int]bool)
	for _, entries := range groups {
		// Only care about names from multiple sources.
		// Empty SourceDir means city-level (inline) — count it as a
		// distinct source so system pack fallbacks yield to inline agents.
		dirs := make(map[string]bool)
		for _, e := range entries {
			dirs[e.srcDir] = true // "" is a valid key (city-level)
		}
		if len(dirs) < 2 {
			continue
		}

		// Separate fallback vs non-fallback entries.
		var fb, nonfb []entry
		for _, e := range entries {
			if e.fallback {
				fb = append(fb, e)
			} else {
				nonfb = append(nonfb, e)
			}
		}

		if len(nonfb) > 0 && len(fb) > 0 {
			// Non-fallback wins: remove all fallback entries.
			for _, e := range fb {
				remove[e.idx] = true
			}
		} else if len(nonfb) == 0 && len(fb) > 1 {
			// All fallback: keep first, remove rest.
			for _, e := range fb[1:] {
				remove[e.idx] = true
			}
		}
		// Both non-fallback: leave alone for collision detection.
	}

	if len(remove) == 0 {
		return agents
	}

	result := make([]Agent, 0, len(agents)-len(remove))
	for i, a := range agents {
		if !remove[i] {
			result = append(result, a)
		}
	}
	return result
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
	agents        []Agent
	namedSessions []NamedSession
	providers     map[string]ProviderSpec
	services      []Service
	topoDirs      []string
	requires      []PackRequirement
	globals       []ResolvedPackGlobal
	commands      []DiscoveredCommand
	doctors       []DiscoveredDoctor
}

//nolint:unparam // compatibility wrapper keeps the recursion-set argument at the public helper boundary.
func loadPack(fs fsys.FS, topoPath, topoDir, cityRoot, rigName string, seen map[string]bool) ([]Agent, []NamedSession, map[string]ProviderSpec, []Service, []string, []PackRequirement, []ResolvedPackGlobal, error) {
	return loadPackWithCache(fs, topoPath, topoDir, cityRoot, rigName, seen, nil)
}

func loadPackWithCache(fs fsys.FS, topoPath, topoDir, cityRoot, rigName string, seen map[string]bool, cache *packLoadCache) ([]Agent, []NamedSession, map[string]ProviderSpec, []Service, []string, []PackRequirement, []ResolvedPackGlobal, error) {
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
		return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("cycle detected: pack %q already visited", topoDir)
	}

	// Dedup: if we've already loaded this exact directory, return a copy
	// of the cached result so the caller can stamp different bindings
	// without mutating the cached canonical copy. This supports both
	// diamond DAGs (same binding, deduped by downstream collision checks)
	// and intentional multi-binding (same pack imported as both "foo"
	// and "bar").
	if cached, ok := cache.results[absTopoDir]; ok {
		cloned := clonePackLoadResult(cached)
		return cloned.agents, cloned.namedSessions, cloned.providers, cloned.services, cloned.topoDirs, cloned.requires, cloned.globals, nil
	}

	seen[absTopoDir] = true
	defer func() { delete(seen, absTopoDir) }()

	data, err := fs.ReadFile(topoPath)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("loading %s: %w", packFile, err)
	}

	var tc packConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("parsing %s: %w", packFile, err)
	}

	if err := validatePackMeta(&tc.Pack); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}

	// Process includes: accumulate base-layer agents, providers,
	// pack dirs, requirements, and globals from included packs.
	var includedAgents []Agent
	var includedNamedSessions []NamedSession
	var includedServices []Service
	var includedTopoDirs []string
	var allRequires []PackRequirement
	var includedGlobals []ResolvedPackGlobal
	var includedCommands []DiscoveredCommand
	var includedDoctors []DiscoveredDoctor
	includedProviders := make(map[string]ProviderSpec)

	for _, inc := range tc.Pack.Includes {
		incTopoDir, err := resolvePackRef(inc, topoDir, cityRoot)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("include %q: %w", inc, err)
		}

		incTopoPath := filepath.Join(incTopoDir, packFile)
		incAgents, incNamedSessions, incProviders, incServices, incTopoDirs, incReqs, incGlobals, err := loadPackWithCache(
			fs, incTopoPath, incTopoDir, cityRoot, rigName, seen, cache)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("include %q: %w", inc, err)
		}

		includedAgents = append(includedAgents, incAgents...)
		includedNamedSessions = append(includedNamedSessions, incNamedSessions...)
		includedServices = append(includedServices, incServices...)
		includedTopoDirs = append(includedTopoDirs, incTopoDirs...)
		allRequires = append(allRequires, incReqs...)
		includedGlobals = append(includedGlobals, incGlobals...)
		includedCommands = append(includedCommands, cachedPackCommands(cache, incTopoDir)...)
		includedDoctors = append(includedDoctors, cachedPackDoctors(cache, incTopoDir)...)

		// Merge providers: included first, no overwrite.
		for name, spec := range incProviders {
			if _, exists := includedProviders[name]; !exists {
				includedProviders[name] = spec
			}
		}
	}

	// Process V2 [imports.X] entries. These are named bindings that
	// produce agents with qualified names (bindingName.agentName).
	// Local-path imports are resolved now; remote imports require
	// gc import install to have already cached them (future work).
	// Process in sorted order for deterministic output.
	importNames := make([]string, 0, len(tc.Imports))
	for name := range tc.Imports {
		importNames = append(importNames, name)
	}
	sort.Strings(importNames)

	for _, bindingName := range importNames {
		imp := tc.Imports[bindingName]

		// Resolve the import source. For now, only local paths are
		// supported. Remote sources require the cache populated by
		// gc import install (which we don't have yet).
		impDir, err := resolvePackRef(imp.Source, topoDir, cityRoot)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: %w", bindingName, err)
		}

		impPath := filepath.Join(impDir, packFile)
		impAgents, impNamedSessions, impProviders, impServices, impTopoDirs, impReqs, impGlobals, err := loadPackWithCache(
			fs, impPath, impDir, cityRoot, rigName, seen, cache)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: %w", bindingName, err)
		}
		impCommands := cachedPackCommands(cache, impDir)
		impDoctors := cachedPackDoctors(cache, impDir)

		// When transitive = false, strip agents that came from the
		// imported pack's own imports (i.e., transitive deps). We keep
		// only agents whose SourceDir matches the import's own directory.
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
			impCommands = filterCommandsBySourceDir(impCommands, impDir)
			impDoctors = filterDoctorsBySourceDir(impDoctors, impDir)
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

		// Read the imported pack name for provenance tracking.
		impData, readErr := fs.ReadFile(impPath)
		if readErr != nil {
			return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: reading %s: %w", bindingName, impPath, readErr)
		}
		packName, err := decodePackName(impData)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("import %q: parsing %s: %w", bindingName, impPath, err)
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

		includedAgents = append(includedAgents, impAgents...)
		includedNamedSessions = append(includedNamedSessions, impNamedSessions...)
		includedServices = append(includedServices, impServices...)
		includedTopoDirs = append(includedTopoDirs, impTopoDirs...)
		allRequires = append(allRequires, impReqs...)
		includedGlobals = append(includedGlobals, impGlobals...)
		includedCommands = append(includedCommands, impCommands...)
		includedDoctors = append(includedDoctors, impDoctors...)

		for name, spec := range impProviders {
			if _, exists := includedProviders[name]; !exists {
				includedProviders[name] = spec
			}
		}
	}

	// Collect this pack's own requirements.
	allRequires = append(allRequires, tc.Pack.Requires...)

	// V2 convention-based agent discovery: scan agents/ directory.
	// Convention-discovered agents are appended AFTER TOML-declared agents
	// so [[agent]] tables take precedence when both exist.
	discovered, dErr := DiscoverPackAgents(fs, topoDir, tc.Pack.Name, agentNameSet(tc.Agents))
	if dErr != nil {
		return nil, nil, nil, nil, nil, nil, nil, dErr
	}
	tc.Agents = append(tc.Agents, discovered...)

	commands, err := DiscoverPackCommands(fs, topoDir, tc.Pack.Name)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	commands = append(commands, legacyPackCommands(tc.Commands, topoDir, tc.Pack.Name)...)
	doctors, err := DiscoverPackDoctors(fs, topoDir, tc.Pack.Name)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	doctors = append(doctors, legacyPackDoctors(tc.Doctor, topoDir, tc.Pack.Name)...)

	// V2 convention-based order discovery: top-level orders/ flat files are the
	// standard layout. Deprecated locations are still discovered so pack loads
	// surface migration warnings consistently.
	if _, err := orders.ScanRoots(fs, []orders.ScanRoot{{
		Dir:          filepath.Join(topoDir, "orders"),
		FormulaLayer: filepath.Join(topoDir, "formulas"),
	}}, nil); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
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
		// Resolve prompt_template paths relative to pack directory.
		if agents[i].PromptTemplate != "" {
			agents[i].PromptTemplate = adjustFragmentPath(
				agents[i].PromptTemplate, topoDir, cityRoot)
		}
		// Resolve session_setup_script paths relative to pack directory.
		if agents[i].SessionSetupScript != "" {
			agents[i].SessionSetupScript = adjustFragmentPath(
				agents[i].SessionSetupScript, topoDir, cityRoot)
		}
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
			return nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("service %q: packs may not set publish_mode=direct", services[i].Name)
		}
	}

	// Merge: included agents first (base), then parent agents (override).
	includedAgents = append(includedAgents, agents...)
	includedNamedSessions = append(includedNamedSessions, namedSessions...)
	includedServices = append(includedServices, services...)
	includedCommands = append(includedCommands, commands...)
	includedDoctors = append(includedDoctors, doctors...)

	// Apply pack-level patches to the merged agent list.
	if !tc.Patches.IsEmpty() {
		adjustPackPatchPaths(&tc.Patches, topoDir, cityRoot)
		if err := applyPackAgentPatches(includedAgents, tc.Patches.Agents); err != nil {
			return nil, nil, nil, nil, nil, nil, nil, err
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

	// Build pack dirs: included pack dirs first (lower priority),
	// then this pack's dir (higher priority).
	var topoDirs []string
	topoDirs = append(topoDirs, includedTopoDirs...)
	topoDirs = append(topoDirs, topoDir)

	// Collect globals: included globals first, then this pack's own.
	var allGlobals []ResolvedPackGlobal
	allGlobals = append(allGlobals, includedGlobals...)
	if len(tc.Global.SessionLive) > 0 {
		allGlobals = append(allGlobals, ResolvedPackGlobal{
			SessionLive: resolveConfigDirInCommands(tc.Global.SessionLive, topoDir),
			PackName:    tc.Pack.Name,
		})
	}

	// Cache result for diamond-DAG dedup.
	cache.results[absTopoDir] = clonePackLoadResult(&packLoadResult{
		agents:        includedAgents,
		namedSessions: includedNamedSessions,
		providers:     mergedProviders,
		services:      includedServices,
		topoDirs:      topoDirs,
		requires:      allRequires,
		globals:       allGlobals,
		commands:      includedCommands,
		doctors:       includedDoctors,
	})

	return includedAgents, includedNamedSessions, mergedProviders, includedServices, topoDirs, allRequires, allGlobals, nil
}

func clonePackLoadResult(in *packLoadResult) *packLoadResult {
	if in == nil {
		return nil
	}
	return &packLoadResult{
		agents:        deepCopyAgents(in.agents),
		namedSessions: deepCopyNamedSessions(in.namedSessions),
		providers:     deepCopyProviderSpecs(in.providers),
		services:      deepCopyServices(in.services),
		topoDirs:      append([]string(nil), in.topoDirs...),
		requires:      append([]PackRequirement(nil), in.requires...),
		globals:       deepCopyResolvedPackGlobals(in.globals),
		commands:      deepCopyCommands(in.commands),
		doctors:       deepCopyDoctors(in.doctors),
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
		out[i].DependsOn = append([]string(nil), in[i].DependsOn...)
		out[i].MaxActiveSessions = copyIntPtr(in[i].MaxActiveSessions)
		out[i].MinActiveSessions = copyIntPtr(in[i].MinActiveSessions)
		out[i].ReadyDelayMs = copyIntPtr(in[i].ReadyDelayMs)
		out[i].EmitsPermissionWarning = copyBoolPtr(in[i].EmitsPermissionWarning)
		out[i].HooksInstalled = copyBoolPtr(in[i].HooksInstalled)
		out[i].DefaultSlingFormula = copyStringPtr(in[i].DefaultSlingFormula)
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

func deepCopyProviderSpec(in ProviderSpec) ProviderSpec {
	out := in
	out.Args = append([]string(nil), in.Args...)
	out.ProcessNames = append([]string(nil), in.ProcessNames...)
	out.Env = deepCopyStringMap(in.Env)
	out.PermissionModes = deepCopyStringMap(in.PermissionModes)
	out.OptionDefaults = deepCopyStringMap(in.OptionDefaults)
	out.OptionsSchema = deepCopyProviderOptions(in.OptionsSchema)
	out.PrintArgs = append([]string(nil), in.PrintArgs...)
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

func filterCommandsBySourceDir(commands []DiscoveredCommand, sourceDir string) []DiscoveredCommand {
	absSource, _ := filepath.Abs(sourceDir)
	var out []DiscoveredCommand
	for _, cmd := range commands {
		absDir, _ := filepath.Abs(cmd.SourceDir)
		if absDir == absSource || strings.HasPrefix(absDir, absSource+string(filepath.Separator)) {
			out = append(out, cmd)
		}
	}
	return out
}

func filterDoctorsBySourceDir(doctors []DiscoveredDoctor, sourceDir string) []DiscoveredDoctor {
	absSource, _ := filepath.Abs(sourceDir)
	var out []DiscoveredDoctor
	for _, check := range doctors {
		absDir, _ := filepath.Abs(check.SourceDir)
		if absDir == absSource || strings.HasPrefix(absDir, absSource+string(filepath.Separator)) {
			out = append(out, check)
		}
	}
	return out
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
		duplicate := false
		for _, existing := range dst {
			if existing.Name == check.Name &&
				existing.BindingName == check.BindingName &&
				existing.RunScript == check.RunScript {
				duplicate = true
				break
			}
		}
		if !duplicate {
			dst = append(dst, check)
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

func legacyPackDoctors(entries []PackDoctorEntry, packDir, packName string) []DiscoveredDoctor {
	out := make([]DiscoveredDoctor, 0, len(entries))
	for _, entry := range entries {
		runScript := entry.Script
		if runScript != "" && !filepath.IsAbs(runScript) {
			runScript = filepath.Join(packDir, runScript)
		}

		out = append(out, DiscoveredDoctor{
			Name:        entry.Name,
			Description: entry.Description,
			RunScript:   runScript,
			SourceDir:   packDir,
			PackDir:     packDir,
			PackName:    packName,
		})
	}
	return out
}

// applyPackGlobals appends [global].session_live commands from packs
// to matching agents. City-level globals affect ALL agents. Rig-level
// globals affect only agents in that rig.
func applyPackGlobals(cfg *City) {
	// City-level globals → all agents.
	for _, g := range cfg.PackGlobals {
		for i := range cfg.Agents {
			cfg.Agents[i].SessionLive = append(
				cfg.Agents[i].SessionLive, g.SessionLive...)
		}
	}
	// Rig-level globals → only that rig's agents.
	for rigName, globals := range cfg.RigPackGlobals {
		for _, g := range globals {
			for i := range cfg.Agents {
				if cfg.Agents[i].Dir == rigName {
					cfg.Agents[i].SessionLive = append(
						cfg.Agents[i].SessionLive, g.SessionLive...)
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
// the pack directory, matching how agent fields are resolved during
// pack loading.
func adjustPackPatchPaths(patches *Patches, topoDir, cityRoot string) {
	for i := range patches.Agents {
		p := &patches.Agents[i]
		if p.SessionSetupScript != nil && *p.SessionSetupScript != "" {
			v := adjustFragmentPath(*p.SessionSetupScript, topoDir, cityRoot)
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
	if ov.StartCommand != nil {
		a.StartCommand = *ov.StartCommand
	}
	if ov.Nudge != nil {
		a.Nudge = *ov.Nudge
	}
	if ov.IdleTimeout != nil {
		a.IdleTimeout = *ov.IdleTimeout
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
	if len(ov.InjectFragments) > 0 {
		a.InjectFragments = append([]string(nil), ov.InjectFragments...)
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
func PackContentHashRecursive(fs fsys.FS, topoDir string) string {
	var paths []string
	collectFiles(fs, topoDir, "", &paths)
	sort.Strings(paths)

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
	return fmt.Sprintf("%x", h.Sum(nil))
}

// collectFiles recursively collects file paths relative to base.
func collectFiles(fs fsys.FS, base, prefix string, out *[]string) {
	dir := base
	if prefix != "" {
		dir = filepath.Join(base, prefix)
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
		if e.IsDir() {
			collectFiles(fs, base, rel, out)
		} else {
			*out = append(*out, rel)
		}
	}
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
	for i, ref := range cfg.Workspace.Includes {
		if src, ok := cfg.Packs[ref]; ok {
			cfg.Workspace.Includes[i] = PackCachePath(cityRoot, ref, src)
		}
	}
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

	agents, _, _, _, _, _, _, err := loadPack(fs, topoPath, topoDir, cityRoot, "", nil)
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
func HasPackRigs(rigs []Rig) bool {
	for _, r := range rigs {
		if len(r.Includes) > 0 || len(r.Imports) > 0 {
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
	var tc packConfig
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

		var tc packConfig
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

		var tc packConfig
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
