// L1 reader/writer for project identity. The L1 layer is the
// canonical, git-tracked source of truth for a beads scope's
// project_id. This file owns reads and writes of L1; reconcile across
// L1/L2/L3 lives in EnsureProjectIdentity (a sibling bead). External
// packages must route writes through WriteProjectIdentity.

package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ProjectIdentityPath returns the canonical L1 path for a scope.
//
// The L1 file is "<scopeRoot>/.beads/identity.toml". This helper
// centralizes the construction so callers (doctor, error messages,
// reconcile) name the file consistently and survive future scope-path
// normalization.
func ProjectIdentityPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", "identity.toml")
}

// ReadProjectIdentity reads the L1 project_id for a scope.
//
// The bool reports whether a usable id was found. Both an absent file
// and a present file with an empty (or whitespace-only) project.id
// return ("", false, nil) — callers must treat both as "L1 not yet
// populated" (legacy rig). A missing [project] section is also
// treated as not-yet-populated; only a malformed document or one
// with unknown keys is an error.
//
// Parse strictness is intentional: unknown keys at the top level or
// inside [project] are rejected with an error wrapped to include the
// file path. This catches typos before they cascade into reconcile
// mismatches.
//
// scopeRoot is the parent of the .beads/ directory (city or rig
// root). The function joins scopeRoot/.beads/identity.toml itself;
// callers should not construct the path.
func ReadProjectIdentity(fs fsys.FS, scopeRoot string) (string, bool, error) {
	path := ProjectIdentityPath(scopeRoot)
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read identity %s: %w", path, err)
	}

	type project struct {
		ID string `toml:"id"`
	}
	type doc struct {
		Project project `toml:"project"`
	}
	var d doc
	md, err := toml.Decode(string(data), &d)
	if err != nil {
		return "", false, fmt.Errorf("parse identity %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return "", false, fmt.Errorf("parse identity %s: unexpected keys %v", path, keys)
	}

	id := strings.TrimSpace(d.Project.ID)
	if id == "" {
		return "", false, nil
	}
	return id, true, nil
}

// identityBodyTemplate is the canonical L1 file body. The two leading
// comment lines are part of the format (designer §10) so a `git diff`
// of the file reads as documentation, not as bytes.
const identityBodyTemplate = `# .beads/identity.toml — canonical, git-tracked.
# Edited only at scope creation or by deliberate human/` + "`gc`" + ` migration.

[project]
id = "%s"
`

// forbiddenIdentityChars are characters that cannot appear in a valid
// project id without corrupting the TOML body. Newline and CR would
// break the single-line `id = "..."` field; the double quote and
// backslash would either close or escape the TOML string.
const forbiddenIdentityChars = "\n\r\"\\"

// WriteProjectIdentity writes the L1 project_id for a scope.
//
// The id is trimmed before validation and serialization. Empty,
// whitespace-only, and ids containing newline (\n), carriage return
// (\r), double quote ("), or backslash (\) are rejected with an error
// that includes the offending value — these inputs would otherwise
// corrupt the TOML body.
//
// The function creates scopeRoot/.beads/ with mode 0o755 if the
// directory does not exist (designer §7.1). The file is written via
// fsys.WriteFileIfChangedAtomic, which provides atomicity (temp file +
// rename) and idempotence (no inode churn when content already
// matches). Symlinks at the target path are replaced with regular
// files (designer §7.2 / atomic.go).
//
// Concurrency: the file-level write is safe under concurrent calls
// passing the same id — the atomic rename ensures readers never see
// partial content. The contract does not serialize callers passing
// *different* ids; that policy lives upstream in the reconciler.
func WriteProjectIdentity(fs fsys.FS, scopeRoot string, id string) error {
	if strings.ContainsAny(id, forbiddenIdentityChars) {
		return fmt.Errorf("write identity: id %q contains forbidden character (newline, CR, quote, or backslash)", id)
	}
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return fmt.Errorf("write identity: id %q is empty or whitespace-only", id)
	}

	path := ProjectIdentityPath(scopeRoot)
	dir := filepath.Dir(path)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	body := fmt.Sprintf(identityBodyTemplate, trimmed)
	if err := fsys.WriteFileIfChangedAtomic(fs, path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write identity %s: %w", path, err)
	}
	return nil
}
