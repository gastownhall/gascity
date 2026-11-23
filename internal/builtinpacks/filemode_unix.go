//go:build !windows

package builtinpacks

import (
	"fmt"
	"os"
)

// checkMaterializedFileMode reports whether a materialized bundled-pack file
// still carries the mode the manifest intends.
//
// Unix stores the whole permission triple, so the contract is exact equality.
// The extractor wrote want and nothing legitimate rewrites a cache file
// afterwards, so any difference — a widened group or other bit, an execute bit
// on a file that is not a script — is tampering and fails the cache.
func checkMaterializedFileMode(got, want os.FileMode) error {
	if got.Perm() != want.Perm() {
		return fmt.Errorf("has mode %s, expected %s", got.Perm(), want.Perm())
	}
	return nil
}

// checkMaterializedFileAccess has nothing to add on Unix: who else may write a
// file is its group and other write bits, which checkMaterializedFileMode
// already holds to the manifest.
func checkMaterializedFileAccess(string) error {
	return nil
}
