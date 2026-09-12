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

// writeOverlayLayer builds a standalone per-provider overlay source supplying
// rel with the given content, for tests that stage several layers in order.
func writeOverlayLayer(t *testing.T, rel, content string) string {
	t.Helper()
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "per-provider", "opencode", rel)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(src): %v", err)
	}
	return srcDir
}

// Overlay layers are last-writer-wins (overlay.CopyDirForProviders documents
// it), and a preserve predicate must not invert that. On a fresh work directory
// the file layer one writes is not a pre-existing local file, so layer two must
// still override it — otherwise the first layer permanently pins the path and a
// city's override pack can never take effect.
func TestStageProviderOverlayDirPreserveKeepsLastWriterWins(t *testing.T) {
	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	layer1 := writeOverlayLayer(t, rel, "// layer one\n")
	layer2 := writeOverlayLayer(t, rel, "// layer two\n")
	dstDir := t.TempDir()

	// One option value spans the whole pass, exactly as the reconcile path
	// reuses a single preserveManaged across packDirs and overlayDir.
	preserve := WithPreserve(func(_ string, _ []byte) bool { return true })
	for _, srcDir := range []string{layer1, layer2} {
		if err := StageProviderOverlayDirSkippingMergeable(
			srcDir, dstDir, []string{"opencode"}, io.Discard, preserve,
		); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}

	got, err := os.ReadFile(filepath.Join(dstDir, rel))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "// layer two\n" {
		t.Fatalf("later overlay layer was suppressed by the preserve predicate: got %q, want %q",
			got, "// layer two\n")
	}
}

// The mirror case: a file that pre-existed the pass is local content, so it
// must survive every layer, not just the first one.
func TestStageProviderOverlayDirPreserveSurvivesAllLayers(t *testing.T) {
	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	layer1 := writeOverlayLayer(t, rel, "// layer one\n")
	layer2 := writeOverlayLayer(t, rel, "// layer two\n")

	dstDir := t.TempDir()
	dst := filepath.Join(dstDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("MkdirAll(dst): %v", err)
	}
	if err := os.WriteFile(dst, []byte("// local, current\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(dst): %v", err)
	}

	preserve := WithPreserve(func(_ string, existing []byte) bool {
		return string(existing) == "// local, current\n"
	})
	for _, srcDir := range []string{layer1, layer2} {
		if err := StageProviderOverlayDirSkippingMergeable(
			srcDir, dstDir, []string{"opencode"}, io.Discard, preserve,
		); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "// local, current\n" {
		t.Fatalf("pre-existing current file was clobbered by a later layer: %q", got)
	}
}
