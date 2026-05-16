package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

func strPtr(s string) *string { return &s }

func TestDefaultCity(t *testing.T) {
	c := DefaultCity("bright-lights")
	if c.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", c.Workspace.Name, "bright-lights")
	}
	if len(c.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(c.Agents))
	}
	if c.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", c.Agents[0].Name, "mayor")
	}
	if c.Agents[0].PromptTemplate != "prompts/mayor.md" {
		t.Errorf("Agents[0].PromptTemplate = %q, want %q", c.Agents[0].PromptTemplate, "prompts/mayor.md")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	c := DefaultCity("bright-lights")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if got.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", got.Workspace.Name, "bright-lights")
	}
	if len(got.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(got.Agents))
	}
	if got.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", got.Agents[0].Name, "mayor")
	}
}

func TestMarshalOmitsEmptyFields(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "provider") {
		t.Errorf("Marshal output should not contain 'provider' when empty:\n%s", s)
	}
	if strings.Contains(s, "start_command") {
		t.Errorf("Marshal output should not contain 'start_command' when empty:\n%s", s)
	}
	// prompt_template IS set on the default mayor, so check an agent without it.
	c2 := City{Workspace: Workspace{Name: "test"}, Agents: []Agent{{Name: "bare"}}}
	data2, err := c2.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data2), "prompt_template") {
		t.Errorf("Marshal output should not contain 'prompt_template' when empty:\n%s", data2)
	}
}

func TestMarshalDefaultCityFormat(t *testing.T) {
	c := DefaultCity("bright-lights")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := "[workspace]\nname = \"bright-lights\"\n\n[[agent]]\nname = \"mayor\"\nprompt_template = \"prompts/mayor.md\"\n\n[[named_session]]\ntemplate = \"mayor\"\nmode = \"always\"\n"
	if string(data) != want {
		t.Errorf("Marshal output:\ngot:\n%s\nwant:\n%s", data, want)
	}
}

func TestParseWithAgentsAndStartCommand(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"

[[agent]]
name = "mayor"
start_command = "claude --dangerously-skip-permissions"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	if cfg.Agents[0].StartCommand != "claude --dangerously-skip-permissions" {
		t.Errorf("Agents[0].StartCommand = %q, want %q", cfg.Agents[0].StartCommand, "claude --dangerously-skip-permissions")
	}
}

func TestParseRigDefaultBranch(t *testing.T) {
	data := []byte(`
[workspace]
name = "lights"

[[rigs]]
name = "scamper"
path = "/scamper"
default_branch = "master"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("len(Rigs) = %d, want 1", len(cfg.Rigs))
	}
	if got := cfg.Rigs[0].DefaultBranch; got != "master" {
		t.Errorf("DefaultBranch = %q, want %q", got, "master")
	}
	if got := cfg.Rigs[0].EffectiveDefaultBranch(); got != "master" {
		t.Errorf("EffectiveDefaultBranch = %q, want %q", got, "master")
	}
}

func TestEffectiveDefaultBranch_EmptyWhenUnset(t *testing.T) {
	r := Rig{Name: "rig"}
	if got := r.EffectiveDefaultBranch(); got != "" {
		t.Errorf("EffectiveDefaultBranch() = %q, want empty", got)
	}
}

func TestParseAgentSkillsAndMCP(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"

[[agent]]
name = "mayor"
skills = ["code-review", "incident-response"]
mcp = ["beads-health", "sentry"]
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if got := cfg.Agents[0].Skills; !reflect.DeepEqual(got, []string{"code-review", "incident-response"}) {
		t.Fatalf("Agents[0].Skills = %v, want [code-review incident-response]", got)
	}
	if got := cfg.Agents[0].MCP; !reflect.DeepEqual(got, []string{"beads-health", "sentry"}) {
		t.Fatalf("Agents[0].MCP = %v, want [beads-health sentry]", got)
	}
}

func TestParseAgentsNoStartCommand(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].StartCommand != "" {
		t.Errorf("Agents[0].StartCommand = %q, want empty", cfg.Agents[0].StartCommand)
	}
}

func TestParseAgentsAliasNormalizesToAgentDefaults(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[agents]
default_sling_formula = "mol-focus-review"
append_fragments = ["command-glossary"]
skills = ["skill-a", "skill-b"]
mcp = ["mcp-a"]
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.AgentDefaults.DefaultSlingFormula != "mol-focus-review" {
		t.Errorf("AgentDefaults.DefaultSlingFormula = %q, want %q", cfg.AgentDefaults.DefaultSlingFormula, "mol-focus-review")
	}
	if !reflect.DeepEqual(cfg.AgentDefaults.AppendFragments, []string{"command-glossary"}) {
		t.Errorf("AgentDefaults.AppendFragments = %v, want %v", cfg.AgentDefaults.AppendFragments, []string{"command-glossary"})
	}
	if !reflect.DeepEqual(cfg.AgentDefaults.Skills, []string{"skill-a", "skill-b"}) {
		t.Errorf("AgentDefaults.Skills = %v, want %v", cfg.AgentDefaults.Skills, []string{"skill-a", "skill-b"})
	}
	if !reflect.DeepEqual(cfg.AgentDefaults.MCP, []string{"mcp-a"}) {
		t.Errorf("AgentDefaults.MCP = %v, want %v", cfg.AgentDefaults.MCP, []string{"mcp-a"})
	}
	if !reflect.DeepEqual(cfg.AgentsDefaults, AgentDefaults{}) {
		t.Errorf("AgentsDefaults = %#v, want zero value after normalization", cfg.AgentsDefaults)
	}
	out, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "[agent_defaults]") {
		t.Errorf("Marshal output missing canonical [agent_defaults]:\n%s", out)
	}
	if strings.Contains(string(out), "[agents]") {
		t.Errorf("Marshal output should not contain compatibility alias [agents]:\n%s", out)
	}
}

func TestParseAgentDefaultsWinsOverAgentsAlias(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[agents]
default_sling_formula = "mol-legacy"
append_fragments = ["legacy-fragment"]

[agent_defaults]
default_sling_formula = "mol-canonical"
append_fragments = []
skills = ["city-skill"]
mcp = ["city-mcp"]
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.AgentDefaults.DefaultSlingFormula != "mol-canonical" {
		t.Errorf("AgentDefaults.DefaultSlingFormula = %q, want %q", cfg.AgentDefaults.DefaultSlingFormula, "mol-canonical")
	}
	if len(cfg.AgentDefaults.AppendFragments) != 0 {
		t.Errorf("AgentDefaults.AppendFragments = %v, want empty canonical override", cfg.AgentDefaults.AppendFragments)
	}
	if !reflect.DeepEqual(cfg.AgentDefaults.Skills, []string{"city-skill"}) {
		t.Errorf("AgentDefaults.Skills = %v, want %v", cfg.AgentDefaults.Skills, []string{"city-skill"})
	}
	if !reflect.DeepEqual(cfg.AgentDefaults.MCP, []string{"city-mcp"}) {
		t.Errorf("AgentDefaults.MCP = %v, want %v", cfg.AgentDefaults.MCP, []string{"city-mcp"})
	}
	if !reflect.DeepEqual(cfg.AgentsDefaults, AgentDefaults{}) {
		t.Errorf("AgentsDefaults = %#v, want zero value after normalization", cfg.AgentsDefaults)
	}
}

func TestParseNoAgents(t *testing.T) {
	data := []byte(`
[workspace]
name = "bare-city"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("len(Agents) = %d, want 0", len(cfg.Agents))
	}
}

func TestParseEmptyFile(t *testing.T) {
	data := []byte("# just a comment\n")
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Workspace.Name != "" {
		t.Errorf("Workspace.Name = %q, want empty", cfg.Workspace.Name)
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("len(Agents) = %d, want 0", len(cfg.Agents))
	}
}

func TestParseCorruptTOML(t *testing.T) {
	data := []byte("[[[invalid toml")
	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected error for corrupt TOML")
	}
}

func TestLoadSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	content := `[workspace]
name = "test"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Workspace.Name != "test" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "test")
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
}

func TestLoadNonexistentFile(t *testing.T) {
	_, err := Load(fsys.OSFS{}, "/nonexistent/city.toml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadReadError(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/city/city.toml"] = fmt.Errorf("permission denied")

	_, err := Load(f, "/city/city.toml")
	if err == nil {
		t.Fatal("expected error when ReadFile fails")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want 'permission denied'", err)
	}
}

func TestLoadWithFake(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte("[workspace]\nname = \"fake-city\"\n")

	cfg, err := Load(f, "/city/city.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Workspace.Name != "fake-city" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "fake-city")
	}
}

// TestLoadSkipsPackExpansion verifies that Load parses a city.toml containing
// pack and rig-include references without attempting to expand them. This is
// the behavior the dashboard relies on — it only needs the workspace name,
// not the fully-expanded agent tree.
func TestLoadSkipsPackExpansion(t *testing.T) {
	f := fsys.NewFake()
	// Config references packs and rig includes that do NOT exist on the
	// fake filesystem. Load must succeed because it does not expand packs.
	f.Files["/city/city.toml"] = []byte(`
[workspace]
name = "brewlife"

[packs.gastown]
source = "https://github.com/example/gastown"
path   = "examples/gastown/packs/gastown"

[[rigs]]
name     = "brewlife"
includes = ["gastown"]
`)

	cfg, err := Load(f, "/city/city.toml")
	if err != nil {
		t.Fatalf("Load should succeed without expanding packs: %v", err)
	}
	if cfg.Workspace.Name != "brewlife" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "brewlife")
	}

	// Confirm that LoadWithIncludes fails on the same config because the
	// referenced packs are not materialized on the filesystem.
	_, _, err = LoadWithIncludes(f, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes should fail when packs are not materialized")
	}
}

func TestLoadCorruptTOML(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte("[[[invalid toml")

	_, err := Load(f, "/city/city.toml")
	if err == nil {
		t.Fatal("expected error for corrupt TOML")
	}
}

func TestParseWithProvider(t *testing.T) {
	data := []byte(`
[workspace]
name = "multi-provider"

[[agent]]
name = "mayor"
provider = "claude"

[[agent]]
name = "worker"
provider = "codex"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Provider != "claude" {
		t.Errorf("Agents[0].Provider = %q, want %q", cfg.Agents[0].Provider, "claude")
	}
	if cfg.Agents[1].Provider != "codex" {
		t.Errorf("Agents[1].Provider = %q, want %q", cfg.Agents[1].Provider, "codex")
	}
}

func TestParseBeadsSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Beads.Provider != "file" {
		t.Errorf("Beads.Provider = %q, want %q", cfg.Beads.Provider, "file")
	}
}

func TestParseNoBeadsSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Beads.Provider != "" {
		t.Errorf("Beads.Provider = %q, want empty", cfg.Beads.Provider)
	}
}

func TestMarshalOmitsEmptyBeadsSection(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[beads]") {
		t.Errorf("Marshal output should not contain '[beads]' when empty:\n%s", data)
	}
}

func TestParseSessionSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[session]
provider = "subprocess"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Session.Provider != "subprocess" {
		t.Errorf("Session.Provider = %q, want %q", cfg.Session.Provider, "subprocess")
	}
}

func TestParseNoSessionSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Session.Provider != "" {
		t.Errorf("Session.Provider = %q, want empty", cfg.Session.Provider)
	}
}

func TestMarshalOmitsEmptySessionSection(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[session]") {
		t.Errorf("Marshal output should not contain '[session]' when empty:\n%s", data)
	}
}

func TestParseMailSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[mail]
provider = "exec:/usr/local/bin/mail-bridge"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Mail.Provider != "exec:/usr/local/bin/mail-bridge" {
		t.Errorf("Mail.Provider = %q, want %q", cfg.Mail.Provider, "exec:/usr/local/bin/mail-bridge")
	}
}

func TestParseNoMailSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Mail.Provider != "" {
		t.Errorf("Mail.Provider = %q, want empty", cfg.Mail.Provider)
	}
}

func TestMarshalOmitsEmptyMailSection(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[mail]") {
		t.Errorf("Marshal output should not contain '[mail]' when empty:\n%s", data)
	}
}

func TestParseEventsSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[events]
provider = "fake"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Events.Provider != "fake" {
		t.Errorf("Events.Provider = %q, want %q", cfg.Events.Provider, "fake")
	}
}

func TestParseNoEventsSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Events.Provider != "" {
		t.Errorf("Events.Provider = %q, want empty", cfg.Events.Provider)
	}
}

func TestMarshalOmitsEmptyEventsSection(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[events]") {
		t.Errorf("Marshal output should not contain '[events]' when empty:\n%s", data)
	}
}

func TestParseWithPromptTemplate(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].PromptTemplate != "prompts/mayor.md" {
		t.Errorf("Agents[0].PromptTemplate = %q, want %q", cfg.Agents[0].PromptTemplate, "prompts/mayor.md")
	}
	if cfg.Agents[1].PromptTemplate != "prompts/worker.md" {
		t.Errorf("Agents[1].PromptTemplate = %q, want %q", cfg.Agents[1].PromptTemplate, "prompts/worker.md")
	}
}

func TestMarshalOmitsEmptyPromptTemplate(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "worker"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "prompt_template") {
		t.Errorf("Marshal output should not contain 'prompt_template' when empty:\n%s", data)
	}
}

func TestParseMultipleAgents(t *testing.T) {
	data := []byte(`
[workspace]
name = "big-city"

[[agent]]
name = "mayor"

[[agent]]
name = "worker"
start_command = "codex --dangerously-bypass-approvals-and-sandbox"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	if cfg.Agents[1].Name != "worker" {
		t.Errorf("Agents[1].Name = %q, want %q", cfg.Agents[1].Name, "worker")
	}
	if cfg.Agents[1].StartCommand != "codex --dangerously-bypass-approvals-and-sandbox" {
		t.Errorf("Agents[1].StartCommand = %q, want codex command", cfg.Agents[1].StartCommand)
	}
}

func TestParseWorkspaceProvider(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"
provider = "claude"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}
}

func TestParseWorkspaceStartCommand(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"
start_command = "my-agent --flag"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Workspace.StartCommand != "my-agent --flag" {
		t.Errorf("Workspace.StartCommand = %q, want %q", cfg.Workspace.StartCommand, "my-agent --flag")
	}
}

func TestWizardCity(t *testing.T) {
	c := WizardCity("bright-lights", "claude", "")
	if c.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", c.Workspace.Name, "bright-lights")
	}
	if c.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", c.Workspace.Provider, "claude")
	}
	if len(c.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(c.Agents))
	}
	if c.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", c.Agents[0].Name, "mayor")
	}
	if c.Agents[0].PromptTemplate != "prompts/mayor.md" {
		t.Errorf("Agents[0].PromptTemplate = %q, want %q", c.Agents[0].PromptTemplate, "prompts/mayor.md")
	}
}

func TestWizardCityMarshal(t *testing.T) {
	c := WizardCity("bright-lights", "claude", "")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `provider = "claude"`) {
		t.Errorf("Marshal output missing provider:\n%s", s)
	}
	if !strings.Contains(s, `name = "mayor"`) {
		t.Errorf("Marshal output missing mayor agent:\n%s", s)
	}
	// Round-trip parse.
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if got.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", got.Workspace.Provider, "claude")
	}
	if len(got.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(got.Agents))
	}
}

func TestWizardCityEmptyProvider(t *testing.T) {
	c := WizardCity("test", "", "")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	// provider should be omitted when empty.
	idx := strings.Index(s, "[[agent]]")
	if idx == -1 {
		t.Fatal("marshal output missing [[agent]] section")
	}
	wsSection := s[:idx]
	if strings.Contains(wsSection, "provider") {
		t.Errorf("workspace section should not contain 'provider' when empty:\n%s", wsSection)
	}
}

func TestWizardCityStartCommand(t *testing.T) {
	c := WizardCity("bright-lights", "", "my-agent --auto")
	if c.Workspace.StartCommand != "my-agent --auto" {
		t.Errorf("Workspace.StartCommand = %q, want %q", c.Workspace.StartCommand, "my-agent --auto")
	}
	if c.Workspace.Provider != "" {
		t.Errorf("Workspace.Provider = %q, want empty (startCommand takes precedence)", c.Workspace.Provider)
	}
	if len(c.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(c.Agents))
	}

	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `start_command = "my-agent --auto"`) {
		t.Errorf("Marshal output missing start_command:\n%s", s)
	}
	// provider should NOT appear.
	idx := strings.Index(s, "[[agent]]")
	if idx == -1 {
		t.Fatal("marshal output missing [[agent]] section")
	}
	wsSection := s[:idx]
	if strings.Contains(wsSection, "provider") {
		t.Errorf("workspace section should not contain 'provider' when startCommand set:\n%s", wsSection)
	}
}

func TestGastownCity(t *testing.T) {
	c := GastownCity("bright-lights", "claude", "")
	if c.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", c.Workspace.Name, "bright-lights")
	}
	if c.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", c.Workspace.Provider, "claude")
	}
	if len(c.Imports) != 1 || c.Imports["gastown"].Source != ".gc/system/packs/gastown" {
		t.Errorf("Imports = %v, want gastown=.gc/system/packs/gastown", c.Imports)
	}
	if len(c.DefaultRigImports) != 1 || c.DefaultRigImports["gastown"].Source != ".gc/system/packs/gastown" {
		t.Errorf("DefaultRigImports = %v, want gastown=.gc/system/packs/gastown", c.DefaultRigImports)
	}
	if len(c.Workspace.GlobalFragments) != 2 {
		t.Errorf("Workspace.GlobalFragments = %v, want 2 entries", c.Workspace.GlobalFragments)
	}
	// No inline agents — they come from the pack.
	if len(c.Agents) != 0 {
		t.Errorf("len(Agents) = %d, want 0 (agents come from pack)", len(c.Agents))
	}
	// Daemon config should be set.
	if c.Daemon.PatrolInterval != "30s" {
		t.Errorf("Daemon.PatrolInterval = %q, want %q", c.Daemon.PatrolInterval, "30s")
	}
	if c.Daemon.MaxRestartsOrDefault() != 5 {
		t.Errorf("Daemon.MaxRestarts = %d, want 5", c.Daemon.MaxRestartsOrDefault())
	}
}

func TestGastownCityStartCommand(t *testing.T) {
	c := GastownCity("test", "", "my-agent --auto")
	if c.Workspace.StartCommand != "my-agent --auto" {
		t.Errorf("Workspace.StartCommand = %q, want %q", c.Workspace.StartCommand, "my-agent --auto")
	}
	if c.Workspace.Provider != "" {
		t.Errorf("Workspace.Provider = %q, want empty", c.Workspace.Provider)
	}
}

func TestGastownCityNoProvider(t *testing.T) {
	c := GastownCity("test", "", "")
	if c.Workspace.Provider != "" {
		t.Errorf("Workspace.Provider = %q, want empty", c.Workspace.Provider)
	}
	if c.Workspace.StartCommand != "" {
		t.Errorf("Workspace.StartCommand = %q, want empty", c.Workspace.StartCommand)
	}
}

func TestGastownCityRoundTrip(t *testing.T) {
	c := GastownCity("bright-lights", "claude", "")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if len(got.Imports) != 1 || got.Imports["gastown"].Source != ".gc/system/packs/gastown" {
		t.Errorf("round-trip Imports = %v, want gastown=.gc/system/packs/gastown", got.Imports)
	}
	if got.Workspace.Provider != "claude" {
		t.Errorf("round-trip Provider = %q, want %q", got.Workspace.Provider, "claude")
	}
	if got.Daemon.PatrolInterval != "30s" {
		t.Errorf("round-trip Daemon.PatrolInterval = %q, want %q", got.Daemon.PatrolInterval, "30s")
	}
}

func TestDefaultRigIncludesOmitEmpty(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "default_rig_includes") {
		t.Errorf("Marshal output should not contain 'default_rig_includes' when empty:\n%s", data)
	}
}

func TestMarshalOmitsEmptyWorkspaceFields(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	// Workspace provider and start_command should not appear when empty.
	// Check the workspace section specifically (before [[agent]]).
	idx := strings.Index(s, "[[agent]]")
	if idx == -1 {
		t.Fatal("marshal output missing [[agent]] section")
	}
	wsSection := s[:idx]
	if strings.Contains(wsSection, "provider") {
		t.Errorf("workspace section should not contain 'provider' when empty:\n%s", wsSection)
	}
	if strings.Contains(wsSection, "start_command") {
		t.Errorf("workspace section should not contain 'start_command' when empty:\n%s", wsSection)
	}
}

func TestParseProvidersSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"
provider = "claude"

[providers.kiro]
command = "kiro-cli"
args = ["chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"]
prompt_mode = "arg"
ready_delay_ms = 5000
process_names = ["kiro-cli", "kiro", "node"]
supports_hooks = true
instructions_file = "AGENTS.md"
resume_flag = "--resume"
resume_style = "flag"

[providers.kiro.env]
KIRO_AGENT_MODE = "headless"

[providers.kiro.permission_modes]
default = "--trust-mode default"
unrestricted = "--trust-mode full"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	kiro, ok := cfg.Providers["kiro"]
	if !ok {
		t.Fatal("Providers[kiro] not found")
	}
	if kiro.Command != "kiro-cli" {
		t.Errorf("Command = %q, want %q", kiro.Command, "kiro-cli")
	}
	if want := []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"}; !reflect.DeepEqual(kiro.Args, want) {
		t.Errorf("Args = %v, want %v", kiro.Args, want)
	}
	if kiro.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", kiro.PromptMode, "arg")
	}
	if kiro.ReadyDelayMs != 5000 {
		t.Errorf("ReadyDelayMs = %d, want 5000", kiro.ReadyDelayMs)
	}
	if len(kiro.ProcessNames) != 3 || kiro.ProcessNames[0] != "kiro-cli" || kiro.ProcessNames[1] != "kiro" || kiro.ProcessNames[2] != "node" {
		t.Errorf("ProcessNames = %v, want [kiro-cli kiro node]", kiro.ProcessNames)
	}
	if !derefBool(kiro.SupportsHooks) {
		t.Error("SupportsHooks = false, want true")
	}
	if kiro.InstructionsFile != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q, want %q", kiro.InstructionsFile, "AGENTS.md")
	}
	if kiro.ResumeFlag != "--resume" {
		t.Errorf("ResumeFlag = %q, want %q", kiro.ResumeFlag, "--resume")
	}
	if kiro.ResumeStyle != "flag" {
		t.Errorf("ResumeStyle = %q, want %q", kiro.ResumeStyle, "flag")
	}
	if kiro.Env["KIRO_AGENT_MODE"] != "headless" {
		t.Errorf("Env[KIRO_AGENT_MODE] = %q, want %q", kiro.Env["KIRO_AGENT_MODE"], "headless")
	}
	if kiro.PermissionModes["unrestricted"] != "--trust-mode full" {
		t.Errorf("PermissionModes[unrestricted] = %q, want %q", kiro.PermissionModes["unrestricted"], "--trust-mode full")
	}
	if kiro.PermissionModes["default"] != "--trust-mode default" {
		t.Errorf("PermissionModes[default] = %q, want %q", kiro.PermissionModes["default"], "--trust-mode default")
	}
}

func TestParseKiroProviderWithOptionsSchema(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[providers.kiro]
command = "kiro-cli"
args = ["chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"]
prompt_mode = "arg"
ready_delay_ms = 5000

[[providers.kiro.options_schema]]
key = "permission_mode"
label = "Trust Mode"
type = "select"
default = "unrestricted"

  [[providers.kiro.options_schema.choices]]
  value = "default"
  label = "Default trust"
  flag_args = ["--trust-mode", "default"]

  [[providers.kiro.options_schema.choices]]
  value = "unrestricted"
  label = "Full trust"
  flag_args = ["--trust-mode", "full"]

[providers.kiro.option_defaults]
permission_mode = "unrestricted"

[[agent]]
name = "worker"
provider = "kiro"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	kiro, ok := cfg.Providers["kiro"]
	if !ok {
		t.Fatal("Providers[kiro] not found")
	}
	if len(kiro.OptionsSchema) != 1 {
		t.Fatalf("len(OptionsSchema) = %d, want 1", len(kiro.OptionsSchema))
	}
	opt := kiro.OptionsSchema[0]
	if opt.Key != "permission_mode" {
		t.Errorf("OptionsSchema[0].Key = %q, want %q", opt.Key, "permission_mode")
	}
	if len(opt.Choices) != 2 {
		t.Fatalf("len(Choices) = %d, want 2", len(opt.Choices))
	}
	if opt.Choices[1].Value != "unrestricted" {
		t.Errorf("Choices[1].Value = %q, want %q", opt.Choices[1].Value, "unrestricted")
	}
	if len(opt.Choices[1].FlagArgs) != 2 || opt.Choices[1].FlagArgs[1] != "full" {
		t.Errorf("Choices[1].FlagArgs = %v, want [--trust-mode full]", opt.Choices[1].FlagArgs)
	}
	if kiro.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("OptionDefaults[permission_mode] = %q, want %q", kiro.OptionDefaults["permission_mode"], "unrestricted")
	}
}

func TestParseAgentOverrideFields(t *testing.T) {
	data := []byte(`
[workspace]
name = "bright-lights"

[[agent]]
name = "scout"
provider = "claude"
args = ["--dangerously-skip-permissions", "--verbose"]
ready_delay_ms = 15000
prompt_mode = "flag"
prompt_flag = "--prompt"
process_names = ["node"]
emits_permission_warning = false
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.Provider != "claude" {
		t.Errorf("Provider = %q, want %q", a.Provider, "claude")
	}
	if len(a.Args) != 2 {
		t.Fatalf("len(Args) = %d, want 2", len(a.Args))
	}
	if a.Args[1] != "--verbose" {
		t.Errorf("Args[1] = %q, want %q", a.Args[1], "--verbose")
	}
	if a.ReadyDelayMs == nil || *a.ReadyDelayMs != 15000 {
		t.Errorf("ReadyDelayMs = %v, want 15000", a.ReadyDelayMs)
	}
	if a.PromptMode != "flag" {
		t.Errorf("PromptMode = %q, want %q", a.PromptMode, "flag")
	}
	if a.PromptFlag != "--prompt" {
		t.Errorf("PromptFlag = %q, want %q", a.PromptFlag, "--prompt")
	}
	if a.EmitsPermissionWarning == nil || *a.EmitsPermissionWarning != false {
		t.Errorf("EmitsPermissionWarning = %v, want false", a.EmitsPermissionWarning)
	}
}

func TestMarshalOmitsEmptyProviders(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[providers") {
		t.Errorf("Marshal output should not contain '[providers' when empty:\n%s", data)
	}
}

func TestMarshalOmitsEmptyAgentOverrideFields(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	for _, field := range []string{"args", "prompt_mode", "prompt_flag", "ready_delay_ms", "ready_prompt_prefix", "process_names", "emits_permission_warning", "env"} {
		if strings.Contains(s, field) {
			t.Errorf("Marshal output should not contain %q when empty:\n%s", field, s)
		}
	}
}

func TestProvidersRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Providers: map[string]ProviderSpec{
			"kiro": {
				Command:          "kiro-cli",
				Args:             []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"},
				PromptMode:       "arg",
				ReadyDelayMs:     5000,
				ProcessNames:     []string{"kiro-cli", "kiro", "node"},
				SupportsHooks:    boolPtr(true),
				InstructionsFile: "AGENTS.md",
				ResumeFlag:       "--resume",
				ResumeStyle:      "flag",
				Env:              map[string]string{"KIRO_AGENT_MODE": "headless"},
				PermissionModes:  map[string]string{"unrestricted": "--trust-mode full"},
			},
		},
		Agents: []Agent{{Name: "mayor"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(got.Providers))
	}
	kiro, ok := got.Providers["kiro"]
	if !ok {
		t.Fatal("Providers[kiro] not found after round-trip")
	}
	if kiro.Command != "kiro-cli" {
		t.Errorf("Command = %q, want %q", kiro.Command, "kiro-cli")
	}
	if want := []string{"chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"}; !reflect.DeepEqual(kiro.Args, want) {
		t.Errorf("Args = %v, want %v", kiro.Args, want)
	}
	if kiro.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", kiro.PromptMode, "arg")
	}
	if kiro.ReadyDelayMs != 5000 {
		t.Errorf("ReadyDelayMs = %d, want 5000", kiro.ReadyDelayMs)
	}
	if len(kiro.ProcessNames) != 3 || kiro.ProcessNames[0] != "kiro-cli" || kiro.ProcessNames[1] != "kiro" || kiro.ProcessNames[2] != "node" {
		t.Errorf("ProcessNames = %v, want [kiro-cli kiro node]", kiro.ProcessNames)
	}
	if !derefBool(kiro.SupportsHooks) {
		t.Error("SupportsHooks = false after round-trip, want true")
	}
	if kiro.InstructionsFile != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q after round-trip, want %q", kiro.InstructionsFile, "AGENTS.md")
	}
	if kiro.ResumeFlag != "--resume" {
		t.Errorf("ResumeFlag = %q after round-trip, want %q", kiro.ResumeFlag, "--resume")
	}
	if kiro.ResumeStyle != "flag" {
		t.Errorf("ResumeStyle = %q after round-trip, want %q", kiro.ResumeStyle, "flag")
	}
	if kiro.Env["KIRO_AGENT_MODE"] != "headless" {
		t.Errorf("Env[KIRO_AGENT_MODE] = %q after round-trip, want %q", kiro.Env["KIRO_AGENT_MODE"], "headless")
	}
	if kiro.PermissionModes["unrestricted"] != "--trust-mode full" {
		t.Errorf("PermissionModes[unrestricted] = %q after round-trip, want %q", kiro.PermissionModes["unrestricted"], "--trust-mode full")
	}
}

func TestParseAgentDir(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"
dir = "projects/frontend"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Dir != "projects/frontend" {
		t.Errorf("Agents[0].Dir = %q, want %q", cfg.Agents[0].Dir, "projects/frontend")
	}
	if cfg.Agents[1].Dir != "" {
		t.Errorf("Agents[1].Dir = %q, want empty", cfg.Agents[1].Dir)
	}
}

func TestParseAgentPreStart(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"
dir = "/repo"
pre_start = ["mkdir -p /tmp/work", "git worktree add /tmp/work"]

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if len(cfg.Agents[0].PreStart) != 2 {
		t.Errorf("Agents[0].PreStart len = %d, want 2", len(cfg.Agents[0].PreStart))
	}
	if len(cfg.Agents[1].PreStart) != 0 {
		t.Errorf("Agents[1].PreStart len = %d, want 0", len(cfg.Agents[1].PreStart))
	}
}

func TestPreStartRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "worker", Dir: "/repo", PreStart: []string{"echo hello"}}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if len(got.Agents[0].PreStart) != 1 || got.Agents[0].PreStart[0] != "echo hello" {
		t.Errorf("PreStart after round-trip = %v, want [echo hello]", got.Agents[0].PreStart)
	}
}

func TestMarshalOmitsEmptyPreStart(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "worker"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "pre_start") {
		t.Errorf("Marshal output should not contain 'pre_start' when empty:\n%s", data)
	}
}

func TestMarshalOmitsEmptyDir(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "worker"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "dir") {
		t.Errorf("Marshal output should not contain 'dir' when empty:\n%s", data)
	}
}

func TestDirRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "worker", Dir: "projects/backend"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if got.Agents[0].Dir != "projects/backend" {
		t.Errorf("Dir after round-trip = %q, want %q", got.Agents[0].Dir, "projects/backend")
	}
}

func TestParseAgentEnv(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"

[agent.env]
EXTRA = "yes"
DEBUG = "1"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Env["EXTRA"] != "yes" {
		t.Errorf("Env[EXTRA] = %q, want %q", cfg.Agents[0].Env["EXTRA"], "yes")
	}
	if cfg.Agents[0].Env["DEBUG"] != "1" {
		t.Errorf("Env[DEBUG] = %q, want %q", cfg.Agents[0].Env["DEBUG"], "1")
	}
}

// --- Pool-in-agent tests ---

func TestParseAgentWithScaling(t *testing.T) {
	data := []byte(`
[workspace]
name = "pool-city"

[[agent]]
name = "worker"
prompt_template = "prompts/pool-worker.md"
start_command = "echo hello"
min_active_sessions = 0
max_active_sessions = 5
scale_check = "echo 3"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.MinActiveSessions == nil || *a.MinActiveSessions != 0 {
		t.Errorf("MinActiveSessions = %v, want 0", a.MinActiveSessions)
	}
	if a.MaxActiveSessions == nil || *a.MaxActiveSessions != 5 {
		t.Errorf("MaxActiveSessions = %v, want 5", a.MaxActiveSessions)
	}
	if a.ScaleCheck != "echo 3" {
		t.Errorf("ScaleCheck = %q, want %q", a.ScaleCheck, "echo 3")
	}
}

func TestParseAgentWithoutScaling(t *testing.T) {
	data := []byte(`
[workspace]
name = "simple"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].MaxActiveSessions != nil {
		t.Errorf("MaxActiveSessions = %v, want nil", cfg.Agents[0].MaxActiveSessions)
	}
}

func TestPoolRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents: []Agent{{
			Name:              "worker",
			MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(5), ScaleCheck: "echo 3",
		}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(got.Agents))
	}
	a := got.Agents[0]
	if a.MinActiveSessions == nil || *a.MinActiveSessions != 1 {
		t.Errorf("MinActiveSessions = %v, want 1", a.MinActiveSessions)
	}
	if a.MaxActiveSessions == nil || *a.MaxActiveSessions != 5 {
		t.Errorf("MaxActiveSessions = %v, want 5", a.MaxActiveSessions)
	}
	if a.ScaleCheck != "echo 3" {
		t.Errorf("ScaleCheck = %q, want %q", a.ScaleCheck, "echo 3")
	}
}

func TestEffectiveWorkQueryDefault(t *testing.T) {
	a := Agent{Name: "mayor"}
	got := a.EffectiveWorkQuery()
	// Tiered query: check that tier 3 (routed_to) and tier 1-2 (assignee resolution) are present.
	if !strings.Contains(got, "bd ready --metadata-field gc.routed_to=mayor --unassigned --exclude-type=epic --json --limit=1") {
		t.Errorf("EffectiveWorkQuery() missing tier 3 routed_to: %q", got)
	}
	if !strings.Contains(got, `"$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"`) {
		t.Errorf("EffectiveWorkQuery() missing multi-identifier resolution: %q", got)
	}
}

func TestEffectiveWorkQueryCustom(t *testing.T) {
	a := Agent{Name: "mayor", WorkQuery: "bd ready --label=pool:polecats"}
	got := a.EffectiveWorkQuery()
	want := "bd ready --label=pool:polecats"
	if got != want {
		t.Errorf("EffectiveWorkQuery() = %q, want %q", got, want)
	}
}

func TestEffectiveWorkQueryWithDir(t *testing.T) {
	a := Agent{Name: "polecat", Dir: "hello-world"}
	got := a.EffectiveWorkQuery()
	if !strings.Contains(got, "bd ready --metadata-field gc.routed_to=hello-world/polecat --unassigned --exclude-type=epic --json --limit=1") {
		t.Errorf("EffectiveWorkQuery() missing tier 3 routed_to: %q", got)
	}
}

func TestEffectiveWorkQueryPoolDefault(t *testing.T) {
	a := Agent{Name: "polecat", Dir: "hello-world", MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3)}
	got := a.EffectiveWorkQuery()
	if !strings.Contains(got, "bd ready --metadata-field gc.routed_to=hello-world/polecat --unassigned --exclude-type=epic --json --limit=1") {
		t.Errorf("EffectiveWorkQuery() missing tier 3 routed_to: %q", got)
	}
	if strings.Contains(got, "--type=molecule") {
		t.Errorf("EffectiveWorkQuery() should not route molecule containers: %q", got)
	}
}

func TestEffectiveSlingQueryFixedAgent(t *testing.T) {
	a := Agent{Name: "mayor"}
	got := a.EffectiveSlingQuery()
	want := "bd update {} --set-metadata gc.routed_to=mayor"
	if got != want {
		t.Errorf("EffectiveSlingQuery() = %q, want %q", got, want)
	}
}

func TestEffectiveSlingQueryFixedAgentWithDir(t *testing.T) {
	a := Agent{Name: "refinery", Dir: "hello-world"}
	got := a.EffectiveSlingQuery()
	want := "bd update {} --set-metadata gc.routed_to=hello-world/refinery"
	if got != want {
		t.Errorf("EffectiveSlingQuery() = %q, want %q", got, want)
	}
}

func TestEffectiveSlingQueryPoolDefault(t *testing.T) {
	a := Agent{Name: "polecat", Dir: "hello-world", MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3)}
	got := a.EffectiveSlingQuery()
	want := "bd update {} --set-metadata gc.routed_to=hello-world/polecat"
	if got != want {
		t.Errorf("EffectiveSlingQuery() = %q, want %q", got, want)
	}
}

func TestEffectiveSlingQueryCustom(t *testing.T) {
	a := Agent{Name: "worker", SlingQuery: "custom-dispatch {} --target=worker"}
	got := a.EffectiveSlingQuery()
	want := "custom-dispatch {} --target=worker"
	if got != want {
		t.Errorf("EffectiveSlingQuery() = %q, want %q", got, want)
	}
}

func TestEffectiveWorkQueryPoolNameOverride(t *testing.T) {
	// Pool instance with PoolName set — work query uses PoolName for gc.routed_to.
	a := Agent{
		Name:              "dog-1",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3),
		PoolName: "hello-world/dog",
	}
	got := a.EffectiveWorkQuery()
	if !strings.Contains(got, "bd ready --metadata-field gc.routed_to=hello-world/dog --unassigned --exclude-type=epic --json --limit=1") {
		t.Errorf("EffectiveWorkQuery() missing tier 3 routed_to with pool name: %q", got)
	}
	if strings.Contains(got, "--type=molecule") {
		t.Errorf("EffectiveWorkQuery() should not route molecule containers with pool name: %q", got)
	}
}

func TestEffectiveWorkQueryPoolNoPoolName(t *testing.T) {
	a := Agent{Name: "dog", Dir: "hello-world", MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3)}
	got := a.EffectiveWorkQuery()
	if !strings.Contains(got, "bd ready --metadata-field gc.routed_to=hello-world/dog --unassigned --exclude-type=epic --json --limit=1") {
		t.Errorf("EffectiveWorkQuery() missing tier 3 routed_to: %q", got)
	}
}

func TestEffectiveWorkQueryControlDispatcherIncludesLegacyWorkflowControlRoute(t *testing.T) {
	a := Agent{Name: ControlDispatcherAgentName, Dir: "gascity"}
	got := a.EffectiveWorkQuery()
	if !strings.Contains(got, "gc.routed_to=gascity/control-dispatcher") {
		t.Fatalf("EffectiveWorkQuery() missing current control-dispatcher route: %q", got)
	}
	if !strings.Contains(got, "gc.routed_to=gascity/workflow-control") {
		t.Fatalf("EffectiveWorkQuery() missing legacy workflow-control route: %q", got)
	}
	if !strings.Contains(got, `workflow-control`) {
		t.Fatalf("EffectiveWorkQuery() missing legacy assignee alias handling: %q", got)
	}
	if strings.Contains(got, "--type=molecule") {
		t.Fatalf("EffectiveWorkQuery() should keep control-dispatcher on the no-molecule path: %q", got)
	}
}

func TestEffectiveWorkQueryControlDispatcherClaimsLegacyAssignedWork(t *testing.T) {
	a := Agent{Name: ControlDispatcherAgentName, Dir: "gascity"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_NAME": "gascity--control-dispatcher",
		"GC_ALIAS":        "gascity/control-dispatcher",
	}, `#!/bin/sh
set -eu
case "$*" in
  "list --status in_progress --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=1"|\
  "list --status in_progress --assignee=gascity/control-dispatcher --exclude-type=epic --json --limit=1"|\
  "list --status in_progress --assignee=gascity--workflow-control --exclude-type=epic --json --limit=1"|\
  "list --status in_progress --assignee=gascity/workflow-control --exclude-type=epic --json --limit=1")
    printf '[]'
    ;;
  "ready --assignee=gascity--workflow-control --exclude-type=epic --json --limit=1"|\
  "ready --assignee=gascity/workflow-control --exclude-type=epic --json --limit=1")
    printf '[{"id":"ga-legacy-ready"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	if got, want := strings.TrimSpace(out), `[{"id":"ga-legacy-ready"}]`; got != want {
		t.Fatalf("legacy assigned work query output = %q, want %q", got, want)
	}
}

func TestEffectiveWorkQueryControlDispatcherClaimsLegacyUnassignedRoute(t *testing.T) {
	a := Agent{Name: ControlDispatcherAgentName, Dir: "gascity"}
	out := runEffectiveWorkQuery(t, a, nil, `#!/bin/sh
set -eu
case "$*" in
  "ready --metadata-field gc.routed_to=gascity/control-dispatcher --unassigned --exclude-type=epic --json --limit=1")
    printf '[]'
    ;;
  "ready --metadata-field gc.routed_to=gascity/workflow-control --unassigned --exclude-type=epic --json --limit=1")
    printf '[{"id":"ga-legacy-route"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	if got, want := strings.TrimSpace(out), `[{"id":"ga-legacy-route"}]`; got != want {
		t.Fatalf("legacy routed work query output = %q, want %q", got, want)
	}
}

func TestEffectiveSlingQueryPoolNameOverride(t *testing.T) {
	a := Agent{
		Name:              "dog-1",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3),
		PoolName: "hello-world/dog",
	}
	got := a.EffectiveSlingQuery()
	want := "bd update {} --set-metadata gc.routed_to=hello-world/dog-1"
	if got != want {
		t.Errorf("EffectiveSlingQuery() = %q, want %q", got, want)
	}
}

// TestEffectiveWorkQueryExcludesEpics verifies that the default work query
// excludes parent epic beads from claimable work — regression coverage for
// gc-udx where workers were claiming open parent epics whose only role is
// "all children done." Workers should only see leaf work; parent epics must
// flow through an explicit override path (a custom work_query in the
// agent's TOML), not the default tier-1/2/3 query.
func TestEffectiveWorkQueryExcludesEpics(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	got := a.EffectiveWorkQuery()
	// All three tiers (in_progress assignee, ready assignee, ready routed)
	// must structurally exclude the epic type.
	wantSnippets := []string{
		`bd list --status in_progress --assignee="$id" --exclude-type=epic --json`,
		`bd ready --assignee="$id" --exclude-type=epic --json`,
		`bd ready --metadata-field gc.routed_to=hello-world/worker --unassigned --exclude-type=epic --json`,
	}
	for _, want := range wantSnippets {
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveWorkQuery() missing exclude-type=epic on tier:\n  want substring: %s\n  got: %s", want, got)
		}
	}
}

// TestEffectiveWorkQueryExcludesEpicsControlDispatcher verifies the
// control-dispatcher path (which has an extra legacy workflow-control
// route) also excludes epics on every tier.
func TestEffectiveWorkQueryExcludesEpicsControlDispatcher(t *testing.T) {
	a := Agent{Name: ControlDispatcherAgentName, Dir: "gascity"}
	got := a.EffectiveWorkQuery()
	wantSnippets := []string{
		`bd list --status in_progress --assignee="$cand" --exclude-type=epic --json`,
		`bd ready --assignee="$cand" --exclude-type=epic --json`,
		`bd ready --metadata-field gc.routed_to=gascity/control-dispatcher --unassigned --exclude-type=epic --json`,
		`bd ready --metadata-field gc.routed_to=gascity/workflow-control --unassigned --exclude-type=epic --json`,
	}
	for _, want := range wantSnippets {
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveWorkQuery() missing exclude-type=epic on control-dispatcher tier:\n  want substring: %s\n  got: %s", want, got)
		}
	}
}

// TestEffectiveWorkQueryCustomPreservesOverride verifies that an agent
// that explicitly opts into epic-handling (e.g. an oversight role that
// closes epics once their children complete) can do so via a custom
// work_query — the explicit-override path required by gc-udx.
func TestEffectiveWorkQueryCustomPreservesOverride(t *testing.T) {
	custom := "bd ready --type=epic --assignee=closer --json --limit=1"
	a := Agent{Name: "closer", WorkQuery: custom}
	if got := a.EffectiveWorkQuery(); got != custom {
		t.Errorf("EffectiveWorkQuery() = %q, want %q (custom override must pass through unmodified)", got, custom)
	}
}

// TestEffectiveWorkQuerySkipsEpicLeafScenario simulates the gc-udx
// reproduction state through the runEffectiveWorkQuery harness:
//   - parent epic is open and routed_to the worker
//   - one open leaf child is also routed_to the worker
//
// The default query must return the leaf, never the epic. The fake bd
// stub here only returns results when --exclude-type=epic is present on
// the tier-3 routed query — i.e. it asserts the flag is on the wire.
func TestEffectiveWorkQuerySkipsEpicLeafScenario(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, `#!/bin/sh
set -eu
case "$*" in
  *"--exclude-type=epic"*"--metadata-field gc.routed_to=hello-world/worker"*|\
  *"--metadata-field gc.routed_to=hello-world/worker"*"--exclude-type=epic"*)
    printf '[{"id":"leaf-child","issue_type":"task"}]'
    ;;
  *"--metadata-field gc.routed_to=hello-world/worker"*)
    printf '[{"id":"parent-epic","issue_type":"epic"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	if !strings.Contains(out, "leaf-child") {
		t.Fatalf("EffectiveWorkQuery() did not return the leaf child: %q", out)
	}
	if strings.Contains(out, "parent-epic") {
		t.Fatalf("EffectiveWorkQuery() surfaced the parent epic to the worker: %q", out)
	}
}

func TestDefaultPoolCheckUsesPoolName(t *testing.T) {
	a := Agent{
		Name:              "dog-1",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3),
		PoolName: "hello-world/dog",
	}
	check := a.EffectiveScaleCheck()
	if !strings.Contains(check, "gc.routed_to=hello-world/dog-1") {
		t.Errorf("EffectiveScaleCheck() = %q, want gc.routed_to=hello-world/dog-1", check)
	}
}

func TestDefaultPoolCheckUsesBdReady(t *testing.T) {
	a := Agent{
		Name:              "dog",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3),
	}
	check := a.EffectiveScaleCheck()
	if !strings.Contains(check, "bd ready") {
		t.Errorf("EffectiveScaleCheck() = %q, want bd ready for blocker-aware counting", check)
	}
	if strings.Contains(check, "--type=molecule") {
		t.Errorf("EffectiveScaleCheck() = %q, should not count molecule containers as demand", check)
	}
	if strings.Contains(check, "--status=in_progress") || strings.Contains(check, "${active:-0}") {
		t.Errorf("EffectiveScaleCheck() = %q, should not count in-progress work as new demand", check)
	}
}

func TestValidateAgentsCustomQueries(t *testing.T) {
	// Both set: OK
	agents := []Agent{{
		Name:              "polecat",
		Dir:               "rig",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3), ScaleCheck: "echo 1",
		WorkQuery:  "custom-query",
		SlingQuery: "custom-sling {}",
	}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("both set: unexpected error: %v", err)
	}

	// Neither set: OK (uses defaults)
	agents = []Agent{{
		Name:              "polecat",
		Dir:               "rig",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3), ScaleCheck: "echo 1",
	}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("neither set: unexpected error: %v", err)
	}

	// Only sling_query set: OK (no matched-pair requirement after pool removal)
	agents = []Agent{{
		Name:              "polecat",
		Dir:               "rig",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3), ScaleCheck: "echo 1",
		SlingQuery: "custom-sling {}",
	}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("only sling_query set: unexpected error: %v", err)
	}

	// Only work_query set: OK
	agents = []Agent{{
		Name:              "polecat",
		Dir:               "rig",
		MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3), ScaleCheck: "echo 1",
		WorkQuery: "custom-query",
	}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("only work_query set: unexpected error: %v", err)
	}
}

func TestValidateAgentsFixedAgentUnpairedOK(t *testing.T) {
	// Fixed agents don't require matched pairs.
	agents := []Agent{{
		Name:       "mayor",
		SlingQuery: "custom-sling {}",
	}}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("fixed agent with only sling_query: unexpected error: %v", err)
	}
}

func TestEffectiveScalingNil(t *testing.T) {
	a := Agent{Name: "mayor"}
	if a.EffectiveMinActiveSessions() != 0 {
		t.Errorf("EffectiveMinActiveSessions = %d, want 0", a.EffectiveMinActiveSessions())
	}
	if m := a.EffectiveMaxActiveSessions(); m != nil {
		t.Errorf("EffectiveMaxActiveSessions = %v, want nil", m)
	}
}

func TestEffectiveScalingExplicit(t *testing.T) {
	a := Agent{
		Name:              "worker",
		MinActiveSessions: ptrInt(2), MaxActiveSessions: ptrInt(10), ScaleCheck: "echo 5",
	}
	if a.EffectiveMinActiveSessions() != 2 {
		t.Errorf("EffectiveMinActiveSessions = %d, want 2", a.EffectiveMinActiveSessions())
	}
	if m := a.EffectiveMaxActiveSessions(); m == nil || *m != 10 {
		t.Errorf("EffectiveMaxActiveSessions = %v, want 10", m)
	}
	if a.EffectiveScaleCheck() != "echo 5" {
		t.Errorf("EffectiveScaleCheck = %q, want %q", a.EffectiveScaleCheck(), "echo 5")
	}
}

func TestEffectiveScaleCheckDefaults(t *testing.T) {
	// Check empty → default uses qualified name.
	a := Agent{
		Name:              "refinery",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(1),
	}
	check := a.EffectiveScaleCheck()
	// Default check uses bd ready for blocker-aware routed demand.
	if !strings.Contains(check, "gc.routed_to=refinery") {
		t.Errorf("EffectiveScaleCheck = %q, want gc.routed_to=refinery", check)
	}
	if !strings.Contains(check, "--unassigned") {
		t.Errorf("EffectiveScaleCheck = %q, want --unassigned for new unassigned demand", check)
	}
	if strings.Contains(check, "--type=molecule") || strings.Contains(check, "${molecules:-0}") {
		t.Errorf("EffectiveScaleCheck = %q, should not count molecule containers as demand", check)
	}
	if strings.Contains(check, "--status=in_progress") || strings.Contains(check, "${active:-0}") {
		t.Errorf("EffectiveScaleCheck = %q, should not count in-progress work as new demand", check)
	}
}

func TestEffectiveScaleCheckDefaultsQualified(t *testing.T) {
	// Rig-scoped agent: default check uses qualified name (dir/name).
	a := Agent{
		Name:              "polecat",
		Dir:               "myproject",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5),
	}
	check := a.EffectiveScaleCheck()
	if !strings.Contains(check, "gc.routed_to=myproject/polecat") {
		t.Errorf("EffectiveScaleCheck = %q, want gc.routed_to=myproject/polecat", check)
	}
	if !strings.Contains(check, "--unassigned") {
		t.Errorf("EffectiveScaleCheck = %q, want --unassigned for new unassigned demand", check)
	}
	if strings.Contains(check, "--type=molecule") {
		t.Errorf("EffectiveScaleCheck = %q, should not count molecule containers as demand", check)
	}
	if strings.Contains(check, "--status=in_progress") || strings.Contains(check, "${active:-0}") {
		t.Errorf("EffectiveScaleCheck = %q, should not count in-progress work as new demand", check)
	}
}

func TestEffectiveScaleCheckUsesReadyOnly(t *testing.T) {
	// Formula-dispatched executable roots must be visible through ready()
	// as runnable wisps/tasks; molecule containers are not demand.
	a := Agent{
		Name:              "worker",
		Dir:               "myrig",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(3),
	}
	check := a.EffectiveScaleCheck()

	if !strings.Contains(check, "bd ready") {
		t.Errorf("missing bd ready query for blocker-aware task counting")
	}
	if strings.Contains(check, "--status=open --type=molecule") {
		t.Errorf("unexpected molecule query in scale check: %q", check)
	}
	if strings.Contains(check, "--status=in_progress") || strings.Contains(check, "${active:-0}") {
		t.Errorf("EffectiveScaleCheck = %q, should not count in-progress work as new demand", check)
	}

	if !strings.Contains(check, "--limit 0") {
		t.Errorf("missing --limit 0 for complete ready count")
	}
	if strings.Contains(check, "2>/dev/null") || strings.Contains(check, "${ready:-0}") || strings.Contains(check, "|| echo 0") {
		t.Errorf("default scale_check masks bd ready failures as zero: %q", check)
	}
	if strings.Contains(check, "${molecules:-0}") {
		t.Errorf("unexpected ${molecules:-0} in arithmetic sum")
	}
	if !strings.Contains(check, "gc.routed_to=myrig/worker") {
		t.Errorf("ready query missing gc.routed_to=myrig/worker")
	}
}

func TestIsMultiSession(t *testing.T) {
	a := Agent{Name: "worker", MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5)}
	maxSess := a.EffectiveMaxActiveSessions()
	if maxSess == nil || *maxSess == 1 {
		t.Error("agent with max=5 should be multi-session")
	}

	b := Agent{Name: "mayor"}
	maxB := b.EffectiveMaxActiveSessions()
	if maxB != nil {
		t.Errorf("agent without scaling should have nil max, got %v", maxB)
	}
}

func TestMarshalOmitsNilPool(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "pool") {
		t.Errorf("Marshal output should not contain 'pool' when nil:\n%s", data)
	}
}

func TestMixedAgentsWithAndWithoutScaling(t *testing.T) {
	data := []byte(`
[workspace]
name = "mixed"

[[agent]]
name = "mayor"

[[agent]]
name = "worker"
start_command = "echo hello"
min_active_sessions = 0
max_active_sessions = 5
scale_check = "echo 2"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].MaxActiveSessions != nil {
		t.Errorf("mayor.MaxActiveSessions = %v, want nil", cfg.Agents[0].MaxActiveSessions)
	}
	if cfg.Agents[1].MaxActiveSessions == nil {
		t.Fatal("worker.MaxActiveSessions is nil, want non-nil")
	}
	if *cfg.Agents[1].MaxActiveSessions != 5 {
		t.Errorf("worker.MaxActiveSessions = %d, want 5", *cfg.Agents[1].MaxActiveSessions)
	}
}

func TestValidateAgentsDupName(t *testing.T) {
	agents := []Agent{
		{Name: "worker"},
		{Name: "worker"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want 'duplicate'", err)
	}
}

func TestValidatePoolMinGtMax(t *testing.T) {
	agents := []Agent{{
		Name:              "worker",
		MinActiveSessions: ptrInt(10), MaxActiveSessions: ptrInt(5),
	}}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for min > max")
	}
	if !strings.Contains(err.Error(), "min") && !strings.Contains(err.Error(), "max") {
		t.Errorf("error = %q, want mention of min/max", err)
	}
}

func TestValidatePoolMaxZero(t *testing.T) {
	// Max=0 is valid (disabled agent).
	agents := []Agent{{
		Name:              "worker",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(0),
	}}
	err := ValidateAgents(agents)
	if err != nil {
		t.Errorf("ValidateAgents: unexpected error: %v", err)
	}
}

func TestValidatePoolMaxUnlimited(t *testing.T) {
	// max=-1 is valid (unlimited pool).
	agents := []Agent{{
		Name:              "polecat",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(-1),
	}}
	err := ValidateAgents(agents)
	if err != nil {
		t.Errorf("ValidateAgents: unexpected error for max=-1: %v", err)
	}
}

func TestValidatePoolMaxBelowNegOne(t *testing.T) {
	// max=-2 is invalid.
	agents := []Agent{{
		Name:              "polecat",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(-2),
	}}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for max=-2")
	}
	if !strings.Contains(err.Error(), "must be >= -1") {
		t.Errorf("error = %q, want mention of >= -1", err)
	}
}

func TestValidatePoolMinGtMaxUnlimited(t *testing.T) {
	// min > 0 with max=-1 should be valid (unlimited allows any min).
	agents := []Agent{{
		Name:              "polecat",
		MinActiveSessions: ptrInt(5), MaxActiveSessions: ptrInt(-1),
	}}
	err := ValidateAgents(agents)
	if err != nil {
		t.Errorf("ValidateAgents: unexpected error for min=5, max=-1: %v", err)
	}
}

func TestMaxActiveSessionsUnlimited(t *testing.T) {
	tests := []struct {
		max  int
		want bool // unlimited = max < 0
	}{
		{-1, true},
		{0, false},
		{1, false},
		{5, false},
	}
	for _, tt := range tests {
		a := Agent{Name: "test", MaxActiveSessions: ptrInt(tt.max)}
		m := a.EffectiveMaxActiveSessions()
		got := m != nil && *m < 0
		if got != tt.want {
			t.Errorf("MaxActiveSessions=%d: unlimited = %v, want %v", tt.max, got, tt.want)
		}
	}
}

func TestMaxActiveSessionsMultiInstance(t *testing.T) {
	tests := []struct {
		max  int
		want bool // multi-instance = max > 1 or max < 0
	}{
		{-1, true}, // unlimited
		{0, false}, // disabled
		{1, false}, // single instance
		{2, true},  // multi-instance
		{10, true}, // multi-instance
	}
	for _, tt := range tests {
		a := Agent{Name: "test", MaxActiveSessions: ptrInt(tt.max)}
		m := a.EffectiveMaxActiveSessions()
		got := m != nil && (*m > 1 || *m < 0)
		if got != tt.want {
			t.Errorf("MaxActiveSessions=%d: multiInstance = %v, want %v", tt.max, got, tt.want)
		}
	}
}

func TestValidateAgentsValid(t *testing.T) {
	agents := []Agent{
		{Name: "mayor"},
		{Name: "worker", MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(10), ScaleCheck: "echo 3"},
	}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("ValidateAgents: unexpected error: %v", err)
	}
}

func TestValidateAgentsMissingName(t *testing.T) {
	agents := []Agent{{MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5)}}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %q, want 'name is required'", err)
	}
}

func TestValidateAgentsInvalidName(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		wantErr string
	}{
		{"spaces", "my agent", "name must match"},
		{"slash", "a/b", "name must match"},
		{"dot", "agent.1", "name must match"},
		{"empty start", "", "name is required"},
		{"starts with hyphen", "-agent", "name must match"},
		{"starts with underscore", "_agent", "name must match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAgents([]Agent{{Name: tt.agent}})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAgentsValidNames(t *testing.T) {
	// These should all pass.
	for _, name := range []string{"mayor", "worker-1", "agent_A", "X", "a1"} {
		err := ValidateAgents([]Agent{{Name: name}})
		if err != nil {
			t.Errorf("ValidateAgents(%q): unexpected error: %v", name, err)
		}
	}
}

func TestValidateAgentsPoolMaxZeroIsValid(t *testing.T) {
	// pool.Max == 0 is valid — used to intentionally disable an agent.
	agents := []Agent{
		{Name: "worker", MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(0)},
	}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("ValidateAgents: unexpected error: %v", err)
	}
}

func TestValidateAgentsPoolCheckEmptyIsValid(t *testing.T) {
	// Empty check is valid — EffectivePool() provides a default check command.
	agents := []Agent{
		{Name: "worker", MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5)},
	}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("ValidateAgents: unexpected error for empty check: %v", err)
	}
}

// --- DaemonConfig tests ---

func TestDaemonPatrolIntervalDefault(t *testing.T) {
	d := DaemonConfig{}
	got := d.PatrolIntervalDuration()
	if got != 30*time.Second {
		t.Errorf("PatrolIntervalDuration() = %v, want 30s", got)
	}
}

func TestDaemonPatrolIntervalCustom(t *testing.T) {
	d := DaemonConfig{PatrolInterval: "10s"}
	got := d.PatrolIntervalDuration()
	if got != 10*time.Second {
		t.Errorf("PatrolIntervalDuration() = %v, want 10s", got)
	}
}

func TestDaemonPatrolIntervalInvalid(t *testing.T) {
	d := DaemonConfig{PatrolInterval: "not-a-duration"}
	got := d.PatrolIntervalDuration()
	if got != 30*time.Second {
		t.Errorf("PatrolIntervalDuration() = %v, want 30s (default for invalid)", got)
	}
}

func TestParseDaemonConfig(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
patrol_interval = "15s"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.PatrolInterval != "15s" {
		t.Errorf("Daemon.PatrolInterval = %q, want %q", cfg.Daemon.PatrolInterval, "15s")
	}
	got := cfg.Daemon.PatrolIntervalDuration()
	if got != 15*time.Second {
		t.Errorf("PatrolIntervalDuration() = %v, want 15s", got)
	}
}

func TestParseDaemonConfigMissing(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.PatrolInterval != "" {
		t.Errorf("Daemon.PatrolInterval = %q, want empty", cfg.Daemon.PatrolInterval)
	}
	// Should still default to 30s.
	got := cfg.Daemon.PatrolIntervalDuration()
	if got != 30*time.Second {
		t.Errorf("PatrolIntervalDuration() = %v, want 30s", got)
	}
}

func TestParseDaemonNudgeDispatcher(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
nudge_dispatcher = "supervisor"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.NudgeDispatcher != "supervisor" {
		t.Errorf("Daemon.NudgeDispatcher = %q, want %q", cfg.Daemon.NudgeDispatcher, "supervisor")
	}
	if got := cfg.Daemon.NudgeDispatcherMode(); got != "supervisor" {
		t.Errorf("NudgeDispatcherMode() = %q, want %q", got, "supervisor")
	}
}

func TestDaemonNudgeDispatcherDefault(t *testing.T) {
	d := DaemonConfig{}
	if got := d.NudgeDispatcherMode(); got != "legacy" {
		t.Errorf("NudgeDispatcherMode() = %q, want %q", got, "legacy")
	}
}

func TestDaemonNudgeDispatcherUnknownFallsBack(t *testing.T) {
	d := DaemonConfig{NudgeDispatcher: "garbage"}
	if got := d.NudgeDispatcherMode(); got != "legacy" {
		t.Errorf("NudgeDispatcherMode() with unknown value = %q, want %q", got, "legacy")
	}
}

func TestDaemonMaxRestartsDefault(t *testing.T) {
	d := DaemonConfig{}
	got := d.MaxRestartsOrDefault()
	if got != 5 {
		t.Errorf("MaxRestartsOrDefault() = %d, want 5", got)
	}
}

func TestDaemonMaxRestartsExplicit(t *testing.T) {
	v := 3
	d := DaemonConfig{MaxRestarts: &v}
	got := d.MaxRestartsOrDefault()
	if got != 3 {
		t.Errorf("MaxRestartsOrDefault() = %d, want 3", got)
	}
}

func TestDaemonMaxRestartsZero(t *testing.T) {
	v := 0
	d := DaemonConfig{MaxRestarts: &v}
	got := d.MaxRestartsOrDefault()
	if got != 0 {
		t.Errorf("MaxRestartsOrDefault() = %d, want 0 (unlimited)", got)
	}
}

func TestDaemonRestartWindowDefault(t *testing.T) {
	d := DaemonConfig{}
	got := d.RestartWindowDuration()
	if got != time.Hour {
		t.Errorf("RestartWindowDuration() = %v, want 1h", got)
	}
}

func TestDaemonRestartWindowCustom(t *testing.T) {
	d := DaemonConfig{RestartWindow: "30m"}
	got := d.RestartWindowDuration()
	if got != 30*time.Minute {
		t.Errorf("RestartWindowDuration() = %v, want 30m", got)
	}
}

func TestDaemonRestartWindowInvalid(t *testing.T) {
	d := DaemonConfig{RestartWindow: "not-a-duration"}
	got := d.RestartWindowDuration()
	if got != time.Hour {
		t.Errorf("RestartWindowDuration() = %v, want 1h (default for invalid)", got)
	}
}

func TestParseDaemonCrashLoopConfig(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
patrol_interval = "15s"
max_restarts = 3
restart_window = "30m"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.MaxRestarts == nil || *cfg.Daemon.MaxRestarts != 3 {
		t.Errorf("Daemon.MaxRestarts = %v, want 3", cfg.Daemon.MaxRestarts)
	}
	if cfg.Daemon.RestartWindow != "30m" {
		t.Errorf("Daemon.RestartWindow = %q, want %q", cfg.Daemon.RestartWindow, "30m")
	}
	if got := cfg.Daemon.MaxRestartsOrDefault(); got != 3 {
		t.Errorf("MaxRestartsOrDefault() = %d, want 3", got)
	}
	if got := cfg.Daemon.RestartWindowDuration(); got != 30*time.Minute {
		t.Errorf("RestartWindowDuration() = %v, want 30m", got)
	}
}

func TestParseDaemonMaxRestartsZero(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
max_restarts = 0

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.MaxRestarts == nil {
		t.Fatal("Daemon.MaxRestarts is nil, want 0")
	}
	if *cfg.Daemon.MaxRestarts != 0 {
		t.Errorf("Daemon.MaxRestarts = %d, want 0", *cfg.Daemon.MaxRestarts)
	}
	if got := cfg.Daemon.MaxRestartsOrDefault(); got != 0 {
		t.Errorf("MaxRestartsOrDefault() = %d, want 0 (unlimited)", got)
	}
}

func TestParseDaemonSessionCircuitBreaker(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
session_circuit_breaker = true
session_circuit_breaker_max_restarts = 2
session_circuit_breaker_window = "10m"
session_circuit_breaker_reset_after = "25m"

[[agent]]
name = "worker"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Daemon.SessionCircuitBreaker {
		t.Fatal("Daemon.SessionCircuitBreaker = false, want true")
	}
	if cfg.Daemon.SessionCircuitBreakerMaxRestarts == nil || *cfg.Daemon.SessionCircuitBreakerMaxRestarts != 2 {
		t.Fatalf("Daemon.SessionCircuitBreakerMaxRestarts = %v, want 2", cfg.Daemon.SessionCircuitBreakerMaxRestarts)
	}
	if got := cfg.Daemon.SessionCircuitBreakerMaxRestartsOrDefault(); got != 2 {
		t.Fatalf("SessionCircuitBreakerMaxRestartsOrDefault() = %d, want 2", got)
	}
	if got := cfg.Daemon.SessionCircuitBreakerWindowDuration(); got != 10*time.Minute {
		t.Fatalf("SessionCircuitBreakerWindowDuration() = %v, want 10m", got)
	}
	if got := cfg.Daemon.SessionCircuitBreakerResetAfterDuration(); got != 25*time.Minute {
		t.Fatalf("SessionCircuitBreakerResetAfterDuration() = %v, want 25m", got)
	}
}

func TestMarshalOmitsEmptyDaemonSection(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[daemon]") {
		t.Errorf("Marshal output should not contain '[daemon]' when empty:\n%s", data)
	}
}

// --- ShutdownTimeout tests ---

func TestDaemonShutdownTimeoutDefault(t *testing.T) {
	d := DaemonConfig{}
	got := d.ShutdownTimeoutDuration()
	if got != 5*time.Second {
		t.Errorf("ShutdownTimeoutDuration() = %v, want 5s", got)
	}
}

func TestDaemonShutdownTimeoutCustom(t *testing.T) {
	d := DaemonConfig{ShutdownTimeout: "3s"}
	got := d.ShutdownTimeoutDuration()
	if got != 3*time.Second {
		t.Errorf("ShutdownTimeoutDuration() = %v, want 3s", got)
	}
}

func TestDaemonShutdownTimeoutZero(t *testing.T) {
	d := DaemonConfig{ShutdownTimeout: "0s"}
	got := d.ShutdownTimeoutDuration()
	if got != 0 {
		t.Errorf("ShutdownTimeoutDuration() = %v, want 0", got)
	}
}

func TestDaemonShutdownTimeoutInvalid(t *testing.T) {
	d := DaemonConfig{ShutdownTimeout: "not-a-duration"}
	got := d.ShutdownTimeoutDuration()
	if got != 5*time.Second {
		t.Errorf("ShutdownTimeoutDuration() = %v, want 5s (default for invalid)", got)
	}
}

func TestParseShutdownTimeout(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
patrol_interval = "15s"
shutdown_timeout = "3s"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.ShutdownTimeout != "3s" {
		t.Errorf("Daemon.ShutdownTimeout = %q, want %q", cfg.Daemon.ShutdownTimeout, "3s")
	}
	got := cfg.Daemon.ShutdownTimeoutDuration()
	if got != 3*time.Second {
		t.Errorf("ShutdownTimeoutDuration() = %v, want 3s", got)
	}
}

// --- DriftDrainTimeout tests ---

func TestDaemonDriftDrainTimeoutDefault(t *testing.T) {
	d := DaemonConfig{}
	got := d.DriftDrainTimeoutDuration()
	if got != 2*time.Minute {
		t.Errorf("DriftDrainTimeoutDuration() = %v, want 2m", got)
	}
}

func TestDaemonDriftDrainTimeoutCustom(t *testing.T) {
	d := DaemonConfig{DriftDrainTimeout: "5m"}
	got := d.DriftDrainTimeoutDuration()
	if got != 5*time.Minute {
		t.Errorf("DriftDrainTimeoutDuration() = %v, want 5m", got)
	}
}

func TestDaemonDriftDrainTimeoutInvalid(t *testing.T) {
	d := DaemonConfig{DriftDrainTimeout: "not-a-duration"}
	got := d.DriftDrainTimeoutDuration()
	if got != 2*time.Minute {
		t.Errorf("DriftDrainTimeoutDuration() = %v, want 2m (default for invalid)", got)
	}
}

func TestParseDriftDrainTimeout(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
drift_drain_timeout = "3m"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.DriftDrainTimeout != "3m" {
		t.Errorf("Daemon.DriftDrainTimeout = %q, want %q", cfg.Daemon.DriftDrainTimeout, "3m")
	}
	got := cfg.Daemon.DriftDrainTimeoutDuration()
	if got != 3*time.Minute {
		t.Errorf("DriftDrainTimeoutDuration() = %v, want 3m", got)
	}
}

// --- ProbeConcurrency tests ---

func TestDaemonProbeConcurrencyDefault(t *testing.T) {
	d := DaemonConfig{}
	got := d.ProbeConcurrencyOrDefault()
	if got != DefaultProbeConcurrency {
		t.Errorf("ProbeConcurrencyOrDefault() = %d, want %d", got, DefaultProbeConcurrency)
	}
}

func TestDaemonProbeConcurrencyExplicit(t *testing.T) {
	v := 16
	d := DaemonConfig{ProbeConcurrency: &v}
	got := d.ProbeConcurrencyOrDefault()
	if got != 16 {
		t.Errorf("ProbeConcurrencyOrDefault() = %d, want 16", got)
	}
}

func TestDaemonProbeConcurrencyZeroClamped(t *testing.T) {
	v := 0
	d := DaemonConfig{ProbeConcurrency: &v}
	got := d.ProbeConcurrencyOrDefault()
	if got != 1 {
		t.Errorf("ProbeConcurrencyOrDefault() = %d, want 1 (clamped)", got)
	}
}

func TestDaemonProbeConcurrencyNegativeClamped(t *testing.T) {
	v := -5
	d := DaemonConfig{ProbeConcurrency: &v}
	got := d.ProbeConcurrencyOrDefault()
	if got != 1 {
		t.Errorf("ProbeConcurrencyOrDefault() = %d, want 1 (clamped)", got)
	}
}

func TestParseProbeConcurrency(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[daemon]
probe_concurrency = 12

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.ProbeConcurrency == nil {
		t.Fatal("Daemon.ProbeConcurrency is nil, want 12")
	}
	if *cfg.Daemon.ProbeConcurrency != 12 {
		t.Errorf("Daemon.ProbeConcurrency = %d, want 12", *cfg.Daemon.ProbeConcurrency)
	}
	got := cfg.Daemon.ProbeConcurrencyOrDefault()
	if got != 12 {
		t.Errorf("ProbeConcurrencyOrDefault() = %d, want 12", got)
	}
}

// --- DrainTimeout tests ---

func TestDrainTimeoutDefault(t *testing.T) {
	a := Agent{Name: "test"}
	got := a.DrainTimeoutDuration()
	if got != 5*time.Minute {
		t.Errorf("DrainTimeoutDuration() = %v, want 5m", got)
	}
}

func TestDrainTimeoutCustom(t *testing.T) {
	a := Agent{Name: "test", DrainTimeout: "30s"}
	got := a.DrainTimeoutDuration()
	if got != 30*time.Second {
		t.Errorf("DrainTimeoutDuration() = %v, want 30s", got)
	}
}

func TestDrainTimeoutInvalid(t *testing.T) {
	a := Agent{Name: "test", DrainTimeout: "not-a-duration"}
	got := a.DrainTimeoutDuration()
	if got != 5*time.Minute {
		t.Errorf("DrainTimeoutDuration() = %v, want 5m (default for invalid)", got)
	}
}

func TestParseDrainTimeout(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"
start_command = "echo hello"
min_active_sessions = 0
max_active_sessions = 5
scale_check = "echo 3"
drain_timeout = "2m"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.DrainTimeout != "2m" {
		t.Errorf("DrainTimeout = %q, want %q", a.DrainTimeout, "2m")
	}
	got := a.DrainTimeoutDuration()
	if got != 2*time.Minute {
		t.Errorf("DrainTimeoutDuration() = %v, want 2m", got)
	}
}

func TestDrainTimeoutRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents: []Agent{{
			Name:              "worker",
			MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5), ScaleCheck: "echo 3", DrainTimeout: "3m",
		}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if got.Agents[0].DrainTimeout != "3m" {
		t.Errorf("DrainTimeout after round-trip = %q, want %q", got.Agents[0].DrainTimeout, "3m")
	}
}

func TestDrainTimeoutOmittedWhenEmpty(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents: []Agent{{
			Name:              "worker",
			MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5), ScaleCheck: "echo 3",
		}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "drain_timeout") {
		t.Errorf("Marshal output should not contain 'drain_timeout' when empty:\n%s", data)
	}
}

func TestRigsParsing(t *testing.T) {
	input := `
[workspace]
name = "my-city"

[[agent]]
name = "mayor"

[[rigs]]
name = "frontend"
path = "/home/user/projects/my-frontend"
prefix = "fe"

[[rigs]]
name = "backend"
path = "/home/user/projects/my-backend"
`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Rigs) != 2 {
		t.Fatalf("len(Rigs) = %d, want 2", len(cfg.Rigs))
	}
	if cfg.Rigs[0].Name != "frontend" {
		t.Errorf("Rigs[0].Name = %q, want %q", cfg.Rigs[0].Name, "frontend")
	}
	if cfg.Rigs[0].Path != "/home/user/projects/my-frontend" {
		t.Errorf("Rigs[0].Path = %q, want %q", cfg.Rigs[0].Path, "/home/user/projects/my-frontend")
	}
	if cfg.Rigs[0].Prefix != "fe" {
		t.Errorf("Rigs[0].Prefix = %q, want %q", cfg.Rigs[0].Prefix, "fe")
	}
	if cfg.Rigs[1].Name != "backend" {
		t.Errorf("Rigs[1].Name = %q, want %q", cfg.Rigs[1].Name, "backend")
	}
	if cfg.Rigs[1].Prefix != "" {
		t.Errorf("Rigs[1].Prefix = %q, want empty (derived at runtime)", cfg.Rigs[1].Prefix)
	}
}

func TestRigsRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor"}},
		Rigs: []Rig{
			{Name: "frontend", Path: "/home/user/frontend", Prefix: "fe"},
			{Name: "backend", Path: "/home/user/backend"},
		},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if len(got.Rigs) != 2 {
		t.Fatalf("len(Rigs) after round-trip = %d, want 2", len(got.Rigs))
	}
	if got.Rigs[0].Prefix != "fe" {
		t.Errorf("Rigs[0].Prefix after round-trip = %q, want %q", got.Rigs[0].Prefix, "fe")
	}
	if got.Rigs[1].Path != "/home/user/backend" {
		t.Errorf("Rigs[1].Path after round-trip = %q, want %q", got.Rigs[1].Path, "/home/user/backend")
	}
}

// TestRigFormulaVarsRoundTrip verifies that rigs.<name>.formula_vars survives
// TOML marshal/unmarshal so city.toml can declare rig-scoped formula defaults.
func TestRigFormulaVarsRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor"}},
		Rigs: []Rig{
			{
				Name: "mo",
				Path: "/home/user/mo",
				FormulaVars: map[string]string{
					"test_command": "make test-fast",
					"lint_command": "golangci-lint run",
				},
			},
		},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if len(got.Rigs) != 1 {
		t.Fatalf("len(Rigs) after round-trip = %d, want 1", len(got.Rigs))
	}
	if v := got.Rigs[0].FormulaVars["test_command"]; v != "make test-fast" {
		t.Errorf("FormulaVars[test_command] = %q, want %q", v, "make test-fast")
	}
	if v := got.Rigs[0].FormulaVars["lint_command"]; v != "golangci-lint run" {
		t.Errorf("FormulaVars[lint_command] = %q, want %q", v, "golangci-lint run")
	}
}

// TestRigFormulaVarsParsing verifies the expected TOML surface — a
// [rigs.formula_vars] table inside a [[rigs]] entry — decodes into the
// FormulaVars map on the rig.
func TestRigFormulaVarsParsing(t *testing.T) {
	input := `
[workspace]
name = "my-city"

[[agent]]
name = "mayor"

[[rigs]]
name = "mo"
path = "/home/user/mo"

[rigs.formula_vars]
test_command = "make test-fast"
build_command = "make build"
`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("len(Rigs) = %d, want 1", len(cfg.Rigs))
	}
	if got := cfg.Rigs[0].FormulaVars["test_command"]; got != "make test-fast" {
		t.Errorf("FormulaVars[test_command] = %q, want %q", got, "make test-fast")
	}
	if got := cfg.Rigs[0].FormulaVars["build_command"]; got != "make build" {
		t.Errorf("FormulaVars[build_command] = %q, want %q", got, "make build")
	}
}

// --- DeriveBeadsPrefix tests ---

func TestDeriveBeadsPrefix(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"my-frontend", "mf"},
		{"my-backend", "mb"},
		{"backend", "ba"},
		{"frontend", "fr"},
		{"tower-of-hanoi", "toh"},
		{"api", "api"},
		{"db", "db"},
		{"x", "x"},
		{"myFrontend", "mf"},
		{"GasCity", "gc"},
		{"my-project-go", "mp"}, // strip -go suffix
		{"my-project-py", "mp"}, // strip -py suffix
		{"hello_world", "hw"},
		{"a-b-c-d", "abcd"},
		{"longname", "lo"},
	}
	for _, tt := range tests {
		got := DeriveBeadsPrefix(tt.name)
		if got != tt.want {
			t.Errorf("DeriveBeadsPrefix(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSplitCompoundWord(t *testing.T) {
	tests := []struct {
		word string
		want []string
	}{
		{"myFrontend", []string{"my", "Frontend"}},
		{"GasCity", []string{"Gas", "City"}},
		{"simple", []string{"simple"}},
		{"ABC", []string{"ABC"}},
		{"", []string{""}},
	}
	for _, tt := range tests {
		got := splitCompoundWord(tt.word)
		if len(got) != len(tt.want) {
			t.Errorf("splitCompoundWord(%q) = %v, want %v", tt.word, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitCompoundWord(%q)[%d] = %q, want %q", tt.word, i, got[i], tt.want[i])
			}
		}
	}
}

func TestEffectivePrefix_Explicit(t *testing.T) {
	r := Rig{Name: "frontend", Path: "/path", Prefix: "fe"}
	if got := r.EffectivePrefix(); got != "fe" {
		t.Errorf("EffectivePrefix() = %q, want %q", got, "fe")
	}
}

func TestEffectivePrefix_Derived(t *testing.T) {
	r := Rig{Name: "my-frontend", Path: "/path"}
	if got := r.EffectivePrefix(); got != "mf" {
		t.Errorf("EffectivePrefix() = %q, want %q", got, "mf")
	}
}

// --- ValidateRigs tests ---

func TestValidateRigs_Valid(t *testing.T) {
	rigs := []Rig{
		{Name: "frontend", Path: "/home/user/frontend", Prefix: "fe"},
		{Name: "backend", Path: "/home/user/backend"},
	}
	if err := ValidateRigs(rigs, "mc"); err != nil {
		t.Errorf("ValidateRigs: unexpected error: %v", err)
	}
}

func TestValidateRigs_Empty(t *testing.T) {
	if err := ValidateRigs(nil, "mc"); err != nil {
		t.Errorf("ValidateRigs(nil): unexpected error: %v", err)
	}
}

func TestValidateRigs_MissingName(t *testing.T) {
	rigs := []Rig{{Path: "/path"}}
	err := ValidateRigs(rigs, "ci")
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error = %q, want 'name is required'", err)
	}
}

func TestValidateRigs_MissingPath(t *testing.T) {
	rigs := []Rig{{Name: "frontend"}}
	err := ValidateRigs(rigs, "ci")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("error = %q, want 'path is required'", err)
	}
}

func TestValidateRigs_WildcardNameRejected(t *testing.T) {
	rigs := []Rig{{Name: "*", Path: "/a"}}
	err := ValidateRigs(rigs, "ci")
	if err == nil {
		t.Fatal(`expected error for rig name "*"`)
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error = %q, want 'wildcard'", err)
	}
}

func TestValidateRigs_DuplicateName(t *testing.T) {
	rigs := []Rig{
		{Name: "frontend", Path: "/a"},
		{Name: "frontend", Path: "/b"},
	}
	err := ValidateRigs(rigs, "ci")
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want 'duplicate'", err)
	}
}

// Regression: Bug 3 — prefix collisions between rigs must be detected.
func TestValidateRigs_PrefixCollision(t *testing.T) {
	rigs := []Rig{
		{Name: "my-frontend", Path: "/a"}, // prefix "mf"
		{Name: "my-foo", Path: "/b"},      // prefix "mf" — collision!
	}
	err := ValidateRigs(rigs, "ci")
	if err == nil {
		t.Fatal("expected error for prefix collision")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error = %q, want 'collides'", err)
	}
}

// Regression: Bug 3 — prefix collision with HQ must also be detected.
func TestValidateRigs_PrefixCollidesWithHQ(t *testing.T) {
	// HQ prefix "mc" collides with rig "my-cloud" (derived prefix "mc")
	rigs := []Rig{
		{Name: "my-cloud", Path: "/path"}, // prefix "mc" — collides with HQ!
	}
	err := ValidateRigs(rigs, "mc")
	if err == nil {
		t.Fatal("expected error for prefix collision with HQ")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error = %q, want 'collides'", err)
	}
	if !strings.Contains(err.Error(), "HQ") {
		t.Errorf("error = %q, want mention of HQ", err)
	}
}

func TestValidateRigs_ExplicitPrefixAvoidsCollision(t *testing.T) {
	// Same derived prefix but explicit override avoids collision.
	rigs := []Rig{
		{Name: "my-frontend", Path: "/a"},            // derived "mf"
		{Name: "my-foo", Path: "/b", Prefix: "mfoo"}, // explicit — no collision
	}
	if err := ValidateRigs(rigs, "ci"); err != nil {
		t.Errorf("ValidateRigs: unexpected error: %v", err)
	}
}

func TestEffectiveHQPrefix_Explicit(t *testing.T) {
	cfg := &City{Workspace: Workspace{Name: "gascity", Prefix: "hq"}}
	if got := EffectiveHQPrefix(cfg); got != "hq" {
		t.Errorf("EffectiveHQPrefix() = %q, want %q", got, "hq")
	}
}

func TestEffectiveHQPrefix_Derived(t *testing.T) {
	cfg := &City{Workspace: Workspace{Name: "gascity"}}
	if got := EffectiveHQPrefix(cfg); got != "ga" {
		t.Errorf("EffectiveHQPrefix() = %q, want %q", got, "ga")
	}
}

func TestEffectiveHQPrefix_FallbackToResolvedName(t *testing.T) {
	cfg := &City{
		Workspace:             Workspace{},
		ResolvedWorkspaceName: "my-project",
	}
	if got := EffectiveHQPrefix(cfg); got != "mp" {
		t.Errorf("EffectiveHQPrefix() = %q, want %q", got, "mp")
	}
}

func TestEffectiveHQPrefix_ExplicitPrefixOverridesAll(t *testing.T) {
	cfg := &City{
		Workspace:             Workspace{Name: "gascity", Prefix: "custom"},
		ResolvedWorkspaceName: "other",
	}
	if got := EffectiveHQPrefix(cfg); got != "custom" {
		t.Errorf("EffectiveHQPrefix() = %q, want %q", got, "custom")
	}
}

func TestEffectiveHQPrefix_ResolvedPrefixOverridesDeclaredPrefix(t *testing.T) {
	cfg := &City{
		Workspace:               Workspace{Name: "declared-city", Prefix: "declared"},
		ResolvedWorkspaceName:   "site-city",
		ResolvedWorkspacePrefix: "sc",
	}
	if got := EffectiveHQPrefix(cfg); got != "sc" {
		t.Errorf("EffectiveHQPrefix() = %q, want %q", got, "sc")
	}
}

// --- Suspended field tests ---

func TestParseSuspended(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "mayor"

[[agent]]
name = "builder"
suspended = true
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Suspended {
		t.Error("Agents[0].Suspended = true, want false")
	}
	if !cfg.Agents[1].Suspended {
		t.Error("Agents[1].Suspended = false, want true")
	}
}

func TestMarshalOmitsSuspendedFalse(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "suspended") {
		t.Errorf("Marshal output should not contain 'suspended' when false:\n%s", data)
	}
}

func TestMarshalIncludesSuspendedTrue(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "builder", Suspended: true}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), "suspended = true") {
		t.Errorf("Marshal output should contain 'suspended = true':\n%s", data)
	}
}

func TestSuspendedRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents: []Agent{
			{Name: "mayor"},
			{Name: "builder", Suspended: true},
		},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if got.Agents[0].Suspended {
		t.Error("Agents[0].Suspended after round-trip = true, want false")
	}
	if !got.Agents[1].Suspended {
		t.Error("Agents[1].Suspended after round-trip = false, want true")
	}
}

func TestRigsOmittedWhenEmpty(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "rigs") {
		t.Errorf("Marshal output should not contain 'rigs' when empty:\n%s", data)
	}
}

// --- QualifiedName tests ---

func TestQualifiedName(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		want string
	}{
		{name: "mayor", dir: "", want: "mayor"},
		{name: "polecat", dir: "hello-world", want: "hello-world/polecat"},
		{name: "worker-1", dir: "backend", want: "backend/worker-1"},
	}
	for _, tt := range tests {
		a := Agent{Name: tt.name, Dir: tt.dir}
		got := a.QualifiedName()
		if got != tt.want {
			t.Errorf("Agent{Name:%q, Dir:%q}.QualifiedName() = %q, want %q",
				tt.name, tt.dir, got, tt.want)
		}
	}
}

func TestParseQualifiedName(t *testing.T) {
	tests := []struct {
		input   string
		wantDir string
		wantN   string
	}{
		{"mayor", "", "mayor"},
		{"hello-world/polecat", "hello-world", "polecat"},
		{"backend/worker-1", "backend", "worker-1"},
		{"deep/nested/name", "deep/nested", "name"},
	}
	for _, tt := range tests {
		dir, name := ParseQualifiedName(tt.input)
		if dir != tt.wantDir || name != tt.wantN {
			t.Errorf("ParseQualifiedName(%q) = (%q, %q), want (%q, %q)",
				tt.input, dir, name, tt.wantDir, tt.wantN)
		}
	}
}

func TestValidateAgentsSameNameDifferentDir(t *testing.T) {
	agents := []Agent{
		{Name: "polecat", Dir: "frontend"},
		{Name: "polecat", Dir: "backend"},
	}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("ValidateAgents: unexpected error for same name different dir: %v", err)
	}
}

func TestValidateAgentsSameNameSameDir(t *testing.T) {
	agents := []Agent{
		{Name: "polecat", Dir: "frontend"},
		{Name: "polecat", Dir: "frontend"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for same name same dir")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want 'duplicate'", err)
	}
}

func TestValidateAgentsSameNameCityWide(t *testing.T) {
	// Two city-wide agents with the same name should still be rejected.
	agents := []Agent{
		{Name: "worker"},
		{Name: "worker"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for duplicate city-wide name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want 'duplicate'", err)
	}
}

func TestValidateAgentsDupNameWithProvenance(t *testing.T) {
	// When both agents have SourceDir set, the error should include provenance.
	agents := []Agent{
		{Name: "worker", Dir: "myrig", SourceDir: "packs/base"},
		{Name: "worker", Dir: "myrig", SourceDir: "packs/extras"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "packs/base") {
		t.Errorf("error should include first source dir, got: %s", errStr)
	}
	if !strings.Contains(errStr, "packs/extras") {
		t.Errorf("error should include second source dir, got: %s", errStr)
	}
}

func TestValidateAgentsDupNameMixedProvenance(t *testing.T) {
	// Inline agent (no SourceDir) colliding with pack agent (has SourceDir)
	// should still include the available provenance.
	agents := []Agent{
		{Name: "worker"},
		{Name: "worker", SourceDir: "packs/extras"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "packs/extras") {
		t.Errorf("error should include source dir, got: %s", errStr)
	}
}

func TestValidateAgentsDupNameNoProvenance(t *testing.T) {
	// Two zero-valued agents (no SourceDir, no source enum). Per ga-tpfc.1
	// the rendered descriptor must always be non-empty; an unstamped agent
	// renders as <unknown: ...>. The error must never contain an empty
	// quoted "" path, regardless of the source category.
	agents := []Agent{
		{Name: "worker"},
		{Name: "worker"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "duplicate name") {
		t.Errorf("error should say 'duplicate name', got: %s", errStr)
	}
	if strings.Contains(errStr, `""`) {
		t.Errorf(`error contains empty quoted "" — descriptors must always be non-empty, got: %s`, errStr)
	}
}

// --- IdleTimeout tests ---

func TestIdleTimeoutDurationEmpty(t *testing.T) {
	a := Agent{Name: "mayor"}
	if got := a.IdleTimeoutDuration(); got != 0 {
		t.Errorf("IdleTimeoutDuration() = %v, want 0", got)
	}
}

func TestIdleTimeoutDurationValid(t *testing.T) {
	a := Agent{Name: "mayor", IdleTimeout: "15m"}
	if got := a.IdleTimeoutDuration(); got != 15*time.Minute {
		t.Errorf("IdleTimeoutDuration() = %v, want 15m", got)
	}
}

func TestIdleTimeoutDurationInvalid(t *testing.T) {
	a := Agent{Name: "mayor", IdleTimeout: "bogus"}
	if got := a.IdleTimeoutDuration(); got != 0 {
		t.Errorf("IdleTimeoutDuration() = %v, want 0 for invalid", got)
	}
}

func TestIdleTimeoutRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor", IdleTimeout: "30m"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Agents[0].IdleTimeout != "30m" {
		t.Errorf("IdleTimeout after round-trip = %q, want %q", got.Agents[0].IdleTimeout, "30m")
	}
}

func TestIdleTimeoutOmittedWhenEmpty(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "idle_timeout") {
		t.Errorf("TOML output should omit idle_timeout when empty, got:\n%s", data)
	}
}

// --- MaxSessionAge tests ---

func TestMaxSessionAgeDurationEmpty(t *testing.T) {
	a := Agent{Name: "witness"}
	if got := a.MaxSessionAgeDuration(); got != 0 {
		t.Errorf("MaxSessionAgeDuration() = %v, want 0", got)
	}
}

func TestMaxSessionAgeDurationValid(t *testing.T) {
	a := Agent{Name: "witness", MaxSessionAge: "5h"}
	if got := a.MaxSessionAgeDuration(); got != 5*time.Hour {
		t.Errorf("MaxSessionAgeDuration() = %v, want 5h", got)
	}
}

func TestMaxSessionAgeDurationInvalid(t *testing.T) {
	a := Agent{Name: "witness", MaxSessionAge: "bogus"}
	if got := a.MaxSessionAgeDuration(); got != 0 {
		t.Errorf("MaxSessionAgeDuration() = %v, want 0 for invalid", got)
	}
}

func TestMaxSessionAgeJitterDurationIgnoredWhenAgeUnset(t *testing.T) {
	a := Agent{Name: "witness", MaxSessionAgeJitter: "15m"}
	if got := a.MaxSessionAgeJitterDuration(); got != 0 {
		t.Errorf("MaxSessionAgeJitterDuration() = %v, want 0 when MaxSessionAge unset", got)
	}
}

func TestMaxSessionAgeJitterDurationValid(t *testing.T) {
	a := Agent{Name: "witness", MaxSessionAge: "5h", MaxSessionAgeJitter: "15m"}
	if got := a.MaxSessionAgeJitterDuration(); got != 15*time.Minute {
		t.Errorf("MaxSessionAgeJitterDuration() = %v, want 15m", got)
	}
}

func TestMaxSessionAgeJitterDurationNegativeRejected(t *testing.T) {
	a := Agent{Name: "witness", MaxSessionAge: "5h", MaxSessionAgeJitter: "-5m"}
	if got := a.MaxSessionAgeJitterDuration(); got != 0 {
		t.Errorf("MaxSessionAgeJitterDuration() = %v, want 0 for negative value", got)
	}
}

func TestMaxSessionAgeRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "witness", MaxSessionAge: "5h", MaxSessionAgeJitter: "15m"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Agents[0].MaxSessionAge != "5h" {
		t.Errorf("MaxSessionAge after round-trip = %q, want %q", got.Agents[0].MaxSessionAge, "5h")
	}
	if got.Agents[0].MaxSessionAgeJitter != "15m" {
		t.Errorf("MaxSessionAgeJitter after round-trip = %q, want %q", got.Agents[0].MaxSessionAgeJitter, "15m")
	}
}

func TestMaxSessionAgeOmittedWhenEmpty(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "witness"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "max_session_age") {
		t.Errorf("TOML output should omit max_session_age when empty, got:\n%s", data)
	}
}

// --- install_agent_hooks ---

func TestParseInstallAgentHooksWorkspace(t *testing.T) {
	toml := `
[workspace]
name = "test"
install_agent_hooks = ["claude", "gemini"]

[[agent]]
name = "mayor"
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Workspace.InstallAgentHooks) != 2 {
		t.Fatalf("Workspace.InstallAgentHooks = %v, want 2 entries", cfg.Workspace.InstallAgentHooks)
	}
	if cfg.Workspace.InstallAgentHooks[0] != "claude" || cfg.Workspace.InstallAgentHooks[1] != "gemini" {
		t.Errorf("Workspace.InstallAgentHooks = %v, want [claude gemini]", cfg.Workspace.InstallAgentHooks)
	}
}

func TestParseInstallAgentHooksAgent(t *testing.T) {
	toml := `
[workspace]
name = "test"
install_agent_hooks = ["claude"]

[[agent]]
name = "polecat"
install_agent_hooks = ["gemini", "copilot"]
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents[0].InstallAgentHooks) != 2 {
		t.Fatalf("Agent.InstallAgentHooks = %v, want 2 entries", cfg.Agents[0].InstallAgentHooks)
	}
	if cfg.Agents[0].InstallAgentHooks[0] != "gemini" {
		t.Errorf("Agent.InstallAgentHooks[0] = %q, want gemini", cfg.Agents[0].InstallAgentHooks[0])
	}
}

func TestInstallAgentHooksRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{
			Name:              "test",
			InstallAgentHooks: []string{"claude", "copilot"},
		},
		Agents: []Agent{{
			Name:              "mayor",
			InstallAgentHooks: []string{"gemini"},
		}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cfg2, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse roundtrip: %v", err)
	}
	if len(cfg2.Workspace.InstallAgentHooks) != 2 {
		t.Errorf("roundtrip workspace hooks = %v", cfg2.Workspace.InstallAgentHooks)
	}
	if len(cfg2.Agents[0].InstallAgentHooks) != 1 || cfg2.Agents[0].InstallAgentHooks[0] != "gemini" {
		t.Errorf("roundtrip agent hooks = %v", cfg2.Agents[0].InstallAgentHooks)
	}
}

func TestInstallAgentHooksOmittedWhenEmpty(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents:    []Agent{{Name: "mayor"}},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "install_agent_hooks") {
		t.Errorf("TOML output should omit install_agent_hooks when empty, got:\n%s", data)
	}
}

// --- WispGC config tests ---

func TestDaemonConfig_WispGCDisabledByDefault(t *testing.T) {
	d := DaemonConfig{}
	if d.WispGCEnabled() {
		t.Error("wisp GC should be disabled by default")
	}
	if d.WispGCIntervalDuration() != 0 {
		t.Errorf("WispGCIntervalDuration = %v, want 0", d.WispGCIntervalDuration())
	}
	if d.WispTTLDuration() != 0 {
		t.Errorf("WispTTLDuration = %v, want 0", d.WispTTLDuration())
	}
}

func TestDaemonConfig_WispGCEnabled(t *testing.T) {
	d := DaemonConfig{
		WispGCInterval: "5m",
		WispTTL:        "24h",
	}
	if !d.WispGCEnabled() {
		t.Error("wisp GC should be enabled when both fields are set")
	}
	if d.WispGCIntervalDuration() != 5*time.Minute {
		t.Errorf("WispGCIntervalDuration = %v, want 5m", d.WispGCIntervalDuration())
	}
	if d.WispTTLDuration() != 24*time.Hour {
		t.Errorf("WispTTLDuration = %v, want 24h", d.WispTTLDuration())
	}
}

func TestDaemonConfig_WispGCPartialNotEnabled(t *testing.T) {
	// Only interval set.
	d := DaemonConfig{WispGCInterval: "5m"}
	if d.WispGCEnabled() {
		t.Error("wisp GC should not be enabled with only interval set")
	}

	// Only TTL set.
	d = DaemonConfig{WispTTL: "24h"}
	if d.WispGCEnabled() {
		t.Error("wisp GC should not be enabled with only TTL set")
	}

	// Invalid duration.
	d = DaemonConfig{WispGCInterval: "bad", WispTTL: "24h"}
	if d.WispGCEnabled() {
		t.Error("wisp GC should not be enabled with invalid interval")
	}
}

// TestEffectiveMethodsQualifyConsistently verifies that EffectiveWorkQuery,
// EffectiveSlingQuery, and EffectivePool().Check all use the qualified name
// (Dir/Name) for rig-scoped pool agents. This prevents the bug where one
// method uses the unqualified name while others use the qualified form.
//
// Fixed agents use env vars ($GC_SESSION_NAME / $GC_SLING_TARGET) instead
// of hardcoded names, so this check only applies to pool agents.
func TestEffectiveMethodsQualifyConsistently(t *testing.T) {
	tests := []struct {
		name  string
		agent Agent
	}{
		{
			name: "rig-scoped pool agent",
			agent: Agent{
				Name:              "polecat",
				Dir:               "hello-world",
				MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(3),
			},
		},
		{
			name: "deep rig path",
			agent: Agent{
				Name:              "worker",
				Dir:               "rigs/deep-project",
				MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(5),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qn := tt.agent.QualifiedName()
			if tt.agent.Dir == "" {
				t.Skip("test only applies to rig-scoped agents")
			}
			maxSess := tt.agent.EffectiveMaxActiveSessions()
			isMulti := maxSess == nil || *maxSess != 1
			if !isMulti {
				t.Skip("fixed agents use env vars, not qualified names")
			}

			// Multi-session agents must contain the qualified name in queries.
			wq := tt.agent.EffectiveWorkQuery()
			if !strings.Contains(wq, qn) {
				t.Errorf("EffectiveWorkQuery() = %q, does not contain qualified name %q", wq, qn)
			}

			sq := tt.agent.EffectiveSlingQuery()
			if !strings.Contains(sq, qn) {
				t.Errorf("EffectiveSlingQuery() = %q, does not contain qualified name %q", sq, qn)
			}

			check := tt.agent.EffectiveScaleCheck()
			if check != "echo 1" {
				if !strings.Contains(check, qn) {
					t.Errorf("EffectiveScaleCheck() = %q, does not contain qualified name %q", check, qn)
				}
			}

			// None should contain the bare name without the dir prefix.
			bareName := tt.agent.Name
			dirPrefix := tt.agent.Dir + "/"

			wqWithoutQN := strings.ReplaceAll(wq, qn, "")
			if strings.Contains(wqWithoutQN, bareName) {
				t.Errorf("EffectiveWorkQuery() contains bare name %q outside qualified name", bareName)
			}

			sqWithoutQN := strings.ReplaceAll(sq, qn, "")
			if strings.Contains(sqWithoutQN, bareName) {
				t.Errorf("EffectiveSlingQuery() contains bare name %q outside qualified name", bareName)
			}

			if check != "echo 1" {
				checkWithoutQN := strings.ReplaceAll(check, qn, "")
				if strings.Contains(checkWithoutQN, bareName) {
					t.Errorf("EffectiveScaleCheck() contains bare name %q outside qualified name", bareName)
				}
			}

			_ = dirPrefix // used conceptually above
		})
	}
}

func runEffectiveWorkQuery(t *testing.T, a Agent, env map[string]string, bdScript string) string {
	t.Helper()

	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	cmd := exec.Command("sh", "-c", a.EffectiveWorkQuery())
	cmd.Env = []string{"PATH=" + tmp + ":" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run work query: %v", err)
	}
	return string(out)
}

func runLifecycleHookCommand(t *testing.T, command string, env map[string]string, bdScript string) string {
	t.Helper()

	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	logPath := filepath.Join(tmp, "bd.log")

	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{
		"PATH=" + tmp + ":" + os.Getenv("PATH"),
		"BD_LOG=" + logPath,
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run lifecycle hook: %v\n%s", err, out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hook log: %v", err)
	}
	return string(data)
}

// TestEffectiveMethodsAgentRouting verifies that all agents use
// gc.routed_to=<qualified-name> metadata routing.
func TestEffectiveMethodsAgentRouting(t *testing.T) {
	a := Agent{Name: "refinery", Dir: "hello-world"}
	wq := a.EffectiveWorkQuery()
	if !strings.Contains(wq, "gc.routed_to=hello-world/refinery") {
		t.Errorf("EffectiveWorkQuery() = %q, want gc.routed_to=hello-world/refinery", wq)
	}
	sq := a.EffectiveSlingQuery()
	if !strings.Contains(sq, "gc.routed_to=hello-world/refinery") {
		t.Errorf("EffectiveSlingQuery() = %q, want gc.routed_to=hello-world/refinery", sq)
	}
}

func TestDefaultSlingFormulaRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Agents: []Agent{
			{Name: "polecat", Dir: "rig", DefaultSlingFormula: strPtr("mol-polecat-work")},
		},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if got.Agents[0].EffectiveDefaultSlingFormula() != "mol-polecat-work" {
		t.Errorf("DefaultSlingFormula = %q, want %q", got.Agents[0].EffectiveDefaultSlingFormula(), "mol-polecat-work")
	}
}

func TestDefaultSlingTargetRoundTrip(t *testing.T) {
	c := City{
		Workspace: Workspace{Name: "test"},
		Rigs: []Rig{
			{Name: "hello-world", Path: "/tmp/hw", DefaultSlingTarget: "hello-world/polecat"},
		},
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal output): %v", err)
	}
	if got.Rigs[0].DefaultSlingTarget != "hello-world/polecat" {
		t.Errorf("DefaultSlingTarget = %q, want %q", got.Rigs[0].DefaultSlingTarget, "hello-world/polecat")
	}
}

// ---------------------------------------------------------------------------
// SessionConfig accessor tests
// ---------------------------------------------------------------------------

func TestSessionSetupTimeoutDefault(t *testing.T) {
	s := SessionConfig{}
	got := s.SetupTimeoutDuration()
	if got != 10*time.Second {
		t.Errorf("SetupTimeoutDuration() = %v, want 10s", got)
	}
}

func TestSessionSetupTimeoutCustom(t *testing.T) {
	s := SessionConfig{SetupTimeout: "30s"}
	got := s.SetupTimeoutDuration()
	if got != 30*time.Second {
		t.Errorf("SetupTimeoutDuration() = %v, want 30s", got)
	}
}

func TestSessionSetupTimeoutInvalid(t *testing.T) {
	s := SessionConfig{SetupTimeout: "not-a-duration"}
	got := s.SetupTimeoutDuration()
	if got != 10*time.Second {
		t.Errorf("SetupTimeoutDuration() = %v, want 10s (default for invalid)", got)
	}
}

func TestSessionNudgeReadyTimeoutDefault(t *testing.T) {
	s := SessionConfig{}
	got := s.NudgeReadyTimeoutDuration()
	if got != 10*time.Second {
		t.Errorf("NudgeReadyTimeoutDuration() = %v, want 10s", got)
	}
}

func TestSessionNudgeReadyTimeoutCustom(t *testing.T) {
	s := SessionConfig{NudgeReadyTimeout: "5s"}
	got := s.NudgeReadyTimeoutDuration()
	if got != 5*time.Second {
		t.Errorf("NudgeReadyTimeoutDuration() = %v, want 5s", got)
	}
}

func TestSessionNudgeReadyTimeoutInvalid(t *testing.T) {
	s := SessionConfig{NudgeReadyTimeout: "bad"}
	got := s.NudgeReadyTimeoutDuration()
	if got != 10*time.Second {
		t.Errorf("NudgeReadyTimeoutDuration() = %v, want 10s (default for invalid)", got)
	}
}

func TestSessionNudgeRetryIntervalDefault(t *testing.T) {
	s := SessionConfig{}
	got := s.NudgeRetryIntervalDuration()
	if got != 500*time.Millisecond {
		t.Errorf("NudgeRetryIntervalDuration() = %v, want 500ms", got)
	}
}

func TestSessionNudgeRetryIntervalCustom(t *testing.T) {
	s := SessionConfig{NudgeRetryInterval: "1s"}
	got := s.NudgeRetryIntervalDuration()
	if got != time.Second {
		t.Errorf("NudgeRetryIntervalDuration() = %v, want 1s", got)
	}
}

func TestSessionNudgeRetryIntervalInvalid(t *testing.T) {
	s := SessionConfig{NudgeRetryInterval: "nope"}
	got := s.NudgeRetryIntervalDuration()
	if got != 500*time.Millisecond {
		t.Errorf("NudgeRetryIntervalDuration() = %v, want 500ms (default for invalid)", got)
	}
}

func TestSessionNudgeLockTimeoutDefault(t *testing.T) {
	s := SessionConfig{}
	got := s.NudgeLockTimeoutDuration()
	if got != 30*time.Second {
		t.Errorf("NudgeLockTimeoutDuration() = %v, want 30s", got)
	}
}

func TestSessionNudgeLockTimeoutCustom(t *testing.T) {
	s := SessionConfig{NudgeLockTimeout: "1m"}
	got := s.NudgeLockTimeoutDuration()
	if got != time.Minute {
		t.Errorf("NudgeLockTimeoutDuration() = %v, want 1m", got)
	}
}

func TestSessionNudgeLockTimeoutInvalid(t *testing.T) {
	s := SessionConfig{NudgeLockTimeout: "xyz"}
	got := s.NudgeLockTimeoutDuration()
	if got != 30*time.Second {
		t.Errorf("NudgeLockTimeoutDuration() = %v, want 30s (default for invalid)", got)
	}
}

func TestSessionStartupTimeoutDefault(t *testing.T) {
	s := SessionConfig{}
	got := s.StartupTimeoutDuration()
	if got != 60*time.Second {
		t.Errorf("StartupTimeoutDuration() = %v, want 60s", got)
	}
}

func TestSessionStartupTimeoutCustom(t *testing.T) {
	s := SessionConfig{StartupTimeout: "2m"}
	got := s.StartupTimeoutDuration()
	if got != 2*time.Minute {
		t.Errorf("StartupTimeoutDuration() = %v, want 2m", got)
	}
}

func TestSessionStartupTimeoutInvalid(t *testing.T) {
	s := SessionConfig{StartupTimeout: "bad"}
	got := s.StartupTimeoutDuration()
	if got != 60*time.Second {
		t.Errorf("StartupTimeoutDuration() = %v, want 60s (default for invalid)", got)
	}
}

func TestSessionDebounceMsDefault(t *testing.T) {
	s := SessionConfig{}
	got := s.DebounceMsOrDefault()
	if got != 500 {
		t.Errorf("DebounceMsOrDefault() = %d, want 500", got)
	}
}

func TestSessionDebounceMsCustom(t *testing.T) {
	v := 200
	s := SessionConfig{DebounceMs: &v}
	got := s.DebounceMsOrDefault()
	if got != 200 {
		t.Errorf("DebounceMsOrDefault() = %d, want 200", got)
	}
}

func TestSessionDisplayMsDefault(t *testing.T) {
	s := SessionConfig{}
	got := s.DisplayMsOrDefault()
	if got != 5000 {
		t.Errorf("DisplayMsOrDefault() = %d, want 5000", got)
	}
}

func TestSessionDisplayMsCustom(t *testing.T) {
	v := 3000
	s := SessionConfig{DisplayMs: &v}
	got := s.DisplayMsOrDefault()
	if got != 3000 {
		t.Errorf("DisplayMsOrDefault() = %d, want 3000", got)
	}
}

func TestSessionSocketDefault(t *testing.T) {
	s := SessionConfig{}
	if s.Socket != "" {
		t.Errorf("Socket = %q, want empty string", s.Socket)
	}
}

func TestSessionSocketParsed(t *testing.T) {
	toml := `
[workspace]
name = "test"

[session]
socket = "bright-lights"

[[agent]]
name = "a"
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Session.Socket != "bright-lights" {
		t.Errorf("Session.Socket = %q, want %q", cfg.Session.Socket, "bright-lights")
	}
}

func TestParseSessionTimeouts(t *testing.T) {
	toml := `
[workspace]
name = "test"

[session]
setup_timeout = "20s"
nudge_ready_timeout = "15s"
nudge_retry_interval = "1s"
nudge_lock_timeout = "45s"
debounce_ms = 300
display_ms = 8000

[[agent]]
name = "a"
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Session.SetupTimeoutDuration(); got != 20*time.Second {
		t.Errorf("SetupTimeoutDuration() = %v, want 20s", got)
	}
	if got := cfg.Session.NudgeReadyTimeoutDuration(); got != 15*time.Second {
		t.Errorf("NudgeReadyTimeoutDuration() = %v, want 15s", got)
	}
	if got := cfg.Session.NudgeRetryIntervalDuration(); got != time.Second {
		t.Errorf("NudgeRetryIntervalDuration() = %v, want 1s", got)
	}
	if got := cfg.Session.NudgeLockTimeoutDuration(); got != 45*time.Second {
		t.Errorf("NudgeLockTimeoutDuration() = %v, want 45s", got)
	}
	if got := cfg.Session.DebounceMsOrDefault(); got != 300 {
		t.Errorf("DebounceMsOrDefault() = %d, want 300", got)
	}
	if got := cfg.Session.DisplayMsOrDefault(); got != 8000 {
		t.Errorf("DisplayMsOrDefault() = %d, want 8000", got)
	}
}

func TestAPIConfigParsing(t *testing.T) {
	toml := `
[workspace]
name = "test"

[api]
port = 8080
bind = "0.0.0.0"

[[agent]]
name = "a"
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.API.Port != 8080 {
		t.Errorf("API.Port = %d, want 8080", cfg.API.Port)
	}
	if cfg.API.Bind != "0.0.0.0" {
		t.Errorf("API.Bind = %q, want %q", cfg.API.Bind, "0.0.0.0")
	}
	if cfg.API.BindOrDefault() != "0.0.0.0" {
		t.Errorf("BindOrDefault() = %q, want %q", cfg.API.BindOrDefault(), "0.0.0.0")
	}
}

func TestAPIConfigDefaults(t *testing.T) {
	toml := `
[workspace]
name = "test"

[[agent]]
name = "a"
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Per-city API is no longer pre-filled — the supervisor serves the API.
	// Port 0 means disabled; callers check cfg.API.Port > 0 before starting.
	if cfg.API.Port != 0 {
		t.Errorf("API.Port = %d, want 0 (supervisor serves API)", cfg.API.Port)
	}
	if cfg.API.BindOrDefault() != "127.0.0.1" {
		t.Errorf("BindOrDefault() = %q, want %q", cfg.API.BindOrDefault(), "127.0.0.1")
	}
}

func TestAgentOnDeathOnBootRoundTrip(t *testing.T) {
	const data = `
[workspace]
name = "test"

[[agent]]
name = "dog"
min_active_sessions = 0
max_active_sessions = 5
on_death = "echo dead"
on_boot = "echo booted"
`
	cfg, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.OnDeath != "echo dead" {
		t.Errorf("OnDeath = %q, want %q", a.OnDeath, "echo dead")
	}
	if a.OnBoot != "echo booted" {
		t.Errorf("OnBoot = %q, want %q", a.OnBoot, "echo booted")
	}
}

func TestEffectiveOnDeathDefault(t *testing.T) {
	a := Agent{
		Name:              "dog",
		Dir:               "myrig",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5),
	}
	got := a.EffectiveOnDeath()
	for _, want := range []string{"bd list --assignee=myrig/dog", "--status=in_progress", `--assignee "" --status open`, "--set-metadata gc.routed_to=myrig/dog"} {
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveOnDeath() = %q, want %q", got, want)
		}
	}
}

func TestEffectiveOnDeathCustom(t *testing.T) {
	a := Agent{
		Name:              "dog",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5), OnDeath: "custom-death-cmd",
	}
	cmd := a.EffectiveOnDeath()
	if cmd != "custom-death-cmd" {
		t.Errorf("EffectiveOnDeath() = %q, want %q", cmd, "custom-death-cmd")
	}
}

func TestEffectiveOnDeathFixedAgent(t *testing.T) {
	a := Agent{Name: "mayor"}
	got := a.EffectiveOnDeath()
	for _, want := range []string{"bd list --assignee=mayor", "--status=in_progress", `--assignee "" --status open`, "--set-metadata gc.routed_to=mayor"} {
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveOnDeath() = %q, want %q", got, want)
		}
	}
}

func TestEffectiveOnDeathBackfillsMissingRouteOnReopen(t *testing.T) {
	a := Agent{
		Name:              "dog-1",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5),
		PoolName: "hello-world/dog",
	}

	log := runLifecycleHookCommand(t, a.EffectiveOnDeath(), nil, `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '[{"id":"ga-missing","metadata":{}}]'
    ;;
  update)
    printf '%s\n' "$*" >> "$BD_LOG"
    ;;
  *)
    exit 1
    ;;
esac
`)
	if !strings.Contains(log, "--status open") {
		t.Fatalf("hook log = %q, want reopened status", log)
	}
	if !strings.Contains(log, "--set-metadata gc.routed_to=hello-world/dog") {
		t.Fatalf("hook log = %q, want fallback route for ownerless reopened work", log)
	}
}

func TestEffectiveOnDeathPreservesExistingRouteOnReopen(t *testing.T) {
	a := Agent{
		Name:              "dog-1",
		Dir:               "hello-world",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5),
		PoolName: "hello-world/dog",
	}

	log := runLifecycleHookCommand(t, a.EffectiveOnDeath(), nil, `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '[{"id":"ga-routed","metadata":{"gc.routed_to":"already/routed"}}]'
    ;;
  update)
    printf '%s\n' "$*" >> "$BD_LOG"
    ;;
  *)
    exit 1
    ;;
esac
`)
	if !strings.Contains(log, "--status open") {
		t.Fatalf("hook log = %q, want reopened status", log)
	}
	if strings.Contains(log, "--set-metadata") {
		t.Fatalf("hook log = %q, want existing route preserved without overwrite", log)
	}
}

func TestEffectiveOnBootDefault(t *testing.T) {
	a := Agent{
		Name:              "dog",
		Dir:               "myrig",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5),
	}
	got := a.EffectiveOnBoot()
	for _, want := range []string{"bd list --metadata-field gc.routed_to=myrig/dog", "--status=in_progress", "--no-assignee", "--status open"} {
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveOnBoot() = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, `--assignee ""`) {
		t.Errorf("EffectiveOnBoot() = %q, want to target only ownerless work instead of bulk-unassigning routed work", got)
	}
}

func TestEffectiveOnBootDefaultPoolName(t *testing.T) {
	// Pool instance uses PoolName for gc.routed_to (template name, not instance name).
	a := Agent{
		Name:              "dog-3",
		Dir:               "myrig",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5),
		PoolName: "myrig/dog",
	}
	got := a.EffectiveOnBoot()
	for _, want := range []string{"bd list --metadata-field gc.routed_to=myrig/dog", "--status=in_progress", "--no-assignee", "--status open"} {
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveOnBoot() = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, `--assignee ""`) {
		t.Errorf("EffectiveOnBoot() = %q, want to target only ownerless work instead of bulk-unassigning routed work", got)
	}
}

func TestEffectiveOnBootCustom(t *testing.T) {
	a := Agent{
		Name:              "dog",
		MinActiveSessions: ptrInt(0), MaxActiveSessions: ptrInt(5), OnBoot: "custom-boot-cmd",
	}
	cmd := a.EffectiveOnBoot()
	if cmd != "custom-boot-cmd" {
		t.Errorf("EffectiveOnBoot() = %q, want %q", cmd, "custom-boot-cmd")
	}
}

func TestEffectiveOnBootNonPool(t *testing.T) {
	a := Agent{Name: "mayor"}
	got := a.EffectiveOnBoot()
	for _, want := range []string{"bd list --metadata-field gc.routed_to=mayor", "--status=in_progress", "--no-assignee", "--status open"} {
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveOnBoot() = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, `--assignee ""`) {
		t.Errorf("EffectiveOnBoot() = %q, want to target only ownerless work instead of bulk-unassigning routed work", got)
	}
}

func TestValidateDependsOn(t *testing.T) {
	tests := []struct {
		name    string
		agents  []Agent
		wantErr string // substring, or "" for no error
	}{
		{
			name: "valid deps",
			agents: []Agent{
				{Name: "mayor"},
				{Name: "worker", DependsOn: []string{"mayor"}},
			},
		},
		{
			name: "qualified deps",
			agents: []Agent{
				{Name: "db", Dir: "infra"},
				{Name: "worker", Dir: "infra", DependsOn: []string{"infra/db"}},
			},
		},
		{
			name: "unknown dep",
			agents: []Agent{
				{Name: "worker", DependsOn: []string{"nobody"}},
			},
			wantErr: "unknown agent",
		},
		{
			name: "self reference",
			agents: []Agent{
				{Name: "worker", DependsOn: []string{"worker"}},
			},
			wantErr: "self-reference",
		},
		{
			name: "cycle",
			agents: []Agent{
				{Name: "a", DependsOn: []string{"b"}},
				{Name: "b", DependsOn: []string{"a"}},
			},
			wantErr: "cycle",
		},
		{
			name:   "empty deps",
			agents: []Agent{{Name: "solo"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDependsOn(tt.agents)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestInjectImplicitAgents_NoProviders(t *testing.T) {
	// Even with no configured model providers, the built-in control-dispatcher
	// lane is always available.
	cfg := &City{Daemon: DaemonConfig{FormulaV2: true}}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1 (control-dispatcher only)", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.Name != ControlDispatcherAgentName {
		t.Fatalf("agent[0].Name = %q, want %q", a.Name, ControlDispatcherAgentName)
	}
	if !a.Implicit {
		t.Fatal("control-dispatcher should be implicit")
	}
}

func TestInjectImplicitAgents_WorkspaceProvider(t *testing.T) {
	// workspace.provider alone is enough — no [providers.claude] section needed.
	cfg := &City{
		Daemon:    DaemonConfig{FormulaV2: true},
		Workspace: Workspace{Provider: "claude"},
	}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.Name != "claude" {
		t.Errorf("Name = %q, want %q", a.Name, "claude")
	}
	if !a.Implicit {
		t.Error("Implicit = false, want true")
	}
	if got := cfg.Agents[1].Name; got != ControlDispatcherAgentName {
		t.Errorf("agent[1].Name = %q, want %q", got, ControlDispatcherAgentName)
	}
}

func TestInjectImplicitAgents_WorkspaceProviderPlusExplicit(t *testing.T) {
	// workspace.provider = "claude" + [providers.codex] → both get implicit agents.
	cfg := &City{
		Daemon:    DaemonConfig{FormulaV2: true},
		Workspace: Workspace{Provider: "claude"},
		Providers: map[string]ProviderSpec{
			"codex": {},
		},
	}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(cfg.Agents))
	}
	// Canonical order: claude before codex.
	if cfg.Agents[0].Name != "claude" {
		t.Errorf("agent[0].Name = %q, want %q", cfg.Agents[0].Name, "claude")
	}
	if cfg.Agents[1].Name != "codex" {
		t.Errorf("agent[1].Name = %q, want %q", cfg.Agents[1].Name, "codex")
	}
	if cfg.Agents[2].Name != ControlDispatcherAgentName {
		t.Errorf("agent[2].Name = %q, want %q", cfg.Agents[2].Name, ControlDispatcherAgentName)
	}
}

func TestInjectImplicitAgents_WorkspaceProviderNoDuplicate(t *testing.T) {
	// workspace.provider = "claude" + [providers.claude] → no duplicate.
	cfg := &City{
		Daemon:    DaemonConfig{FormulaV2: true},
		Workspace: Workspace{Provider: "claude"},
		Providers: map[string]ProviderSpec{
			"claude": {},
		},
	}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
}

func TestInjectImplicitAgents_WorkspaceProviderNonBuiltin(t *testing.T) {
	// A non-builtin workspace.provider without a matching [providers.X]
	// section must NOT create an implicit agent (it would fail at resolution).
	cfg := &City{
		Daemon:    DaemonConfig{FormulaV2: true},
		Workspace: Workspace{Provider: "my-custom-llm"},
	}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1 (control-dispatcher only)", len(cfg.Agents))
	}
}

func TestInjectImplicitAgents_WorkspaceProviderNonBuiltinWithEntry(t *testing.T) {
	// A non-builtin workspace.provider WITH a matching [providers.X]
	// section should still work.
	cfg := &City{
		Daemon:    DaemonConfig{FormulaV2: true},
		Workspace: Workspace{Provider: "my-custom-llm"},
		Providers: map[string]ProviderSpec{
			"my-custom-llm": {Command: "ollama"},
		},
	}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "my-custom-llm" {
		t.Errorf("Name = %q, want %q", cfg.Agents[0].Name, "my-custom-llm")
	}
}

func TestInjectImplicitAgents_ExplicitAgentUnconfiguredProvider(t *testing.T) {
	// An explicit agent referencing a provider NOT in cfg.Providers or
	// workspace.provider is preserved, but no implicit agent is created
	// for that provider.
	cfg := &City{
		Daemon: DaemonConfig{FormulaV2: true},
		Providers: map[string]ProviderSpec{
			"claude": {},
		},
		Agents: []Agent{
			{Name: "my-gemini-worker", Provider: "gemini"},
		},
	}
	InjectImplicitAgents(cfg)

	// 1 explicit (gemini) + 1 implicit (claude) + control-dispatcher = 3
	if len(cfg.Agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(cfg.Agents))
	}

	// Explicit agent preserved.
	if cfg.Agents[0].Name != "my-gemini-worker" {
		t.Errorf("agent[0].Name = %q, want %q", cfg.Agents[0].Name, "my-gemini-worker")
	}
	if cfg.Agents[0].Implicit {
		t.Error("explicit agent should not be marked implicit")
	}

	// No implicit gemini agent.
	for _, a := range cfg.Agents {
		if a.Name == "gemini" && a.Implicit {
			t.Error("should not create implicit agent for unconfigured provider 'gemini'")
		}
	}
}

func TestInjectImplicitAgents_ConfiguredOnly(t *testing.T) {
	// Only providers in cfg.Providers get implicit agents.
	cfg := &City{
		Daemon: DaemonConfig{FormulaV2: true},
		Providers: map[string]ProviderSpec{
			"claude": {},
			"codex":  {},
		},
	}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(cfg.Agents))
	}
	// Canonical order: claude before codex.
	for i, wantName := range []string{"claude", "codex"} {
		a := cfg.Agents[i]
		if a.Name != wantName {
			t.Errorf("agent[%d].Name = %q, want %q", i, a.Name, wantName)
		}
		if a.Provider != wantName {
			t.Errorf("agent[%d].Provider = %q, want %q", i, a.Provider, wantName)
		}
		if !a.Implicit {
			t.Errorf("agent[%d].Implicit = false, want true", i)
		}
		// Implicit agents no longer set MinActiveSessions/MaxActiveSessions;
		// they are nil (unlimited, on-demand).
		if a.MinActiveSessions != nil {
			t.Errorf("agent[%d].MinActiveSessions = %v, want nil", i, a.MinActiveSessions)
		}
		if a.MaxActiveSessions != nil {
			t.Errorf("agent[%d].MaxActiveSessions = %v, want nil", i, a.MaxActiveSessions)
		}
	}
}

func TestInjectImplicitAgents_CustomProvider(t *testing.T) {
	// Multiple builtins + multiple custom providers: builtins come first
	// in canonical order, then customs in alphabetical order.
	cfg := &City{
		Daemon: DaemonConfig{FormulaV2: true},
		Providers: map[string]ProviderSpec{
			"codex":    {},
			"claude":   {},
			"zebra":    {Command: "zebra-llm"},
			"my-local": {Command: "ollama"},
		},
	}
	InjectImplicitAgents(cfg)

	if len(cfg.Agents) != 5 {
		t.Fatalf("got %d agents, want 5", len(cfg.Agents))
	}
	// Builtins in canonical order (claude before codex), then customs alphabetical.
	wantOrder := []string{"claude", "codex", "my-local", "zebra"}
	for i, want := range wantOrder {
		if cfg.Agents[i].Name != want {
			t.Errorf("agent[%d].Name = %q, want %q", i, cfg.Agents[i].Name, want)
		}
	}
	if got := cfg.Agents[len(cfg.Agents)-1].Name; got != ControlDispatcherAgentName {
		t.Errorf("last implicit agent = %q, want %q", got, ControlDispatcherAgentName)
	}
}

func TestInjectImplicitAgents_ExplicitWins(t *testing.T) {
	cfg := &City{
		Daemon: DaemonConfig{FormulaV2: true},
		Providers: map[string]ProviderSpec{
			"claude": {},
			"codex":  {},
		},
		Agents: []Agent{
			{Name: "claude", Provider: "claude", MinActiveSessions: ptrInt(1), MaxActiveSessions: ptrInt(3)},
		},
	}
	InjectImplicitAgents(cfg)

	// 1 explicit claude + 1 implicit codex + control-dispatcher.
	if len(cfg.Agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(cfg.Agents))
	}

	// First agent is the explicit one — not overwritten.
	claude := cfg.Agents[0]
	if claude.Implicit {
		t.Error("explicit claude should not be marked implicit")
	}
	if claude.MaxActiveSessions == nil || *claude.MaxActiveSessions != 3 {
		t.Errorf("explicit claude MaxActiveSessions = %v, want 3", claude.MaxActiveSessions)
	}

	// No duplicate claude.
	count := 0
	for _, a := range cfg.Agents {
		if a.Name == "claude" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("found %d claude agents, want 1", count)
	}
}

func TestInjectImplicitAgents_RigScopedExplicitDoesNotBlockCity(t *testing.T) {
	// An explicit rig-scoped "claude" should NOT prevent the implicit city-scoped one.
	cfg := &City{
		Daemon: DaemonConfig{FormulaV2: true},
		Providers: map[string]ProviderSpec{
			"claude": {},
			"codex":  {},
		},
		Rigs: []Rig{{Name: "my-rig", Path: "/tmp/my-rig"}},
		Agents: []Agent{
			{Name: "claude", Dir: "my-rig", Provider: "claude"},
		},
	}
	InjectImplicitAgents(cfg)

	// 1 explicit rig-scoped claude + 2 implicit city-scoped + 1 implicit rig-scoped codex
	// (the explicit rig-scoped claude blocks the implicit rig-scoped claude).
	want := 1 + 2 + 1 + 2 // + city & rig control-dispatcher
	if len(cfg.Agents) != want {
		t.Fatalf("got %d agents, want %d", len(cfg.Agents), want)
	}

	// Both the explicit rig-scoped and implicit city-scoped claude should exist.
	var rigExplicit, cityImplicit, rigImplicit int
	for _, a := range cfg.Agents {
		if a.Name == "claude" && a.Dir == "my-rig" && !a.Implicit {
			rigExplicit++
		}
		if a.Name == "claude" && a.Dir == "" && a.Implicit {
			cityImplicit++
		}
		if a.Name == "claude" && a.Dir == "my-rig" && a.Implicit {
			rigImplicit++
		}
	}
	if rigExplicit != 1 {
		t.Errorf("explicit rig-scoped claude count = %d, want 1", rigExplicit)
	}
	if cityImplicit != 1 {
		t.Errorf("implicit city-scoped claude count = %d, want 1", cityImplicit)
	}
	if rigImplicit != 0 {
		t.Errorf("implicit rig-scoped claude count = %d, want 0 (blocked by explicit)", rigImplicit)
	}
}

func TestInjectImplicitAgents_RigInjection(t *testing.T) {
	// With rigs defined, implicit agents are injected for each rig too.
	cfg := &City{
		Daemon: DaemonConfig{FormulaV2: true},
		Providers: map[string]ProviderSpec{
			"claude": {},
			"codex":  {},
		},
		Rigs: []Rig{
			{Name: "frontend", Path: "/tmp/frontend"},
			{Name: "backend", Path: "/tmp/backend"},
		},
	}
	InjectImplicitAgents(cfg)

	// 2 city-scoped + 2×2 rig-scoped + 3 control-dispatcher (city + 2 rigs) = 9
	want := 9
	if len(cfg.Agents) != want {
		t.Fatalf("got %d agents, want %d", len(cfg.Agents), want)
	}

	// Verify each rig has all configured providers.
	for _, rigName := range []string{"frontend", "backend"} {
		rigAgents := 0
		for _, a := range cfg.Agents {
			if a.Dir == rigName && a.Implicit {
				rigAgents++
			}
		}
		if rigAgents != 3 {
			t.Errorf("rig %q: got %d implicit agents, want 3 (2 providers + control-dispatcher)", rigName, rigAgents)
		}
	}

	// Verify all rig-scoped provider agents have nil scaling (on-demand).
	for _, a := range cfg.Agents {
		if a.Dir != "" && a.Implicit && a.Name != ControlDispatcherAgentName {
			if a.MinActiveSessions != nil || a.MaxActiveSessions != nil {
				t.Errorf("rig agent %s/%s: unexpected scaling min=%v max=%v, want nil/nil", a.Dir, a.Name, a.MinActiveSessions, a.MaxActiveSessions)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// agent_defaults.default_sling_formula
// ---------------------------------------------------------------------------

func TestAgentDefaultsSlingFormula_ImplicitAgents(t *testing.T) {
	// When agent_defaults.default_sling_formula is set, implicit agents
	// should use it instead of the hardcoded "mol-do-work".
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"claude": {},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-focus-review",
		},
	}
	InjectImplicitAgents(cfg)
	ApplyAgentDefaults(cfg)

	for _, a := range cfg.Agents {
		if a.Implicit && a.Name != ControlDispatcherAgentName && a.EffectiveDefaultSlingFormula() != "mol-focus-review" {
			t.Errorf("implicit agent %q: DefaultSlingFormula = %q, want %q",
				a.Name, a.EffectiveDefaultSlingFormula(), "mol-focus-review")
		}
	}
}

func TestAgentDefaultsSlingFormula_ExplicitAgentInherits(t *testing.T) {
	// Explicit agents without their own default_sling_formula should
	// inherit from agent_defaults.
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"claude": {},
		},
		Agents: []Agent{
			{Name: "worker", Provider: "claude"},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-focus-review",
		},
	}
	InjectImplicitAgents(cfg)
	ApplyAgentDefaults(cfg)

	for _, a := range cfg.Agents {
		if a.Name == "worker" {
			if a.EffectiveDefaultSlingFormula() != "mol-focus-review" {
				t.Errorf("explicit agent %q: DefaultSlingFormula = %q, want %q",
					a.Name, a.EffectiveDefaultSlingFormula(), "mol-focus-review")
			}
			return
		}
	}
	t.Fatal("explicit agent 'worker' not found")
}

func TestAgentDefaultsSlingFormula_InheritedPackDefaultBeatsCityDefault(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{
				Name:                         "worker",
				InheritedDefaultSlingFormula: strPtr("mol-pack-default"),
			},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-city-default",
		},
	}
	ApplyAgentDefaults(cfg)

	if got := cfg.Agents[0].EffectiveDefaultSlingFormula(); got != "mol-pack-default" {
		t.Fatalf("EffectiveDefaultSlingFormula() = %q, want %q", got, "mol-pack-default")
	}
	if cfg.Agents[0].DefaultSlingFormula != nil {
		t.Fatalf("DefaultSlingFormula = %q, want nil when inherited pack default wins", *cfg.Agents[0].DefaultSlingFormula)
	}
}

func TestAgentDefaultsSharedAttachments_InheritAndPreserveExplicitLists(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{
				Name:         "worker",
				Skills:       []string{"agent-skill"},
				MCP:          []string{"agent-mcp"},
				SharedSkills: []string{"pack-skill"},
				SharedMCP:    []string{"pack-mcp"},
			},
		},
		AgentDefaults: AgentDefaults{
			Skills: []string{"city-skill", "pack-skill"},
			MCP:    []string{"city-mcp", "pack-mcp"},
		},
	}
	ApplyAgentDefaults(cfg)

	got := cfg.Agents[0]
	if want := []string{"pack-skill", "city-skill"}; !reflect.DeepEqual(got.SharedSkills, want) {
		t.Fatalf("SharedSkills = %v, want %v", got.SharedSkills, want)
	}
	if want := []string{"pack-mcp", "city-mcp"}; !reflect.DeepEqual(got.SharedMCP, want) {
		t.Fatalf("SharedMCP = %v, want %v", got.SharedMCP, want)
	}
	if want := []string{"agent-skill"}; !reflect.DeepEqual(got.Skills, want) {
		t.Fatalf("Skills = %v, want %v", got.Skills, want)
	}
	if want := []string{"agent-mcp"}; !reflect.DeepEqual(got.MCP, want) {
		t.Fatalf("MCP = %v, want %v", got.MCP, want)
	}
}

func TestAgentDefaultsSlingFormula_ExplicitOverrideWins(t *testing.T) {
	// Explicit agents with their own default_sling_formula should NOT be
	// overridden by agent_defaults.
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"claude": {},
		},
		Agents: []Agent{
			{Name: "worker", Provider: "claude", DefaultSlingFormula: strPtr("mol-custom")},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-focus-review",
		},
	}
	InjectImplicitAgents(cfg)
	ApplyAgentDefaults(cfg)

	for _, a := range cfg.Agents {
		if a.Name == "worker" {
			if a.EffectiveDefaultSlingFormula() != "mol-custom" {
				t.Errorf("explicit agent %q: DefaultSlingFormula = %q, want %q (explicit override)",
					a.Name, a.EffectiveDefaultSlingFormula(), "mol-custom")
			}
			return
		}
	}
	t.Fatal("explicit agent 'worker' not found")
}

func TestAgentDefaultsSlingFormula_FallbackToMolDoWork(t *testing.T) {
	// When agent_defaults.default_sling_formula is empty, implicit agents
	// should still get "mol-do-work" as the fallback.
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"claude": {},
		},
	}
	InjectImplicitAgents(cfg)
	ApplyAgentDefaults(cfg)

	for _, a := range cfg.Agents {
		if a.Implicit && a.Name != ControlDispatcherAgentName && a.EffectiveDefaultSlingFormula() != "mol-do-work" {
			t.Errorf("implicit agent %q: DefaultSlingFormula = %q, want %q (fallback)",
				a.Name, a.EffectiveDefaultSlingFormula(), "mol-do-work")
		}
	}
}

func TestAgentDefaultsSlingFormula_RigScoped(t *testing.T) {
	// Rig-scoped implicit agents should also inherit from agent_defaults.
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"claude": {},
		},
		Rigs: []Rig{
			{Name: "myrig", Path: "/tmp/myrig"},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-focus-review",
		},
	}
	InjectImplicitAgents(cfg)
	ApplyAgentDefaults(cfg)

	for _, a := range cfg.Agents {
		if a.Dir == "myrig" && a.Implicit && a.Name != ControlDispatcherAgentName && a.EffectiveDefaultSlingFormula() != "mol-focus-review" {
			t.Errorf("rig-scoped agent %s/%s: DefaultSlingFormula = %q, want %q",
				a.Dir, a.Name, a.EffectiveDefaultSlingFormula(), "mol-focus-review")
		}
	}
}

func TestAgentDefaultsSlingFormula_NoProviders(t *testing.T) {
	// Explicit agents should receive the default even when no providers
	// are configured (InjectImplicitAgents early-returns in this case).
	cfg := &City{
		Agents: []Agent{
			{Name: "worker"},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-focus-review",
		},
	}
	InjectImplicitAgents(cfg)
	ApplyAgentDefaults(cfg)

	if cfg.Agents[0].EffectiveDefaultSlingFormula() != "mol-focus-review" {
		t.Errorf("explicit agent with no providers: DefaultSlingFormula = %q, want %q",
			cfg.Agents[0].EffectiveDefaultSlingFormula(), "mol-focus-review")
	}
}

func TestAgentDefaultsSlingFormula_ExplicitEmptyClearSurvives(t *testing.T) {
	// An explicit empty-string clear via AgentPatch should survive
	// ApplyAgentDefaults — the city default must not clobber it.
	cfg := &City{
		Agents: []Agent{
			{Name: "worker", DefaultSlingFormula: strPtr("")},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-focus-review",
		},
	}
	ApplyAgentDefaults(cfg)

	if cfg.Agents[0].DefaultSlingFormula == nil {
		t.Fatal("DefaultSlingFormula is nil, want explicit empty string")
	}
	if *cfg.Agents[0].DefaultSlingFormula != "" {
		t.Errorf("DefaultSlingFormula = %q, want %q (explicit clear should survive)",
			*cfg.Agents[0].DefaultSlingFormula, "")
	}
}

func TestAgentDefaultsSlingFormula_InheritedPackDefaultFallback(t *testing.T) {
	a := Agent{
		Name:                         "worker",
		InheritedDefaultSlingFormula: strPtr("mol-pack-default"),
	}

	if got := a.EffectiveDefaultSlingFormula(); got != "mol-pack-default" {
		t.Fatalf("EffectiveDefaultSlingFormula() = %q, want %q", got, "mol-pack-default")
	}
}

func TestAgentDefaultsSlingFormula_ExplicitValueBeatsInheritedPackDefault(t *testing.T) {
	a := Agent{
		Name:                         "worker",
		DefaultSlingFormula:          strPtr(""),
		InheritedDefaultSlingFormula: strPtr("mol-pack-default"),
	}

	if got := a.EffectiveDefaultSlingFormula(); got != "" {
		t.Fatalf("EffectiveDefaultSlingFormula() = %q, want explicit empty-string override", got)
	}
}

func TestAgentDefaultsSlingFormula_ControlDispatcherSkipped(t *testing.T) {
	// Control-dispatcher agents should not receive the city default.
	cfg := &City{
		Agents: []Agent{
			{Name: ControlDispatcherAgentName, Implicit: true},
		},
		AgentDefaults: AgentDefaults{
			DefaultSlingFormula: "mol-focus-review",
		},
	}
	ApplyAgentDefaults(cfg)

	if cfg.Agents[0].DefaultSlingFormula != nil {
		t.Errorf("control-dispatcher got DefaultSlingFormula = %q, want nil",
			*cfg.Agents[0].DefaultSlingFormula)
	}
}

// ---------------------------------------------------------------------------
// max_active_sessions / min_active_sessions / scale_check
// ---------------------------------------------------------------------------

func TestMaxActiveSessionsInheritance(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"
max_active_sessions = 10

[[rigs]]
name = "myrig"
path = "/tmp/myrig"
max_active_sessions = 4

[[agent]]
name = "claude"
dir = "myrig"
max_active_sessions = 2
min_active_sessions = 0

[[agent]]
name = "codex"
dir = "myrig"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Workspace level
	if cfg.Workspace.MaxActiveSessions == nil || *cfg.Workspace.MaxActiveSessions != 10 {
		t.Errorf("workspace max = %v, want 10", cfg.Workspace.MaxActiveSessions)
	}

	// Rig level
	if len(cfg.Rigs) < 1 {
		t.Fatal("no rigs parsed")
	}
	if cfg.Rigs[0].MaxActiveSessions == nil {
		t.Fatal("rig max_active_sessions is nil, want 4")
	}
	if *cfg.Rigs[0].MaxActiveSessions != 4 {
		t.Errorf("rig max = %d, want 4", *cfg.Rigs[0].MaxActiveSessions)
	}

	// Agent with explicit max
	claude := cfg.Agents[0]
	if claude.MaxActiveSessions == nil || *claude.MaxActiveSessions != 2 {
		t.Errorf("claude max = %v, want 2", claude.MaxActiveSessions)
	}
	if claude.MinActiveSessions == nil || *claude.MinActiveSessions != 0 {
		t.Errorf("claude min = %v, want 0 (explicitly set)", claude.MinActiveSessions)
	}

	// Agent without explicit max inherits from rig
	codex := cfg.Agents[1]
	resolved := codex.ResolvedMaxActiveSessions(cfg)
	if resolved == nil || *resolved != 4 {
		t.Errorf("codex resolved max = %v, want 4 (from rig)", resolved)
	}
}

func TestMaxActiveSessionsInheritanceWorkspaceOnly(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"
max_active_sessions = 5

[[agent]]
name = "worker"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	worker := cfg.Agents[0]
	resolved := worker.ResolvedMaxActiveSessions(cfg)
	if resolved == nil || *resolved != 5 {
		t.Errorf("worker resolved max = %v, want 5 (from workspace)", resolved)
	}
}

func TestMaxActiveSessionsUnlimitedWhenUnset(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	worker := cfg.Agents[0]
	resolved := worker.ResolvedMaxActiveSessions(cfg)
	if resolved != nil {
		t.Errorf("worker resolved max = %v, want nil (unlimited)", resolved)
	}
}

func TestScaleCheckTopLevel(t *testing.T) {
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"
scale_check = "echo 3"
max_active_sessions = 5
min_active_sessions = 1
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	worker := cfg.Agents[0]
	if worker.ScaleCheck != "echo 3" {
		t.Errorf("scale_check = %q, want %q", worker.ScaleCheck, "echo 3")
	}
	if worker.MaxActiveSessions == nil || *worker.MaxActiveSessions != 5 {
		t.Errorf("max = %v, want 5", worker.MaxActiveSessions)
	}
	if worker.MinActiveSessions == nil || *worker.MinActiveSessions != 1 {
		t.Errorf("min = %v, want 1", worker.MinActiveSessions)
	}
}

func TestFlatScalingFields(t *testing.T) {
	// Scaling is configured via flat agent fields.
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"
min_active_sessions = 0
max_active_sessions = 5
scale_check = "echo 2"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	worker := cfg.Agents[0]
	resolved := worker.EffectiveMaxActiveSessions()
	if resolved == nil || *resolved != 5 {
		t.Errorf("effective max = %v, want 5", resolved)
	}
	if worker.EffectiveMinActiveSessions() != 0 {
		t.Errorf("effective min = %d, want 0", worker.EffectiveMinActiveSessions())
	}
	if worker.EffectiveScaleCheck() != "echo 2" {
		t.Errorf("effective scale_check = %q, want %q", worker.EffectiveScaleCheck(), "echo 2")
	}
}

func TestFlatScalingFieldsExplicit(t *testing.T) {
	// Explicit flat scaling fields take priority.
	data := []byte(`
[workspace]
name = "test"

[[agent]]
name = "worker"
max_active_sessions = 10
min_active_sessions = 2
scale_check = "echo 5"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	worker := cfg.Agents[0]
	resolved := worker.EffectiveMaxActiveSessions()
	if resolved == nil || *resolved != 10 {
		t.Errorf("effective max = %v, want 10", resolved)
	}
	if worker.EffectiveMinActiveSessions() != 2 {
		t.Errorf("effective min = %d, want 2", worker.EffectiveMinActiveSessions())
	}
	if worker.EffectiveScaleCheck() != "echo 5" {
		t.Errorf("effective scale_check = %q, want %q", worker.EffectiveScaleCheck(), "echo 5")
	}
}

// TestLoadWithIncludes_DeprecatedAttachmentWarning confirms that a config
// containing the v0.15.0 attachment-list tombstone fields still parses,
// and that a single deprecation warning is surfaced through provenance.
func TestLoadWithIncludes_DeprecatedAttachmentWarning(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "deprecated-attachments"

[[agent]]
name = "mayor"
skills = ["code-review", "incident-response"]
mcp = ["beads-health"]
`)

	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Tombstone fields must still parse into the struct — that is the
	// backwards-compat contract for v0.15.1.
	var mayor *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "mayor" {
			mayor = &cfg.Agents[i]
			break
		}
	}
	if mayor == nil {
		t.Fatal("agent 'mayor' not found in loaded config")
	}
	if !reflect.DeepEqual(mayor.Skills, []string{"code-review", "incident-response"}) {
		t.Errorf("mayor.Skills = %v, want tombstone parse-through", mayor.Skills)
	}
	if !reflect.DeepEqual(mayor.MCP, []string{"beads-health"}) {
		t.Errorf("mayor.MCP = %v, want tombstone parse-through", mayor.MCP)
	}

	got := strings.Join(prov.Warnings, "\n")
	if !strings.Contains(got, "deprecated as of v0.15.1 and ignored") {
		t.Fatalf("deprecation warning not surfaced, got:\n%s", got)
	}
	// Exactly one warning line — the warning is one-per-load.
	if n := strings.Count(got, "gc: warning:"); n != 1 {
		t.Errorf("emitted %d warnings, want exactly 1\nwarnings:\n%s", n, got)
	}
}

// TestLoadWithIncludes_DeprecatedAttachmentWarning_RigPatches confirms
// that the deprecation warning fires when tombstone attachment-list
// fields appear under [[rigs.patches]] — the PackV2 successor to
// [[rigs.overrides]]. Prior to this test, the scan only covered
// rig.Overrides and would silently miss the rig.RigPatches surface.
func TestLoadWithIncludes_DeprecatedAttachmentWarning_RigPatches(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "deprecated-in-rig-patches"

[[rigs]]
name = "main"
path = "/tmp/main"

[[rigs.patches]]
name = "polecat"
skills_append = ["incident-response"]
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	got := strings.Join(prov.Warnings, "\n")
	if !strings.Contains(got, "deprecated as of v0.15.1 and ignored") {
		t.Fatalf("rig.patches attachment fields should trigger deprecation warning, got:\n%s", got)
	}
	if n := strings.Count(got, "gc: warning:"); n != 1 {
		t.Errorf("emitted %d warnings, want exactly 1\nwarnings:\n%s", n, got)
	}
}

// TestLoadWithIncludes_NoAttachmentsSilent confirms that a clean config
// (no attachment-list tombstone fields) emits no deprecation warning.
func TestLoadWithIncludes_NoAttachmentsSilent(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "clean-city"

[[agent]]
name = "mayor"
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// This test guards the attachment-list tombstone warning, not the
	// v1-surface warnings. Filter v1-surface warnings (the [[agent]] in
	// the fixture intentionally exercises the legacy surface).
	if other := warningsExcludingV1Surfaces(prov.Warnings); len(other) != 0 {
		t.Errorf("expected no non-v1-surface warning, got:\n%s", strings.Join(other, "\n"))
	}
}

func TestParseOrderOverrideTriggerKey(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test-city"

[orders]

[[orders.overrides]]
name = "digest"
trigger = "cooldown"
interval = "24h"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("len(overrides) = %d, want 1", len(cfg.Orders.Overrides))
	}
	if cfg.Orders.Overrides[0].Trigger == nil || *cfg.Orders.Overrides[0].Trigger != "cooldown" {
		t.Fatalf("Trigger = %#v, want cooldown", cfg.Orders.Overrides[0].Trigger)
	}
}

func TestParseOrderOverrideLegacyGateAlias(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test-city"

[orders]

[[orders.overrides]]
name = "digest"
gate = "cooldown"
interval = "24h"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("len(overrides) = %d, want 1", len(cfg.Orders.Overrides))
	}
	if cfg.Orders.Overrides[0].Trigger == nil || *cfg.Orders.Overrides[0].Trigger != "cooldown" {
		t.Fatalf("Trigger = %#v, want cooldown", cfg.Orders.Overrides[0].Trigger)
	}
}

func TestParseOrderOverrideTriggerWinsOverLegacyGate(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test-city"

[orders]

[[orders.overrides]]
name = "digest"
trigger = "cron"
gate = "cooldown"
schedule = "0 3 * * *"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("len(overrides) = %d, want 1", len(cfg.Orders.Overrides))
	}
	if cfg.Orders.Overrides[0].Trigger == nil || *cfg.Orders.Overrides[0].Trigger != "cron" {
		t.Fatalf("Trigger = %#v, want cron", cfg.Orders.Overrides[0].Trigger)
	}
}

// TestControlDispatcherStartCommandTracesUnderGCRuntime pins the trace-log
// default location for the built-in control-dispatcher worker.
//
// The control-dispatcher writes to ${GC_WORKFLOW_TRACE} every few seconds
// while serving workflow control beads. The default path must live under
// .gc/runtime/ so that the controller's recursive fsnotify watcher
// (cmd/gc/controller.go shouldIgnoreConfigWatchEvent) ignores writes to it
// — that function excludes the .gc and .beads path segments. Placing the
// default at city root caused every append to fire markDirty() through the
// 200ms debouncer, keeping patrol cycles in continuous reconciliation and
// driving cycle duration well past the configured patrol_interval.
//
// Regression guard: do not move the trace default out of .gc/runtime/
// without a paired update to the controller's watcher exclusion list.
func TestControlDispatcherStartCommandTracesUnderGCRuntime(t *testing.T) {
	const (
		wantTraceExport    = `export GC_WORKFLOW_TRACE="${GC_WORKFLOW_TRACE:-${GC_CONTROL_DISPATCHER_TRACE_DEFAULT:-${GC_CITY}/` + citylayout.RuntimeDataRoot + `/control-dispatcher-trace.log}}"`
		wantTraceDirExpr   = `trace_dir="${GC_WORKFLOW_TRACE%/*}"`
		wantRootTraceGuard = `elif [ -z "$trace_dir" ]; then trace_dir="/"; fi`
		wantMkdirSnip      = `mkdir -p "$trace_dir"`
		oldTracePath       = "${GC_CITY}/control-dispatcher-trace.log"
		qualifiedName      = "qcore/control-dispatcher"
	)

	t.Run("city-level constant", func(t *testing.T) {
		got := ControlDispatcherStartCommand
		if !strings.Contains(got, "GC_CONTROL_DISPATCHER_TRACE_DEFAULT") {
			t.Errorf("ControlDispatcherStartCommand must route through GC_CONTROL_DISPATCHER_TRACE_DEFAULT so runtime-root trust decisions happen in Go\n got: %s", got)
		}
		if !strings.Contains(got, wantTraceExport) {
			t.Errorf("ControlDispatcherStartCommand missing %q\n got: %s", wantTraceExport, got)
		}
		if !strings.Contains(got, wantTraceDirExpr) {
			t.Errorf("ControlDispatcherStartCommand missing %q so explicit GC_WORKFLOW_TRACE overrides create their own parent dir\n got: %s", wantTraceDirExpr, got)
		}
		if !strings.Contains(got, wantRootTraceGuard) {
			t.Errorf("ControlDispatcherStartCommand missing %q so absolute root trace overrides normalize to /\n got: %s", wantRootTraceGuard, got)
		}
		if !strings.Contains(got, wantMkdirSnip) {
			t.Errorf("ControlDispatcherStartCommand missing %q (needed so the resolved trace parent exists on first start)\n got: %s", wantMkdirSnip, got)
		}
		if strings.Contains(got, oldTracePath) {
			t.Errorf("ControlDispatcherStartCommand still references the old city-root trace path %q\n got: %s", oldTracePath, got)
		}
	})

	t.Run("qualified-name builder", func(t *testing.T) {
		got := ControlDispatcherStartCommandFor(qualifiedName)
		if !strings.Contains(got, "GC_CONTROL_DISPATCHER_TRACE_DEFAULT") {
			t.Errorf("ControlDispatcherStartCommandFor must route through GC_CONTROL_DISPATCHER_TRACE_DEFAULT so runtime-root trust decisions happen in Go\n got: %s", got)
		}
		if !strings.Contains(got, wantTraceExport) {
			t.Errorf("ControlDispatcherStartCommandFor missing %q\n got: %s", wantTraceExport, got)
		}
		if !strings.Contains(got, wantTraceDirExpr) {
			t.Errorf("ControlDispatcherStartCommandFor missing %q so explicit GC_WORKFLOW_TRACE overrides create their own parent dir\n got: %s", wantTraceDirExpr, got)
		}
		if !strings.Contains(got, wantRootTraceGuard) {
			t.Errorf("ControlDispatcherStartCommandFor missing %q so absolute root trace overrides normalize to /\n got: %s", wantRootTraceGuard, got)
		}
		if !strings.Contains(got, wantMkdirSnip) {
			t.Errorf("ControlDispatcherStartCommandFor missing %q\n got: %s", wantMkdirSnip, got)
		}
		if !strings.Contains(got, "--follow "+qualifiedName) {
			t.Errorf("ControlDispatcherStartCommandFor must --follow the qualified name %q\n got: %s", qualifiedName, got)
		}
	})
}

func TestControlDispatcherStartCommandExecResolvesRuntimeTracePath(t *testing.T) {
	t.Run("default runtime root", func(t *testing.T) {
		cityDir := t.TempDir()
		tracePath, args := runControlDispatcherStartCommand(t, ControlDispatcherStartCommand, cityDir, nil)
		wantTracePath := filepath.Join(cityDir, citylayout.RuntimeDataRoot, "control-dispatcher-trace.log")
		if tracePath != wantTracePath {
			t.Fatalf("trace path = %q, want %q", tracePath, wantTracePath)
		}
		if args != "convoy control --serve --follow "+ControlDispatcherAgentName {
			t.Fatalf("args = %q, want follow command for %q", args, ControlDispatcherAgentName)
		}
		if _, err := os.Stat(wantTracePath); err != nil {
			t.Fatalf("trace file %q not created: %v", wantTracePath, err)
		}
	})

	t.Run("injected trusted default override", func(t *testing.T) {
		cityDir := t.TempDir()
		runtimeDir := filepath.Join(t.TempDir(), "runtime-root")
		tracePath, args := runControlDispatcherStartCommand(t, ControlDispatcherStartCommandFor("qcore/control-dispatcher"), cityDir, map[string]string{
			"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": filepath.Join(runtimeDir, "control-dispatcher-trace.log"),
		})
		wantTracePath := filepath.Join(runtimeDir, "control-dispatcher-trace.log")
		if tracePath != wantTracePath {
			t.Fatalf("trace path = %q, want %q", tracePath, wantTracePath)
		}
		if args != "convoy control --serve --follow qcore/control-dispatcher" {
			t.Fatalf("args = %q, want qualified follow command", args)
		}
		if _, err := os.Stat(wantTracePath); err != nil {
			t.Fatalf("trace file %q not created: %v", wantTracePath, err)
		}
	})

	t.Run("explicit trace override ignores injected default", func(t *testing.T) {
		cityDir := t.TempDir()
		injectedDefault := filepath.Join(t.TempDir(), "runtime-root", "control-dispatcher-trace.log")
		overrideTrace := filepath.Join(t.TempDir(), "override-runtime", "dispatcher.log")
		tracePath, args := runControlDispatcherStartCommand(t, ControlDispatcherStartCommand, cityDir, map[string]string{
			"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": injectedDefault,
			"GC_WORKFLOW_TRACE":                   overrideTrace,
		})
		if tracePath != overrideTrace {
			t.Fatalf("trace path = %q, want explicit override %q", tracePath, overrideTrace)
		}
		if args != "convoy control --serve --follow "+ControlDispatcherAgentName {
			t.Fatalf("args = %q, want follow command for %q", args, ControlDispatcherAgentName)
		}
		if _, err := os.Stat(overrideTrace); err != nil {
			t.Fatalf("override trace file %q not created: %v", overrideTrace, err)
		}
	})
}

func runControlDispatcherStartCommand(t *testing.T, command, cityDir string, extraEnv map[string]string) (tracePath, args string) {
	t.Helper()

	tmp := t.TempDir()
	resultPath := filepath.Join(tmp, "gc-result")
	gcPath := filepath.Join(tmp, "gc")
	gcScript := fmt.Sprintf(`#!/bin/sh
set -eu
trace_parent=${GC_WORKFLOW_TRACE%%/*}
if [ "$trace_parent" = "$GC_WORKFLOW_TRACE" ]; then
  trace_parent=.
elif [ -z "$trace_parent" ]; then
  trace_parent=/
fi
[ -d "$trace_parent" ]
: > "$GC_WORKFLOW_TRACE"
printf 'TRACE=%%s\nARGS=%%s\n' "$GC_WORKFLOW_TRACE" "$*" > %q
`, resultPath)
	if err := os.WriteFile(gcPath, []byte(gcScript), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{
		"PATH=" + tmp + ":" + os.Getenv("PATH"),
		"GC_BIN=" + gcPath,
		"GC_CITY=" + cityDir,
	}
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run control-dispatcher start command: %v\n%s", err, out)
	}

	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read fake gc result: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.HasPrefix(line, "TRACE="):
			tracePath = strings.TrimPrefix(line, "TRACE=")
		case strings.HasPrefix(line, "ARGS="):
			args = strings.TrimPrefix(line, "ARGS=")
		}
	}
	if tracePath == "" {
		t.Fatalf("fake gc result missing trace path:\n%s", data)
	}
	if args == "" {
		t.Fatalf("fake gc result missing args:\n%s", data)
	}
	return tracePath, args
}
