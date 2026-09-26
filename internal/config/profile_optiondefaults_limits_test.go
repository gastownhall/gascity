package config

import (
	"strings"
	"testing"
)

// Characterization tests for the two known limits of the #5441
// profile-aware OptionDefaults gate (suppressPresetModelEffortForProfile /
// argsRouteThroughProfile in resolve.go). Both are accepted behavior on
// this branch; these tests pin them so a future change to the gate has to
// say so out loud.

// TestProfileGateWrapperOuterProfileFalsePositive: argsRouteThroughProfile
// scans the merged argv flatly - it has no notion of which side of the
// wrapper separator a flag sits on. A wrapper whose OUTER tool takes its
// own --profile therefore trips the gate even though the codex invocation
// past "--" carries no profile at all, and the preset model/effort defaults
// are dropped.
//
// The direction is fail-safe: the launch loses the preset pins and codex
// falls back to its own config, rather than clobbering a profile that does
// not exist. It is recoverable - see the explicit-option_defaults subtest.
func TestProfileGateWrapperOuterProfileFalsePositive(t *testing.T) {
	builtinCodex := "builtin:codex"
	outerProfileWrapper := func(defaults map[string]string) map[string]ProviderSpec {
		return map[string]ProviderSpec{
			"codex-wrapper": {
				Base:    &builtinCodex,
				Command: "aimux",
				// The --profile here belongs to aimux, not to codex:
				// everything past "--" is the codex command line.
				Args:           []string{"--profile", "hardened", "run", "codex", "--"},
				OptionDefaults: defaults,
				ResumeCommand:  "aimux --profile hardened run codex -- resume {{.SessionKey}}",
			},
		}
	}

	t.Run("outer flag suppresses the preset defaults", func(t *testing.T) {
		providers := outerProfileWrapper(nil)
		resolved, err := ResolveProviderChain("codex-wrapper", providers["codex-wrapper"], providers)
		if err != nil {
			t.Fatalf("ResolveProviderChain: %v", err)
		}
		if got := resolved.EffectiveDefaults["model"]; got != "" {
			t.Errorf("EffectiveDefaults[model] = %q, want empty: the outer --profile trips the gate (known limitation)", got)
		}
		if got := resolved.EffectiveDefaults["effort"]; got != "" {
			t.Errorf("EffectiveDefaults[effort] = %q, want empty: the outer --profile trips the gate (known limitation)", got)
		}
		// Fail-safe direction: the permission_mode default is untouched, so
		// the launch is still well formed - it just lost the model pins.
		if got := resolved.EffectiveDefaults["permission_mode"]; got != "unrestricted" {
			t.Errorf("EffectiveDefaults[permission_mode] = %q, want unrestricted", got)
		}
		if line := strings.Join(resolved.ResolveDefaultArgs(), " "); strings.Contains(line, "--model") {
			t.Errorf("ResolveDefaultArgs() = %v, want no --model under the false positive", resolved.ResolveDefaultArgs())
		}
	})

	t.Run("explicit option_defaults recover the pins", func(t *testing.T) {
		providers := outerProfileWrapper(map[string]string{"model": "gpt-5.5", "effort": "xhigh"})
		resolved, err := ResolveProviderChain("codex-wrapper", providers["codex-wrapper"], providers)
		if err != nil {
			t.Fatalf("ResolveProviderChain: %v", err)
		}
		// The gate exempts keys the merging layer set explicitly, so naming
		// them on the wrapper is the documented workaround.
		if got := resolved.EffectiveDefaults["model"]; got != "gpt-5.5" {
			t.Errorf("EffectiveDefaults[model] = %q, want gpt-5.5 (explicit defaults are exempt)", got)
		}
		if got := resolved.EffectiveDefaults["effort"]; got != "xhigh" {
			t.Errorf("EffectiveDefaults[effort] = %q, want xhigh (explicit defaults are exempt)", got)
		}
		if line := strings.Join(resolved.ResolveDefaultArgs(), " "); !strings.Contains(line, "--model gpt-5.5") {
			t.Errorf("ResolveDefaultArgs() = %v, want --model gpt-5.5 restored", resolved.ResolveDefaultArgs())
		}
	})
}

// TestProfileGateAgentArgsBypass: the gate runs during provider-spec
// resolution (lookupProvider / the chain walk), but mergeAgentOverrides
// REPLACES rp.Args wholesale afterwards (resolve.go, mergeAgentOverrides)
// and does not recompute EffectiveDefaults. An agent that supplies the
// --profile in agent.args therefore never reaches the gate: the preset
// model/effort defaults survive and are injected next to the agent's
// --profile - the exact clobber #5441 exists to prevent.
func TestProfileGateAgentArgsBypass(t *testing.T) {
	city := map[string]ProviderSpec{
		"codex": {
			Command:       "codex",
			ResumeCommand: "codex resume {{.SessionKey}}",
		},
	}
	agent := &Agent{
		Name:     "worker",
		Provider: "codex",
		// The provider layer carries no --profile; the agent does.
		Args: []string{"exec", "--profile", "pin-model"},
	}
	resolved, err := ResolveProvider(agent, nil, city, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if !containsArgPair(resolved.Args, []string{"--profile", "pin-model"}) {
		t.Fatalf("Args = %v, want the agent's --profile pin-model to reach the effective args", resolved.Args)
	}
	// Known limitation: the gate never saw these args, so the preset pins
	// are still live and still rendered.
	if got := resolved.EffectiveDefaults["model"]; got == "" {
		t.Errorf("EffectiveDefaults[model] = %q, want the preset model to survive: agent.args bypasses the gate (known limitation)", got)
	}
	if got := resolved.EffectiveDefaults["effort"]; got == "" {
		t.Errorf("EffectiveDefaults[effort] = %q, want the preset effort to survive: agent.args bypasses the gate (known limitation)", got)
	}
	line := strings.Join(resolved.ResolveDefaultArgs(), " ")
	if !strings.Contains(line, "--model") {
		t.Errorf("ResolveDefaultArgs() = %v, want --model still injected alongside the agent's --profile (known limitation)", resolved.ResolveDefaultArgs())
	}
	launch, err := BuildProviderLaunchCommand("", resolved, nil, "tmux")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}
	// The end state the gate is meant to prevent, reached through agent.args:
	// --profile and --model on the same command line.
	if !strings.Contains(launch.Command, "--profile") || !strings.Contains(launch.Command, "--model") {
		t.Errorf("launch command = %q, want both --profile and --model present (the bypass)", launch.Command)
	}
}

// TestProfileGateStaleAfterAgentArgsReplacement is the mirror of the bypass:
// the provider layer routes through a profile (so the gate fired and dropped
// the preset defaults), then agent.args replaces those args with a command
// line that has no --profile. EffectiveDefaults is not recomputed, so the
// launch keeps the suppression it no longer needs. Fail-safe direction, same
// root cause: the gate is a resolution-time decision over args that a later
// layer can still replace.
func TestProfileGateStaleAfterAgentArgsReplacement(t *testing.T) {
	city := map[string]ProviderSpec{
		"codex": {
			Command:       "codex",
			ArgsAppend:    []string{"exec", "--profile", "pin-model"},
			ResumeCommand: "codex resume {{.SessionKey}}",
		},
	}
	agent := &Agent{
		Name:     "worker",
		Provider: "codex",
		Args:     []string{"exec"}, // no --profile any more
	}
	resolved, err := ResolveProvider(agent, nil, city, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if containsArgPair(resolved.Args, []string{"--profile", "pin-model"}) {
		t.Fatalf("Args = %v, want the agent replacement to have dropped --profile", resolved.Args)
	}
	if got := resolved.EffectiveDefaults["model"]; got != "" {
		t.Errorf("EffectiveDefaults[model] = %q, want empty: the gate's decision outlives the args it was made from (known limitation)", got)
	}
}
