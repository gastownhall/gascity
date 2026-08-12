package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// buildRecipe extracts the command lines of the Makefile's `build` target.
func buildRecipe(t *testing.T, makefile string) string {
	t.Helper()
	recipe := regexp.MustCompile(`(?m)^build:\n((?:\t[^\n]+\n?)+)`).FindStringSubmatch(makefile)
	if len(recipe) != 2 {
		t.Fatal("Makefile has no build target with a command recipe")
	}
	return recipe[1]
}

func readMakefile(t *testing.T) string {
	t.Helper()
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	return string(makefile)
}

// TestBuildTargetDisablesToolchainVCSStamping asserts that `make build` keeps
// the Go toolchain out of the provenance business.
//
// Go's buildvcs identifies a repository by looking for a `.git` *directory*.
// Inside a git worktree `.git` is a gitdir *file*, so the toolchain does not
// recognize the worktree as a repository root and keeps walking up the
// filesystem. Polecat worktrees live under the city directory, which is itself
// a git repo, so buildvcs finds the *city's* `.git` and stamps the city's
// commit and the city's dirtiness into gc — a pristine gascity checkout gets
// labeled with an unrelated repository's revision and a false `-dirty`
// (ga-u7fb).
//
// The Makefile's own VERSION/COMMIT/DIRTY variables run git in the build
// directory, which resolves the gitdir pointer correctly and is therefore
// right regardless of how the worktree is nested. Passing -buildvcs=false
// makes those variables the single source of truth instead of leaving two
// competing stamps in the binary, one of which can describe another project.
func TestBuildTargetDisablesToolchainVCSStamping(t *testing.T) {
	recipe := buildRecipe(t, readMakefile(t))
	if !strings.Contains(recipe, "go build") {
		t.Fatalf("build recipe does not invoke go build:\n%s", recipe)
	}
	if !strings.Contains(recipe, "-buildvcs=false") {
		t.Fatalf("build recipe must pass -buildvcs=false so the Go toolchain cannot stamp an "+
			"enclosing repository's commit into gc when built from a git worktree (ga-u7fb):\n%s", recipe)
	}
}

// TestBuildStampsWorkingTreeDirtiness asserts that disabling buildvcs did not
// silently drop dirty detection along with the bogus stamp. `gc version
// --long`, the supervisor's /health build_id, and binary-drift detection all
// read the same injected commit string, so the `-dirty` marker has to come
// from somewhere — and with buildvcs off, that somewhere is git run in the
// build directory.
func TestBuildStampsWorkingTreeDirtiness(t *testing.T) {
	makefile := readMakefile(t)

	dirty := regexp.MustCompile(`(?m)^DIRTY\s*:?=\s*(.+)$`).FindStringSubmatch(makefile)
	if len(dirty) != 2 {
		t.Fatal("Makefile defines no DIRTY variable; buildvcs is off, so nothing would mark a dirty build")
	}
	if !strings.Contains(dirty[1], "git status --porcelain") {
		t.Fatalf("DIRTY must be derived from `git status --porcelain` in the build directory, got: %s", dirty[1])
	}
	if !strings.Contains(dirty[1], "-dirty") {
		t.Fatalf("DIRTY must expand to the -dirty suffix consumers already parse, got: %s", dirty[1])
	}

	ldflags := regexp.MustCompile(`(?ms)^LDFLAGS\s*:?=\s*(.*?)\n\n`).FindStringSubmatch(makefile)
	if len(ldflags) != 2 {
		t.Fatal("Makefile has no LDFLAGS assignment")
	}
	commitFlag := regexp.MustCompile(`-X main\.commit=(\S+)`).FindStringSubmatch(ldflags[1])
	if len(commitFlag) != 2 {
		t.Fatalf("LDFLAGS does not inject main.commit:\n%s", ldflags[1])
	}
	if !strings.Contains(commitFlag[1], "$(DIRTY)") {
		t.Fatalf("main.commit must carry $(DIRTY) so a modified tree is visible in `gc version --long`, got: %s", commitFlag[1])
	}
}
