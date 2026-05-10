package config

import (
	"reflect"
	"testing"
)

func TestBuiltinProviders(t *testing.T) {
	providers := BuiltinProviders()
	order := BuiltinProviderOrder()

	// Must have exactly 11 built-in providers.
	if len(providers) != 11 {
		t.Fatalf("len(BuiltinProviders()) = %d, want 11", len(providers))
	}
	if len(order) != 11 {
		t.Fatalf("len(BuiltinProviderOrder()) = %d, want 11", len(order))
	}

	// Every entry in order must exist in providers.
	for _, name := range order {
		p, ok := providers[name]
		if !ok {
			t.Errorf("BuiltinProviders() missing %q", name)
			continue
		}
		if p.Command == "" {
			t.Errorf("provider %q has empty Command", name)
		}
		if p.DisplayName == "" {
			t.Errorf("provider %q has empty DisplayName", name)
		}
	}

	// Every provider must be in order.
	for name := range providers {
		found := false
		for _, o := range order {
			if o == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("provider %q not in BuiltinProviderOrder()", name)
		}
	}
}

func TestBuiltinProvidersClaude(t *testing.T) {
	p := BuiltinProviders()["claude"]
	if p.Command != "claude" {
		t.Errorf("Command = %q, want %q", p.Command, "claude")
	}
	// Args is nil -- schema-managed flags moved to OptionDefaults.
	if p.Args != nil {
		t.Errorf("Args = %v, want nil (schema flags moved to OptionDefaults)", p.Args)
	}
	if p.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("OptionDefaults[permission_mode] = %q, want unrestricted", p.OptionDefaults["permission_mode"])
	}
	if p.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "arg")
	}
	if p.ReadyDelayMs != 10000 {
		t.Errorf("ReadyDelayMs = %d, want 10000", p.ReadyDelayMs)
	}
	if !derefBool(p.EmitsPermissionWarning) {
		t.Error("EmitsPermissionWarning = false, want true")
	}
}

func TestBuiltinClaudeCommandString(t *testing.T) {
	// After migration, claude's Args is nil. CommandString() returns just "claude".
	// Schema-managed flags come from ResolveDefaultArgs() instead.
	p := BuiltinProviders()["claude"]
	rp := &ResolvedProvider{
		Command:           p.Command,
		Args:              p.Args,
		OptionsSchema:     p.OptionsSchema,
		EffectiveDefaults: ComputeEffectiveDefaults(p.OptionsSchema, p.OptionDefaults, nil),
	}
	cs := rp.CommandString()
	if cs != "claude" {
		t.Errorf("CommandString() = %q, want %q", cs, "claude")
	}
	// Default args should produce the permission flag and effort flag.
	defaultArgs := rp.ResolveDefaultArgs()
	wantArgs := []string{"--dangerously-skip-permissions", "--effort", "max"}
	if len(defaultArgs) != len(wantArgs) {
		t.Errorf("ResolveDefaultArgs() = %v, want %v", defaultArgs, wantArgs)
	} else {
		for i, w := range wantArgs {
			if defaultArgs[i] != w {
				t.Errorf("ResolveDefaultArgs()[%d] = %q, want %q", i, defaultArgs[i], w)
			}
		}
	}
}

func TestBuiltinProvidersCodex(t *testing.T) {
	p := BuiltinProviders()["codex"]
	if p.Command != "codex" {
		t.Errorf("Command = %q, want %q", p.Command, "codex")
	}
	if p.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "arg")
	}
	if p.ReadyDelayMs != 3000 {
		t.Errorf("ReadyDelayMs = %d, want 3000", p.ReadyDelayMs)
	}
	if derefBool(p.EmitsPermissionWarning) {
		t.Error("EmitsPermissionWarning = true, want false")
	}
}

func TestBuiltinProvidersGemini(t *testing.T) {
	p := BuiltinProviders()["gemini"]
	if p.Command != "gemini" {
		t.Errorf("Command = %q, want %q", p.Command, "gemini")
	}
	// Args is nil -- schema-managed flags moved to OptionDefaults.
	if p.Args != nil {
		t.Errorf("Args = %v, want nil (schema flags moved to OptionDefaults)", p.Args)
	}
	if p.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("OptionDefaults[permission_mode] = %q, want unrestricted", p.OptionDefaults["permission_mode"])
	}
	if p.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "arg")
	}
	if p.ReadyPromptPrefix != "> " {
		t.Errorf("ReadyPromptPrefix = %q, want %q", p.ReadyPromptPrefix, "> ")
	}
	if p.ReadyDelayMs != 5000 {
		t.Errorf("ReadyDelayMs = %d, want 5000", p.ReadyDelayMs)
	}
	if len(p.ProcessNames) != 2 || p.ProcessNames[0] != "gemini" || p.ProcessNames[1] != "node" {
		t.Errorf("ProcessNames = %v, want [gemini node]", p.ProcessNames)
	}
}

func TestBuiltinProvidersCursor(t *testing.T) {
	p := BuiltinProviders()["cursor"]
	if p.Command != "cursor-agent" {
		t.Errorf("Command = %q, want %q", p.Command, "cursor-agent")
	}
	if len(p.Args) != 1 || p.Args[0] != "-f" {
		t.Errorf("Args = %v, want [-f]", p.Args)
	}
	if p.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "arg")
	}
	if p.ReadyPromptPrefix != "\u2192 " {
		t.Errorf("ReadyPromptPrefix = %q, want %q", p.ReadyPromptPrefix, "\u2192 ")
	}
	if p.ReadyDelayMs != 10000 {
		t.Errorf("ReadyDelayMs = %d, want 10000", p.ReadyDelayMs)
	}
	if len(p.ProcessNames) != 1 || p.ProcessNames[0] != "cursor-agent" {
		t.Errorf("ProcessNames = %v, want [cursor-agent]", p.ProcessNames)
	}
	if !derefBool(p.SupportsHooks) {
		t.Error("SupportsHooks = false, want true")
	}
	if p.InstructionsFile != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q, want %q", p.InstructionsFile, "AGENTS.md")
	}
}

func TestBuiltinProvidersReturnsNewMap(t *testing.T) {
	a := BuiltinProviders()
	b := BuiltinProviders()
	a["claude"] = ProviderSpec{Command: "mutated"}
	if b["claude"].Command == "mutated" {
		t.Error("BuiltinProviders() should return a new map each time")
	}
}

// TestBuiltinProvidersOpenCode verifies the opencode provider keeps startup
// instructions out of bare argv. OpenCode treats positional prompt payloads as
// project paths in TUI mode, so tmux startup delivery must use --prompt.
func TestBuiltinProvidersOpenCode(t *testing.T) {
	p := BuiltinProviders()["opencode"]
	if p.Command != "opencode" {
		t.Errorf("Command = %q, want %q", p.Command, "opencode")
	}
	if p.ACPCommand != "" {
		t.Errorf("ACPCommand = %q, want empty fallback to Command", p.ACPCommand)
	}
	if !reflect.DeepEqual(p.ACPArgs, []string{"acp"}) {
		t.Errorf("ACPArgs = %v, want [acp]", p.ACPArgs)
	}
	if p.PromptMode != "flag" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "flag")
	}
	if p.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want --prompt", p.PromptFlag)
	}
	if !derefBool(p.SupportsHooks) {
		t.Error("SupportsHooks = false, want true")
	}
	if !derefBool(p.SupportsACP) {
		t.Error("SupportsACP = false, want true")
	}
	if p.InstructionsFile != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q, want %q", p.InstructionsFile, "AGENTS.md")
	}
	if p.ResumeFlag != "--session" {
		t.Errorf("ResumeFlag = %q, want --session", p.ResumeFlag)
	}
	if p.ResumeStyle != "flag" {
		t.Errorf("ResumeStyle = %q, want flag", p.ResumeStyle)
	}
	if p.ReadyDelayMs != 8000 {
		t.Errorf("ReadyDelayMs = %d, want 8000", p.ReadyDelayMs)
	}
}

func TestBuiltinProvidersKiro(t *testing.T) {
	p := BuiltinProviders()["kiro"]
	if p.Command != "kiro-cli" {
		t.Errorf("Command = %q, want %q", p.Command, "kiro-cli")
	}
	if !reflect.DeepEqual(p.Args, []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"}) {
		t.Errorf("Args = %v, want [chat --no-interactive --agent gascity --trust-all-tools]", p.Args)
	}
	if !reflect.DeepEqual(p.ACPArgs, []string{"acp", "--agent", "gascity"}) {
		t.Errorf("ACPArgs = %v, want [acp --agent gascity]", p.ACPArgs)
	}
	if !derefBool(p.SupportsACP) {
		t.Error("SupportsACP = false, want true")
	}
	if !derefBool(p.SupportsHooks) {
		t.Error("SupportsHooks = false, want true")
	}
}

// TestBuiltinProvidersOpenCodePromptModeRegression guards against switching
// OpenCode back to argv-based prompt delivery. Gas City renders the startup
// prompt as startup material, so OpenCode must not receive it as a bare
// positional argument at startup.
func TestBuiltinProvidersOpenCodePromptModeRegression(t *testing.T) {
	p := BuiltinProviders()["opencode"]
	if p.PromptMode == "arg" {
		t.Fatal("PromptMode must not be \"arg\" — OpenCode interprets positional prompt argv as a project path")
	}
	if p.PromptMode != "flag" || p.PromptFlag != "--prompt" {
		t.Fatalf("OpenCode prompt delivery = %q %q, want flag --prompt", p.PromptMode, p.PromptFlag)
	}
}

func TestBuiltinProviderOrderReturnsNewSlice(t *testing.T) {
	a := BuiltinProviderOrder()
	b := BuiltinProviderOrder()
	a[0] = "mutated"
	if b[0] == "mutated" {
		t.Error("BuiltinProviderOrder() should return a new slice each time")
	}
}

func TestCommandStringNoArgs(t *testing.T) {
	rp := &ResolvedProvider{Command: "claude"}
	if got := rp.CommandString(); got != "claude" {
		t.Errorf("CommandString() = %q, want %q", got, "claude")
	}
}

func TestCommandStringWithArgs(t *testing.T) {
	rp := &ResolvedProvider{
		Command: "claude",
		Args:    []string{"--dangerously-skip-permissions"},
	}
	want := "claude --dangerously-skip-permissions"
	if got := rp.CommandString(); got != want {
		t.Errorf("CommandString() = %q, want %q", got, want)
	}
}

func TestCommandStringMultipleArgs(t *testing.T) {
	rp := &ResolvedProvider{
		Command: "gemini",
		Args:    []string{"--approval-mode", "yolo"},
	}
	want := "gemini --approval-mode yolo"
	if got := rp.CommandString(); got != want {
		t.Errorf("CommandString() = %q, want %q", got, want)
	}
}

func TestCommandStringQuotesShellMetacharacters(t *testing.T) {
	rp := &ResolvedProvider{
		Command: "codex",
		Args:    []string{"--model", "sonnet[1m]", "--message", "it's ready"},
	}
	want := "codex --model 'sonnet[1m]' --message 'it'\\''s ready'"
	if got := rp.CommandString(); got != want {
		t.Errorf("CommandString() = %q, want %q", got, want)
	}
}

func TestACPCommandString(t *testing.T) {
	tests := []struct {
		name string
		rp   ResolvedProvider
		want string
	}{
		{
			name: "FullOverride",
			rp: ResolvedProvider{
				Command:    "opencode",
				Args:       []string{"--verbose"},
				ACPCommand: "opencode-acp",
				ACPArgs:    []string{"--json-rpc"},
			},
			want: "opencode-acp --json-rpc",
		},
		{
			name: "FallbackToCommand",
			rp: ResolvedProvider{
				Command: "opencode",
				Args:    []string{"--verbose"},
			},
			want: "opencode --verbose",
		},
		{
			name: "PartialOverride_CommandOnly",
			rp: ResolvedProvider{
				Command:    "opencode",
				Args:       []string{"--verbose"},
				ACPCommand: "opencode-acp",
			},
			want: "opencode-acp --verbose",
		},
		{
			name: "PartialOverride_ArgsOnly",
			rp: ResolvedProvider{
				Command: "opencode",
				Args:    []string{"--verbose"},
				ACPArgs: []string{"--json-rpc"},
			},
			want: "opencode --json-rpc",
		},
		{
			name: "EmptyACPArgs",
			rp: ResolvedProvider{
				Command:    "opencode",
				Args:       []string{"--verbose"},
				ACPCommand: "opencode-acp",
				ACPArgs:    []string{},
			},
			want: "opencode-acp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.rp.ACPCommandString()
			if got != tt.want {
				t.Errorf("ACPCommandString() = %q, want %q", got, tt.want)
			}
		})
	}

	// Verify FallbackToCommand produces same result as CommandString().
	t.Run("FallbackMatchesCommandString", func(t *testing.T) {
		rp := &ResolvedProvider{Command: "opencode", Args: []string{"--verbose"}}
		if rp.ACPCommandString() != rp.CommandString() {
			t.Errorf("ACPCommandString() = %q, but CommandString() = %q — should match when no ACP overrides",
				rp.ACPCommandString(), rp.CommandString())
		}
	})
}

func TestDefaultSessionTransportOpenCodeFamilyDefaultsToACP(t *testing.T) {
	tests := []struct {
		name string
		rp   ResolvedProvider
	}{
		{
			name: "direct builtin name",
			rp: ResolvedProvider{
				Name:        "opencode",
				SupportsACP: true,
			},
		},
		{
			name: "builtin ancestor",
			rp: ResolvedProvider{
				Name:            "custom-opencode",
				BuiltinAncestor: "opencode",
				SupportsACP:     true,
			},
		},
		{
			name: "deprecated kind fallback",
			rp: ResolvedProvider{
				Name:        "custom-opencode",
				Kind:        "opencode",
				SupportsACP: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rp.DefaultSessionTransport(); got != "acp" {
				t.Fatalf("DefaultSessionTransport() = %q, want %q", got, "acp")
			}
		})
	}
}

func TestDefaultSessionTransportSupportsACPDoesNotImplyACPDefault(t *testing.T) {
	rp := &ResolvedProvider{
		Name:        "custom-acp",
		SupportsACP: true,
	}
	if got := rp.DefaultSessionTransport(); got != "" {
		t.Fatalf("DefaultSessionTransport() = %q, want empty default transport", got)
	}
}

func TestProviderSessionCreateTransportUsesExplicitACPOverrides(t *testing.T) {
	tests := []struct {
		name string
		rp   ResolvedProvider
	}{
		{
			name: "explicit acp command",
			rp: ResolvedProvider{
				Name:        "custom-acp",
				SupportsACP: true,
				ACPCommand:  "/bin/custom-acp",
			},
		},
		{
			name: "explicit acp args",
			rp: ResolvedProvider{
				Name:        "custom-acp",
				SupportsACP: true,
				ACPArgs:     []string{"acp"},
			},
		},
		{
			name: "opencode family remains acp",
			rp: ResolvedProvider{
				Name:            "custom-opencode",
				BuiltinAncestor: "opencode",
				SupportsACP:     true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rp.ProviderSessionCreateTransport(); got != "acp" {
				t.Fatalf("ProviderSessionCreateTransport() = %q, want %q", got, "acp")
			}
		})
	}
}

func TestProviderSessionCreateTransportBuiltinKiroStaysOnCLIByDefault(t *testing.T) {
	rp := &ResolvedProvider{
		Name:        "kiro",
		Command:     "kiro-cli",
		Args:        []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"},
		SupportsACP: true,
		ACPArgs:     []string{"acp", "--agent", "gascity"},
	}
	if got := rp.ProviderSessionCreateTransport(); got != "" {
		t.Fatalf("ProviderSessionCreateTransport() = %q, want empty default transport", got)
	}
	if got := ResolveSessionCreateTransport("", rp); got != "" {
		t.Fatalf("ResolveSessionCreateTransport(empty) = %q, want empty default transport", got)
	}
	if got := ResolveSessionCreateTransport("acp", rp); got != "acp" {
		t.Fatalf("ResolveSessionCreateTransport(acp) = %q, want acp", got)
	}
	if got := rp.ACPCommandString(); got != "kiro-cli acp --agent gascity" {
		t.Fatalf("ACPCommandString() = %q, want explicit Kiro ACP command", got)
	}
}

func TestProviderSessionCreateTransportSupportsACPAloneStaysDefault(t *testing.T) {
	rp := &ResolvedProvider{
		Name:        "custom-acp",
		SupportsACP: true,
	}
	if got := rp.ProviderSessionCreateTransport(); got != "" {
		t.Fatalf("ProviderSessionCreateTransport() = %q, want empty transport", got)
	}
}

func TestResolveSessionCreateTransportPrefersAgentSessionOverride(t *testing.T) {
	got := ResolveSessionCreateTransport("acp", &ResolvedProvider{
		Name:        "custom-acp",
		SupportsACP: true,
	})
	if got != "acp" {
		t.Fatalf("ResolveSessionCreateTransport() = %q, want %q", got, "acp")
	}
}

func TestResolveSessionCreateTransportExplicitTmuxOverridesProviderACPDefault(t *testing.T) {
	got := ResolveSessionCreateTransport("tmux", &ResolvedProvider{
		Name:        "opencode",
		SupportsACP: true,
		ACPArgs:     []string{"acp"},
	})
	if got != "tmux" {
		t.Fatalf("ResolveSessionCreateTransport() = %q, want %q", got, "tmux")
	}
}

func TestResolveSessionCreateTransportFallsBackToProviderCreateTransport(t *testing.T) {
	got := ResolveSessionCreateTransport("", &ResolvedProvider{
		Name:        "custom-acp",
		SupportsACP: true,
		ACPCommand:  "/bin/echo",
	})
	if got != "acp" {
		t.Fatalf("ResolveSessionCreateTransport() = %q, want %q", got, "acp")
	}
}
