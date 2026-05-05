package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Provenance tracks where each configuration element originated during
// composition. Built into the merge API from the start — retrofitting
// provenance later is expensive.
type Provenance struct {
	// Root is the path to the root city.toml.
	Root string
	// Sources lists all source files in load order (root first).
	Sources []string
	// Imports maps import binding names to the source that added them.
	// Implicit imports use the sentinel value "(implicit)".
	Imports map[string]string
	// Agents maps agent QualifiedName → source file path.
	Agents map[string]string
	// Rigs maps rig name → source file path.
	Rigs map[string]string
	// Workspace maps workspace field name → source file path.
	Workspace map[string]string
	// Warnings collects non-fatal collision warnings from composition.
	Warnings []string

	sourceContents   map[string][]byte
	revisionSnapshot *revisionSnapshot
}

// LoadOptions controls optional config-loading behavior.
type LoadOptions struct {
	// SuppressDeprecatedOrderWarnings suppresses only legacy order-path
	// migration warnings produced while discovering pack orders.
	SuppressDeprecatedOrderWarnings bool
}

// LoadWithIncludes loads a city.toml and merges all included fragments.
// Includes are NOT recursive — fragments cannot include other fragments.
// Extra includes (from CLI -f flags) are appended after the root's
// include list and processed identically.
// Returns the fully-merged config, provenance tracking, and any error.
func LoadWithIncludes(fs fsys.FS, path string, extraIncludes ...string) (*City, *Provenance, error) {
	return LoadWithIncludesOptions(fs, path, LoadOptions{}, extraIncludes...)
}

// LoadWithIncludesOptions loads a city.toml with the supplied load options.
func LoadWithIncludesOptions(fs fsys.FS, path string, opts LoadOptions, extraIncludes ...string) (*City, *Provenance, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config %q: %w", path, err)
	}

	root, rootMeta, rootWarnings, err := parseWithMeta(data, path)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config %q: %w", path, err)
	}
	cityRoot := filepath.Dir(path)
	prov := newProvenance(path)
	prov.recordSource(path, data)
	prov.Warnings = append(prov.Warnings, rootWarnings...)
	cityAgentsForProvenance := root.Agents

	// V2: if a pack.toml exists alongside city.toml, it is the city's
	// definition layer. Parse it and merge its content (imports, agents,
	// commands, doctors, providers, named sessions) into the root config.
	// pack.toml content is the city pack's own content; city.toml carries
	// deployment (rigs, substrates, capacity) plus any inline agents.
	cityImportCount := len(root.Imports)
	packExists := false
	var rootPackIncludes []string
	var rootPackGlobals []ResolvedPackGlobal
	var rootPackRequires []PackRequirement
	packPath := filepath.Join(cityRoot, packFile)
	if packData, pErr := fs.ReadFile(packPath); pErr == nil {
		packExists = true
		pc, md, packWarnings, decErr := parsePackConfigWithMetadata(packData, packPath)
		if decErr != nil {
			return nil, nil, fmt.Errorf("parsing city pack.toml: %w", decErr)
		}
		if fatalWarnings := fatalUndecodedWarnings(md, packPath); len(fatalWarnings) > 0 {
			return nil, nil, fmt.Errorf("parsing city pack.toml: %s", strings.Join(fatalWarnings, "; "))
		}
		prov.Warnings = append(prov.Warnings, packWarnings...)
		if err := validatePackMeta(&pc.Pack); err != nil {
			return nil, nil, fmt.Errorf("city pack.toml: %w", err)
		}
		// Preserve the city.toml agents so they can override pack-defined
		// and convention-discovered agents.
		cityAgents := append([]Agent{}, root.Agents...)
		cityAgentsForProvenance = cityAgents
		rootPackIncludes = append([]string(nil), pc.Pack.Includes...)
		rootPackRequires = append([]PackRequirement(nil), pc.Pack.Requires...)
		// Dedup: city.toml agents override pack.toml agents with the same
		// name. Build a set of city.toml agent names and skip pack.toml
		// agents that would duplicate.
		cityAgentNames := make(map[string]bool)
		for _, a := range cityAgents {
			cityAgentNames[a.Name] = true
		}
		var packAgents []Agent
		for _, a := range pc.Agents {
			if !cityAgentNames[a.Name] {
				packAgents = append(packAgents, a)
			}
		}
		// Merge pack.toml imports into city imports (pack is base).
		if len(pc.Imports) > 0 {
			if root.Imports == nil {
				root.Imports = make(map[string]Import)
			}
			for name, imp := range pc.Imports {
				if _, exists := root.Imports[name]; !exists {
					root.Imports[name] = imp
				}
			}
		}
		defaultRigIncludes, err := defaultRigIncludesFromPackDefaults(pc.Defaults, md)
		if err != nil {
			return nil, nil, fmt.Errorf("city pack.toml: %w", err)
		}
		if len(defaultRigIncludes) > 0 {
			root.Workspace.DefaultRigIncludes = append(defaultRigIncludes, root.Workspace.DefaultRigIncludes...)
		}
		// Merge pack.toml providers (pack is base, city wins).
		if len(pc.Providers) > 0 {
			if root.Providers == nil {
				root.Providers = make(map[string]ProviderSpec)
			}
			for name, spec := range pc.Providers {
				if _, exists := root.Providers[name]; !exists {
					root.Providers[name] = spec
				}
			}
		}
		// Merge named sessions.
		root.NamedSessions = append(pc.NamedSessions, root.NamedSessions...)
		// Merge root-pack services as the portable base layer. city.toml
		// services stay later in the slice and therefore remain the more
		// local declaration when callers inspect the merged config.
		if len(pc.Services) > 0 {
			packServices := make([]Service, len(pc.Services))
			copy(packServices, pc.Services)
			for i := range packServices {
				if packServices[i].PublishMode == "direct" {
					return nil, nil, fmt.Errorf("city pack.toml: service %q: packs may not set publish_mode=direct", packServices[i].Name)
				}
				packServices[i].SourceDir = cityRoot
			}
			root.Services = append(packServices, root.Services...)
		}
		// Merge patches (accumulated, applied later).
		root.Patches.Agents = append(pc.Patches.Agents, root.Patches.Agents...)
		root.Patches.Rigs = append(pc.Patches.Rigs, root.Patches.Rigs...)
		root.Patches.Providers = append(pc.Patches.Providers, root.Patches.Providers...)
		// Merge formulas config with pack.toml as the base and city.toml as
		// the more local override.
		if root.Formulas.Dir == "" {
			root.Formulas = pc.Formulas
		}
		// Merge pack-level agent defaults before city fragments so the
		// city layer can append on top of the portable baseline.
		mergedAgentDefaults := pc.AgentDefaults
		mergeAgentDefaults(&mergedAgentDefaults, root.AgentDefaults, packPath, nil)
		root.AgentDefaults = mergedAgentDefaults
		if len(pc.Global.SessionLive) > 0 {
			rootPackGlobals = append(rootPackGlobals, ResolvedPackGlobal{
				SessionLive: resolveConfigDirInCommands(pc.Global.SessionLive, cityRoot),
				PackName:    pc.Pack.Name,
			})
		}
		// Track pack.toml agents in provenance.
		trackAgents(prov, pc.Agents, packPath)
		prov.Sources = append(prov.Sources, packPath)
		prov.recordSource(packPath, packData)

		packCommands, err := DiscoverPackCommands(fs, cityRoot, pc.Pack.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("city pack.toml: %w", err)
		}
		packCommands = append(packCommands, legacyPackCommands(pc.Commands, cityRoot, pc.Pack.Name)...)
		if len(packCommands) > 0 {
			root.PackCommands = appendDiscoveredCommands(root.PackCommands, stampDefaultBinding(packCommands, pc.Pack.Name)...)
		}

		packDoctors, err := DiscoverPackDoctors(fs, cityRoot, pc.Pack.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("city pack.toml: %w", err)
		}
		legacyDoctors, err := legacyPackDoctors(fs, pc.Doctor, cityRoot, pc.Pack.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("city pack.toml: %w", err)
		}
		packDoctors = append(packDoctors, legacyDoctors...)
		if len(packDoctors) > 0 {
			root.PackDoctors = appendDiscoveredDoctors(root.PackDoctors, packDoctors...)
		}

		if root.PackSkillsDir == "" || root.PackMCPDir == "" {
			skillsDir, mcpDir := DiscoverPackAttachmentRoots(fs, cityRoot)
			if root.PackSkillsDir == "" {
				root.PackSkillsDir = skillsDir
			}
			if root.PackMCPDir == "" {
				root.PackMCPDir = mcpDir
			}
		}

		// Convention-discovered agents from the city pack root.
		// Explicit pack.toml agents win over discovered agents, and
		// city.toml agents win over both.
		skipNames := agentNameSet(packAgents)
		for _, a := range cityAgents {
			skipNames[a.Name] = true
		}
		packDiscoveredAgents, err := DiscoverPackAgents(fs, cityRoot, pc.Pack.Name, skipNames)
		if err != nil {
			return nil, nil, fmt.Errorf("city pack.toml: %w", err)
		}
		trackAgents(prov, packDiscoveredAgents, packPath)
		root.Agents = append([]Agent{}, packAgents...)
		root.Agents = append(root.Agents, packDiscoveredAgents...)
		root.Agents = append(root.Agents, cityAgents...)
	} // end pack.toml merge

	// V2 guidance: when pack.toml exists, city.toml imports should move
	// to pack.toml (imports are definition, city.toml is deployment).
	// Warn but don't error — city.toml imports still work for compatibility.
	if packExists && cityImportCount > 0 {
		prov.Warnings = append(prov.Warnings,
			fmt.Sprintf("city.toml declares %d [imports] — consider moving them to pack.toml (imports are definition, city.toml is deployment)", cityImportCount))
	}

	// Track root's resources.
	trackAgents(prov, cityAgentsForProvenance, path)
	trackRigs(prov, root.Rigs, path)
	trackWorkspace(prov, rootMeta, path)

	// Extract includes for processing. CLI -f files are appended after.
	// Preserve the original Include value so Marshal() round-trips it.
	// Pack includes (pack.toml paths) are separated and handled later
	// via Workspace.Includes → ExpandCityPacks.
	origInclude := root.Include
	includes := append([]string{}, root.Include...)
	var packIncludes []string
	for _, inc := range extraIncludes {
		// Detect pack directories (contain pack.toml) vs TOML fragments.
		if info, err := fs.Stat(inc); err == nil && info.IsDir() {
			packIncludes = append(packIncludes, inc)
		} else {
			includes = append(includes, inc)
		}
	}
	root.Include = origInclude

	for _, inc := range includes {
		var fragPath string
		if isRemoteInclude(inc) || isGitHubTreeURL(inc) {
			resolved, err := resolvePackRef(inc, cityRoot, cityRoot)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving include %q: %w", inc, err)
			}
			fragPath = resolved
		} else {
			fragPath = resolveConfigPath(inc, cityRoot, cityRoot)
		}

		fragData, err := fs.ReadFile(fragPath)
		if err != nil {
			return nil, nil, fmt.Errorf("loading fragment %q: %w", inc, err)
		}

		frag, fragMeta, fragWarnings, err := parseWithMeta(fragData, fragPath)
		if err != nil {
			return nil, nil, fmt.Errorf("fragment %q: %w", inc, err)
		}
		prov.Warnings = append(prov.Warnings, fragWarnings...)

		// Fragments cannot include other fragments.
		if len(frag.Include) > 0 {
			return nil, nil, fmt.Errorf(
				"fragment %q: includes are not allowed in fragments (no recursive includes)", inc)
		}

		// Adjust fragment agent paths to be city-root-relative.
		fragDir := filepath.Dir(fragPath)
		adjustAgentPaths(frag.Agents, fragDir, cityRoot)
		adjustPatchPaths(&frag.Patches, fragDir, cityRoot)
		adjustRigOverridePaths(frag.Rigs, fragDir, cityRoot)

		// Merge fragment into root.
		mergeFragment(root, frag, fragMeta, fragPath, prov)
		prov.Sources = append(prov.Sources, fragPath)
		prov.recordSource(fragPath, fragData)
	}

	// Inject system pack includes into Workspace.Includes. These are
	// appended AFTER user includes so user packs override system pack
	// fallbacks via the normal dedup/fallback resolution.
	// Skip packs already reachable from user includes or top-level imports
	// (avoids duplicate agent errors when a user pack transitively includes
	// a system pack).
	root.Workspace.Includes = append(rootPackIncludes, root.Workspace.Includes...)
	existingPacks := resolvedPackNames(root.Workspace.Includes, root.Imports, fs, cityRoot)
	for _, inc := range packIncludes {
		name := readPackNameFromDir(inc)
		if name != "" && existingPacks[name] {
			continue
		}
		root.Workspace.Includes = append(root.Workspace.Includes, inc)
	}

	adjustPatchPaths(&root.Patches, cityRoot, cityRoot)
	adjustRigOverridePaths(root.Rigs, cityRoot, cityRoot)

	// Resolve named pack references to cache paths before any expansion.
	resolveNamedPacks(root, cityRoot)

	implicitImports, implicitPath, implicitData, implicitErr := readImplicitImportsWithData()
	if implicitErr != nil {
		return nil, nil, implicitErr
	}
	if len(implicitImports) > 0 {
		// v0.15.1 collision gate: if a user's [imports.<name>] would
		// silently shadow a **bootstrap** implicit-import pack, hard-stop
		// with a diagnostic. Non-bootstrap implicit imports retain the
		// pre-v0.15.1 "explicit wins over implicit" contract and are
		// shadowed silently (see docs/packv2/doc-packman.md). See
		// engdocs/proposals/skill-materialization.md — "Name-collision
		// with a user-declared [imports.core]".
		bootstrapNames := bootstrapImportNames(implicitImports)
		if collisions := collidesWithImplicitImports(root.Imports, bootstrapNames); len(collisions) > 0 {
			names := strings.Join(collisions, ", ")
			return nil, nil, fmt.Errorf(
				"gc: city pack declares [imports.%s] which shadows the bootstrap implicit import(s) with the same name; rename one side",
				names,
			)
		}

		if root.Imports == nil {
			root.Imports = make(map[string]Import)
		}
		if root.ImplicitImportBindings == nil {
			root.ImplicitImportBindings = make(map[string]bool)
		}
		if root.BootstrapImportBindings == nil {
			root.BootstrapImportBindings = make(map[string]bool)
		}
		addedImplicit := false
		bootstrapSet := make(map[string]bool, len(bootstrapNames))
		for _, name := range bootstrapNames {
			bootstrapSet[name] = true
		}
		for name, imp := range implicitImports {
			if !bootstrapSet[name] {
				continue
			}
			if _, exists := root.Imports[name]; exists {
				continue
			}
			root.Imports[name] = resolveImplicitImport(imp)
			prov.Imports[name] = "(implicit)"
			addedImplicit = true
			root.ImplicitImportBindings[name] = true
			if bootstrapSet[name] {
				root.BootstrapImportBindings[name] = true
			}
		}
		if addedImplicit && implicitPath != "" {
			prov.Sources = append(prov.Sources, implicitPath)
			if implicitData != nil {
				prov.recordSource(implicitPath, implicitData)
			}
		}
	}

	// Expand city packs before patches (so patches can target city-topo agents).
	cityTopoFormulas, cityReqs, shadowWarnings, ctErr := expandCityPacks(root, fs, cityRoot, opts)
	if ctErr != nil {
		return nil, nil, ctErr
	}
	cityReqs = append(cityReqs, rootPackRequires...)
	prov.Warnings = append(prov.Warnings, shadowWarnings...)
	if len(root.LoadWarnings) > 0 {
		prov.Warnings = appendUnique(prov.Warnings, root.LoadWarnings...)
	}
	// Track city pack agents in provenance.
	for _, ref := range root.Workspace.Includes {
		topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
		topoPath := filepath.Join(topoDir, packFile)
		for _, a := range root.Agents {
			if a.Dir == "" {
				if _, tracked := prov.Agents[a.QualifiedName()]; !tracked {
					prov.Agents[a.QualifiedName()] = topoPath
				}
			}
		}
	}

	// Apply patches after all fragments are merged + city packs expanded.
	if !root.Patches.IsEmpty() {
		if err := ApplyPatches(root, root.Patches); err != nil {
			return nil, nil, fmt.Errorf("applying patches: %w", err)
		}
		root.Patches = Patches{} // clear after application
	}

	// Expand rig packs after patches (pack agents get rig overrides).
	rigFormulaDirs := make(map[string][]string)
	if HasPackRigs(root.Rigs) {
		if err := expandPacks(root, fs, cityRoot, rigFormulaDirs, opts); err != nil {
			return nil, nil, fmt.Errorf("expanding packs: %w", err)
		}
		if len(root.LoadWarnings) > 0 {
			prov.Warnings = appendUnique(prov.Warnings, root.LoadWarnings...)
		}
		// Track pack-expanded agents in provenance.
		for _, r := range root.Rigs {
			topoRefs := r.Includes
			for _, ref := range topoRefs {
				topoDir, _ := resolvePackRef(ref, cityRoot, cityRoot)
				topoPath := filepath.Join(topoDir, packFile)
				for _, a := range root.Agents {
					if a.Dir == r.Name {
						prov.Agents[a.QualifiedName()] = topoPath
					}
				}
			}
		}
	}

	// Apply [global] sections from packs to agents in scope.
	root.PackGlobals = append(root.PackGlobals, rootPackGlobals...)
	applyPackGlobals(root)

	// Validate city-scoped pack requirements.
	if err := validateCityRequirements(cityReqs, root.Agents); err != nil {
		return nil, nil, err
	}

	// Compute formula layers from all sources.
	// Always use FormulasDir() which defaults to "formulas" when
	// [formulas] is not explicitly configured in city.toml.
	cityLocalFormulas := citylayout.ResolveFormulasDir(cityRoot, root.FormulasDir())
	root.FormulaLayers = ComputeFormulaLayers(
		cityTopoFormulas, cityLocalFormulas, rigFormulaDirs, root.Rigs, cityRoot)

	// Inject implicit agents for built-in providers not already defined.
	// Must happen after all composition (fragments, packs, patches) so
	// explicit agents always take precedence.
	InjectImplicitAgents(root)

	// Apply [agent_defaults] values to all agents (explicit and implicit)
	// that don't set their own override. Deprecated [agents] aliases are
	// normalized during parse/load before composition reaches this point.
	if root.AgentDefaults.DefaultSlingFormula != "" {
		for i := range root.Agents {
			if root.Agents[i].BindingName == "" {
				root.Agents[i].InheritedDefaultSlingFormula = nil
			}
		}
	}
	ApplyAgentDefaults(root)

	// Canonicalize duration-or-"off" session sleep fields after all config
	// layers have been applied so runtime consumers can trust the values.
	NormalizeSessionSleepFields(root)

	siteBindingWarnings, err := ApplySiteBindings(fs, cityRoot, root)
	if err != nil {
		return nil, nil, err
	}
	prov.Warnings = append(prov.Warnings, siteBindingWarnings...)

	// Validate named session declarations after pack expansion and site
	// binding resolution so stamped identities and deterministic runtime
	// names reflect the effective workspace identity.
	if err := ValidateNamedSessions(root); err != nil {
		return nil, nil, err
	}

	// Validate all duration strings in the fully-merged config.
	prov.Warnings = append(prov.Warnings, ValidateDurations(root, path)...)

	// Validate cross-entity semantic constraints.
	prov.Warnings = append(prov.Warnings, ValidateSemantics(root, path)...)
	prov.Warnings = append(prov.Warnings, DetectLegacyProviderInheritance(root, path)...)

	// Build the resolved provider cache now that compose + patch have
	// populated the full provider table. Chain resolution errors
	// (cycles, unknown base, wrapper-resume missing) surface here so
	// they fail at config load rather than at session spawn. If the
	// cache cannot be built, emit a warning and leave the cache nil —
	// callers can still fall back to ResolveProvider per lookup.
	if err := BuildResolvedProviderCache(root); err != nil {
		return nil, nil, fmt.Errorf("%s: provider cache build failed: %w", path, err)
	}

	// v0.15.1: enrich every agent with its convention-discovered
	// agent-local asset paths (agents/<name>/skills/, agents/<name>/mcp/).
	// DiscoverPackAgents only does this for agents it creates — it skips
	// names already present in pack.toml [[agent]] or city.toml
	// [[agent]] entries, so those agents leave the discovery pass with
	// empty SkillsDir/MCPDir even when agents/<name>/skills/ exists on
	// disk. The materializer and collision validator both key off
	// SkillsDir, so that gap silently loses agent-local skills for every
	// explicitly-declared agent. Populate the fields here so the
	// convention works uniformly.
	populateAgentLocalAssetDirs(fs, root, cityRoot)

	// Load namepool files for pool agents.
	loadNamepools(fs, root, cityRoot)

	// Backwards compat: promote deprecated graph_workflows → formula_v2.
	if root.Daemon.GraphWorkflows && !root.Daemon.FormulaV2 {
		root.Daemon.FormulaV2 = true
	}

	// v0.15.1: emit a one-time deprecation warning if the loaded config
	// still populates the v0.15.0 attachment-list tombstone fields. The
	// fields still parse (TOML won't error) but are ignored by the new
	// materializer.
	if warning := WarnDeprecatedAttachmentFields(root); warning != "" {
		prov.Warnings = append(prov.Warnings, warning)
	}

	// Capture revision inputs after all config and pack discovery so callers
	// can compare the loaded snapshot to future reloads without re-reading
	// mutable files from disk.
	prov.captureRevisionSnapshot(fs, root, cityRoot)
	return root, prov, nil
}

// adjustPatchPaths resolves patch session_setup_script values to absolute
// paths rooted at the declaring config directory. Patches do not retain
// independent source provenance after merge, so runtime cannot otherwise
// distinguish whether a relative override came from the target agent's source
// or from the patch file itself.
func adjustPatchPaths(patches *Patches, declDir, cityRoot string) {
	for i := range patches.Agents {
		p := &patches.Agents[i]
		if p.SessionSetupScript == nil || *p.SessionSetupScript == "" {
			continue
		}
		v := resolveConfigPath(*p.SessionSetupScript, declDir, cityRoot)
		p.SessionSetupScript = &v
	}
}

// adjustRigOverridePaths resolves rig override session_setup_script values to
// absolute paths rooted at the declaring config directory. Once overrides are
// applied to pack-stamped agents, runtime only sees the target agent's
// SourceDir, so relative override paths must be normalized during composition.
func adjustRigOverridePaths(rigs []Rig, declDir, cityRoot string) {
	for i := range rigs {
		adjustAgentOverridePaths(rigs[i].Overrides, declDir, cityRoot)
		adjustAgentOverridePaths(rigs[i].RigPatches, declDir, cityRoot)
	}
}

func adjustAgentOverridePaths(overrides []AgentOverride, declDir, cityRoot string) {
	for i := range overrides {
		ov := &overrides[i]
		if ov.SessionSetupScript == nil || *ov.SessionSetupScript == "" {
			continue
		}
		v := resolveConfigPath(*ov.SessionSetupScript, declDir, cityRoot)
		ov.SessionSetupScript = &v
	}
}

// populateAgentLocalAssetDirs fills Agent.SkillsDir and Agent.MCPDir for
// every agent whose convention path exists on disk but wasn't already
// set by DiscoverPackAgents (e.g., because the agent was explicitly
// declared in pack.toml or city.toml and therefore skipped by the
// convention-discovery pass). Agents whose field is already set keep
// it — so a pack that already carried SkillsDir via discovery isn't
// overwritten.
func populateAgentLocalAssetDirs(fs fsys.FS, root *City, cityRoot string) {
	if root == nil {
		return
	}
	for i := range root.Agents {
		a := &root.Agents[i]
		base := a.SourceDir
		if base == "" {
			base = cityRoot
		}
		applyAgentConventionDefaults(fs, base, a)
		if a.SkillsDir == "" {
			skillsDir := filepath.Join(base, "agents", a.Name, "skills")
			if info, err := fs.Stat(skillsDir); err == nil && info.IsDir() {
				a.SkillsDir = skillsDir
			}
		}
		if a.MCPDir == "" {
			mcpDir := filepath.Join(base, "agents", a.Name, "mcp")
			if info, err := fs.Stat(mcpDir); err == nil && info.IsDir() {
				a.MCPDir = mcpDir
			}
		}
	}
}

// collidesWithImplicitImports reports which bootstrap implicit-import
// names are shadowed by an explicit [imports.<name>] entry on the loaded
// city. Returns colliding names in sorted order; an empty slice means
// no collision.
//
// This mirrors internal/bootstrap.CollidesWithBootstrapPack but stays in
// the config package to avoid an import cycle (bootstrap already imports
// config). The two callers agree on the predicate: any user-declared
// binding name equal to an implicit-import binding name is a collision.
//
// Only bootstrap-managed implicit import names should be passed in
// here — user-added implicit imports retain the pre-v0.15.1 "explicit
// wins over implicit" contract and are not subject to the hard stop.
// Callers must pre-filter the name set via bootstrapImportNames.
func collidesWithImplicitImports(userImports map[string]Import, implicitNames []string) []string {
	if len(userImports) == 0 || len(implicitNames) == 0 {
		return nil
	}
	var collisions []string
	seen := make(map[string]struct{}, len(implicitNames))
	for _, name := range implicitNames {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, exists := userImports[name]; exists {
			collisions = append(collisions, name)
		}
	}
	sort.Strings(collisions)
	return collisions
}

// bootstrapManagedImportNames lists the implicit-import binding names
// that are managed by the gc binary's bootstrap pack mechanism (see
// internal/bootstrap/bootstrap.go:BootstrapPacks). This list must stay
// in sync with that slice — a Go unit test
// (TestBootstrapManagedNames_MatchesBootstrapPacks in
// internal/bootstrap) asserts the two agree by calling
// BootstrapManagedImportNames() and comparing to BootstrapPackNames().
//
// Only these names participate in the v0.15.1 hard-stop collision gate.
// User-added implicit imports (e.g. custom entries that a user wrote
// into ~/.gc/implicit-import.toml by hand) retain the pre-v0.15.1
// "explicit wins over implicit" contract and are shadowed silently.
var bootstrapManagedImportNames = []string{"registry", "core"}

// BootstrapManagedImportNames returns a copy of the bootstrap-managed
// implicit-import binding names recognized by the composer's collision
// gate. Exported so the bootstrap package's sync test can assert the
// two lists agree.
func BootstrapManagedImportNames() []string {
	out := make([]string, len(bootstrapManagedImportNames))
	copy(out, bootstrapManagedImportNames)
	return out
}

func resolveImplicitImport(imp ImplicitImport) Import {
	out := Import{Source: strings.TrimSpace(imp.Source)}
	if version := strings.TrimSpace(imp.Version); version != "" {
		out.Version = version
	} else if commit := strings.TrimSpace(imp.Commit); commit != "" {
		out.Version = "sha:" + commit
	}
	return out
}

// bootstrapImportNames filters the caller-supplied implicit-import map
// down to the subset of names that are bootstrap-managed. Used by the
// compose-time collision gate so we only hard-stop on names the gc
// binary owns.
func bootstrapImportNames(implicit map[string]ImplicitImport) []string {
	if len(implicit) == 0 {
		return nil
	}
	managed := make(map[string]struct{}, len(bootstrapManagedImportNames))
	for _, name := range bootstrapManagedImportNames {
		managed[name] = struct{}{}
	}
	var names []string
	for name := range implicit {
		if _, ok := managed[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// validateCityRequirements checks that all city-scoped pack requirements
// are satisfied by the expanded agent list.
func validateCityRequirements(reqs []PackRequirement, agents []Agent) error {
	for _, req := range reqs {
		if req.Scope != "city" {
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
			return fmt.Errorf("pack requires city agent %q — include a pack that provides it", req.Agent)
		}
	}
	return nil
}

// mergeFragment merges a fragment into the base config in-place.
// Arrays concatenate, providers deep-merge, workspace per-field merges.
func mergeFragment(base, fragment *City, fragMeta toml.MetaData, fragPath string, prov *Provenance) {
	// Agents and named sessions: concatenate.
	trackAgents(prov, fragment.Agents, fragPath)
	base.Agents = append(base.Agents, fragment.Agents...)
	base.NamedSessions = append(base.NamedSessions, fragment.NamedSessions...)

	// Rigs: concatenate.
	trackRigs(prov, fragment.Rigs, fragPath)
	base.Rigs = append(base.Rigs, fragment.Rigs...)

	// Services: concatenate.
	base.Services = append(base.Services, fragment.Services...)

	// Providers: deep-merge per-field.
	mergeProviders(base, fragment, fragMeta, fragPath, prov)

	// Workspace: per-field merge.
	mergeWorkspace(base, fragment, fragMeta, fragPath, prov)

	// Packs: additive merge.
	mergePacks(base, fragment, fragPath, prov)

	// Patches: accumulate from fragments (applied after all merges).
	base.Patches.Agents = append(base.Patches.Agents, fragment.Patches.Agents...)
	base.Patches.Rigs = append(base.Patches.Rigs, fragment.Patches.Rigs...)
	base.Patches.Providers = append(base.Patches.Providers, fragment.Patches.Providers...)

	// Simple sections: last-writer-wins if fragment defines them.
	if fragMeta.IsDefined("beads") {
		base.Beads = fragment.Beads
	}
	if fragMeta.IsDefined("dolt") {
		base.Dolt = fragment.Dolt
	}
	if fragMeta.IsDefined("formulas") {
		base.Formulas = fragment.Formulas
	}
	if fragMeta.IsDefined("daemon") {
		base.Daemon = fragment.Daemon
	}
	if fragMeta.IsDefined("session") {
		base.Session = fragment.Session
	}
	if fragMeta.IsDefined("mail") {
		base.Mail = fragment.Mail
	}
	if fragMeta.IsDefined("events") {
		base.Events = fragment.Events
	}
	if fragMeta.IsDefined("orders") {
		base.Orders = fragment.Orders
	}
	if fragMeta.IsDefined("api") {
		base.API = fragment.API
	}
	mergeSessionSleep(base, fragment, fragMeta, fragPath, prov)
	if fragMeta.IsDefined("convergence") {
		base.Convergence = fragment.Convergence
	}
	if fragMeta.IsDefined("agent_defaults") || fragMeta.IsDefined("agents") {
		mergeAgentDefaults(&base.AgentDefaults, fragment.AgentDefaults, fragPath, prov)
	}
}

type sessionSleepField struct {
	key string
	get func() string
	set func()
}

func sessionSleepMergeFields(base, fragment *City) []sessionSleepField {
	return []sessionSleepField{
		{
			key: "interactive_resume",
			get: func() string { return base.SessionSleep.InteractiveResume },
			set: func() { base.SessionSleep.InteractiveResume = fragment.SessionSleep.InteractiveResume },
		},
		{
			key: "interactive_fresh",
			get: func() string { return base.SessionSleep.InteractiveFresh },
			set: func() { base.SessionSleep.InteractiveFresh = fragment.SessionSleep.InteractiveFresh },
		},
		{
			key: "noninteractive",
			get: func() string { return base.SessionSleep.NonInteractive },
			set: func() { base.SessionSleep.NonInteractive = fragment.SessionSleep.NonInteractive },
		},
	}
}

func mergeSessionSleep(base, fragment *City, fragMeta toml.MetaData, fragPath string, prov *Provenance) {
	for _, field := range sessionSleepMergeFields(base, fragment) {
		if !fragMeta.IsDefined("session_sleep", field.key) {
			continue
		}
		if field.get() != "" {
			prov.Warnings = append(prov.Warnings,
				fmt.Sprintf("session_sleep.%s redefined by %q", field.key, fragPath))
		}
		field.set()
	}
}

// mergePacks additively merges fragment packs into base.
// New pack names are added. Duplicate names generate a warning.
func mergePacks(base, fragment *City, fragPath string, prov *Provenance) {
	if len(fragment.Packs) == 0 {
		return
	}
	if base.Packs == nil {
		base.Packs = make(map[string]PackSource)
	}
	for name, src := range fragment.Packs {
		if _, exists := base.Packs[name]; exists {
			prov.Warnings = append(prov.Warnings,
				fmt.Sprintf("pack %q redefined by %q", name, fragPath))
		}
		base.Packs[name] = src
	}
}

// mergeProviders deep-merges fragment providers into base providers.
// New providers are added. Existing providers are merged per-field with
// collision warnings.
func mergeProviders(base, fragment *City, fragMeta toml.MetaData, fragPath string, prov *Provenance) {
	if len(fragment.Providers) == 0 {
		return
	}
	if base.Providers == nil {
		base.Providers = make(map[string]ProviderSpec)
	}
	for name, fragSpec := range fragment.Providers {
		baseSpec, exists := base.Providers[name]
		if !exists {
			base.Providers[name] = fragSpec
			continue
		}
		base.Providers[name] = deepMergeProvider(
			baseSpec, fragSpec, name, fragMeta, fragPath, prov)
	}
}

// deepMergeProvider merges fragment provider fields into base field by field.
// Only explicitly-defined fields in the fragment override the base.
// Warns when both define the same field (accidental collision).
func deepMergeProvider(base, frag ProviderSpec, name string, fragMeta toml.MetaData, fragPath string, prov *Provenance) ProviderSpec {
	result := base

	// Scalar fields: override if fragment defines them.
	type scalarField struct {
		key     string
		hasBase func() bool
		apply   func()
	}
	scalars := []scalarField{
		{
			"display_name",
			func() bool { return base.DisplayName != "" },
			func() { result.DisplayName = frag.DisplayName },
		},
		{
			"command",
			func() bool { return base.Command != "" },
			func() { result.Command = frag.Command },
		},
		{
			"prompt_mode",
			func() bool { return base.PromptMode != "" },
			func() { result.PromptMode = frag.PromptMode },
		},
		{
			"prompt_flag",
			func() bool { return base.PromptFlag != "" },
			func() { result.PromptFlag = frag.PromptFlag },
		},
		{
			"ready_delay_ms",
			func() bool { return base.ReadyDelayMs != 0 },
			func() { result.ReadyDelayMs = frag.ReadyDelayMs },
		},
		{
			"ready_prompt_prefix",
			func() bool { return base.ReadyPromptPrefix != "" },
			func() { result.ReadyPromptPrefix = frag.ReadyPromptPrefix },
		},
		{
			"emits_permission_warning",
			func() bool { return base.EmitsPermissionWarning != nil },
			func() { result.EmitsPermissionWarning = frag.EmitsPermissionWarning },
		},
	}
	for _, sf := range scalars {
		if fragMeta.IsDefined("providers", name, sf.key) {
			if sf.hasBase() {
				prov.Warnings = append(prov.Warnings,
					fmt.Sprintf("provider %q.%s redefined by %q", name, sf.key, fragPath))
			}
			sf.apply()
		}
	}

	// Slice fields: replace entirely.
	if fragMeta.IsDefined("providers", name, "args") {
		if len(base.Args) > 0 {
			prov.Warnings = append(prov.Warnings,
				fmt.Sprintf("provider %q.args redefined by %q", name, fragPath))
		}
		result.Args = make([]string, len(frag.Args))
		copy(result.Args, frag.Args)
	}
	if fragMeta.IsDefined("providers", name, "process_names") {
		if len(base.ProcessNames) > 0 {
			prov.Warnings = append(prov.Warnings,
				fmt.Sprintf("provider %q.process_names redefined by %q", name, fragPath))
		}
		result.ProcessNames = make([]string, len(frag.ProcessNames))
		copy(result.ProcessNames, frag.ProcessNames)
	}

	// Env merges additively (individual keys override).
	// Clone the map to avoid mutating the original base Env.
	if fragMeta.IsDefined("providers", name, "env") {
		cloned := make(map[string]string, len(result.Env)+len(frag.Env))
		for k, v := range result.Env {
			cloned[k] = v
		}
		for k, v := range frag.Env {
			if _, exists := base.Env[k]; exists {
				prov.Warnings = append(prov.Warnings,
					fmt.Sprintf("provider %q.env.%s redefined by %q", name, k, fragPath))
			}
			cloned[k] = v
		}
		result.Env = cloned
	}

	return result
}

// mergeWorkspace per-field merges fragment workspace into base.
// Uses IsDefined() which works correctly for regular tables (not
// arrays-of-tables).
func mergeWorkspace(base, fragment *City, fragMeta toml.MetaData, fragPath string, prov *Provenance) {
	type wsField struct {
		key string
		get func() string
		set func()
	}
	fields := []wsField{
		{
			"name",
			func() string { return base.Workspace.Name },
			func() { base.Workspace.Name = fragment.Workspace.Name },
		},
		{
			"provider",
			func() string { return base.Workspace.Provider },
			func() { base.Workspace.Provider = fragment.Workspace.Provider },
		},
		{
			"start_command",
			func() string { return base.Workspace.StartCommand },
			func() { base.Workspace.StartCommand = fragment.Workspace.StartCommand },
		},
		{
			"session_template",
			func() string { return base.Workspace.SessionTemplate },
			func() { base.Workspace.SessionTemplate = fragment.Workspace.SessionTemplate },
		},
	}
	for _, f := range fields {
		if fragMeta.IsDefined("workspace", f.key) {
			if f.get() != "" {
				prov.Warnings = append(prov.Warnings,
					fmt.Sprintf("workspace.%s redefined by %q", f.key, fragPath))
			}
			f.set()
			prov.Workspace[f.key] = fragPath
		}
	}
	// install_agent_hooks is a []string — handle outside the wsField loop.
	if fragMeta.IsDefined("workspace", "install_agent_hooks") {
		if len(base.Workspace.InstallAgentHooks) > 0 {
			prov.Warnings = append(prov.Warnings,
				fmt.Sprintf("workspace.install_agent_hooks redefined by %q", fragPath))
		}
		base.Workspace.InstallAgentHooks = append([]string(nil), fragment.Workspace.InstallAgentHooks...)
		prov.Workspace["install_agent_hooks"] = fragPath
	}
	// includes is a []string — additive merge (append, not replace).
	if fragMeta.IsDefined("workspace", "includes") {
		base.Workspace.Includes = append(
			base.Workspace.Includes, fragment.Workspace.Includes...)
		prov.Workspace["includes"] = fragPath
	}
	// default_rig_includes is a []string — additive merge (append, not replace).
	if fragMeta.IsDefined("workspace", "default_rig_includes") {
		base.Workspace.DefaultRigIncludes = append(
			base.Workspace.DefaultRigIncludes, fragment.Workspace.DefaultRigIncludes...)
		prov.Workspace["default_rig_includes"] = fragPath
	}
	// global_fragments is a []string — additive merge (append, not replace).
	if fragMeta.IsDefined("workspace", "global_fragments") {
		base.Workspace.GlobalFragments = append(
			base.Workspace.GlobalFragments, fragment.Workspace.GlobalFragments...)
		prov.Workspace["global_fragments"] = fragPath
	}
}

// resolveConfigPath resolves a path for composition. Paths prefixed with
// "//" resolve relative to the city root (Bazel convention). Other relative
// paths resolve relative to declDir.
func resolveConfigPath(p, declDir, cityRoot string) string {
	if strings.HasPrefix(p, "//") {
		return filepath.Join(cityRoot, strings.TrimPrefix(p, "//"))
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(declDir, p)
}

// adjustAgentPaths converts relative prompt_template, overlay_dir, and
// namepool paths in fragment agents to be city-root-relative, based on the
// fragment's directory. session_setup_script is left as-authored so runtime
// resolution can interpret it relative to SourceDir. SourceDir is always set
// so session_setup templates and runtime path resolution know the declaring
// config directory.
func adjustAgentPaths(agents []Agent, fragDir, cityRoot string) {
	for i := range agents {
		agents[i].SourceDir = fragDir
		if agents[i].PromptTemplate != "" {
			agents[i].PromptTemplate = adjustFragmentPath(
				agents[i].PromptTemplate, fragDir, cityRoot)
		}
		if agents[i].OverlayDir != "" {
			agents[i].OverlayDir = adjustFragmentPath(
				agents[i].OverlayDir, fragDir, cityRoot)
		}
		if agents[i].Namepool != "" {
			agents[i].Namepool = adjustFragmentPath(
				agents[i].Namepool, fragDir, cityRoot)
		}
	}
}

// loadNamepools loads namepool files for all agents with a configured
// namepool path. Called after all path adjustment and composition is complete.
func loadNamepools(fs fsys.FS, cfg *City, cityRoot string) {
	for i := range cfg.Agents {
		if cfg.Agents[i].Namepool == "" {
			continue
		}
		path := cfg.Agents[i].Namepool
		if !filepath.IsAbs(path) {
			path = filepath.Join(cityRoot, path)
		}
		names, err := LoadNamepool(fs, path)
		if err != nil {
			continue // silent fallback to numeric names
		}
		cfg.Agents[i].NamepoolNames = names
	}
}

// adjustFragmentPath converts a fragment-relative path to city-root-relative.
// "//" paths resolve to city root. Absolute paths pass through unchanged.
func adjustFragmentPath(p, fragDir, cityRoot string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "//") {
		return strings.TrimPrefix(p, "//")
	}
	if filepath.IsAbs(p) {
		return p
	}
	// Fragment-relative → absolute → city-root-relative.
	abs := filepath.Join(fragDir, p)
	rel, err := filepath.Rel(cityRoot, abs)
	if err != nil {
		return abs
	}
	return rel
}

// parseWithMeta parses TOML data into a City, preserving metadata for
// field-level merge decisions. Also returns warnings for unknown keys.
func parseWithMeta(data []byte, source string) (*City, toml.MetaData, []string, error) {
	var cfg City
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, md, nil, fmt.Errorf("parsing config: %w", err)
	}
	normalizeAgentDefaultsAlias(&cfg, md)
	warnings := agentDefaultsCompatibilityWarnings(md, source)
	normalizeLegacyOrderOverrideAliases(&cfg)
	warnings = append(warnings, CheckUndecodedKeys(md, source)...)
	return &cfg, md, warnings, nil
}

// LoadRootPackDefaultRigImports loads the canonical [defaults.rig.imports]
// entries from the root pack without expanding the full config.
func LoadRootPackDefaultRigImports(fs fsys.FS, cityRoot string) ([]BoundImport, error) {
	packPath := filepath.Join(cityRoot, packFile)
	packData, err := fs.ReadFile(packPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading city pack.toml: %w", err)
	}
	var pc packConfig
	md, err := toml.Decode(string(packData), &pc)
	if err != nil {
		return nil, fmt.Errorf("parsing city pack.toml: %w", err)
	}
	if warnings := fatalUndecodedWarnings(md, packPath); len(warnings) > 0 {
		return nil, fmt.Errorf("parsing city pack.toml: %s", strings.Join(warnings, "; "))
	}
	return defaultRigImportsFromPackDefaults(pc.Defaults, md)
}

func defaultRigImportsFromPackDefaults(defaults packDefaults, md toml.MetaData) ([]BoundImport, error) {
	if len(defaults.Rig.Imports) == 0 {
		return nil, nil
	}

	names := orderedDefaultRigImportNames(defaults.Rig.Imports, md)
	imports := make([]BoundImport, 0, len(names))
	for _, name := range names {
		imp := defaults.Rig.Imports[name]
		if strings.TrimSpace(imp.Source) == "" {
			return nil, fmt.Errorf("defaults.rig.imports.%s.source is required", name)
		}
		imports = append(imports, BoundImport{Binding: name, Import: imp})
	}
	return imports, nil
}

func defaultRigIncludesFromPackDefaults(defaults packDefaults, md toml.MetaData) ([]string, error) {
	imports, err := defaultRigImportsFromPackDefaults(defaults, md)
	if err != nil {
		return nil, err
	}
	includes := make([]string, 0, len(imports))
	for _, bound := range imports {
		includes = append(includes, bound.Import.Source)
	}
	return includes, nil
}

func orderedDefaultRigImportNames(imports map[string]Import, md toml.MetaData) []string {
	if len(imports) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(imports))
	names := make([]string, 0, len(imports))
	for _, key := range md.Keys() {
		if len(key) < 4 || key[0] != "defaults" || key[1] != "rig" || key[2] != "imports" {
			continue
		}
		name := key[3]
		if _, ok := imports[name]; ok && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == len(imports) {
		return names
	}
	remaining := make([]string, 0, len(imports)-len(names))
	for name := range imports {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(names, remaining...)
}

func newProvenance(rootPath string) *Provenance {
	return &Provenance{
		Root:      rootPath,
		Sources:   []string{rootPath},
		Imports:   make(map[string]string),
		Agents:    make(map[string]string),
		Rigs:      make(map[string]string),
		Workspace: make(map[string]string),
	}
}

func (p *Provenance) recordSource(path string, data []byte) {
	if p.sourceContents == nil {
		p.sourceContents = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	p.sourceContents[path] = cp
}

func trackAgents(prov *Provenance, agents []Agent, source string) {
	for _, a := range agents {
		prov.Agents[a.QualifiedName()] = source
	}
}

func trackRigs(prov *Provenance, rigs []Rig, source string) {
	for _, r := range rigs {
		prov.Rigs[r.Name] = source
	}
}

func trackWorkspace(prov *Provenance, meta toml.MetaData, source string) {
	for _, f := range []string{"name", "provider", "start_command", "session_template", "install_agent_hooks"} {
		if meta.IsDefined("workspace", f) {
			prov.Workspace[f] = source
		}
	}
}

// resolvedPackNames collects pack names that are reachable from a set of
// top-level include paths and imports. It walks both legacy [pack].includes
// and V2 [imports] transitively so builtin system-pack injection can be
// skipped when a user pack already brings the same pack into the city
// closure.
func resolvedPackNames(includes []string, imports map[string]Import, sysFS fsys.FS, cityRoot string) map[string]bool {
	names := make(map[string]bool, len(includes)+len(imports))
	seenDirs := make(map[string]bool)

	var visit func(ref, declDir string)
	visit = func(ref, declDir string) {
		dir, err := resolvePackRef(ref, declDir, cityRoot)
		if err != nil {
			return
		}
		absDir, absErr := filepath.Abs(dir)
		if absErr != nil {
			absDir = dir
		}
		if seenDirs[absDir] {
			return
		}
		seenDirs[absDir] = true

		data, err := sysFS.ReadFile(filepath.Join(dir, packFile))
		if err != nil {
			return
		}
		var pc struct {
			Pack struct {
				Name     string   `toml:"name"`
				Includes []string `toml:"includes"`
			} `toml:"pack"`
			Imports map[string]Import `toml:"imports"`
		}
		if _, decErr := toml.Decode(string(data), &pc); decErr != nil || pc.Pack.Name == "" {
			return
		}

		names[pc.Pack.Name] = true
		for _, sub := range pc.Pack.Includes {
			visit(sub, dir)
		}
		for _, imp := range pc.Imports {
			visit(imp.Source, dir)
		}
	}

	for _, inc := range includes {
		visit(inc, cityRoot)
	}
	for _, imp := range imports {
		visit(imp.Source, cityRoot)
	}
	return names
}

// readPackNameFromDir reads [pack].name from pack.toml in the given directory.
func readPackNameFromDir(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, packFile))
	if err != nil {
		return ""
	}
	var pc struct {
		Pack struct {
			Name string `toml:"name"`
		} `toml:"pack"`
	}
	if _, err := toml.Decode(string(data), &pc); err != nil {
		return ""
	}
	return pc.Pack.Name
}
