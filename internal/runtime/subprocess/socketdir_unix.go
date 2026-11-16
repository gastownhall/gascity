//go:build !windows

package subprocess

import (
	"fmt"
	"os"
	"syscall"
)

// privateFallbackNamespace separates one user's fallback socket directories
// from another's. The root is shared — /tmp is writable by everyone on the
// host — so the euid has to be in the name, or two legitimate users resolve to
// one path and whoever creates it first owns the other's sockets.
func privateFallbackNamespace(euid int) string {
	return fmt.Sprintf("%s-%d", fallbackSocketDirName, euid)
}

func validatePrivateSocketDirPlatform(path string, euid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("private socket directory %q is not a directory", path)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		return fmt.Errorf("private socket directory %q has mode %04o, want 0700", path, got)
	}
	if special := info.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky); special != 0 {
		return fmt.Errorf("private socket directory %q has special mode bits %v", path, special)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("private socket directory %q has unsupported ownership metadata", path)
	}
	if got, want := stat.Uid, uint32(euid); got != want {
		return fmt.Errorf("private socket directory %q is owned by uid %d, want %d", path, got, want)
	}
	return nil
}
