package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
)

func TestConfigHash_Canonical(t *testing.T) {
	// Same params should produce the same hash regardless of call order.
	params := TemplateParams{
		Command: "claude --dangerously-skip-permissions",
		Prompt:  "You are a helpful agent.",
		Env:     map[string]string{"FOO": "bar", "BAZ": "qux"},
		Hints: agent.StartupHints{
			PreStart:     []string{"echo setup"},
			SessionSetup: []string{"echo ready"},
		},
		WorkDir:     "/home/user/project",
		SessionName: "test-session",
	}

	h1 := canonicalConfigHash(params, nil)
	h2 := canonicalConfigHash(params, nil)
	if h1 != h2 {
		t.Errorf("same params produced different hashes: %q vs %q", h1, h2)
	}
	if len(h1) != 16 {
		t.Errorf("hash length = %d, want 16", len(h1))
	}
}

func TestConfigHash_Behavioral(t *testing.T) {
	base := TemplateParams{
		Command:      "claude",
		Prompt:       "prompt",
		Env:          map[string]string{"KEY": "val"},
		WorkDir:      "/work",
		SessionName:  "s1",
		TemplateName: "worker",
		RigName:      "my-rig",
	}
	baseHash := canonicalConfigHash(base, nil)

	// Changing non-behavioral fields should NOT change hash.
	nonBehavioral := base
	nonBehavioral.SessionName = "s2"        // name excluded
	nonBehavioral.TemplateName = "overseer" // template excluded
	nonBehavioral.RigName = "other-rig"     // rig excluded

	if h := canonicalConfigHash(nonBehavioral, nil); h != baseHash {
		t.Errorf("non-behavioral change produced different hash: %q vs %q", h, baseHash)
	}

	// Changing behavioral fields SHOULD change hash.
	behavioral := base
	behavioral.Command = "gemini"
	if h := canonicalConfigHash(behavioral, nil); h == baseHash {
		t.Error("command change should produce different hash")
	}

	envChanged := base
	envChanged.Env = map[string]string{"KEY": "different"}
	if h := canonicalConfigHash(envChanged, nil); h == baseHash {
		t.Error("env change should produce different hash")
	}
}

func TestConfigHash_IgnoresNudge(t *testing.T) {
	base := TemplateParams{
		Command: "claude",
		Prompt:  "prompt",
		Hints: agent.StartupHints{
			Nudge: "first work item",
		},
	}
	baseHash := canonicalConfigHash(base, nil)

	changed := base
	changed.Hints.Nudge = "second work item"
	if h := canonicalConfigHash(changed, nil); h != baseHash {
		t.Errorf("nudge change produced different hash: %q vs %q", h, baseHash)
	}
}

func TestConfigHash_IncludesOverlayProviderIdentity(t *testing.T) {
	claudeFallback := TemplateParams{
		Command: "agent",
		Prompt:  "prompt",
		Hints: agent.StartupHints{
			ProviderName: "claude",
		},
	}
	kiroOverlay := claudeFallback
	kiroOverlay.Hints.ProviderOverlayName = "kiro"
	if canonicalConfigHash(claudeFallback, nil) == canonicalConfigHash(kiroOverlay, nil) {
		t.Fatal("ProviderOverlayName should affect canonical config hash")
	}

	withHook := kiroOverlay
	withHook.Hints.InstallAgentHooks = []string{"gemini"}
	if canonicalConfigHash(kiroOverlay, nil) == canonicalConfigHash(withHook, nil) {
		t.Fatal("InstallAgentHooks should affect canonical config hash")
	}
}

func TestConfigHash_Overlay(t *testing.T) {
	// template + overlay should produce the same hash as an equivalent
	// flat config.
	params := TemplateParams{
		Command: "claude",
		Prompt:  "base prompt",
		Env:     map[string]string{"KEY": "val"},
		WorkDir: "/work",
	}

	// Overlay overrides command and adds an env var.
	overlay := map[string]string{
		"command":  "gemini",
		"env.TOOL": "hammer",
	}

	overlayHash := canonicalConfigHash(params, overlay)

	// Equivalent flat config (as if overlay was pre-applied).
	flat := TemplateParams{
		Command: "gemini",
		Prompt:  "base prompt",
		Env:     map[string]string{"KEY": "val", "TOOL": "hammer"},
		WorkDir: "/work",
	}
	flatHash := canonicalConfigHash(flat, nil)

	if overlayHash != flatHash {
		t.Errorf("overlay hash %q != flat hash %q", overlayHash, flatHash)
	}
}

func TestConfigHash_DifferentPrompts(t *testing.T) {
	p1 := TemplateParams{Command: "claude", Prompt: "prompt A"}
	p2 := TemplateParams{Command: "claude", Prompt: "prompt B"}

	if canonicalConfigHash(p1, nil) == canonicalConfigHash(p2, nil) {
		t.Error("different prompts should produce different hashes")
	}
}

func TestConfigHash_BeaconTimeStability(t *testing.T) {
	// Two prompts with different beacon timestamps but same content
	// should produce the same hash.
	prompt := "You are a helpful agent.\n\nDo your work."
	p1 := TemplateParams{
		Command: "claude",
		Prompt:  "[bright-lights] worker \u2022 2026-03-08T10:00:00\n\n" + prompt,
	}
	p2 := TemplateParams{
		Command: "claude",
		Prompt:  "[bright-lights] worker \u2022 2026-03-08T11:30:00\n\n" + prompt,
	}

	h1 := canonicalConfigHash(p1, nil)
	h2 := canonicalConfigHash(p2, nil)
	if h1 != h2 {
		t.Errorf("beacon time change should not affect hash: %q vs %q", h1, h2)
	}

	// Hash should match a prompt without any beacon at all.
	p3 := TemplateParams{
		Command: "claude",
		Prompt:  prompt,
	}
	h3 := canonicalConfigHash(p3, nil)
	if h1 != h3 {
		t.Errorf("beacon-stripped hash should match no-beacon hash: %q vs %q", h1, h3)
	}
}

func TestStripBeaconPrefix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"with beacon", "[city] agent \u2022 2026-01-01T00:00:00\n\nreal prompt", "real prompt"},
		{"no beacon", "plain prompt text", "plain prompt text"},
		{"empty", "", ""},
		{"bracket but no newline", "[city] agent", "[city] agent"},
	}
	for _, tt := range tests {
		if got := stripBeaconPrefix(tt.input); got != tt.want {
			t.Errorf("%s: stripBeaconPrefix() = %q, want %q", tt.name, got, tt.want)
		}
	}
}
