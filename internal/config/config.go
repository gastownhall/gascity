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

const (
	// WorkflowControlAgentName is the built-in deterministic control lane for
	// graph.v2 workflow control beads.
	WorkflowControlAgentName = "workflow-control"
	// WorkflowControlStartCommand runs the built-in workflow control worker.
	// Wrapped in `sh -c` so any appended prompt suffix is ignored as $0.
	// The control lane is kept resident and blocks on workflow-relevant city
	// events instead of exiting after each one-shot drain.
	WorkflowControlStartCommand = `sh -c 'export GC_WORKFLOW_TRACE="${GC_WORKFLOW_TRACE:-${GC_CITY}/workflow-control-trace.log}"; exec "${GC_BIN:-gc}" workflow serve --follow ` + WorkflowControlAgentName + `'`
	// WorkflowControlPoolLabel is the city-scoped pool label for workflow
	// control beads. The control lane is intentionally city-scoped because
	// graph.v2 workflow beads live in the city store.
	// WorkflowControlPoolLabel is deprecated — routing now uses gc.routed_to
	// metadata. Retained for backward compatibility with existing labeled beads.
	WorkflowControlPoolLabel = "pool:" + WorkflowControlAgentName
)

// WorkflowControlStartCommandFor returns the start command for a
// workflow-control agent with the given qualified name.
func WorkflowControlStartCommandFor(qualifiedName string) string {
	return `sh -c 'export GC_WORKFLOW_TRACE="${GC_WORKFLOW_TRACE:-${GC_CITY}/workflow-control-trace.log}"; exec "${GC_BIN:-gc}" workflow serve --follow ` + qualifiedName + `'`
}

// QualifiedName returns the agent's canonical identity.
// Rig-scoped: "hello-world/polecat". City-wide: "mayor".
func (a *Agent) QualifiedName() string {
	if a.Dir == "" {
		return a.Name
	}
	return a.Dir + "/" + a.Name
}

// ParseQualifiedName splits an agent identity into (dir, name).
// "hello-world/polecat" → ("hello-world", "polecat").
// "mayor" → ("", "mayor").
func ParseQualifiedName(identity string) (dir, name string) {
	if i := strings.LastIndex(identity, "/"); i >= 0 {
		return identity[:i], identity[i+1:]
	}
	return "", identity
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
	// Packs defines named remote pack sources fetched via git.
	Packs map[string]PackSource `toml:"packs,omitempty"`
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
	// AgentDefaults provides default values applied to all agents that
	// don't override them. Useful for setting city-wide model, wake_mode,
	// and overlay allowlists.
	AgentDefaults AgentDefaults `toml:"agent_defaults,omitempty"`
	// ResolvedWorkspaceName is the effective city name derived from the
	// config file path when workspace.name is omitted. Runtime-only.
	ResolvedWorkspaceName string `toml:"-" json:"-"`

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
	// RigPackDirs maps rig name to its ordered pack directories.
	// Used when rig packs differ from city packs.
	// Populated during pack expansion. Not from TOML.
	RigPackDirs map[string][]string `toml:"-" json:"-"`
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
}

// NamedSession defines a canonical persistent session backed by an agent
// template. Unlike Agent, it does not carry behavior itself; it only
// declares runtime identity and controller policy.
type NamedSession struct {
	// Template is the referenced agent template name.
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
}

// QualifiedName returns the canonical identity of the named session.
func (s *NamedSession) QualifiedName() string {
	if s == nil || s.Dir == "" {
		if s == nil {
			return ""
		}
		return s.Template
	}
	return s.Dir + "/" + s.Template
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
	Path string `toml:"path" jsonschema:"required"`
	// Prefix overrides the auto-derived bead ID prefix for this rig.
	Prefix string `toml:"prefix,omitempty"`
	// Suspended prevents the reconciler from spawning agents in this rig. Toggle with gc rig suspend/resume.
	Suspended bool `toml:"suspended,omitempty"`
	// FormulasDir is a rig-local formula directory (Layer 4). Overrides
	// pack formulas for this rig by filename.
	// Relative paths resolve against the city directory.
	FormulasDir string `toml:"formulas_dir,omitempty"`
	// Includes lists pack directories or URLs for this rig.
	// Replaces the older pack/packs fields. Each entry is a
	// local path, a git source//sub#ref URL, or a GitHub tree URL.
	Includes []string `toml:"includes,omitempty"`
	// MaxActiveSessions is the rig-level cap on total concurrent sessions across
	// all agents in this rig. Nil means inherit from workspace (or unlimited).
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// Overrides are per-agent patches applied after pack expansion.
	Overrides []AgentOverride `toml:"overrides,omitempty"`
	// DefaultSlingTarget is the agent qualified name used when gc sling is
	// invoked with only a bead ID (no explicit target). Resolved via
	// resolveAgentIdentity. Example: "rig/polecat"
	DefaultSlingTarget string `toml:"default_sling_target,omitempty"`
	// SessionSleep overrides workspace-level idle sleep defaults for agents in
	// this rig.
	SessionSleep SessionSleepConfig `toml:"session_sleep,omitempty"`
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
	// Pool overrides pool configuration fields.
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
	// HooksInstalled overrides automatic hook detection.
	HooksInstalled *bool `toml:"hooks_installed,omitempty"`
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
	// Includes lists other packs to compose into this one.
	// Each entry is a local relative path (e.g. "../maintenance") or a
	// remote git URL (SSH or HTTPS) with optional //subpath and #ref.
	Includes []string `toml:"includes,omitempty"`
	// Requires declares agents that must exist in the expanded config
	// for this pack's formulas/orders to function. Validated
	// after all packs are expanded.
	Requires []PackRequirement `toml:"requires,omitempty"`
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
// Uses the explicit workspace Prefix if set, otherwise derives one from
// the city name (falling back to ResolvedWorkspaceName when the TOML
// name field is omitted).
func EffectiveHQPrefix(cfg *City) string {
	if cfg.Workspace.Prefix != "" {
		return cfg.Workspace.Prefix
	}
	name := cfg.Workspace.Name
	if name == "" {
		name = cfg.ResolvedWorkspaceName
	}
	return DeriveBeadsPrefix(name)
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
	// Gate overrides the gate type.
	Gate *string `toml:"gate,omitempty"`
	// Interval overrides the cooldown interval. Go duration string.
	Interval *string `toml:"interval,omitempty"`
	// Schedule overrides the cron expression.
	Schedule *string `toml:"schedule,omitempty"`
	// Check overrides the condition gate check command.
	Check *string `toml:"check,omitempty"`
	// On overrides the event gate event type.
	On *string `toml:"on,omitempty"`
	// Pool overrides the target agent/pool.
	Pool *string `toml:"pool,omitempty"`
	// Timeout overrides the per-order timeout. Go duration string.
	Timeout *string `toml:"timeout,omitempty"`
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
	// GraphWorkflows enables formula v2 graph workflow infrastructure:
	// the workflow-control implicit agent, graph.v2 formula compilation,
	// and batch graph-apply bead creation. Requires bd with --graph support.
	// Default: false (opt-in while the feature stabilizes).
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

// AgentDefaults provides default values applied to all agents that don't
// explicitly override them. Declared once at the city level via
// [agent_defaults] in city.toml.
//
// NOTE: This is a config-only scaffold for Phase 1. Runtime merging of
// defaults into individual agents is wired in Phase 2 (PR 2c). Until
// then, these values are parsed and composed but not consumed at runtime.
type AgentDefaults struct {
	// Model is the default model name for agents (e.g., "claude-sonnet-4-6").
	// Agents with their own model override take precedence.
	Model string `toml:"model,omitempty"`
	// WakeMode is the default wake mode ("resume" or "fresh").
	WakeMode string `toml:"wake_mode,omitempty" jsonschema:"enum=resume,enum=fresh"`
	// AllowOverlay lists template fields that sessions may override at
	// creation time (e.g., ["model", "prompt", "title"]).
	AllowOverlay []string `toml:"allow_overlay,omitempty"`
	// AllowEnvOverride lists environment variable names that sessions may
	// override at creation time. Names must match ^[A-Z][A-Z0-9_]{0,127}$.
	AllowEnvOverride []string `toml:"allow_env_override,omitempty"`
}

// PoolConfig defines elastic pool parameters for an agent. When present
// on an Agent, that agent becomes a pool with scaling behavior.
type PoolConfig struct {
	// Min is the minimum number of pool instances. Defaults to 0.
	Min int `toml:"min,omitempty" jsonschema:"minimum=0,default=0"`
	// Max is the maximum number of pool instances. 0 means the pool is
	// disabled (no instances will be created). -1 means unlimited (the
	// check command's output determines scale with no upper cap).
	// Defaults to 0.
	Max int `toml:"max,omitempty" jsonschema:"minimum=-1,default=0"`
	// Check is a shell command whose output determines desired pool size. Defaults to "echo 1".
	Check string `toml:"check,omitempty" jsonschema:"default=echo 1"`
	// DrainTimeout is the maximum time to wait for a pool instance to finish its
	// current work before force-killing it. Duration string (e.g., "5m", "30m", "1h").
	// Defaults to "5m".
	DrainTimeout string `toml:"drain_timeout,omitempty" jsonschema:"default=5m"`
	// OnDeath is a shell command run when a pool instance dies.
	// Default: unclaims in_progress beads assigned to the dead instance.
	OnDeath string `toml:"on_death,omitempty"`
	// OnBoot is a shell command run once at controller startup for each pool.
	// Default: unclaims all in_progress beads labeled for this pool.
	OnBoot string `toml:"on_boot,omitempty"`
	// Namepool is the path to a plain text file with one name per line.
	// When set, pool instances use names from the file instead of numeric
	// suffixes (e.g., "furiosa" instead of "polecat-1"). The path is
	// resolved via adjustFragmentPath (relative to pack dir, absolute, or
	// "//" for city-root-relative). Requires a bounded pool (max > 0).
	Namepool string `toml:"namepool,omitempty"`
	// NamepoolNames holds names loaded from the Namepool file at config load
	// time. Not serialized to TOML.
	NamepoolNames []string `toml:"-"`
}

// DrainTimeoutDuration returns the drain timeout as a time.Duration.
// Defaults to 5m if empty or unparseable.
func (p *PoolConfig) DrainTimeoutDuration() time.Duration {
	if p.DrainTimeout == "" {
		return 5 * time.Minute
	}
	dur, err := time.ParseDuration(p.DrainTimeout)
	if err != nil {
		return 5 * time.Minute
	}
	return dur
}

// IsUnlimited returns true if the pool has no upper bound (max < 0).
func (p PoolConfig) IsUnlimited() bool { return p.Max < 0 }

// IsMultiInstance returns true if the pool can have more than one instance.
// This covers both bounded pools (max > 1) and unlimited pools (max < 0).
func (p PoolConfig) IsMultiInstance() bool { return p.Max > 1 || p.Max < 0 }

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
	// MaxActiveSessions is the agent-level cap on concurrent sessions.
	// Nil means inherit from rig, then workspace, then unlimited.
	// Replaces pool.max.
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// MinActiveSessions is the minimum number of sessions to keep alive.
	// Agent-level only. Counts against rig/workspace caps. Replaces pool.min.
	MinActiveSessions *int `toml:"min_active_sessions,omitempty"`
	// ScaleCheck is a shell command whose output determines desired session count.
	// Optional override — when set, its output is the desired count (still clamped
	// by all cap levels). Replaces pool.check.
	ScaleCheck string `toml:"scale_check,omitempty"`
	// Pool configures elastic pool behavior (deprecated — use max_active_sessions,
	// min_active_sessions, scale_check instead). When set, values are used as
	// fallbacks for the new fields.
	Pool *PoolConfig `toml:"pool,omitempty"`
	// WorkQuery is the shell command to find available work for this agent.
	// Used by gc hook and available in prompt templates as {{.WorkQuery}}.
	// Default for fixed agents: "bd ready --assignee=<qualified-name>".
	// Default for pool agents:
	// "bd ready --metadata-field gc.routed_to=<qualified-name> --unassigned --json --limit=1 2>/dev/null".
	// Override to integrate with external task systems.
	WorkQuery string `toml:"work_query,omitempty"`
	// SlingQuery is the command template to route a bead to this agent/pool.
	// Used by gc sling to make a bead visible to the target's work_query.
	// The placeholder {} is replaced with the bead ID at runtime.
	// Default for fixed agents: "bd update {} --assignee=<qualified-name>".
	// Default for pool agents: "bd update {} --add-label=pool:<qualified-name>".
	// Pool agents must set both sling_query and work_query, or neither.
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
	// Implicit marks agents auto-generated from built-in providers.
	// These have pool min=0, max=-1 and are available as sling targets
	// even without an explicit [[agent]] entry in city.toml.
	// Runtime-only — not persisted to TOML or JSON.
	Implicit bool `toml:"-" json:"-"`
	// DefaultSlingFormula is the formula name automatically applied via --on
	// when beads are slung to this agent, unless --no-formula is set.
	// Example: "mol-polecat-work"
	DefaultSlingFormula string `toml:"default_sling_formula,omitempty"`
	// InjectFragments lists named template fragments to append to this agent's
	// rendered prompt. Fragments come from shared template directories across
	// all loaded packs. Each name must match a {{ define "name" }} block.
	InjectFragments []string `toml:"inject_fragments,omitempty"`
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
	// expansion. Pool instances use this for label-based work discovery
	// (e.g., pool:dog) rather than their instance name (e.g., pool:dog-1).
	PoolName string `toml:"-"`
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
// If WorkQuery is set, returns it as-is. Otherwise returns the default:
//   - Pool agents: "bd ready --label=pool:<pool-name> --unassigned --json --limit=1 2>/dev/null"
//   - Fixed agents: "bd ready --assignee=$GC_SESSION_NAME"
//
// Fixed agents use the $GC_SESSION_NAME env var (set by the reconciler
// and by gc hook when invoked with a positional agent name) so each
// session has its own work queue. Clones of a template don't race for
// work — each checks only its own queue.
//
// Pool instances use PoolName (the template's qualified name) so all
// instances in the pool search with the same label (e.g., pool:dog)
// rather than their instance name (e.g., pool:dog-1).
func (a *Agent) EffectiveWorkQuery() string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	if a.IsPool() {
		// Default: find unassigned ready beads routed to this pool template.
		// Pool instances use PoolName so all instances share the same queue.
		target := a.QualifiedName()
		if a.PoolName != "" {
			target = a.PoolName
		}
		return "bd ready --metadata-field gc.routed_to=" + target + " --unassigned --json --limit=1 2>/dev/null"
	}
	return `bd ready --assignee="$GC_SESSION_NAME" --json --limit=1 2>/dev/null`
}

// EffectiveSlingQuery returns the sling query command template for this agent.
// The template uses {} as a placeholder for the bead ID.
// If SlingQuery is set, returns it as-is. Otherwise returns the default:
//   - Pool agents: "bd update {} --set-metadata gc.routed_to=<template>"
//   - Fixed agents: "bd update {} --assignee=$GC_SLING_TARGET"
//
// Fixed agents use $GC_SLING_TARGET, which is set by gc sling to the
// resolved session name of the target. This gives each session its own
// work queue — clones don't race for the same assignments.
func (a *Agent) EffectiveSlingQuery() string {
	if a.SlingQuery != "" {
		return a.SlingQuery
	}
	if a.IsPool() {
		return "bd update {} --set-metadata gc.routed_to=" + a.QualifiedName()
	}
	return "bd update {} --assignee=$GC_SLING_TARGET"
}

// EffectivePool returns the pool configuration for this agent, applying
// defaults. If Pool is nil, returns an always-on singleton (min=1, max=1,
// check="echo 1"). If Pool is set and Check is empty, generates a default
// that counts open beads labeled with the agent's qualified name — consistent
// with EffectiveSlingQuery and EffectiveWorkQuery.
func (a *Agent) EffectivePool() PoolConfig {
	if a.Pool == nil {
		return PoolConfig{Min: 1, Max: 1, Check: "echo 1"}
	}
	p := *a.Pool
	if p.Check == "" {
		p.Check = a.defaultPoolCheck()
	}
	return p
}

// defaultPoolCheck returns the default pool check command that counts
// actionable work for this pool. Uses bd ready (blocker-aware) for open
// beads plus bd list --status=in_progress for claimed work. Routes via
// gc.routed_to metadata.
func (a *Agent) defaultPoolCheck() string {
	template := a.QualifiedName()
	return `ready=$(bd ready --metadata-field gc.routed_to=` + template +
		` --unassigned --json 2>/dev/null | jq 'length' 2>/dev/null); ` +
		`active=$(bd list --metadata-field gc.routed_to=` + template +
		` --status=in_progress --json 2>/dev/null | jq 'length' 2>/dev/null); ` +
		`echo "$(( ${ready:-0} + ${active:-0} ))" || echo 0`
}

// IsPool reports whether this agent has explicit pool configuration.
func (a *Agent) IsPool() bool {
	return a.Pool != nil
}

// EffectiveMaxActiveSessions returns the agent's max active sessions.
// Priority: agent.MaxActiveSessions > pool.Max > nil (unlimited).
func (a *Agent) EffectiveMaxActiveSessions() *int {
	if a.MaxActiveSessions != nil {
		return a.MaxActiveSessions
	}
	if a.Pool != nil {
		max := a.Pool.Max
		return &max
	}
	return nil
}

// EffectiveMinActiveSessions returns the agent's min active sessions.
// Priority: agent.MinActiveSessions > pool.Min > 0.
func (a *Agent) EffectiveMinActiveSessions() int {
	if a.MinActiveSessions != nil && *a.MinActiveSessions > 0 {
		return *a.MinActiveSessions
	}
	if a.Pool != nil {
		return a.Pool.Min
	}
	return 0
}

// EffectiveScaleCheck returns the agent's scale check command.
// Priority: agent.ScaleCheck > pool.Check > "".
func (a *Agent) EffectiveScaleCheck() string {
	if a.ScaleCheck != "" {
		return a.ScaleCheck
	}
	if a.Pool != nil && a.Pool.Check != "" {
		return a.Pool.Check
	}
	return ""
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

// EffectiveOnDeath returns the on_death command for this pool agent.
// Default: unclaims in_progress beads assigned to this agent.
func (a *Agent) EffectiveOnDeath() string {
	pool := a.EffectivePool()
	if pool.OnDeath != "" {
		return pool.OnDeath
	}
	if !a.IsPool() {
		return ""
	}
	return a.defaultOnDeath()
}

func (a *Agent) defaultOnDeath() string {
	return `bd list --assignee=` + a.QualifiedName() +
		` --status=in_progress --json 2>/dev/null | ` +
		`jq -r '.[].id' 2>/dev/null | ` +
		`xargs -rI{} bd update {} --unclaim 2>/dev/null`
}

// EffectiveOnBoot returns the on_boot command for this pool agent.
// Default: unclaims all in_progress beads labeled for this pool.
func (a *Agent) EffectiveOnBoot() string {
	pool := a.EffectivePool()
	if pool.OnBoot != "" {
		return pool.OnBoot
	}
	if !a.IsPool() {
		return ""
	}
	return a.defaultOnBoot()
}

func (a *Agent) defaultOnBoot() string {
	label := a.QualifiedName()
	if a.PoolName != "" {
		label = a.PoolName
	}
	return `bd list --label=pool:` + label +
		` --status=in_progress --json 2>/dev/null | ` +
		`jq -r '.[].id' 2>/dev/null | ` +
		`xargs -rI{} bd update {} --unclaim 2>/dev/null`
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

func InjectImplicitAgents(cfg *City) {
	// Build set of existing agent keys (dir, name).
	existing := make(map[agentKey]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		existing[agentKey{a.Dir, a.Name}] = true
	}

	configured := configuredProviders(cfg)
	if len(configured) == 0 {
		injectWorkflowControlAgents(cfg, existing)
		return
	}

	// Deterministic order: built-in providers first (in canonical order),
	// then any custom providers in sorted order.
	providers := configuredProviderOrder(configured)

	promptTemplate := citylayout.PromptsRoot + "/pool-worker.md"

	// City-scoped implicit agents.
	for _, name := range providers {
		if existing[agentKey{"", name}] {
			continue
		}
		cfg.Agents = append(cfg.Agents, Agent{
			Name:                name,
			Provider:            name,
			PromptTemplate:      promptTemplate,
			Pool:                &PoolConfig{Min: 0, Max: -1},
			DefaultSlingFormula: "mol-do-work",
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
				Pool:                &PoolConfig{Min: 0, Max: -1},
				DefaultSlingFormula: "mol-do-work",
				Implicit:            true,
			})
		}
	}
	injectWorkflowControlAgents(cfg, existing)
}

// injectWorkflowControlAgents adds city-scoped and rig-scoped workflow-control
// agents when graph_workflows is enabled and no explicit entry exists.
func injectWorkflowControlAgents(cfg *City, existing map[agentKey]bool) {
	if !cfg.Daemon.GraphWorkflows {
		return
	}
	if !existing[agentKey{"", WorkflowControlAgentName}] {
		cfg.Agents = append(cfg.Agents, newWorkflowControlAgent(""))
	}
	for _, rig := range cfg.Rigs {
		if !existing[agentKey{rig.Name, WorkflowControlAgentName}] {
			cfg.Agents = append(cfg.Agents, newWorkflowControlAgent(rig.Name))
		}
	}
}

// newWorkflowControlAgent creates a workflow-control agent for the given scope.
func newWorkflowControlAgent(dir string) Agent {
	qualifiedName := WorkflowControlAgentName
	if dir != "" {
		qualifiedName = dir + "/" + WorkflowControlAgentName
	}
	a := Agent{
		Name:         WorkflowControlAgentName,
		Dir:          dir,
		Description:  "Built-in deterministic graph.v2 workflow control worker",
		StartCommand: WorkflowControlStartCommandFor(qualifiedName),
		Pool:         &PoolConfig{Min: 1, Max: 1},
		Implicit:     true,
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
		if a.Pool != nil {
			if a.Pool.Min < 0 {
				return fmt.Errorf("agent %q: pool min must be >= 0", a.Name)
			}
			if a.Pool.Max < -1 {
				return fmt.Errorf("agent %q: pool max must be >= -1 (use -1 for unlimited)", a.Name)
			}
			if a.Pool.Max >= 0 && a.Pool.Min > a.Pool.Max {
				return fmt.Errorf("agent %q: pool min (%d) must be <= max (%d)", a.Name, a.Pool.Min, a.Pool.Max)
			}
			// Pool agents: sling_query and work_query must be both set or both unset.
			hasSQ := a.SlingQuery != ""
			hasWQ := a.WorkQuery != ""
			if hasSQ != hasWQ {
				return fmt.Errorf("agent %q: pool agents must set both sling_query and work_query, or neither (got sling_query=%v, work_query=%v)",
					a.QualifiedName(), hasSQ, hasWQ)
			}
		}
	}

	// Validate depends_on references and detect cycles.
	if err := validateDependsOn(agents); err != nil {
		return err
	}

	return nil
}

// ValidateNamedSessions checks named session declarations for structural
// errors and cross-references against the expanded agent set.
func ValidateNamedSessions(cfg *City) error {
	if cfg == nil || len(cfg.NamedSessions) == 0 {
		return nil
	}
	type sessionKey struct{ dir, template string }
	seen := make(map[sessionKey]bool, len(cfg.NamedSessions))
	reservedAliases := make(map[string]string, len(cfg.NamedSessions))
	reservedSessionNames := make(map[string]string, len(cfg.NamedSessions))
	agentsByTemplate := make(map[string]*Agent, len(cfg.Agents))
	for i := range cfg.Agents {
		agentsByTemplate[cfg.Agents[i].QualifiedName()] = &cfg.Agents[i]
	}
	for i := range cfg.NamedSessions {
		s := &cfg.NamedSessions[i]
		if s.Template == "" {
			return fmt.Errorf("named_session[%d]: template is required", i)
		}
		if !validAgentName.MatchString(s.Template) {
			return fmt.Errorf("named_session[%d]: template %q must match [a-zA-Z0-9][a-zA-Z0-9_-]*", i, s.Template)
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
		key := sessionKey{dir: s.Dir, template: s.Template}
		if seen[key] {
			return fmt.Errorf("named_session %q: duplicate identity", s.QualifiedName())
		}
		seen[key] = true
		agent := agentsByTemplate[s.QualifiedName()]
		if agent == nil {
			return fmt.Errorf("named_session %q: referenced template not found after pack expansion", s.QualifiedName())
		}
		if agent.IsPool() {
			return fmt.Errorf("named_session %q: referenced template %q is a pool; use the pool directly", s.QualifiedName(), agent.QualifiedName())
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
		if s.ModeOrDefault() == "always" {
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
	seenPrefixes := make(map[string]string) // prefix → rig name (for error messages)

	// HQ prefix participates in collision detection.
	seenPrefixes[hqPrefix] = "HQ"

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

		prefix := r.EffectivePrefix()
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
		NamedSessions: []NamedSession{{Template: "mayor"}},
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
	}
	return City{
		Workspace: ws,
		Agents: []Agent{
			{Name: "mayor", PromptTemplate: "prompts/mayor.md"},
		},
		NamedSessions: []NamedSession{{Template: "mayor"}},
	}
}

// GastownCity returns a City configured for the gastown orchestration pack.
// Agents come from the pack (packs/gastown); no inline agents are defined.
// Sets workspace.includes, default_rig_includes, global_fragments, and daemon
// config. If startCommand is set, it takes precedence over provider.
func GastownCity(name, provider, startCommand string) City {
	ws := Workspace{
		Name:               name,
		Includes:           []string{"packs/gastown"},
		DefaultRigIncludes: []string{"packs/gastown"},
		GlobalFragments:    []string{"command-glossary", "operational-awareness"},
	}
	if startCommand != "" {
		ws.StartCommand = startCommand
	} else if provider != "" {
		ws.Provider = provider
	}
	maxRestarts := 5
	return City{
		Workspace: ws,
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
	cfg.ResolvedWorkspaceName = filepath.Base(filepath.Dir(path))
	return cfg, nil
}

// Parse decodes TOML data into a City config.
func Parse(data []byte) (*City, error) {
	cfg := City{}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	NormalizeSessionSleepFields(&cfg)
	return &cfg, nil
}
