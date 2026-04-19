package configedit_test

import (
	"os"
	"path/filepath"
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
	newName := "Updated Agent"
	err := ed.UpdateProvider("custom", configedit.ProviderUpdate{
		Command:     &newCmd,
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
	err := ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("SetOrderOverride: %v", err)
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
}

func TestSetOrderOverride_UpdateExisting(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	disabled := false
	_ = ed.SetOrderOverride(config.OrderOverride{
		Name:    "health-check",
		Enabled: &disabled,
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
	if cfg.Orders.Overrides[0].Enabled == nil || !*cfg.Orders.Overrides[0].Enabled {
		t.Error("expected enabled=true after update")
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
