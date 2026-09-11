//go:build (linux && !android) || (darwin && !ios)

package main

import (
	"os"
	"syscall"
)

// runMapDirOwnedByTrustedUser reports whether dir is owned by root or the
// current effective user. The run-map writer only trusts a group- or
// other-writable dir when it is a sticky handoff (see runMapDirSafeToPublish),
// and the sticky bit alone is not enough: the directory owner can delete or
// replace any file in the dir regardless of the sticky bit, so a dir owned by
// some other (potentially hostile) user could forge or clobber the
// <session>.json the manifold proxy trusts for spend attribution. Requiring a
// root or self owner pins the dir to its intended provisioner — root's 0o1777
// /run/gc-manifold-runmap, or a dir this user provisioned itself. os.Geteuid is
// the identity that actually creates the published files. This gate bounds the
// directory owner, not the file author: authenticating the writer of an
// individual <session>.json is the proxy's job (see runMapDirSafeToPublish).
func runMapDirOwnedByTrustedUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())
}

// runMapExistingFileIsOurs reports whether an existing publish target is a file
// this effective user owns — the only kind writeRunMap may safely replace on a
// refresh. Unlike runMapDirOwnedByTrustedUser (which trusts root as the handoff
// provisioner), an individual <session>.json is trusted only when WE wrote it:
// a foreign-owned file in the shared handoff is a squat the writer cannot
// overwrite (sticky yields EPERM) and the proxy would trust, so publishRunMapKey
// refuses it instead of following it or reporting best-effort success.
// os.Geteuid is the identity that writes the published files.
func runMapExistingFileIsOurs(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return stat.Uid == uint32(os.Geteuid())
}
