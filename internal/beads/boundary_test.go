package beads_test

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

// TestNoBdExecOutsideBeads enforces the architectural invariant that all bd
// subprocess calls must live in internal/beads/. This prevents coupling sprawl
// and ensures all bd interactions go through the BdStore abstraction.
//
// Two categories of violations:
//  1. exec.Command("bd"...) or exec.CommandContext(..."bd"...) — direct subprocess calls
//  2. Variable assignments building bd command strings for shell execution
//     (e.g., cmd := "bd mol cook ...")
//
// Not violations (allowed):
//   - internal/beads/ — that's where bd calls belong
//   - test/integration/ — integration tests may use real bd for setup
//   - Config defaults returning bd command templates (WorkQuery, SlingQuery)
//   - Test fixture data (map keys, runner output, assertions)
//   - Binary existence checks (LookPath)
//   - Provider comparisons (== "bd", != "bd")
func TestNoBdExecOutsideBeads(t *testing.T) {
	root := repoRoot()

	// Directories where bd calls are allowed.
	allowedDirs := []string{
		filepath.Join("internal", "beads") + string(filepath.Separator),
		filepath.Join("internal", "deps") + string(filepath.Separator),   // version checks only (bd version)
		filepath.Join("internal", "doctor") + string(filepath.Separator), // health checks query bd config directly
		filepath.Join("internal", "dolt") + string(filepath.Separator),   // upstream-synced from gastown
		filepath.Join("test", "integration") + string(filepath.Separator),
		filepath.Join("cmd", "gc", "dashboard") + string(filepath.Separator), // dashboard server uses bd directly
	}

	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "vendor" || base == ".claude" || strings.HasPrefix(base, ".beads-src") {
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
		for _, dir := range allowedDirs {
			if strings.HasPrefix(rel, dir) {
				return nil
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		isTest := strings.HasSuffix(rel, "_test.go")
		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)

			// Skip comment-only lines.
			if strings.HasPrefix(trimmed, "//") {
				continue
			}

			// Category 1: exec.Command("bd"...) — direct subprocess call.
			if strings.Contains(line, `exec.Command("bd"`) {
				if !strings.Contains(line, `LookPath`) {
					violations = append(violations, rel+":"+itoa(lineNum)+": "+trimmed)
				}
			}

			// Category 1b: exec.CommandContext(ctx, "bd"...) — context subprocess call.
			if strings.Contains(line, `exec.CommandContext(`) && strings.Contains(line, `"bd"`) {
				violations = append(violations, rel+":"+itoa(lineNum)+": "+trimmed)
			}

			// Category 2: Variable assignment building a bd command string.
			// Catches: cmd := "bd mol cook --formula=" + ...
			// Skips: return "bd ready ..." (config defaults)
			// Skips: test files (fixture data)
			// Skips: fmt.Fprint*/fmt.Errorf (error messages)
			if !isTest && isBdCommandAssignment(trimmed) {
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
		t.Errorf("bd subprocess calls found outside internal/beads/ (%d violations):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s", v)
		}
		t.Error("Move these calls to internal/beads/bdstore.go behind the BdStore abstraction.")
	}
}

// isBdCommandAssignment detects lines that build bd command strings for shell
// execution via variable assignment. Returns true for patterns like:
//
//	cmd := "bd mol cook --formula=" + formulaName
//	routeCmd := fmt.Sprintf("bd update %s ...", id)
//
// Returns false for config defaults (return statements), error formatting,
// and other non-execution uses of "bd " strings.
func isBdCommandAssignment(trimmed string) bool {
	// Must be a variable assignment containing "bd ".
	if !strings.Contains(trimmed, `"bd `) {
		return false
	}
	// Must be an assignment (not a return, function call, etc.).
	if !strings.Contains(trimmed, `:=`) && !strings.Contains(trimmed, ` = `) {
		return false
	}
	// Exclude return statements (config default values).
	if strings.HasPrefix(trimmed, "return ") {
		return false
	}
	// Exclude error formatting and sling dispatch (Sprintf builds sling_query
	// commands for the SlingRunner pattern — architectural, not direct exec).
	if strings.Contains(trimmed, "Errorf") || strings.Contains(trimmed, "Fprint") ||
		strings.Contains(trimmed, "Sprintf") {
		return false
	}
	return true
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
