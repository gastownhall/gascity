//go:build windows

package fsys

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// readRegularFileSnapshot reads name without following a final symlink or
// other reparse point and returns the opened file's identity, its volume serial
// number and file index, for post-read stability checks. The handle shares
// read, write and delete access, so the read never blocks a concurrent
// replacement of the file.
func (OSFS) readRegularFileSnapshot(name string) (regularFileSnapshot, error) {
	file, info, err := openNoFollow(name, windows.GENERIC_READ)
	if err != nil {
		return regularFileSnapshot{}, err
	}
	defer func() {
		_ = file.Close()
	}()
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return regularFileSnapshot{}, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return regularFileSnapshot{}, &os.PathError{Op: "read", Path: name, Err: err}
	}
	return regularFileSnapshot{data: data, id: identityFromHandleInfo(info), hasID: true}, nil
}

// pathFileIdentity returns the identity of the file name currently names,
// without following a final reparse point. os.Lstat leaves it unloaded on
// Windows, so it is read from a handle opened for no data access.
func (OSFS) pathFileIdentity(name string) (fileIdentity, bool) {
	file, info, err := openNoFollow(name, 0)
	if err != nil {
		return fileIdentity{}, false
	}
	_ = file.Close()
	return identityFromHandleInfo(info), true
}

// openNoFollow opens name itself, not the target of a final reparse point,
// and returns the handle with its file information.
func openNoFollow(name string, access uint32) (*os.File, windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, info, &os.PathError{Op: "open", Path: name, Err: err}
	}
	handle, err := windows.CreateFile(path, access, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, info, &os.PathError{Op: "open", Path: name, Err: err}
	}
	file := os.NewFile(uintptr(handle), name)
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = file.Close()
		return nil, info, &os.PathError{Op: "stat", Path: name, Err: err}
	}
	return file, info, nil
}

func identityFromHandleInfo(info windows.ByHandleFileInformation) fileIdentity {
	return fileIdentity{
		dev: uint64(info.VolumeSerialNumber),
		ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}
}
