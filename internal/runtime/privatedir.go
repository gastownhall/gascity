package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// privateDirMode is the only mode a directory holding credential-adjacent state
// may carry: owner-only, with no group or other access at all.
const privateDirMode os.FileMode = 0o700

// privateFileMode is the matching mode for the files inside such a directory.
const privateFileMode os.FileMode = 0o600

// EnsurePrivateDir creates path (and any missing parents) as an owner-only
// directory and verifies it is one, returning an error if it cannot be made
// private.
//
// The check exists because creation alone proves nothing. [os.MkdirAll] returns
// nil when the directory already exists, whatever its mode and whoever owns it,
// so a provider that only ever calls MkdirAll with 0700 will happily adopt a
// directory another user created first and read everything written into it
// afterwards. That is the realistic attack on any predictable path under
// [os.TempDir], which is where a city-less provider keeps its sidecar state.
//
// A directory this process already owns but that is merely too permissive is
// tightened in place rather than rejected: fleets upgrading across this change
// carry 0755 directories holding live session state, and failing them would
// turn a leak fix into an outage. Foreign ownership, a symlink, or a non-
// directory fails closed — none of those can be repaired by a chmod, and a
// symlink in particular can be repointed between validation and use.
func EnsurePrivateDir(path string) error { return ensurePrivateDir(path, os.Geteuid()) }

// ensurePrivateDir is the body of [EnsurePrivateDir], taking the effective uid
// as a parameter so tests can exercise the foreign-ownership branch without
// privileges.
func ensurePrivateDir(path string, euid int) error {
	if err := os.MkdirAll(path, privateDirMode); err != nil {
		return fmt.Errorf("creating private directory %q: %w", path, err)
	}
	return tightenPrivateDir(path, euid)
}

// tightenPrivateDir verifies ownership and shape, then narrows the mode if this
// process owns the directory.
func tightenPrivateDir(path string, euid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspecting private directory %q: %w", path, err)
	}
	// Lstat rather than Stat, and this check before IsDir, so the message names
	// the symlink instead of reporting a confusing "not a directory".
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private directory %q is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("private directory %q is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("private directory %q has unsupported ownership metadata", path)
	}
	if got, want := stat.Uid, uint32(euid); got != want {
		return fmt.Errorf("private directory %q is owned by uid %d, want %d", path, got, want)
	}

	// Chmod unconditionally when anything is off, including the special bits: a
	// setgid directory silently propagates group ownership to everything written
	// beneath it, which defeats the point of the 0700.
	special := info.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if info.Mode().Perm() != privateDirMode || special != 0 {
		if err := os.Chmod(path, privateDirMode); err != nil {
			return fmt.Errorf("tightening private directory %q: %w", path, err)
		}
	}
	return nil
}

// WritePrivateFile writes data to path as an owner-only file, replacing any
// existing content.
//
// Writing in place would not do. [os.WriteFile]'s perm argument applies only
// when the file is created, so on the hosts that matter most — the ones already
// carrying 0644 sidecars from an older binary — an in-place write deposits the
// new secret into the still-world-readable file and only then narrows it. A
// chmod afterwards also cannot revoke a descriptor a reader already holds.
// Writing a fresh 0600 temp file and renaming it over the target closes both:
// the visible path never exists with a wide mode, and the rename swaps the
// inode, so a held descriptor keeps the stale bytes rather than following the
// new ones.
//
// [internal/fsys.WriteFileAtomic] does the same thing and would be the obvious
// reuse, but this package is pinned stdlib-only by
// TestRuntimeContractPackageStaysStdlibOnly.
func WritePrivateFile(path string, data []byte) error {
	// Same directory as the target so the rename stays within one filesystem;
	// a cross-device rename is not atomic and would fail outright.
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for %q: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		// No-op once the rename has succeeded and the name is gone.
		_ = os.Remove(tmp)
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	// CreateTemp opens 0600, but umask can only ever narrow that, and an
	// unusual umask would leave the file unreadable to its own owner.
	if err := os.Chmod(tmp, privateFileMode); err != nil {
		return fmt.Errorf("setting mode on %q: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing %q: %w", path, err)
	}
	return nil
}
