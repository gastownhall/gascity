//go:build windows

package materialize

import (
	"os"

	"golang.org/x/sys/windows"
)

// openRegularNoFollow opens path for reading without following a symbolic
// link or other reparse point at it. The handle is opened with
// FILE_FLAG_OPEN_REPARSE_POINT and inspected itself, so a link swapped in
// after any earlier check is refused rather than followed (the pattern in
// internal/fsys/read_regular_windows.go). The caller still refuses anything
// but a regular file.
func openRegularNoFollow(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = f.Close()
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	switch {
	case info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		_ = f.Close()
		return nil, &NotRegularFileError{Path: path, Mode: os.ModeSymlink}
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		_ = f.Close()
		return nil, &NotRegularFileError{Path: path, Mode: os.ModeDir}
	}
	return f, nil
}
