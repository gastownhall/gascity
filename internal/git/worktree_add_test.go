package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRepoWithTrackedFile commits tracked.txt with the given content to repo so
// that a checkout into a new worktree has observable content, returning the
// committed HEAD sha.
func seedRepoWithTrackedFile(t *testing.T, repo, content string) string {
	t.Helper()
	const name = "tracked.txt"
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", name)
	runGit(t, repo, "commit", "-m", "add "+name)
	g := New(repo)
	head, err := g.run("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(head)
}

func TestWorktreeAdd_FreshPath(t *testing.T) {
	repo := initTestRepo(t)
	head := seedRepoWithTrackedFile(t, repo, "hello")
	g := New(repo)

	wt := filepath.Join(t.TempDir(), "fresh")
	if err := g.WorktreeAdd(wt, head, true); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "tracked.txt"))
	if err != nil {
		t.Fatalf("reading checked-out file: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("tracked.txt = %q, want %q", got, "hello")
	}
}

func TestWorktreeAdd_EmptyExistingDir(t *testing.T) {
	repo := initTestRepo(t)
	head := seedRepoWithTrackedFile(t, repo, "hello")
	g := New(repo)

	wt := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.WorktreeAdd(wt, head, true); err != nil {
		t.Fatalf("WorktreeAdd into empty existing dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "tracked.txt")); err != nil {
		t.Errorf("tracked file missing after add: %v", err)
	}
}

// TestWorktreeAdd_PreSeededDir is the core case: pool member slots already
// contain seed files (.claude/.gc), and git worktree add refuses a non-empty
// existing directory even with --force. The primitive must evacuate the seed,
// populate the worktree, then restore the seed on top.
func TestWorktreeAdd_PreSeededDir(t *testing.T) {
	repo := initTestRepo(t)
	head := seedRepoWithTrackedFile(t, repo, "hello")
	g := New(repo)

	wt := filepath.Join(t.TempDir(), "seeded")
	if err := os.MkdirAll(filepath.Join(wt, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".gc", "marker"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := g.WorktreeAdd(wt, head, true); err != nil {
		t.Fatalf("WorktreeAdd into pre-seeded dir: %v", err)
	}
	// Tracked checkout landed.
	if _, err := os.Stat(filepath.Join(wt, "tracked.txt")); err != nil {
		t.Errorf("tracked file missing after add into seeded dir: %v", err)
	}
	// Seed survived.
	marker, err := os.ReadFile(filepath.Join(wt, ".gc", "marker"))
	if err != nil {
		t.Fatalf("seed marker lost: %v", err)
	}
	if string(marker) != "seed" {
		t.Errorf("seed marker = %q, want %q", marker, "seed")
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude")); err != nil {
		t.Errorf("seed .claude dir lost: %v", err)
	}
}

func TestWorktreeAdd_DetachHead(t *testing.T) {
	repo := initTestRepo(t)
	head := seedRepoWithTrackedFile(t, repo, "hello")

	wt := filepath.Join(t.TempDir(), "detached")
	if err := New(repo).WorktreeAdd(wt, head, true); err != nil {
		t.Fatalf("WorktreeAdd(detach): %v", err)
	}
	branch, err := New(wt).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "HEAD" {
		t.Errorf("branch = %q, want detached HEAD", branch)
	}
}

// TestWorktreeAdd_SeedCollisionKeepsTracked verifies that when a seed entry
// collides with a tracked file, the checked-out (tracked) content wins — the
// seed copy must not clobber repository state.
func TestWorktreeAdd_SeedCollisionKeepsTracked(t *testing.T) {
	repo := initTestRepo(t)
	head := seedRepoWithTrackedFile(t, repo, "from-repo")
	g := New(repo)

	wt := filepath.Join(t.TempDir(), "collide")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a file with the same name as a tracked file but different content.
	if err := os.WriteFile(filepath.Join(wt, "tracked.txt"), []byte("from-seed"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := g.WorktreeAdd(wt, head, true); err != nil {
		t.Fatalf("WorktreeAdd with colliding seed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-repo" {
		t.Errorf("tracked.txt = %q, want repo content to win", got)
	}
}

func TestWorktreeAdd_Validation(t *testing.T) {
	g := New(initTestRepo(t))
	if err := g.WorktreeAdd("", "HEAD", true); err == nil {
		t.Error("WorktreeAdd(empty path) = nil, want error")
	}
	if err := g.WorktreeAdd(filepath.Join(t.TempDir(), "x"), "", true); err == nil {
		t.Error("WorktreeAdd(empty ref) = nil, want error")
	}
}
