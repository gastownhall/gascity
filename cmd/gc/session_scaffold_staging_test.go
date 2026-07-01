package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPrepareStartCandidateStagesScaffoldInResolvedTaskWorkDirWhenCWDIsSharedWorktree(t *testing.T) {
	root := t.TempDir()
	cityPath := filepath.Join(root, "city")
	sharedWorktree := filepath.Join(root, "shared-builder")
	beadSlug := "ga-ajw1no-1-as-a-maintainer-i-can-reproduce-stray-session-scaffold-leakage"
	leakedWorkDir := filepath.Join(sharedWorktree, beadSlug)
	relativeTargetWorkDir := filepath.Join(".gc", "worktrees", "gascity", "builder", beadSlug)
	targetWorkDir := filepath.Join(cityPath, relativeTargetWorkDir)
	packOverlay := filepath.Join(cityPath, "packs", "core", "overlay")

	writeScaffoldFixture(t, filepath.Join(packOverlay, ".claude", "skills", "triage", "SKILL.md"), "---\nname: triage\n---\n")
	writeScaffoldFixture(t, filepath.Join(packOverlay, ".codex", "hooks.json"), `{"hooks":{"SessionStart":[]}}`+"\n")
	writeScaffoldFixture(t, filepath.Join(packOverlay, ".gc", "settings.json"), "{}\n")
	if err := os.MkdirAll(targetWorkDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", targetWorkDir, err)
	}
	if err := os.MkdirAll(sharedWorktree, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", sharedWorktree, err)
	}
	t.Chdir(sharedWorktree)

	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "builder",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:gascity/builder"},
		Metadata: map[string]string{
			"template":     "builder",
			"session_name": "builder-ga-ajw1no",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.Create(beads.Bead{
		Title: "task",
		Metadata: map[string]string{
			"work_dir": relativeTargetWorkDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	assignee := session.ID
	if err := store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareStartCandidateForCity(startCandidate{
		session: &session,
		tp: TemplateParams{
			TemplateName: "gascity/builder",
			SessionName:  "builder-ga-ajw1no",
			WorkDir:      leakedWorkDir,
			Env: map[string]string{
				"GC_DIR": leakedWorkDir,
			},
			Hints: agent.StartupHints{
				ProviderName:        "codex",
				ProviderOverlayName: "codex",
				PackOverlayDirs:     []string{packOverlay},
				PreStart:            appendMaterializeSkillsPreStart(nil, "gascity/builder", leakedWorkDir),
			},
		},
		order: 0,
	}, cityPath, "city", &config.City{
		Agents: []config.Agent{
			{
				Name:              "builder",
				Dir:               "gascity",
				MinActiveSessions: intPtrScaffoldRegression(1),
				MaxActiveSessions: intPtrScaffoldRegression(2),
			},
		},
	}, nil, store, &clock.Fake{Time: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}, io.Discard, nil)
	if err != nil {
		t.Fatalf("prepareStartCandidateForCity: %v", err)
	}

	if prepared.cfg.WorkDir != targetWorkDir {
		t.Errorf("prepared.cfg.WorkDir = %q, want resolved task work_dir %q", prepared.cfg.WorkDir, targetWorkDir)
	}
	if prepared.cfg.Env["GC_DIR"] != targetWorkDir {
		t.Errorf("prepared.cfg.Env[GC_DIR] = %q, want %q", prepared.cfg.Env["GC_DIR"], targetWorkDir)
	}
	if len(prepared.cfg.PreStart) != 1 {
		t.Fatalf("PreStart = %v, want materialize-skills entry", prepared.cfg.PreStart)
	}
	if !strings.Contains(prepared.cfg.PreStart[0], "--workdir "+targetWorkDir) {
		t.Errorf("materialize-skills PreStart = %q, want resolved target workdir %q", prepared.cfg.PreStart[0], targetWorkDir)
	}
	if strings.Contains(prepared.cfg.PreStart[0], leakedWorkDir) {
		t.Errorf("materialize-skills PreStart still targets shared-cwd bead slug %q: %q", leakedWorkDir, prepared.cfg.PreStart[0])
	}

	if err := runtime.StageSessionWorkDir(prepared.cfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	for _, rel := range []string{
		filepath.Join(".claude", "skills", "triage", "SKILL.md"),
		filepath.Join(".codex", "hooks.json"),
		filepath.Join(".gc", "settings.json"),
	} {
		if _, err := os.Stat(filepath.Join(targetWorkDir, rel)); err != nil {
			t.Errorf("target scaffold %s missing under resolved workdir %q: %v", rel, targetWorkDir, err)
		}
	}
	if _, err := os.Stat(leakedWorkDir); err == nil {
		t.Fatalf("shared cwd contains stray bead-slug scaffold directory %q; scaffold must stay under %q", leakedWorkDir, targetWorkDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat leaked workdir %q: %v", leakedWorkDir, err)
	}
}

func writeScaffoldFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func intPtrScaffoldRegression(n int) *int {
	return &n
}
