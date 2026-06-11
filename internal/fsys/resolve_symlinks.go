package fsys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// maxSymlinkDepth bounds symlink chain resolution, mirroring the kernel's
// nested-link limit so cycles fail instead of looping forever.
const maxSymlinkDepth = 40

// ResolveSymlinks follows any chain of symlinks at path and returns the
// final non-symlink path. Relative link targets are resolved against the
// directory of the link that contains them. A path (or dangling link
// target) that does not exist is returned as-is so callers can create it.
//
// Use this before an atomic temp-file + rename write: renaming over a
// symlink replaces the link itself, so writers that must preserve links
// resolve the target first and write there instead.
func ResolveSymlinks(fs FS, path string) (string, error) {
	for range maxSymlinkDepth {
		info, err := fs.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return path, nil
			}
			return "", fmt.Errorf("resolving symlinks for %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		rl, ok := fs.(readlinkFS)
		if !ok {
			return "", fmt.Errorf("resolving symlinks for %s: filesystem does not support Readlink", path)
		}
		target, err := rl.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("resolving symlinks for %s: %w", path, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return "", fmt.Errorf("resolving symlinks for %s: too many levels of symbolic links", path)
}
