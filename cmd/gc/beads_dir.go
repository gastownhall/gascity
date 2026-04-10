package main

import (
	"log"
	"os"

	"github.com/gastownhall/gascity/internal/fsys"
)

// beadsDirPerm is the permission bd recommends for .beads/ directories.
// Wider permissions cause bd to emit a warning on every call, which
// pollutes agent pod output.
const beadsDirPerm os.FileMode = 0o700

// ensureBeadsDir creates path with restrictive permissions, tightening
// any pre-existing directory whose mode was set by an older gascity
// version (or another tool) to a wider value. Idempotent — safe to
// call on every init pass.
//
// Chmod failure is best-effort: the directory may live on a filesystem
// that does not support permission changes (e.g. certain container
// mounts). In that case we log a warning and continue — a working
// .beads/ dir with wider permissions is better than a hard failure.
func ensureBeadsDir(fs fsys.FS, path string) error {
	if err := fs.MkdirAll(path, beadsDirPerm); err != nil {
		return err
	}
	if err := fs.Chmod(path, beadsDirPerm); err != nil {
		log.Printf("warning: chmod %s to %o: %v (continuing with existing permissions)", path, beadsDirPerm, err)
	}
	return nil
}
