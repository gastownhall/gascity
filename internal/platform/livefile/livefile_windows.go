//go:build windows

package livefile

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// extendedLengthFrom is the length at which os.OpenFile passes a path to
// CreateFile in its \\?\ form: beyond MAX_PATH (260) less room for an 8.3
// name, CreateFile rejects a plain path unless the system enables long paths.
const extendedLengthFrom = 248

// stat opens path with no data access, sharing read, write and delete, and
// stats the handle. GetFileInformationByHandle returns the size and the file
// identity together from that one handle, so the result describes a single
// file and never blocks a concurrent append, rename or removal.
//
// FILE_FLAG_BACKUP_SEMANTICS lets the handle name a directory. A reparse point
// with no file-system handler, such as an AF_UNIX socket, cannot be opened as
// its target and fails with ERROR_CANT_ACCESS_FILE; it is opened as the reparse
// point itself instead, which is what os.Stat reports for it as well.
func stat(path string) (os.FileInfo, error) {
	f, err := createFile(path, 0, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if errors.Is(err, windows.ERROR_CANT_ACCESS_FILE) {
		f, err = createFile(path, 0, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	}
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	defer f.Close() //nolint:errcheck // attribute-only handle
	return f.Stat()
}

// open matches os.Open (generic read, not inheritable, backup semantics so a
// directory opens) and adds FILE_SHARE_DELETE, which os.Open omits.
func open(path string) (*os.File, error) {
	return openFile(path, os.O_RDONLY, 0)
}

// openFile maps flag and perm onto CreateFile exactly as os.OpenFile does, and
// adds FILE_SHARE_DELETE to the share mode. O_APPEND grants every right in
// GENERIC_WRITE except FILE_WRITE_DATA, which makes every write land at the end
// of the file; a perm without owner write creates the file read-only; a
// read-only open adds FILE_FLAG_BACKUP_SEMANTICS so a directory opens. The
// handle is not inheritable.
func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access = windows.GENERIC_READ
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if flag&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}
	if flag&os.O_APPEND != 0 {
		access &^= windows.GENERIC_WRITE
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE
	}

	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	if flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		attrs |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}

	createMode := uint32(windows.OPEN_EXISTING)
	if flag&os.O_CREATE != 0 {
		createMode = windows.OPEN_ALWAYS
	}

	f, err := createFile(path, access, createMode, attrs)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return f, nil
}

func createFile(path string, access, createMode, attrs uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(extendedLengthPath(path))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, access, shareAll, nil, createMode, attrs, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

// extendedLengthPath returns path in the \\?\ form when its absolute form is
// extendedLengthFrom bytes or longer, as os.OpenFile does, so a long path opens
// whether or not the system enables long paths. A UNC path takes the
// \\?\UNC\ form. Shorter paths, and paths already in an extended-length or
// device form, are returned unchanged.
func extendedLengthPath(path string) string {
	if hasExtendedOrDevicePrefix(path) {
		return path
	}
	abs, err := windows.FullPath(path)
	if err != nil || len(abs) < extendedLengthFrom {
		return path
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}

// hasExtendedOrDevicePrefix reports whether path starts with \??\, or with
// \\?\ or \\.\ written with either separator.
func hasExtendedOrDevicePrefix(path string) bool {
	if strings.HasPrefix(path, `\??\`) {
		return true
	}
	isSep := func(c byte) bool { return c == '\\' || c == '/' }
	return len(path) >= 4 && isSep(path[0]) && isSep(path[1]) && (path[2] == '?' || path[2] == '.') && isSep(path[3])
}
