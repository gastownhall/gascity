package config

import (
	"testing"
)

func TestProviderProvenance_TwoLayerChain(t *testing.T) {
	b := "builtin:codex"
	city := map[string]ProviderSpec{
		"codex-max": {
			Base:          &b,
			Command:       "aimux",
			Args:          []string{"run", "codex"},
			ReadyDelayMs:  5000,
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	r, err := ResolveProviderChain("codex-max", city["codex-max"], city)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Leaf (custom) contributes Command, Args, ReadyDelayMs, ResumeCommand.
	if got := r.Provenance.FieldLayer["command"]; got != "providers.codex-max" {
		t.Errorf("command layer = %q, want providers.codex-max", got)
	}
	if got := r.Provenance.FieldLayer["ready_delay_ms"]; got != "providers.codex-max" {
		t.Errorf("ready_delay_ms layer = %q, want providers.codex-max", got)
	}

	// Built-in codex contributes scalars the leaf didn't override
	// (prompt_mode, resume_flag, resume_style, title_model).
	if got := r.Provenance.FieldLayer["prompt_mode"]; got != "builtin:codex" {
		t.Errorf("prompt_mode layer = %q, want builtin:codex", got)
	}
	if got := r.Provenance.FieldLayer["title_model"]; got != "builtin:codex" {
		t.Errorf("title_model layer = %q, want builtin:codex", got)
	}
}

func TestProviderProvenance_MapKeyAttribution(t *testing.T) {
	b := "builtin:codex"
	city := map[string]ProviderSpec{
		"codex-max": {
			Base: &b,
			// Leaf adds its own effort override; permission_mode stays
			// inherited from builtin.
			OptionDefaults: map[string]string{
				"effort": "xhigh-leaf",
			},
			Command:       "aimux",
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	r, err := ResolveProviderChain("codex-max", city["codex-max"], city)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	optKeys := r.Provenance.MapKeyLayer["option_defaults"]
	if optKeys == nil {
		t.Fatal("option_defaults provenance missing")
	}
	if got := optKeys["effort"]; got != "providers.codex-max" {
		t.Errorf("option_defaults[effort] layer = %q, want providers.codex-max", got)
	}
	if got := optKeys["permission_mode"]; got != "builtin:codex" {
		t.Errorf("option_defaults[permission_mode] layer = %q, want builtin:codex", got)
	}

	// PermissionModes keys come entirely from the built-in since leaf
	// didn't declare any.
	pm := r.Provenance.MapKeyLayer["permission_modes"]
	if pm == nil {
		t.Fatal("permission_modes provenance missing")
	}
	if got := pm["unrestricted"]; got != "builtin:codex" {
		t.Errorf("permission_modes[unrestricted] layer = %q, want builtin:codex", got)
	}
}

func TestProviderProvenance_ChainPopulated(t *testing.T) {
	b := "builtin:codex"
	r, err := ResolveProviderChain("foo", ProviderSpec{
		Base:          &b,
		Command:       "aimux",
		ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
	}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(r.Provenance.Chain) != 2 {
		t.Errorf("Chain len = %d, want 2", len(r.Provenance.Chain))
	}
}

func TestProviderProvenance_NoInheritance(t *testing.T) {
	r, err := ResolveProviderChain("stand-alone", ProviderSpec{Command: "vim"}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := r.Provenance.FieldLayer["command"]; got != "providers.stand-alone" {
		t.Errorf("command layer = %q, want providers.stand-alone", got)
	}
	// No built-in hop → no fields attributed to builtin:*.
	for field, layer := range r.Provenance.FieldLayer {
		if layer == BasePrefixBuiltin+"codex" || layer == BasePrefixBuiltin+"claude" {
			t.Errorf("unexpected built-in attribution for %q: %q", field, layer)
		}
	}
}
