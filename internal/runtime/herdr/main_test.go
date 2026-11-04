package herdr

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	os.Exit(runWithPrivateHerdrConfig(m))
}

// runWithPrivateHerdrConfig runs the package's tests with XDG_CONFIG_HOME
// pointing at a directory only this process uses, and removes it afterwards.
//
// herdr keeps per-session-name state under <config>/herdr/sessions/<name>, and
// a server started under a name restores the workspaces saved in that
// directory's session.json. The live tier uses fixed session names, so on the
// invoking user's real config dir one run inherits the workspaces a previous
// (or concurrent) run left behind, and its Start can find a stale workspace
// it did not create (ga-19onv3). A private root gives every test process its
// own copy of that state.
//
// Linux only: there herdr resolves its socket from XDG_CONFIG_HOME; macOS
// resolves it elsewhere, so redirecting the variable would isolate nothing.
// The root name is short on purpose: the session socket lives beneath it and
// sockaddr_un.sun_path holds 108 bytes.
func runWithPrivateHerdrConfig(m *testing.M) int {
	if goruntime.GOOS != "linux" {
		return m.Run()
	}
	root, err := os.MkdirTemp("", "gchx")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: creating private herdr config root: %v\n", err)
		return 1
	}
	if err := os.Setenv("XDG_CONFIG_HOME", root); err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: pointing XDG_CONFIG_HOME at the private root: %v\n", err)
		_ = os.RemoveAll(root)
		return 1
	}
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: removing private herdr config root %s: %v\n", root, err)
	}
	return code
}

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
