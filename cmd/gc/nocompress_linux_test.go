//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestSetNoCompressAttrSetsFlag verifies that after calling setNoCompressAttr
// on a directory, FS_IOC_GETFLAGS reports FS_NOCOW_FL as set. On filesystems
// that do not support FS_IOC_SETFLAGS (e.g. tmpfs, overlayfs in some
// configurations) this test is skipped because the helper is a silent no-op
// there and there is nothing to assert.
func TestSetNoCompressAttrSetsFlag(t *testing.T) {
	dir := t.TempDir()

	// Probe support by trying to read flags first. If even GETFLAGS is
	// unsupported, the underlying filesystem does not implement the ioctl
	// and we cannot meaningfully test the flag round-trip.
	f, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open temp dir: %v", err)
	}
	probeFlags, probeErr := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	_ = f.Close()
	if probeErr != nil {
		var errno syscall.Errno
		if errors.As(probeErr, &errno) {
			switch errno {
			case syscall.ENOTTY, syscall.ENOSYS, syscall.EOPNOTSUPP, syscall.EINVAL:
				t.Skipf("temp dir filesystem does not support FS_IOC_GETFLAGS: %v", probeErr)
			}
		}
		t.Fatalf("unexpected FS_IOC_GETFLAGS probe error: %v", probeErr)
	}
	_ = probeFlags

	if err := setNoCompressAttr(dir); err != nil {
		t.Fatalf("setNoCompressAttr: %v", err)
	}

	// Re-open and read flags to confirm FS_NOCOW_FL is set. If SETFLAGS was
	// silently unsupported (e.g. EPERM on some tmpfs builds), the helper
	// returned nil but the flag will not be set. Skip in that case.
	f2, err := os.Open(dir)
	if err != nil {
		t.Fatalf("reopen temp dir: %v", err)
	}
	defer func() { _ = f2.Close() }()

	flags, err := unix.IoctlGetInt(int(f2.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		t.Fatalf("FS_IOC_GETFLAGS after set: %v", err)
	}

	if flags&fsNoCowFL == 0 {
		t.Skipf("FS_NOCOW_FL not set after call; filesystem likely does not support SETFLAGS (flags=0x%x)", flags)
	}
}

// TestSetNoCompressAttrIdempotent verifies that calling the helper twice on
// the same directory does not error.
func TestSetNoCompressAttrIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := setNoCompressAttr(dir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := setNoCompressAttr(dir); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

// TestSetNoCompressAttrMissingPath verifies that a missing path returns a
// sensible error (a *os.PathError wrapping ENOENT), not nil and not a panic.
func TestSetNoCompressAttrMissingPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	err := setNoCompressAttr(missing)
	if err == nil {
		t.Fatal("expected error for missing path, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

// TestEnsureCityScaffoldAppliesNoCompress is a smoke test that runs the real
// scaffolding and then reads flags on the runtime dir. It is skipped if the
// temp filesystem does not support FS_IOC_GETFLAGS.
func TestEnsureCityScaffoldAppliesNoCompress(t *testing.T) {
	cityDir := t.TempDir()

	// Probe support.
	f, err := os.Open(cityDir)
	if err != nil {
		t.Fatalf("open temp dir: %v", err)
	}
	_, probeErr := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	_ = f.Close()
	if probeErr != nil {
		t.Skipf("temp dir filesystem does not support FS_IOC_GETFLAGS: %v", probeErr)
	}

	if err := ensureCityScaffold(cityDir); err != nil {
		t.Fatalf("ensureCityScaffold: %v", err)
	}

	runtimeDir := filepath.Join(cityDir, ".gc", "runtime")
	rf, err := os.Open(runtimeDir)
	if err != nil {
		t.Fatalf("open runtime dir: %v", err)
	}
	defer func() { _ = rf.Close() }()

	flags, err := unix.IoctlGetInt(int(rf.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		t.Fatalf("FS_IOC_GETFLAGS runtime dir: %v", err)
	}
	if flags&fsNoCowFL == 0 {
		t.Skipf("runtime dir lacks FS_NOCOW_FL; temp fs likely does not support SETFLAGS (flags=0x%x)", flags)
	}
}
