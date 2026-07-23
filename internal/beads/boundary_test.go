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

// bdExecAllowedDirs lists directories where bd calls are allowed.
var bdExecAllowedDirs = []string{
	filepath.Join("internal", "beads") + string(filepath.Separator),
	filepath.Join("internal", "deps") + string(filepath.Separator),   // version checks only (bd version)
	filepath.Join("internal", "doctor") + string(filepath.Separator), // health checks query bd config directly
	filepath.Join("internal", "dolt") + string(filepath.Separator),   // upstream-synced from gastown
	// env.ledger conformance probe execs `bd ready` INSIDE the provisioned
	// box (via the runtime exec op), to verify the session's bd can reach the
	// work ledger — a box-side capability probe, not a gc-side bd subprocess.
	filepath.Join("internal", "runtime", "runtimecapability") + string(filepath.Separator),
	filepath.Join("test", "integration") + string(filepath.Separator),
	// dashboard BFF runs read-only `bd doctor` health probes against
	// arbitrary per-rig .beads stores (supervisor-reported paths). This is
	// the same direct-bd usage the retired cmd/gc/dashboard server had.
	filepath.Join("internal", "api", "dashboardbff") + string(filepath.Separator),
}

// findBdExecViolations walks root looking for bd subprocess calls outside
// bdExecAllowedDirs. It skips vendored/cache directories, including any
// nested Go module (a directory other than root that owns its own go.mod) —
// an in-tree GOMODCACHE, the canonical layout on CI systems that require
// caches to live inside the checkout, otherwise gets walked as if it were
// first-party source and flags third-party (or bd's own vendored) sources
// as violations (#4480).
func findBdExecViolations(root string) ([]string, error) {
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
			// Skip git worktrees embedded in the repo (have a .git file, not dir).
			if fi, serr := os.Stat(filepath.Join(path, ".git")); serr == nil && !fi.IsDir() {
				return filepath.SkipDir
			}
			// Skip nested Go modules: any directory other than root that owns
			// its own go.mod is a separate module's source tree (a module-cache
			// entry), not part of this repo.
			if path != root {
				if _, serr := os.Stat(filepath.Join(path, "go.mod")); serr == nil {
					return filepath.SkipDir
				}
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
		for _, dir := range bdExecAllowedDirs {
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
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
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
	return violations, err
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
//     and command-name token consts (bdReadyOracleCommand = "bd ready")
//   - Test fixture data (map keys, runner output, assertions)
//   - Binary existence checks (LookPath)
//   - Provider comparisons (== "bd", != "bd")
func TestNoBdExecOutsideBeads(t *testing.T) {
	violations, err := findBdExecViolations(repoRoot())
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

// TestFindBdExecViolationsSkipsNestedGoModules pins the fix for #4480: an
// in-tree GOMODCACHE (the canonical CI layout when cache paths must live
// inside the checkout) previously got walked like first-party source,
// flagging third-party — or even bd's own vendored — sources as violations.
// A directory that owns its own go.mod is a separate module's source tree
// and must be skipped, while a real violation living directly in the
// checkout must still be caught.
func TestFindBdExecViolationsSkipsNestedGoModules(t *testing.T) {
	root := t.TempDir()

	mustWriteFile(t, filepath.Join(root, "go.mod"), "module example.com/fixture\n")

	// A real violation, directly in the checkout, outside any allowed dir.
	mustWriteFile(t, filepath.Join(root, "cmd", "gc", "example.go"),
		"package main\n\nfunc run() { exec.Command(\"bd\", \"prime\") }\n")

	// A nested module (module-cache-shaped): its own go.mod plus a source
	// file containing the exact same call pattern. Must be skipped entirely.
	nestedModule := filepath.Join(root, ".cache", "go-mod", "github.com", "steveyegge", "beads@v1.1.0")
	mustWriteFile(t, filepath.Join(nestedModule, "go.mod"), "module github.com/steveyegge/beads\n")
	mustWriteFile(t, filepath.Join(nestedModule, "cmd", "bd", "doctor", "claude.go"),
		"package doctor\n\nfunc run() { exec.Command(\"bd\", \"prime\") }\n")

	violations, err := findBdExecViolations(root)
	if err != nil {
		t.Fatalf("findBdExecViolations: %v", err)
	}

	if len(violations) != 1 {
		t.Fatalf("violations = %v, want exactly 1 (the real violation, nested module skipped)", violations)
	}
	if !strings.Contains(violations[0], filepath.Join("cmd", "gc", "example.go")) {
		t.Fatalf("violations[0] = %q, want the cmd/gc/example.go violation", violations[0])
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
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
	// Category 2 targets commands BUILT for shell execution — dynamic arguments
	// concatenated onto a "bd " prefix (cmd := "bd mol cook --formula=" + name).
	// A complete standalone literal with no concatenation is a command-name
	// token or config default (e.g. bdReadyOracleCommand = "bd ready", the
	// ready-oracle template used for query rewriting), which this invariant
	// explicitly allows — so require a concatenation before flagging.
	if !strings.Contains(trimmed, "+") {
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
