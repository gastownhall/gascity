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
	mustMkdirAll(t, filepath.Join(src, "per-provider", "claude"))
	mustMkdirAll(t, filepath.Join(src, "per-provider", "codex"))
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
	mustMkdirAll(t, filepath.Join(src, "per-provider", "claude"))
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

func TestCopyDirForProviders_KiroPreservesExistingWorkspaceInstructions(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustMkdirAll(t, filepath.Join(src, "per-provider", "kiro", ".kiro", "agents"))
	mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", "AGENTS.md"), []byte("fallback instructions"), 0o644)
	mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", ".kiro", "agents", "gascity.json"), []byte(`{"name":"gascity"}`), 0o644)
	mustWriteFile(t, filepath.Join(dst, "AGENTS.md"), []byte("project instructions"), 0o600)

	if err := CopyDirForProviders(src, dst, []string{"kiro"}, io.Discard); err != nil {
		t.Fatalf("CopyDirForProviders: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want existing project instructions preserved", string(data))
	}
	info, err := os.Stat(filepath.Join(dst, "AGENTS.md"))
	if err != nil {
		t.Fatalf("stat AGENTS.md: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("AGENTS.md mode = %v, want 0600", got)
	}
	if _, err := os.Stat(filepath.Join(dst, ".kiro", "agents", "gascity.json")); err != nil {
		t.Fatalf("expected Kiro agent config to be staged: %v", err)
	}
}

func TestCopyDirForProviders_KiroInstallsFallbackInstructionsWhenMissing(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustMkdirAll(t, filepath.Join(src, "per-provider", "kiro"))
	mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", "AGENTS.md"), []byte("fallback instructions"), 0o644)

	if err := CopyDirForProviders(src, dst, []string{"kiro"}, io.Discard); err != nil {
		t.Fatalf("CopyDirForProviders: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "fallback instructions" {
		t.Fatalf("AGENTS.md = %q, want fallback instructions", string(data))
	}
}

func TestCopyDirForProviders_KiroPreservesEarlierInstructionsAcrossOverlayLayers(t *testing.T) {
	packOverlay := t.TempDir()
	agentOverlay := t.TempDir()
	dst := t.TempDir()

	mustMkdirAll(t, filepath.Join(packOverlay, "per-provider", "kiro"))
	mustWriteFile(t, filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md"), []byte("pack fallback"), 0o644)
	mustMkdirAll(t, filepath.Join(agentOverlay, "per-provider", "kiro"))
	mustWriteFile(t, filepath.Join(agentOverlay, "per-provider", "kiro", "AGENTS.md"), []byte("agent override"), 0o644)

	if err := CopyDirForProviders(packOverlay, dst, []string{"kiro"}, io.Discard); err != nil {
		t.Fatalf("CopyDirForProviders(pack): %v", err)
	}
	if err := CopyDirForProviders(agentOverlay, dst, []string{"kiro"}, io.Discard); err != nil {
		t.Fatalf("CopyDirForProviders(agent): %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "pack fallback" {
		t.Fatalf("AGENTS.md = %q, want earlier Kiro instructions preserved", string(data))
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
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
