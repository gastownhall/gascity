package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFormulaFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFormulas_SingleLayer(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	// Mix canonical and legacy source names — both must produce canonical
	// .toml symlinks in the target.
	writeFormulaFile(t, layer, "mol-a.toml", "formula a")
	writeFormulaFile(t, layer, "mol-b.formula.toml", "formula b")

	target := filepath.Join(dir, "rig")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	symlinkDir := filepath.Join(target, ".beads", "formulas")
	cases := []struct {
		linkName, srcName string
	}{
		{"mol-a.toml", "mol-a.toml"},
		{"mol-b.toml", "mol-b.formula.toml"},
	}
	for _, c := range cases {
		linkPath := filepath.Join(symlinkDir, c.linkName)
		fi, err := os.Lstat(linkPath)
		if err != nil {
			t.Errorf("%s: %v", c.linkName, err)
			continue
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: not a symlink", c.linkName)
		}
		dest, err := os.Readlink(linkPath)
		if err != nil {
			t.Errorf("%s: readlink: %v", c.linkName, err)
			continue
		}
		want := filepath.Join(layer, c.srcName)
		if dest != want {
			t.Errorf("%s: link target = %q, want %q", c.linkName, dest, want)
		}
	}
}

func TestResolveFormulas_Shadow(t *testing.T) {
	dir := t.TempDir()
	layer1 := filepath.Join(dir, "layer1")
	layer2 := filepath.Join(dir, "layer2")

	writeFormulaFile(t, layer1, "mol-a.formula.toml", "layer1 version")
	writeFormulaFile(t, layer1, "mol-b.formula.toml", "layer1 only")
	writeFormulaFile(t, layer2, "mol-a.formula.toml", "layer2 version")
	writeFormulaFile(t, layer2, "mol-c.formula.toml", "layer2 only")

	target := filepath.Join(dir, "rig")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveFormulas(target, []string{layer1, layer2}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	symlinkDir := filepath.Join(target, ".beads", "formulas")

	// mol-a should point to layer2 (higher priority shadow), via canonical link name.
	dest, err := os.Readlink(filepath.Join(symlinkDir, "mol-a.toml"))
	if err != nil {
		t.Fatalf("mol-a readlink: %v", err)
	}
	if dest != filepath.Join(layer2, "mol-a.formula.toml") {
		t.Errorf("mol-a target = %q, want layer2 version", dest)
	}

	// mol-b should point to layer1 (only source).
	dest, err = os.Readlink(filepath.Join(symlinkDir, "mol-b.toml"))
	if err != nil {
		t.Fatalf("mol-b readlink: %v", err)
	}
	if dest != filepath.Join(layer1, "mol-b.formula.toml") {
		t.Errorf("mol-b target = %q, want layer1 version", dest)
	}

	// mol-c should point to layer2 (only source).
	dest, err = os.Readlink(filepath.Join(symlinkDir, "mol-c.toml"))
	if err != nil {
		t.Fatalf("mol-c readlink: %v", err)
	}
	if dest != filepath.Join(layer2, "mol-c.formula.toml") {
		t.Errorf("mol-c target = %q, want layer2 version", dest)
	}
}

// TestResolveFormulas_MixedLayerPrefersCanonical verifies that within a single
// layer, a canonical .toml file wins over a sibling .formula.toml file for the
// same formula name, regardless of ReadDir order.
func TestResolveFormulas_MixedLayerPrefersCanonical(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.toml", "canonical")
	writeFormulaFile(t, layer, "mol-a.formula.toml", "legacy")

	target := filepath.Join(dir, "rig")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	symlinkDir := filepath.Join(target, ".beads", "formulas")
	// Only one canonical symlink; it must point at the canonical source.
	dest, err := os.Readlink(filepath.Join(symlinkDir, "mol-a.toml"))
	if err != nil {
		t.Fatalf("mol-a readlink: %v", err)
	}
	if dest != filepath.Join(layer, "mol-a.toml") {
		t.Errorf("mol-a target = %q, want canonical source", dest)
	}
	// The legacy-named symlink must NOT exist — we emit only canonical.
	if _, err := os.Lstat(filepath.Join(symlinkDir, "mol-a.formula.toml")); !os.IsNotExist(err) {
		t.Errorf("legacy mol-a.formula.toml symlink should not exist: %v", err)
	}
}

// TestResolveFormulas_HigherLayerLegacyWinsOverLowerCanonical verifies that a
// higher-priority layer wins even when it uses the legacy extension and the
// lower-priority layer uses the canonical extension.
func TestResolveFormulas_HigherLayerLegacyWinsOverLowerCanonical(t *testing.T) {
	dir := t.TempDir()
	layer1 := filepath.Join(dir, "layer1")
	layer2 := filepath.Join(dir, "layer2")

	writeFormulaFile(t, layer1, "mol-a.toml", "layer1 canonical")
	writeFormulaFile(t, layer2, "mol-a.formula.toml", "layer2 legacy")

	target := filepath.Join(dir, "rig")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveFormulas(target, []string{layer1, layer2}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	// The canonical link name must point to layer2's (legacy-named) source,
	// because layer2 is higher priority.
	dest, err := os.Readlink(filepath.Join(target, ".beads", "formulas", "mol-a.toml"))
	if err != nil {
		t.Fatalf("mol-a readlink: %v", err)
	}
	if dest != filepath.Join(layer2, "mol-a.formula.toml") {
		t.Errorf("mol-a target = %q, want layer2 legacy source", dest)
	}
}

func TestResolveFormulas_Idempotent(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.formula.toml", "formula a")

	target := filepath.Join(dir, "rig")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	// Run twice — should not error.
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("first ResolveFormulas: %v", err)
	}
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("second ResolveFormulas: %v", err)
	}

	// Symlink should still be correct (canonical link, legacy source).
	dest, err := os.Readlink(filepath.Join(target, ".beads", "formulas", "mol-a.toml"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if dest != filepath.Join(layer, "mol-a.formula.toml") {
		t.Errorf("symlink target = %q, want %q", dest, filepath.Join(layer, "mol-a.formula.toml"))
	}
}

func TestResolveFormulas_StaleCleanup(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.formula.toml", "formula a")
	writeFormulaFile(t, layer, "mol-b.formula.toml", "formula b")

	target := filepath.Join(dir, "rig")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	// First pass: both formulas.
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("first ResolveFormulas: %v", err)
	}

	// Remove mol-b from the layer.
	os.Remove(filepath.Join(layer, "mol-b.formula.toml")) //nolint:errcheck

	// Second pass: only mol-a.
	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("second ResolveFormulas: %v", err)
	}

	symlinkDir := filepath.Join(target, ".beads", "formulas")

	// mol-a should still exist under the canonical name.
	if _, err := os.Lstat(filepath.Join(symlinkDir, "mol-a.toml")); err != nil {
		t.Errorf("mol-a should still exist: %v", err)
	}

	// mol-b should be removed (stale).
	if _, err := os.Lstat(filepath.Join(symlinkDir, "mol-b.toml")); !os.IsNotExist(err) {
		t.Error("mol-b should have been removed (stale symlink)")
	}
}

// TestResolveFormulas_LegacySymlinkCleanup verifies that a legacy-named
// symlink left over from an earlier gc version (or partial migration) is
// garbage-collected on the next resolve pass.
func TestResolveFormulas_LegacySymlinkCleanup(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.toml", "formula a")

	target := filepath.Join(dir, "rig")
	symlinkDir := filepath.Join(target, ".beads", "formulas")
	os.MkdirAll(symlinkDir, 0o755) //nolint:errcheck

	// Simulate a stale legacy-named symlink from a prior run.
	stale := filepath.Join(symlinkDir, "mol-a.formula.toml")
	if err := os.Symlink(filepath.Join(layer, "mol-a.toml"), stale); err != nil {
		t.Fatalf("create stale symlink: %v", err)
	}

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale legacy symlink should have been removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(symlinkDir, "mol-a.toml")); err != nil {
		t.Errorf("canonical mol-a symlink should exist: %v", err)
	}
}

func TestResolveFormulas_RealFileNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.toml", "layer version")

	target := filepath.Join(dir, "rig")
	symlinkDir := filepath.Join(target, ".beads", "formulas")
	os.MkdirAll(symlinkDir, 0o755) //nolint:errcheck

	// Create a real file (not a symlink) at the canonical link location.
	realFile := filepath.Join(symlinkDir, "mol-a.toml")
	os.WriteFile(realFile, []byte("real file"), 0o644) //nolint:errcheck

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	// The real file should be preserved (not replaced with symlink).
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

func TestResolveFormulas_EmptyLayers(t *testing.T) {
	if err := ResolveFormulas("/tmp/nonexistent", nil); err != nil {
		t.Errorf("nil layers should be no-op: %v", err)
	}
	if err := ResolveFormulas("/tmp/nonexistent", []string{}); err != nil {
		t.Errorf("empty layers should be no-op: %v", err)
	}
}

func TestResolveFormulas_MissingLayerDir(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.formula.toml", "formula a")

	target := filepath.Join(dir, "rig")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	// Include a missing dir — should be skipped, not error.
	if err := ResolveFormulas(target, []string{"/nonexistent", layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	// mol-a from the existing layer should still be resolved, under the canonical name.
	if _, err := os.Lstat(filepath.Join(target, ".beads", "formulas", "mol-a.toml")); err != nil {
		t.Errorf("mol-a should exist: %v", err)
	}
}

func TestResolveFormulas_NonFormulaFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.formula.toml", "formula")
	writeFormulaFile(t, layer, "readme.md", "not a formula")
	// A directory sibling must also be ignored.
	if err := os.MkdirAll(filepath.Join(layer, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "rig")
	os.MkdirAll(target, 0o755) //nolint:errcheck

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	symlinkDir := filepath.Join(target, ".beads", "formulas")
	entries, err := os.ReadDir(symlinkDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1 (only TOML formula files)", len(entries))
	}
}
