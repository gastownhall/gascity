package splittest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// selfImportPath is the package this guard fences off.
const selfImportPath = "github.com/gastownhall/gascity/internal/beads/splittest"

// TestSplittestIsNotImportedByProductionCode fences the kit into test builds.
//
// splittest's files are not _test.go files — they cannot be, because other
// packages import them — yet they import "testing" and take a *testing.T. That
// is the net/http/httptest shape and it is the right one for a cross-package
// fixture, but it costs the compiler's natural boundary: nothing stops a
// production file from importing this package, linking testing into the gc
// binary, and building a store whose failure mode is t.Fatalf. Worse, the kit's
// whole job is to be STRICTER than production; a production path that ended up
// on a strict leaf would reject writes the real backend accepts.
//
// The mechanism is the one internal/rollout/boundary_test.go already uses for
// its import boundary and internal/testenv/lint_test.go already uses for a
// whole-tree scan: parse imports, name the offending file, fail the build. It
// is deliberately NOT a new entry in scripts/check-core-boundary.sh, whose
// checks are all about commercial coupling in the open-core boundary, and not a
// resourcecensus resource, which counts syntax-observable resources in _test.go
// files and by construction cannot see a non-test file's import.
//
// The rule is flat on purpose: only _test.go files may import splittest. A
// future test-support package with a genuine need is a deliberate edit here,
// not a heuristic ("package name ends in test") that decides for itself.
func TestSplittestIsNotImportedByProductionCode(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	selfDir := filepath.Join(root, filepath.FromSlash("internal/beads/splittest"))

	var offenders []string
	scanned := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipImportScanDir(path, root, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Dir(path) == selfDir {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// An unparseable file cannot be cleared, and a guard that passes
			// when it cannot evaluate manufactures false confidence.
			return err
		}
		scanned++
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) == selfImportPath {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				offenders = append(offenders, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scanning the module for splittest imports: %v", walkErr)
	}
	if scanned == 0 {
		t.Fatal("import-boundary guard scanned zero non-test Go files; it is not evaluating anything")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("non-test files import %s:\n  %s\n\n"+
			"splittest is a test-only fixture: it imports \"testing\", its constructors take *testing.T, "+
			"and its stores are deliberately stricter than the production backends. Move the usage into a "+
			"_test.go file, or promote the behavior you need into internal/beads proper.",
			selfImportPath, strings.Join(offenders, "\n  "))
	}
}

// skipImportScanDir prunes directories the scan must not descend into: vendored
// and generated trees, dot/underscore directories, and linked git worktrees
// checked out inside the repo (whose files belong to another branch entirely).
// Mirrors internal/testenv/lint_test.go's skipRepoLintDir plus its nested
// worktree detection.
func skipImportScanDir(path, root, name string) bool {
	// The root escape must come before the name rules, not after them.
	// filepath.WalkDir calls back on the root itself, so a checkout in a
	// directory named `.wt-something` matched the dot-prefix rule below and
	// returned SkipDir on the very first callback, skipping the whole scan.
	// Only the `scanned == 0` floor in the caller kept that from passing
	// silently. Whatever the root is named, it is always in scope (ga-xoioq).
	if path == root {
		return false
	}
	if name == "vendor" || name == "node_modules" || name == "testdata" {
		return true
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	if name == "worktrees" || strings.HasPrefix(name, "worktree-") {
		return true
	}
	// A linked worktree has a .git FILE (a "gitdir: ..." pointer) rather than a
	// .git directory, which catches worktrees whatever they are named.
	info, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil && !info.IsDir()
}

// TestImportScanRootIsNeverPruned pins the ordering inside skipImportScanDir:
// the walk root is in scope whatever it is named. See ga-xoioq.
func TestImportScanRootIsNeverPruned(t *testing.T) {
	t.Parallel()
	for _, root := range []string{
		"/data/projects/.wt-classroute",
		"/data/projects/_wt-govuln-public",
		"/data/projects/vendor",
		"/data/projects/testdata",
		"/data/projects/worktree-scratch",
	} {
		if skipImportScanDir(root, root, filepath.Base(root)) {
			t.Errorf("skipImportScanDir pruned the walk root %q; the entire import scan would be skipped", root)
		}
	}
}

// repoRoot walks up from the working directory to the module root. It reads the
// filesystem rather than shelling out to git: a `git rev-parse` here would be a
// subprocess in a test file, which the resource census counts and ratchets.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root above the working directory")
		}
		dir = parent
	}
}
