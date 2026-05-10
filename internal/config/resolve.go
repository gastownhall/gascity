package config

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for provider resolution.
var (
	// ErrProviderNotFound indicates the provider name is not known.
	ErrProviderNotFound = errors.New("unknown provider")
	// ErrProviderNotInPATH indicates the provider binary is not in PATH.
	ErrProviderNotInPATH = errors.New("provider not found in PATH")
	// ErrUnknownOption indicates an option key not in the schema.
	ErrUnknownOption = errors.New("unknown option")
)

// LookPathFunc is the signature for exec.LookPath (or a test fake).
type LookPathFunc func(string) (string, error)

// ResolveProvider determines the fully-resolved provider for an agent.
//
// Resolution chain:
//  1. agent.StartCommand set? Escape hatch → ResolvedProvider{Command: startCommand}
//  2. Determine provider name: agent.Provider > workspace.Provider > auto-detect
//     (workspace.StartCommand is escape hatch if no provider name found)
//  3. Look up ProviderSpec: cityProviders[name] > BuiltinProviders()[name]
//     (verify binary exists in PATH via lookPath)
//  4. Merge agent-level overrides: non-zero agent fields replace base spec fields
//     (env merges additively — agent env adds to/overrides base env)
//     4b. workspace.StartCommand overrides command (preserves provider settings,
//     clears Args/OptionsSchema/EffectiveDefaults)
//  5. Default prompt_mode to "arg" if still empty
func ResolveProvider(agent *Agent, ws *Workspace, cityProviders map[string]ProviderSpec, lookPath LookPathFunc) (*ResolvedProvider, error) {
	// Step 1: agent.StartCommand is the escape hatch.
	if agent.StartCommand != "" {
		mode := strings.TrimSpace(agent.PromptMode)
		if mode == "" {
			mode = "none"
		}
		return &ResolvedProvider{Command: agent.StartCommand, PromptMode: mode, PromptFlag: agent.PromptFlag}, nil
	}

	// Step 2: determine provider name.
	name := agent.Provider
	if name == "" && ws != nil {
		name = ws.Provider
	}
	if name == "" {
		// No provider name — check workspace start_command escape hatch.
		if ws != nil && ws.StartCommand != "" {
			return &ResolvedProvider{Command: ws.StartCommand, PromptMode: "none"}, nil
		}
		// Auto-detect: scan PATH for known binaries.
		detected, err := detectProviderName(lookPath)
		if err != nil {
			return nil, err
		}
		name = detected
	}

	// Step 3: look up the ProviderSpec.
	spec, err := lookupProvider(name, cityProviders, lookPath)
	if err != nil {
		return nil, err
	}

	// Step 4: merge agent-level overrides.
	resolved := specToResolved(name, spec)
	resolved.Kind = resolveProviderKind(name, cityProviders)
	// BuiltinAncestor is the chain-derived family name (e.g. "claude"
	// for a custom provider with base = "builtin:claude"). Runtime sites
	// that branch on provider family should consume this field instead
	// of the raw Name. See engdocs/design/provider-inheritance.md
	// §Kind / provider-family propagation.
	resolved.BuiltinAncestor = BuiltinFamily(name, cityProviders)
	mergeAgentOverrides(resolved, agent)
	if agent.ResumeCommand == "" {
		completeResolvedProviderResumeCommand(resolved)
	}

	// Step 4b: workspace.start_command overrides the resolved command when
	// the agent doesn't set its own. Unlike the escape hatch at step 2
	// (which returns a bare provider for the no-provider case), this path
	// preserves all provider settings (PromptMode, ProcessNames, etc.)
	// while replacing the command. Args, OptionsSchema, and
	// EffectiveDefaults are cleared because start_command is the complete
	// command line — appending schema-derived flags would conflict with
	// the user's explicit command.
	if agent.StartCommand == "" && ws != nil && ws.StartCommand != "" {
		resolved.Command = ws.StartCommand
		resolved.Args = nil
		resolved.OptionsSchema = nil
		resolved.EffectiveDefaults = nil
	}

	// Step 5: default prompt_mode.
	if resolved.PromptMode == "" {
		resolved.PromptMode = "arg"
	}

	return resolved, nil
}

// ResolveInstallHooks returns the hook providers to install for an agent.
// Agent-level overrides workspace-level (replace, not additive).
// Returns nil if neither specifies hooks.
func ResolveInstallHooks(agent *Agent, ws *Workspace) []string {
	if len(agent.InstallAgentHooks) > 0 {
		return agent.InstallAgentHooks
	}
	if ws != nil {
		return ws.InstallAgentHooks
	}
	return nil
}

// lookupProvider finds a ProviderSpec by name, checking city-level providers
// first, then built-in presets. Verifies the binary exists in PATH.
//
// When a city-level provider's Command matches a built-in provider name,
// the built-in is used as a base and city-level fields override it. This
// lets custom provider tiers (e.g. [providers.fast] command = "copilot")
// inherit PromptMode, PromptFlag, ReadyPromptPrefix, etc.
func lookupProvider(name string, cityProviders map[string]ProviderSpec, lookPath LookPathFunc) (*ProviderSpec, error) {
	// City-level providers take precedence.
	if cityProviders != nil {
		if spec, ok := cityProviders[name]; ok {
			if spec.Command != "" {
				if _, err := lookPath(spec.pathCheckBinary()); err != nil {
					return nil, fmt.Errorf("%w: provider %q command %q", ErrProviderNotInPATH, name, spec.pathCheckBinary())
				}
			}
			// Phase 2+: if the spec has explicit Base declared,
			// resolve via the chain walker so inherited fields propagate.
			// Wrapper providers (aimux-wrapped codex) rely on this path to
			// pick up PermissionModes / OptionsSchema / ReadyDelayMs from
			// the built-in ancestor. base = "" is an explicit standalone
			// opt-out and must not fall through to legacy auto-inheritance.
			if spec.Base != nil {
				if strings.TrimSpace(*spec.Base) == "" {
					standalone := normalizeProviderLayerArgsForSchema(spec, spec.OptionsSchema)
					return &standalone, nil
				}
				resolved, err := resolveProviderChain(name, spec, cityProviders, false)
				if err != nil {
					return nil, err
				}
				merged := resolvedChainToSpec(resolved, spec)
				return &merged, nil
			}
			// Phase A legacy: layer city overrides on top of the built-in
			// if the provider name or command matches a known builtin.
			builtins := BuiltinProviders()
			if base, ok := builtins[name]; ok {
				base = normalizeProviderLayerArgsForSchema(base, base.OptionsSchema)
				child := normalizeProviderLayerArgsForSchema(spec, providerSchemaForLayerArgs(base, spec))
				merged := MergeProviderOverBuiltin(base, child)
				return &merged, nil
			}
			if base, ok := builtins[spec.Command]; ok {
				base = normalizeProviderLayerArgsForSchema(base, base.OptionsSchema)
				child := normalizeProviderLayerArgsForSchema(spec, providerSchemaForLayerArgs(base, spec))
				merged := MergeProviderOverBuiltin(base, child)
				return &merged, nil
			}
			standalone := normalizeProviderLayerArgsForSchema(spec, spec.OptionsSchema)
			return &standalone, nil
		}
	}

	// Fall back to built-in presets.
	builtins := BuiltinProviders()
	if spec, ok := builtins[name]; ok {
		if _, err := lookPath(spec.pathCheckBinary()); err != nil {
			return nil, fmt.Errorf("%w: provider %q command %q", ErrProviderNotInPATH, name, spec.pathCheckBinary())
		}
		return &spec, nil
	}

	return nil, fmt.Errorf("%w: %q", ErrProviderNotFound, name)
}

// MergeProviderOverBuiltin layers city-level provider fields over a built-in
// base. Non-zero city fields override; zero-value fields inherit the built-in
// defaults. Slice fields (Args, ProcessNames, OptionsSchema) replace entirely
// when non-nil. Map fields (Env, PermissionModes) merge additively (city keys
// override base keys).
//
// Capability bools (EmitsPermissionWarning, SupportsACP, SupportsHooks)
// are tri-state *bool: nil = inherit base, &true = enable, &false =
// explicit disable. A child that sets `supports_hooks = false` now
// suppresses the feature even when inherited from a built-in with &true.
func MergeProviderOverBuiltin(base, city ProviderSpec) ProviderSpec {
	result := base

	// Inheritance control fields: presence-aware for Base.
	if city.Base != nil {
		// City explicitly declared base (may be "" for opt-out, or a
		// named value). Copy the pointer so the presence is preserved
		// through the merge; we do not deep-copy the underlying string.
		b := *city.Base
		result.Base = &b
	}
	if city.OptionsSchemaMerge != "" {
		result.OptionsSchemaMerge = city.OptionsSchemaMerge
	}

	// Scalar fields: override if city defines them.
	if city.DisplayName != "" {
		result.DisplayName = city.DisplayName
	}
	if city.Command != "" {
		result.Command = city.Command
	}
	if city.PromptMode != "" {
		result.PromptMode = city.PromptMode
	}
	if city.PromptFlag != "" {
		result.PromptFlag = city.PromptFlag
	}
	if city.ReadyDelayMs != 0 {
		result.ReadyDelayMs = city.ReadyDelayMs
	}
	if city.ReadyPromptPrefix != "" {
		result.ReadyPromptPrefix = city.ReadyPromptPrefix
	}
	// Tri-state capability bools: city pointer wins when non-nil,
	// otherwise base is preserved (including base's own &false).
	if city.EmitsPermissionWarning != nil {
		result.EmitsPermissionWarning = city.EmitsPermissionWarning
	}
	if city.PathCheck != "" {
		result.PathCheck = city.PathCheck
	}
	if city.SupportsACP != nil {
		result.SupportsACP = city.SupportsACP
	}
	if city.SupportsHooks != nil {
		result.SupportsHooks = city.SupportsHooks
	}
	if city.InstructionsFile != "" {
		result.InstructionsFile = city.InstructionsFile
	}
	if city.ResumeFlag != "" {
		result.ResumeFlag = city.ResumeFlag
	}
	if city.ResumeStyle != "" {
		result.ResumeStyle = city.ResumeStyle
	}
	if city.ResumeCommand != "" {
		result.ResumeCommand = city.ResumeCommand
	}
	if city.SessionIDFlag != "" {
		result.SessionIDFlag = city.SessionIDFlag
	}

	if city.TitleModel != "" {
		result.TitleModel = city.TitleModel
	}
	if city.ACPCommand != "" {
		result.ACPCommand = city.ACPCommand
	}

	// Slice fields: replace entirely when non-nil.
	if city.Args != nil {
		result.Args = city.Args
	}
	if city.ArgsAppend != nil {
		result.ArgsAppend = append(append([]string(nil), base.ArgsAppend...), city.ArgsAppend...)
		result.Args = append(append([]string(nil), result.Args...), city.ArgsAppend...)
	}
	if city.ProcessNames != nil {
		result.ProcessNames = city.ProcessNames
	}
	pruneOptionDefaults := map[string]bool{}
	if city.OptionsSchema != nil {
		if city.OptionsSchemaMerge == "by_key" {
			result.OptionsSchema, pruneOptionDefaults = mergeOptionsSchemaByKey(base.OptionsSchema, city.OptionsSchema)
		} else {
			result.OptionsSchema = city.OptionsSchema
			pruneOptionDefaults = optionKeysRemovedByReplacement(base.OptionsSchema, city.OptionsSchema)
		}
	}
	if city.PrintArgs != nil {
		result.PrintArgs = city.PrintArgs
	}
	if city.ACPArgs != nil {
		result.ACPArgs = city.ACPArgs
	}

	// Map fields: merge additively (city keys win).
	if city.PermissionModes != nil {
		merged := make(map[string]string, len(base.PermissionModes)+len(city.PermissionModes))
		for k, v := range base.PermissionModes {
			merged[k] = v
		}
		for k, v := range city.PermissionModes {
			merged[k] = v
		}
		result.PermissionModes = merged
	}
	if city.Env != nil {
		merged := make(map[string]string, len(base.Env)+len(city.Env))
		for k, v := range base.Env {
			merged[k] = v
		}
		for k, v := range city.Env {
			merged[k] = v
		}
		result.Env = merged
	}

	// OptionDefaults: merge additively (city keys win), same as Env and PermissionModes.
	if city.OptionDefaults != nil {
		merged := make(map[string]string, len(base.OptionDefaults)+len(city.OptionDefaults))
		for k, v := range base.OptionDefaults {
			merged[k] = v
		}
		for k, v := range city.OptionDefaults {
			merged[k] = v
		}
		result.OptionDefaults = merged
	}
	if len(pruneOptionDefaults) > 0 && result.OptionDefaults != nil {
		merged := make(map[string]string, len(result.OptionDefaults))
		for k, v := range result.OptionDefaults {
			if !pruneOptionDefaults[k] {
				merged[k] = v
			}
		}
		result.OptionDefaults = merged
	}

	return result
}

func mergeOptionsSchemaByKey(base, city []ProviderOption) ([]ProviderOption, map[string]bool) {
	out := make([]ProviderOption, 0, len(base)+len(city))
	index := make(map[string]int, len(base)+len(city))
	pruned := make(map[string]bool)
	for _, opt := range base {
		if opt.Key == "" {
			out = append(out, opt)
			continue
		}
		index[opt.Key] = len(out)
		out = append(out, opt)
	}
	for _, opt := range city {
		if opt.Omit {
			if idx, ok := index[opt.Key]; ok {
				out = append(out[:idx], out[idx+1:]...)
				delete(index, opt.Key)
				for k, v := range index {
					if v > idx {
						index[k] = v - 1
					}
				}
			}
			if opt.Key != "" {
				pruned[opt.Key] = true
			}
			continue
		}
		if idx, ok := index[opt.Key]; ok && opt.Key != "" {
			out[idx] = opt
			continue
		}
		if opt.Key != "" {
			index[opt.Key] = len(out)
		}
		out = append(out, opt)
	}
	return out, pruned
}

func optionKeysRemovedByReplacement(base, replacement []ProviderOption) map[string]bool {
	if len(base) == 0 {
		return nil
	}
	kept := make(map[string]bool, len(replacement))
	for _, opt := range replacement {
		if opt.Key != "" {
			kept[opt.Key] = true
		}
	}
	removed := make(map[string]bool)
	for _, opt := range base {
		if opt.Key != "" && !kept[opt.Key] {
			removed[opt.Key] = true
		}
	}
	return removed
}

func providerSchemaForLayerArgs(parent, child ProviderSpec) []ProviderOption {
	if child.OptionsSchema == nil {
		return parent.OptionsSchema
	}
	if child.OptionsSchemaMerge == "by_key" {
		schema, _ := mergeOptionsSchemaByKey(parent.OptionsSchema, child.OptionsSchema)
		return schema
	}
	return child.OptionsSchema
}

func normalizeProviderLayerArgsForSchema(spec ProviderSpec, schema []ProviderOption) ProviderSpec {
	if len(schema) == 0 {
		return spec
	}
	allFlags := CollectAllSchemaFlags(schema)
	if len(allFlags) == 0 {
		return spec
	}
	defaults := cloneStringMap(spec.OptionDefaults)
	if defaults == nil {
		defaults = make(map[string]string)
	}
	if spec.Args != nil {
		spec.Args = stripArgsSlice(spec.Args, allFlags, schema, defaults)
	}
	if spec.ArgsAppend != nil {
		spec.ArgsAppend = stripArgsSlice(spec.ArgsAppend, allFlags, schema, defaults)
	}
	if len(defaults) > 0 || spec.OptionDefaults != nil {
		spec.OptionDefaults = defaults
	}
	return spec
}

// resolveProviderKind determines the canonical builtin provider name for a
// given provider name. If the name is a builtin, it returns itself. If
// it's a custom alias whose Command matches a builtin, it returns the
// builtin name. Otherwise returns the name as-is (no known builtin base).
//
// Limitation: wrapper aliases that use an intermediary launcher
// (e.g., command = "aimux", args = ["run", "gemini"]) are not resolved
// to the underlying builtin provider. The kind will be "aimux" rather
// than "gemini". Fixing this requires a deeper design decision about
// how to parse args for wrapped providers and is deferred.
func resolveProviderKind(name string, cityProviders map[string]ProviderSpec) string {
	builtins := BuiltinProviders()
	if _, ok := builtins[name]; ok {
		return name
	}
	if cityProviders != nil {
		if spec, ok := cityProviders[name]; ok && spec.Command != "" {
			if _, ok := builtins[spec.Command]; ok {
				return spec.Command
			}
		}
	}
	return name
}

// BuiltinFamily returns the built-in ancestor for a provider name,
// resolving the chain if the name refers to a custom provider with
// `base` set. Returns the name itself when it's a built-in, or "" when
// the name is fully custom with no built-in ancestor (including when
// chain resolution fails — callers should treat "" as "family
// undetermined" rather than silently widening the match).
//
// Runtime sites that branch on provider family (soft-escape interrupt,
// default submit, hook handler, skill-sink vendor) MUST consume this
// helper (or ResolvedProvider.BuiltinAncestor when available) instead
// of comparing the raw provider name. This lets a wrapped custom
// provider (e.g. [providers.my-fast-claude] base = "builtin:claude")
// be recognized as claude-family.
func BuiltinFamily(name string, cityProviders map[string]ProviderSpec) string {
	builtins := BuiltinProviders()
	if cityProviders != nil {
		if spec, ok := cityProviders[name]; ok {
			// A city provider with an explicit base declaration owns its
			// family identity, even when it shadows a built-in name.
			if spec.Base != nil {
				if strings.TrimSpace(*spec.Base) == "" {
					return ""
				}
				resolved, err := ResolveProviderChain(name, spec, cityProviders)
				if err != nil {
					return ""
				}
				return resolved.BuiltinAncestor
			}
			// Phase A legacy auto-inheritance: no `base` declared. Same-name
			// shadowing and command-match both retain the legacy built-in family.
			if _, ok := builtins[name]; ok {
				return name
			}
			if spec.Command != "" {
				if _, ok := builtins[spec.Command]; ok {
					return spec.Command
				}
			}
			return ""
		}
	}
	// Direct built-in match when there is no city-level shadowing provider.
	if _, ok := builtins[name]; ok {
		return name
	}
	return ""
}

// detectProviderName scans PATH for known built-in provider binaries.
// Returns the first found in priority order (see BuiltinProviderOrder).
func detectProviderName(lookPath LookPathFunc) (string, error) {
	builtins := BuiltinProviders()
	order := BuiltinProviderOrder()
	for _, name := range order {
		spec := builtins[name]
		if _, err := lookPath(spec.pathCheckBinary()); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("no supported agent CLI found in PATH (looked for: %s)", strings.Join(order, ", "))
}

// specToResolved converts a ProviderSpec to a ResolvedProvider.
func specToResolved(name string, spec *ProviderSpec) *ResolvedProvider {
	rp := &ResolvedProvider{
		Name:                   name,
		Command:                spec.Command,
		PromptMode:             spec.PromptMode,
		PromptFlag:             spec.PromptFlag,
		ReadyDelayMs:           spec.ReadyDelayMs,
		ReadyPromptPrefix:      spec.ReadyPromptPrefix,
		EmitsPermissionWarning: derefBool(spec.EmitsPermissionWarning),
		SupportsACP:            derefBool(spec.SupportsACP),
		SupportsHooks:          derefBool(spec.SupportsHooks),
		InstructionsFile:       spec.InstructionsFile,
		ResumeFlag:             spec.ResumeFlag,
		ResumeStyle:            spec.ResumeStyle,
		ResumeCommand:          spec.ResumeCommand,
		SessionIDFlag:          spec.SessionIDFlag,
		TitleModel:             spec.TitleModel,
		ACPCommand:             spec.ACPCommand,
	}
	// Deep-copy OptionsSchema to avoid aliasing the spec's slice.
	if len(spec.OptionsSchema) > 0 {
		rp.OptionsSchema = make([]ProviderOption, len(spec.OptionsSchema))
		for i, opt := range spec.OptionsSchema {
			rp.OptionsSchema[i] = opt
			if len(opt.Choices) > 0 {
				rp.OptionsSchema[i].Choices = make([]OptionChoice, len(opt.Choices))
				for j, c := range opt.Choices {
					rp.OptionsSchema[i].Choices[j] = c
					if len(c.FlagArgs) > 0 {
						rp.OptionsSchema[i].Choices[j].FlagArgs = make([]string, len(c.FlagArgs))
						copy(rp.OptionsSchema[i].Choices[j].FlagArgs, c.FlagArgs)
					}
					if len(c.FlagAliases) > 0 {
						rp.OptionsSchema[i].Choices[j].FlagAliases = cloneStringSlices(c.FlagAliases)
					}
				}
			}
		}
	}
	// Default InstructionsFile to "AGENTS.md" if unset.
	if rp.InstructionsFile == "" {
		rp.InstructionsFile = "AGENTS.md"
	}
	// Copy slices to avoid aliasing.
	if spec.Args != nil {
		rp.Args = make([]string, len(spec.Args))
		copy(rp.Args, spec.Args)
	}

	// Strip schema-managed flags from Args. This handles backward compatibility:
	// if a city.toml still has schema-managed flags in args (e.g.,
	// --dangerously-skip-permissions), they get removed because the option is
	// covered by OptionsSchema. Inferred defaults preserve user intent.
	if len(rp.OptionsSchema) > 0 && rp.Args != nil {
		allFlags := CollectAllSchemaFlags(rp.OptionsSchema)
		inferredDefaults := make(map[string]string)
		// Seed with existing OptionDefaults; same-layer Args override them
		// when stripArgsSlice infers a schema-managed choice.
		for k, v := range spec.OptionDefaults {
			inferredDefaults[k] = v
		}
		rp.Args = stripArgsSlice(rp.Args, allFlags, rp.OptionsSchema, inferredDefaults)
		// Compute EffectiveDefaults using inferred defaults (which include
		// both the spec's OptionDefaults and any values inferred from stripped Args).
		rp.EffectiveDefaults = ComputeEffectiveDefaults(rp.OptionsSchema, inferredDefaults, nil)
	} else {
		rp.EffectiveDefaults = ComputeEffectiveDefaults(rp.OptionsSchema, spec.OptionDefaults, nil)
	}
	if len(spec.ProcessNames) > 0 {
		rp.ProcessNames = make([]string, len(spec.ProcessNames))
		copy(rp.ProcessNames, spec.ProcessNames)
	}
	if len(spec.Env) > 0 {
		rp.Env = make(map[string]string, len(spec.Env))
		for k, v := range spec.Env {
			rp.Env[k] = v
		}
	}
	if len(spec.PermissionModes) > 0 {
		rp.PermissionModes = make(map[string]string, len(spec.PermissionModes))
		for k, v := range spec.PermissionModes {
			rp.PermissionModes[k] = v
		}
	}
	if len(spec.PrintArgs) > 0 {
		rp.PrintArgs = make([]string, len(spec.PrintArgs))
		copy(rp.PrintArgs, spec.PrintArgs)
	}
	if spec.ACPArgs != nil {
		rp.ACPArgs = make([]string, len(spec.ACPArgs))
		copy(rp.ACPArgs, spec.ACPArgs)
	}
	return rp
}

func completeResolvedProviderResumeCommand(rp *ResolvedProvider) {
	rp.ResumeCommand = completeResumeCommandDefaults(rp.ResumeCommand, rp.ResumeFlag, rp.ResumeStyle, rp.OptionsSchema, rp.EffectiveDefaults)
}

// AgentHasHooks reports whether an agent has provider hooks installed
// (either auto-installed or manually). The determination considers:
//
//  1. Explicit override: agent.HooksInstalled is set → use that value.
//  2. Claude-family always has hooks (via --settings override).
//  3. Provider name appears in the resolved install_agent_hooks list.
//  4. Otherwise: no hooks.
//
// cityProviders is consulted via BuiltinFamily so a wrapped custom
// provider (e.g. [providers.claude-max] base = "builtin:claude") is
// recognized as claude-family and gets the same default behavior as
// literal "claude". Passing nil falls back to raw name comparison and
// is only correct when the caller is certain no wrapped alias is in
// play.
func AgentHasHooks(agent *Agent, ws *Workspace, providerName string, cityProviders map[string]ProviderSpec) bool {
	// 1. Explicit override wins.
	if agent.HooksInstalled != nil {
		return *agent.HooksInstalled
	}
	// 2. Claude-family always has hooks via --settings. Use BuiltinFamily
	//    so wrapped custom providers (e.g. claude-max with
	//    base = "builtin:claude") are correctly recognized.
	if BuiltinFamily(providerName, cityProviders) == "claude" {
		return true
	}
	// 3. Check install_agent_hooks (agent-level overrides workspace-level).
	installHooks := ResolveInstallHooks(agent, ws)
	for _, h := range installHooks {
		if h == providerName {
			return true
		}
	}
	return false
}

// mergeAgentOverrides applies non-zero agent-level fields on top of the
// resolved provider. Env merges additively (agent keys add to / override
// base keys). All other fields replace when set.
func mergeAgentOverrides(rp *ResolvedProvider, agent *Agent) {
	if len(agent.Args) > 0 {
		rp.Args = make([]string, len(agent.Args))
		copy(rp.Args, agent.Args)
	}
	if agent.PromptMode != "" {
		rp.PromptMode = agent.PromptMode
	}
	if agent.PromptFlag != "" {
		rp.PromptFlag = agent.PromptFlag
	}
	if agent.ReadyDelayMs != nil {
		rp.ReadyDelayMs = *agent.ReadyDelayMs
	}
	if agent.ReadyPromptPrefix != "" {
		rp.ReadyPromptPrefix = agent.ReadyPromptPrefix
	}
	if len(agent.ProcessNames) > 0 {
		rp.ProcessNames = make([]string, len(agent.ProcessNames))
		copy(rp.ProcessNames, agent.ProcessNames)
	}
	if agent.EmitsPermissionWarning != nil {
		rp.EmitsPermissionWarning = *agent.EmitsPermissionWarning
	}
	if agent.ResumeCommand != "" {
		rp.ResumeCommand = agent.ResumeCommand
	}
	// Env merges additively.
	if len(agent.Env) > 0 {
		if rp.Env == nil {
			rp.Env = make(map[string]string, len(agent.Env))
		}
		for k, v := range agent.Env {
			rp.Env[k] = v
		}
	}

	// OptionDefaults: agent overrides merge on top of effective defaults.
	if len(agent.OptionDefaults) > 0 {
		if rp.EffectiveDefaults == nil {
			rp.EffectiveDefaults = make(map[string]string)
		}
		for k, v := range agent.OptionDefaults {
			rp.EffectiveDefaults[k] = v
		}
	}
}

// resolvedChainToSpec folds a chain-resolved ResolvedProvider back into
// a ProviderSpec. Used by lookupProvider so downstream callers (agent
// merge, specToResolved) see the inherited fields from the chain walk.
// Preserves the original leaf spec's fields that ResolvedProvider
// doesn't carry (DisplayName, PathCheck).
func resolvedChainToSpec(r ResolvedProvider, leaf ProviderSpec) ProviderSpec {
	out := leaf
	out.Command = r.Command
	if r.Args != nil {
		out.Args = append([]string(nil), r.Args...)
	}
	if r.PromptMode != "" {
		out.PromptMode = r.PromptMode
	}
	if r.PromptFlag != "" {
		out.PromptFlag = r.PromptFlag
	}
	if r.ReadyDelayMs != 0 {
		out.ReadyDelayMs = r.ReadyDelayMs
	}
	if r.ReadyPromptPrefix != "" {
		out.ReadyPromptPrefix = r.ReadyPromptPrefix
	}
	if r.ProcessNames != nil {
		out.ProcessNames = append([]string(nil), r.ProcessNames...)
	}
	// Tri-state *bool: preserve from leaf if set; else fold from the
	// resolved value only when some chain layer explicitly contributed it.
	if leaf.EmitsPermissionWarning == nil && providerBoolFieldSet(r, "emits_permission_warning") {
		v := r.EmitsPermissionWarning
		out.EmitsPermissionWarning = &v
	}
	if leaf.SupportsACP == nil && providerBoolFieldSet(r, "supports_acp") {
		v := r.SupportsACP
		out.SupportsACP = &v
	}
	if leaf.SupportsHooks == nil && providerBoolFieldSet(r, "supports_hooks") {
		v := r.SupportsHooks
		out.SupportsHooks = &v
	}
	if r.InstructionsFile != "" {
		out.InstructionsFile = r.InstructionsFile
	}
	if r.ResumeFlag != "" {
		out.ResumeFlag = r.ResumeFlag
	}
	if r.ResumeStyle != "" {
		out.ResumeStyle = r.ResumeStyle
	}
	if r.ResumeCommand != "" {
		out.ResumeCommand = r.ResumeCommand
	}
	if r.SessionIDFlag != "" {
		out.SessionIDFlag = r.SessionIDFlag
	}
	if r.TitleModel != "" {
		out.TitleModel = r.TitleModel
	}
	if r.ACPCommand != "" {
		out.ACPCommand = r.ACPCommand
	}
	if r.ACPArgs != nil {
		out.ACPArgs = make([]string, len(r.ACPArgs))
		copy(out.ACPArgs, r.ACPArgs)
	}
	if r.PrintArgs != nil {
		out.PrintArgs = append([]string(nil), r.PrintArgs...)
	}
	if r.Env != nil {
		out.Env = make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			out.Env[k] = v
		}
	}
	if r.PermissionModes != nil {
		out.PermissionModes = make(map[string]string, len(r.PermissionModes))
		for k, v := range r.PermissionModes {
			out.PermissionModes[k] = v
		}
	}
	if r.OptionsSchema != nil {
		out.OptionsSchema = deepCopyProviderOptions(r.OptionsSchema)
	}
	// EffectiveDefaults on ResolvedProvider is the normalized merged defaults;
	// replace OptionDefaults on the folded spec so same-layer schema-managed
	// args cannot be shadowed again by the original stale leaf map.
	if r.EffectiveDefaults != nil {
		out.OptionDefaults = cloneStringMap(r.EffectiveDefaults)
	}
	return out
}

func providerBoolFieldSet(r ResolvedProvider, field string) bool {
	if r.Provenance.FieldLayer == nil {
		return false
	}
	_, ok := r.Provenance.FieldLayer[field]
	return ok
}
