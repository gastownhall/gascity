package doctor

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestProviderParityCheck_NoConfig(t *testing.T) {
	c := NewProviderParityCheck(nil)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK", r.Status)
	}
	if !strings.Contains(r.Message, "no config") {
		t.Errorf("Message = %q, want mention of no config", r.Message)
	}
}

func TestProviderParityCheck_NoAgents(t *testing.T) {
	cfg := &config.City{}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
	if !strings.Contains(r.Message, "no providers referenced") {
		t.Errorf("Message = %q, want mention of no providers", r.Message)
	}
}

func TestProviderParityCheck_ClaudeOnly(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor", Provider: "claude"}},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestProviderParityCheck_FlagsProviderWithoutResume(t *testing.T) {
	// Inject a city-defined provider that explicitly opts out of resume
	// (empty ResumeFlag + ResumeCommand) and reference it from an agent.
	cfg := &config.City{
		Agents: []config.Agent{{Name: "tester", Provider: "noresume"}},
		Providers: map[string]config.ProviderSpec{
			"noresume": {Command: "noresume"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], `"noresume"`) || !strings.Contains(r.Details[0], "ResumeFlag") {
		t.Errorf("Details[0] missing provider name or ResumeFlag mention: %q", r.Details[0])
	}
	if r.FixHint == "" {
		t.Error("expected non-empty FixHint")
	}
}

func TestProviderParityCheck_BypassedWhenStartCommandSet(t *testing.T) {
	// Agents that pin StartCommand bypass ProviderSpec entirely, so the
	// provider parity warning should not fire.
	cfg := &config.City{
		Agents: []config.Agent{{Name: "raw", StartCommand: "raw --foo", Provider: "noresume"}},
		Providers: map[string]config.ProviderSpec{
			"noresume": {Command: "noresume"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestProviderParityCheck_ChecksWorkspaceDefaultProvider(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "noresume"},
		Providers: map[string]config.ProviderSpec{
			"noresume": {Command: "noresume"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
}

func TestProviderParityCheck_CityOverrideExtendsBuiltin(t *testing.T) {
	// City-defined "claude" overrides only DisplayName; ResumeFlag inherits
	// from the builtin so the check should pass.
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor", Provider: "claude"}},
		Providers: map[string]config.ProviderSpec{
			"claude": {DisplayName: "MyClaude"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestProviderParityCheck_CustomProviderBaseInheritsBuiltinResume(t *testing.T) {
	base := "builtin:codex"
	cfg := &config.City{
		Agents: []config.Agent{{Name: "coder", Provider: "wrapped-codex"}},
		Providers: map[string]config.ProviderSpec{
			"wrapped-codex": {Base: &base},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestProviderParityCheck_CommandMatchInheritsBuiltinResume(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "helper", Provider: "fast-claude"}},
		Providers: map[string]config.ProviderSpec{
			"fast-claude": {Command: "claude"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestProviderParityCheck_AgentResumeCommandSuppressesWarning(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:          "tester",
			Provider:      "noresume",
			ResumeCommand: "noresume --continue {{.SessionKey}}",
		}},
		Providers: map[string]config.ProviderSpec{
			"noresume": {Command: "noresume"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestProviderParityCheck_ExplicitStandaloneBuiltinNameWarns(t *testing.T) {
	base := ""
	cfg := &config.City{
		Agents: []config.Agent{{Name: "standalone", Provider: "claude"}},
		Providers: map[string]config.ProviderSpec{
			"claude": {Base: &base, Command: "claude"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], `"claude"`) {
		t.Fatalf("Details = %v, want one warning for claude", r.Details)
	}
}

func TestProviderParityCheck_DeterministicOrdering(t *testing.T) {
	// Multiple gaps must be reported in stable alphabetic order.
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "a1", Provider: "zproblem"},
			{Name: "a2", Provider: "aproblem"},
		},
		Providers: map[string]config.ProviderSpec{
			"zproblem": {Command: "z"},
			"aproblem": {Command: "a"},
		},
	}
	r := NewProviderParityCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 2 {
		t.Fatalf("Details = %d, want 2: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], `"aproblem"`) {
		t.Errorf("Details[0] should mention aproblem first: %q", r.Details[0])
	}
	if !strings.Contains(r.Details[1], `"zproblem"`) {
		t.Errorf("Details[1] should mention zproblem second: %q", r.Details[1])
	}
}
