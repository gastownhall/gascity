package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeSkillStubs(t *testing.T) {
	dir := t.TempDir()
	if err := materializeSkillStubs(dir); err != nil {
		t.Fatalf("materializeSkillStubs: %v", err)
	}

	// Verify all 6 stubs were written.
	for _, topic := range skillTopics {
		path := filepath.Join(dir, ".claude", "skills", topic.Name, "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("missing stub for %s: %v", topic.Name, err)
			continue
		}
		content := string(data)
		// Check YAML frontmatter.
		if !strings.Contains(content, "name: "+topic.Name) {
			t.Errorf("%s stub missing name in frontmatter", topic.Name)
		}
		if !strings.Contains(content, "description: "+topic.Desc) {
			t.Errorf("%s stub missing description in frontmatter", topic.Name)
		}
		// Check dynamic command.
		if !strings.Contains(content, "!`gc skills "+topic.Arg+"`") {
			t.Errorf("%s stub missing dynamic command", topic.Name)
		}
	}
}

func TestMaterializeSkillStubsMultipleDirs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()

	if err := materializeSkillStubs(cityDir, rigDir); err != nil {
		t.Fatalf("materializeSkillStubs: %v", err)
	}

	// Both dirs should have stubs.
	for _, dir := range []string{cityDir, rigDir} {
		path := filepath.Join(dir, ".claude", "skills", "gc-work", "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("stub missing in %s: %v", dir, err)
		}
	}
}

func TestMaterializeSkillStubsOverwrites(t *testing.T) {
	dir := t.TempDir()

	// Write initial stubs.
	if err := materializeSkillStubs(dir); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Manually modify a stub.
	path := filepath.Join(dir, ".claude", "skills", "gc-work", "SKILL.md")
	if err := os.WriteFile(path, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-materialize — should overwrite.
	if err := materializeSkillStubs(dir); err != nil {
		t.Fatalf("second call: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "old content" {
		t.Error("stub was not overwritten")
	}
	if !strings.Contains(string(data), "gc-work") {
		t.Error("overwritten stub missing expected content")
	}
}
