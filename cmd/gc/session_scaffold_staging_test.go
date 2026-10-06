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
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/shellquote"
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
		info: sessiontest.SeedBead(t, session),
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
	} {
		if _, err := os.Stat(filepath.Join(targetWorkDir, rel)); err != nil {
			t.Errorf("target scaffold %s missing under resolved workdir %q: %v", rel, targetWorkDir, err)
		}
	}
	// A top-level .gc/ in the overlay source is a runtime mirror and must never
	// be staged into a session workdir (overlay.skipRuntimeMirror). The session's
	// own .gc/settings.json is staged separately through the hook-file path
	// (see claudeSettingsSource/stageHookFiles), not copied verbatim from the
	// pack overlay, so the mirror is expected to be skipped here.
	if _, err := os.Stat(filepath.Join(targetWorkDir, ".gc", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("overlay .gc runtime mirror must not be staged under resolved workdir %q (stat err = %v)", targetWorkDir, err)
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

// TestRetargetPreStartWorkDirPreservesShellQuoting proves that retargeting a
// generated materialize-skills / project-mcp PreStart command onto a resolved
// task work_dir keeps the `--workdir` argument shell-safe. The generators emit
// the workdir as a shell-quoted token; a resolved work_dir that contains a
// space (macOS "/Users/First Last/...") or a shell metacharacter must not be
// spliced in raw, or the rendered `sh -c` command breaks argument boundaries or
// opens a command-substitution surface.
func TestRetargetPreStartWorkDirPreservesShellQuoting(t *testing.T) {
	t.Parallel()

	const (
		agentName  = "gascity/builder"
		identity   = "gascity/gc.builder"
		oldWorkDir = "/data/worktrees/gascity/builder/ga-clean"
	)

	generators := []struct {
		label    string
		preStart func(workDir string) []string
	}{
		{
			label: "materialize-skills",
			preStart: func(workDir string) []string {
				return appendMaterializeSkillsPreStart(nil, agentName, workDir)
			},
		},
		{
			label: "project-mcp",
			preStart: func(workDir string) []string {
				return appendProjectMCPPreStart(nil, agentName, identity, workDir)
			},
		},
	}

	cases := []struct {
		name       string
		newWorkDir string
	}{
		{name: "space", newWorkDir: "/Users/John Doe/city/worktrees/gascity/builder/ga-target"},
		{name: "command_substitution_with_space", newWorkDir: "/opt/proj $(touch pwned)/builder"},
		{name: "command_substitution_no_space", newWorkDir: "/opt/$(id)/builder"},
	}

	for _, g := range generators {
		for _, tc := range cases {
			t.Run(g.label+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				retargeted := retargetPreStartWorkDir(g.preStart(oldWorkDir), oldWorkDir, tc.newWorkDir)
				if len(retargeted) != 1 {
					t.Fatalf("retarget produced %d entries, want 1: %v", len(retargeted), retargeted)
				}
				cmd := retargeted[0]

				// Structural: the new value must be embedded shell-quoted, exactly as
				// a from-scratch generation would emit it. This catches metacharacter
				// injection even when no whitespace forces a re-split.
				wantToken := "--workdir " + shellquote.Join([]string{tc.newWorkDir})
				if !strings.Contains(cmd, wantToken) {
					t.Errorf("retargeted command missing shell-quoted workdir token %q:\n%s", wantToken, cmd)
				}

				// Behavioral: parsing the command with the same quoting rules the
				// generator used must recover the intended workdir as a single arg.
				if got := workdirArgFromCommand(t, cmd); got != tc.newWorkDir {
					t.Errorf("parsed --workdir = %q, want %q\ncommand: %s", got, tc.newWorkDir, cmd)
				}

				// The stale pre-override path must be gone entirely.
				if strings.Contains(cmd, oldWorkDir) {
					t.Errorf("retargeted command still references old workdir %q:\n%s", oldWorkDir, cmd)
				}
			})
		}
	}
}

// TestRetargetPreStartWorkDirPreservesUserAuthoredLiterals is the regression
// for #4069: retargetPreStartWorkDir used to blindly ReplaceAll the
// pre-override workdir across every PreStart entry, including user-authored
// commands that reference the rig root as a deliberate hardcoded literal
// (the canonical case: `git worktree add` must run against the main
// checkout, not the not-yet-existing per-session directory). Only the
// engine-generated materialize-skills / project-mcp entries should ever be
// rewritten; {{.WorkDir}} already exists for users who want the session dir.
func TestRetargetPreStartWorkDirPreservesUserAuthoredLiterals(t *testing.T) {
	t.Parallel()

	const (
		oldWorkDir = "/Users/klashesselman/Claude/flow-city"
		newWorkDir = "/Users/klashesselman/Claude/flow-city/fc-r1xz-load-context-and-verify-assignment"
	)
	userCmd := "worktree-setup.sh " + oldWorkDir + " \"$GC_DIR\" gc-worker --sync"

	preStart := []string{userCmd}
	preStart = appendMaterializeSkillsPreStart(preStart, "gascity/builder", oldWorkDir)

	retargeted := retargetPreStartWorkDir(preStart, oldWorkDir, newWorkDir)
	if len(retargeted) != 2 {
		t.Fatalf("retarget produced %d entries, want 2: %v", len(retargeted), retargeted)
	}

	if retargeted[0] != userCmd {
		t.Errorf("user-authored literal was rewritten:\n got:  %s\n want: %s", retargeted[0], userCmd)
	}
	// newWorkDir is old+suffix here (matching the real-world rig-scoped
	// per-session worktree naming from the report), so a plain
	// strings.Contains(retargeted[1], oldWorkDir) check would pass
	// spuriously even on a correctly-retargeted value. Compare against
	// what a fresh generation against newWorkDir produces instead.
	want := appendMaterializeSkillsPreStart(nil, "gascity/builder", newWorkDir)[0]
	if retargeted[1] != want {
		t.Errorf("generated materialize-skills entry not retargeted:\n got:  %s\n want: %s", retargeted[1], want)
	}
}

// workdirArgFromCommand parses a generated PreStart command with the same
// POSIX quoting rules the generators use and returns the argument following the
// final --workdir flag.
func workdirArgFromCommand(t *testing.T, command string) string {
	t.Helper()
	args := shellquote.Split(command)
	for i := len(args) - 1; i > 0; i-- {
		if args[i-1] == "--workdir" {
			return args[i]
		}
	}
	t.Fatalf("no --workdir argument in command: %s", command)
	return ""
}
