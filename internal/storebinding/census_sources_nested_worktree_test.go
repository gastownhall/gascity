package storebinding

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCensusSourcesFromExcludesUntrackedNestedWorktree pins the false-fail
// this bead exists to close (ga-5em): a nested sibling git worktree
// registered under the scanned root — the common gitignored
// worktrees/<bead> pool-slot pattern — must never contribute duplicate
// source to the publication census. Its files live in the nested
// worktree's own git index, never in this tree's, so a walk scoped to this
// tree's tracked files must not see them.
func TestCensusSourcesFromExcludesUntrackedNestedWorktree(t *testing.T) {
	root := t.TempDir()
	runCensusFixtureGit(t, root, "init", "-q")
	runCensusFixtureGit(t, root, "config", "user.email", "fixture@example.com")
	runCensusFixtureGit(t, root, "config", "user.name", "fixture")

	writeCensusFixtureFile(t, root, "tracked.go", "package fixture\n")
	runCensusFixtureGit(t, root, "add", "tracked.go")

	writeCensusFixtureFile(t, root, filepath.Join("worktrees", "sibling", "tracked.go"), "package fixture\n")

	got := censusSourcesFrom(t, root)
	var rels []string
	for _, source := range got {
		rels = append(rels, source.rel)
	}
	if len(rels) != 1 || rels[0] != "tracked.go" {
		t.Fatalf("censusSourcesFrom(%s) = %v, want exactly [\"tracked.go\"]; the walk must be scoped to git-tracked files so a nested sibling worktree's checkout is never counted", root, rels)
	}
}

func runCensusFixtureGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeCensusFixtureFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
