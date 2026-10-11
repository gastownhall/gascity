//go:build !linux

package sqlite

import "os"

func makeRecoverableSQLiteSnapshotTempDir(dir, pattern string) (string, error) {
	return os.MkdirTemp(dir, pattern)
}
