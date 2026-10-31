//go:build windows

package materialize

import "os"

// openRegularNoFollow opens path for reading, refusing a symbolic link or other
// reparse point at it. Windows has no O_NOFOLLOW, so the path is checked just
// before the open; the caller then refuses anything but a regular file.
func openRegularNoFollow(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, &NotRegularFileError{Path: path, Mode: info.Mode().Type()}
	}
	return os.Open(path)
}
