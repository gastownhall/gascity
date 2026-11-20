package main

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// configureTapMemory starts the tap with huge pages disabled for its lifetime.
// The Go runtime allocates before main: disabling future huge pages here alone
// leaves startup mappings resident. Re-exec once under the inherited kernel
// policy, before reading stdin or publishing anything.
func configureTapMemory() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	disabled, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0)
	if err != nil {
		return fmt.Errorf("reading huge-page policy: %w", err)
	}
	if disabled == 1 {
		return nil
	}
	if err := unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("disabling huge pages: %w", err)
	}
	// Execute this image even if an upgrade has replaced or unlinked its path.
	if err := unix.Exec("/proc/self/exe", os.Args, os.Environ()); err != nil {
		return fmt.Errorf("re-executing tap image: %w", err)
	}
	return nil
}
