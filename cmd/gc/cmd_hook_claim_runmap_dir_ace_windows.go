//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// runMapAceSID returns the SID an access-control entry names. Windows stores
// that SID inline, starting at the entry's SidStart field and running past the
// end of the struct, and golang.org/x/sys/windows has no typed accessor for it.
//
// This cast is the only unsafe code the run-map dir check needs, and it lives
// in its own file because internal/storebinding's TestNoLinknameOrUnsafeEscape
// rejects an unsafe import in any file whose text matches its storage-boundary
// tokens, which cmd_hook_claim_runmap_dir_windows.go does.
func runMapAceSID(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	return (*windows.SID)(unsafe.Pointer(&ace.SidStart))
}
