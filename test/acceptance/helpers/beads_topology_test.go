package acceptancehelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveLegacyGCBinarySeparatesUnsetFromMisconfigured pins the distinction
// the M5 shape used to lose. A "" meant both "the operator did not set
// GC_ACCEPTANCE_LEGACY_GC_BIN" and "they set it to something unusable", and
// StartTopology reads "" as the former, so a deleted binary or a typo skipped
// M5 with a message telling the operator to set a variable they already set.
func TestResolveLegacyGCBinarySeparatesUnsetFromMisconfigured(t *testing.T) {
	dir := t.TempDir()
	usable := filepath.Join(dir, "gc-pre-journal")
	if err := os.WriteFile(usable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write fixture binary: %v", err)
	}

	t.Run("unset is absence", func(t *testing.T) {
		bin, err := resolveLegacyGCBinary("")
		if err != nil || bin != "" {
			t.Fatalf("resolveLegacyGCBinary(\"\") = (%q, %v), want (\"\", nil)", bin, err)
		}
	})

	t.Run("blank is absence", func(t *testing.T) {
		bin, err := resolveLegacyGCBinary("   ")
		if err != nil || bin != "" {
			t.Fatalf("resolveLegacyGCBinary(blank) = (%q, %v), want (\"\", nil)", bin, err)
		}
	})

	t.Run("missing file fails", func(t *testing.T) {
		missing := filepath.Join(dir, "gone")
		bin, err := resolveLegacyGCBinary(missing)
		if err == nil {
			t.Fatalf("resolveLegacyGCBinary(%q) = (%q, nil), want an error", missing, bin)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Fatalf("error %q does not name the path %q", err, missing)
		}
		if !strings.Contains(err.Error(), "is not an executable file") {
			t.Fatalf("error %q does not say the path is unusable", err)
		}
	})

	t.Run("directory fails", func(t *testing.T) {
		if _, err := resolveLegacyGCBinary(dir); err == nil {
			t.Fatalf("resolveLegacyGCBinary(%q) accepted a directory", dir)
		}
	})

	t.Run("usable file resolves absolute", func(t *testing.T) {
		bin, err := resolveLegacyGCBinary(usable)
		if err != nil {
			t.Fatalf("resolveLegacyGCBinary(%q): %v", usable, err)
		}
		if !filepath.IsAbs(bin) || bin != usable {
			t.Fatalf("resolveLegacyGCBinary(%q) = %q, want %q", usable, bin, usable)
		}
	})
}
