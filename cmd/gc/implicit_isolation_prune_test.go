package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	corepack "github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// These tests pin #1181's cleanup story: worktree isolation mode provisions
// slot worktrees at city/.gc/worktrees/<rig>/<slot> via the core pack's
// worktree-setup.sh, and the existing session-close prune path — unchanged —
// must reclaim them (clean slots) or keep them with a stale marker (dirty
// slots), while the script's reuse path preserves in-flight content.

// slotGit runs git with a test identity in the given directory.
func slotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// mkSlotRig provisions the exact shape worktree isolation mode produces: a
// git rig whose origin carries the initial commit, and a slot worktree at
// cityPath/.gc/worktrees/<rig>/<slot> created by the core pack's
// worktree-setup.sh — the pre_start the mode injects. The origin matters:
// without a remote-tracking ref the unpushed-commits gate fails closed and
// no worktree is ever prunable.
func mkSlotRig(t *testing.T, cityPath, rigName, slot string) (repoDir, slotDir string) {
	t.Helper()
	repoDir = t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	slotGit(t, origin, "init", "--bare", "-b", "main")
	slotGit(t, repoDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("rig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	slotGit(t, repoDir, "add", "README.md")
	slotGit(t, repoDir, "commit", "-m", "initial")
	slotGit(t, repoDir, "remote", "add", "origin", origin)
	slotGit(t, repoDir, "push", "origin", "main")

	slotDir = filepath.Join(cityPath, ".gc", "worktrees", rigName, slot)
	runCoreSetupScript(t, repoDir, slotDir, slot)
	return repoDir, slotDir
}

// runCoreSetupScript materializes the core pack's worktree-setup.sh and runs
// it the way the injected pre_start does.
func runCoreSetupScript(t *testing.T, repoDir, wt, agent string) {
	t.Helper()
	const rel = "assets/scripts/worktree-setup.sh"
	data, err := fs.ReadFile(corepack.PackFS, rel)
	if err != nil {
		t.Fatalf("reading embedded core %s: %v", rel, err)
	}
	dst := filepath.Join(t.TempDir(), "worktree-setup.sh")
	if err := os.WriteFile(dst, data, builtinpacks.MaterializedFileMode(rel)); err != nil {
		t.Fatalf("materializing %s: %v", rel, err)
	}
	out, err := exec.Command("sh", dst, repoDir, wt, agent, "--sync").CombinedOutput()
	if err != nil {
		t.Fatalf("worktree-setup.sh failed: %v\n%s", err, out)
	}
}

// slotSessionBead builds the closed session bead for a slot worktree: the
// worker_dir the reconciler reads, and the qualified template that resolves
// the owning rig.
func slotSessionBead(slotDir, template string) beads.Bead {
	return beads.Bead{Metadata: beads.StringMap{
		contract.WorkerDirKey: slotDir,
		"template":            template,
		"session_name":        "gc-session-slot-test",
	}}
}

// TestPruneRemovesCleanImplicitIsolationSlotWorktree proves the session-close
// prune path reclaims a clean slot worktree created by the core pack script,
// with no new lifecycle wiring: the existing gates all pass and git's
// worktree list forgets the slot.
func TestPruneRemovesCleanImplicitIsolationSlotWorktree(t *testing.T) {
	cityPath := t.TempDir()
	repoDir, slotDir := mkSlotRig(t, cityPath, "demo", "claude-1")
	if _, err := os.Stat(filepath.Join(slotDir, ".git")); err != nil {
		t.Fatalf("slot worktree not provisioned: %v", err)
	}

	cfg := &config.City{Rigs: []config.Rig{{Name: "demo", Path: repoDir}}}
	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(slotSessionBead(slotDir, "demo/claude-1"), cityPath, cfg, &stderr) {
		t.Fatalf("prune refused a clean slot worktree: %s", stderr.String())
	}
	if _, err := os.Stat(slotDir); !os.IsNotExist(err) {
		t.Fatalf("clean slot worktree still on disk: %v", err)
	}
	if out := runGit(t, repoDir, "worktree", "list"); strings.Contains(out, slotDir) {
		t.Fatalf("git still lists the pruned slot:\n%s", out)
	}
}

// TestPruneKeepsDirtyImplicitIsolationSlotWorktreeWithMarker proves the
// fail-closed side: a slot with uncommitted work is kept, its content is
// intact, and it carries the .worktree-stale marker naming the reason, so
// an operator can find out why cleanup skipped it.
func TestPruneKeepsDirtyImplicitIsolationSlotWorktreeWithMarker(t *testing.T) {
	cityPath := t.TempDir()
	repoDir, slotDir := mkSlotRig(t, cityPath, "demo", "claude-1")
	if err := os.WriteFile(filepath.Join(slotDir, "wip.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Rigs: []config.Rig{{Name: "demo", Path: repoDir}}}
	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(slotSessionBead(slotDir, "demo/claude-1"), cityPath, cfg, &stderr) {
		t.Fatal("prune removed a slot worktree with uncommitted work")
	}
	if _, err := os.Stat(filepath.Join(slotDir, "wip.txt")); err != nil {
		t.Fatalf("uncommitted slot content lost: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(slotDir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("%s marker not written: %v", worktreeStaleFileName, err)
	}
	if !strings.Contains(string(marker), "reason=uncommitted-work") {
		t.Fatalf("marker = %q, want reason=uncommitted-work", string(marker))
	}
}

// TestCoreSetupScriptReuseKeepsSlotContent proves the crash-recovery shape:
// a session dies mid-work and the slot's pre_start runs again on restart —
// the reuse path must leave the uncommitted content and the slot branch
// exactly as they were.
func TestCoreSetupScriptReuseKeepsSlotContent(t *testing.T) {
	cityPath := t.TempDir()
	repoDir, slotDir := mkSlotRig(t, cityPath, "demo", "claude-1")
	if err := os.WriteFile(filepath.Join(slotDir, "wip.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	branch := slotGit(t, slotDir, "rev-parse", "--abbrev-ref", "HEAD")

	runCoreSetupScript(t, repoDir, slotDir, "claude-1")

	data, err := os.ReadFile(filepath.Join(slotDir, "wip.txt"))
	if err != nil {
		t.Fatalf("reuse path lost uncommitted content: %v", err)
	}
	if string(data) != "uncommitted\n" {
		t.Fatalf("reuse path rewrote uncommitted content: %q", string(data))
	}
	if got := slotGit(t, slotDir, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("reuse path moved the branch %q -> %q", branch, got)
	}
	if out := slotGit(t, slotDir, "status", "--porcelain"); !strings.Contains(out, "wip.txt") {
		t.Fatalf("reuse path must not commit or stage in-flight content; status:\n%s", out)
	}
}
