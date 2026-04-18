package validation

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeAgentSkill creates a skill directory under baseSkillsDir with the
// given name and a minimal SKILL.md file. Returns the path to the
// agent's skills dir (mkdir -p'd) so tests can reuse it.
func writeAgentSkill(t *testing.T, baseSkillsDir, skillName string) {
	t.Helper()
	path := filepath.Join(baseSkillsDir, skillName)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("# "+skillName), 0o644); err != nil {
		t.Fatalf("WriteFile(SKILL.md): %v", err)
	}
}

// makeAgentSkillsDir returns <root>/<agentName>/skills after ensuring the
// directory exists.
func makeAgentSkillsDir(t *testing.T, root, agentName string) string {
	t.Helper()
	dir := filepath.Join(root, agentName, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	return dir
}

func TestValidateSkillCollisions(t *testing.T) {
	t.Run("no agents returns nil", func(t *testing.T) {
		if got := ValidateSkillCollisions(&config.City{}); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("nil cfg returns nil", func(t *testing.T) {
		if got := ValidateSkillCollisions(nil); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("single agent no collision", func(t *testing.T) {
		tmp := t.TempDir()
		skills := makeAgentSkillsDir(t, tmp, "mayor")
		writeAgentSkill(t, skills, "plan")

		cfg := &config.City{
			Agents: []config.Agent{{
				Name:      "mayor",
				Provider:  "claude",
				Scope:     "city",
				SkillsDir: skills,
			}},
		}
		if got := ValidateSkillCollisions(cfg); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("same scope and vendor collide", func(t *testing.T) {
		tmp := t.TempDir()
		mayorSkills := makeAgentSkillsDir(t, tmp, "mayor")
		supervisorSkills := makeAgentSkillsDir(t, tmp, "supervisor")
		writeAgentSkill(t, mayorSkills, "plan")
		writeAgentSkill(t, supervisorSkills, "plan")

		cfg := &config.City{
			Agents: []config.Agent{
				{
					Name:      "mayor",
					Provider:  "claude",
					Scope:     "city",
					SkillsDir: mayorSkills,
				},
				{
					Name:      "supervisor",
					Provider:  "claude",
					Scope:     "city",
					SkillsDir: supervisorSkills,
				},
			},
		}
		got := ValidateSkillCollisions(cfg)
		want := []SkillCollision{{
			ScopeRoot:  "<city>",
			Vendor:     "claude",
			SkillName:  "plan",
			AgentNames: []string{"mayor", "supervisor"},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("different vendors do not collide", func(t *testing.T) {
		tmp := t.TempDir()
		aSkills := makeAgentSkillsDir(t, tmp, "a")
		bSkills := makeAgentSkillsDir(t, tmp, "b")
		writeAgentSkill(t, aSkills, "plan")
		writeAgentSkill(t, bSkills, "plan")

		cfg := &config.City{
			Agents: []config.Agent{
				{Name: "a", Provider: "claude", Scope: "city", SkillsDir: aSkills},
				{Name: "b", Provider: "codex", Scope: "city", SkillsDir: bSkills},
			},
		}
		if got := ValidateSkillCollisions(cfg); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("different scopes do not collide", func(t *testing.T) {
		tmp := t.TempDir()
		citySkills := makeAgentSkillsDir(t, tmp, "city-agent")
		rigSkills := makeAgentSkillsDir(t, tmp, "rig-agent")
		writeAgentSkill(t, citySkills, "plan")
		writeAgentSkill(t, rigSkills, "plan")

		cfg := &config.City{
			Rigs: []config.Rig{{Name: "pit", Path: "/path/to/rig"}},
			Agents: []config.Agent{
				{Name: "city-agent", Provider: "claude", Scope: "city", SkillsDir: citySkills},
				{Name: "rig-agent", Provider: "claude", Scope: "rig", Dir: "pit", SkillsDir: rigSkills},
			},
		}
		if got := ValidateSkillCollisions(cfg); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})

	t.Run("three agents one colliding pair", func(t *testing.T) {
		tmp := t.TempDir()
		aSkills := makeAgentSkillsDir(t, tmp, "a")
		bSkills := makeAgentSkillsDir(t, tmp, "b")
		cSkills := makeAgentSkillsDir(t, tmp, "c")
		writeAgentSkill(t, aSkills, "A")
		writeAgentSkill(t, bSkills, "A")
		writeAgentSkill(t, cSkills, "B")

		cfg := &config.City{
			Rigs: []config.Rig{{Name: "pit", Path: "/path/to/rig"}},
			Agents: []config.Agent{
				{Name: "a", Provider: "claude", Scope: "rig", Dir: "pit", SkillsDir: aSkills},
				{Name: "b", Provider: "claude", Scope: "rig", Dir: "pit", SkillsDir: bSkills},
				{Name: "c", Provider: "claude", Scope: "rig", Dir: "pit", SkillsDir: cSkills},
			},
		}
		got := ValidateSkillCollisions(cfg)
		want := []SkillCollision{{
			ScopeRoot:  "/path/to/rig",
			Vendor:     "claude",
			SkillName:  "A",
			AgentNames: []string{"pit/a", "pit/b"},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("agents without provider are skipped", func(t *testing.T) {
		tmp := t.TempDir()
		aSkills := makeAgentSkillsDir(t, tmp, "a")
		bSkills := makeAgentSkillsDir(t, tmp, "b")
		writeAgentSkill(t, aSkills, "plan")
		writeAgentSkill(t, bSkills, "plan")

		cfg := &config.City{
			Agents: []config.Agent{
				{Name: "a", Scope: "city", SkillsDir: aSkills},
				{Name: "b", Scope: "city", SkillsDir: bSkills},
			},
		}
		if got := ValidateSkillCollisions(cfg); got != nil {
			t.Fatalf("want nil (no provider → no sink → no collision), got %+v", got)
		}
	})

	t.Run("non-skill-sink provider is skipped", func(t *testing.T) {
		tmp := t.TempDir()
		aSkills := makeAgentSkillsDir(t, tmp, "a")
		bSkills := makeAgentSkillsDir(t, tmp, "b")
		writeAgentSkill(t, aSkills, "plan")
		writeAgentSkill(t, bSkills, "plan")

		cfg := &config.City{
			Agents: []config.Agent{
				{Name: "a", Provider: "copilot", Scope: "city", SkillsDir: aSkills},
				{Name: "b", Provider: "copilot", Scope: "city", SkillsDir: bSkills},
			},
		}
		if got := ValidateSkillCollisions(cfg); got != nil {
			t.Fatalf("want nil (copilot has no sink), got %+v", got)
		}
	})

	t.Run("empty scope treated as rig", func(t *testing.T) {
		tmp := t.TempDir()
		aSkills := makeAgentSkillsDir(t, tmp, "a")
		bSkills := makeAgentSkillsDir(t, tmp, "b")
		writeAgentSkill(t, aSkills, "plan")
		writeAgentSkill(t, bSkills, "plan")

		cfg := &config.City{
			Rigs: []config.Rig{{Name: "pit", Path: "/path/to/rig"}},
			Agents: []config.Agent{
				{Name: "a", Provider: "claude", Dir: "pit", SkillsDir: aSkills},
				{Name: "b", Provider: "claude", Dir: "pit", SkillsDir: bSkills},
			},
		}
		got := ValidateSkillCollisions(cfg)
		want := []SkillCollision{{
			ScopeRoot:  "/path/to/rig",
			Vendor:     "claude",
			SkillName:  "plan",
			AgentNames: []string{"pit/a", "pit/b"},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("skills dir without SKILL.md is ignored", func(t *testing.T) {
		tmp := t.TempDir()
		aSkills := makeAgentSkillsDir(t, tmp, "a")
		bSkills := makeAgentSkillsDir(t, tmp, "b")
		// Agent a has a real skill.
		writeAgentSkill(t, aSkills, "plan")
		// Agent b has a directory named "plan" but no SKILL.md (not a skill).
		if err := os.MkdirAll(filepath.Join(bSkills, "plan"), 0o755); err != nil {
			t.Fatal(err)
		}

		cfg := &config.City{
			Agents: []config.Agent{
				{Name: "a", Provider: "claude", Scope: "city", SkillsDir: aSkills},
				{Name: "b", Provider: "claude", Scope: "city", SkillsDir: bSkills},
			},
		}
		if got := ValidateSkillCollisions(cfg); got != nil {
			t.Fatalf("want nil (b's plan/ has no SKILL.md), got %+v", got)
		}
	})

	t.Run("agent SkillsDir missing from disk is a no-op", func(t *testing.T) {
		cfg := &config.City{
			Agents: []config.Agent{
				{Name: "a", Provider: "claude", Scope: "city", SkillsDir: "/nonexistent"},
				{Name: "b", Provider: "claude", Scope: "city", SkillsDir: "/also-nonexistent"},
			},
		}
		if got := ValidateSkillCollisions(cfg); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
}
