package searchpath

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExpandIncludesUserLocalAndBasePath(t *testing.T) {
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := Expand(home, "linux", "/usr/bin:/bin")
	if !slices.Contains(dirs, localBin) {
		t.Fatalf("expected %q in dirs: %v", localBin, dirs)
	}
	if !slices.Contains(dirs, "/usr/bin") {
		t.Fatalf("expected /usr/bin in dirs: %v", dirs)
	}
}

func TestExpandEmptyHomeDir(t *testing.T) {
	dirs := Expand("", "linux", "/usr/bin")
	if !slices.Contains(dirs, "/usr/bin") {
		t.Fatalf("expected /usr/bin in dirs: %v", dirs)
	}
	// Should not panic or produce home-relative paths.
	for _, d := range dirs {
		if strings.Contains(d, "//") {
			t.Fatalf("malformed path with empty home: %q", d)
		}
	}
}

func TestExpandEmptyBasePath(t *testing.T) {
	home := t.TempDir()
	dirs := Expand(home, "linux", "")
	// Should produce at least the home-relative dirs that exist.
	if len(dirs) == 0 {
		t.Fatal("expected non-empty dirs even with empty basePath")
	}
}

func TestExpandWhitespaceOnlyInputs(t *testing.T) {
	dirs := Expand("  ", "linux", "  ")
	// Whitespace-only home and basePath treated as empty.
	for _, d := range dirs {
		if strings.TrimSpace(d) == "" {
			t.Fatalf("empty dir in output: %v", dirs)
		}
	}
}

func TestExpandDarwinIncludesHomebrew(t *testing.T) {
	dirs := Expand("", "darwin", "/usr/bin")
	if !slices.Contains(dirs, "/opt/homebrew/bin") {
		t.Fatalf("expected /opt/homebrew/bin for darwin: %v", dirs)
	}
}

func TestExpandLinuxIncludesSnap(t *testing.T) {
	dirs := Expand("", "linux", "/usr/bin")
	if !slices.Contains(dirs, "/snap/bin") {
		t.Fatalf("expected /snap/bin for linux: %v", dirs)
	}
}

func TestExpandUnknownGOOS(t *testing.T) {
	dirs := Expand("", "freebsd", "/usr/bin")
	// Should not include darwin or linux specific dirs.
	for _, d := range dirs {
		if d == "/opt/homebrew/bin" || d == "/snap/bin" {
			t.Fatalf("unexpected OS-specific dir %q for freebsd", d)
		}
	}
}

func TestExpandPathJoinsWithSeparator(t *testing.T) {
	home := t.TempDir()
	result := ExpandPath(home, "linux", "/usr/bin")
	parts := filepath.SplitList(result)
	if len(parts) < 2 {
		t.Fatalf("expected multiple path entries, got: %q", result)
	}
}

func TestDedupeRemovesDuplicates(t *testing.T) {
	input := []string{"/usr/bin", "/bin", "/usr/bin", "/bin"}
	got := Dedupe(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(got), got)
	}
	if got[0] != "/usr/bin" || got[1] != "/bin" {
		t.Fatalf("unexpected order: %v", got)
	}
}

func TestDedupeRemovesEmptyAndWhitespace(t *testing.T) {
	input := []string{"", "  ", "/usr/bin", " ", "/bin"}
	got := Dedupe(input)
	for _, d := range got {
		if strings.TrimSpace(d) == "" {
			t.Fatalf("empty entry in output: %v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(got), got)
	}
}

func TestDedupePreservesOrder(t *testing.T) {
	input := []string{"/c", "/a", "/b", "/a"}
	got := Dedupe(input)
	want := []string{"/c", "/a", "/b"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExpandNVMVersionsReverseSorted(t *testing.T) {
	home := t.TempDir()
	// Create two nvm version dirs.
	v18 := filepath.Join(home, ".nvm", "versions", "node", "v18.20.0", "bin")
	v22 := filepath.Join(home, ".nvm", "versions", "node", "v22.14.0", "bin")
	for _, d := range []string{v18, v22} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dirs := Expand(home, "linux", "/usr/bin")

	idx18 := slices.Index(dirs, v18)
	idx22 := slices.Index(dirs, v22)
	if idx18 == -1 || idx22 == -1 {
		t.Fatalf("expected both nvm versions in dirs: %v", dirs)
	}
	if idx22 > idx18 {
		t.Fatalf("v22 (%d) should appear before v18 (%d) in dirs: %v", idx22, idx18, dirs)
	}
}

func TestExpandCurrentSymlinkBeforeGlobVersions(t *testing.T) {
	home := t.TempDir()
	// Create both a "current" symlink dir and a versioned dir.
	currentBin := filepath.Join(home, ".nvm", "current", "bin")
	v22 := filepath.Join(home, ".nvm", "versions", "node", "v22.14.0", "bin")
	for _, d := range []string{currentBin, v22} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dirs := Expand(home, "linux", "/usr/bin")

	idxCurrent := slices.Index(dirs, currentBin)
	idxV22 := slices.Index(dirs, v22)
	if idxCurrent == -1 || idxV22 == -1 {
		t.Fatalf("expected both current and v22 in dirs: %v", dirs)
	}
	if idxCurrent > idxV22 {
		t.Fatalf("current (%d) should appear before v22 (%d) in dirs: %v", idxCurrent, idxV22, dirs)
	}
}

func TestExpandUserManagedDirsOnlyExisting(t *testing.T) {
	home := t.TempDir()
	// Create only cargo bin, not go bin.
	cargoBin := filepath.Join(home, ".cargo", "bin")
	if err := os.MkdirAll(cargoBin, 0o755); err != nil {
		t.Fatal(err)
	}
	goBin := filepath.Join(home, "go", "bin")

	dirs := Expand(home, "linux", "/usr/bin")
	if !slices.Contains(dirs, cargoBin) {
		t.Fatalf("expected %q in dirs: %v", cargoBin, dirs)
	}
	if slices.Contains(dirs, goBin) {
		t.Fatalf("did not expect non-existent %q in dirs: %v", goBin, dirs)
	}
}
