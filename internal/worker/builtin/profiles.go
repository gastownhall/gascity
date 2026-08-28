// Package builtin defines the canonical builtin worker provider catalog.
package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// BuiltinProviderOption declares one configurable option for a builtin worker.
//
//nolint:revive // Mirrors the config boundary naming intentionally.
type BuiltinProviderOption struct {
	Key     string
	Label   string
	Type    string
	Default string
	Choices []BuiltinOptionChoice
	// FlagTemplate makes the option open: a pinned value outside Choices is
	// honored by substituting it for optionValuePlaceholder. Nil means closed.
	// See config.ProviderOption.FlagTemplate for why model options are open.
	FlagTemplate []string
}

// BuiltinOptionChoice is one allowed value for a builtin provider option.
//
//nolint:revive // Mirrors the config boundary naming intentionally.
type BuiltinOptionChoice struct {
	Value       string
	Label       string
	FlagArgs    []string
	FlagAliases [][]string
}

// BuiltinProviderSpec is the canonical builtin worker materialization source.
// config.ProviderSpec is derived from this in Phase 4+.
//
//nolint:revive // Mirrors the config boundary naming intentionally.
type BuiltinProviderSpec struct {
	DisplayName            string
	Command                string
	Args                   []string
	PromptMode             string
	PromptFlag             string
	ReadyDelayMs           int
	ReadyPromptPrefix      string
	ProcessNames           []string
	EmitsPermissionWarning bool
	AcceptStartupDialogs   *bool
	Env                    map[string]string
	PathCheck              string
	SupportsACP            bool
	SupportsHooks          bool
	InstructionsFile       string
	ResumeFlag             string
	ResumeStyle            string
	ResumeCommand          string
	SessionIDFlag          string
	// ForkFlag is the CLI flag that forks a resumed conversation into a new
	// branch. Combined with ResumeFlag + SessionIDFlag it yields the fork-launch
	// form (resume a parent brain, fork off it, bind gc's own session id). Empty
	// for providers with no fork verb (currently every provider except claude).
	ForkFlag        string
	PermissionModes map[string]string
	OptionDefaults  map[string]string
	OptionsSchema   []BuiltinProviderOption
	PrintArgs       []string
	TitleModel      string
	ACPCommand      string
	ACPArgs         []string
	// Upstream serving-env binding (Phase C — the Upstream axis): the env-var
	// NAMES this harness reads for the model-serving base URL and credential, so
	// an abstract [upstreams.<name>] renders onto the right names for this CLI.
	// Empty = no built-in binding (the operator declares one, or uses the raw env
	// escape hatch). Kept as plain strings (this package cannot import config).
	UpstreamBaseURLEnv   string
	UpstreamAPIKeyEnv    string
	UpstreamAuthTokenEnv string
}

func boolPtr(b bool) *bool { return &b }

// optionValuePlaceholder mirrors config.OptionValuePlaceholder. This package
// sits below config and cannot import it, so the token is duplicated here and
// pinned by config.TestBuiltinTemplatesUseConfigPlaceholder.
const optionValuePlaceholder = "{value}"

// effortTiers is the canonical, provider-independent effort vocabulary. A
// provider declares the subset its CLI accepts; every provider draws from this
// list so "high" means the same thing everywhere and a tier is never invented
// per-provider. Enforced by TestEffortChoicesUseCanonicalTiers.
var effortTiers = []string{"low", "medium", "high", "xhigh", "max"}

var effortTierLabels = map[string]string{
	"low":    "Low",
	"medium": "Medium",
	"high":   "High",
	"xhigh":  "Extra High",
	"max":    "Max",
}

// effortOption builds the effort schema option for a provider whose CLI takes
// the effort tier as the final token of flagPrefix (e.g. {"--effort"}), or
// substitutes it into an assignment token containing the placeholder (codex's
// {"-c", "model_reasoning_effort={value}"}).
//
// The option is always open: no provider CLI publishes a closed tier set, so a
// tier this catalog has not caught up to is passed through to the CLI to accept
// or reject, rather than silently dropped (ga-fyh).
func effortOption(flagTemplate []string, tiers ...string) BuiltinProviderOption {
	choices := []BuiltinOptionChoice{{Value: "", Label: "Default"}}
	for _, tier := range tiers {
		choices = append(choices, BuiltinOptionChoice{
			Value:    tier,
			Label:    effortTierLabels[tier],
			FlagArgs: renderTemplate(flagTemplate, tier),
		})
	}
	return BuiltinProviderOption{
		Key:          "effort",
		Label:        "Effort",
		Type:         "select",
		Choices:      choices,
		FlagTemplate: cloneStrings(flagTemplate),
	}
}

// codexEffortOption is effortOption for codex, whose CLI takes the tier as a
// `-c model_reasoning_effort=<tier>` config assignment rather than a flag.
// The quoted alias form is what older rendered commands used, so it stays a
// recognized alias for stripping.
func codexEffortOption() BuiltinProviderOption {
	opt := effortOption([]string{"-c", "model_reasoning_effort=" + optionValuePlaceholder}, effortTiers...)
	for i, choice := range opt.Choices {
		if choice.Value == "" {
			continue
		}
		opt.Choices[i].FlagAliases = [][]string{{"-c", fmt.Sprintf("model_reasoning_effort=%q", choice.Value)}}
	}
	return opt
}

// modelOption builds an open model option: choices are the curated suggestion
// list shown in pickers, and any other id is still honored by rendering the
// template. Model ids move faster than this catalog ever has, which is the
// whole ga-fyh / ra-jbbv0 failure mode. Every provider CLI in the catalog
// spells the flag "--model <id>"; the short "-m" form, where a CLI has one,
// stays on the individual choices as a stripping alias.
func modelOption(choices ...BuiltinOptionChoice) BuiltinProviderOption {
	return BuiltinProviderOption{
		Key:          "model",
		Label:        "Model",
		Type:         "select",
		Choices:      append([]BuiltinOptionChoice{{Value: "", Label: "Default"}}, choices...),
		FlagTemplate: []string{"--model", optionValuePlaceholder},
	}
}

// modelChoice is a curated model suggestion whose flag is the plain
// "--model <id>" form, with the "-m <id>" alias.
func modelChoice(value, label string) BuiltinOptionChoice {
	return BuiltinOptionChoice{
		Value:       value,
		Label:       label,
		FlagArgs:    []string{"--model", value},
		FlagAliases: [][]string{{"-m", value}},
	}
}

// modelAlias is a curated short name that expands to a different canonical id
// (e.g. "sonnet" -> "claude-sonnet-5"). The expansion is why choices win over
// the template: the template would emit the short name verbatim.
func modelAlias(value, label, modelID string) BuiltinOptionChoice {
	return BuiltinOptionChoice{
		Value:       value,
		Label:       label,
		FlagArgs:    []string{"--model", modelID},
		FlagAliases: [][]string{{"-m", modelID}},
	}
}

// modelChoiceNoAlias is modelChoice for CLIs that define no "-m" short flag.
func modelChoiceNoAlias(value, label string) BuiltinOptionChoice {
	return BuiltinOptionChoice{
		Value:    value,
		Label:    label,
		FlagArgs: []string{"--model", value},
	}
}

func renderTemplate(template []string, value string) []string {
	out := make([]string, len(template))
	for i, tok := range template {
		out[i] = strings.ReplaceAll(tok, optionValuePlaceholder, value)
	}
	return out
}

// ProfileIdentity captures the explicit production identity for a canonical
// worker profile.
type ProfileIdentity struct {
	Profile                  string
	ProviderFamily           string
	TransportClass           string
	BehaviorClaimsVersion    string
	TranscriptAdapterVersion string
	CompatibilityVersion     string
	CertificationFingerprint string
}

const (
	canonicalBehaviorClaimsVersion    = "behavior-v1"
	canonicalTranscriptAdapterVersion = "sessionlog-v1"
)

var builtinProviderOrder = []string{
	"claude", "codex", "gemini", "grok", "kimi", "kiro", "cursor", "copilot",
	"amp", "opencode", "mimocode", "zcode", "groq", "cerebras", "auggie", "pi",
	"omp", "antigravity",
}

var builtinProviderSpecs = map[string]BuiltinProviderSpec{
	"claude": {
		DisplayName: "Claude Code",
		Command:     "claude",
		// Anthropic serving-env binding (Claude Code reads these for a custom
		// endpoint + credential).
		UpstreamBaseURLEnv:   "ANTHROPIC_BASE_URL",
		UpstreamAPIKeyEnv:    "ANTHROPIC_API_KEY",
		UpstreamAuthTokenEnv: "ANTHROPIC_AUTH_TOKEN",
		OptionDefaults: map[string]string{
			"permission_mode": "unrestricted",
			"effort":          "max",
		},
		PromptMode:             "arg",
		ReadyDelayMs:           10000,
		ReadyPromptPrefix:      "\u276f ",
		ProcessNames:           []string{"node", "claude"},
		EmitsPermissionWarning: true,
		SupportsACP:            true,
		SupportsHooks:          true,
		InstructionsFile:       "CLAUDE.md",
		ResumeFlag:             "--resume",
		ResumeStyle:            "flag",
		// Claude Code accepts a caller-supplied UUID at fresh start
		// (`claude --session-id <uuid>`), which is what lets gc generate the
		// durable session_key up front and hand it back as `--resume <uuid>`
		// on restart. Without this the key is never generated and a restart
		// silently launches a brand-new conversation.
		SessionIDFlag: "--session-id",
		ForkFlag:      "--fork-session",
		PrintArgs:     []string{"-p"},
		TitleModel:    "haiku",
		// Config-facing names map to current CLI values: Claude Code rejects the
		// legacy "auto-edit"/"full-auto" it used to accept (GH#4602).
		PermissionModes: map[string]string{
			"unrestricted": "--dangerously-skip-permissions",
			"plan":         "--permission-mode plan",
			"auto-edit":    "--permission-mode acceptEdits",
			"full-auto":    "--permission-mode dontAsk",
		},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:     "permission_mode",
				Label:   "Permission Mode",
				Type:    "select",
				Default: "auto-edit",
				Choices: []BuiltinOptionChoice{
					{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "acceptEdits"}},
					{Value: "full-auto", Label: "Full auto", FlagArgs: []string{"--permission-mode", "dontAsk"}},
					{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
					{Value: "unrestricted", Label: "Bypass permissions", FlagArgs: []string{"--dangerously-skip-permissions"}},
				},
			},
			effortOption([]string{"--effort", optionValuePlaceholder}, effortTiers...),
			// Open: an id outside this curated list is honored verbatim rather
			// than dropped. Before that, a canonical "claude-*" pin was not in
			// the enum at all — the launch path found no FlagArgs and emitted
			// NO --model, while named-session resolution hard-errored on the
			// same value. A whole city ran unpinned for hours while four
			// agents were unwakeable (ra-jbbv0).
			modelOption(
				modelAlias("fable-5", "Fable 5", "claude-fable-5"),
				modelAlias("opus", "Opus", "claude-opus-4-8"),
				modelAlias("opus-5", "Opus 5", "claude-opus-5"),
				modelAlias("opus-4-7", "Opus 4.7", "claude-opus-4-7"),
				modelAlias("sonnet", "Sonnet", "claude-sonnet-5"),
				modelAlias("sonnet-5", "Sonnet 5", "claude-sonnet-5"),
				modelAlias("sonnet-4-6", "Sonnet 4.6", "claude-sonnet-4-6"),
				modelAlias("haiku", "Haiku", "claude-haiku-4-5-20251001"),
				// Canonical provider ids, accepted verbatim. Operators pin the
				// full id rather than the short alias.
				modelChoice("claude-opus-5", "Opus 5 (canonical id)"),
				// The "[1m]" launch suffix is a valid Claude Code model-id form
				// and is emitted verbatim rather than normalized down to
				// "claude-opus-5": silently rewriting an explicit pin is the
				// same class of surprise these entries exist to eliminate.
				modelChoice("claude-opus-5[1m]", "Opus 5 1M (canonical id)"),
				modelChoice("claude-sonnet-5", "Sonnet 5 (canonical id)"),
				modelChoice("claude-fable-5", "Fable 5 (canonical id)"),
			),
		},
	},
	"codex": {
		DisplayName: "Codex CLI",
		Command:     "codex",
		// OpenAI serving-env binding (Codex reads these for a custom endpoint +
		// API key). Codex auth is the API key; no separate auth-token var.
		UpstreamBaseURLEnv: "OPENAI_BASE_URL",
		UpstreamAPIKeyEnv:  "OPENAI_API_KEY",
		OptionDefaults: map[string]string{
			"permission_mode": "unrestricted",
			"model":           "gpt-5.5",
			"effort":          "xhigh",
		},
		PromptMode:        "arg",
		ReadyPromptPrefix: "\u203a ",
		ReadyDelayMs:      3000,
		ProcessNames:      []string{"codex", "codex-raw"},
		SupportsHooks:     true,
		InstructionsFile:  "AGENTS.md",
		ResumeFlag:        "resume",
		ResumeStyle:       "subcommand",
		PrintArgs:         []string{"exec"},
		TitleModel:        "o4-mini",
		PermissionModes: map[string]string{
			"suggest":      "--ask-for-approval untrusted --sandbox read-only",
			"auto-edit":    "--full-auto",
			"unrestricted": "--dangerously-bypass-approvals-and-sandbox",
		},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:     "permission_mode",
				Label:   "Approval Policy",
				Type:    "select",
				Default: "unrestricted",
				Choices: []BuiltinOptionChoice{
					{Value: "suggest", Label: "Suggest (ask for approval)", FlagArgs: []string{"--ask-for-approval", "untrusted", "--sandbox", "read-only"}},
					{Value: "auto-edit", Label: "Full auto (sandboxed)", FlagArgs: []string{"--full-auto"}},
					{Value: "unrestricted", Label: "Bypass all (no sandbox)", FlagArgs: []string{"--dangerously-bypass-approvals-and-sandbox"}},
				},
			},
			modelOption(
				modelChoice("gpt-5.6-sol", "GPT-5.6 Sol"),
				modelChoice("gpt-5.6-terra", "GPT-5.6 Terra"),
				modelChoice("gpt-5.6-luna", "GPT-5.6 Luna"),
				modelChoice("gpt-5.5", "GPT-5.5"),
				modelChoice("gpt-5.3-codex", "GPT-5.3 Codex"),
				modelChoice("o3", "o3"),
				modelChoice("o4-mini", "o4-mini"),
			),
			{
				Key:   "sandbox",
				Label: "Sandbox",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "read-only", Label: "Read Only", FlagArgs: []string{"--sandbox", "read-only"}},
					{Value: "network-off", Label: "Network Off", FlagArgs: []string{"--sandbox", "network-off"}},
				},
			},
			// codex takes the tier as a config assignment rather than a flag.
			// The tier set is open for the same reason as every other
			// provider's: "max" is a canonical tier here and pinning it used
			// to be dropped on the floor because this enum stopped at xhigh,
			// even though the value looks blessed by claude's own defaults
			// (ga-fyh addendum). Passing it through lets codex accept or
			// reject it; silently ignoring it was never a correct answer.
			codexEffortOption(),
		},
	},
	"gemini": {
		DisplayName: "Gemini CLI",
		Command:     "gemini",
		// Gemini API key path (GEMINI_API_KEY canonical; GOOGLE_API_KEY is
		// Vertex-only). GOOGLE_GEMINI_BASE_URL overrides the endpoint.
		UpstreamBaseURLEnv: "GOOGLE_GEMINI_BASE_URL",
		UpstreamAPIKeyEnv:  "GEMINI_API_KEY",
		OptionDefaults: map[string]string{
			"permission_mode": "unrestricted",
		},
		PromptMode:        "arg",
		ReadyPromptPrefix: "> ",
		ReadyDelayMs:      5000,
		ProcessNames:      []string{"gemini", "node"},
		SupportsHooks:     true,
		InstructionsFile:  "AGENTS.md",
		ResumeFlag:        "--resume",
		ResumeStyle:       "flag",
		PrintArgs:         []string{"-p"},
		TitleModel:        "gemini-2.5-flash",
		PermissionModes: map[string]string{
			"default":      "--approval-mode default",
			"auto-edit":    "--approval-mode auto_edit",
			"plan":         "--approval-mode plan",
			"unrestricted": "--approval-mode yolo",
		},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:     "permission_mode",
				Label:   "Approval Mode",
				Type:    "select",
				Default: "unrestricted",
				Choices: []BuiltinOptionChoice{
					{Value: "default", Label: "Ask before actions", FlagArgs: []string{"--approval-mode", "default"}},
					{Value: "auto-edit", Label: "Auto-approve edits", FlagArgs: []string{"--approval-mode", "auto_edit"}},
					{Value: "plan", Label: "Read-only (plan)", FlagArgs: []string{"--approval-mode", "plan"}},
					{Value: "unrestricted", Label: "YOLO (approve all)", FlagArgs: []string{"--approval-mode", "yolo"}},
				},
			},
			modelOption(
				modelChoice("gemini-2.5-pro", "Gemini 2.5 Pro"),
				modelChoice("gemini-2.5-flash", "Gemini 2.5 Flash"),
			),
		},
	},
	"grok": {
		DisplayName: "Grok Build",
		Command:     "grok",
		// xAI Grok Build: XAI_API_KEY for headless (login is the default). No
		// documented base-URL override env (per-model base_url in config.toml).
		UpstreamAPIKeyEnv: "XAI_API_KEY",
		OptionDefaults: map[string]string{
			"permission_mode": "unrestricted",
			"model":           "grok-composer-2.5-fast",
		},
		// The grok TUI accepts no positional or flag-delivered initial
		// prompt (`-p/--single` is print-and-exit), so prompts are
		// delivered via tmux send-keys once the TUI is ready.
		//
		// grok's input handler does not accept send-keys until ~5-6s after
		// launch (TUI init: auth check + model-list load). Its prompt box
		// renders earlier (~3s) but silently drops keystrokes until then, so
		// ReadyPromptPrefix-based readiness detection can't be used here — the
		// box would match and we'd send into a not-yet-listening TUI. A blind
		// 5000ms delay raced that window: the initial nudge was lost and the
		// worker idled forever at the welcome screen (never running `gc hook`).
		// 12000ms clears the ready threshold with margin for spawn-time load.
		// Empirically verified against grok 0.2.32: send-keys is dropped at 5s
		// and lands reliably from ~6s onward.
		PromptMode:       "none",
		ReadyDelayMs:     12000,
		ProcessNames:     []string{"grok"},
		InstructionsFile: "AGENTS.md",
		ResumeFlag:       "--resume",
		ResumeStyle:      "flag",
		PrintArgs:        []string{"-p"},
		TitleModel:       "grok-composer-2.5-fast",
		PermissionModes: map[string]string{
			"default":      "--permission-mode default",
			"auto-edit":    "--permission-mode acceptEdits",
			"plan":         "--permission-mode plan",
			"full-auto":    "--permission-mode dontAsk",
			"unrestricted": "--permission-mode bypassPermissions",
		},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:     "permission_mode",
				Label:   "Permission Mode",
				Type:    "select",
				Default: "unrestricted",
				Choices: []BuiltinOptionChoice{
					{Value: "default", Label: "Ask before actions", FlagArgs: []string{"--permission-mode", "default"}},
					{Value: "auto-edit", Label: "Auto-approve edits", FlagArgs: []string{"--permission-mode", "acceptEdits"}},
					{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
					{Value: "full-auto", Label: "Full auto", FlagArgs: []string{"--permission-mode", "dontAsk"}},
					{Value: "unrestricted", Label: "Bypass permissions", FlagArgs: []string{"--permission-mode", "bypassPermissions"}},
				},
			},
			effortOption([]string{"--effort", optionValuePlaceholder}, effortTiers...),
			modelOption(
				modelChoice("grok-build", "Grok Build"),
				modelChoice("grok-composer-2.5", "Grok Composer 2.5"),
				modelChoice("grok-composer-2.5-fast", "Grok Composer 2.5 Fast"),
				// Frontier coding ids. Gasburger pins grok-4.6 on refinery and
				// gorkcats; this enum had not been touched since grok was added
				// and carried none of them, so the launch path found no
				// FlagArgs and silently omitted --model (ga-fyh). grok-4.7 is
				// listed ahead of its rollout.
				modelChoice("grok-4.6", "Grok 4.6"),
				modelChoice("grok-4.7", "Grok 4.7"),
			),
		},
	},
	"kimi": {
		DisplayName: "Kimi Code CLI",
		Command:     "kimi",
		// Moonshot Kimi CLI: KIMI_API_KEY / KIMI_BASE_URL (NOT MOONSHOT_API_KEY,
		// which is the raw Moonshot SDK var, nor OPENAI_* which is openai-type only).
		UpstreamBaseURLEnv:   "KIMI_BASE_URL",
		UpstreamAPIKeyEnv:    "KIMI_API_KEY",
		Args:                 []string{"--yolo", "--no-thinking"},
		PromptMode:           "none",
		ReadyDelayMs:         5000,
		ProcessNames:         []string{"kimi", "python"},
		AcceptStartupDialogs: boolPtr(false),
		SupportsACP:          true,
		SupportsHooks:        true,
		InstructionsFile:     "AGENTS.md",
		ResumeFlag:           "--session",
		ResumeStyle:          "flag",
		PrintArgs:            []string{"--quiet", "--prompt"},
		TitleModel:           "kimi-k2.6",
		ACPArgs:              []string{"--yolo", "--no-thinking", "acp"},
		OptionsSchema: []BuiltinProviderOption{
			modelOption(
				modelChoice("kimi-k2.6", "Kimi K2.6"),
				modelChoice("kimi-k2-thinking-turbo", "Kimi K2 Thinking Turbo"),
			),
		},
	},
	"kiro": {
		DisplayName: "Kiro",
		Command:     "kiro-cli",
		// AWS Kiro: KIRO_API_KEY for headless (ksk_…; login is the default). No
		// documented serving base-URL override env.
		UpstreamAPIKeyEnv: "KIRO_API_KEY",
		Args:              []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"},
		PromptMode:        "arg",
		ReadyDelayMs:      5000,
		ProcessNames:      []string{"kiro-cli", "kiro", "node"},
		// kiro launches with --trust-all-tools and never shows trust/permission
		// dialogs, so skip the 7-dialog startup polling (~56s/call, run twice).
		AcceptStartupDialogs: boolPtr(false),
		SupportsACP:          true,
		SupportsHooks:        true,
		InstructionsFile:     "AGENTS.md",
		ACPArgs:              []string{"acp", "--agent", "gascity"},
	},
	"cursor": {
		DisplayName: "Cursor Agent",
		Command:     "cursor-agent",
		// Cursor: CURSOR_API_KEY for headless (login is the default). Serving is
		// Cursor's own backend — no base-URL override env.
		UpstreamAPIKeyEnv: "CURSOR_API_KEY",
		Args:              []string{"-f"},
		PromptMode:        "arg",
		ReadyPromptPrefix: "\u2192 ",
		ReadyDelayMs:      10000,
		ProcessNames:      []string{"cursor-agent"},
		SupportsHooks:     true,
		InstructionsFile:  "AGENTS.md",
		ResumeFlag:        "--resume",
		ResumeStyle:       "flag",
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:     "mcp_approval",
				Label:   "MCP Approval",
				Type:    "select",
				Default: "prompt",
				Choices: []BuiltinOptionChoice{
					{Value: "prompt", Label: "Prompt for MCP approval"},
					{Value: "approve", Label: "Approve visible MCP servers", FlagArgs: []string{"--approve-mcps"}},
				},
			},
			// cursor-agent takes --model but publishes no -m short flag, and its
			// catalog is account-scoped (`cursor-agent --list-models`) and
			// includes parameterized bracket forms such as
			// claude-opus-4-8[context=1m,effort=high]. There is no closed set to
			// enumerate, which is exactly why the option is open: the curated
			// entries are suggestions and any id the account has reaches the
			// CLI. Before this option existed a cursor model pin was not in the
			// schema at all and was dropped without a flag (ga-fyh addendum).
			modelOption(
				modelChoiceNoAlias("auto", "Auto (default)"),
				modelChoiceNoAlias("composer-2.5", "Composer 2.5"),
				modelChoiceNoAlias("gpt-5.3-codex", "Codex 5.3"),
				modelChoiceNoAlias("cursor-grok-4.6-high", "Cursor Grok 4.6"),
				modelChoiceNoAlias("claude-opus-5-thinking-high", "Claude Opus 5 1M Thinking"),
				modelChoiceNoAlias("claude-sonnet-5-thinking-high", "Claude Sonnet 5 1M Thinking"),
			),
		},
	},
	"copilot": {
		DisplayName: "GitHub Copilot",
		Command:     "copilot",
		// Custom model serving (COPILOT_PROVIDER_BASE_URL/_API_KEY; a custom
		// upstream may also need COPILOT_PROVIDER_TYPE/COPILOT_MODEL via raw env).
		// auth_token = the GitHub-account bearer for the default GitHub-hosted path.
		UpstreamBaseURLEnv:   "COPILOT_PROVIDER_BASE_URL",
		UpstreamAPIKeyEnv:    "COPILOT_PROVIDER_API_KEY",
		UpstreamAuthTokenEnv: "COPILOT_GITHUB_TOKEN",
		Args:                 []string{"--yolo"},
		// PromptMode "none" delivers the prompt via tmux send-keys after the
		// ready prefix is detected (Step 6 in doStartSession), instead of
		// appending to argv. Required for copilot CLI 1.0.x which rejects
		// positional prompt arguments ("error: too many arguments"). The old
		// 0.0.x line accepted argv prompts; the rewrite in 1.0 made -p the
		// only non-interactive entry, but -p exits after completion and
		// breaks the long-running session contract gascity needs. Using
		// "none" + send-keys preserves the interactive REPL.
		PromptMode:        "none",
		ReadyPromptPrefix: "\u276f ",
		ReadyDelayMs:      5000,
		ProcessNames:      []string{"copilot"},
		SupportsHooks:     true,
		InstructionsFile:  "AGENTS.md",
		ResumeFlag:        "--resume",
		ResumeStyle:       "flag",
	},
	"amp": {
		// Hook mechanism: Amp CLI's plugin system (session.start,
		// tool.call) is documented at https://ampcode.com/manual.
		// Gas Town has not yet wired hook installation for amp —
		// tracked as gap 4 of gastownhall/gascity#672. Nudges still
		// drain via the supervisor dispatcher / per-session poller
		// without requiring provider hooks; the remaining work is
		// event-driven coordination (session-start priming,
		// pre-compaction handoff).
		DisplayName: "Sourcegraph AMP",
		Command:     "amp",
		// Amp connected mode: AMP_API_KEY credential, AMP_URL server/base-URL
		// override (verified in the compiled CLI). Login is the interactive default.
		UpstreamBaseURLEnv: "AMP_URL",
		UpstreamAPIKeyEnv:  "AMP_API_KEY",
		Args:               []string{"--dangerously-allow-all", "--no-ide"},
		PromptMode:         "arg",
		ProcessNames:       []string{"amp"},
		InstructionsFile:   "AGENTS.md",
		ResumeFlag:         "threads continue",
		ResumeStyle:        "subcommand",
	},
	"opencode": {
		DisplayName:  "OpenCode",
		Command:      "opencode",
		Args:         []string{},
		PromptMode:   "flag",
		PromptFlag:   "--prompt",
		ReadyDelayMs: 8000,
		ProcessNames: []string{"opencode", "node", "bun"},
		// OpenCode handles permissions through OPENCODE_PERMISSION and does not
		// show the Claude/Codex startup dialogs. Without this override, its
		// process-name hint enables two acceptance passes. Each pass polls
		// multiple unsupported dialog classes with independent timeouts, so the
		// first can exhaust the managed startup lease while OpenCode is working.
		AcceptStartupDialogs: boolPtr(false),
		Env:                  map[string]string{"OPENCODE_PERMISSION": `{"*":"allow"}`},
		SupportsACP:          true,
		SupportsHooks:        true,
		InstructionsFile:     "AGENTS.md",
		ResumeFlag:           "--session",
		ResumeStyle:          "flag",
		ACPArgs:              []string{"acp"},
		OptionsSchema: []BuiltinProviderOption{
			modelOption(
				modelChoice("opencode/deepseek-v4-flash-free", "DeepSeek V4 Flash Free"),
				modelChoice("opencode/nemotron-3-super-free", "Nemotron 3 Super Free"),
				modelChoice("opencode/big-pickle", "Big Pickle"),
			),
		},
	},
	"mimocode": {
		// MiMo Code (Xiaomi's `mimo` CLI) is an OpenCode fork. Permission
		// defaults are already permissive for bash/edit; only the
		// question/plan interaction gates block headless runs, so
		// --never-ask is the only default arg needed. The flag is
		// not taken by the `mimo acp` subcommand, so sessions default to the
		// CLI transport (config.ProviderSessionCreateTransport) and ACP stays
		// explicit opt-in until `mimo acp` has equivalent non-interactive
		// conformance coverage. No mimocode.json is staged — staging one
		// would clobber user config.
		DisplayName:      "MiMo Code",
		Command:          "mimo",
		Args:             []string{"--never-ask"},
		PromptMode:       "flag",
		PromptFlag:       "--prompt",
		ReadyDelayMs:     8000,
		ProcessNames:     []string{"mimo", ".mimocode", "node", "bun"},
		SupportsACP:      true,
		SupportsHooks:    true,
		InstructionsFile: "AGENTS.md",
		ResumeFlag:       "--session",
		ResumeStyle:      "flag",
		ACPArgs:          []string{"acp"},
		OptionsSchema: []BuiltinProviderOption{
			modelOption(
				modelChoice("mimo/mimo-auto", "MiMo Auto (free)"),
				modelChoice("xiaomi/mimo-v2.5-pro", "MiMo V2.5 Pro"),
				modelChoice("xiaomi/mimo-v2.5", "MiMo V2.5"),
				modelChoice("xiaomi-token-plan-sgp/mimo-v2.5-pro", "MiMo V2.5 Pro (Token Plan SGP)"),
				modelChoice("xiaomi-token-plan-sgp/mimo-v2.5", "MiMo V2.5 (Token Plan SGP)"),
			),
		},
	},
	"zcode": {
		// ZCode is Z.ai's GLM coding harness. It ships no public TUI — the
		// interactive terminal lives only in Z.ai's standalone SEA binary,
		// which has no public release — so the pane runs the engine's own
		// adapter (internal/worker/adapters/zcode), a persistent REPL that
		// turns each send-keys prompt into a headless `--json --prompt=` call.
		// Install it onto PATH with `gc`'s worker-inference setup script or
		// zcode.Install(binDir); the adapter needs ZCODE_CJS (the CLI bundle)
		// and ZCODE_API_KEY, and reads ZCODE_MODEL / ZCODE_BASE_URL.
		//
		// PromptMode "none": the adapter is a REPL, so the prompt arrives via
		// send-keys after the ready marker, never on argv.
		//
		// No resume_flag / resume_style / session_id_flag: restart continuity
		// is adapter-internal. ZCode's session ids are minted by the CLI and
		// its sessions live in a sqlite database gc cannot key, so the adapter
		// persists the provider session id under XDG_STATE_HOME and replays it
		// with --resume on the next process's first turn. Handing gc a resume
		// flag here would compose a launch flag the adapter does not take.
		DisplayName:          "ZCode (Z.ai GLM harness)",
		Command:              "zcode-repl",
		PromptMode:           "none",
		ReadyPromptPrefix:    "zcode-repl ready",
		ReadyDelayMs:         3000,
		ProcessNames:         []string{"bash", "node"},
		AcceptStartupDialogs: boolPtr(false),
		SupportsHooks:        false,
		InstructionsFile:     "AGENTS.md",
		UpstreamBaseURLEnv:   "ZCODE_BASE_URL",
		UpstreamAPIKeyEnv:    "ZCODE_API_KEY",
	},
	"cerebras": {
		DisplayName: "Cerebras (OpenCode)",
		Command:     "opencode",
		OptionDefaults: map[string]string{
			"model": "cerebras/gpt-oss-120b",
		},
		PromptMode:       "none",
		ReadyDelayMs:     8000,
		ProcessNames:     []string{"opencode", "node", "bun"},
		Env:              map[string]string{"OPENCODE_PERMISSION": `{"*":"allow"}`},
		SupportsACP:      true,
		SupportsHooks:    true,
		InstructionsFile: "AGENTS.md",
		ACPArgs:          []string{"acp"},
		TitleModel:       "cerebras/gpt-oss-120b",
		OptionsSchema: []BuiltinProviderOption{
			modelOption(
				modelChoiceNoAlias("cerebras/gpt-oss-120b", "GPT-OSS 120B"),
				modelChoiceNoAlias("cerebras/zai-glm-4.7", "GLM 4.7"),
				modelChoiceNoAlias("cerebras/qwen-3-235b-a22b-instruct-2507", "Qwen 3 235B A22B Instruct"),
			),
		},
	},
	"groq": {
		DisplayName: "Groq (OpenCode)",
		Command:     "opencode",
		OptionDefaults: map[string]string{
			"model": "groq/openai/gpt-oss-120b",
		},
		PromptMode:       "none",
		ReadyDelayMs:     8000,
		ProcessNames:     []string{"opencode", "node", "bun"},
		Env:              map[string]string{"OPENCODE_PERMISSION": `{"*":"allow"}`},
		SupportsACP:      true,
		SupportsHooks:    true,
		InstructionsFile: "AGENTS.md",
		ACPArgs:          []string{"acp"},
		TitleModel:       "groq/openai/gpt-oss-20b",
		OptionsSchema: []BuiltinProviderOption{
			modelOption(
				modelChoiceNoAlias("groq/openai/gpt-oss-120b", "GPT-OSS 120B"),
				modelChoiceNoAlias("groq/openai/gpt-oss-20b", "GPT-OSS 20B"),
				modelChoiceNoAlias("groq/llama-3.3-70b-versatile", "Llama 3.3 70B Versatile"),
				modelChoiceNoAlias("groq/llama-3.1-8b-instant", "Llama 3.1 8B Instant"),
				modelChoiceNoAlias("groq/qwen/qwen3-32b", "Qwen 3 32B"),
				modelChoiceNoAlias("groq/meta-llama/llama-4-scout-17b-16e-instruct", "Llama 4 Scout 17B"),
			),
		},
	},
	"auggie": {
		// Hook mechanism: Auggie CLI exposes SessionStart, SessionEnd,
		// Stop, PreToolUse, PostToolUse hooks via ~/.augment/settings.json
		// (https://docs.augmentcode.com/cli/overview). The config is
		// USER-global rather than project-local, which complicates Gas
		// Town's per-workdir installation model — wiring auggie hooks
		// requires either merging into the user's existing config or
		// designing a per-rig override mechanism. Tracked as gap 4 of
		// gastownhall/gascity#672. Nudges still drain via the supervisor
		// dispatcher / per-session poller without requiring provider
		// hooks.
		DisplayName:      "Auggie CLI",
		Command:          "auggie",
		Args:             []string{"--allow-indexing"},
		PromptMode:       "arg",
		ProcessNames:     []string{"auggie"},
		InstructionsFile: "AGENTS.md",
		ResumeFlag:       "--resume",
		ResumeStyle:      "flag",
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
		ResumeFlag:       "--session",
		ResumeStyle:      "flag",
		OptionsSchema: []BuiltinProviderOption{
			// The curated entry pairs a model with its --provider, which is why
			// declared choices win over the template. A bare id still resolves
			// through the template against pi's configured default provider,
			// rather than being dropped without a flag (ga-fyh).
			modelOption(BuiltinOptionChoice{
				Value:    "ollama-cloud-gpt-oss-20b",
				Label:    "Ollama Cloud GPT-OSS 20B",
				FlagArgs: []string{"--provider", "ollama-cloud", "--model", "gpt-oss:20b"},
			}),
		},
	},
	"omp": {
		DisplayName:      "Oh My Pi (OMP)",
		Command:          "omp",
		Args:             []string{"--hook", ".omp/hooks/gc-hook.ts"},
		PromptMode:       "arg",
		ProcessNames:     []string{"omp", "node", "bun"},
		SupportsHooks:    true,
		InstructionsFile: "AGENTS.md",
		ResumeFlag:       "--resume",
		ResumeStyle:      "flag",
	},
	"antigravity": {
		DisplayName: "Antigravity",
		Command:     "agy",
		OptionDefaults: map[string]string{
			"permission_mode": "unrestricted",
		},
		PromptMode:        "flag",
		PromptFlag:        "--prompt-interactive",
		ReadyPromptPrefix: "> ",
		ReadyDelayMs:      5000,
		ProcessNames:      []string{"agy"},
		SupportsHooks:     true,
		InstructionsFile:  "AGENTS.md",
		ResumeFlag:        "--conversation",
		ResumeStyle:       "flag",
		PrintArgs:         []string{"--print"},
		PermissionModes: map[string]string{
			"unrestricted": "--dangerously-skip-permissions",
			"accept-edits": "--mode accept-edits",
			"plan":         "--mode plan",
		},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:     "permission_mode",
				Label:   "Permission Mode",
				Type:    "select",
				Default: "unrestricted",
				Choices: []BuiltinOptionChoice{
					{Value: "unrestricted", Label: "Bypass permissions", FlagArgs: []string{"--dangerously-skip-permissions"}},
					{Value: "standard", Label: "Standard (prompt for permissions)", FlagArgs: []string{}},
					{Value: "accept-edits", Label: "Accept edits", FlagArgs: []string{"--mode", "accept-edits"}},
					{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--mode", "plan"}},
				},
			},
			// effort intentionally has no Default and no OptionDefaults entry:
			// agy < 1.1.10 silently ignores --effort (and --model) on the
			// --prompt-interactive launch path, so the flag must only ever be
			// sent when a user opts in explicitly.
			//
			// The curated tiers stop at high because that is what `agy models`
			// enumerates; the option is still open, so an xhigh/max pin reaches
			// agy to accept or reject rather than being dropped (ga-fyh).
			effortOption([]string{"--effort", optionValuePlaceholder}, "low", "medium", "high"),
			// Stable slugs + display names as enumerated by `agy models`; agy
			// defines no -m short alias. Same no-default rule as effort
			// (agy < 1.1.10 drops --model silently at launch).
			modelOption(
				modelChoiceNoAlias("gemini-3.6-flash-high", "Gemini 3.6 Flash (High)"),
				modelChoiceNoAlias("gemini-3.6-flash-medium", "Gemini 3.6 Flash (Medium)"),
				modelChoiceNoAlias("gemini-3.6-flash-low", "Gemini 3.6 Flash (Low)"),
				modelChoiceNoAlias("gemini-3.5-flash-high", "Gemini 3.5 Flash (High)"),
				modelChoiceNoAlias("gemini-3.5-flash-medium", "Gemini 3.5 Flash (Medium)"),
				modelChoiceNoAlias("gemini-3.5-flash-low", "Gemini 3.5 Flash (Low)"),
				modelChoiceNoAlias("gemini-3.1-pro-high", "Gemini 3.1 Pro (High)"),
				modelChoiceNoAlias("gemini-3.1-pro-low", "Gemini 3.1 Pro (Low)"),
				modelChoiceNoAlias("claude-sonnet-4-6", "Claude Sonnet 4.6 (Thinking)"),
				modelChoiceNoAlias("claude-opus-4-6-thinking", "Claude Opus 4.6 (Thinking)"),
				modelChoiceNoAlias("gpt-oss-120b-medium", "GPT-OSS 120B (Medium)"),
			),
			{
				Key:   "sandbox",
				Label: "Sandbox",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "enabled", Label: "Enabled", FlagArgs: []string{"--sandbox"}},
				},
			},
		},
	},
}

// BuiltinProviderOrder returns provider names in canonical order.
//
//nolint:revive // Mirrors the config boundary naming intentionally.
func BuiltinProviderOrder() []string {
	out := make([]string, len(builtinProviderOrder))
	copy(out, builtinProviderOrder)
	return out
}

// BuiltinProviders returns the canonical builtin worker provider definitions.
//
//nolint:revive // Mirrors the config boundary naming intentionally.
func BuiltinProviders() map[string]BuiltinProviderSpec {
	out := make(map[string]BuiltinProviderSpec, len(builtinProviderSpecs))
	for name, spec := range builtinProviderSpecs {
		out[name] = cloneBuiltinProviderSpec(spec)
	}
	return out
}

// CanonicalProfileIdentity returns the explicit compatibility identity for one
// of the canonical worker profiles.
func CanonicalProfileIdentity(profile string) (ProfileIdentity, bool) {
	switch profile {
	case "claude/tmux-cli":
		return newProfileIdentity(profile, "claude"), true
	case "codex/tmux-cli":
		return newProfileIdentity(profile, "codex"), true
	case "gemini/tmux-cli":
		return newProfileIdentity(profile, "gemini"), true
	case "kimi/tmux-cli":
		return newProfileIdentity(profile, "kimi"), true
	case "opencode/tmux-cli":
		return newProfileIdentity(profile, "opencode"), true
	case "mimocode/tmux-cli":
		return newProfileIdentity(profile, "mimocode"), true
	case "zcode/tmux-cli":
		return newProfileIdentity(profile, "zcode"), true
	case "pi/tmux-cli":
		return newProfileIdentity(profile, "pi"), true
	case "antigravity/tmux-cli":
		return newProfileIdentity(profile, "antigravity"), true
	default:
		return ProfileIdentity{}, false
	}
}

func newProfileIdentity(profile, family string) ProfileIdentity {
	compatibility := fmt.Sprintf("%s|behavior=%s|transcript=%s", profile, canonicalBehaviorClaimsVersion, canonicalTranscriptAdapterVersion)
	sum := sha256.Sum256([]byte(compatibility))
	return ProfileIdentity{
		Profile:                  profile,
		ProviderFamily:           family,
		TransportClass:           "tmux-cli",
		BehaviorClaimsVersion:    canonicalBehaviorClaimsVersion,
		TranscriptAdapterVersion: canonicalTranscriptAdapterVersion,
		CompatibilityVersion:     compatibility,
		CertificationFingerprint: hex.EncodeToString(sum[:8]),
	}
}

func cloneBuiltinProviderSpec(spec BuiltinProviderSpec) BuiltinProviderSpec {
	spec.Args = cloneStrings(spec.Args)
	spec.ProcessNames = cloneStrings(spec.ProcessNames)
	spec.Env = cloneStringMap(spec.Env)
	spec.PermissionModes = cloneStringMap(spec.PermissionModes)
	spec.OptionDefaults = cloneStringMap(spec.OptionDefaults)
	spec.PrintArgs = cloneStrings(spec.PrintArgs)
	spec.OptionsSchema = cloneBuiltinOptions(spec.OptionsSchema)
	spec.ACPArgs = cloneStrings(spec.ACPArgs)
	return spec
}

func cloneBuiltinOptions(options []BuiltinProviderOption) []BuiltinProviderOption {
	if options == nil {
		return nil
	}
	out := make([]BuiltinProviderOption, len(options))
	for i, option := range options {
		out[i] = BuiltinProviderOption{
			Key:          option.Key,
			Label:        option.Label,
			Type:         option.Type,
			Default:      option.Default,
			Choices:      cloneBuiltinChoices(option.Choices),
			FlagTemplate: cloneStrings(option.FlagTemplate),
		}
	}
	return out
}

func cloneBuiltinChoices(choices []BuiltinOptionChoice) []BuiltinOptionChoice {
	if choices == nil {
		return nil
	}
	out := make([]BuiltinOptionChoice, len(choices))
	for i, choice := range choices {
		out[i] = BuiltinOptionChoice{
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
