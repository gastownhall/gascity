// Package config handles loading and parsing city.toml configuration files.
package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// validAgentName matches names safe for use in session identifiers.
// Must start with a letter or digit, followed by letters, digits, hyphens,
// or underscores. Slashes, spaces, and dots are not allowed.
var validAgentName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// validNamedSessionTemplate matches either a bare agent name ("mayor") or a
// PackV2 import-qualified template ("gastown.mayor"). Rig qualification is
// carried separately in NamedSession.Dir, so slashes remain invalid here.
var validNamedSessionTemplate = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*(\.[a-zA-Z0-9][a-zA-Z0-9_-]*)?$`)

const (
	// ControlDispatcherAgentName is the built-in deterministic control lane for
	// graph.v2 workflow control beads.
	ControlDispatcherAgentName = "control-dispatcher"
	// ControlDispatcherStartCommand runs the built-in control-dispatcher worker.
	// Wrapped in `sh -c` so any appended prompt suffix is ignored as $0.
	// The control lane is kept resident and blocks on workflow-relevant city
	// events instead of exiting after each one-shot drain.
	ControlDispatcherStartCommand = `sh -c 'export GC_WORKFLOW_TRACE="${GC_WORKFLOW_TRACE:-${GC_CITY}/control-dispatcher-trace.log}"; exec "${GC_BIN:-gc}" convoy control --serve --follow ` + ControlDispatcherAgentName + `'`
)

// ControlDispatcherStartCommandFor returns the start command for a
// control-dispatcher agent with the given qualified name.
func ControlDispatcherStartCommandFor(qualifiedName string) string {
	return `sh -c 'export GC_WORKFLOW_TRACE="${GC_WORKFLOW_TRACE:-${GC_CITY}/control-dispatcher-trace.log}"; exec "${GC_BIN:-gc}" convoy control --serve --follow ` + qualifiedName + `'`
}

// BindingQualifiedName returns the binding-qualified agent identity without a
// rig prefix. Examples: "polecat", "gastown.polecat", or "gastown.mayor".
func (a *Agent) BindingQualifiedName() string {
	if a.BindingName == "" {
		return a.Name
	}
	return a.BindingName + "." + a.Name
}

// QualifiedName returns the agent's canonical identity, including the rig
// prefix when present. Examples: "mayor", "gastown.mayor",
// "hello-world/polecat", and "hello-world/gastown.polecat".
func (a *Agent) QualifiedName() string {
	name := a.BindingQualifiedName()
	if a.Dir == "" {
		return name
	}
	return a.Dir + "/" + name
}

// ParseQualifiedName splits an agent identity into (dir, name).
// "hello-world/polecat" → ("hello-world", "polecat").
// "hello-world/gastown.polecat" → ("hello-world", "gastown.polecat").
// "gastown.mayor" → ("", "gastown.mayor").
// "mayor" → ("", "mayor").
func ParseQualifiedName(identity string) (dir, name string) {
	if i := strings.LastIndex(identity, "/"); i >= 0 {
		return identity[:i], identity[i+1:]
	}
	return "", identity
}

// QualifiedInstanceName builds a qualified identity for a pool instance
// of this agent. For V2 agents with a BindingName, produces
// "dir/binding.instanceName" or "binding.instanceName". For V1 agents,
// produces "dir/instanceName" or just "instanceName".
func (a *Agent) QualifiedInstanceName(instanceName string) string {
	name := instanceName
	if a.BindingName != "" {
		name = a.BindingName + "." + instanceName
	}
	if a.Dir == "" {
		return name
	}
	return a.Dir + "/" + name
}

// AgentMatchesIdentity returns true if the agent's qualified name matches
// the given identity string. Handles both V1 format ("dir/name") and V2
// format ("dir/binding.name", "binding.name"). This is the canonical way
// to match user-supplied identity strings against agents; prefer it over
// manual Dir+Name comparisons. The V1 fallback only applies to agents
// without a BindingName — imported V2 agents must be addressed by their
// qualified name.
func AgentMatchesIdentity(a *Agent, identity string) bool {
	// Try V2 qualified name first (includes binding).
	if a.QualifiedName() == identity {
		return true
	}
	// Fallback: V1-style dir+name match. Only allowed when the agent
	// has no binding name — imported V2 agents must be addressed by
	// their qualified name (binding.name), not bare name.
	if a.BindingName == "" {
		dir, name := ParseQualifiedName(identity)
		return a.Dir == dir && a.Name == name
	}
	return false
}

// City is the top-level configuration for a Gas City instance.
// Parsed from city.toml at the root of a city directory.
type City struct {
	// Include lists config fragment files to merge into this config.
	// Processed by LoadWithIncludes; not recursive (fragments cannot include).
	Include []string `toml:"include,omitempty"`
	// Workspace holds city-level metadata (name, default provider).
	Workspace Workspace `toml:"workspace"`
	// Providers defines named provider presets for agent startup.
	Providers map[string]ProviderSpec `toml:"providers,omitempty"`
	// Packs defines named remote pack sources fetched via git (V1 mechanism).
	Packs map[string]PackSource `toml:"packs,omitempty"`
	// Imports defines named pack imports (V2 mechanism). Each key is a
	// binding name; the value specifies the source and optional version,
	// export, and transitive controls. Processed during ExpandCityPacks.
	Imports map[string]Import `toml:"imports,omitempty"`
	// Agents lists all configured agents in this city.
	Agents []Agent `toml:"agent"`
	// NamedSessions lists canonical alias-backed sessions built from
	// reusable agent templates.
	NamedSessions []NamedSession `toml:"named_session,omitempty"`
	// Rigs lists external projects registered in the city.
	Rigs []Rig `toml:"rigs,omitempty"`
	// Patches holds targeted modifications applied after fragment merge.
	Patches Patches `toml:"patches,omitempty"`
	// Beads configures the bead store backend.
	Beads BeadsConfig `toml:"beads,omitempty"`
	// Session configures the session provider backend.
	Session SessionConfig `toml:"session,omitempty"`
	// Mail configures the mail provider backend.
	Mail MailConfig `toml:"mail,omitempty"`
	// Events configures the events provider backend.
	Events EventsConfig `toml:"events,omitempty"`
	// Dolt configures optional dolt server connection overrides.
	Dolt DoltConfig `toml:"dolt,omitempty"`
	// Formulas configures formula directory settings.
	Formulas FormulasConfig `toml:"formulas,omitempty"`
	// Daemon configures controller daemon settings.
	Daemon DaemonConfig `toml:"daemon,omitempty"`
	// Orders configures order settings (skip list).
	Orders OrdersConfig `toml:"orders,omitempty"`
	// API configures the optional HTTP API server.
	API APIConfig `toml:"api,omitempty"`
	// ChatSessions configures chat session behavior (auto-suspend).
	ChatSessions ChatSessionsConfig `toml:"chat_sessions,omitempty"`
	// SessionSleep configures idle sleep policy defaults for managed sessions.
	SessionSleep SessionSleepConfig `toml:"session_sleep,omitempty"`
	// Convergence configures convergence loop limits.
	Convergence ConvergenceConfig `toml:"convergence,omitempty"`
	// Services declares workspace-owned HTTP services mounted on the
	// controller edge under /svc/{name}.
	Services []Service `toml:"service,omitempty"`
	// AgentDefaults provides city-level defaults for agents that don't
	// override them (canonical TOML key: agent_defaults). The runtime
	// currently applies default_sling_formula and append_fragments; the
	// attachment-list fields remain tombstones, and the other fields are
	// parsed/composed but not yet inherited automatically.
	AgentDefaults AgentDefaults `toml:"agent_defaults,omitempty"`
	// AgentsDefaults is a temporary compatibility alias for [agent_defaults].
	// Parse/load normalize it into AgentDefaults and prefer [agent_defaults]
	// when both tables are present.
	AgentsDefaults AgentDefaults `toml:"agents,omitempty" jsonschema:"-"`
	// LoadWarnings accumulates non-fatal warnings discovered while expanding
	// imported packs so LoadWithIncludes can surface them through provenance.
	// Runtime-only — not persisted to TOML or JSON.
	LoadWarnings []string `toml:"-" json:"-"`
	// ResolvedWorkspaceName is the effective city name derived from the
	// config file path when workspace.name is omitted. Runtime-only.
	ResolvedWorkspaceName string `toml:"-" json:"-"`
	// ResolvedWorkspacePrefix is the effective HQ prefix after applying site
	// binding and declared config. Runtime-only.
	ResolvedWorkspacePrefix string `toml:"-" json:"-"`

	// FormulaLayers holds the resolved formula directories per scope.
	// Populated during pack expansion in LoadWithIncludes. Not from TOML.
	FormulaLayers FormulaLayers `toml:"-" json:"-"`
	// ScriptLayers holds the resolved script directories per scope.
	// Populated during pack expansion in LoadWithIncludes. Not from TOML.
	ScriptLayers ScriptLayers `toml:"-" json:"-"`
	// PackDirs is the ordered, deduplicated list of pack directories
	// from all loaded city packs (includes resolved). Consumers derive
	// resource-specific search paths by scanning subdirectories:
	//   prompts/shared/  — shared prompt templates
	//   formulas/        — formula definitions
	// Populated during pack expansion. Not from TOML.
	PackDirs []string `toml:"-" json:"-"`
	// PackGraphOnlyDirs is the city pack closure rooted at workspace.includes,
	// including nested pack.includes and nested imports reached from those
	// packs, ordered low→high precedence for MCP resolution.
	// Runtime-only — not persisted to TOML or JSON.
	PackGraphOnlyDirs []string `toml:"-" json:"-"`
	// ExplicitImportPackDirs is the ordered low→high city-level explicit-import
	// pack closure used by MCP resolution. Runtime-only.
	ExplicitImportPackDirs []string `toml:"-" json:"-"`
	// ImplicitImportPackDirs is the ordered low→high city-level non-bootstrap
	// implicit-import closure used by MCP resolution. Runtime-only.
	ImplicitImportPackDirs []string `toml:"-" json:"-"`
	// BootstrapImportPackDirs is the ordered low→high bootstrap implicit-import
	// closure used by MCP resolution. Runtime-only.
	BootstrapImportPackDirs []string `toml:"-" json:"-"`
	// RigPackDirs maps rig name to its ordered pack directories.
	// Used when rig packs differ from city packs.
	// Populated during pack expansion. Not from TOML.
	RigPackDirs map[string][]string `toml:"-" json:"-"`
	// RigPackGraphOnlyDirs maps rig name to the rig's pack closure rooted at
	// rig.includes, including nested pack.includes and nested imports reached
	// from those packs, ordered low→high precedence for MCP resolution.
	// Runtime-only.
	RigPackGraphOnlyDirs map[string][]string `toml:"-" json:"-"`
	// RigImportPackDirs maps rig name to the rig's explicit-import closure,
	// ordered low→high precedence for MCP resolution. Runtime-only.
	RigImportPackDirs map[string][]string `toml:"-" json:"-"`
	// PackOverlayDirs is the ordered list of overlay/ directories
	// from all loaded city packs. Contents are copied to each agent's
	// workdir during startup (before the agent's own OverlayDir).
	// Populated during pack expansion. Not from TOML.
	PackOverlayDirs []string `toml:"-" json:"-"`
	// RigOverlayDirs maps rig name to its ordered overlay directories
	// from rig packs. Merged with PackOverlayDirs during agent build.
	// Populated during pack expansion. Not from TOML.
	RigOverlayDirs map[string][]string `toml:"-" json:"-"`
	// PackGlobals holds resolved [global] sections from city-level packs.
	// City-level globals apply to ALL agents. Populated during pack expansion.
	PackGlobals []ResolvedPackGlobal `toml:"-" json:"-"`
	// RigPackGlobals maps rig name to resolved [global] sections from
	// rig-level packs. Rig globals apply only to that rig's agents.
	RigPackGlobals map[string][]ResolvedPackGlobal `toml:"-" json:"-"`
	// PackScriptDirs is the ordered list of scripts/ directories from
	// city packs. Populated during pack expansion. Not from TOML.
	PackScriptDirs []string `toml:"-" json:"-"`
	// RigScriptDirs maps rig name to its ordered scripts/ directories
	// from rig packs. Populated during pack expansion. Not from TOML.
	RigScriptDirs map[string][]string `toml:"-" json:"-"`
	// PackCommands holds convention-discovered pack commands composed
	// during city expansion. Runtime-only.
	PackCommands []DiscoveredCommand `toml:"-" json:"-"`
	// PackDoctors holds convention-discovered pack doctor checks composed
	// during city and rig expansion. Runtime-only.
	PackDoctors []DiscoveredDoctor `toml:"-" json:"-"`
	// PackSkills holds binding-qualified shared skill catalogs composed
	// from city-level imported packs. Runtime-only.
	PackSkills []DiscoveredSkillCatalog `toml:"-" json:"-"`
	// PackSkillsDir holds the current city pack's shared skills catalog root.
	// Runtime-only — not persisted to TOML or JSON.
	PackSkillsDir string `toml:"-" json:"-"`
	// PackMCPDir holds the current city pack's shared MCP catalog root.
	// Runtime-only — not persisted to TOML or JSON.
	PackMCPDir string `toml:"-" json:"-"`
	// RigPackSkills maps rig name to the binding-qualified shared skill
	// catalogs composed from that rig's imports. Runtime-only.
	RigPackSkills map[string][]DiscoveredSkillCatalog `toml:"-" json:"-"`
	// ImplicitImportBindings records which city-level import bindings were
	// injected from ~/.gc/implicit-import.toml. Runtime-only.
	ImplicitImportBindings map[string]bool `toml:"-" json:"-"`
	// BootstrapImportBindings records which implicit-import bindings are
	// bootstrap-managed. Runtime-only.
	BootstrapImportBindings map[string]bool `toml:"-" json:"-"`
	// ExplicitImportMCPBindings records the city-level explicit-import binding
	// that currently owns each MCP pack dir after precedence flattening.
	// Runtime-only.
	ExplicitImportMCPBindings map[string]string `toml:"-" json:"-"`
	// ImplicitImportMCPBindings records the city-level non-bootstrap implicit
	// binding that currently owns each MCP pack dir after precedence
	// flattening. Runtime-only.
	ImplicitImportMCPBindings map[string]string `toml:"-" json:"-"`
	// BootstrapImportMCPBindings records the bootstrap implicit-import binding
	// that currently owns each MCP pack dir after precedence flattening.
	// Runtime-only.
	BootstrapImportMCPBindings map[string]string `toml:"-" json:"-"`
	// RigImportMCPBindings records, per rig, the rig-import binding that
	// currently owns each MCP pack dir after precedence flattening.
	// Runtime-only.
	RigImportMCPBindings map[string]map[string]string `toml:"-" json:"-"`
	// DefaultRigImports holds the canonical [defaults.rig.imports] entries
	// declared by the city root pack. Runtime-only.
	DefaultRigImports map[string]Import `toml:"-" json:"-"`
	// DefaultRigImportOrder preserves declaration order for
	// [defaults.rig.imports]. Runtime-only.
	DefaultRigImportOrder []string `toml:"-" json:"-"`
	// ResolvedProviders is the eager-resolution cache populated by
	// BuildResolvedProviderCache after compose + patch. Runtime-only.
	ResolvedProviders map[string]ResolvedProvider `toml:"-" json:"-"`
}

// NamedSession defines a canonical persistent session backed by an agent
// template. Unlike Agent, it does not carry behavior itself; it only
// declares runtime identity and controller policy.
type NamedSession struct {
	// Name is the configured public session identity. When omitted, Template
	// remains the compatibility identity.
	Name string `toml:"name,omitempty"`
	// Template is the referenced agent template name. Root declarations may
	// target imported PackV2 agents via "binding.agent".
	Template string `toml:"template" jsonschema:"required"`
	// Scope defines where this named session is instantiated in pack
	// expansion: "city" (one per city) or "rig" (one per rig).
	Scope string `toml:"scope,omitempty" jsonschema:"enum=city,enum=rig"`
	// Dir is the identity prefix for rig-scoped named sessions after pack
	// expansion. Empty means city-scoped.
	Dir string `toml:"dir,omitempty"`
	// Mode controls controller behavior for this named session.
	// "on_demand" (default): reserve identity and materialize when work or
	// an explicit reference requires it.
	// "always": keep the canonical session controller-managed.
	Mode string `toml:"mode,omitempty" jsonschema:"enum=on_demand,enum=always"`
	// SourceDir is the directory where this named session's config was
	// defined. Set during pack/fragment loading; empty for inline config.
	// Runtime-only — not persisted to TOML or JSON.
	SourceDir string `toml:"-" json:"-"`
	// BindingName is the import binding that brought this named session
	// into scope. Set during V2 import expansion. Empty for the city
	// pack's own sessions.
	// Runtime-only — not persisted to TOML or JSON.
	BindingName string `toml:"-" json:"-"`
}

// QualifiedName returns the canonical identity of the named session.
// For V2 sessions with a binding, the public identity is qualified as
// "binding.name" or "binding.template".
func (s *NamedSession) QualifiedName() string {
	if s == nil {
		return ""
	}
	identity := s.IdentityName()
	if s.Dir == "" {
		return identity
	}
	return s.Dir + "/" + identity
}

// IdentityName returns the unqualified configured public session identity.
func (s *NamedSession) IdentityName() string {
	if s == nil {
		return ""
	}
	identity := s.Name
	if identity == "" {
		identity = s.Template
	}
	if s.BindingName != "" {
		return s.BindingName + "." + identity
	}
	return identity
}

// TemplateQualifiedName returns the canonical backing agent config identity.
func (s *NamedSession) TemplateQualifiedName() string {
	if s == nil {
		return ""
	}
	tmpl := s.Template
	if s.BindingName != "" {
		tmpl = s.BindingName + "." + s.Template
	}
	if s.Dir == "" {
		return tmpl
	}
	return s.Dir + "/" + tmpl
}

// ModeOrDefault returns the normalized controller mode.
func (s *NamedSession) ModeOrDefault() string {
	if s == nil || s.Mode == "" {
		return "on_demand"
	}
	return s.Mode
}

// FormulaLayers holds resolved formula directories for symlink materialization.
// Each slice is ordered lowest→highest priority; later entries shadow earlier
// ones by filename.
type FormulaLayers struct {
	// City holds formula dirs for city-scoped agents (no rig).
	// Typically [city-topo-formulas, city-local-formulas].
	City []string
	// Rigs maps rig name → formula dir layers.
	// Typically [city-topo, city-local, rig-topo, rig-local].
	Rigs map[string][]string
}

// SearchPaths returns the ordered formula search directories for a rig.
// Falls back to city-level layers if no rig-specific layers exist.
// Returns nil if no formula layers are configured.
func (fl FormulaLayers) SearchPaths(rigName string) []string {
	if rigName != "" {
		if paths, ok := fl.Rigs[rigName]; ok && len(paths) > 0 {
			return paths
		}
	}
	return fl.City
}

// ScriptLayers holds resolved script directories for symlink materialization.
// Each slice is ordered lowest→highest priority; later entries shadow earlier
// ones by relative path.
type ScriptLayers struct {
	// City holds script dirs for city-scoped materialization.
	City []string
	// Rigs maps rig name → script dir layers.
	Rigs map[string][]string
}

// Rig defines an external project registered in the city.
type Rig struct {
	// Name is the unique identifier for this rig.
	Name string `toml:"name" jsonschema:"required"`
	// Path is the absolute filesystem path to the rig's repository.
	Path string `toml:"path,omitempty"`
	// Prefix overrides the auto-derived bead ID prefix for this rig.
	Prefix string `toml:"prefix,omitempty"`
	// Suspended prevents the reconciler from spawning agents in this rig. Toggle with gc rig suspend/resume.
	Suspended bool `toml:"suspended,omitempty"`
	// FormulasDir is a rig-local formula directory (Layer 4). Overrides
	// pack formulas for this rig by filename.
	// Relative paths resolve against the city directory.
	FormulasDir string `toml:"formulas_dir,omitempty"`
	// Includes lists pack directories or URLs for this rig (V1 mechanism).
	// Each entry is a local path, a git source//sub#ref URL, or a GitHub tree URL.
	Includes []string `toml:"includes,omitempty"`
	// Imports defines named pack imports for this rig (V2 mechanism).
	// Each key is a binding name; agents from these imports get qualified
	// names like "rigName/bindingName.agentName".
	Imports map[string]Import `toml:"imports,omitempty"`
	// MaxActiveSessions is the rig-level cap on total concurrent sessions across
	// all agents in this rig. Nil means inherit from workspace (or unlimited).
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// Overrides are per-agent patches applied after pack expansion.
	// V2 renames this to "patches" for consistency with [[patches.agent]].
	// Both TOML keys are accepted during migration.
	Overrides []AgentOverride `toml:"overrides,omitempty"`
	// Patches is the V2 name for rig-level agent overrides. Takes
	// precedence over Overrides if both are set.
	RigPatches []AgentOverride `toml:"patches,omitempty"`
	// DefaultSlingTarget is the agent qualified name used when gc sling is
	// invoked with only a bead ID (no explicit target). Resolved via
	// resolveAgentIdentity. Example: "rig/polecat"
	DefaultSlingTarget string `toml:"default_sling_target,omitempty"`
	// SessionSleep overrides workspace-level idle sleep defaults for agents in
	// this rig.
	SessionSleep SessionSleepConfig `toml:"session_sleep,omitempty"`
	// DoltHost overrides the city-level Dolt host for this rig's beads.
	// Use when the rig's database lives on a different Dolt server (e.g.,
	// shared from another city).
	DoltHost string `toml:"dolt_host,omitempty"`
	// DoltPort overrides the city-level Dolt port for this rig's beads.
	// When set, controller commands (scale_check, work_query) prefix their
	// shell invocations with BEADS_DOLT_SERVER_PORT=<port> so bd connects to the
	// correct server instead of the city-level default.
	DoltPort string `toml:"dolt_port,omitempty"`
}

// AgentOverride modifies a pack-stamped agent for a specific rig.
// Uses pointer fields to distinguish "not set" from "set to zero value."
type AgentOverride struct {
	// Agent is the name of the pack agent to override (required).
	Agent string `toml:"agent" jsonschema:"required"`
	// Dir overrides the stamped dir (default: rig name).
	Dir *string `toml:"dir,omitempty"`
	// WorkDir overrides the agent's working directory without changing
	// its qualified identity or rig association.
	WorkDir *string `toml:"work_dir,omitempty"`
	// Scope overrides the agent's scope ("city" or "rig").
	Scope *string `toml:"scope,omitempty"`
	// Suspended sets the agent's suspended state.
	Suspended *bool `toml:"suspended,omitempty"`
	// Pool overrides legacy [pool] fields that map to session scaling.
	Pool *PoolOverride `toml:"pool,omitempty"`
	// Env adds or overrides environment variables.
	Env map[string]string `toml:"env,omitempty"`
	// EnvRemove lists env var keys to remove.
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
	// IdleTimeout overrides the idle timeout duration string (e.g., "30s", "5m", "1h").
	IdleTimeout *string `toml:"idle_timeout,omitempty"`
	// SleepAfterIdle overrides idle sleep policy for this agent. Accepts a
	// duration string (e.g., "30s") or "off".
	SleepAfterIdle *string `toml:"sleep_after_idle,omitempty"`
	// InstallAgentHooks overrides the agent's install_agent_hooks list.
	InstallAgentHooks []string `toml:"install_agent_hooks,omitempty"`
	// Skills is a tombstone field retained for v0.15.1 backwards
	// compatibility. Parsed for migration visibility, but attachment-list
	// fields are accepted but ignored by the active materializer.
	Skills []string `toml:"skills,omitempty"`
	// MCP is a tombstone field retained for v0.15.1 backwards compatibility.
	// Parsed for migration visibility, but attachment-list fields are
	// accepted but ignored by the active materializer.
	MCP []string `toml:"mcp,omitempty"`
	// HooksInstalled overrides automatic hook detection.
	HooksInstalled *bool `toml:"hooks_installed,omitempty"`
	// InjectAssignedSkills overrides Agent.InjectAssignedSkills
	// (see that field for semantics).
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
	// PreStartAppend appends commands to the agent's pre_start list
	// (instead of replacing). Applied after PreStart if both are set.
	PreStartAppend []string `toml:"pre_start_append,omitempty"`
	// SessionSetupAppend appends commands to the agent's session_setup list.
	SessionSetupAppend []string `toml:"session_setup_append,omitempty"`
	// SessionLiveAppend appends commands to the agent's session_live list.
	SessionLiveAppend []string `toml:"session_live_append,omitempty"`
	// InstallAgentHooksAppend appends to the agent's install_agent_hooks list.
	InstallAgentHooksAppend []string `toml:"install_agent_hooks_append,omitempty"`
	// SkillsAppend is a tombstone field retained for v0.15.1 backwards
	// compatibility. Parsed for migration visibility, but attachment-list
	// fields are accepted but ignored by the active materializer.
	SkillsAppend []string `toml:"skills_append,omitempty"`
	// MCPAppend is a tombstone field retained for v0.15.1 backwards
	// compatibility. Parsed for migration visibility, but attachment-list
	// fields are accepted but ignored by the active materializer.
	MCPAppend []string `toml:"mcp_append,omitempty"`
	// Attach overrides the agent's attach setting.
	Attach *bool `toml:"attach,omitempty"`
	// DependsOn overrides the agent's dependency list.
	DependsOn []string `toml:"depends_on,omitempty"`
	// ResumeCommand overrides the agent's resume_command template.
	ResumeCommand *string `toml:"resume_command,omitempty"`
	// WakeMode overrides the agent's wake mode ("resume" or "fresh").
	WakeMode *string `toml:"wake_mode,omitempty" jsonschema:"enum=resume,enum=fresh"`
	// InjectFragmentsAppend appends to the agent's inject_fragments list.
	InjectFragmentsAppend []string `toml:"inject_fragments_append,omitempty"`
	// MaxActiveSessions overrides the agent-level cap on concurrent sessions.
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// MinActiveSessions overrides the minimum number of sessions to keep alive.
	MinActiveSessions *int `toml:"min_active_sessions,omitempty"`
	// ScaleCheck overrides the shell command whose output determines desired session count.
	ScaleCheck *string `toml:"scale_check,omitempty"`
	// OptionDefaults adds or overrides provider option defaults for this agent.
	// Keys are option keys, values are choice values. Merges additively
	// (override keys win over existing agent keys).
	// Example: option_defaults = { model = "sonnet" }
	OptionDefaults map[string]string `toml:"option_defaults,omitempty"`
}

// PackSource defines a remote pack repository.
// Referenced by name in rig pack fields and fetched into the cache.
type PackSource struct {
	// Source is the git repository URL.
	Source string `toml:"source" jsonschema:"required"`
	// Ref is the git ref to checkout (branch, tag, or commit). Defaults to HEAD.
	Ref string `toml:"ref,omitempty"`
	// Path is a subdirectory within the repo containing the pack files.
	Path string `toml:"path,omitempty"`
}

// Import defines a named import of another pack. This is the V2
// replacement for the flat `includes` list. Each import has a binding
// name (the TOML key), a source (local path or remote URL), and
// optional version/export/transitive controls.
type Import struct {
	// Source is the pack location: a local relative path (e.g.,
	// "./assets/imports/gastown") or a remote URL (e.g.,
	// "github.com/gastownhall/gastown"). Local paths have no version.
	Source string `toml:"source" jsonschema:"required"`
	// Version is a semver constraint for remote imports (e.g., "^1.2").
	// Empty for local paths. "sha:<hex>" for commit pinning.
	Version string `toml:"version,omitempty"`
	// Export re-exports this import's contents into the parent pack's
	// namespace. Consumers of the parent get this import's agents
	// flattened under the parent's binding name.
	Export bool `toml:"export,omitempty"`
	// Transitive controls whether this import's own imports are visible
	// to the consumer. Defaults to true (transitive). Set to false to
	// suppress transitive resolution for this specific import.
	Transitive *bool `toml:"transitive,omitempty"`
	// Shadow controls shadow warnings when the importer defines an agent
	// with the same name as one from this import. "warn" (default) emits
	// a warning; "silent" suppresses it.
	Shadow string `toml:"shadow,omitempty" jsonschema:"enum=warn,enum=silent"`
}

// PackMeta holds metadata from a pack's [pack] header.
type PackMeta struct {
	// Name is the pack's identifier.
	Name string `toml:"name" jsonschema:"required"`
	// Version is a semver-style version string.
	Version string `toml:"version"`
	// Schema is the pack format version (currently 1).
	Schema int `toml:"schema" jsonschema:"required"`
	// RequiresGC is an optional minimum gc version requirement.
	RequiresGC string `toml:"requires_gc,omitempty"`
	// Includes lists other packs to compose into this one (V1 mechanism).
	// Each entry is a local relative path (e.g. "../maintenance") or a
	// remote git URL (SSH or HTTPS) with optional //subpath and #ref.
	Includes []string `toml:"includes,omitempty"`
	// Requires declares agents that must exist in the expanded config
	// for this pack's formulas/orders to function. Validated
	// after all packs are expanded.
	Requires []PackRequirement `toml:"requires,omitempty"`
}

// ImportIsTransitive returns whether an Import should resolve
// transitively. Defaults to true if Transitive is nil.
func (imp *Import) ImportIsTransitive() bool {
	if imp.Transitive == nil {
		return true
	}
	return *imp.Transitive
}

// BoundImport preserves the user-visible binding name associated with an
// import when edit paths need ordered root-pack defaults.
type BoundImport struct {
	Binding string
	Import  Import
}

// PackRequirement declares an agent that must exist in the
// expanded config for this pack's formulas/orders to function.
type PackRequirement struct {
	// Scope is the agent scope: "city" or "rig".
	Scope string `toml:"scope" jsonschema:"required,enum=city,enum=rig"`
	// Agent is the name of the required agent.
	Agent string `toml:"agent" jsonschema:"required"`
}

// PackDoctorEntry declares a diagnostic check shipped with a pack.
// The script is executed by gc doctor to validate pack-specific
// prerequisites (binaries, permissions, directory structures, etc.).
type PackDoctorEntry struct {
	// Name is a short identifier for the check (e.g. "check-binaries").
	// The full check name shown in doctor output is "<pack>:<name>".
	Name string `toml:"name" jsonschema:"required"`
	// Script is the path to the check script, relative to the pack
	// directory. The script must be executable and follow the exit-code
	// protocol: 0=OK, 1=Warning, 2=Error. First line of stdout is the
	// message; remaining lines are details (shown in verbose mode).
	Script string `toml:"script" jsonschema:"required"`
	// Description is an optional human-readable description of the check.
	Description string `toml:"description,omitempty"`
	// Fix is an optional path to a remediation script, relative to the pack
	// directory. When set, the check opts into `gc doctor --fix`.
	Fix string `toml:"fix,omitempty"`
}

// PackCommandEntry declares a CLI subcommand provided by a pack.
// Pack commands appear as gc <pack-name> <command-name> and let packs
// ship operational tooling alongside orchestration config.
type PackCommandEntry struct {
	// Name is the subcommand name (e.g. "status", "audit").
	Name string `toml:"name" jsonschema:"required"`
	// Description is a short one-line description shown in help listings.
	Description string `toml:"description" jsonschema:"required"`
	// LongDescription is a path (relative to pack dir) to a text file
	// with the full help text shown by gc <pack> <command> --help.
	LongDescription string `toml:"long_description" jsonschema:"required"`
	// Script is the path to the script (relative to pack dir).
	// Supports Go text/template variables: {{.CityRoot}}, {{.ConfigDir}}, etc.
	Script string `toml:"script" jsonschema:"required"`
}

// PackGlobal defines commands a pack applies to all agents in scope.
// Parsed from the [global] section in pack.toml.
type PackGlobal struct {
	SessionLive []string `toml:"session_live,omitempty"`
}

// ResolvedPackGlobal is a PackGlobal with {{.ConfigDir}} pre-resolved
// to the pack's concrete cache/directory path. Other template vars
// ({{.Session}}, {{.Agent}}, etc.) remain for per-agent expansion.
type ResolvedPackGlobal struct {
	SessionLive []string
	PackName    string
}

// EffectivePrefix returns the bead ID prefix for this rig. Uses the
// explicit Prefix if set, otherwise derives one from the Name.
func (r *Rig) EffectivePrefix() string {
	if r.Prefix != "" {
		return r.Prefix
	}
	return DeriveBeadsPrefix(r.Name)
}

// EffectiveHQPrefix returns the bead ID prefix for the city's HQ store.
// Uses the effective site-bound prefix first, then the declared workspace
// Prefix, then derives one from the effective city name.
func EffectiveHQPrefix(cfg *City) string {
	if cfg == nil {
		return ""
	}
	if cfg.ResolvedWorkspacePrefix != "" {
		return cfg.ResolvedWorkspacePrefix
	}
	if cfg.Workspace.Prefix != "" {
		return cfg.Workspace.Prefix
	}
	return DeriveBeadsPrefix(cfg.EffectiveCityName())
}

// DeriveBeadsPrefix computes a short bead ID prefix from a rig/city name.
// Ported from gastown/internal/rig/manager.go:deriveBeadsPrefix.
//
// Algorithm:
//  1. Strip -py, -go suffixes
//  2. Split on - or _
//  3. If single word, try splitting compound word (camelCase, etc.)
//  4. If 2+ parts: first letter of each part
//  5. If 1 part and ≤3 chars: use as-is
//  6. If 1 part and >3 chars: first 2 chars
func DeriveBeadsPrefix(name string) string {
	name = strings.TrimSuffix(name, "-py")
	name = strings.TrimSuffix(name, "-go")

	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_'
	})

	if len(parts) == 1 {
		parts = splitCompoundWord(parts[0])
	}

	if len(parts) >= 2 {
		var prefix strings.Builder
		for _, p := range parts {
			if len(p) > 0 {
				prefix.WriteByte(p[0])
			}
		}
		return strings.ToLower(prefix.String())
	}

	if len(name) <= 3 {
		return strings.ToLower(name)
	}
	return strings.ToLower(name[:2])
}

// splitCompoundWord splits a camelCase or PascalCase word into parts.
// e.g. "myFrontend" → ["my", "Frontend"], "GasCity" → ["Gas", "City"]
func splitCompoundWord(word string) []string {
	if word == "" {
		return []string{word}
	}
	var parts []string
	start := 0
	runes := []rune(word)
	for i := 1; i < len(runes); i++ {
		if unicode.IsUpper(runes[i]) && !unicode.IsUpper(runes[i-1]) {
			parts = append(parts, string(runes[start:i]))
			start = i
		}
	}
	parts = append(parts, string(runes[start:]))
	if len(parts) <= 1 {
		return []string{word}
	}
	return parts
}

// Workspace holds city-level metadata and optional defaults that apply
// to all agents unless overridden per-agent.
type Workspace struct {
	// Name is the human-readable name for this city.
	Name string `toml:"name" jsonschema:"required"`
	// Prefix overrides the auto-derived HQ bead ID prefix. When empty,
	// the prefix is derived from the city Name via DeriveBeadsPrefix.
	Prefix string `toml:"prefix,omitempty"`
	// Provider is the default provider name used by agents that don't specify one.
	Provider string `toml:"provider,omitempty"`
	// StartCommand overrides the provider's command for all agents.
	StartCommand string `toml:"start_command,omitempty"`
	// Suspended controls whether the city is suspended. When true, all
	// agents are effectively suspended: the reconciler won't spawn them,
	// and gc hook/prime return empty. Inherits downward — individual
	// agent/rig suspended fields are checked independently.
	Suspended bool `toml:"suspended,omitempty"`
	// MaxActiveSessions is the workspace-level cap on total concurrent sessions.
	// Nil means unlimited. Agents and rigs inherit this if they don't set their own.
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// SessionTemplate is a template string supporting placeholders: {{.City}},
	// {{.Agent}} (sanitized), {{.Dir}}, {{.Name}}. Controls tmux session naming.
	// Default (empty): "{{.Agent}}" — just the sanitized agent name. Per-city
	// tmux socket isolation makes a city prefix unnecessary.
	SessionTemplate string `toml:"session_template,omitempty"`
	// InstallAgentHooks lists provider names whose hooks should be installed
	// into agent working directories. Agent-level overrides workspace-level
	// (replace, not additive). Supported: "claude", "codex", "gemini",
	// "opencode", "copilot", "cursor", "pi", "omp".
	InstallAgentHooks []string `toml:"install_agent_hooks,omitempty"`
	// GlobalFragments lists named template fragments injected into every
	// agent's rendered prompt. Applied before per-agent InjectFragments.
	// Each name must match a {{ define "name" }} block from a pack's
	// prompts/shared/ directory.
	GlobalFragments []string `toml:"global_fragments,omitempty"`
	// Includes lists pack directories or URLs to compose into this
	// workspace. Replaces the older pack/packs fields. Each entry
	// is a local path, a git source//sub#ref URL, or a GitHub tree URL.
	Includes []string `toml:"includes,omitempty"`
	// DefaultRigIncludes lists pack directories applied to new rigs when
	// "gc rig add" is called without --include. Allows cities to define
	// a default pack for all rigs.
	DefaultRigIncludes []string `toml:"default_rig_includes,omitempty"`
}

// BeadsConfig holds bead store settings.
type BeadsConfig struct {
	// Provider selects the bead store backend: "bd" (default), "file",
	// or "exec:<script>" for a user-supplied script.
	Provider string `toml:"provider,omitempty" jsonschema:"default=bd"`
}

// SessionConfig holds session provider settings.
type SessionConfig struct {
	// Provider selects the session backend: "fake", "fail", "subprocess",
	// "acp", "exec:<script>", "k8s", or "" (default: tmux).
	Provider string `toml:"provider,omitempty"`
	// K8s holds Kubernetes-specific settings for the native K8s provider.
	K8s K8sConfig `toml:"k8s,omitempty"`
	// ACP holds settings for the ACP (Agent Client Protocol) session provider.
	ACP ACPSessionConfig `toml:"acp,omitempty"`
	// SetupTimeout is the per-command/script timeout for session setup and
	// pre_start commands. Duration string (e.g., "10s", "30s"). Defaults to "10s".
	SetupTimeout string `toml:"setup_timeout,omitempty" jsonschema:"default=10s"`
	// NudgeReadyTimeout is how long to wait for the agent to be ready before
	// sending nudge text. Duration string. Defaults to "10s".
	NudgeReadyTimeout string `toml:"nudge_ready_timeout,omitempty" jsonschema:"default=10s"`
	// NudgeRetryInterval is the retry interval between nudge readiness polls.
	// Duration string. Defaults to "500ms".
	NudgeRetryInterval string `toml:"nudge_retry_interval,omitempty" jsonschema:"default=500ms"`
	// NudgeLockTimeout is how long to wait to acquire the per-session nudge lock.
	// Duration string. Defaults to "30s".
	NudgeLockTimeout string `toml:"nudge_lock_timeout,omitempty" jsonschema:"default=30s"`
	// DebounceMs is the default debounce interval in milliseconds for send-keys.
	// Defaults to 500.
	DebounceMs *int `toml:"debounce_ms,omitempty" jsonschema:"default=500"`
	// DisplayMs is the default display duration in milliseconds for status messages.
	// Defaults to 5000.
	DisplayMs *int `toml:"display_ms,omitempty" jsonschema:"default=5000"`
	// StartupTimeout is how long to wait for each agent's Start() call before
	// treating it as failed. Duration string (e.g., "60s", "2m"). Defaults to "60s".
	StartupTimeout string `toml:"startup_timeout,omitempty" jsonschema:"default=60s"`
	// Socket specifies the tmux socket name for per-city isolation.
	// When set, all tmux commands use "tmux -L <socket>" to connect to
	// a dedicated server. When empty, defaults to the city name
	// (workspace.name) — giving every city its own tmux server
	// automatically. Set explicitly to override.
	Socket string `toml:"socket,omitempty"`
	// RemoteMatch is a substring pattern for the hybrid provider to route
	// sessions to the remote (K8s) backend. Sessions whose names contain
	// this pattern go to K8s; all others stay local (tmux).
	// Overridden by the GC_HYBRID_REMOTE_MATCH env var if set.
	RemoteMatch string `toml:"remote_match,omitempty"`
}

// SetupTimeoutDuration returns the setup timeout as a time.Duration.
// Defaults to 10s if empty or unparseable.
func (s *SessionConfig) SetupTimeoutDuration() time.Duration {
	if s.SetupTimeout == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(s.SetupTimeout)
	if err != nil {
		return 10 * time.Second
	}
	return d
}

// NudgeReadyTimeoutDuration returns the nudge ready timeout as a time.Duration.
// Defaults to 10s if empty or unparseable.
func (s *SessionConfig) NudgeReadyTimeoutDuration() time.Duration {
	if s.NudgeReadyTimeout == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(s.NudgeReadyTimeout)
	if err != nil {
		return 10 * time.Second
	}
	return d
}

// NudgeRetryIntervalDuration returns the nudge retry interval as a time.Duration.
// Defaults to 500ms if empty or unparseable.
func (s *SessionConfig) NudgeRetryIntervalDuration() time.Duration {
	if s.NudgeRetryInterval == "" {
		return 500 * time.Millisecond
	}
	d, err := time.ParseDuration(s.NudgeRetryInterval)
	if err != nil {
		return 500 * time.Millisecond
	}
	return d
}

// NudgeLockTimeoutDuration returns the nudge lock timeout as a time.Duration.
// Defaults to 30s if empty or unparseable.
func (s *SessionConfig) NudgeLockTimeoutDuration() time.Duration {
	if s.NudgeLockTimeout == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(s.NudgeLockTimeout)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// StartupTimeoutDuration returns the startup timeout as a time.Duration.
// Defaults to 60s if empty or unparseable.
func (s *SessionConfig) StartupTimeoutDuration() time.Duration {
	if s.StartupTimeout == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(s.StartupTimeout)
	if err != nil {
		return 60 * time.Second
	}
	return d
}

// DebounceMsOrDefault returns the debounce interval in milliseconds.
// Defaults to 500 if nil.
func (s *SessionConfig) DebounceMsOrDefault() int {
	if s.DebounceMs == nil {
		return 500
	}
	return *s.DebounceMs
}

// DisplayMsOrDefault returns the display duration in milliseconds.
// Defaults to 5000 if nil.
func (s *SessionConfig) DisplayMsOrDefault() int {
	if s.DisplayMs == nil {
		return 5000
	}
	return *s.DisplayMs
}

// ACPSessionConfig holds settings for the ACP session provider.
type ACPSessionConfig struct {
	// HandshakeTimeout is how long to wait for the ACP handshake to complete.
	// Duration string (e.g., "30s", "1m"). Defaults to "30s".
	HandshakeTimeout string `toml:"handshake_timeout,omitempty" jsonschema:"default=30s"`
	// NudgeBusyTimeout is how long to wait for an agent to become idle
	// before sending a new prompt. Duration string. Defaults to "60s".
	NudgeBusyTimeout string `toml:"nudge_busy_timeout,omitempty" jsonschema:"default=60s"`
	// OutputBufferLines is the number of output lines to keep in the
	// circular buffer for Peek. Defaults to 1000.
	OutputBufferLines int `toml:"output_buffer_lines,omitempty" jsonschema:"default=1000"`
}

// HandshakeTimeoutDuration returns the handshake timeout as a time.Duration.
// Defaults to 30s if empty or unparseable.
func (a *ACPSessionConfig) HandshakeTimeoutDuration() time.Duration {
	if a.HandshakeTimeout == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(a.HandshakeTimeout)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// NudgeBusyTimeoutDuration returns the nudge busy timeout as a time.Duration.
// Defaults to 60s if empty or unparseable.
func (a *ACPSessionConfig) NudgeBusyTimeoutDuration() time.Duration {
	if a.NudgeBusyTimeout == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(a.NudgeBusyTimeout)
	if err != nil {
		return 60 * time.Second
	}
	return d
}

// OutputBufferLinesOrDefault returns the output buffer line count.
// Defaults to 1000 if zero.
func (a *ACPSessionConfig) OutputBufferLinesOrDefault() int {
	if a.OutputBufferLines <= 0 {
		return 1000
	}
	return a.OutputBufferLines
}

// K8sConfig holds native K8s session provider settings.
// Env vars (GC_K8S_*) override TOML values.
type K8sConfig struct {
	// Namespace is the K8s namespace for agent pods. Default: "gc".
	Namespace string `toml:"namespace,omitempty" jsonschema:"default=gc"`
	// Image is the container image for agents.
	Image string `toml:"image,omitempty"`
	// Context is the kubectl/kubeconfig context. Default: current.
	Context string `toml:"context,omitempty"`
	// CPURequest is the pod CPU request. Default: "500m".
	CPURequest string `toml:"cpu_request,omitempty" jsonschema:"default=500m"`
	// MemRequest is the pod memory request. Default: "1Gi".
	MemRequest string `toml:"mem_request,omitempty" jsonschema:"default=1Gi"`
	// CPULimit is the pod CPU limit. Default: "2".
	CPULimit string `toml:"cpu_limit,omitempty" jsonschema:"default=2"`
	// MemLimit is the pod memory limit. Default: "4Gi".
	MemLimit string `toml:"mem_limit,omitempty" jsonschema:"default=4Gi"`
	// Prebaked skips init container staging and EmptyDir volumes when true.
	// Use with images built by `gc build-image` that have city content baked in.
	Prebaked bool `toml:"prebaked,omitempty"`
}

// MailConfig holds mail provider settings.
type MailConfig struct {
	// Provider selects the mail backend: "fake", "fail",
	// "exec:<script>", or "" (default: beadmail).
	Provider string `toml:"provider,omitempty"`
}

// EventsConfig holds events provider settings.
type EventsConfig struct {
	// Provider selects the events backend: "fake", "fail",
	// "exec:<script>", or "" (default: file-backed JSONL).
	Provider string `toml:"provider,omitempty"`
}

// DoltConfig holds optional dolt server overrides.
// When present in city.toml, these override the defaults.
type DoltConfig struct {
	// Port is the dolt server port. 0 means use ephemeral port allocation
	// (hashed from city path). Set explicitly to override.
	Port int `toml:"port,omitempty" jsonschema:"default=0"`
	// Host is the dolt server hostname. Defaults to localhost.
	Host string `toml:"host,omitempty" jsonschema:"default=localhost"`
}

// FormulasConfig holds formula directory settings.
type FormulasConfig struct {
	// Dir is the path to the formulas directory. Defaults to "formulas".
	Dir string `toml:"dir,omitempty" jsonschema:"default=formulas"`
}

// OrdersConfig holds order settings.
type OrdersConfig struct {
	// Skip lists order names to exclude from scanning.
	Skip []string `toml:"skip,omitempty"`
	// MaxTimeout is an operator hard cap on per-order timeouts.
	// No order gets more than this duration. Go duration string (e.g., "60s").
	// Empty means uncapped (no override).
	MaxTimeout string `toml:"max_timeout,omitempty"`
	// Overrides apply per-order field overrides after scanning.
	// Each override targets an order by name and optionally by rig.
	Overrides []OrderOverride `toml:"overrides,omitempty"`
}

// OrderOverride modifies a scanned order's scheduling fields.
// Uses pointer fields to distinguish "not set" from "set to zero value."
type OrderOverride struct {
	// Name is the order name to target (required).
	Name string `toml:"name" jsonschema:"required"`
	// Rig scopes the override to a specific rig's order.
	// Empty matches city-level orders.
	Rig string `toml:"rig,omitempty"`
	// Enabled overrides whether the order is active.
	Enabled *bool `toml:"enabled,omitempty"`
	// Trigger overrides the trigger type.
	Trigger *string `toml:"trigger,omitempty"`
	// Gate is a deprecated alias for Trigger accepted during the
	// gate->trigger migration. Parsed inputs are normalized to Trigger.
	Gate *string `toml:"gate,omitempty" jsonschema_extras:"deprecated=true"`
	// Interval overrides the cooldown interval. Go duration string.
	Interval *string `toml:"interval,omitempty"`
	// Schedule overrides the cron expression.
	Schedule *string `toml:"schedule,omitempty"`
	// Check overrides the condition trigger check command.
	Check *string `toml:"check,omitempty"`
	// On overrides the event trigger event type.
	On *string `toml:"on,omitempty"`
	// Pool overrides the target session config.
	Pool *string `toml:"pool,omitempty"`
	// Timeout overrides the per-order timeout. Go duration string.
	Timeout *string `toml:"timeout,omitempty"`
}

func (o *OrderOverride) normalizeLegacyAliases() {
	if o.Trigger == nil {
		o.Trigger = o.Gate
	}
	o.Gate = nil
}

func normalizeLegacyOrderOverrideAliases(cfg *City) {
	for i := range cfg.Orders.Overrides {
		cfg.Orders.Overrides[i].normalizeLegacyAliases()
	}
}

// MaxTimeoutDuration parses MaxTimeout as a Go duration.
// Returns 0 if unset or unparseable (meaning no cap).
func (c OrdersConfig) MaxTimeoutDuration() time.Duration {
	if c.MaxTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(c.MaxTimeout)
	if err != nil {
		return 0
	}
	return d
}

// DefaultAPIPort is the default TCP port for the API server.
const DefaultAPIPort = 9443

// APIConfig configures the HTTP API server.
// The API server starts by default on port 9443. Set port = 0 to disable.
type APIConfig struct {
	// Port is the TCP port to listen on. Defaults to 9443; 0 = disabled.
	Port int `toml:"port,omitempty"`
	// Bind is the address to bind the listener to. Defaults to "127.0.0.1".
	Bind string `toml:"bind,omitempty"`
	// AllowMutations overrides the default read-only behavior when bind is
	// non-localhost. Set to true in containerized environments where the API
	// must bind to 0.0.0.0 for health probes but mutations are still safe.
	AllowMutations bool `toml:"allow_mutations,omitempty"`
}

// BindOrDefault returns the bind address, defaulting to "127.0.0.1".
func (c APIConfig) BindOrDefault() string {
	if c.Bind == "" {
		return "127.0.0.1"
	}
	return c.Bind
}

// ChatSessionsConfig configures chat session behavior.
// Progressive activation: absent or empty = no auto-suspend.
type ChatSessionsConfig struct {
	// IdleTimeout is the duration after which a detached chat session
	// is auto-suspended. Duration string (e.g., "30m", "1h"). 0 = disabled.
	IdleTimeout string `toml:"idle_timeout,omitempty"`
}

// SessionSleepConfig configures default idle sleep policies by session class.
type SessionSleepConfig struct {
	// InteractiveResume applies to attachable sessions using wake_mode=resume.
	// Accepts a duration string or "off".
	InteractiveResume string `toml:"interactive_resume,omitempty"`
	// InteractiveFresh applies to attachable sessions using wake_mode=fresh.
	// Accepts a duration string or "off".
	InteractiveFresh string `toml:"interactive_fresh,omitempty"`
	// NonInteractive applies to sessions with attach=false. Accepts a duration
	// string or "off".
	NonInteractive string `toml:"noninteractive,omitempty"`
}

// IdleTimeoutDuration parses IdleTimeout, returning 0 if unset or invalid.
func (c ChatSessionsConfig) IdleTimeoutDuration() time.Duration {
	if c.IdleTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil {
		return 0
	}
	return d
}

// ConvergenceConfig holds convergence loop limits.
type ConvergenceConfig struct {
	// MaxPerAgent is the maximum number of active convergence loops per agent.
	// 0 means use default (2).
	MaxPerAgent int `toml:"max_per_agent,omitempty" jsonschema:"default=2"`
	// MaxTotal is the maximum total number of active convergence loops.
	// 0 means use default (10).
	MaxTotal int `toml:"max_total,omitempty" jsonschema:"default=10"`
}

// MaxPerAgentOrDefault returns MaxPerAgent, defaulting to 2.
func (c ConvergenceConfig) MaxPerAgentOrDefault() int {
	if c.MaxPerAgent <= 0 {
		return 2
	}
	return c.MaxPerAgent
}

// MaxTotalOrDefault returns MaxTotal, defaulting to 10.
func (c ConvergenceConfig) MaxTotalOrDefault() int {
	if c.MaxTotal <= 0 {
		return 10
	}
	return c.MaxTotal
}

// DaemonConfig holds controller daemon settings.
type DaemonConfig struct {
	// FormulaV2 enables formula v2 graph workflow infrastructure:
	// the control-dispatcher implicit agent, graph.v2 formula compilation,
	// and batch graph-apply bead creation. Requires bd with --graph support.
	// Default: false (opt-in while the feature stabilizes).
	FormulaV2 bool `toml:"formula_v2,omitempty"`
	// GraphWorkflows is the deprecated predecessor of FormulaV2. Retained
	// for backwards compatibility: if graph_workflows is true in TOML and
	// formula_v2 is not set, FormulaV2 is promoted automatically during
	// parsing.
	GraphWorkflows bool `toml:"graph_workflows,omitempty"`
	// PatrolInterval is the health patrol interval. Duration string (e.g., "30s", "5m", "1h"). Defaults to "30s".
	PatrolInterval string `toml:"patrol_interval,omitempty" jsonschema:"default=30s"`
	// MaxRestarts is the maximum number of agent restarts within RestartWindow before
	// the agent is quarantined. 0 means unlimited (no crash loop detection). Defaults to 5.
	MaxRestarts *int `toml:"max_restarts,omitempty" jsonschema:"default=5"`
	// RestartWindow is the sliding time window for counting restarts.
	// Duration string (e.g., "30s", "5m", "1h"). Defaults to "1h".
	RestartWindow string `toml:"restart_window,omitempty" jsonschema:"default=1h"`
	// ShutdownTimeout is the time to wait after sending Ctrl-C before force-killing
	// agents during shutdown. Duration string (e.g., "5s", "30s"). Set to "0s"
	// for immediate kill. Defaults to "5s".
	ShutdownTimeout string `toml:"shutdown_timeout,omitempty" jsonschema:"default=5s"`
	// WispGCInterval is how often wisp GC runs. Duration string (e.g., "5m", "1h").
	// Wisp GC is disabled unless both WispGCInterval and WispTTL are set.
	WispGCInterval string `toml:"wisp_gc_interval,omitempty"`
	// WispTTL is how long a closed molecule survives before being purged.
	// Duration string (e.g., "24h", "7d"). Wisp GC is disabled unless both
	// WispGCInterval and WispTTL are set.
	WispTTL string `toml:"wisp_ttl,omitempty"`
	// DriftDrainTimeout is the maximum time to wait for an agent to acknowledge
	// a drain signal during a config-drift restart. If the agent doesn't ack
	// within this window, the controller force-kills and restarts it.
	// Duration string (e.g., "2m", "5m"). Defaults to "2m".
	DriftDrainTimeout string `toml:"drift_drain_timeout,omitempty" jsonschema:"default=2m"`
	// ObservePaths lists extra directories to search for Claude JSONL session
	// files (e.g., aimux session paths). The default search path
	// (~/.claude/projects/) is always included.
	ObservePaths []string `toml:"observe_paths,omitempty"`
	// ProbeConcurrency bounds the number of concurrent bd subprocess probes
	// issued by the pool scale_check and work_query paths. bd serializes on
	// a shared dolt sql-server, so unbounded parallelism causes contention.
	// Nil (unset) defaults to 8. Set higher for workspaces with a fast
	// dedicated dolt server, or lower to reduce contention on slow storage.
	ProbeConcurrency *int `toml:"probe_concurrency,omitempty" jsonschema:"default=8"`
	// MaxWakesPerTick caps how many sessions the reconciler may start in a
	// single tick. Nil (unset) defaults to 5. Values <= 0 are treated as the
	// default — set a positive integer to override.
	MaxWakesPerTick *int `toml:"max_wakes_per_tick,omitempty" jsonschema:"default=5"`
}

// PatrolIntervalDuration returns the patrol interval as a time.Duration.
// Defaults to 30s if empty or unparseable.
func (d *DaemonConfig) PatrolIntervalDuration() time.Duration {
	if d.PatrolInterval == "" {
		return 30 * time.Second
	}
	dur, err := time.ParseDuration(d.PatrolInterval)
	if err != nil {
		return 30 * time.Second
	}
	return dur
}

// MaxRestartsOrDefault returns the max restarts threshold. Nil (unset) defaults
// to 5. Zero means unlimited (no crash loop detection).
func (d *DaemonConfig) MaxRestartsOrDefault() int {
	if d.MaxRestarts == nil {
		return 5
	}
	return *d.MaxRestarts
}

// RestartWindowDuration returns the restart window as a time.Duration.
// Defaults to 1h if empty or unparseable.
func (d *DaemonConfig) RestartWindowDuration() time.Duration {
	if d.RestartWindow == "" {
		return time.Hour
	}
	dur, err := time.ParseDuration(d.RestartWindow)
	if err != nil {
		return time.Hour
	}
	return dur
}

// ShutdownTimeoutDuration returns the shutdown timeout as a time.Duration.
// Defaults to 5s if empty or unparseable. Zero means immediate kill.
func (d *DaemonConfig) ShutdownTimeoutDuration() time.Duration {
	if d.ShutdownTimeout == "" {
		return 5 * time.Second
	}
	dur, err := time.ParseDuration(d.ShutdownTimeout)
	if err != nil {
		return 5 * time.Second
	}
	return dur
}

// DefaultProbeConcurrency is the default bd probe concurrency limit.
// Used by ProbeConcurrencyOrDefault and referenced by cmd/gc/pool.go
// so the default lives in one place.
const DefaultProbeConcurrency = 8

// ProbeConcurrencyOrDefault returns the bd probe concurrency limit.
// Nil (unset) defaults to DefaultProbeConcurrency. Values below 1 are
// clamped to 1 to prevent deadlock on a zero-capacity semaphore.
func (d *DaemonConfig) ProbeConcurrencyOrDefault() int {
	if d.ProbeConcurrency == nil {
		return DefaultProbeConcurrency
	}
	if *d.ProbeConcurrency < 1 {
		return 1
	}
	return *d.ProbeConcurrency
}

// DefaultMaxWakesPerTick is the per-tick wake budget the reconciler uses
// when [daemon].max_wakes_per_tick is unset.
const DefaultMaxWakesPerTick = 5

// MaxWakesPerTickOrDefault returns the per-tick wake budget. Nil (unset)
// and non-positive values fall back to DefaultMaxWakesPerTick.
func (d *DaemonConfig) MaxWakesPerTickOrDefault() int {
	if d.MaxWakesPerTick == nil || *d.MaxWakesPerTick <= 0 {
		return DefaultMaxWakesPerTick
	}
	return *d.MaxWakesPerTick
}

// DriftDrainTimeoutDuration returns the drift drain timeout as a time.Duration.
// Defaults to 2m if empty or unparseable.
func (d *DaemonConfig) DriftDrainTimeoutDuration() time.Duration {
	if d.DriftDrainTimeout == "" {
		return 2 * time.Minute
	}
	dur, err := time.ParseDuration(d.DriftDrainTimeout)
	if err != nil {
		return 2 * time.Minute
	}
	return dur
}

// WispGCIntervalDuration returns the wisp GC interval as a time.Duration.
// Returns 0 if empty or unparseable.
func (d *DaemonConfig) WispGCIntervalDuration() time.Duration {
	if d.WispGCInterval == "" {
		return 0
	}
	dur, err := time.ParseDuration(d.WispGCInterval)
	if err != nil {
		return 0
	}
	return dur
}

// WispTTLDuration returns the wisp TTL as a time.Duration.
// Returns 0 if empty or unparseable.
func (d *DaemonConfig) WispTTLDuration() time.Duration {
	if d.WispTTL == "" {
		return 0
	}
	dur, err := time.ParseDuration(d.WispTTL)
	if err != nil {
		return 0
	}
	return dur
}

// WispGCEnabled reports whether wisp GC is configured. Both wisp_gc_interval
// and wisp_ttl must be set to non-zero durations.
func (d *DaemonConfig) WispGCEnabled() bool {
	return d.WispGCIntervalDuration() > 0 && d.WispTTLDuration() > 0
}

// FormulasDir returns the formulas directory, defaulting to "formulas".
func (c *City) FormulasDir() string {
	if c.Formulas.Dir != "" {
		return c.Formulas.Dir
	}
	return citylayout.FormulasRoot
}

// AgentDefaults provides city-level agent defaults declared via
// [agent_defaults] in city.toml. The runtime currently applies
// default_sling_formula and append_fragments; the remaining fields are
// parsed and composed but are not yet inherited onto agents automatically.
type AgentDefaults struct {
	// Model is the parsed/composed default model name for agents
	// (e.g., "claude-sonnet-4-6"), but it is not yet auto-applied at
	// runtime. Agents with their own model override would take precedence.
	Model string `toml:"model,omitempty"`
	// WakeMode is the parsed/composed default wake mode ("resume" or
	// "fresh"), but it is not yet auto-applied at runtime.
	WakeMode string `toml:"wake_mode,omitempty" jsonschema:"enum=resume,enum=fresh"`
	// DefaultSlingFormula is the city-level default formula used for agents
	// that inherit [agent_defaults]. Explicit agents only receive this value
	// when agent_defaults.default_sling_formula is set; implicit multi-session
	// configs are seeded with "mol-do-work" elsewhere when no explicit default is set.
	DefaultSlingFormula string `toml:"default_sling_formula,omitempty"`
	// AllowOverlay is parsed and composed as a city-level allowlist for
	// session overlays, but it is not yet inherited onto agents
	// automatically at runtime.
	AllowOverlay []string `toml:"allow_overlay,omitempty"`
	// AllowEnvOverride is parsed and composed as a city-level allowlist for
	// session env overrides, but it is not yet inherited onto agents
	// automatically at runtime. Names must match ^[A-Z][A-Z0-9_]{0,127}$.
	AllowEnvOverride []string `toml:"allow_env_override,omitempty"`
	// AppendFragments lists named template fragments to auto-append to
	// .template.md prompts after rendering. Legacy .md.tmpl prompts are
	// still supported during the transition; plain .md remains inert.
	// V2 migration convenience — replaces global_fragments/inject_fragments
	// for city-wide defaults.
	AppendFragments []string `toml:"append_fragments,omitempty"`
	// Skills is a tombstone field retained for v0.15.1 backwards
	// compatibility. Parsed and composed for migration visibility, but
	// attachment-list fields are accepted but ignored by the active
	// materializer.
	Skills []string `toml:"skills,omitempty"`
	// MCP is a tombstone field retained for v0.15.1 backwards compatibility.
	// Parsed and composed for migration visibility, but attachment-list
	// fields are accepted but ignored by the active materializer.
	MCP []string `toml:"mcp,omitempty"`
}

func mergeAgentDefaultsAliasPreferCanonical(dst *AgentDefaults, src AgentDefaults, meta toml.MetaData) {
	if !meta.IsDefined("agent_defaults", "model") {
		dst.Model = src.Model
	}
	if !meta.IsDefined("agent_defaults", "wake_mode") {
		dst.WakeMode = src.WakeMode
	}
	if !meta.IsDefined("agent_defaults", "default_sling_formula") {
		dst.DefaultSlingFormula = src.DefaultSlingFormula
	}
	if !meta.IsDefined("agent_defaults", "allow_overlay") {
		dst.AllowOverlay = append([]string(nil), src.AllowOverlay...)
	}
	if !meta.IsDefined("agent_defaults", "allow_env_override") {
		dst.AllowEnvOverride = append([]string(nil), src.AllowEnvOverride...)
	}
	if !meta.IsDefined("agent_defaults", "append_fragments") {
		dst.AppendFragments = append([]string(nil), src.AppendFragments...)
	}
	if !meta.IsDefined("agent_defaults", "skills") {
		dst.Skills = append([]string(nil), src.Skills...)
	}
	if !meta.IsDefined("agent_defaults", "mcp") {
		dst.MCP = append([]string(nil), src.MCP...)
	}
}

func normalizeAgentDefaultsAlias(cfg *City, meta toml.MetaData) {
	if meta.IsDefined("agent_defaults") {
		if meta.IsDefined("agents") {
			mergeAgentDefaultsAliasPreferCanonical(&cfg.AgentDefaults, cfg.AgentsDefaults, meta)
		}
		cfg.AgentsDefaults = AgentDefaults{}
		return
	}
	if meta.IsDefined("agents") {
		cfg.AgentDefaults = cfg.AgentsDefaults
		cfg.AgentsDefaults = AgentDefaults{}
	}
}

// Agent defines a configured agent in the city.
type Agent struct {
	// Name is the unique identifier for this agent.
	Name string `toml:"name" jsonschema:"required"`
	// Description is a human-readable description shown in MC's session creation UI.
	Description string `toml:"description,omitempty"`
	// Dir is the identity prefix for rig-scoped agents and the default
	// working directory when WorkDir is not set.
	Dir string `toml:"dir,omitempty"`
	// WorkDir overrides the session working directory without changing the
	// agent's qualified identity. Relative paths resolve against city root
	// and may use the same template placeholders as session_setup.
	WorkDir string `toml:"work_dir,omitempty"`
	// Scope defines where this agent is instantiated: "city" (one per city)
	// or "rig" (one per rig, the default). Only meaningful for pack-defined
	// agents; inline agents in city.toml use Dir directly.
	Scope string `toml:"scope,omitempty" jsonschema:"enum=city,enum=rig"`
	// Suspended prevents the reconciler from spawning this agent. Toggle with gc agent suspend/resume.
	Suspended bool `toml:"suspended,omitempty"`
	// PreStart is a list of shell commands run before session creation.
	// Commands run on the target filesystem: locally for tmux, inside the
	// pod/container for exec providers. Template variables same as session_setup.
	PreStart []string `toml:"pre_start,omitempty"`
	// PromptTemplate is the path to this agent's prompt template file.
	// Relative paths resolve against the city directory.
	PromptTemplate string `toml:"prompt_template,omitempty"`
	// Nudge is text typed into the agent's tmux session after startup.
	// Used for CLI agents that don't accept command-line prompts.
	Nudge string `toml:"nudge,omitempty"`
	// Session overrides the session transport for this agent.
	// "" (default) uses the city-level session provider (typically tmux).
	// "acp" uses the Agent Client Protocol (JSON-RPC over stdio).
	// The agent's resolved provider must have supports_acp = true.
	Session string `toml:"session,omitempty" jsonschema:"enum=acp"`
	// Provider names the provider preset to use for this agent.
	Provider string `toml:"provider,omitempty"`
	// StartCommand overrides the provider's command for this agent.
	StartCommand string `toml:"start_command,omitempty"`
	// Args overrides the provider's default arguments.
	Args []string `toml:"args,omitempty"`
	// PromptMode controls how prompts are delivered: "arg", "flag", or "none".
	PromptMode string `toml:"prompt_mode,omitempty" jsonschema:"enum=arg,enum=flag,enum=none,default=arg"`
	// PromptFlag is the CLI flag used to pass prompts when prompt_mode is "flag".
	PromptFlag string `toml:"prompt_flag,omitempty"`
	// ReadyDelayMs is milliseconds to wait after launch before considering the agent ready.
	ReadyDelayMs *int `toml:"ready_delay_ms,omitempty" jsonschema:"minimum=0"`
	// ReadyPromptPrefix is the string prefix that indicates the agent is ready for input.
	ReadyPromptPrefix string `toml:"ready_prompt_prefix,omitempty"`
	// ProcessNames lists process names to look for when checking if the agent is running.
	ProcessNames []string `toml:"process_names,omitempty"`
	// EmitsPermissionWarning indicates whether the agent emits permission prompts that should be suppressed.
	EmitsPermissionWarning *bool `toml:"emits_permission_warning,omitempty"`
	// Env sets additional environment variables for the agent process.
	Env map[string]string `toml:"env,omitempty"`
	// OptionDefaults overrides the provider's effective schema defaults
	// for this agent. Keys are option keys, values are choice values.
	// Applied on top of the provider's OptionDefaults (agent keys win).
	// Example: option_defaults = { permission_mode = "plan", model = "sonnet" }
	OptionDefaults map[string]string `toml:"option_defaults,omitempty"`
	// MaxActiveSessions is the agent-level cap on concurrent sessions.
	// Nil means inherit from rig, then workspace, then unlimited.
	// Replaces pool.max.
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// MinActiveSessions is the minimum number of sessions to keep alive.
	// Agent-level only. Counts against rig/workspace caps. Replaces pool.min.
	MinActiveSessions *int `toml:"min_active_sessions,omitempty"`
	// ScaleCheck is a shell command template whose output determines desired
	// session count. Optional override — when set, its output is the desired
	// count (still clamped by all cap levels). If it contains Go template
	// placeholders, gc expands them using the same PathContext fields as
	// work_dir and session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot,
	// CityName) before running the command.
	ScaleCheck string `toml:"scale_check,omitempty"`
	// DrainTimeout is the maximum time to wait for a session to finish its
	// current work before force-killing it during scale-down. Duration string
	// (e.g., "5m", "30m", "1h"). Defaults to "5m".
	DrainTimeout string `toml:"drain_timeout,omitempty" jsonschema:"default=5m"`
	// OnBoot is a shell command template run once at controller startup for
	// this agent. If it contains Go template placeholders, gc expands them
	// using the same PathContext fields as work_dir and session_setup
	// (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName) before running
	// the command.
	OnBoot string `toml:"on_boot,omitempty"`
	// OnDeath is a shell command template run when a session dies unexpectedly.
	// If it contains Go template placeholders, gc expands them using the same
	// PathContext fields as work_dir and session_setup (Agent, AgentBase,
	// Rig, RigRoot, CityRoot, CityName) before running the command.
	OnDeath string `toml:"on_death,omitempty"`
	// Namepool is the path to a plain text file with one name per line.
	// When set, sessions use names from the file as display aliases.
	Namepool string `toml:"namepool,omitempty"`
	// NamepoolNames holds names loaded from the Namepool file at config load
	// time. Not serialized to TOML.
	NamepoolNames []string `toml:"-"`
	// WorkQuery is the shell command template to find available work for this
	// agent. If it contains Go template placeholders, gc expands them using
	// the same PathContext fields as work_dir and session_setup (Agent,
	// AgentBase, Rig, RigRoot, CityRoot, CityName) before probe, hook, and
	// prompt-context execution. Used by gc hook and available in prompt
	// templates as {{.WorkQuery}}.
	// If unset, Gas City uses a three-tier default query:
	//   1. in_progress work assigned to this session/alias (crash recovery)
	//   2. ready work assigned to this session/alias (pre-assigned work)
	//   3. ready unassigned work with gc.routed_to=<qualified-name>
	// When the controller probes for demand without session context, only the
	// routed_to tier applies. Override to integrate with external task systems.
	WorkQuery string `toml:"work_query,omitempty"`
	// SlingQuery is the command template to route a bead to this session config.
	// If it contains Go template placeholders, gc expands them using the same
	// PathContext fields as work_dir and session_setup (Agent, AgentBase,
	// Rig, RigRoot, CityRoot, CityName) before replacing {} with the bead
	// ID. Used by gc sling to make a bead visible to the target's work_query.
	// The placeholder {} is replaced with the bead ID at runtime.
	// Default for all agents:
	// "bd update {} --set-metadata gc.routed_to=<qualified-name>".
	// Routing is metadata-based; sling stamps the target template and the
	// reconciler/scale_check paths decide when sessions are created.
	// Custom sling_query and work_query can be overridden independently.
	SlingQuery string `toml:"sling_query,omitempty"`
	// IdleTimeout is the maximum time an agent session can be inactive before
	// the controller kills and restarts it. Duration string (e.g., "15m", "1h").
	// Empty (default) disables idle checking.
	IdleTimeout string `toml:"idle_timeout,omitempty"`
	// SleepAfterIdle overrides idle sleep policy for this agent. Accepts a
	// duration string (e.g., "30s") or "off".
	SleepAfterIdle string `toml:"sleep_after_idle,omitempty"`
	// InstallAgentHooks overrides workspace-level install_agent_hooks for this agent.
	// When set, replaces (not adds to) the workspace default.
	InstallAgentHooks []string `toml:"install_agent_hooks,omitempty"`
	// Skills is a tombstone field retained for v0.15.1 backwards
	// compatibility. Accepted during parse for migration visibility, but
	// attachment-list fields are accepted but ignored by the active
	// materializer.
	Skills []string `toml:"skills,omitempty"`
	// MCP is a tombstone field retained for v0.15.1 backwards compatibility.
	// Accepted during parse for migration visibility, but attachment-list
	// fields are accepted but ignored by the active materializer.
	MCP []string `toml:"mcp,omitempty"`
	// HooksInstalled overrides automatic hook detection. Set to true when hooks
	// are manually installed (e.g., merged into the project's own hook config)
	// and auto-installation via install_agent_hooks is not desired. When true,
	// the agent is treated as hook-enabled for startup behavior: no prime
	// instruction in beacon and no delayed nudge. Interacts with
	// install_agent_hooks — set this instead when hooks are pre-installed.
	HooksInstalled *bool `toml:"hooks_installed,omitempty"`
	// SessionSetup is a list of shell commands run after session creation.
	// Each command is a template string supporting placeholders:
	// {{.Session}}, {{.Agent}}, {{.AgentBase}}, {{.Rig}}, {{.RigRoot}},
	// {{.CityRoot}}, {{.CityName}}, {{.WorkDir}}.
	// Commands run in gc's process (not inside the agent session) via sh -c.
	SessionSetup []string `toml:"session_setup,omitempty"`
	// SessionSetupScript is the path to a script run after session_setup commands.
	// Relative paths resolve against the city directory. The script receives
	// context via environment variables (GC_SESSION plus existing GC_* vars).
	SessionSetupScript string `toml:"session_setup_script,omitempty"`
	// SessionLive is a list of shell commands that are safe to re-apply
	// without restarting the agent. Run at startup (after session_setup)
	// and re-applied on config change without triggering a restart.
	// Must be idempotent. Typical use: tmux theming, keybindings, status bars.
	// Same template placeholders as session_setup.
	SessionLive []string `toml:"session_live,omitempty"`
	// OverlayDir is a directory whose contents are recursively copied (additive)
	// into the agent's working directory at startup. Existing files are not
	// overwritten. Relative paths resolve against the declaring config file's
	// directory (pack-safe).
	OverlayDir string `toml:"overlay_dir,omitempty"`
	// SourceDir is the directory where this agent's config was defined.
	// Set during pack/fragment loading; empty for inline agents.
	// Runtime-only — not persisted to TOML or JSON.
	SourceDir string `toml:"-" json:"-"`
	// SharedSkills holds legacy derived attachment-list state for this agent.
	// Runtime-only compatibility data — not persisted to TOML or JSON, and
	// not consumed by the active skill materializer.
	SharedSkills []string `toml:"-" json:"-"`
	// SharedMCP holds legacy derived attachment-list state for this agent.
	// Runtime-only compatibility data — not persisted to TOML or JSON, and
	// not consumed by the active MCP materializer.
	SharedMCP []string `toml:"-" json:"-"`
	// SkillsDir is the agent-local private skills catalog root.
	// Runtime-only — not persisted to TOML or JSON.
	SkillsDir string `toml:"-" json:"-"`
	// MCPDir is the agent-local private MCP catalog root.
	// Runtime-only — not persisted to TOML or JSON.
	MCPDir string `toml:"-" json:"-"`
	// Implicit marks agents auto-generated from built-in providers.
	// These have pool min=0, max=-1 and are available as sling targets
	// even without an explicit [[agent]] entry in city.toml.
	// Runtime-only — not persisted to TOML or JSON.
	Implicit bool `toml:"-" json:"-"`
	// DefaultSlingFormula is the formula name automatically applied via --on
	// when beads are slung to this agent, unless --no-formula is set.
	// Example: "mol-polecat-work"
	DefaultSlingFormula *string `toml:"default_sling_formula,omitempty"`
	// InheritedDefaultSlingFormula records the pack-scoped default formula for
	// agents loaded from imported packs. Runtime-only.
	InheritedDefaultSlingFormula *string `toml:"-" json:"-"`
	// InjectFragments lists named template fragments to append to this agent's
	// rendered prompt. Fragments come from shared template directories across
	// all loaded packs. Each name must match a {{ define "name" }} block.
	InjectFragments []string `toml:"inject_fragments,omitempty"`
	// InheritedAppendFragments records pack-scoped append_fragments inherited
	// from an imported pack's [agent_defaults]. Runtime-only.
	InheritedAppendFragments []string `toml:"-" json:"-"`
	// InjectAssignedSkills controls whether gc appends an
	// "assigned skills" appendix to the agent's rendered prompt. The
	// appendix lists every skill visible to this agent, partitioned
	// into (assigned-to-you, shared-with-every-agent), so agents
	// sharing a scope-root sink can tell which skills are their
	// specialization vs which are the city-wide set.
	//
	// Pointer tri-state:
	//   nil   -> inherit: inject when the agent has a vendor sink
	//   *true -> explicitly inject (equivalent to the default)
	//   *false -> disable; the template is responsible for rendering
	//             any skill guidance itself
	InjectAssignedSkills *bool `toml:"inject_assigned_skills,omitempty"`
	// Attach controls whether the agent's session supports interactive
	// attachment (e.g., tmux attach). When false, the agent can use a
	// lighter runtime (subprocess instead of tmux). Defaults to true.
	Attach *bool `toml:"attach,omitempty"`
	// Fallback marks this agent as a fallback definition. During pack
	// composition, a non-fallback agent with the same name wins silently.
	// When two fallbacks collide, the first loaded (depth-first) wins.
	Fallback bool `toml:"fallback,omitempty"`
	// DependsOn lists agent names that must be awake before this agent wakes.
	// Used for dependency-ordered startup and shutdown. Validated for cycles
	// at config load time.
	DependsOn []string `toml:"depends_on,omitempty"`
	// ResumeCommand is the full shell command to run when resuming this agent.
	// Supports {{.SessionKey}} template variable. When set, takes precedence
	// over the provider's ResumeFlag/ResumeStyle. Example:
	//   "claude --resume {{.SessionKey}} --dangerously-skip-permissions"
	ResumeCommand string `toml:"resume_command,omitempty"`
	// WakeMode controls context freshness across sleep/wake cycles.
	// "resume" (default): reuse provider session key for conversation continuity.
	// "fresh": start a new provider session on every wake (polecat pattern).
	WakeMode string `toml:"wake_mode,omitempty" jsonschema:"enum=resume,enum=fresh"`
	// SleepAfterIdleSource records which config layer supplied SleepAfterIdle.
	// Runtime-only — not persisted to TOML or JSON.
	SleepAfterIdleSource string `toml:"-" json:"-"`
	// PoolName is the template agent's qualified name, set during pool
	// expansion. Pool instances use this for gc.routed_to-based work
	// discovery (e.g., dog) rather than their concrete instance name (e.g., dog-1).
	PoolName string `toml:"-"`
	// BindingName is the name of the [imports.X] block that brought this
	// agent into scope. Empty for the city pack's own agents. Set during
	// V2 import expansion. Used to construct qualified names like
	// "gastown.mayor" or "proj/gastown.polecat".
	// Runtime-only — not persisted to TOML or JSON.
	BindingName string `toml:"-" json:"-"`
	// PackName is the pack.name of the pack that defined this agent.
	// Set during V2 import expansion.
	// Runtime-only — not persisted to TOML or JSON.
	PackName string `toml:"-" json:"-"`
}

// IdleTimeoutDuration returns the idle timeout as a time.Duration.
// Returns 0 if empty or unparseable (disabled).
func (a *Agent) IdleTimeoutDuration() time.Duration {
	if a.IdleTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(a.IdleTimeout)
	if err != nil {
		return 0
	}
	return d
}

// EffectiveWakeMode returns the configured wake mode, defaulting to "resume".
func (a *Agent) EffectiveWakeMode() string {
	if a.WakeMode == "fresh" {
		return "fresh"
	}
	return "resume"
}

// AttachEnabled reports whether the agent supports interactive attachment.
func (a *Agent) AttachEnabled() bool {
	return a.Attach == nil || *a.Attach
}

// EffectiveWorkQuery returns the work query command for this agent.
// If WorkQuery is set, returns it as-is. Otherwise returns the default
// three-tier query with multi-identifier assignee resolution.
//
// Assignee resolution order: $GC_SESSION_ID (bead ID) > $GC_SESSION_NAME
// (tmux session name) > $GC_ALIAS (named identity / qualified name).
// All three are checked so work is found regardless of which identifier
// was used when assigning.
//
// State priority: in_progress+assigned (crash recovery) >
// ready+assigned (pre-assigned) > ready+unassigned+routed_to (pool).
//
// When the reconciler runs the query for demand detection (no session
// context), all identity vars are empty → assignee tiers skip → only
// the routed_to tier fires to detect new demand.
func (a *Agent) EffectiveWorkQuery() string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.QualifiedName()
	if a.PoolName != "" {
		target = a.PoolName
	}
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		return `sh -c '` +
			// Tier 1: in_progress assigned to any of my identifiers (crash recovery)
			`for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
			`[ -z "$id" ] && continue; ` +
			`r=$(bd list --status in_progress --assignee="$id" --json --limit=1 2>/dev/null); ` +
			`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
			`done; ` +
			// Tier 2: ready assigned to any of my identifiers (pre-assigned)
			`for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
			`[ -z "$id" ] && continue; ` +
			`r=$(bd ready --assignee="$id" --json --limit=1 2>/dev/null); ` +
			`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
			`done; ` +
			// Tier 3: ready unassigned routed to this config (shared routed queue).
			// Only ephemeral sessions and controller probes consume generic config demand.
			`case "$GC_SESSION_ORIGIN" in ` +
			`ephemeral|"") ;; ` +
			`*) exit 0 ;; ` +
			`esac; ` +
			`r=$(bd ready --metadata-field gc.routed_to=` + target +
			` --unassigned --json --limit=1 2>/dev/null); ` +
			`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
			// Tier 4: open routed molecule roots. scale_check already counts
			// these, so startup must be able to see them too.
			`bd list --metadata-field gc.routed_to=` + target +
			` --status=open --type=molecule --no-assignee --json --limit=1 2>/dev/null'`
	}
	return `sh -c '` +
		// Tier 1: in_progress assigned to any of my identifiers (crash recovery).
		// Built-in control-dispatchers also claim legacy workflow-control names so
		// pre-rename workflows keep moving without live metadata rewrites.
		`for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(bd list --status in_progress --assignee="$cand" --json --limit=1 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`done; ` +
		`done; ` +
		// Tier 2: ready assigned to any of my identifiers (pre-assigned)
		`for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(bd ready --assignee="$cand" --json --limit=1 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`done; ` +
		`done; ` +
		// Tier 3: ready unassigned routed to this config (shared routed queue),
		// then the legacy workflow-control route for pre-rename graphs.
		// Only ephemeral sessions and controller probes consume generic config demand.
		`case "$GC_SESSION_ORIGIN" in ` +
		`ephemeral|"") ;; ` +
		`*) exit 0 ;; ` +
		`esac; ` +
		`r=$(bd ready --metadata-field gc.routed_to=` + target +
		` --unassigned --json --limit=1 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`bd ready --metadata-field gc.routed_to=` + legacyTarget +
		` --unassigned --json --limit=1 2>/dev/null'`
}

func legacyWorkflowControlQualifiedName(target string) string {
	target = strings.TrimSpace(target)
	if target == ControlDispatcherAgentName {
		return "workflow-control"
	}
	const suffix = "/" + ControlDispatcherAgentName
	if strings.HasSuffix(target, suffix) {
		return strings.TrimSuffix(target, suffix) + "/workflow-control"
	}
	return ""
}

// EffectiveSlingQuery returns the sling query command template for this agent.
// The template uses {} as a placeholder for the bead ID.
// If SlingQuery is set, returns it as-is. Otherwise returns the default:
// "bd update {} --set-metadata gc.routed_to=<template>"
//
// All agents use metadata-based routing. The reconciler and scale_check
// handle session creation; sling just stamps the target template.
func (a *Agent) EffectiveSlingQuery() string {
	if a.SlingQuery != "" {
		return a.SlingQuery
	}
	return a.DefaultSlingQuery()
}

// DefaultSlingQuery returns the built-in metadata-routing sling query for
// this agent. Callers outside config should prefer this helper over rebuilding
// the command string to preserve the bd boundary invariant.
func (a *Agent) DefaultSlingQuery() string {
	return "bd update {} --set-metadata gc.routed_to=" + a.QualifiedName()
}

// EffectiveDefaultSlingFormula returns the default sling formula for
// this agent, or "" if none is set.
func (a *Agent) EffectiveDefaultSlingFormula() string {
	if a.DefaultSlingFormula != nil {
		return *a.DefaultSlingFormula
	}
	if a.InheritedDefaultSlingFormula != nil {
		return *a.InheritedDefaultSlingFormula
	}
	return ""
}

// DrainTimeoutDuration returns the drain timeout as a time.Duration.
// Defaults to 5m if empty or unparseable.
func (a *Agent) DrainTimeoutDuration() time.Duration {
	if a.DrainTimeout == "" {
		return 5 * time.Minute
	}
	dur, err := time.ParseDuration(a.DrainTimeout)
	if err != nil {
		return 5 * time.Minute
	}
	return dur
}

// EffectiveScaleCheck returns the scale check command for this agent.
// If ScaleCheck is set, returns it. Otherwise returns a default that
// counts actionable work routed to this agent's template, including
// formula-dispatched molecule beads (which bd ready excludes).
func (a *Agent) EffectiveScaleCheck() string {
	if a.ScaleCheck != "" {
		return a.ScaleCheck
	}
	template := a.QualifiedName()
	return `ready=$(bd ready --metadata-field gc.routed_to=` + template +
		` --unassigned --json 2>/dev/null | jq 'length' 2>/dev/null); ` +
		`active=$(bd list --metadata-field gc.routed_to=` + template +
		` --status=in_progress --no-assignee --json 2>/dev/null | jq 'length' 2>/dev/null); ` +
		`molecules=$(bd list --metadata-field gc.routed_to=` + template +
		` --status=open --type=molecule --no-assignee --json 2>/dev/null | jq 'length' 2>/dev/null); ` +
		`echo "$(( ${ready:-0} + ${active:-0} + ${molecules:-0} ))" || echo 0`
}

// EffectiveMaxActiveSessions returns the agent's max active sessions.
// Priority: agent.MaxActiveSessions > pool.Max > nil (unlimited).
func (a *Agent) EffectiveMaxActiveSessions() *int {
	return a.MaxActiveSessions // nil = unlimited (default)
}

// EffectiveMinActiveSessions returns the agent's min active sessions.
func (a *Agent) EffectiveMinActiveSessions() int {
	if a.MinActiveSessions != nil && *a.MinActiveSessions > 0 {
		return *a.MinActiveSessions
	}
	return 0
}

// SupportsGenericEphemeralSessions reports whether the template may satisfy
// generic controller demand with ephemeral sessions.
func (a *Agent) SupportsGenericEphemeralSessions() bool {
	if a == nil {
		return false
	}
	if m := a.EffectiveMaxActiveSessions(); m != nil && *m == 0 {
		return false
	}
	return true
}

// SupportsMultipleSessions reports whether the template may materialize more
// than one distinct concrete session identity. Unlike
// SupportsGenericEphemeralSessions, max_active_sessions = 0 still represents a
// multi-session template shape even though generic ephemeral session creation
// is disabled.
func (a *Agent) SupportsMultipleSessions() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	maxSessions := a.EffectiveMaxActiveSessions()
	return maxSessions == nil || *maxSessions != 1
}

// SupportsInstanceExpansion reports whether the template may have multiple
// simultaneously addressable concrete instances and therefore needs instance
// discovery / synthetic member naming.
func (a *Agent) SupportsInstanceExpansion() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	if m := a.EffectiveMaxActiveSessions(); m != nil {
		return *m < 0 || *m > 1
	}
	return true
}

// HasUnlimitedSessionCapacity reports whether max_active_sessions is unbounded.
func (a *Agent) HasUnlimitedSessionCapacity() bool {
	if a == nil {
		return false
	}
	m := a.EffectiveMaxActiveSessions()
	return m == nil || *m < 0
}

// ResolvedMaxActiveSessions returns the effective max for this agent,
// inheriting from rig then workspace if not set on the agent directly.
func (a *Agent) ResolvedMaxActiveSessions(cfg *City) *int {
	if m := a.EffectiveMaxActiveSessions(); m != nil {
		return m
	}
	// Inherit from rig.
	if a.Dir != "" && cfg != nil {
		for _, rig := range cfg.Rigs {
			if rig.Name == a.Dir && rig.MaxActiveSessions != nil {
				return rig.MaxActiveSessions
			}
		}
	}
	// Inherit from workspace.
	if cfg != nil && cfg.Workspace.MaxActiveSessions != nil {
		return cfg.Workspace.MaxActiveSessions
	}
	return nil // unlimited
}

// EffectiveOnDeath returns the on_death command for this agent.
// If OnDeath is set, returns it. Otherwise returns the default recovery hook
// that unclaims in-progress work assigned to this concrete agent identity.
func (a *Agent) EffectiveOnDeath() string {
	if a.OnDeath != "" {
		return a.OnDeath
	}
	return `bd list --assignee=` + a.QualifiedName() +
		` --status=in_progress --json 2>/dev/null | ` +
		`jq -r '.[].id' 2>/dev/null | ` +
		`xargs -rI{} bd update {} --assignee "" 2>/dev/null`
}

// EffectiveOnBoot returns the on_boot command for this agent.
// If OnBoot is set, returns it. Otherwise returns the default recovery hook
// that unclaims in-progress work routed to this backing config.
func (a *Agent) EffectiveOnBoot() string {
	if a.OnBoot != "" {
		return a.OnBoot
	}
	template := a.QualifiedName()
	if a.PoolName != "" {
		template = a.PoolName
	}
	return `bd list --metadata-field gc.routed_to=` + template +
		` --status=in_progress --json 2>/dev/null | ` +
		`jq -r '.[].id' 2>/dev/null | ` +
		`xargs -rI{} bd update {} --assignee "" 2>/dev/null`
}

// InjectImplicitAgents adds on-demand agents for each configured provider at
// both city scope and each rig scope. A provider is "configured" if it
// appears in cfg.Providers OR is named by cfg.Workspace.Provider — so the
// common single-provider case (workspace.provider = "claude") works without
// a redundant [providers.claude] section. Unconfigured built-in providers
// are skipped. Pool min=0, max=-1 (unlimited) so they are available as
// sling targets without an explicit [[agent]] entry. Explicit agents always
// win — if city.toml defines [[agent]] name="claude" (or a rig-scoped
// equivalent), no implicit agent is added for that scope.
// agentKey identifies an agent by its rig directory and name.
type agentKey struct{ dir, name string }

// InjectImplicitAgents adds implicit agent entries for configured providers
// that lack an explicit [[agent]] entry, enabling auto-materialization of
// sling targets without requiring manual agent declarations.
func InjectImplicitAgents(cfg *City) {
	// Build set of existing agent keys (dir, name).
	existing := make(map[agentKey]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		existing[agentKey{a.Dir, a.Name}] = true
	}

	configured := configuredProviders(cfg)
	if len(configured) == 0 {
		injectControlDispatcherAgents(cfg, existing)
		return
	}

	// Deterministic order: built-in providers first (in canonical order),
	// then any custom providers in sorted order.
	providers := configuredProviderOrder(configured)

	promptTemplate := citylayout.SystemPacksRoot + "/core/assets/prompts/pool-worker.md"

	slingFormula := cfg.AgentDefaults.DefaultSlingFormula
	if slingFormula == "" {
		slingFormula = "mol-do-work"
	}

	// City-scoped implicit agents.
	for _, name := range providers {
		if existing[agentKey{"", name}] {
			continue
		}
		cfg.Agents = append(cfg.Agents, Agent{
			Name:                name,
			Provider:            name,
			PromptTemplate:      promptTemplate,
			DefaultSlingFormula: &slingFormula,
			Implicit:            true,
		})
	}

	// Rig-scoped implicit agents.
	for _, rig := range cfg.Rigs {
		for _, name := range providers {
			if existing[agentKey{rig.Name, name}] {
				continue
			}
			cfg.Agents = append(cfg.Agents, Agent{
				Name:                name,
				Dir:                 rig.Name,
				Provider:            name,
				PromptTemplate:      promptTemplate,
				DefaultSlingFormula: &slingFormula,
				Implicit:            true,
			})
		}
	}

	injectControlDispatcherAgents(cfg, existing)
}

// ApplyAgentDefaults applies [agent_defaults] values to all agents that
// don't set their own override. Call after InjectImplicitAgents so
// implicit agents are already present. Control-dispatcher agents are
// skipped because they are infrastructure, not work agents. Imported
// pack defaults take precedence over the root city default.
func ApplyAgentDefaults(cfg *City) {
	applyAgentSharedAttachmentDefaults(cfg.Agents, cfg.AgentDefaults)

	formula := cfg.AgentDefaults.DefaultSlingFormula
	if formula != "" {
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == ControlDispatcherAgentName {
				continue
			}
			if cfg.Agents[i].DefaultSlingFormula == nil && cfg.Agents[i].InheritedDefaultSlingFormula == nil {
				cfg.Agents[i].DefaultSlingFormula = &formula
			}
		}
	}
}

// applyAgentSharedAttachmentDefaults preserves legacy derived attachment-list
// state in SharedSkills/SharedMCP for compatibility checks. The active skill
// and MCP materializers do not consume these fields.
func applyAgentSharedAttachmentDefaults(agents []Agent, defaults AgentDefaults) {
	if len(defaults.Skills) == 0 && len(defaults.MCP) == 0 {
		return
	}
	for i := range agents {
		if agents[i].Name == ControlDispatcherAgentName {
			continue
		}
		if len(defaults.Skills) > 0 {
			agents[i].SharedSkills = appendUnique(agents[i].SharedSkills, defaults.Skills...)
		}
		if len(defaults.MCP) > 0 {
			agents[i].SharedMCP = appendUnique(agents[i].SharedMCP, defaults.MCP...)
		}
	}
}

// deprecatedAttachmentWarning is the canonical warning message emitted when
// a loaded config still references the tombstone attachment-list fields
// removed from the active materializer path in v0.15.1.
const deprecatedAttachmentWarning = "gc: warning: attachment-list fields (`skills`, `mcp`, `skills_append`, `mcp_append`, `shared_skills`) are deprecated as of v0.15.1 and ignored. They may appear on agents, [agent_defaults], [[patches.agent]], [[rigs.overrides]], or [[rigs.patches]]. Remove them from your config (or run `gc doctor --fix` once available). Hard parse error lands in v0.16."

// WarnDeprecatedAttachmentFields returns the canonical deprecation warning if
// any v0.15.0 attachment-list tombstone field appears populated anywhere in
// the loaded config. Callers are responsible for routing the warning through
// their chosen sink.
func WarnDeprecatedAttachmentFields(cfg *City) string {
	if cfg == nil {
		return ""
	}
	if !hasDeprecatedAttachmentFields(cfg) {
		return ""
	}
	return deprecatedAttachmentWarning
}

func hasDeprecatedAttachmentFields(cfg *City) bool {
	if len(cfg.AgentDefaults.Skills) > 0 || len(cfg.AgentDefaults.MCP) > 0 {
		return true
	}
	if len(cfg.AgentsDefaults.Skills) > 0 || len(cfg.AgentsDefaults.MCP) > 0 {
		return true
	}
	for _, a := range cfg.Agents {
		if len(a.Skills) > 0 || len(a.MCP) > 0 || len(a.SharedSkills) > 0 || len(a.SharedMCP) > 0 {
			return true
		}
	}
	for _, p := range cfg.Patches.Agents {
		if len(p.Skills) > 0 || len(p.MCP) > 0 || len(p.SkillsAppend) > 0 || len(p.MCPAppend) > 0 {
			return true
		}
	}
	for _, rig := range cfg.Rigs {
		for _, ov := range rig.Overrides {
			if len(ov.Skills) > 0 || len(ov.MCP) > 0 || len(ov.SkillsAppend) > 0 || len(ov.MCPAppend) > 0 {
				return true
			}
		}
		for _, ov := range rig.RigPatches {
			if len(ov.Skills) > 0 || len(ov.MCP) > 0 || len(ov.SkillsAppend) > 0 || len(ov.MCPAppend) > 0 {
				return true
			}
		}
	}
	return false
}

// mergeAgentDefaults merges src into dst using later-layer precedence for
// scalars and additive append semantics for list fields.
func mergeAgentDefaults(dst *AgentDefaults, src AgentDefaults, label string, prov *Provenance) {
	if src.Model != "" {
		if prov != nil && dst.Model != "" && dst.Model != src.Model {
			prov.Warnings = append(prov.Warnings, fmt.Sprintf("agent_defaults.model redefined by %q", label))
		}
		dst.Model = src.Model
	}
	if src.WakeMode != "" {
		if prov != nil && dst.WakeMode != "" && dst.WakeMode != src.WakeMode {
			prov.Warnings = append(prov.Warnings, fmt.Sprintf("agent_defaults.wake_mode redefined by %q", label))
		}
		dst.WakeMode = src.WakeMode
	}
	if src.DefaultSlingFormula != "" {
		if prov != nil && dst.DefaultSlingFormula != "" && dst.DefaultSlingFormula != src.DefaultSlingFormula {
			prov.Warnings = append(prov.Warnings, fmt.Sprintf("agent_defaults.default_sling_formula redefined by %q", label))
		}
		dst.DefaultSlingFormula = src.DefaultSlingFormula
	}
	if len(src.AllowOverlay) > 0 {
		dst.AllowOverlay = appendUnique(dst.AllowOverlay, src.AllowOverlay...)
	}
	if len(src.AllowEnvOverride) > 0 {
		dst.AllowEnvOverride = appendUnique(dst.AllowEnvOverride, src.AllowEnvOverride...)
	}
	if len(src.AppendFragments) > 0 {
		dst.AppendFragments = appendUnique(dst.AppendFragments, src.AppendFragments...)
	}
	if len(src.Skills) > 0 {
		dst.Skills = appendUnique(dst.Skills, src.Skills...)
	}
	if len(src.MCP) > 0 {
		dst.MCP = appendUnique(dst.MCP, src.MCP...)
	}
}

// injectControlDispatcherAgents adds city-scoped and rig-scoped control-dispatcher
// agents and named sessions when formula_v2 is enabled and no explicit
// entry exists. Using named sessions ensures the reconciler reopens the
// existing session bead on restart instead of creating a new one (which
// would conflict on the session alias).
func injectControlDispatcherAgents(cfg *City, existing map[agentKey]bool) {
	if !cfg.Daemon.FormulaV2 {
		return
	}
	existingNS := make(map[string]bool, len(cfg.NamedSessions))
	for _, ns := range cfg.NamedSessions {
		existingNS[ns.QualifiedName()] = true
	}
	if !existing[agentKey{"", ControlDispatcherAgentName}] {
		cfg.Agents = append(cfg.Agents, newControlDispatcherAgent(""))
		if !existingNS[ControlDispatcherAgentName] {
			cfg.NamedSessions = append(cfg.NamedSessions, NamedSession{
				Template: ControlDispatcherAgentName,
				Mode:     "always",
			})
		}
	}
	for _, rig := range cfg.Rigs {
		if !existing[agentKey{rig.Name, ControlDispatcherAgentName}] {
			cfg.Agents = append(cfg.Agents, newControlDispatcherAgent(rig.Name))
			qn := rig.Name + "/" + ControlDispatcherAgentName
			if !existingNS[qn] {
				cfg.NamedSessions = append(cfg.NamedSessions, NamedSession{
					Template: ControlDispatcherAgentName,
					Dir:      rig.Name,
					Mode:     "always",
				})
			}
		}
	}
}

// newControlDispatcherAgent creates a control-dispatcher agent for the given scope.
func newControlDispatcherAgent(dir string) Agent {
	qualifiedName := ControlDispatcherAgentName
	if dir != "" {
		qualifiedName = dir + "/" + ControlDispatcherAgentName
	}
	one := 1
	a := Agent{
		Name:              ControlDispatcherAgentName,
		Dir:               dir,
		Description:       "Built-in deterministic graph.v2 workflow control worker",
		StartCommand:      ControlDispatcherStartCommandFor(qualifiedName),
		MaxActiveSessions: &one,
		Implicit:          true,
	}
	return a
}

// configuredProviders returns the merged set of providers that are explicitly
// configured: the union of cfg.Providers keys and cfg.Workspace.Provider.
// workspace.provider is only included if it names a built-in provider or one
// already defined in cfg.Providers — a non-builtin workspace.provider without
// a matching [providers.X] section is ignored (it would create an implicit
// agent that fails at resolution time).
func configuredProviders(cfg *City) map[string]ProviderSpec {
	merged := make(map[string]ProviderSpec, len(cfg.Providers)+1)
	for k, v := range cfg.Providers {
		merged[k] = v
	}
	if wp := cfg.Workspace.Provider; wp != "" {
		if _, ok := merged[wp]; !ok {
			// Only promote workspace.provider if it's a known builtin.
			if _, builtin := BuiltinProviders()[wp]; builtin {
				merged[wp] = ProviderSpec{}
			}
		}
	}
	return merged
}

// configuredProviderOrder returns provider names from the map in a
// deterministic order: built-in providers first (in canonical order),
// then any custom providers in sorted order.
func configuredProviderOrder(providers map[string]ProviderSpec) []string {
	builtins := BuiltinProviders()
	order := make([]string, 0, len(providers))

	// Built-in providers in canonical order.
	for _, name := range BuiltinProviderOrder() {
		if _, ok := providers[name]; ok {
			order = append(order, name)
		}
	}

	// Custom providers in sorted order.
	var custom []string
	for name := range providers {
		if _, ok := builtins[name]; !ok {
			custom = append(custom, name)
		}
	}
	sort.Strings(custom)
	order = append(order, custom...)

	return order
}

// ValidateAgents checks agent configurations for errors. It returns an error
// if any agent is missing required fields, has duplicate identities, or has
// invalid pool bounds. Uniqueness is keyed on (dir, name) — the same name
// in different dirs is allowed.
func ValidateAgents(agents []Agent) error {
	seen := make(map[agentKey]bool, len(agents))
	sourceOf := make(map[agentKey]string, len(agents))
	for i, a := range agents {
		if a.Name == "" {
			return fmt.Errorf("agent[%d]: name is required", i)
		}
		if !validAgentName.MatchString(a.Name) {
			return fmt.Errorf("agent %q: name must match [a-zA-Z0-9][a-zA-Z0-9_-]* (no spaces, slashes, or dots)", a.Name)
		}
		key := agentKey{dir: a.Dir, name: a.Name}
		if seen[key] {
			prev := sourceOf[key]
			curr := a.SourceDir
			if prev != "" || curr != "" {
				return fmt.Errorf("agent %q: duplicate name (from %q and %q)",
					a.QualifiedName(), prev, curr)
			}
			return fmt.Errorf("agent %q: duplicate name", a.QualifiedName())
		}
		seen[key] = true
		sourceOf[key] = a.SourceDir
		// Scope enum.
		switch a.Scope {
		case "", "city", "rig":
			// valid
		default:
			return fmt.Errorf("agent %q: scope must be \"city\", \"rig\", or empty, got %q", a.QualifiedName(), a.Scope)
		}
		// PromptMode enum.
		switch a.PromptMode {
		case "", "arg", "flag", "none":
			// valid
		default:
			return fmt.Errorf("agent %q: prompt_mode must be \"arg\", \"flag\", \"none\", or empty, got %q", a.QualifiedName(), a.PromptMode)
		}
		// PromptFlag required when prompt_mode = "flag".
		if a.PromptMode == "flag" && a.PromptFlag == "" {
			return fmt.Errorf("agent %q: prompt_flag is required when prompt_mode = \"flag\"", a.QualifiedName())
		}
		// WakeMode enum.
		switch a.WakeMode {
		case "", "resume", "fresh":
			// valid
		default:
			return fmt.Errorf("agent %q: wake_mode must be \"resume\", \"fresh\", or empty, got %q", a.QualifiedName(), a.WakeMode)
		}
		if a.MinActiveSessions != nil && *a.MinActiveSessions < 0 {
			return fmt.Errorf("agent %q: min_active_sessions must be >= 0", a.Name)
		}
		if a.MaxActiveSessions != nil && *a.MaxActiveSessions < -1 {
			return fmt.Errorf("agent %q: max_active_sessions must be >= -1 (use -1 for unlimited)", a.Name)
		}
		if a.MaxActiveSessions != nil && a.MinActiveSessions != nil &&
			*a.MaxActiveSessions >= 0 && *a.MinActiveSessions > *a.MaxActiveSessions {
			return fmt.Errorf("agent %q: min_active_sessions (%d) must be <= max_active_sessions (%d)",
				a.Name, *a.MinActiveSessions, *a.MaxActiveSessions)
		}
	}

	// Validate depends_on references and detect cycles.
	if err := validateDependsOn(agents); err != nil {
		return err
	}

	return nil
}

// ValidateNamedSessions checks named session declarations after pack expansion.
func ValidateNamedSessions(cfg *City) error {
	return validateNamedSessions(cfg, true)
}

// validateNamedSessions checks named session declarations for structural
// errors. When requireBackingTemplate is true, it also requires every named
// session to resolve to an expanded backing agent template.
func validateNamedSessions(cfg *City, requireBackingTemplate bool) error {
	if cfg == nil || len(cfg.NamedSessions) == 0 {
		return nil
	}
	type sessionKey struct{ dir, identity string }
	seen := make(map[sessionKey]bool, len(cfg.NamedSessions))
	reservedAliases := make(map[string]string, len(cfg.NamedSessions))
	reservedSessionNames := make(map[string]string, len(cfg.NamedSessions))
	agentsByTemplate := make(map[string]*Agent, len(cfg.Agents))
	for i := range cfg.Agents {
		agentsByTemplate[cfg.Agents[i].QualifiedName()] = &cfg.Agents[i]
	}
	alwaysByTemplate := make(map[string]int)
	for i := range cfg.NamedSessions {
		s := &cfg.NamedSessions[i]
		if s.Template == "" {
			return fmt.Errorf("named_session[%d]: template is required", i)
		}
		if !validNamedSessionTemplate.MatchString(s.Template) {
			return fmt.Errorf("named_session[%d]: template %q must match [a-zA-Z0-9][a-zA-Z0-9_-]* or binding.agent", i, s.Template)
		}
		if s.Name != "" && !validAgentName.MatchString(s.Name) {
			return fmt.Errorf("named_session[%d]: name %q must match [a-zA-Z0-9][a-zA-Z0-9_-]*", i, s.Name)
		}
		switch s.Scope {
		case "", "city", "rig":
			// valid
		default:
			return fmt.Errorf("named_session %q: scope must be \"city\", \"rig\", or empty, got %q", s.QualifiedName(), s.Scope)
		}
		switch s.ModeOrDefault() {
		case "on_demand", "always":
			// valid
		default:
			return fmt.Errorf("named_session %q: mode must be \"on_demand\", \"always\", or empty, got %q", s.QualifiedName(), s.Mode)
		}
		key := sessionKey{dir: s.Dir, identity: s.IdentityName()}
		if seen[key] {
			return fmt.Errorf("named_session %q: duplicate identity", s.QualifiedName())
		}
		seen[key] = true
		agent := agentsByTemplate[s.TemplateQualifiedName()]
		if agent == nil {
			if requireBackingTemplate {
				return fmt.Errorf("named_session %q: referenced template not found after pack expansion", s.QualifiedName())
			}
		}
		identity := s.QualifiedName()
		sessionName := NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
		if other, ok := reservedAliases[sessionName]; ok && other != identity {
			return fmt.Errorf(
				"named_session %q: reserved alias collides with deterministic session_name for %q (%q)",
				identity, other, sessionName,
			)
		}
		if other, ok := reservedSessionNames[identity]; ok && other != identity {
			return fmt.Errorf(
				"named_session %q: reserved alias collides with deterministic session_name for %q (%q)",
				identity, other, identity,
			)
		}
		if other, ok := reservedSessionNames[sessionName]; ok && other != identity {
			return fmt.Errorf(
				"named_session %q: deterministic session_name %q collides with configured named session %q",
				identity, sessionName, other,
			)
		}
		reservedAliases[identity] = identity
		reservedSessionNames[sessionName] = identity
		if s.ModeOrDefault() == "always" && agent != nil {
			alwaysByTemplate[agent.QualifiedName()]++
			if maxActive := agent.EffectiveMaxActiveSessions(); maxActive != nil && *maxActive < alwaysByTemplate[agent.QualifiedName()] {
				return fmt.Errorf(
					"named_session %q: mode %q exceeds max_active_sessions capacity %d on template %q",
					s.QualifiedName(), s.ModeOrDefault(), *maxActive, agent.QualifiedName(),
				)
			}
			policy := ResolveSessionSleepPolicy(cfg, agent)
			if normalized := NormalizeSleepAfterIdle(policy.Value); normalized != "" && normalized != SessionSleepOff {
				return fmt.Errorf(
					"named_session %q: mode %q is incompatible with sleep_after_idle=%q on template %q",
					s.QualifiedName(), s.ModeOrDefault(), normalized, agent.QualifiedName(),
				)
			}
		}
	}
	return nil
}

// validateDependsOn checks that all depends_on references are valid agent
// names and that the dependency graph is acyclic.
//
// Note: this runs before pool expansion, so depends_on must reference
// template names (e.g. "worker"), not pool instance names (e.g. "worker-1").
// Pool instances inherit their template's dependencies via deep-copy.
func validateDependsOn(agents []Agent) error {
	names := make(map[string]bool, len(agents))
	for _, a := range agents {
		names[a.QualifiedName()] = true
	}

	// Check all references exist.
	for _, a := range agents {
		for _, dep := range a.DependsOn {
			if !names[dep] {
				return fmt.Errorf("agent %q: depends_on references unknown agent %q", a.QualifiedName(), dep)
			}
			if dep == a.QualifiedName() {
				return fmt.Errorf("agent %q: depends_on contains self-reference", a.QualifiedName())
			}
		}
	}

	// Detect cycles via DFS with visiting/visited coloring.
	const (
		white = 0 // unvisited
		gray  = 1 // visiting (on current path)
		black = 2 // visited (fully explored)
	)
	color := make(map[string]int, len(agents))
	adj := make(map[string][]string, len(agents))
	for _, a := range agents {
		adj[a.QualifiedName()] = a.DependsOn
	}

	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		for _, dep := range adj[name] {
			switch color[dep] {
			case gray:
				return fmt.Errorf("agent %q: dependency cycle detected (%s -> %s)", name, name, dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[name] = black
		return nil
	}

	for _, a := range agents {
		n := a.QualifiedName()
		if color[n] == white {
			if err := visit(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateRigs checks rig configurations for errors. It returns an error if
// any rig is missing required fields, has duplicate names, or has colliding
// prefixes. The hqPrefix is the city's HQ prefix for collision checks.
func ValidateRigs(rigs []Rig, hqPrefix string) error {
	seenNames := make(map[string]bool, len(rigs))
	seenPrefixes := make(map[string]string) // lowercase prefix → rig name (for error messages)

	// HQ prefix participates in collision detection.
	// Lowercase to match runtime lookup (findRigByPrefix is case-insensitive).
	seenPrefixes[strings.ToLower(hqPrefix)] = "HQ"

	for i, r := range rigs {
		if r.Name == "" {
			return fmt.Errorf("rig[%d]: name is required", i)
		}
		if r.Path == "" {
			return fmt.Errorf("rig %q: path is required", r.Name)
		}
		if seenNames[r.Name] {
			return fmt.Errorf("rig %q: duplicate name", r.Name)
		}
		seenNames[r.Name] = true

		prefix := strings.ToLower(r.EffectivePrefix())
		if other, ok := seenPrefixes[prefix]; ok {
			return fmt.Errorf("rig %q: prefix %q collides with %s", r.Name, prefix, other)
		}
		seenPrefixes[prefix] = r.Name
	}
	return nil
}

// DefaultCity returns a City with the given name and a single default
// agent named "mayor". This is the config written by "gc init".
func DefaultCity(name string) City {
	return City{
		Workspace:     Workspace{Name: name},
		Agents:        []Agent{{Name: "mayor", PromptTemplate: "prompts/mayor.md"}},
		NamedSessions: []NamedSession{{Template: "mayor", Mode: "always"}},
	}
}

func defaultInstallAgentHooksForProvider(provider string) []string {
	switch strings.TrimSpace(provider) {
	case "opencode":
		return []string{"opencode"}
	default:
		return nil
	}
}

// WizardCity returns a City with the given name, a workspace-level provider
// or start command, and one agent (mayor). This is the config written by
// "gc init" when the interactive wizard runs. If startCommand is set, it
// takes precedence over provider.
func WizardCity(name, provider, startCommand string) City {
	ws := Workspace{Name: name}
	if startCommand != "" {
		ws.StartCommand = startCommand
	} else {
		ws.Provider = provider
		ws.InstallAgentHooks = defaultInstallAgentHooksForProvider(provider)
	}
	return City{
		Workspace: ws,
		Agents: []Agent{
			{Name: "mayor", PromptTemplate: "prompts/mayor.md"},
		},
		NamedSessions: []NamedSession{{Template: "mayor", Mode: "always"}},
	}
}

// GastownCity returns a City configured for the gastown orchestration pack.
// Agents come from the pack (packs/gastown); no inline agents are defined.
// Sets workspace.includes, default_rig_includes, global_fragments, and daemon
// config. If startCommand is set, it takes precedence over provider.
func GastownCity(name, provider, startCommand string) City {
	ws := Workspace{
		Name:            name,
		GlobalFragments: []string{"command-glossary", "operational-awareness"},
	}
	if startCommand != "" {
		ws.StartCommand = startCommand
	} else if provider != "" {
		ws.Provider = provider
		ws.InstallAgentHooks = defaultInstallAgentHooksForProvider(provider)
	}
	maxRestarts := 5
	return City{
		Workspace: ws,
		Imports: map[string]Import{
			"gastown": {Source: ".gc/system/packs/gastown"},
		},
		DefaultRigImports: map[string]Import{
			"gastown": {Source: ".gc/system/packs/gastown"},
		},
		DefaultRigImportOrder: []string{"gastown"},
		Daemon: DaemonConfig{
			PatrolInterval:  "30s",
			MaxRestarts:     &maxRestarts,
			RestartWindow:   "1h",
			ShutdownTimeout: "5s",
		},
	}
}

// Marshal encodes a City to TOML bytes.
func (c *City) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("marshaling config: %w", err)
	}
	return buf.Bytes(), nil
}

// MarshalForWrite emits the checked-in city.toml form by stripping
// machine-local rig path bindings before encoding.
func (c *City) MarshalForWrite() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("marshaling config: nil city")
	}
	clone := *c
	if len(c.Rigs) > 0 {
		clone.Rigs = append([]Rig(nil), c.Rigs...)
		for i := range clone.Rigs {
			clone.Rigs[i].Path = ""
		}
	}
	return clone.Marshal()
}

// Load reads and parses a city.toml file at the given path using the
// provided filesystem. All file I/O goes through fs for testability.
func Load(fs fsys.FS, path string) (*City, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := ResolveWorkspaceIdentity(fs, filepath.Dir(path), cfg); err != nil {
		return nil, err
	}
	// Load intentionally skips include and pack expansion, so validate the
	// direct named-session declarations without requiring pack-provided
	// backing templates to be present yet.
	if err := validateNamedSessions(cfg, false); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse decodes TOML data into a City config.
func Parse(data []byte) (*City, error) {
	cfg := City{}
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	normalizeAgentDefaultsAlias(&cfg, md)
	normalizeLegacyOrderOverrideAliases(&cfg)
	NormalizeSessionSleepFields(&cfg)
	// Backwards compat: promote deprecated graph_workflows → formula_v2.
	if cfg.Daemon.GraphWorkflows && !cfg.Daemon.FormulaV2 {
		cfg.Daemon.FormulaV2 = true
	}
	return &cfg, nil
}
