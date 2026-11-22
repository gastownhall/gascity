package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

// TestMakeInstallStopsBeforeGCWhenInstallBdFails is AC-4: install-bd runs
// before gc is copied, so a failed bd build leaves the previously installed
// gc and gc-osc7501 untouched and the installed pair stays consistent
// (the old gc with the old bd) instead of pairing a new gc with a stale bd.
func TestMakeInstallStopsBeforeGCWhenInstallBdFails(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	buildDir := filepath.Join(tmp, "build")
	installDir := filepath.Join(tmp, "install")
	if err := os.Mkdir(buildDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", buildDir, err)
	}
	if err := os.Mkdir(installDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", installDir, err)
	}
	// A fresh gc waiting in BUILD_DIR and an older pair already installed.
	if err := os.WriteFile(filepath.Join(buildDir, "gc"), []byte("new gc"), 0o755); err != nil {
		t.Fatalf("write build gc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "gc-osc7501"), []byte("new companion"), 0o755); err != nil {
		t.Fatalf("write build companion: %v", err)
	}
	oldBinaries := map[string]string{}
	for _, bin := range []string{"gc", "gc-osc7501"} {
		old := "old " + bin
		if err := os.WriteFile(filepath.Join(installDir, bin), []byte(old), 0o755); err != nil {
			t.Fatalf("write installed %s: %v", bin, err)
		}
		oldBinaries[bin] = old
	}
	failingScript := filepath.Join(tmp, "fail-install-bd.sh")
	writeExecutable(t, failingScript, "#!/bin/sh\necho 'install-bd stub: bd build failed' >&2\nexit 1\n")

	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefileText := string(makefile)
	if !strings.Contains(makefileText, "\ninstall: check-self-contained install-bd\n") {
		t.Fatal("Makefile install target no longer depends on check-self-contained and install-bd as expected")
	}
	// check-self-contained is host-dependent (a Nix ICU probe); drop it and
	// point install-bd at the failing stub so only the bd failure is under
	// test.
	makefileContent := strings.Replace(makefileText, "\ninstall: check-self-contained install-bd\n", "\ninstall: install-bd\n", 1)
	makefileContent = strings.Replace(makefileContent, "scripts/install-bd-pinned.sh", failingScript, 1)
	testMakefile := filepath.Join(tmp, "Makefile")
	if err := os.WriteFile(testMakefile, []byte(makefileContent), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}

	cmd := exec.Command("make", "--no-print-directory", "-f", testMakefile, "install",
		"BUILD_DIR="+buildDir,
		"INSTALL_DIR="+installDir,
		"BINARY=gc",
		"COMPANION=gc-osc7501",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(tmp, "home"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("make install succeeded although install-bd failed:\n%s", out)
	}
	for bin, want := range oldBinaries {
		content, readErr := os.ReadFile(filepath.Join(installDir, bin))
		if readErr != nil {
			t.Fatalf("read installed %s: %v\nmake output:\n%s", bin, readErr, out)
		}
		if string(content) != want {
			t.Fatalf("installed %s = %q after a failed install-bd, want %q; a new gc must not ship with a stale bd:\n%s", bin, content, want, out)
		}
	}
}
