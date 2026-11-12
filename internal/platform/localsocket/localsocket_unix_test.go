//go:build !windows

package localsocket

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestListenAppliesSocketModeOverPermissiveUmask pins that SocketMode is the
// mode the endpoint ends up with, not whatever a permissive umask would have
// left: under umask 002 a fresh socket is group-writable unless Listen
// tightens it. It changes the process umask, so it must not run in parallel.
func TestListenAppliesSocketModeOverPermissiveUmask(t *testing.T) {
	old := syscall.Umask(0o002)
	t.Cleanup(func() { syscall.Umask(old) })

	sock := filepath.Join(shortTempDir(t), "mode.sock")
	lis, err := Listen(sock, Options{DirMode: 0o700, SocketMode: 0o600})
	if err != nil {
		t.Fatalf("Listen(%q): %v", sock, err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat %s: %v", sock, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s mode = %v, want a socket", sock, info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s permissions = %#o, want 0600", sock, got)
	}
}
