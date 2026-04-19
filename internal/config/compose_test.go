package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadWithIncludes_NoIncludes(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[agent]]
name = "mayor"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 1 {
		t.Fatalf("len(explicit Agents) = %d, want 1", len(explicit))
	}
	if explicit[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", explicit[0].Name, "mayor")
	}
	if prov.Root != "/city/city.toml" {
		t.Errorf("Root = %q, want %q", prov.Root, "/city/city.toml")
	}
	if len(prov.Sources) != 1 {
		t.Errorf("len(Sources) = %d, want 1", len(prov.Sources))
	}
	if len(prov.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", prov.Warnings)
	}
	// Include should be cleared from the result.
	if cfg.Include != nil {
		t.Errorf("Include should be nil, got %v", cfg.Include)
	}
}

func TestLoadWithIncludes_RootPackDefaultRigImportsPreserveOrder(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "test"
schema = 2

[defaults.rig.imports.z-pack]
source = "packs/z-pack"

[defaults.rig.imports.a-pack]
source = "packs/a-pack"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	want := []string{"packs/z-pack", "packs/a-pack"}
	if !reflect.DeepEqual(cfg.Workspace.DefaultRigIncludes, want) {
		t.Fatalf("DefaultRigIncludes = %v, want %v", cfg.Workspace.DefaultRigIncludes, want)
	}
}

func TestLoadWithIncludes_SkipsSystemPackWhenReachableFromRootImport(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, data string) {
		path := filepath.Join(dir, rel)
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
`)
	write("pack.toml", `
[pack]
name = "test"
schema = 2

[imports.gs]
source = "./packs/gastown"
`)
	write("packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 2
includes = ["../maintenance"]

[[agent]]
name = "mayor"
scope = "city"
`)
	write("packs/maintenance/pack.toml", `
[pack]
name = "maintenance"
schema = 2

[[agent]]
name = "dog"
scope = "city"
`)
	write("system/maintenance/pack.toml", `
[pack]
name = "maintenance"
schema = 2

[[agent]]
name = "dog"
scope = "city"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"), filepath.Join(dir, "system", "maintenance"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	found := map[string]bool{}
	for _, a := range explicitAgents(cfg.Agents) {
		found[a.QualifiedName()] = true
	}
	if !found["gs.mayor"] {
		t.Fatalf("missing gs.mayor: %v", found)
	}
	if !found["gs.dog"] {
		t.Fatalf("missing gs.dog from imported maintenance closure: %v", found)
	}
	if found["dog"] {
		t.Fatalf("system maintenance should have been skipped when root import already reaches maintenance: %v", found)
	}
}

func TestLoadWithIncludes_CityPackSchema2(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "test"
schema = 2

[[agent]]
name = "mayor"
`)

	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 1 {
		t.Fatalf("len(explicit Agents) = %d, want 1", len(explicit))
	}
	if explicit[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", explicit[0].Name, "mayor")
	}
	if len(prov.Sources) != 2 {
		t.Errorf("len(Sources) = %d, want 2", len(prov.Sources))
	}
	if prov.Agents["mayor"] != "/city/pack.toml" {
		t.Errorf("mayor source = %q, want /city/pack.toml", prov.Agents["mayor"])
	}
}

func TestLoadWithIncludes_ConcatAgents(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["agents/workers.toml"]

[workspace]
name = "test"

[[agent]]
name = "mayor"
`)
	fs.Files["/city/agents/workers.toml"] = []byte(`
[[agent]]
name = "worker"
dir = "project"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 2 {
		t.Fatalf("len(explicit Agents) = %d, want 2", len(explicit))
	}
	if explicit[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", explicit[0].Name, "mayor")
	}
	if explicit[1].Name != "worker" {
		t.Errorf("Agents[1].Name = %q, want %q", explicit[1].Name, "worker")
	}
	if explicit[1].Dir != "project" {
		t.Errorf("Agents[1].Dir = %q, want %q", explicit[1].Dir, "project")
	}

	// Provenance.
	if prov.Agents["mayor"] != "/city/city.toml" {
		t.Errorf("mayor source = %q, want root", prov.Agents["mayor"])
	}
	if prov.Agents["project/worker"] != "/city/agents/workers.toml" {
		t.Errorf("worker source = %q, want fragment", prov.Agents["project/worker"])
	}
	if len(prov.Sources) != 2 {
		t.Errorf("len(Sources) = %d, want 2", len(prov.Sources))
	}
}

func TestLoadWithIncludes_AgentDefaultsAliasFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["defaults.toml"]

[workspace]
name = "test"

[[agent]]
name = "mayor"
`)
	fs.Files["/city/defaults.toml"] = []byte(`
[agents]
default_sling_formula = "mol-focus-review"
append_fragments = ["command-glossary"]
skills = ["shared-skill", "common-skill"]
mcp = ["shared-mcp", "common-mcp"]
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.AgentDefaults.DefaultSlingFormula != "mol-focus-review" {
		t.Errorf("AgentDefaults.DefaultSlingFormula = %q, want %q", cfg.AgentDefaults.DefaultSlingFormula, "mol-focus-review")
	}
	if got := cfg.AgentDefaults.AppendFragments; len(got) != 1 || got[0] != "command-glossary" {
		t.Errorf("AgentDefaults.AppendFragments = %v, want [command-glossary]", got)
	}
	if !reflect.DeepEqual(cfg.AgentsDefaults, AgentDefaults{}) {
		t.Errorf("AgentsDefaults = %#v, want zero value after normalization", cfg.AgentsDefaults)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			found = true
			if got := a.EffectiveDefaultSlingFormula(); got != "mol-focus-review" {
				t.Errorf("mayor EffectiveDefaultSlingFormula = %q, want %q", got, "mol-focus-review")
			}
		}
	}
	if !found {
		t.Fatal("agent 'mayor' not found")
	}
}

func TestLoadWithIncludes_ConcatRigs(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["rigs/hw.toml"]

[workspace]
name = "test"

[[rigs]]
name = "project-a"
path = "/tmp/a"
`)
	fs.Files["/city/rigs/hw.toml"] = []byte(`
[[rigs]]
name = "hello-world"
path = "/tmp/hw"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Rigs) != 2 {
		t.Fatalf("len(Rigs) = %d, want 2", len(cfg.Rigs))
	}
	if cfg.Rigs[0].Name != "project-a" {
		t.Errorf("Rigs[0].Name = %q, want %q", cfg.Rigs[0].Name, "project-a")
	}
	if cfg.Rigs[1].Name != "hello-world" {
		t.Errorf("Rigs[1].Name = %q, want %q", cfg.Rigs[1].Name, "hello-world")
	}
	if prov.Rigs["project-a"] != "/city/city.toml" {
		t.Errorf("project-a source = %q, want root", prov.Rigs["project-a"])
	}
	if prov.Rigs["hello-world"] != "/city/rigs/hw.toml" {
		t.Errorf("hello-world source = %q, want fragment", prov.Rigs["hello-world"])
	}
}

func TestLoadWithIncludes_MultipleFragments(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["a.toml", "b.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/a.toml"] = []byte(`
[[agent]]
name = "alpha"
`)
	fs.Files["/city/b.toml"] = []byte(`
[[agent]]
name = "beta"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 2 {
		t.Fatalf("len(explicit Agents) = %d, want 2", len(explicit))
	}
	if explicit[0].Name != "alpha" {
		t.Errorf("Agents[0].Name = %q, want %q", explicit[0].Name, "alpha")
	}
	if explicit[1].Name != "beta" {
		t.Errorf("Agents[1].Name = %q, want %q", explicit[1].Name, "beta")
	}
	if len(prov.Sources) != 3 {
		t.Errorf("len(Sources) = %d, want 3", len(prov.Sources))
	}
}

func TestLoadWithIncludes_RecursiveIncludeFails(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
include = ["other.toml"]

[[agent]]
name = "worker"
`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("expected error for recursive includes")
	}
	if !strings.Contains(err.Error(), "not allowed in fragments") {
		t.Errorf("error = %q, want contains 'not allowed in fragments'", err)
	}
}

func TestLoadWithIncludes_FragmentNotFound(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["missing.toml"]

[workspace]
name = "test"
`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("expected error for missing fragment")
	}
	if !strings.Contains(err.Error(), "missing.toml") {
		t.Errorf("error = %q, want mention of missing.toml", err)
	}
}

func TestLoadWithIncludes_FragmentParseError(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["bad.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/bad.toml"] = []byte(`{{invalid toml`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "bad.toml") {
		t.Errorf("error = %q, want mention of bad.toml", err)
	}
}

func TestLoadWithIncludes_ProviderDeepMerge(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["override.toml"]

[workspace]
name = "test"

[providers.custom]
command = "my-agent"
prompt_mode = "arg"
ready_delay_ms = 5000
`)
	fs.Files["/city/override.toml"] = []byte(`
[providers.custom]
ready_delay_ms = 10000
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	p := cfg.Providers["custom"]
	// Unchanged fields preserved.
	if p.Command != "my-agent" {
		t.Errorf("Command = %q, want %q", p.Command, "my-agent")
	}
	if p.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "arg")
	}
	// Overridden field.
	if p.ReadyDelayMs != 10000 {
		t.Errorf("ReadyDelayMs = %d, want 10000", p.ReadyDelayMs)
	}
	// Collision warning for ready_delay_ms.
	if len(prov.Warnings) != 1 {
		t.Fatalf("len(Warnings) = %d, want 1: %v", len(prov.Warnings), prov.Warnings)
	}
	if !strings.Contains(prov.Warnings[0], "ready_delay_ms") {
		t.Errorf("warning = %q, want mention of ready_delay_ms", prov.Warnings[0])
	}
}

func TestLoadWithIncludes_ProviderAddsNew(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["providers.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/providers.toml"] = []byte(`
[providers.custom]
command = "my-agent"
prompt_mode = "flag"
prompt_flag = "--prompt"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	p, ok := cfg.Providers["custom"]
	if !ok {
		t.Fatal("provider 'custom' not found")
	}
	if p.Command != "my-agent" {
		t.Errorf("Command = %q, want %q", p.Command, "my-agent")
	}
	if p.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", p.PromptFlag, "--prompt")
	}
	// No collision warnings for new provider.
	if len(prov.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", prov.Warnings)
	}
}

func TestLoadWithIncludes_ProviderEnvMerge(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["env.toml"]

[workspace]
name = "test"

[providers.custom]
command = "agent"

[providers.custom.env]
KEY_A = "1"
KEY_B = "2"
`)
	fs.Files["/city/env.toml"] = []byte(`
[providers.custom.env]
KEY_B = "override"
KEY_C = "3"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	env := cfg.Providers["custom"].Env
	if env["KEY_A"] != "1" {
		t.Errorf("KEY_A = %q, want %q", env["KEY_A"], "1")
	}
	if env["KEY_B"] != "override" {
		t.Errorf("KEY_B = %q, want %q", env["KEY_B"], "override")
	}
	if env["KEY_C"] != "3" {
		t.Errorf("KEY_C = %q, want %q", env["KEY_C"], "3")
	}
	// KEY_B collision warning.
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "KEY_B") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about KEY_B collision, got: %v", prov.Warnings)
	}
}

func TestLoadWithIncludes_WorkspaceMerge(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["ws.toml"]

[workspace]
name = "bright-lights"
provider = "claude"
`)
	fs.Files["/city/ws.toml"] = []byte(`
[workspace]
provider = "gemini"
session_template = "custom-{{.Agent}}"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// Name unchanged (fragment didn't define it).
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	// Provider overridden.
	if cfg.Workspace.Provider != "gemini" {
		t.Errorf("Provider = %q, want %q", cfg.Workspace.Provider, "gemini")
	}
	// SessionTemplate added from fragment.
	if cfg.Workspace.SessionTemplate != "custom-{{.Agent}}" {
		t.Errorf("SessionTemplate = %q, want %q", cfg.Workspace.SessionTemplate, "custom-{{.Agent}}")
	}
	// Provenance tracking.
	if prov.Workspace["name"] != "/city/city.toml" {
		t.Errorf("name source = %q, want root", prov.Workspace["name"])
	}
	if prov.Workspace["provider"] != "/city/ws.toml" {
		t.Errorf("provider source = %q, want fragment", prov.Workspace["provider"])
	}
	// Collision warning for provider.
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "workspace.provider") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about workspace.provider collision, got: %v", prov.Warnings)
	}
}

func TestLoadWithIncludes_PromptTemplatePathAdjustment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["agents/team.toml"]

[workspace]
name = "test"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	fs.Files["/city/agents/team.toml"] = []byte(`
[[agent]]
name = "worker"
dir = "project"
prompt_template = "prompts/worker.md"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// Root agent's path unchanged (already city-root-relative).
	if cfg.Agents[0].PromptTemplate != "prompts/mayor.md" {
		t.Errorf("mayor prompt_template = %q, want %q",
			cfg.Agents[0].PromptTemplate, "prompts/mayor.md")
	}
	// Fragment agent's path adjusted to city-root-relative.
	// "prompts/worker.md" relative to /city/agents/ → "agents/prompts/worker.md"
	want := "agents/prompts/worker.md"
	if cfg.Agents[1].PromptTemplate != want {
		t.Errorf("worker prompt_template = %q, want %q",
			cfg.Agents[1].PromptTemplate, want)
	}
}

func TestLoadWithIncludes_CityRootPath(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["agents/team.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/agents/team.toml"] = []byte(`
[[agent]]
name = "worker"
prompt_template = "//prompts/worker.md"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// "//" prefix resolves to city root.
	if cfg.Agents[0].PromptTemplate != "prompts/worker.md" {
		t.Errorf("prompt_template = %q, want %q",
			cfg.Agents[0].PromptTemplate, "prompts/worker.md")
	}
}

func TestLoadWithIncludes_IncludePreserved(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["a.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/a.toml"] = []byte(`
[[agent]]
name = "worker"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// Include must be preserved so Marshal() round-trips city.toml correctly.
	if len(cfg.Include) != 1 || cfg.Include[0] != "a.toml" {
		t.Errorf("Include = %v, want [a.toml]", cfg.Include)
	}
}

func TestLoadWithIncludes_SimpleSectionOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[beads]
provider = "bd"
`)
	fs.Files["/city/infra.toml"] = []byte(`
[beads]
provider = "file"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Beads.Provider != "file" {
		t.Errorf("Beads.Provider = %q, want %q", cfg.Beads.Provider, "file")
	}
}

func TestResolveConfigPath(t *testing.T) {
	tests := []struct {
		name     string
		p        string
		declDir  string
		cityRoot string
		want     string
	}{
		{"relative", "agents/mayor.toml", "/city", "/city", "/city/agents/mayor.toml"},
		{"absolute", "/etc/config.toml", "/city", "/city", "/etc/config.toml"},
		{"city-root", "//prompts/mayor.md", "/city/agents", "/city", "/city/prompts/mayor.md"},
		{"nested-relative", "sub/file.toml", "/city/agents", "/city", "/city/agents/sub/file.toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveConfigPath(tt.p, tt.declDir, tt.cityRoot)
			if got != tt.want {
				t.Errorf("resolveConfigPath(%q, %q, %q) = %q, want %q",
					tt.p, tt.declDir, tt.cityRoot, got, tt.want)
			}
		})
	}
}

func TestAdjustFragmentPath(t *testing.T) {
	tests := []struct {
		name     string
		p        string
		fragDir  string
		cityRoot string
		want     string
	}{
		{"empty", "", "/city/agents", "/city", ""},
		{"absolute", "/abs/path.md", "/city/agents", "/city", "/abs/path.md"},
		{"city-root", "//prompts/foo.md", "/city/agents", "/city", "prompts/foo.md"},
		{"relative", "prompts/foo.md", "/city/agents", "/city", "agents/prompts/foo.md"},
		{"same-dir", "foo.md", "/city", "/city", "foo.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adjustFragmentPath(tt.p, tt.fragDir, tt.cityRoot)
			if got != tt.want {
				t.Errorf("adjustFragmentPath(%q, %q, %q) = %q, want %q",
					tt.p, tt.fragDir, tt.cityRoot, got, tt.want)
			}
		})
	}
}

func TestLoadWithIncludes_WorkspaceProvenanceTracking(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
provider = "claude"
`)
	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if prov.Workspace["name"] != "/city/city.toml" {
		t.Errorf("name source = %q, want root", prov.Workspace["name"])
	}
	if prov.Workspace["provider"] != "/city/city.toml" {
		t.Errorf("provider source = %q, want root", prov.Workspace["provider"])
	}
	// session_template not defined → not in provenance.
	if _, ok := prov.Workspace["session_template"]; ok {
		t.Error("session_template should not be in provenance (not defined)")
	}
}

func TestLoadWithIncludes_MergePacks(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["remote.toml"]

[workspace]
name = "test"

[packs.gastown]
source = "https://github.com/example/gastown"
ref = "v1.0.0"
`)
	fs.Files["/city/remote.toml"] = []byte(`
[packs.ralph]
source = "https://github.com/example/ralph"
ref = "main"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Packs) != 2 {
		t.Fatalf("len(Packs) = %d, want 2", len(cfg.Packs))
	}
	if cfg.Packs["gastown"].Source != "https://github.com/example/gastown" {
		t.Errorf("gastown source = %q", cfg.Packs["gastown"].Source)
	}
	if cfg.Packs["ralph"].Source != "https://github.com/example/ralph" {
		t.Errorf("ralph source = %q", cfg.Packs["ralph"].Source)
	}
	if len(prov.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", prov.Warnings)
	}
}

func TestLoadWithIncludes_MergePacks_Collision(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["override.toml"]

[workspace]
name = "test"

[packs.gastown]
source = "https://github.com/example/gastown"
ref = "v1.0.0"
`)
	fs.Files["/city/override.toml"] = []byte(`
[packs.gastown]
source = "https://github.com/other/gastown"
ref = "v2.0.0"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// Last writer wins.
	if cfg.Packs["gastown"].Ref != "v2.0.0" {
		t.Errorf("gastown ref = %q, want v2.0.0", cfg.Packs["gastown"].Ref)
	}
	// Collision warning.
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "gastown") && strings.Contains(w, "redefined") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected collision warning for gastown, got: %v", prov.Warnings)
	}
}

func TestLoadWithIncludes_WorkspaceInstallAgentHooksMerge(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["frag.toml"]

[workspace]
name = "test"
install_agent_hooks = ["claude"]
`)
	fs.Files["/city/frag.toml"] = []byte(`
[workspace]
install_agent_hooks = ["gemini", "copilot"]
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// Fragment replaces root.
	got := cfg.Workspace.InstallAgentHooks
	if len(got) != 2 || got[0] != "gemini" || got[1] != "copilot" {
		t.Errorf("InstallAgentHooks = %v, want [gemini copilot]", got)
	}
	// Provenance tracks the override.
	if prov.Workspace["install_agent_hooks"] != "/city/frag.toml" {
		t.Errorf("provenance = %q, want frag.toml", prov.Workspace["install_agent_hooks"])
	}
	// Should produce a collision warning.
	foundWarning := false
	for _, w := range prov.Warnings {
		if w == `workspace.install_agent_hooks redefined by "/city/frag.toml"` {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected collision warning, got: %v", prov.Warnings)
	}
}

func TestLoadWithIncludes_WorkspaceInstallAgentHooksProvenance(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
install_agent_hooks = ["claude"]
`)
	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if prov.Workspace["install_agent_hooks"] != "/city/city.toml" {
		t.Errorf("provenance = %q, want root", prov.Workspace["install_agent_hooks"])
	}
}

func TestAdjustAgentPaths_SourceDirSet(t *testing.T) {
	agents := []Agent{
		{Name: "worker", PromptTemplate: "prompts/worker.md"},
		{Name: "boss"},
	}
	adjustAgentPaths(agents, "/city/fragments", "/city")

	// Both agents should get SourceDir set to fragment dir.
	for _, a := range agents {
		if a.SourceDir != "/city/fragments" {
			t.Errorf("agent %q: SourceDir = %q, want /city/fragments", a.Name, a.SourceDir)
		}
	}
}

func TestAdjustAgentPaths_SessionSetupScriptAdjusted(t *testing.T) {
	agents := []Agent{
		{Name: "worker", SessionSetupScript: "scripts/setup.sh"},
		{Name: "boss", SessionSetupScript: "//scripts/global.sh"},
		{Name: "plain"},
	}
	adjustAgentPaths(agents, "/city/fragments", "/city")

	// Relative path: resolved fragment-relative → city-root-relative.
	if agents[0].SessionSetupScript != "fragments/scripts/setup.sh" {
		t.Errorf("worker script = %q, want fragments/scripts/setup.sh", agents[0].SessionSetupScript)
	}
	// "//" path: resolved to city root.
	if agents[1].SessionSetupScript != "scripts/global.sh" {
		t.Errorf("boss script = %q, want scripts/global.sh", agents[1].SessionSetupScript)
	}
	// Empty: unchanged.
	if agents[2].SessionSetupScript != "" {
		t.Errorf("plain script = %q, want empty", agents[2].SessionSetupScript)
	}
}

func TestAdjustAgentPaths_OverlayDirAdjusted(t *testing.T) {
	agents := []Agent{
		{Name: "worker", OverlayDir: "overlays/worker"},
		{Name: "boss", OverlayDir: "//overlays/global"},
		{Name: "plain"},
	}
	adjustAgentPaths(agents, "/city/fragments", "/city")

	// Relative path: resolved fragment-relative → city-root-relative.
	if agents[0].OverlayDir != "fragments/overlays/worker" {
		t.Errorf("worker overlay = %q, want fragments/overlays/worker", agents[0].OverlayDir)
	}
	// "//" path: resolved to city root.
	if agents[1].OverlayDir != "overlays/global" {
		t.Errorf("boss overlay = %q, want overlays/global", agents[1].OverlayDir)
	}
	// Empty: unchanged.
	if agents[2].OverlayDir != "" {
		t.Errorf("plain overlay = %q, want empty", agents[2].OverlayDir)
	}
}

func TestLoadWithIncludes_MultipleCityPacks(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/packs/alpha/pack.toml"] = []byte(`
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "agent-a"
`)
	fs.Files["/city/packs/beta/pack.toml"] = []byte(`
[pack]
name = "beta"
schema = 1

[[agent]]
name = "agent-b"
`)
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
includes = ["packs/alpha", "packs/beta"]

[[agent]]
name = "existing"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Should have 3 explicit agents: agent-a, agent-b (from packs), then existing.
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 3 {
		t.Fatalf("got %d explicit agents, want 3", len(explicit))
	}
	if explicit[0].Name != "agent-a" {
		t.Errorf("first agent = %q, want agent-a", explicit[0].Name)
	}
	if explicit[1].Name != "agent-b" {
		t.Errorf("second agent = %q, want agent-b", explicit[1].Name)
	}
	if explicit[2].Name != "existing" {
		t.Errorf("third agent = %q, want existing", explicit[2].Name)
	}

	// Provenance should track city pack agents.
	if _, ok := prov.Agents["agent-a"]; !ok {
		t.Error("provenance should track agent-a")
	}
	if _, ok := prov.Agents["agent-b"]; !ok {
		t.Error("provenance should track agent-b")
	}
}

func TestLoadWithIncludes_MultipleRigPacks(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/packs/alpha/pack.toml"] = []byte(`
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "worker-a"
`)
	fs.Files["/city/packs/beta/pack.toml"] = []byte(`
[pack]
name = "beta"
schema = 1

[[agent]]
name = "worker-b"
`)
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[agent]]
name = "mayor"

[[rigs]]
name = "hw"
path = "/home/user/hw"
includes = ["packs/alpha", "packs/beta"]
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Should have 3 explicit agents: mayor, then worker-a and worker-b from rig packs.
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 3 {
		t.Fatalf("got %d explicit agents, want 3", len(explicit))
	}
	if explicit[0].Name != "mayor" {
		t.Errorf("first agent = %q, want mayor", explicit[0].Name)
	}
	if explicit[1].Name != "worker-a" || explicit[1].Dir != "hw" {
		t.Errorf("second agent: name=%q dir=%q, want worker-a/hw", explicit[1].Name, explicit[1].Dir)
	}
	if explicit[2].Name != "worker-b" || explicit[2].Dir != "hw" {
		t.Errorf("third agent: name=%q dir=%q, want worker-b/hw", explicit[2].Name, explicit[2].Dir)
	}

	// Provenance should track rig pack agents.
	if _, ok := prov.Agents["hw/worker-a"]; !ok {
		t.Error("provenance should track hw/worker-a")
	}
	if _, ok := prov.Agents["hw/worker-b"]; !ok {
		t.Error("provenance should track hw/worker-b")
	}
}

func TestLoadWithIncludes_BothSingularAndPluralPacks(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/packs/singular/pack.toml"] = []byte(`
[pack]
name = "singular"
schema = 1

[[agent]]
name = "from-singular"
`)
	fs.Files["/city/packs/plural/pack.toml"] = []byte(`
[pack]
name = "plural"
schema = 1

[[agent]]
name = "from-plural"
`)
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
includes = ["packs/singular", "packs/plural"]
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Should have 2 explicit agents: from-singular first, then from-plural.
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 2 {
		t.Fatalf("got %d explicit agents, want 2", len(explicit))
	}
	if explicit[0].Name != "from-singular" {
		t.Errorf("first agent = %q, want from-singular", explicit[0].Name)
	}
	if explicit[1].Name != "from-plural" {
		t.Errorf("second agent = %q, want from-plural", explicit[1].Name)
	}
}

func TestLoadWithIncludes_SessionSectionOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[session]
provider = "subprocess"
`)
	fs.Files["/city/infra.toml"] = []byte(`
[session]
provider = "fake"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Session.Provider != "fake" {
		t.Errorf("Session.Provider = %q, want %q", cfg.Session.Provider, "fake")
	}
}

func TestLoadWithIncludes_SessionSleepMergesPerField(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[session_sleep]
interactive_resume = "60s"
interactive_fresh = "off"
`)
	fs.Files["/city/infra.toml"] = []byte(`
[session_sleep]
noninteractive = "0s"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.SessionSleep.InteractiveResume != "60s" {
		t.Fatalf("InteractiveResume = %q, want 60s", cfg.SessionSleep.InteractiveResume)
	}
	if cfg.SessionSleep.InteractiveFresh != SessionSleepOff {
		t.Fatalf("InteractiveFresh = %q, want %q", cfg.SessionSleep.InteractiveFresh, SessionSleepOff)
	}
	if cfg.SessionSleep.NonInteractive != "0s" {
		t.Fatalf("NonInteractive = %q, want 0s", cfg.SessionSleep.NonInteractive)
	}
}

func TestLoadWithIncludes_MailSectionOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[mail]
provider = "fake"
`)
	fs.Files["/city/infra.toml"] = []byte(`
[mail]
provider = "exec:/usr/local/bin/mail-bridge"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Mail.Provider != "exec:/usr/local/bin/mail-bridge" {
		t.Errorf("Mail.Provider = %q, want %q", cfg.Mail.Provider, "exec:/usr/local/bin/mail-bridge")
	}
}

func TestLoadWithIncludes_EventsSectionOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[events]
provider = "fake"
`)
	fs.Files["/city/infra.toml"] = []byte(`
[events]
provider = "exec:/usr/local/bin/events-bridge"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Events.Provider != "exec:/usr/local/bin/events-bridge" {
		t.Errorf("Events.Provider = %q, want %q", cfg.Events.Provider, "exec:/usr/local/bin/events-bridge")
	}
}

func TestLoadWithIncludes_OrdersSectionOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[orders]
max_timeout = "30s"
`)
	fs.Files["/city/infra.toml"] = []byte(`
[orders]
max_timeout = "120s"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Orders.MaxTimeout != "120s" {
		t.Errorf("Orders.MaxTimeout = %q, want %q", cfg.Orders.MaxTimeout, "120s")
	}
}

func TestLoadWithIncludes_APISectionOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[api]
port = 8080
`)
	fs.Files["/city/infra.toml"] = []byte(`
[api]
port = 9090
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.API.Port != 9090 {
		t.Errorf("API.Port = %d, want %d", cfg.API.Port, 9090)
	}
}

func TestLoadWithIncludes_ConvergenceSectionOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["infra.toml"]

[workspace]
name = "test"

[convergence]
max_per_agent = 2
max_total = 10
`)
	fs.Files["/city/infra.toml"] = []byte(`
[convergence]
max_per_agent = 5
max_total = 20
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Convergence.MaxPerAgent != 5 {
		t.Errorf("Convergence.MaxPerAgent = %d, want %d", cfg.Convergence.MaxPerAgent, 5)
	}
	if cfg.Convergence.MaxTotal != 20 {
		t.Errorf("Convergence.MaxTotal = %d, want %d", cfg.Convergence.MaxTotal, 20)
	}
}

// initBareRepoWithFragment creates a bare git repo containing a TOML config
// fragment file. Returns the bare repo path.
func initBareRepoWithFragment(t *testing.T, fragmentPath, content string) string {
	t.Helper()
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, "fragments.git")

	mustGit(t, "", "init", workDir)

	fragFile := filepath.Join(workDir, fragmentPath)
	if err := os.MkdirAll(filepath.Dir(fragFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fragFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "add fragment")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)

	return bareDir
}

func TestLoadWithIncludes_RemoteInclude(t *testing.T) {
	// Create a bare git repo with a TOML fragment.
	fragment := `
[[agent]]
name = "reviewer"
`
	bare := initBareRepoWithFragment(t, "agents.toml", fragment)

	// Set up a city that includes the remote repo.
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use file:// protocol to reference the bare repo with //subpath.
	remoteInclude := "file://" + bare + "//agents.toml"
	cacheName := includeCacheName("file://" + bare)
	cacheDir := filepath.Join(cityDir, ".gc", "cache", "includes", cacheName)
	if err := clonePack(bare, cacheDir, ""); err != nil {
		t.Fatalf("pre-clone cached include: %v", err)
	}

	cityToml := `
include = ["` + remoteInclude + `"]

[workspace]
name = "test-remote"

[[agent]]
name = "mayor"
`
	cityTomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(cityTomlPath, []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityTomlPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes with remote include: %v", err)
	}

	// Root agent + remote fragment agent.
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 2 {
		t.Fatalf("len(explicit Agents) = %d, want 2", len(explicit))
	}
	if explicit[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", explicit[0].Name, "mayor")
	}
	if explicit[1].Name != "reviewer" {
		t.Errorf("Agents[1].Name = %q, want %q", explicit[1].Name, "reviewer")
	}
}

func TestLoadWithIncludes_RemoteIncludeError(t *testing.T) {
	// A bogus remote URL should produce a clear error, not panic.
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := `
include = ["https://example.com/nonexistent.git//agents.toml"]

[workspace]
name = "test-fail"
`
	cityTomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(cityTomlPath, []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadWithIncludes(fsys.OSFS{}, cityTomlPath)
	if err == nil {
		t.Fatal("expected error for bogus remote include, got nil")
	}
	if !strings.Contains(err.Error(), "resolving include") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "resolving include")
	}

	cacheDir := filepath.Join(cityDir, ".gc", "cache", "includes", includeCacheName("https://example.com/nonexistent.git"))
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Errorf("cache dir %q should not have been created; stat err = %v", cacheDir, statErr)
	}
}

func TestLoadWithIncludes_PackGlobal(t *testing.T) {
	dir := t.TempDir()

	// Create a pack with [global] section and one agent.
	writeFile(t, dir, "packs/ui/pack.toml", `
[pack]
name = "ui-theme"
schema = 1

[global]
session_live = [
    "{{.ConfigDir}}/theme.sh {{.Session}}",
]

[[agent]]
name = "designer"
`)

	// Create city.toml that includes the pack and has an inline agent.
	writeFile(t, dir, "city.toml", `
[workspace]
name = "global-test"
includes = ["packs/ui"]

[[agent]]
name = "coder"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Should have 2 explicit agents: designer (from pack) + coder (inline).
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 2 {
		t.Fatalf("got %d explicit agents, want 2", len(explicit))
	}

	packDir := filepath.Join(dir, "packs/ui")
	wantCmd := packDir + "/theme.sh {{.Session}}"

	// Both explicit agents should have the global session_live command.
	for _, a := range explicit {
		if len(a.SessionLive) != 1 {
			t.Errorf("agent %q: got %d SessionLive, want 1", a.Name, len(a.SessionLive))
			continue
		}
		if a.SessionLive[0] != wantCmd {
			t.Errorf("agent %q: SessionLive[0] = %q, want %q", a.Name, a.SessionLive[0], wantCmd)
		}
	}
}

// TestLoadWithIncludes_ImplicitImportCollisionHardStops verifies that the
// composer rejects a city whose explicit [imports.<name>] would shadow a
// bootstrap implicit-import pack. Prior behavior was silent shadowing on
// upgrade; v0.15.1 hard-stops with a diagnostic directing the operator to
// rename one side.
func TestLoadWithIncludes_ImplicitImportCollisionHardStops(t *testing.T) {
	gcHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.core]
source = "github.com/gastownhall/gc-core"
version = "0.1.0"
commit = "deadbeef"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", gcHome)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[imports.core]
source = "github.com/me/my-core"
version = "1.0.0"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil {
		t.Fatal("LoadWithIncludes should fail on implicit-import collision")
	}
	msg := err.Error()
	if !strings.Contains(msg, "shadows the bootstrap implicit import") {
		t.Fatalf("error missing diagnostic: %v", err)
	}
	if !strings.Contains(msg, "core") {
		t.Fatalf("error should name the colliding import: %v", err)
	}
	if !strings.Contains(msg, "rename one side") {
		t.Fatalf("error should suggest remediation: %v", err)
	}
}

// TestLoadWithIncludes_NoImplicitImportCollisionSucceeds verifies the
// composer does not error when the city declares unrelated imports
// alongside bootstrap implicit imports.
func TestLoadWithIncludes_NoImplicitImportCollisionSucceeds(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	coreCacheDir := GlobalRepoCachePath(gcHome, "github.com/gastownhall/gc-core", "deadbeef")
	if err := os.MkdirAll(coreCacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coreCacheDir, "pack.toml"), []byte(`
[pack]
name = "core"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.core]
source = "github.com/gastownhall/gc-core"
version = "0.1.0"
commit = "deadbeef"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	myteamDir := filepath.Join(dir, "packs", "myteam")
	if err := os.MkdirAll(myteamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(myteamDir, "pack.toml"), []byte(`
[pack]
name = "myteam"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[imports.myteam]
source = "./packs/myteam"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if _, ok := cfg.Imports["core"]; !ok {
		t.Fatalf("implicit core import should still splice in when no collision: imports=%v", cfg.Imports)
	}
	if _, ok := cfg.Imports["myteam"]; !ok {
		t.Fatalf("explicit myteam import should be preserved: imports=%v", cfg.Imports)
	}
}

// TestPopulateAgentLocalAssetDirsForDeclaredAgent verifies that an
// agent declared explicitly in city.toml gets its SkillsDir populated
// from agents/<name>/skills/ at compose time. Without this, the
// materializer and collision validator see an empty SkillsDir for
// every city.toml-declared agent and silently drop agent-local
// skills. Regression for the bug found during Phase 4 smoke testing.
func TestPopulateAgentLocalAssetDirsForDeclaredAgent(t *testing.T) {
	dir := t.TempDir()

	// agents/mayor/skills/ exists on disk.
	skillsDir := filepath.Join(dir, "agents", "mayor", "skills", "plan")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// agents/mayor/mcp/ exists too — verify both get populated.
	mcpDir := filepath.Join(dir, "agents", "mayor", "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// City.toml declares mayor explicitly — this path doesn't go
	// through DiscoverPackAgents, so historically SkillsDir stayed
	// empty for this agent.
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"
provider = "claude"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var mayor *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "mayor" {
			mayor = &cfg.Agents[i]
			break
		}
	}
	if mayor == nil {
		t.Fatal("mayor agent missing from loaded config")
	}
	wantSkills := filepath.Join(dir, "agents", "mayor", "skills")
	if mayor.SkillsDir != wantSkills {
		t.Errorf("mayor.SkillsDir = %q, want %q", mayor.SkillsDir, wantSkills)
	}
	wantMCP := filepath.Join(dir, "agents", "mayor", "mcp")
	if mayor.MCPDir != wantMCP {
		t.Errorf("mayor.MCPDir = %q, want %q", mayor.MCPDir, wantMCP)
	}
}

// TestPopulateAgentLocalAssetDirsPreservesExisting ensures the
// post-compose enrichment doesn't overwrite a SkillsDir/MCPDir that
// was already set (e.g., by DiscoverPackAgents for a conventional
// pack-agent, or explicitly set elsewhere).
func TestPopulateAgentLocalAssetDirsPreservesExisting(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "mayor", SkillsDir: "/already/set/skills", MCPDir: "/already/set/mcp"},
		},
	}
	populateAgentLocalAssetDirs(fsys.OSFS{}, cfg, "/nonexistent-city-root")
	if cfg.Agents[0].SkillsDir != "/already/set/skills" {
		t.Errorf("SkillsDir overwritten: %q", cfg.Agents[0].SkillsDir)
	}
	if cfg.Agents[0].MCPDir != "/already/set/mcp" {
		t.Errorf("MCPDir overwritten: %q", cfg.Agents[0].MCPDir)
	}
}
