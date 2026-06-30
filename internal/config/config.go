// Package config handles loading and parsing city.toml configuration files.
package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/pricing"
	"github.com/gastownhall/gascity/internal/remotesource"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// validAgentName matches names safe for use in session identifiers.
// Must start with a letter or digit, followed by letters, digits, hyphens,
// or underscores. Slashes, spaces, and dots are not allowed.
var validAgentName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// validNamedSessionTemplate matches either a bare agent name ("mayor") or a
// import-qualified template ("gastown.mayor"). Rig qualification is
// carried separately in NamedSession.Dir, so slashes remain invalid here.
var validNamedSessionTemplate = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*(\.[a-zA-Z0-9][a-zA-Z0-9_-]*)?$`)

const (
	// ControlDispatcherAgentName is the built-in deterministic control lane for
	// graph.v2 workflow control beads.
	ControlDispatcherAgentName = "control-dispatcher"
	// controlDispatcherDefaultTracePathExpr is the watcher-safe default trace
	// target for the control-dispatcher. The controller ignores the hidden .gc
	// subtree recursively, so defaults must stay under it to avoid self-triggered
	// config-watch churn. The trace intentionally stays a flat, well-known file
	// under .gc/runtime because operators and tests tail a single canonical path.
	//
	// Per-dispatcher distinction (closes #1650) is layered on top in
	// cmd/gc/template_resolve.go at session-spawn time: agentEnv there
	// overrides GC_CONTROL_DISPATCHER_TRACE_DEFAULT with a per-name absolute
	// path, which the ${GC_CONTROL_DISPATCHER_TRACE_DEFAULT:-...} expression
	// below consumes. The shell template stays uniform so the trust decision
	// lives in Go.
	controlDispatcherDefaultTracePathExpr = `${GC_CONTROL_DISPATCHER_TRACE_DEFAULT:-${GC_CITY}/` + citylayout.RuntimeDataRoot + `/control-dispatcher-trace.log}`
	// controlDispatcherTraceInit exports the resolved trace path. Explicit
	// GC_WORKFLOW_TRACE overrides win first; otherwise the runtime injects a
	// precomputed watcher-safe default trace path for the current city/session.
	controlDispatcherTraceInit = `export GC_WORKFLOW_TRACE="${GC_WORKFLOW_TRACE:-` + controlDispatcherDefaultTracePathExpr + `}"`
	// controlDispatcherTraceDirInit creates the parent directory for the
	// resolved trace path. This preserves explicit GC_WORKFLOW_TRACE overrides
	// instead of unconditionally depending on the default runtime root.
	controlDispatcherTraceDirInit = `trace_dir="${GC_WORKFLOW_TRACE%/*}"; if [ "$trace_dir" = "$GC_WORKFLOW_TRACE" ]; then trace_dir="."; elif [ -z "$trace_dir" ]; then trace_dir="/"; fi; mkdir -p "$trace_dir"`
	// ControlDispatcherStartCommand runs the built-in control-dispatcher worker.
	// Wrapped in `sh -c` so any appended prompt suffix is ignored as $0.
	// The control lane is kept resident and blocks on workflow-relevant city
	// events instead of exiting after each one-shot drain.
	//
	// The trace log default is under .gc/runtime/ so it sits inside the
	// controller's fsnotify exclusion (cmd/gc/controller.go shouldIgnoreConfigWatchEvent
	// excludes the .gc and .beads path segments). Placing it at city root
	// caused every append to fire markDirty() through the watcher debouncer,
	// which kept the patrol loop in continuous reconciliation and blew patrol
	// cycle duration well past the configured patrol_interval. See
	// engdocs/design/session-reconciler-tracing.md for the canonical
	// .gc/runtime/ convention for trace data.
	ControlDispatcherStartCommand = `sh -c '` + controlDispatcherTraceInit + `; ` + controlDispatcherTraceDirInit + `; exec "${GC_BIN:-gc}" convoy control --serve --follow ` + ControlDispatcherAgentName + `'`
)

// ControlDispatcherStartCommandFor returns the start command for a
// control-dispatcher agent with the given qualified name. The shell-level
// trace default is uniform across dispatchers; per-dispatcher distinction
// (closes #1650) is injected via agentEnv in cmd/gc/template_resolve.go at
// session-spawn time so the trust decision happens in Go.
func ControlDispatcherStartCommandFor(qualifiedName string) string {
	return `sh -c '` + controlDispatcherTraceInit + `; ` + controlDispatcherTraceDirInit + `; exec "${GC_BIN:-gc}" convoy control --serve --follow ` + qualifiedName + `'`
}

// IsDeterministicControlDispatcher reports whether an agent is the providerless
// control-dispatcher worker that runs the deterministic control loop.
func IsDeterministicControlDispatcher(agent *Agent) bool {
	return agent != nil &&
		agent.Name == ControlDispatcherAgentName &&
		strings.TrimSpace(agent.StartCommand) != "" &&
		strings.TrimSpace(agent.Provider) == "" &&
		strings.Contains(agent.StartCommand, "convoy control --serve")
}

// PreferredDeterministicControlDispatcher returns the deterministic control-
// dispatcher to route a scope's control beads to, binding-agnostic. The
// city-level singleton (Dir == "") is preferred for every scope — given
// max_active_sessions=1, it is the one whose session actually runs and claims
// the control queue — and a rig-scoped instance (Dir == rigContext) is used only
// when no city-level deterministic dispatcher is configured. Routing to a
// rig-scoped copy when a city singleton exists strands the control bead, since
// the singleton session never claims a <rig>/... route. This is the canonical
// selection shared by the graph.v2 decoration path (internal/graphroute) and the
// attempt-time control re-route path (internal/dispatch); keep them in lockstep.
func PreferredDeterministicControlDispatcher(cfg *City, rigContext string) (Agent, bool) {
	if cfg == nil {
		return Agent{}, false
	}
	rigContext = strings.TrimSpace(rigContext)
	var rigScoped Agent
	haveRigScoped := false
	for _, a := range cfg.Agents {
		if !IsDeterministicControlDispatcher(&a) {
			continue
		}
		if strings.TrimSpace(a.Dir) == "" {
			return a, true
		}
		if !haveRigScoped && strings.TrimSpace(a.Dir) == rigContext {
			rigScoped = a
			haveRigScoped = true
		}
	}
	if haveRigScoped {
		return rigScoped, true
	}
	return Agent{}, false
}

// BindingQualifiedName returns the binding-qualified agent identity without a
// rig prefix. Examples: "polecat", "gastown.polecat", or "gastown.mayor".
func (a *Agent) BindingQualifiedName() string {
	if a.BindingName == "" {
		return a.Name
	}
	return a.BindingName + "." + a.Name
}

// BindingPrefix returns the import binding prefix for route/template
// interpolation, including the trailing dot when a binding is present.
func (a *Agent) BindingPrefix() string {
	bindingName := strings.TrimSpace(a.BindingName)
	if bindingName == "" {
		return ""
	}
	return bindingName + "."
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
	// Upstreams defines named model-serving endpoint presets selectable per
	// agent via the Upstream axis (Phase C). Each maps a name → serving env
	// (base URL + credential refs); see UpstreamSpec.
	Upstreams map[string]UpstreamSpec `toml:"upstreams,omitempty"`
	// Packs defines named remote pack sources fetched via git (V1 mechanism).
	//
	// Legacy pack source map, accepted for migration and fetch/list
	// compatibility only. Authored schema-2 config uses [imports.*] with source
	// plus optional version, so this legacy surface is intentionally omitted
	// from generated public schemas and reference docs.
	Packs map[string]PackSource `toml:"packs,omitempty" jsonschema:"-"`
	// Imports defines named pack imports. Each key is a local
	// binding name; the authored public contract stores a durable source plus
	// optional version. Processed during ExpandCityPacks.
	Imports map[string]Import `toml:"imports,omitempty"`
	// Defaults holds city-level defaults that seed generated config. The
	// canonical default-rig import table is [defaults.rig.imports].
	Defaults PackDefaults `toml:"defaults,omitempty"`
	// Agents lists all configured agents in this city. Pack-composed cities can
	// compose agents through [imports.*] and ship without any
	// [[agent]] block.
	Agents []Agent `toml:"agent,omitempty"`
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
	// Usage configures the usage-fact sink backend.
	Usage UsageConfig `toml:"usage,omitempty"`
	// Dolt configures optional dolt server connection overrides.
	Dolt DoltConfig `toml:"dolt,omitempty"`
	// Formulas is the legacy [formulas] table; authored [formulas].dir is
	// rejected at config load. Formulas live in the well-known formulas/
	// directory.
	Formulas FormulasConfig `toml:"formulas,omitempty"`
	// Daemon configures controller daemon settings.
	Daemon DaemonConfig `toml:"daemon,omitempty"`
	// Orders configures order settings: skip list, max_timeout cap, and
	// per-order overrides.
	Orders OrdersConfig `toml:"orders,omitempty"`
	// API configures the optional HTTP API server.
	API APIConfig `toml:"api,omitempty"`
	// ChatSessions configures chat session behavior (auto-suspend).
	ChatSessions ChatSessionsConfig `toml:"chat_sessions,omitempty"`
	// SessionSleep configures idle sleep policy defaults for managed sessions.
	SessionSleep SessionSleepConfig `toml:"session_sleep,omitempty"`
	// Convergence configures convergence loop limits.
	Convergence ConvergenceConfig `toml:"convergence,omitempty"`
	// Doctor configures gc doctor thresholds and policy toggles
	// (worktree size warnings, nested-worktree auto-prune).
	Doctor DoctorConfig `toml:"doctor,omitempty"`
	// Maintenance configures periodic store-maintenance loops.
	Maintenance MaintenanceConfig `toml:"maintenance,omitempty"`
	// Services declares workspace-owned HTTP services mounted on the
	// controller edge under /svc/{name}.
	Services []Service `toml:"service,omitempty"`
	// GitHub configures GitHub-facing repository monitors.
	GitHub GitHubConfig `toml:"github,omitempty"`
	// ExtMsg configures the external-messaging fabric (default routes
	// for inbound conversations with no binding).
	ExtMsg ExtMsgConfig `toml:"extmsg,omitempty"`
	// AgentDefaults provides root city defaults for agents that don't override
	// them (canonical TOML key: agent_defaults). Pack-local defaults use the
	// same table shape in pack.toml. The runtime currently applies provider,
	// default_sling_formula, and append_fragments; the attachment-list fields
	// remain tombstones, and the other fields are parsed/composed but not yet
	// inherited automatically.
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
	// PackCommands holds convention-discovered pack commands composed
	// during city expansion. Runtime-only.
	PackCommands []DiscoveredCommand `toml:"-" json:"-"`
	// PackDoctors holds convention-discovered pack doctor checks composed
	// during city and rig expansion. Runtime-only.
	PackDoctors []DiscoveredDoctor `toml:"-" json:"-"`
	// Runtimes maps pack-declared runtime selection names ([runtimes.<name>]
	// in pack.toml) to their resolved declarations, composed during city and
	// rig expansion. Selection is city-wide, so rig-imported runtime packs
	// land here too; conflicting re-declarations are composition errors.
	// Runtime-only.
	Runtimes map[string]DiscoveredRuntime `toml:"-" json:"-"`
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
	// Pricing holds per-model cost rate overrides keyed by (provider, model).
	// City-level entries override pack-level entries which override the
	// defaults shipped with the pricing package. See internal/pricing for the
	// estimation seam introduced by issue #1255 (1d).
	Pricing []pricing.ModelPricing `toml:"pricing,omitempty"`
	// PackPricing preserves the pack-level pricing layer before Pricing is
	// flattened for legacy callers. Runtime-only.
	PackPricing []pricing.ModelPricing `toml:"-" json:"-"`
	// CityPricing preserves the city-level pricing layer before Pricing is
	// flattened for legacy callers. Runtime-only.
	CityPricing []pricing.ModelPricing `toml:"-" json:"-"`
}

// NamedSession defines a canonical persistent session backed by an agent
// template. Unlike Agent, it does not carry behavior itself; it only
// declares runtime identity and controller policy.
type NamedSession struct {
	// Name is the configured public session identity. When omitted, Template
	// remains the compatibility identity.
	Name string `toml:"name,omitempty"`
	// Template is the referenced agent template name. Root declarations may
	// target imported agents via "binding.agent".
	Template string `toml:"template" jsonschema:"required"`
	// Scope defines where this named session is instantiated in pack
	// expansion: "city" (one per city) or "rig" (one per rig). Omit the
	// field for an unscoped session instantiated in both city and rig
	// expansion contexts.
	Scope string `toml:"scope,omitempty" jsonschema:"enum=city,enum=rig"`
	// Dir is the identity prefix for rig-scoped named sessions after pack
	// expansion. Empty means city-scoped.
	Dir string `toml:"dir,omitempty"`
	// Mode controls when the controller ensures this named session is live.
	// "on_demand" (default): reserve identity and materialize when work or
	// an explicit reference requires it.
	// "always": keep the canonical session controller-managed.
	// Note: mode="always" is independent of min_active_sessions; both produce
	// sessions, and gc doctor reports accidental duplicate-pool combinations.
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

// ExpandGenericRigNamedSessions stamps inline scope="rig" named sessions that
// omit Dir into one concrete identity per configured rig.
func ExpandGenericRigNamedSessions(cfg *City) {
	if cfg == nil || len(cfg.NamedSessions) == 0 {
		return
	}
	expanded := make([]NamedSession, 0, len(cfg.NamedSessions))
	for _, ns := range cfg.NamedSessions {
		if ns.Scope == "rig" && ns.Dir == "" {
			for _, rig := range cfg.Rigs {
				stamped := ns
				stamped.Dir = rig.Name
				expanded = append(expanded, stamped)
			}
			continue
		}
		expanded = append(expanded, ns)
	}
	cfg.NamedSessions = expanded
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

// Rig defines an external project registered in the city.
type Rig struct {
	// Name is the unique identifier for this rig.
	Name string `toml:"name" jsonschema:"required"`
	// Path is the absolute filesystem path to the rig's repository.
	Path string `toml:"path,omitempty"`
	// Prefix overrides the auto-derived bead ID prefix for this rig.
	Prefix string `toml:"prefix,omitempty"`
	// DefaultBranch is the rig repository's mainline branch (e.g. "main",
	// "master", "develop"). When set, routing formulas use this as the
	// default merge target instead of probing origin/HEAD at sling time.
	// Captured by `gc rig add` from the rig's git config; set manually for
	// rigs whose mainline isn't reachable via origin/HEAD.
	DefaultBranch string `toml:"default_branch,omitempty"`
	// Suspended is the deprecated pre-runtime-state suspension flag.
	// Parsed for backwards compatibility and treated as an alias for
	// SuspendedOnStart by [Rig.EffectiveSuspendedOnStart], so existing
	// cities with `suspended = true` continue to start their rigs
	// suspended after upgrade. Live suspend/resume commands no longer
	// write this field. `gc doctor` flags it and offers `--fix` to
	// rename to suspended_on_start.
	Suspended bool `toml:"suspended,omitempty"`
	// SuspendedOnStart is the rig's desired suspension state at city
	// start. When true and no explicit entry exists for this rig in
	// .gc/runtime/suspension-state.json, the rig is treated as
	// suspended. Once the user has explicitly suspended or resumed the
	// rig via `gc rig suspend/resume`, the runtime state wins.
	SuspendedOnStart bool `toml:"suspended_on_start,omitempty"`
	// FormulasDir is a rig-local formula directory — the highest-priority
	// formula layer, above city pack formulas, the city formulas/
	// directory, and rig pack formulas. Overrides pack formulas for this
	// rig by filename.
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
	// FormulaVars provides rig-scoped defaults for formula vars. Keys match
	// var names declared in formula `[vars.<name>]` blocks. Values are used
	// when a formula runs in this rig and the caller did not pass an
	// explicit --var override. Takes precedence over formula-level defaults
	// but loses to --var flags.
	FormulaVars map[string]string `toml:"formula_vars,omitempty"`
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
	// TmuxAlias overrides the tmux session name template
	// (see Agent.TmuxAlias for semantics).
	TmuxAlias *string `toml:"tmux_alias,omitempty"`
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
	// Relative paths resolve against the declaring config file's directory
	// (pack-safe). Paths prefixed with "//" resolve against the city root.
	PromptTemplate *string `toml:"prompt_template,omitempty"`
	// Session overrides the session transport ("acp").
	Session *string `toml:"session,omitempty"`
	// Provider overrides the provider name.
	Provider *string `toml:"provider,omitempty"`
	// Upstream overrides the model-serving endpoint selection (Phase C).
	Upstream *string `toml:"upstream,omitempty"`
	// Args overrides the provider's default arguments. Leave unset to keep
	// the pack-defined args; set to an empty list to clear them; set to a
	// populated list to replace them entirely (full replace, not append).
	Args *[]string `toml:"args,omitempty"`
	// StartCommand overrides the start command.
	StartCommand *string `toml:"start_command,omitempty"`
	// Lifecycle overrides the runtime lifecycle ("one_shot" or empty).
	Lifecycle *string `toml:"lifecycle,omitempty" jsonschema:"enum=one_shot"`
	// Nudge overrides the nudge text.
	Nudge *string `toml:"nudge,omitempty"`
	// IdleTimeout overrides the idle timeout duration string (e.g., "30s", "5m", "1h").
	IdleTimeout *string `toml:"idle_timeout,omitempty"`
	// MaxSessionAge overrides the max session age. Duration string (e.g., "5h").
	// Empty disables preemptive restart.
	MaxSessionAge *string `toml:"max_session_age,omitempty"`
	// MaxSessionAgeJitter overrides the jitter added on top of MaxSessionAge.
	// Duration string (e.g., "15m"). Empty disables jitter.
	MaxSessionAgeJitter *string `toml:"max_session_age_jitter,omitempty"`
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
	// Relative paths resolve against the declaring config file's directory
	// (pack-safe). Paths prefixed with "//" resolve against the city root.
	SessionSetupScript *string `toml:"session_setup_script,omitempty"`
	// SessionLive overrides the agent's session_live commands.
	SessionLive []string `toml:"session_live,omitempty"`
	// OverlayDir overrides the agent's overlay_dir path. Copies contents
	// additively into the agent's working directory at startup.
	// Relative paths resolve against the declaring config file's directory
	// (pack-safe). Paths prefixed with "//" resolve against the city root.
	OverlayDir *string `toml:"overlay_dir,omitempty"`
	// DefaultSlingFormula overrides the default sling formula.
	DefaultSlingFormula *string `toml:"default_sling_formula,omitempty"`
	// InjectFragments overrides the agent's inject_fragments list. Leave this
	// field unset to keep inherited fragments; JSON callers may send null for
	// the same no-op. Set an empty list to clear fragments; set a populated
	// list to replace fragments.
	InjectFragments *[]string `toml:"inject_fragments,omitempty"`
	// AppendFragments appends named template fragments to this agent's rendered
	// prompt. It is the V2 spelling for per-agent fragment selection.
	AppendFragments []string `toml:"append_fragments,omitempty"`
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
	// MouseMode overrides whether tmux mouse mode is preserved ("on" or "off").
	MouseMode *string `toml:"mouse_mode,omitempty" jsonschema:"enum=on,enum=off"`
	// InjectFragmentsAppend appends to the agent's inject_fragments list.
	InjectFragmentsAppend []string `toml:"inject_fragments_append,omitempty"`
	// MaxActiveSessions overrides the agent-level cap on concurrent sessions.
	MaxActiveSessions *int `toml:"max_active_sessions,omitempty"`
	// MinActiveSessions overrides the minimum number of sessions to keep alive.
	MinActiveSessions *int `toml:"min_active_sessions,omitempty"`
	// ScaleCheck overrides the shell command whose output reports new
	// unassigned session demand for bead-backed reconciliation.
	ScaleCheck *string `toml:"scale_check,omitempty"`
	// OptionDefaults adds or overrides provider option defaults for this agent.
	// Keys are option keys, values are choice values. Merges additively
	// (override keys win over existing agent keys).
	// Example: option_defaults = { model = "sonnet" }
	OptionDefaults map[string]string `toml:"option_defaults,omitempty"`
}

// PackSource defines a legacy remote pack repository.
// Referenced by name in V1 pack fields and fetched into the cache.
//
// PackSource is retained for legacy migration and fetch/list compatibility.
// Authored schema-2 imports use Import.Source and Import.Version instead.
type PackSource struct {
	// Source is the git repository URL.
	Source string `toml:"source" jsonschema:"required"`
	// Ref is the git ref to checkout (branch, tag, or commit). Defaults to HEAD.
	Ref string `toml:"ref,omitempty"`
	// Path is a subdirectory within the repo containing the pack files.
	Path string `toml:"path,omitempty"`
}

// Import defines a named import of another pack. The binding name is the TOML
// key; authored public config uses source plus optional version. Package names
// discovered from the imported pack are advisory/display names, not identity.
type Import struct {
	// Source is the durable authored pack location: a local path, a remote git
	// URL, or a dereferenceable GitHub tree URL for a pack below a repository
	// root, such as "https://github.com/org/repo/tree/main/packs/foo". Registry
	// handles are lookup-only in this release wave; authored [imports.*]
	// entries store the resolved source plus optional version.
	Source string `toml:"source" jsonschema:"required"`
	// Version is an optional semver constraint for git-backed imports (e.g.,
	// "^1.2"). Empty for local paths. "sha:<hex>" pins a specific commit.
	Version string `toml:"version,omitempty"`
	// Export is a compatibility-only loader knob retained for older
	// configs. It is intentionally omitted from generated public schemas.
	Export bool `toml:"export,omitempty" jsonschema:"-"`
	// Transitive is a compatibility-only loader knob retained for older
	// configs. Authored public imports are a DAG through source plus version.
	// It is intentionally omitted from generated public schemas.
	Transitive *bool `toml:"transitive,omitempty" jsonschema:"-"`
	// Shadow is a compatibility-only loader knob retained for older
	// configs. It is intentionally omitted from generated public schemas.
	Shadow string `toml:"shadow,omitempty" jsonschema:"-"`
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
	// Description is an optional human-readable summary of the pack.
	Description string `toml:"description,omitempty"`
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

// HasDefaultOptionSemantics reports whether the import carries the
// default option semantics: not exported, transitive resolution enabled,
// and shadow handling empty or "warn". This is the option half of the
// reuse policy composition applies when deciding whether an existing
// same-source binding can stand in for a converted legacy include
// (existingDefaultImportBindingForSource); the doctor migration's
// same-source dedup shares it so the two policies cannot drift.
func (imp *Import) HasDefaultOptionSemantics() bool {
	if imp.Export {
		return false
	}
	if imp.Transitive != nil && !*imp.Transitive {
		return false
	}
	shadow := strings.TrimSpace(imp.Shadow)
	return shadow == "" || shadow == "warn"
}

// BoundImport preserves the user-visible binding name associated with an
// import when edit paths need ordered root-pack defaults.
type BoundImport struct {
	Binding string
	Import  Import
}

var (
	legacyImportInvalidBindingChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	legacyImportRepeatedDash        = regexp.MustCompile(`-+`)
)

// AddLegacyImports converts legacy include tokens into canonical V2 imports.
func AddLegacyImports(target map[string]Import, includes []string, packs map[string]PackSource) bool {
	changed := false
	for _, include := range includes {
		source := legacyImportSourceFor(include, packs)
		if _, exists := existingDefaultImportBindingForSource(target, source); exists {
			continue
		}
		binding := UniqueLegacyImportBinding(target, legacyImportBindingName(include, source, packs))
		target[binding] = Import{Source: source}
		changed = true
	}
	return changed
}

// AddOrderedLegacyImports converts legacy include tokens while preserving the import order list.
func AddOrderedLegacyImports(target map[string]Import, order []string, includes []string, packs map[string]PackSource) ([]string, bool) {
	changed := false
	for _, include := range includes {
		source := legacyImportSourceFor(include, packs)
		binding, exists := existingDefaultImportBindingForSource(target, source)
		if !exists {
			binding = UniqueLegacyImportBinding(target, legacyImportBindingName(include, source, packs))
			target[binding] = Import{Source: source}
			changed = true
		}
		if !stringSliceContains(order, binding) {
			order = append(order, binding)
			changed = true
		}
	}
	return order, changed
}

// BoundImportsFromLegacySources converts legacy include tokens into sorted bound imports.
func BoundImportsFromLegacySources(sources []string, packs map[string]PackSource) []BoundImport {
	if len(sources) == 0 {
		return nil
	}
	target := make(map[string]Import, len(sources))
	AddLegacyImports(target, sources, packs)
	return sortedBoundImports(target)
}

func legacyImportSourceFor(include string, packs map[string]PackSource) string {
	include = strings.TrimSpace(include)
	if spec, ok := packs[include]; ok {
		source := spec.Source
		if spec.Path != "" {
			if treeURL, ok := remotesource.FormatGitHubTreeSource(source, spec.Ref, spec.Path); ok {
				return treeURL
			}
			source += "//" + strings.TrimPrefix(spec.Path, "/")
		}
		if spec.Ref != "" {
			source += "#" + spec.Ref
		}
		return source
	}
	if looksLikeLegacyLocalImportPath(include) && !strings.HasPrefix(include, "./") && !strings.HasPrefix(include, "../") && !filepath.IsAbs(include) {
		return "./" + include
	}
	return include
}

func legacyImportBindingName(include, source string, packs map[string]PackSource) string {
	include = strings.TrimSpace(include)
	if _, ok := packs[include]; ok {
		return sanitizeLegacyImportBindingName(include)
	}
	base := source
	if idx := strings.Index(base, "#"); idx >= 0 {
		base = base[:idx]
	}
	if idx := strings.LastIndex(base, "//"); idx >= 0 && idx > strings.Index(base, "://")+2 {
		base = base[idx+2:]
	}
	base = strings.TrimSuffix(base, "/")
	base = strings.TrimSuffix(base, ".git")
	base = pathBase(base)
	return sanitizeLegacyImportBindingName(base)
}

func existingDefaultImportBindingForSource(target map[string]Import, source string) (string, bool) {
	for binding, imp := range target {
		if imp.Source != source {
			continue
		}
		if strings.TrimSpace(imp.Version) != "" {
			continue
		}
		if imp.HasDefaultOptionSemantics() {
			return binding, true
		}
	}
	return "", false
}

// UniqueLegacyImportBinding returns base when no import occupies it in
// target, or the first free "base-N" (N ≥ 2) suffix — the binding
// allocation policy for converting legacy includes into imports when the
// natural binding collides with an existing import.
func UniqueLegacyImportBinding(target map[string]Import, base string) string {
	if base == "" {
		base = "import"
	}
	if _, exists := target[base]; !exists {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, exists := target[candidate]; !exists {
			return candidate
		}
	}
}

func sanitizeLegacyImportBindingName(value string) string {
	value = legacyImportInvalidBindingChars.ReplaceAllString(value, "-")
	value = legacyImportRepeatedDash.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "import"
	}
	return value
}

func looksLikeLegacyLocalImportPath(value string) bool {
	if strings.Contains(value, "://") || strings.HasPrefix(value, "git@") {
		return false
	}
	if strings.HasPrefix(value, "github.com/") {
		return false
	}
	return true
}

func pathBase(value string) string {
	value = strings.TrimRight(value, "/")
	if idx := strings.LastIndex(value, "/"); idx >= 0 {
		return value[idx+1:]
	}
	return value
}

func sortedBoundImports(imports map[string]Import) []BoundImport {
	if len(imports) == 0 {
		return nil
	}
	bindings := make([]string, 0, len(imports))
	for binding := range imports {
		bindings = append(bindings, binding)
	}
	sort.Strings(bindings)
	bound := make([]BoundImport, 0, len(bindings))
	for _, binding := range bindings {
		bound = append(bound, BoundImport{
			Binding: binding,
			Import:  imports[binding],
		})
	}
	return bound
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
	// Warmup, when true, includes this check in the `gc start` warm-up
	// scan. Default false. The check still runs on demand via `gc doctor`
	// regardless of this flag.
	Warmup bool `toml:"warmup,omitempty"`
}

// PackRuntimeEntry declares a pack-shipped runtime provider executable
// under [runtimes.<name>] in pack.toml. The executable speaks the Runtime
// Provider Protocol (docs/reference/exec-session-provider.md); city
// composition registers the name into the runtime selection registry so
// `[session] provider = "<name>"` resolves to it. Name collisions with
// builtin runtimes (or other packs) are composition errors — see the
// RUNTIME-SEL rows in internal/runtime/REQUIREMENTS.md.
type PackRuntimeEntry struct {
	// Command is the runtime executable: a path relative to the pack
	// directory (anything containing a path separator) or a bare name
	// resolved on PATH at session start.
	Command string `toml:"command" jsonschema:"required"`
	// Protocol is the RPP version the executable speaks. Version 0 is
	// the only version today; the declaration exists so future protocol
	// bumps fail at composition instead of at session start.
	Protocol int `toml:"protocol,omitempty"`
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

// Coordination class names, mirroring coordclass.Class.String(). They are part of
// the [beads.classes.<name>] config contract and must not change without a
// migration.
const (
	BeadClassWork      = "work"
	BeadClassGraph     = "graph"
	BeadClassMessaging = "messaging"
	BeadClassSessions  = "sessions"
	BeadClassOrders    = "orders"
	BeadClassNudges    = "nudges"
)

// EffectiveDefaultBranch returns the rig's recorded default branch, or the
// empty string if none is set. Callers should fall back to a runtime probe
// (e.g., git symbolic-ref) when this returns "".
func (r *Rig) EffectiveDefaultBranch() string {
	return strings.TrimSpace(r.DefaultBranch)
}

// EffectiveSuspendedOnStart returns the rig's committable startup
// suspension default. The deprecated `suspended` field is honored as
// an alias for `suspended_on_start` so legacy city.toml files keep
// their behavior on upgrade. Use this everywhere a read site needs
// the authored default — never read r.Suspended directly for
// behavior; only `gc doctor` consults it (to warn about the legacy
// field).
func (r *Rig) EffectiveSuspendedOnStart() bool {
	return r.Suspended || r.SuspendedOnStart
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
	// Name is the legacy checked-in city name. Runtime identity now resolves
	// from site binding (.gc/site.toml workspace_name), declared config, and
	// basename precedence instead; gc init writes the machine-local name to
	// site.toml and omits it from city.toml.
	Name string `toml:"name,omitempty"`
	// Prefix overrides the auto-derived HQ bead ID prefix. When empty,
	// the prefix is derived from the city Name via DeriveBeadsPrefix.
	Prefix string `toml:"prefix,omitempty"`
	// Provider is the default provider name used by agents that don't specify one.
	Provider string `toml:"provider,omitempty"`
	// StartCommand overrides the provider's command for all agents.
	StartCommand string `toml:"start_command,omitempty"`
	// Suspended is the deprecated pre-runtime-state city suspension
	// flag. Parsed for backwards compatibility and treated as an alias
	// for SuspendedOnStart by [Workspace.EffectiveSuspendedOnStart], so
	// existing cities with `suspended = true` continue to start
	// suspended after upgrade. Live suspend/resume commands no longer
	// write this field. `gc doctor` flags it and offers `--fix` to
	// rename to suspended_on_start.
	Suspended bool `toml:"suspended,omitempty"`
	// SuspendedOnStart is the city's desired suspension state at start.
	// When true and no explicit entry exists in
	// .gc/runtime/suspension-state.json, the city is treated as
	// suspended. Once the user has explicitly suspended or resumed via
	// `gc suspend/resume`, the runtime state wins.
	SuspendedOnStart bool `toml:"suspended_on_start,omitempty"`
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
	// "antigravity", "kiro", "opencode", "mimocode", "groq", "cerebras",
	// "copilot", "cursor", "pi", "omp", "kimi".
	InstallAgentHooks []string `toml:"install_agent_hooks,omitempty"`
	// GlobalFragments lists named template fragments injected into every
	// agent's rendered prompt. Applied before per-agent InjectFragments.
	// Each name must match a {{ define "name" }} block from a pack's
	// prompts/shared/ directory.
	GlobalFragments []string `toml:"global_fragments,omitempty"`
	// Includes is the legacy city.toml pack-composition list.
	//
	// Deprecated: use root pack.toml [imports.*] instead. Run gc doctor to
	// inspect; gc doctor --fix handles the safe mechanical rewrites available
	// in this release wave. Each entry is a local path, a git source//sub#ref
	// URL, or a GitHub tree URL.
	Includes []string `toml:"includes,omitempty"`
	// DefaultRigIncludes is the legacy city.toml default-rig pack list.
	//
	// Deprecated: use city.toml [defaults.rig.imports.<binding>] instead.
	// Run gc doctor to inspect; gc doctor --fix handles the safe mechanical
	// rewrites available in this release wave.
	DefaultRigIncludes []string `toml:"default_rig_includes,omitempty"`
	// Env defines workspace-wide environment variables applied to every
	// managed session. Lowest config-precedence — overridden by provider,
	// agent, and patch env. Use for cross-cutting variables like
	// GC_TARGET_BRANCH that every agent should inherit.
	Env map[string]string `toml:"env,omitempty"`
}

// LegacyIncludes returns the compatibility-only city.toml include list.
func (w *Workspace) LegacyIncludes() []string {
	return w.Includes
}

// SetLegacyIncludes updates the compatibility-only city.toml include list.
func (w *Workspace) SetLegacyIncludes(includes []string) {
	w.Includes = includes
}

// LegacyDefaultRigIncludes returns the compatibility-only city.toml default-rig include list.
func (w *Workspace) LegacyDefaultRigIncludes() []string {
	return w.DefaultRigIncludes
}

// SetLegacyDefaultRigIncludes updates the compatibility-only city.toml default-rig include list.
func (w *Workspace) SetLegacyDefaultRigIncludes(includes []string) {
	w.DefaultRigIncludes = includes
}

// EffectiveSuspendedOnStart returns the workspace's committable
// startup suspension default. The deprecated `suspended` field is
// honored as an alias for `suspended_on_start` so legacy city.toml
// files keep their behavior on upgrade. Use this everywhere a read
// site needs the authored default — never read w.Suspended directly
// for behavior; only `gc doctor` consults it (to warn about the
// legacy field).
func (w *Workspace) EffectiveSuspendedOnStart() bool {
	return w.Suspended || w.SuspendedOnStart
}

// BeadsConfig holds bead store settings.
type BeadsConfig struct {
	// Provider selects the bead store backend: "bd" (default, Dolt-backed),
	// "file", or "exec:<script>" for a user-supplied script. The "sqlite",
	// "sqlite-cgo", and "coordstore" coordination-store providers were removed
	// and now hard-error; migrate to "doltlite" or remove the setting.
	Provider string `toml:"provider,omitempty" jsonschema:"default=bd"`
	// Backend selects the bd storage engine when Provider is "bd".
	// Empty defaults to "dolt"; T3Code uses "doltlite" for local dev stores.
	Backend string `toml:"backend,omitempty"`
	// EventHooks controls installation of the bead event-forwarding hooks
	// (.beads/hooks/on_create,on_update,on_close) that shell out to
	// `gc event emit` on every bead write. Defaults to true. Set to false
	// once the controller's native cache-events already observe bead changes
	// (the bd_hooks doctor gate): the lifecycle then removes the event hooks
	// (leaving git hooks untouched) and stops reinstalling them, clearing the
	// per-write churn and the native-store gate.
	EventHooks *bool `toml:"event_hooks,omitempty" jsonschema:"default=true"`
	// BDCompatibility selects the bd CLI semantics Gas City may rely on.
	// Empty defaults to "bd-1.0.4", which keeps claimable work history-backed
	// and avoids bd ready/list flags that are unavailable or incomplete in bd
	// 1.0.4.
	BDCompatibility string `toml:"bd_compatibility,omitempty" jsonschema:"enum=bd-1.0.4,enum=bd-1.0.5"`
	// Policies defines per-bead-use storage and garbage-collection defaults.
	// Policy names are interpreted by higher-level systems; unknown names are
	// preserved so packs can stage future policy classes without breaking load.
	Policies map[string]BeadPolicyConfig `toml:"policies,omitempty"`
}

// EventHooksEnabled reports whether bead event hooks should be installed.
// Unset preserves the current default of enabled hooks.
func (b BeadsConfig) EventHooksEnabled() bool {
	return b.EventHooks == nil || *b.EventHooks
}

const (
	// BeadsBDCompatibility104 preserves behavior supported by installed bd
	// 1.0.4, where ready filtering is reliable only for history-backed rows.
	BeadsBDCompatibility104 = "bd-1.0.4"
	// BeadsBDCompatibility105 opts into bd 1.0.5 CLI/storage semantics.
	BeadsBDCompatibility105 = "bd-1.0.5"
)

// NormalizedBDCompatibility returns the configured bd compatibility mode.
// Empty and unknown values are treated as bd 1.0.4 by runtime code; validation
// reports unknown values separately when loading user config.
func (b BeadsConfig) NormalizedBDCompatibility() string {
	switch b.BDCompatibility {
	case "", BeadsBDCompatibility104:
		return BeadsBDCompatibility104
	case BeadsBDCompatibility105:
		return BeadsBDCompatibility105
	default:
		return BeadsBDCompatibility104
	}
}

// UsesBD105CLISemantics reports whether bd-backed code may rely on bd 1.0.5
// command-line behavior.
func (b BeadsConfig) UsesBD105CLISemantics() bool {
	return b.NormalizedBDCompatibility() == BeadsBDCompatibility105
}

// UsesBD105ReadySemantics reports whether generated bd ready commands may use
// flags whose complete filter semantics require bd 1.0.5 or newer.
func (b BeadsConfig) UsesBD105ReadySemantics() bool {
	return b.UsesBD105CLISemantics()
}

// BeadPolicyConfig holds storage and retention defaults for a named bead use.
type BeadPolicyConfig struct {
	// Storage selects the intended persistence tier: "history", "no_history",
	// or "ephemeral". Creation paths apply this incrementally as they opt in.
	Storage string `toml:"storage,omitempty" jsonschema:"enum=history,enum=no_history,enum=ephemeral"`
	// DeleteAfterClose deletes matching GC-owned beads after they have been
	// closed for this duration. Accepts Go duration syntax plus whole-day "d"
	// units, e.g. "7d" or "1d12h". ApplyBeadPolicyDefaults fills in a
	// non-empty default for recognized policy types (order_tracking: "7d"),
	// so this field is populated after config load even when the city.toml
	// omits it.
	DeleteAfterClose string `toml:"delete_after_close,omitempty"`
}

const (
	// BeadStorageHistory stores beads in the normal history-tracked table.
	BeadStorageHistory = "history"
	// BeadStorageNoHistory stores beads without Dolt history while keeping
	// non-ephemeral semantics.
	BeadStorageNoHistory = "no_history"
	// BeadStorageEphemeral stores beads as ephemeral wisps.
	BeadStorageEphemeral = "ephemeral"
)

// NormalizeBeadPolicyStorage returns the configured bead policy storage value.
// Storage spellings are intentionally canonical so runtime validation matches
// the generated schema enum.
func NormalizeBeadPolicyStorage(storage string) string {
	return storage
}

// ValidBeadPolicyStorage reports whether storage is one of the supported bead
// storage classes. Empty storage is valid and means use the default.
func ValidBeadPolicyStorage(storage string) bool {
	switch NormalizeBeadPolicyStorage(storage) {
	case "", BeadStorageHistory, BeadStorageNoHistory, BeadStorageEphemeral:
		return true
	default:
		return false
	}
}

// NormalizedStorage returns the canonical storage class for this policy.
func (p BeadPolicyConfig) NormalizedStorage() string {
	return NormalizeBeadPolicyStorage(p.Storage)
}

// DeleteAfterCloseDuration returns DeleteAfterClose as a duration. It accepts
// ordinary Go durations plus a whole-day "d" unit, e.g. "7d" and "1d12h".
func (p BeadPolicyConfig) DeleteAfterCloseDuration() time.Duration {
	if p.DeleteAfterClose == "" {
		return 0
	}
	dur, err := parseConfigDurationWithDays(p.DeleteAfterClose)
	if err != nil || dur <= 0 {
		return 0
	}
	return dur
}

// ProgressStallTimeoutMinimum is the minimum positive progress-stall recycle
// timeout. Values below this floor are clamped so an opt-in automated restart
// loop cannot spin faster than the storm-protection backstops can observe.
const ProgressStallTimeoutMinimum = 5 * time.Minute

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
	// ProgressStallTimeout, when set, enables progress-aware session recycling:
	// a desired, alive, claim-less session on a healthy provider whose last
	// provider-reported activity is older than this duration is restarted fresh.
	// Such a session has likely parked (e.g. its turn ended on a provider auth
	// error) and will not self-recover. Set this above the longest legitimate
	// alive-idle period for the city; values below 5m are clamped to 5m.
	// Duration string (e.g. "30m"). Unset/zero disables it.
	ProgressStallTimeout string `toml:"progress_stall_timeout,omitempty"`
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

// ProgressStallTimeoutDuration returns the progress-stall recycle timeout, or
// 0 when unset, zero, negative, or unparseable. Positive values below
// ProgressStallTimeoutMinimum are clamped to that floor. Zero disables
// progress-aware recycling (the default): only a city that explicitly opts in
// by setting a duration above its agents' longest legitimate quiet period gets
// the behavior.
func (s *SessionConfig) ProgressStallTimeoutDuration() time.Duration {
	if s.ProgressStallTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(s.ProgressStallTimeout)
	if err != nil {
		return 0
	}
	if d <= 0 {
		return 0
	}
	if d < ProgressStallTimeoutMinimum {
		return ProgressStallTimeoutMinimum
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
	// RetentionTTL is how long read messages are retained before purge. Empty
	// or "0" disables read-message retention.
	RetentionTTL string `toml:"retention_ttl,omitempty"`
}

// RetentionTTLDuration parses RetentionTTL as a Go time.Duration. Empty or
// zero disables read-message retention.
func (m MailConfig) RetentionTTLDuration() (time.Duration, error) {
	raw := strings.TrimSpace(m.RetentionTTL)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("[mail] retention_ttl %q is not a valid Go duration: %w", raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("[mail] retention_ttl must not be negative: got %q", raw)
	}
	return d, nil
}

// EventsConfig holds events provider settings.
type EventsConfig struct {
	// Provider selects the events backend: "fake", "fail",
	// "exec:<script>", or "" (default: file-backed JSONL).
	Provider string `toml:"provider,omitempty"`
	// Rotation configures file-backed JSONL rotation. Defaults are applied
	// by EventsRotationConfig helper methods when this table is absent.
	Rotation EventsRotationConfig `toml:"rotation,omitempty"`
}

// UsageConfig holds usage-fact sink settings.
type UsageConfig struct {
	// Provider selects the usage sink backend:
	//   - "discard" / "fake" → drop all facts
	//   - "exec:<script>" → user-supplied script (JSON fact per line on stdin)
	//   - "" / "local" → durable file-backed JSONL at .gc/usage.jsonl (default)
	Provider string `toml:"provider,omitempty"`
}

const (
	// DefaultEventsRotationMaxSizeBytes is the default active events.jsonl
	// size threshold before auto-rotation.
	DefaultEventsRotationMaxSizeBytes int64 = 256 * 1024 * 1024
	// DefaultEventsRotationCheckIntervalRecords is the default number of
	// records between active file size checks.
	DefaultEventsRotationCheckIntervalRecords = 1024
	// DefaultEventsRotationCheckIntervalSeconds is the default time backstop
	// between active file size checks.
	DefaultEventsRotationCheckIntervalSeconds = 60
	// DefaultEventsRotationCheckInterval is the default time backstop between
	// active file size checks.
	DefaultEventsRotationCheckInterval = time.Duration(DefaultEventsRotationCheckIntervalSeconds) * time.Second
)

// EventsRotationConfig holds file-backed events rotation settings.
type EventsRotationConfig struct {
	// Enabled controls automatic size-triggered rotation. Defaults to true.
	Enabled *bool `toml:"enabled,omitempty" jsonschema:"default=true"`
	// MaxSizeBytes is the active events.jsonl size threshold. Defaults to
	// DefaultEventsRotationMaxSizeBytes.
	MaxSizeBytes *int64 `toml:"max_size_bytes,omitempty" jsonschema:"default=268435456"`
	// CheckIntervalRecords is the number of records between size checks.
	// Defaults to DefaultEventsRotationCheckIntervalRecords.
	CheckIntervalRecords *int `toml:"check_interval_records,omitempty" jsonschema:"default=1024"`
	// CheckIntervalSeconds is the time backstop between size checks. Defaults
	// to DefaultEventsRotationCheckIntervalSeconds.
	CheckIntervalSeconds *int `toml:"check_interval_seconds,omitempty" jsonschema:"default=60"`
	// ArchiveRetainAge is an optional Go duration. Empty keeps all archives.
	ArchiveRetainAge string `toml:"archive_retain_age,omitempty"`
}

// EnabledOrDefault reports whether automatic rotation is enabled.
func (c EventsRotationConfig) EnabledOrDefault() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// MaxSizeBytesOrDefault returns the configured active log size threshold.
func (c EventsRotationConfig) MaxSizeBytesOrDefault() int64 {
	if c.MaxSizeBytes == nil {
		return DefaultEventsRotationMaxSizeBytes
	}
	return *c.MaxSizeBytes
}

// CheckIntervalRecordsOrDefault returns the configured record-count gate.
func (c EventsRotationConfig) CheckIntervalRecordsOrDefault() int {
	if c.CheckIntervalRecords == nil {
		return DefaultEventsRotationCheckIntervalRecords
	}
	return *c.CheckIntervalRecords
}

// CheckIntervalDurationOrDefault returns the configured time gate.
func (c EventsRotationConfig) CheckIntervalDurationOrDefault() time.Duration {
	if c.CheckIntervalSeconds == nil {
		return DefaultEventsRotationCheckInterval
	}
	return time.Duration(*c.CheckIntervalSeconds) * time.Second
}

// ArchiveRetainAgeDuration parses ArchiveRetainAge. Empty or invalid values
// return zero, which keeps all archives.
func (c EventsRotationConfig) ArchiveRetainAgeDuration() time.Duration {
	if strings.TrimSpace(c.ArchiveRetainAge) == "" {
		return 0
	}
	d, err := time.ParseDuration(c.ArchiveRetainAge)
	if err != nil {
		return 0
	}
	return d
}

const (
	// DefaultDoltMaxConnections is the managed Dolt listener connection cap.
	DefaultDoltMaxConnections = 256
	// DefaultDoltReadTimeoutMillis is the managed Dolt listener read timeout.
	// Managed multi-agent cities open a short-lived bd/dolt-sql client
	// connection per operation and frequently SIGKILL it on a client-side
	// deadline (e.g. agents wrap `gc hook` in `timeout 10`), so the server
	// orphans the socket in Sleep until read_timeout fires. Lowering this from
	// the former 30s reaps those dead per-call connections sooner, before they
	// accumulate into a store-wide read collapse under load. read_timeout is the
	// listener socket idle/produce timeout: it reaps idle (Sleep) connections
	// and bounds the inter-row produce gap (go-mysql-server ErrRowTimeout
	// re-arms per row), not total query wall-clock — so it does not cut a long
	// but steadily-producing query. Do NOT drop it to/below the client kill
	// budget (`timeout 10`) on the assumption it is purely idle-reaping. Cities
	// with slower live operations raise it via city.toml [dolt]
	// read_timeout_millis. See #3022 (5m->30s) and the scale_check storm RCA
	// (30s->15s).
	DefaultDoltReadTimeoutMillis = 15000
	// DefaultDoltWriteTimeoutMillis is the managed Dolt listener write timeout.
	DefaultDoltWriteTimeoutMillis = 300000
)

// DoltConfig holds optional dolt server overrides.
// When present in city.toml, these override the defaults.
type DoltConfig struct {
	// Port is the dolt server port. 0 means use ephemeral port allocation
	// (hashed from city path). Set explicitly to override.
	Port int `toml:"port,omitempty" jsonschema:"default=0"`
	// Host is the dolt server hostname. Defaults to localhost.
	Host string `toml:"host,omitempty" jsonschema:"default=localhost"`
	// ArchiveLevel controls Dolt's auto_gc archive aggressiveness.
	// 0 disables archive compaction (lower CPU on startup).
	// 1 enables archive compaction (higher CPU on startup).
	// nil (omitted) defaults to 0.
	ArchiveLevel *int `toml:"archive_level,omitempty" jsonschema:"default=0"`
	// AutoGCEnabled toggles Dolt's incremental auto-GC on the managed
	// sql-server. Auto-GC bounds the noms journal so it never reaches
	// GB scale, which shrinks both the unclean-stop corruption window
	// and the recovery blast radius. nil (omitted) defaults to true.
	AutoGCEnabled *bool `toml:"auto_gc_enabled,omitempty" jsonschema:"default=true"`
	// MaxConnections overrides the managed Dolt listener max_connections.
	// 0 means use the managed default.
	MaxConnections int `toml:"max_connections,omitempty" jsonschema:"default=256"`
	// ReadTimeoutMillis overrides the managed Dolt listener read_timeout_millis.
	// 0 means use the managed default.
	ReadTimeoutMillis int `toml:"read_timeout_millis,omitempty" jsonschema:"default=15000"`
	// WriteTimeoutMillis overrides the managed Dolt listener write_timeout_millis.
	// 0 means use the managed default.
	WriteTimeoutMillis int `toml:"write_timeout_millis,omitempty" jsonschema:"default=300000"`
	// DoltLockReleaseTimeout is how long managed-dolt lifecycle operations
	// wait for dolt's on-disk exclusive store locks (the root-level
	// `<data_dir>/.dolt/noms/LOCK` and per-database
	// `<data_dir>/<db>/.dolt/noms/LOCK` forms) to be released by a prior
	// server process before failing closed. The start path refuses to launch a
	// second `dolt sql-server` against a data_dir whose lock is still held —
	// a prior instance that is shutting down holds the lock until its chunk
	// journal is flushed, and binding before release corrupts the journal
	// (see gastownhall/gascity#3174). The stop path uses the same window to
	// wait for lock release after process exit before reporting success.
	// Duration string (e.g., "1m", "90s"). Defaults to "1m", which covers
	// the flush window of multi-GB journals on commodity SSDs. Set to "0s"
	// to probe once with no wait (still fail-closed when held). Negative
	// values are rejected at config load. The managed lifecycle also
	// projects this value into the gc-beads-bd.sh shell fallback as
	// GC_DOLT_LOCK_RELEASE_TIMEOUT_MS (milliseconds), so both paths honor
	// the configured window.
	DoltLockReleaseTimeout string `toml:"dolt_lock_release_timeout,omitempty" jsonschema:"default=1m"`
}

// EffectiveArchiveLevel returns the configured Dolt archive level, defaulting
// omitted values to 0.
func (d DoltConfig) EffectiveArchiveLevel() int {
	if d.ArchiveLevel != nil {
		return *d.ArchiveLevel
	}
	return 0
}

// EffectiveAutoGCEnabled returns whether Dolt incremental auto-GC is enabled
// for the managed sql-server, defaulting omitted values to true.
func (d DoltConfig) EffectiveAutoGCEnabled() bool {
	if d.AutoGCEnabled != nil {
		return *d.AutoGCEnabled
	}
	return true
}

// AutoGCSysVar returns the dolt_auto_gc_enabled system-variable value
// ("ON"/"OFF") matching EffectiveAutoGCEnabled, so the config writer and the
// doctor contract derive it from one place.
func (d DoltConfig) AutoGCSysVar() string {
	if d.EffectiveAutoGCEnabled() {
		return "ON"
	}
	return "OFF"
}

// EffectiveMaxConnections returns the managed Dolt listener max_connections.
func (d DoltConfig) EffectiveMaxConnections() int {
	if d.MaxConnections > 0 {
		return d.MaxConnections
	}
	return DefaultDoltMaxConnections
}

// EffectiveReadTimeoutMillis returns the managed Dolt listener read timeout.
func (d DoltConfig) EffectiveReadTimeoutMillis() int {
	if d.ReadTimeoutMillis > 0 {
		return d.ReadTimeoutMillis
	}
	return DefaultDoltReadTimeoutMillis
}

// EffectiveWriteTimeoutMillis returns the managed Dolt listener write timeout.
func (d DoltConfig) EffectiveWriteTimeoutMillis() int {
	if d.WriteTimeoutMillis > 0 {
		return d.WriteTimeoutMillis
	}
	return DefaultDoltWriteTimeoutMillis
}

// DefaultDoltLockReleaseTimeout is the wait window for dolt's on-disk
// exclusive store lock to be released when no value is configured. 1m covers
// the longest observed clean-shutdown flush of a multi-GB chunk journal on
// commodity SSDs (gastownhall/gascity#3174) while still bounding how long a
// start or stop can stall on a wedged holder before failing closed.
const DefaultDoltLockReleaseTimeout = time.Minute

// DoltLockReleaseTimeoutDuration returns the configured wait window for
// dolt's on-disk exclusive lock as a time.Duration. Defaults to
// DefaultDoltLockReleaseTimeout (1m) when empty or unparseable. Zero means a
// single probe with no wait — the operation still fails closed when the lock
// is held. Negative values pass through unchanged: callers that route
// through loadCityConfig already reject them via
// ValidateNonNegativeDurations. Mirrors DoltStopTimeoutDuration's policy.
func (d *DoltConfig) DoltLockReleaseTimeoutDuration() time.Duration {
	if d.DoltLockReleaseTimeout == "" {
		return DefaultDoltLockReleaseTimeout
	}
	dur, err := time.ParseDuration(d.DoltLockReleaseTimeout)
	if err != nil {
		return DefaultDoltLockReleaseTimeout
	}
	return dur
}

// FormulasConfig is the legacy [formulas] table with no supported fields:
// authored [formulas].dir is rejected at config load (use the well-known
// formulas/ directory instead), and gc doctor flags any declaration as a
// fixable v2-formulas-dir error.
type FormulasConfig struct {
	// Dir is the legacy path to the formulas directory. Authored
	// [formulas].dir is rejected at config load; schema-2 cities and packs
	// use the well-known formulas/ directory.
	Dir string `toml:"dir,omitempty" jsonschema:"-"`
}

// OrdersConfig holds order settings for orders discovered from flat TOML
// files (one file per order) in the orders/ directory beside each formula
// layer (packs, the city directory, and rig-local layers).
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

// OrderOverride modifies a scanned order's scheduling fields and exec env.
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
	// Idempotent overrides whether the order's dispatch is safe to repeat.
	// Idempotent orders fail open when the open-work gate times out (#2893).
	Idempotent *bool `toml:"idempotent,omitempty"`
	// Env adds or overrides environment variables exported into an exec
	// order's child process.
	Env map[string]string `toml:"env,omitempty"`
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
	// WriteAuthVerifyKey, when set, requires every mutating request to an
	// already-registered city — the per-city routes under /v0/city/{cityName} —
	// to carry a signed write grant from a configured trusted authority. It
	// gates all per-city writes (beads, mail, sessions, agents, and config), not
	// only config edits. City registry creation (POST /v0/city) is not covered:
	// a grant binds a path-resident city name, which a not-yet-created city
	// lacks, so creation stays governed by the supervisor-registry guards.
	// Built-in callers (the bundled gc API client and dashboard SPA) send only
	// the CSRF header and mint no grant, so enabling this gate turns their direct
	// city mutations away with a clear 401; such deployments front mutations
	// through the trusted authority that mints grants instead. The value is one
	// or more "kid:base64-ed25519-pubkey" entries, comma separated.
	// The GC_CITY_WRITE_PUBKEY env var overrides this. Grant revocation via an
	// epoch floor is an ops-plane control set only through the
	// GC_CITY_WRITE_EPOCH_FLOOR env var; it has no config field.
	WriteAuthVerifyKey string `toml:"write_auth_verify_key,omitempty"`
	// WriteAuthRequired makes a missing or empty WriteAuthVerifyKey a startup
	// error instead of silently disabling the gate, so a config that intends to
	// gate writes fails closed if the key is ever dropped. The
	// GC_CITY_WRITE_REQUIRED=1 env var has the same effect.
	WriteAuthRequired bool `toml:"write_auth_required,omitempty"`
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
	// GracePeriod is the duration after creation during which a manual
	// session is protected from idle-sleep scale-to-zero. Duration string
	// (e.g., "10m"). Empty = use default (10m). "0" = disabled.
	GracePeriod string `toml:"grace_period,omitempty"`
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

// DefaultManualGracePeriod is the grace period for manual sessions when
// no explicit grace_period is configured. Protects ad-hoc sessions from
// idle-sleep scale-to-zero during their initial startup window.
const DefaultManualGracePeriod = 10 * time.Minute

// GracePeriodDuration parses GracePeriod, returning DefaultManualGracePeriod
// if unset, 0 if explicitly set to "0", or the parsed duration.
func (c ChatSessionsConfig) GracePeriodDuration() time.Duration {
	switch c.GracePeriod {
	case "":
		return DefaultManualGracePeriod
	case "0", "0s":
		return 0
	}
	d, err := time.ParseDuration(c.GracePeriod)
	if err != nil {
		return DefaultManualGracePeriod
	}
	return d
}

// LocalDoctorCheck is a city-local doctor check declared inline in city.toml
// via [[doctor.check]]. Scripts use the same exit-code protocol as pack
// doctor scripts: 0=OK, 1=Warning, 2+=Error.
type LocalDoctorCheck struct {
	// Name is the bare check name. The SDK injects the "local:" prefix;
	// do not include it here.
	Name string `toml:"name"`

	// Script is the path to the check script, relative to the city root.
	// Execution registration enforces containment within the city directory.
	Script string `toml:"script"`

	// Description is optional human-readable text shown in verbose output.
	Description string `toml:"description,omitempty"`

	// Fix is the optional path to a remediation script, relative to the
	// city root.
	Fix string `toml:"fix,omitempty"`
}

// DoctorConfig holds settings for the gc doctor surface. Operator-tunable
// thresholds and policy toggles live here; mechanical structural checks
// (broken-worktree pointers, missing files) remain hardcoded since they
// cannot be operator-tuned in any meaningful sense.
type DoctorConfig struct {
	// WorktreeRigWarnSize is the per-rig warning threshold for the total
	// disk footprint under .gc/worktrees/<rig>/. Reported by the
	// worktree-disk-size check. Go-style human size string ("10GB", "500MB").
	// Empty or unparseable falls back to the default (10 GB).
	WorktreeRigWarnSize string `toml:"worktree_rig_warn_size,omitempty" jsonschema:"default=10GB"`

	// WorktreeRigErrorSize is the per-rig error threshold. When any rig
	// exceeds this, the worktree-disk-size check reports an error rather
	// than a warning. Empty or unparseable falls back to the default
	// (50 GB).
	WorktreeRigErrorSize string `toml:"worktree_rig_error_size,omitempty" jsonschema:"default=50GB"`

	// NestedWorktreePrune escalates the nested-worktree-prune check
	// from warning to error severity when safely-prunable nested
	// worktrees are present, so CI / scripted doctor runs fail until
	// the operator runs `gc doctor --fix`. Actual removal still
	// requires --fix; this flag does not auto-prune. Safety is
	// enforced by mechanical checks (no uncommitted changes, no
	// unpushed commits, no stashes) — never by role identity.
	NestedWorktreePrune bool `toml:"nested_worktree_prune,omitempty" jsonschema:"default=false"`

	// Checks holds city-local inline doctor checks declared via
	// [[doctor.check]] in city.toml.
	Checks []LocalDoctorCheck `toml:"check,omitempty"`
}

const (
	defaultWorktreeRigWarnBytes  = int64(10) * 1024 * 1024 * 1024 // 10 GB
	defaultWorktreeRigErrorBytes = int64(50) * 1024 * 1024 * 1024 // 50 GB
)

// WorktreeRigWarnBytes returns the warning threshold in bytes. Falls
// back to defaultWorktreeRigWarnBytes when unset, unparseable, or
// non-positive.
func (c DoctorConfig) WorktreeRigWarnBytes() int64 {
	if n, ok := parseHumanSize(c.WorktreeRigWarnSize); ok && n > 0 {
		return n
	}
	return defaultWorktreeRigWarnBytes
}

// WorktreeRigErrorBytes returns the error threshold in bytes. Falls
// back to defaultWorktreeRigErrorBytes when unset, unparseable, or
// non-positive. The error threshold is clamped to at least the warn
// threshold to keep the two-tier semantics monotonic; if the operator
// configures error < warn, the warn value wins.
func (c DoctorConfig) WorktreeRigErrorBytes() int64 {
	warn := c.WorktreeRigWarnBytes()
	n, ok := parseHumanSize(c.WorktreeRigErrorSize)
	if !ok || n <= 0 {
		n = defaultWorktreeRigErrorBytes
	}
	if n < warn {
		return warn
	}
	return n
}

// parseHumanSize parses sizes like "10GB", "500 MB", "1024" (bytes
// implied) into a byte count. Whitespace tolerant, case-insensitive.
// Returns ok=false when the string is empty or unparseable so callers
// can apply their own default.
func parseHumanSize(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, false
	}
	var unit int64 = 1
	switch {
	case strings.HasSuffix(s, "GB"):
		unit = 1024 * 1024 * 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "GB"))
	case strings.HasSuffix(s, "MB"):
		unit = 1024 * 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "MB"))
	case strings.HasSuffix(s, "KB"):
		unit = 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "KB"))
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSpace(strings.TrimSuffix(s, "B"))
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n * unit, true
}

// ConvergenceConfig holds convergence loop limits.
type ConvergenceConfig struct {
	// MaxPerAgent is the maximum number of active convergence loops per agent
	// in each bead store scope. City/HQ and each bound rig enforce the limit
	// independently. 0 means use default (2).
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

// MaintenanceConfig groups periodic store-maintenance subsections.
type MaintenanceConfig struct {
	// Dolt configures the weekly Dolt store maintenance loop
	// (CALL DOLT_GC + backup snapshot).
	Dolt DoltMaintenance `toml:"dolt,omitempty"`
}

// DoltMaintenance configures the periodic Dolt store maintenance loop.
// Opt-in for v1: omission or enabled=false leaves the loop disabled.
type DoltMaintenance struct {
	// Enabled toggles the maintenance loop. Defaults to false (opt-in).
	Enabled bool `toml:"enabled,omitempty"`
	// Interval is the cadence between maintenance runs as a duration
	// string (e.g., "168h"). Defaults to 168h (weekly).
	Interval string `toml:"interval,omitempty" jsonschema:"default=168h"`
	// AlertTo is the agent identity to mail on failure (e.g.,
	// "gascity/mayor"). Empty disables alert mail.
	AlertTo string `toml:"alert_to,omitempty"`
	// GCTimeout is the ceiling for CALL DOLT_GC() as a duration string.
	// Defaults to 10m.
	GCTimeout string `toml:"gc_timeout,omitempty" jsonschema:"default=10m"`
}

// IntervalOrDefault returns the parsed Interval, falling back to 168h
// (weekly) when unset or unparseable. Invalid values should already have
// surfaced as warnings from ValidateDurations at load time.
func (d DoltMaintenance) IntervalOrDefault() time.Duration {
	if d.Interval == "" {
		return 168 * time.Hour
	}
	v, err := time.ParseDuration(d.Interval)
	if err != nil {
		return 168 * time.Hour
	}
	return v
}

// GCTimeoutOrDefault returns the parsed GCTimeout, falling back to 10m
// when unset or unparseable.
func (d DoltMaintenance) GCTimeoutOrDefault() time.Duration {
	if d.GCTimeout == "" {
		return 10 * time.Minute
	}
	v, err := time.ParseDuration(d.GCTimeout)
	if err != nil {
		return 10 * time.Minute
	}
	return v
}

// DaemonConfig holds controller daemon settings.
type DaemonConfig struct {
	// FormulaV2 enables formula compiler v2 workflow infrastructure:
	// compiler-v2 workflow compilation, batch graph-apply bead creation, and
	// routing to the core pack's control-dispatcher worker.
	// Requires bd with --graph support. Default: ENABLED. A nil pointer means
	// the default-on behavior and is OMITTED from generated configs (so
	// auto-generated city.toml files never pin the default and never
	// accidentally write formula_v2=false); an explicit formula_v2=false (or
	// the deprecated graph_workflows=false alias) is preserved as a non-nil
	// false. Read the effective value via FormulaV2Enabled(), never the field.
	FormulaV2 *bool `toml:"formula_v2,omitempty" jsonschema:"default=true"`
	// GraphWorkflows is the deprecated predecessor of FormulaV2. Retained
	// for backwards compatibility as an alias. Explicit formula_v2 wins.
	GraphWorkflows bool `toml:"graph_workflows,omitempty"`
	// PatrolInterval is the health patrol interval. Duration string (e.g., "30s", "5m", "1h"). Defaults to "30s".
	PatrolInterval string `toml:"patrol_interval,omitempty" jsonschema:"default=30s"`
	// MaxRestarts is the maximum number of agent restarts within RestartWindow before
	// the agent is quarantined. 0 means unlimited (no crash loop detection). Defaults to 5.
	MaxRestarts *int `toml:"max_restarts,omitempty" jsonschema:"default=5"`
	// RestartWindow is the sliding time window for counting restarts.
	// Duration string (e.g., "30s", "5m", "1h"). Defaults to "1h".
	RestartWindow string `toml:"restart_window,omitempty" jsonschema:"default=1h"`
	// SessionCircuitBreaker enables the named-session respawn circuit breaker.
	// When enabled, the controller suppresses no-progress named-session respawns
	// after the configured restart threshold is exceeded.
	SessionCircuitBreaker bool `toml:"session_circuit_breaker,omitempty"`
	// SessionCircuitBreakerMaxRestarts overrides MaxRestarts for the
	// named-session respawn circuit breaker. Nil reuses MaxRestartsOrDefault.
	// 0 disables the circuit breaker even when SessionCircuitBreaker is true.
	SessionCircuitBreakerMaxRestarts *int `toml:"session_circuit_breaker_max_restarts,omitempty" jsonschema:"default=5"`
	// SessionCircuitBreakerWindow overrides RestartWindow for the named-session
	// respawn circuit breaker. Empty reuses RestartWindowDuration.
	SessionCircuitBreakerWindow string `toml:"session_circuit_breaker_window,omitempty" jsonschema:"default=1h"`
	// SessionCircuitBreakerResetAfter is the cooldown before an open named-session
	// breaker resets automatically. Empty defaults to 2 * SessionCircuitBreakerWindowDuration.
	SessionCircuitBreakerResetAfter string `toml:"session_circuit_breaker_reset_after,omitempty"`
	// ShutdownTimeout is the time to wait after sending Ctrl-C before force-killing
	// agents during shutdown. Duration string (e.g., "5s", "30s"). Set to "0s"
	// for immediate kill. Defaults to "5s".
	ShutdownTimeout string `toml:"shutdown_timeout,omitempty" jsonschema:"default=5s"`
	// DoltStopTimeout is the SIGTERM→SIGKILL grace period for the managed dolt
	// subprocess during stop, unregister, restart, and startup/recovery
	// cleanup. Independent of ShutdownTimeout (which gates agent drain) so a
	// slow session drain cannot steal dolt's flush window. Duration string
	// (e.g., "30s", "1m"). A too-short value risks SIGKILL during a journal
	// index update or manifest rotation, which corrupts dolt's chunk journal
	// (see gastownhall/gascity#2090). Defaults to "30s", which absorbs the
	// longest observed flush window on commodity SSDs without unduly delaying
	// unregister. Set to "0s" for immediate SIGKILL with no grace. Negative
	// values are rejected at config load. Note: when a city is stopped via the
	// controller (`gc stop` while a controller is running), the standalone
	// controller-stop wait budget is `shutdown_timeout` + 15s (20s at the
	// default `shutdown_timeout` of "5s"); a `dolt_stop_timeout` larger than
	// that budget can be cut short on that path even though the direct
	// stop/unregister path always honors the full grace.
	DoltStopTimeout string `toml:"dolt_stop_timeout,omitempty" jsonschema:"default=30s"`
	// DoltStartAddressInUseRetryWindow is how long the managed dolt start
	// path waits on the originally requested port when bind fails with
	// "address already in use" before falling back to a higher port. The
	// common cause is a TIME_WAIT socket left by an abrupt stop of a sibling
	// dolt subprocess (external SIGTERM, supervisor restart, OOM kill); on
	// Linux the listening-socket slot typically frees within ~30s. Falling
	// back immediately publishes the rebound port to provider state, after
	// which `recoverManagedDoltShouldReuseExisting` keeps accepting the
	// rebound instance as canonical and consumers hardcoded to the original
	// port stay broken until the orphan is killed. Duration string (e.g.,
	// "30s", "1m"). Set to "0s" to disable the retry (legacy fall-back-
	// immediately behavior). Defaults to "30s". Each port is waited on at
	// most once per startManagedDoltProcessWithOptions invocation, so the
	// worst-case wall time per startup is bounded by
	// (DoltStartAddressInUseRetryWindow + per-attempt-startup) × min(5,
	// distinct-ports-tried) rather than DoltStartAddressInUseRetryWindow × 5.
	// Negative values are rejected at config load.
	DoltStartAddressInUseRetryWindow string `toml:"dolt_start_address_in_use_retry_window,omitempty" jsonschema:"default=30s"`
	// WispGCInterval is how often the garbage collector for wisps runs. A wisp is
	// an ephemeral bead produced by a v1 formula run; this knob controls how often
	// the closed ones are swept. Duration string (e.g., "5m", "1h"). Wisp GC is
	// disabled unless both WispGCInterval and WispTTL are set.
	WispGCInterval string `toml:"wisp_gc_interval,omitempty"`
	// WispTTL is how long a closed wisp (an ephemeral v1 formula-run bead) survives
	// before being purged. Duration string (e.g., "24h", "7d"). Wisp GC is disabled
	// unless both WispGCInterval and WispTTL are set.
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
	// single tick. Fresh generic pool session-bead creation uses the same
	// budget so the controller does not materialize more ordinary pool sessions
	// than it can wake. Bounded dependency-floor prerequisites are exempt.
	// Nil (unset) defaults to 5. Values <= 0 are treated as the default — set a
	// positive integer to override.
	MaxWakesPerTick *int `toml:"max_wakes_per_tick,omitempty" jsonschema:"default=5"`
	// NudgeDispatcher selects how queued nudges get delivered to running
	// sessions. "legacy" (default) auto-spawns a per-session `gc nudge poll`
	// process that polls the file-backed queue every 2s. "supervisor" runs
	// the delivery loop inside the city runtime instead, with a unix-socket
	// wake fast path triggered by enqueue, eliminating the per-session bd
	// shellout storm.
	NudgeDispatcher string `toml:"nudge_dispatcher,omitempty" jsonschema:"default=legacy,enum=legacy,enum=supervisor"`
	// AutoRestartOnDrift controls whether `gc start` automatically restarts
	// the supervisor when it detects the running supervisor's binary or
	// pack snapshot has drifted from on-disk state. Nil (unset) defaults
	// to true — operators get the correct-by-default behavior. Set to
	// false as a global kill switch (e.g., for production cities where a
	// rebuild on the host should not auto-restart the supervisor).
	AutoRestartOnDrift *bool `toml:"auto_restart_on_drift,omitempty" jsonschema:"default=true"`
	// AutoReapClosedBeadWorktrees controls whether the reconciler patrol
	// automatically removes per-bead git worktrees once their associated
	// work bead reaches closed status. Only worktrees with a clean working
	// tree, no unpushed commits, and no stashes are removed; unsafe worktrees
	// are logged as warnings and left in place for operator review. Session
	// home directories (agent template directories) are never touched.
	// Defaults to false. Set to true to enable automated worktree cleanup.
	AutoReapClosedBeadWorktrees *bool `toml:"auto_reap_closed_bead_worktrees,omitempty" jsonschema:"default=false"`
	// StartReadyTimeout is how long `gc start` and `gc register` wait for
	// the supervisor to report the city as Running. Cities with many
	// registered or adopted sessions take longer to start because the
	// per-tick wake budget (max_wakes_per_tick) throttles startup: wall
	// time to wake N sessions is roughly ceil(N / max_wakes_per_tick) *
	// patrol_interval. At the defaults (5 wakes / 30s), ~40 sessions
	// need ~4 minutes. Duration string (e.g., "5m", "10m"). Defaults to
	// DefaultStartReadyTimeout (5m). When set, this value replaces the
	// default start/register budget; [session].startup_timeout may still
	// extend the effective wait for a slow single session.
	StartReadyTimeout string `toml:"start_ready_timeout,omitempty" jsonschema:"default=5m"`
	// TickDebounce coalesces bursty event-driven ticks (pokeCh,
	// controlDispatcherCh) within this window. A first event in a quiet
	// period arms a timer; subsequent events arriving before the timer
	// fires are dropped (the single delayed tick re-reads authoritative
	// state covering all collapsed events). Zero (the default) disables
	// debouncing — each event fires its own tick, matching pre-existing
	// behavior. Duration string (e.g., "250ms", "500ms"). Trade-off:
	// adds tick latency up to this value when set.
	TickDebounce string `toml:"tick_debounce,omitempty"`
	// AutoPruneWorkerDir controls whether the reconciler removes a
	// pool-managed session's worker_dir (agent worktree) after the session
	// bead is closed. Removal is gated on: path lives under the city's
	// .gc/worktrees/ tree, clean working tree, no unpushed commits, no
	// stashed work. Nil (unset) defaults to true so pool worktrees do not
	// accumulate without bound across pool recycles. Set to false to
	// retain worktrees for post-session diagnostics.
	AutoPruneWorkerDir *bool `toml:"auto_prune_worker_dir,omitempty" jsonschema:"default=true"`
}

// AutoRestartOnDriftEnabled reports whether the supervisor should be
// auto-restarted when `gc start` detects binary or pack drift. The
// default is true: operators get correct-by-default behavior. The
// per-invocation `--no-auto-restart` flag does NOT override the config
// kill switch — production safety wins.
func (d *DaemonConfig) AutoRestartOnDriftEnabled() bool {
	if d.AutoRestartOnDrift == nil {
		return true
	}
	return *d.AutoRestartOnDrift
}

// AutoReapClosedBeadWorktreesEnabled reports whether the patrol should
// automatically remove per-bead git worktrees for closed beads. Defaults
// to false when the field is unset (nil). Set to true in city.toml to
// enable automated cleanup.
func (d *DaemonConfig) AutoReapClosedBeadWorktreesEnabled() bool {
	if d.AutoReapClosedBeadWorktrees == nil {
		return false
	}
	return *d.AutoReapClosedBeadWorktrees
}

// AutoPruneWorkerDirEnabled reports whether the reconciler should remove a
// pool-managed session's worker_dir after the session bead is closed. The
// default is true: pool worktrees are transient by design and accumulate
// without bound otherwise. Removal is still gated on per-worktree safety
// probes (clean tree, no unpushed commits, no stashes).
func (d *DaemonConfig) AutoPruneWorkerDirEnabled() bool {
	if d.AutoPruneWorkerDir == nil {
		return true
	}
	return *d.AutoPruneWorkerDir
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

// TickDebounceDuration returns the tick-debounce window as a
// time.Duration. Returns 0 (debouncing disabled) on empty, unparseable,
// or negative input.
func (d *DaemonConfig) TickDebounceDuration() time.Duration {
	if d.TickDebounce == "" {
		return 0
	}
	dur, err := time.ParseDuration(d.TickDebounce)
	if err != nil || dur < 0 {
		return 0
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

// SessionCircuitBreakerMaxRestartsOrDefault returns the named-session respawn
// circuit-breaker threshold. Nil reuses MaxRestartsOrDefault; zero disables it.
func (d *DaemonConfig) SessionCircuitBreakerMaxRestartsOrDefault() int {
	if d.SessionCircuitBreakerMaxRestarts == nil {
		return d.MaxRestartsOrDefault()
	}
	return *d.SessionCircuitBreakerMaxRestarts
}

// SessionCircuitBreakerWindowDuration returns the named-session respawn
// circuit-breaker rolling window. Empty reuses RestartWindowDuration.
func (d *DaemonConfig) SessionCircuitBreakerWindowDuration() time.Duration {
	if d.SessionCircuitBreakerWindow == "" {
		return d.RestartWindowDuration()
	}
	dur, err := time.ParseDuration(d.SessionCircuitBreakerWindow)
	if err != nil {
		return d.RestartWindowDuration()
	}
	return dur
}

// SessionCircuitBreakerResetAfterDuration returns the named-session respawn
// circuit-breaker cooldown. Empty or invalid values default to 2 * window.
func (d *DaemonConfig) SessionCircuitBreakerResetAfterDuration() time.Duration {
	window := d.SessionCircuitBreakerWindowDuration()
	if d.SessionCircuitBreakerResetAfter == "" {
		return 2 * window
	}
	dur, err := time.ParseDuration(d.SessionCircuitBreakerResetAfter)
	if err != nil {
		return 2 * window
	}
	return dur
}

// NudgeDispatcherMode returns the nudge dispatcher mode, defaulting to
// "legacy". Unknown values are treated as "legacy" so a malformed config
// does not silently disable the per-session pollers.
func (d *DaemonConfig) NudgeDispatcherMode() string {
	switch d.NudgeDispatcher {
	case "supervisor":
		return "supervisor"
	default:
		return "legacy"
	}
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

// DefaultDoltStopTimeout is the SIGTERM→SIGKILL grace for the managed dolt
// subprocess when no value is configured. 30s is long enough to ride out the
// longest observed journal-index update on commodity SSDs (#2090) while still
// guaranteeing forward progress on unregister/restart.
const DefaultDoltStopTimeout = 30 * time.Second

// DoltStopTimeoutDuration returns the managed-dolt stop grace as a
// time.Duration. Defaults to DefaultDoltStopTimeout (30s) when empty or
// unparseable. Zero is allowed and means immediate SIGKILL — callers that
// must guarantee a flush window should treat zero as a misconfiguration
// upstream rather than silently overriding it here.
func (d *DaemonConfig) DoltStopTimeoutDuration() time.Duration {
	if d.DoltStopTimeout == "" {
		return DefaultDoltStopTimeout
	}
	dur, err := time.ParseDuration(d.DoltStopTimeout)
	if err != nil {
		return DefaultDoltStopTimeout
	}
	return dur
}

// DefaultDoltStartAddressInUseRetryWindow is the per-port retry window used
// when dolt's bind fails with "address already in use" before the start path
// falls back to the next available port. 30s is roughly half Linux's default
// TCP TIME_WAIT — the listening-socket slot typically frees well before the
// full TIME_WAIT elapses because there are no active half-open connections
// during a clean restart. Values up to 60s are safer for kernels with
// tcp_fin_timeout raised; values below 10s materially shrink the window for
// outliving TIME_WAIT.
const DefaultDoltStartAddressInUseRetryWindow = 30 * time.Second

// DoltStartAddressInUseRetryWindowDuration returns the configured retry
// window for the managed-dolt address-in-use loop as a time.Duration.
// Defaults to DefaultDoltStartAddressInUseRetryWindow (30s) when empty or
// unparseable. Zero disables the retry — callers fall back to a higher port
// immediately, matching legacy behavior. Negative values pass through
// unchanged: callers that route through loadCityConfig already reject them
// via ValidateNonNegativeDurations, so a negative reaching this helper
// implies a hand-rolled DaemonConfig that bypassed validation — treat zero
// as a misconfiguration upstream rather than silently overriding it here.
// Mirrors DoltStopTimeoutDuration's policy.
func (d *DaemonConfig) DoltStartAddressInUseRetryWindowDuration() time.Duration {
	if d.DoltStartAddressInUseRetryWindow == "" {
		return DefaultDoltStartAddressInUseRetryWindow
	}
	dur, err := time.ParseDuration(d.DoltStartAddressInUseRetryWindow)
	if err != nil {
		return DefaultDoltStartAddressInUseRetryWindow
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

// DefaultStartReadyTimeout is the default wall-clock budget `gc start` and
// `gc register` allow for the supervisor to report a city as Running.
// Sized to cover cities with up to ~40 sessions at the default per-tick
// wake budget; operators with larger cities override via
// [daemon].start_ready_timeout.
const DefaultStartReadyTimeout = 5 * time.Minute

// StartReadyTimeoutDuration returns the start-ready wait budget as a
// time.Duration. Defaults to DefaultStartReadyTimeout when empty or
// unparseable.
func (d *DaemonConfig) StartReadyTimeoutDuration() time.Duration {
	if d.StartReadyTimeout == "" {
		return DefaultStartReadyTimeout
	}
	dur, err := time.ParseDuration(d.StartReadyTimeout)
	if err != nil {
		return DefaultStartReadyTimeout
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

// parseConfigDurationWithDays extends time.ParseDuration with a whole-day "d"
// unit. It accepts ordinary Go durations unchanged and supports one leading day
// component such as "7d" or "1d12h".
func parseConfigDurationWithDays(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty duration")
	}
	dayIdx := strings.IndexByte(raw, 'd')
	if dayIdx < 0 {
		return time.ParseDuration(raw)
	}
	days, err := parsePositiveDurationDays(raw[:dayIdx])
	if err != nil {
		return 0, err
	}
	dayDuration := time.Duration(days) * 24 * time.Hour
	rest := raw[dayIdx+1:]
	if rest == "" {
		return dayDuration, nil
	}
	dur, err := time.ParseDuration(rest)
	if err != nil {
		return 0, err
	}
	return addConfigDurations(dayDuration, dur)
}

func parsePositiveDurationDays(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("missing day count")
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid day count %q", raw)
		}
	}
	days, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if days <= 0 {
		return 0, fmt.Errorf("day count must be positive")
	}
	if days > maxConfigDurationDays {
		return 0, fmt.Errorf("day count %q exceeds maximum %d", raw, maxConfigDurationDays)
	}
	return days, nil
}

const maxConfigDurationDays = int64(1<<63-1) / int64(24*time.Hour)

func addConfigDurations(a, b time.Duration) (time.Duration, error) {
	if b > 0 && a > time.Duration(1<<63-1)-b {
		return 0, fmt.Errorf("duration exceeds maximum")
	}
	if b < 0 && a < time.Duration(-1<<63)-b {
		return 0, fmt.Errorf("duration is below minimum")
	}
	return a + b, nil
}

// FormulasDir returns the formulas directory, defaulting to "formulas".
func (c *City) FormulasDir() string {
	if c.Formulas.Dir != "" {
		return c.Formulas.Dir
	}
	return citylayout.FormulasRoot
}

// AllPackDirs returns the union of city-level and all rig-level pack directories
// (city dirs first, then sorted-by-rig-name dirs), deduplicated. Use this for
// global scans that intentionally need the full pack-fragment universe. Prompt
// rendering for a specific rig should use PackDirsForRig so one rig's fragments
// cannot override another rig's same-named fragments.
func (c *City) AllPackDirs() []string {
	var dirs []string
	dirs = appendUnique(dirs, c.PackDirs...)
	rigNames := make([]string, 0, len(c.RigPackDirs))
	for name := range c.RigPackDirs {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)
	for _, name := range rigNames {
		dirs = appendUnique(dirs, c.RigPackDirs[name]...)
	}
	return dirs
}

// PackDirsForRig returns the city-level pack directories plus the pack
// directories imported by rigName, deduplicated with city-level dirs kept first.
// Use this when rendering prompts for one agent so rig-imported template
// fragments are available without exposing fragments imported by other rigs.
func (c *City) PackDirsForRig(rigName string) []string {
	var dirs []string
	dirs = appendUnique(dirs, c.PackDirs...)
	if rigName != "" {
		dirs = appendUnique(dirs, c.RigPackDirs[rigName]...)
	}
	return dirs
}

// AgentDefaults provides agent defaults declared via [agent_defaults] in
// city.toml or pack.toml. The runtime currently applies provider,
// default_sling_formula, and append_fragments; the remaining fields are parsed
// and composed but are not yet inherited onto agents automatically.
type AgentDefaults struct {
	// Provider is the default provider name for agents that do not set their
	// own provider. It also counts as a configured provider for implicit agent
	// injection.
	Provider string `toml:"provider,omitempty"`
	// Model is the parsed/composed default model name for agents
	// (e.g., "claude-sonnet-4-6"), but it is not yet auto-applied at
	// runtime. Agents with their own model override would take precedence.
	Model string `toml:"model,omitempty"`
	// Upstream is the default model-serving endpoint (a key in [upstreams])
	// for agents that do not set their own upstream (Phase C — the Upstream
	// axis). Applied to agents with an empty Upstream by ApplyAgentDefaults.
	Upstream string `toml:"upstream,omitempty"`
	// WakeMode is the parsed/composed default wake mode ("resume" or
	// "fresh"), but it is not yet auto-applied at runtime.
	WakeMode string `toml:"wake_mode,omitempty" jsonschema:"enum=resume,enum=fresh"`
	// DefaultSlingFormula is the default formula used for agents that inherit
	// [agent_defaults]. Explicit agents only receive this value when
	// agent_defaults.default_sling_formula is set; implicit multi-session
	// configs are seeded with "mol-do-work" elsewhere when no explicit default is set.
	DefaultSlingFormula string `toml:"default_sling_formula,omitempty"`
	// AllowOverlay is parsed and composed as a config-level allowlist for
	// session overlays, but it is not yet inherited onto agents automatically
	// at runtime.
	AllowOverlay []string `toml:"allow_overlay,omitempty"`
	// AllowEnvOverride is parsed and composed as a config-level allowlist for
	// session env overrides, but it is not yet inherited onto agents
	// automatically at runtime. Names must match ^[A-Z][A-Z0-9_]{0,127}$.
	AllowEnvOverride []string `toml:"allow_env_override,omitempty"`
	// AppendFragments lists named template fragments to auto-append to
	// .template.md prompts after rendering. Legacy .md.tmpl prompts are
	// still supported during the transition; plain .md remains inert.
	// V2 migration convenience — replaces global_fragments/inject_fragments
	// for config-wide defaults.
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
	if !meta.IsDefined("agent_defaults", "provider") {
		dst.Provider = src.Provider
	}
	if !meta.IsDefined("agent_defaults", "model") {
		dst.Model = src.Model
	}
	if !meta.IsDefined("agent_defaults", "upstream") {
		dst.Upstream = src.Upstream
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

// FoldAgentDefaultsAlias folds a legacy [agents] alias table into the canonical
// [agent_defaults] target so a struct round-trip that emits only
// [agent_defaults] does not silently drop the alias. When both tables are
// present the canonical values win on overlapping keys and the alias only fills
// gaps; when only the alias is present its values become the canonical defaults.
// meta must be the toml.MetaData from decoding the same document. Callers are
// responsible for clearing their own alias field afterward.
func FoldAgentDefaultsAlias(canonical *AgentDefaults, alias AgentDefaults, meta toml.MetaData) {
	if meta.IsDefined("agent_defaults") {
		if meta.IsDefined("agents") {
			mergeAgentDefaultsAliasPreferCanonical(canonical, alias, meta)
		}
		return
	}
	if meta.IsDefined("agents") {
		*canonical = alias
	}
}

func normalizeAgentDefaultsAlias(cfg *City, meta toml.MetaData) {
	FoldAgentDefaultsAlias(&cfg.AgentDefaults, cfg.AgentsDefaults, meta)
	cfg.AgentsDefaults = AgentDefaults{}
}

const (
	// AgentLifecycleOneShot marks an agent command as intentionally short-lived.
	AgentLifecycleOneShot = "one_shot"
)

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
	// TmuxAlias overrides the tmux session_name for pool and factory-created
	// manual sessions of this agent. When unset, sessions fall back to the
	// universal derivation ("s-<beadID>" for ad-hoc sessions,
	// "<basename>-<beadID>" for pool sessions). When set, it is expanded as a
	// Go text/template using the same PathContext fields as work_dir /
	// session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName),
	// sanitized for tmux, and validated as an explicit session name. For pool
	// sessions, a live-name collision appends the bead ID as a deterministic
	// suffix. For manual `gc session new` sessions, tmux_alias becomes the
	// explicit session_name and takes precedence over --alias, which remains the
	// command/mail alias; duplicate explicit names fail closed. Configured named
	// sessions keep their named-session runtime name instead of using
	// tmux_alias. When no --alias is supplied, work_dir templates that use
	// {{.Agent}} see the resolved tmux_alias as the concrete session identity.
	TmuxAlias string `toml:"tmux_alias,omitempty"`
	// Scope defines where this agent is instantiated: "city" (one per city) or
	// "rig" (one per rig). Omit the field for an unscoped agent instantiated in
	// both city and rig expansion contexts. Only meaningful for pack-defined
	// agents; inline agents in city.toml use Dir directly.
	Scope string `toml:"scope,omitempty" jsonschema:"enum=city,enum=rig"`
	// Suspended prevents the reconciler from spawning this agent. Toggle with gc agent suspend/resume.
	Suspended bool `toml:"suspended,omitempty"`
	// PreStart is a list of shell commands run before session creation.
	// Commands run on the target filesystem: locally for tmux, inside the
	// pod/container for exec providers. Template variables same as session_setup.
	// On failure, the last 4 KiB of the command's stdout/stderr is included
	// in the error and may appear in controller and reconciler logs; avoid
	// set -x or echoing secrets in setup commands.
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
	// Upstream selects the model-serving endpoint (a key in [upstreams]) for
	// this agent — WHO serves the model. "" (default) falls back to
	// agent_defaults.upstream; if still empty, no upstream env is injected
	// (ambient behavior). Switching it relaunches the agent in the warm box.
	Upstream string `toml:"upstream,omitempty"`
	// InheritedProvider records the pack-scoped default provider for agents
	// loaded from imported packs. Runtime-only.
	InheritedProvider string `toml:"-" json:"-"`
	// StartCommand overrides the provider's command for this agent.
	StartCommand string `toml:"start_command,omitempty"`
	// Lifecycle controls runtime lifetime semantics. Empty uses the default
	// long-lived session lifecycle; "one_shot" means the command is expected
	// to do bounded work and exit cleanly.
	Lifecycle string `toml:"lifecycle,omitempty" jsonschema:"enum=one_shot"`
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
	// This controls pool sessions independently of [[named_session]]
	// mode="always"; both produce sessions, and gc doctor reports accidental
	// combinations.
	MinActiveSessions *int `toml:"min_active_sessions,omitempty"`
	// ScaleCheck is a shell command template whose output reports new
	// unassigned session demand. In bead-backed reconciliation this is
	// additive: assigned work is resumed separately, and ScaleCheck reports
	// only how many new generic sessions to start, still bounded by all cap
	// levels. Legacy no-store evaluation continues to treat the output as
	// the desired session count. If it contains Go template placeholders, gc
	// expands them using the same PathContext fields as work_dir and
	// session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName)
	// before running the command.
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
	// MaxSessionAge is the maximum wall-clock lifetime of a single runtime
	// session before the controller preemptively restarts it. Duration string
	// (e.g., "5h"). Empty (default) disables preemptive restarts. The restart
	// is idle-gated: sessions with a pending interaction or an in-progress
	// assigned work bead are left alone until they settle.
	//
	// Motivation: provider SDKs that cache credentials at session start (e.g.,
	// Claude Code via Bedrock) can wedge when the underlying token expires if
	// the SDK doesn't re-chain providers. Cycling long-running sessions before
	// the token-expiry window prevents that failure mode without requiring
	// upstream provider fixes.
	MaxSessionAge string `toml:"max_session_age,omitempty"`
	// MaxSessionAgeJitter bounds random jitter added to MaxSessionAge on a
	// per-session basis so a fleet of identically-configured agents doesn't
	// synchronize restarts. Duration string (e.g., "15m"). Empty or 0
	// disables jitter (every session restarts at exactly MaxSessionAge).
	// Ignored when MaxSessionAge is unset.
	MaxSessionAgeJitter string `toml:"max_session_age_jitter,omitempty"`
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
	// On failure, the last 4 KiB of the command's stdout/stderr is included
	// in the error and may appear in controller and reconciler logs; avoid
	// set -x or echoing secrets in setup commands.
	SessionSetup []string `toml:"session_setup,omitempty"`
	// SessionSetupScript is the path to a script run after session_setup commands.
	// Relative paths resolve against the declaring config file's directory
	// (pack-safe). Paths prefixed with "//" resolve against the city root.
	// The script receives context via environment variables (GC_SESSION plus
	// existing GC_* vars). On failure, the last 4 KiB of the script's
	// stdout/stderr is included in the error and may appear in controller
	// and reconciler logs; avoid set -x or echoing secrets in setup scripts.
	SessionSetupScript string `toml:"session_setup_script,omitempty"`
	// SessionLive is a list of shell commands that are safe to re-apply
	// without restarting the agent. Run at startup (after session_setup)
	// and re-applied on config change without triggering a restart.
	// Must be idempotent. Typical use: tmux theming, keybindings, status bars.
	// Same template placeholders as session_setup.
	// On failure, the last 4 KiB of the command's stdout/stderr is included
	// in the error and may appear in controller and reconciler logs; avoid
	// set -x or echoing secrets in setup commands.
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
	// AppendFragments is the V2 per-agent alias for prompt fragment injection.
	// It layers after InjectFragments and before inherited/default fragments.
	AppendFragments []string `toml:"append_fragments,omitempty"`
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
	// MouseMode controls whether tmux mouse mode is preserved for this agent.
	// "on" leaves the session's mouse setting alone for human-attached
	// sessions; "off" or empty preserves the SDK's default mouse-off startup
	// behavior for headless sessions.
	MouseMode string `toml:"mouse_mode,omitempty" jsonschema:"enum=on,enum=off"`
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
	// source records which configuration origin this agent came from
	// (inline city.toml, an explicit pack import, or an auto-imported
	// system pack). Stamped once at discovery; consumed by
	// describeSource to render duplicate-name errors that never carry
	// an empty quoted path. Unexported so the value is package-private
	// runtime state and never appears on any wire. (See ga-tpfc.)
	source agentSource
	// layout records which pack layout (v1 inline [[agent]] block in
	// pack.toml vs. v2 agents/<name>/agent.toml convention) produced
	// this agent. Stamped once at discovery; consumed by
	// formatDuplicateAgentError to specialize the (V1Inline,
	// V2Convention) collision into a migration-guidance variant.
	// Unexported, runtime-only, never on any wire. (See ga-9ogb.)
	layout agentLayout
}

// agentSource enumerates the configuration origins recognized by
// describeSource. Discovery sites stamp exactly one value per agent.
type agentSource uint8

const (
	// sourceUnknown is the zero value; agents reach validation
	// without a stamp when they came through a non-discovery code
	// path (e.g., test fixtures that build Agents directly).
	sourceUnknown agentSource = iota
	// sourceInline marks agents declared as inline [[agent]] tables
	// in the root city.toml.
	sourceInline
	// sourcePack marks agents that came from an explicit pack
	// import or a workspace.includes entry.
	sourcePack
	// sourceAutoImport marks agents that came from a binding the
	// city pack added via [defaults.rig.imports] (e.g., the
	// gastown system pack). Even though the binding ends up in
	// city.Imports after composition, the user did not write it.
	sourceAutoImport
)

// agentLayout enumerates the pack-layout origin of an agent: which
// on-disk shape produced it. Exactly one value is stamped at
// discovery time. Used by formatDuplicateAgentError to detect a v1↔v2
// migration collision and emit a migration-guidance error. Unexported
// so the type is package-private runtime state and never appears on
// any wire.
type agentLayout uint8

const (
	// layoutUnknown is the zero value. Agents reach validation
	// without a layout stamp when they came through a non-discovery
	// path (test fixtures, city.toml inline [[agent]] blocks — those
	// last are a third category, not v1).
	layoutUnknown agentLayout = iota
	// layoutV1Inline marks agents declared as inline [[agent]] blocks
	// in a pack's pack.toml.
	layoutV1Inline
	// layoutV2Convention marks agents discovered from a pack's
	// agents/<name>/agent.toml file.
	layoutV2Convention
)

// String renders an agentLayout for debug logs only.
func (l agentLayout) String() string {
	switch l {
	case layoutV1Inline:
		return "v1-inline"
	case layoutV2Convention:
		return "v2-convention"
	}
	return "unknown"
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

// MaxSessionAgeDuration returns the maximum session age as a time.Duration.
// Returns 0 if empty or unparseable (disabled: no preemptive restart).
func (a *Agent) MaxSessionAgeDuration() time.Duration {
	if a.MaxSessionAge == "" {
		return 0
	}
	d, err := time.ParseDuration(a.MaxSessionAge)
	if err != nil {
		return 0
	}
	return d
}

// MaxSessionAgeJitterDuration returns the jitter bound for max session age
// as a time.Duration. Returns 0 when the jitter field is empty, unparseable,
// or when MaxSessionAge itself is disabled.
func (a *Agent) MaxSessionAgeJitterDuration() time.Duration {
	if a.MaxSessionAgeDuration() == 0 {
		return 0
	}
	if a.MaxSessionAgeJitter == "" {
		return 0
	}
	d, err := time.ParseDuration(a.MaxSessionAgeJitter)
	if err != nil || d < 0 {
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

// MouseModeOn reports whether tmux mouse mode should be preserved for this agent.
func (a *Agent) MouseModeOn() bool {
	return a.MouseMode == "on"
}

// AttachEnabled reports whether the agent supports interactive attachment.
func (a *Agent) AttachEnabled() bool {
	return a.Attach == nil || *a.Attach
}

// bdReadyPoolDemandShell returns the canonical bd ready predicate for
// unassigned, non-epic pool demand routed to target. gc.routed_to is the
// canonical persisted routing key: the graph.v2 stamper and the legacy stamper
// both stamp it on every routable bead, including the workflow root (ga-eld2x
// retired the short-lived gc.run_target wire field). This predicate is the main
// source of truth for "is there work on this routed queue?" that both the
// worker (via EffectiveWorkQuery Tier 3) and the reconciler (via
// EffectivePoolDemandQuery, count-form) ask; diverging the two re-introduces
// the protocol-mismatch class (see the "scale_check ↔ work_query
// correspondence" note in engdocs/architecture/dispatch.md).
//
// target is passed as a positional argument to the outer sh -c command, not
// interpolated into the nested shell body. That keeps routes containing shell
// metacharacters as data instead of executable syntax.
func bdReadyIncludeEphemeralArg(includeEphemeralReady bool) string {
	if includeEphemeralReady {
		return " --include-ephemeral"
	}
	return ""
}

// jqMeta renders the jq expression that reads a bead-metadata key with an
// empty-string default, e.g. (.metadata["gc.routed_to"] // ""). Shell/jq
// builders use it so embedded key spellings stay anchored to the beadmeta
// vocabulary constants.
func jqMeta(key string) string {
	return `(.metadata["` + key + `"] // "")`
}

func bdReadyPoolDemandShell(limitFlag string, includeEphemeralReady bool) string {
	return `bd ready` + bdReadyIncludeEphemeralArg(includeEphemeralReady) + ` --metadata-field "` + beadmeta.RoutedToMetadataKey + `=$target" --unassigned --exclude-type=epic --json ` + limitFlag
}

// bdReadyPoolDemandMigrationShell is a temporary raw compatibility probe for
// graph.v2 workflow roots created before gc.routed_to root stamping shipped.
// It is scoped to workflow roots so gc.run_target remains an authoring hint
// everywhere else. Callers must pass its output through
// poolDemandMigrationFilterJQ so a stale divergent gc.run_target cannot remain
// visible once a root carries gc.routed_to. This retirement-window fallback
// requires jq in the default worker/reconciler environment; remove it with the
// Go-side legacy candidates after the backfill completion tracked by ga-dhf44.
func bdReadyPoolDemandMigrationShell(limitFlag string, includeEphemeralReady bool) string {
	return `bd ready` + bdReadyIncludeEphemeralArg(includeEphemeralReady) + ` --metadata-field "` + beadmeta.RunTargetMetadataKey + `=$target" --metadata-field "` + beadmeta.KindMetadataKey + `=` + beadmeta.KindWorkflow + `" --unassigned --exclude-type=epic --json --sort oldest ` + limitFlag
}

func poolDemandMigrationFilterJQ(limit int) string {
	filter := `[.[] | select(` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "")]`
	if limit > 0 {
		filter += ` | .[:` + strconv.Itoa(limit) + `]`
	}
	return shellquote.Join([]string{"jq", filter})
}

func bdQueryEphemeralStatusShell(status string) string {
	return `bd query --json ` + shellquote.Quote("ephemeral=true AND status="+status) + ` --limit=0`
}

func bdQueryEphemeralStatusQuietShell(status string) string {
	return bdQueryEphemeralStatusShell(status) + ` 2>/dev/null`
}

func legacyEphemeralReadyFilterJQ(selector string, limit int) string {
	filter := `[.[] | ` + selector +
		` | select(((.issue_type // .type // "") != "epic"))` +
		` | select(([ (.dependencies // [])[]` +
		` | select((.type // .dep_type // "") as $t | ($t == "blocks" or $t == "waits-for" or $t == "conditional-blocks"))` +
		` | select((.status // .depends_on_status // "") != "closed") ] | length) == 0)]` +
		` | sort_by(.created_at // "")`
	if limit > 0 {
		filter += ` | .[:` + strconv.Itoa(limit) + `]`
	}
	return filter
}

func legacyEphemeralPoolDemandShell(limit int, includeEphemeralReady, quiet bool) string {
	if includeEphemeralReady {
		return `printf "[]"`
	}
	filter := legacyEphemeralReadyFilterJQ(
		`select((.assignee // "") == "")`+
			` | select((`+jqMeta(beadmeta.RoutedToMetadataKey)+` == $target) or ((`+jqMeta(beadmeta.RoutedToMetadataKey)+` == "") and (`+jqMeta(beadmeta.RunTargetMetadataKey)+` == $target) and (`+jqMeta(beadmeta.KindMetadataKey)+` == "`+beadmeta.KindWorkflow+`")))`,
		limit,
	)
	query := bdQueryEphemeralStatusShell("open")
	if quiet {
		query = bdQueryEphemeralStatusQuietShell("open")
	}
	jqStderr := ""
	if quiet {
		jqStderr = ` 2>/dev/null`
	}
	return `{ ` + query + ` | jq --arg target "$target" ` + shellquote.Quote(filter) + jqStderr + `; } || printf "[]"`
}

// poolDemandFirstRowFunctionScript emits the work_query Tier 3 function: it
// reads the first ready, unassigned, routed bead for the supplied target,
// prints it, and exits 0. The caller appends a terminal fallthrough
// (printf "[]") for the empty case.
func poolDemandFirstRowFunctionScript(includeEphemeralReady bool) string {
	return `probe_pool_demand() { ` +
		`target="$1"; ` +
		`[ -z "$target" ] && return 1; ` +
		`r=$(` + routedReadyTierCommand(includeEphemeralReady) + `); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`legacy_candidates=$(` + bdReadyPoolDemandMigrationShell("--limit=20", includeEphemeralReady) + ` 2>/dev/null); ` +
		`r=$(printf "%s" "$legacy_candidates" | ` + poolDemandMigrationFilterJQ(1) + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`legacy_ephemeral_candidates=$(` + legacyEphemeralPoolDemandShell(20, includeEphemeralReady, true) + `); ` +
		`r=$(printf "%s" "$legacy_ephemeral_candidates" | jq '.[0:1]' 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`return 1; ` +
		`}; `
}

func routedReadyTierCommand(includeEphemeralReady bool) string {
	// The shared predicate stays order-free so the count-form does no wasted
	// sorting; the worker first-row path asks bd for the oldest candidate.
	return bdReadyPoolDemandShell("--sort oldest --limit=1", includeEphemeralReady) + ` 2>/dev/null`
}

// poolDemandCountShell emits the reconciler count-form for target: it counts
// ready, unassigned, routed demand and prints the array length. It shares the
// canonical and migration predicates with poolDemandFirstRowFunctionScript so
// the reconciler's spawn decision and the worker's claim decision read the
// same demand shape.
//
// Unlike the work_query probe, this form must NOT redirect bd stderr or default
// to zero: a failed `bd ready` has to surface as an error rather than
// masquerade as "no demand", which would silently stop the pool from spawning.
// The && chain ensures any non-zero bd exit short-circuits the whole expression
// (TestEffectiveScaleCheckUsesReadyOnly).
func poolDemandCountShell(target string, includeEphemeralReady bool) string {
	script := `target="$1"; ` +
		`ready_json=$(` + bdReadyPoolDemandShell("--limit 0", includeEphemeralReady) + `) || exit $?; ` +
		`legacy_candidates=$(` + bdReadyPoolDemandMigrationShell("--limit 0", includeEphemeralReady) + `) || exit $?; ` +
		`legacy_json=$(printf "%s" "$legacy_candidates" | ` + poolDemandMigrationFilterJQ(0) + `) || exit $?; ` +
		`legacy_ephemeral_json=$(` + legacyEphemeralPoolDemandShell(0, includeEphemeralReady, false) + `); ` +
		`printf "%s\n%s\n%s\n" "$ready_json" "$legacy_json" "$legacy_ephemeral_json" | jq -s "(add // []) | unique_by(.id) | length"`
	return shellquote.Join([]string{"sh", "-c", script, "--", target})
}

func (a *Agent) poolDemandTarget() string {
	target := a.QualifiedName()
	if a.PoolName != "" {
		target = a.PoolName
	}
	return target
}

func standardAssignedWorkQueryScript(includeEphemeralReady bool) string {
	return standardAssignedInProgressWorkQueryScript(includeEphemeralReady) +
		standardAssignedReadyWorkQueryScript(includeEphemeralReady)
}

func standardAssignedInProgressWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`r=$(bd list --status in_progress --assignee="$id" --json --limit=1 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedInProgressProbeScript("id", includeEphemeralReady) +
		`done; `
}

func standardAssignedReadyWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`r=$(bd ready` + bdReadyIncludeEphemeralArg(includeEphemeralReady) + ` --assignee="$id" --json --limit=1 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedReadyProbeScript("id", includeEphemeralReady) +
		`done; `
}

func legacyControlAssignedWorkQueryScript(includeEphemeralReady bool) string {
	return legacyControlAssignedInProgressWorkQueryScript(includeEphemeralReady) +
		legacyControlAssignedReadyWorkQueryScript(includeEphemeralReady)
}

func legacyControlAssignedInProgressWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(bd list --status in_progress --assignee="$cand" --json --limit=1 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedInProgressProbeScript("cand", includeEphemeralReady) +
		`done; ` +
		`done; `
}

func legacyControlAssignedReadyWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(bd ready` + bdReadyIncludeEphemeralArg(includeEphemeralReady) + ` --assignee="$cand" --json --limit=1 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedReadyProbeScript("cand", includeEphemeralReady) +
		`done; ` +
		`done; `
}

func ephemeralAssignedInProgressProbeScript(shellVar string, includeEphemeralReady bool) string {
	_ = includeEphemeralReady
	return `r=$(` + bdQueryEphemeralStatusQuietShell("in_progress") + ` | ` +
		`jq --arg id "$` + shellVar + `" '[.[] | select((.assignee // "") == $id)] | .[:1]' 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; `
}

func ephemeralAssignedReadyProbeScript(shellVar string, includeEphemeralReady bool) string {
	if includeEphemeralReady {
		return ""
	}
	filter := legacyEphemeralReadyFilterJQ(`select((.assignee // "") == $id)`, 1)
	return `r=$(` + bdQueryEphemeralStatusQuietShell("open") + ` | ` +
		`jq --arg id "$` + shellVar + `" ` + shellquote.Quote(filter) + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; `
}

func poolDemandOriginGateScript() string {
	return `case "$GC_SESSION_ORIGIN" in ` +
		`ephemeral|"") ;; ` +
		`*) exit 0 ;; ` +
		`esac; `
}

func routedPoolWorkQueryProbeScript(includeEphemeralReady bool, targetCount int) string {
	script := poolDemandOriginGateScript() + poolDemandFirstRowFunctionScript(includeEphemeralReady)
	for i := 1; i <= targetCount; i++ {
		script += fmt.Sprintf(`probe_pool_demand "$%d"; `, i)
	}
	return script + `printf "[]"`
}

func routedPoolWorkQueryCommand(includeEphemeralReady bool, targets ...string) string {
	args := []string{"sh", "-c", routedPoolWorkQueryProbeScript(includeEphemeralReady, len(targets)), "--"}
	args = append(args, targets...)
	return shellquote.Join(args)
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
// Executable formula roots can be epic-typed; the bead storage policy decides
// whether those roots are history-backed, no-history, or ephemeral for the
// configured bd compatibility mode. Molecule containers are not routable
// demand.
//
// Parent epics are excluded from the routed (pool) tier only
// (--exclude-type=epic). An unassigned parent epic has no executable spec —
// its semantic is "all children done" — so a pool worker claiming one does
// undefined work (gc-udx; the repro is a routed parent epic, see
// TestEffectiveWorkQuerySkipsEpicLeafScenario). The assigned tiers do NOT
// exclude epics: work already assigned to this agent is owned, and the
// patrol-loop pattern (gastown witness/refinery/deacon) can self-assign an
// epic wisp that the agent must resume after a session restart. Excluding
// epics there silently stranded those wisps (gc hook exited 1 with empty
// output). Roles that need different behavior still opt in via an explicit
// work_query in their agent config; that custom query is returned unchanged
// above.
//
// When the reconciler runs the query for demand detection (no session
// context), all identity vars are empty → assignee tiers skip → only
// the routed_to tier fires to detect new demand.
//
// Tier 3's canonical and migration predicates are shared with
// EffectivePoolDemandQuery so reconciler spawn decisions and worker claim
// decisions stay symmetric.
func (a *Agent) EffectiveWorkQuery() string {
	return a.effectiveWorkQuery(false)
}

// EffectiveWorkQueryForBeads returns the default work query using the bd
// compatibility semantics configured for the city.
func (a *Agent) EffectiveWorkQueryForBeads(beads BeadsConfig) string {
	return a.effectiveWorkQuery(beads.UsesBD105ReadySemantics())
}

func (a *Agent) effectiveWorkQuery(includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		script := standardAssignedWorkQueryScript(includeEphemeralReady) +
			poolDemandOriginGateScript() +
			poolDemandFirstRowFunctionScript(includeEphemeralReady) +
			`probe_pool_demand "$1"; ` +
			`printf "[]"`
		return shellquote.Join([]string{"sh", "-c", script, "--", target})
	}
	script := legacyControlAssignedWorkQueryScript(includeEphemeralReady) +
		poolDemandOriginGateScript() +
		poolDemandFirstRowFunctionScript(includeEphemeralReady) +
		`probe_pool_demand "$1"; ` +
		`probe_pool_demand "$2"; ` +
		`printf "[]"`
	return shellquote.Join([]string{"sh", "-c", script, "--", target, legacyTarget})
}

// EffectiveAssignedInProgressQuery returns the assigned-in-progress-only command
// for prompt templates that spell out crash recovery as a separate startup tier.
// A custom WorkQuery is treated as the caller-owned full discovery contract, so
// split-tier prompts may run that same custom command in each query slot.
func (a *Agent) EffectiveAssignedInProgressQuery() string {
	return a.effectiveAssignedInProgressQuery(false)
}

// EffectiveAssignedInProgressQueryForBeads returns the assigned-in-progress
// query using the bd compatibility semantics configured for the city.
func (a *Agent) EffectiveAssignedInProgressQueryForBeads(beads BeadsConfig) string {
	return a.effectiveAssignedInProgressQuery(beads.UsesBD105ReadySemantics())
}

func (a *Agent) effectiveAssignedInProgressQuery(includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedInProgressWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedInProgressWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
}

// EffectiveAssignedReadyQuery returns the assigned-ready-only command for
// prompt templates that spell out claim-first startup in separate tiers. A
// custom WorkQuery is treated as the caller-owned full discovery contract, so
// split-tier prompts may run that same custom command in each query slot.
func (a *Agent) EffectiveAssignedReadyQuery() string {
	return a.effectiveAssignedReadyQuery(false)
}

// EffectiveAssignedReadyQueryForBeads returns the assigned-ready-only query
// using the bd compatibility semantics configured for the city.
func (a *Agent) EffectiveAssignedReadyQueryForBeads(beads BeadsConfig) string {
	return a.effectiveAssignedReadyQuery(beads.UsesBD105ReadySemantics())
}

func (a *Agent) effectiveAssignedReadyQuery(includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedReadyWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedReadyWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
}

// EffectiveRoutedPoolQuery returns the routed-pool-only command for prompt
// templates that spell out claim-first startup in separate tiers. It is the
// prompt-side counterpart to EffectiveWorkQuery's routed pool tier.
func (a *Agent) EffectiveRoutedPoolQuery() string {
	return a.effectiveRoutedPoolQuery(false)
}

// EffectiveRoutedPoolQueryForBeads returns the routed-pool-only command using
// the bd compatibility semantics configured for the city.
func (a *Agent) EffectiveRoutedPoolQueryForBeads(beads BeadsConfig) string {
	return a.effectiveRoutedPoolQuery(beads.UsesBD105ReadySemantics())
}

func (a *Agent) effectiveRoutedPoolQuery(includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		return routedPoolWorkQueryCommand(includeEphemeralReady, target)
	}
	return routedPoolWorkQueryCommand(includeEphemeralReady, target, legacyTarget)
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
	return "bd update {} --set-metadata " + beadmeta.RoutedToMetadataKey + "=" + a.QualifiedName()
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

// EffectivePoolDemandQuery returns the count-form pool-demand query the
// reconciler runs to detect new unassigned routed work. It is the
// reconciler-side counterpart to EffectiveWorkQuery's Tier 3 (the worker
// claim path): both derive their predicates from the same helpers so
// any future change to the pool-demand shape flows to both paths
// simultaneously.
//
// If ScaleCheck is set (user override), it takes precedence and is
// returned as-is. Otherwise the default count-form is returned.
//
// Assigned in-progress work is resumed from session beads, so it must
// not create additional generic pool demand here.
//
// See engdocs/architecture/dispatch.md "scale_check ↔ work_query
// correspondence" and the protocol-mismatch class regression addressed
// by PR #1516.
func (a *Agent) EffectivePoolDemandQuery() string {
	return a.effectivePoolDemandQuery(false)
}

// EffectivePoolDemandQueryForBeads returns the count-form demand query using
// the bd compatibility semantics configured for the city.
func (a *Agent) EffectivePoolDemandQueryForBeads(beads BeadsConfig) string {
	return a.effectivePoolDemandQuery(beads.UsesBD105ReadySemantics())
}

func (a *Agent) effectivePoolDemandQuery(includeEphemeralReady bool) string {
	if a.ScaleCheck != "" {
		return a.ScaleCheck
	}
	target := a.poolDemandTarget()
	return poolDemandCountShell(target, includeEphemeralReady)
}

// EffectiveScaleCheck returns the scale check command for this agent.
// Pass-through to EffectivePoolDemandQuery for back-compat with code and
// configs that name the predicate "scale_check"; new call sites should
// prefer EffectivePoolDemandQuery to make the dependency on the
// work_query predicate explicit.
func (a *Agent) EffectiveScaleCheck() string {
	return a.EffectivePoolDemandQuery()
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

// UsesCanonicalSingletonPoolIdentity reports whether singleton pool-shaped
// surfaces should use the configured agent identity instead of synthesizing a
// slot identity such as "{name}-1".
func (a *Agent) UsesCanonicalSingletonPoolIdentity() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return false
	}
	maxSessions := a.EffectiveMaxActiveSessions()
	return maxSessions != nil && *maxSessions == 1
}

// SupportsExpandedSessionIdentities reports whether callers should expose or
// discover concrete member identities instead of only the configured identity.
func (a *Agent) SupportsExpandedSessionIdentities() bool {
	if a == nil {
		return false
	}
	if m := a.EffectiveMaxActiveSessions(); m != nil && *m == 0 {
		return false
	}
	return a.SupportsInstanceExpansion() && !a.UsesCanonicalSingletonPoolIdentity()
}

// SupportsInstanceExpansion reports whether the template may have multiple
// simultaneously addressable concrete instances and therefore needs instance
// discovery / synthetic member naming.
//
// max_active_sessions=1 has two distinct flavors:
//
//   - Pool agents (MinActiveSessions or ScaleCheck set) keep pool controller
//     semantics. Non-namepool singleton pools still use the canonical
//     configured identity; see UsesCanonicalSingletonPoolIdentity.
//   - Named-session agents (MaxActiveSessions=1 with a [[named_session]]
//     entry, no Min/ScaleCheck) addressed as just "{name}" — they have a
//     stable canonical identity and a phantom "-1" suffix breaks tools that
//     resolve by qualified name.
//
// We keep instance expansion on for the pool flavor so controller paths still
// run pool reconciliation, and turn it off for the named-session flavor so the
// bare name resolves correctly.
func (a *Agent) SupportsInstanceExpansion() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	m := a.EffectiveMaxActiveSessions()
	if m == nil {
		return true
	}
	if *m < 0 || *m > 1 {
		return true
	}
	// *m == 1: distinguish pool agents (keep numbered instances) from
	// named-session agents (collapse to base identity). Pool agents are
	// identified by an explicit MinActiveSessions or a ScaleCheck override.
	if a.MinActiveSessions != nil || strings.TrimSpace(a.ScaleCheck) != "" {
		return true
	}
	return false
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
	return a.effectiveOnDeath(false)
}

// EffectiveOnDeathForBeads returns the default on_death command using the bd
// compatibility semantics configured for the city.
func (a *Agent) EffectiveOnDeathForBeads(beads BeadsConfig) string {
	return a.effectiveOnDeath(beads.UsesBD105ReadySemantics())
}

func (a *Agent) effectiveOnDeath(includeEphemeralInProgress bool) string {
	if a.OnDeath != "" {
		return a.OnDeath
	}
	route := a.QualifiedName()
	if a.PoolName != "" {
		route = a.PoolName
	}
	_ = includeEphemeralInProgress
	ephemeralRead := bdQueryEphemeralStatusQuietShell("in_progress") + ` | ` +
		`jq -r --arg assignee ` + shellquote.Quote(a.QualifiedName()) + ` '.[] | select((.assignee // "") == $assignee) | [.id, ` + jqMeta(beadmeta.RunTargetMetadataKey) + `, ` + jqMeta(beadmeta.RoutedToMetadataKey) + `] | @tsv' 2>/dev/null; `
	// Reset both assignee and status: clearing assignee alone leaves the bead
	// invisible to every work_query tier (Tier 1 needs assignee match, Tiers
	// 2/3 only match "ready" status). The next worker re-claims via Tier 3.
	// If routed metadata is missing entirely, backfill the canonical
	// gc.run_target route so reopened direct-assigned work does not stay
	// invisible.
	return `{ ` +
		`bd list --assignee=` + a.QualifiedName() +
		` --status=in_progress --json 2>/dev/null | ` +
		`jq -r '.[] | [.id, ` + jqMeta(beadmeta.RunTargetMetadataKey) + `, ` + jqMeta(beadmeta.RoutedToMetadataKey) + `] | @tsv' 2>/dev/null; ` +
		ephemeralRead +
		`} | ` +
		`while IFS="$(printf '\t')" read -r id run_target routed_to; do ` +
		`[ -z "$id" ] && continue; ` +
		`if [ -n "$run_target" ] || [ -n "$routed_to" ]; then ` +
		`bd update "$id" --assignee "" --status open 2>/dev/null; ` +
		`else bd update "$id" --assignee "" --status open --set-metadata ` + shellquote.Quote(beadmeta.RunTargetMetadataKey+"="+route) + ` 2>/dev/null; ` +
		`fi; ` +
		`done`
}

// EffectiveOnBoot returns the on_boot command for this agent.
// If OnBoot is set, returns it. Otherwise returns the default recovery hook
// that unclaims in-progress work routed to this backing config.
func (a *Agent) EffectiveOnBoot() string {
	return a.effectiveOnBoot(false)
}

// EffectiveOnBootForBeads returns the default on_boot command using the bd
// compatibility semantics configured for the city.
func (a *Agent) EffectiveOnBootForBeads(beads BeadsConfig) string {
	return a.effectiveOnBoot(beads.UsesBD105ReadySemantics())
}

func (a *Agent) effectiveOnBoot(includeEphemeralInProgress bool) string {
	if a.OnBoot != "" {
		return a.OnBoot
	}
	template := a.QualifiedName()
	if a.PoolName != "" {
		template = a.PoolName
	}
	_ = includeEphemeralInProgress
	ephemeralRead := bdQueryEphemeralStatusQuietShell("in_progress") + ` | ` +
		`jq -r --arg template "$template" '.[] | select((.assignee // "") == "") | select((` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == $template) or ((` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "") and (` + jqMeta(beadmeta.RunTargetMetadataKey) + ` == $template) and (` + jqMeta(beadmeta.KindMetadataKey) + ` == "` + beadmeta.KindWorkflow + `"))) | .id' 2>/dev/null; `
	return `template=` + shellquote.Quote(template) + `; ` +
		`{ ` +
		`bd list --metadata-field "` + beadmeta.RoutedToMetadataKey + `=$template" --status=in_progress --no-assignee --json 2>/dev/null | ` +
		`jq -r '.[].id' 2>/dev/null; ` +
		`bd list --metadata-field "` + beadmeta.RunTargetMetadataKey + `=$template" --metadata-field "` + beadmeta.KindMetadataKey + `=` + beadmeta.KindWorkflow + `" --status=in_progress --no-assignee --json 2>/dev/null | ` +
		`jq -r '.[] | select(` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "") | .id' 2>/dev/null; ` +
		ephemeralRead +
		`} | awk 'NF && !seen[$0]++' | ` +
		`xargs -rI{} bd update {} --status open 2>/dev/null`
}

// InjectImplicitAgents adds on-demand agents for each explicitly configured
// provider at both city scope and each rig scope. A provider is configured
// only when it appears in cfg.Providers; workspace.provider selects the
// default from that catalog but does not create a catalog entry. Pool min=0,
// max=-1 (unlimited) so they are available as sling targets without an
// explicit [[agent]] entry. Explicit agents always win — if city.toml defines
// [[agent]] name="claude" (or a rig-scoped equivalent), no implicit agent is
// added for that scope.
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
		return
	}

	// Deterministic order: built-in providers first (in canonical order),
	// then any custom providers in sorted order.
	providers := configuredProviderOrder(configured)

	// Implicit agents default to the core pack's pool-worker prompt when
	// the core pack is composed (it resolves from the user-global cache,
	// so the path is absolute). Without core the template stays empty and
	// prompt rendering falls back to the embedded baseline.
	promptTemplate := ""
	if coreDir := cfg.PackDirByName("core"); coreDir != "" {
		promptTemplate = filepath.Join(coreDir, "assets", "prompts", "pool-worker.md")
	}

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
}

// implicitAgentIdentities returns the set of (dir, name) keys for agents that
// InjectImplicitAgents would create given the current config state. This is
// used by compose.go to partition [[patches.agent]] blocks so that patches
// targeting not-yet-injected implicit agents can be deferred until after
// InjectImplicitAgents runs.
func implicitAgentIdentities(cfg *City) map[agentKey]bool {
	configured := configuredProviders(cfg)
	if len(configured) == 0 {
		return nil
	}
	providers := configuredProviderOrder(configured)

	existing := make(map[agentKey]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		existing[agentKey{a.Dir, a.Name}] = true
	}

	result := make(map[agentKey]bool)
	for _, name := range providers {
		if !existing[agentKey{"", name}] {
			result[agentKey{"", name}] = true
		}
	}
	for _, rig := range cfg.Rigs {
		for _, name := range providers {
			if !existing[agentKey{rig.Name, name}] {
				result[agentKey{rig.Name, name}] = true
			}
		}
	}
	return result
}

// ApplyAgentDefaults applies [agent_defaults] values to all agents that
// don't set their own override. Call after InjectImplicitAgents so
// implicit agents are already present. Control-dispatcher agents are
// skipped because they are infrastructure, not work agents. Imported
// pack defaults take precedence over the root city default.
func ApplyAgentDefaults(cfg *City) {
	applyAgentSharedAttachmentDefaults(cfg.Agents, cfg.AgentDefaults)

	provider := cfg.AgentDefaults.Provider
	if provider != "" {
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == ControlDispatcherAgentName {
				continue
			}
			if cfg.Agents[i].Provider == "" {
				if cfg.Agents[i].InheritedProvider != "" {
					cfg.Agents[i].Provider = cfg.Agents[i].InheritedProvider
				} else {
					cfg.Agents[i].Provider = provider
				}
			}
		}
	} else {
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == ControlDispatcherAgentName {
				continue
			}
			if cfg.Agents[i].Provider == "" && cfg.Agents[i].InheritedProvider != "" {
				cfg.Agents[i].Provider = cfg.Agents[i].InheritedProvider
			}
		}
	}

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

	// Upstream axis (Phase C): agents with no explicit upstream inherit the
	// city-level default so model-serving selection can be set once city-wide.
	if upstream := cfg.AgentDefaults.Upstream; upstream != "" {
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == ControlDispatcherAgentName {
				continue
			}
			if cfg.Agents[i].Upstream == "" {
				cfg.Agents[i].Upstream = upstream
			}
		}
	}
}

// DefaultOrderTrackingDeleteAfterClose is the canonical default closed-bead
// TTL for the order_tracking policy. Applied by ApplyBeadPolicyDefaults when
// [beads.policies.order_tracking].delete_after_close is unset in city.toml.
// cmd/gc/order_dispatch.go derives its runtime fallback from this constant so
// both values stay in sync.
const DefaultOrderTrackingDeleteAfterClose = "7d"

// ApplyBeadPolicyDefaults fills in controller-managed defaults for bead
// policies that have sane non-empty defaults. Call after all config
// composition layers have been merged so user-supplied values take
// precedence. Currently:
//   - [beads.policies.order_tracking].delete_after_close defaults to "7d".
func ApplyBeadPolicyDefaults(cfg *City) {
	if cfg == nil {
		return
	}
	if cfg.Beads.Policies == nil {
		cfg.Beads.Policies = make(map[string]BeadPolicyConfig)
	}
	p := cfg.Beads.Policies["order_tracking"]
	if p.DeleteAfterClose == "" {
		p.DeleteAfterClose = DefaultOrderTrackingDeleteAfterClose
		cfg.Beads.Policies["order_tracking"] = p
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
	if src.Provider != "" {
		if prov != nil && dst.Provider != "" && dst.Provider != src.Provider {
			prov.Warnings = append(prov.Warnings, fmt.Sprintf("agent_defaults.provider redefined by %q", label))
		}
		dst.Provider = src.Provider
	}
	if src.Model != "" {
		if prov != nil && dst.Model != "" && dst.Model != src.Model {
			prov.Warnings = append(prov.Warnings, fmt.Sprintf("agent_defaults.model redefined by %q", label))
		}
		dst.Model = src.Model
	}
	if src.Upstream != "" {
		if prov != nil && dst.Upstream != "" && dst.Upstream != src.Upstream {
			prov.Warnings = append(prov.Warnings, fmt.Sprintf("agent_defaults.upstream redefined by %q", label))
		}
		dst.Upstream = src.Upstream
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

// configuredProviders returns the providers that are explicitly configured in
// the provider catalog.
func configuredProviders(cfg *City) map[string]ProviderSpec {
	merged := make(map[string]ProviderSpec, len(cfg.Providers))
	for k, v := range cfg.Providers {
		merged[k] = v
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

// validationAgentKey identifies the canonical route template for an agent.
type validationAgentKey struct{ dir, binding, name string }

// ValidateAgents checks agent configurations for errors. It returns an error
// if any agent is missing required fields, has duplicate canonical identities,
// or has invalid pool bounds. Uniqueness is keyed on (dir, binding, name), so
// the same bare name may exist in the same scope when imports qualify it under
// different bindings.
func ValidateAgents(agents []Agent) error {
	seen := make(map[validationAgentKey]int, len(agents))
	layoutSeen := make(map[agentKey][]int, len(agents))
	for i, a := range agents {
		if a.Name == "" {
			return fmt.Errorf("agent[%d]: name is required", i)
		}
		if !validAgentName.MatchString(a.Name) {
			return fmt.Errorf("agent %q: name must match [a-zA-Z0-9][a-zA-Z0-9_-]* (no spaces, slashes, or dots)", a.Name)
		}
		layoutKey := agentKey{dir: a.Dir, name: a.Name}
		for _, priorIdx := range layoutSeen[layoutKey] {
			if _, _, ok := orderV1V2(agents[priorIdx], a); ok {
				return formatDuplicateAgentError(agents[priorIdx], a)
			}
		}
		layoutSeen[layoutKey] = append(layoutSeen[layoutKey], i)

		key := validationAgentKey{dir: a.Dir, binding: a.BindingName, name: a.Name}
		if priorIdx, dup := seen[key]; dup {
			return formatDuplicateAgentError(agents[priorIdx], a)
		}
		seen[key] = i
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
		// Lifecycle enum.
		switch a.Lifecycle {
		case "", AgentLifecycleOneShot:
			// valid
		default:
			return fmt.Errorf("agent %q: lifecycle must be %q or empty, got %q", a.QualifiedName(), AgentLifecycleOneShot, a.Lifecycle)
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
		// MouseMode enum.
		switch a.MouseMode {
		case "", "on", "off":
			// valid
		default:
			return fmt.Errorf("agent %q: mouse_mode must be \"on\", \"off\", or empty, got %q", a.QualifiedName(), a.MouseMode)
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
// It returns non-fatal warnings (e.g. a named session whose backing template
// did not resolve) alongside any fatal structural error.
func ValidateNamedSessions(cfg *City) (warnings []string, err error) {
	return validateNamedSessions(cfg, true)
}

// validateNamedSessions checks named session declarations for structural
// errors. When requireBackingTemplate is true, a named session whose backing
// agent template does not resolve after pack expansion is reported as a
// non-fatal warning and the session is skipped, rather than failing the whole
// config load. This keeps one broken named session from bricking every command
// (a typo'd template should not stop `gc session attach <other>`). The session
// still errors clearly when a command actually targets it, at materialization
// time. Genuine structural problems (duplicate identity, invalid scope/mode,
// name collisions, capacity overflow) remain fatal.
func validateNamedSessions(cfg *City, requireBackingTemplate bool) (warnings []string, err error) {
	if cfg == nil || len(cfg.NamedSessions) == 0 {
		return nil, nil
	}
	type sessionKey struct{ dir, identity string }
	seen := make(map[sessionKey]bool, len(cfg.NamedSessions))
	reservedAliases := make(map[string]string, len(cfg.NamedSessions))
	reservedSessionNames := make(map[string]string, len(cfg.NamedSessions))
	alwaysByTemplate := make(map[string]int)
	for i := range cfg.NamedSessions {
		s := &cfg.NamedSessions[i]
		if s.Template == "" {
			return nil, fmt.Errorf("named_session[%d]: template is required", i)
		}
		if !validNamedSessionTemplate.MatchString(s.Template) {
			return nil, fmt.Errorf("named_session[%d]: template %q must match [a-zA-Z0-9][a-zA-Z0-9_-]* or binding.agent", i, s.Template)
		}
		if s.Name != "" && !validAgentName.MatchString(s.Name) {
			return nil, fmt.Errorf("named_session[%d]: name %q must match [a-zA-Z0-9][a-zA-Z0-9_-]*", i, s.Name)
		}
		switch s.Scope {
		case "", "city", "rig":
			// valid
		default:
			return nil, fmt.Errorf("named_session %q: scope must be \"city\", \"rig\", or empty, got %q", s.QualifiedName(), s.Scope)
		}
		switch s.ModeOrDefault() {
		case "on_demand", "always":
			// valid
		default:
			return nil, fmt.Errorf("named_session %q: mode must be \"on_demand\", \"always\", or empty, got %q", s.QualifiedName(), s.Mode)
		}
		key := sessionKey{dir: s.Dir, identity: s.IdentityName()}
		if seen[key] {
			return nil, fmt.Errorf("named_session %q: duplicate identity", s.QualifiedName())
		}
		seen[key] = true
		agent := FindAgent(cfg, s.TemplateQualifiedName())
		if agent == nil && requireBackingTemplate {
			// Non-fatal: a named session whose backing template did not
			// resolve is skipped, not a hard error. Skipping here also
			// keeps it out of the name-reservation and always-mode capacity
			// bookkeeping below, since a disabled session holds no slot.
			warnings = append(warnings, disabledNamedSessionWarning(s))
			continue
		}
		identity := s.QualifiedName()
		sessionName := NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
		if other, ok := reservedAliases[sessionName]; ok && other != identity {
			return nil, fmt.Errorf(
				"named_session %q: reserved alias collides with deterministic session_name for %q (%q)",
				identity, other, sessionName,
			)
		}
		if other, ok := reservedSessionNames[identity]; ok && other != identity {
			return nil, fmt.Errorf(
				"named_session %q: reserved alias collides with deterministic session_name for %q (%q)",
				identity, other, identity,
			)
		}
		if other, ok := reservedSessionNames[sessionName]; ok && other != identity {
			return nil, fmt.Errorf(
				"named_session %q: deterministic session_name %q collides with configured named session %q",
				identity, sessionName, other,
			)
		}
		reservedAliases[identity] = identity
		reservedSessionNames[sessionName] = identity
		if s.ModeOrDefault() == "always" && agent != nil {
			alwaysByTemplate[agent.QualifiedName()]++
			if maxActive := agent.EffectiveMaxActiveSessions(); maxActive != nil && *maxActive < alwaysByTemplate[agent.QualifiedName()] {
				return nil, fmt.Errorf(
					"named_session %q: mode %q exceeds max_active_sessions capacity %d on template %q",
					s.QualifiedName(), s.ModeOrDefault(), *maxActive, agent.QualifiedName(),
				)
			}
			policy := ResolveSessionSleepPolicy(cfg, agent)
			if normalized := NormalizeSleepAfterIdle(policy.Value); normalized != "" && normalized != SessionSleepOff {
				return nil, fmt.Errorf(
					"named_session %q: mode %q is incompatible with sleep_after_idle=%q on template %q",
					s.QualifiedName(), s.ModeOrDefault(), normalized, agent.QualifiedName(),
				)
			}
		}
	}
	return warnings, nil
}

// disabledNamedSessionMarker is a stable suffix on the warning emitted when a
// named session is skipped because its backing template did not resolve after
// pack expansion. CLI warning classification keys off this marker, so keep it
// in sync with IsDisabledNamedSessionWarning.
const disabledNamedSessionMarker = "; named session disabled until its template resolves"

// disabledNamedSessionWarning formats the non-fatal warning for a named session
// whose backing template could not be resolved.
func disabledNamedSessionWarning(s *NamedSession) string {
	return fmt.Sprintf(
		"named_session %q: backing template %q not found after pack expansion%s",
		s.QualifiedName(), s.TemplateQualifiedName(), disabledNamedSessionMarker,
	)
}

// IsDisabledNamedSessionWarning reports whether a load warning is the non-fatal
// notice that a named session was skipped because its backing template did not
// resolve. CLI warning filters use this to print the notice and keep it
// non-fatal in strict mode.
func IsDisabledNamedSessionWarning(warning string) bool {
	return strings.HasSuffix(warning, disabledNamedSessionMarker)
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
//
// Reserved coordination-class id-prefixes (gcg/gcm/gcs/gco/gcn) are not rejected
// here. On a default city the relocated SQLite class stores are an identity
// seam — every class store resolves to the work store and the by-id class-prefix
// routing arm never fires — so a work prefix that shadows one is harmless until
// the multi-backend fork makes per-class stores independently routable. Making
// the prefix fatal would break gc start and config reload for an existing city
// that already uses one, so ReservedPrefixWarnings surfaces it as a non-fatal
// advisory instead. Promote it back into a hard error here once per-class
// routing activates.
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
		// orders.RigWildcard is reserved as the [[orders.overrides]]
		// token; a real rig with that name would be silently shadowed.
		if r.Name == orders.RigWildcard {
			return fmt.Errorf("rig[%d]: name %q is reserved as the [[orders.overrides]] wildcard", i, r.Name)
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

// ReservedPrefixWarnings returns advisory warnings for any effective HQ or rig
// work-store prefix that shadows a reserved coordination-class id-prefix
// (gcg/gcm/gcs/gco/gcn). The relocated SQLite class stores that mint these
// prefixes are an identity seam on a default city, so such a prefix is allowed
// today (see ValidateRigs) and only becomes ambiguous once the multi-backend
// fork activates per-class routing. Callers should surface these as non-fatal
// operator warnings so an existing city or rig that already uses one keeps
// starting and reloading while it still has time to rename. The hqPrefix must
// already be site-bound resolved (e.g. via EffectiveHQPrefix).
func ReservedPrefixWarnings(rigs []Rig, hqPrefix string) []string {
	var warnings []string
	if IsReservedClassPrefix(hqPrefix) {
		warnings = append(warnings, fmt.Sprintf("HQ prefix %q is a reserved coordination-class id-prefix (%s); it is allowed today because class-store relocation is inert, but rename it before per-class stores activate or class ids will be ambiguous", strings.ToLower(strings.TrimSpace(hqPrefix)), reservedClassPrefixListText()))
	}
	for _, r := range rigs {
		prefix := strings.ToLower(r.EffectivePrefix())
		if IsReservedClassPrefix(prefix) {
			warnings = append(warnings, fmt.Sprintf("rig %q prefix %q is a reserved coordination-class id-prefix (%s); it is allowed today because class-store relocation is inert, but rename it before per-class stores activate or class ids will be ambiguous", r.Name, prefix, reservedClassPrefixListText()))
		}
	}
	return warnings
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
	case "kiro", "opencode", "mimocode", "groq", "kimi":
		return []string{strings.TrimSpace(provider)}
	default:
		return nil
	}
}

func defaultInstallAgentHooksForProviders(providers []string) []string {
	seen := map[string]bool{}
	var hooks []string
	for _, provider := range providers {
		for _, hook := range defaultInstallAgentHooksForProvider(provider) {
			if seen[hook] {
				continue
			}
			seen[hook] = true
			hooks = append(hooks, hook)
		}
	}
	return hooks
}

func builtinProviderAliases(providers []string) map[string]ProviderSpec {
	out := make(map[string]ProviderSpec)
	for _, provider := range providers {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			continue
		}
		out[provider] = BuiltinProviderAlias(provider)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EmptyCity returns a providerless city scaffold with no managed agents.
func EmptyCity(name string) City {
	return City{Workspace: Workspace{Name: name}}
}

// WizardCity returns a City with the given name, a workspace-level provider
// or start command, and one agent (mayor). This is the config written by
// "gc init" when the interactive wizard runs. If startCommand is set, it
// takes precedence over provider.
func WizardCity(name, provider, startCommand string) City {
	ws := Workspace{Name: name}
	if startCommand != "" {
		ws.StartCommand = startCommand
		return City{
			Workspace: ws,
			Agents: []Agent{
				{Name: "mayor", PromptTemplate: "prompts/mayor.md"},
			},
			NamedSessions: []NamedSession{{Template: "mayor", Mode: "always"}},
		}
	}
	return WizardCityWithProviders(name, provider, []string{provider})
}

// WizardCityWithProviders returns a minimal managed city whose default
// provider is selected from an explicit built-in provider catalog.
func WizardCityWithProviders(name, defaultProvider string, providers []string) City {
	ws := Workspace{Name: name}
	if defaultProvider != "" {
		ws.Provider = defaultProvider
	}
	ws.InstallAgentHooks = defaultInstallAgentHooksForProviders(providers)
	return City{
		Workspace: ws,
		Providers: builtinProviderAliases(providers),
		Agents: []Agent{
			{Name: "mayor", PromptTemplate: "prompts/mayor.md"},
		},
		NamedSessions: []NamedSession{{Template: "mayor", Mode: "always"}},
	}
}

// GastownCity returns a City configured for the gastown orchestration pack.
// Agents come from the public gastown pack; no inline agents are defined. The
// root city pack imports gastown explicitly and sets canonical
// DefaultRigImports so newly added rigs inherit the same pack by default. It
// also sets global fragments and daemon config. If startCommand is set, it
// takes precedence over provider.
func GastownCity(name, provider, startCommand string) City {
	ws := Workspace{
		Name:            name,
		GlobalFragments: []string{"command-glossary", "operational-awareness"},
	}
	if startCommand != "" {
		ws.StartCommand = startCommand
		return gastownCityWithWorkspace(name, ws, nil)
	}
	return GastownCityWithProviders(name, provider, []string{provider})
}

// GascityCityWithProviders returns a minimal managed city that imports the
// public gascity planning/implementation skills pack: a single mayor agent
// plus [imports.gascity] pinned to the registry release. The pack ships
// skills and formulas only (no agents), so the city shape matches the
// minimal template with the pack layered on top.
func GascityCityWithProviders(name, defaultProvider string, providers []string) City {
	city := WizardCityWithProviders(name, defaultProvider, providers)
	city.Imports = map[string]Import{
		"gascity": {
			Source:  PublicGascityPackSource,
			Version: PublicGascityPackVersion,
		},
	}
	return city
}

// GastownCityWithProviders returns a Gas Town city whose default provider is
// selected from an explicit built-in provider catalog.
func GastownCityWithProviders(name, defaultProvider string, providers []string) City {
	ws := Workspace{
		Name:            name,
		GlobalFragments: []string{"command-glossary", "operational-awareness"},
	}
	if defaultProvider != "" {
		ws.Provider = defaultProvider
	}
	ws.InstallAgentHooks = defaultInstallAgentHooksForProviders(providers)
	return gastownCityWithWorkspace(name, ws, builtinProviderAliases(providers))
}

func gastownCityWithWorkspace(_ string, ws Workspace, providers map[string]ProviderSpec) City {
	maxRestarts := 5
	return City{
		Workspace: ws,
		Providers: providers,
		Imports: map[string]Import{
			"gastown": {
				Source:  PublicGastownPackSource,
				Version: PublicGastownPackVersion,
			},
		},
		DefaultRigImports: map[string]Import{
			"gastown": {
				Source:  PublicGastownPackSource,
				Version: PublicGastownPackVersion,
			},
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
	if _, err := validateNamedSessions(cfg, false); err != nil {
		return nil, err
	}
	if err := ValidateGitHubPRMonitors(cfg); err != nil {
		return nil, err
	}
	if err := ValidateDoltConfig(cfg, path); err != nil {
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
	applyDaemonFormulaV2Default(&cfg, md)
	normalizeLegacyOrderOverrideAliases(&cfg)
	NormalizeSessionSleepFields(&cfg)
	// Stamp source=sourceInline on agents declared via [[agent]] in
	// the parsed TOML. These are city.toml inline agents (or test
	// fixtures using Parse directly); pack agents go through a
	// different parser (parsePackConfigWithMetadata) which does not
	// stamp source.
	for i := range cfg.Agents {
		cfg.Agents[i].source = sourceInline
	}
	return &cfg, nil
}

// FormulaV2Enabled reports the effective formula-v2 setting. It is ENABLED by
// default: a nil pointer (the absent/omitted state) means enabled; only an
// explicit formula_v2=false (or the deprecated graph_workflows=false alias)
// disables it. Always read the effective value through this helper, never the
// raw pointer field.
func (d DaemonConfig) FormulaV2Enabled() bool {
	return d.FormulaV2 == nil || *d.FormulaV2
}

func applyDaemonFormulaV2Default(cfg *City, md toml.MetaData) {
	if cfg == nil {
		return
	}
	// An explicit formula_v2 always wins: the decoder already populated the
	// pointer (&true or &false), so leave it untouched.
	if md.IsDefined("daemon", "formula_v2") {
		return
	}
	// Honor the deprecated graph_workflows alias only when formula_v2 is absent.
	if md.IsDefined("daemon", "graph_workflows") {
		v := cfg.Daemon.GraphWorkflows
		cfg.Daemon.FormulaV2 = &v
		return
	}
	// Neither set: leave FormulaV2 nil so it stays default-on (via
	// FormulaV2Enabled) and is omitted from any generated/round-tripped config.
}
