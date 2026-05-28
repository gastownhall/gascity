package fsys

import (
	"errors"
	"os"
	"path/filepath"
)

// RemoveAll removes path and any children using the provided filesystem.
// Missing paths are treated as already removed. Symlink paths are removed as
// links and are not followed.
func RemoveAll(fs FS, path string) error {
	info, err := fs.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err := fs.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	entries, err := fs.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var removeErr error
	for _, entry := range entries {
		if err := RemoveAll(fs, filepath.Join(path, entry.Name())); err != nil {
			removeErr = errors.Join(removeErr, err)
		}
	}
	if removeErr != nil {
		return removeErr
	}
	if err := fs.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
