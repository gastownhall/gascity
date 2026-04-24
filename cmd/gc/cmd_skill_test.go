package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSkillRejectsTopicMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "work"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("gc skill work should fail")
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr = %q, want 'unknown subcommand'", stderr.String())
	}
}

func TestSkillListCityCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "skills/code-review/SKILL.md", "city skill")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "code-review", "city", "skills/code-review/SKILL.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list output missing %q:\n%s", want, out)
		}
	}
}

func TestSkillListAgentCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "skills/code-review/SKILL.md", "city skill")
	writeCatalogFile(t, cityDir, "agents/mayor/skills/private-workflow/SKILL.md", "agent skill")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list --agent exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"code-review", "city", "private-workflow", "agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list --agent output missing %q:\n%s", want, out)
		}
	}
}

func TestSkillListImportedSharedCatalog(t *testing.T) {
	clearGCEnv(t)
	rootDir := t.TempDir()
	cityDir := filepath.Join(rootDir, "city")
	packDir := filepath.Join(rootDir, "helper")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, packDir, "pack.toml", "[pack]\nname = \"helper\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeCatalogFile(t, packDir, "skills/code-review/SKILL.md", "imported skill")
	writeCatalogFile(t, cityDir, "pack.toml", "[pack]\nname = \"city\"\nversion = \"0.1.0\"\nschema = 2\n\n[imports.helper]\nsource = \"../helper\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"helper.code-review", "helper"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list output missing %q:\n%s", want, out)
		}
	}
}

func TestSkillListAgentCityScopedDirMatchingRigDoesNotShowRigSharedSkills(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "fe")
	rigSkills := filepath.Join(cityDir, "imports", "helper", "skills")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCatalogFile(t, cityDir, "imports/helper/skills/plan/SKILL.md", "rig-import skill")

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "fe", Path: rigDir}},
		RigPackSkills: map[string][]config.DiscoveredSkillCatalog{
			"fe": {{
				SourceDir:   rigSkills,
				BindingName: "helper",
				PackName:    "helper",
			}},
		},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Dir: "fe"},
		},
	}

	entries, err := listVisibleSkillEntries(cityDir, cfg, nil, "mayor", "")
	if err != nil {
		t.Fatalf("listVisibleSkillEntries: %v", err)
	}
	for _, entry := range entries {
		if entry.Name == "helper.plan" {
			t.Fatalf("city-scoped agent should not list rig-shared skill: %+v", entries)
		}
	}
}

func TestSkillListSessionCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "skills/code-review/SKILL.md", "city skill")
	writeCatalogFile(t, cityDir, "agents/mayor/skills/private-workflow/SKILL.md", "agent skill")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "mayor session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     "mayor",
			"session_name": "s-mayor-1",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list", "--session", bead.ID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list --session exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"code-review", "city", "private-workflow", "agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list --session output missing %q:\n%s", want, out)
		}
	}
}

// TestSkillListAgentShowsFullCityCatalog verifies that an agent-scoped
// `gc skill list --agent mayor` returns the entire city catalog plus the
// agent's private skills. Per engdocs/proposals/skill-materialization.md
// there is no attachment filtering — every agent sees every city skill.
// The `skills = [...]` tombstone on the agent is accepted but ignored.
func TestSkillListAgentShowsFullCityCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	// mayor declares an attachment list — this is a v0.15.0 tombstone and
	// must be ignored; other-skill should still appear in the agent's view.
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "codex"
start_command = "echo"
skills = ["attached-skill"]

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeCatalogFile(t, cityDir, "skills/attached-skill/SKILL.md", "attached")
	writeCatalogFile(t, cityDir, "skills/other-skill/SKILL.md", "other")
	writeCatalogFile(t, cityDir, "agents/mayor/skills/private-workflow/SKILL.md", "agent-local")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list --agent mayor exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "attached-skill") {
		t.Errorf("attached-skill missing from output:\n%s", out)
	}
	if !strings.Contains(out, "private-workflow") {
		t.Errorf("agent-local private-workflow missing from output:\n%s", out)
	}
	if !strings.Contains(out, "other-skill") {
		t.Errorf("other-skill must remain visible — no attachment filtering:\n%s", out)
	}
}

func writeCatalogFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
