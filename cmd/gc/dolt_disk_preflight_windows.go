//go:build windows

package main

// doltContainerFreeBytesFunc is the injectable disk-space reader.
// On Windows the check is skipped; the stub returns errDiskPreflightUnsupported
// so checkManagedDoltDiskPreflight fails-open without logging.
var doltContainerFreeBytesFunc = containerFreeBytes

func containerFreeBytes(_ string) (int64, error) {
	return -1, errDiskPreflightUnsupported
}
