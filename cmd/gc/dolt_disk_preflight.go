package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

const (
	// doltDiskDefaultMinFreeBytes is the critical floor (500 MiB). Below this
	// threshold managed-Dolt startup is refused to prevent ENOSPC crashes.
	doltDiskDefaultMinFreeBytes = 500 << 20 // 500 MiB

	// doltDiskDefaultWarnFreeBytes is the soft floor (2 GiB). Below this
	// threshold a warning is emitted but operations are not blocked.
	doltDiskDefaultWarnFreeBytes = 2 << 30 // 2 GiB

	doltDiskGiB = float64(1 << 30)
)

// errDiskPreflightUnsupported is returned by the Windows stub (and any
// platform where statfs is unavailable). Call sites that receive this error
// must fail-open without logging — it is not a probe failure.
var errDiskPreflightUnsupported = errors.New("disk preflight unavailable on this platform")

// doltDiskMinFreeBytes returns the critical floor from GC_DOLT_MIN_FREE_BYTES,
// defaulting to 500 MiB. Zero disables the check entirely.
func doltDiskMinFreeBytes() int64 {
	if v := os.Getenv("GC_DOLT_MIN_FREE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return doltDiskDefaultMinFreeBytes
}

// doltDiskWarnFreeBytes returns the soft floor from GC_DOLT_WARN_FREE_BYTES,
// defaulting to 2 GiB.
func doltDiskWarnFreeBytes() int64 {
	if v := os.Getenv("GC_DOLT_WARN_FREE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return doltDiskDefaultWarnFreeBytes
}

// checkManagedDoltDiskPreflight checks free disk space before a disk-growing
// managed-Dolt operation. Returns a non-nil error when free space is below
// minFree (CRITICAL) and minFree > 0. Logs a warning to stderr when free
// space is below warnFree but above minFree. Fails open on probe error or
// unsupported platform; callers must never block on a probe failure.
func checkManagedDoltDiskPreflight(dataDir string, minFree, warnFree int64, stderr io.Writer) error {
	if minFree == 0 {
		return nil // check disabled via escape hatch
	}
	free, err := doltContainerFreeBytesFunc(dataDir)
	if err != nil {
		if errors.Is(err, errDiskPreflightUnsupported) {
			return nil // platform stub — skip silently
		}
		fmt.Fprintf(stderr, "managed-dolt: disk pre-flight probe failed (fail-open): %v\n", err) //nolint:errcheck
		return nil
	}
	if free < minFree {
		return fmt.Errorf(
			"refusing to start managed Dolt: container free space %d bytes (%.1f GiB) "+
				"is below the floor %d bytes (%.1f GiB) on %s; "+
				"free disk space or set GC_DOLT_MIN_FREE_BYTES=0 to disable",
			free, float64(free)/doltDiskGiB,
			minFree, float64(minFree)/doltDiskGiB,
			dataDir)
	}
	if warnFree > 0 && free < warnFree {
		fmt.Fprintf(stderr, //nolint:errcheck
			"managed-dolt: disk WARN — %.1f GiB free (floor %.1f GiB) on %s\n",
			float64(free)/doltDiskGiB, float64(warnFree)/doltDiskGiB, dataDir)
	}
	return nil
}
