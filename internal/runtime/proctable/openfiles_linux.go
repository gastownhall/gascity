//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// OpenFiles returns the absolute paths pid holds open, read from its fd
// directory under the same procfs root as [ScanBySessionID]; it refuses the
// live /proc under go test for the same reason. A process that is gone holds
// nothing. A descriptor closed between the listing and its read is skipped,
// and so is one that names no path (a pipe, socket or anonymous inode).
func OpenFiles(pid int) ([]string, error) {
	if err := liveScanGuard(); err != nil {
		return nil, err
	}
	dir := filepath.Join(scanRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading open files of pid %d: %w", pid, err)
	}
	var paths []string
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil || !filepath.IsAbs(target) {
			continue
		}
		paths = append(paths, target)
	}
	return paths, nil
}
