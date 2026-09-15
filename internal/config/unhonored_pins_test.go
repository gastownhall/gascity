package config

import (
	"slices"
	"strings"
	"testing"
)

func TestBuiltinProviderOptionDefaultsAreHonored(t *testing.T) {
	for name, spec := range BuiltinProviders() {
		rp := &ResolvedProvider{
			Name:              name,
			OptionsSchema:     spec.OptionsSchema,
			EffectiveDefaults: ComputeEffectiveDefaults(spec.OptionsSchema, spec.OptionDefaults, nil),
		}
		if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
			t.Errorf("provider %q builtin defaults are unhonored: %+v", name, pins)
		}
	}
}

func TestGrok46AgentPinEmitsModelFlag(t *testing.T) {
	grok := BuiltinProviders()["grok"]
	rp := &ResolvedProvider{
		Name:          "grok",
		OptionsSchema: grok.OptionsSchema,
		EffectiveDefaults: ComputeEffectiveDefaults(grok.OptionsSchema, grok.OptionDefaults, map[string]string{
			"model":  "grok-4.6",
			"effort": "high",
		}),
	}
	if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
		t.Fatalf("gasburger grok-4.6 pin unhonored: %+v", pins)
	}
	args := rp.ResolveDefaultArgs()
	if !containsFlagValue(args, "--model", "grok-4.6") {
		t.Errorf("ResolveDefaultArgs() = %v, want --model grok-4.6", args)
	}
	if !containsFlagValue(args, "--effort", "high") {
		t.Errorf("ResolveDefaultArgs() = %v, want --effort high", args)
	}
}

// TestUnhonoredOptionPinsUnknownValue covers a CLOSED option: permission_mode
// is a fixed set the CLI publishes, so a value outside it cannot be rendered
// and must be reported rather than silently dropped.
func TestUnhonoredOptionPinsUnknownValue(t *testing.T) {
	grok := BuiltinProviders()["grok"]
	rp := &ResolvedProvider{
		Name:              "grok",
		OptionsSchema:     grok.OptionsSchema,
		EffectiveDefaults: map[string]string{"permission_mode": "not-a-mode"},
	}
	pins := rp.UnhonoredOptionPins()
	if len(pins) != 1 {
		t.Fatalf("UnhonoredOptionPins() = %+v, want one unknown_value pin", pins)
	}
	if pins[0].Key != "permission_mode" || pins[0].Value != "not-a-mode" || pins[0].Reason != UnhonoredPinUnknownValue {
		t.Errorf("pin = %+v, want permission_mode=not-a-mode unknown_value", pins[0])
	}
	if !slices.Contains(pins[0].Valid, "bypassPermissions") && !slices.Contains(pins[0].Valid, "unrestricted") {
		t.Errorf("valid set %v does not name the real choices", pins[0].Valid)
	}
	if args := rp.ResolveDefaultArgs(); len(args) != 0 {
		t.Errorf("ResolveDefaultArgs() emitted flags for an unhonorable pin: %v", args)
	}
	warn := FormatUnhonoredOptionPin("gascity/gasburger.gorkcats", "grok", pins[0])
	for _, want := range []string{
		"WARNING:",
		`agent "gascity/gasburger.gorkcats"`,
		`permission_mode="not-a-mode"`,
		`provider "grok"`,
		"flag omitted",
	} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning %q missing %q", warn, want)
		}
	}
}

// TestOpenModelPinIsHonoredForEveryProvider is the systemic half of ga-fyh.
// Model ids move faster than this catalog: the catalog carried no grok-4.x id
// for over two months while gasburger pinned grok-4.6, and the launch path
// answered that by emitting no --model at all. Every provider that exposes a
// model option must honor an id it has never heard of, so the next id cannot
// reopen the same hole.
func TestOpenModelPinIsHonoredForEveryProvider(t *testing.T) {
	const unheardOf = "some-model-shipped-after-this-release"
	checked := 0
	for name, spec := range BuiltinProviders() {
		opt := findOption(spec.OptionsSchema, "model")
		if opt == nil {
			continue
		}
		checked++
		t.Run(name, func(t *testing.T) {
			if !IsOpenOption(*opt) {
				t.Fatalf("provider %q model option is closed; an id outside its enum would be silently dropped", name)
			}
			rp := &ResolvedProvider{
				Name:              name,
				OptionsSchema:     spec.OptionsSchema,
				EffectiveDefaults: map[string]string{"model": unheardOf},
			}
			if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
				t.Fatalf("unheard-of model reported unhonored: %+v", pins)
			}
			if args := rp.ResolveDefaultArgs(); !containsFlagValue(args, "--model", unheardOf) {
				t.Errorf("ResolveDefaultArgs() = %v, want --model %s", args, unheardOf)
			}
		})
	}
	if checked == 0 {
		t.Fatal("no provider exposes a model option")
	}
}

// TestOpenEffortPinIsHonoredForEveryProvider is the effort half. "max" is a
// canonical tier that claude's own defaults use, and pinning it on codex used
// to be dropped on the floor because that enum stopped at xhigh — the value
// looks blessed and was not portable (ga-fyh addendum).
func TestOpenEffortPinIsHonoredForEveryProvider(t *testing.T) {
	for name, spec := range BuiltinProviders() {
		opt := findOption(spec.OptionsSchema, "effort")
		if opt == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if !IsOpenOption(*opt) {
				t.Fatalf("provider %q effort option is closed", name)
			}
			for _, tier := range []string{"low", "medium", "high", "xhigh", "max"} {
				rp := &ResolvedProvider{
					Name:              name,
					OptionsSchema:     spec.OptionsSchema,
					EffectiveDefaults: map[string]string{"effort": tier},
				}
				if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
					t.Errorf("effort=%q unhonored: %+v", tier, pins)
					continue
				}
				if args := rp.ResolveDefaultArgs(); len(args) == 0 {
					t.Errorf("effort=%q produced no flags", tier)
				}
			}
		})
	}
}

// TestProviderWithoutOptionWarnsOnPin pins the other half of the invariant: a
// provider that genuinely has no such concept must report the pin, not no-op.
// Most providers expose no effort option, and a handful (amp, auggie, copilot,
// kiro, omp, zcode) expose neither — their CLIs are not installable here to
// confirm a flag shape, and inventing one would emit a flag the CLI rejects.
// Unsupported is therefore explicit and loud rather than silent: every such
// pin lands here as an unknown_option warning naming the agent and value.
func TestProviderWithoutOptionWarnsOnPin(t *testing.T) {
	for name, spec := range BuiltinProviders() {
		for _, key := range []string{"model", "effort"} {
			if findOption(spec.OptionsSchema, key) != nil {
				continue
			}
			rp := &ResolvedProvider{
				Name:              name,
				OptionsSchema:     spec.OptionsSchema,
				EffectiveDefaults: map[string]string{key: "high"},
			}
			pins := rp.UnhonoredOptionPins()
			if len(pins) != 1 || pins[0].Key != key || pins[0].Reason != UnhonoredPinUnknownOption {
				t.Errorf("provider %q: UnhonoredOptionPins() = %+v, want one %s unknown_option pin", name, pins, key)
				continue
			}
			warn := FormatUnhonoredOptionPin("gascity/someagent", name, pins[0])
			for _, want := range []string{"WARNING:", `agent "gascity/someagent"`, key} {
				if !strings.Contains(warn, want) {
					t.Errorf("provider %q %s warning %q missing %q", name, key, warn, want)
				}
			}
		}
	}
}

func TestUnhonoredOptionPinsUnknownKey(t *testing.T) {
	gemini := BuiltinProviders()["gemini"]
	rp := &ResolvedProvider{
		Name:          "gemini",
		OptionsSchema: gemini.OptionsSchema,
		EffectiveDefaults: map[string]string{
			"permission_mode": "unrestricted",
			"effort":          "high",
		},
	}
	pins := rp.UnhonoredOptionPins()
	if len(pins) != 1 || pins[0].Key != "effort" || pins[0].Reason != UnhonoredPinUnknownOption {
		t.Fatalf("UnhonoredOptionPins() = %+v, want effort unknown_option", pins)
	}
	warn := FormatUnhonoredOptionPin("reviewer", "gemini", pins[0])
	if !strings.Contains(warn, `effort="high"`) || !strings.Contains(warn, "not in the") {
		t.Errorf("warning %q", warn)
	}
}

func TestGasburgerPackPinsResolveDefaultArgs(t *testing.T) {
	type pin struct {
		provider string
		key      string
		value    string
	}
	pins := []pin{
		{provider: "grok", key: "model", value: "grok-4.6"},
		{provider: "grok", key: "effort", value: "high"},
		{provider: "claude", key: "model", value: "opus-5"},
		{provider: "claude", key: "effort", value: "medium"},
		{provider: "claude", key: "model", value: "fable-5"},
		{provider: "claude", key: "effort", value: "high"},
		{provider: "claude", key: "model", value: "sonnet"},
		{provider: "claude", key: "effort", value: "low"},
	}
	builtins := BuiltinProviders()
	for _, p := range pins {
		spec, ok := builtins[p.provider]
		if !ok {
			t.Fatalf("missing provider %q", p.provider)
		}
		rp := &ResolvedProvider{
			Name:          p.provider,
			OptionsSchema: spec.OptionsSchema,
			EffectiveDefaults: ComputeEffectiveDefaults(spec.OptionsSchema, spec.OptionDefaults, map[string]string{
				p.key: p.value,
			}),
		}
		if unhonored := rp.UnhonoredOptionPins(); len(unhonored) > 0 {
			t.Errorf("%s %s=%q unhonored: %+v", p.provider, p.key, p.value, unhonored)
			continue
		}
		args := rp.ResolveDefaultArgs()
		if len(args) == 0 {
			t.Errorf("%s %s=%q produced no FlagArgs", p.provider, p.key, p.value)
		}
	}
}

func containsFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestBuiltinTemplatesUseConfigPlaceholder pins the duplicated placeholder
// token. internal/worker/builtin sits below this package and cannot import it,
// so it declares its own copy; if the two ever diverge, templates would render
// a literal placeholder onto the command line.
func TestBuiltinTemplatesUseConfigPlaceholder(t *testing.T) {
	found := false
	for name, spec := range BuiltinProviders() {
		for _, opt := range spec.OptionsSchema {
			if len(opt.FlagTemplate) == 0 {
				continue
			}
			found = true
			hasPlaceholder := false
			for _, tok := range opt.FlagTemplate {
				if strings.Contains(tok, OptionValuePlaceholder) {
					hasPlaceholder = true
				}
			}
			if !hasPlaceholder {
				t.Errorf("provider %q option %q template %v contains no %q", name, opt.Key, opt.FlagTemplate, OptionValuePlaceholder)
			}
		}
	}
	if !found {
		t.Fatal("no builtin provider declares a flag template")
	}
}

// TestCodexMaxEffortPinIsHonored is the concrete addendum case: codex's enum
// stopped at xhigh, so effort = "max" — a tier claude's own OptionDefaults
// use — was silently discarded on a codex agent.
func TestCodexMaxEffortPinIsHonored(t *testing.T) {
	codex := BuiltinProviders()["codex"]
	rp := &ResolvedProvider{
		Name:              "codex",
		OptionsSchema:     codex.OptionsSchema,
		EffectiveDefaults: ComputeEffectiveDefaults(codex.OptionsSchema, codex.OptionDefaults, map[string]string{"effort": "max"}),
	}
	if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
		t.Fatalf("codex effort=max unhonored: %+v", pins)
	}
	args := rp.ResolveDefaultArgs()
	if !containsFlagValue(args, "-c", "model_reasoning_effort=max") {
		t.Errorf("ResolveDefaultArgs() = %v, want -c model_reasoning_effort=max", args)
	}
}

// TestCursorModelPinIsHonored covers a provider that had no model option at
// all: every cursor model pin was dropped as an unknown key (ga-fyh addendum
// listed seven such providers). cursor-agent's catalog is account-scoped and
// includes parameterized bracket ids, so the option is open by necessity.
func TestCursorModelPinIsHonored(t *testing.T) {
	cursor := BuiltinProviders()["cursor"]
	for _, model := range []string{"auto", "claude-opus-4-8[context=1m,effort=high]"} {
		rp := &ResolvedProvider{
			Name:              "cursor",
			OptionsSchema:     cursor.OptionsSchema,
			EffectiveDefaults: map[string]string{"model": model},
		}
		if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
			t.Errorf("cursor model=%q unhonored: %+v", model, pins)
			continue
		}
		if args := rp.ResolveDefaultArgs(); !containsFlagValue(args, "--model", model) {
			t.Errorf("ResolveDefaultArgs() = %v, want --model %s", args, model)
		}
	}
}
