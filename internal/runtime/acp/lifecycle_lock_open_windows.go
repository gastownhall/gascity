//go:build windows

package acp

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func openLifecycleLockFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var handle syscall.Handle
	for attempt := 0; attempt < 2; attempt++ {
		handle, err = syscall.CreateFile(
			name,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
			nil,
			syscall.OPEN_EXISTING,
			syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS,
			0,
		)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		handle, err = syscall.CreateFile(
			name,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
			nil,
			syscall.CREATE_NEW,
			syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS,
			0,
		)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ERROR_FILE_EXISTS) && !errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			return nil, err
		}
	}
	if err != nil {
		return nil, fmt.Errorf("opening lifecycle lock %q after concurrent creation: %w", path, err)
	}

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		_ = syscall.CloseHandle(handle)
		return nil, fmt.Errorf("inspecting lifecycle lock %q: %w", path, err)
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = syscall.CloseHandle(handle)
		return nil, fmt.Errorf("lifecycle lock %q is a reparse point", path)
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = syscall.CloseHandle(handle)
		return nil, fmt.Errorf("lifecycle lock %q is not a regular file", path)
	}

	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = syscall.CloseHandle(handle)
		return nil, fmt.Errorf("opening lifecycle lock %q: invalid file handle", path)
	}
	return f, nil
}
