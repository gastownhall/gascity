//go:build !windows

package subprocess

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestPrivateFallbackRootIsUnchangedByTheLocalsocketCutover pins the exact
// pathname this package produced before the fallback root moved into
// localsocket. The root is a shared /tmp deliberately — TMPDIR on macOS
// resolves under /var/folders, which is long enough on its own to defeat the
// budget the fallback exists to satisfy — so a change here is a change to
// every host's live socket paths, not a refactor.
func TestPrivateFallbackRootIsUnchangedByTheLocalsocketCutover(t *testing.T) {
	euid := os.Geteuid()
	want := filepath.Join("/tmp", fmt.Sprintf("%s-%d", fallbackSocketDirName, euid))
	if got := privateFallbackRoot(euid); got != want {
		t.Fatalf("privateFallbackRoot(%d) = %q, want %q", euid, got, want)
	}
}

// TestPrivateFallbackRootIsNamespacedByEUID holds the reason the euid is in the
// name on this platform: /tmp is host-shared, so two users resolve to one path
// without it and the first to create the directory owns the other's sockets.
func TestPrivateFallbackRootIsNamespacedByEUID(t *testing.T) {
	if privateFallbackRoot(0) == privateFallbackRoot(1) {
		t.Fatal("two euids share one private fallback root")
	}
}
