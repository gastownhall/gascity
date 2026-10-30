package config

import (
	"slices"
	"strings"
	"testing"
)

// argsAppendBespoke is a provider args_append entry with no OptionsSchema
// equivalent: nothing but the args themselves carries it, so losing it from
// Args loses the setting.
var argsAppendBespoke = []string{"-c", "model_context_window=1000000"}

// argsAppendShapes are the provider layouts an args_append can arrive through.
// Each returns the explicit provider catalog and the provider name an agent
// selects. The same agent-level args override must keep the append in all of
// them.
var argsAppendShapes = []struct {
	name  string
	build func(appendArgs []string) (map[string]ProviderSpec, string)
}{
	{
		name: "leaf over builtin",
		build: func(appendArgs []string) (map[string]ProviderSpec, string) {
			return map[string]ProviderSpec{
				"codex": {Base: basePtr("builtin:codex"), ArgsAppend: appendArgs},
			}, "codex"
		},
	},
	{
		name: "inherited from parent",
		build: func(appendArgs []string) (map[string]ProviderSpec, string) {
			return map[string]ProviderSpec{
				"codex-wrapper": {Base: basePtr("builtin:codex"), ArgsAppend: appendArgs},
				"codex-max":     {Base: basePtr("codex-wrapper")},
			}, "codex-max"
		},
	},
	{
		name: "legacy same-name override",
		build: func(appendArgs []string) (map[string]ProviderSpec, string) {
			return map[string]ProviderSpec{
				"codex": {ArgsAppend: appendArgs},
			}, "codex"
		},
	},
}

func countSequence(args, seq []string) int {
	n := 0
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			n++
		}
	}
	return n
}

func TestResolveProviderAgentArgsOverrideKeepsProviderArgsAppend(t *testing.T) {
	for _, shape := range argsAppendShapes {
		t.Run(shape.name, func(t *testing.T) {
			providers, name := shape.build(argsAppendBespoke)
			agent := &Agent{Name: "scout", Provider: name, Args: []string{"--agent-flag"}}
			rp, err := ResolveProvider(agent, nil, providers, lookPathAll)
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			want := append([]string{"--agent-flag"}, argsAppendBespoke...)
			if !slices.Equal(rp.Args, want) {
				t.Errorf("Args = %v, want the agent's args then the provider's args_append: %v", rp.Args, want)
			}
		})
	}
}

// A [[rigs.patches]] args override is applied to the agent by
// applyAgentOverride, then resolved exactly like a plain agent.toml args, so
// both trigger shapes named in the bug report get their own test.
func TestResolveProviderRigPatchArgsOverrideKeepsProviderArgsAppend(t *testing.T) {
	for _, shape := range argsAppendShapes {
		t.Run(shape.name, func(t *testing.T) {
			providers, name := shape.build(argsAppendBespoke)
			agent := Agent{Name: "scout", Provider: name}
			patched := []string{"--patched-flag"}
			applyAgentOverride(&agent, &AgentOverride{Agent: "scout", Args: &patched})
			rp, err := ResolveProvider(&agent, nil, providers, lookPathAll)
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			want := append([]string{"--patched-flag"}, argsAppendBespoke...)
			if !slices.Equal(rp.Args, want) {
				t.Errorf("Args = %v, want the patched args then the provider's args_append: %v", rp.Args, want)
			}
		})
	}
}

// An agent with no args override must resolve exactly as it did before the
// fix: the provider's own Args with its args_append already folded in, once.
func TestResolveProviderNoAgentArgsLeavesArgsAppendUnchanged(t *testing.T) {
	for _, shape := range argsAppendShapes {
		t.Run(shape.name, func(t *testing.T) {
			baseProviders, baseName := shape.build(nil)
			baseline, err := ResolveProvider(&Agent{Name: "scout", Provider: baseName}, nil, baseProviders, lookPathAll)
			if err != nil {
				t.Fatalf("ResolveProvider baseline: %v", err)
			}
			providers, name := shape.build(argsAppendBespoke)
			rp, err := ResolveProvider(&Agent{Name: "scout", Provider: name}, nil, providers, lookPathAll)
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			want := append(slices.Clone(baseline.Args), argsAppendBespoke...)
			if !slices.Equal(rp.Args, want) {
				t.Errorf("Args = %v, want provider args then args_append once: %v", rp.Args, want)
			}
		})
	}
}

// The pitfall case: args_append mixes a schema-managed flag with a bespoke
// one. The schema-managed value rides EffectiveDefaults and is re-synthesized
// into argv downstream, so copying the raw append back onto Args would emit
// it twice.
func TestResolveProviderAgentArgsOverrideMixedSchemaAndBespokeArgsAppend(t *testing.T) {
	mixed := []string{"-m", "gpt-5.3-codex", "-c", "model_context_window=1000000"}
	for _, shape := range argsAppendShapes {
		t.Run(shape.name, func(t *testing.T) {
			providers, name := shape.build(mixed)
			agent := &Agent{Name: "scout", Provider: name, Args: []string{"--agent-flag"}}
			rp, err := ResolveProvider(agent, nil, providers, lookPathAll)
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			if got := countSequence(rp.Args, argsAppendBespoke); got != 1 {
				t.Errorf("bespoke args_append appears %d times in Args %v, want exactly once", got, rp.Args)
			}
			if got := rp.EffectiveDefaults["model"]; got != "gpt-5.3-codex" {
				t.Errorf("EffectiveDefaults[model] = %q, want gpt-5.3-codex", got)
			}
			for _, tok := range []string{"-m", "gpt-5.3-codex"} {
				if slices.Contains(rp.Args, tok) {
					t.Errorf("Args = %v carries schema-managed token %q a second time; EffectiveDefaults already carries it", rp.Args, tok)
				}
			}
			if !strings.Contains(strings.Join(rp.ResolveDefaultArgs(), " "), "--model gpt-5.3-codex") {
				t.Errorf("ResolveDefaultArgs() = %v, want the model default re-synthesized once", rp.ResolveDefaultArgs())
			}
		})
	}
}

// workspace.start_command is a complete command line: nothing is appended to
// it, with or without an args_append on the provider.
func TestResolveProviderWorkspaceStartCommandIgnoresArgsAppend(t *testing.T) {
	providers, name := argsAppendShapes[0].build(argsAppendBespoke)
	agent := &Agent{Name: "scout", Provider: name, Args: []string{"--agent-flag"}}
	ws := &Workspace{StartCommand: "my-launcher"}
	rp, err := ResolveProvider(agent, ws, providers, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Command != "my-launcher" || rp.Args != nil {
		t.Errorf("Command = %q, Args = %v, want my-launcher with nil Args", rp.Command, rp.Args)
	}
}
