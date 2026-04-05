package main

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestMergeEnvEmptyMaps(t *testing.T) {
	got := mergeEnv(map[string]string{}, map[string]string{})
	if got != nil {
		t.Errorf("mergeEnv(empty, empty) = %v, want nil", got)
	}
}

func TestMergeEnvNilAndValues(t *testing.T) {
	got := mergeEnv(nil, map[string]string{"A": "1"})
	if got["A"] != "1" {
		t.Errorf("mergeEnv[A] = %q, want %q", got["A"], "1")
	}
}

func TestPassthroughEnvIncludesPath(t *testing.T) {
	// PATH is always set in a normal environment.
	got := passthroughEnv()
	if _, ok := got["PATH"]; !ok {
		t.Error("passthroughEnv() missing PATH")
	}
}

func TestPassthroughEnvPicksUpGCBeads(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	got := passthroughEnv()
	if got["GC_BEADS"] != "file" {
		t.Errorf("passthroughEnv()[GC_BEADS] = %q, want %q", got["GC_BEADS"], "file")
	}
}

func TestPassthroughEnvOmitsUnset(t *testing.T) {
	t.Setenv("GC_DOLT", "")
	got := passthroughEnv()
	if _, ok := got["GC_DOLT"]; ok {
		t.Error("passthroughEnv() should omit empty GC_DOLT")
	}
}

func TestComputePoolSessions_NamepoolMaxOneUsesPoolInstance(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{},
		Agents: []config.Agent{
			{
				Name:              "polecat",
				Dir:               "repo",
				MaxActiveSessions: intPtr(1),
				Namepool:          "namepools/mad-max.txt",
				NamepoolNames:     []string{"furiosa"},
			},
		},
	}

	got := computePoolSessions(cfg, "city", "", runtime.NewFake())
	want := startupSessionName("city", "repo/furiosa", cfg.Workspace.SessionTemplate)
	if _, ok := got[want]; !ok {
		t.Fatalf("computePoolSessions missing %q in %v", want, got)
	}
	if len(got) != 1 {
		t.Fatalf("computePoolSessions len = %d, want 1 (%v)", len(got), got)
	}
}

func TestStandaloneBuildAgentsFnWithSessionBeads_UsesRigStoresForAssignedWork(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	handoff, err := rigStore.Create(beads.Bead{
		Title:    "merge me",
		Type:     "task",
		Status:   "open",
		Assignee: "repo/refinery",
	})
	if err != nil {
		t.Fatalf("rigStore.Create: %v", err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "repo", Path: t.TempDir()},
		},
	}

	buildFn := standaloneBuildAgentsFnWithSessionBeads("city", "/tmp/city", time.Now().UTC(), io.Discard)
	result := buildFn(cfg, runtime.NewFake(), cityStore, map[string]beads.Store{"repo": rigStore}, nil, nil)
	if len(result.AssignedWorkBeads) != 1 {
		t.Fatalf("AssignedWorkBeads len = %d, want 1 (%#v)", len(result.AssignedWorkBeads), result.AssignedWorkBeads)
	}
	if result.AssignedWorkBeads[0].ID != handoff.ID {
		t.Fatalf("AssignedWorkBeads[0].ID = %q, want %q", result.AssignedWorkBeads[0].ID, handoff.ID)
	}
}

func TestMergeEnvOverrideOrder(t *testing.T) {
	a := map[string]string{"KEY": "first", "A": "a"}
	b := map[string]string{"KEY": "second", "B": "b"}
	got := mergeEnv(a, b)
	if got["KEY"] != "second" {
		t.Errorf("mergeEnv override: KEY = %q, want %q", got["KEY"], "second")
	}
	if got["A"] != "a" {
		t.Errorf("mergeEnv: A = %q, want %q", got["A"], "a")
	}
	if got["B"] != "b" {
		t.Errorf("mergeEnv: B = %q, want %q", got["B"], "b")
	}
}

func TestMergeEnvAllNil(t *testing.T) {
	got := mergeEnv(nil, nil, nil)
	if got != nil {
		t.Errorf("mergeEnv(nil, nil, nil) = %v, want nil", got)
	}
}

func TestPassthroughEnvDoltConnectionVars(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "dolt.gc.svc.cluster.local")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")

	got := passthroughEnv()

	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		if _, ok := got[key]; !ok {
			t.Errorf("passthroughEnv() missing %s", key)
		}
	}
	if got["GC_DOLT_HOST"] != "dolt.gc.svc.cluster.local" {
		t.Errorf("GC_DOLT_HOST = %q, want %q", got["GC_DOLT_HOST"], "dolt.gc.svc.cluster.local")
	}
	if got["GC_DOLT_PORT"] != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "3307")
	}
}

func TestPassthroughEnvOmitsUnsetDoltVars(t *testing.T) {
	// Ensure the vars are NOT set.
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		t.Setenv(key, "")
	}

	got := passthroughEnv()

	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		if _, ok := got[key]; ok {
			t.Errorf("passthroughEnv() should omit empty %s", key)
		}
	}
}

func TestPassthroughEnvIncludesClaudeAuthContext(t *testing.T) {
	t.Setenv("HOME", "/tmp/gc-home")
	t.Setenv("USER", "gcuser")
	t.Setenv("LOGNAME", "gcuser")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/gc-home/.config")
	t.Setenv("XDG_STATE_HOME", "/tmp/gc-home/.local/state")
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/gc-home/.claude")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-123")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "anth-auth-token")

	got := passthroughEnv()

	for key, want := range map[string]string{
		"HOME":                    "/tmp/gc-home",
		"USER":                    "gcuser",
		"LOGNAME":                 "gcuser",
		"XDG_CONFIG_HOME":         "/tmp/gc-home/.config",
		"XDG_STATE_HOME":          "/tmp/gc-home/.local/state",
		"CLAUDE_CONFIG_DIR":       "/tmp/gc-home/.claude",
		"CLAUDE_CODE_OAUTH_TOKEN": "oauth-token",
		"ANTHROPIC_API_KEY":       "sk-ant-123",
		"ANTHROPIC_AUTH_TOKEN":    "anth-auth-token",
	} {
		if got[key] != want {
			t.Errorf("passthroughEnv()[%s] = %q, want %q", key, got[key], want)
		}
	}
}

func TestPassthroughEnvStripsClaudeNesting(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")

	got := passthroughEnv()

	// Should be present but empty so tmux -e overrides the inherited server env.
	if v, ok := got["CLAUDECODE"]; !ok || v != "" {
		t.Errorf("CLAUDECODE = %q (present=%v), want empty string present", v, ok)
	}
	if v, ok := got["CLAUDE_CODE_ENTRYPOINT"]; !ok || v != "" {
		t.Errorf("CLAUDE_CODE_ENTRYPOINT = %q (present=%v), want empty string present", v, ok)
	}
}

func TestPassthroughEnvClearsClaudeNestingUnconditionally(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")

	got := passthroughEnv()

	// passthroughEnv always sets these to "" unconditionally so the
	// fingerprint is stable regardless of whether the supervisor or
	// a user shell created the session bead.
	if v, ok := got["CLAUDECODE"]; !ok || v != "" {
		t.Errorf("CLAUDECODE should be present and empty, got ok=%v v=%q", ok, v)
	}
	if v, ok := got["CLAUDE_CODE_ENTRYPOINT"]; !ok || v != "" {
		t.Errorf("CLAUDE_CODE_ENTRYPOINT should be present and empty, got ok=%v v=%q", ok, v)
	}
}

func TestStageHookFilesIncludesCodexAndCopilotExecutableHooks(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	workDir := filepath.Join(cityDir, "worker")
	hookRels := []string{
		path.Join(".codex", "hooks.json"),
		path.Join(".github", "hooks", "gascity.json"),
		path.Join(".github", "copilot-instructions.md"),
	}
	for _, rel := range hookRels {
		p := filepath.Join(workDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", p, err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", p, err)
		}
	}

	got := stageHookFiles(nil, cityDir, workDir)
	rels := make(map[string]bool, len(got))
	for _, entry := range got {
		rels[entry.RelDst] = true
	}
	// RelDst must include the relative workDir prefix so K8s staging
	// places files under the agent's container WorkingDir, not at /workspace/.
	for _, rel := range hookRels {
		want := path.Join("worker", rel)
		if !rels[want] {
			t.Errorf("stageHookFiles() missing %q (got %v)", want, rels)
		}
	}
}

func TestStageHookFilesIncludesCanonicalClaudeHook(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	workDir := filepath.Join(cityDir, "worker")
	settingsPath := filepath.Join(cityDir, ".gc", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", settingsPath, err)
	}
	if err := os.WriteFile(settingsPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", settingsPath, err)
	}

	got := stageHookFiles(nil, cityDir, workDir)
	for _, entry := range got {
		// City-root-relative hook: no workDir prefix in RelDst.
		if entry.RelDst == path.Join(".gc", "settings.json") {
			if entry.Src != settingsPath {
				t.Fatalf("stageHookFiles() staged %q, want %q", entry.Src, settingsPath)
			}
			return
		}
	}
	t.Fatal("stageHookFiles() did not stage .gc/settings.json")
}

func TestStageHookFilesFallsBackToLegacyClaudeHook(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	workDir := filepath.Join(cityDir, "worker")
	hookPath := filepath.Join(cityDir, "hooks", "claude.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", hookPath, err)
	}
	if err := os.WriteFile(hookPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", hookPath, err)
	}

	got := stageHookFiles(nil, cityDir, workDir)
	for _, entry := range got {
		if entry.RelDst == path.Join("hooks", "claude.json") {
			if entry.Src != hookPath {
				t.Fatalf("stageHookFiles() staged %q, want %q", entry.Src, hookPath)
			}
			return
		}
	}
	t.Fatal("stageHookFiles() did not stage hooks/claude.json")
}

func TestConfiguredRigNameMatchesRigByPathWithoutCreatingDirs(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	agentDir := filepath.Join("repos", "demo")
	agent := &config.Agent{Name: "witness", Dir: agentDir}
	rigs := []config.Rig{{Name: "demo", Path: rigRoot}}

	got := configuredRigName(cityPath, agent, rigs)
	if got != "demo" {
		t.Fatalf("configuredRigName() = %q, want demo", got)
	}
	if _, err := os.Stat(filepath.Join(cityPath, agentDir)); !os.IsNotExist(err) {
		t.Fatalf("configuredRigName() created %q as a side effect", filepath.Join(cityPath, agentDir))
	}
}

func TestConfiguredRigNameUnmatchedPathReturnsEmpty(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "witness", Dir: filepath.Join("repos", "other")}
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}

	if got := configuredRigName(cityPath, agent, rigs); got != "" {
		t.Fatalf("configuredRigName() = %q, want empty", got)
	}
}
