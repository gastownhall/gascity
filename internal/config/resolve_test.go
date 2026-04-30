package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// --- helper lookPath functions ---

func lookPathAll(name string) (string, error) {
	return "/usr/bin/" + name, nil
}

func lookPathNone(string) (string, error) {
	return "", fmt.Errorf("not found")
}

func lookPathOnly(bins ...string) LookPathFunc {
	set := make(map[string]bool, len(bins))
	for _, b := range bins {
		set[b] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", fmt.Errorf("not found: %s", name)
	}
}

// --- ResolveProvider tests ---

func TestResolveProviderAgentStartCommand(t *testing.T) {
	agent := &Agent{Name: "mayor", StartCommand: "my-custom-cli --flag"}
	rp, err := ResolveProvider(agent, nil, nil, lookPathNone)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Command != "my-custom-cli --flag" {
		t.Errorf("Command = %q, want %q", rp.Command, "my-custom-cli --flag")
	}
	if rp.PromptMode != "none" {
		t.Errorf("PromptMode = %q, want %q", rp.PromptMode, "none")
	}
}

func TestResolveProviderAgentStartCommandHonorsExplicitPromptMode(t *testing.T) {
	agent := &Agent{
		Name:         "mayor",
		StartCommand: "my-custom-cli --flag",
		PromptMode:   "arg",
		PromptFlag:   "--prompt",
	}
	rp, err := ResolveProvider(agent, nil, nil, lookPathNone)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", rp.PromptMode, "arg")
	}
	if rp.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", rp.PromptFlag, "--prompt")
	}
}

func TestResolveProviderAgentProvider(t *testing.T) {
	agent := &Agent{Name: "mayor", Provider: "claude"}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Name != "claude" {
		t.Errorf("Name = %q, want %q", rp.Name, "claude")
	}
	if rp.Command != "claude" {
		t.Errorf("Command = %q, want %q", rp.Command, "claude")
	}
	// After migration, CommandString() is just "claude" -- schema flags come from ResolveDefaultArgs.
	cs := rp.CommandString()
	if cs != "claude" {
		t.Errorf("CommandString() = %q, want %q", cs, "claude")
	}
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

func TestResolveProviderWorkspaceProvider(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{Name: "city", Provider: "codex"}
	rp, err := ResolveProvider(agent, ws, nil, lookPathOnly("codex"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Name != "codex" {
		t.Errorf("Name = %q, want %q", rp.Name, "codex")
	}
	// After migration, CommandString() is just "codex" -- schema flags come from ResolveDefaultArgs.
	if rp.CommandString() != "codex" {
		t.Errorf("CommandString() = %q, want %q", rp.CommandString(), "codex")
	}
	defaultArgs := rp.ResolveDefaultArgs()
	codexWantArgs := []string{
		"--dangerously-bypass-approvals-and-sandbox",
		"--model", "gpt-5.5",
		"-c", "model_reasoning_effort=xhigh",
	}
	if len(defaultArgs) != len(codexWantArgs) {
		t.Errorf("ResolveDefaultArgs() = %v, want %v", defaultArgs, codexWantArgs)
	} else {
		for i, w := range codexWantArgs {
			if defaultArgs[i] != w {
				t.Errorf("ResolveDefaultArgs()[%d] = %q, want %q", i, defaultArgs[i], w)
			}
		}
	}
	if rp.ResumeFlag != "resume" {
		t.Errorf("ResumeFlag = %q, want resume", rp.ResumeFlag)
	}
	if rp.ResumeStyle != "subcommand" {
		t.Errorf("ResumeStyle = %q, want subcommand", rp.ResumeStyle)
	}
	if rp.ReadyPromptPrefix != "› " {
		t.Errorf("ReadyPromptPrefix = %q, want %q", rp.ReadyPromptPrefix, "› ")
	}
}

func TestResolveProviderWorkspaceStartCommand(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{Name: "city", StartCommand: "my-agent --flag"}
	rp, err := ResolveProvider(agent, ws, nil, lookPathNone)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Command != "my-agent --flag" {
		t.Errorf("Command = %q, want %q", rp.Command, "my-agent --flag")
	}
	if rp.PromptMode != "none" {
		t.Errorf("PromptMode = %q, want %q", rp.PromptMode, "none")
	}
}

// TestResolveProviderWorkspaceStartCommandWithProvider verifies that
// workspace.start_command overrides the provider command when a provider
// name is resolved (via workspace.provider or auto-detect), preserving
// provider settings like PromptMode while clearing schema-managed flags.
func TestResolveProviderWorkspaceStartCommandWithProvider(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{Name: "city", Provider: "claude", StartCommand: "claude --auto"}
	rp, err := ResolveProvider(agent, ws, nil, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Command != "claude --auto" {
		t.Errorf("Command = %q, want %q", rp.Command, "claude --auto")
	}
	if rp.CommandString() != "claude --auto" {
		t.Errorf("CommandString() = %q, want %q (Args should be nil)", rp.CommandString(), "claude --auto")
	}
	// Schema-managed defaults must be cleared so they aren't appended.
	if len(rp.ResolveDefaultArgs()) != 0 {
		t.Errorf("ResolveDefaultArgs() = %v, want nil (start_command is complete command)", rp.ResolveDefaultArgs())
	}
	// Provider settings should be preserved.
	if rp.Name != "claude" {
		t.Errorf("Name = %q, want %q (provider settings should be preserved)", rp.Name, "claude")
	}
	builtins := BuiltinProviders()
	claudeSpec := builtins["claude"]
	if rp.ReadyPromptPrefix != claudeSpec.ReadyPromptPrefix {
		t.Errorf("ReadyPromptPrefix = %q, want %q", rp.ReadyPromptPrefix, claudeSpec.ReadyPromptPrefix)
	}
}

// TestResolveProviderAgentStartCommandWinsOverWorkspace verifies that
// agent.start_command takes precedence over workspace.start_command.
func TestResolveProviderAgentStartCommandWinsOverWorkspace(t *testing.T) {
	agent := &Agent{Name: "worker", StartCommand: "my-agent --custom"}
	ws := &Workspace{Name: "city", Provider: "claude", StartCommand: "claude --auto"}
	rp, err := ResolveProvider(agent, ws, nil, lookPathNone)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Command != "my-agent --custom" {
		t.Errorf("Command = %q, want %q (agent.StartCommand should win)", rp.Command, "my-agent --custom")
	}
}

func TestResolveProviderAutoDetect(t *testing.T) {
	agent := &Agent{Name: "worker"}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("codex"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Name != "codex" {
		t.Errorf("Name = %q, want %q", rp.Name, "codex")
	}
}

func TestResolveProviderAutoDetectNone(t *testing.T) {
	agent := &Agent{Name: "worker"}
	_, err := ResolveProvider(agent, nil, nil, lookPathNone)
	if err == nil {
		t.Fatal("expected error when no provider found")
	}
}

func TestResolveProviderAgentOverridesWorkspace(t *testing.T) {
	agent := &Agent{Name: "worker", Provider: "claude"}
	ws := &Workspace{Name: "city", Provider: "codex"}
	rp, err := ResolveProvider(agent, ws, nil, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Name != "claude" {
		t.Errorf("Name = %q, want %q (agent.Provider should win)", rp.Name, "claude")
	}
}

func TestResolveProviderStartCommandWinsOverProvider(t *testing.T) {
	agent := &Agent{Name: "mayor", StartCommand: "custom-cmd", Provider: "claude"}
	rp, err := ResolveProvider(agent, nil, nil, lookPathNone)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Command != "custom-cmd" {
		t.Errorf("Command = %q, want %q", rp.Command, "custom-cmd")
	}
}

func TestResolveProviderCityOverridesBuiltin(t *testing.T) {
	agent := &Agent{Name: "mayor", Provider: "claude"}
	cityProviders := map[string]ProviderSpec{
		"claude": {
			Command:      "claude",
			Args:         []string{"--custom-flag"},
			PromptMode:   "flag",
			PromptFlag:   "--prompt",
			ReadyDelayMs: 20000,
		},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.CommandString() != "claude --custom-flag" {
		t.Errorf("CommandString() = %q, want %q", rp.CommandString(), "claude --custom-flag")
	}
	if rp.PromptMode != "flag" {
		t.Errorf("PromptMode = %q, want %q", rp.PromptMode, "flag")
	}
	if rp.ReadyDelayMs != 20000 {
		t.Errorf("ReadyDelayMs = %d, want 20000", rp.ReadyDelayMs)
	}
}

func TestResolveProviderUserDefinedProvider(t *testing.T) {
	agent := &Agent{Name: "scout", Provider: "kiro"}
	cityProviders := map[string]ProviderSpec{
		"kiro": {
			Command:      "kiro",
			Args:         []string{"--autonomous"},
			PromptMode:   "arg",
			ReadyDelayMs: 5000,
			ProcessNames: []string{"kiro", "node"},
		},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("kiro"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Name != "kiro" {
		t.Errorf("Name = %q, want %q", rp.Name, "kiro")
	}
	if rp.CommandString() != "kiro --autonomous" {
		t.Errorf("CommandString() = %q, want %q", rp.CommandString(), "kiro --autonomous")
	}
	if rp.ReadyDelayMs != 5000 {
		t.Errorf("ReadyDelayMs = %d, want 5000", rp.ReadyDelayMs)
	}
}

func TestResolveProviderQuotesMetacharacterArgs(t *testing.T) {
	agent := &Agent{Name: "worker", Provider: "codex"}
	cityProviders := map[string]ProviderSpec{
		"codex": {
			Command:    "codex",
			Args:       []string{"--model", "sonnet[1m]", "--message", "it's ready"},
			PromptMode: "none",
		},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("codex"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	want := "codex --model 'sonnet[1m]' --message 'it'\\''s ready'"
	if got := rp.CommandString(); got != want {
		t.Errorf("CommandString() = %q, want %q", got, want)
	}
}

func TestResolveProviderUnknown(t *testing.T) {
	agent := &Agent{Name: "mayor", Provider: "vim"}
	_, err := ResolveProvider(agent, nil, nil, lookPathAll)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestResolveProviderNotInPath(t *testing.T) {
	agent := &Agent{Name: "mayor", Provider: "claude"}
	_, err := ResolveProvider(agent, nil, nil, lookPathNone)
	if err == nil {
		t.Fatal("expected error when provider not in PATH")
	}
}

// --- Agent-level field overrides ---

func TestResolveProviderAgentArgsOverride(t *testing.T) {
	agent := &Agent{
		Name:     "scout",
		Provider: "claude",
		Args:     []string{"--dangerously-skip-permissions", "--verbose"},
	}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	// Agent-level args override replaces provider args entirely.
	if len(rp.Args) != 2 || rp.Args[1] != "--verbose" {
		t.Errorf("Args = %v, want [--dangerously-skip-permissions --verbose]", rp.Args)
	}
}

func TestResolveProviderAgentReadyDelayOverride(t *testing.T) {
	delay := 15000
	agent := &Agent{
		Name:         "scout",
		Provider:     "claude",
		ReadyDelayMs: &delay,
	}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.ReadyDelayMs != 15000 {
		t.Errorf("ReadyDelayMs = %d, want 15000", rp.ReadyDelayMs)
	}
}

func TestResolveProviderAgentEmitsPermissionWarningOverride(t *testing.T) {
	f := false
	agent := &Agent{
		Name:                   "scout",
		Provider:               "claude",
		EmitsPermissionWarning: &f,
	}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	// Claude preset has EmitsPermissionWarning=true, agent overrides to false.
	if rp.EmitsPermissionWarning {
		t.Error("EmitsPermissionWarning = true, want false (agent override)")
	}
}

func TestResolveProviderAgentEnvMerges(t *testing.T) {
	agent := &Agent{
		Name:     "scout",
		Provider: "claude",
		Env:      map[string]string{"EXTRA": "yes"},
	}
	cityProviders := map[string]ProviderSpec{
		"claude": {
			Command: "claude",
			Args:    []string{"--dangerously-skip-permissions"},
			Env:     map[string]string{"BASE": "1"},
		},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Env["BASE"] != "1" {
		t.Errorf("Env[BASE] = %q, want %q", rp.Env["BASE"], "1")
	}
	if rp.Env["EXTRA"] != "yes" {
		t.Errorf("Env[EXTRA] = %q, want %q", rp.Env["EXTRA"], "yes")
	}
}

func TestResolveProviderAgentEnvOverridesBase(t *testing.T) {
	agent := &Agent{
		Name:     "scout",
		Provider: "claude",
		Env:      map[string]string{"KEY": "agent-val"},
	}
	cityProviders := map[string]ProviderSpec{
		"claude": {
			Command: "claude",
			Env:     map[string]string{"KEY": "base-val"},
		},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Env["KEY"] != "agent-val" {
		t.Errorf("Env[KEY] = %q, want %q (agent should override)", rp.Env["KEY"], "agent-val")
	}
}

func TestResolveProviderDefaultPromptMode(t *testing.T) {
	agent := &Agent{Name: "worker", Provider: "codex"}
	// Codex preset has prompt_mode = "arg", so it should stay "arg".
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("codex"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", rp.PromptMode, "arg")
	}
}

func TestResolveProviderDefaultPromptModeWhenEmpty(t *testing.T) {
	// A city-defined provider with no prompt_mode should get "arg" default.
	agent := &Agent{Name: "worker", Provider: "custom"}
	cityProviders := map[string]ProviderSpec{
		"custom": {Command: "custom-agent"},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("custom-agent"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q (default)", rp.PromptMode, "arg")
	}
}

// --- detectProviderName ---

func TestDetectProviderNameClaude(t *testing.T) {
	name, err := detectProviderName(lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("detectProviderName: %v", err)
	}
	if name != "claude" {
		t.Errorf("name = %q, want %q", name, "claude")
	}
}

func TestDetectProviderNameFallbackToCodex(t *testing.T) {
	name, err := detectProviderName(lookPathOnly("codex"))
	if err != nil {
		t.Fatalf("detectProviderName: %v", err)
	}
	if name != "codex" {
		t.Errorf("name = %q, want %q", name, "codex")
	}
}

func TestDetectProviderNameNone(t *testing.T) {
	_, err := detectProviderName(lookPathNone)
	if err == nil {
		t.Fatal("expected error when no provider found")
	}
}

// --- lookupProvider ---

func TestLookupProviderBuiltin(t *testing.T) {
	spec, err := lookupProvider("claude", nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	if spec.Command != "claude" {
		t.Errorf("Command = %q, want %q", spec.Command, "claude")
	}
}

func TestLookupProviderCityOverride(t *testing.T) {
	city := map[string]ProviderSpec{
		"claude": {Command: "claude", Args: []string{"--custom"}},
	}
	spec, err := lookupProvider("claude", city, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--custom" {
		t.Errorf("Args = %v, want [--custom]", spec.Args)
	}
}

// TestLookupProviderBaseChainIntegration verifies the full path from
// lookupProvider through the chain walker: a wrapper provider with
// base = "builtin:codex" must come back with inherited PermissionModes
// and OptionsSchema from the built-in codex. This test would have
// caught the bug where the runtime launch command for codex-mini was
// missing --dangerously-bypass-approvals-and-sandbox because
// lookupProvider ignored the Base field.
func TestLookupProviderBaseChainIntegration(t *testing.T) {
	b := "builtin:codex"
	city := map[string]ProviderSpec{
		"codex-mini": {
			Base:          &b,
			Command:       "aimux",
			Args:          []string{"run", "codex", "--", "-m", "gpt-5.3"},
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	spec, err := lookupProvider("codex-mini", city, lookPathAll)
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	// Leaf-level overrides preserved.
	if spec.Command != "aimux" {
		t.Errorf("Command = %q, want aimux (leaf override)", spec.Command)
	}
	// Inherited from built-in codex: PermissionModes must contain the
	// unrestricted key that maps to --dangerously-bypass flag.
	if spec.PermissionModes == nil {
		t.Fatal("PermissionModes is nil — built-in codex inheritance did not propagate via lookupProvider")
	}
	want := "--dangerously-bypass-approvals-and-sandbox"
	if got := spec.PermissionModes["unrestricted"]; got != want {
		t.Errorf("PermissionModes[\"unrestricted\"] = %q, want %q", got, want)
	}
	// Inherited OptionsSchema: must contain permission_mode with choices
	// including unrestricted → FlagArgs [--dangerously-bypass-approvals-and-sandbox].
	found := false
	for _, opt := range spec.OptionsSchema {
		if opt.Key != "permission_mode" {
			continue
		}
		for _, c := range opt.Choices {
			if c.Value == "unrestricted" {
				if len(c.FlagArgs) == 0 || c.FlagArgs[0] != want {
					t.Errorf("permission_mode unrestricted FlagArgs = %v, want %v", c.FlagArgs, []string{want})
				}
				found = true
			}
		}
	}
	if !found {
		t.Error("OptionsSchema did not inherit permission_mode.unrestricted from built-in codex")
	}
	// Inherited scalars.
	if spec.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want arg (inherited)", spec.PromptMode)
	}
	if spec.ReadyDelayMs != 3000 {
		t.Errorf("ReadyDelayMs = %d, want 3000 (inherited)", spec.ReadyDelayMs)
	}
}

func TestLookupProviderExplicitEmptyBaseOptsOutOfLegacyMerge(t *testing.T) {
	empty := ""
	city := map[string]ProviderSpec{
		"codex": {
			Base:    &empty,
			Command: "codex",
		},
	}
	spec, err := lookupProvider("codex", city, lookPathAll)
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	if spec.Base == nil || *spec.Base != "" {
		t.Fatalf("Base = %v, want explicit empty", spec.Base)
	}
	if len(spec.PermissionModes) != 0 {
		t.Errorf("explicit base=\"\" should not inherit PermissionModes, got %v", spec.PermissionModes)
	}
	if len(spec.OptionsSchema) != 0 {
		t.Errorf("explicit base=\"\" should not inherit OptionsSchema, got %v", spec.OptionsSchema)
	}
}

// TestResolveProviderBaseChainEmitsDangerousBypass verifies that a
// wrapped codex provider with base = "builtin:codex" produces a
// ResolvedProvider whose ResolveDefaultArgs() includes
// --dangerously-bypass-approvals-and-sandbox. This is the end-to-end
// launch-command invariant for the aimux-codex fix.
func TestResolveProviderBaseChainEmitsDangerousBypass(t *testing.T) {
	b := "builtin:codex"
	city := map[string]ProviderSpec{
		"codex-mini": {
			Base:          &b,
			Command:       "aimux",
			Args:          []string{"run", "codex", "--", "-m", "gpt-5.3"},
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	agent := &Agent{Name: "codex-mini", Provider: "codex-mini"}
	resolved, err := ResolveProvider(agent, nil, city, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if resolved.BuiltinAncestor != "codex" {
		t.Errorf("BuiltinAncestor = %q, want codex", resolved.BuiltinAncestor)
	}
	if len(resolved.OptionsSchema) == 0 {
		t.Fatal("OptionsSchema empty — built-in inheritance did not reach ResolvedProvider")
	}
	args := resolved.ResolveDefaultArgs()
	want := "--dangerously-bypass-approvals-and-sandbox"
	found := false
	for _, a := range args {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ResolveDefaultArgs() = %v, missing %q — session would hang on first sandboxed command", args, want)
	}
}

func TestResolveProviderBaseChainStripsCodexAliases(t *testing.T) {
	b := "builtin:codex"
	city := map[string]ProviderSpec{
		"codex-max": {
			Base:    &b,
			Command: "aimux",
			Args: []string{
				"run", "codex", "--",
				"--dangerously-bypass-approvals-and-sandbox",
				"-m", "gpt-5.5",
				"-c", "model_reasoning_effort=\"xhigh\"",
			},
			ResumeCommand: "aimux run codex -- --dangerously-bypass-approvals-and-sandbox -m gpt-5.5 resume {{.SessionKey}}",
		},
	}
	agent := &Agent{Name: "codex-max", Provider: "codex-max"}
	resolved, err := ResolveProvider(agent, nil, city, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	wantArgs := []string{"run", "codex", "--"}
	if !reflect.DeepEqual(resolved.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", resolved.Args, wantArgs)
	}
	if got := resolved.EffectiveDefaults["model"]; got != "gpt-5.5" {
		t.Fatalf("EffectiveDefaults[model] = %q, want gpt-5.5", got)
	}
	if got := resolved.EffectiveDefaults["effort"]; got != "xhigh" {
		t.Fatalf("EffectiveDefaults[effort] = %q, want xhigh", got)
	}
	command := resolved.CommandString()
	if defaultArgs := resolved.ResolveDefaultArgs(); len(defaultArgs) > 0 {
		command = command + " " + strings.Join(defaultArgs, " ")
	}
	if strings.Count(command, "gpt-5.5") != 1 {
		t.Fatalf("resolved launch command = %q, want one model flag", command)
	}
	if strings.Count(command, "model_reasoning_effort") != 1 {
		t.Fatalf("resolved launch command = %q, want one effort flag", command)
	}
}

func TestResolveProviderChainArgsAppendAffectsResolvedArgs(t *testing.T) {
	custom := map[string]ProviderSpec{
		"codex": {
			Base:          basePtr("builtin:codex"),
			Command:       "aimux",
			Args:          []string{"run", "codex", "--"},
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
		"codex-max": {
			Base:       basePtr("codex"),
			ArgsAppend: []string{"-m", "gpt-5.4"},
		},
	}
	resolved, err := ResolveProviderChain("codex-max", custom["codex-max"], custom)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	want := []string{"run", "codex", "--", "-m", "gpt-5.4"}
	if !reflect.DeepEqual(resolved.Args, want) {
		t.Fatalf("Args = %v, want %v", resolved.Args, want)
	}
}

func TestMergeProviderOverBuiltinOptionsSchemaByKeyAndOmit(t *testing.T) {
	base := ProviderSpec{
		OptionsSchema: []ProviderOption{
			{Key: "model", Label: "Model", Default: "old"},
			{Key: "permission_mode", Label: "Permission", Default: "plan"},
		},
		OptionDefaults: map[string]string{
			"model":           "old",
			"permission_mode": "plan",
		},
	}
	city := ProviderSpec{
		OptionsSchemaMerge: "by_key",
		OptionsSchema: []ProviderOption{
			{Key: "model", Label: "Model", Default: "new"},
			{Key: "permission_mode", Omit: true},
			{Key: "effort", Label: "Effort", Default: "high"},
		},
		OptionDefaults: map[string]string{
			"model":  "new",
			"effort": "high",
		},
	}
	merged := MergeProviderOverBuiltin(base, city)
	if got := optionKeys(merged.OptionsSchema); !reflect.DeepEqual(got, []string{"model", "effort"}) {
		t.Fatalf("option keys = %v, want [model effort]", got)
	}
	if merged.OptionsSchema[0].Default != "new" {
		t.Errorf("model default = %q, want new", merged.OptionsSchema[0].Default)
	}
	if _, ok := merged.OptionDefaults["permission_mode"]; ok {
		t.Errorf("omitted option default survived: %v", merged.OptionDefaults)
	}
	if got := merged.OptionDefaults["effort"]; got != "high" {
		t.Errorf("effort default = %q, want high", got)
	}
}

func optionKeys(opts []ProviderOption) []string {
	keys := make([]string, 0, len(opts))
	for _, opt := range opts {
		keys = append(keys, opt.Key)
	}
	return keys
}

func TestLookupProviderUnknown(t *testing.T) {
	_, err := lookupProvider("vim", nil, lookPathAll)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestLookupProviderNotInPath(t *testing.T) {
	_, err := lookupProvider("claude", nil, lookPathNone)
	if err == nil {
		t.Fatal("expected error when binary not in PATH")
	}
}

func TestLookupProviderCityNotInPath(t *testing.T) {
	city := map[string]ProviderSpec{
		"kiro": {Command: "kiro"},
	}
	_, err := lookupProvider("kiro", city, lookPathNone)
	if err == nil {
		t.Fatal("expected error when city provider binary not in PATH")
	}
}

// Verify city provider with empty command doesn't fail PATH check.
func TestLookupProviderCityEmptyCommand(t *testing.T) {
	city := map[string]ProviderSpec{
		"custom": {Args: []string{"--flag"}},
	}
	spec, err := lookupProvider("custom", city, lookPathNone)
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	if len(spec.Args) != 1 {
		t.Errorf("Args = %v, want [--flag]", spec.Args)
	}
}

// --- lookupProvider built-in inheritance tests ---

// Verify that a city provider whose Command matches a built-in inherits
// the built-in's PromptMode, PromptFlag, ReadyDelayMs, etc.
func TestLookupProviderCityInheritsBuiltin(t *testing.T) {
	city := map[string]ProviderSpec{
		"fast": {Command: "copilot", Args: []string{"--yolo", "--model", "claude-haiku-4.5"}},
	}
	spec, err := lookupProvider("fast", city, lookPathOnly("copilot"))
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	// Should inherit copilot's built-in PromptMode.
	builtinCopilot := BuiltinProviders()["copilot"]
	if spec.PromptMode != builtinCopilot.PromptMode {
		t.Errorf("PromptMode = %q, want %q (inherited)", spec.PromptMode, builtinCopilot.PromptMode)
	}
	// Should inherit ReadyDelayMs.
	if spec.ReadyDelayMs != builtinCopilot.ReadyDelayMs {
		t.Errorf("ReadyDelayMs = %d, want %d (inherited)", spec.ReadyDelayMs, builtinCopilot.ReadyDelayMs)
	}
	// Should inherit ReadyPromptPrefix.
	if spec.ReadyPromptPrefix != builtinCopilot.ReadyPromptPrefix {
		t.Errorf("ReadyPromptPrefix = %q, want %q (inherited)", spec.ReadyPromptPrefix, builtinCopilot.ReadyPromptPrefix)
	}
	// City args should override built-in args.
	if len(spec.Args) != 3 || spec.Args[2] != "claude-haiku-4.5" {
		t.Errorf("Args = %v, want [--yolo --model claude-haiku-4.5]", spec.Args)
	}
	// Should inherit SupportsHooks from built-in copilot.
	if derefBool(spec.SupportsHooks) != derefBool(builtinCopilot.SupportsHooks) {
		t.Errorf("SupportsHooks = %v, want %v (inherited)", derefBool(spec.SupportsHooks), derefBool(builtinCopilot.SupportsHooks))
	}
}

// Verify that a city provider can override inherited fields.
func TestLookupProviderCityOverridesInheritedField(t *testing.T) {
	city := map[string]ProviderSpec{
		"custom-claude": {
			Command:    "claude",
			PromptMode: "none",
			Args:       []string{"--custom"},
		},
	}
	spec, err := lookupProvider("custom-claude", city, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	if spec.PromptMode != "none" {
		t.Errorf("PromptMode = %q, want %q (city override)", spec.PromptMode, "none")
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--custom" {
		t.Errorf("Args = %v, want [--custom]", spec.Args)
	}
}

// Verify that a city provider with a non-builtin command is not merged.
func TestLookupProviderCityNoMergeForUnknownCommand(t *testing.T) {
	city := map[string]ProviderSpec{
		"mybot": {Command: "mybot", Args: []string{"run"}},
	}
	spec, err := lookupProvider("mybot", city, lookPathOnly("mybot"))
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	if spec.PromptMode != "" {
		t.Errorf("PromptMode = %q, want empty (no built-in to inherit from)", spec.PromptMode)
	}
}

// --- MergeProviderOverBuiltin tests ---

func TestMergeProviderOverBuiltin(t *testing.T) {
	base := ProviderSpec{
		Command:           "copilot",
		Args:              []string{"--yolo"},
		PromptMode:        "flag",
		PromptFlag:        "--prompt",
		ReadyDelayMs:      5000,
		ReadyPromptPrefix: "❯ ",
		SupportsACP:       boolPtr(true),
		Env:               map[string]string{"BASE_KEY": "base_val"},
		PermissionModes:   map[string]string{"unrestricted": "--yolo"},
	}

	city := ProviderSpec{
		Command: "copilot",
		Args:    []string{"--yolo", "--model", "claude-haiku-4.5"},
		Env:     map[string]string{"CITY_KEY": "city_val"},
	}

	result := MergeProviderOverBuiltin(base, city)

	// City args replace entirely.
	if len(result.Args) != 3 {
		t.Fatalf("Args = %v, want 3 elements", result.Args)
	}
	// Inherited fields preserved.
	if result.PromptMode != "flag" {
		t.Errorf("PromptMode = %q, want %q", result.PromptMode, "flag")
	}
	if result.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", result.PromptFlag, "--prompt")
	}
	if result.ReadyDelayMs != 5000 {
		t.Errorf("ReadyDelayMs = %d, want 5000", result.ReadyDelayMs)
	}
	if !derefBool(result.SupportsACP) {
		t.Error("SupportsACP should be inherited")
	}
	// Env merged additively.
	if result.Env["BASE_KEY"] != "base_val" {
		t.Error("base env key lost")
	}
	if result.Env["CITY_KEY"] != "city_val" {
		t.Error("city env key missing")
	}
	// PermissionModes inherited.
	if result.PermissionModes["unrestricted"] != "--yolo" {
		t.Error("PermissionModes not inherited")
	}
}

func TestResolveProviderBuiltinOpenCodeCustomCommandKeepsACPArgsOnCustomBinary(t *testing.T) {
	base := "builtin:opencode"
	cityProviders := map[string]ProviderSpec{
		"custom-opencode": {
			Base:    &base,
			Command: "custom-opencode",
		},
	}
	agent := &Agent{Name: "worker", Provider: "custom-opencode"}

	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("custom-opencode"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Command != "custom-opencode" {
		t.Fatalf("Command = %q, want custom-opencode", rp.Command)
	}
	if rp.ACPCommand != "" {
		t.Fatalf("ACPCommand = %q, want empty fallback to Command", rp.ACPCommand)
	}
	if got := rp.ACPCommandString(); got != "custom-opencode acp" {
		t.Fatalf("ACPCommandString() = %q, want custom-opencode acp", got)
	}
}

// --- Tri-state capability bool tests ---
//
// These verify the three-way *bool semantics for SupportsHooks,
// SupportsACP, and EmitsPermissionWarning per the provider-inheritance
// design §Tri-state capability bools.

func TestMergeProviderOverBuiltinTriStateChildDisablesParentEnable(t *testing.T) {
	// Parent sets &true, child explicitly sets &false → final &false.
	base := ProviderSpec{Command: "x", SupportsHooks: boolPtr(true)}
	city := ProviderSpec{SupportsHooks: boolPtr(false)}
	result := MergeProviderOverBuiltin(base, city)
	if result.SupportsHooks == nil {
		t.Fatal("SupportsHooks = nil, want &false")
	}
	if *result.SupportsHooks != false {
		t.Errorf("SupportsHooks = %v, want false (child explicit disable wins)", *result.SupportsHooks)
	}
}

func TestMergeProviderOverBuiltinTriStateChildNilInheritsParent(t *testing.T) {
	// Parent sets &true, child absent (nil) → final inherits &true.
	base := ProviderSpec{Command: "x", SupportsHooks: boolPtr(true)}
	city := ProviderSpec{}
	result := MergeProviderOverBuiltin(base, city)
	if result.SupportsHooks == nil {
		t.Fatal("SupportsHooks = nil, want inherited &true")
	}
	if *result.SupportsHooks != true {
		t.Errorf("SupportsHooks = %v, want true (inherited)", *result.SupportsHooks)
	}
}

func TestMergeProviderOverBuiltinTriStateChildEnablesParentNil(t *testing.T) {
	// Parent absent (nil), child sets &true → final &true.
	base := ProviderSpec{Command: "x"}
	city := ProviderSpec{SupportsHooks: boolPtr(true)}
	result := MergeProviderOverBuiltin(base, city)
	if result.SupportsHooks == nil {
		t.Fatal("SupportsHooks = nil, want &true")
	}
	if *result.SupportsHooks != true {
		t.Errorf("SupportsHooks = %v, want true (child enabled)", *result.SupportsHooks)
	}
}

// TestSupportsHooksFalseRegressionTOML verifies that a raw TOML config
// with supports_hooks = false decodes into *bool = &false and propagates
// through resolution as a suppression (resolved.SupportsHooks == false).
// This is the back-compat regression test called out in the migration.
func TestSupportsHooksFalseRegressionTOML(t *testing.T) {
	// Parse TOML that sets supports_hooks = false on a custom provider
	// that inherits from builtin claude (which has SupportsHooks = &true).
	// The explicit false must win over the inherited true.
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[providers.no-hooks-claude]
base = "builtin:claude"
supports_hooks = false
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	spec, ok := cfg.Providers["no-hooks-claude"]
	if !ok {
		t.Fatal("provider no-hooks-claude not loaded")
	}
	if spec.SupportsHooks == nil {
		t.Fatal("SupportsHooks decoded as nil, want &false (TOML explicit)")
	}
	if *spec.SupportsHooks != false {
		t.Errorf("SupportsHooks = %v, want false", *spec.SupportsHooks)
	}

	// Resolve through the chain and confirm the explicit false survives
	// inheritance from builtin claude (which has SupportsHooks = &true).
	resolved, err := ResolveProviderChain("no-hooks-claude", spec, cfg.Providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	if resolved.SupportsHooks {
		t.Error("resolved SupportsHooks = true, want false (explicit disable must win over inherited enable)")
	}
}

// TestSupportsHooksComposeFragmentDisables verifies that a fragment with
// supports_hooks = false, composed over a builtin-derived provider with
// no local declaration, produces a final &false on the merged spec.
func TestSupportsHooksComposeFragmentDisables(t *testing.T) {
	// Fragment that disables hooks on a provider already present in base.
	base := ProviderSpec{Command: "claude", SupportsHooks: boolPtr(true)}
	// deepMergeProvider uses fragMeta.IsDefined to detect explicit
	// presence, so simulate that by merging directly through
	// MergeProviderOverBuiltin which is the authoritative path.
	frag := ProviderSpec{SupportsHooks: boolPtr(false)}
	merged := MergeProviderOverBuiltin(base, frag)
	if merged.SupportsHooks == nil || *merged.SupportsHooks != false {
		t.Errorf("merged SupportsHooks = %v, want &false", merged.SupportsHooks)
	}
}

// --- ResolveInstallHooks tests ---

func TestResolveInstallHooksAgentOverridesWorkspace(t *testing.T) {
	agent := &Agent{Name: "polecat", InstallAgentHooks: []string{"gemini"}}
	ws := &Workspace{InstallAgentHooks: []string{"claude", "copilot"}}
	got := ResolveInstallHooks(agent, ws)
	if len(got) != 1 || got[0] != "gemini" {
		t.Errorf("ResolveInstallHooks = %v, want [gemini]", got)
	}
}

func TestResolveInstallHooksFallsBackToWorkspace(t *testing.T) {
	agent := &Agent{Name: "mayor"}
	ws := &Workspace{InstallAgentHooks: []string{"claude", "copilot"}}
	got := ResolveInstallHooks(agent, ws)
	if len(got) != 2 || got[0] != "claude" || got[1] != "copilot" {
		t.Errorf("ResolveInstallHooks = %v, want [claude copilot]", got)
	}
}

func TestResolveInstallHooksNilWorkspace(t *testing.T) {
	agent := &Agent{Name: "mayor"}
	got := ResolveInstallHooks(agent, nil)
	if got != nil {
		t.Errorf("ResolveInstallHooks = %v, want nil", got)
	}
}

func TestResolveInstallHooksNeitherSet(t *testing.T) {
	agent := &Agent{Name: "mayor"}
	ws := &Workspace{Name: "test"}
	got := ResolveInstallHooks(agent, ws)
	if got != nil {
		t.Errorf("ResolveInstallHooks = %v, want nil", got)
	}
}

// --- AgentHasHooks tests ---

func TestAgentHasHooks_ClaudeAlways(t *testing.T) {
	agent := &Agent{Name: "mayor"}
	ws := &Workspace{Name: "test"}
	if !AgentHasHooks(agent, ws, "claude", nil) {
		t.Error("claude should always have hooks")
	}
}

func TestAgentHasHooks_InstallHooksMatch(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{InstallAgentHooks: []string{"gemini", "opencode"}}
	if !AgentHasHooks(agent, ws, "gemini", nil) {
		t.Error("gemini with install_agent_hooks should have hooks")
	}
}

func TestAgentHasHooks_InstallHooksNoMatch(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{InstallAgentHooks: []string{"claude"}}
	if AgentHasHooks(agent, ws, "codex", nil) {
		t.Error("codex not in install_agent_hooks should not have hooks")
	}
}

func TestAgentHasHooks_NoHooksByDefault(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{Name: "test"}
	if AgentHasHooks(agent, ws, "codex", nil) {
		t.Error("codex with no install_agent_hooks should not have hooks")
	}
}

func TestAgentHasHooks_ExplicitOverrideTrue(t *testing.T) {
	yes := true
	agent := &Agent{Name: "worker", HooksInstalled: &yes}
	ws := &Workspace{Name: "test"}
	if !AgentHasHooks(agent, ws, "codex", nil) {
		t.Error("hooks_installed=true should override to true")
	}
}

func TestAgentHasHooks_ExplicitOverrideFalse(t *testing.T) {
	no := false
	agent := &Agent{Name: "worker", HooksInstalled: &no}
	ws := &Workspace{Name: "test"}
	// Even claude should be overridden to false when explicit.
	if AgentHasHooks(agent, ws, "claude", nil) {
		t.Error("hooks_installed=false should override even claude")
	}
}

func TestAgentHasHooks_AgentLevelInstallHooks(t *testing.T) {
	agent := &Agent{Name: "worker", InstallAgentHooks: []string{"copilot"}}
	ws := &Workspace{InstallAgentHooks: []string{"claude"}}
	// Agent-level overrides workspace — only copilot in list.
	if !AgentHasHooks(agent, ws, "copilot", nil) {
		t.Error("agent install_agent_hooks should be checked")
	}
	if AgentHasHooks(agent, ws, "opencode", nil) {
		t.Error("opencode not in agent install_agent_hooks")
	}
}

// TestAgentHasHooks_WrappedClaudeRecognizedViaBuiltinFamily verifies
// that a wrapped custom provider (e.g. claude-max with base = "builtin:claude")
// is recognized as claude-family and gets hooks installed by default —
// matching what literal "claude" would get.
func TestAgentHasHooks_WrappedClaudeRecognizedViaBuiltinFamily(t *testing.T) {
	base := "builtin:claude"
	cityProviders := map[string]ProviderSpec{
		"claude-max": {Base: &base, Command: "claude-max"},
	}
	agent := &Agent{Name: "mayor"}
	ws := &Workspace{Name: "test"}
	if !AgentHasHooks(agent, ws, "claude-max", cityProviders) {
		t.Error("claude-max (wrapped claude) should be recognized as claude-family and have hooks")
	}
}

// --- InstructionsFile default ---

func TestResolveProviderInstructionsFileDefault(t *testing.T) {
	// A provider with no InstructionsFile should default to "AGENTS.md".
	agent := &Agent{Name: "worker", Provider: "custom"}
	cityProviders := map[string]ProviderSpec{
		"custom": {Command: "custom-agent"},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("custom-agent"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.InstructionsFile != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q, want %q", rp.InstructionsFile, "AGENTS.md")
	}
}

func TestResolveProviderInstructionsFileExplicit(t *testing.T) {
	// Claude's explicit InstructionsFile should be preserved.
	agent := &Agent{Name: "mayor", Provider: "claude"}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.InstructionsFile != "CLAUDE.md" {
		t.Errorf("InstructionsFile = %q, want %q", rp.InstructionsFile, "CLAUDE.md")
	}
}

func TestResolveProviderPermissionModesDeepCopy(t *testing.T) {
	agent := &Agent{Name: "worker", Provider: "claude"}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}

	// Builtin Claude provider should have permission modes.
	if len(rp.PermissionModes) == 0 {
		t.Fatal("PermissionModes should not be empty for claude provider")
	}
	if _, ok := rp.PermissionModes["unrestricted"]; !ok {
		t.Error("PermissionModes missing 'unrestricted' key")
	}
	if _, ok := rp.PermissionModes["plan"]; !ok {
		t.Error("PermissionModes missing 'plan' key")
	}

	// Verify deep copy: mutating the resolved map must not affect builtins.
	rp.PermissionModes["injected"] = "malicious"
	builtins := BuiltinProviders()
	if _, ok := builtins["claude"].PermissionModes["injected"]; ok {
		t.Error("mutating ResolvedProvider.PermissionModes leaked into builtin ProviderSpec")
	}
}

func TestResolveProviderCustomPermissionModes(t *testing.T) {
	agent := &Agent{Name: "worker", Provider: "custom"}
	providers := map[string]ProviderSpec{
		"custom": {
			Command:    "my-agent",
			PromptMode: "arg",
			PermissionModes: map[string]string{
				"safe": "--safe-mode",
				"yolo": "--unsafe",
			},
		},
	}
	rp, err := ResolveProvider(agent, nil, providers, lookPathOnly("my-agent"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if len(rp.PermissionModes) != 2 {
		t.Fatalf("got %d permission modes, want 2", len(rp.PermissionModes))
	}
	if rp.PermissionModes["safe"] != "--safe-mode" {
		t.Errorf("safe mode = %q, want %q", rp.PermissionModes["safe"], "--safe-mode")
	}
}

// --- ResumeCommand ---

func TestResolveProviderResumeCommandFromSpec(t *testing.T) {
	agent := &Agent{Name: "worker", Provider: "custom"}
	providers := map[string]ProviderSpec{
		"custom": {
			Command:       "my-agent",
			ResumeCommand: "my-agent --resume {{.SessionKey}}",
		},
	}
	rp, err := ResolveProvider(agent, nil, providers, lookPathOnly("my-agent"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.ResumeCommand != "my-agent --resume {{.SessionKey}}" {
		t.Errorf("ResumeCommand = %q, want %q", rp.ResumeCommand, "my-agent --resume {{.SessionKey}}")
	}
}

func TestResolveProviderResumeCommandAgentOverride(t *testing.T) {
	agent := &Agent{
		Name:          "worker",
		Provider:      "claude",
		ResumeCommand: "claude --resume {{.SessionKey}} --custom-flag",
	}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.ResumeCommand != "claude --resume {{.SessionKey}} --custom-flag" {
		t.Errorf("ResumeCommand = %q, want agent override", rp.ResumeCommand)
	}
	// ResumeFlag should still be set from builtin (not cleared by ResumeCommand).
	if rp.ResumeFlag != "--resume" {
		t.Errorf("ResumeFlag = %q, want %q (builtin preserved)", rp.ResumeFlag, "--resume")
	}
}

// --- MergeProviderOverBuiltin field sync ---

// TestMergeProviderOverBuiltinFieldSync uses reflection to verify that
// MergeProviderOverBuiltin handles every field on ProviderSpec. When a
// new field is added to ProviderSpec, the merge function must be updated
// or this test will fail.
//
// Approach: set every ProviderSpec field to a non-zero value on the city
// side, merge over a zero-value base, and verify no field remains at its
// zero value. This catches fields that were added to the struct but not
// wired into the merge function.
func TestMergeProviderOverBuiltinFieldSync(t *testing.T) {
	basePtr := "builtin:custom"
	city := ProviderSpec{
		Base:                   &basePtr,
		ArgsAppend:             []string{"--extra"},
		OptionsSchemaMerge:     "by_key",
		DisplayName:            "Custom",
		Command:                "custom-cmd",
		Args:                   []string{"--flag"},
		PromptMode:             "flag",
		PromptFlag:             "--prompt",
		ReadyDelayMs:           5000,
		ReadyPromptPrefix:      "$ ",
		ProcessNames:           []string{"custom"},
		EmitsPermissionWarning: boolPtr(true),
		Env:                    map[string]string{"K": "V"},
		PathCheck:              "custom-bin",
		SupportsACP:            boolPtr(true),
		SupportsHooks:          boolPtr(true),
		InstructionsFile:       "CUSTOM.md",
		ResumeFlag:             "--resume",
		ResumeStyle:            "flag",
		ResumeCommand:          "custom-cmd --resume {{.SessionKey}}",
		SessionIDFlag:          "--session-id",
		PermissionModes:        map[string]string{"yolo": "--yolo"},
		OptionDefaults:         map[string]string{"permission_mode": "yolo"},
		OptionsSchema:          []ProviderOption{{Key: "model"}},
		PrintArgs:              []string{"-p"},
		TitleModel:             "haiku",
		ACPCommand:             "custom-acp",
		ACPArgs:                []string{"acp-mode"},
	}

	// Verify every field on city is non-zero (catches new fields not added to test data).
	cv := reflect.ValueOf(city)
	ct := cv.Type()
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		if cv.Field(i).IsZero() {
			t.Errorf("ProviderSpec field %q is zero in test city data — add it to the test", f.Name)
		}
	}

	// Merge city over a zero-value base.
	base := ProviderSpec{}
	result := MergeProviderOverBuiltin(base, city)

	// Every field on the result should be non-zero (city values should propagate).
	rv := reflect.ValueOf(result)
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		if rv.Field(i).IsZero() {
			t.Errorf("MergeProviderOverBuiltin did not propagate field %q from city to result", f.Name)
		}
	}
}

// TestOptionDefaultsTOMLThroughResolve exercises the full path:
// TOML config → LoadWithIncludes (parses + applies patches) → ResolveProvider → EffectiveDefaults.
//
// Three merge layers are verified:
//
//	Layer 1: schema-declared default       (permission_mode → "plan")
//	Layer 2: provider-level option_defaults (model → "sonnet", overriding schema "opus")
//	Layer 3: agent-level option_defaults    (permission_mode → "unrestricted", model → "haiku" via patch)
func TestOptionDefaultsTOMLThroughResolve(t *testing.T) {
	fs := fsys.NewFake()

	// city.toml: custom provider with options_schema + option_defaults,
	// an agent with its own option_defaults, and a patch that adds more.
	fs.Files["/city/city.toml"] = []byte(`
include = ["overrides.toml"]

[workspace]
name = "test"

[providers.testprov]
command = "testprov"
prompt_mode = "arg"

[[providers.testprov.options_schema]]
key = "model"
label = "Model"
type = "select"
default = "opus"

  [[providers.testprov.options_schema.choices]]
  value = "opus"
  label = "Opus"
  flag_args = ["--model", "opus"]

  [[providers.testprov.options_schema.choices]]
  value = "sonnet"
  label = "Sonnet"
  flag_args = ["--model", "sonnet"]

  [[providers.testprov.options_schema.choices]]
  value = "haiku"
  label = "Haiku"
  flag_args = ["--model", "haiku"]

[[providers.testprov.options_schema]]
key = "permission_mode"
label = "Permission Mode"
type = "select"
default = "plan"

  [[providers.testprov.options_schema.choices]]
  value = "plan"
  label = "Plan"
  flag_args = ["--permission-mode", "plan"]

  [[providers.testprov.options_schema.choices]]
  value = "unrestricted"
  label = "Unrestricted"
  flag_args = ["--dangerously-skip-permissions"]

[[providers.testprov.options_schema]]
key = "output_format"
label = "Output Format"
type = "select"
default = "text"

  [[providers.testprov.options_schema.choices]]
  value = "text"
  label = "Text"
  flag_args = ["--output", "text"]

  [[providers.testprov.options_schema.choices]]
  value = "json"
  label = "JSON"
  flag_args = ["--output", "json"]

# Provider-level overrides: model "sonnet" (instead of schema "opus"),
# output_format "json" (instead of schema "text").
# output_format is provider-only — no agent overrides it, proving the
# provider layer independently participates in the merge.
[providers.testprov.option_defaults]
model = "sonnet"
output_format = "json"

[[agent]]
name = "worker"
provider = "testprov"

# Agent-level overrides: permission_mode and model.
# model = "sonnet" here will be overwritten by the patch (model = "haiku"),
# proving patch-wins-over-agent overwrite semantics (not just additive insertion).
[agent.option_defaults]
permission_mode = "unrestricted"
model = "sonnet"
`)

	// Patch fragment: override agent's model to "haiku".
	fs.Files["/city/overrides.toml"] = []byte(`
[[patches.agent]]
name = "worker"

[patches.agent.option_defaults]
model = "haiku"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Find the worker agent.
	var worker *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "worker" {
			worker = &cfg.Agents[i]
			break
		}
	}
	if worker == nil {
		t.Fatal("worker agent not found in loaded config")
	}

	// After patching, agent.OptionDefaults should have both keys.
	if got := worker.OptionDefaults["permission_mode"]; got != "unrestricted" {
		t.Errorf("after patch: agent.OptionDefaults[permission_mode] = %q, want %q", got, "unrestricted")
	}
	if got := worker.OptionDefaults["model"]; got != "haiku" {
		t.Errorf("after patch: agent.OptionDefaults[model] = %q, want %q", got, "haiku")
	}

	// Resolve the provider — this merges all three layers into EffectiveDefaults.
	rp, err := ResolveProvider(worker, &cfg.Workspace, cfg.Providers, lookPathOnly("testprov"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}

	// Layer 1 (schema default "opus") overridden by Layer 2 (provider "sonnet"),
	// then overridden by Layer 3 (agent "haiku" via patch).
	// This also proves overwrite semantics: agent inline had model = "sonnet",
	// but the patch overwrites it to "haiku".
	if got := rp.EffectiveDefaults["model"]; got != "haiku" {
		t.Errorf("EffectiveDefaults[model] = %q, want %q (agent patch should override agent inline and provider default)", got, "haiku")
	}

	// Layer 1 (schema default "plan") overridden by Layer 3 (agent "unrestricted").
	if got := rp.EffectiveDefaults["permission_mode"]; got != "unrestricted" {
		t.Errorf("EffectiveDefaults[permission_mode] = %q, want %q (agent default should override schema default)", got, "unrestricted")
	}

	// Layer 2 (provider "json") is NOT overridden by any agent-level source.
	// This proves the provider layer independently participates in the merge —
	// without it, output_format would remain at schema default "text".
	if got := rp.EffectiveDefaults["output_format"]; got != "json" {
		t.Errorf("EffectiveDefaults[output_format] = %q, want %q (provider default should override schema default)", got, "json")
	}
}

// TestOptionDefaultsRigOverrideThroughResolve exercises the rig-level override
// path: TOML config → LoadWithIncludes (which internally calls ExpandPacks,
// applying AgentOverride) → ResolveProvider → EffectiveDefaults.
//
// This complements TestOptionDefaultsTOMLThroughResolve which tests the patch path.
// The rig override path is a separate code flow through applyAgentOverride (pack.go).
func TestOptionDefaultsRigOverrideThroughResolve(t *testing.T) {
	fs := fsys.NewFake()

	// Pack defines an agent with no option_defaults.
	fs.Files["/city/packs/svc/pack.toml"] = []byte(`[pack]
name = "svc"
schema = 1

[[agent]]
name = "coder"
provider = "testprov"
`)

	// city.toml: provider with options_schema + rig with override option_defaults.
	// No provider-level option_defaults — only schema defaults + agent overrides.
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[providers.testprov]
command = "testprov"
prompt_mode = "arg"

[[providers.testprov.options_schema]]
key = "model"
label = "Model"
type = "select"
default = "opus"

  [[providers.testprov.options_schema.choices]]
  value = "opus"
  label = "Opus"
  flag_args = ["--model", "opus"]

  [[providers.testprov.options_schema.choices]]
  value = "haiku"
  label = "Haiku"
  flag_args = ["--model", "haiku"]

[[providers.testprov.options_schema]]
key = "permission_mode"
label = "Permission Mode"
type = "select"
default = "plan"

  [[providers.testprov.options_schema.choices]]
  value = "plan"
  label = "Plan"
  flag_args = ["--permission-mode", "plan"]

  [[providers.testprov.options_schema.choices]]
  value = "unrestricted"
  label = "Unrestricted"
  flag_args = ["--dangerously-skip-permissions"]

[[rigs]]
name = "myrig"
path = "/repo"
includes = ["packs/svc"]

[[rigs.overrides]]
agent = "coder"

[rigs.overrides.option_defaults]
model = "haiku"
permission_mode = "unrestricted"
`)

	// LoadWithIncludes handles the full pipeline: parse TOML → apply patches →
	// ExpandPacks (which applies rig overrides). No separate ExpandPacks call needed.
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Find the expanded agent — verify exactly one exists (LoadWithIncludes
	// already expanded packs; a duplicate would indicate double expansion).
	var coder *Agent
	coderCount := 0
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "coder" {
			coder = &cfg.Agents[i]
			coderCount++
		}
	}
	if coder == nil {
		t.Fatal("coder agent not found after expansion")
	}
	if coderCount != 1 {
		t.Fatalf("expected exactly 1 coder agent, got %d (double expansion?)", coderCount)
	}

	// Override should have set agent.OptionDefaults.
	if got := coder.OptionDefaults["model"]; got != "haiku" {
		t.Errorf("after override: agent.OptionDefaults[model] = %q, want %q", got, "haiku")
	}

	// Resolve: no provider option_defaults, so only schema defaults + agent overrides.
	rp, err := ResolveProvider(coder, &cfg.Workspace, cfg.Providers, lookPathOnly("testprov"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}

	// Schema default "opus" overridden by agent override "haiku".
	if got := rp.EffectiveDefaults["model"]; got != "haiku" {
		t.Errorf("EffectiveDefaults[model] = %q, want %q", got, "haiku")
	}
	// Schema default "plan" overridden by agent override "unrestricted".
	if got := rp.EffectiveDefaults["permission_mode"]; got != "unrestricted" {
		t.Errorf("EffectiveDefaults[permission_mode] = %q, want %q", got, "unrestricted")
	}
}

func TestResolveProviderImportedPackProvidersMergeAndCityOverrideWins(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	helperDir := filepath.Join(cityDir, "assets", "helper")

	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "test-city"
schema = 2

[imports.helper]
source = "./assets/helper"

[providers.claude]
command = "claude"
args = ["--city"]
prompt_mode = "flag"
prompt_flag = "--city-prompt"
`)

	writeTestFile(t, helperDir, "pack.toml", `
[pack]
name = "helper"
schema = 2

[providers.claude]
command = "claude"
args = ["--helper"]
prompt_mode = "none"

[providers.codex]
command = "codex"
args = ["--from-helper"]
prompt_mode = "flag"
prompt_flag = "--message"
`)

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[[agent]]
name = "worker"
provider = "codex"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if _, ok := cfg.Providers["codex"]; !ok {
		t.Fatal("codex provider should be merged from imported pack")
	}
	if got := cfg.Providers["claude"].Args; !reflect.DeepEqual(got, []string{"--city"}) {
		t.Fatalf("claude provider args = %v, want city override", got)
	}

	var mayor, worker *Agent
	for i := range cfg.Agents {
		switch cfg.Agents[i].Name {
		case "mayor":
			mayor = &cfg.Agents[i]
		case "worker":
			worker = &cfg.Agents[i]
		}
	}
	if mayor == nil || worker == nil {
		t.Fatalf("expected mayor and worker agents, got mayor=%v worker=%v", mayor != nil, worker != nil)
	}

	mayorProvider, err := ResolveProvider(mayor, &cfg.Workspace, cfg.Providers, lookPathOnly("claude", "codex"))
	if err != nil {
		t.Fatalf("ResolveProvider(mayor): %v", err)
	}
	if !reflect.DeepEqual(mayorProvider.Args, []string{"--city"}) {
		t.Errorf("mayor provider args = %v, want city override", mayorProvider.Args)
	}
	if mayorProvider.PromptMode != "flag" {
		t.Errorf("mayor prompt mode = %q, want %q", mayorProvider.PromptMode, "flag")
	}
	if mayorProvider.PromptFlag != "--city-prompt" {
		t.Errorf("mayor prompt flag = %q, want %q", mayorProvider.PromptFlag, "--city-prompt")
	}

	workerProvider, err := ResolveProvider(worker, &cfg.Workspace, cfg.Providers, lookPathOnly("claude", "codex"))
	if err != nil {
		t.Fatalf("ResolveProvider(worker): %v", err)
	}
	if !reflect.DeepEqual(workerProvider.Args, []string{"--from-helper"}) {
		t.Errorf("worker provider args = %v, want imported provider args", workerProvider.Args)
	}
	if workerProvider.PromptMode != "flag" {
		t.Errorf("worker prompt mode = %q, want %q", workerProvider.PromptMode, "flag")
	}
	if workerProvider.PromptFlag != "--message" {
		t.Errorf("worker prompt flag = %q, want %q", workerProvider.PromptFlag, "--message")
	}
}
