package builtinpacks

import (
	"errors"
	"io/fs"
	"testing"
)

func TestBundledGastownEmbedsOverlayFiles(t *testing.T) {
	pack, ok := ByName("gastown")
	if !ok {
		t.Fatal("missing bundled gastown pack")
	}
	if _, err := fs.Stat(pack.FS, "overlay/per-provider/codex/.codex/hooks.json"); err != nil {
		t.Fatalf("bundled gastown pack missing codex overlay hooks: %v", err)
	}
	if _, err := fs.Stat(pack.FS, "embed.go"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("embed.go stat err = %v, want not exist in embedded pack data", err)
	}
}
