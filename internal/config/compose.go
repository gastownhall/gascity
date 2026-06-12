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
	"github.com/gastownhall/gascity/internal/pricing"
)

// mergePricingByKey merges base and override pricing slices keyed by
// (provider, model). When the same key appears in both, the override entry
// wins. Duplicate keys within either input are collapsed to their last
// occurrence. The returned slice preserves surviving base order followed by
// surviving override-only entries in their original order. Used to compose
// pack->city pricing layers during config load.
func mergePricingByKey(base, override []pricing.ModelPricing) []pricing.ModelPricing {
	base = dedupePricingByKey(base)
	override = dedupePricingByKey(override)
	if len(base) == 0 {
		out := make([]pricing.ModelPricing, len(override))
		copy(out, override)
		return out
	}
	overrideIdx := make(map[string]int, len(override))
	for i, p := range override {
		overrideIdx[pricing.Key(p.Provider, p.Model)] = i
	}
	out := make([]pricing.ModelPricing, 0, len(base)+len(override))
	usedOverride := make(map[int]bool, len(override))
	for _, b := range base {
		if i, ok := overrideIdx[pricing.Key(b.Provider, b.Model)]; ok {
			out = append(out, override[i])
			usedOverride[i] = true
			continue
		}
		out = append(out, b)
	}
	for i, o := range override {
		if !usedOverride[i] {
			out = append(out, o)
		}
	}
	return out
}

func dedupePricingByKey(in []pricing.ModelPricing) []pricing.ModelPricing {
	if len(in) == 0 {
		return nil
	}
	lastIdx := make(map[string]int, len(in))
	for i, p := range in {
		lastIdx[pricing.Key(p.Provider, p.Model)] = i
	}
	out := make([]pricing.ModelPricing, 0, len(lastIdx))
	for i, p := range in {
		if lastIdx[pricing.Key(p.Provider, p.Model)] == i {
			out = append(out, p)
		}
	}
	return out
}

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
	// AllowMissingProviderReferences leaves provider-reference catalog errors
	// non-fatal for repair tools that need to inspect broken configs.
	AllowMissingProviderReferences bool
	deferRigPatches                bool
	deferredRigPatches             *[]deferredRigPatches
	allowLegacyOrderLayouts        bool
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
	root.Pricing = dedupePricingByKey(root.Pricing)
	root.CityPricing = append([]pricing.ModelPricing(nil), root.Pricing...)
	defaultRigImports, err := defaultRigImportsFromPackDefaults(root.Defaults, rootMeta)
	if err != nil {
		return nil, nil, fmt.Errorf("city.toml: %w", err)
	}
	if len(defaultRigImports) > 0 {
		root.DefaultRigImports = make(map[string]Import, len(defaultRigImports))
		root.DefaultRigImportOrder = make([]string, 0, len(defaultRigImports))
		for _, bound := range defaultRigImports {
			root.DefaultRigImports[bound.Binding] = bound.Import
			root.DefaultRigImportOrder = append(root.DefaultRigImportOrder, bound.Binding)
		}
	}
	// defaultBindings names the [defaults.rig.imports] bindings declared
	// by city.toml. After expansion, agents whose BindingName
	// matches one of these names are auto-imports (the user did not
	// write the [imports.<name>] entry; gc init auto-added it). See
	// ga-tpfc and the source enum.
	var defaultBindings map[string]bool
	if len(root.Defaults.Rig.Imports) > 0 {
		defaultBindings = make(map[string]bool, len(root.Defaults.Rig.Imports))
		for name := range root.Defaults.Rig.Imports {
			defaultBindings[name] = true
		}
	}

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
	legacyV1SurfaceWarningsEnabled := false
	packData, pErr := fs.ReadFile(packPath)
	if pErr != nil && !os.IsNotExist(pErr) {
		return nil, nil, fmt.Errorf("loading city pack.toml: %w", pErr)
	}
	if pErr == nil {
		packExists = true
		pc, md, packWarnings, decErr := parsePackConfigWithMetadata(packData, packPath)
		if decErr != nil {
			return nil, nil, fmt.Errorf("parsing city pack.toml: %w", decErr)
		}
		if err := validatePackAuthoringSurface(md, packPath); err != nil {
			return nil, nil, fmt.Errorf("parsing city pack.toml: %w", err)
		}
		if fatalWarnings := fatalUndecodedWarnings(md, packPath); len(fatalWarnings) > 0 {
			return nil, nil, fmt.Errorf("parsing city pack.toml: %s", strings.Join(fatalWarnings, "; "))
		}
		prov.Warnings = append(prov.Warnings, packWarnings...)
		if err := validatePackMeta(&pc.Pack); err != nil {
			return nil, nil, fmt.Errorf("city pack.toml: %w", err)
		}
		legacyV1SurfaceWarningsEnabled = pc.Pack.Schema >= 2
		if legacyV1SurfaceWarningsEnabled {
			// Wave 2 hard-stop: schema=2 city packs no longer tolerate PackV1
			// city.toml authoring surfaces. Check the freshly-parsed city.toml
			// before pack merging or fragment processing can inject
			// pack-discovered agents or pack-default rig includes into the same
			// fields.
			if err := LegacyV1SurfaceError(root, path, data); err != nil {
				return nil, nil, err
			}
			prov.Warnings = append(prov.Warnings, legacyWorkspaceIdentitySurfaceWarnings(root, path)...)
			if err := LegacySiteBindingSurfaceError(root, path, data); err != nil {
				return nil, nil, err
			}
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
				// Stamp provenance on the city pack's own [[agent]]
				// blocks; these are v1 inline pack agents, distinct
				// from v2 convention-discovered agents.
				a.source = sourcePack
				a.layout = layoutV1Inline
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
		// Merge pack.toml pricing (pack is base, city wins by (provider, model) key).
		if len(pc.Pricing) > 0 {
			root.PackPricing = mergePricingByKey(root.PackPricing, pc.Pricing)
			root.Pricing = mergePricingByKey(pc.Pricing, root.Pricing)
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
		// Merge pack agent patches (accumulated, applied later).
		root.Patches.Agents = append(pc.Patches.Agents, root.Patches.Agents...)
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
		if legacyV1SurfaceWarningsEnabled {
			if err := LegacyV1SurfaceError(frag, fragPath, fragData); err != nil {
				return nil, nil, &fragmentLegacyV1SurfaceError{include: inc, err: err}
			}
			prov.Warnings = append(prov.Warnings, legacyWorkspaceIdentitySurfaceWarnings(frag, fragPath)...)
			prov.Warnings = append(prov.Warnings, legacyRigPathSurfaceWarnings(frag, fragPath)...)
		}

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

	// Append caller-supplied extra pack includes to Workspace.Includes,
	// after user includes. Skip packs already reachable from user includes
	// or top-level imports (avoids duplicate agent errors when a user pack
	// transitively includes the same pack). Builtin packs are NOT spliced
	// here — they compose only through explicit city.toml includes; this
	// path serves explicit extra includes passed by tests and tools.
	rootIncludes := append([]string{}, rootPackIncludes...)
	rootIncludes = append(rootIncludes, root.Workspace.LegacyIncludes()...)
	root.Workspace.SetLegacyIncludes(rootIncludes)

	// Resolve named pack references before computing reachability so builtin
	// injection sees the same cache paths that pack expansion will use.
	resolveNamedPacks(root, cityRoot)
	rootIncludes = root.Workspace.LegacyIncludes()

	existingPacks := resolvedConfigPackNames(root, fs, cityRoot)
	for _, inc := range packIncludes {
		name := readPackNameFromDir(inc)
		if name != "" && existingPacks[name] {
			continue
		}
		rootIncludes = append(rootIncludes, inc)
		root.Workspace.SetLegacyIncludes(rootIncludes)
	}

	adjustPatchPaths(&root.Patches, cityRoot, cityRoot)
	adjustRigOverridePaths(root.Rigs, cityRoot, cityRoot)

	implicitImports, implicitPath, implicitData, implicitErr := readImplicitImportsWithData()
	if implicitErr != nil {
		return nil, nil, implicitErr
	}
	if len(implicitImports) > 0 {
		// v0.15.1 collision gate: if a user's [imports.<name>] would
		// silently shadow a **bootstrap** implicit-import pack, hard-stop
		// with a diagnostic. Non-bootstrap implicit imports retain the
		// pre-v0.15.1 "explicit wins over implicit" contract and are
		// shadowed silently (see engdocs/design/packv2/doc-packman.md). See
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
	for _, ref := range root.Workspace.LegacyIncludes() {
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

	// Expand rig packs so the merged agent list (city- and rig-scope) is
	// complete before city-level patches are applied. Per-rig patches are
	// deferred so they still run after city-level [[patches.agent]].
	rigFormulaDirs := make(map[string][]string)
	var deferredRigPatches []deferredRigPatches
	if HasPackRigs(root.Rigs) {
		rigPackOpts := opts
		rigPackOpts.deferRigPatches = true
		rigPackOpts.deferredRigPatches = &deferredRigPatches
		if err := expandPacks(root, fs, cityRoot, rigFormulaDirs, rigPackOpts); err != nil {
			return nil, nil, fmt.Errorf("expanding packs: %w", err)
		}
		if len(root.LoadWarnings) > 0 {
			prov.Warnings = appendUnique(prov.Warnings, root.LoadWarnings...)
		}
	}

	// Apply patches after all packs (city and rig) are expanded so that
	// [[patches.agent]] blocks in city.toml can target pack-derived
	// rig-scope agents (e.g., dir="rig" name="gastown.refinery"), not
	// just city-scope agents.
	if !root.Patches.IsEmpty() {
		if err := ApplyPatches(root, root.Patches); err != nil {
			return nil, nil, fmt.Errorf("applying patches: %w", err)
		}
		root.Patches = Patches{} // clear after application
	}
	if err := applyDeferredRigPatches(root, deferredRigPatches); err != nil {
		return nil, nil, fmt.Errorf("applying rig patches: %w", err)
	}
	if HasPackRigs(root.Rigs) {
		// Track pack-expanded agents in provenance after deferred rig patches so
		// Dir overrides are keyed by the agent's final qualified identity.
		trackPackExpandedAgents(prov, root.Agents)
	}

	// Apply [global] sections from packs to agents in scope.
	root.PackGlobals = append(root.PackGlobals, rootPackGlobals...)
	applyPackGlobals(root)

	// Refine source provenance for default-binding imports (ga-tpfc).
	// Discovery stamps every pack-loaded agent as sourcePack; here we
	// promote the subset that came in via pack.toml's
	// [defaults.rig.imports] to sourceAutoImport so describeSource can
	// distinguish them in duplicate-name errors.
	if len(defaultBindings) > 0 {
		for i := range root.Agents {
			a := &root.Agents[i]
			if a.source != sourcePack || a.BindingName == "" {
				continue
			}
			if defaultBindings[a.BindingName] {
				a.source = sourceAutoImport
			}
		}
	}

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
	if root.AgentDefaults.Provider != "" {
		for i := range root.Agents {
			if root.Agents[i].BindingName == "" {
				root.Agents[i].InheritedProvider = ""
			}
		}
	}
	if root.AgentDefaults.DefaultSlingFormula != "" {
		for i := range root.Agents {
			if root.Agents[i].BindingName == "" {
				root.Agents[i].InheritedDefaultSlingFormula = nil
			}
		}
	}
	ApplyAgentDefaults(root)
	ApplyBeadPolicyDefaults(root)

	// Canonicalize duration-or-"off" session sleep fields after all config
	// layers have been applied so runtime consumers can trust the values.
	NormalizeSessionSleepFields(root)

	siteBindingWarnings, err := ApplySiteBindings(fs, cityRoot, root)
	if err != nil {
		return nil, nil, err
	}
	prov.Warnings = append(prov.Warnings, siteBindingWarnings...)

	// Inline scope="rig" named sessions are generic declarations until the
	// city's rig set is finalized. Stamp them after site bindings so the
	// controller sees one concrete identity per rig.
	ExpandGenericRigNamedSessions(root)

	// Validate named session declarations after pack expansion and site
	// binding resolution so stamped identities and deterministic runtime
	// names reflect the effective workspace identity.
	namedSessionWarnings, err := ValidateNamedSessions(root)
	if err != nil {
		return nil, nil, err
	}
	prov.Warnings = append(prov.Warnings, namedSessionWarnings...)

	if err := ValidateGitHubPRMonitors(root); err != nil {
		return nil, nil, err
	}

	// Validate all duration strings in the fully-merged config.
	prov.Warnings = append(prov.Warnings, ValidateDurations(root, path)...)
	prov.Warnings = append(prov.Warnings, ValidateEventsRotation(root)...)
	if err := ValidateBeadPolicyStorageCompatibility(root, path); err != nil {
		return nil, nil, err
	}

	// Reject negative durations that parse cleanly but are silently
	// destructive at runtime (e.g. a negative dolt_stop_timeout collapses
	// the managed-dolt SIGTERM→SIGKILL grace to an immediate kill).
	if err := ValidateNonNegativeDurations(root, path); err != nil {
		return nil, nil, err
	}
	if err := ValidateDoltConfig(root, path); err != nil {
		return nil, nil, err
	}

	// Validate cross-entity semantic constraints.
	if !opts.AllowMissingProviderReferences {
		if err := ValidateProviderReferences(root); err != nil {
			return nil, nil, err
		}
	}
	prov.Warnings = append(prov.Warnings, ValidateSemantics(root, path)...)
	prov.Warnings = append(prov.Warnings, DetectLegacyProviderInheritance(root, path)...)
	prov.Warnings = append(prov.Warnings, detectLegacyWorkspaceFields(root, path, prov.Workspace)...)

	// Build the resolved provider cache now that compose + patch have
	// populated the full provider table. Chain resolution errors
	// (cycles, unknown base, wrapper-resume missing) surface here so
	// they fail at config load rather than at session spawn. If the
	// cache cannot be built, emit a warning and leave the cache nil —
	// callers can still fall back to ResolveProvider per lookup.
	if err := BuildResolvedProviderCache(root); err != nil {
		return nil, nil, fmt.Errorf("%s: provider cache build failed: %w", path, err)
	}

	populateAgentLocalAssetDirs(fs, root, cityRoot)

	// Load namepool files for pool agents.
	loadNamepools(fs, root, cityRoot)

	// v0.15.1: emit a one-time deprecation warning if the loaded config
	// still populates the v0.15.0 attachment-list tombstone fields. The
	// fields still parse (TOML won't error) but are ignored by the new
	// materializer.
	if warning := WarnDeprecatedAttachmentFields(root); warning != "" {
		prov.Warnings = append(prov.Warnings, warning)
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

	// ga-tpfc.1: promote pack-stamped agents to sourceAutoImport when
	// their binding came in via implicit-import expansion (the gastown
	// system pack and similar). describeSource then renders
	// "<auto-import: …>" for these in duplicate-name errors instead of
	// the generic "<pack: …>" or empty-string forms. Agents loaded via
	// loadPackWithCacheOptions arrive with source=sourcePack; we override
	// based on the post-composition ImplicitImportBindings set so the
	// override is computed exactly once over the data the loader already
	// stamped.
	if len(root.ImplicitImportBindings) > 0 {
		for i := range root.Agents {
			a := &root.Agents[i]
			if a.BindingName == "" {
				continue
			}
			if root.ImplicitImportBindings[a.BindingName] {
				a.source = sourceAutoImport
			}
		}
	}

	// Capture revision inputs after all config and pack discovery so callers
	// can compare the loaded snapshot to future reloads without re-reading
	// mutable files from disk.
	prov.captureRevisionSnapshot(fs, root, cityRoot)

	return root, prov, nil
}

// adjustPatchPaths resolves patch path fields rooted at the declaring config
// directory. Patches do not retain independent source provenance after merge,
// so runtime cannot otherwise distinguish whether a relative override came
// from the target agent's source or from the patch file itself.
func adjustPatchPaths(patches *Patches, declDir, cityRoot string) {
	for i := range patches.Agents {
		p := &patches.Agents[i]
		if p.SessionSetupScript != nil && *p.SessionSetupScript != "" {
			v := resolveConfigPath(*p.SessionSetupScript, declDir, cityRoot)
			p.SessionSetupScript = &v
		}
		if p.PromptTemplate != nil && *p.PromptTemplate != "" {
			v := adjustFragmentPath(*p.PromptTemplate, declDir, cityRoot)
			p.PromptTemplate = &v
		}
		if p.OverlayDir != nil && *p.OverlayDir != "" {
			v := adjustFragmentPath(*p.OverlayDir, declDir, cityRoot)
			p.OverlayDir = &v
		}
	}
}

// adjustRigOverridePaths resolves rig override path fields to stable forms
// rooted at the declaring config directory. Once overrides are applied to
// pack-stamped agents, runtime only sees the target agent's SourceDir, so
// relative override paths must be normalized during composition.
func adjustRigOverridePaths(rigs []Rig, declDir, cityRoot string) {
	for i := range rigs {
		adjustAgentOverridePaths(rigs[i].Overrides, declDir, cityRoot)
		adjustAgentOverridePaths(rigs[i].RigPatches, declDir, cityRoot)
	}
}

func adjustAgentOverridePaths(overrides []AgentOverride, declDir, cityRoot string) {
	for i := range overrides {
		ov := &overrides[i]
		if ov.SessionSetupScript != nil && *ov.SessionSetupScript != "" {
			v := resolveConfigPath(*ov.SessionSetupScript, declDir, cityRoot)
			ov.SessionSetupScript = &v
		}
		if ov.PromptTemplate != nil && *ov.PromptTemplate != "" {
			v := adjustFragmentPath(*ov.PromptTemplate, declDir, cityRoot)
			ov.PromptTemplate = &v
		}
		if ov.OverlayDir != nil && *ov.OverlayDir != "" {
			v := adjustFragmentPath(*ov.OverlayDir, declDir, cityRoot)
			ov.OverlayDir = &v
		}
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

	// GitHub PR monitors: concatenate. Validation rejects duplicate repo/base
	// ownership after patches have had a chance to adjust declarations.
	base.GitHub.PRMonitors = append(base.GitHub.PRMonitors, fragment.GitHub.PRMonitors...)

	// Providers: deep-merge per-field.
	mergeProviders(base, fragment, fragMeta, fragPath, prov)

	// Workspace: per-field merge.
	mergeWorkspace(base, fragment, fragMeta, fragPath, prov)

	// Packs: additive merge.
	mergePacks(base, fragment, fragPath, prov)

	// Pricing: city fragments are city-layer overrides.
	if len(fragment.Pricing) > 0 {
		base.CityPricing = mergePricingByKey(base.CityPricing, fragment.Pricing)
		base.Pricing = mergePricingByKey(base.Pricing, fragment.Pricing)
	}

	// Patches: accumulate from fragments (applied after all merges).
	base.Patches.Agents = append(base.Patches.Agents, fragment.Patches.Agents...)
	base.Patches.NamedSessions = append(base.Patches.NamedSessions, fragment.Patches.NamedSessions...)
	base.Patches.Rigs = append(base.Patches.Rigs, fragment.Patches.Rigs...)
	base.Patches.Providers = append(base.Patches.Providers, fragment.Patches.Providers...)
	base.Patches.GitHubPRMonitors = append(base.Patches.GitHubPRMonitors, fragment.Patches.GitHubPRMonitors...)

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
		formulaV2 := base.Daemon.FormulaV2
		formulaV2Set := base.Daemon.formulaV2Set
		base.Daemon = fragment.Daemon
		if !fragMeta.IsDefined("daemon", "formula_v2") && !fragMeta.IsDefined("daemon", "graph_workflows") {
			base.Daemon.FormulaV2 = formulaV2
			base.Daemon.formulaV2Set = formulaV2Set
		}
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
	if fragMeta.IsDefined("maintenance") {
		base.Maintenance = fragment.Maintenance
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
		{
			"accept_startup_dialogs",
			func() bool { return base.AcceptStartupDialogs != nil },
			func() { result.AcceptStartupDialogs = cloneBoolPtr(frag.AcceptStartupDialogs) },
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
	if fragMeta.IsDefined("workspace", "suspended") {
		if base.Workspace.Suspended {
			prov.Warnings = append(prov.Warnings,
				fmt.Sprintf("workspace.suspended redefined by %q", fragPath))
		}
		base.Workspace.Suspended = fragment.Workspace.Suspended
		prov.Workspace["suspended"] = fragPath
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
		base.Workspace.SetLegacyIncludes(append(
			base.Workspace.LegacyIncludes(), fragment.Workspace.LegacyIncludes()...))
		prov.Workspace["includes"] = fragPath
	}
	// default_rig_includes is a []string — additive merge (append, not replace).
	if fragMeta.IsDefined("workspace", "default_rig_includes") {
		base.Workspace.SetLegacyDefaultRigIncludes(append(
			base.Workspace.LegacyDefaultRigIncludes(), fragment.Workspace.LegacyDefaultRigIncludes()...))
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
	if err := validateCityAuthoringSurface(md); err != nil {
		return nil, md, nil, fmt.Errorf("parsing config: %w", err)
	}
	normalizeAgentDefaultsAlias(&cfg, md)
	applyDaemonFormulaV2Default(&cfg, md)
	warnings := agentDefaultsCompatibilityWarnings(md, source)
	normalizeLegacyOrderOverrideAliases(&cfg)
	warnings = append(warnings, CheckUndecodedKeys(md, source)...)
	// Stamp source=sourceInline on inline [[agent]] tables. For fragments,
	// adjustAgentPaths later sets SourceDir, which takes precedence in
	// describeSource (FR-1). For the root city.toml, SourceDir is empty
	// and the inline stamp drives the fallback descriptor (FR-3).
	for i := range cfg.Agents {
		cfg.Agents[i].source = sourceInline
	}
	return &cfg, md, warnings, nil
}

// LoadRootPackDefaultRigImports loads the canonical city.toml
// [defaults.rig.imports] entries without expanding the full config.
//
// Deprecated name retained for callers in the #2126 wave; the default-rig
// import table is no longer a root pack.toml surface.
func LoadRootPackDefaultRigImports(fs fsys.FS, cityRoot string) ([]BoundImport, error) {
	cityPath := filepath.Join(cityRoot, "city.toml")
	data, err := fs.ReadFile(cityPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading city.toml: %w", err)
	}
	var cfg City
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing city.toml: %w", err)
	}
	if err := validateCityAuthoringSurface(md); err != nil {
		return nil, fmt.Errorf("parsing city.toml: %w", err)
	}
	imports, err := defaultRigImportsFromPackDefaults(cfg.Defaults, md)
	if err != nil || len(imports) > 0 {
		return imports, err
	}

	// Compatibility for CLI migration helpers that need to read older root
	// pack defaults without performing a full config load. Normal PackV2
	// loading rejects this surface via validatePackAuthoringSurface.
	packPath := filepath.Join(cityRoot, packFile)
	packData, err := fs.ReadFile(packPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading city pack.toml: %w", err)
	}
	var pc PackConfig
	packMD, err := toml.Decode(string(packData), &pc)
	if err != nil {
		return nil, fmt.Errorf("parsing city pack.toml: %w", err)
	}
	return defaultRigImportsFromPackDefaults(pc.Defaults, packMD)
}

// LoadPackGraphDirsForDoctor returns the pack directories the canonical city
// loader would evaluate while allowing legacy PackV1 order layouts to remain
// on disk for doctor diagnostics and migration.
func LoadPackGraphDirsForDoctor(fs fsys.FS, cityTomlPath string) ([]string, error) {
	cfg, _, err := LoadWithIncludesOptions(fs, cityTomlPath, LoadOptions{allowLegacyOrderLayouts: true})
	if err != nil {
		return nil, err
	}

	var dirs []string
	dirs = appendUnique(dirs, cfg.PackDirs...)

	rigNames := make([]string, 0, len(cfg.RigPackDirs))
	for name := range cfg.RigPackDirs {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)
	for _, name := range rigNames {
		dirs = appendUnique(dirs, cfg.RigPackDirs[name]...)
	}

	defaultNames := make([]string, 0, len(cfg.DefaultRigImports))
	seenDefaults := make(map[string]bool, len(cfg.DefaultRigImports))
	for _, name := range cfg.DefaultRigImportOrder {
		if _, ok := cfg.DefaultRigImports[name]; !ok || seenDefaults[name] {
			continue
		}
		seenDefaults[name] = true
		defaultNames = append(defaultNames, name)
	}
	var missingDefaults []string
	for name := range cfg.DefaultRigImports {
		if !seenDefaults[name] {
			missingDefaults = append(missingDefaults, name)
		}
	}
	sort.Strings(missingDefaults)
	defaultNames = append(defaultNames, missingDefaults...)
	if len(defaultNames) == 0 {
		return dirs, nil
	}

	cityRoot := filepath.Dir(cityTomlPath)
	cache := &packLoadCache{results: make(map[string]*packLoadResult)}
	for _, name := range defaultNames {
		imp := cfg.DefaultRigImports[name]
		topoDirs, err := loadImportPackGraphDirsForDoctor(fs, imp, cityRoot, cityRoot, cache)
		if err != nil {
			return nil, fmt.Errorf("default rig import %q: %w", name, err)
		}
		dirs = appendUnique(dirs, topoDirs...)
	}
	return dirs, nil
}

func loadImportPackGraphDirsForDoctor(fs fsys.FS, imp Import, declDir, cityRoot string, cache *packLoadCache) ([]string, error) {
	impDir, err := resolveImportPackRef(imp.Source, declDir, cityRoot)
	if err != nil {
		return nil, err
	}
	topoPath := filepath.Join(impDir, packFile)
	_, _, _, _, topoDirs, _, _, err := loadPackWithCacheOptions(
		fs, topoPath, impDir, cityRoot, "", nil, cache, LoadOptions{allowLegacyOrderLayouts: true})
	if err != nil {
		return nil, err
	}
	if !imp.ImportIsTransitive() {
		return cachedPackLocalTopoDirs(cache, impDir), nil
	}
	return topoDirs, nil
}

func defaultRigImportsFromPackDefaults(defaults PackDefaults, md toml.MetaData) ([]BoundImport, error) {
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

func trackPackExpandedAgents(prov *Provenance, agents []Agent) {
	for _, a := range agents {
		if a.SourceDir == "" || (a.source != sourcePack && a.source != sourceAutoImport) {
			continue
		}
		prov.Agents[a.QualifiedName()] = filepath.Join(a.SourceDir, packFile)
	}
}

func trackRigs(prov *Provenance, rigs []Rig, source string) {
	for _, r := range rigs {
		prov.Rigs[r.Name] = source
	}
}

func trackWorkspace(prov *Provenance, meta toml.MetaData, source string) {
	for _, f := range []string{"name", "provider", "start_command", "session_template", "suspended", "install_agent_hooks", "global_fragments"} {
		if meta.IsDefined("workspace", f) {
			prov.Workspace[f] = source
		}
	}
}

// resolvedPackNames collects pack names that are reachable from a set of
// top-level include paths and imports. It walks legacy [pack].includes and V2
// [imports] according to each import's transitive setting so builtin system-pack
// injection can be skipped when a user pack already brings the same pack into
// the city closure.
func resolvedPackNames(includes []string, imports map[string]Import, sysFS fsys.FS, cityRoot string) map[string]bool {
	names := make(map[string]bool, len(includes)+len(imports))
	seenShallowDirs := make(map[string]bool)
	expandedDirs := make(map[string]bool)

	var visitDir func(dir string, transitive bool)
	var visitInclude func(ref, declDir string, transitive bool)
	var visitImport func(ref, declDir string, transitive bool)

	visitDir = func(dir string, transitive bool) {
		absDir, absErr := filepath.Abs(dir)
		if absErr != nil {
			absDir = dir
		}
		if transitive {
			if expandedDirs[absDir] {
				return
			}
		} else {
			// Shallow visits record the pack name but must not block a later
			// transitive visit from expanding the pack's children.
			if seenShallowDirs[absDir] || expandedDirs[absDir] {
				return
			}
		}

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
		if !transitive {
			seenShallowDirs[absDir] = true
			return
		}
		expandedDirs[absDir] = true
		for _, sub := range pc.Pack.Includes {
			visitInclude(sub, dir, true)
		}
		for _, imp := range pc.Imports {
			visitImport(imp.Source, dir, imp.ImportIsTransitive())
		}
	}

	visitInclude = func(ref, declDir string, transitive bool) {
		dir, err := resolvePackRef(ref, declDir, cityRoot)
		if err != nil {
			return
		}
		visitDir(dir, transitive)
	}

	visitImport = func(ref, declDir string, transitive bool) {
		dir, err := resolveImportPackRef(ref, declDir, cityRoot)
		if err != nil {
			return
		}
		visitDir(dir, transitive)
	}

	for _, inc := range includes {
		visitInclude(inc, cityRoot, true)
	}
	for _, imp := range imports {
		visitImport(imp.Source, cityRoot, imp.ImportIsTransitive())
	}
	return names
}

// ReachablePackNames reports every pack name reachable from the config's
// explicit includes, imports, rig pack graphs, and default-rig pack graphs.
// The gc binary uses it after composition to verify that required builtin
// packs (core, and bd for bd-provider cities) are explicitly included.
func ReachablePackNames(cfg *City, sysFS fsys.FS, cityRoot string) map[string]bool {
	return resolvedConfigPackNames(cfg, sysFS, cityRoot)
}

// resolvedConfigPackNames collects all pack names reachable from the city,
// rig, and default-rig pack graphs before builtin extra includes are injected.
func resolvedConfigPackNames(cfg *City, sysFS fsys.FS, cityRoot string) map[string]bool {
	names := resolvedPackNames(cfg.Workspace.LegacyIncludes(), cfg.Imports, sysFS, cityRoot)
	add := func(more map[string]bool) {
		for name := range more {
			names[name] = true
		}
	}

	for _, rig := range cfg.Rigs {
		add(resolvedPackNames(rig.Includes, rig.Imports, sysFS, cityRoot))
	}

	add(resolvedPackNames(cfg.Workspace.LegacyDefaultRigIncludes(), nil, sysFS, cityRoot))
	add(resolvedPackNames(nil, cfg.DefaultRigImports, sysFS, cityRoot))

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
