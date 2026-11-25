package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestModuleRootResolution pins the order moduleRoot resolves the scan root
// in. The Bazel arm is the one that matters: a test elsewhere in the same
// process can leave the working directory inside an unrelated tree that holds
// a go.mod, and a root derived from the cwd then points the storage source
// scans at a tree with nothing to scan.
func TestModuleRootResolution(t *testing.T) {
	srcDir := t.TempDir()
	runfiles := filepath.Join(srcDir, "_main")
	writeModuleScanFile(t, runfiles, "go.mod", "module runfiles.example\n")
	stray := t.TempDir()
	writeModuleScanFile(t, stray, "go.mod", "module stray.example\n")
	writeModuleScanFile(t, stray, filepath.Join("sub", "keep"), "")

	t.Run("runfiles tree wins over a stray go.mod in the cwd", func(t *testing.T) {
		t.Setenv("GC_TEST_REPO_ROOT", "")
		t.Setenv("TEST_SRCDIR", srcDir)
		t.Setenv("TEST_WORKSPACE", "_main")
		t.Chdir(stray)
		if got := moduleRoot(t); got != runfiles {
			t.Fatalf("moduleRoot(t) = %q, want the runfiles workspace %q (cwd %q holds an unrelated go.mod)", got, runfiles, stray)
		}
	})

	t.Run("explicit override wins over the runfiles tree", func(t *testing.T) {
		override := t.TempDir()
		t.Setenv("GC_TEST_REPO_ROOT", override)
		t.Setenv("TEST_SRCDIR", srcDir)
		t.Setenv("TEST_WORKSPACE", "_main")
		t.Chdir(stray)
		if got := moduleRoot(t); got != override {
			t.Fatalf("moduleRoot(t) = %q, want the GC_TEST_REPO_ROOT override %q", got, override)
		}
	})

	t.Run("plain go test walks up from the cwd", func(t *testing.T) {
		t.Setenv("GC_TEST_REPO_ROOT", "")
		t.Setenv("TEST_SRCDIR", "")
		t.Setenv("TEST_WORKSPACE", "")
		t.Chdir(filepath.Join(stray, "sub"))
		if got := moduleRoot(t); got != stray {
			t.Fatalf("moduleRoot(t) = %q, want the nearest go.mod directory %q", got, stray)
		}
	})
}

// TestModuleGoFilesRefusesAnEmptyScan proves a scan root with no non-test Go
// source is an error naming the root and the working directory, not an empty
// list the census tests would read as "nothing found".
func TestModuleGoFilesRefusesAnEmptyScan(t *testing.T) {
	root := t.TempDir()
	runModuleScanGit(t, root, "init", "-q")
	runModuleScanGit(t, root, "config", "user.email", "fixture@example.com")
	runModuleScanGit(t, root, "config", "user.name", "fixture")
	// Tracked files that the scan deliberately skips must not satisfy it.
	writeModuleScanFile(t, root, "only_test.go", "package fixture\n")
	writeModuleScanFile(t, root, filepath.Join("testdata", "fixture.go"), "package fixture\n")
	runModuleScanGit(t, root, "add", ".")

	cwd := t.TempDir()
	t.Chdir(cwd)

	files, err := listModuleGoFiles(root)
	if err == nil {
		t.Fatalf("listModuleGoFiles(%q) = %v, nil; want an error for a root with no non-test Go files", root, files)
	}
	for _, want := range []string{root, cwd} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("listModuleGoFiles error %q does not name %q", err, want)
		}
	}
}
