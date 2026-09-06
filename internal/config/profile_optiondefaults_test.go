package config

import (
	"reflect"
	"strings"
	"testing"
)

// Tests for the profile-aware preset OptionDefaults gate
// (gastownhall/gascity#5441): when the merged effective args route the
// harness through a codex --profile, the preset's hardcoded model/effort
// OptionDefaults must not be injected as CLI flags, because CLI flags beat
// profile config and would silently clobber the operator's pins for every
// session on the provider. permission_mode is out of scope for suppression.

// profileChainCity builds the canonical leaf -> custom wrapper ->
// builtin:codex chain used by the profile-routed tests.
func profileChainCity(leafArgsAppend []string) map[string]ProviderSpec {
	builtinCodex := "builtin:codex"
	return map[string]ProviderSpec{
		"codex-wrapper": {
			Base:          &builtinCodex,
			Command:       "aimux",
			Args:          []string{"run", "codex", "--"},
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
		"pinned": {
			Base:       basePtr("codex-wrapper"),
			ArgsAppend: leafArgsAppend,
		},
	}
}

func assertProfileSuppressed(t *testing.T, resolved ResolvedProvider) {
	t.Helper()
	if got := resolved.EffectiveDefaults["model"]; got != "" {
		t.Errorf("EffectiveDefaults[model] = %q, want absent (profile pins the model)", got)
	}
	if got := resolved.EffectiveDefaults["effort"]; got != "" {
		t.Errorf("EffectiveDefaults[effort] = %q, want absent (profile pins the effort)", got)
	}
	if got := resolved.EffectiveDefaults["permission_mode"]; got != "unrestricted" {
		t.Errorf("EffectiveDefaults[permission_mode] = %q, want unrestricted (not in scope for suppression)", got)
	}
	defaultLine := strings.Join(resolved.ResolveDefaultArgs(), " ")
	if strings.Contains(defaultLine, "--model") {
		t.Errorf("ResolveDefaultArgs() = %v, must not inject --model for a profile-routed provider", resolved.ResolveDefaultArgs())
	}
	if strings.Contains(defaultLine, "model_reasoning_effort") {
		t.Errorf("ResolveDefaultArgs() = %v, must not inject reasoning-effort for a profile-routed provider", resolved.ResolveDefaultArgs())
	}
	if !strings.Contains(defaultLine, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("ResolveDefaultArgs() = %v, permission_mode default flag must still be injected", resolved.ResolveDefaultArgs())
	}
	assertProfileLaunchCommand(t, &resolved)
}

// assertProfileLaunchCommand checks the launch-command composition path
// (BuildProviderLaunchCommand): the composed command must keep the
// profile flag and the still-defaulted permission_mode args, but must not
// inject the preset --model / reasoning-effort flags.
func assertProfileLaunchCommand(t *testing.T, resolved *ResolvedProvider) {
	t.Helper()
	launch, err := BuildProviderLaunchCommand("", resolved, nil, "tmux")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}
	if strings.Contains(launch.Command, "--model") {
		t.Errorf("launch command = %q, must not inject --model for a profile-routed provider", launch.Command)
	}
	if strings.Contains(launch.Command, "model_reasoning_effort") {
		t.Errorf("launch command = %q, must not inject reasoning-effort for a profile-routed provider", launch.Command)
	}
	if !strings.Contains(launch.Command, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("launch command = %q, permission_mode default flag must still be injected", launch.Command)
	}
	if !strings.Contains(launch.Command, "--profile") {
		t.Errorf("launch command = %q, profile flag must be preserved", launch.Command)
	}
}

// assertNoModelEffortInResumeCommand checks the resume-command completion
// path (completeResumeCommandDefaults -> missingDefaultArgsForCommand): the
// completed template must not gain preset --model / reasoning-effort args.
func assertNoModelEffortInResumeCommand(t *testing.T, rc string) {
	t.Helper()
	if !strings.Contains(rc, "{{.SessionKey}}") {
		t.Fatalf("ResumeCommand = %q, resume completion did not run", rc)
	}
	if strings.Contains(rc, "--model") {
		t.Errorf("ResumeCommand = %q, must not inject --model for a profile-routed provider", rc)
	}
	if strings.Contains(rc, "model_reasoning_effort") {
		t.Errorf("ResumeCommand = %q, must not inject reasoning-effort for a profile-routed provider", rc)
	}
	if !strings.Contains(rc, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("ResumeCommand = %q, permission_mode default must still be injected", rc)
	}
}

func TestProfileRoutedChainSuppressesPresetModelAndEffort(t *testing.T) {
	providers := profileChainCity([]string{"--profile", "pin-model"})
	resolved, err := ResolveProviderChain("pinned", providers["pinned"], providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	// The profile flag keeps flowing into the effective argv.
	if !containsArgPair(resolved.Args, []string{"--profile", "pin-model"}) {
		t.Fatalf("Args = %v, want --profile pin-model preserved in effective args", resolved.Args)
	}
	assertProfileSuppressed(t, resolved)
	// Resume command path: the wrapper's resume template is inherited and
	// completed from the gated effective defaults.
	assertNoModelEffortInResumeCommand(t, resolved.ResumeCommand)
	// Resume command path with a schema-flag override: the substitution
	// path must apply the override without re-introducing preset
	// model/effort flags.
	resume, err := BuildProviderResumeCommand(&resolved, map[string]string{"permission_mode": "suggest"})
	if err != nil {
		t.Fatalf("BuildProviderResumeCommand: %v", err)
	}
	if !strings.Contains(resume, "--ask-for-approval") {
		t.Errorf("resume command = %q, want the permission_mode override applied", resume)
	}
	if strings.Contains(resume, "--model") || strings.Contains(resume, "model_reasoning_effort") {
		t.Errorf("resume command = %q, must not inject preset --model / reasoning-effort", resume)
	}
}

func TestProfileEqualsFormSuppressesPresetModelAndEffort(t *testing.T) {
	providers := profileChainCity([]string{"--profile=pin-model"})
	resolved, err := ResolveProviderChain("pinned", providers["pinned"], providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	if !containsArgPair(resolved.Args, []string{"--profile=pin-model"}) {
		t.Fatalf("Args = %v, want --profile=pin-model preserved in effective args", resolved.Args)
	}
	assertProfileSuppressed(t, resolved)
	assertNoModelEffortInResumeCommand(t, resolved.ResumeCommand)
}

// TestProfileRoutedLegacyProviderSuppressesPresetModelAndEffort is the exact
// issue reproduction shape: a city provider with no declared base whose name
// matches the built-in (Phase A legacy merge path in lookupProvider) and no
// option_defaults of its own, so the gate must apply to the inherited
// preset defaults unconditionally after the merge.
func TestProfileRoutedLegacyProviderSuppressesPresetModelAndEffort(t *testing.T) {
	city := map[string]ProviderSpec{
		"codex": {
			Command:       "codex",
			ArgsAppend:    []string{"exec", "--profile", "pin-model"},
			ResumeCommand: "codex resume {{.SessionKey}}",
		},
	}
	agent := &Agent{Name: "worker", Provider: "codex"}
	resolved, err := ResolveProvider(agent, nil, city, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if want := []string{"exec", "--profile", "pin-model"}; !reflect.DeepEqual(resolved.Args, want) {
		t.Fatalf("Args = %v, want %v", resolved.Args, want)
	}
	assertProfileSuppressed(t, *resolved)
	// Resume command path: completion runs because the agent sets none.
	assertNoModelEffortInResumeCommand(t, resolved.ResumeCommand)
}

// TestNonProfileCodexProviderKeepsPresetDefaults is the no-regression guard:
// a codex-derived provider that does NOT route through a profile keeps the
// preset model/effort defaults on every consumer path.
func TestNonProfileCodexProviderKeepsPresetDefaults(t *testing.T) {
	builtinCodex := "builtin:codex"
	providers := map[string]ProviderSpec{
		"codex-wrapper": {
			Base:          &builtinCodex,
			Command:       "aimux",
			Args:          []string{"run", "codex", "--"},
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	resolved, err := ResolveProviderChain("codex-wrapper", providers["codex-wrapper"], providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	if got := resolved.EffectiveDefaults["model"]; got != "gpt-5.5" {
		t.Errorf("EffectiveDefaults[model] = %q, want gpt-5.5 (preset default kept without --profile)", got)
	}
	if got := resolved.EffectiveDefaults["effort"]; got != "xhigh" {
		t.Errorf("EffectiveDefaults[effort] = %q, want xhigh (preset default kept without --profile)", got)
	}
	line := strings.Join(resolved.ResolveDefaultArgs(), " ")
	if !strings.Contains(line, "--model gpt-5.5") {
		t.Errorf("ResolveDefaultArgs() = %v, want --model gpt-5.5", resolved.ResolveDefaultArgs())
	}
	if !strings.Contains(line, "-c model_reasoning_effort=xhigh") {
		t.Errorf("ResolveDefaultArgs() = %v, want -c model_reasoning_effort=xhigh", resolved.ResolveDefaultArgs())
	}
	// Resume completion keeps injecting the preset defaults too.
	rc := resolved.ResumeCommand
	if !strings.Contains(rc, "--model gpt-5.5") || !strings.Contains(rc, "model_reasoning_effort=xhigh") {
		t.Errorf("ResumeCommand = %q, want preset --model and reasoning-effort defaults injected", rc)
	}
	// Launch command path keeps injecting the preset defaults too.
	launch, err := BuildProviderLaunchCommand("", &resolved, nil, "tmux")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}
	if !strings.Contains(launch.Command, "--model gpt-5.5") || !strings.Contains(launch.Command, "-c model_reasoning_effort=xhigh") {
		t.Errorf("launch command = %q, want preset --model and reasoning-effort defaults injected", launch.Command)
	}
}

// TestExplicitOptionDefaultsStillWin pins that the gate does not flatten
// the normal precedence layers: a provider-level explicit pin still beats
// the preset, an agent-level explicit pin still beats the provider, and an
// agent-level pin on a profile-routed provider (applied after the gate)
// still materializes — only the *inherited preset* model/effort are
// suppressed.
func TestExplicitOptionDefaultsStillWin(t *testing.T) {
	builtinCodex := "builtin:codex"
	// (a) Provider-level explicit pin beats the preset (no profile).
	providers := map[string]ProviderSpec{
		"codex-fast": {
			Base:           &builtinCodex,
			Command:        "aimux",
			Args:           []string{"run", "codex", "--"},
			ResumeCommand:  "aimux run codex -- resume {{.SessionKey}}",
			OptionDefaults: map[string]string{"model": "gpt-5.3-codex"},
		},
	}
	resolved, err := ResolveProviderChain("codex-fast", providers["codex-fast"], providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	if got := resolved.EffectiveDefaults["model"]; got != "gpt-5.3-codex" {
		t.Errorf("EffectiveDefaults[model] = %q, want gpt-5.3-codex (explicit provider pin wins over preset)", got)
	}

	// (b) Agent-level explicit pin beats the provider's (no profile).
	agent := &Agent{Name: "worker", Provider: "codex-fast", OptionDefaults: map[string]string{"model": "o3"}}
	resolvedB, err := ResolveProvider(agent, nil, providers, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if got := resolvedB.EffectiveDefaults["model"]; got != "o3" {
		t.Errorf("EffectiveDefaults[model] = %q, want o3 (explicit agent pin wins)", got)
	}

	// (c) Agent-level explicit pin on a profile-routed provider: applied
	// after the gate, so it still materializes.
	profileCity := map[string]ProviderSpec{
		"pinned": {
			Command:       "codex",
			ArgsAppend:    []string{"exec", "--profile", "pin-model"},
			ResumeCommand: "codex resume {{.SessionKey}}",
		},
	}
	agent = &Agent{Name: "worker", Provider: "pinned", OptionDefaults: map[string]string{"model": "o3"}}
	resolvedC, err := ResolveProvider(agent, nil, profileCity, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if got := resolvedC.EffectiveDefaults["model"]; got != "o3" {
		t.Errorf("EffectiveDefaults[model] = %q, want o3 (explicit agent pin survives profile suppression)", got)
	}
	if got := resolvedC.EffectiveDefaults["effort"]; got != "" {
		t.Errorf("EffectiveDefaults[effort] = %q, want absent (preset effort still suppressed)", got)
	}
}

// TestIntermediateLayerModelPinSuppressedByLeafProfile pins the documented
// caveat: in a leaf -> custom -> builtin chain, an intermediate layer's
// explicit model pin is folded into the merged OptionDefaults before the
// profile-carrying leaf merges over it, so it is suppressed along with the
// preset default. That is acceptable: the profile is the operator's
// explicit model pin and should win.
func TestIntermediateLayerModelPinSuppressedByLeafProfile(t *testing.T) {
	builtinCodex := "builtin:codex"
	providers := map[string]ProviderSpec{
		"codex-pinned-mid": {
			Base:           &builtinCodex,
			Command:        "aimux",
			Args:           []string{"run", "codex", "--"},
			ResumeCommand:  "aimux run codex -- resume {{.SessionKey}}",
			OptionDefaults: map[string]string{"model": "gpt-5.3-codex"},
		},
		"pinned": {
			Base:       basePtr("codex-pinned-mid"),
			ArgsAppend: []string{"--profile", "fast"},
		},
	}
	resolved, err := ResolveProviderChain("pinned", providers["pinned"], providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	if got := resolved.EffectiveDefaults["model"]; got != "" {
		t.Errorf("EffectiveDefaults[model] = %q, want absent (intermediate pin suppressed by leaf --profile, known caveat)", got)
	}
	assertNoModelEffortInResumeCommand(t, resolved.ResumeCommand)
}

// TestArgsRouteThroughProfile pins the profile-routing detector: only a
// "--profile" token followed by a non-empty value (space form) or a
// "--profile=<name>" token with a non-empty name counts as routed. A bare
// or empty-valued flag is not routed (the command is malformed), and
// short-form or lookalike flags are out of scope.
func TestArgsRouteThroughProfile(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"space form", []string{"exec", "--profile", "pin-model"}, true},
		{"space form mid-args", []string{"run", "codex", "--", "--profile", "fast"}, true},
		{"equals form", []string{"exec", "--profile=pin-model"}, true},
		{"both forms", []string{"exec", "--profile", "a", "--profile=b"}, true},
		{"bare no value", []string{"exec", "--profile"}, false},
		{"bare at end", []string{"--profile"}, false},
		{"empty equals value", []string{"exec", "--profile="}, false},
		{"short form not routed", []string{"exec", "-p", "x"}, false},
		{"lookalike flag not routed", []string{"exec", "--profilex", "y"}, false},
		{"nil args", nil, false},
		{"empty args", []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argsRouteThroughProfile(tc.args); got != tc.want {
				t.Errorf("argsRouteThroughProfile(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
