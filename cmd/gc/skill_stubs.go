package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// materializeSkillStubs writes Claude Code skill stubs to
// .claude/skills/gc-<topic>/SKILL.md in each of the given directories.
// Stubs contain YAML frontmatter (name + description) and a dynamic
// command that calls gc skills <topic> for content.
//
// Always overwrites — same philosophy as MaterializeBuiltinPacks and
// MaterializeSystemFormulas. Idempotent: safe to call on every gc start.
func materializeSkillStubs(dirs ...string) error {
	for _, dir := range dirs {
		for _, t := range skillTopics {
			stub := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n!`gc skills %s`\n",
				t.Name, t.Desc, t.Arg)
			path := filepath.Join(dir, ".claude", "skills", t.Name, "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("creating skill stub dir for %s: %w", t.Name, err)
			}
			if err := os.WriteFile(path, []byte(stub), 0o644); err != nil {
				return fmt.Errorf("writing skill stub %s: %w", t.Name, err)
			}
		}
	}
	return nil
}
