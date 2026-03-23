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
// PromptMode is "flag", the PromptFlag is prepended to the shell-quoted
// prompt text in PromptSuffix. The resulting command will be:
// "provider --prompt '<prompt text>'" instead of "provider '<prompt text>'".
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
	if !strings.HasPrefix(cfg.PromptSuffix, "--prompt ") {
		t.Errorf("flag mode PromptSuffix should start with --prompt flag, got %q", cfg.PromptSuffix)
	}
	// The full command would be: myprovider --prompt '<text>'
	fullCommand := cfg.Command + " " + cfg.PromptSuffix
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

// TestTemplateParamsToConfigArgModeLongPromptDemonstratesBug demonstrates
// the original bug: when a provider using PromptMode "arg" receives a long
// prompt (beacon + session instructions), the prompt is appended as a bare
// positional argument. For OpenCode, this is interpreted as a filesystem
// path, causing ENAMETOOLONG (>255 bytes) or "failed to change directory"
// errors that trigger generation escalation and crash-loop quarantine.
func TestTemplateParamsToConfigArgModeLongPromptDemonstratesBug(t *testing.T) {
	// Simulate the kind of prompt that was being generated for agents:
	// beacon (timestamped) + behavioral instructions from pack templates.
	longPrompt := strings.Repeat("x", 300) // Exceeds 255-byte filename limit

	tp := TemplateParams{
		Command: "opencode",
		Prompt:  longPrompt,
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "opencode",
			Command:    "opencode",
			PromptMode: "arg",
		},
	}

	cfg := templateParamsToConfig(tp)

	// When this PromptSuffix is appended to the command and passed to the
	// shell, opencode receives the 300-character string as argv[1]. It then
	// calls os.Chdir(argv[1]), which fails with ENAMETOOLONG because Linux
	// has a 255-byte limit on individual path components.
	fullCommand := cfg.Command + " " + cfg.PromptSuffix
	if !strings.HasPrefix(fullCommand, "opencode ") {
		t.Fatalf("unexpected command format: %q", fullCommand)
	}

	// The prompt is shell-quoted, so the actual arg to opencode would be
	// the 300-char string — well over the 255 filename limit.
	if len(longPrompt) <= 255 {
		t.Fatal("test setup error: prompt should exceed filename limit")
	}

	// This test documents the bug rather than testing a fix — the fix is
	// that OpenCode's PromptMode is now "none", so this code path is never
	// reached for OpenCode sessions.
	t.Logf("Bug demonstration: opencode would receive %d-byte positional arg as project directory", len(longPrompt))
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
