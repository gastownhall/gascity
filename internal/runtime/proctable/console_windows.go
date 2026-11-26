//go:build windows

package proctable

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ConsoleHost returns the process ID of the console host (conhost.exe, or the
// OpenConsole.exe behind a pseudoconsole) that process pid is attached to, or 0
// when it has no console.
//
// Processes attached to one console share its host. A program a shell runs in
// its own terminal inherits the shell's console. One started in a window of its
// own, with no window, or detached (Start-Process without -NoNewWindow,
// Start-Job) gets a console of its own or none (measured).
//
// created fences the answer to the process a snapshot saw. When it is not zero
// and pid now names a process created at another time, pid was reused, and
// ConsoleHost reports ErrProcessGone, as it does for a pid nothing holds and
// for a process that has exited, even while a handle keeps it open.
func ConsoleHost(pid int, created time.Time) (int, error) {
	handle, err := openFencedProcess(pid, created, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	var value uintptr
	if err := windows.NtQueryInformationProcess(handle, windows.ProcessConsoleHostProcess, unsafe.Pointer(&value), uint32(unsafe.Sizeof(value)), nil); err != nil {
		return 0, fmt.Errorf("proctable: reading the console host of PID %d: %w", pid, err)
	}
	// The low two bits are flags; a process with no console reads as 1.
	return int(value &^ 3), nil
}

// ProcessCommandLine returns the command line process pid was started with,
// fenced by created as ConsoleHost is.
func ProcessCommandLine(pid int, created time.Time) (string, error) {
	handle, err := openProcessForInspection(pid)
	if err != nil {
		if openErrorMeansProcessGone(err) {
			return "", fmt.Errorf("%w: PID %d", ErrProcessGone, pid)
		}
		return "", err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if err := fenceHandle(handle, pid, created); err != nil {
		return "", err
	}
	return readProcessCommandLine(handle)
}

// openFencedProcess opens pid with access, which must include SYNCHRONIZE, and
// checks that it is still running and, when created is not zero, still the
// process created then.
func openFencedProcess(pid int, created time.Time, access uint32) (windows.Handle, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("proctable: invalid PID %d", pid)
	}
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		if openErrorMeansProcessGone(err) {
			return 0, fmt.Errorf("%w: PID %d", ErrProcessGone, pid)
		}
		return 0, fmt.Errorf("proctable: opening PID %d: %w", pid, err)
	}
	if err := fenceHandle(handle, pid, created); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, err
	}
	return handle, nil
}

// fenceHandle checks that handle, opened with SYNCHRONIZE, is a running
// process created at created, or at any time when created is zero. An exited
// process is gone however long a handle keeps its object: its console and its
// memory are.
func fenceHandle(handle windows.Handle, pid int, created time.Time) error {
	if processHandleExited(handle) {
		return fmt.Errorf("%w: PID %d has exited", ErrProcessGone, pid)
	}
	if created.IsZero() {
		return nil
	}
	creation, err := handleCreationTime(handle)
	if err != nil {
		return fmt.Errorf("proctable: PID %d: %w", pid, err)
	}
	if creation != created.UnixNano() {
		return fmt.Errorf("%w: PID %d now names a process created later", ErrProcessGone, pid)
	}
	return nil
}
