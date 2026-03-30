package config

import (
	"fmt"
	"reflect"
	"testing"
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
	if rp.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", rp.PromptMode, "arg")
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
	cs := rp.CommandString()
	if cs != "claude --dangerously-skip-permissions" {
		t.Errorf("CommandString() = %q, want %q", cs, "claude --dangerously-skip-permissions")
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
	if rp.CommandString() != "codex --dangerously-bypass-approvals-and-sandbox" {
		t.Errorf("CommandString() = %q, want codex command", rp.CommandString())
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
	if spec.SupportsHooks != builtinCopilot.SupportsHooks {
		t.Errorf("SupportsHooks = %v, want %v (inherited)", spec.SupportsHooks, builtinCopilot.SupportsHooks)
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

// --- mergeProviderOverBuiltin tests ---

func TestMergeProviderOverBuiltin(t *testing.T) {
	base := ProviderSpec{
		Command:           "copilot",
		Args:              []string{"--yolo"},
		PromptMode:        "flag",
		PromptFlag:        "--prompt",
		ReadyDelayMs:      5000,
		ReadyPromptPrefix: "❯ ",
		SupportsACP:       true,
		Env:               map[string]string{"BASE_KEY": "base_val"},
		PermissionModes:   map[string]string{"unrestricted": "--yolo"},
	}

	city := ProviderSpec{
		Command: "copilot",
		Args:    []string{"--yolo", "--model", "claude-haiku-4.5"},
		Env:     map[string]string{"CITY_KEY": "city_val"},
	}

	result := mergeProviderOverBuiltin(base, city)

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
	if !result.SupportsACP {
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
	if !AgentHasHooks(agent, ws, "claude") {
		t.Error("claude should always have hooks")
	}
}

func TestAgentHasHooks_InstallHooksMatch(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{InstallAgentHooks: []string{"gemini", "opencode"}}
	if !AgentHasHooks(agent, ws, "gemini") {
		t.Error("gemini with install_agent_hooks should have hooks")
	}
}

func TestAgentHasHooks_InstallHooksNoMatch(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{InstallAgentHooks: []string{"claude"}}
	if AgentHasHooks(agent, ws, "codex") {
		t.Error("codex not in install_agent_hooks should not have hooks")
	}
}

func TestAgentHasHooks_NoHooksByDefault(t *testing.T) {
	agent := &Agent{Name: "worker"}
	ws := &Workspace{Name: "test"}
	if AgentHasHooks(agent, ws, "codex") {
		t.Error("codex with no install_agent_hooks should not have hooks")
	}
}

func TestAgentHasHooks_ExplicitOverrideTrue(t *testing.T) {
	yes := true
	agent := &Agent{Name: "worker", HooksInstalled: &yes}
	ws := &Workspace{Name: "test"}
	if !AgentHasHooks(agent, ws, "codex") {
		t.Error("hooks_installed=true should override to true")
	}
}

func TestAgentHasHooks_ExplicitOverrideFalse(t *testing.T) {
	no := false
	agent := &Agent{Name: "worker", HooksInstalled: &no}
	ws := &Workspace{Name: "test"}
	// Even claude should be overridden to false when explicit.
	if AgentHasHooks(agent, ws, "claude") {
		t.Error("hooks_installed=false should override even claude")
	}
}

func TestAgentHasHooks_AgentLevelInstallHooks(t *testing.T) {
	agent := &Agent{Name: "worker", InstallAgentHooks: []string{"copilot"}}
	ws := &Workspace{InstallAgentHooks: []string{"claude"}}
	// Agent-level overrides workspace — only copilot in list.
	if !AgentHasHooks(agent, ws, "copilot") {
		t.Error("agent install_agent_hooks should be checked")
	}
	if AgentHasHooks(agent, ws, "opencode") {
		t.Error("opencode not in agent install_agent_hooks")
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

// --- mergeProviderOverBuiltin field sync ---

// TestMergeProviderOverBuiltinFieldSync uses reflection to verify that
// mergeProviderOverBuiltin handles every field on ProviderSpec. When a
// new field is added to ProviderSpec, the merge function must be updated
// or this test will fail.
//
// Approach: set every ProviderSpec field to a non-zero value on the city
// side, merge over a zero-value base, and verify no field remains at its
// zero value. This catches fields that were added to the struct but not
// wired into the merge function.
func TestMergeProviderOverBuiltinFieldSync(t *testing.T) {
	city := ProviderSpec{
		DisplayName:            "Custom",
		Command:                "custom-cmd",
		Args:                   []string{"--flag"},
		PromptMode:             "flag",
		PromptFlag:             "--prompt",
		ReadyDelayMs:           5000,
		ReadyPromptPrefix:      "$ ",
		ProcessNames:           []string{"custom"},
		EmitsPermissionWarning: true,
		Env:                    map[string]string{"K": "V"},
		PathCheck:              "custom-bin",
		SupportsACP:            true,
		SupportsHooks:          true,
		InstructionsFile:       "CUSTOM.md",
		ResumeFlag:             "--resume",
		ResumeStyle:            "flag",
		ResumeCommand:          "custom-cmd --resume {{.SessionKey}}",
		SessionIDFlag:          "--session-id",
		PermissionModes:        map[string]string{"yolo": "--yolo"},
		OptionsSchema:          []ProviderOption{{Key: "model"}},
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
	result := mergeProviderOverBuiltin(base, city)

	// Every field on the result should be non-zero (city values should propagate).
	rv := reflect.ValueOf(result)
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		if rv.Field(i).IsZero() {
			t.Errorf("mergeProviderOverBuiltin did not propagate field %q from city to result", f.Name)
		}
	}
}
