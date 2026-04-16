package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

var testBeaconTime = time.Unix(1_700_000_000, 0)

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

// TestTemplateParamsToConfigNoneModeUsesNudge verifies that when PromptMode is
// "none" and hooks are not available, startup instructions are delivered via
// runtime.Config.Nudge instead of PromptSuffix.
func TestTemplateParamsToConfigNoneModeUsesNudge(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "You are an agent. Do work.",
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
	if cfg.Nudge != "You are an agent. Do work." {
		t.Errorf("Nudge = %q, want startup prompt", cfg.Nudge)
	}
}

func TestTemplateParamsToConfigNoneModeWithHooksSkipsStartupNudge(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "You are an agent. Do work.",
		Hints: agent.StartupHints{
			Nudge: "existing nudge",
		},
		HookEnabled: true,
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "opencode",
			Command:       "opencode",
			PromptMode:    "none",
			SupportsHooks: true,
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix should be empty for none mode, got %q", cfg.PromptSuffix)
	}
	if cfg.Nudge != "existing nudge" {
		t.Errorf("Nudge = %q, want existing nudge only", cfg.Nudge)
	}
}

func TestTemplateParamsToConfigHookEnabledProviderSkipsLaunchPrompt(t *testing.T) {
	tests := []struct {
		name       string
		promptMode string
		promptFlag string
	}{
		{name: "arg mode", promptMode: "arg"},
		{name: "flag mode", promptMode: "flag", promptFlag: "--prompt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := TemplateParams{
				Command: "claude",
				Prompt:  "startup prompt",
				Hints: agent.StartupHints{
					Nudge: "existing nudge",
				},
				HookEnabled: true,
				ResolvedProvider: &config.ResolvedProvider{
					Name:          "claude",
					Command:       "claude",
					PromptMode:    tt.promptMode,
					PromptFlag:    tt.promptFlag,
					SupportsHooks: true,
				},
			}

			cfg := templateParamsToConfig(tp)

			if cfg.PromptSuffix != "" {
				t.Fatalf("PromptSuffix should be empty for hook-enabled startup, got %q", cfg.PromptSuffix)
			}
			if cfg.PromptFlag != "" {
				t.Fatalf("PromptFlag should be empty for hook-enabled startup, got %q", cfg.PromptFlag)
			}
			if cfg.Nudge != "existing nudge" {
				t.Fatalf("Nudge = %q, want existing nudge only", cfg.Nudge)
			}
		})
	}
}

func TestTemplateParamsToConfigNoneModePreservesExistingNudge(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "startup prompt",
		Hints: agent.StartupHints{
			Nudge: "existing nudge",
		},
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
	want := "startup prompt\n\n---\n\nexisting nudge"
	if cfg.Nudge != want {
		t.Errorf("Nudge = %q, want %q", cfg.Nudge, want)
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

func TestResolveTemplateNoneModeRetainsPromptForDeferredDelivery(t *testing.T) {
	cityPath := t.TempDir()
	fs := fsys.NewFake()
	fs.Files[cityPath+"/prompts/pool-worker.md"] = []byte("pool prompt body")

	params := &agentBuildParams{
		fs:              fs,
		cityName:        "bright-lights",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "bright-lights", Provider: "opencode"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/opencode", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}
	agent := &config.Agent{
		Name:           "opencode",
		PromptTemplate: "prompts/pool-worker.md",
		Provider:       "opencode",
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if tp.Prompt == "" {
		t.Fatal("Prompt should be preserved for PromptMode=none providers so it can be delivered via nudge")
	}
	if !strings.Contains(tp.Prompt, "pool prompt body") {
		t.Fatalf("Prompt missing rendered template body: %q", tp.Prompt)
	}
	if !strings.Contains(tp.Prompt, "[bright-lights] opencode") {
		t.Fatalf("Prompt missing beacon: %q", tp.Prompt)
	}
}

func TestResolveTemplateHookEnabledOpencodeOmitsPrimeInstruction(t *testing.T) {
	cityPath := t.TempDir()
	fs := fsys.NewFake()
	fs.Files[cityPath+"/prompts/mayor.md"] = []byte("mayor prompt body")

	params := &agentBuildParams{
		fs:              fs,
		cityName:        "bright-lights",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "bright-lights", Provider: "opencode", InstallAgentHooks: []string{"opencode"}},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/opencode", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}
	agent := &config.Agent{
		Name:           "mayor",
		PromptTemplate: "prompts/mayor.md",
		Provider:       "opencode",
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if !tp.HookEnabled {
		t.Fatal("HookEnabled = false, want true")
	}
	if !strings.Contains(tp.Prompt, "mayor prompt body") {
		t.Fatalf("Prompt missing rendered template body: %q", tp.Prompt)
	}
	if strings.Contains(tp.Prompt, "Run `gc prime`") {
		t.Fatalf("hook-enabled prompt should omit manual gc prime instruction: %q", tp.Prompt)
	}
}
