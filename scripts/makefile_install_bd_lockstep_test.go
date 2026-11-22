package scripts_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// TestMakeInstallAlsoInstallsPinnedBD pins that `make install` keeps the bd on
// PATH in lockstep with the beads module gc links (ga-upttvz). gc migrates a
// city store forward to its linked beads schema; a bd from another beads
// version then fails every read with "schema version mismatch". The install
// recipe must therefore build and install a matching bd alongside gc.
func TestMakeInstallAlsoInstallsPinnedBD(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	cmd := exec.Command("make", "--no-print-directory", "-n", "install",
		"INSTALL_DIR="+filepath.Join(tmp, "install"),
	)
	cmd.Dir = repoRoot
	cmd.Env = append(cmd.Environ(), "HOME="+filepath.Join(tmp, "home"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n install: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(^|[^A-Za-z0-9_-])bd([^A-Za-z0-9_-]|$)|steveyegge/beads`).Match(out) {
		t.Fatalf("make install recipe never builds or installs a bd matching gc's beads pin; a PATH bd from another beads version is left skewed:\n%s", out)
	}
}
