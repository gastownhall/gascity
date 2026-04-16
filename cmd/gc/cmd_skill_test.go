package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSkillsWorkOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"skills", "work"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skills work exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if out == "" {
		t.Fatal("gc skills work produced no output")
	}
	// Should contain bd commands.
	for _, want := range []string{"bd create", "bd list", "bd close", "bd ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestSkillsListTopics(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"skills"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skills exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, topic := range []string{"work", "dispatch", "agents", "rigs", "mail", "city", "dashboard"} {
		if !strings.Contains(out, topic) {
			t.Errorf("topic listing missing %q", topic)
		}
	}
}

func TestSkillsUnknownTopic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"skills", "bogus"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("gc skills bogus should fail")
	}
	if !strings.Contains(stderr.String(), "unknown topic") {
		t.Errorf("stderr = %q, want 'unknown topic'", stderr.String())
	}
}

func TestSkillsAllTopicsReadable(t *testing.T) {
	// Verify every registered topic has a matching embedded file.
	for _, topic := range skillTopics {
		var stdout, stderr bytes.Buffer
		code := run([]string{"skills", topic.Arg}, &stdout, &stderr)
		if code != 0 {
			t.Errorf("gc skills %s failed: %s", topic.Arg, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Errorf("gc skills %s produced no output", topic.Arg)
		}
	}
}

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

// TestSkillListAgentAttachmentFilter verifies that when an agent declares an
// explicit skills attachment list, the city catalog is filtered to those
// names. Agent-local entries remain visible regardless of attachment config.
func TestSkillListAgentAttachmentFilter(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	// mayor attaches only "attached-skill" from the city catalog; "other-skill"
	// must be filtered out.
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
	if strings.Contains(out, "other-skill") {
		t.Errorf("other-skill should be filtered out (not attached):\n%s", out)
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
