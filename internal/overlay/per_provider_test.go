package overlay

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyDirForProvider_UniversalAndProviderSpecific(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create universal file.
	mustWriteFile(t, filepath.Join(src, "AGENTS.md"), []byte("universal"), 0o644)

	// Create per-provider files.
	mustMkdirAll(t, filepath.Join(src, "per-provider", "claude"), 0o755)
	mustMkdirAll(t, filepath.Join(src, "per-provider", "codex"), 0o755)
	mustWriteFile(t, filepath.Join(src, "per-provider", "claude", "CLAUDE.md"), []byte("claude-specific"), 0o644)
	mustWriteFile(t, filepath.Join(src, "per-provider", "codex", "AGENTS.md"), []byte("codex-specific"), 0o644)

	// Copy for claude provider.
	if err := CopyDirForProvider(src, dst, "claude", io.Discard); err != nil {
		t.Fatalf("CopyDirForProvider: %v", err)
	}

	// Universal file should be present.
	if _, err := os.ReadFile(filepath.Join(dst, "AGENTS.md")); err != nil {
		t.Fatalf("missing universal AGENTS.md: %v", err)
	}
	// Claude's CLAUDE.md should be present (flattened from per-provider/claude/).
	data, err := os.ReadFile(filepath.Join(dst, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("missing claude CLAUDE.md: %v", err)
	}
	if string(data) != "claude-specific" {
		t.Errorf("CLAUDE.md = %q, want %q", string(data), "claude-specific")
	}
	// Codex's AGENTS.md should NOT be present (wrong provider).
	// The universal AGENTS.md should not have been overwritten by codex's version.
	data, _ = os.ReadFile(filepath.Join(dst, "AGENTS.md"))
	if string(data) != "universal" {
		t.Errorf("AGENTS.md = %q, want %q (universal, not codex)", string(data), "universal")
	}
	// per-provider/ directory itself should not appear in dst.
	if _, err := os.Stat(filepath.Join(dst, "per-provider")); err == nil {
		t.Error("per-provider/ directory should not be copied to dst")
	}
}

func TestCopyDirForProvider_NoPerProviderDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWriteFile(t, filepath.Join(src, "file.txt"), []byte("content"), 0o644)

	if err := CopyDirForProvider(src, dst, "claude", io.Discard); err != nil {
		t.Fatalf("CopyDirForProvider: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("missing file.txt: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("file.txt = %q, want %q", string(data), "content")
	}
}

func TestCopyDirForProvider_EmptyProviderName(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWriteFile(t, filepath.Join(src, "file.txt"), []byte("content"), 0o644)
	mustMkdirAll(t, filepath.Join(src, "per-provider", "claude"), 0o755)
	mustWriteFile(t, filepath.Join(src, "per-provider", "claude", "CLAUDE.md"), []byte("claude"), 0o644)

	// Empty provider name: only universal files copied.
	if err := CopyDirForProvider(src, dst, "", io.Discard); err != nil {
		t.Fatalf("CopyDirForProvider: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "file.txt")); err != nil {
		t.Error("universal file should be copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "CLAUDE.md")); err == nil {
		t.Error("provider-specific file should NOT be copied with empty provider name")
	}
}

func TestCopyDirForProvider_MissingSrcDir(t *testing.T) {
	dst := t.TempDir()

	// Missing source should be a no-op.
	if err := CopyDirForProvider("/nonexistent", dst, "claude", io.Discard); err != nil {
		t.Fatalf("expected no-op for missing src, got: %v", err)
	}
}

func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

//nolint:unparam // test helper keeps the permission explicit at each call site.
func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
