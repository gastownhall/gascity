//go:build windows

package beads

import "github.com/gastownhall/gascity/internal/platform/filelock"

// On Windows the sequence floor's directory is opened for its sync through
// filelock: syncing a directory there needs a native handle with write access,
// which os.Open does not request.
func init() {
	openSQLiteSequenceFloorDirectory = func(path string) (sqliteSequenceFloorFile, error) {
		return filelock.OpenDirectoryForSync(path)
	}
}
