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

func TestExpandNVMSingleDigitMajorSortedNumerically(t *testing.T) {
	home := t.TempDir()
	// v8 is lexicographically greater than v22 ('8' > '2'), so a plain
	// reverse-lexicographic sort would incorrectly place v8 before v22.
	// Natural sort must place v22 first because it is a newer version.
	v8 := filepath.Join(home, ".nvm", "versions", "node", "v8.17.0", "bin")
	v22 := filepath.Join(home, ".nvm", "versions", "node", "v22.14.0", "bin")
	for _, d := range []string{v8, v22} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dirs := Expand(home, "linux", "/usr/bin")

	idx8 := slices.Index(dirs, v8)
	idx22 := slices.Index(dirs, v22)
	if idx8 == -1 || idx22 == -1 {
		t.Fatalf("expected both nvm versions in dirs: %v", dirs)
	}
	if idx22 > idx8 {
		t.Fatalf("v22 (%d) should appear before v8 (%d) in dirs: %v", idx22, idx8, dirs)
	}
}

func TestExpandFnmMixedMajorsSortedNumerically(t *testing.T) {
	home := t.TempDir()
	// Exercise fnm layout with three majors that would break under
	// lexicographic sort: v2 < v10 < v20 numerically, but lex puts v2
	// between v20 and v10.
	v2 := filepath.Join(home, ".fnm", "node-versions", "v2.5.0", "installation", "bin")
	v10 := filepath.Join(home, ".fnm", "node-versions", "v10.24.1", "installation", "bin")
	v20 := filepath.Join(home, ".fnm", "node-versions", "v20.11.1", "installation", "bin")
	for _, d := range []string{v2, v10, v20} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dirs := Expand(home, "linux", "/usr/bin")

	idx2 := slices.Index(dirs, v2)
	idx10 := slices.Index(dirs, v10)
	idx20 := slices.Index(dirs, v20)
	if idx2 == -1 || idx10 == -1 || idx20 == -1 {
		t.Fatalf("expected all three fnm versions in dirs: %v", dirs)
	}
	if idx20 >= idx10 || idx10 >= idx2 {
		t.Fatalf("expected v20 < v10 < v2 ordering, got v20=%d v10=%d v2=%d: %v", idx20, idx10, idx2, dirs)
	}
}

func TestExpandNVMPatchAndMinorSortedNumerically(t *testing.T) {
	home := t.TempDir()
	// Same major, mixed minor/patch widths: v20.9.0 vs v20.11.0 — lex
	// sort would place v20.9.0 before v20.11.0 because '9' > '1'.
	v20_9 := filepath.Join(home, ".nvm", "versions", "node", "v20.9.0", "bin")
	v20_11 := filepath.Join(home, ".nvm", "versions", "node", "v20.11.0", "bin")
	for _, d := range []string{v20_9, v20_11} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dirs := Expand(home, "linux", "/usr/bin")

	idx9 := slices.Index(dirs, v20_9)
	idx11 := slices.Index(dirs, v20_11)
	if idx9 == -1 || idx11 == -1 {
		t.Fatalf("expected both versions in dirs: %v", dirs)
	}
	if idx11 > idx9 {
		t.Fatalf("v20.11.0 (%d) should appear before v20.9.0 (%d): %v", idx11, idx9, dirs)
	}
}

func TestCompareNatural(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{name: "equal strings", a: "foo", b: "foo", want: 0},
		{name: "pure lex less", a: "abc", b: "abd", want: -1},
		{name: "pure lex greater", a: "abd", b: "abc", want: 1},
		{name: "single digit vs two digit", a: "v8", b: "v22", want: -1},
		{name: "two digit vs single digit", a: "v22", b: "v8", want: 1},
		{name: "same width numeric", a: "v18", b: "v22", want: -1},
		{name: "multi-segment version less", a: "v20.9.0", b: "v20.11.0", want: -1},
		{name: "multi-segment version greater", a: "v20.11.0", b: "v20.9.0", want: 1},
		{name: "three majors", a: "v2.5.0", b: "v10.0.0", want: -1},
		{name: "leading zeros equal value", a: "v007", b: "v7", want: 1},
		{name: "numeric prefix equal suffix differs", a: "node-22-lts", b: "node-22-alpha", want: 1},
		{name: "empty a", a: "", b: "foo", want: -1},
		{name: "empty b", a: "foo", b: "", want: 1},
		{name: "both empty", a: "", b: "", want: 0},
		{name: "prefix is less", a: "v22", b: "v22.1", want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareNatural(tt.a, tt.b); got != tt.want {
				t.Fatalf("compareNatural(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
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
