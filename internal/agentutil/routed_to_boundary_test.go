package agentutil

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoRoot returns the repository root by navigating from this file's location.
func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// TestRoutedToIdentityDerivationIsCentralized enforces that every gc.routed_to
// derivation (PoolName-first, falling back to QualifiedName()) flows through
// RoutedToIdentity. Reimplementing the check inline lets a pool instance's
// routing diverge from what gc sling stamped for its base template, making
// the bead invisible to pool demand/claim — this has independently regressed
// at multiple call sites (see ga-79uuwq, ga-s635qm).
//
// Not violations (allowed):
//   - internal/agentutil/resolve.go — RoutedToIdentity's own implementation.
//   - internal/config/workquery.go — poolDemandTarget, DefaultSlingQuery,
//     effectiveOnDeath, and effectiveOnBoot must inline the check: agentutil
//     imports config, so config cannot call back into agentutil without an
//     import cycle. This is a reviewed, permanent exception, not a pending
//     migration.
func TestRoutedToIdentityDerivationIsCentralized(t *testing.T) {
	root := repoRoot()

	allowedFiles := []string{
		filepath.Join("internal", "agentutil", "resolve.go"),
		filepath.Join("internal", "config", "workquery.go"),
	}

	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "vendor" || base == ".claude" || base == ".gc" || strings.HasPrefix(base, ".beads-src") {
				return filepath.SkipDir
			}
			// Skip git worktrees embedded in the repo (have a .git file, not
			// dir) — but never apply this to root itself. gc builder/reviewer/
			// deployer sessions run from inside a worktree, so root legitimately
			// has a .git file rather than a .git directory; skipping on that
			// condition here would SkipDir the walk's very first entry and
			// silently visit zero files.
			if path != root {
				if fi, serr := os.Stat(filepath.Join(path, ".git")); serr == nil && !fi.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, allowed := range allowedFiles {
			if rel == allowed {
				return nil
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, `PoolName != ""`) {
				violations = append(violations, rel+":"+itoa(lineNum)+": "+trimmed)
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("inline PoolName collapse found outside RoutedToIdentity (%d violations):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s", v)
		}
		t.Error("Call agentutil.RoutedToIdentity(agent) instead of reimplementing the PoolName-first collapse.")
	}
}

// itoa converts an int to a string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
