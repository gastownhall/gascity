package config

import (
	"strings"
	"testing"
)

func TestValidateSemanticsNoWarnings(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Provider: "claude"},
		Agents: []Agent{
			{Name: "mayor", Provider: "claude"},
			{Name: "worker", Provider: "codex"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestValidateSemanticsUnknownAgentProvider(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "mayor", Provider: "cloude"}, // typo
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "cloude") {
		t.Errorf("warning should mention bad provider: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "mayor") {
		t.Errorf("warning should mention agent: %s", warnings[0])
	}
}

func TestValidateSemanticsCustomProviderOK(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"my-agent": {Command: "my-agent-cli"},
		},
		Agents: []Agent{
			{Name: "worker", Provider: "my-agent"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for custom provider, got: %v", warnings)
	}
}

func TestValidateSemanticsUnknownWorkspaceProvider(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Provider: "bogus"},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "[workspace]") {
		t.Errorf("warning should mention workspace: %s", warnings[0])
	}
}

func TestValidateSemanticsStartCommandSkipsProviderCheck(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "custom", Provider: "nonexistent", StartCommand: "my-binary"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("start_command should skip provider check, got: %v", warnings)
	}
}

func TestValidateSemanticsAgentSessionTransportAllowsTmux(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "worker", Provider: "claude", Session: "tmux"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings for tmux session transport, got: %v", warnings)
	}
}

func TestValidateSemanticsAgentSessionTransportRejectsUnknown(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "worker", Provider: "claude", Session: "stdio"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "stdio") || !strings.Contains(warnings[0], "tmux") {
		t.Fatalf("warning should mention bad value and allowed transports: %s", warnings[0])
	}
}

func TestValidateSemanticsProviderPromptModeBad(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"bad": {PromptMode: "pipe"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "pipe") {
		t.Errorf("warning should mention bad value: %s", warnings[0])
	}
}

func TestValidateSemanticsProviderPromptFlagRequired(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"needsflag": {PromptMode: "flag"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "prompt_flag") {
		t.Errorf("warning should mention prompt_flag: %s", warnings[0])
	}
}

func TestValidateSemanticsProviderPromptFlagOK(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"ok": {PromptMode: "flag", PromptFlag: "--prompt"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestValidateSemanticsMultipleIssues(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Provider: "nope"},
		Providers: map[string]ProviderSpec{
			"bad": {PromptMode: "pipe"},
		},
		Agents: []Agent{
			{Name: "a1", Provider: "missing1"},
			{Name: "a2", Provider: "missing2"},
		},
	}
	warnings := ValidateSemantics(cfg, "test.toml")
	// 1 workspace + 2 agents + 1 provider = 4
	if len(warnings) != 4 {
		t.Fatalf("expected 4 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateSemanticsIncludesSource(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "bad", Provider: "missing"},
		},
	}
	warnings := ValidateSemantics(cfg, "/path/to/city.toml")
	if len(warnings) == 0 {
		t.Fatal("expected warning")
	}
	if !strings.Contains(warnings[0], "/path/to/city.toml") {
		t.Errorf("warning should include source path: %s", warnings[0])
	}
}

func TestValidateAgentsScopeBadEnum(t *testing.T) {
	agents := []Agent{
		{Name: "bad", Scope: "global"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for bad scope")
	}
	if !strings.Contains(err.Error(), "global") {
		t.Errorf("error should mention bad value: %v", err)
	}
}

func TestValidateAgentsScopeValidValues(t *testing.T) {
	for _, scope := range []string{"", "city", "rig"} {
		agents := []Agent{
			{Name: "ok", Scope: scope},
		}
		if err := ValidateAgents(agents); err != nil {
			t.Errorf("scope %q should be valid, got: %v", scope, err)
		}
	}
}

func TestValidateAgentsPromptModeBadEnum(t *testing.T) {
	agents := []Agent{
		{Name: "bad", PromptMode: "pipe"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for bad prompt_mode")
	}
	if !strings.Contains(err.Error(), "pipe") {
		t.Errorf("error should mention bad value: %v", err)
	}
}

func TestValidateAgentsPromptModeValidValues(t *testing.T) {
	for _, mode := range []string{"", "arg", "flag", "none"} {
		agents := []Agent{
			{Name: "ok", PromptMode: mode, PromptFlag: "--p"},
		}
		if err := ValidateAgents(agents); err != nil {
			t.Errorf("prompt_mode %q should be valid, got: %v", mode, err)
		}
	}
}

func TestValidateAgentsPromptFlagRequiredForFlagMode(t *testing.T) {
	agents := []Agent{
		{Name: "bad", PromptMode: "flag"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for missing prompt_flag")
	}
	if !strings.Contains(err.Error(), "prompt_flag") {
		t.Errorf("error should mention prompt_flag: %v", err)
	}
}

func TestValidateAgentsPromptFlagWithFlagModeOK(t *testing.T) {
	agents := []Agent{
		{Name: "ok", PromptMode: "flag", PromptFlag: "--prompt"},
	}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("should be valid: %v", err)
	}
}
