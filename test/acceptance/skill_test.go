//go:build acceptance_a

// Skill materialization acceptance tests (Phase 4B).
//
// These tests invoke the gc binary as a black box to prove:
//   - `gc internal materialize-skills` correctly creates sink
//     symlinks at a non-scope-root workdir (stage-2 per-session).
//   - `gc doctor` flags agent-local skill name collisions.
//   - `gc skill list` includes bootstrap implicit-import pack skills
//     alongside city-pack skills (Phase 3C wiring).
//
// Stage-1 supervisor-tick materialization and the full lifecycle
// (add/edit/delete/rename) are covered by the unit tests in
// cmd/gc/skill_supervisor_test.go and the integration test in
// test/integration/skill_lifecycle_test.go — this acceptance layer
// focuses on the CLI surfaces operators actually invoke.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestSkillMaterializeCLI runs `gc internal materialize-skills` with
// a non-default workdir and asserts the expected symlinks appear.
// Exercises the Phase 3A + the full config-load-plus-vendor-sink
// lookup path through the real binary.
func TestSkillMaterializeCLI(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// Add a skill to the city pack's skills directory.
	skillDir := filepath.Join(c.Dir, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}

	// Invoke the internal materializer against a fresh workdir.
	// `gc init --provider claude` creates an agent named "mayor"
	// that inherits provider=claude from the workspace.
	workdir := t.TempDir()
	out, err := helpers.RunGC(testEnv, c.Dir,
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	)
	if err != nil {
		t.Fatalf("gc internal materialize-skills: %v\n%s", err, out)
	}
	if !strings.Contains(out, "materialized") {
		t.Errorf("expected 'materialized' in output: %s", out)
	}

	// Assert the symlink landed at the expected path.
	link := filepath.Join(workdir, ".claude", "skills", "plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%q is not a symlink", link)
	}
	tgt, _ := os.Readlink(link)
	if !strings.HasSuffix(tgt, filepath.Join("skills", "plan")) {
		t.Errorf("symlink target = %q, want suffix skills/plan", tgt)
	}
}

// TestSkillDoctorFlagsCollision creates two convention-discovered
// agents whose agent-local skills directories both contribute a
// skill with the same name. `gc doctor` must surface the collision.
//
// Uses agent.toml convention-discovery rather than city.toml
// [[agent]] entries because SkillsDir is only populated for
// convention-discovered agents (see internal/config/agent_discovery.go).
func TestSkillDoctorFlagsCollision(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// Two convention-discovered agents with agent.toml so their
	// Provider + SkillsDir both populate at config load.
	for _, name := range []string{"collider-a", "collider-b"} {
		agentDir := filepath.Join(c.Dir, "agents", name)
		if err := os.MkdirAll(filepath.Join(agentDir, "skills", "plan"), 0o755); err != nil {
			t.Fatalf("mkdir skills for %s: %v", name, err)
		}
		// agent.toml with matching provider.
		if err := os.WriteFile(
			filepath.Join(agentDir, "agent.toml"),
			[]byte(`provider = "claude"`+"\n"), 0o644); err != nil {
			t.Fatalf("agent.toml for %s: %v", name, err)
		}
		// prompt.md so the agent is structurally valid.
		if err := os.WriteFile(
			filepath.Join(agentDir, "prompt.md"),
			[]byte("test prompt\n"), 0o644); err != nil {
			t.Fatalf("prompt for %s: %v", name, err)
		}
		// The colliding skill.
		if err := os.WriteFile(
			filepath.Join(agentDir, "skills", "plan", "SKILL.md"),
			[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
			t.Fatalf("SKILL.md for %s: %v", name, err)
		}
	}

	out, _ := helpers.RunGC(testEnv, c.Dir, "doctor")
	// Doctor reports collisions via the skill-collision check. The
	// exit code varies depending on other check results, so don't
	// gate on it — assert the message surfaces.
	if !strings.Contains(out, "collision") {
		t.Errorf("doctor output missing 'collision':\n%s", out)
	}
	if !strings.Contains(out, "plan") {
		t.Errorf("doctor output missing skill name 'plan':\n%s", out)
	}
	if !strings.Contains(out, "collider-a") || !strings.Contains(out, "collider-b") {
		t.Errorf("doctor output missing one of the colliding agent names:\n%s", out)
	}
}

// TestSkillListIncludesBootstrap is the acceptance-level check for
// Phase 3C: `gc skill list` shows bootstrap implicit-import pack
// skills alongside city-pack skills.
func TestSkillListIncludesBootstrap(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// Add a city-pack skill.
	cityPlan := filepath.Join(c.Dir, "skills", "plan")
	if err := os.MkdirAll(cityPlan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cityPlan, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := helpers.RunGC(testEnv, c.Dir, "skill", "list")
	if err != nil {
		t.Fatalf("gc skill list: %v\n%s", err, out)
	}
	// City-pack skill present.
	if !strings.Contains(out, "plan") {
		t.Errorf("skill list missing 'plan' (city-pack):\n%s", out)
	}
	// The core bootstrap pack ships gc-work etc. as of Phase 1. When
	// gc init completes, the implicit-import.toml has core wired in.
	// At least one gc-<topic> entry should show up.
	if !strings.Contains(out, "gc-work") {
		t.Logf("skill list output:\n%s", out)
		t.Error("skill list missing 'gc-work' from core bootstrap pack — Phase 3C wiring may be broken")
	}
}
