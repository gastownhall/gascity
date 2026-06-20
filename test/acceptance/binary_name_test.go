//go:build acceptance_a

// Binary name acceptance tests.
//
// These verify the configurable binary name system end-to-end: build-time
// ldflags, runtime os.Args[0] detection, and symlink-based name override.
// Each test builds the real binary with a custom -X main.binaryName and
// asserts that help output, error messages, and subcommand help all reflect
// the correct name.
package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// buildBinaryWithName compiles the gc binary into dir with the given
// binaryName baked in via ldflags. Returns the path to the compiled binary.
func buildBinaryWithName(t *testing.T, dir, name string) string {
	t.Helper()
	binPath := filepath.Join(dir, name)
	ldflags := "-X main.binaryName=" + name
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", binPath, "./cmd/gc")
	cmd.Dir = helpers.FindModuleRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building binary %q: %v\n%s", name, err, out)
	}
	return binPath
}

// runBinary executes the binary at binPath with the given args and returns
// combined stdout+stderr output.
func runBinary(t *testing.T, binPath string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestBinaryName_BuildTimeDefault_Help(t *testing.T) {
	dir := t.TempDir()
	bin := buildBinaryWithName(t, dir, "city")

	out, err := runBinary(t, bin, "--help")
	if err != nil {
		t.Fatalf("city --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "city") {
		t.Errorf("expected help output to contain %q, got:\n%s", "city", out)
	}
}

func TestBinaryName_Symlink_OverridesName(t *testing.T) {
	dir := t.TempDir()
	bin := buildBinaryWithName(t, dir, "testprog")

	symPath := filepath.Join(dir, "meow")
	if err := os.Symlink(bin, symPath); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	out, err := runBinary(t, symPath, "--help")
	if err != nil {
		t.Fatalf("meow --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "meow") {
		t.Errorf("expected help output to contain %q, got:\n%s", "meow", out)
	}
	if strings.Contains(out, "testprog") {
		t.Errorf("expected help output NOT to contain build-time name %q when invoked as symlink, got:\n%s", "testprog", out)
	}
}

func TestBinaryName_ArbitrarySymlink_Works(t *testing.T) {
	dir := t.TempDir()
	bin := buildBinaryWithName(t, dir, "gc")

	symPath := filepath.Join(dir, "wombat")
	if err := os.Symlink(bin, symPath); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	out, err := runBinary(t, symPath, "--help")
	if err != nil {
		t.Fatalf("wombat --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wombat") {
		t.Errorf("expected help output to contain %q, got:\n%s", "wombat", out)
	}
}

func TestBinaryName_ErrorMessages_UseCorrectName(t *testing.T) {
	dir := t.TempDir()
	bin := buildBinaryWithName(t, dir, "city")

	symPath := filepath.Join(dir, "meow")
	if err := os.Symlink(bin, symPath); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	// Run an unknown subcommand to trigger an error message.
	out, _ := runBinary(t, symPath, "nosuchcommand")
	// Cobra error format: 'unknown command "nosuchcommand" for "meow"'
	if !strings.Contains(out, "meow") {
		t.Errorf("expected error output to reference %q, got:\n%s", "meow", out)
	}
}

func TestBinaryName_SubcommandHelp_UsesCorrectName(t *testing.T) {
	dir := t.TempDir()
	bin := buildBinaryWithName(t, dir, "city")

	symPath := filepath.Join(dir, "meow")
	if err := os.Symlink(bin, symPath); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	// Check subcommand help shows the symlink name.
	out, err := runBinary(t, symPath, "version", "--help")
	if err != nil {
		t.Fatalf("meow version --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "meow") {
		t.Errorf("expected subcommand help to contain %q, got:\n%s", "meow", out)
	}
}

func TestBinaryName_DirectInvocation_MatchesBuildName(t *testing.T) {
	dir := t.TempDir()
	bin := buildBinaryWithName(t, dir, "city")

	out, err := runBinary(t, bin, "--help")
	if err != nil {
		t.Fatalf("city --help failed: %v\n%s", err, out)
	}
	// When invoked directly (not via symlink), the name should be "city"
	// (derived from os.Args[0] basename which is the binary filename).
	if !strings.Contains(out, "city") {
		t.Errorf("expected direct invocation help to contain %q, got:\n%s", "city", out)
	}
}
