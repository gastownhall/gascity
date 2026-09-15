package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakeDryRunPackageListsAreNonEmpty is a regression guard: under normal
// conditions, MAC_UNIT_PKGS and UNIT_COVER_PKGS_NONCMDGC must expand to a
// real package list. See ga-1qp5qo.
func TestMakeDryRunPackageListsAreNonEmpty(t *testing.T) {
	repoRoot := repoRoot(t)

	for _, target := range []string{"test-mac", "test-cover-noncmdgc"} {
		t.Run(target, func(t *testing.T) {
			cmd := exec.Command("make", "--no-print-directory", "-n", target)
			cmd.Dir = repoRoot
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make -n %s failed: %v\n%s", target, err, out)
			}
			if !strings.Contains(string(out), "github.com/gastownhall/gascity/internal/") {
				t.Fatalf("make -n %s produced an empty package list:\n%s", target, out)
			}
		})
	}
}

// TestMakeDryRunFailsLoudlyWhenGoListFails is the RED target for ga-1qp5qo:
// MAC_UNIT_PKGS (Makefile:458) and UNIT_COVER_PKGS_NONCMDGC (Makefile:778)
// compute their package lists via $(shell go list ...), evaluated outside
// the recipe's env -i TEST_ENV wrapper. $(shell ...) swallows a non-zero
// exit and yields an empty variable, so a broken `go list` today silently
// produces `go test`/`go test -coverprofile=...` with zero package
// arguments and make still exits 0 -- confirmed locally via a PATH shim
// that fails only `go list`. A target that intends to test N packages must
// never run go test with zero package arguments; it must fail the build
// instead.
func TestMakeDryRunFailsLoudlyWhenGoListFails(t *testing.T) {
	repoRoot := repoRoot(t)
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("look up go: %v", err)
	}

	for _, target := range []string{"test-mac", "test-cover-noncmdgc"} {
		t.Run(target, func(t *testing.T) {
			tmp := t.TempDir()
			binDir := filepath.Join(tmp, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}
			writeExecutable(t, filepath.Join(binDir, "go"), "#!/usr/bin/env sh\n"+
				"if [ \"$1\" = \"list\" ]; then\n"+
				"  exit 1\n"+
				"fi\n"+
				"exec \""+realGo+"\" \"$@\"\n")

			cmd := exec.Command("make", "--no-print-directory", "-n", target)
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("make -n %s must fail loudly when go list fails, but exited 0:\n%s", target, out)
			}
		})
	}
}
