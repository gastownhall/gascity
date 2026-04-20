package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageDirPreservesBestEffortOverlayWarnings(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	dstDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	if err := StageDir(srcDir, dstDir); err != nil {
		t.Fatalf("StageDir() error = %v, want nil", err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "ok.txt"))
	if err != nil {
		t.Fatalf("read copied overlay file: %v", err)
	}
	if string(data) != "copied" {
		t.Fatalf("copied overlay file = %q, want %q", string(data), "copied")
	}
}
