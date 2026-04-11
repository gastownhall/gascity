package main

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// cityGitignoreEntries are the paths that gc init writes into .gitignore.
var cityGitignoreEntries = []string{".gc/", ".beads/", "hooks/", ".runtime/"}

// rigGitignoreEntries are the paths that gc rig add writes into
// the rig-scoped .gitignore.
var rigGitignoreEntries = []string{".beads/"}

// ensureGitignoreEntries is an idempotent append helper for .gitignore files.
// It reads the existing .gitignore at dir/.gitignore (if any), skips entries
// that are already present, and appends a "# Gas City" section for new ones.
// Preserves all existing content including user-added entries.
func ensureGitignoreEntries(fs fsys.FS, dir string, entries []string) error {
	gitignorePath := filepath.Join(dir, ".gitignore")

	existing, err := fs.ReadFile(gitignorePath)
	if err != nil {
		// File doesn't exist — start fresh.
		existing = nil
	}

	// Build a set of lines already present (trimmed, to handle trailing whitespace).
	presentLines := make(map[string]bool)
	for _, line := range strings.Split(string(existing), "\n") {
		presentLines[strings.TrimSpace(line)] = true
	}

	// Collect entries that need to be added.
	var newEntries []string
	for _, entry := range entries {
		if !presentLines[entry] {
			newEntries = append(newEntries, entry)
		}
	}

	if len(newEntries) == 0 {
		return nil // nothing to add
	}

	// Build the new content: existing + separator + section header + entries.
	var b strings.Builder
	if len(existing) > 0 {
		b.Write(existing)
		// Ensure there's a blank line before our section.
		if !strings.HasSuffix(string(existing), "\n") {
			b.WriteByte('\n')
		}
		if !strings.HasSuffix(string(existing), "\n\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("# Gas City\n")
	for _, entry := range newEntries {
		b.WriteString(entry)
		b.WriteByte('\n')
	}

	return fs.WriteFile(gitignorePath, []byte(b.String()), 0o644)
}
