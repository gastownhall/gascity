// Package builtin defines the canonical builtin worker provider catalog.
package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	PermissionModes        map[string]string
	OptionDefaults         map[string]string
	OptionsSchema          []BuiltinProviderOption
	PrintArgs              []string
	TitleModel             string
	ACPCommand             string
	ACPArgs                []string
}

func boolPtr(b bool) *bool { return &b }

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
	"amp", "opencode", "groq", "cerebras", "auggie", "pi", "omp", "antigravity",
}

var builtinProviderSpecs = map[string]BuiltinProviderSpec{
	"claude": {
		DisplayName: "Claude Code",
		Command:     "claude",
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
		SessionIDFlag:          "--session-id",
		PrintArgs:              []string{"-p"},
		TitleModel:             "haiku",
		PermissionModes: map[string]string{
			"unrestricted": "--dangerously-skip-permissions",
			"plan":         "--permission-mode plan",
			"auto-edit":    "--permission-mode auto-edit",
			"full-auto":    "--permission-mode full-auto",
		},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:     "permission_mode",
				Label:   "Permission Mode",
				Type:    "select",
				Default: "auto-edit",
				Choices: []BuiltinOptionChoice{
					{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
					{Value: "full-auto", Label: "Full auto", FlagArgs: []string{"--permission-mode", "full-auto"}},
					{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
					{Value: "unrestricted", Label: "Bypass permissions", FlagArgs: []string{"--dangerously-skip-permissions"}},
				},
			},
			{
				Key:   "effort",
				Label: "Effort",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "low", Label: "Low", FlagArgs: []string{"--effort", "low"}},
					{Value: "medium", Label: "Medium", FlagArgs: []string{"--effort", "medium"}},
					{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}},
					{Value: "xhigh", Label: "Extra High", FlagArgs: []string{"--effort", "xhigh"}},
					{Value: "max", Label: "Max", FlagArgs: []string{"--effort", "max"}},
				},
			},
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "opus", Label: "Opus", FlagArgs: []string{"--model", "claude-opus-4-8"}, FlagAliases: [][]string{{"-m", "claude-opus-4-8"}}},
					{Value: "opus-4-7", Label: "Opus 4.7", FlagArgs: []string{"--model", "claude-opus-4-7"}, FlagAliases: [][]string{{"-m", "claude-opus-4-7"}}},
					{Value: "sonnet", Label: "Sonnet", FlagArgs: []string{"--model", "claude-sonnet-4-6"}, FlagAliases: [][]string{{"-m", "claude-sonnet-4-6"}}},
					{Value: "haiku", Label: "Haiku", FlagArgs: []string{"--model", "claude-haiku-4-5-20251001"}, FlagAliases: [][]string{{"-m", "claude-haiku-4-5-20251001"}}},
				},
			},
		},
	},
	"codex": {
		DisplayName: "Codex CLI",
		Command:     "codex",
		OptionDefaults: map[string]string{
			"permission_mode": "unrestricted",
			"model":           "gpt-5.5",
			"effort":          "xhigh",
		},
		PromptMode:        "arg",
		ReadyPromptPrefix: "\u203a ",
		ReadyDelayMs:      3000,
		ProcessNames:      []string{"codex"},
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
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "gpt-5.5", Label: "GPT-5.5", FlagArgs: []string{"--model", "gpt-5.5"}, FlagAliases: [][]string{{"-m", "gpt-5.5"}}},
					{Value: "gpt-5.3-codex-spark", Label: "GPT-5.3 Codex Spark", FlagArgs: []string{"--model", "gpt-5.3-codex-spark"}, FlagAliases: [][]string{{"-m", "gpt-5.3-codex-spark"}}},
					{Value: "o3", Label: "o3", FlagArgs: []string{"--model", "o3"}, FlagAliases: [][]string{{"-m", "o3"}}},
					{Value: "o4-mini", Label: "o4-mini", FlagArgs: []string{"--model", "o4-mini"}, FlagAliases: [][]string{{"-m", "o4-mini"}}},
				},
			},
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
			{
				Key:   "effort",
				Label: "Effort",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "low", Label: "Low", FlagArgs: []string{"-c", "model_reasoning_effort=low"}, FlagAliases: [][]string{{"-c", "model_reasoning_effort=\"low\""}}},
					{Value: "medium", Label: "Medium", FlagArgs: []string{"-c", "model_reasoning_effort=medium"}, FlagAliases: [][]string{{"-c", "model_reasoning_effort=\"medium\""}}},
					{Value: "high", Label: "High", FlagArgs: []string{"-c", "model_reasoning_effort=high"}, FlagAliases: [][]string{{"-c", "model_reasoning_effort=\"high\""}}},
					{Value: "xhigh", Label: "Extra High", FlagArgs: []string{"-c", "model_reasoning_effort=xhigh"}, FlagAliases: [][]string{{"-c", "model_reasoning_effort=\"xhigh\""}}},
				},
			},
		},
	},
	"gemini": {
		DisplayName: "Gemini CLI",
		Command:     "gemini",
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
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "gemini-2.5-pro", Label: "Gemini 2.5 Pro", FlagArgs: []string{"--model", "gemini-2.5-pro"}, FlagAliases: [][]string{{"-m", "gemini-2.5-pro"}}},
					{Value: "gemini-2.5-flash", Label: "Gemini 2.5 Flash", FlagArgs: []string{"--model", "gemini-2.5-flash"}, FlagAliases: [][]string{{"-m", "gemini-2.5-flash"}}},
				},
			},
		},
	},
	"grok": {
		DisplayName: "Grok Build",
		Command:     "grok",
		OptionDefaults: map[string]string{
			"permission_mode": "unrestricted",
			"model":           "grok-composer-2.5",
		},
		// The grok TUI accepts no positional or flag-delivered initial
		// prompt (`-p/--single` is print-and-exit), so prompts are
		// delivered via tmux send-keys.
		PromptMode:       "none",
		ReadyDelayMs:     5000,
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
			{
				Key:   "effort",
				Label: "Effort",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "low", Label: "Low", FlagArgs: []string{"--effort", "low"}},
					{Value: "medium", Label: "Medium", FlagArgs: []string{"--effort", "medium"}},
					{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}},
					{Value: "xhigh", Label: "Extra High", FlagArgs: []string{"--effort", "xhigh"}},
					{Value: "max", Label: "Max", FlagArgs: []string{"--effort", "max"}},
				},
			},
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "grok-build", Label: "Grok Build", FlagArgs: []string{"--model", "grok-build"}, FlagAliases: [][]string{{"-m", "grok-build"}}},
					{Value: "grok-composer-2.5", Label: "Grok Composer 2.5", FlagArgs: []string{"--model", "grok-composer-2.5"}, FlagAliases: [][]string{{"-m", "grok-composer-2.5"}}},
					{Value: "grok-composer-2.5-fast", Label: "Grok Composer 2.5 Fast", FlagArgs: []string{"--model", "grok-composer-2.5-fast"}, FlagAliases: [][]string{{"-m", "grok-composer-2.5-fast"}}},
				},
			},
		},
	},
	"kimi": {
		DisplayName:          "Kimi Code CLI",
		Command:              "kimi",
		Args:                 []string{"--yolo", "--no-thinking"},
		PromptMode:           "none",
		ReadyDelayMs:         5000,
		ProcessNames:         []string{"kimi", "python"},
		AcceptStartupDialogs: boolPtr(false),
		SupportsACP:          true,
		InstructionsFile:     "AGENTS.md",
		ResumeFlag:           "--session",
		ResumeStyle:          "flag",
		PrintArgs:            []string{"--quiet", "--prompt"},
		TitleModel:           "kimi-k2.6",
		ACPArgs:              []string{"--yolo", "--no-thinking", "acp"},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "kimi-k2.6", Label: "Kimi K2.6", FlagArgs: []string{"--model", "kimi-k2.6"}, FlagAliases: [][]string{{"-m", "kimi-k2.6"}}},
					{Value: "kimi-k2-thinking-turbo", Label: "Kimi K2 Thinking Turbo", FlagArgs: []string{"--model", "kimi-k2-thinking-turbo"}, FlagAliases: [][]string{{"-m", "kimi-k2-thinking-turbo"}}},
				},
			},
		},
	},
	"kiro": {
		DisplayName:  "Kiro",
		Command:      "kiro-cli",
		Args:         []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"},
		PromptMode:   "arg",
		ReadyDelayMs: 5000,
		ProcessNames: []string{"kiro-cli", "kiro", "node"},
		// kiro launches with --trust-all-tools and never shows trust/permission
		// dialogs, so skip the 7-dialog startup polling (~56s/call, run twice).
		AcceptStartupDialogs: boolPtr(false),
		SupportsACP:          true,
		SupportsHooks:        true,
		InstructionsFile:     "AGENTS.md",
		ACPArgs:              []string{"acp", "--agent", "gascity"},
	},
	"cursor": {
		DisplayName:       "Cursor Agent",
		Command:           "cursor-agent",
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
		},
	},
	"copilot": {
		DisplayName: "GitHub Copilot",
		Command:     "copilot",
		Args:        []string{"--yolo"},
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
		DisplayName:      "Sourcegraph AMP",
		Command:          "amp",
		Args:             []string{"--dangerously-allow-all", "--no-ide"},
		PromptMode:       "arg",
		ProcessNames:     []string{"amp"},
		InstructionsFile: "AGENTS.md",
		ResumeFlag:       "threads continue",
		ResumeStyle:      "subcommand",
	},
	"opencode": {
		DisplayName:      "OpenCode",
		Command:          "opencode",
		Args:             []string{},
		PromptMode:       "flag",
		PromptFlag:       "--prompt",
		ReadyDelayMs:     8000,
		ProcessNames:     []string{"opencode", "node", "bun"},
		Env:              map[string]string{"OPENCODE_PERMISSION": `{"*":"allow"}`},
		SupportsACP:      true,
		SupportsHooks:    true,
		InstructionsFile: "AGENTS.md",
		ResumeFlag:       "--session",
		ResumeStyle:      "flag",
		ACPArgs:          []string{"acp"},
		OptionsSchema: []BuiltinProviderOption{
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "opencode/deepseek-v4-flash-free", Label: "DeepSeek V4 Flash Free", FlagArgs: []string{"--model", "opencode/deepseek-v4-flash-free"}, FlagAliases: [][]string{{"-m", "opencode/deepseek-v4-flash-free"}}},
					{Value: "opencode/nemotron-3-super-free", Label: "Nemotron 3 Super Free", FlagArgs: []string{"--model", "opencode/nemotron-3-super-free"}, FlagAliases: [][]string{{"-m", "opencode/nemotron-3-super-free"}}},
					{Value: "opencode/big-pickle", Label: "Big Pickle", FlagArgs: []string{"--model", "opencode/big-pickle"}, FlagAliases: [][]string{{"-m", "opencode/big-pickle"}}},
				},
			},
		},
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
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "cerebras/gpt-oss-120b", Label: "GPT-OSS 120B", FlagArgs: []string{"--model", "cerebras/gpt-oss-120b"}},
					{Value: "cerebras/zai-glm-4.7", Label: "GLM 4.7", FlagArgs: []string{"--model", "cerebras/zai-glm-4.7"}},
					{Value: "cerebras/qwen-3-235b-a22b-instruct-2507", Label: "Qwen 3 235B A22B Instruct", FlagArgs: []string{"--model", "cerebras/qwen-3-235b-a22b-instruct-2507"}},
				},
			},
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
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "groq/openai/gpt-oss-120b", Label: "GPT-OSS 120B", FlagArgs: []string{"--model", "groq/openai/gpt-oss-120b"}},
					{Value: "groq/openai/gpt-oss-20b", Label: "GPT-OSS 20B", FlagArgs: []string{"--model", "groq/openai/gpt-oss-20b"}},
					{Value: "groq/llama-3.3-70b-versatile", Label: "Llama 3.3 70B Versatile", FlagArgs: []string{"--model", "groq/llama-3.3-70b-versatile"}},
					{Value: "groq/llama-3.1-8b-instant", Label: "Llama 3.1 8B Instant", FlagArgs: []string{"--model", "groq/llama-3.1-8b-instant"}},
					{Value: "groq/qwen/qwen3-32b", Label: "Qwen 3 32B", FlagArgs: []string{"--model", "groq/qwen/qwen3-32b"}},
					{Value: "groq/meta-llama/llama-4-scout-17b-16e-instruct", Label: "Llama 4 Scout 17B", FlagArgs: []string{"--model", "groq/meta-llama/llama-4-scout-17b-16e-instruct"}},
				},
			},
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
			{
				Key:   "model",
				Label: "Model",
				Type:  "select",
				Choices: []BuiltinOptionChoice{
					{Value: "", Label: "Default"},
					{Value: "ollama-cloud-gpt-oss-20b", Label: "Ollama Cloud GPT-OSS 20B", FlagArgs: []string{"--provider", "ollama-cloud", "--model", "gpt-oss:20b"}},
				},
			},
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
				},
			},
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
			Key:     option.Key,
			Label:   option.Label,
			Type:    option.Type,
			Default: option.Default,
			Choices: cloneBuiltinChoices(option.Choices),
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
