//go:build windows

package beads

import "os"

// Windows has no syscall.Flock equivalent in stdlib; LockFileEx (golang.org/x/sys/windows)
// would be the proper port. gc currently ships only on darwin/linux, so the Windows path
// no-ops the bd list throttle rather than refuse the command outright. The exclusive-lock
// semantics that protect bd subprocess fan-out from concurrent gc bd list pileups are
// therefore disabled on Windows builds until a proper implementation lands.
func tryBdListReadFileLock(_ *os.File) (acquired, retryable bool, err error) {
	return true, false, nil
}

func releaseBdListReadFileLock(_ *os.File) {}
