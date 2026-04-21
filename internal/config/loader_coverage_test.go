package config

// Loader characterization tests — filling coverage gaps in the V1 composition
// pipeline before the V2 rewrite. These are pure additions that document
// current behavior as assertions. See gastownhall/gascity#360.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// --- Tier A: fragment merge rules (gap coverage) ---

func TestLoadWithIncludes_ConcatNamedSessions(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["extra.toml"]

[workspace]
name = "test"

[[agent]]
name = "mayor"

[[agent]]
name = "polecat"

[[named_session]]
template = "mayor"
mode = "always"
`)
	fs.Files["/city/extra.toml"] = []byte(`
[[named_session]]
template = "polecat"
mode = "on_demand"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.NamedSessions) != 2 {
		t.Fatalf("len(NamedSessions) = %d, want 2", len(cfg.NamedSessions))
	}
	if cfg.NamedSessions[0].Template != "mayor" {
		t.Errorf("NamedSessions[0].Template = %q, want %q", cfg.NamedSessions[0].Template, "mayor")
	}
	if cfg.NamedSessions[1].Template != "polecat" {
		t.Errorf("NamedSessions[1].Template = %q, want %q", cfg.NamedSessions[1].Template, "polecat")
	}
}

func TestLoadWithIncludesLocalOnlyDoesNotRequireHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("home", "")

	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
`)

	if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err != nil {
		t.Fatalf("LoadWithIncludes local-only config: %v", err)
	}
}

func TestLoadWithIncludes_ConcatServices(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["svc.toml"]

[workspace]
name = "test"

[[service]]
name = "api"
kind = "workflow"
`)
	fs.Files["/city/svc.toml"] = []byte(`
[[service]]
name = "health"
kind = "workflow"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("len(Services) = %d, want 2", len(cfg.Services))
	}
	if cfg.Services[0].Name != "api" {
		t.Errorf("Services[0].Name = %q, want %q", cfg.Services[0].Name, "api")
	}
	if cfg.Services[1].Name != "health" {
		t.Errorf("Services[1].Name = %q, want %q", cfg.Services[1].Name, "health")
	}
}

func TestLoadWithIncludes_WorkspaceGlobalFragmentsAppend(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["more.toml"]

[workspace]
name = "test"
global_fragments = ["frag-a"]
`)
	fs.Files["/city/more.toml"] = []byte(`
[workspace]
global_fragments = ["frag-b"]
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Workspace.GlobalFragments) != 2 {
		t.Fatalf("len(GlobalFragments) = %d, want 2", len(cfg.Workspace.GlobalFragments))
	}
	if cfg.Workspace.GlobalFragments[0] != "frag-a" {
		t.Errorf("GlobalFragments[0] = %q, want %q", cfg.Workspace.GlobalFragments[0], "frag-a")
	}
	if cfg.Workspace.GlobalFragments[1] != "frag-b" {
		t.Errorf("GlobalFragments[1] = %q, want %q", cfg.Workspace.GlobalFragments[1], "frag-b")
	}
}

func TestLoadWithIncludes_WorkspaceDefaultRigIncludesAppend(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["more.toml"]

[workspace]
name = "test"
default_rig_includes = ["pack-a"]
`)
	fs.Files["/city/more.toml"] = []byte(`
[workspace]
default_rig_includes = ["pack-b"]
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Workspace.DefaultRigIncludes) != 2 {
		t.Fatalf("len(DefaultRigIncludes) = %d, want 2", len(cfg.Workspace.DefaultRigIncludes))
	}
	if cfg.Workspace.DefaultRigIncludes[0] != "pack-a" {
		t.Errorf("DefaultRigIncludes[0] = %q, want %q", cfg.Workspace.DefaultRigIncludes[0], "pack-a")
	}
	if cfg.Workspace.DefaultRigIncludes[1] != "pack-b" {
		t.Errorf("DefaultRigIncludes[1] = %q, want %q", cfg.Workspace.DefaultRigIncludes[1], "pack-b")
	}
}

// --- Tier A: extraIncludes directory peeling ---

func TestLoadWithIncludes_ExtraIncludesDirectoryBecomesPack(t *testing.T) {
	// When a directory is passed as an extraInclude (CLI -f), it's peeled
	// into packIncludes and appended to Workspace.Includes, not treated
	// as a fragment.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(cityToml, []byte(`
[workspace]
name = "test"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a pack directory.
	packDir := filepath.Join(dir, "test-pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`
[pack]
name = "test-pack"
schema = 1

[[agent]]
name = "worker"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityToml, packDir)
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)

	// Should have both the inline "mayor" and the pack "worker".
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.Name] = true
	}
	if !found["mayor"] {
		t.Error("missing agent 'mayor' from inline config")
	}
	if !found["worker"] {
		t.Error("missing agent 'worker' from pack passed as extraInclude directory")
	}
}

// --- Tier A: provenance through full pipeline ---

func TestLoadWithIncludes_ProvenanceThroughPacks(t *testing.T) {
	// Verify provenance tracking works through pack expansion.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "city", "mypk")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test"
includes = ["mypk"]

[[agent]]
name = "inline-agent"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "pack-agent"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Inline agent provenance should point to city.toml.
	if src, ok := prov.Agents["inline-agent"]; !ok {
		t.Error("provenance missing for inline-agent")
	} else if !strings.HasSuffix(src, "city.toml") {
		t.Errorf("inline-agent source = %q, want suffix city.toml", src)
	}

	// Pack agent provenance should point to pack.toml.
	if src, ok := prov.Agents["pack-agent"]; !ok {
		t.Error("provenance missing for pack-agent")
	} else if !strings.HasSuffix(src, "pack.toml") {
		t.Errorf("pack-agent source = %q, want suffix pack.toml", src)
	}

	// Both agents should be in the composed city.
	explicit := explicitAgents(cfg.Agents)
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.Name] = true
	}
	if !found["inline-agent"] {
		t.Error("missing inline-agent in composed city")
	}
	if !found["pack-agent"] {
		t.Error("missing pack-agent in composed city")
	}
}

// --- Tier A: patches accumulate from fragments ---

func TestLoadWithIncludes_PatchesAccumulate(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["patches.toml"]

[workspace]
name = "test"

[[agent]]
name = "mayor"

[[agent]]
name = "worker"
dir = "hw"

[[patches.agent]]
name = "mayor"
suspended = true
`)
	fs.Files["/city/patches.toml"] = []byte(`
[[patches.agent]]
name = "worker"
dir = "hw"
suspended = true
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// After patch application, both agents should be suspended.
	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if !a.Suspended {
			t.Errorf("agent %q should be suspended after patch", a.QualifiedName())
		}
	}
	// Patches should be cleared after application.
	if !cfg.Patches.IsEmpty() {
		t.Error("Patches should be empty after application")
	}
}

// --- Tier B: end-to-end full pipeline characterization ---

func TestLoadWithIncludes_FullPipeline(t *testing.T) {
	// This test exercises the entire V1 composition pipeline end-to-end:
	// root → fragment → city packs → patches → rig packs → overrides →
	// pack globals → implicit agents → defaults → formula layers.
	//
	// It exists to catch regressions during the V2 rewrite. If this test
	// passes, the V1 loader's composed output has the right shape.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "city", "packs", "mypk")
	formulasDir := filepath.Join(packDir, "formulas")

	for _, d := range []string{cityDir, packDir, formulasDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Root city.toml: one inline agent, one fragment, one pack, one rig.
	// Set workspace.provider so implicit agent injection creates at least one.
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
include = ["fragment.toml"]

[workspace]
name = "full-pipeline-city"
provider = "claude"
includes = ["packs/mypk"]

[[agent]]
name = "mayor"
scope = "city"

[[rigs]]
name = "proj"
path = "/tmp/proj"
includes = ["packs/mypk"]

[[patches.agent]]
name = "mayor"
suspended = true

[agent_defaults]
default_sling_formula = "standard-sling"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fragment: adds a named session.
	if err := os.WriteFile(filepath.Join(cityDir, "fragment.toml"), []byte(`
[[named_session]]
template = "mayor"
mode = "always"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pack: one city-scoped agent, one rig-scoped agent, a formula, a global.
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "pack-city-agent"
scope = "city"

[[agent]]
name = "pack-rig-agent"
scope = "rig"

[global]
session_live = ["echo global-from-mypk"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(formulasDir, "demo.toml"), []byte(`
formula = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// 1. Fragment merged: named session should be present.
	if len(cfg.NamedSessions) != 1 {
		t.Fatalf("len(NamedSessions) = %d, want 1", len(cfg.NamedSessions))
	}
	if cfg.NamedSessions[0].Template != "mayor" {
		t.Errorf("NamedSession template = %q, want %q", cfg.NamedSessions[0].Template, "mayor")
	}

	// 2. City pack expanded: pack-city-agent should be present.
	explicit := explicitAgents(cfg.Agents)
	agentMap := map[string]*Agent{}
	for i := range explicit {
		agentMap[explicit[i].QualifiedName()] = &explicit[i]
	}

	if _, ok := agentMap["pack-city-agent"]; !ok {
		t.Error("missing pack-city-agent from city pack expansion")
	}
	// pack-rig-agent should NOT appear at city level (scope = "rig").
	if _, ok := agentMap["pack-rig-agent"]; ok {
		t.Error("pack-rig-agent should not appear at city level (scope=rig)")
	}

	// 3. Patch applied: mayor should be suspended.
	if mayor, ok := agentMap["mayor"]; ok {
		if !mayor.Suspended {
			t.Error("mayor should be suspended after patch")
		}
	} else {
		t.Error("missing mayor after composition")
	}

	// 4. Rig pack expanded: pack-rig-agent should appear under rig "proj".
	if a, ok := agentMap["proj/pack-rig-agent"]; ok {
		if a.Dir != "proj" {
			t.Errorf("rig agent Dir = %q, want %q", a.Dir, "proj")
		}
	} else {
		t.Error("missing proj/pack-rig-agent from rig pack expansion")
	}

	// 5. Pack globals applied: city-scope agents should have the global.
	if a, ok := agentMap["pack-city-agent"]; ok {
		found := false
		for _, cmd := range a.SessionLive {
			if strings.Contains(cmd, "global-from-mypk") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("pack-city-agent missing pack global; SessionLive = %v", a.SessionLive)
		}
	}

	// 6. Implicit agents: at least one implicit agent should exist (for the
	//    workspace provider, if set, or for built-in providers).
	hasImplicit := false
	for _, a := range cfg.Agents {
		if a.Implicit {
			hasImplicit = true
			break
		}
	}
	if !hasImplicit {
		t.Error("expected at least one implicit agent")
	}

	// 7. Patches cleared.
	if !cfg.Patches.IsEmpty() {
		t.Error("Patches should be cleared after application")
	}

	// 8. Provenance: root file tracked.
	if prov.Root == "" {
		t.Error("Provenance.Root is empty")
	}
	if len(prov.Sources) < 2 {
		t.Errorf("len(Sources) = %d, want >= 2 (root + fragment)", len(prov.Sources))
	}

	// 9. Formula layers: city pack formulas should be present.
	if len(cfg.FormulaLayers.City) == 0 {
		t.Error("expected city formula layers from pack")
	}

	// 10. Include preserved for round-trip.
	if len(cfg.Include) != 1 || cfg.Include[0] != "fragment.toml" {
		t.Errorf("Include = %v, want [fragment.toml]", cfg.Include)
	}
}

// --- Tier B: install_agent_hooks REPLACES (not appends) ---

func TestLoadWithIncludes_InstallAgentHooksReplaces(t *testing.T) {
	// Per codex audit: install_agent_hooks replaces, not appends.
	// Verify this is the actual behavior.
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["override.toml"]

[workspace]
name = "test"
install_agent_hooks = ["hook-a", "hook-b"]
`)
	fs.Files["/city/override.toml"] = []byte(`
[workspace]
install_agent_hooks = ["hook-c"]
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// Should REPLACE, not append. Result should be ["hook-c"] only.
	hooks := cfg.Workspace.InstallAgentHooks
	if len(hooks) != 1 || hooks[0] != "hook-c" {
		t.Errorf("InstallAgentHooks = %v, want [hook-c] (replace, not append)", hooks)
	}
	// Should produce a collision warning.
	found := false
	for _, w := range prov.Warnings {
		if strings.Contains(w, "install_agent_hooks") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected collision warning for install_agent_hooks redefinition")
	}
}

// --- Tier B: workspace.includes append semantics ---

func TestLoadWithIncludes_WorkspaceIncludesAppendSemantic(t *testing.T) {
	// Verify that workspace.includes from a fragment is appended to the root's,
	// not replaced. Uses real filesystem with actual pack dirs so ExpandCityPacks
	// can resolve them.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packA := filepath.Join(cityDir, "pack-a")
	packB := filepath.Join(cityDir, "pack-b")
	for _, d := range []string{cityDir, packA, packB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
include = ["extra.toml"]

[workspace]
name = "test"
includes = ["pack-a"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "extra.toml"), []byte(`
[workspace]
includes = ["pack-b"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Minimal pack.toml for both.
	for name, d := range map[string]string{"pack-a": packA, "pack-b": packB} {
		if err := os.WriteFile(filepath.Join(d, "pack.toml"), []byte(`
[pack]
name = "`+name+`"
schema = 1

[[agent]]
name = "`+name+`-agent"
scope = "city"
`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Both pack agents should be present (proving append, not replace).
	explicit := explicitAgents(cfg.Agents)
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.Name] = true
	}
	if !found["pack-a-agent"] {
		t.Error("missing pack-a-agent (root includes)")
	}
	if !found["pack-b-agent"] {
		t.Error("missing pack-b-agent (fragment includes — should be appended)")
	}
}

// --- Tier B: scope filtering with unscoped agents ---

func TestLoadWithIncludes_UnscopedAgentsAppearEverywhere(t *testing.T) {
	// An agent with no scope set (empty string) should appear in both
	// city expansion and rig expansion.
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(cityDir, "mypk")
	for _, d := range []string{cityDir, packDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test"
includes = ["mypk"]

[[rigs]]
name = "proj"
path = "/tmp/proj"
includes = ["mypk"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "unscoped"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	foundCity := false
	foundRig := false
	for _, a := range explicit {
		if a.Name == "unscoped" && a.Dir == "" {
			foundCity = true
		}
		if a.Name == "unscoped" && a.Dir == "proj" {
			foundRig = true
		}
	}
	if !foundCity {
		t.Error("unscoped agent should appear at city level")
	}
	if !foundRig {
		t.Error("unscoped agent should appear under rig 'proj'")
	}
}

// --- Tier B: rig overrides applied ---

func TestLoadWithIncludes_RigOverridesApplied(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	packDir := filepath.Join(cityDir, "mypk")
	for _, d := range []string{cityDir, packDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"
includes = ["mypk"]

[[rigs.overrides]]
agent = "worker"
suspended = true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "worker"
scope = "rig"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "worker" && a.Dir == "proj" {
			if !a.Suspended {
				t.Error("rig override should have suspended the worker agent")
			}
			return
		}
	}
	t.Error("worker agent not found under rig 'proj'")
}

// --- Tier B: agent_defaults default_sling_formula flows through ---

func TestLoadWithIncludes_AgentDefaultsFlowThrough(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
provider = "claude"

[[agent]]
name = "worker"

[agent_defaults]
default_sling_formula = "my-formula"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "worker" {
			if a.DefaultSlingFormula == nil || *a.DefaultSlingFormula != "my-formula" {
				got := "<nil>"
				if a.DefaultSlingFormula != nil {
					got = *a.DefaultSlingFormula
				}
				t.Errorf("worker DefaultSlingFormula = %s, want %q", got, "my-formula")
			}
			return
		}
	}
	t.Error("worker agent not found")
}
