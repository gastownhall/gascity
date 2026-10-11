//go:build !((linux && !android) || (darwin && !ios))

package main

import "os"

// runMapDirOwnedByTrustedUser conservatively refuses on platforms without Unix
// file ownership. The manifold-proxy run-map handoff is a Unix (/run) mechanism,
// so no group- or other-writable dir can be validated as a safe sticky handoff
// here and the writer never trusts one; a dir with neither the group- nor
// other-write bit set is still accepted upstream of this call.
func runMapDirOwnedByTrustedUser(os.FileInfo) bool {
	return false
}

// runMapExistingFileIsOurs cannot verify file ownership without Unix stat, so it
// conservatively treats an existing regular file as ours: the shared /run handoff
// is a Unix mechanism, and on other platforms the only run-map dir is a
// GC_RUNMAP_DIR override this user controls. publishRunMapKey's symlink refusal
// (which needs no ownership check) still applies cross-platform.
func runMapExistingFileIsOurs(os.FileInfo) bool {
	return true
}
