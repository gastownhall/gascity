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
//  5. Default prompt_mode to "arg" if still empty
func ResolveProvider(agent *Agent, ws *Workspace, cityProviders map[string]ProviderSpec, lookPath LookPathFunc) (*ResolvedProvider, error) {
	// Step 1: agent.StartCommand is the escape hatch.
	if agent.StartCommand != "" {
		return &ResolvedProvider{Command: agent.StartCommand, PromptMode: "arg"}, nil
	}

	// Step 2: determine provider name.
	name := agent.Provider
	if name == "" && ws != nil {
		name = ws.Provider
	}
	if name == "" {
		// No provider name — check workspace start_command escape hatch.
		if ws != nil && ws.StartCommand != "" {
			return &ResolvedProvider{Command: ws.StartCommand, PromptMode: "arg"}, nil
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
	mergeAgentOverrides(resolved, agent)

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
			// Layer city overrides on top of the built-in if the provider
			// name or command matches a known builtin. This lets city
			// configs override command/args while inheriting OptionsSchema,
			// PromptMode, ResumeFlag, etc. from the builtin defaults.
			builtins := BuiltinProviders()
			if base, ok := builtins[name]; ok {
				merged := MergeProviderOverBuiltin(base, spec)
				return &merged, nil
			}
			if base, ok := builtins[spec.Command]; ok {
				merged := MergeProviderOverBuiltin(base, spec)
				return &merged, nil
			}
			return &spec, nil
		}
	}

	// Fall back to built-in presets.
	builtins := BuiltinProviders()
	if spec, ok := builtins[name]; ok {
		if _, err := lookPath(spec.pathCheckBinary()); err != nil {
			return nil, fmt.Errorf("%w: %q", ErrProviderNotInPATH, name)
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
// Note: booleans are one-directional (can enable, not disable) due to TOML
// zero-value ambiguity — city providers cannot override a built-in's true
// to false for EmitsPermissionWarning, SupportsACP, or SupportsHooks.
func MergeProviderOverBuiltin(base, city ProviderSpec) ProviderSpec {
	result := base

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
	if city.EmitsPermissionWarning {
		result.EmitsPermissionWarning = true
	}
	if city.PathCheck != "" {
		result.PathCheck = city.PathCheck
	}
	if city.SupportsACP {
		result.SupportsACP = true
	}
	if city.SupportsHooks {
		result.SupportsHooks = true
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

	// Slice fields: replace entirely when non-nil.
	if city.Args != nil {
		result.Args = city.Args
	}
	if city.ProcessNames != nil {
		result.ProcessNames = city.ProcessNames
	}
	if city.OptionsSchema != nil {
		result.OptionsSchema = city.OptionsSchema
	}
	if city.PrintArgs != nil {
		result.PrintArgs = city.PrintArgs
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

	return result
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
		EmitsPermissionWarning: spec.EmitsPermissionWarning,
		SupportsACP:            spec.SupportsACP,
		SupportsHooks:          spec.SupportsHooks,
		InstructionsFile:       spec.InstructionsFile,
		ResumeFlag:             spec.ResumeFlag,
		ResumeStyle:            spec.ResumeStyle,
		ResumeCommand:          spec.ResumeCommand,
		SessionIDFlag:          spec.SessionIDFlag,
		TitleModel:             spec.TitleModel,
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
				}
			}
		}
	}
	// Default InstructionsFile to "AGENTS.md" if unset.
	if rp.InstructionsFile == "" {
		rp.InstructionsFile = "AGENTS.md"
	}
	// Copy slices to avoid aliasing.
	if len(spec.Args) > 0 {
		rp.Args = make([]string, len(spec.Args))
		copy(rp.Args, spec.Args)
	}

	// Strip schema-managed flags from Args. This handles backward compatibility:
	// if a city.toml still has schema-managed flags in args (e.g.,
	// --dangerously-skip-permissions), they get removed because the option is
	// covered by OptionsSchema. Inferred defaults preserve user intent.
	if len(rp.OptionsSchema) > 0 && len(rp.Args) > 0 {
		allFlags := CollectAllSchemaFlags(rp.OptionsSchema)
		inferredDefaults := make(map[string]string)
		// Seed with existing OptionDefaults so they aren't overridden.
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
	return rp
}

// AgentHasHooks reports whether an agent has provider hooks installed
// (either auto-installed or manually). The determination considers:
//
//  1. Explicit override: agent.HooksInstalled is set → use that value.
//  2. Claude always has hooks (via --settings override).
//  3. Provider name appears in the resolved install_agent_hooks list.
//  4. Otherwise: no hooks.
func AgentHasHooks(agent *Agent, ws *Workspace, providerName string) bool {
	// 1. Explicit override wins.
	if agent.HooksInstalled != nil {
		return *agent.HooksInstalled
	}
	// 2. Claude always has hooks via --settings.
	if providerName == "claude" {
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
