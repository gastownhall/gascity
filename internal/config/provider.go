package config

import "github.com/gastownhall/gascity/internal/shellquote"

// ProviderOption declares a single configurable option for a provider.
// Options are rendered as UI controls in a dashboard's session creation form.
type ProviderOption struct {
	Key     string         `toml:"key"     json:"key"`
	Label   string         `toml:"label"   json:"label"`
	Type    string         `toml:"type"    json:"type"` // "select" only (v1)
	Default string         `toml:"default" json:"default"`
	Choices []OptionChoice `toml:"choices" json:"choices"`
}

// OptionChoice is one allowed value for a "select" option.
type OptionChoice struct {
	Value string `toml:"value"     json:"value"`
	Label string `toml:"label"     json:"label"`
	// FlagArgs are the CLI arguments injected when this choice is selected.
	// json:"-" is intentional: FlagArgs must never appear in the public API DTO
	// (security boundary — prevents clients from seeing internal CLI flags).
	FlagArgs []string `toml:"flag_args" json:"-"`
}

// ProviderSpec defines a named provider's startup parameters.
// Built-in presets are returned by BuiltinProviders(). Users can override
// or define new providers via [providers.xxx] in city.toml.
type ProviderSpec struct {
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
	// EmitsPermissionWarning indicates whether the provider emits permission prompts.
	EmitsPermissionWarning bool `toml:"emits_permission_warning,omitempty"`
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
	SupportsACP bool `toml:"supports_acp,omitempty"`
	// SupportsHooks indicates the provider has an executable hook mechanism
	// (settings.json, plugins, etc.) for lifecycle events.
	SupportsHooks bool `toml:"supports_hooks,omitempty"`
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
	// Supports {{.SessionKey}} template variable. When set, takes precedence
	// over ResumeFlag/ResumeStyle. Example:
	//   "claude --resume {{.SessionKey}} --dangerously-skip-permissions"
	ResumeCommand string `toml:"resume_command,omitempty"`
	// SessionIDFlag is the CLI flag for creating a session with a specific ID.
	// Enables the Generate & Pass strategy for session key management.
	// Example: "--session-id" (claude)
	SessionIDFlag string `toml:"session_id_flag,omitempty"`
	// PermissionModes maps permission mode names to CLI flags.
	// Example: {"unrestricted": "--dangerously-skip-permissions", "plan": "--permission-mode plan"}
	// This is a config-only lookup table consumed by external clients
	// (e.g., Mission Control) to populate permission mode dropdowns.
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
}

// ResolvedProvider is the fully-merged, ready-to-use provider config.
// All fields are populated after resolution (built-in + city override + agent override).
type ResolvedProvider struct {
	Name string
	// Kind is the canonical builtin provider name when this provider derives
	// from a builtin (e.g. "claude" even if Name is "my-fast-claude"). Empty
	// when the provider is fully custom with no builtin base.
	Kind                   string
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
	// EffectiveDefaults is the fully-merged option default map.
	// Computed from: schema Default -> provider OptionDefaults -> agent OptionDefaults.
	// Used by ResolveDefaultArgs() to produce CLI flags and by the API to
	// tell MC what pre-selections to show.
	EffectiveDefaults map[string]string
}

// CommandString returns the full command line: command followed by args.
func (rp *ResolvedProvider) CommandString() string {
	if len(rp.Args) == 0 {
		return rp.Command
	}
	return rp.Command + " " + shellquote.Join(rp.Args)
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

// builtinProviderOrder is the priority order for provider detection and
// wizard display. Claude is first (default), followed by major providers
// in rough popularity order.
var builtinProviderOrder = []string{
	"claude", "codex", "gemini", "cursor", "copilot",
	"amp", "opencode", "auggie", "pi", "omp",
}

// BuiltinProviderOrder returns the provider names in their canonical order.
// Used by the wizard for display and by auto-detection for priority.
func BuiltinProviderOrder() []string {
	out := make([]string, len(builtinProviderOrder))
	copy(out, builtinProviderOrder)
	return out
}

// BuiltinProviders returns the built-in provider presets.
// These are available without any [providers] section in city.toml.
// Lifted from gastown's AgentPresetInfo table — only the runtime-facing
// fields Gas City currently uses are included here.
func BuiltinProviders() map[string]ProviderSpec {
	return map[string]ProviderSpec{
		"claude": {
			DisplayName: "Claude Code",
			Command:     "claude",
			Args:        nil,
			OptionDefaults: map[string]string{
				"permission_mode": "unrestricted",
				"effort":          "max",
			},
			PromptMode:             "arg",
			ReadyDelayMs:           10000,
			ReadyPromptPrefix:      "\u276f ", // ❯
			ProcessNames:           []string{"node", "claude"},
			EmitsPermissionWarning: true,
			SupportsACP:            true,
			SupportsHooks:          true,
			InstructionsFile:       "CLAUDE.md",
			ResumeFlag:             "--resume",
			ResumeStyle:            "flag",
			SessionIDFlag:          "--session-id",
			PrintArgs:              []string{"-p"},
			TitleModel:             "haiku",
			PermissionModes: map[string]string{
				"unrestricted": "--dangerously-skip-permissions",
				"plan":         "--permission-mode plan",
				"auto-edit":    "--permission-mode auto-edit",
				"full-auto":    "--permission-mode full-auto",
			},
			OptionsSchema: []ProviderOption{
				{
					Key: "permission_mode", Label: "Permission Mode", Type: "select",
					Default: "auto-edit",
					Choices: []OptionChoice{
						{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
						{Value: "full-auto", Label: "Full auto", FlagArgs: []string{"--permission-mode", "full-auto"}},
						{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
						{Value: "unrestricted", Label: "Bypass permissions", FlagArgs: []string{"--dangerously-skip-permissions"}},
					},
				},
				{
					Key: "effort", Label: "Effort", Type: "select",
					Default: "",
					Choices: []OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "low", Label: "Low", FlagArgs: []string{"--effort", "low"}},
						{Value: "medium", Label: "Medium", FlagArgs: []string{"--effort", "medium"}},
						{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}},
						{Value: "max", Label: "Max", FlagArgs: []string{"--effort", "max"}},
					},
				},
				{
					Key: "model", Label: "Model", Type: "select",
					Default: "",
					Choices: []OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "opus", Label: "Opus", FlagArgs: []string{"--model", "claude-opus-4-6"}},
						{Value: "sonnet", Label: "Sonnet", FlagArgs: []string{"--model", "claude-sonnet-4-6"}},
						{Value: "haiku", Label: "Haiku", FlagArgs: []string{"--model", "claude-haiku-4-5-20251001"}},
					},
				},
			},
		},
		"codex": {
			DisplayName: "Codex CLI",
			Command:     "codex",
			Args:        nil,
			OptionDefaults: map[string]string{
				"permission_mode": "unrestricted",
				"effort":          "xhigh",
			},
			PromptMode:       "arg",
			ReadyDelayMs:     3000,
			ProcessNames:     []string{"codex"},
			SupportsHooks:    true,
			InstructionsFile: "AGENTS.md",
			PrintArgs:        []string{"exec"},
			TitleModel:       "o4-mini",
			PermissionModes: map[string]string{
				"suggest":      "--ask-for-approval untrusted --sandbox read-only",
				"auto-edit":    "--full-auto",
				"unrestricted": "--dangerously-bypass-approvals-and-sandbox",
			},
			OptionsSchema: []ProviderOption{
				{
					Key: "permission_mode", Label: "Approval Policy", Type: "select",
					Default: "unrestricted",
					Choices: []OptionChoice{
						{Value: "suggest", Label: "Suggest (ask for approval)", FlagArgs: []string{"--ask-for-approval", "untrusted", "--sandbox", "read-only"}},
						{Value: "auto-edit", Label: "Full auto (sandboxed)", FlagArgs: []string{"--full-auto"}},
						{Value: "unrestricted", Label: "Bypass all (no sandbox)", FlagArgs: []string{"--dangerously-bypass-approvals-and-sandbox"}},
					},
				},
				{
					Key: "model", Label: "Model", Type: "select",
					Default: "",
					Choices: []OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "o3", Label: "o3", FlagArgs: []string{"--model", "o3"}},
						{Value: "o4-mini", Label: "o4-mini", FlagArgs: []string{"--model", "o4-mini"}},
					},
				},
				{
					Key: "sandbox", Label: "Sandbox", Type: "select",
					Default: "",
					Choices: []OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "read-only", Label: "Read Only", FlagArgs: []string{"--sandbox", "read-only"}},
						{Value: "network-off", Label: "Network Off", FlagArgs: []string{"--sandbox", "network-off"}},
					},
				},
				{
					Key: "effort", Label: "Effort", Type: "select",
					Default: "",
					Choices: []OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "low", Label: "Low", FlagArgs: []string{"-c", "model_reasoning_effort=low"}},
						{Value: "medium", Label: "Medium", FlagArgs: []string{"-c", "model_reasoning_effort=medium"}},
						{Value: "high", Label: "High", FlagArgs: []string{"-c", "model_reasoning_effort=high"}},
						{Value: "xhigh", Label: "Extra High", FlagArgs: []string{"-c", "model_reasoning_effort=xhigh"}},
					},
				},
			},
		},
		"gemini": {
			DisplayName: "Gemini CLI",
			Command:     "gemini",
			Args:        nil,
			OptionDefaults: map[string]string{
				"permission_mode": "unrestricted",
			},
			PromptMode:        "arg",
			ReadyPromptPrefix: "> ",
			ReadyDelayMs:      5000,
			ProcessNames:      []string{"gemini", "node"},
			SupportsHooks:     true,
			InstructionsFile:  "AGENTS.md",
			PrintArgs:         []string{"-p"},
			TitleModel:        "gemini-2.5-flash",
			PermissionModes: map[string]string{
				"default":      "--approval-mode default",
				"auto-edit":    "--approval-mode auto_edit",
				"plan":         "--approval-mode plan",
				"unrestricted": "--approval-mode yolo",
			},
			OptionsSchema: []ProviderOption{
				{
					Key: "permission_mode", Label: "Approval Mode", Type: "select",
					Default: "unrestricted",
					Choices: []OptionChoice{
						{Value: "default", Label: "Ask before actions", FlagArgs: []string{"--approval-mode", "default"}},
						{Value: "auto-edit", Label: "Auto-approve edits", FlagArgs: []string{"--approval-mode", "auto_edit"}},
						{Value: "plan", Label: "Read-only (plan)", FlagArgs: []string{"--approval-mode", "plan"}},
						{Value: "unrestricted", Label: "YOLO (approve all)", FlagArgs: []string{"--approval-mode", "yolo"}},
					},
				},
				{
					Key: "model", Label: "Model", Type: "select",
					Default: "",
					Choices: []OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "gemini-2.5-pro", Label: "Gemini 2.5 Pro", FlagArgs: []string{"--model", "gemini-2.5-pro"}},
						{Value: "gemini-2.5-flash", Label: "Gemini 2.5 Flash", FlagArgs: []string{"--model", "gemini-2.5-flash"}},
					},
				},
			},
		},
		"cursor": {
			DisplayName:      "Cursor Agent",
			Command:          "cursor-agent",
			Args:             []string{"-f"},
			PromptMode:       "arg",
			ProcessNames:     []string{"cursor-agent"},
			SupportsHooks:    true,
			InstructionsFile: "AGENTS.md",
		},
		"copilot": {
			DisplayName:       "GitHub Copilot",
			Command:           "copilot",
			Args:              []string{"--yolo"},
			PromptMode:        "arg",
			ReadyPromptPrefix: "\u276f ", // ❯
			ReadyDelayMs:      5000,
			ProcessNames:      []string{"copilot"},
			SupportsHooks:     true,
			InstructionsFile:  "AGENTS.md",
		},
		"amp": {
			DisplayName:      "Sourcegraph AMP",
			Command:          "amp",
			Args:             []string{"--dangerously-allow-all", "--no-ide"},
			PromptMode:       "arg",
			ProcessNames:     []string{"amp"},
			InstructionsFile: "AGENTS.md",
		},
		"opencode": {
			DisplayName:      "OpenCode",
			Command:          "opencode",
			Args:             []string{},
			PromptMode:       "none",
			ReadyDelayMs:     8000,
			ProcessNames:     []string{"opencode", "node", "bun"},
			Env:              map[string]string{"OPENCODE_PERMISSION": `{"*":"allow"}`},
			SupportsACP:      true,
			SupportsHooks:    true,
			InstructionsFile: "AGENTS.md",
		},
		"auggie": {
			DisplayName:      "Auggie CLI",
			Command:          "auggie",
			Args:             []string{"--allow-indexing"},
			PromptMode:       "arg",
			ProcessNames:     []string{"auggie"},
			InstructionsFile: "AGENTS.md",
		},
		"pi": {
			DisplayName:      "Pi Coding Agent",
			Command:          "pi",
			Args:             []string{"-e", ".pi/extensions/gc-hooks.js"},
			PromptMode:       "arg",
			ReadyDelayMs:     8000,
			ProcessNames:     []string{"pi", "node", "bun"},
			SupportsHooks:    true,
			InstructionsFile: "AGENTS.md",
		},
		"omp": {
			DisplayName:      "Oh My Pi (OMP)",
			Command:          "omp",
			Args:             []string{"--hook", ".omp/hooks/gc-hook.ts"},
			PromptMode:       "arg",
			ProcessNames:     []string{"omp", "node", "bun"},
			SupportsHooks:    true,
			InstructionsFile: "AGENTS.md",
		},
	}
}
