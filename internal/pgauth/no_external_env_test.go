package pgauth_test

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestNoDirectPostgresEnvReadsOutsidePgauth enforces the architectural
// invariant that every direct read of BEADS_POSTGRES_* and GC_POSTGRES_*
// lives inside internal/pgauth/. Slice 4's observability guarantees and
// slice 3's projection contract both depend on a single resolution point.
//
// Allowed locations:
//   - internal/pgauth/ — the resolver itself.
//   - scripts/ — build-time scripts may freely read env (not part of the
//     production binary).
//   - vendor/ — third-party code is out of scope.
func TestNoDirectPostgresEnvReadsOutsidePgauth(t *testing.T) {
	root := repoRoot(t)

	allowedPrefixes := []string{
		filepath.Join("internal", "pgauth") + string(filepath.Separator),
		"scripts" + string(filepath.Separator),
		"vendor" + string(filepath.Separator),
	}

	forbiddenSubstrings := forbiddenPostgresEnvReadSubstrings()

	var violations []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "vendor" || base == ".claude" || base == ".beads" || base == "worktrees" || strings.HasPrefix(base, ".beads-src") || strings.HasPrefix(base, "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, prefix := range allowedPrefixes {
			if strings.HasPrefix(rel, prefix) {
				return nil
			}
		}
		f, err := os.Open(path) //nolint:gosec // path comes from filepath.Walk over the repo root
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if containsForbiddenPostgresEnvRead(line, forbiddenSubstrings) {
				violations = append(violations, rel+":"+itoa(lineNum)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("direct BEADS_POSTGRES_* / GC_POSTGRES_* env reads may only appear under internal/pgauth/. Found:\n  %s", strings.Join(violations, "\n  "))
	}
}

func TestForbiddenPostgresEnvReadSubstringsCoverCommonAccessors(t *testing.T) {
	forbiddenSubstrings := forbiddenPostgresEnvReadSubstrings()
	cases := []string{
		`os.Getenv("BEADS_POSTGRES_`,
		`os.Getenv("GC_POSTGRES_`,
		`os.LookupEnv("BEADS_POSTGRES_`,
		`os.LookupEnv("GC_POSTGRES_`,
		`syscall.Getenv("BEADS_POSTGRES_`,
		`syscall.Getenv("GC_POSTGRES_`,
	}
	for _, line := range cases {
		if !containsForbiddenPostgresEnvRead(line, forbiddenSubstrings) {
			t.Fatalf("containsForbiddenPostgresEnvRead(%q) = false, want true", line)
		}
	}
}

func forbiddenPostgresEnvReadSubstrings() []string {
	return []string{
		`os.Getenv("BEADS_POSTGRES_`,
		`os.Getenv("GC_POSTGRES_`,
		`os.LookupEnv("BEADS_POSTGRES_`,
		`os.LookupEnv("GC_POSTGRES_`,
		`syscall.Getenv("BEADS_POSTGRES_`,
		`syscall.Getenv("GC_POSTGRES_`,
	}
}

func containsForbiddenPostgresEnvRead(line string, forbiddenSubstrings []string) bool {
	for _, sub := range forbiddenSubstrings {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// repoRoot returns the repository root by navigating from this file's
// location (internal/pgauth/no_external_env_test.go → repo root).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// itoa is a tiny helper to avoid pulling in strconv just for line numbers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
