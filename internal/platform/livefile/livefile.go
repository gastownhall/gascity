// Package livefile opens and stats files that another process may rename,
// replace or remove while they are being observed: an append-only log that its
// writer rotates, or a socket that its owner re-binds at the same path.
//
// On Unix the os package already behaves this way, and these functions are
// os.Stat, os.Open and os.OpenFile. Windows differs in two ways that break that
// pattern:
//
//   - os.Stat on a regular file or directory reads its size by path but leaves
//     the file identity (volume serial number and file index) unloaded. The
//     first os.SameFile call loads it by reopening the path at that moment, so
//     a snapshot taken before a rotation compares as the file that holds the
//     path afterwards.
//   - os.Open and os.OpenFile do not share delete access, so while any process
//     holds the file open, no other process can rename or remove it.
package livefile

import (
	"fmt"
	"os"
)

// supportedFlags are the os.OpenFile flags OpenFile accepts.
const supportedFlags = os.O_RDONLY | os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_APPEND

// Stat returns the FileInfo for path, following symbolic links, with its file
// identity captured at call time. Comparing the result with os.SameFile against
// a later snapshot compares the two files the snapshots saw, not whichever file
// holds path when os.SameFile runs.
func Stat(path string) (os.FileInfo, error) {
	return stat(path)
}

// Open opens path for reading without preventing another process from
// renaming or removing it while it is open. A rename or removal does not
// disturb the reader: it keeps reading the file it opened.
func Open(path string) (*os.File, error) {
	return open(path)
}

// OpenFile is os.OpenFile for a file that other processes may rename or remove
// while it is open, such as an append-only log another writer rotates. The
// handle does not prevent the rename or removal, and keeps referring to the
// file it opened afterwards. flag may combine O_RDONLY, O_WRONLY, O_RDWR,
// O_CREATE and O_APPEND; any other flag is an error.
func OpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	if unsupported := flag &^ supportedFlags; unsupported != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: fmt.Errorf("unsupported open flags %#x", unsupported)}
	}
	return openFile(path, flag, perm)
}
