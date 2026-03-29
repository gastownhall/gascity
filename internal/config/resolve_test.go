package config

import (
	"fmt"
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
	// Codex preset has prompt_mode = "none", so it should stay "none".
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("codex"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.PromptMode != "none" {
		t.Errorf("PromptMode = %q, want %q", rp.PromptMode, "none")
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

// --- Multi-provider rotation tests ---

func TestResolveProviderMultiProvider(t *testing.T) {
	agent := &Agent{
		Name:      "worker",
		Providers: []string{"claude", "codex"},
	}
	rp, err := ResolveProvider(agent, nil, nil, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	// Result must be one of the providers in the list.
	if rp.Name != "claude" && rp.Name != "codex" {
		t.Errorf("Name = %q, want one of [claude codex]", rp.Name)
	}
}

func TestResolveProviderSingleProviderFieldStillWorks(t *testing.T) {
	// When only provider (not providers) is set, backward compat holds.
	agent := &Agent{Name: "worker", Provider: "claude"}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Name != "claude" {
		t.Errorf("Name = %q, want %q", rp.Name, "claude")
	}
}

func TestResolveProviderProvidersOverridesProvider(t *testing.T) {
	// When both providers (list) and provider (string) are set,
	// providers + strategy takes priority.
	agent := &Agent{
		Name:      "worker",
		Provider:  "codex",
		Providers: []string{"claude"},
	}
	rp, err := ResolveProvider(agent, nil, nil, lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if rp.Name != "claude" {
		t.Errorf("Name = %q, want %q (providers should override provider)", rp.Name, "claude")
	}
}

func TestResolveProviderUnknownStrategy(t *testing.T) {
	agent := &Agent{
		Name:                 "worker",
		Providers:            []string{"claude", "codex"},
		ProviderStrategyName: "round-robin",
	}
	_, err := ResolveProvider(agent, nil, nil, lookPathAll)
	if err == nil {
		t.Fatal("expected error for unknown strategy name")
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

// TestResolveProviderRotationGeminiFlashClaudeSonnet verifies that provider
// rotation selects between city-level providers like gemini-flash and
// claude-sonnet when configured via the providers list.
func TestResolveProviderRotationGeminiFlashClaudeSonnet(t *testing.T) {
	cityProviders := map[string]ProviderSpec{
		"gemini-flash": {
			Command:    "gemini",
			PromptMode: "arg",
		},
		"claude-sonnet": {
			Command:    "claude",
			PromptMode: "arg",
		},
	}
	agent := &Agent{
		Name:      "worker",
		Providers: []string{"gemini-flash", "claude-sonnet"},
	}

	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		rp, err := ResolveProvider(agent, nil, cityProviders, lookPathAll)
		if err != nil {
			t.Fatalf("ResolveProvider iteration %d: %v", i, err)
		}
		if rp.Name != "gemini-flash" && rp.Name != "claude-sonnet" {
			t.Fatalf("unexpected provider %q, want gemini-flash or claude-sonnet", rp.Name)
		}
		seen[rp.Name] = true
	}

	if !seen["gemini-flash"] {
		t.Error("gemini-flash was never selected in 200 iterations")
	}
	if !seen["claude-sonnet"] {
		t.Error("claude-sonnet was never selected in 200 iterations")
	}
}

// TestResolveProviderRotationResolvesCorrectSpec verifies that each
// rotated provider resolves to its own ProviderSpec (correct command).
func TestResolveProviderRotationResolvesCorrectSpec(t *testing.T) {
	cityProviders := map[string]ProviderSpec{
		"gemini-flash": {
			Command:    "gemini",
			PromptMode: "stdin",
			Args:       []string{"--model", "flash"},
		},
		"claude-sonnet": {
			Command:    "claude",
			PromptMode: "arg",
			Args:       []string{"--model", "sonnet"},
		},
	}
	agent := &Agent{
		Name:      "worker",
		Providers: []string{"gemini-flash", "claude-sonnet"},
	}

	for i := 0; i < 50; i++ {
		rp, err := ResolveProvider(agent, nil, cityProviders, lookPathAll)
		if err != nil {
			t.Fatalf("ResolveProvider: %v", err)
		}
		switch rp.Name {
		case "gemini-flash":
			if rp.Command != "gemini" {
				t.Errorf("gemini-flash: Command = %q, want %q", rp.Command, "gemini")
			}
			if rp.PromptMode != "stdin" {
				t.Errorf("gemini-flash: PromptMode = %q, want %q", rp.PromptMode, "stdin")
			}
		case "claude-sonnet":
			if rp.Command != "claude" {
				t.Errorf("claude-sonnet: Command = %q, want %q", rp.Command, "claude")
			}
			if rp.PromptMode != "arg" {
				t.Errorf("claude-sonnet: PromptMode = %q, want %q", rp.PromptMode, "arg")
			}
		default:
			t.Fatalf("unexpected provider %q", rp.Name)
		}
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
