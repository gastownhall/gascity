package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeScriptFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveScripts_SingleLayer(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "pack", "scripts")
	writeScriptFile(t, layer, "setup.sh", "#!/bin/sh\necho setup")
	writeScriptFile(t, layer, "teardown.sh", "#!/bin/sh\necho teardown")

	target := filepath.Join(dir, "city")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("ResolveScripts: %v", err)
	}

	symlinkDir := filepath.Join(target, "scripts")
	for _, name := range []string{"setup.sh", "teardown.sh"} {
		linkPath := filepath.Join(symlinkDir, name)
		fi, err := os.Lstat(linkPath)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: not a symlink", name)
		}
		dest, err := os.Readlink(linkPath)
		if err != nil {
			t.Errorf("%s: readlink: %v", name, err)
			continue
		}
		want := filepath.Join(layer, name)
		if dest != want {
			t.Errorf("%s: link target = %q, want %q", name, dest, want)
		}
	}
}

func TestResolveScripts_Subdirectory(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "pack", "scripts")
	writeScriptFile(t, layer, "checks/review-approved.sh", "#!/bin/sh\nexit 0")
	writeScriptFile(t, layer, "checks/design-approved.sh", "#!/bin/sh\nexit 0")
	writeScriptFile(t, layer, "top-level.sh", "#!/bin/sh\necho hi")

	target := filepath.Join(dir, "city")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("ResolveScripts: %v", err)
	}

	symlinkDir := filepath.Join(target, "scripts")

	// Check nested file.
	linkPath := filepath.Join(symlinkDir, "checks", "review-approved.sh")
	fi, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("checks/review-approved.sh: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("checks/review-approved.sh: not a symlink")
	}
	dest, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if dest != filepath.Join(layer, "checks", "review-approved.sh") {
		t.Errorf("link target = %q, want pack source", dest)
	}

	// Check top-level file.
	if _, err := os.Lstat(filepath.Join(symlinkDir, "top-level.sh")); err != nil {
		t.Errorf("top-level.sh should exist: %v", err)
	}
}

func TestResolveScripts_Shadow(t *testing.T) {
	dir := t.TempDir()
	layer1 := filepath.Join(dir, "pack1", "scripts")
	layer2 := filepath.Join(dir, "pack2", "scripts")

	writeScriptFile(t, layer1, "setup.sh", "layer1 version")
	writeScriptFile(t, layer1, "only-in-1.sh", "layer1 only")
	writeScriptFile(t, layer2, "setup.sh", "layer2 version")
	writeScriptFile(t, layer2, "only-in-2.sh", "layer2 only")

	target := filepath.Join(dir, "city")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveScripts(target, []string{layer1, layer2}); err != nil {
		t.Fatalf("ResolveScripts: %v", err)
	}

	symlinkDir := filepath.Join(target, "scripts")

	// setup.sh should point to layer2 (higher priority).
	dest, err := os.Readlink(filepath.Join(symlinkDir, "setup.sh"))
	if err != nil {
		t.Fatalf("setup.sh readlink: %v", err)
	}
	if dest != filepath.Join(layer2, "setup.sh") {
		t.Errorf("setup.sh target = %q, want layer2 version", dest)
	}

	// only-in-1.sh should point to layer1.
	dest, err = os.Readlink(filepath.Join(symlinkDir, "only-in-1.sh"))
	if err != nil {
		t.Fatalf("only-in-1.sh readlink: %v", err)
	}
	if dest != filepath.Join(layer1, "only-in-1.sh") {
		t.Errorf("only-in-1.sh target = %q, want layer1 version", dest)
	}

	// only-in-2.sh should point to layer2.
	dest, err = os.Readlink(filepath.Join(symlinkDir, "only-in-2.sh"))
	if err != nil {
		t.Fatalf("only-in-2.sh readlink: %v", err)
	}
	if dest != filepath.Join(layer2, "only-in-2.sh") {
		t.Errorf("only-in-2.sh target = %q, want layer2 version", dest)
	}
}

func TestResolveScripts_Idempotent(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "pack", "scripts")
	writeScriptFile(t, layer, "setup.sh", "#!/bin/sh\necho setup")

	target := filepath.Join(dir, "city")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("first ResolveScripts: %v", err)
	}
	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("second ResolveScripts: %v", err)
	}

	dest, err := os.Readlink(filepath.Join(target, "scripts", "setup.sh"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if dest != filepath.Join(layer, "setup.sh") {
		t.Errorf("symlink target = %q, want %q", dest, filepath.Join(layer, "setup.sh"))
	}
}

func TestResolveScripts_StaleCleanup(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "pack", "scripts")
	writeScriptFile(t, layer, "setup.sh", "setup")
	writeScriptFile(t, layer, "old.sh", "old")

	target := filepath.Join(dir, "city")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	// First pass: both scripts.
	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("first ResolveScripts: %v", err)
	}

	// Remove old.sh from the layer.
	os.Remove(filepath.Join(layer, "old.sh")) //nolint:errcheck

	// Second pass.
	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("second ResolveScripts: %v", err)
	}

	// setup.sh should still exist.
	if _, err := os.Lstat(filepath.Join(target, "scripts", "setup.sh")); err != nil {
		t.Errorf("setup.sh should still exist: %v", err)
	}

	// old.sh should be removed.
	if _, err := os.Lstat(filepath.Join(target, "scripts", "old.sh")); !os.IsNotExist(err) {
		t.Error("old.sh should have been removed (stale symlink)")
	}
}

func TestResolveScripts_RealFileNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "pack", "scripts")
	writeScriptFile(t, layer, "setup.sh", "layer version")

	target := filepath.Join(dir, "city")
	symlinkDir := filepath.Join(target, "scripts")
	os.MkdirAll(symlinkDir, 0o755) //nolint:errcheck

	// Create a real file in the target.
	realFile := filepath.Join(symlinkDir, "setup.sh")
	os.WriteFile(realFile, []byte("real file"), 0o644) //nolint:errcheck

	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("ResolveScripts: %v", err)
	}

	fi, err := os.Lstat(realFile)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("real file should not have been replaced with symlink")
	}
	content, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "real file" {
		t.Errorf("real file content = %q, want %q", content, "real file")
	}
}

func TestResolveScripts_EmptyLayers(t *testing.T) {
	if err := ResolveScripts("/tmp/nonexistent", nil); err != nil {
		t.Errorf("nil layers should be no-op: %v", err)
	}
	if err := ResolveScripts("/tmp/nonexistent", []string{}); err != nil {
		t.Errorf("empty layers should be no-op: %v", err)
	}
}

func TestResolveScripts_MissingLayerDir(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "pack", "scripts")
	writeScriptFile(t, layer, "setup.sh", "setup")

	target := filepath.Join(dir, "city")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	// Include a missing dir — should be skipped.
	if err := ResolveScripts(target, []string{"/nonexistent", layer}); err != nil {
		t.Fatalf("ResolveScripts: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(target, "scripts", "setup.sh")); err != nil {
		t.Errorf("setup.sh should exist: %v", err)
	}
}

func TestResolveScripts_EmptySubdirCleanup(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "pack", "scripts")
	writeScriptFile(t, layer, "checks/review.sh", "review")

	target := filepath.Join(dir, "city")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	// First pass: creates checks/ subdir.
	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("first ResolveScripts: %v", err)
	}

	// Remove the script from the layer.
	os.Remove(filepath.Join(layer, "checks", "review.sh")) //nolint:errcheck

	// Second pass: stale symlink and empty dir should be cleaned.
	if err := ResolveScripts(target, []string{layer}); err != nil {
		t.Fatalf("second ResolveScripts: %v", err)
	}

	checksDir := filepath.Join(target, "scripts", "checks")
	if _, err := os.Stat(checksDir); !os.IsNotExist(err) {
		t.Errorf("empty checks/ subdir should have been removed, err=%v", err)
	}
}
