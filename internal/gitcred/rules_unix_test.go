//go:build !windows

package gitcred

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStatOwnerReportsRealOwnership pins the plumbing between os.Stat and
// secureMode. Every other permission test is a rejection, and a broken
// statOwner would fail closed and still pass them; only the accept path
// depends on these values being the real uid/gid, and that path needs root to
// reproduce (see TestLoadAcceptsRootOwnedGroupReadable).
func TestStatOwnerReportsRealOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	uid, gid, ok := statOwner(info)
	if !ok {
		t.Fatalf("statOwner reported no Unix ownership for %s", path)
	}
	if uid != uint32(os.Geteuid()) {
		t.Fatalf("uid = %d, want %d", uid, os.Geteuid())
	}
	if gid != uint32(os.Getegid()) {
		t.Fatalf("gid = %d, want %d", gid, os.Getegid())
	}
}
