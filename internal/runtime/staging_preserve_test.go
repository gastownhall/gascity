package runtime

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeOverlayFixture(t *testing.T) (srcDir, dstDir, rel string) {
	t.Helper()
	srcDir = t.TempDir()
	dstDir = t.TempDir()
	rel = filepath.Join(".opencode", "plugins", "gascity.js")

	src := filepath.Join(srcDir, "per-provider", "opencode", rel)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(src, []byte("// bundled\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(src): %v", err)
	}

	dst := filepath.Join(dstDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("MkdirAll(dst): %v", err)
	}
	if err := os.WriteFile(dst, []byte("// local, current\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(dst): %v", err)
	}
	return srcDir, dstDir, rel
}

// Without a preserve predicate, staging keeps its existing behavior: the
// bundled copy wins. This is the baseline the fix must not change.
func TestStageProviderOverlayDirOverwritesWithoutPreserve(t *testing.T) {
	srcDir, dstDir, rel := writeOverlayFixture(t)

	if err := StageProviderOverlayDirSkippingMergeable(srcDir, dstDir, []string{"opencode"}, io.Discard); err != nil {
		t.Fatalf("stage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, rel))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "// bundled\n" {
		t.Fatalf("expected overlay to overwrite without a preserve predicate, got %q", got)
	}
}

// A managed hook file the predicate reports as current must survive staging —
// the reconcile tick must not clobber it (#5554).
func TestStageProviderOverlayDirPreservesCurrentManagedFile(t *testing.T) {
	srcDir, dstDir, rel := writeOverlayFixture(t)

	var askedFor string
	preserve := func(relPath string, existing []byte) bool {
		askedFor = relPath
		return string(existing) == "// local, current\n"
	}

	if err := StageProviderOverlayDirSkippingMergeable(
		srcDir, dstDir, []string{"opencode"}, io.Discard, WithPreserve(preserve),
	); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if askedFor != rel {
		t.Fatalf("preserve called with %q, want the flattened per-provider path %q", askedFor, rel)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, rel))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "// local, current\n" {
		t.Fatalf("current managed file was clobbered by staging: %q", got)
	}
}

// A stale file is still replaced, so the predicate cannot strand a hook file.
func TestStageProviderOverlayDirReplacesStaleManagedFile(t *testing.T) {
	srcDir, dstDir, rel := writeOverlayFixture(t)

	preserve := func(_ string, _ []byte) bool { return false }
	if err := StageProviderOverlayDirSkippingMergeable(
		srcDir, dstDir, []string{"opencode"}, io.Discard, WithPreserve(preserve),
	); err != nil {
		t.Fatalf("stage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, rel))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "// bundled\n" {
		t.Fatalf("stale managed file was not replaced: %q", got)
	}
}
