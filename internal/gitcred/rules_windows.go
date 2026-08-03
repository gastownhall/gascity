//go:build windows

package gitcred

import "io/fs"

// statOwner has no Unix ownership to report on Windows. loadFileLayer skips the
// permission gate there entirely; returning ok=false keeps any other caller
// fail-closed.
func statOwner(fs.FileInfo) (uid, gid uint32, ok bool) {
	return 0, 0, false
}
