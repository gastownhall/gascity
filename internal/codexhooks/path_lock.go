package codexhooks

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// WithPathLock serializes Gas City read/merge/write operations for target
// across processes. In-memory filesystems retain their existing in-process
// synchronization; only the real OS filesystem can participate in a file
// lock shared by independent processes.
func WithPathLock(fs fsys.FS, target string, fn func() error) error {
	if !isOSFS(fs) {
		return fn()
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("creating Codex hooks parent %s: %w", parent, err)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspecting Codex hooks parent %s: %w", parent, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-directory or symlinked Codex hooks parent %s", parent)
	}
	return withExclusiveOSPathLock(parent, ".hooks.json.gascity.lock", fn)
}

// WriteFileAtomicNoFollow publishes target through a pinned directory handle
// on the OS filesystem, so a leaf symlink is replaced rather than followed and
// a symlinked immediate parent is rejected. Test filesystems use their normal
// atomic rename primitive after the same higher-level validation.
func WriteFileAtomicNoFollow(fs fsys.FS, target string, data []byte, perm os.FileMode) error {
	if !isOSFS(fs) {
		return fsys.WriteFileAtomic(fs, target, data, perm)
	}
	return writeFileAtomicNoFollowOS(target, data, perm)
}

func isOSFS(fs fsys.FS) bool {
	switch fs.(type) {
	case fsys.OSFS, *fsys.OSFS:
		return true
	default:
		return false
	}
}
