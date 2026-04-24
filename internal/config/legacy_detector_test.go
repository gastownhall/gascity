package config

import (
	"strings"
	"testing"
)

func TestDetectLegacyProviderInheritance_NameMatch(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex": {Command: "aimux", Args: []string{"run", "codex"}},
		},
	}
	warnings := DetectLegacyProviderInheritance(cfg, "test.toml")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "name matches built-in") {
		t.Errorf("warning text missing name-match reason: %q", warnings[0])
	}
	if !strings.Contains(warnings[0], `base = "builtin:codex"`) {
		t.Errorf("warning text missing suggested fix: %q", warnings[0])
	}
}

func TestDetectLegacyProviderInheritance_CommandMatch(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"my-alias": {Command: "claude"},
		},
	}
	warnings := DetectLegacyProviderInheritance(cfg, "test.toml")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0], "command") || !strings.Contains(warnings[0], `built-in "claude"`) {
		t.Errorf("warning text missing command-match reason: %q", warnings[0])
	}
}

func TestDetectLegacyProviderInheritance_ExplicitBaseSilences(t *testing.T) {
	base := "builtin:codex"
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex": {Base: &base, Command: "aimux"},
		},
	}
	warnings := DetectLegacyProviderInheritance(cfg, "test.toml")
	if len(warnings) != 0 {
		t.Errorf("explicit base should silence warning, got %v", warnings)
	}
}

func TestDetectLegacyProviderInheritance_ExplicitEmptySilences(t *testing.T) {
	empty := ""
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex": {Base: &empty, Command: "aimux"},
		},
	}
	warnings := DetectLegacyProviderInheritance(cfg, "test.toml")
	if len(warnings) != 0 {
		t.Errorf("explicit empty base should silence warning, got %v", warnings)
	}
}

func TestDetectLegacyProviderInheritance_NoMatch(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"my-custom": {Command: "my-bin"},
		},
	}
	warnings := DetectLegacyProviderInheritance(cfg, "test.toml")
	if len(warnings) != 0 {
		t.Errorf("no match should produce no warnings, got %v", warnings)
	}
}

func TestDetectLegacyProviderInheritance_MultipleDeterministic(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex":  {Command: "aimux"},
			"claude": {Command: "aimux"},
			"gemini": {Command: "aimux"},
		},
	}
	warnings := DetectLegacyProviderInheritance(cfg, "t.toml")
	if len(warnings) != 3 {
		t.Fatalf("warnings = %d, want 3: %v", len(warnings), warnings)
	}
	// Alphabetical order by provider name.
	for i, expected := range []string{"claude", "codex", "gemini"} {
		if !strings.Contains(warnings[i], "[providers."+expected+"]") {
			t.Errorf("warning %d = %q, expected to reference %s", i, warnings[i], expected)
		}
	}
}
