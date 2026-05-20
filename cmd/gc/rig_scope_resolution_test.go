package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRigFromRedirectedBeadsDirIgnoresCwdOutsideCity verifies that when the
// caller's cwd is outside cityPath, any .beads/redirect found while walking
// the cwd's ancestor chain is ignored. The walk must be bounded by cityPath
// so that a polecat worktree's foreign-rig redirect (e.g., the shared rig
// repo checkout at /home/b/GIT/gascity/.beads) cannot bleed into rig
// resolution against an unrelated city.
func TestRigFromRedirectedBeadsDirIgnoresCwdOutsideCity(t *testing.T) {
	foreignRoot := filepath.Join(t.TempDir(), "foreign")
	if err := os.MkdirAll(filepath.Join(foreignRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwdRoot := t.TempDir()
	cwd := filepath.Join(cwdRoot, "worktree", "polecat-1")
	if err := os.MkdirAll(filepath.Join(cwd, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cwd, ".beads", "redirect"),
		[]byte(filepath.Join(foreignRoot, ".beads")+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"},
		},
	}

	rig, ok, err := rigFromRedirectedBeadsDir(cfg, cityDir, normalizePathForCompare(cwd))
	if err != nil {
		t.Fatalf("rigFromRedirectedBeadsDir() error = %v, want nil (cwd outside cityPath)", err)
	}
	if ok {
		t.Fatalf("rigFromRedirectedBeadsDir() ok = true, want false; rig = %+v", rig)
	}
}

// TestRigFromGitWorktreeResolvesPolecatWorktreeWithoutRedirect verifies that
// rig auto-detection recovers from a missing .beads/redirect by asking git
// for the worktree's main repository. This is the failure mode behind ga-cuk:
// polecat worktrees occasionally land beads in the HQ rig because their
// redirect file was lost or never created, and path-only resolution has no
// other signal that maps the worktree back to its rig.
func TestRigFromGitWorktreeResolvesPolecatWorktreeWithoutRedirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	cityDir := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "frontend-rig")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, rigRoot, "init", "-q")
	runGitForTest(t, rigRoot, "config", "user.email", "test@test.com")
	runGitForTest(t, rigRoot, "config", "user.name", "Test")
	runGitForTest(t, rigRoot, "commit", "--allow-empty", "-m", "init")

	// Polecat-style worktree under cityPath, deliberately created without
	// a .beads/redirect file — the case ga-cuk is fixing.
	worktree := filepath.Join(cityDir, ".gc", "worktrees", "frontend", "polecats", "polecat-1")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, rigRoot, "worktree", "add", "--detach", worktree)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: rigRoot, Prefix: "fr"},
		},
	}

	rig, ok, err := resolveRigForDir(cfg, cityDir, normalizePathForCompare(worktree))
	if err != nil {
		t.Fatalf("resolveRigForDir() error = %v", err)
	}
	if !ok {
		t.Fatalf("resolveRigForDir() ok = false; want frontend rig resolved via git worktree fallback")
	}
	if rig.Name != "frontend" {
		t.Fatalf("resolveRigForDir() rig.Name = %q, want %q", rig.Name, "frontend")
	}
}

// TestRigFromGitWorktreeIgnoresNonRigRepo verifies that the git-based
// fallback returns false when the worktree's main repo is not a configured
// rig. Without this guard, an unrelated git checkout would route bd commands
// to a rig it has nothing to do with.
func TestRigFromGitWorktreeIgnoresNonRigRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cityDir := t.TempDir()
	unrelatedRepo := filepath.Join(t.TempDir(), "unrelated")
	if err := os.MkdirAll(unrelatedRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, unrelatedRepo, "init", "-q")
	runGitForTest(t, unrelatedRepo, "config", "user.email", "test@test.com")
	runGitForTest(t, unrelatedRepo, "config", "user.name", "Test")
	runGitForTest(t, unrelatedRepo, "commit", "--allow-empty", "-m", "init")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join(t.TempDir(), "frontend-rig"), Prefix: "fr"},
		},
	}

	_, ok, err := rigFromGitWorktree(cfg, cityDir, normalizePathForCompare(unrelatedRepo))
	if err != nil {
		t.Fatalf("rigFromGitWorktree() error = %v", err)
	}
	if ok {
		t.Fatalf("rigFromGitWorktree() ok = true; want false for unrelated repo")
	}
}

// TestRigFromGitWorktreeIgnoresCityCheckout verifies that when cityPath itself
// is a git repo, rigFromGitWorktree does not treat it as a rig. The city
// checkout maps to the HQ scope, not to any rig.
func TestRigFromGitWorktreeIgnoresCityCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cityDir := t.TempDir()
	runGitForTest(t, cityDir, "init", "-q")
	runGitForTest(t, cityDir, "config", "user.email", "test@test.com")
	runGitForTest(t, cityDir, "config", "user.name", "Test")
	runGitForTest(t, cityDir, "commit", "--allow-empty", "-m", "init")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join(t.TempDir(), "frontend-rig"), Prefix: "fr"},
		},
	}
	subdir := filepath.Join(cityDir, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rigFromGitWorktree(cfg, cityDir, normalizePathForCompare(subdir)); err != nil || ok {
		t.Fatalf("rigFromGitWorktree() in city checkout = (_, %v, %v); want (_, false, nil)", ok, err)
	}
}

// TestRigFromGitWorktreeStub exercises the matching logic without invoking
// git, so the test runs in environments where git is not installed.
func TestRigFromGitWorktreeStub(t *testing.T) {
	rigRoot := filepath.Join(t.TempDir(), "frontend-rig")
	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: rigRoot, Prefix: "fr"},
		},
	}
	prev := gitCommonDirForDir
	t.Cleanup(func() { gitCommonDirForDir = prev })

	tests := []struct {
		name    string
		stub    func(string) (string, bool)
		wantOK  bool
		wantRig string
	}{
		{
			name:    "matches configured rig",
			stub:    func(string) (string, bool) { return filepath.Join(rigRoot, ".git"), true },
			wantOK:  true,
			wantRig: "frontend",
		},
		{
			name:   "git unavailable",
			stub:   func(string) (string, bool) { return "", false },
			wantOK: false,
		},
		{
			name:   "non-matching repo",
			stub:   func(string) (string, bool) { return filepath.Join(t.TempDir(), "other", ".git"), true },
			wantOK: false,
		},
		{
			name:   "skips city checkout",
			stub:   func(string) (string, bool) { return filepath.Join(cityDir, ".git"), true },
			wantOK: false,
		},
		{
			name:    "bare-style common dir without .git suffix",
			stub:    func(string) (string, bool) { return rigRoot, true },
			wantOK:  true,
			wantRig: "frontend",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gitCommonDirForDir = tt.stub
			rig, ok, err := rigFromGitWorktree(cfg, cityDir, normalizePathForCompare(t.TempDir()))
			if err != nil {
				t.Fatalf("rigFromGitWorktree() error = %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("rigFromGitWorktree() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && rig.Name != tt.wantRig {
				t.Fatalf("rigFromGitWorktree() rig.Name = %q, want %q", rig.Name, tt.wantRig)
			}
		})
	}
}

func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Strip git env vars so tests do not inherit ambient repo state.
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}
