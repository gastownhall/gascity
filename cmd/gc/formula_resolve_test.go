package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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

func assertFormulaEntryMatchesSource(t *testing.T, entryPath, sourcePath string) {
	t.Helper()
	fi, err := os.Lstat(entryPath)
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(entryPath), err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		dest, err := os.Readlink(entryPath)
		if err != nil {
			t.Fatalf("%s: readlink: %v", filepath.Base(entryPath), err)
		}
		if dest != sourcePath {
			t.Fatalf("%s: link target = %q, want %q", filepath.Base(entryPath), dest, sourcePath)
		}
		return
	}
	if runtime.GOOS != "windows" {
		t.Fatalf("%s: not a symlink", filepath.Base(entryPath))
	}
	got, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("%s: read copy: %v", filepath.Base(entryPath), err)
	}
	want, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("%s: read source: %v", filepath.Base(entryPath), err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: copy content = %q, want %q", filepath.Base(entryPath), got, want)
	}
}

func createSymlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatalf("create symlink: %v", err)
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
		{"mol-a.formula.toml", "mol-a.toml"},
		{"mol-b.toml", "mol-b.formula.toml"},
		{"mol-b.formula.toml", "mol-b.formula.toml"},
	}
	for _, c := range cases {
		linkPath := filepath.Join(symlinkDir, c.linkName)
		want := filepath.Join(layer, c.srcName)
		assertFormulaEntryMatchesSource(t, linkPath, want)
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
	assertFormulaEntryMatchesSource(t, filepath.Join(symlinkDir, "mol-a.toml"), filepath.Join(layer2, "mol-a.formula.toml"))

	// mol-b should point to layer1 (only source).
	assertFormulaEntryMatchesSource(t, filepath.Join(symlinkDir, "mol-b.toml"), filepath.Join(layer1, "mol-b.formula.toml"))

	// mol-c should point to layer2 (only source).
	assertFormulaEntryMatchesSource(t, filepath.Join(symlinkDir, "mol-c.toml"), filepath.Join(layer2, "mol-c.formula.toml"))
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
	for _, linkName := range []string{"mol-a.toml", "mol-a.formula.toml"} {
		assertFormulaEntryMatchesSource(t, filepath.Join(symlinkDir, linkName), filepath.Join(layer, "mol-a.toml"))
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

	for _, linkName := range []string{"mol-a.toml", "mol-a.formula.toml"} {
		assertFormulaEntryMatchesSource(t, filepath.Join(target, ".beads", "formulas", linkName), filepath.Join(layer2, "mol-a.formula.toml"))
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

	for _, linkName := range []string{"mol-a.toml", "mol-a.formula.toml"} {
		assertFormulaEntryMatchesSource(t, filepath.Join(target, ".beads", "formulas", linkName), filepath.Join(layer, "mol-a.formula.toml"))
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

	for _, linkName := range []string{"mol-a.toml", "mol-a.formula.toml"} {
		if _, err := os.Lstat(filepath.Join(symlinkDir, linkName)); err != nil {
			t.Errorf("%s should still exist: %v", linkName, err)
		}
	}

	for _, linkName := range []string{"mol-b.toml", "mol-b.formula.toml"} {
		if _, err := os.Lstat(filepath.Join(symlinkDir, linkName)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed (stale symlink)", linkName)
		}
	}
}

// TestResolveFormulas_LegacySymlinkCompatibility verifies that the legacy
// compatibility alias is created and refreshed alongside the canonical link.
func TestResolveFormulas_LegacySymlinkCompatibility(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	writeFormulaFile(t, layer, "mol-a.toml", "formula a")

	target := filepath.Join(dir, "rig")
	symlinkDir := filepath.Join(target, ".beads", "formulas")
	os.MkdirAll(symlinkDir, 0o755) //nolint:errcheck

	// Simulate a stale legacy compatibility link from a prior run.
	stale := filepath.Join(symlinkDir, "mol-a.formula.toml")
	createSymlinkOrSkip(t, filepath.Join(layer, "mol-a.toml"), stale)

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatalf("ResolveFormulas: %v", err)
	}

	for _, linkName := range []string{"mol-a.toml", "mol-a.formula.toml"} {
		assertFormulaEntryMatchesSource(t, filepath.Join(symlinkDir, linkName), filepath.Join(layer, "mol-a.toml"))
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

	// The real file should be preserved (not replaced with symlink), while the
	// legacy compatibility alias is still created.
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
	if _, err := os.Lstat(filepath.Join(symlinkDir, "mol-a.formula.toml")); err != nil {
		t.Errorf("legacy compatibility alias should still exist: %v", err)
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

func TestResolveFormulas_NoWinnersCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "formulas")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "city")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ResolveFormulas(target, []string{layer}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("zero-winner resolve created .beads: %v", err)
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

	for _, linkName := range []string{"mol-a.toml", "mol-a.formula.toml"} {
		if _, err := os.Lstat(filepath.Join(target, ".beads", "formulas", linkName)); err != nil {
			t.Errorf("%s should exist: %v", linkName, err)
		}
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
	entryCount := 0
	for _, entry := range entries {
		if runtime.GOOS == "windows" && (entry.Name() == ".gc-managed-formulas.json" || entry.Name() == ".gc-managed-formulas.lock") {
			continue
		}
		entryCount++
	}
	if entryCount != 2 {
		t.Errorf("got %d entries, want 2 (canonical + legacy compatibility entries)", entryCount)
	}
}
