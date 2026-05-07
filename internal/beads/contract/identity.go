// L1 reader for project identity. The L1 layer is the canonical,
// git-tracked source of truth for a beads scope's project_id. This
// file owns reads of L1; reconcile across L1/L2/L3 lives in
// EnsureProjectIdentity (a sibling bead). Writes will land in
// WriteProjectIdentity (a sibling bead) — until then, the test
// helpers and reconcile callers populate the file via os primitives
// or git checkout.

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
