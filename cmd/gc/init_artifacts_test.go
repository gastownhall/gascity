package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestWriteInitFormulasSeedsScopedWorkBuiltin(t *testing.T) {
	dir := t.TempDir()

	if err := writeInitFormulas(fsys.OSFS{}, dir, false); err != nil {
		t.Fatalf("writeInitFormulas: %v", err)
	}

	path := filepath.Join(dir, citylayout.FormulasRoot, "mol-scoped-work.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded formula: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `formula = "mol-scoped-work"`) {
		t.Fatalf("seeded formula missing name; got:\n%s", text)
	}
	if !strings.Contains(text, `version = 2`) {
		t.Fatalf("seeded formula missing v2 marker; got:\n%s", text)
	}
}
