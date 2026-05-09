package main

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// cityGitignoreEntries are the paths that gc init writes into .gitignore.
var cityGitignoreEntries = []string{".gc/", ".beads/*", "!.beads/config.yaml", "!.beads/metadata.json", "!.beads/identity.toml", "hooks/", ".runtime/"}

// rigGitignoreEntries are the paths that gc rig add writes into
// the rig-scoped .gitignore.
var rigGitignoreEntries = []string{".beads/*", "!.beads/config.yaml", "!.beads/metadata.json", "!.beads/identity.toml"}

func usesCanonicalBeadsEntries(entries []string) bool {
	for _, entry := range entries {
		if entry == ".beads/*" {
			return true
		}
	}
	return false
}

func isLegacyWholeBeadsIgnore(line string) bool {
	switch strings.TrimSpace(line) {
	case ".beads", ".beads/", "/.beads", "/.beads/":
		return true
	default:
		return false
	}
}

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

	upgradeCanonicalBeads := usesCanonicalBeadsEntries(entries)

	existingLines := strings.Split(string(existing), "\n")
	cleanedLines := make([]string, 0, len(existingLines))
	presentLines := make(map[string]bool)
	removedLegacyBeadsIgnore := false
	for _, line := range existingLines {
		trimmed := strings.TrimSpace(line)
		if upgradeCanonicalBeads && isLegacyWholeBeadsIgnore(trimmed) {
			removedLegacyBeadsIgnore = true
			continue
		}
		cleanedLines = append(cleanedLines, line)
		presentLines[trimmed] = true
	}
	cleanedExisting := strings.Join(cleanedLines, "\n")

	// Collect entries that need to be added.
	var newEntries []string
	for _, entry := range entries {
		if !presentLines[entry] {
			newEntries = append(newEntries, entry)
		}
	}

	if len(newEntries) == 0 {
		if !removedLegacyBeadsIgnore {
			return nil // nothing to add
		}
		return fs.WriteFile(gitignorePath, []byte(cleanedExisting), 0o644)
	}

	// Build the new content: existing + separator + section header + entries.
	var b strings.Builder
	if len(cleanedExisting) > 0 {
		b.WriteString(cleanedExisting)
		// Ensure there's a blank line before our section.
		if !strings.HasSuffix(cleanedExisting, "\n") {
			b.WriteByte('\n')
		}
		if !strings.HasSuffix(cleanedExisting, "\n\n") {
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
