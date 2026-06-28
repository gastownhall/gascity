package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the symlink-resolution behavior added to
// normalizeDiscoveryPath and its effect on findCity's ceiling comparison. They
// reproduce on Linux the macOS failure mode where os.Getwd()/t.Chdir yields
// /tmp/... while the same directory resolves to /private/tmp/..., which
// silently defeated GC_CEILING_DIRECTORIES before discovery paths were run
// through filepath.EvalSymlinks. Linux CI never exercised the symlink/fallback
// branches otherwise, so the fix shipped without executable coverage.

func TestNormalizeDiscoveryPathResolvesExistingSymlink(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Clean(resolved)

	if got := normalizeDiscoveryPath(link); got != want {
		t.Errorf("normalizeDiscoveryPath(%q) = %q, want resolved real path %q", link, got, want)
	}
	// The symlink and the real directory must normalize identically, or ceiling
	// comparisons would not be symmetric.
	if got, viaReal := normalizeDiscoveryPath(link), normalizeDiscoveryPath(realDir); got != viaReal {
		t.Errorf("symlink and real path normalize differently: %q vs %q", got, viaReal)
	}
}

func TestNormalizeDiscoveryPathFallsBackToLongestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	// A configured-but-not-yet-created ceiling under the symlink. The existing
	// ancestor (link -> realDir) must resolve while the missing remainder is
	// re-appended verbatim, instead of the whole path dropping out of the
	// comparison because EvalSymlinks failed on the leaf.
	missing := filepath.Join(link, "not", "yet", "created")

	resolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Clean(resolved), "not", "yet", "created")

	if got := normalizeDiscoveryPath(missing); got != want {
		t.Errorf("normalizeDiscoveryPath(%q) = %q, want %q", missing, got, want)
	}
	// The same not-yet-created path expressed through the real directory must
	// normalize identically, keeping discovery comparisons symmetric.
	viaReal := normalizeDiscoveryPath(filepath.Join(realDir, "not", "yet", "created"))
	if got := normalizeDiscoveryPath(missing); got != viaReal {
		t.Errorf("symlinked vs real missing path normalize differently: %q vs %q", got, viaReal)
	}
}

func TestFindCitySymlinkedCeilingBoundsDiscovery(t *testing.T) {
	root := t.TempDir()
	realCeiling := filepath.Join(root, "ceiling")
	if err := os.MkdirAll(realCeiling, 0o755); err != nil {
		t.Fatal(err)
	}
	linkCeiling := filepath.Join(root, "ceiling-link")
	if err := os.Symlink(realCeiling, linkCeiling); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	// A stray city above the ceiling that discovery must NOT escape up to.
	if err := os.WriteFile(filepath.Join(root, "city.toml"), []byte("[workspace]\nname = \"stray\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := filepath.Join(realCeiling, "child", "deep")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}

	// Configure the ceiling via its symlinked form while discovery walks the
	// real form. Before discovery paths were symlink-resolved, the raw strings
	// differed, the ceiling never fired, and findCity escaped to the stray city
	// above. With resolution both sides collapse to the same real path and the
	// ceiling bounds the walk.
	t.Setenv("GC_CEILING_DIRECTORIES", linkCeiling)

	_, err := findCity(start)
	if err == nil {
		t.Fatal("findCity() escaped a symlinked ceiling and found the stray city above")
	}
	if !strings.Contains(err.Error(), "not in a city directory") {
		t.Errorf("error = %q, want 'not in a city directory'", err)
	}
}
