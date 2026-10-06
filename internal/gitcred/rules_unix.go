//go:build !windows

package gitcred

import (
	"io/fs"
	"syscall"
)

// statOwner returns the file's owning uid and gid. ok is false when the
// FileInfo exposes no Unix ownership metadata; callers must treat that as
// "owner unknown", never as a match.
func statOwner(info fs.FileInfo) (uid, gid uint32, ok bool) {
	stat, isUnix := info.Sys().(*syscall.Stat_t)
	if !isUnix {
		return 0, 0, false
	}
	return stat.Uid, stat.Gid, true
}
