package config

import "fmt"

// Patches holds all patch blocks from composition. Patches target existing
// resources by identity key and modify specific fields. They are applied
// after fragment merge, before validation.
type Patches struct {
	// Agents targets agents by (dir, name).
	Agents []AgentPatch `toml:"agent,omitempty"`
	// Rigs targets rigs by name.
	Rigs []RigPatch `toml:"rigs,omitempty"`
	// Providers targets providers by name.
	Providers []ProviderPatch `toml:"providers,omitempty"`
}

// AgentPatch modifies an existing agent identified by (Dir, Name).
// Pointer fields distinguish "not set" from "set to zero value."
type AgentPatch struct {
	// Dir is the targeting key (required with Name). Identifies the agent's
	// working directory scope. Empty for city-scoped agents.
	Dir string `toml:"dir" jsonschema:"required"`
	// Name is the targeting key (required). Must match an existing agent's name.
	Name string `toml:"name" jsonschema:"required"`
	// WorkDir overrides the agent's session working directory.
	WorkDir *string `toml:"work_dir,omitempty"`
	// Scope overrides the agent's scope ("city" or "rig").
	Scope *string `toml:"scope,omitempty"`
	// Suspended overrides the agent's suspended state.
	Suspended *bool `toml:"suspended,omitempty"`
	// Pool overrides legacy [pool] fields that map to session scaling.
	Pool *PoolOverride `toml:"pool,omitempty"`
	// Env adds or overrides environment variables.
	Env map[string]string `toml:"env,omitempty"`
	// EnvRemove lists env var keys to remove after merging.
	EnvRemove []string `toml:"env_remove,omitempty"`
	// PreStart overrides the agent's pre_start commands.
	PreStart []string `toml:"pre_start,omitempty"`
	// PromptTemplate overrides the prompt template path.
	// Relative paths resolve against the city directory.
	PromptTemplate *string `toml:"prompt_template,omitempty"`
	// Session overrides the session transport ("acp").
	Session *string `toml:"session,omitempty"`
	// Provider overrides the provider name.
	Provider *string `toml:"provider,omitempty"`
	// StartCommand overrides the start command.
	StartCommand *string `toml:"start_command,omitempty"`
	// Nudge overrides the nudge text.
	Nudge *string `toml:"nudge,omitempty"`
	// IdleTimeout overrides the idle timeout. Duration string (e.g., "30s", "5m", "1h").
	IdleTimeout *string `toml:"idle_timeout,omitempty"`
	// SleepAfterIdle overrides idle sleep policy for this agent. Accepts a
	// duration string or "off".
	SleepAfterIdle *string `toml:"sleep_after_idle,omitempty"`
	// InstallAgentHooks overrides the agent's install_agent_hooks list.
	InstallAgentHooks []string `toml:"install_agent_hooks,omitempty"`
	// Skills is a tombstone field retained for v0.15.1 backwards compatibility.
	//
	// Deprecated: removed in v0.16. Tombstone — accepted but ignored. See
	// engdocs/proposals/skill-materialization.md
	Skills []string `toml:"skills,omitempty"`
	// MCP is a tombstone field retained for v0.15.1 backwards compatibility.
	//
	// Deprecated: removed in v0.16. Tombstone — accepted but ignored. See
	// engdocs/proposals/skill-materialization.md
	MCP []string `toml:"mcp,omitempty"`
	// SkillsAppend is a tombstone field retained for v0.15.1 backwards
	// compatibility.
	//
	// Deprecated: removed in v0.16. Tombstone — accepted but ignored. See
	// engdocs/proposals/skill-materialization.md
	SkillsAppend []string `toml:"skills_append,omitempty"`
	// MCPAppend is a tombstone field retained for v0.15.1 backwards
	// compatibility.
	//
	// Deprecated: removed in v0.16. Tombstone — accepted but ignored. See
	// engdocs/proposals/skill-materialization.md
	MCPAppend []string `toml:"mcp_append,omitempty"`
	// HooksInstalled overrides automatic hook detection.
	HooksInstalled *bool `toml:"hooks_installed,omitempty"`
	// InjectAssignedSkills overrides per-agent appendix injection
	// (see Agent.InjectAssignedSkills).
	InjectAssignedSkills *bool `toml:"inject_assigned_skills,omitempty"`
	// SessionSetup overrides the agent's session_setup commands.
	SessionSetup []string `toml:"session_setup,omitempty"`
	// SessionSetupScript overrides the agent's session_setup_script path.
	// Relative paths resolve against the city directory.
	SessionSetupScript *string `toml:"session_setup_script,omitempty"`
	// SessionLive overrides the agent's session_live commands.
	SessionLive []string `toml:"session_live,omitempty"`
	// OverlayDir overrides the agent's overlay_dir path. Copies contents
	// additively into the agent's working directory at startup.
	// Relative paths resolve against the city directory.
	OverlayDir *string `toml:"overlay_dir,omitempty"`
	// DefaultSlingFormula overrides the default sling formula.
	DefaultSlingFormula *string `toml:"default_sling_formula,omitempty"`
	// InjectFragments overrides the agent's inject_fragments list.
	InjectFragments []string `toml:"inject_fragments,omitempty"`
	// Attach overrides the agent's attach setting.
	Attach *bool `toml:"attach,omitempty"`
	// DependsOn overrides the agent's dependency list.
	DependsOn []string `toml:"depends_on,omitempty"`
	// ResumeCommand overrides the agent's resume_command template.
	ResumeCommand *string `toml:"resume_command,omitempty"`
	// WakeMode overrides the agent's wake mode ("resume" or "fresh").
	WakeMode *string `toml:"wake_mode,omitempty" jsonschema:"enum=resume,enum=fresh"`
	// PreStartAppend appends commands to the agent's pre_start list
	// (instead of replacing). Applied after PreStart if both are set.
	PreStartAppend []string `toml:"pre_start_append,omitempty"`
	// SessionSetupAppend appends commands to the agent's session_setup list.
	SessionSetupAppend []string `toml:"session_setup_append,omitempty"`
	// SessionLiveAppend appends commands to the agent's session_live list.
	SessionLiveAppend []string `toml:"session_live_append,omitempty"`
	// InstallAgentHooksAppend appends to the agent's install_agent_hooks list.
	InstallAgentHooksAppend []string `toml:"install_agent_hooks_append,omitempty"`
	// InjectFragmentsAppend appends to the agent's inject_fragments list.
	InjectFragmentsAppend []string `toml:"inject_fragments_append,omitempty"`
	// MaxActiveSessions overrides the agent-level cap on concurrent sessions.
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// MinActiveSessions overrides the minimum number of sessions to keep alive.
	MinActiveSessions *int `toml:"min_active_sessions,omitempty"`
	// ScaleCheck overrides the command template whose output determines desired
	// session count. Supports the same Go template placeholders as
	// Agent.scale_check.
	ScaleCheck *string `toml:"scale_check,omitempty"`
	// OptionDefaults adds or overrides provider option defaults for this agent.
	// Keys are option keys, values are choice values. Merges additively
	// (patch keys win over existing agent keys).
	// Example: option_defaults = { model = "sonnet" }
	OptionDefaults map[string]string `toml:"option_defaults,omitempty"`
}

// PoolOverride modifies legacy [pool] fields that map to session scaling. Nil fields are not changed.
type PoolOverride struct {
	// Min overrides the minimum number of sessions.
	Min *int `toml:"min,omitempty" jsonschema:"minimum=0"`
	// Max overrides the maximum number of sessions. 0 means no sessions can claim routed work.
	Max *int `toml:"max,omitempty" jsonschema:"minimum=0"`
	// Check overrides the session scale check command template. Supports the
	// same Go template placeholders as Agent.scale_check.
	Check *string `toml:"check,omitempty"`
	// DrainTimeout overrides the drain timeout. Duration string (e.g., "5m", "30m", "1h").
	DrainTimeout *string `toml:"drain_timeout,omitempty"`
	// OnDeath overrides the on_death command template. Supports the same Go
	// template placeholders as Agent.on_death.
	OnDeath *string `toml:"on_death,omitempty"`
	// OnBoot overrides the on_boot command template. Supports the same Go
	// template placeholders as Agent.on_boot.
	OnBoot *string `toml:"on_boot,omitempty"`
}

// RigPatch modifies an existing rig identified by Name.
type RigPatch struct {
	// Name is the targeting key (required). Must match an existing rig's name.
	Name string `toml:"name" jsonschema:"required"`
	// Path overrides the rig's filesystem path.
	Path *string `toml:"path,omitempty"`
	// Prefix overrides the bead ID prefix.
	Prefix *string `toml:"prefix,omitempty"`
	// Suspended overrides the rig's suspended state.
	Suspended *bool `toml:"suspended,omitempty"`
}

// ProviderPatch modifies an existing provider identified by Name.
type ProviderPatch struct {
	// Name is the targeting key (required). Must match an existing provider's name.
	Name string `toml:"name" jsonschema:"required"`
	// Base overrides the provider's inheritance parent (presence-aware).
	// Pointer to a pointer so the patch can distinguish "no change"
	// (double-nil) from "clear to inherit default" (single-nil value in
	// outer pointer) from "set to explicit empty opt-out" (value "" in
	// inner pointer) from "set to <name>". Callers use:
	//   nil          = patch does not touch Base
	//   &(*string)(nil) = patch clears Base to absent
	//   &(&"")       = patch sets Base = "" (explicit opt-out)
	//   &(&"builtin:codex") = patch sets Base to that value
	Base **string `toml:"base,omitempty"`
	// Command overrides the provider command.
	Command *string `toml:"command,omitempty"`
	// Args overrides the provider args.
	Args []string `toml:"args,omitempty"`
	// ArgsAppend overrides the provider args_append list.
	ArgsAppend []string `toml:"args_append,omitempty"`
	// OptionsSchemaMerge overrides the options_schema merge mode.
	OptionsSchemaMerge *string `toml:"options_schema_merge,omitempty"`
	// PromptMode overrides prompt delivery mode.
	PromptMode *string `toml:"prompt_mode,omitempty" jsonschema:"enum=arg,enum=flag,enum=none"`
	// PromptFlag overrides the prompt flag.
	PromptFlag *string `toml:"prompt_flag,omitempty"`
	// ReadyDelayMs overrides the ready delay in milliseconds.
	ReadyDelayMs *int `toml:"ready_delay_ms,omitempty" jsonschema:"minimum=0"`
	// Env adds or overrides environment variables.
	Env map[string]string `toml:"env,omitempty"`
	// EnvRemove lists env var keys to remove.
	EnvRemove []string `toml:"env_remove,omitempty"`
	// Replace replaces the entire provider block instead of deep-merging.
	Replace bool `toml:"_replace,omitempty"`
}

// IsEmpty reports whether p has no patch operations.
func (p *Patches) IsEmpty() bool {
	return len(p.Agents) == 0 && len(p.Rigs) == 0 && len(p.Providers) == 0
}

// ApplyPatches applies all patches to the config. Patches target existing
// resources by identity key. If a patch targets a nonexistent resource,
// an error is returned. Patches are intentional — they never generate
// collision warnings.
func ApplyPatches(cfg *City, patches Patches) error {
	for i, p := range patches.Agents {
		if err := applyAgentPatch(cfg, &p); err != nil {
			return fmt.Errorf("patches.agent[%d]: %w", i, err)
		}
	}
	for i, p := range patches.Rigs {
		if err := applyRigPatch(cfg, &p); err != nil {
			return fmt.Errorf("patches.rigs[%d]: %w", i, err)
		}
	}
	for i, p := range patches.Providers {
		if err := applyProviderPatch(cfg, &p); err != nil {
			return fmt.Errorf("patches.providers[%d]: %w", i, err)
		}
	}
	return nil
}

// applyAgentPatch finds an agent by (dir, name) and applies the patch.
func applyAgentPatch(cfg *City, patch *AgentPatch) error {
	if patch.Name == "" {
		return fmt.Errorf("agent patch: name is required")
	}
	target := qualifiedNameFromPatch(patch.Dir, patch.Name)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		// V2: match by qualified name so patches targeting "gastown.mayor"
		// find agents with BindingName="gastown" and Name="mayor".
		if AgentMatchesIdentity(a, target) {
			applyAgentPatchFields(a, patch)
			return nil
		}
		// V1 fallback: direct Dir+Name match.
		if a.Dir == patch.Dir && a.Name == patch.Name {
			applyAgentPatchFields(a, patch)
			return nil
		}
	}
	return fmt.Errorf("agent %q not found in merged config", target)
}

func applyAgentPatchFields(a *Agent, p *AgentPatch) {
	if p.WorkDir != nil {
		a.WorkDir = *p.WorkDir
	}
	if p.Scope != nil {
		a.Scope = *p.Scope
	}
	if p.Suspended != nil {
		a.Suspended = *p.Suspended
	}
	if len(p.PreStart) > 0 {
		a.PreStart = append([]string(nil), p.PreStart...)
	}
	if len(p.PreStartAppend) > 0 {
		a.PreStart = append(a.PreStart, p.PreStartAppend...)
	}
	if p.PromptTemplate != nil {
		a.PromptTemplate = *p.PromptTemplate
	}
	if p.Session != nil {
		a.Session = *p.Session
	}
	if p.Provider != nil {
		a.Provider = *p.Provider
	}
	if p.StartCommand != nil {
		a.StartCommand = *p.StartCommand
	}
	if p.Nudge != nil {
		a.Nudge = *p.Nudge
	}
	if p.IdleTimeout != nil {
		a.IdleTimeout = *p.IdleTimeout
	}
	if p.SleepAfterIdle != nil {
		a.SleepAfterIdle = NormalizeSleepAfterIdle(*p.SleepAfterIdle)
		a.SleepAfterIdleSource = "agent_patch"
	}
	if len(p.InstallAgentHooks) > 0 {
		a.InstallAgentHooks = append([]string(nil), p.InstallAgentHooks...)
	}
	if len(p.InstallAgentHooksAppend) > 0 {
		a.InstallAgentHooks = append(a.InstallAgentHooks, p.InstallAgentHooksAppend...)
	}
	if p.HooksInstalled != nil {
		a.HooksInstalled = p.HooksInstalled
	}
	if p.InjectAssignedSkills != nil {
		a.InjectAssignedSkills = p.InjectAssignedSkills
	}
	if len(p.SessionSetup) > 0 {
		a.SessionSetup = append([]string(nil), p.SessionSetup...)
	}
	if len(p.SessionSetupAppend) > 0 {
		a.SessionSetup = append(a.SessionSetup, p.SessionSetupAppend...)
	}
	if p.SessionSetupScript != nil {
		a.SessionSetupScript = *p.SessionSetupScript
	}
	if len(p.SessionLive) > 0 {
		a.SessionLive = append([]string(nil), p.SessionLive...)
	}
	if len(p.SessionLiveAppend) > 0 {
		a.SessionLive = append(a.SessionLive, p.SessionLiveAppend...)
	}
	if p.OverlayDir != nil {
		a.OverlayDir = *p.OverlayDir
	}
	if p.DefaultSlingFormula != nil {
		a.DefaultSlingFormula = p.DefaultSlingFormula
	}
	if p.Attach != nil {
		a.Attach = p.Attach
	}
	// TODO: depends_on = [] cannot clear inherited deps (len check skips
	// empty lists). This matches the existing pattern for all list fields
	// (PreStart, SessionSetup, etc.) but limits composability. A broader
	// fix would use *[]string or a presence flag across all list fields.
	if len(p.DependsOn) > 0 {
		a.DependsOn = append([]string(nil), p.DependsOn...)
	}
	if p.ResumeCommand != nil {
		a.ResumeCommand = *p.ResumeCommand
	}
	if p.WakeMode != nil {
		a.WakeMode = *p.WakeMode
	}
	if len(p.InjectFragments) > 0 {
		a.InjectFragments = append([]string(nil), p.InjectFragments...)
	}
	if len(p.InjectFragmentsAppend) > 0 {
		a.InjectFragments = append(a.InjectFragments, p.InjectFragmentsAppend...)
	}
	// Env: additive merge.
	if len(p.Env) > 0 {
		if a.Env == nil {
			a.Env = make(map[string]string, len(p.Env))
		}
		for k, v := range p.Env {
			a.Env[k] = v
		}
	}
	// EnvRemove: remove keys after merge.
	for _, k := range p.EnvRemove {
		delete(a.Env, k)
	}
	if p.MaxActiveSessions != nil {
		a.MaxActiveSessions = p.MaxActiveSessions
	}
	if p.MinActiveSessions != nil {
		a.MinActiveSessions = p.MinActiveSessions
	}
	if p.ScaleCheck != nil {
		a.ScaleCheck = *p.ScaleCheck
	}
	// OptionDefaults: additive merge (patch keys win).
	if len(p.OptionDefaults) > 0 {
		if a.OptionDefaults == nil {
			a.OptionDefaults = make(map[string]string, len(p.OptionDefaults))
		}
		for k, v := range p.OptionDefaults {
			a.OptionDefaults[k] = v
		}
	}
	// Pool: sub-field patching.
	if p.Pool != nil {
		applyPoolOverride(a, p.Pool)
	}
}

// applyPoolOverride maps legacy pool override fields to the new Agent fields.
func applyPoolOverride(a *Agent, po *PoolOverride) {
	if po.Min != nil {
		a.MinActiveSessions = po.Min
	}
	if po.Max != nil {
		a.MaxActiveSessions = po.Max
	}
	if po.Check != nil {
		a.ScaleCheck = *po.Check
	}
	if po.DrainTimeout != nil {
		a.DrainTimeout = *po.DrainTimeout
	}
	if po.OnDeath != nil {
		a.OnDeath = *po.OnDeath
	}
	if po.OnBoot != nil {
		a.OnBoot = *po.OnBoot
	}
}

// applyRigPatch finds a rig by name and applies the patch.
func applyRigPatch(cfg *City, patch *RigPatch) error {
	if patch.Name == "" {
		return fmt.Errorf("rig patch: name is required")
	}
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if r.Name == patch.Name {
			if patch.Path != nil {
				r.Path = *patch.Path
			}
			if patch.Prefix != nil {
				r.Prefix = *patch.Prefix
			}
			if patch.Suspended != nil {
				r.Suspended = *patch.Suspended
			}
			return nil
		}
	}
	return fmt.Errorf("rig %q not found in merged config", patch.Name)
}

// applyProviderPatch modifies a provider. If Replace is true, replaces the
// entire block. Otherwise deep-merges per-field.
func applyProviderPatch(cfg *City, patch *ProviderPatch) error {
	if patch.Name == "" {
		return fmt.Errorf("provider patch: name is required")
	}
	if cfg.Providers == nil {
		return fmt.Errorf("provider %q not found in merged config", patch.Name)
	}
	spec, ok := cfg.Providers[patch.Name]
	if !ok {
		return fmt.Errorf("provider %q not found in merged config", patch.Name)
	}
	if patch.Replace {
		// Full replacement — build a new spec from patch fields only.
		var newSpec ProviderSpec
		if patch.Base != nil {
			newSpec.Base = *patch.Base
		}
		if patch.Command != nil {
			newSpec.Command = *patch.Command
		}
		if len(patch.Args) > 0 {
			newSpec.Args = make([]string, len(patch.Args))
			copy(newSpec.Args, patch.Args)
		}
		if len(patch.ArgsAppend) > 0 {
			newSpec.ArgsAppend = make([]string, len(patch.ArgsAppend))
			copy(newSpec.ArgsAppend, patch.ArgsAppend)
		}
		if patch.OptionsSchemaMerge != nil {
			newSpec.OptionsSchemaMerge = *patch.OptionsSchemaMerge
		}
		if patch.PromptMode != nil {
			newSpec.PromptMode = *patch.PromptMode
		}
		if patch.PromptFlag != nil {
			newSpec.PromptFlag = *patch.PromptFlag
		}
		if patch.ReadyDelayMs != nil {
			newSpec.ReadyDelayMs = *patch.ReadyDelayMs
		}
		if len(patch.Env) > 0 {
			newSpec.Env = make(map[string]string, len(patch.Env))
			for k, v := range patch.Env {
				newSpec.Env[k] = v
			}
		}
		cfg.Providers[patch.Name] = newSpec
		return nil
	}
	// Deep merge: only set fields override.
	if patch.Base != nil {
		spec.Base = *patch.Base // outer nil handled above; *patch.Base may be nil (clear) or valid
	}
	if patch.Command != nil {
		spec.Command = *patch.Command
	}
	if len(patch.Args) > 0 {
		spec.Args = make([]string, len(patch.Args))
		copy(spec.Args, patch.Args)
	}
	if len(patch.ArgsAppend) > 0 {
		spec.ArgsAppend = make([]string, len(patch.ArgsAppend))
		copy(spec.ArgsAppend, patch.ArgsAppend)
	}
	if patch.OptionsSchemaMerge != nil {
		spec.OptionsSchemaMerge = *patch.OptionsSchemaMerge
	}
	if patch.PromptMode != nil {
		spec.PromptMode = *patch.PromptMode
	}
	if patch.PromptFlag != nil {
		spec.PromptFlag = *patch.PromptFlag
	}
	if patch.ReadyDelayMs != nil {
		spec.ReadyDelayMs = *patch.ReadyDelayMs
	}
	// Env: additive merge.
	if len(patch.Env) > 0 {
		if spec.Env == nil {
			spec.Env = make(map[string]string, len(patch.Env))
		}
		for k, v := range patch.Env {
			spec.Env[k] = v
		}
	}
	for _, k := range patch.EnvRemove {
		delete(spec.Env, k)
	}
	cfg.Providers[patch.Name] = spec
	return nil
}

func qualifiedNameFromPatch(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
