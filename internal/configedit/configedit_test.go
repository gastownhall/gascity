package configedit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

// minimalCity returns a minimal valid city.toml with one agent.
func minimalCity() string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"
`
}

// cityWithRig returns a city.toml with one agent and one rig.
func cityWithRig() string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
`
}

func writeTOML(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTOML(t *testing.T, path string) *config.City {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	return cfg
}

func readEffectiveTOML(t *testing.T, path string) *config.City {
	t.Helper()
	cfg := readTOML(t, path)
	if _, err := config.ApplySiteBindings(fsys.OSFS{}, filepath.Dir(path), cfg); err != nil {
		t.Fatalf("ApplySiteBindings: %v", err)
	}
	return cfg
}

// readExpandedTOML loads the city config with full pack expansion via
// LoadWithIncludes. Use this when a test needs to observe the merged
// state of pack-discovered or convention-discovered agents (e.g. that
// suspended state set in agents/<name>/agent.toml propagates back into
// the expanded config). Tests that only need the raw city.toml should
// use readTOML; tests verifying site-binding rig paths should use
// readEffectiveTOML.
func readExpandedTOML(t *testing.T, path string) *config.City {
	t.Helper()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reloading expanded config: %v", err)
	}
	return cfg
}

func readSiteBinding(t *testing.T, dir string) *config.SiteBinding {
	t.Helper()
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	return binding
}

func TestEdit_SetsAgentSuspended(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		return configedit.SetAgentSuspended(cfg, "mayor", true)
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	cfg := readTOML(t, path)
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			if !a.Suspended {
				t.Error("expected mayor to be suspended")
			}
			return
		}
	}
	t.Error("mayor not found after edit")
}

func TestEdit_ValidationFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		// Add an agent with an invalid name to trigger validation failure.
		cfg.Agents = append(cfg.Agents, config.Agent{Name: ""})
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

func TestEdit_ValidatesRigsAgainstEffectiveHQPrefix(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
provider = "claude"

[[agent]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "big-lane"
path = "/tmp/my-rig"
`)
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, dir, "bright-lights", ""); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(_ *config.City) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), `rig "big-lane": prefix "bl" collides with HQ`) {
		t.Fatalf("Edit error = %v, want HQ prefix collision", err)
	}
}

func TestEditExpanded_ValidatesRigsAgainstEffectiveHQPrefix(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
provider = "claude"

[[agent]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "big-lane"
path = "/tmp/my-rig"
`)
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, dir, "bright-lights", ""); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.EditExpanded(func(_, _ *config.City) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), `rig "big-lane": prefix "bl" collides with HQ`) {
		t.Fatalf("EditExpanded error = %v, want HQ prefix collision", err)
	}
}

func TestSetAgentSuspended_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := configedit.SetAgentSuspended(cfg, "nonexistent", true); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestSetRigSuspended(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		return configedit.SetRigSuspended(cfg, "my-rig", true)
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	cfg := readTOML(t, path)
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			if !r.Suspended {
				t.Error("expected my-rig to be suspended")
			}
			return
		}
	}
	t.Error("my-rig not found after edit")
}

func TestSetRigSuspended_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := configedit.SetRigSuspended(cfg, "nonexistent", true); err == nil {
		t.Error("expected error for nonexistent rig")
	}
}

func TestAgentOrigin_Inline(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	origin := configedit.AgentOrigin(cfg, cfg, "mayor")
	if origin != configedit.OriginInline {
		t.Errorf("got %v, want OriginInline", origin)
	}
}

func TestAgentOrigin_Derived(t *testing.T) {
	raw := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	expanded := &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "polecat", Dir: "my-rig"},
		},
	}
	origin := configedit.AgentOrigin(raw, expanded, "my-rig/polecat")
	if origin != configedit.OriginDerived {
		t.Errorf("got %v, want OriginDerived", origin)
	}
}

func TestAgentOrigin_NotFound(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	origin := configedit.AgentOrigin(cfg, cfg, "nonexistent")
	if origin != configedit.OriginNotFound {
		t.Errorf("got %v, want OriginNotFound", origin)
	}
}

func TestRigOrigin(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "my-rig"}},
	}
	if configedit.RigOrigin(cfg, "my-rig") != configedit.OriginInline {
		t.Error("expected OriginInline for existing rig")
	}
	if configedit.RigOrigin(cfg, "nope") != configedit.OriginNotFound {
		t.Error("expected OriginNotFound for missing rig")
	}
}

func TestAddOrUpdateAgentPatch_New(t *testing.T) {
	cfg := &config.City{}
	err := configedit.AddOrUpdateAgentPatch(cfg, "my-rig/polecat", func(p *config.AgentPatch) {
		suspended := true
		p.Suspended = &suspended
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(cfg.Patches.Agents))
	}
	p := cfg.Patches.Agents[0]
	if p.Dir != "my-rig" || p.Name != "polecat" {
		t.Errorf("patch target = %s/%s, want my-rig/polecat", p.Dir, p.Name)
	}
	if p.Suspended == nil || !*p.Suspended {
		t.Error("expected suspended=true in patch")
	}
}

func TestAddOrUpdateAgentPatch_Existing(t *testing.T) {
	suspended := false
	cfg := &config.City{
		Patches: config.Patches{
			Agents: []config.AgentPatch{
				{Dir: "my-rig", Name: "polecat", Suspended: &suspended},
			},
		},
	}
	err := configedit.AddOrUpdateAgentPatch(cfg, "my-rig/polecat", func(p *config.AgentPatch) {
		s := true
		p.Suspended = &s
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("expected 1 patch (updated), got %d", len(cfg.Patches.Agents))
	}
	if cfg.Patches.Agents[0].Suspended == nil || !*cfg.Patches.Agents[0].Suspended {
		t.Error("expected suspended=true after update")
	}
}

func TestAddOrUpdateRigPatch(t *testing.T) {
	cfg := &config.City{}
	err := configedit.AddOrUpdateRigPatch(cfg, "my-rig", func(p *config.RigPatch) {
		s := true
		p.Suspended = &s
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Rigs) != 1 {
		t.Fatalf("expected 1 rig patch, got %d", len(cfg.Patches.Rigs))
	}
	if cfg.Patches.Rigs[0].Name != "my-rig" {
		t.Errorf("patch target = %s, want my-rig", cfg.Patches.Rigs[0].Name)
	}
}

func TestEdit_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Successful edit should leave no temp files.
	err := ed.Edit(func(cfg *config.City) error {
		cfg.Agents[0].Suspended = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "city.toml" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSuspendAgent_Inline(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendAgent("mayor"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if !cfg.Agents[0].Suspended {
		t.Error("expected mayor to be suspended")
	}
}

func TestResumeAgent_Inline(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeAgent("mayor"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Suspended {
		t.Error("expected mayor to not be suspended")
	}
}

func TestSuspendAgent_LocalDiscovered(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("city.toml should not gain agent patch:\n%s", raw)
	}
	agentToml := string(mustReadFile(t, filepath.Join(agentDir, "agent.toml")))
	if !strings.Contains(agentToml, "suspended = true") {
		t.Fatalf("agent.toml = %q, want suspended = true", agentToml)
	}

	cfg := readExpandedTOML(t, path)
	if !findAgent(t, cfg, "worker").Suspended {
		t.Fatal("worker should be suspended in expanded config")
	}
}

func TestResumeAgent_LocalDiscovered(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("provider = \"codex\"\nsuspended = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("city.toml should not gain agent patch:\n%s", raw)
	}
	agentToml := string(mustReadFile(t, filepath.Join(agentDir, "agent.toml")))
	if !strings.Contains(agentToml, "provider = \"codex\"") {
		t.Fatalf("agent.toml = %q, want provider preserved", agentToml)
	}
	if strings.Contains(agentToml, "suspended") {
		t.Fatalf("agent.toml = %q, want suspended cleared", agentToml)
	}

	cfg := readExpandedTOML(t, path)
	worker := findAgent(t, cfg, "worker")
	if worker.Suspended {
		t.Fatal("worker should not be suspended in expanded config")
	}
	if worker.Provider != "codex" {
		t.Fatalf("worker.Provider = %q, want codex", worker.Provider)
	}
}

// TestSuspendAgent_PackDeclaredAgentUsesPatch ensures that an [[agent]]
// explicitly declared in the city's pack.toml is suspended via
// [[patches.agent]] in city.toml — not via agents/<name>/agent.toml,
// which would be silently shadowed by the pack.toml declaration during
// composition. Regression for the SourceDir == cityRoot heuristic that
// also matched pack-declared agents.
func TestSuspendAgent_PackDeclaredAgentUsesPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2

[[agent]]
name = "worker"
provider = "claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A conventional prompt template at the discovery location must NOT
	// trigger the agent.toml write path when an [[agent]] entry exists.
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	if _, err := os.Stat(filepath.Join(agentDir, "agent.toml")); err == nil {
		t.Fatalf("agent.toml must not be created for pack-declared agent")
	}

	raw := string(mustReadFile(t, path))
	if !strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("city.toml should gain agent patch:\n%s", raw)
	}

	cfg := readExpandedTOML(t, path)
	if !findAgent(t, cfg, "worker").Suspended {
		t.Fatal("worker should be suspended in expanded config")
	}
}

// TestResumeAgent_StripsLegacyPatchSuspended covers the migration case
// where a city.toml has a stale [[patches.agent]] suspended override
// from older code. Resuming a convention-discovered agent must strip
// that patch override so it doesn't continue to shadow agent.toml.
func TestResumeAgent_StripsLegacyPatchSuspended(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[patches.agent]]
dir = ""
name = "worker"
suspended = true
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("suspended = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if strings.Contains(raw, "[[patches.agent]]") {
		t.Fatalf("legacy patch should be stripped:\n%s", raw)
	}

	cfg := readExpandedTOML(t, path)
	if findAgent(t, cfg, "worker").Suspended {
		t.Fatal("worker should not be suspended in expanded config after resume")
	}
}

// TestSuspendAgent_StripsLegacyPatchSuspendedKeepsOtherFields ensures
// that an existing patch with overrides beyond Suspended keeps the
// non-Suspended fields intact when the Suspended override is stripped.
func TestSuspendAgent_StripsLegacyPatchSuspendedKeepsOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[patches.agent]]
dir = ""
name = "worker"
suspended = false
provider = "codex"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	raw := string(mustReadFile(t, path))
	if !strings.Contains(raw, `provider = "codex"`) {
		t.Fatalf("non-Suspended patch fields should be preserved:\n%s", raw)
	}
	if strings.Contains(raw, "suspended =") {
		t.Fatalf("Suspended override should be removed from patch:\n%s", raw)
	}
}

// TestStripAgentPatchSuspended_OnlyMatchingIdentity unit-tests the
// patch-stripping helper directly. Iteration-2 fix: callers must thread
// the resolved (Dir, Name) qualified identity so a same-bare-name patch
// targeting a different rig is never accidentally cleared.
func TestStripAgentPatchSuspended_OnlyMatchingIdentity(t *testing.T) {
	cfg := &config.City{
		Patches: config.Patches{
			Agents: []config.AgentPatch{
				{Dir: "rigA", Name: "worker", Suspended: boolPtrTest(true)},
				{Dir: "rigB", Name: "worker", Suspended: boolPtrTest(true)},
				{Dir: "", Name: "worker", Suspended: boolPtrTest(true)},
			},
		},
	}
	// Strip city-scoped (dir="") only.
	if !configedit.StripAgentPatchSuspended(cfg, "worker") {
		t.Fatal("StripAgentPatchSuspended should report a change")
	}
	if got := len(cfg.Patches.Agents); got != 2 {
		t.Fatalf("Patches.Agents len = %d, want 2; got %#v", got, cfg.Patches.Agents)
	}
	for _, p := range cfg.Patches.Agents {
		if p.Dir == "" {
			t.Errorf("city-scoped patch should be removed; remaining: %#v", p)
		}
	}

	// Strip rigA-scoped via qualified identity.
	if !configedit.StripAgentPatchSuspended(cfg, "rigA/worker") {
		t.Fatal("StripAgentPatchSuspended should report a change for rigA")
	}
	if got := len(cfg.Patches.Agents); got != 1 || cfg.Patches.Agents[0].Dir != "rigB" {
		t.Fatalf("after stripping rigA, expected only rigB patch, got %#v", cfg.Patches.Agents)
	}

	// Stripping a non-matching identity is a no-op.
	if configedit.StripAgentPatchSuspended(cfg, "rigC/worker") {
		t.Fatal("StripAgentPatchSuspended should be a no-op for non-matching identity")
	}
}

func boolPtrTest(b bool) *bool { return &b }

// TestLocalDiscoveredAgent_RejectsRigScopedAgentWithCityPromptPath
// guards against the iteration-3 Major finding (Gemini): a rig-scoped
// agent whose prompt_template happens to point at the city's
// <cityRoot>/agents/<name>/ template must NOT be classified as local
// discovered. Writing agent.toml for it would corrupt the city agent's
// durable state instead of producing the correct [[patches.agent]].
func TestLocalDiscoveredAgent_RejectsRigScopedAgentWithCityPromptPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigAgent := config.Agent{
		Dir:            "myrig",
		Name:           "worker",
		PromptTemplate: filepath.Join(dir, "agents", "worker", "prompt.template.md"),
	}
	if configedit.LocalDiscoveredAgent(fsys.OSFS{}, dir, rigAgent) {
		t.Fatal("rig-scoped agent must not be classified as local-discovered even when prompt_template points at the city's agents/<name>/ tree")
	}

	cityAgent := config.Agent{
		Dir:            "",
		Name:           "worker",
		PromptTemplate: filepath.Join(dir, "agents", "worker", "prompt.template.md"),
	}
	if !configedit.LocalDiscoveredAgent(fsys.OSFS{}, dir, cityAgent) {
		t.Fatal("city-scoped scaffolded agent should be classified as local-discovered")
	}
}

func TestSuspendAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendAgent("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestSuspendRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}

	cfg := readTOML(t, path)
	if !cfg.Rigs[0].Suspended {
		t.Error("expected my-rig to be suspended")
	}
}

func TestResumeRig(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeRig("my-rig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Rigs[0].Suspended {
		t.Error("expected my-rig to not be suspended")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

func findAgent(t *testing.T, cfg *config.City, name string) config.Agent { //nolint:unparam // helper kept generic for future tests
	t.Helper()
	for _, a := range cfg.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %q not found in %#v", name, cfg.Agents)
	return config.Agent{}
}

func TestSuspendCity(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendCity(); err != nil {
		t.Fatalf("SuspendCity: %v", err)
	}

	cfg := readTOML(t, path)
	if !cfg.Workspace.Suspended {
		t.Error("expected workspace to be suspended")
	}
}

func TestResumeCity(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeCity(); err != nil {
		t.Fatalf("ResumeCity: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Workspace.Suspended {
		t.Error("expected workspace to not be suspended")
	}
}

func TestCreateAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "coder" {
			found = true
		}
	}
	if !found {
		t.Error("agent 'coder' not found after create")
	}
}

func TestCreateAgent_Duplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "mayor", Provider: "claude"})
	if err == nil {
		t.Error("expected error for duplicate agent")
	}
}

func TestUpdateAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateAgent("mayor", configedit.AgentUpdate{Provider: "gemini"})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Provider != "gemini" {
		t.Errorf("provider = %q, want %q", cfg.Agents[0].Provider, "gemini")
	}
}

func TestUpdateAgent_PreservesSuspended(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// PATCH provider only — suspended must NOT be reset.
	err := ed.UpdateAgent("mayor", configedit.AgentUpdate{Provider: "gemini"})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Provider != "gemini" {
		t.Errorf("provider = %q, want %q", cfg.Agents[0].Provider, "gemini")
	}
	if !cfg.Agents[0].Suspended {
		t.Error("suspended was reset to false — zero-value bug")
	}
}

func TestUpdateAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateAgent("nonexistent", configedit.AgentUpdate{Provider: "claude"})
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestDeleteAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgent("mayor"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	cfg := readTOML(t, path)
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			t.Error("agent 'mayor' still exists after delete")
		}
	}
}

func TestDeleteAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgent("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestCreateRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateRig(config.Rig{Name: "new-rig", Path: "/tmp/new-rig"})
	if err != nil {
		t.Fatalf("CreateRig: %v", err)
	}

	cfg := readTOML(t, path)
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == "new-rig" {
			found = true
		}
	}
	if !found {
		t.Error("rig 'new-rig' not found after create")
	}
}

func TestCreateRig_Duplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateRig(config.Rig{Name: "my-rig", Path: "/tmp/x"})
	if err == nil {
		t.Error("expected error for duplicate rig")
	}
}

func TestUpdateRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateRig("my-rig", configedit.RigUpdate{Path: "/tmp/updated"})
	if err != nil {
		t.Fatalf("UpdateRig: %v", err)
	}

	raw := readTOML(t, path)
	if raw.Rigs[0].Path != "" {
		t.Errorf("raw path = %q, want empty city.toml binding", raw.Rigs[0].Path)
	}
	cfg := readEffectiveTOML(t, path)
	if cfg.Rigs[0].Path != "/tmp/updated" {
		t.Errorf("effective path = %q, want %q", cfg.Rigs[0].Path, "/tmp/updated")
	}
	binding := readSiteBinding(t, dir)
	if len(binding.Rigs) != 1 || binding.Rigs[0].Path != "/tmp/updated" {
		t.Errorf("site binding = %+v, want updated path", binding.Rigs)
	}
}

func TestUpdateRig_PreservesSuspended(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// PATCH path only — suspended must NOT be reset.
	err := ed.UpdateRig("my-rig", configedit.RigUpdate{Path: "/tmp/updated"})
	if err != nil {
		t.Fatalf("UpdateRig: %v", err)
	}

	raw := readTOML(t, path)
	if raw.Rigs[0].Path != "" {
		t.Errorf("raw path = %q, want empty city.toml binding", raw.Rigs[0].Path)
	}
	cfg := readEffectiveTOML(t, path)
	if cfg.Rigs[0].Path != "/tmp/updated" {
		t.Errorf("effective path = %q, want %q", cfg.Rigs[0].Path, "/tmp/updated")
	}
	if !cfg.Rigs[0].Suspended {
		t.Error("suspended was reset to false — zero-value bug")
	}
}

func TestDeleteRig(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[[agent]]
name = "polecat"
dir = "my-rig"
provider = "claude"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteRig("my-rig"); err != nil {
		t.Fatalf("DeleteRig: %v", err)
	}

	cfg := readTOML(t, path)
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			t.Error("rig 'my-rig' still exists after delete")
		}
	}
	// Rig-scoped agents should also be removed.
	for _, a := range cfg.Agents {
		if a.Dir == "my-rig" {
			t.Errorf("rig-scoped agent %q still exists after rig delete", a.QualifiedName())
		}
	}
	// City-scoped agent should remain.
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			found = true
		}
	}
	if !found {
		t.Error("city-scoped agent 'mayor' was incorrectly removed")
	}
}

func TestDeleteRig_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteRig("nonexistent"); err == nil {
		t.Error("expected error for nonexistent rig")
	}
}

// cityWithProvider returns a city.toml with a custom provider.
func cityWithProvider() string {
	return `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[providers.custom]
display_name = "Custom Agent"
command = "custom-cli"
`
}

func TestCreateProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	spec := config.ProviderSpec{
		DisplayName: "My Provider",
		Command:     "my-provider-cli",
		Args:        []string{"--flag"},
	}
	if err := ed.CreateProvider("myprov", spec); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got, ok := cfg.Providers["myprov"]
	if !ok {
		t.Fatal("provider 'myprov' not found after create")
	}
	if got.Command != "my-provider-cli" {
		t.Errorf("command = %q, want %q", got.Command, "my-provider-cli")
	}
	if got.DisplayName != "My Provider" {
		t.Errorf("display_name = %q, want %q", got.DisplayName, "My Provider")
	}
}

// TestCreateProvider_BaseOnlyNoCommand verifies the relaxed validation:
// a provider with only `base` set is valid — the chain walk inherits
// the command from the ancestor.
func TestCreateProvider_BaseOnlyNoCommand(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	base := "builtin:codex"
	spec := config.ProviderSpec{Base: &base}
	if err := ed.CreateProvider("codex-max", spec); err != nil {
		t.Fatalf("CreateProvider with base and no command: %v", err)
	}

	cfg := readTOML(t, path)
	got, ok := cfg.Providers["codex-max"]
	if !ok {
		t.Fatal("provider 'codex-max' not found after create")
	}
	if got.Base == nil {
		t.Fatal("Base pointer is nil after round-trip")
	}
	if *got.Base != "builtin:codex" {
		t.Errorf("*Base = %q, want builtin:codex", *got.Base)
	}
	if got.Command != "" {
		t.Errorf("Command = %q, want empty (inherited)", got.Command)
	}
}

// TestCreateProvider_NoBaseNoCommandRejected ensures that a provider
// that declares neither command nor base is still rejected by
// validateProviders.
func TestCreateProvider_NoBaseNoCommandRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateProvider("nothing", config.ProviderSpec{})
	if err == nil {
		t.Fatal("expected error for provider without command or base")
	}
}

func TestCreateProvider_RejectsInvalidLegacyBuiltinOptionDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateProvider("codex-fast", config.ProviderSpec{
		Command: "codex",
		OptionDefaults: map[string]string{
			"permission_mode": "typo",
		},
	})
	if err == nil {
		t.Fatal("expected invalid option_defaults to be rejected")
	}
	if !strings.Contains(err.Error(), `option_defaults key "permission_mode"`) {
		t.Fatalf("error = %v, want option_defaults validation detail", err)
	}
}

func TestCreateProvider_Duplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateProvider("custom", config.ProviderSpec{Command: "other"})
	if err == nil {
		t.Error("expected error for duplicate provider")
	}
}

func TestUpdateProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	newCmd := "updated-cli"
	newACPCmd := "updated-cli-acp"
	newName := "Updated Agent"
	err := ed.UpdateProvider("custom", configedit.ProviderUpdate{
		Command:     &newCmd,
		ACPCommand:  &newACPCmd,
		ACPArgs:     []string{"rpc", "--stdio"},
		DisplayName: &newName,
	})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got := cfg.Providers["custom"]
	if got.Command != "updated-cli" {
		t.Errorf("command = %q, want %q", got.Command, "updated-cli")
	}
	if got.ACPCommand != "updated-cli-acp" {
		t.Errorf("acp_command = %q, want %q", got.ACPCommand, "updated-cli-acp")
	}
	if len(got.ACPArgs) != 2 || got.ACPArgs[0] != "rpc" || got.ACPArgs[1] != "--stdio" {
		t.Errorf("acp_args = %#v, want [rpc --stdio]", got.ACPArgs)
	}
	if got.DisplayName != "Updated Agent" {
		t.Errorf("display_name = %q, want %q", got.DisplayName, "Updated Agent")
	}
}

func TestUpdateProvider_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	cmd := "x"
	err := ed.UpdateProvider("nonexistent", configedit.ProviderUpdate{Command: &cmd})
	if err == nil {
		t.Error("expected error for nonexistent provider")
	}
}

func TestUpdateProvider_PreservesUnchangedFields(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Only update command — display_name should be preserved.
	newCmd := "updated-cli"
	err := ed.UpdateProvider("custom", configedit.ProviderUpdate{Command: &newCmd})
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	cfg := readTOML(t, path)
	got := cfg.Providers["custom"]
	if got.Command != "updated-cli" {
		t.Errorf("command = %q, want %q", got.Command, "updated-cli")
	}
	if got.DisplayName != "Custom Agent" {
		t.Errorf("display_name was lost: %q", got.DisplayName)
	}
}

func TestDeleteProvider(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithProvider())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteProvider("custom"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	cfg := readTOML(t, path)
	if _, ok := cfg.Providers["custom"]; ok {
		t.Error("provider 'custom' still exists after delete")
	}
}

func TestDeleteProvider_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteProvider("nonexistent"); err == nil {
		t.Error("expected error for nonexistent provider")
	}
}

// --- Patch resource tests ---

func TestSetAgentPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	err := ed.SetAgentPatch(config.AgentPatch{
		Dir: "rig1", Name: "worker", Suspended: &suspended,
	})
	if err != nil {
		t.Fatalf("SetAgentPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1", len(cfg.Patches.Agents))
	}
	if cfg.Patches.Agents[0].Name != "worker" {
		t.Errorf("name = %q, want %q", cfg.Patches.Agents[0].Name, "worker")
	}
}

func TestSetAgentPatch_Replaces(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Set initial patch.
	suspended := true
	_ = ed.SetAgentPatch(config.AgentPatch{Dir: "rig1", Name: "worker", Suspended: &suspended})

	// Replace with different values.
	suspended = false
	err := ed.SetAgentPatch(config.AgentPatch{Dir: "rig1", Name: "worker", Suspended: &suspended})
	if err != nil {
		t.Fatalf("SetAgentPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1 (should replace, not append)", len(cfg.Patches.Agents))
	}
}

func TestDeleteAgentPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	_ = ed.SetAgentPatch(config.AgentPatch{Dir: "rig1", Name: "worker", Suspended: &suspended})

	if err := ed.DeleteAgentPatch("rig1/worker"); err != nil {
		t.Fatalf("DeleteAgentPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Agents) != 0 {
		t.Error("patches.agent should be empty after delete")
	}
}

func TestDeleteAgentPatch_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgentPatch("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent patch")
	}
}

func TestSetRigPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	err := ed.SetRigPatch(config.RigPatch{Name: "myrig", Suspended: &suspended})
	if err != nil {
		t.Fatalf("SetRigPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Rigs) != 1 {
		t.Fatalf("patches.rigs count = %d, want 1", len(cfg.Patches.Rigs))
	}
}

func TestDeleteRigPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	suspended := true
	_ = ed.SetRigPatch(config.RigPatch{Name: "myrig", Suspended: &suspended})

	if err := ed.DeleteRigPatch("myrig"); err != nil {
		t.Fatalf("DeleteRigPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Rigs) != 0 {
		t.Error("patches.rigs should be empty after delete")
	}
}

func TestSetProviderPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	cmd := "my-claude"
	err := ed.SetProviderPatch(config.ProviderPatch{Name: "claude", Command: &cmd})
	if err != nil {
		t.Fatalf("SetProviderPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Providers) != 1 {
		t.Fatalf("patches.providers count = %d, want 1", len(cfg.Patches.Providers))
	}
}

func TestDeleteProviderPatch(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	cmd := "my-claude"
	_ = ed.SetProviderPatch(config.ProviderPatch{Name: "claude", Command: &cmd})

	if err := ed.DeleteProviderPatch("claude"); err != nil {
		t.Fatalf("DeleteProviderPatch: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Patches.Providers) != 0 {
		t.Error("patches.providers should be empty after delete")
	}
}

func TestSetOrderOverride(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	enabled := false
	trigger := "cooldown"
	err := ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
		Trigger: &trigger,
	})
	if err != nil {
		t.Fatalf("SetOrderOverride: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(raw); got != "" && strings.Contains(got, "gate =") {
		t.Fatalf("city.toml still contains legacy gate key:\n%s", got)
	}
	if !strings.Contains(string(raw), `trigger = "cooldown"`) {
		t.Fatalf("city.toml missing canonical trigger key:\n%s", string(raw))
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(cfg.Orders.Overrides))
	}
	ov := cfg.Orders.Overrides[0]
	if ov.Name != "health-check" {
		t.Errorf("override name = %q, want %q", ov.Name, "health-check")
	}
	if ov.Enabled == nil || *ov.Enabled {
		t.Error("expected enabled=false")
	}
	if ov.Trigger == nil || *ov.Trigger != "cooldown" {
		t.Fatalf("override trigger = %#v, want cooldown", ov.Trigger)
	}
}

func TestSetOrderOverride_UpdateExisting(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	disabled := false
	trigger := "cooldown"
	_ = ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &disabled,
		Trigger: &trigger,
	})

	enabled := true
	err := ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("SetOrderOverride (update): %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(cfg.Orders.Overrides))
	}
	ov := cfg.Orders.Overrides[0]
	if ov.Enabled == nil || !*ov.Enabled {
		t.Error("expected enabled=true after update")
	}
	if ov.Trigger != nil {
		t.Fatalf("expected trigger to be replaced away, got %#v", ov.Trigger)
	}
}

func TestMergeOrderOverridePreservesExistingTriggerOnPartialUpdate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	disabled := false
	trigger := "cooldown"
	_ = ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &disabled,
		Trigger: &trigger,
	})

	enabled := true
	err := ed.MergeOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("MergeOrderOverride (partial update): %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(cfg.Orders.Overrides))
	}
	ov := cfg.Orders.Overrides[0]
	if ov.Enabled == nil || !*ov.Enabled {
		t.Fatal("expected enabled=true after partial update")
	}
	if ov.Trigger == nil || *ov.Trigger != "cooldown" {
		t.Fatalf("trigger = %#v, want cooldown", ov.Trigger)
	}
}

func TestDeleteOrderOverride(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	enabled := false
	_ = ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})

	if err := ed.DeleteOrderOverride("health-check", ""); err != nil {
		t.Fatalf("DeleteOrderOverride: %v", err)
	}

	cfg := readTOML(t, path)
	if len(cfg.Orders.Overrides) != 0 {
		t.Error("overrides should be empty after delete")
	}
}

func TestDeleteOrderOverride_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.DeleteOrderOverride("nonexistent", "")
	if err == nil {
		t.Fatal("expected error for deleting nonexistent override")
	}
}

func TestMergeOrderOverrideNormalizesLegacyGateToTriggerOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity()+`
[orders]

[[orders.overrides]]
name = "health-check"
gate = "cooldown"
`)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	enabled := true
	err := ed.MergeOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("MergeOrderOverride: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, "gate =") {
		t.Fatalf("city.toml still contains legacy gate key:\n%s", got)
	}
	if !strings.Contains(got, `trigger = "cooldown"`) {
		t.Fatalf("city.toml missing canonical trigger after enabled-only update:\n%s", got)
	}
}
