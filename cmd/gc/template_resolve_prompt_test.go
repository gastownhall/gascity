package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestTemplateParamsToConfigArgModeAppendsPromptAsBareArg verifies that
// when PromptMode is "arg" (the default), the prompt text is shell-quoted
// and placed in PromptSuffix without any flag prefix. The tmux adapter
// then appends this directly to the command: "provider <prompt>".
//
// This is the behavior that caused the OpenCode crash: the prompt text
// (containing beacon + behavioral instructions) was passed as a bare
// positional argument, which OpenCode v1.3+ interprets as a project
// directory path.
func TestTemplateParamsToConfigArgModeAppendsPromptAsBareArg(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "You are an agent. Do work.",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "opencode",
			Command:    "opencode",
			PromptMode: "arg",
		},
	}

	cfg := templateParamsToConfig(tp)

	// PromptSuffix should be a shell-quoted string without any flag.
	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty for arg mode with non-empty prompt")
	}
	// Must not start with a flag like --prompt.
	if strings.HasPrefix(cfg.PromptSuffix, "--") {
		t.Errorf("arg mode PromptSuffix should not start with a flag, got %q", cfg.PromptSuffix)
	}
	// The resulting command would be: opencode '<prompt text>'
	// For opencode this is fatal — it treats the arg as a project directory.
	fullCommand := cfg.Command + " " + cfg.PromptSuffix
	if !strings.HasPrefix(fullCommand, "opencode '") {
		t.Errorf("fullCommand = %q, expected opencode followed by quoted prompt", fullCommand)
	}
}

// TestTemplateParamsToConfigFlagModePrependsFlag verifies that when
// PromptMode is "flag", the PromptFlag is stored separately in
// runtime.Config.PromptFlag and PromptSuffix contains only the
// shell-quoted prompt text. The runtime (tmux adapter, ACP) combines
// them: "provider --prompt '<prompt text>'".
func TestTemplateParamsToConfigFlagModePrependsFlag(t *testing.T) {
	tp := TemplateParams{
		Command: "myprovider",
		Prompt:  "You are an agent.",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "myprovider",
			Command:    "myprovider",
			PromptMode: "flag",
			PromptFlag: "--prompt",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty for flag mode with non-empty prompt")
	}
	if cfg.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", cfg.PromptFlag, "--prompt")
	}
	// PromptSuffix should be just the quoted text, not the flag.
	if strings.HasPrefix(cfg.PromptSuffix, "--") {
		t.Errorf("flag mode PromptSuffix should not contain the flag prefix, got %q", cfg.PromptSuffix)
	}
	// The runtime reconstructs: myprovider --prompt '<text>'
	fullCommand := cfg.Command + " " + cfg.PromptFlag + " " + cfg.PromptSuffix
	if !strings.Contains(fullCommand, "--prompt '") {
		t.Errorf("fullCommand = %q, expected --prompt followed by quoted text", fullCommand)
	}
}

// TestTemplateParamsToConfigNoneModeNoPromptSuffix verifies that when
// PromptMode is "none", no prompt is generated regardless of the Prompt
// field. This is the correct mode for providers like OpenCode and Codex
// that don't accept prompts as command-line arguments.
func TestTemplateParamsToConfigNoneModeNoPromptSuffix(t *testing.T) {
	// When PromptMode is "none", resolveTemplate sets tp.Prompt to "" (Step 9
	// skips prompt rendering when PromptMode == "none"). So tp.Prompt will be
	// empty by the time templateParamsToConfig is called.
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "", // PromptMode "none" means resolveTemplate leaves this empty.
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "opencode",
			Command:    "opencode",
			PromptMode: "none",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix should be empty for none mode, got %q", cfg.PromptSuffix)
	}
}

// TestTemplateParamsToConfigFlagModeEmptyPrompt verifies that when
// PromptMode is "flag" but the prompt is empty, no PromptSuffix is set.
func TestTemplateParamsToConfigFlagModeEmptyPrompt(t *testing.T) {
	tp := TemplateParams{
		Command: "myprovider",
		Prompt:  "",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "myprovider",
			Command:    "myprovider",
			PromptMode: "flag",
			PromptFlag: "--prompt",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix should be empty when prompt is empty, got %q", cfg.PromptSuffix)
	}
}

// TestTemplateParamsToConfigFlagModeNoFlagInSuffix verifies that flag
// mode stores the flag in PromptFlag, not in PromptSuffix. This is
// critical: the tmux adapter's file-expansion path needs them separate
// to reconstruct the command correctly for long prompts.
func TestTemplateParamsToConfigFlagModeNoFlagInSuffix(t *testing.T) {
	longPrompt := strings.Repeat("x", 2000) // Exceeds maxInlinePromptLen

	tp := TemplateParams{
		Command: "myprovider",
		Prompt:  longPrompt,
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "myprovider",
			Command:    "myprovider",
			PromptMode: "flag",
			PromptFlag: "--prompt",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", cfg.PromptFlag, "--prompt")
	}
	// PromptSuffix must contain only the quoted prompt, not the flag.
	if strings.Contains(cfg.PromptSuffix, "--prompt") {
		t.Errorf("PromptSuffix should not contain the flag, got %q (truncated)", cfg.PromptSuffix[:80])
	}
	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty")
	}
}

// TestTemplateParamsToConfigNilResolvedProvider verifies that
// templateParamsToConfig doesn't panic when ResolvedProvider is nil.
func TestTemplateParamsToConfigNilResolvedProvider(t *testing.T) {
	tp := TemplateParams{
		Command:          "echo",
		Prompt:           "hello",
		ResolvedProvider: nil,
	}

	cfg := templateParamsToConfig(tp)

	// Should fall back to bare arg mode (no flag prefix).
	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty")
	}
	if strings.HasPrefix(cfg.PromptSuffix, "--") {
		t.Errorf("nil ResolvedProvider should not add flag prefix, got %q", cfg.PromptSuffix)
	}
}
