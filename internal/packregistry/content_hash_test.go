package packregistry

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackContentHashUsesTrackedPackManifest(t *testing.T) {
	repo := initContentHashRepo(t)
	writeContentHashFile(t, repo, "packs/demo/pack.toml", "[pack]\nname = \"demo\"\nschema = 2\n", 0o644)
	writeContentHashFile(t, repo, "packs/demo/commands/run.sh", "#!/bin/sh\nexit 0\n", 0o755)
	writeContentHashFile(t, repo, "packs/demo/untracked.txt", "ignored\n", 0o644)
	runContentHashGit(t, repo, "add", "packs/demo/pack.toml", "packs/demo/commands/run.sh")
	runContentHashGit(t, repo, "commit", "-m", "add demo pack")
	commit := strings.TrimSpace(outputContentHashGit(t, repo, "rev-parse", "HEAD"))

	got, err := PackContentHash(repo, commit, "packs/demo")
	if err != nil {
		t.Fatalf("PackContentHash: %v", err)
	}
	want := manifestHash(
		"commands/run.sh 0755 "+blobHash("#!/bin/sh\nexit 0\n"),
		"pack.toml 0644 "+blobHash("[pack]\nname = \"demo\"\nschema = 2\n"),
	)
	if got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}

	writeContentHashFile(t, repo, "packs/demo/untracked.txt", "changed but ignored\n", 0o644)
	again, err := PackContentHash(repo, commit, "packs/demo")
	if err != nil {
		t.Fatalf("PackContentHash after untracked change: %v", err)
	}
	if again != want {
		t.Fatalf("hash changed after untracked edit: %q != %q", again, want)
	}
}

func TestPackContentHashRequiresPackToml(t *testing.T) {
	repo := initContentHashRepo(t)
	writeContentHashFile(t, repo, "packs/not-pack/readme.md", "hello\n", 0o644)
	runContentHashGit(t, repo, "add", ".")
	runContentHashGit(t, repo, "commit", "-m", "add non-pack")
	commit := strings.TrimSpace(outputContentHashGit(t, repo, "rev-parse", "HEAD"))

	_, err := PackContentHash(repo, commit, "packs/not-pack")
	if err == nil || !strings.Contains(err.Error(), "pack.toml") {
		t.Fatalf("PackContentHash err = %v, want missing pack.toml", err)
	}
}

func initContentHashRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runContentHashGit(t, repo, "init")
	runContentHashGit(t, repo, "config", "user.email", "test@example.com")
	runContentHashGit(t, repo, "config", "user.name", "Test User")
	return repo
}

func writeContentHashFile(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(%s): %v", rel, err)
	}
}

func runContentHashGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = outputContentHashGit(t, dir, args...)
}

func outputContentHashGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
	return string(out)
}

func blobHash(data string) string {
	sum := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", sum[:])
}

func manifestHash(entries ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("sha256:%x", sum[:])
}
