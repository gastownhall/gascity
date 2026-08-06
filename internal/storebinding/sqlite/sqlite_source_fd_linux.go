//go:build linux

package sqlite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ensureNoSQLiteSourceDescriptors enforces the migration startup invariant
// before reservation opens any descriptor for the source. Linux releases a
// process's traditional F_SETLK locks for an inode when *any* descriptor for
// that inode closes; a locally open modernc SQLite connection would otherwise
// lose locks when the OFD reservation releases its independent descriptors.
func ensureNoSQLiteSourceDescriptors(databasePath string) error {
	targetPaths := make(map[string]struct{}, 4)
	targetIdentities := make(map[string]string, 4)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		path, err := canonicalPath(databasePath + suffix)
		if err != nil {
			return fmt.Errorf("canonicalizing SQLite source descriptor path: %w", err)
		}
		targetPaths[path] = struct{}{}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stating SQLite source descriptor target: %w", err)
		}
		if info.Mode().IsRegular() {
			targetIdentities[platformFileIdentity(info)] = path
		}
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("listing process descriptors before SQLite fence: %w", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			return fmt.Errorf("parsing process descriptor number %q before SQLite fence: %w", entry.Name(), err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err == nil {
			identity := fmt.Sprintf("dev=%d;ino=%d", stat.Dev, stat.Ino)
			if target, ok := targetIdentities[identity]; ok {
				return fmt.Errorf("%w: %q", ErrSQLiteSourceOpenInProcess, target)
			}
		} else if !errors.Is(err, unix.EBADF) {
			return fmt.Errorf("stating process descriptor %d before SQLite fence: %w", fd, err)
		}
		link, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading process descriptor before SQLite fence: %w", err)
		}
		if !filepath.IsAbs(link) {
			continue
		}
		link = strings.TrimSuffix(link, " (deleted)")
		canonical, err := canonicalPath(link)
		if err != nil {
			continue
		}
		if _, ok := targetPaths[canonical]; ok {
			return fmt.Errorf("%w: %q", ErrSQLiteSourceOpenInProcess, canonical)
		}
	}
	return nil
}
