package overlay

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyDir_RecursiveCopy(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create source tree:
	//   file.txt
	//   sub/nested.txt
	//   sub/deep/leaf.txt
	writeFile(t, filepath.Join(src, "file.txt"), "top-level")
	mkdirAll(t, filepath.Join(src, "sub"))
	writeFile(t, filepath.Join(src, "sub", "nested.txt"), "nested content")
	mkdirAll(t, filepath.Join(src, "sub", "deep"))
	writeFile(t, filepath.Join(src, "sub", "deep", "leaf.txt"), "deep content")

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %s", stderr.String())
	}

	assertFileContent(t, filepath.Join(dst, "file.txt"), "top-level")
	assertFileContent(t, filepath.Join(dst, "sub", "nested.txt"), "nested content")
	assertFileContent(t, filepath.Join(dst, "sub", "deep", "leaf.txt"), "deep content")
}

func TestCopyDir_PreservesPermissions(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create an executable file.
	path := filepath.Join(src, "run.sh")
	writeFile(t, path, "#!/bin/sh\necho hello")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	info, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("permissions = %o, want 755", info.Mode().Perm())
	}
}

func TestCopyDir_MissingSrcDir(t *testing.T) {
	dst := t.TempDir()
	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")

	var stderr bytes.Buffer
	err := CopyDir(nonExistent, dst, &stderr)
	if err != nil {
		t.Errorf("CopyDir should return nil for missing src, got: %v", err)
	}
}

func TestCopyDir_EmptyDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %s", stderr.String())
	}
}

func TestCopyDir_OverwriteExisting(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeFile(t, filepath.Join(src, "config.toml"), "new content")
	writeFile(t, filepath.Join(dst, "config.toml"), "old content")

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "config.toml"), "new content")
}

func TestCopyFileOrDir_FileIntoExistingDirectoryPreservesBaseName(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	src := filepath.Join(srcDir, "notes.txt")
	writeFile(t, src, "hello")

	var stderr bytes.Buffer
	if err := CopyFileOrDir(src, dstDir, &stderr); err != nil {
		t.Fatalf("CopyFileOrDir: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	assertFileContent(t, filepath.Join(dstDir, "notes.txt"), "hello")
}

func TestCopyDir_NestedSubdirs(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create deeply nested structure.
	mkdirAll(t, filepath.Join(src, "a", "b", "c"))
	writeFile(t, filepath.Join(src, "a", "b", "c", "deep.txt"), "deep")

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "a", "b", "c", "deep.txt"), "deep")
}

func TestCopyDir_SrcNotADirectory(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "file.txt")
	writeFile(t, src, "not a dir")
	dst := t.TempDir()

	var stderr bytes.Buffer
	err := CopyDir(src, dst, &stderr)
	if err == nil {
		t.Fatal("expected error when src is a file, got nil")
	}
}

// --- CopyDirWithSkip tests ---

func TestCopyDirWithSkip_NilSkipCopiesEverything(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeFile(t, filepath.Join(src, "a.txt"), "alpha")
	mkdirAll(t, filepath.Join(src, "sub"))
	writeFile(t, filepath.Join(src, "sub", "b.txt"), "beta")

	var stderr bytes.Buffer
	if err := CopyDirWithSkip(src, dst, nil, &stderr); err != nil {
		t.Fatalf("CopyDirWithSkip: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "a.txt"), "alpha")
	assertFileContent(t, filepath.Join(dst, "sub", "b.txt"), "beta")
}

func TestCopyDirWithSkip_SkipFile(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeFile(t, filepath.Join(src, "keep.txt"), "kept")
	writeFile(t, filepath.Join(src, "skip_test.go"), "skipped")

	skip := func(relPath string, isDir bool) bool {
		return !isDir && filepath.Ext(relPath) == ".go"
	}

	var stderr bytes.Buffer
	if err := CopyDirWithSkip(src, dst, skip, &stderr); err != nil {
		t.Fatalf("CopyDirWithSkip: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "keep.txt"), "kept")
	if _, err := os.Stat(filepath.Join(dst, "skip_test.go")); !os.IsNotExist(err) {
		t.Error("skip_test.go should not have been copied")
	}
}

func TestCopyDirWithSkip_SkipDirExcludesSubtree(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeFile(t, filepath.Join(src, "top.txt"), "top")
	mkdirAll(t, filepath.Join(src, ".gc"))
	writeFile(t, filepath.Join(src, ".gc", "state.json"), "state")
	mkdirAll(t, filepath.Join(src, "keep"))
	writeFile(t, filepath.Join(src, "keep", "data.txt"), "data")

	skip := func(relPath string, isDir bool) bool {
		return isDir && relPath == ".gc"
	}

	var stderr bytes.Buffer
	if err := CopyDirWithSkip(src, dst, skip, &stderr); err != nil {
		t.Fatalf("CopyDirWithSkip: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, "top.txt"), "top")
	assertFileContent(t, filepath.Join(dst, "keep", "data.txt"), "data")
	if _, err := os.Stat(filepath.Join(dst, ".gc")); !os.IsNotExist(err) {
		t.Error(".gc directory should not have been copied")
	}
}

func TestCopyDirWithSkip_PreservesPermissions(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	path := filepath.Join(src, "run.sh")
	writeFile(t, path, "#!/bin/sh\necho hello")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var stderr bytes.Buffer
	if err := CopyDirWithSkip(src, dst, nil, &stderr); err != nil {
		t.Fatalf("CopyDirWithSkip: %v", err)
	}

	info, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("permissions = %o, want 755", info.Mode().Perm())
	}
}

// --- Merge integration tests ---

func TestCopyDir_MergesSettingsJSON(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Pre-populate dst with base hooks.
	baseJSON := `{
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gc prime"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "gc hook --inject"}]}]
  }
}`
	writeFile(t, filepath.Join(dst, ".claude", "settings.json"), baseJSON)

	// Src overlay adds PreToolUse only.
	overJSON := `{
  "hooks": {
    "PreToolUse": [{"matcher": "Bash(*foo*)", "hooks": [{"type": "command", "command": "guard"}]}]
  }
}`
	writeFile(t, filepath.Join(src, ".claude", "settings.json"), overJSON)

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %s", stderr.String())
	}

	// Verify merged result has all three categories.
	data, err := os.ReadFile(filepath.Join(dst, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)
	for _, cat := range []string{"SessionStart", "Stop", "PreToolUse"} {
		if _, ok := hooks[cat]; !ok {
			t.Errorf("missing category %q after merge", cat)
		}
	}
}

func TestCopyDir_NonMergeableOverwrite(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeFile(t, filepath.Join(dst, ".opencode", "config.js"), "old content")
	writeFile(t, filepath.Join(src, ".opencode", "config.js"), "new content")

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	// Non-mergeable file should be overwritten.
	assertFileContent(t, filepath.Join(dst, ".opencode", "config.js"), "new content")
}

func TestCopyDir_MergeableNewFile(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	overJSON := `{"hooks": {"Stop": [{"matcher": "", "hooks": []}]}}`
	writeFile(t, filepath.Join(src, ".claude", "settings.json"), overJSON)

	// No pre-existing dst file — should create normally.
	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("reading copied settings: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal copied settings: %v", err)
	}
	if !bytes.Contains(data, []byte("\n  ")) {
		t.Fatalf("copied mergeable JSON was not canonicalized:\n%s", data)
	}
}

func TestCopyDir_MergeableNewFileCanonicalizesJSON(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeFile(t, filepath.Join(src, ".codex", "hooks.json"), `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"gc prime && gc hook"}]}]}}`)

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("reading copied hooks: %v", err)
	}
	if bytes.Contains(data, []byte(`\u0026`)) {
		t.Fatalf("copied mergeable JSON escaped command operator:\n%s", data)
	}
	if !bytes.Contains(data, []byte("\n  ")) {
		t.Fatalf("copied mergeable JSON was not pretty canonicalized:\n%s", data)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("copied mergeable JSON missing trailing newline:\n%s", data)
	}
}

func TestCopyDir_MergeInvalidJSON(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Pre-populate dst with invalid JSON.
	writeFile(t, filepath.Join(dst, ".claude", "settings.json"), "not json")
	writeFile(t, filepath.Join(src, ".claude", "settings.json"), `{"hooks": {}}`)

	// Should fall back to overwrite (no error).
	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	assertFileContent(t, filepath.Join(dst, ".claude", "settings.json"), `{"hooks": {}}`)
}

func TestCopyDirWithSkip_MergesSettingsJSON(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	baseJSON := `{"hooks": {"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "stop"}]}]}}`
	writeFile(t, filepath.Join(dst, ".claude", "settings.json"), baseJSON)

	overJSON := `{"hooks": {"PreToolUse": [{"matcher": "Bash(*x*)", "hooks": []}]}}`
	writeFile(t, filepath.Join(src, ".claude", "settings.json"), overJSON)

	var stderr bytes.Buffer
	if err := CopyDirWithSkip(src, dst, nil, &stderr); err != nil {
		t.Fatalf("CopyDirWithSkip: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)
	for _, cat := range []string{"Stop", "PreToolUse"} {
		if _, ok := hooks[cat]; !ok {
			t.Errorf("missing category %q", cat)
		}
	}
}

func TestCopyDir_MergePreservesPermissions(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Pre-populate dst with restricted permissions.
	dstPath := filepath.Join(dst, ".claude", "settings.json")
	writeFile(t, dstPath, `{"hooks": {"Stop": [{"matcher": "", "hooks": []}]}}`)
	if err := os.Chmod(dstPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	writeFile(t, filepath.Join(src, ".claude", "settings.json"),
		`{"hooks": {"PreToolUse": [{"matcher": "Bash(*x*)", "hooks": []}]}}`)

	var stderr bytes.Buffer
	if err := CopyDir(src, dst, &stderr); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}

	info, err := os.Stat(dstPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %o, want 600", info.Mode().Perm())
	}
}

// helpers

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %q: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%q content = %q, want %q", path, string(data), want)
	}
}
