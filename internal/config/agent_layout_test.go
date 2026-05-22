package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestAgentLayoutString covers the debug-only String renderer.
func TestAgentLayoutString(t *testing.T) {
	cases := []struct {
		l    agentLayout
		want string
	}{
		{layoutUnknown, "unknown"},
		{layoutV1Inline, "v1-inline"},
		{layoutV2Convention, "v2-convention"},
	}
	for _, tc := range cases {
		if got := tc.l.String(); got != tc.want {
			t.Errorf("agentLayout(%d).String() = %q, want %q", tc.l, got, tc.want)
		}
	}
}

// TestDiscoverPackAgents_StampsV2Layout asserts that agents discovered
// from a pack's agents/<name>/agent.toml carry layout=layoutV2Convention.
func TestDiscoverPackAgents_StampsV2Layout(t *testing.T) {
	packDir := t.TempDir()
	agentDir := filepath.Join(packDir, "agents", "mayor")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(`
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	agents, err := DiscoverPackAgents(fsys.OSFS{}, packDir, "test-pack", nil)
	if err != nil {
		t.Fatalf("DiscoverPackAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].layout != layoutV2Convention {
		t.Errorf("agent.layout = %v, want layoutV2Convention", agents[0].layout)
	}
}

// TestLoadPack_StampsV1Layout asserts that agents declared as
// [[agent]] blocks in a pack's pack.toml carry layout=layoutV1Inline.
func TestLoadPack_StampsV1Layout(t *testing.T) {
	packDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`
[pack]
name = "test-pack"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	agents, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(packDir, "pack.toml"),
		packDir,
		packDir,
		"test-rig",
		nil,
	)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Name != "mayor" {
		t.Fatalf("agent name = %q, want mayor", agents[0].Name)
	}
	if agents[0].layout != layoutV1Inline {
		t.Errorf("agent.layout = %v, want layoutV1Inline", agents[0].layout)
	}
}

// TestLoadPack_PreservesV2LayoutOnMixedPack asserts that when a pack
// has both v1 [[agent]] blocks and v2 agents/<name>/ entries, each
// agent retains its discovery-time layout stamp.
func TestLoadPack_PreservesV2LayoutOnMixedPack(t *testing.T) {
	packDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`
[pack]
name = "test-pack"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	v2Dir := filepath.Join(packDir, "agents", "polecat")
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2Dir, "agent.toml"), []byte(`
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	agents, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(packDir, "pack.toml"),
		packDir,
		packDir,
		"test-rig",
		nil,
	)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}

	var mayor, polecat *Agent
	for i := range agents {
		switch agents[i].Name {
		case "mayor":
			mayor = &agents[i]
		case "polecat":
			polecat = &agents[i]
		}
	}
	if mayor == nil || polecat == nil {
		t.Fatalf("expected both mayor and polecat, got %d agents", len(agents))
	}
	if mayor.layout != layoutV1Inline {
		t.Errorf("mayor.layout = %v, want layoutV1Inline", mayor.layout)
	}
	if polecat.layout != layoutV2Convention {
		t.Errorf("polecat.layout = %v, want layoutV2Convention", polecat.layout)
	}
}

// TestLoadWithIncludes_CityInlineHasUnknownLayout asserts that agents
// declared via inline [[agent]] in city.toml carry
// layout=layoutUnknown — they are a third category, not v1.
func TestLoadWithIncludes_CityInlineHasUnknownLayout(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var mayor *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "mayor" && cfg.Agents[i].Dir == "" {
			mayor = &cfg.Agents[i]
			break
		}
	}
	if mayor == nil {
		t.Fatalf("mayor not found in cfg.Agents")
	}
	if mayor.layout != layoutUnknown {
		t.Errorf("mayor.layout = %v, want layoutUnknown for city-inline agent", mayor.layout)
	}
}

// TestApplyAgentPatch_PreservesLayout asserts the layout stamp survives
// a patch application — same propagation invariant as ga-tpfc's
// `source` field.
func TestApplyAgentPatch_PreservesLayout(t *testing.T) {
	strVal := func(s string) *string { return &s }
	agent := Agent{Name: "mayor", layout: layoutV2Convention}
	patch := AgentPatch{Name: "mayor", PromptTemplate: strVal("prompts/new.md")}
	applyAgentPatchFields(&agent, &patch)
	if agent.layout != layoutV2Convention {
		t.Errorf("agent.layout after patch = %v, want layoutV2Convention (preserved)", agent.layout)
	}
}

// TestApplyAgentOverride_PreservesLayout asserts the layout stamp
// survives an override application.
func TestApplyAgentOverride_PreservesLayout(t *testing.T) {
	strVal := func(s string) *string { return &s }
	agent := Agent{Name: "polecat", layout: layoutV1Inline}
	override := AgentOverride{Agent: "polecat", PromptTemplate: strVal("prompts/p.md")}
	applyAgentOverride(&agent, &override)
	if agent.layout != layoutV1Inline {
		t.Errorf("agent.layout after override = %v, want layoutV1Inline (preserved)", agent.layout)
	}
}
