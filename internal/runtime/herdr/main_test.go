package herdr

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// TestHerdrTestsRunOnAPrivateConfigRoot pins TestMain's isolation: the live
// tier's fixed session names must resolve to a root only this test process
// uses, never the invoking user's config dir (ga-19onv3).
func TestHerdrTestsRunOnAPrivateConfigRoot(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("the private config root is Linux-only; see TestMain")
	}
	root := os.Getenv("XDG_CONFIG_HOME")
	if !strings.HasPrefix(filepath.Base(root), "gchx") || filepath.Dir(root) != filepath.Clean(os.TempDir()) {
		t.Fatalf("XDG_CONFIG_HOME = %q; want a private gchx* directory directly under %s", root, os.TempDir())
	}
	if got := newClient("privroot", "").socketPath(); !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Errorf("socketPath() = %q; want it under the private root %q", got, root)
	}
}
