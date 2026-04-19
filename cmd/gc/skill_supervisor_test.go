package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRunStage1SkillMaterialization exercises the happy path of the
// Phase 4A supervisor-tick helper: a tmux city with a claude-provider
// city-scoped agent receives skills materialized at
// <cityPath>/.claude/skills/<name> pointing at the city pack source.
func TestRunStage1SkillMaterializationCityScoped(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}

	link := filepath.Join(cityPath, ".claude", "skills", "plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%q is not a symlink", link)
	}
	tgt, _ := os.Readlink(link)
	if tgt != filepath.Join(cityPath, "skills", "plan") {
		t.Errorf("symlink target = %q, want %q", tgt, filepath.Join(cityPath, "skills", "plan"))
	}
	if stderr.Len() > 0 {
		t.Logf("stderr: %s", stderr.String())
	}
}

func TestRunStage1CityScopedDirMatchingRigDoesNotGetRigSharedSkills(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())

	rigPath := filepath.Join(cityPath, "rigs", "fe")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	rigSkills := filepath.Join(cityPath, "imports", "helper", "skills")
	writeSkillSource(t, filepath.Join(rigSkills, "plan"))

	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
		Rigs:    []config.Rig{{Name: "fe", Path: rigPath}},
		RigPackSkills: map[string][]config.DiscoveredSkillCatalog{
			"fe": {{
				SourceDir:   rigSkills,
				BindingName: "helper",
				PackName:    "helper",
			}},
		},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Dir: "fe", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "helper.plan")); !os.IsNotExist(err) {
		t.Fatalf("city-scoped agent should not receive rig-shared skill, lstat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatalf("rig sink should remain untouched for city-scoped agent, stat err=%v", err)
	}
}

// TestRunStage1MaterializesIntoRigScope confirms that a rig-scoped
// agent materializes into the rig path, not the city path.
func TestRunStage1MaterializesIntoRigScope(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	rigPath := filepath.Join(cityPath, "rigs", "fe")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Rigs:          []config.Rig{{Name: "fe", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "polecat", Scope: "rig", Dir: "fe", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}

	// Should exist in rig path.
	if _, err := os.Lstat(filepath.Join(rigPath, ".claude", "skills", "plan")); err != nil {
		t.Errorf("rig sink missing: %v", err)
	}
	// Should NOT exist in city path.
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "plan")); err == nil {
		t.Errorf("city sink unexpectedly created for rig-scoped agent")
	}
}

// TestRunStage1SkipsIneligibleRuntimes confirms k8s and acp
// agents get no materialization even if their provider has a vendor
// sink — the spec forbids populating skills for agents whose
// runtime cannot reach the scope root.
func TestRunStage1SkipsIneligibleRuntimes(t *testing.T) {
	cases := []struct {
		name         string
		citySession  string
		agentSession string
	}{
		{"k8s city session", "k8s", ""},
		{"tmux city + acp agent", "tmux", "acp"},
		{"hybrid city session", "hybrid", ""},
		// Note: subprocess is STAGE-1 eligible (host scope root is
		// reachable) even though it's not stage-2 eligible (no
		// PreStart execution). See TestRunStage1SubprocessEligible.
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			clearGCEnv(t)
			cityPath := t.TempDir()
			t.Setenv("GC_HOME", t.TempDir())
			writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))

			cfg := &config.City{
				PackSkillsDir: filepath.Join(cityPath, "skills"),
				Session:       config.SessionConfig{Provider: c.citySession},
				Agents: []config.Agent{
					{Name: "x", Scope: "city", Provider: "claude", Session: c.agentSession},
				},
			}

			var stderr bytes.Buffer
			if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
				t.Fatal(err)
			}

			if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "plan")); err == nil {
				t.Errorf("ineligible runtime materialized skill; sink exists at %q", filepath.Join(cityPath, ".claude", "skills", "plan"))
			}
		})
	}
}

// TestRunStage1SkipsUnsupportedProvider confirms agents with no vendor
// sink (e.g., copilot) don't generate sink directories.
func TestRunStage1SkipsUnsupportedProvider(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "copilot-agent", Scope: "city", Provider: "copilot"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(cityPath)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("unexpected sink dir created: %s", e.Name())
		}
	}
}

// TestRunStage1MixedProvidersCreateSiblingSinks verifies the spec's
// mixed-provider scenario: a claude agent and a codex agent at the
// same scope root produce sibling .claude/skills/ and .codex/skills/
// sinks with the same city-pack skill.
func TestRunStage1MixedProvidersCreateSiblingSinks(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "deputy", Scope: "city", Provider: "codex"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}

	for _, vendor := range []string{".claude", ".codex"} {
		sink := filepath.Join(cityPath, vendor, "skills", "plan")
		info, err := os.Lstat(sink)
		if err != nil {
			t.Errorf("%s sink missing: %v", vendor, err)
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s sink is not a symlink", vendor)
		}
	}
}

func TestCheckSkillCollisionsReturnsFormattedError(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	agentASkills := filepath.Join(cityPath, "agents", "mayor", "skills")
	agentBSkills := filepath.Join(cityPath, "agents", "deputy", "skills")
	writeSkillSource(t, filepath.Join(agentASkills, "plan"))
	writeSkillSource(t, filepath.Join(agentBSkills, "plan"))

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: agentASkills},
			{Name: "deputy", Scope: "city", Provider: "claude", SkillsDir: agentBSkills},
		},
	}

	err := checkSkillCollisions(cfg, cityPath)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"agent-local skill collision",
		"plan",
		"mayor",
		"deputy",
		"claude",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("collision message missing %q:\n%s", want, msg)
		}
	}
}

func TestCheckSkillCollisionsPassesWhenClean(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}
	if err := checkSkillCollisions(cfg, "/city"); err != nil {
		t.Fatalf("expected nil for collision-free config, got %v", err)
	}
}

func TestRunStage1IdempotentConverges(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	for i := 0; i < 3; i++ {
		if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	// Symlink still present after 3 passes.
	link := filepath.Join(cityPath, ".claude", "skills", "plan")
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink lost after idempotent passes: %v", err)
	}
}

// TestRunStage1MaterializesBootstrapPackSkills is the regression for
// Phase 4 pass-1 Claude finding: stage-1 materialization must
// surface bootstrap implicit-import pack skills (e.g. `core`) into
// the agent sink, not just city-pack skills. LoadCityCatalog already
// folds bootstrap entries into its return, but a test pins the
// wiring so a future refactor can't silently drop it.
func TestRunStage1MaterializesBootstrapPackSkills(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()

	// Stand up a fake GC_HOME with a bootstrap-named implicit
	// import that resolves to a cache dir containing one skill.
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	// Use the first real bootstrap pack name so the materializer's
	// bootstrap-name filter lets the entry through.
	bootstrapName := bootstrapPackNameForTest(t)
	source := "github.com/example/" + bootstrapName
	commit := bootstrapName + "-commit"
	cacheDir := globalRepoCachePathForTest(gcHome, source, commit)
	if err := os.MkdirAll(filepath.Join(cacheDir, "skills", bootstrapName+"-sample"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cacheDir, "skills", bootstrapName+"-sample", "SKILL.md"),
		[]byte("---\nname: "+bootstrapName+"-sample\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pack.toml at the cache root so LoadCityCatalog's discovery
	// path doesn't reject the pack as malformed.
	if err := os.WriteFile(
		filepath.Join(cacheDir, "pack.toml"),
		[]byte("[pack]\nname = \""+bootstrapName+"\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// implicit-import.toml pointing at the cache.
	if err := os.WriteFile(
		filepath.Join(gcHome, "implicit-import.toml"),
		[]byte("schema = 1\n\n[imports.\""+bootstrapName+"\"]\nsource = \""+source+"\"\nversion = \"0.1.0\"\ncommit = \""+commit+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		// No city pack skills — bootstrap-only case.
		Session: config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}

	// Bootstrap skill symlink lands in the claude sink.
	link := filepath.Join(cityPath, ".claude", "skills", bootstrapName+"-sample")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("bootstrap skill symlink missing at %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%q is not a symlink", link)
	}
}

func TestRunStage1MaterializesAgentLocalWhenSharedCatalogFails(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	rigPath := filepath.Join(cityPath, "rigs", "fe")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	badRigCatalog := filepath.Join(cityPath, "broken-rig-catalog")
	if err := os.Mkdir(badRigCatalog, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(badRigCatalog, 0o755) })
	if _, err := os.ReadDir(badRigCatalog); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	agentSkills := filepath.Join(cityPath, "agents", "polecat", "skills")
	writeSkillSource(t, filepath.Join(agentSkills, "local-only"))

	cfg := &config.City{
		Session:       config.SessionConfig{Provider: "tmux"},
		Rigs:          []config.Rig{{Name: "fe", Path: rigPath}},
		RigPackSkills: map[string][]config.DiscoveredSkillCatalog{"fe": {{SourceDir: badRigCatalog, BindingName: "ops"}}},
		Agents: []config.Agent{
			{Name: "polecat", Scope: "rig", Dir: "fe", Provider: "claude", SkillsDir: agentSkills},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}
	if !strings.Contains(stderr.String(), "load shared skill catalog") {
		t.Fatalf("stderr = %q, want shared catalog load warning", stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(rigPath, ".claude", "skills", "local-only")); err != nil {
		t.Fatalf("agent-local skill should still materialize: %v", err)
	}
}

func TestRunStage1SharedCatalogFailurePrunesStaleSharedSymlink(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())

	skillsDir := filepath.Join(cityPath, "skills")
	writeSkillSource(t, filepath.Join(skillsDir, "plan"))

	cfg := &config.City{
		PackSkillsDir: skillsDir,
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("initial runStage1SkillMaterialization: %v", err)
	}
	link := filepath.Join(cityPath, ".claude", "skills", "plan")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("initial shared symlink missing: %v", err)
	}

	if err := os.Chmod(skillsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := os.ReadDir(skillsDir); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("second runStage1SkillMaterialization: %v", err)
	}
	if !strings.Contains(stderr.String(), "load shared skill catalog") {
		t.Fatalf("stderr = %q, want shared catalog load warning", stderr.String())
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("stale shared symlink should be pruned on catalog failure, lstat err=%v", err)
	}
}

// TestRunStage1SubprocessEligible confirms that Phase 4 split the
// stage-1 / stage-2 eligibility predicates correctly: a subprocess
// city session receives stage-1 materialization at its scope root
// (host-reachable filesystem) even though stage-2 PreStart isn't
// executed by the subprocess runtime. Regression for the Phase 4
// pass-1 Claude finding that over-gating was leaving subprocess
// agents with no skills.
func TestRunStage1SubprocessEligible(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "subprocess"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "plan")); err != nil {
		t.Errorf("subprocess session should receive stage-1 materialization: %v", err)
	}
}

// TestRunStage1AgentLocalOnlyInItsOwnSink confirms that an agent-local
// skill materializes only into that agent's sink, not into other
// agents' sinks at the same scope root.
func TestRunStage1AgentLocalOnlyInItsOwnSink(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())

	// mayor has its own private skill; deputy has no private skills.
	mayorSkills := filepath.Join(cityPath, "agents", "mayor", "skills")
	writeSkillSource(t, filepath.Join(mayorSkills, "mayor-only"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: mayorSkills},
			{Name: "deputy", Scope: "city", Provider: "codex"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}

	// mayor's claude sink gets the private skill.
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "mayor-only")); err != nil {
		t.Errorf("mayor-only missing from claude sink: %v", err)
	}
	// deputy's codex sink does NOT get mayor's private skill.
	if _, err := os.Lstat(filepath.Join(cityPath, ".codex", "skills", "mayor-only")); !os.IsNotExist(err) {
		t.Errorf("mayor-only leaked into codex sink; err=%v", err)
	}
}

// TestRunStage1RenameSkillLifecycle confirms that renaming a skill
// (delete old, add new name with same content) correctly cleans up
// the old symlink and creates the new one in a single tick. This is
// the spec's "rename = delete + add" lifecycle scenario.
func TestRunStage1RenameSkillLifecycle(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "old-name"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "old-name")); err != nil {
		t.Fatalf("old-name symlink missing: %v", err)
	}

	// Rename: delete old, create new.
	if err := os.RemoveAll(filepath.Join(cityPath, "skills", "old-name")); err != nil {
		t.Fatal(err)
	}
	writeSkillSource(t, filepath.Join(cityPath, "skills", "new-name"))

	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "old-name")); !os.IsNotExist(err) {
		t.Errorf("old-name symlink should be cleaned up, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "new-name")); err != nil {
		t.Errorf("new-name symlink missing: %v", err)
	}
}

// TestRunStage1CleansRemovedSkills confirms stage-1 cleanup removes
// symlinks that were in the catalog in an earlier pass but aren't
// anymore. Mirrors the MaterializeAgent orphan-delete path but
// verifies the wire-up from runStage1SkillMaterialization.
func TestRunStage1CleansRemovedSkills(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	writeSkillSource(t, filepath.Join(cityPath, "skills", "code-review"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	// Pass 1 — both skills materialized.
	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "plan")); err != nil {
		t.Fatalf("plan symlink missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "code-review")); err != nil {
		t.Fatalf("code-review symlink missing: %v", err)
	}

	// Remove plan from the catalog on disk.
	if err := os.RemoveAll(filepath.Join(cityPath, "skills", "plan")); err != nil {
		t.Fatal(err)
	}

	// Pass 2 — plan should be removed, code-review preserved.
	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "plan")); !os.IsNotExist(err) {
		t.Errorf("plan symlink should be removed, got err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, ".claude", "skills", "code-review")); err != nil {
		t.Errorf("code-review symlink should remain: %v", err)
	}
}
