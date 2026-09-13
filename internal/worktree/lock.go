package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/git"
)

// pathLock serializes provisioning operations on one workspace path across
// processes. Acquire it with lockPath and release it with unlock; the
// acquisition itself is platform-specific and lives in lock_unix.go and
// lock_windows.go.
//
// Cleanup's safety is a check-then-act sequence: it verifies registration,
// ownership, cleanliness, reachability, and merge state, and only then removes.
// Without a lock, another process can remove that worktree and provision a new
// one at the same path in between, and the removal then lands on a workspace
// none of the checks ever examined. Ensure has the mirror-image race, where two
// callers both observe a missing path and one loses the creation.

// openLockFile creates and opens the lock file for a workspace path.
//
// The lock file lives under the repository's common git dir rather than beside
// the workspace, for two reasons. It must outlive the removal it guards, so it
// cannot live inside the workspace; and it must not litter the workspace root,
// which callers list and expect to hold only workspaces. The common dir is also
// the correct scope: every worktree of one repository shares it, so two
// processes contending for the same path always agree on the same lock file.
//
// The file is never unlinked. An unlink would let one process delete the file a
// second process is already blocked on, after which a third could create a
// fresh one and hold "the same" lock simultaneously.
func openLockFile(repoDir, path string) (*os.File, error) {
	lockFile, err := lockFilePath(repoDir, path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		return nil, fmt.Errorf("preparing worktree lock dir for %q: %w", path, err)
	}
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening worktree lock for %q: %w", path, err)
	}
	return f, nil
}

// lockFilePath derives the lock file for a workspace path. The name is a digest
// of the canonical path rather than the path itself, so that no workspace name
// can produce an invalid or colliding file name.
func lockFilePath(repoDir, path string) (string, error) {
	common, err := git.New(repoDir).CommonDir()
	if err != nil {
		return "", fmt.Errorf("locating worktree lock dir for %q: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving worktree path %q for locking: %w", path, err)
	}
	sum := sha256.Sum256([]byte(canonicalLockKey(abs)))
	return filepath.Join(common, "gc-worktree-locks", hex.EncodeToString(sum[:])+".lock"), nil
}

// canonicalLockKey reduces an absolute workspace path to the string two callers
// naming the same workspace will agree on.
//
// Two specs can name one workspace through different symlinked ancestors, and a
// lock keyed on the literal path would hand each of them a different lock file,
// which excludes nothing. This uses the same resolver the rest of the package
// uses to establish path identity, so the lock and the ownership checks cannot
// disagree about which workspace is which. It resolves as much of the path as
// exists, which matters because the workspace and even its parent usually do
// not exist yet at lock time.
//
// When nothing resolves, the cleaned path is the best key available, and using
// it is strictly better than failing to lock.
func canonicalLockKey(abs string) string {
	canonical, err := canonicalPathAllowMissing(abs)
	if err != nil {
		return filepath.Clean(abs)
	}
	return canonical
}
