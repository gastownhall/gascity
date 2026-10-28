//go:build windows

package pidutil

import "fmt"

// platformCmdline has no Windows implementation yet.
//
// Reading another process's command line on Windows means reading its PEB
// across the process boundary (NtQueryInformationProcess plus a remote memory
// read), which is not something this package does today. The error wraps
// ErrUnsupported so callers can tell "cannot identify this process" apart from
// "this process is not the one I am looking for" — the distinction that decides
// whether a singleton gate may start a replacement.
func platformCmdline(pid int) ([]string, error) {
	return nil, fmt.Errorf("pidutil: command line inspection for PID %d is unavailable on Windows: %w", pid, ErrUnsupported)
}
