package main

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/bootstrap"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
)

func bootstrapPackNameForTest(t *testing.T) string {
	t.Helper()
	const name = "bootstrap-pack"
	prev := append([]bootstrap.Entry(nil), bootstrap.BootstrapPacks...)
	bootstrap.BootstrapPacks = []bootstrap.Entry{{
		Name:   name,
		Source: "github.com/example/" + name,
	}}
	t.Cleanup(func() { bootstrap.BootstrapPacks = prev })
	return name
}

func globalRepoCachePathForTest(gcHome, source, commit string) string {
	return config.GlobalRepoCachePath(gcHome, source, commit)
}

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

func TestReleaseOrphanedPoolAssignmentsWhenSnapshotsComplete_PartialSkipsCompleteReleases(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		ID:       "ga-live",
		Title:    "live assigned work from partial snapshot",
		Type:     "task",
		Assignee: "worker-session",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work.Status = inProgress

	released := releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		"",
		nil,
		DesiredStateResult{
			AssignedWorkBeads:  []beads.Bead{work},
			AssignedWorkStores: []beads.Store{store},
			StoreQueryPartial:  true,
		},
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released %d work bead(s) from a partial snapshot, want none", len(released))
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work after partial one-shot release: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-session" {
		t.Fatalf("partial one-shot snapshot released work: status=%q assignee=%q", got.Status, got.Assignee)
	}

	released = releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		"",
		nil,
		DesiredStateResult{
			AssignedWorkBeads:   []beads.Bead{work},
			AssignedWorkStores:  []beads.Store{store},
			SessionQueryPartial: true,
		},
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released %d work bead(s) from a partial session snapshot, want none", len(released))
	}
	got, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work after partial session snapshot release: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-session" {
		t.Fatalf("partial session snapshot released work: status=%q assignee=%q", got.Status, got.Assignee)
	}

	released = releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		"",
		nil,
		DesiredStateResult{
			AssignedWorkBeads:  []beads.Bead{work},
			AssignedWorkStores: []beads.Store{store},
		},
		nil,
	)
	if len(released) != 1 {
		t.Fatalf("complete one-shot snapshot released %d work bead(s), want 1", len(released))
	}
	got, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work after complete one-shot release: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("complete one-shot snapshot did not release orphaned work: status=%q assignee=%q", got.Status, got.Assignee)
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

func TestPassthroughEnvIncludesProviderCredentialEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-123")
	t.Setenv("OPENAI_API_KEY", "sk-openai-123")
	t.Setenv("OPENAI_BASE_URL", "https://openai.example.test")
	t.Setenv("GEMINI_API_KEY", "gemini-123")
	t.Setenv("GOOGLE_API_KEY", "google-123")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/google-credentials.json")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "gc-project")

	got := passthroughEnv()

	for key, want := range map[string]string{
		"ANTHROPIC_API_KEY":              "sk-ant-123",
		"OPENAI_API_KEY":                 "sk-openai-123",
		"OPENAI_BASE_URL":                "https://openai.example.test",
		"GEMINI_API_KEY":                 "gemini-123",
		"GOOGLE_API_KEY":                 "google-123",
		"GOOGLE_APPLICATION_CREDENTIALS": "/tmp/google-credentials.json",
		"GOOGLE_CLOUD_PROJECT":           "gc-project",
	} {
		if got[key] != want {
			t.Errorf("passthroughEnv()[%s] = %q, want %q", key, got[key], want)
		}
	}
}

func TestPassthroughEnvXDGFallbackFromHOME(t *testing.T) {
	t.Setenv("HOME", "/tmp/gc-home")
	// Explicitly unset XDG vars so fallback logic fires.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	got := passthroughEnv()

	if got["XDG_CONFIG_HOME"] != "/tmp/gc-home/.config" {
		t.Errorf("XDG_CONFIG_HOME = %q, want %q (fallback from HOME)", got["XDG_CONFIG_HOME"], "/tmp/gc-home/.config")
	}
	if got["XDG_STATE_HOME"] != "/tmp/gc-home/.local/state" {
		t.Errorf("XDG_STATE_HOME = %q, want %q (fallback from HOME)", got["XDG_STATE_HOME"], "/tmp/gc-home/.local/state")
	}
}

func TestPassthroughEnvOmitsEmptyAnthropicVars(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	got := passthroughEnv()

	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if _, ok := got[key]; ok {
			t.Errorf("passthroughEnv() should omit empty %s", key)
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

func TestPassthroughEnvLANGFallback(t *testing.T) {
	// When no locale is set (e.g. launchd supervisor), fall back to
	// en_US.UTF-8 so TUI tools render UTF-8 glyphs correctly in managed
	// sessions. Empty LC_* entries clear stale higher-precedence tmux env.
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")

	got := passthroughEnv()

	if got["LANG"] != "en_US.UTF-8" {
		t.Errorf("LANG = %q, want %q (fallback for launchd)", got["LANG"], "en_US.UTF-8")
	}
	if got["LC_ALL"] != "" {
		t.Errorf("LC_ALL = %q, want empty string to clear stale tmux env", got["LC_ALL"])
	}
	if got["LC_CTYPE"] != "" {
		t.Errorf("LC_CTYPE = %q, want empty string to clear stale tmux env", got["LC_CTYPE"])
	}
}

func TestPassthroughEnvLANGPassthrough(t *testing.T) {
	// When LANG is set, pass it through as-is.
	t.Setenv("LANG", "ja_JP.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")

	got := passthroughEnv()

	if got["LANG"] != "ja_JP.UTF-8" {
		t.Errorf("LANG = %q, want %q", got["LANG"], "ja_JP.UTF-8")
	}
	if got["LC_ALL"] != "" {
		t.Errorf("LC_ALL = %q, want empty string to clear stale tmux env", got["LC_ALL"])
	}
	if got["LC_CTYPE"] != "" {
		t.Errorf("LC_CTYPE = %q, want empty string to clear stale tmux env", got["LC_CTYPE"])
	}
}

func TestPassthroughEnvLocalePassthrough(t *testing.T) {
	t.Setenv("LANG", "en_GB.UTF-8")
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LC_CTYPE", "ja_JP.UTF-8")

	got := passthroughEnv()

	for key, want := range map[string]string{
		"LANG":     "en_GB.UTF-8",
		"LC_ALL":   "fr_FR.UTF-8",
		"LC_CTYPE": "ja_JP.UTF-8",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestPassthroughEnvLCTypeSuppressesLANGFallback(t *testing.T) {
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "ja_JP.UTF-8")

	got := passthroughEnv()

	if _, ok := got["LANG"]; ok {
		t.Errorf("LANG present as %q, want omitted when LC_CTYPE provides locale", got["LANG"])
	}
	if got["LC_ALL"] != "" {
		t.Errorf("LC_ALL = %q, want empty string to clear stale tmux env", got["LC_ALL"])
	}
	if got["LC_CTYPE"] != "ja_JP.UTF-8" {
		t.Errorf("LC_CTYPE = %q, want %q", got["LC_CTYPE"], "ja_JP.UTF-8")
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
			if !entry.Probed {
				t.Fatal("stageHookFiles() .gc/settings.json not marked Probed")
			}
			if entry.ContentHash == "" {
				t.Fatal("stageHookFiles() .gc/settings.json has empty ContentHash")
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
			if !entry.Probed {
				t.Fatal("stageHookFiles() hooks/claude.json not marked Probed")
			}
			if entry.ContentHash == "" {
				t.Fatal("stageHookFiles() hooks/claude.json has empty ContentHash")
			}
			return
		}
	}
	t.Fatal("stageHookFiles() did not stage hooks/claude.json")
}

func TestStageHookFilesDoesNotStageClaudeSkillsDir(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	workDir := filepath.Join(cityDir, "worker")
	skillsDir := filepath.Join(workDir, ".claude", "skills", "plan")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", skillsDir, err)
	}
	skillPath := filepath.Join(skillsDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: plan\n---\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", skillPath, err)
	}

	got := stageHookFiles(nil, cityDir, workDir)
	wantRelDst := path.Join("worker", ".claude", "skills")
	for _, entry := range got {
		if entry.RelDst == wantRelDst {
			t.Fatalf("stageHookFiles() staged %q at %q; want skills drift tracked via FingerprintExtra only", entry.Src, entry.RelDst)
		}
	}
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

// TestBuildFingerprintExtra_StableAcrossBaseAndInstance is a regression test
// for the config-drift oscillation that was reaping live pool and named
// sessions "minutes into work". Different code paths in buildDesiredState
// resolve the same session bead with either the BASE agent
// (cfgAgent, QualifiedName = "rig/pool") or a deepCopied INSTANCE agent
// (QualifiedName = "rig/pool-1"). Those two shapes must produce the same
// FingerprintExtra or the reconciler's CoreFingerprint flips every tick
// and drains every live pool session with close_reason=stale-session.
//
// The fix drops pool.check from FingerprintExtra — it's a runtime probe for
// demand, not a behavioral-identity field, and it was the only piece that
// carried the agent's QualifiedName into the fingerprint. pool.min,
// pool.max, depends_on, wake_mode remain.
func TestBuildFingerprintExtra_StableAcrossBaseAndInstance(t *testing.T) {
	baseAgent := &config.Agent{
		Name:              "opus",
		Dir:               "gascity",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: nil, // unlimited
	}
	instanceAgent := deepCopyAgent(baseAgent, "opus-1", "gascity")

	baseExtra := buildFingerprintExtra(baseAgent)
	instExtra := buildFingerprintExtra(&instanceAgent)

	if len(baseExtra) != len(instExtra) {
		t.Fatalf("buildFingerprintExtra size differs base=%d instance=%d (base=%v instance=%v)",
			len(baseExtra), len(instExtra), baseExtra, instExtra)
	}
	for k, bv := range baseExtra {
		iv, ok := instExtra[k]
		if !ok {
			t.Fatalf("instance fpExtra missing key %q (base=%q)", k, bv)
		}
		if bv != iv {
			t.Fatalf("fpExtra[%q] differs: base=%q instance=%q — this drives the reconciler's CoreFingerprint to oscillate between ticks and drains every live pool/named session", k, bv, iv)
		}
	}
	// pool.check must NOT be present — it bakes QualifiedName which differs
	// between base and instance agents and is the drift source.
	if _, has := baseExtra["pool.check"]; has {
		t.Fatalf("buildFingerprintExtra must not include pool.check (it bakes QualifiedName and differs across base/instance forms): %v", baseExtra)
	}
}

// TestResolveTemplateFPExtra_StableAcrossBaseAndInstance asserts the FULL
// FPExtra (including skill entries merged inside resolveTemplate) matches
// byte-for-byte between the base agent and its deepCopied instance. This
// covers the drift pattern where two buildDesiredState code paths produce
// different tp.FPExtra for the same logical session bead, causing the
// reconciler's CoreFingerprint to oscillate and drain live sessions. The
// plain buildFingerprintExtra test above catches the pool/wake_mode half;
// this one catches the skills-merge half.
func TestResolveTemplateFPExtra_StableAcrossBaseAndInstance(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"fp-test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sharedCat, err := materialize.LoadCityCatalog(filepath.Join(cityPath, "skills"))
	if err != nil {
		t.Fatal(err)
	}

	makeParams := func() *agentBuildParams {
		return &agentBuildParams{
			cityName:  "city",
			cityPath:  cityPath,
			workspace: &config.Workspace{Provider: "claude"},
			providers: map[string]config.ProviderSpec{
				"claude": {Command: "echo", PromptMode: "none", SupportsACP: boolPtr(true)},
			},
			lookPath:        func(string) (string, error) { return "/bin/echo", nil },
			fs:              fsys.OSFS{},
			rigs:            []config.Rig{},
			beaconTime:      time.Unix(0, 0),
			beadNames:       make(map[string]string),
			stderr:          io.Discard,
			skillCatalog:    &sharedCat,
			sessionProvider: "tmux",
		}
	}

	baseAgent := &config.Agent{
		Name:              "claude",
		Scope:             "city",
		Provider:          "claude",
		MaxActiveSessions: intPtr(1),
		WakeMode:          "fresh",
	}
	instanceAgent := deepCopyAgent(baseAgent, "claude-1", baseAgent.Dir)

	tpBase, err := resolveTemplate(makeParams(), baseAgent, baseAgent.QualifiedName(), buildFingerprintExtra(baseAgent))
	if err != nil {
		t.Fatalf("resolveTemplate(base): %v", err)
	}
	tpInst, err := resolveTemplate(makeParams(), &instanceAgent, instanceAgent.QualifiedName(), buildFingerprintExtra(&instanceAgent))
	if err != nil {
		t.Fatalf("resolveTemplate(instance): %v", err)
	}

	if len(tpBase.FPExtra) != len(tpInst.FPExtra) {
		t.Fatalf("FPExtra size differs base=%d instance=%d (base=%v instance=%v)",
			len(tpBase.FPExtra), len(tpInst.FPExtra), tpBase.FPExtra, tpInst.FPExtra)
	}
	for k, bv := range tpBase.FPExtra {
		iv, ok := tpInst.FPExtra[k]
		if !ok {
			t.Fatalf("instance FPExtra missing key %q (base=%q)", k, bv)
		}
		if bv != iv {
			t.Fatalf("FPExtra[%q] differs: base=%q instance=%q — reconciler CoreFingerprint will oscillate and drain live sessions", k, bv, iv)
		}
	}
}

// TestAgentBuildParams_FPExtraStableAcrossCatalogTransients is an
// integration test that reproduces the observed "FPExtra: map[] (len=0)"
// drift end-to-end: tick N loads the skill catalog successfully → a
// session is started with `skills:*` entries in FPExtra → tick N+1's
// catalog discovery fails from a transient filesystem error → without
// the cache, FPExtra drops skills and the CoreFingerprint flips. Asserts
// that newAgentBuildParams' last-good cache keeps params.skillCatalog
// populated so resolveTemplate produces a byte-identical FPExtra on both
// ticks.
func TestAgentBuildParams_FPExtraStableAcrossCatalogTransients(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"fp-test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillsRoot := filepath.Join(cityPath, "skills")
	skillDir := filepath.Join(skillsRoot, "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyImportRoot := filepath.Join(cityPath, "empty-import")
	emptyImportLink := filepath.Join(cityPath, "empty-import-link")
	if err := os.MkdirAll(emptyImportRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, emptyImportRoot, emptyImportLink)

	cfgGood := &config.City{
		Workspace:     config.Workspace{Provider: "claude"},
		PackSkillsDir: skillsRoot,
		PackSkills: []config.DiscoveredSkillCatalog{{
			SourceDir:   emptyImportLink,
			BindingName: "transient",
			PackName:    "helper",
		}},
		Providers: map[string]config.ProviderSpec{
			"claude": {Command: "echo", PromptMode: "none", SupportsACP: boolPtr(true)},
		},
		Session: config.SessionConfig{Provider: "tmux"},
	}

	agent := &config.Agent{
		Name:              "claude",
		Dir:               "gascity",
		Scope:             "rig",
		Provider:          "claude",
		MaxActiveSessions: intPtr(6),
	}

	// Tick N: catalog loads fully.
	bpGood := newAgentBuildParams("city", cityPath, cfgGood, nil, time.Unix(0, 0), nil, io.Discard)
	bpGood.lookPath = func(string) (string, error) { return "/bin/echo", nil }
	tpN, err := resolveTemplate(bpGood, agent, agent.QualifiedName(), buildFingerprintExtra(agent))
	if err != nil {
		t.Fatalf("tickN resolveTemplate: %v", err)
	}
	if _, has := tpN.FPExtra["skills:plan"]; !has {
		t.Fatalf("tickN FPExtra missing skills:plan (%+v)", tpN.FPExtra)
	}

	// Tick N+1: catalog discovery fails from a transient filesystem error.
	// The cache must kick in.
	replaceWithSelfSymlink(t, emptyImportLink)
	bpDegraded := newAgentBuildParams("city", cityPath, cfgGood, nil, time.Unix(0, 0), nil, io.Discard)
	bpDegraded.lookPath = func(string) (string, error) { return "/bin/echo", nil }
	tpN1, err := resolveTemplate(bpDegraded, agent, agent.QualifiedName(), buildFingerprintExtra(agent))
	if err != nil {
		t.Fatalf("tickN+1 resolveTemplate: %v", err)
	}
	if len(tpN.FPExtra) != len(tpN1.FPExtra) {
		t.Fatalf("FPExtra size differs across catalog-transient ticks: tickN=%d tickN+1=%d (tickN=%v tickN+1=%v) — config-drift drain-storm reproducer",
			len(tpN.FPExtra), len(tpN1.FPExtra), tpN.FPExtra, tpN1.FPExtra)
	}
	for k, bv := range tpN.FPExtra {
		iv, ok := tpN1.FPExtra[k]
		if !ok {
			t.Errorf("FPExtra key %q present on tickN but dropped on tickN+1 (base=%q)", k, bv)
			continue
		}
		if bv != iv {
			t.Errorf("FPExtra[%q] differs across ticks: tickN=%q tickN+1=%q", k, bv, iv)
		}
	}
}

// TestNewAgentBuildParams_CachesLastGoodCatalog verifies that a
// transient LoadCityCatalog failure reuses the most recently cached
// catalog so FingerprintExtra stays stable. The production drift was
// reproduced as: tick N loads catalog successfully → session starts
// with skills:* entries → tick N+1 load fails → skillCatalog=nil →
// FPExtra drops skills → CoreFingerprint flips → every live session
// drains in config-drift. The fix is a process-level last-good cache.
func TestNewAgentBuildParams_CachesLastGoodCatalog(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"fp-test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillsRoot := filepath.Join(cityPath, "skills")
	skillDir := filepath.Join(skillsRoot, "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyImportRoot := filepath.Join(cityPath, "empty-import")
	emptyImportLink := filepath.Join(cityPath, "empty-import-link")
	if err := os.MkdirAll(emptyImportRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, emptyImportRoot, emptyImportLink)

	cfgGood := &config.City{
		PackSkillsDir: skillsRoot,
		PackSkills: []config.DiscoveredSkillCatalog{{
			SourceDir:   emptyImportLink,
			BindingName: "transient",
			PackName:    "helper",
		}},
	}
	// First call: real load succeeds and caches the catalog.
	bpGood := newAgentBuildParams("city", cityPath, cfgGood, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGood.skillCatalog == nil {
		t.Fatalf("baseline: skillCatalog is nil despite successful load")
	}
	baselineEntries := len(bpGood.skillCatalog.Entries)
	if baselineEntries == 0 {
		t.Fatalf("baseline: expected >=1 skill entry, got 0 (catalog=%+v)", bpGood.skillCatalog)
	}

	// Second call: the same catalog root now fails to stat, simulating a
	// transient filesystem error. The cache must kick in and restore the
	// catalog so FingerprintExtra stays byte-identical across ticks.
	replaceWithSelfSymlink(t, emptyImportLink)
	bpDegraded := newAgentBuildParams("city", cityPath, cfgGood, nil, time.Unix(0, 0), nil, io.Discard)
	if bpDegraded.skillCatalog == nil {
		t.Fatalf("cache miss: skillCatalog is nil after LoadCityCatalog failure — the last-good catalog cache is not kicking in; this is the config-drift drain-storm reproducer")
	}
	if got := len(bpDegraded.skillCatalog.Entries); got != baselineEntries {
		t.Errorf("cache mismatch: degraded-tick catalog has %d entries, want %d (baseline)", got, baselineEntries)
	}
}

func TestNewAgentBuildParams_SharedCatalogErrorReusesLastGoodCatalogAcrossRepeatedFailures(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	importRoot := filepath.Join(cityPath, "imports", "helper")
	importSkills := filepath.Join(importRoot, "skills")
	importLink := filepath.Join(cityPath, "imports", "helper-link")
	writeCatalogFile(t, importSkills, "plan/SKILL.md", "helper skill")
	symlinkOrSkip(t, importSkills, importLink)

	cfg := &config.City{
		PackSkills: []config.DiscoveredSkillCatalog{{
			SourceDir:   importLink,
			BindingName: "helper",
			PackName:    "helper",
		}},
	}
	bpGood := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGood.skillCatalog == nil || len(bpGood.skillCatalog.Entries) == 0 {
		t.Fatalf("baseline: expected non-empty imported catalog, got %+v", bpGood.skillCatalog)
	}

	replaceWithSelfSymlink(t, importLink)
	bpGrace := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGrace.skillCatalog == nil || len(bpGrace.skillCatalog.Entries) == 0 {
		t.Fatalf("first repeated root failure should reuse cached catalog, got %+v", bpGrace.skillCatalog)
	}

	bpRepeated := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpRepeated.skillCatalog == nil || len(bpRepeated.skillCatalog.Entries) == 0 {
		t.Fatalf("second repeated root failure should still reuse cached catalog, got %+v", bpRepeated.skillCatalog)
	}
}

func TestNewAgentBuildParams_EmptyCatalogClearsLastGoodCatalog(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{PackSkillsDir: filepath.Join(cityPath, "skills")}
	bpGood := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGood.skillCatalog == nil || len(bpGood.skillCatalog.Entries) == 0 {
		t.Fatalf("baseline: expected non-empty catalog, got %+v", bpGood.skillCatalog)
	}

	if err := os.RemoveAll(skillDir); err != nil {
		t.Fatal(err)
	}
	bpEmpty := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpEmpty.skillCatalog == nil {
		t.Fatal("empty successful catalog should be represented as an empty catalog, not nil")
	}
	if got := len(bpEmpty.skillCatalog.Entries); got != 0 {
		t.Fatalf("empty successful catalog reused stale cache with %d entries", got)
	}
}

func TestNewAgentBuildParams_EmptyBootstrapCatalogReusesLastGoodCatalogOnceThenClears(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	bootstrapName := bootstrapPackNameForTest(t)
	source := "github.com/example/" + bootstrapName
	commit := bootstrapName + "-commit"
	cacheDir := globalRepoCachePathForTest(gcHome, source, commit)
	writeCatalogFile(t, cacheDir, "skills/"+bootstrapName+"-sample/SKILL.md", "bootstrap skill")
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	implicit := "schema = 1\n\n[imports.\"" + bootstrapName + "\"]\nsource = \"" + source + "\"\nversion = \"0.1.0\"\ncommit = \"" + commit + "\"\n"
	if err := os.WriteFile(implicitPath, []byte(implicit), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	bpGood := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGood.skillCatalog == nil || len(bpGood.skillCatalog.Entries) == 0 {
		t.Fatalf("baseline: expected non-empty bootstrap-backed catalog, got %+v", bpGood.skillCatalog)
	}

	if err := os.RemoveAll(filepath.Join(cacheDir, "skills")); err != nil {
		t.Fatal(err)
	}
	bpEmpty := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpEmpty.skillCatalog == nil {
		t.Fatal("missing bootstrap skills dir should reuse cached catalog, got nil")
	}
	if got := len(bpEmpty.skillCatalog.Entries); got != len(bpGood.skillCatalog.Entries) {
		t.Fatalf("bootstrap empty-success should reuse cached catalog: got %d entries want %d", got, len(bpGood.skillCatalog.Entries))
	}

	bpCleared := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpCleared.skillCatalog == nil {
		t.Fatal("second bootstrap empty-success should clear to an empty catalog, not nil")
	}
	if got := len(bpCleared.skillCatalog.Entries); got != 0 {
		t.Fatalf("second bootstrap empty-success should clear stale cache: got %d entries", got)
	}
}

func TestNewAgentBuildParams_ImplicitImportReadFailureReusesLastGoodCatalog(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	bootstrapName := bootstrapPackNameForTest(t)
	source := "github.com/example/" + bootstrapName
	commit := bootstrapName + "-commit"
	cacheDir := globalRepoCachePathForTest(gcHome, source, commit)
	writeCatalogFile(t, cacheDir, "skills/"+bootstrapName+"-sample/SKILL.md", "bootstrap skill")
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	implicit := "schema = 1\n\n[imports.\"" + bootstrapName + "\"]\nsource = \"" + source + "\"\nversion = \"0.1.0\"\ncommit = \"" + commit + "\"\n"
	if err := os.WriteFile(implicitPath, []byte(implicit), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	bpGood := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGood.skillCatalog == nil || len(bpGood.skillCatalog.Entries) == 0 {
		t.Fatalf("baseline: expected non-empty bootstrap-backed catalog, got %+v", bpGood.skillCatalog)
	}

	if err := os.Remove(implicitPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(implicitPath, 0o755); err != nil {
		t.Fatal(err)
	}

	bpError := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpError.skillCatalog == nil {
		t.Fatal("implicit-import read failure should reuse cached catalog, got nil")
	}
	if got := len(bpError.skillCatalog.Entries); got != len(bpGood.skillCatalog.Entries) {
		t.Fatalf("implicit-import read failure should reuse cached catalog: got %d entries want %d", got, len(bpGood.skillCatalog.Entries))
	}
}

func TestNewAgentBuildParams_BootstrapCommitChangeReusesCacheOnceThenClears(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	bootstrapName := bootstrapPackNameForTest(t)
	source := "github.com/example/" + bootstrapName
	commitA := bootstrapName + "-commit-a"
	cacheDirA := globalRepoCachePathForTest(gcHome, source, commitA)
	writeCatalogFile(t, cacheDirA, "skills/"+bootstrapName+"-sample/SKILL.md", "bootstrap skill")
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	writeImplicit := func(commit string) {
		t.Helper()
		implicit := "schema = 1\n\n[imports.\"" + bootstrapName + "\"]\nsource = \"" + source + "\"\nversion = \"0.1.0\"\ncommit = \"" + commit + "\"\n"
		if err := os.WriteFile(implicitPath, []byte(implicit), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeImplicit(commitA)
	cfg := &config.City{}
	bpGood := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGood.skillCatalog == nil || len(bpGood.skillCatalog.Entries) == 0 {
		t.Fatalf("baseline: expected non-empty bootstrap-backed catalog, got %+v", bpGood.skillCatalog)
	}

	writeImplicit(bootstrapName + "-commit-b")
	bpGrace := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpGrace.skillCatalog == nil {
		t.Fatal("bootstrap commit change should grace once with cached catalog, got nil")
	}
	if got := len(bpGrace.skillCatalog.Entries); got != len(bpGood.skillCatalog.Entries) {
		t.Fatalf("bootstrap commit change grace tick should reuse cached catalog: got %d entries want %d", got, len(bpGood.skillCatalog.Entries))
	}

	bpCleared := newAgentBuildParams("city", cityPath, cfg, nil, time.Unix(0, 0), nil, io.Discard)
	if bpCleared.skillCatalog == nil {
		t.Fatal("second bootstrap commit-change tick should clear to an empty catalog, not nil")
	}
	if got := len(bpCleared.skillCatalog.Entries); got != 0 {
		t.Fatalf("second bootstrap commit-change tick should clear stale cache: got %d entries", got)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func replaceWithSelfSymlink(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

// TestResolveTemplateFPExtra_NotEmptyForPoolAgent pins the observed
// "FPExtra: map[] (len=0)" drift: a mayor-like or pool-like agent with
// MaxActiveSessions set and WakeMode != "" must never produce an empty
// FingerprintExtra, regardless of sessionProvider, catalog state, or
// agent struct shape. If a code path ever constructs tp with empty FPExtra
// for such an agent, the reconciler's stored fingerprint (built at session
// start with full FPExtra) will never match the reconcile-time computation
// and every tick drains the session.
//
// Matrix covers the inputs the reconcile-side build_params sees:
//   - sessionProvider: "tmux" (stage-2 eligible) vs "" (isStage2 returns
//     true for empty too) vs "subprocess" (ineligible, skills don't merge
//     but pool/wake must still populate FPExtra)
//   - skill catalog: loaded vs nil (simulates LoadCityCatalog failure)
//   - WakeMode: "fresh" vs "" vs "resume" (resume is intentionally
//     excluded from FPExtra; assert that only wake_mode drops, not pool.*)
func TestResolveTemplateFPExtra_NotEmptyForPoolAgent(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"fp-test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sharedCat, err := materialize.LoadCityCatalog(filepath.Join(cityPath, "skills"))
	if err != nil {
		t.Fatal(err)
	}

	makeParams := func(sessionProvider string, skills *materialize.CityCatalog) *agentBuildParams {
		return &agentBuildParams{
			cityName:  "city",
			cityPath:  cityPath,
			workspace: &config.Workspace{Provider: "claude"},
			providers: map[string]config.ProviderSpec{
				"claude": {Command: "echo", PromptMode: "none", SupportsACP: boolPtr(true)},
			},
			lookPath:        func(string) (string, error) { return "/bin/echo", nil },
			fs:              fsys.OSFS{},
			rigs:            []config.Rig{},
			beaconTime:      time.Unix(0, 0),
			beadNames:       make(map[string]string),
			stderr:          io.Discard,
			skillCatalog:    skills,
			sessionProvider: sessionProvider,
		}
	}

	cases := []struct {
		name            string
		sessionProvider string
		skills          *materialize.CityCatalog
		agent           *config.Agent
	}{
		{
			name:            "tmux+skills",
			sessionProvider: "tmux",
			skills:          &sharedCat,
			agent: &config.Agent{
				Name: "mayor", Scope: "city", Provider: "claude",
				MaxActiveSessions: intPtr(1), WakeMode: "fresh",
			},
		},
		{
			name:            "tmux+nil-catalog",
			sessionProvider: "tmux",
			skills:          nil,
			agent: &config.Agent{
				Name: "mayor", Scope: "city", Provider: "claude",
				MaxActiveSessions: intPtr(1), WakeMode: "fresh",
			},
		},
		{
			name:            "subprocess+nil-catalog",
			sessionProvider: "subprocess",
			skills:          nil,
			agent: &config.Agent{
				Name: "mayor", Scope: "city", Provider: "claude",
				MaxActiveSessions: intPtr(1), WakeMode: "fresh",
			},
		},
		{
			name:            "subprocess+resume-wake",
			sessionProvider: "subprocess",
			skills:          nil,
			agent: &config.Agent{
				Name: "claude", Dir: "gascity", Scope: "rig", Provider: "claude",
				MaxActiveSessions: intPtr(6), WakeMode: "resume",
			},
		},
		{
			name:            "tmux+pool+empty-wake",
			sessionProvider: "tmux",
			skills:          &sharedCat,
			agent: &config.Agent{
				Name: "claude", Dir: "gascity", Scope: "rig", Provider: "claude",
				MaxActiveSessions: intPtr(6),
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fpExtra := buildFingerprintExtra(c.agent)
			tp, err := resolveTemplate(makeParams(c.sessionProvider, c.skills), c.agent, c.agent.QualifiedName(), fpExtra)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}
			if len(tp.FPExtra) == 0 {
				t.Fatalf("tp.FPExtra must not be empty for pool agent %q (MaxActiveSessions=%d wake_mode=%q provider=%q) — empty FPExtra is the observed drift signature in production (stored=7687ba... current=e3b0c44... = empty hash)",
					c.agent.QualifiedName(), func() int {
						if c.agent.MaxActiveSessions != nil {
							return *c.agent.MaxActiveSessions
						}
						return 0
					}(), c.agent.WakeMode, c.sessionProvider)
			}
			// At minimum, pool.min and pool.max must be present for any agent
			// with MaxActiveSessions set — those are pure identity and never
			// depend on catalog state or session provider.
			if _, has := tp.FPExtra["pool.max"]; !has {
				t.Errorf("tp.FPExtra missing pool.max for pool agent (FPExtra=%v)", tp.FPExtra)
			}
			if _, has := tp.FPExtra["pool.min"]; !has {
				t.Errorf("tp.FPExtra missing pool.min for pool agent (FPExtra=%v)", tp.FPExtra)
			}
		})
	}
}
