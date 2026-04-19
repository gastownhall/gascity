package fsys

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"
)

// WriteFileAtomic writes data to path atomically using a temp file + rename.
// The temp file is created in the same directory as path to ensure the rename
// is on the same filesystem (required for atomic rename on POSIX).
func WriteFileAtomic(fs FS, path string, data []byte, perm os.FileMode) error {
	suffix := strconv.Itoa(os.Getpid()) + "." + strconv.FormatInt(time.Now().UnixNano(), 36)
	tmp := path + ".tmp." + suffix
	if err := fs.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := fs.Rename(tmp, path); err != nil {
		// Best-effort cleanup of temp file.
		_ = fs.Remove(tmp)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// WriteFileIfChangedAtomic writes data to path atomically only when the
// existing on-disk bytes differ. Returns nil with no write when the
// content already matches. A read error other than "not exist" is
// ignored and the write proceeds — this is a best-effort optimization to
// avoid churning mtime (and fsnotify watchers) on no-op writes, not a
// safety check.
func WriteFileIfChangedAtomic(fs FS, path string, data []byte, perm os.FileMode) error {
	if existing, err := fs.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	return WriteFileAtomic(fs, path, data, perm)
}
