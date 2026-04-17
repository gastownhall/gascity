//go:build acceptance_a

// Static analysis test: every `exit` in example prompts and formulas
// must have a preceding `gc runtime drain-ack`.
//
// This is a regression test for the drain-ack audit performed on
// 2026-03-18, where bare `exit` calls were found in 14 files across
// gastown, maintenance, dolt, and swarm packs.
package acceptance_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDrainAckBeforeExit scans all .md.tmpl and formula .toml files
// (canonical .toml or legacy .formula.toml) in examples/ for exit lines
// inside code blocks, and verifies each has `gc runtime drain-ack` on
// the preceding line.
//
// Matches: exit, exit 0, exit 1, exit $? — any line starting with
// "exit" followed by whitespace or end-of-line.
func TestDrainAckBeforeExit(t *testing.T) {
	root := filepath.Join(helpers_FindModuleRoot(t), "examples")

	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		isPrompt := strings.HasSuffix(name, ".md.tmpl")
		// Formula files under examples/*/formulas/ may use either the
		// canonical .toml extension or the legacy .formula.toml extension
		// during the infix migration window. Match both.
		inFormulasDir := strings.Contains(filepath.ToSlash(path), "/formulas/")
		isFormula := strings.HasSuffix(name, ".formula.toml") ||
			(inFormulasDir && strings.HasSuffix(name, ".toml"))
		if !isPrompt && !isFormula {
			return nil
		}

		v := checkFileForBareExit(t, path, root, isFormula)
		violations = append(violations, v...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking examples: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("found %d exit calls without drain-ack:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

func checkFileForBareExit(t *testing.T, path, root string, isToml bool) []string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()

	rel, _ := filepath.Rel(root, path)
	var violations []string
	var prevLine string
	lineNum := 0
	inCodeBlock := false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if !isToml {
			// Markdown: track code block boundaries.
			if strings.HasPrefix(trimmed, "```") {
				inCodeBlock = !inCodeBlock
				prevLine = trimmed
				continue
			}
			if !inCodeBlock {
				prevLine = trimmed
				continue
			}
		}
		// For TOML formula files, scan all lines (exit commands appear
		// inside triple-quoted description strings, not markdown fences).

		// Match exit commands: "exit", "exit 0", "exit 1", "exit $?", etc.
		// Exclude: "exit_code", "exit_status", "Exit criteria:", comments.
		if isExitCommand(trimmed) {
			prevTrimmed := strings.TrimSpace(prevLine)
			if !strings.Contains(prevTrimmed, "drain-ack") {
				violations = append(violations,
					"  "+rel+":"+strconv.Itoa(lineNum)+": exit without drain-ack (prev: "+prevTrimmed+")")
			}
		}

		prevLine = trimmed
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning %s: %v", path, err)
	}

	return violations
}

// isExitCommand returns true for shell exit commands but not for
// variable names, prose, or comments containing "exit".
func isExitCommand(line string) bool {
	// Must start with "exit" (not a substring like "exit_code").
	if !strings.HasPrefix(line, "exit") {
		return false
	}
	// "exit" alone.
	if line == "exit" {
		return true
	}
	// "exit" followed by whitespace or end (exit 0, exit 1, exit $?).
	if len(line) > 4 && (line[4] == ' ' || line[4] == '\t') {
		// Exclude prose like "Exit criteria:" or "exit status".
		rest := strings.TrimSpace(line[4:])
		if rest == "" {
			return true
		}
		// Numeric exit codes or shell vars are commands.
		if rest[0] >= '0' && rest[0] <= '9' {
			return true
		}
		if rest[0] == '$' {
			return true
		}
	}
	return false
}

// helpers_FindModuleRoot finds go.mod walking up from cwd.
// Named with prefix to avoid collision with worktree_test.go's version.
func helpers_FindModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
