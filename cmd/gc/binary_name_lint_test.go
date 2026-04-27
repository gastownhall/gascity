package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHardcodedGCInErrorMessages walks all non-test *.go files in cmd/gc/
// and finds string literals containing hardcoded "gc " binary name references
// that should use prog(), cmdName(), or cmdErr() instead.
//
// This test acts as a ratchet: the violation count can go DOWN (migration
// progress) but not UP (no new hardcoded references). Update maxViolations
// as migration proceeds.
func TestNoHardcodedGCInErrorMessages(t *testing.T) {
	// Ratchet threshold — set to current count. Lower this as files are migrated.
	const maxViolations = 1159

	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading cmd/gc directory: %v", err)
	}

	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip test files — they legitimately reference "gc " in assertions.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			lineNo := i + 1
			trimmed := strings.TrimSpace(line)

			// Skip comments.
			if strings.HasPrefix(trimmed, "//") {
				continue
			}

			// Check for hardcoded binary name patterns.
			if !containsHardcodedGC(line) {
				continue
			}

			// Allowlist: patterns that are NOT the binary name.
			if isAllowlisted(line, name, lineNo) {
				continue
			}

			violations = append(violations, violation{
				file: name,
				line: lineNo,
				text: trimmed,
			})
		}
	}

	count := len(violations)
	t.Logf("found %d hardcoded binary name references (threshold: %d)", count, maxViolations)

	if count > maxViolations {
		// Print first 20 violations for context.
		limit := 20
		if count < limit {
			limit = count
		}
		for _, v := range violations[:limit] {
			t.Logf("  %s:%d: %s", v.file, v.line, v.text)
		}
		if count > limit {
			t.Logf("  ... and %d more", count-limit)
		}
		t.Fatalf("found %d hardcoded binary name references (max allowed: %d); use prog(), cmdName(), or cmdErr() instead", count, maxViolations)
	}

	if count > 0 {
		t.Logf("migration progress: %d/%d remaining (%.0f%% done)",
			count, maxViolations, 100*(1-float64(count)/float64(maxViolations)))
	}
}

// containsHardcodedGC checks whether a line contains patterns indicating
// a hardcoded "gc" binary name reference.
func containsHardcodedGC(line string) bool {
	// "gc " — gc followed by space (command reference in strings)
	if strings.Contains(line, `"gc `) {
		return true
	}
	// Strings starting with "gc: (error prefix)
	if strings.Contains(line, `"gc:`) {
		return true
	}
	return false
}

// isAllowlisted returns true if the line matches a known-safe pattern
// that is NOT a hardcoded binary name.
func isAllowlisted(line, filename string, lineNo int) bool {
	// .gc/ runtime directory references
	if strings.Contains(line, `".gc`) {
		return true
	}
	// gc- bead ID prefixes
	if strings.Contains(line, `"gc-`) {
		return true
	}
	// GC_ environment variable prefix
	if strings.Contains(line, `"GC_`) {
		return true
	}
	// Import paths and module names
	if strings.Contains(line, `"gascity`) || strings.Contains(line, `"gastownhall`) {
		return true
	}
	if strings.Contains(line, `gascity`) || strings.Contains(line, `gastownhall`) {
		return true
	}
	// The binaryName default in progname.go itself
	if filename == "progname.go" && strings.Contains(line, `binaryName`) {
		return true
	}
	// gc.test — test binary detection
	if strings.Contains(line, `"gc.test`) {
		return true
	}
	return false
}
