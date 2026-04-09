package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/fsys"
)

// beadsDirPerm is the permission bd recommends for .beads/ directories.
// Wider permissions cause bd to emit a warning on every call, which
// pollutes agent pod output and is mistreated as a hard List() failure
// by the controller's stderr-as-error path.
const beadsDirPerm os.FileMode = 0o700

// ensureBeadsDir creates path with restrictive permissions, tightening
// any pre-existing directory whose mode was set by an older gascity
// version (or another tool) to a wider value. Idempotent — safe to
// call on every init pass.
func ensureBeadsDir(fs fsys.FS, path string) error {
	if err := fs.MkdirAll(path, beadsDirPerm); err != nil {
		return err
	}
	return fs.Chmod(path, beadsDirPerm)
}
