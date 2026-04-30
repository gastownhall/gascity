package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeCommands(t *testing.T) {
	cityPath := t.TempDir()
	cityPath, _ = filepath.EvalSymlinks(cityPath)
	// Set up city
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	targetRigPath := t.TempDir()
	targetRigPath, _ = filepath.EvalSymlinks(targetRigPath)

	// git init the target rig
	cmd := exec.Command("git", "init")
	cmd.Dir = targetRigPath
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = targetRigPath
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = targetRigPath
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(targetRigPath, "test.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "add", "test.txt")
	cmd.Dir = targetRigPath
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "commit", "-m", "initial commit")
	cmd.Dir = targetRigPath
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	cityToml := `[workspace]
name = "test-city"

[[rigs]]
name = "target"
path = "` + targetRigPath + `"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_CITY", cityPath)

	var stdout, stderr bytes.Buffer
	err := doWorktreeAdd("target", "alice", "sourcerig", &stdout, &stderr)
	if err != nil {
		t.Fatalf("add failed: %v\nstderr: %s", err, stderr.String())
	}
	
	out := stdout.String()
	if !strings.Contains(out, "Added worktree") {
		t.Errorf("unexpected output: %s", out)
	}

	// List
	stdout.Reset()
	stderr.Reset()
	err = doWorktreeList("target", &stdout, &stderr)
	if err != nil {
		t.Fatalf("list failed: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "alice-from-sourcerig") {
		t.Errorf("expected to find worktree in list, got: %s", stdout.String())
	}

	// Remove
	stdout.Reset()
	stderr.Reset()
	
	cmdList := exec.Command("git", "worktree", "list")
	cmdList.Dir = targetRigPath
	outWT, _ := cmdList.CombinedOutput()
	t.Logf("git worktree list:\n%s", outWT)
	
	err = doWorktreeRemove("target", "alice", "sourcerig", &stdout, &stderr)
	if err != nil {
		t.Fatalf("remove failed: %v\nstderr: %s", err, stderr.String())
	}

	// List again
	stdout.Reset()
	stderr.Reset()
	err = doWorktreeList("target", &stdout, &stderr)
	if err != nil {
		t.Fatalf("list failed: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "alice-from-sourcerig") {
		t.Errorf("expected worktree to be removed from list, got: %s", stdout.String())
	}
}
