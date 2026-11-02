package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

// An imported skill is cataloged under its binding-qualified name
// ("core.gc-work") while its SKILL.md frontmatter says "name: gc-work".
// Providers that load skills by frontmatter name (opencode 1.x) need the
// appendix to surface that name too (#8501).
func TestSkillAppendixShowsFrontmatterNameForImportedSkill(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(src, "gc-work"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: gc-work\ndescription: Working with beads\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(src, "gc-work", "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	city, err := materialize.LoadCityCatalog("", config.DiscoveredSkillCatalog{SourceDir: src, PackName: "core", BindingName: "core"})
	if err != nil {
		t.Fatal(err)
	}
	if len(city.Entries) != 1 || city.Entries[0].Name != "core.gc-work" {
		t.Fatalf("catalog entries = %+v, want one entry named core.gc-work", city.Entries)
	}

	got := buildAssignedSkillsPromptFragment(&config.Agent{Name: "worker", Scope: "city"}, &city, materialize.AgentCatalog{})
	if !strings.Contains(got, "`gc-work`") {
		t.Errorf("appendix lists only the binding-qualified name; want the SKILL.md frontmatter name `gc-work` shown too:\n%s", got)
	}
}
