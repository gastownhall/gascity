//go:build windows

package pidutil

import (
	"errors"
	"fmt"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformAlive(pid int) bool {
	handle, err := openProcessForQuery(pid)
	if err != nil {
		if pid <= 0 || errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_FOUND) {
			return false
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true
		}
		// An observation failure must not invent process death.
		return true
	}
	defer windows.CloseHandle(handle)

	waitResult, waitErr := windows.WaitForSingleObject(handle, 0)
	switch waitResult {
	case uint32(windows.WAIT_TIMEOUT):
		return true
	case windows.WAIT_OBJECT_0:
		return false
	}
	if waitErr == nil {
		return true
	}

	// The wait state is authoritative. The exit code is only a conservative
	// fallback for an unexpected wait failure because 259 is also a valid exit
	// code and cannot establish liveness by itself.
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err == nil && code != 259 {
		return false
	}
	return true
}

func platformStartTime(pid int) (string, error) {
	handle, err := openProcessForQuery(pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", fmt.Errorf("pidutil: reading creation time for PID %d: %w", pid, err)
	}
	return strconv.FormatInt(creation.Nanoseconds(), 10), nil
}

func platformChildPIDs(parent int) ([]int, error) {
	if parent <= 0 {
		return nil, fmt.Errorf("pidutil: invalid parent PID %d", parent)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("pidutil: process snapshot failed: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, fmt.Errorf("pidutil: process snapshot read failed: %w", err)
	}
	var children []int
	for {
		if int(entry.ParentProcessID) == parent {
			children = append(children, int(entry.ProcessID))
		}
		entry.Size = uint32(unsafe.Sizeof(windows.ProcessEntry32{}))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, fmt.Errorf("pidutil: process snapshot read failed: %w", err)
		}
	}
	return children, nil
}

func openProcessForQuery(pid int) (windows.Handle, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("pidutil: invalid PID %d", pid)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("pidutil: opening PID %d: %w", pid, err)
	}
	return handle, nil
}
