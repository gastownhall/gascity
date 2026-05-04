package fsys

import (
	"os"
	"path/filepath"
)

// OSScaffoldFS extends [OSFS] with tree-walking, symlink, and
// recursive-remove operations needed by scaffold rollback.
type OSScaffoldFS struct{ OSFS }

// Walk delegates to [filepath.Walk].
func (OSScaffoldFS) Walk(root string, fn filepath.WalkFunc) error {
	return filepath.Walk(root, fn)
}

// Readlink delegates to [os.Readlink].
func (OSScaffoldFS) Readlink(name string) (string, error) {
	return os.Readlink(name)
}

// Symlink delegates to [os.Symlink].
func (OSScaffoldFS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

// RemoveAll delegates to [os.RemoveAll].
func (OSScaffoldFS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}
