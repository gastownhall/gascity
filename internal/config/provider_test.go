package config

import (
	"testing"
)

func TestBuiltinProviders(t *testing.T) {
	providers := BuiltinProviders()
	order := BuiltinProviderOrder()

	// Must have exactly 10 built-in providers.
	if len(providers) != 10 {
		t.Fatalf("len(BuiltinProviders()) = %d, want 10", len(providers))
	}
	if len(order) != 10 {
		t.Fatalf("len(BuiltinProviderOrder()) = %d, want 10", len(order))
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
	if !p.EmitsPermissionWarning {
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
	if p.EmitsPermissionWarning {
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

func TestBuiltinProvidersReturnsNewMap(t *testing.T) {
	a := BuiltinProviders()
	b := BuiltinProviders()
	a["claude"] = ProviderSpec{Command: "mutated"}
	if b["claude"].Command == "mutated" {
		t.Error("BuiltinProviders() should return a new map each time")
	}
}

// TestBuiltinProvidersOpenCode verifies the opencode provider keeps startup
// instructions out of argv. OpenCode treats argv prompt payloads as a normal
// user message, so hook-enabled sessions must receive startup context through
// gc prime --hook instead of argv.
func TestBuiltinProvidersOpenCode(t *testing.T) {
	p := BuiltinProviders()["opencode"]
	if p.Command != "opencode" {
		t.Errorf("Command = %q, want %q", p.Command, "opencode")
	}
	if p.PromptMode != "none" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "none")
	}
	if p.PromptFlag != "" {
		t.Errorf("PromptFlag = %q, want empty", p.PromptFlag)
	}
	if !p.SupportsHooks {
		t.Error("SupportsHooks = false, want true")
	}
	if !p.SupportsACP {
		t.Error("SupportsACP = false, want true")
	}
	if p.InstructionsFile != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q, want %q", p.InstructionsFile, "AGENTS.md")
	}
	if p.ReadyDelayMs != 8000 {
		t.Errorf("ReadyDelayMs = %d, want 8000", p.ReadyDelayMs)
	}
}

// TestBuiltinProvidersOpenCodePromptModeRegression guards against switching
// OpenCode back to argv-based prompt delivery. Gas City renders the startup
// prompt as persona instructions, not as the first user task, so OpenCode must
// not receive it through argv at startup.
func TestBuiltinProvidersOpenCodePromptModeRegression(t *testing.T) {
	p := BuiltinProviders()["opencode"]
	if p.PromptMode == "arg" {
		t.Fatal("PromptMode must not be \"arg\" — OpenCode interprets positional prompt argv as a project path")
	}
	if p.PromptMode == "flag" {
		t.Fatal("PromptMode must not be \"flag\" — OpenCode treats --prompt as the first user message instead of startup persona context")
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

// TestBuiltinCodexNeedsNudgePoller guards the codex preset's nudge-poller
// opt-in. Codex has no UserPromptSubmit-equivalent hook, so queued nudges
// must drain via the background poller. Claude stays hook-driven.
func TestBuiltinCodexNeedsNudgePoller(t *testing.T) {
	builtins := BuiltinProviders()
	if !builtins["codex"].NeedsNudgePoller {
		t.Error("codex: NeedsNudgePoller = false, want true")
	}
	if builtins["claude"].NeedsNudgePoller {
		t.Error("claude: NeedsNudgePoller = true, want false (drains via UserPromptSubmit hook)")
	}
}

func TestMergeProviderOverBuiltinPreservesNeedsNudgePoller(t *testing.T) {
	base := ProviderSpec{Command: "codex", NeedsNudgePoller: true}
	city := ProviderSpec{DisplayName: "Custom Codex"}
	merged := MergeProviderOverBuiltin(base, city)
	if !merged.NeedsNudgePoller {
		t.Error("merged.NeedsNudgePoller = false, want true (inherited from base)")
	}
}

func TestMergeProviderOverBuiltinEnablesNeedsNudgePoller(t *testing.T) {
	base := ProviderSpec{Command: "custom"}
	city := ProviderSpec{NeedsNudgePoller: true}
	merged := MergeProviderOverBuiltin(base, city)
	if !merged.NeedsNudgePoller {
		t.Error("merged.NeedsNudgePoller = false, want true (set by city override)")
	}
}

func TestSpecToResolvedCopiesNeedsNudgePoller(t *testing.T) {
	spec := &ProviderSpec{Command: "codex", NeedsNudgePoller: true}
	rp := specToResolved("codex", spec)
	if !rp.NeedsNudgePoller {
		t.Error("resolved.NeedsNudgePoller = false, want true")
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
