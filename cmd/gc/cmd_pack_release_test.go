package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/packregistry"
)

func TestPackReleaseHashCommandPrintsContentHash(t *testing.T) {
	repo, commit := initPackReleaseRepo(t)

	var stdout, stderr bytes.Buffer
	code := doPackReleaseHash(repo, "packs/demo", commit, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("hash code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want, err := packregistry.PackContentHash(repo, commit, "packs/demo")
	if err != nil {
		t.Fatalf("PackContentHash: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("hash stdout = %q, want %q", got, want)
	}
}

func TestPackReleaseHashRemoteRootWithExplicitPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo, commit := initPackReleaseRepo(t)
	source := "file://" + repo

	var stdout, stderr bytes.Buffer
	code := doPackReleaseHash(source, "packs/demo", commit, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("hash code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want, err := packregistry.PackContentHash(repo, commit, "packs/demo")
	if err != nil {
		t.Fatalf("PackContentHash: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("hash stdout = %q, want %q", got, want)
	}
}

func TestPackReleaseStampCreatesAndValidatesRegistryRelease(t *testing.T) {
	repo, commit := initPackReleaseRepo(t)
	registryPath := filepath.Join(t.TempDir(), "registry.toml")
	if err := os.WriteFile(registryPath, []byte("schema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(registry): %v", err)
	}

	var stdout, stderr bytes.Buffer
	opts := packReleaseStampOptions{
		Version:         "0.1.0",
		Ref:             "main",
		Commit:          commit,
		ReleaseDesc:     "Initial demo pack release.",
		Source:          repo,
		PackPath:        "packs/demo",
		PackDescription: "Demo pack.",
	}
	if code := doPackReleaseStamp(registryPath, "demo", opts, &stdout, &stderr); code != 0 {
		t.Fatalf("stamp code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "stamped demo 0.1.0") {
		t.Fatalf("stamp stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackReleaseValidate(registryPath, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("validate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "registry release hashes ok") {
		t.Fatalf("validate stdout = %q", stdout.String())
	}
}

func TestPackReleaseValidateRejectsHashMismatch(t *testing.T) {
	repo, commit := initPackReleaseRepo(t)
	registryPath := filepath.Join(t.TempDir(), "registry.toml")
	source := filepath.Join(repo, "packs", "demo")
	text := `schema = 1

[[pack]]
name = "demo"
description = "Demo pack."
source = "` + source + `"
source_kind = "git"
`
	text += `
  [[pack.release]]
  version = "0.1.0"
  ref = "main"
  commit = "` + commit + `"
  hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  description = "Initial demo pack release."
`
	if err := os.WriteFile(registryPath, []byte(text), 0o644); err != nil {
		t.Fatalf("WriteFile(registry): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doPackReleaseValidate(registryPath, "", false, &stdout, &stderr); code == 0 {
		t.Fatalf("validate succeeded with bad hash stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "hash mismatch") {
		t.Fatalf("validate stderr = %q, want hash mismatch", stderr.String())
	}
}

func TestPackReleaseValidateSkipsWithdrawnByDefault(t *testing.T) {
	repo, commit := initPackReleaseRepo(t)
	registryPath := filepath.Join(t.TempDir(), "registry.toml")
	source := filepath.Join(repo, "packs", "demo")
	text := `schema = 1

[[pack]]
name = "demo"
description = "Demo pack."
source = "` + source + `"
source_kind = "git"

  [[pack.release]]
  version = "0.1.0"
  ref = "main"
  commit = "` + commit + `"
  hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  description = "Initial demo pack release."
  withdrawn = true
  withdrawn_reason = "bad hash"
`
	if err := os.WriteFile(registryPath, []byte(text), 0o644); err != nil {
		t.Fatalf("WriteFile(registry): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doPackReleaseValidate(registryPath, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("validate active code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "(0 checked)") {
		t.Fatalf("validate stdout = %q, want 0 checked", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackReleaseValidate(registryPath, "", true, &stdout, &stderr); code == 0 {
		t.Fatalf("validate include withdrawn succeeded stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "hash mismatch") {
		t.Fatalf("validate include withdrawn stderr = %q, want hash mismatch", stderr.String())
	}
}

func initPackReleaseRepo(t *testing.T) (repo string, commit string) {
	t.Helper()
	repo = t.TempDir()
	runPackReleaseGit(t, repo, "init")
	runPackReleaseGit(t, repo, "config", "user.email", "test@example.com")
	runPackReleaseGit(t, repo, "config", "user.name", "Test User")
	writePackReleaseFile(t, repo, "packs/demo/pack.toml", "[pack]\nname = \"demo\"\nschema = 2\n", 0o644)
	writePackReleaseFile(t, repo, "packs/demo/commands/run.sh", "#!/bin/sh\nexit 0\n", 0o755)
	runPackReleaseGit(t, repo, "add", ".")
	runPackReleaseGit(t, repo, "commit", "-m", "add demo pack")
	commit = strings.TrimSpace(outputPackReleaseGit(t, repo, "rev-parse", "HEAD"))
	return repo, commit
}

func writePackReleaseFile(t *testing.T, root, rel, body string, mode os.FileMode) {
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

func runPackReleaseGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = outputPackReleaseGit(t, dir, args...)
}

func outputPackReleaseGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
	return string(out)
}
