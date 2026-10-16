package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestInstallBdPinnedWiring guards the gc↔bd version-lockstep install path:
// `make install` must also install a bd built from the beads commit
// go.mod pins, and the script must keep the properties that make that safe —
// pin from go.mod, scratch-module build outside the repo, post-build embed
// verification, atomic install. Without it gc and bd drift apart: gc migrated the city stores to a schema the standalone bd on
// PATH did not know, and every bare-bd caller failed.
func TestInstallBdPinnedWiring(t *testing.T) {
	root := repoRoot(t)

	scriptPath := filepath.Join(root, "scripts", "install-bd-pinned.sh")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("install-bd-pinned.sh missing: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("install-bd-pinned.sh is not executable (mode %v)", info.Mode())
	}
	script := readFile(t, root, filepath.Join("scripts", "install-bd-pinned.sh"))

	for _, want := range []struct{ pattern, why string }{
		{
			`go list -m -f '\{\{\.Version\}\}' github\.com/steveyegge/beads`,
			"the pin must come from go.mod, the version gc actually compiles against",
		},
		{
			`mktemp -d /var/tmp/`,
			"the scratch build must live on disk, never the shared /tmp tmpfs, and never inside the gascity module (building bd there mutates go.sum)",
		},
		{
			`go version -m`,
			"the script must verify the beads version the built/installed binary actually embeds, not trust a version string",
		},
		{
			`mv -f "\$tmp" "\$installed"`,
			"the install must be atomic: write a temp file, then rename onto the target",
		},
	} {
		if !regexp.MustCompile(want.pattern).MatchString(script) {
			t.Errorf("install-bd-pinned.sh no longer matches %q — %s", want.pattern, want.why)
		}
	}

	makefile := readFile(t, root, "Makefile")
	installLine := regexp.MustCompile(`(?m)^install:.*$`).FindString(makefile)
	if installLine == "" {
		t.Fatal("Makefile has no install target")
	}
	if !strings.Contains(installLine, "install-bd") {
		t.Fatalf("Makefile `install` target does not depend on install-bd (line %q); gc and bd would drift apart again", installLine)
	}
	if !regexp.MustCompile(`(?m)^install-bd:`).MatchString(makefile) {
		t.Fatal("Makefile missing the install-bd target")
	}
	if !regexp.MustCompile(`(?m)^\.PHONY:.*\binstall-bd\b`).MatchString(makefile) {
		t.Fatal("install-bd is not declared .PHONY")
	}
}
