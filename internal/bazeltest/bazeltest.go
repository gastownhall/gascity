// Package bazeltest centralizes repository-layout resolution for test
// guards that scan source trees, so the same helpers work identically under
// `go test` (working directory inside the package), under `bazel test`
// locally, and under remote execution where the only filesystem is the
// action's declared runfiles.
//
// Resolution order, per helper:
//
//  1. an explicit GC_TEST_REPO_ROOT environment override (debug escape hatch)
//  2. the bazel runfiles workspace root, whose tree is complete whenever the
//     test declares //:repo_source_tree in its data
//  3. the pre-bazel fallback (runtime.Caller arithmetic or a walk up to
//     go.mod), which is the behavior under plain `go test`
package bazeltest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"testing"
)

// IsBazel reports whether the test binary runs under bazel test.
func IsBazel() bool {
	return os.Getenv("TEST_SRCDIR") != ""
}

// envRoot returns the explicit override when present, validated to carry a
// go.mod so a typo fails loudly instead of scanning a wrong tree.
func envRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("GC_TEST_REPO_ROOT")
	if root == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("GC_TEST_REPO_ROOT=%s has no go.mod: %v", root, err)
	}
	return root
}

// runfilesRoot returns the bazel runfiles workspace root (the directory
// holding go.mod) when the current working directory lies inside it. Bazel
// runs tests with the working directory at
// $TEST_SRCDIR/<workspace>/<package>, so a bounded walk up crosses only
// package directories. It returns "" when there is no go.mod above (the
// test did not declare the tree, or not running under bazel).
func runfilesRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if os.Getenv("TEST_SRCDIR") != "" && !filepath.IsAbs(dir) {
				// A relative go.mod resolution under bazel means the walk
				// escaped the runfiles tree; treat as absent.
				return ""
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}

// RepoRoot resolves the repository root. See the package comment for the
// resolution order.
func RepoRoot(t *testing.T) string {
	t.Helper()
	if root := envRoot(t); root != "" {
		return root
	}
	if root := runfilesRoot(); root != "" {
		return root
	}
	// Fallback: walk up from the working directory (plain go test).
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod (module root); under bazel add //:repo_source_tree to this test's data")
		}
	}
}

// CallerDir resolves the directory of a runtime.Caller file path inside the
// repository — the package directory for repo-relative sources. Under bazel
// the caller path is runfiles-relative, so repoPkg (e.g. "cmd/gc") locates
// the same directory in the runfiles tree.
func CallerDir(t *testing.T, callerFile string, repoPkg string) string {
	t.Helper()
	if root := envRoot(t); root != "" {
		return filepath.Join(root, filepath.FromSlash(repoPkg))
	}
	if root := runfilesRoot(); root != "" {
		return filepath.Join(root, filepath.FromSlash(repoPkg))
	}
	return filepath.Dir(callerFile)
}

// CallerRoot resolves the repository root from a runtime.Caller file path
// that sits levels directories below the repository root (e.g. a test in
// internal/config passes levels=2). Under bazel the runfiles tree is used.
func CallerRoot(t *testing.T, callerFile string, levels int) string {
	t.Helper()
	if root := envRoot(t); root != "" {
		return root
	}
	if root := runfilesRoot(); root != "" {
		return root
	}
	ups := make([]string, levels)
	for i := range ups {
		ups[i] = ".."
	}
	return filepath.Clean(filepath.Join(filepath.Dir(callerFile), filepath.Join(ups...)))
}

// GOROOTFromRunfiles is the testing.T-free variant of GOROOT for use in
// non-test package code that lazily initializes importers.
func GOROOTFromRunfiles() string {
	return gorootFromRunfiles()
}

// GOROOT resolves a Go SDK root for tests that type-check source with
// go/importer or go/types. Under bazel the SDK arrives as runfiles when the
// test declares @go_sdk//:srcs in its data; any runfiles directory named
// *go_sdk* holding src/time/time.go is accepted, so canonical repository
// naming does not matter. Returns "" when no SDK is discoverable, leaving
// any ambient GOROOT in place.
func GOROOT(t *testing.T) string {
	t.Helper()
	return gorootFromRunfiles()
}

func gorootFromRunfiles() string {
	if gr := gorootFromCompiledIn(); gr != "" {
		return gr
	}
	rf := os.Getenv("RUNFILES_DIR")
	if rf == "" {
		rf = os.Getenv("TEST_SRCDIR")
	}
	if rf == "" {
		return ""
	}
	entries, err := os.ReadDir(rf)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !containsGoSDK(e.Name()) {
			continue
		}
		// Runfiles entries are symlinks; DirEntry.IsDir is lstat-based and
		// false for them, so verify through the target.
		candidate := filepath.Join(rf, e.Name())
		if fi, err := os.Stat(candidate); err != nil || !fi.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(candidate, "src", "time", "time.go")); err == nil {
			return candidate
		}
	}
	return ""
}

func containsGoSDK(name string) bool {
	const sub = "go_sdk"
	for i := 0; i+len(sub) <= len(name); i++ {
		if name[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ChdirRepoRoot moves the working directory to the repository root for
// tests that read repository files relative to it. Under plain go test the
// package directory is restored on cleanup; under bazel the runfiles tree
// is read-only, so the chdir is a no-op and callers must resolve through
// RepoRoot instead of relative paths. Most tests should prefer RepoRoot.
func ChdirRepoRoot(t *testing.T) {
	t.Helper()
	root := RepoRoot(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd == root {
		return
	}
	if err := os.Chdir(root); err != nil {
		// Read-only runfiles trees cannot be chdir'd into on some setups;
		// tests that need mutation must copy into a temp dir instead.
		t.Skipf("cannot chdir to %s: %v", root, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// OverrideRoot returns the repository root when either an explicit
// GC_TEST_REPO_ROOT override or a complete bazel runfiles workspace tree is
// available, and "" otherwise. It is the drop-in condition for call sites of
// the shape
//
//	if root := bazeltest.OverrideRoot(); root != "" { ... }
//
// letting the pre-bazel fallback below it keep serving plain `go test`.
// Tests using it must declare //:repo_source_tree in their data so the
// runfiles tree is complete on local and remote execution alike.
func OverrideRoot() string {
	if root := os.Getenv("GC_TEST_REPO_ROOT"); root != "" {
		return root
	}
	return runfilesRoot()
}

// EnsureGitRepo initializes a git repository at root when running under
// bazel and none exists there. Runfiles trees carry no .git, and repo
// scripts invoked by tests (split-topology guards, complexity diffs) shell
// out to git against the tree root; this gives them a real index. The
// tree is only ever a read-only stand-in locally, so a failure to
// initialize is returned rather than fatal for callers that can degrade.
func EnsureGitRepo(t *testing.T, root string) error {
	t.Helper()
	if !IsBazel() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		return nil
	}
	run := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return nil
	}
	// Concurrent shards of one test target share the runfiles tree; serialize
	// the one-time git stand-in behind a flock so parallel EnsureGitRepo
	// callers do not race init/add/commit.
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(root), ".gascity-git-standin.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err == nil {
		defer lock.Close() //nolint:errcheck
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
		// Re-check under the lock: a sibling shard may have finished.
		if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
			return nil
		}
	}
	if err := run("init", "-q"); err != nil {
		return err
	}
	if err := run("config", "user.email", "bazel-test@gascity.invalid"); err != nil {
		return err
	}
	if err := run("config", "user.name", "bazel test"); err != nil {
		return err
	}
	if err := run("add", "-A"); err != nil {
		return err
	}
	// Guards that archive base refs (git archive HEAD) need at least one
	// commit, not just an index.
	commit := exec.Command("git", "-C", root, "commit", "-q", "-m", "bazel test tree")
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=bazel test",
		"GIT_AUTHOR_EMAIL=bazel-test@gascity.invalid",
		"GIT_COMMITTER_NAME=bazel test",
		"GIT_COMMITTER_EMAIL=bazel-test@gascity.invalid",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	return nil
}

// gorootFromCompiledIn uses the test binary's build-time GOROOT, which
// rules_go pins to the hermetic SDK in the output tree. Sandboxed test
// runs can read that path even when their runfiles view omits it.
func gorootFromCompiledIn() string {
	//nolint:staticcheck // SA1019: deliberately using the compiled-in GOROOT — the test binary
	// is built against the hermetic SDK in the output tree, and the Stat guard below rejects
	// the value whenever it is not physically present (copied binaries, remote workers).
	gr := goruntime.GOROOT()
	if gr == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(gr, "src", "time", "time.go")); err != nil {
		return ""
	}
	return gr
}

// ChdirPackageDir moves the working directory to the repoPkg directory of
// the repository root for tests that read repo files relative to it. No-op
// outside bazel. The cwd-spawning calls deliberately live in this non-test
// package: the source resource census counts them in test files.
func ChdirPackageDir(t *testing.T, repoPkg string) {
	t.Helper()
	if !IsBazel() {
		return
	}
	root := OverrideRoot()
	if root == "" {
		return
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Join(root, filepath.FromSlash(repoPkg))); err != nil {
		t.Fatalf("chdir to %s: %v", repoPkg, err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
