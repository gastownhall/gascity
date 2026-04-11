package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// gcManagedGitignoreHeader marks the section of .gitignore that gc init
// and gc rig add append to. Operators can delete the whole block by
// removing from this header to the next blank line.
const gcManagedGitignoreHeader = "# Gas City managed state"

// cityGitignoreEntries covers state that the city root generates and
// that must not be committed: the .gc/ runtime, the .beads/ database,
// and the hooks/ directory where per-provider agent hook files are
// materialized. The beads backend (bd init) writes its own entries for
// .dolt/, *.db, and .beads-credential-key; ensureGitignoreEntries
// complements that set rather than replacing it.
var cityGitignoreEntries = []string{".gc/", ".beads/", "hooks/"}

// rigGitignoreEntries covers state that a rig root generates. Unlike
// cities, rigs do not own .gc/ or hooks/ — only the .beads/ database.
var rigGitignoreEntries = []string{".beads/"}

// ensureGitignoreEntries appends any entries that are not already
// present to dir/.gitignore. A missing file is created; an existing
// file keeps its prior content and receives only the missing entries
// under a [gcManagedGitignoreHeader] block. The function is idempotent:
// once every entry is present, subsequent calls with the same set leave
// the file untouched.
func ensureGitignoreEntries(fs fsys.FS, dir string, entries []string) error {
	path := filepath.Join(dir, ".gitignore")

	existing, err := fs.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	present := make(map[string]bool)
	for _, line := range bytes.Split(existing, []byte("\n")) {
		trimmed := string(bytes.TrimSpace(line))
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		present[trimmed] = true
	}

	var missing []string
	for _, e := range entries {
		if !present[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	if len(existing) > 0 {
		buf.WriteByte('\n')
	}
	buf.WriteString(gcManagedGitignoreHeader)
	buf.WriteByte('\n')
	for _, e := range missing {
		buf.WriteString(e)
		buf.WriteByte('\n')
	}

	if err := fs.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
