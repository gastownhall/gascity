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
// same scope root produce sibling .claude/skills/ and .agents/skills/
// sinks (the codex CLI reads .agents/skills, not .codex/skills) with the
// same city-pack skill.
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

	for _, vendor := range []string{".claude", ".agents"} {
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

// TestRunStage1PiMaterializesIntoAgentsSkills verifies that a pi agent
// gets a real on-disk sink, not just a lookup-table entry: pi shares
// codex's .agents/skills location, so a city-scoped pi agent must end up
// with a symlink there resolving back to the shared catalog source.
func TestRunStage1PiMaterializesIntoAgentsSkills(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	source := filepath.Join(cityPath, "skills", "plan")
	writeSkillSource(t, source)

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "pi"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}

	sink := filepath.Join(cityPath, ".agents", "skills", "plan")
	info, err := os.Lstat(sink)
	if err != nil {
		t.Fatalf(".agents/skills sink missing for pi agent: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".agents/skills sink is not a symlink")
	}
	got, err := filepath.EvalSymlinks(sink)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", sink, err)
	}
	want, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", source, err)
	}
	if got != want {
		t.Fatalf("pi sink resolves to %s, want %s", got, want)
	}

	// pi must not get its own .pi/skills sink — that would make pi scan
	// the same skill twice and hit its name-collision path.
	if _, err := os.Lstat(filepath.Join(cityPath, ".pi", "skills")); !os.IsNotExist(err) {
		t.Fatalf(".pi/skills should not be created, Lstat err = %v", err)
	}
}

func TestRunStage1UsesCachedCatalogAfterSharedCatalogFailureAcrossRepeatedFailures(t *testing.T) {
	clearGCEnv(t)
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())

	importRoot := filepath.Join(cityPath, "imports", "helper")
	importSkills := filepath.Join(importRoot, "skills")
	importLink := filepath.Join(cityPath, "imports", "helper-link")
	writeSkillSource(t, filepath.Join(importSkills, "plan"))
	if err := os.MkdirAll(filepath.Dir(importLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(importSkills, importLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
		PackSkills: []config.DiscoveredSkillCatalog{{
			SourceDir:   importLink,
			BindingName: "helper",
			PackName:    "helper",
		}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("baseline runStage1SkillMaterialization: %v", err)
	}
	link := filepath.Join(cityPath, ".claude", "skills", "helper.plan")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("baseline shared symlink missing: %v", err)
	}

	replaceWithSelfSymlink(t, importLink)
	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("degraded runStage1SkillMaterialization: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("cached stage-1 materialization should preserve shared symlink, got %v", err)
	}
	if !strings.Contains(stderr.String(), "load shared skill catalog for city scope") {
		t.Fatalf("stderr = %q, want shared catalog warning", stderr.String())
	}

	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("second degraded runStage1SkillMaterialization: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("second repeated shared-root failure should still preserve shared symlink, got %v", err)
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

func TestRunStage1MaterializesImportedPackSkills(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	helperDir := filepath.Join(cityPath, "imports", "helper")
	writeSkillSource(t, filepath.Join(helperDir, "skills", "plan"))

	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
		PackSkills: []config.DiscoveredSkillCatalog{{
			SourceDir:   filepath.Join(helperDir, "skills"),
			PackDir:     helperDir,
			PackName:    "helper",
			BindingName: "helper",
		}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}

	link := filepath.Join(cityPath, ".claude", "skills", "helper.plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("imported skill symlink missing at %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%q is not a symlink", link)
	}
	tgt, _ := os.Readlink(link)
	if want := filepath.Join(helperDir, "skills", "plan"); tgt != want {
		t.Fatalf("symlink target = %q, want %q", tgt, want)
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

func TestRunStage1SharedCatalogFailureKeepsLastGoodSharedSymlink(t *testing.T) {
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
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("cached stage-1 materialization should keep the last-good shared symlink, got %v", err)
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
	// deputy's codex sink (.agents/skills) does NOT get mayor's private skill.
	if _, err := os.Lstat(filepath.Join(cityPath, ".agents", "skills", "mayor-only")); !os.IsNotExist(err) {
		t.Errorf("mayor-only leaked into codex sink; err=%v", err)
	}
}

// TestRunStage1SharedSinkKeepsAgentLocalSkill covers two agents that share
// one sink: same scope root (the city) and same provider family, so both
// materialize into <city>/.claude/skills. Only mayor has an agent-local
// skill, and it must survive the pass. The sink's ownership manifest marks
// every gc-written link as prunable by any later pass, so a pass per agent
// would let deputy, later in cfg.Agents, find mayor's link undesired and
// remove it; the sink is materialized once with both agents' skills.
func TestRunStage1SharedSinkKeepsAgentLocalSkill(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())

	// A shared skill keeps deputy's pass from being skipped as empty.
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	mayorSkills := filepath.Join(cityPath, "agents", "mayor", "skills")
	writeSkillSource(t, filepath.Join(mayorSkills, "a-only"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: mayorSkills},
			{Name: "deputy", Scope: "city", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}

	sink := filepath.Join(cityPath, ".claude", "skills")
	if _, err := os.Lstat(filepath.Join(sink, "plan")); err != nil {
		t.Fatalf("shared skill plan missing from shared sink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sink, "a-only")); err != nil {
		t.Errorf("mayor's agent-local skill a-only removed from the sink it shares with deputy: %v; stderr=%q", err, stderr.String())
	}
}

// TestRunStage1SharedSinkAgentLocalOverridesShared: mayor's agent-local
// plan overrides the shared plan, as it does for mayor alone. deputy shares
// the sink and wants the shared plan; the sink holds one link per name, so
// the override wins whichever agent comes first in the config.
func TestRunStage1SharedSinkAgentLocalOverridesShared(t *testing.T) {
	for _, order := range []string{"mayor-first", "deputy-first"} {
		t.Run(order, func(t *testing.T) {
			clearGCEnv(t)
			cityPath := t.TempDir()
			t.Setenv("GC_HOME", t.TempDir())
			writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
			mayorSkills := filepath.Join(cityPath, "agents", "mayor", "skills")
			writeSkillSource(t, filepath.Join(mayorSkills, "plan"))

			agents := []config.Agent{
				{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: mayorSkills},
				{Name: "deputy", Scope: "city", Provider: "claude"},
			}
			if order == "deputy-first" {
				agents[0], agents[1] = agents[1], agents[0]
			}
			cfg := &config.City{
				PackSkillsDir: filepath.Join(cityPath, "skills"),
				Session:       config.SessionConfig{Provider: "tmux"},
				Agents:        agents,
			}
			var stderr bytes.Buffer
			if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
				t.Fatal(err)
			}
			tgt, err := os.Readlink(filepath.Join(cityPath, ".claude", "skills", "plan"))
			if err != nil || tgt != filepath.Join(mayorSkills, "plan") {
				t.Errorf("plan -> %q (err %v), want mayor's agent-local source; stderr=%q", tgt, err, stderr.String())
			}
		})
	}
}

// TestRunStage1SharedSinkLocalConflictKeepsExistingLinks covers two
// agents in one sink that provide the same agent-local skill name from
// different directories. checkSkillCollisions rejects this config before
// materialization; called directly, the stage-1 pass must not pick one
// source over the other. It reports the conflict and does not reconcile
// the sink that pass, so the links the previous pass wrote stay.
func TestRunStage1SharedSinkLocalConflictKeepsExistingLinks(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())

	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	mayorSkills := filepath.Join(cityPath, "agents", "mayor", "skills")
	deputySkills := filepath.Join(cityPath, "agents", "deputy", "skills")
	writeSkillSource(t, filepath.Join(mayorSkills, "dup"))

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: mayorSkills},
			{Name: "deputy", Scope: "city", Provider: "claude", SkillsDir: deputySkills},
		},
	}
	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(cityPath, ".claude", "skills")
	link := filepath.Join(sink, "dup")
	if tgt, err := os.Readlink(link); err != nil || tgt != filepath.Join(mayorSkills, "dup") {
		t.Fatalf("precondition: dup -> %q (err %v), want mayor's source", tgt, err)
	}

	writeSkillSource(t, filepath.Join(deputySkills, "dup"))
	writeSkillSource(t, filepath.Join(cityPath, "skills", "review"))
	if err := checkSkillCollisions(cfg, cityPath); err == nil {
		t.Fatal("checkSkillCollisions accepted two agent-local dup skills in one sink")
	}

	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err == nil || !strings.Contains(err.Error(), sink) {
		t.Errorf("pass over a conflicting sink returned %v, want an error naming %s", err, sink)
	}
	for _, want := range []string{`skill "dup"`, `agent "mayor"`, `agent "deputy"`, sink, "sink not reconciled this pass"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %q", want, stderr.String())
		}
	}
	if tgt, err := os.Readlink(link); err != nil || tgt != filepath.Join(mayorSkills, "dup") {
		t.Errorf("dup -> %q (err %v) after the collision, want mayor's source left in place", tgt, err)
	}
	if _, err := os.Lstat(filepath.Join(sink, "review")); !os.IsNotExist(err) {
		t.Errorf("sink with a conflict was reconciled anyway: review lstat err=%v", err)
	}
}

// sharedPathRigsCity returns a config with two rigs, fe and be, at one
// path. ValidateRigs requires unique rig names and prefixes, not unique
// paths, so their claude agents share one sink. Each rig imports a pack
// under the binding "ops", from feOps and beOps respectively.
func sharedPathRigsCity(cityPath, feOps, beOps string) *config.City {
	rigPath := filepath.Join(cityPath, "rigs", "shared")
	return &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Rigs:          []config.Rig{{Name: "fe", Path: rigPath}, {Name: "be", Path: rigPath}},
		RigPackSkills: map[string][]config.DiscoveredSkillCatalog{
			"fe": {{SourceDir: feOps, BindingName: "ops", PackName: "ops"}},
			"be": {{SourceDir: beOps, BindingName: "ops", PackName: "ops"}},
		},
		Agents: []config.Agent{
			{Name: "polecat", Scope: "rig", Dir: "fe", Provider: "claude"},
			{Name: "witness", Scope: "rig", Dir: "be", Provider: "claude"},
		},
	}
}

// TestRunStage1SharedSinkLocalOverrideSettlesSharedConflict: the two rigs'
// imports give ops.review different sources in one sink, but polecat's
// agent-local ops.review overrides it. The sink links the agent-local
// source either way, so the shared clash is not a conflict and the sink
// reconciles.
func TestRunStage1SharedSinkLocalOverrideSettlesSharedConflict(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	opsA := filepath.Join(cityPath, "imports", "ops-a", "skills")
	opsB := filepath.Join(cityPath, "imports", "ops-b", "skills")
	writeSkillSource(t, filepath.Join(opsA, "review"))
	writeSkillSource(t, filepath.Join(opsB, "review"))
	polecatSkills := filepath.Join(cityPath, "agents", "polecat", "skills")
	writeSkillSource(t, filepath.Join(polecatSkills, "ops.review"))

	cfg := sharedPathRigsCity(cityPath, opsA, opsB)
	cfg.Agents[0].SkillsDir = polecatSkills

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "not reconciled") {
		t.Errorf("agent-local override reported as a conflict: %q", stderr.String())
	}
	link := filepath.Join(cityPath, "rigs", "shared", ".claude", "skills", "ops.review")
	if tgt, err := os.Readlink(link); err != nil || tgt != filepath.Join(polecatSkills, "ops.review") {
		t.Errorf("ops.review -> %q (err %v), want polecat's agent-local source", tgt, err)
	}
}

// TestRunStage1SharedSinkSharedConflictDegradesReconciliation reaches a
// conflict through the production gates: ValidateRigs accepts two rigs at
// one path and checkSkillCollisions checks only agent-local names, so
// both pass when the rigs' imports give ops.review two sources. The
// sink's pass is degraded, not rejected: existing links stay, a new
// shared skill is not added and a removed one is not pruned until the
// conflict is resolved, and then the sink reconciles.
func TestRunStage1SharedSinkSharedConflictDegradesReconciliation(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	opsA := filepath.Join(cityPath, "imports", "ops-a", "skills")
	opsB := filepath.Join(cityPath, "imports", "ops-b", "skills")
	writeSkillSource(t, filepath.Join(opsA, "review"))
	writeSkillSource(t, filepath.Join(opsB, "review"))
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	dropped := filepath.Join(cityPath, "skills", "dropped")
	writeSkillSource(t, dropped)

	cfg := sharedPathRigsCity(cityPath, opsA, opsA)
	sink := filepath.Join(cityPath, "rigs", "shared", ".claude", "skills")
	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plan", "dropped", "ops.review"} {
		if _, err := os.Lstat(filepath.Join(sink, name)); err != nil {
			t.Fatalf("precondition: %s missing: %v", name, err)
		}
	}

	cfg.RigPackSkills["be"] = []config.DiscoveredSkillCatalog{{SourceDir: opsB, BindingName: "ops", PackName: "ops"}}
	if err := config.ValidateRigs(cfg.Rigs, "hq"); err != nil {
		t.Fatalf("ValidateRigs rejected two rigs at one path: %v", err)
	}
	if err := checkSkillCollisions(cfg, cityPath); err != nil {
		t.Fatalf("checkSkillCollisions rejected a shared-source clash: %v", err)
	}
	writeSkillSource(t, filepath.Join(cityPath, "skills", "added"))
	if err := os.RemoveAll(dropped); err != nil {
		t.Fatal(err)
	}

	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err == nil || !strings.Contains(err.Error(), sink) {
		t.Errorf("pass over a conflicting sink returned %v, want an error naming %s", err, sink)
	}
	for _, want := range []string{`skill "ops.review"`, `agent "fe/polecat"`, `agent "be/witness"`, "existing links kept"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %q", want, stderr.String())
		}
	}
	if tgt, err := os.Readlink(filepath.Join(sink, "ops.review")); err != nil || tgt != filepath.Join(opsA, "review") {
		t.Errorf("ops.review -> %q (err %v) during the conflict, want the existing link kept", tgt, err)
	}
	if _, err := os.Lstat(filepath.Join(sink, "dropped")); err != nil {
		t.Errorf("dropped was pruned during the conflict: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sink, "added")); !os.IsNotExist(err) {
		t.Errorf("added was linked during the conflict: lstat err=%v", err)
	}

	cfg.RigPackSkills["be"] = []config.DiscoveredSkillCatalog{{SourceDir: opsA, BindingName: "ops", PackName: "ops"}}
	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "not reconciled") {
		t.Errorf("conflict still reported after it was resolved: %q", stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(sink, "added")); err != nil {
		t.Errorf("added missing after the conflict was resolved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sink, "dropped")); !os.IsNotExist(err) {
		t.Errorf("dropped not pruned after the conflict was resolved: lstat err=%v", err)
	}
}

// TestRunStage1SharedSinkIncompletePassKeepsEveryAgentsSkips: user content
// blocks one agent-local skill of each agent in a shared sink, so the
// first pass reports one skip per agent. A pass that does not reconcile
// the sink must carry the skip history of every agent in it, not only
// one, so neither skip prints again once the sink reconciles.
func TestRunStage1SharedSinkIncompletePassKeepsEveryAgentsSkips(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "")
	cityPath := t.TempDir()
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	mayorSkills := filepath.Join(cityPath, "agents", "mayor", "skills")
	deputySkills := filepath.Join(cityPath, "agents", "deputy", "skills")
	writeSkillSource(t, filepath.Join(mayorSkills, "m-only"))
	writeSkillSource(t, filepath.Join(deputySkills, "d-only"))
	sink := filepath.Join(cityPath, ".claude", "skills")
	for _, name := range []string{"m-only", "d-only"} {
		if err := os.MkdirAll(filepath.Join(sink, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: mayorSkills},
			{Name: "deputy", Scope: "city", Provider: "claude", SkillsDir: deputySkills},
		},
	}

	var stderr bytes.Buffer
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)
	// An agent-local clash keeps the next pass from reconciling the sink.
	writeSkillSource(t, filepath.Join(mayorSkills, "dup"))
	dup := filepath.Join(deputySkills, "dup")
	writeSkillSource(t, dup)
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err == nil {
		t.Error("pass over a conflicting sink returned no error")
	}
	if !strings.Contains(stderr.String(), "not reconciled") {
		t.Fatalf("second pass did not hit the conflict: %q", stderr.String())
	}
	if err := os.RemoveAll(dup); err != nil {
		t.Fatal(err)
	}
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)

	for _, want := range []string{`agent "mayor" skipped skill "m-only"`, `agent "deputy" skipped skill "d-only"`} {
		if got := strings.Count(stderr.String(), want); got != 1 {
			t.Errorf("%s reported %d times over three passes, want 1:\n%s", want, got, stderr.String())
		}
	}
}

// TestRunStage1SharedSinkRunErrorKeepsEveryAgentsSkips: when the
// materializer fails on a shared sink, the pass reports the failure once,
// under the sink rather than an agent, and carries the skip history of
// every agent in the sink, so neither agent's skip prints again once the
// sink materializes.
func TestRunStage1SharedSinkRunErrorKeepsEveryAgentsSkips(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "")
	cityPath := t.TempDir()
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	mayorSkills := filepath.Join(cityPath, "agents", "mayor", "skills")
	deputySkills := filepath.Join(cityPath, "agents", "deputy", "skills")
	writeSkillSource(t, filepath.Join(mayorSkills, "m-only"))
	writeSkillSource(t, filepath.Join(deputySkills, "d-only"))
	sink := filepath.Join(cityPath, ".claude", "skills")
	for _, name := range []string{"m-only", "d-only"} {
		if err := os.MkdirAll(filepath.Join(sink, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: mayorSkills},
			{Name: "deputy", Scope: "city", Provider: "claude", SkillsDir: deputySkills},
		},
	}

	var stderr bytes.Buffer
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)

	// A regular file where the sink's .claude directory belongs makes the
	// materializer fail to create the sink.
	claudeDir := filepath.Join(cityPath, ".claude")
	aside := filepath.Join(cityPath, "claude-aside")
	if err := os.Rename(claudeDir, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := stderr.Len()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err == nil || !strings.Contains(err.Error(), sink) {
		t.Errorf("failed pass returned %v, want an error naming %s", err, sink)
	}
	failed := stderr.String()[before:]
	want := "gc: stage-1 materialize-skills at " + sink + ` (agents "mayor", "deputy"): `
	if got := strings.Count(failed, want); got != 1 {
		t.Fatalf("failed pass reported %q %d times, want 1:\n%s", want, got, failed)
	}

	if err := os.Remove(claudeDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, claudeDir); err != nil {
		t.Fatal(err)
	}
	runStage1ForSkipTest(t, cityPath, cfg, &stderr)

	for _, want := range []string{`agent "mayor" skipped skill "m-only"`, `agent "deputy" skipped skill "d-only"`} {
		if got := strings.Count(stderr.String(), want); got != 1 {
			t.Errorf("%s reported %d times over three passes, want 1:\n%s", want, got, stderr.String())
		}
	}
}

// TestRunStage1SharedSinkCatalogLoadErrorKeepsLinks: when one agent's
// skill catalog cannot be read, the pass cannot tell that agent's links
// from stale ones, so it must leave the shared sink as it is rather than
// prune them, and report the load error once. The sink reconciles again
// once the catalog reads.
func TestRunStage1SharedSinkCatalogLoadErrorKeepsLinks(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	mayorDir := filepath.Join(cityPath, "agents", "mayor")
	mayorSkills := filepath.Join(mayorDir, "skills")
	deputySkills := filepath.Join(cityPath, "agents", "deputy", "skills")
	writeSkillSource(t, filepath.Join(mayorSkills, "m-only"))
	writeSkillSource(t, filepath.Join(deputySkills, "d-only"))
	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", SkillsDir: mayorSkills},
			{Name: "deputy", Scope: "city", Provider: "claude", SkillsDir: deputySkills},
		},
	}
	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(cityPath, ".claude", "skills")
	link := filepath.Join(sink, "m-only")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("precondition: m-only missing: %v", err)
	}

	// A regular file where mayor's agent directory belongs makes reading
	// mayor's skills directory fail with an error other than not-exist.
	aside := filepath.Join(cityPath, "mayor-aside")
	if err := os.Rename(mayorDir, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mayorDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err == nil || !strings.Contains(err.Error(), sink) {
		t.Errorf("pass with an unreadable catalog returned %v, want an error naming %s", err, sink)
	}
	if got := strings.Count(stderr.String(), "LoadAgentCatalog"); got != 1 {
		t.Errorf("load error reported %d times, want 1: %q", got, stderr.String())
	}
	for _, want := range []string{sink, `agent "mayor"`, "sink not reconciled this pass"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %q", want, stderr.String())
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("m-only pruned while mayor's catalog could not be read: %v", err)
	}

	// A shared skill added while the sink is held waits for the next pass
	// that can read every catalog, so its link proves that pass reconciled.
	writeSkillSource(t, filepath.Join(cityPath, "skills", "fresh"))
	stderr.Reset()
	_ = runStage1SkillMaterialization(cityPath, cfg, &stderr)
	if _, err := os.Lstat(filepath.Join(sink, "fresh")); err == nil {
		t.Errorf("fresh linked while mayor's catalog could not be read")
	}

	if err := os.Remove(mayorDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(aside, mayorDir); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plan", "m-only", "d-only", "fresh"} {
		if _, err := os.Lstat(filepath.Join(sink, name)); err != nil {
			t.Errorf("%s missing after mayor's catalog reads again: %v; stderr=%q", name, err, stderr.String())
		}
	}
}

// TestRunStage1AliasedRigPathsShareOneSink: rig be's path is a symlink
// to rig fe's, so polecat and witness write one physical sink through two
// spellings of its path, which does not exist before the first pass. The
// pass must treat it as one sink, or the later of two passes over it
// prunes the other agent's agent-local skill.
func TestRunStage1AliasedRigPathsShareOneSink(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	writeSkillSource(t, filepath.Join(cityPath, "skills", "plan"))
	polecatSkills := filepath.Join(cityPath, "agents", "polecat", "skills")
	witnessSkills := filepath.Join(cityPath, "agents", "witness", "skills")
	writeSkillSource(t, filepath.Join(polecatSkills, "p-only"))
	writeSkillSource(t, filepath.Join(witnessSkills, "w-only"))
	realRig := filepath.Join(cityPath, "rigs", "real")
	aliasRig := filepath.Join(cityPath, "rigs", "alias")
	if err := os.MkdirAll(realRig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRig, aliasRig); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Rigs:          []config.Rig{{Name: "fe", Path: realRig}, {Name: "be", Path: aliasRig}},
		Agents: []config.Agent{
			{Name: "polecat", Scope: "rig", Dir: "fe", Provider: "claude", SkillsDir: polecatSkills},
			{Name: "witness", Scope: "rig", Dir: "be", Provider: "claude", SkillsDir: witnessSkills},
		},
	}

	sink := filepath.Join(realRig, ".claude", "skills")
	for pass := 1; pass <= 2; pass++ {
		var stderr bytes.Buffer
		if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"plan", "p-only", "w-only"} {
			if _, err := os.Lstat(filepath.Join(sink, name)); err != nil {
				t.Errorf("pass %d: %s missing from the sink both rigs alias: %v; stderr=%q", pass, name, err, stderr.String())
			}
		}
	}
}

// TestRunStage1AliasedCatalogPathsAreNotAConflict: the two rigs' imports
// reach ops.review through two spellings of one directory, one of them a
// symlink. One physical source is not a conflict, so the sink reconciles
// and links it.
func TestRunStage1AliasedCatalogPathsAreNotAConflict(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	ops := filepath.Join(cityPath, "imports", "ops", "skills")
	writeSkillSource(t, filepath.Join(ops, "review"))
	opsAlias := filepath.Join(cityPath, "imports", "ops-alias")
	if err := os.Symlink(ops, opsAlias); err != nil {
		t.Fatal(err)
	}

	cfg := sharedPathRigsCity(cityPath, ops, opsAlias)
	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "not reconciled") {
		t.Errorf("one source reached through two paths reported as a conflict: %q", stderr.String())
	}
	link := filepath.Join(cityPath, "rigs", "shared", ".claude", "skills", "ops.review")
	if _, err := os.Stat(link); err != nil {
		t.Errorf("ops.review not linked: %v; stderr=%q", err, stderr.String())
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
