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

// TestDockerCityVersionIsData rejects evaluating tag text as recipe shell code.
func TestDockerCityVersionIsData(t *testing.T) {
	for _, tag := range []string{"", "v1.2.3", "v1.2.3;#literal", "v1.2.3${GC_TEST_UNUSED}"} {
		t.Run(tag, func(t *testing.T) {
			tmp := t.TempDir()
			writeExecutable(t, filepath.Join(tmp, "git"), `#!/bin/sh
case "$1" in
  describe) [ -n "$GC_TEST_TAG" ] || exit 1; printf '%s' "$GC_TEST_TAG" ;;
  rev-parse) printf 'abcdef123' ;;
  show) printf '2026-09-05T00:00:00Z' ;;
  status) exit 0 ;;
  *) exit 1 ;;
esac
`)
			writeExecutable(t, filepath.Join(tmp, "docker"), `#!/bin/sh
printf '%s\n' "$@" > "$GC_TEST_ARGS"
printf '%s' "$GC_VERSION" > "$GC_TEST_VERSION"
`)
			argsPath := filepath.Join(tmp, "args")
			versionPath := filepath.Join(tmp, "version")
			cmd := makeCommand("--no-print-directory", "-f", filepath.Join(repoRoot(t), "Makefile"), "-o", "docker-base", "docker-city")
			cmd.Dir = tmp
			cmd.Env = []string{
				"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + tmp, "GC_TEST_TAG=" + tag,
				"GC_TEST_ARGS=" + argsPath, "GC_TEST_VERSION=" + versionPath,
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make docker-city: %v\n%s", err, out)
			}
			want := strings.TrimPrefix(tag, "v")
			if tag == "" {
				want = "dev"
			}
			version, err := os.ReadFile(versionPath)
			if err != nil || string(version) != want {
				t.Fatalf("exported version = %q, want %q; err=%v", version, want, err)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil || !strings.Contains(string(args), "--build-arg\nGC_VERSION\n") {
				t.Fatalf("Docker must take GC_VERSION from the environment: %s; err=%v", args, err)
			}
		})
	}
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

// TestBuildTimeIsSourceDerivedNotWallClock asserts that BUILD_TIME is a
// function of the source tree (the HEAD commit's date), not the wall clock.
//
// ldflags participate in the link step's cache key. A wall-clock BUILD_TIME
// changes on every invocation, so `-X main.date=$(BUILD_TIME)` guarantees a
// cache miss on every link even when the source is byte-identical to the
// build that just ran — each miss stores a fresh ~270MB binary in the shared
// GOCACHE that nothing will ever reuse (ga-vx62cc). The commit date is
// stable for a given tree, so identical-source relinks become cache hits,
// and this is also the standard reproducible-build posture (cf.
// SOURCE_DATE_EPOCH).
func TestBuildTimeIsSourceDerivedNotWallClock(t *testing.T) {
	makefile := readMakefile(t)

	buildTime := regexp.MustCompile(`(?m)^BUILD_TIME\s*:?=\s*(.+)$`).FindStringSubmatch(makefile)
	if len(buildTime) != 2 {
		t.Fatal("Makefile defines no BUILD_TIME variable")
	}
	expr := buildTime[1]

	if strings.Contains(expr, "date -u") {
		t.Fatalf("BUILD_TIME must not be derived from the wall clock (date -u) — that "+
			"changes on every invocation and makes the link step's ldflags cache key "+
			"guaranteed to miss even for byte-identical source, got: %s", expr)
	}
	if !strings.Contains(expr, "git") || !strings.Contains(expr, "show") {
		t.Fatalf("BUILD_TIME must be derived from `git show` on HEAD so it is a function "+
			"of the source tree, not the clock, got: %s", expr)
	}
	if !strings.Contains(expr, "%cI") {
		t.Fatalf("BUILD_TIME must use the commit date format %%cI (ISO 8601), got: %s", expr)
	}
	if !strings.Contains(expr, "HEAD") {
		t.Fatalf("BUILD_TIME must read the HEAD commit's date, got: %s", expr)
	}
	if !strings.Contains(expr, "unknown") {
		t.Fatalf("BUILD_TIME must fall back to a literal value when git metadata is "+
			"unavailable (e.g. a non-git build context), got: %s", expr)
	}
}
