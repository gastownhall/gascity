package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildImageContextOnly(t *testing.T) {
	// Create a minimal city directory.
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "prompts", "mayor.md"), []byte("You are the mayor."), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doBuildImage(
		[]string{cityDir},
		"", // no tag needed for context-only
		"gc-agent:latest",
		nil,   // no rig paths
		false, // no push
		true,  // context-only
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("doBuildImage returned %d; stderr: %s", code, stderr.String())
	}

	// Output should mention the build context directory.
	if !strings.Contains(stdout.String(), "Build context written to:") {
		t.Errorf("stdout = %q, want 'Build context written to:'", stdout.String())
	}

	// Extract the output directory from stdout.
	line := strings.TrimSpace(stdout.String())
	parts := strings.SplitN(line, ": ", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected stdout format: %q", line)
	}
	outputDir := parts[1]

	// Verify Dockerfile and workspace exist.
	if _, err := os.Stat(filepath.Join(outputDir, "Dockerfile")); err != nil {
		t.Errorf("missing Dockerfile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "workspace", "city.toml")); err != nil {
		t.Errorf("missing workspace/city.toml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "workspace", "prompts", "mayor.md")); err != nil {
		t.Errorf("missing workspace/prompts/mayor.md: %v", err)
	}

	// Clean up — context-only doesn't auto-cleanup.
	_ = os.RemoveAll(outputDir)
}

func TestBuildImageRequiresTag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doBuildImage(
		[]string{t.TempDir()},
		"", // no tag
		"gc-agent:latest",
		nil,
		false,
		false, // not context-only, so tag is required
		&stdout, &stderr,
	)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "--tag is required") {
		t.Errorf("stderr = %q, want containing --tag is required", stderr.String())
	}
}

func TestBuildImageInvalidRigPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doBuildImage(
		[]string{t.TempDir()},
		"test:latest",
		"gc-agent:latest",
		[]string{"bad-format"}, // missing colon
		false,
		true,
		&stdout, &stderr,
	)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid --rig-path") {
		t.Errorf("stderr = %q, want containing 'invalid --rig-path'", stderr.String())
	}
}

func TestBuildImageWithRigPaths(t *testing.T) {
	// Create city and rig directories.
	cityDir := t.TempDir()
	rigDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doBuildImage(
		[]string{cityDir},
		"",
		"gc-agent:latest",
		[]string{"my-rig:" + rigDir},
		false,
		true, // context-only
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("doBuildImage returned %d; stderr: %s", code, stderr.String())
	}

	// Extract output directory.
	line := strings.TrimSpace(stdout.String())
	parts := strings.SplitN(line, ": ", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected stdout format: %q", line)
	}
	outputDir := parts[1]

	// Verify rig content was included.
	if _, err := os.Stat(filepath.Join(outputDir, "workspace", "my-rig", "main.go")); err != nil {
		t.Errorf("missing workspace/my-rig/main.go: %v", err)
	}

	_ = os.RemoveAll(outputDir)
}

func TestBuildImageRoutesContextWarningsToStderr(t *testing.T) {
	cityDir := t.TempDir()
	targetDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "skill.md"), []byte("# skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, filepath.Join(cityDir, ".claude", "skills", "core.gc-agents")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doBuildImage(
		[]string{cityDir},
		"",
		"gc-agent:latest",
		nil,
		false,
		true,
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("doBuildImage returned %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "skipping symlinked directory") {
		t.Fatalf("stderr = %q, want skipped directory symlink diagnostic", stderr.String())
	}

	line := strings.TrimSpace(stdout.String())
	parts := strings.SplitN(line, ": ", 2)
	if len(parts) == 2 {
		_ = os.RemoveAll(parts[1])
	}
}

func TestBuildImageCLIRegistered(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)
	root.SetOut(&stdout)
	root.SetArgs([]string{"build-image", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("build-image --help: %v", err)
	}
	if !strings.Contains(stdout.String(), "prebaked") {
		t.Errorf("help output should mention prebaked, got:\n%s", stdout.String())
	}
}
