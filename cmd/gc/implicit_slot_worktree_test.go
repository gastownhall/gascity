package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
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

// provisionImplicitSlot provisions app/claude-1 and returns its slot.
func provisionImplicitSlot(t *testing.T, city string, cfg *config.City, a *config.Agent) workdirutil.ImplicitSlot {
	t.Helper()
	slot, ok := workdirutil.ImplicitSlotFor(city, "app/claude-1", *a, cfg.Rigs)
	if !ok {
		t.Fatal("app/claude-1 is not a pool slot")
	}
	if err := ensureImplicitSlotWorktree(slot); err != nil {
		t.Fatalf("provisioning %s: %v", slot.Path, err)
	}
	return slot
}

// slotSnapshot is a slot's checked-out ref, commit, status and top-level
// entries, the state a reused slot must keep.
func slotSnapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return strings.Join([]string{
		gitOut(t, dir, "symbolic-ref", "HEAD"),
		gitOut(t, dir, "rev-parse", "HEAD"),
		gitOut(t, dir, "status", "--porcelain"),
		strings.Join(names, ","),
	}, "\n")
}

func TestImplicitSlotWithStaleMarkerIsReusedWithoutGit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty func(t *testing.T, dir string)
	}{
		{"dirty", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"committed", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "done.txt"), []byte("done\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			mustGit(t, dir, "add", "done.txt")
			mustGit(t, dir, "commit", "-q", "-m", "slot work")
		}},
		{"switched_branch", func(t *testing.T, dir string) {
			mustGit(t, dir, "checkout", "-q", "-b", "feature-x")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, _, cfg, a := implicitSlotCity(t)
			slot := provisionImplicitSlot(t, city, cfg, a)
			tc.dirty(t, slot.Path)
			marker := filepath.Join(slot.Path, worktreeStaleFileName)
			before := slotSnapshot(t, slot.Path)
			if err := os.WriteFile(marker, []byte("branch=x\nreason=uncommitted-work\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			path := os.Getenv("PATH")
			t.Setenv("PATH", t.TempDir()) // any git subprocess fails
			err := ensureImplicitSlotWorktree(slot)
			t.Setenv("PATH", path)
			if err != nil {
				t.Fatalf("reusing marked slot: %v", err)
			}
			if _, err := os.Lstat(marker); !os.IsNotExist(err) {
				t.Errorf("marker %s still present (lstat err %v)", marker, err)
			}
			if after := slotSnapshot(t, slot.Path); after != before {
				t.Errorf("reused slot changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestImplicitSlotMissingDirIsRecreated(t *testing.T) {
	advanceRig := func(t *testing.T, rig string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(rig, "next.txt"), []byte("next\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, rig, "add", "next.txt")
		mustGit(t, rig, "commit", "-q", "-m", "next")
	}
	for _, tc := range []struct {
		name         string
		unmerged     bool
		dropRegister bool
	}{
		{name: "merged_registered"},
		{name: "merged_unregistered", dropRegister: true},
		{name: "unmerged_registered", unmerged: true},
		{name: "unmerged_unregistered", unmerged: true, dropRegister: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, rig, cfg, a := implicitSlotCity(t)
			slot := provisionImplicitSlot(t, city, cfg, a)
			var slotWork string
			if tc.unmerged {
				if err := os.WriteFile(filepath.Join(slot.Path, "work.txt"), []byte("work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				mustGit(t, slot.Path, "add", "work.txt")
				mustGit(t, slot.Path, "commit", "-q", "-m", "slot work")
				slotWork = gitOut(t, slot.Path, "rev-parse", "HEAD")
			}
			if err := os.RemoveAll(slot.Path); err != nil {
				t.Fatal(err)
			}
			if tc.dropRegister {
				mustGit(t, rig, "worktree", "prune")
			}
			advanceRig(t, rig)
			rigHead := gitOut(t, rig, "rev-parse", "HEAD")

			if err := ensureImplicitSlotWorktree(slot); err != nil {
				t.Fatalf("re-creating missing slot: %v", err)
			}
			if got := gitOut(t, slot.Path, "symbolic-ref", "--short", "HEAD"); got != slot.Branch {
				t.Errorf("re-created slot branch = %q, want %q", got, slot.Branch)
			}
			want := rigHead
			if tc.unmerged {
				want = slotWork
			}
			if got := gitOut(t, slot.Path, "rev-parse", "HEAD"); got != want {
				t.Errorf("re-created slot HEAD = %s, want %s", got, want)
			}
			if got := strings.Count(gitOut(t, rig, "worktree", "list", "--porcelain"), "worktree "); got != 2 {
				t.Errorf("rig has %d worktrees, want the rig root and the slot", got)
			}
		})
	}
}

// treeSnapshot records every path under root with its mode and, for files and
// symlinks, its content, so any file, ref or worktree registration change
// shows up as a diff.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(&b, "%s %s", rel, info.Mode())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " -> %s", target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %x", sha256.Sum256(data))
		}
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestImplicitSlotThatCannotBeProvisionedFailsClosed(t *testing.T) {
	slotPath := func(city string) string { return filepath.Join(city, ".gc", "worktrees", "app", "claude-1") }
	for _, tc := range []struct {
		name  string
		prep  func(cfg *config.City, rig string)
		setup func(t *testing.T, city, rig string, cfg *config.City, a *config.Agent)
	}{
		{name: "unborn_head", prep: func(_ *config.City, rig string) {
			if err := os.RemoveAll(filepath.Join(rig, ".git")); err != nil {
				panic(err)
			}
			if out, err := exec.Command("git", "-C", rig, "init", "-q").CombinedOutput(); err != nil {
				panic(string(out))
			}
		}},
		{name: "non_empty_plain_dir", setup: func(t *testing.T, city, _ string, _ *config.City, _ *config.Agent) {
			if err := os.MkdirAll(slotPath(city), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(slotPath(city), "keep.txt"), []byte("keep\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "dot_git_directory", setup: func(t *testing.T, city, _ string, _ *config.City, _ *config.Agent) {
			if err := os.MkdirAll(slotPath(city), 0o755); err != nil {
				t.Fatal(err)
			}
			mustGit(t, slotPath(city), "init", "-q")
		}},
		{name: "other_repo_worktree", setup: func(t *testing.T, city, _ string, _ *config.City, _ *config.Agent) {
			other := filepath.Join(city, "other")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			mustGit(t, other, "init", "-q")
			mustGit(t, other, "commit", "-q", "--allow-empty", "-m", "other")
			mustGit(t, other, "worktree", "add", "-q", "-b", "other-slot", slotPath(city))
		}},
		{name: "locked_registration", setup: func(t *testing.T, city, rig string, cfg *config.City, a *config.Agent) {
			slot := provisionImplicitSlot(t, city, cfg, a)
			mustGit(t, rig, "worktree", "lock", slot.Path)
			if err := os.RemoveAll(slot.Path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "branch_checked_out_elsewhere", setup: func(t *testing.T, city, rig string, cfg *config.City, a *config.Agent) {
			slot := provisionImplicitSlot(t, city, cfg, a)
			if err := os.RemoveAll(slot.Path); err != nil {
				t.Fatal(err)
			}
			mustGit(t, rig, "worktree", "prune")
			mustGit(t, rig, "worktree", "add", "-q", filepath.Join(city, "elsewhere"), slot.Branch)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_SESSION", "tmux")
			city, rig, cfg, a := implicitSlotCityPrepared(t, tc.prep)
			if tc.setup != nil {
				tc.setup(t, city, rig, cfg, a)
			}
			before := treeSnapshot(t, city)

			dir, err := resolveWorkDirForQualifiedName(city, cfg, a, "app/claude-1")
			if err == nil {
				t.Fatalf("app/claude-1 resolved to %q, want a provisioning refusal", dir)
			}
			for _, want := range []string{slotPath(city), `implicit_isolation = "none"`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			if after := treeSnapshot(t, city); after != before {
				t.Errorf("refused provisioning changed the city tree:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}
