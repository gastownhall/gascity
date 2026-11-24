//go:build windows

package pidutil

import (
	"fmt"
	"os"
)

// Windows never consults /proc, and the reason is a security boundary rather
// than a portability gap.
//
// An absolute "/proc/<pid>/cmdline" resolves to C:\proc\<pid>\cmdline on the
// current drive. The root of the system drive grants Authenticated Users
// add-subdirectory rights by default and C:\proc does not exist, so any local
// user can create that tree and plant a record that forges the identity of an
// arbitrary PID. Process identity on Windows comes from kernel handles,
// creation time and Toolhelp snapshots; a filesystem path is not a source of
// identity here.
//
// These helpers therefore fail without touching the filesystem at all. Falling
// back to a platform mechanism on the error keeps callers' control flow
// identical to a host that simply has no procfs.

func readProcFile(pid int, _ ...string) ([]byte, error) {
	return nil, procfsUnavailable(pid)
}

func readProcDir(pid int, _ ...string) ([]os.DirEntry, error) {
	return nil, procfsUnavailable(pid)
}

func procfsUnavailable(pid int) error {
	return fmt.Errorf("pidutil: /proc is not a trusted identity source for PID %d on Windows: %w", pid, ErrUnsupported)
}
