package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

// #8501: an imported skill is listed under its binding-prefixed name
// ("core.gc-work") while its SKILL.md keeps the pack's name ("gc-work"),
// which is the only name a provider that registers skills by frontmatter
// name (opencode 1.x) can load. The appendix must show both.
func TestSkillAppendixFrontmatterName(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	write := func(root, dir, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: d\n---\nbody\n"
		if err := os.WriteFile(filepath.Join(root, dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	city := filepath.Join(t.TempDir(), "skills")
	imported := filepath.Join(t.TempDir(), "skills")
	write(city, "plan", "plan")
	write(imported, "gc-work", "gc-work")

	cat, err := materialize.LoadCityCatalog(city, config.DiscoveredSkillCatalog{
		SourceDir: imported, PackName: "core", BindingName: "core",
	})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	writeSkillBullets(&b, cat.Entries, "origin")
	out := b.String()

	if !strings.Contains(out, "- `core.gc-work` (SKILL.md name: `gc-work`)") {
		t.Errorf("imported skill bullet does not name the SKILL.md name a provider can load:\n%s", out)
	}
	if strings.Contains(out, "`plan` (SKILL.md name") {
		t.Errorf("a skill whose SKILL.md name equals its listed name gained a note:\n%s", out)
	}
}
