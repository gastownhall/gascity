package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// withTargetLock serializes writers that target the same provider-native MCP
// file. The lock sits under .gc/mcp-locks/<sha256(provider|target)>.lock and
// is acquired with flock(LOCK_EX) so supervisor ticks and stage-2 pre-start
// commands cannot interleave read-modify-write against the same file.
//
// Lock files persist across runs (the flock is released when the fd closes);
// they are small (a single process PID written on first acquire) and reused
// on subsequent runs, so no cleanup path is required.
//
// The lock only engages for the real OSFS. Tests using fsys.Fake never race
// against another process and should not pay the filesystem cost — callers
// pass the lock root explicitly and skip it for in-memory fixtures.
func withTargetLock(lockRoot, provider, target string, fn func() error) error {
	if lockRoot == "" {
		return fn()
	}
	if err := os.MkdirAll(lockRoot, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", lockRoot, err)
	}
	sum := sha256.Sum256([]byte(provider + "|" + target))
	lockPath := filepath.Join(lockRoot, hex.EncodeToString(sum[:])+".lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening lock %s: %w", lockPath, err)
	}
	defer f.Close() //nolint:errcheck // lock released on close
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking %s: %w", lockPath, err)
	}
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}()
	return fn()
}

// lockRootForProjection returns the .gc/mcp-locks directory under the
// projection's workspace root. Callers using the real OS filesystem pass
// this into withTargetLock; in-memory tests pass an empty string.
func lockRootForProjection(p MCPProjection) string {
	return filepath.Join(p.Root, ".gc", "mcp-locks")
}
