package materialize

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/gastownhall/gascity/internal/fsys"
)

// MaxMCPFileSize is the most gc reads of a provider-native MCP file or an
// adoption snapshot: 4 MiB. Provider MCP config is a few KiB, so a larger
// file is none gc wrote; gc leaves it alone rather than read it.
const MaxMCPFileSize = 4 << 20

// NotRegularFileError reports a path that is not a regular file: a symlink,
// a named pipe, a device, a socket, or a directory. gc reads none of them,
// since reading one can block forever (a named pipe) or never end (a device
// such as /dev/zero).
type NotRegularFileError struct {
	Path string
	Mode os.FileMode
}

func (e *NotRegularFileError) Error() string {
	switch t := e.Mode.Type(); {
	case t&os.ModeSymlink != 0:
		return "a symlink, not a regular file"
	case t&os.ModeNamedPipe != 0:
		return "a named pipe, not a regular file"
	case t&os.ModeSocket != 0:
		return "a socket, not a regular file"
	case t&os.ModeDevice != 0:
		return "a device, not a regular file"
	case t&os.ModeDir != 0:
		return "a directory, not a regular file"
	default:
		return "not a regular file"
	}
}

// FileTooLargeError reports a regular file larger than gc reads.
type FileTooLargeError struct {
	Path  string
	Size  int64
	Limit int64
}

func (e *FileTooLargeError) Error() string {
	return fmt.Sprintf("%d bytes, more than the %d gc reads", e.Size, e.Limit)
}

// ReadMCPFile reads a provider-native MCP file or an adoption snapshot when
// it is a regular file of at most MaxMCPFileSize bytes; see readRegularFile.
func ReadMCPFile(fs fsys.FS, path string) ([]byte, error) {
	return readRegularFile(fs, path)
}

// readRegularFile reads path when it is a regular file of at most
// MaxMCPFileSize bytes, and otherwise returns *NotRegularFileError or
// *FileTooLargeError without reading it. It never follows a symlink at path,
// and never blocks on a named pipe: on the real filesystem it opens path
// with O_NOFOLLOW and O_NONBLOCK and checks the file it opened, so the file
// cannot be swapped for another between the check and the read. A missing
// path returns an error errorsIsNotExist reports.
func readRegularFile(fs fsys.FS, path string) ([]byte, error) {
	if _, ok := fs.(fsys.OSFS); ok {
		return readRegularOSFile(path)
	}
	info, err := fs.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := checkRegularFile(path, info); err != nil {
		return nil, err
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxMCPFileSize {
		return nil, &FileTooLargeError{Path: path, Size: int64(len(data)), Limit: MaxMCPFileSize}
	}
	return data, nil
}

func readRegularOSFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, &NotRegularFileError{Path: path, Mode: os.ModeSymlink}
		}
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkRegularFile(path, info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxMCPFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxMCPFileSize {
		return nil, &FileTooLargeError{Path: path, Size: int64(len(data)), Limit: MaxMCPFileSize}
	}
	return data, nil
}

func checkRegularFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return &NotRegularFileError{Path: path, Mode: info.Mode()}
	}
	if info.Size() > MaxMCPFileSize {
		return &FileTooLargeError{Path: path, Size: info.Size(), Limit: MaxMCPFileSize}
	}
	return nil
}
