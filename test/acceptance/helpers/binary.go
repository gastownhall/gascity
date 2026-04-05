package acceptancehelpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// BuildGC compiles the gc binary to dir and returns its path.
// Panics on failure — intended for TestMain.
func BuildGC(dir string) string {
	if override := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_GC_BIN")); override != "" {
		bin, err := filepath.Abs(override)
		if err != nil {
			panic("acceptance: resolving GC_ACCEPTANCE_GC_BIN: " + err.Error())
		}
		info, err := os.Stat(bin)
		if err != nil {
			panic("acceptance: GC_ACCEPTANCE_GC_BIN: " + err.Error())
		}
		if info.IsDir() {
			panic("acceptance: GC_ACCEPTANCE_GC_BIN points to a directory: " + bin)
		}
		return bin
	}

	bin := filepath.Join(dir, "gc")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gc")
	cmd.Dir = FindModuleRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("acceptance: building gc binary: " + err.Error() + "\n" + string(out))
	}
	return bin
}

// FindModuleRoot walks up from cwd to find go.mod.
func FindModuleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic("acceptance: getting cwd: " + err.Error())
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("acceptance: go.mod not found")
		}
		dir = parent
	}
}

// FindBD returns the path to the bd binary, or empty string if not found.
func FindBD() string {
	p, err := exec.LookPath("bd")
	if err != nil {
		return ""
	}
	return p
}

// RequireBD skips t if bd is not available.
func RequireBD(t *testing.T) string {
	t.Helper()
	p := FindBD()
	if p == "" {
		t.Skip("bd not available")
	}
	return p
}
