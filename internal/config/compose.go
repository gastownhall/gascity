package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gascity/internal/fsys"
)

// Provenance tracks where each configuration element originated during
// composition. Built into the merge API from the start — retrofitting
// provenance later is expensive.
type Provenance struct {
	// Root is the path to the root city.toml.
	Root string
	// Sources lists all source files in load order (root first).
	Sources []string
	// Agents maps agent QualifiedName → source file path.
	Agents map[string]string
	// Rigs maps rig name → source file path.
	Rigs map[string]string
	// Workspace maps workspace field name → source file path.
	Workspace map[string]string
	// Warnings collects non-fatal collision warnings from composition.
	Warnings []string
}

// LoadWithIncludes loads a city.toml and merges all included fragments.
// Includes are NOT recursive — fragments cannot include other fragments.
// Extra includes (from CLI -f flags) are appended after the root's
// include list and processed identically.
// Returns the fully-merged config, provenance tracking, and any error.
func LoadWithIncludes(fs fsys.FS, path string, extraIncludes ...string) (*City, *Provenance, error) {
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
	prov.Warnings = append(prov.Warnings, rootWarnings...)

	// Track root's resources.
	trackAgents(prov, root.Agents, path)
	trackRigs(prov, root.Rigs, path)
	trackWorkspace(prov, rootMeta, path)

	// Extract includes for processing. CLI -f files are appended after.
	// Preserve the original Include value so Marshal() round-trips it.
	origInclude := root.Include
	includes := append([]string{}, root.Include...)
	includes = append(includes, extraIncludes...)
	root.Include = origInclude

	for _, inc := range includes {
		var fragPath string
		if isRemoteInclude(inc) || isGitHubTreeURL(inc) {
			resolved, err := resolveTopologyRef(inc, cityRoot, cityRoot)
			if err != nil {
				return nil, nil, fmt.Errorf("fetching include %q: %w", inc, err)
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

		// Merge fragment into root.
		mergeFragment(root, frag, fragMeta, fragPath, prov)
		prov.Sources = append(prov.Sources, fragPath)
	}

	// Resolve named topology references to cache paths before any expansion.
	resolveNamedTopologies(root, cityRoot)

	// Expand city topologies before patches (so patches can target city-topo agents).
	cityTopoFormulas, cityReqs, ctErr := ExpandCityTopologies(root, fs, cityRoot)
	if ctErr != nil {
		return nil, nil, ctErr
	}
	// Track city topology agents in provenance.
	for _, ref := range EffectiveCityTopologies(root.Workspace) {
		topoDir, _ := resolveTopologyRef(ref, cityRoot, cityRoot)
		topoPath := filepath.Join(topoDir, topologyFile)
		for _, a := range root.Agents {
			if a.Dir == "" {
				if _, tracked := prov.Agents[a.QualifiedName()]; !tracked {
					prov.Agents[a.QualifiedName()] = topoPath
				}
			}
		}
	}

	// Apply patches after all fragments are merged + city topologies expanded.
	if !root.Patches.IsEmpty() {
		if err := ApplyPatches(root, root.Patches); err != nil {
			return nil, nil, fmt.Errorf("applying patches: %w", err)
		}
		root.Patches = Patches{} // clear after application
	}

	// Expand rig topologies after patches (topology agents get rig overrides).
	rigFormulaDirs := make(map[string][]string)
	if HasTopologyRigs(root.Rigs) {
		if err := ExpandTopologies(root, fs, cityRoot, rigFormulaDirs); err != nil {
			return nil, nil, fmt.Errorf("expanding topologies: %w", err)
		}
		// Track topology-expanded agents in provenance.
		for _, r := range root.Rigs {
			topoRefs := EffectiveRigTopologies(r)
			for _, ref := range topoRefs {
				topoDir, _ := resolveTopologyRef(ref, cityRoot, cityRoot)
				topoPath := filepath.Join(topoDir, topologyFile)
				for _, a := range root.Agents {
					if a.Dir == r.Name {
						prov.Agents[a.QualifiedName()] = topoPath
					}
				}
			}
		}
	}

	// Validate city-scoped topology requirements.
	if err := validateCityRequirements(cityReqs, root.Agents); err != nil {
		return nil, nil, err
	}

	// Compute formula layers from all sources.
	cityLocalFormulas := ""
	if root.Formulas.Dir != "" {
		cityLocalFormulas = resolveConfigPath(root.FormulasDir(), cityRoot, cityRoot)
	}
	root.FormulaLayers = ComputeFormulaLayers(
		cityTopoFormulas, cityLocalFormulas, rigFormulaDirs, root.Rigs, cityRoot)

	// Validate all duration strings in the fully-merged config.
	prov.Warnings = append(prov.Warnings, ValidateDurations(root, path)...)

	// Validate cross-entity semantic constraints.
	prov.Warnings = append(prov.Warnings, ValidateSemantics(root, path)...)

	return root, prov, nil
}

// validateCityRequirements checks that all city-scoped topology requirements
// are satisfied by the expanded agent list.
func validateCityRequirements(reqs []TopologyRequirement, agents []Agent) error {
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
			return fmt.Errorf("topology requires city agent %q — include a topology that provides it", req.Agent)
		}
	}
	return nil
}

// mergeFragment merges a fragment into the base config in-place.
// Arrays concatenate, providers deep-merge, workspace per-field merges.
func mergeFragment(base, fragment *City, fragMeta toml.MetaData, fragPath string, prov *Provenance) {
	// Agents: concatenate.
	trackAgents(prov, fragment.Agents, fragPath)
	base.Agents = append(base.Agents, fragment.Agents...)

	// Rigs: concatenate.
	trackRigs(prov, fragment.Rigs, fragPath)
	base.Rigs = append(base.Rigs, fragment.Rigs...)

	// Providers: deep-merge per-field.
	mergeProviders(base, fragment, fragMeta, fragPath, prov)

	// Workspace: per-field merge.
	mergeWorkspace(base, fragment, fragMeta, fragPath, prov)

	// Topologies: additive merge.
	mergeTopologies(base, fragment, fragPath, prov)

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
}

// mergeTopologies additively merges fragment topologies into base.
// New topology names are added. Duplicate names generate a warning.
func mergeTopologies(base, fragment *City, fragPath string, prov *Provenance) {
	if len(fragment.Topologies) == 0 {
		return
	}
	if base.Topologies == nil {
		base.Topologies = make(map[string]TopologySource)
	}
	for name, src := range fragment.Topologies {
		if _, exists := base.Topologies[name]; exists {
			prov.Warnings = append(prov.Warnings,
				fmt.Sprintf("topology %q redefined by %q", name, fragPath))
		}
		base.Topologies[name] = src
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
			func() bool { return base.EmitsPermissionWarning },
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
	if fragMeta.IsDefined("providers", name, "env") {
		if result.Env == nil {
			result.Env = make(map[string]string)
		}
		for k, v := range frag.Env {
			if _, exists := base.Env[k]; exists {
				prov.Warnings = append(prov.Warnings,
					fmt.Sprintf("provider %q.env.%s redefined by %q", name, k, fragPath))
			}
			result.Env[k] = v
		}
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
	// topologies is a []string — additive merge (append, not replace).
	if fragMeta.IsDefined("workspace", "topologies") {
		base.Workspace.CityTopologies = append(
			base.Workspace.CityTopologies, fragment.Workspace.CityTopologies...)
		prov.Workspace["topologies"] = fragPath
	}
	// includes is a []string — additive merge (append, not replace).
	if fragMeta.IsDefined("workspace", "includes") {
		base.Workspace.Includes = append(
			base.Workspace.Includes, fragment.Workspace.Includes...)
		prov.Workspace["includes"] = fragPath
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

// adjustAgentPaths converts relative prompt_template and session_setup_script
// paths in fragment agents to be city-root-relative, based on the fragment's
// directory. Also sets SourceDir so session_setup templates can reference
// scripts relative to their source directory.
func adjustAgentPaths(agents []Agent, fragDir, cityRoot string) {
	for i := range agents {
		agents[i].SourceDir = fragDir
		if agents[i].PromptTemplate != "" {
			agents[i].PromptTemplate = adjustFragmentPath(
				agents[i].PromptTemplate, fragDir, cityRoot)
		}
		if agents[i].SessionSetupScript != "" {
			agents[i].SessionSetupScript = adjustFragmentPath(
				agents[i].SessionSetupScript, fragDir, cityRoot)
		}
		if agents[i].OverlayDir != "" {
			agents[i].OverlayDir = adjustFragmentPath(
				agents[i].OverlayDir, fragDir, cityRoot)
		}
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
	warnings := CheckUndecodedKeys(md, source)
	return &cfg, md, warnings, nil
}

func newProvenance(rootPath string) *Provenance {
	return &Provenance{
		Root:      rootPath,
		Sources:   []string{rootPath},
		Agents:    make(map[string]string),
		Rigs:      make(map[string]string),
		Workspace: make(map[string]string),
	}
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
