package builtinpacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCommit = "abcdef123456abcdef123456abcdef123456abcd"

func TestAllAndSourceAreDeterministic(t *testing.T) {
	first := packIdentityList()
	second := packIdentityList()
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("All changed between calls:\nfirst: %v\nsecond: %v", first, second)
	}

	want := []string{
		"core=internal/bootstrap/packs/core",
		"bd=examples/bd",
		"dolt=examples/dolt",
		"maintenance=examples/gastown/packs/maintenance",
		"gastown=examples/gastown/packs/gastown",
	}
	if strings.Join(first, "\n") != strings.Join(want, "\n") {
		t.Fatalf("All = %v, want %v", first, want)
	}

	for _, pack := range All() {
		source, ok := Source(pack.Name)
		if !ok {
			t.Fatalf("Source(%q) ok = false, want true", pack.Name)
		}
		if source != Repository+"//"+pack.Subpath {
			t.Fatalf("Source(%q) = %q, want canonical source", pack.Name, source)
		}
	}
}

func TestSourceRecognitionVariants(t *testing.T) {
	coreSource := MustSource("core")
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{name: "canonical", src: coreSource, want: true},
		{name: "short github form", src: "github.com/gastownhall/gascity//internal/bootstrap/packs/core", want: true},
		{name: "without git suffix", src: "https://github.com/gastownhall/gascity//internal/bootstrap/packs/core", want: true},
		{name: "trailing slash", src: coreSource + "/", want: true},
		{name: "with ref", src: coreSource + "#main", want: true},
		{name: "different repo", src: "https://github.com/example/gascity.git//internal/bootstrap/packs/core", want: false},
		{name: "unknown subpath", src: Repository + "//internal/bootstrap/packs/missing", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSource(tc.src); got != tc.want {
				t.Fatalf("IsSource(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestSyntheticContentHashDeterministic(t *testing.T) {
	first, err := SyntheticContentHash()
	if err != nil {
		t.Fatalf("SyntheticContentHash first: %v", err)
	}
	second, err := SyntheticContentHash()
	if err != nil {
		t.Fatalf("SyntheticContentHash second: %v", err)
	}
	if first != second {
		t.Fatalf("SyntheticContentHash changed between calls: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("SyntheticContentHash = %q, want sha256 prefix", first)
	}
}

func TestMaterializeSyntheticRepoRoundTripReplacesDestination(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "cache")
	writeFile(t, filepath.Join(dst, "stale.txt"), "stale")

	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file stat err = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(dst, syntheticMarkerFile)); err != nil {
		t.Fatalf("marker stat: %v", err)
	}
	if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
		t.Fatalf("ValidateSyntheticRepo: %v", err)
	}
}

func TestMaterializeSyntheticRepoRejectsEmptyCommit(t *testing.T) {
	err := MaterializeSyntheticRepo(filepath.Join(t.TempDir(), "cache"), " \t\n")
	if err == nil {
		t.Fatal("MaterializeSyntheticRepo accepted empty commit")
	}
	if !strings.Contains(err.Error(), "commit is required") {
		t.Fatalf("error = %v, want commit-required detail", err)
	}
}

func TestValidateSyntheticRepoAcceptsEquivalentCommit(t *testing.T) {
	dst := materializeTestRepo(t)
	if err := ValidateSyntheticRepo(dst, "ABCDEF1"); err != nil {
		t.Fatalf("ValidateSyntheticRepo with abbreviated uppercase commit: %v", err)
	}
}

func TestValidateSyntheticRepoRejectsTamperedContent(t *testing.T) {
	dst := materializeTestRepo(t)
	writeFile(t, filepath.Join(dst, "internal/bootstrap/packs/core/pack.toml"), `
[pack]
name = "tampered"
schema = 1
`)

	err := ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted tampered content")
	}
	if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want content differs", err)
	}
}

func TestValidateSyntheticRepoRejectsUnexpectedFiles(t *testing.T) {
	dst := materializeTestRepo(t)
	writeFile(t, filepath.Join(dst, "internal/bootstrap/packs/core/agents/injected/prompt.md"), "malicious")

	err := ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted unexpected file")
	}
	if !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("error = %v, want unexpected file detail", err)
	}
}

func materializeTestRepo(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "cache")
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	return dst
}

func packIdentityList() []string {
	packs := All()
	ids := make([]string, 0, len(packs))
	for _, pack := range packs {
		ids = append(ids, pack.Name+"="+pack.Subpath)
	}
	return ids
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
