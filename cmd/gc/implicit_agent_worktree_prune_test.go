package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workdir"
)

// implicitWorktreePruneCityTOML is a worktree-mode tmux city with worker-dir
// pruning on, the "claude" provider and rig "demo".
const implicitWorktreePruneCityTOML = "[workspace]\nname = \"city\"\nimplicit_agent_isolation = \"worktree\"\n" +
	"\n[session]\nprovider = \"tmux\"\n\n[daemon]\nauto_prune_worker_dir = true\n" +
	"\n[providers.claude]\nbase = \"builtin:claude\"\n\n[[rigs]]\nname = \"demo\"\n"

// setGitTestEnv isolates git from the host config, for both the pre_start
// script and the prune probes.
func setGitTestEnv(t *testing.T) {
	t.Helper()
	for key, val := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(key, val)
	}
}

// gitOut runs git in dir and returns its trimmed output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, dir, args...))
}

// cloneRigWithPushedMain makes rigRoot a clone of a bare origin whose main
// holds one commit, so a fresh slot branch has nothing unpushed.
func cloneRigWithPushedMain(t *testing.T, rigRoot string) {
	t.Helper()
	seed := t.TempDir()
	runGit(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("rig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-q", "-m", "initial")
	origin := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, seed, "clone", "-q", "--bare", seed, origin)
	runGit(t, filepath.Dir(rigRoot), "clone", "-q", origin, rigRoot)
}

// implicitSlot is one session slot of the rig's implicit agent: its resolved
// work dir and its expanded pre_start.
type implicitSlot struct {
	workDir  string
	preStart []string
}

func resolveImplicitSlot(t *testing.T, cityPath string, cfg *config.City, agentBase string) implicitSlot {
	t.Helper()
	var agent *config.Agent
	for i := range cfg.Agents {
		if a := &cfg.Agents[i]; a.Implicit && a.Dir == "demo" && a.Name == "claude" {
			agent = a
		}
	}
	if agent == nil {
		t.Fatal("no implicit demo/claude agent")
	}
	wd, err := workdir.ResolveWorkDirPathStrict(cityPath, "city", agent.QualifiedInstanceName(agentBase), *agent, cfg.Rigs)
	if err != nil {
		t.Fatalf("resolving %s work dir: %v", agentBase, err)
	}
	return implicitSlot{
		workDir: wd,
		preStart: expandSessionSetup(agent.PreStart, SessionSetupContext{
			AgentBase: agentBase, Rig: "demo", RigRoot: cfg.Rigs[0].Path,
			CityRoot: cityPath, CityName: "city", WorkDir: wd,
		}),
	}
}

// start runs the slot's pre_start the way a session start does.
func (s implicitSlot) start(t *testing.T) {
	t.Helper()
	for _, cmd := range s.preStart {
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("pre_start %q: %v\n%s", cmd, err, out)
		}
	}
}

func (s implicitSlot) closedSession() beads.Bead {
	return beads.Bead{ID: "session-1", Metadata: map[string]string{
		"worker_dir": s.workDir, "template": "demo/claude", "session_name": "demo/claude-1",
	}}
}

// AC-8: closing an implicit session prunes a clean slot worktree and keeps a
// dirty, unpushed or stashed one, which the slot's next session reuses.
func TestImplicitAgentWorktreeSlotPruneOnClose(t *testing.T) {
	setGitTestEnv(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(implicitWorktreePruneCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	bindImplicitWorktreeCity(t, cityPath, "city")
	if err := os.MkdirAll(filepath.Join(cityPath, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	cloneRigWithPushedMain(t, filepath.Join(cityPath, "repos", "demo"))
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	if !cfg.Daemon.AutoPruneWorkerDirEnabled() {
		t.Fatal("auto_prune_worker_dir is off")
	}
	slot := resolveImplicitSlot(t, cityPath, cfg, "claude-1")
	if want := filepath.Join(cityPath, ".gc", "worktrees", "demo", "claude-1"); slot.workDir != want {
		t.Fatalf("slot work dir = %q, want %q", slot.workDir, want)
	}

	t.Run("clean slot is removed", func(t *testing.T) {
		slot.start(t)
		var stderr bytes.Buffer
		if !pruneAgentHomeWorktreeIfSafe(slot.closedSession(), cityPath, cfg, &stderr) {
			t.Fatalf("clean slot not pruned; stderr=%s", stderr.String())
		}
		if _, err := os.Stat(slot.workDir); !os.IsNotExist(err) {
			t.Fatalf("clean slot worktree still present (stat err %v)", err)
		}
	})

	for _, tc := range []struct {
		name   string
		reason string
		// work leaves unsaved work in the slot; kept reports whether it
		// is still there.
		work func(t *testing.T, wt string)
		kept func(t *testing.T, wt string) bool
	}{
		{
			"dirty", "uncommitted-work",
			func(t *testing.T, wt string) {
				if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			func(_ *testing.T, wt string) bool {
				data, err := os.ReadFile(filepath.Join(wt, "wip.txt"))
				return err == nil && string(data) == "wip\n"
			},
		},
		{
			"unpushed", "unpushed-commits",
			func(t *testing.T, wt string) {
				if err := os.WriteFile(filepath.Join(wt, "done.txt"), []byte("done\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, wt, "add", "done.txt")
				runGit(t, wt, "commit", "-q", "-m", "unpushed work")
			},
			func(t *testing.T, wt string) bool {
				return gitOut(t, wt, "log", "-1", "--format=%s") == "unpushed work"
			},
		},
		{
			"stashed", "stashed-work",
			func(t *testing.T, wt string) {
				if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("stashed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, wt, "stash", "push", "-q", "-m", "slot stash")
			},
			func(t *testing.T, wt string) bool {
				return strings.Contains(gitOut(t, wt, "stash", "list"), "slot stash")
			},
		},
	} {
		t.Run(tc.name+" slot is kept and reused", func(t *testing.T) {
			slot.start(t)
			branch := gitOut(t, slot.workDir, "branch", "--show-current")
			tc.work(t, slot.workDir)

			var stderr bytes.Buffer
			if pruneAgentHomeWorktreeIfSafe(slot.closedSession(), cityPath, cfg, &stderr) {
				t.Fatalf("%s slot pruned", tc.name)
			}
			assertWorktreeStaleMarker(t, slot.workDir, branch, tc.reason)

			slot.start(t)
			if got := gitOut(t, slot.workDir, "branch", "--show-current"); got != branch {
				t.Fatalf("next session is on branch %q, want the kept %q", got, branch)
			}
			if !tc.kept(t, slot.workDir) {
				t.Fatalf("next session lost the kept %s work", tc.name)
			}

			// Drop the kept work so the next case starts from a clean slot.
			if err := os.RemoveAll(slot.workDir); err != nil {
				t.Fatal(err)
			}
			runGit(t, cfg.Rigs[0].Path, "worktree", "prune")
			runGit(t, cfg.Rigs[0].Path, "branch", "-D", branch)
		})
	}
}
