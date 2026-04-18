package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

var testBeaconTime = time.Unix(1_700_000_000, 0)

// TestTemplateParamsToConfigArgModeAppendsPromptAsBareArg verifies that
// when PromptMode is "arg" (the default), the prompt text is shell-quoted
// and placed in PromptSuffix without any flag prefix. The tmux adapter
// then appends this directly to the command: "provider <prompt>".
//
// This is the behavior that caused the OpenCode crash: the prompt text
// (containing beacon + behavioral instructions) was passed as a bare
// positional argument, which OpenCode v1.3+ interprets as a project
// directory path.
func TestTemplateParamsToConfigArgModeAppendsPromptAsBareArg(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "You are an agent. Do work.",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "opencode",
			Command:    "opencode",
			PromptMode: "arg",
		},
	}

	cfg := templateParamsToConfig(tp)

	// PromptSuffix should be a shell-quoted string without any flag.
	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty for arg mode with non-empty prompt")
	}
	// Must not start with a flag like --prompt.
	if strings.HasPrefix(cfg.PromptSuffix, "--") {
		t.Errorf("arg mode PromptSuffix should not start with a flag, got %q", cfg.PromptSuffix)
	}
	// The resulting command would be: opencode '<prompt text>'
	// For opencode this is fatal — it treats the arg as a project directory.
	fullCommand := cfg.Command + " " + cfg.PromptSuffix
	if !strings.HasPrefix(fullCommand, "opencode '") {
		t.Errorf("fullCommand = %q, expected opencode followed by quoted prompt", fullCommand)
	}
}

// TestTemplateParamsToConfigFlagModePrependsFlag verifies that when
// PromptMode is "flag", the PromptFlag is stored separately in
// runtime.Config.PromptFlag and PromptSuffix contains only the
// shell-quoted prompt text. The runtime (tmux adapter, ACP) combines
// them: "provider --prompt '<prompt text>'".
func TestTemplateParamsToConfigFlagModePrependsFlag(t *testing.T) {
	tp := TemplateParams{
		Command: "myprovider",
		Prompt:  "You are an agent.",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "myprovider",
			Command:    "myprovider",
			PromptMode: "flag",
			PromptFlag: "--prompt",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty for flag mode with non-empty prompt")
	}
	if cfg.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", cfg.PromptFlag, "--prompt")
	}
	// PromptSuffix should be just the quoted text, not the flag.
	if strings.HasPrefix(cfg.PromptSuffix, "--") {
		t.Errorf("flag mode PromptSuffix should not contain the flag prefix, got %q", cfg.PromptSuffix)
	}
	// The runtime reconstructs: myprovider --prompt '<text>'
	fullCommand := cfg.Command + " " + cfg.PromptFlag + " " + cfg.PromptSuffix
	if !strings.Contains(fullCommand, "--prompt '") {
		t.Errorf("fullCommand = %q, expected --prompt followed by quoted text", fullCommand)
	}
}

// TestTemplateParamsToConfigNoneModeUsesNudge verifies that when PromptMode is
// "none" and hooks are not available, startup instructions are delivered via
// runtime.Config.Nudge instead of PromptSuffix.
func TestTemplateParamsToConfigNoneModeUsesNudge(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "You are an agent. Do work.",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "opencode",
			Command:    "opencode",
			PromptMode: "none",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix should be empty for none mode, got %q", cfg.PromptSuffix)
	}
	if cfg.Nudge != "You are an agent. Do work." {
		t.Errorf("Nudge = %q, want startup prompt", cfg.Nudge)
	}
}

func TestTemplateParamsToConfigNoneModeWithHooksSkipsStartupNudge(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "You are an agent. Do work.",
		Hints: agent.StartupHints{
			Nudge: "existing nudge",
		},
		HookEnabled: true,
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "opencode",
			Command:       "opencode",
			PromptMode:    "none",
			SupportsHooks: true,
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix should be empty for none mode, got %q", cfg.PromptSuffix)
	}
	if cfg.Nudge != "existing nudge" {
		t.Errorf("Nudge = %q, want existing nudge only", cfg.Nudge)
	}
}

func TestTemplateParamsToConfigHookEnabledProviderSkipsLaunchPrompt(t *testing.T) {
	tests := []struct {
		name       string
		promptMode string
		promptFlag string
	}{
		{name: "arg mode", promptMode: "arg"},
		{name: "flag mode", promptMode: "flag", promptFlag: "--prompt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := TemplateParams{
				Command: "claude",
				Prompt:  "startup prompt",
				Hints: agent.StartupHints{
					Nudge: "existing nudge",
				},
				HookEnabled: true,
				ResolvedProvider: &config.ResolvedProvider{
					Name:          "claude",
					Command:       "claude",
					PromptMode:    tt.promptMode,
					PromptFlag:    tt.promptFlag,
					SupportsHooks: true,
				},
			}

			cfg := templateParamsToConfig(tp)

			if cfg.PromptSuffix != "" {
				t.Fatalf("PromptSuffix should be empty for hook-enabled startup, got %q", cfg.PromptSuffix)
			}
			if cfg.PromptFlag != "" {
				t.Fatalf("PromptFlag should be empty for hook-enabled startup, got %q", cfg.PromptFlag)
			}
			if cfg.Nudge != "existing nudge" {
				t.Fatalf("Nudge = %q, want existing nudge only", cfg.Nudge)
			}
		})
	}
}

func TestTemplateParamsToConfigNoneModePreservesExistingNudge(t *testing.T) {
	tp := TemplateParams{
		Command: "opencode",
		Prompt:  "startup prompt",
		Hints: agent.StartupHints{
			Nudge: "existing nudge",
		},
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "opencode",
			Command:    "opencode",
			PromptMode: "none",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix should be empty for none mode, got %q", cfg.PromptSuffix)
	}
	want := "startup prompt\n\n---\n\nexisting nudge"
	if cfg.Nudge != want {
		t.Errorf("Nudge = %q, want %q", cfg.Nudge, want)
	}
}

// TestTemplateParamsToConfigFlagModeEmptyPrompt verifies that when
// PromptMode is "flag" but the prompt is empty, no PromptSuffix is set.
func TestTemplateParamsToConfigFlagModeEmptyPrompt(t *testing.T) {
	tp := TemplateParams{
		Command: "myprovider",
		Prompt:  "",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "myprovider",
			Command:    "myprovider",
			PromptMode: "flag",
			PromptFlag: "--prompt",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptSuffix != "" {
		t.Errorf("PromptSuffix should be empty when prompt is empty, got %q", cfg.PromptSuffix)
	}
}

// TestTemplateParamsToConfigFlagModeNoFlagInSuffix verifies that flag
// mode stores the flag in PromptFlag, not in PromptSuffix. This is
// critical: the tmux adapter's file-expansion path needs them separate
// to reconstruct the command correctly for long prompts.
func TestTemplateParamsToConfigFlagModeNoFlagInSuffix(t *testing.T) {
	longPrompt := strings.Repeat("x", 2000) // Exceeds maxInlinePromptLen

	tp := TemplateParams{
		Command: "myprovider",
		Prompt:  longPrompt,
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "myprovider",
			Command:    "myprovider",
			PromptMode: "flag",
			PromptFlag: "--prompt",
		},
	}

	cfg := templateParamsToConfig(tp)

	if cfg.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", cfg.PromptFlag, "--prompt")
	}
	// PromptSuffix must contain only the quoted prompt, not the flag.
	if strings.Contains(cfg.PromptSuffix, "--prompt") {
		t.Errorf("PromptSuffix should not contain the flag, got %q (truncated)", cfg.PromptSuffix[:80])
	}
	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty")
	}
}

// TestTemplateParamsToConfigNilResolvedProvider verifies that
// templateParamsToConfig doesn't panic when ResolvedProvider is nil.
func TestTemplateParamsToConfigNilResolvedProvider(t *testing.T) {
	tp := TemplateParams{
		Command:          "echo",
		Prompt:           "hello",
		ResolvedProvider: nil,
	}

	cfg := templateParamsToConfig(tp)

	// Should fall back to bare arg mode (no flag prefix).
	if cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix should not be empty")
	}
	if strings.HasPrefix(cfg.PromptSuffix, "--") {
		t.Errorf("nil ResolvedProvider should not add flag prefix, got %q", cfg.PromptSuffix)
	}
}

func TestResolveTemplateNoneModeRetainsPromptForDeferredDelivery(t *testing.T) {
	cityPath := t.TempDir()
	fs := fsys.NewFake()
	fs.Files[cityPath+"/prompts/pool-worker.md"] = []byte("pool prompt body")

	params := &agentBuildParams{
		fs:              fs,
		cityName:        "bright-lights",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "bright-lights", Provider: "opencode"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/opencode", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}
	agent := &config.Agent{
		Name:           "opencode",
		PromptTemplate: "prompts/pool-worker.md",
		Provider:       "opencode",
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if tp.Prompt == "" {
		t.Fatal("Prompt should be preserved for PromptMode=none providers so it can be delivered via nudge")
	}
	if !strings.Contains(tp.Prompt, "pool prompt body") {
		t.Fatalf("Prompt missing rendered template body: %q", tp.Prompt)
	}
	if !strings.Contains(tp.Prompt, "[bright-lights] opencode") {
		t.Fatalf("Prompt missing beacon: %q", tp.Prompt)
	}
}

func TestResolveTemplateHookEnabledOpencodeOmitsPrimeInstruction(t *testing.T) {
	cityPath := t.TempDir()
	fs := fsys.NewFake()
	fs.Files[cityPath+"/prompts/mayor.md"] = []byte("mayor prompt body")

	params := &agentBuildParams{
		fs:              fs,
		cityName:        "bright-lights",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "bright-lights", Provider: "opencode", InstallAgentHooks: []string{"opencode"}},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/opencode", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}
	agent := &config.Agent{
		Name:           "mayor",
		PromptTemplate: "prompts/mayor.md",
		Provider:       "opencode",
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if !tp.HookEnabled {
		t.Fatal("HookEnabled = false, want true")
	}
	if !strings.Contains(tp.Prompt, "mayor prompt body") {
		t.Fatalf("Prompt missing rendered template body: %q", tp.Prompt)
	}
	if strings.Contains(tp.Prompt, "Run `gc prime`") {
		t.Fatalf("hook-enabled prompt should omit manual gc prime instruction: %q", tp.Prompt)
	}
}

func TestResolveTemplateExpandsPromptCommandTemplates(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	fs := fsys.NewFake()
	fs.Files[cityPath+"/prompts/worker.template.md"] = []byte("Work={{ .WorkQuery }}\nSling={{ .SlingQuery }}")

	params := &agentBuildParams{
		fs:              fs,
		cityName:        "",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Provider: "opencode"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/opencode", nil },
		rigs:            []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}
	agent := &config.Agent{
		Name:           "worker",
		Dir:            "demo",
		PromptTemplate: "prompts/worker.template.md",
		Provider:       "opencode",
		WorkQuery:      "echo {{.CityName}} {{.Rig}} {{.AgentBase}}",
		SlingQuery:     "dispatch {} --route={{.Rig}}/{{.AgentBase}} --city={{.CityName}}",
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if !strings.Contains(tp.Prompt, "Work=echo demo-city demo worker") {
		t.Fatalf("Prompt missing expanded WorkQuery: %q", tp.Prompt)
	}
	if !strings.Contains(tp.Prompt, "Sling=dispatch {} --route=demo/worker --city=demo-city") {
		t.Fatalf("Prompt missing expanded SlingQuery: %q", tp.Prompt)
	}
}

func TestResolveTemplateClaudeProjectsCityDotClaudeSettingsIntoRuntimeFile(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "prompts", "mayor.md"), []byte("mayor prompt body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".claude", "settings.json"), []byte(`{"custom": true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "bright-lights",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "bright-lights", Provider: "claude"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}
	agent := &config.Agent{
		Name:           "mayor",
		PromptTemplate: "prompts/mayor.md",
		Provider:       "claude",
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if !strings.Contains(tp.Command, `.gc/settings.json`) {
		t.Fatalf("command missing Claude settings path: %q", tp.Command)
	}
	runtimePath := filepath.Join(cityPath, ".gc", "settings.json")
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatalf("resolveTemplate did not materialize %s: %v", runtimePath, err)
	}
	rendered := string(data)
	if !strings.Contains(rendered, `"custom": true`) {
		t.Fatalf("runtime settings missing city .claude override:\n%s", rendered)
	}
	if !strings.Contains(rendered, "SessionStart") {
		t.Fatalf("runtime settings lost default Claude hooks:\n%s", rendered)
	}
}

func TestResolveTemplateImportedPackAppendFragmentsLayerBeforeCityDefaults(t *testing.T) {
	cityPath := t.TempDir()
	write := func(rel, data string) {
		path := filepath.Join(cityPath, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	write("city.toml", `
[workspace]
name = "test"
includes = ["packs/imported"]

[agent_defaults]
append_fragments = ["city-footer"]
`)
	write("packs/imported/pack.toml", `
[pack]
name = "imported"
schema = 2

[agent_defaults]
append_fragments = ["pack-footer"]

[[agent]]
name = "mayor"
provider = "claude"
scope = "city"
prompt_template = "agents/mayor/prompt.template.md"
`)
	write("packs/imported/agents/mayor/prompt.template.md", "Hello")
	write("packs/imported/agents/mayor/template-fragments/pack-footer.template.md", `{{ define "pack-footer" }}Pack Footer{{ end }}`)
	write("packs/imported/agents/mayor/template-fragments/city-footer.template.md", `{{ define "city-footer" }}City Footer{{ end }}`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var agentCfg config.Agent
	found := false
	for _, a := range cfg.Agents {
		if !a.Implicit && a.Name == "mayor" {
			agentCfg = a
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected explicit imported mayor agent, got %v", cfg.Agents)
	}
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "test",
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime:      testBeaconTime,
		packDirs:        cfg.PackDirs,
		globalFragments: cfg.Workspace.GlobalFragments,
		appendFragments: mergeFragmentLists(cfg.AgentDefaults.AppendFragments, cfg.AgentsDefaults.AppendFragments),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}

	tp, err := resolveTemplate(params, &agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	packIdx := strings.Index(tp.Prompt, "Pack Footer")
	cityIdx := strings.Index(tp.Prompt, "City Footer")
	if packIdx < 0 || cityIdx < 0 {
		t.Fatalf("prompt missing inherited fragments: %q", tp.Prompt)
	}
	if packIdx > cityIdx {
		t.Fatalf("pack fragment should render before city fragment: %q", tp.Prompt)
	}
}

func TestResolveTemplateNestedIncludedPackAppendFragmentsLayerBeforeCityDefaults(t *testing.T) {
	cityPath := t.TempDir()
	write := func(rel, data string) {
		path := filepath.Join(cityPath, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	write("city.toml", `
[workspace]
name = "test"
includes = ["packs/imported"]

[agent_defaults]
append_fragments = ["city-footer"]
`)
	write("packs/imported/pack.toml", `
[pack]
name = "imported"
schema = 2
includes = ["../base"]

[agent_defaults]
append_fragments = ["pack-footer"]
`)
	write("packs/base/pack.toml", `
[pack]
name = "base"
schema = 2

[[agent]]
name = "mayor"
provider = "claude"
scope = "city"
prompt_template = "agents/mayor/prompt.template.md"
`)
	write("packs/base/agents/mayor/prompt.template.md", "Hello")
	write("packs/base/agents/mayor/template-fragments/pack-footer.template.md", `{{ define "pack-footer" }}Pack Footer{{ end }}`)
	write("packs/base/agents/mayor/template-fragments/city-footer.template.md", `{{ define "city-footer" }}City Footer{{ end }}`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var agentCfg config.Agent
	found := false
	for _, a := range cfg.Agents {
		if !a.Implicit && a.Name == "mayor" {
			agentCfg = a
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected explicit imported mayor agent, got %v", cfg.Agents)
	}
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "test",
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime:      testBeaconTime,
		packDirs:        cfg.PackDirs,
		globalFragments: cfg.Workspace.GlobalFragments,
		appendFragments: mergeFragmentLists(cfg.AgentDefaults.AppendFragments, cfg.AgentsDefaults.AppendFragments),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}

	tp, err := resolveTemplate(params, &agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	packIdx := strings.Index(tp.Prompt, "Pack Footer")
	cityIdx := strings.Index(tp.Prompt, "City Footer")
	if packIdx < 0 || cityIdx < 0 {
		t.Fatalf("prompt missing inherited fragments: %q", tp.Prompt)
	}
	if packIdx > cityIdx {
		t.Fatalf("pack fragment should render before city fragment: %q", tp.Prompt)
	}
}

func TestResolveTemplateWrapperPackDefaultsDoNotBleedAcrossImports(t *testing.T) {
	cityPath := t.TempDir()
	write := func(rel, data string) {
		path := filepath.Join(cityPath, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	write("city.toml", `
[workspace]
name = "test"
includes = ["packs/wrapper"]

[agent_defaults]
append_fragments = ["city-footer"]
`)
	write("packs/wrapper/pack.toml", `
[pack]
name = "wrapper"
schema = 2

[agent_defaults]
append_fragments = ["wrapper-footer"]

[imports.dep]
source = "../dep"
`)
	write("packs/dep/pack.toml", `
[pack]
name = "dep"
schema = 2

[agent_defaults]
append_fragments = ["dep-footer"]

[[agent]]
name = "mayor"
provider = "claude"
scope = "city"
prompt_template = "agents/mayor/prompt.template.md"
`)
	write("packs/dep/agents/mayor/prompt.template.md", "Hello")
	write("packs/dep/agents/mayor/template-fragments/dep-footer.template.md", `{{ define "dep-footer" }}Dep Footer{{ end }}`)
	write("packs/dep/agents/mayor/template-fragments/wrapper-footer.template.md", `{{ define "wrapper-footer" }}Wrapper Footer{{ end }}`)
	write("packs/dep/agents/mayor/template-fragments/city-footer.template.md", `{{ define "city-footer" }}City Footer{{ end }}`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var agentCfg config.Agent
	found := false
	for _, a := range cfg.Agents {
		if !a.Implicit && a.BindingName == "dep" && a.Name == "mayor" {
			agentCfg = a
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected explicit imported dep.mayor agent, got %v", cfg.Agents)
	}
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "test",
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime:      testBeaconTime,
		packDirs:        cfg.PackDirs,
		globalFragments: cfg.Workspace.GlobalFragments,
		appendFragments: mergeFragmentLists(cfg.AgentDefaults.AppendFragments, cfg.AgentsDefaults.AppendFragments),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}

	tp, err := resolveTemplate(params, &agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if strings.Contains(tp.Prompt, "Wrapper Footer") {
		t.Fatalf("wrapper fragment should not bleed across imports: %q", tp.Prompt)
	}
	if !strings.Contains(tp.Prompt, "Dep Footer") || !strings.Contains(tp.Prompt, "City Footer") {
		t.Fatalf("prompt missing expected fragments: %q", tp.Prompt)
	}
}

func TestResolveTemplateIncludingPackDefaultsDoNotBleedAcrossNestedImportBoundaries(t *testing.T) {
	cityPath := t.TempDir()
	write := func(rel, data string) {
		path := filepath.Join(cityPath, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	write("city.toml", `
[workspace]
name = "test"
includes = ["packs/outer"]

[agent_defaults]
append_fragments = ["city-footer"]
`)
	write("packs/outer/pack.toml", `
[pack]
name = "outer"
schema = 2
includes = ["../mid"]

[agent_defaults]
append_fragments = ["outer-footer"]
`)
	write("packs/mid/pack.toml", `
[pack]
name = "mid"
schema = 2

[imports.dep]
source = "../dep"
`)
	write("packs/dep/pack.toml", `
[pack]
name = "dep"
schema = 2

[agent_defaults]
append_fragments = ["dep-footer"]

[[agent]]
name = "mayor"
provider = "claude"
scope = "city"
prompt_template = "agents/mayor/prompt.template.md"
`)
	write("packs/dep/agents/mayor/prompt.template.md", "Hello")
	write("packs/dep/agents/mayor/template-fragments/dep-footer.template.md", `{{ define "dep-footer" }}Dep Footer{{ end }}`)
	write("packs/dep/agents/mayor/template-fragments/outer-footer.template.md", `{{ define "outer-footer" }}Outer Footer{{ end }}`)
	write("packs/dep/agents/mayor/template-fragments/city-footer.template.md", `{{ define "city-footer" }}City Footer{{ end }}`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var agentCfg config.Agent
	found := false
	for _, a := range cfg.Agents {
		if !a.Implicit && a.BindingName == "dep" && a.Name == "mayor" {
			agentCfg = a
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected explicit imported dep.mayor agent, got %v", cfg.Agents)
	}
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "test",
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime:      testBeaconTime,
		packDirs:        cfg.PackDirs,
		globalFragments: cfg.Workspace.GlobalFragments,
		appendFragments: mergeFragmentLists(cfg.AgentDefaults.AppendFragments, cfg.AgentsDefaults.AppendFragments),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}

	tp, err := resolveTemplate(params, &agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if strings.Contains(tp.Prompt, "Outer Footer") {
		t.Fatalf("including-pack fragment should not bleed across nested import boundaries: %q", tp.Prompt)
	}
	if !strings.Contains(tp.Prompt, "Dep Footer") || !strings.Contains(tp.Prompt, "City Footer") {
		t.Fatalf("prompt missing expected fragments: %q", tp.Prompt)
	}
}
