package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestMaterializeBuiltinPrompts(t *testing.T) {
	dir := t.TempDir()
	if err := materializeBuiltinPrompts(dir); err != nil {
		t.Fatalf("materializeBuiltinPrompts: %v", err)
	}

	// All embedded prompts should exist.
	want := []string{
		"foreman.md", "loop-mail.md", "loop.md", "mayor.md",
		"one-shot.md", "pool-worker.md", "scoped-worker.md", "worker.md",
		"graph-worker.md",
	}
	promptsDir := filepath.Join(dir, citylayout.PromptsRoot)
	for _, name := range want {
		path := filepath.Join(promptsDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("missing prompt %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("prompt %s is empty", name)
		}
	}
}

func TestMaterializeBuiltinPromptsOverwrites(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, citylayout.PromptsRoot)
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write stale content.
	stale := filepath.Join(promptsDir, "mayor.md")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := materializeBuiltinPrompts(dir); err != nil {
		t.Fatalf("materializeBuiltinPrompts: %v", err)
	}

	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Error("stale content was not overwritten")
	}
}

func TestMaterializeBuiltinFormulas(t *testing.T) {
	dir := t.TempDir()
	if err := materializeBuiltinFormulas(dir); err != nil {
		t.Fatalf("materializeBuiltinFormulas: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, citylayout.FormulasRoot, "pancakes.toml")); !os.IsNotExist(err) {
		t.Fatalf("materializeBuiltinFormulas should not write city-local formula seeds on start")
	}
}

func TestMaterializeBuiltinFormulasOverwrites(t *testing.T) {
	dir := t.TempDir()
	formulasDir := filepath.Join(dir, citylayout.FormulasRoot)
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write stale content.
	stale := filepath.Join(formulasDir, "pancakes.toml")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := materializeBuiltinFormulas(dir); err != nil {
		t.Fatalf("materializeBuiltinFormulas: %v", err)
	}

	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "stale" {
		t.Error("materializeBuiltinFormulas should leave city-local formula seeds untouched")
	}
}
