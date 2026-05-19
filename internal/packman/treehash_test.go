package packman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHashPackTreeDigestChangesWhenFileBytesChange(t *testing.T) {
	root := t.TempDir()
	writeTreeHashTestFile(t, root, "pack.toml", "name = \"demo\"\n")

	first := hashPackTreeForTest(t, root)
	writeTreeHashTestFile(t, root, "pack.toml", "name = \"changed\"\n")
	second := hashPackTreeForTest(t, root)

	if first == second {
		t.Fatalf("HashPackTree digest did not change after file bytes changed: %s", first)
	}
}

func TestHashPackTreeLexicalOrderingStableIndependentOfCreationOrder(t *testing.T) {
	firstRoot := t.TempDir()
	writeTreeHashTestFile(t, firstRoot, "b.txt", "bravo\n")
	writeTreeHashTestFile(t, firstRoot, "a.txt", "alpha\n")
	writeTreeHashTestFile(t, firstRoot, "nested/z.txt", "zulu\n")
	writeTreeHashTestFile(t, firstRoot, "nested/c.txt", "charlie\n")

	secondRoot := t.TempDir()
	writeTreeHashTestFile(t, secondRoot, "nested/c.txt", "charlie\n")
	writeTreeHashTestFile(t, secondRoot, "a.txt", "alpha\n")
	writeTreeHashTestFile(t, secondRoot, "nested/z.txt", "zulu\n")
	writeTreeHashTestFile(t, secondRoot, "b.txt", "bravo\n")

	first := hashPackTreeForTest(t, firstRoot)
	second := hashPackTreeForTest(t, secondRoot)
	if first != second {
		t.Fatalf("HashPackTree differs by creation order: %q != %q", first, second)
	}
}

func TestHashPackTreeExecutableBitAffectsDigest(t *testing.T) {
	root := t.TempDir()
	path := writeTreeHashTestFile(t, root, "bin/tool", "#!/bin/sh\n")

	plain := hashPackTreeForTest(t, root)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	executable := hashPackTreeForTest(t, root)

	if plain == executable {
		t.Fatalf("HashPackTree digest did not change after executable bit changed: %s", plain)
	}
}

func TestHashPackTreeSymlinkTargetAffectsDigest(t *testing.T) {
	root := t.TempDir()
	writeTreeHashTestFile(t, root, "a.txt", "same\n")
	writeTreeHashTestFile(t, root, "b.txt", "same\n")
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	first := hashPackTreeForTest(t, root)
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatalf("Remove(link): %v", err)
	}
	if err := os.Symlink("b.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	second := hashPackTreeForTest(t, root)

	if first == second {
		t.Fatalf("HashPackTree digest did not change after symlink target changed: %s", first)
	}
}

func TestHashPackTreeIgnoresLocalMetadata(t *testing.T) {
	cleanRoot := t.TempDir()
	writeTreeHashTestFile(t, cleanRoot, "pack.toml", "name = \"demo\"\n")
	writeTreeHashTestFile(t, cleanRoot, "src/main.gc", "body\n")

	noisyRoot := t.TempDir()
	writeTreeHashTestFile(t, noisyRoot, "pack.toml", "name = \"demo\"\n")
	writeTreeHashTestFile(t, noisyRoot, "src/main.gc", "body\n")
	writeTreeHashTestFile(t, noisyRoot, ".DS_Store", "root metadata\n")
	writeTreeHashTestFile(t, noisyRoot, "src/.DS_Store", "nested metadata\n")
	writeTreeHashTestFile(t, noisyRoot, ".git/config", "ignored git metadata\n")
	writeTreeHashTestFile(t, noisyRoot, ".gc/cache/state", "ignored gc state\n")
	writeTreeHashTestFile(t, noisyRoot, "runtime/cache/result", "ignored runtime cache\n")

	clean := hashPackTreeForTest(t, cleanRoot)
	noisy := hashPackTreeForTest(t, noisyRoot)
	if clean != noisy {
		t.Fatalf("HashPackTree included ignored metadata: %q != %q", clean, noisy)
	}
}

func TestHashPackTreeRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := writeTreeHashTestFile(t, t.TempDir(), "outside.txt", "outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := HashPackTree(root)
	if err == nil {
		t.Fatal("HashPackTree succeeded for symlink escaping pack root")
	}
	if !strings.Contains(err.Error(), "symlink escape") {
		t.Fatalf("HashPackTree error = %v, want symlink escape", err)
	}
}

func TestHashPackTreeRejectsNestedGitFile(t *testing.T) {
	root := t.TempDir()
	writeTreeHashTestFile(t, root, "nested/.git", "gitdir: ../.git/modules/nested\n")
	writeTreeHashTestFile(t, root, "nested/pack.toml", "name = \"nested\"\n")

	_, err := HashPackTree(root)
	if err == nil {
		t.Fatal("HashPackTree succeeded for nested .git file")
	}
	if !strings.Contains(err.Error(), "nested git metadata file") {
		t.Fatalf("HashPackTree error = %v, want nested git metadata rejection", err)
	}
}

func hashPackTreeForTest(t *testing.T, root string) string {
	t.Helper()
	got, err := HashPackTree(root)
	if err != nil {
		t.Fatalf("HashPackTree(%q): %v", root, err)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("HashPackTree(%q) = %q, want sha256: prefix", root, got)
	}
	return got
}

func writeTreeHashTestFile(t *testing.T, root, rel, contents string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}
