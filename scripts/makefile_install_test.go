package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMakeInstallFailsClosedWhenCopyFails(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(tmp, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(path, 0o755)
			} else {
				_ = os.Chmod(path, 0o644)
			}
			return nil
		})
	})
	buildDir := filepath.Join(tmp, "build")
	installDir := filepath.Join(tmp, "install")
	binDir := filepath.Join(tmp, "bin")
	for _, dir := range []string{buildDir, installDir, binDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	sourceBinary := filepath.Join(buildDir, "gc")
	if err := os.WriteFile(sourceBinary, []byte("new binary"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	installedBinary := filepath.Join(installDir, "gc")
	if err := os.WriteFile(installedBinary, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("write installed binary: %v", err)
	}

	writeExecutable(t, filepath.Join(binDir, "cp"), `#!/usr/bin/env sh
for last do :; done
printf 'partial binary' > "$last"
exit 1
`)

	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	testMakefile := filepath.Join(tmp, "Makefile")
	makefileText := string(makefile)
	if !strings.Contains(makefileText, "\ninstall: check-self-contained\n") {
		t.Fatal("Makefile install target no longer depends on check-self-contained as expected")
	}
	makefileContent := strings.Replace(makefileText, "\ninstall: check-self-contained\n", "\ninstall:\n", 1)
	if err := os.WriteFile(testMakefile, []byte(makefileContent), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}

	cmd := exec.Command("make", "--no-print-directory", "-f", testMakefile, "install",
		"BUILD_DIR="+buildDir,
		"INSTALL_DIR="+installDir,
		"BINARY=gc",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+filepath.Join(tmp, "home"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("make install succeeded after cp failure:\n%s", out)
	}

	content, readErr := os.ReadFile(installedBinary)
	if readErr != nil {
		t.Fatalf("read installed binary: %v\nmake output:\n%s", readErr, out)
	}
	if string(content) != "old binary" {
		t.Fatalf("installed binary = %q, want old binary after cp failure\nmake output:\n%s", content, out)
	}

	entries, readDirErr := os.ReadDir(installDir)
	if readDirErr != nil {
		t.Fatalf("read install dir: %v", readDirErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gc.tmp.") {
			t.Fatalf("temporary install file was not cleaned up: %s\nmake output:\n%s", entry.Name(), out)
		}
	}
}

// TestMakeInstallOapiCodegenRetriesTransientGoInstallFailure pins that the
// install-oapi-codegen prerequisite of spec-ci retries a transient module-proxy
// failure the way $(GOLANGCI_LINT) has since #2041, and still fails closed once
// the retries run out. Unretried, one proxy.golang.org stream error failed the
// required Check on main at 7416d68a3e before spec-ci ever compared a spec
// (ga-6hg5mi).
func TestMakeInstallOapiCodegenRetriesTransientGoInstallFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failures     int
		wantSuccess  bool
		wantAttempts int
	}{
		{name: "transient", failures: 2, wantSuccess: true, wantAttempts: 3},
		{name: "persistent", failures: 99, wantSuccess: false, wantAttempts: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := repoRoot(t)
			tmp := t.TempDir()
			binDir := filepath.Join(tmp, "bin")
			attemptsDir := filepath.Join(tmp, "attempts")
			for _, dir := range []string{binDir, attemptsDir} {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}

			// The first $GO_INSTALL_FAILURES `go install` calls fail with the
			// error proxy.golang.org returned on 7416d68a3e; any other go call
			// (the Makefile's parse-time `go env` probes) succeeds silently.
			// Builtins only, so the filtered PATH below cannot break the fake.
			writeExecutable(t, filepath.Join(binDir, "go"), `#!/bin/sh
[ "$1" = install ] || exit 0
n=1
while [ -e "$GO_INSTALL_ATTEMPTS_DIR/$n" ]; do n=$((n + 1)); done
: > "$GO_INSTALL_ATTEMPTS_DIR/$n"
if [ "$n" -le "$GO_INSTALL_FAILURES" ]; then
	echo 'golang.org/x/tools@v0.30.0: read "https://proxy.golang.org/golang.org/x/tools/@v/v0.30.0.zip": stream error: stream ID 49; INTERNAL_ERROR; received from peer' >&2
	exit 1
fi
`)
			writeExecutable(t, filepath.Join(binDir, "sleep"), "#!/bin/sh\n")

			cmd := exec.Command("make", "--no-print-directory", "-f", filepath.Join(repoRoot, "Makefile"), "install-oapi-codegen")
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(),
				"PATH="+binDir+string(os.PathListSeparator)+pathWithoutCommand("oapi-codegen"),
				"GO_INSTALL_ATTEMPTS_DIR="+attemptsDir,
				"GO_INSTALL_FAILURES="+strconv.Itoa(tc.failures),
			)
			out, err := cmd.CombinedOutput()
			if tc.wantSuccess && err != nil {
				t.Fatalf("make install-oapi-codegen failed on a transient go install error: %v\n%s", err, out)
			}
			if !tc.wantSuccess {
				if err == nil {
					t.Fatalf("make install-oapi-codegen succeeded although every go install failed:\n%s", out)
				}
				if !strings.Contains(string(out), "ERROR: failed to install oapi-codegen") {
					t.Fatalf("make output lacks the terminal install error:\n%s", out)
				}
			}

			attempts, err := os.ReadDir(attemptsDir)
			if err != nil {
				t.Fatalf("read attempts dir: %v", err)
			}
			if len(attempts) != tc.wantAttempts {
				t.Fatalf("go install attempts = %d, want %d\nmake output:\n%s", len(attempts), tc.wantAttempts, out)
			}
		})
	}
}

// pathWithoutCommand returns $PATH minus every directory holding name, so an
// installed copy cannot satisfy a Makefile's `command -v` guard and skip the
// install path under test.
func pathWithoutCommand(name string) string {
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}
