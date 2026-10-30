package builtin

import (
	"slices"
	"testing"
)

// TestBuiltinGrokModelChoicesIncludeGrok46And47 is the grok half of ga-fyh:
// gasburger.refinery and gasburger.gorkcats pin model = "grok-4.6", and the
// catalog must also list grok-4.7 ahead of the next rollout. The option is
// open, so an unlisted id is no longer dropped — but these two are the ids
// this city actually runs, and a curated entry is what keeps them visible in
// pickers and documented in the recipes guide.
func TestBuiltinGrokModelChoicesIncludeGrok46And47(t *testing.T) {
	grok, ok := BuiltinProviders()["grok"]
	if !ok {
		t.Fatal("BuiltinProviders() missing grok")
	}
	for _, value := range []string{"grok-4.6", "grok-4.7"} {
		args := mustChoiceFlagArgs(t, grok, "model", value)
		want := []string{"--model", value}
		if !slices.Equal(args, want) {
			t.Errorf("%s FlagArgs = %v, want %v", value, args, want)
		}
	}
}

// TestGasburgerPackPinsResolveToFlagArgs asserts that every model and effort
// pin used by the shipped gasburger pack resolves to real FlagArgs for its
// provider. This would have caught grok-4.6 on day one.
func TestGasburgerPackPinsResolveToFlagArgs(t *testing.T) {
	providers := BuiltinProviders()
	pins := []struct {
		agent    string
		provider string
		key      string
		value    string
	}{
		{agent: "refinery", provider: "grok", key: "model", value: "grok-4.6"},
		{agent: "refinery", provider: "grok", key: "effort", value: "high"},
		{agent: "gorkcats", provider: "grok", key: "model", value: "grok-4.6"},
		{agent: "gorkcats", provider: "grok", key: "effort", value: "high"},
		{agent: "mayor", provider: "claude", key: "model", value: "opus-5"},
		{agent: "mayor", provider: "claude", key: "effort", value: "medium"},
		{agent: "clodcats", provider: "claude", key: "model", value: "opus-5"},
		{agent: "clodcats", provider: "claude", key: "effort", value: "medium"},
		{agent: "fabcats", provider: "claude", key: "model", value: "fable-5"},
		{agent: "fabcats", provider: "claude", key: "effort", value: "high"},
		{agent: "crew", provider: "claude", key: "model", value: "fable-5"},
		{agent: "crew", provider: "claude", key: "effort", value: "medium"},
		{agent: "kittens", provider: "claude", key: "model", value: "sonnet"},
		{agent: "kittens", provider: "claude", key: "effort", value: "medium"},
		{agent: "boot", provider: "claude", key: "model", value: "sonnet"},
		{agent: "boot", provider: "claude", key: "effort", value: "low"},
		{agent: "deacon", provider: "claude", key: "model", value: "sonnet"},
		{agent: "deacon", provider: "claude", key: "effort", value: "medium"},
		{agent: "dog", provider: "claude", key: "model", value: "sonnet"},
		{agent: "dog", provider: "claude", key: "effort", value: "medium"},
		{agent: "worker", provider: "claude", key: "model", value: "sonnet"},
		{agent: "worker", provider: "claude", key: "effort", value: "medium"},
		{agent: "witness", provider: "claude", key: "model", value: "sonnet"},
		{agent: "witness", provider: "claude", key: "effort", value: "medium"},
	}
	for _, pin := range pins {
		spec, ok := providers[pin.provider]
		if !ok {
			t.Errorf("%s: BuiltinProviders() missing %q", pin.agent, pin.provider)
			continue
		}
		args := mustChoiceFlagArgs(t, spec, pin.key, pin.value)
		if len(args) == 0 {
			t.Errorf("%s: provider %s %s=%q has empty FlagArgs (launch would omit the flag)",
				pin.agent, pin.provider, pin.key, pin.value)
		}
	}
}

func mustChoiceFlagArgs(t *testing.T, spec BuiltinProviderSpec, key, value string) []string {
	t.Helper()
	for _, opt := range spec.OptionsSchema {
		if opt.Key != key {
			continue
		}
		for _, choice := range opt.Choices {
			if choice.Value == value {
				return choice.FlagArgs
			}
		}
		t.Fatalf("provider %q option %q missing choice %q", spec.DisplayName, key, value)
	}
	t.Fatalf("provider %q missing option %q", spec.DisplayName, key)
	return nil
}

// TestEffortChoicesUseCanonicalTiers keeps the effort vocabulary aligned across
// providers. Before ga-fyh each provider hand-maintained its own tier list and
// they drifted apart: claude went to "max", codex stopped at "xhigh", and
// antigravity at "high", so a value that looked blessed by one provider's own
// defaults was silently dropped by another.
func TestEffortChoicesUseCanonicalTiers(t *testing.T) {
	canonical := make(map[string]bool, len(effortTiers))
	for _, tier := range effortTiers {
		canonical[tier] = true
	}
	for name, spec := range BuiltinProviders() {
		for _, opt := range spec.OptionsSchema {
			if opt.Key != "effort" {
				continue
			}
			if len(opt.FlagTemplate) == 0 {
				t.Errorf("provider %q effort option has no FlagTemplate; a tier outside its choices would be dropped", name)
			}
			for _, choice := range opt.Choices {
				if choice.Value == "" {
					continue
				}
				if !canonical[choice.Value] {
					t.Errorf("provider %q effort choice %q is not a canonical tier %v", name, choice.Value, effortTiers)
				}
				if len(choice.FlagArgs) == 0 {
					t.Errorf("provider %q effort choice %q has no FlagArgs", name, choice.Value)
				}
			}
		}
	}
}

// TestEveryDeclaredChoiceRendersFlags is the catalog-wide floor: a declared
// choice that yields no args is a pin the launch path would accept and then
// drop. The empty "Default" entry is the one legitimate no-args choice.
func TestEveryDeclaredChoiceRendersFlags(t *testing.T) {
	// permission_mode "standard" (antigravity) and mcp_approval "prompt"
	// (cursor) intentionally render nothing: they name the CLI's own default
	// behavior rather than a flag.
	noFlagChoices := map[string]bool{
		"antigravity/permission_mode/standard": true,
		"cursor/mcp_approval/prompt":           true,
	}
	for name, spec := range BuiltinProviders() {
		for _, opt := range spec.OptionsSchema {
			for _, choice := range opt.Choices {
				if choice.Value == "" || len(choice.FlagArgs) > 0 {
					continue
				}
				key := name + "/" + opt.Key + "/" + choice.Value
				if noFlagChoices[key] {
					continue
				}
				t.Errorf("%s renders no flags; pinning it silently unpins the agent", key)
			}
		}
	}
}

// TestProviderOwnDefaultsAreDeclared catches the catalog contradicting itself:
// a provider whose own OptionDefaults value is not resolvable ships a latent
// silent unpin for every agent that does not override it.
func TestProviderOwnDefaultsAreDeclared(t *testing.T) {
	for name, spec := range BuiltinProviders() {
		for key, value := range spec.OptionDefaults {
			if value == "" {
				continue
			}
			var opt *BuiltinProviderOption
			for i := range spec.OptionsSchema {
				if spec.OptionsSchema[i].Key == key {
					opt = &spec.OptionsSchema[i]
					break
				}
			}
			if opt == nil {
				t.Errorf("provider %q OptionDefaults %s=%q has no matching schema option", name, key, value)
				continue
			}
			hasChoice := false
			for _, choice := range opt.Choices {
				if choice.Value == value {
					hasChoice = true
					break
				}
			}
			if !hasChoice && len(opt.FlagTemplate) == 0 {
				t.Errorf("provider %q OptionDefaults %s=%q is neither a declared choice nor open", name, key, value)
			}
		}
	}
}
