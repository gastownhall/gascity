// Package fsys defines a minimal filesystem interface for testability.
//
// Production code uses [OSFS] which delegates to the os package.
// Tests use [Fake] which provides an in-memory filesystem with spy
// capabilities and error injection — following the same pattern as
// [session.Provider] / [session.Fake].
package fsys

import (
	"os"
)

// FS abstracts the filesystem operations used by CLI commands.
type FS interface {
	// MkdirAll creates a directory path and all parents that do not exist.
	MkdirAll(path string, perm os.FileMode) error

	// WriteFile writes data to the named file, creating it if necessary.
	WriteFile(name string, data []byte, perm os.FileMode) error

	// ReadFile reads the named file and returns its contents.
	ReadFile(name string) ([]byte, error)

	// Stat returns file info for the named file.
	Stat(name string) (os.FileInfo, error)

	// Lstat returns file info for the named file without following symlinks.
	// Callers that must reject symlinked targets should call Lstat and check
	// the mode's ModeSymlink bit before touching the path.
	Lstat(name string) (os.FileInfo, error)

	// ReadDir reads the named directory and returns its entries.
	ReadDir(name string) ([]os.DirEntry, error)

	// Rename renames (moves) oldpath to newpath.
	Rename(oldpath, newpath string) error

	// Remove removes the named file or empty directory.
	Remove(name string) error

	// Chmod changes the mode of the named file or directory.
	Chmod(name string, mode os.FileMode) error
}

// OSFS implements [FS] by delegating to the os package.
type OSFS struct{}

// MkdirAll delegates to [os.MkdirAll].
func (OSFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// WriteFile delegates to [os.WriteFile].
func (OSFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// ReadFile delegates to [os.ReadFile].
func (OSFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

// Stat delegates to [os.Stat].
func (OSFS) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

// Lstat delegates to [os.Lstat].
func (OSFS) Lstat(name string) (os.FileInfo, error) {
	return os.Lstat(name)
}

// ReadDir delegates to [os.ReadDir].
func (OSFS) ReadDir(name string) ([]os.DirEntry, error) {
	return os.ReadDir(name)
}

// Rename delegates to [os.Rename].
func (OSFS) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// Remove delegates to [os.Remove].
func (OSFS) Remove(name string) error {
	return os.Remove(name)
}

// Chmod delegates to [os.Chmod].
func (OSFS) Chmod(name string, mode os.FileMode) error {
	return os.Chmod(name, mode)
}
