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
	"claude", "codex", "gemini", "kiro", "cursor", "copilot",
	"amp", "opencode", "auggie", "pi", "omp",
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
					{Value: "opus", Label: "Opus", FlagArgs: []string{"--model", "claude-opus-4-7"}, FlagAliases: [][]string{{"-m", "claude-opus-4-7"}}},
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
	"kiro": {
		DisplayName:      "Kiro",
		Command:          "kiro-cli",
		Args:             []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"},
		PromptMode:       "arg",
		ReadyDelayMs:     5000,
		ProcessNames:     []string{"kiro-cli", "kiro", "node"},
		SupportsACP:      true,
		SupportsHooks:    true,
		InstructionsFile: "AGENTS.md",
		ACPArgs:          []string{"acp", "--agent", "gascity"},
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
	case "opencode/tmux-cli":
		return newProfileIdentity(profile, "opencode"), true
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
