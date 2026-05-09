package config

import (
	"strings"

	"github.com/gastownhall/gascity/internal/shellquote"
	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// ProviderOption declares a single configurable option for a provider.
// Options are rendered as UI controls in a dashboard's session creation form.
type ProviderOption struct {
	Key     string         `toml:"key"     json:"key"`
	Label   string         `toml:"label"   json:"label"`
	Type    string         `toml:"type"    json:"type"` // "select" only (v1)
	Default string         `toml:"default" json:"default"`
	Choices []OptionChoice `toml:"choices" json:"choices"`
	// Omit is the removal sentinel for options_schema_merge = "by_key".
	// When set on a child layer's entry, the matching Key inherited from
	// a parent layer is pruned from the resolved schema.
	Omit bool `toml:"omit,omitempty" json:"omit,omitempty"`
}

// OptionChoice is one allowed value for a "select" option.
type OptionChoice struct {
	Value string `toml:"value"     json:"value"`
	Label string `toml:"label"     json:"label"`
	// FlagArgs are the CLI arguments injected when this choice is selected.
	// json:"-" is intentional: FlagArgs must never appear in the public API DTO
	// (security boundary — prevents clients from seeing internal CLI flags).
	FlagArgs []string `toml:"flag_args" json:"-"`
	// FlagAliases are equivalent CLI argument sequences stripped from legacy
	// provider args. Like FlagArgs, they stay server-side only.
	FlagAliases [][]string `toml:"flag_aliases,omitempty" json:"-"`
}

// ProviderSpec defines a named provider's startup parameters.
// Built-in presets are returned by BuiltinProviders(). Users can override
// or define new providers via [providers.xxx] in city.toml.
type ProviderSpec struct {
	// Base names the parent provider this spec inherits from. Supported
	// forms:
	//   "<name>"          - custom first (self-excluded), then built-in
	//   "builtin:<name>"  - force built-in lookup
	//   "provider:<name>" - force custom lookup
	//   ""                - explicit standalone opt-out
	//   nil               - field absent; no explicit declaration
	Base *string `toml:"base,omitempty"`
	// ArgsAppend accumulates extra args after each layer's Args replacement.
	ArgsAppend []string `toml:"args_append,omitempty"`
	// OptionsSchemaMerge controls OptionsSchema merge mode across the
	// chain: "replace" (default) or "by_key".
	OptionsSchemaMerge string `toml:"options_schema_merge,omitempty" jsonschema:"enum=replace,enum=by_key"`
	// DisplayName is the human-readable name shown in UI and logs.
	DisplayName string `toml:"display_name,omitempty"`
	// Command is the executable to run for this provider.
	Command string `toml:"command,omitempty"`
	// Args are default command-line arguments passed to the provider.
	Args []string `toml:"args,omitempty"`
	// PromptMode controls how prompts are delivered: "arg", "flag", or "none".
	PromptMode string `toml:"prompt_mode,omitempty" jsonschema:"enum=arg,enum=flag,enum=none,default=arg"`
	// PromptFlag is the CLI flag used when prompt_mode is "flag" (e.g. "--prompt").
	PromptFlag string `toml:"prompt_flag,omitempty"`
	// ReadyDelayMs is milliseconds to wait after launch before the provider is considered ready.
	ReadyDelayMs int `toml:"ready_delay_ms,omitempty" jsonschema:"minimum=0"`
	// ReadyPromptPrefix is the string prefix that indicates the provider is ready for input.
	ReadyPromptPrefix string `toml:"ready_prompt_prefix,omitempty"`
	// ProcessNames lists process names to look for when checking if the provider is running.
	ProcessNames []string `toml:"process_names,omitempty"`
	// EmitsPermissionWarning is tri-state: nil = inherit, &true = enable,
	// &false = explicit disable.
	EmitsPermissionWarning *bool `toml:"emits_permission_warning,omitempty"`
	// Env sets additional environment variables for the provider process.
	Env map[string]string `toml:"env,omitempty"`
	// PathCheck overrides the binary name used for PATH detection.
	// When set, lookupProvider and detectProviderName use this instead
	// of Command for exec.LookPath checks. Useful when Command is a
	// shell wrapper (e.g. sh -c '...') but we need to verify the real
	// binary is installed.
	PathCheck string `toml:"path_check,omitempty"`
	// SupportsACP indicates the binary speaks the Agent Client Protocol
	// (JSON-RPC 2.0 over stdio). When an agent sets session = "acp",
	// its resolved provider must have SupportsACP = true.
	SupportsACP *bool `toml:"supports_acp,omitempty"`
	// SupportsHooks indicates the provider has an executable hook mechanism
	// (settings.json, plugins, etc.) for lifecycle events.
	SupportsHooks *bool `toml:"supports_hooks,omitempty"`
	// InstructionsFile is the filename the provider reads for project instructions
	// (e.g., "CLAUDE.md", "AGENTS.md"). Empty defaults to "AGENTS.md".
	InstructionsFile string `toml:"instructions_file,omitempty"`
	// ResumeFlag is the CLI flag for resuming a session by ID.
	// Empty means the provider does not support resume.
	// Examples: "--resume" (claude), "resume" (codex)
	ResumeFlag string `toml:"resume_flag,omitempty"`
	// ResumeStyle controls how ResumeFlag is applied:
	//   "flag"       → command --resume <key>              (default)
	//   "subcommand" → command resume <key>
	ResumeStyle string `toml:"resume_style,omitempty"`
	// ResumeCommand is the full shell command to run when resuming a session.
	// Supports only the {{.SessionKey}} template variable. When set, takes precedence
	// over ResumeFlag/ResumeStyle. When schema-managed defaults are inserted, the
	// resolver tokenizes and re-emits the command; for subcommand-style resume it
	// inserts after the ResumeFlag token that precedes {{.SessionKey}}. Example:
	//   "claude --resume {{.SessionKey}} --dangerously-skip-permissions"
	// Schema-managed defaults missing from a subcommand-style resume command
	// are inserted before {{.SessionKey}} during provider resolution.
	ResumeCommand string `toml:"resume_command,omitempty"`
	// SessionIDFlag is the CLI flag for creating a session with a specific ID.
	// Enables the Generate & Pass strategy for session key management.
	// Example: "--session-id" (claude)
	SessionIDFlag string `toml:"session_id_flag,omitempty"`
	// PermissionModes maps permission mode names to CLI flags.
	// Example: {"unrestricted": "--dangerously-skip-permissions", "plan": "--permission-mode plan"}
	// This is a config-only lookup table consumed by external clients
	// (e.g., real-world app) to populate permission mode dropdowns.
	// Launch-time flag substitution is planned for a follow-up PR —
	// currently no runtime code reads this field.
	PermissionModes map[string]string `toml:"permission_modes,omitempty"`
	// OptionDefaults overrides the Default value in OptionsSchema entries
	// without redefining the schema itself. Keys are option keys (e.g.,
	// "permission_mode"), values are choice values (e.g., "unrestricted").
	// city.toml users set this to customize provider behavior without
	// touching Args or OptionsSchema.
	OptionDefaults map[string]string `toml:"option_defaults,omitempty"`
	// OptionsSchema declares the configurable options this provider supports.
	// Each option maps to CLI args via its Choices[].FlagArgs field.
	// Serialized via a dedicated DTO (not directly to JSON) so FlagArgs stays server-side.
	OptionsSchema []ProviderOption `toml:"options_schema,omitempty" json:"-"`
	// PrintArgs are CLI arguments that enable one-shot non-interactive mode.
	// The provider prints its response to stdout and exits. When empty, the
	// provider does not support one-shot invocation.
	// Examples: ["-p"] (claude, gemini), ["exec"] (codex)
	PrintArgs []string `toml:"print_args,omitempty"`
	// TitleModel is the OptionsSchema model key used for title generation.
	// Resolved via the "model" option in OptionsSchema to get FlagArgs.
	// Defaults to the cheapest/fastest model for each provider.
	// Examples: "haiku" (claude), "o4-mini" (codex), "gemini-2.5-flash" (gemini)
	TitleModel string `toml:"title_model,omitempty"`
	// ACPCommand overrides Command when the session transport is ACP.
	// When empty, Command is used for both tmux and ACP transports.
	ACPCommand string `toml:"acp_command,omitempty"`
	// ACPArgs overrides Args when the session transport is ACP.
	// When nil, Args is used for both tmux and ACP transports.
	ACPArgs []string `toml:"acp_args,omitempty"`
}

// Reserved prefixes for the Base field.
const (
	BasePrefixBuiltin  = "builtin:"
	BasePrefixProvider = "provider:"
)

// RawProviderSpec marks a ProviderSpec as unresolved.
type RawProviderSpec = ProviderSpec

// HopIdentity identifies a single hop in a resolved provider chain.
type HopIdentity struct {
	Kind string // "builtin" | "custom"
	Name string // canonical name (without prefix)
}

// ChainEntry annotates one hop of the resolved chain.
type ChainEntry struct {
	HopIdentity
	BaseTagIsExplicit bool
}

// ResolvedProvider is the fully-merged, ready-to-use provider config.
// All fields are populated after resolution (built-in + city override + agent override).
type ResolvedProvider struct {
	Name string
	// Kind is the canonical builtin provider name when this provider derives
	// from a builtin (e.g. "claude" even if Name is "my-fast-claude"). Empty
	// when the provider is fully custom with no builtin base.
	//
	// Deprecated: use BuiltinAncestor. Kept during transition.
	Kind string
	// BuiltinAncestor is the nearest built-in provider in the resolved
	// chain, derived from hop identity during the chain walk.
	BuiltinAncestor string
	// Chain records the resolved ancestry from leaf (index 0) to root.
	Chain []HopIdentity
	// Provenance records per-field and per-map-key layer attribution.
	Provenance             ProviderProvenance
	Command                string
	Args                   []string
	PromptMode             string
	PromptFlag             string
	ReadyDelayMs           int
	ReadyPromptPrefix      string
	ProcessNames           []string
	EmitsPermissionWarning bool
	Env                    map[string]string
	SupportsACP            bool
	SupportsHooks          bool
	InstructionsFile       string
	ResumeFlag             string
	ResumeStyle            string
	ResumeCommand          string
	SessionIDFlag          string
	PermissionModes        map[string]string
	OptionsSchema          []ProviderOption
	PrintArgs              []string
	TitleModel             string
	ACPCommand             string
	ACPArgs                []string
	// EffectiveDefaults is the fully-merged option default map.
	// Computed from: schema Default -> provider OptionDefaults -> agent OptionDefaults.
	// Used by ResolveDefaultArgs() to produce CLI flags and by the API to
	// tell real-world apps what pre-selections to show.
	EffectiveDefaults map[string]string
}

const (
	// SessionTransportACP creates sessions through the Agent Client Protocol.
	SessionTransportACP = "acp"
	// SessionTransportTmux creates sessions through the tmux-backed CLI path.
	SessionTransportTmux = "tmux"
)

// IsValidSessionTransport reports whether transport is a recognized explicit
// session transport. The empty string is valid and means provider default.
func IsValidSessionTransport(transport string) bool {
	switch strings.TrimSpace(transport) {
	case "", SessionTransportACP, SessionTransportTmux:
		return true
	default:
		return false
	}
}

// CommandString returns the full command line: command followed by args.
func (rp *ResolvedProvider) CommandString() string {
	if len(rp.Args) == 0 {
		return rp.Command
	}
	return rp.Command + " " + shellquote.Join(rp.Args)
}

// ACPCommandString returns the command line for ACP transport sessions.
// Each field falls back independently: ACPCommand defaults to Command,
// and ACPArgs defaults to Args, so partial overrides are supported.
func (rp *ResolvedProvider) ACPCommandString() string {
	cmd := rp.ACPCommand
	args := rp.ACPArgs
	if cmd == "" {
		cmd = rp.Command
	}
	if args == nil {
		args = rp.Args
	}
	if len(args) == 0 {
		return cmd
	}
	return cmd + " " + shellquote.Join(args)
}

// DefaultSessionTransport returns the transport used for provider-backed
// sessions when no template-level session override exists.
func (rp *ResolvedProvider) DefaultSessionTransport() string {
	if rp == nil || !rp.SupportsACP {
		return ""
	}
	family := strings.TrimSpace(rp.BuiltinAncestor)
	if family == "" {
		family = strings.TrimSpace(rp.Kind)
	}
	if family == "" {
		family = strings.TrimSpace(rp.Name)
	}
	if family == "opencode" {
		return SessionTransportACP
	}
	return ""
}

// ProviderSessionCreateTransport returns the transport to use when creating a
// provider-backed session without any template-level session override.
func (rp *ResolvedProvider) ProviderSessionCreateTransport() string {
	if rp == nil || !rp.SupportsACP {
		return ""
	}
	if transport := rp.DefaultSessionTransport(); transport != "" {
		return transport
	}
	family := strings.TrimSpace(rp.BuiltinAncestor)
	if family == "" {
		family = strings.TrimSpace(rp.Kind)
	}
	if family == "" {
		family = strings.TrimSpace(rp.Name)
	}
	if family == "kiro" {
		// Kiro supports explicit ACP sessions, but its chat transport carries
		// the non-interactive tool trust contract required by coding agents.
		return ""
	}
	if strings.TrimSpace(rp.ACPCommand) != "" || rp.ACPArgs != nil {
		return SessionTransportACP
	}
	return ""
}

// ResolveSessionCreateTransport returns the transport to use when creating a
// fresh session from an agent/template configuration.
func ResolveSessionCreateTransport(agentSession string, resolved *ResolvedProvider) string {
	agentSession = strings.TrimSpace(agentSession)
	switch agentSession {
	case SessionTransportACP:
		return SessionTransportACP
	case SessionTransportTmux:
		return SessionTransportTmux
	case "":
		if resolved == nil {
			return ""
		}
		return strings.TrimSpace(resolved.ProviderSessionCreateTransport())
	default:
		return agentSession
	}
}

// TitleModelFlagArgs resolves the TitleModel key against the "model"
// OptionsSchema entry. Returns the CLI flag args for the title model,
// or nil if TitleModel is empty or not found in the schema.
func (rp *ResolvedProvider) TitleModelFlagArgs() []string {
	if rp.TitleModel == "" {
		return nil
	}
	for _, opt := range rp.OptionsSchema {
		if opt.Key != "model" {
			continue
		}
		for _, c := range opt.Choices {
			if c.Value == rp.TitleModel {
				return c.FlagArgs
			}
		}
	}
	return nil
}

// ResolveDefaultArgs produces CLI flag args from EffectiveDefaults.
// For each schema option with an effective default, the corresponding
// FlagArgs are emitted. Options with no effective default (or whose
// default is "") are skipped.
// Args are emitted in schema declaration order for deterministic output.
func (rp *ResolvedProvider) ResolveDefaultArgs() []string {
	var args []string
	for _, opt := range rp.OptionsSchema {
		value := rp.EffectiveDefaults[opt.Key]
		if value == "" {
			continue
		}
		choice := findChoice(opt.Choices, value)
		if choice != nil {
			args = append(args, choice.FlagArgs...)
		}
	}
	return args
}

// pathCheckBinary returns the binary name to use for PATH detection.
// If PathCheck is set, it is used; otherwise Command is used directly.
func (ps *ProviderSpec) pathCheckBinary() string {
	if ps.PathCheck != "" {
		return ps.PathCheck
	}
	return ps.Command
}

// boolPtr returns a pointer to the given bool for tri-state capability fields.
func boolPtr(b bool) *bool { return &b }

// derefBool safely dereferences a *bool, returning false for nil.
func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// BuiltinProviderOrder returns the provider names in their canonical order.
// Used by the wizard for display and by auto-detection for priority.
func BuiltinProviderOrder() []string {
	return workerbuiltin.BuiltinProviderOrder()
}

// BuiltinProviders returns the built-in provider presets.
// These are available without any [providers] section in city.toml.
func BuiltinProviders() map[string]ProviderSpec {
	specs := workerbuiltin.BuiltinProviders()
	out := make(map[string]ProviderSpec, len(specs))
	for name, spec := range specs {
		out[name] = providerSpecFromWorker(spec)
	}
	return out
}

func providerSpecFromWorker(spec workerbuiltin.BuiltinProviderSpec) ProviderSpec {
	return ProviderSpec{
		Base:                   nil,
		ArgsAppend:             nil,
		OptionsSchemaMerge:     "",
		DisplayName:            spec.DisplayName,
		Command:                spec.Command,
		Args:                   cloneStrings(spec.Args),
		PromptMode:             spec.PromptMode,
		PromptFlag:             spec.PromptFlag,
		ReadyDelayMs:           spec.ReadyDelayMs,
		ReadyPromptPrefix:      spec.ReadyPromptPrefix,
		ProcessNames:           cloneStrings(spec.ProcessNames),
		EmitsPermissionWarning: boolPtr(spec.EmitsPermissionWarning),
		Env:                    cloneStringMap(spec.Env),
		PathCheck:              spec.PathCheck,
		SupportsACP:            boolPtr(spec.SupportsACP),
		SupportsHooks:          boolPtr(spec.SupportsHooks),
		InstructionsFile:       spec.InstructionsFile,
		ResumeFlag:             spec.ResumeFlag,
		ResumeStyle:            spec.ResumeStyle,
		ResumeCommand:          spec.ResumeCommand,
		SessionIDFlag:          spec.SessionIDFlag,
		PermissionModes:        cloneStringMap(spec.PermissionModes),
		OptionDefaults:         cloneStringMap(spec.OptionDefaults),
		OptionsSchema:          providerOptionsFromWorker(spec.OptionsSchema),
		PrintArgs:              cloneStrings(spec.PrintArgs),
		TitleModel:             spec.TitleModel,
		ACPCommand:             spec.ACPCommand,
		ACPArgs:                cloneStrings(spec.ACPArgs),
	}
}

func providerOptionsFromWorker(options []workerbuiltin.BuiltinProviderOption) []ProviderOption {
	if options == nil {
		return nil
	}
	out := make([]ProviderOption, len(options))
	for i, option := range options {
		out[i] = ProviderOption{
			Key:     option.Key,
			Label:   option.Label,
			Type:    option.Type,
			Default: option.Default,
			Choices: providerChoicesFromWorker(option.Choices),
		}
	}
	return out
}

func providerChoicesFromWorker(choices []workerbuiltin.BuiltinOptionChoice) []OptionChoice {
	if choices == nil {
		return nil
	}
	out := make([]OptionChoice, len(choices))
	for i, choice := range choices {
		out[i] = OptionChoice{
			Value:       choice.Value,
			Label:       choice.Label,
			FlagArgs:    cloneStrings(choice.FlagArgs),
			FlagAliases: cloneStringSlices(choice.FlagAliases),
		}
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneStringSlices(values [][]string) [][]string {
	if values == nil {
		return nil
	}
	out := make([][]string, len(values))
	for i := range values {
		out[i] = cloneStrings(values[i])
	}
	return out
}
