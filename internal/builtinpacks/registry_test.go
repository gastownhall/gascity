package builtinpacks

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
		{name: "github tree form", src: "https://github.com/gastownhall/gascity/tree/main/internal/bootstrap/packs/core", want: true},
		{name: "github blob form", src: "https://github.com/gastownhall/gascity/blob/main/internal/bootstrap/packs/core/pack.toml", want: true},
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

func TestMaterializeSyntheticRepoRejectsUnsafeDestination(t *testing.T) {
	for _, dst := range []string{"", string(filepath.Separator)} {
		t.Run(dst, func(t *testing.T) {
			err := MaterializeSyntheticRepo(dst, testCommit)
			if err == nil {
				t.Fatalf("MaterializeSyntheticRepo(%q) succeeded, want unsafe-path error", dst)
			}
			if !strings.Contains(err.Error(), "refusing to materialize") {
				t.Fatalf("error = %v, want refusing-to-materialize detail", err)
			}
		})
	}
}

func TestMaterializeSyntheticRepoProductionCallersStayInPackman(t *testing.T) {
	repoRoot := testRepoRoot(t)
	allowed := map[string]bool{
		"internal/packman/cache.go": true,
	}
	var offenders []string
	if err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".gc", "node_modules", "worktrees":
				return filepath.SkipDir
			}
			// Skip git worktrees embedded in the repo (have a .git file, not dir).
			if fi, serr := os.Stat(filepath.Join(path, ".git")); serr == nil && !fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "internal/builtinpacks/registry.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("MaterializeSyntheticRepo")) && !allowed[rel] {
			offenders = append(offenders, rel)
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir(%q): %v", repoRoot, err)
	}
	if len(offenders) > 0 {
		t.Fatalf("MaterializeSyntheticRepo production callers = %v, want only internal/packman/cache.go", offenders)
	}
}

func TestValidateSyntheticRepoAcceptsEquivalentCommit(t *testing.T) {
	dst := materializeTestRepo(t)
	if err := ValidateSyntheticRepo(dst, "ABCDEF1"); err != nil {
		t.Fatalf("ValidateSyntheticRepo with abbreviated uppercase commit: %v", err)
	}
}

func TestMaterializedFileModeTreatsScriptExtensionsAsExecutable(t *testing.T) {
	for _, path := range []string{"run.sh", "tool.py", "script.bash"} {
		t.Run(path, func(t *testing.T) {
			if got := MaterializedFileMode(path); got != 0o755 {
				t.Fatalf("MaterializedFileMode(%q) = %04o, want 0755", path, got)
			}
		})
	}
	if got := MaterializedFileMode("pack.toml"); got != 0o644 {
		t.Fatalf("MaterializedFileMode(pack.toml) = %04o, want 0644", got)
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

func TestValidateSyntheticRepoRejectsTamperedMode(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, "internal/bootstrap/packs/core/pack.toml")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("Chmod(%q): %v", target, err)
	}

	err := ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted tampered file mode")
	}
	if !strings.Contains(err.Error(), "has mode") {
		t.Fatalf("error = %v, want mode mismatch", err)
	}
}

func TestValidateSyntheticRepoRejectsSymlinkAncestor(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", target, err)
	}
	if err := os.RemoveAll(filepath.Join(dst, "internal")); err != nil {
		t.Fatalf("RemoveAll(internal): %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dst, "internal")); err != nil {
		t.Fatalf("Symlink(internal): %v", err)
	}

	err := ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted symlink ancestor")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want symlink detail", err)
	}
}

func TestValidateSyntheticRepoRejectsSymlinkRoot(t *testing.T) {
	dst := materializeTestRepo(t)
	link := filepath.Join(t.TempDir(), "cache-link")
	if err := os.Symlink(dst, link); err != nil {
		t.Fatalf("Symlink(cache-link): %v", err)
	}

	err := ValidateSyntheticRepo(link, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted symlink root")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want symlink detail", err)
	}
}

func TestValidateSyntheticRepoRejectsNonDirectoryRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	writeFile(t, root, "not a directory")

	err := ValidateSyntheticRepo(root, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted non-directory root")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %v, want not-a-directory detail", err)
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

func TestValidateSyntheticRepoRejectsUnexpectedRootSibling(t *testing.T) {
	dst := materializeTestRepo(t)
	writeFile(t, filepath.Join(dst, "scratch.txt"), "malicious")

	err := ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted unexpected root sibling")
	}
	if !strings.Contains(err.Error(), "unexpected file scratch.txt") {
		t.Fatalf("error = %v, want unexpected root path", err)
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

func testRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %q missing go.mod: %v", root, err)
	}
	return root
}

func TestSyntheticCacheKeyComponentMatchesContentHash(t *testing.T) {
	want, err := SyntheticContentHash()
	if err != nil {
		t.Fatalf("SyntheticContentHash: %v", err)
	}
	got := SyntheticCacheKeyComponent()
	if got == "" {
		t.Fatal("SyntheticCacheKeyComponent returned empty for a valid binary")
	}
	if got != want {
		t.Fatalf("SyntheticCacheKeyComponent = %q, want content hash %q", got, want)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("SyntheticCacheKeyComponent = %q, want sha256 prefix", got)
	}
	if second := SyntheticCacheKeyComponent(); second != got {
		t.Fatalf("SyntheticCacheKeyComponent not stable across calls: %q != %q", got, second)
	}
}
