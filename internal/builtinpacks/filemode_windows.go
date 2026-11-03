//go:build windows

package builtinpacks

import (
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/runtime"
)

// checkMaterializedFileMode reports whether a materialized bundled-pack file
// still carries the mode the manifest intends.
//
// Windows stores one bit of the POSIX triple. [os.FileMode] here is synthesized
// from the read-only attribute alone — 0666 for a writable file, 0444 for a
// read-only one — so the group, other, and execute bits describe nothing that
// exists on disk, and an exact comparison against a manifest mode such as 0644
// can never hold. Comparing them anyway would not be strict, it would be
// comparing values the filesystem never stored.
//
// What the mode does model is writability, and that is the whole of the
// contract kept here: a file the manifest intends to be owner-writable must be
// writable, and one it intends to be read-only must carry the read-only
// attribute. A cache file flipped to read-only still fails the cache. Who else
// may write the file is the other half of the Unix comparison, and here it is
// recorded in the DACL rather than the mode; checkMaterializedFileAccess reads
// it. The file being regular is checked by the caller on every platform.
func checkMaterializedFileMode(got, want os.FileMode) error {
	gotWritable := got.Perm()&0o200 != 0
	wantWritable := want.Perm()&0o200 != 0
	if gotWritable != wantWritable {
		return fmt.Errorf("has mode %s, expected a %s file", got.Perm(), writabilityWord(wantWritable))
	}
	return nil
}

func writabilityWord(writable bool) string {
	if writable {
		return "writable"
	}
	return "read-only"
}

// checkMaterializedFileAccess reports whether an account other than this user
// and the system principals can change a materialized bundled-pack file.
//
// On Unix that is a group or other write bit, which the mode comparison
// already fails. Here it is an entry in the file's DACL — granting Everyone
// write leaves the synthesized mode reading 0666 exactly as before — so the
// DACL is read instead. Only the file is examined, as only the file's mode is
// on Unix; the directories holding it are not.
func checkMaterializedFileAccess(path string) error {
	return runtime.VerifyNotWritableByOthers(path)
}
