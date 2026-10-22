package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// implicitSlotCity builds a city whose rig app is a git checkout with one
// commit, and returns the rig-scoped implicit agent the default config mints
// for provider claude.
func implicitSlotCity(t *testing.T) (string, string, *config.City, *config.Agent) {
	t.Helper()
	return implicitSlotCityPrepared(t, nil)
}

// implicitSlotCityPrepared is implicitSlotCity with prep run on the config and
// rig root before the implicit agents are injected.
func implicitSlotCityPrepared(t *testing.T, prep func(cfg *config.City, rig string)) (string, string, *config.City, *config.Agent) {
	t.Helper()
	city := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(city); err == nil {
		city = resolved
	}
	rig := filepath.Join(city, "app")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, rig, "init", "-q")
	if err := os.WriteFile(filepath.Join(rig, "README.md"), []byte("rig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, rig, "add", "README.md")
	mustGit(t, rig, "commit", "-q", "-m", "initial")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{{Name: "app", Path: rig}},
	}
	if prep != nil {
		prep(cfg, rig)
	}
	config.InjectImplicitAgents(cfg)
	for i := range cfg.Agents {
		if cfg.Agents[i].Dir == "app" && cfg.Agents[i].Name == "claude" {
			return city, rig, cfg, &cfg.Agents[i]
		}
	}
	t.Fatalf("InjectImplicitAgents minted no rig-scoped claude agent: %+v", cfg.Agents)
	return "", "", nil, nil
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, dir, args...))
}

func TestImplicitRigPoolSlotsProvisionIsolatedWorktrees(t *testing.T) {
	t.Setenv("GC_SESSION", "tmux")
	city, rig, cfg, a := implicitSlotCity(t)
	rigHead := gitOut(t, rig, "rev-parse", "HEAD")
	rigBranch := gitOut(t, rig, "symbolic-ref", "--short", "HEAD")

	dirs := map[string]string{}
	branches := map[string]string{}
	for _, n := range []string{"1", "2"} {
		dir, err := resolveWorkDirForQualifiedName(city, cfg, a, "app/claude-"+n)
		if err != nil {
			t.Fatalf("resolving app/claude-%s: %v", n, err)
		}
		if want := filepath.Join(city, ".gc", "worktrees", "app", "claude-"+n); dir != want {
			t.Fatalf("app/claude-%s work dir = %q, want %q", n, dir, want)
		}
		branch := gitOut(t, dir, "symbolic-ref", "--short", "HEAD")
		if !strings.HasPrefix(branch, "gc-claude-"+n+"-") {
			t.Errorf("app/claude-%s branch = %q, want gc-claude-%s-<hash>", n, branch, n)
		}
		if got := gitOut(t, dir, "rev-parse", "HEAD"); got != rigHead {
			t.Errorf("app/claude-%s HEAD = %s, want rig HEAD %s", n, got, rigHead)
		}
		if err := os.WriteFile(filepath.Join(dir, "scratch-"+n+".txt"), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
		dirs[n], branches[n] = dir, branch
	}
	if branches["1"] == branches["2"] {
		t.Errorf("both slots are on branch %q", branches["1"])
	}
	for _, check := range []struct{ file, dir string }{
		{"scratch-1.txt", dirs["2"]},
		{"scratch-1.txt", rig},
		{"scratch-2.txt", dirs["1"]},
		{"scratch-2.txt", rig},
	} {
		if _, err := os.Stat(filepath.Join(check.dir, check.file)); err == nil {
			t.Errorf("%s leaked into %s", check.file, check.dir)
		}
	}
	if got := gitOut(t, rig, "symbolic-ref", "--short", "HEAD"); got != rigBranch {
		t.Errorf("rig root branch = %q, want unchanged %q", got, rigBranch)
	}
	if got := gitOut(t, rig, "status", "--porcelain"); got != "" {
		t.Errorf("rig root status = %q, want clean", got)
	}

	for _, n := range []string{"1", "2"} {
		dir := dirs[n]
		ops := hookClaimOps{
			ResolveWorkBranch:     hookResolveWorkBranch,
			ResolveSessionWorkDir: func(string) string { return dir },
		}
		opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=sess-" + n}}
		patch := hookClaimIdentityPatch(beads.Bead{ID: "work-" + n, Metadata: beads.StringMap{}}, opts, ops, rig)
		if got := patch[beadmeta.WorkDirMetadataKey]; got != dir {
			t.Errorf("claim by app/claude-%s stamped gc.work_dir=%q, want %q", n, got, dir)
		}
		if got := patch[beadmeta.WorkBranchMetadataKey]; got != branches[n] {
			t.Errorf("claim by app/claude-%s stamped gc.work_branch=%q, want %q", n, got, branches[n])
		}
	}
}

func TestImplicitSlotWorktreesKeepTodaysWorkDirOutsideTheirScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		session    string
		identity   string
		prep       func(cfg *config.City, rig string)
		patchAgent func(a *config.Agent, rig string)
	}{
		{name: "canonical", session: "tmux", identity: "app/claude"},
		{name: "adhoc", session: "tmux", identity: "app/claude-adhoc-1a2b3c"},
		{name: "work_dir", session: "tmux", identity: "app/claude-1", patchAgent: func(a *config.Agent, rig string) { a.WorkDir = rig }},
		{name: "k8s", session: "k8s", identity: "app/claude-1"},
		{name: "hybrid", session: "hybrid", identity: "app/claude-1"},
		{name: "ssh", session: "ssh:build-host", identity: "app/claude-1"},
		{name: "exec", session: "exec:/bin/true", identity: "app/claude-1"},
		{name: "city_k8s", identity: "app/claude-1", prep: func(cfg *config.City, _ string) { cfg.Session.Provider = "k8s" }},
		{name: "no_git", session: "tmux", identity: "app/claude-1", prep: func(_ *config.City, rig string) {
			if err := os.RemoveAll(filepath.Join(rig, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "isolation_none", session: "tmux", identity: "app/claude-1", prep: func(cfg *config.City, _ string) {
			cfg.Workspace.ImplicitIsolation = "none"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_SESSION", tc.session)
			city, rig, cfg, a := implicitSlotCityPrepared(t, tc.prep)
			if tc.patchAgent != nil {
				tc.patchAgent(a, rig)
			}
			_, isRepo := os.Lstat(filepath.Join(rig, ".git"))
			var refs string
			if isRepo == nil {
				refs = gitOut(t, rig, "for-each-ref")
			}

			dir, err := resolveWorkDirForQualifiedName(city, cfg, a, tc.identity)
			if err != nil {
				t.Fatalf("resolving %s: %v", tc.identity, err)
			}
			if dir != rig {
				t.Errorf("%s work dir = %q, want today's rig root %q", tc.identity, dir, rig)
			}
			if _, err := os.Lstat(filepath.Join(city, ".gc", "worktrees")); !os.IsNotExist(err) {
				t.Errorf("%s created %s (lstat err %v)", tc.identity, filepath.Join(city, ".gc", "worktrees"), err)
			}
			if isRepo != nil {
				return
			}
			if got := strings.Count(gitOut(t, rig, "worktree", "list", "--porcelain"), "worktree "); got != 1 {
				t.Errorf("rig has %d worktrees, want only the rig root", got)
			}
			if got := gitOut(t, rig, "for-each-ref"); got != refs {
				t.Errorf("rig refs changed:\nbefore:\n%s\nafter:\n%s", refs, got)
			}
		})
	}
}
