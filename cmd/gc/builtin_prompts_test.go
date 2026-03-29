package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

func materializeBuiltinPromptsForTest(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := materializeBuiltinPrompts(dir); err != nil {
		t.Fatalf("materializeBuiltinPrompts: %v", err)
	}
	return dir
}

func renderBuiltinPromptForTest(t *testing.T, dir, name string, ctx PromptContext) string {
	t.Helper()

	got := renderPrompt(fsys.OSFS{}, dir, "gastown", filepath.Join(citylayout.PromptsRoot, name), ctx, "", io.Discard, nil, nil, nil)
	if got == "" {
		t.Fatalf("renderPrompt(%s) returned empty output", name)
	}
	return got
}

func assertRenderedPromptContains(t *testing.T, rendered, name string, want []string) {
	t.Helper()

	for _, needle := range want {
		if strings.Contains(rendered, needle) {
			continue
		}
		t.Errorf("prompt %s missing %q", name, needle)
	}
}

func assertRenderedPromptDoesNotContain(t *testing.T, rendered, name string, blocked []string) {
	t.Helper()

	for _, needle := range blocked {
		if !strings.Contains(rendered, needle) {
			continue
		}
		t.Errorf("prompt %s unexpectedly contains %q", name, needle)
	}
}

func currentSlingCommand(target, bead string) string {
	return "gc " + newSlingCmd(io.Discard, io.Discard).Name() + " " + target + " " + bead
}

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

func TestMaterializeBuiltinFixedWorkerPromptsUseInjectedWorkQuery(t *testing.T) {
	dir := materializeBuiltinPromptsForTest(t)

	workQuery := "custom-work-query --agent=$GC_SESSION_NAME"
	tests := map[string]PromptContext{
		"worker.md": {
			AgentName:    "mayor",
			TemplateName: "mayor",
			WorkQuery:    workQuery,
		},
		"one-shot.md": {
			AgentName:    "mayor",
			TemplateName: "mayor",
			WorkQuery:    workQuery,
		},
		"scoped-worker.md": {
			AgentName:    "hello-world/worker",
			TemplateName: "worker",
			RigName:      "hello-world",
			WorkDir:      "/city/hello-world",
			WorkQuery:    workQuery,
		},
	}
	for name, ctx := range tests {
		rendered := renderBuiltinPromptForTest(t, dir, name, ctx)
		assertRenderedPromptContains(t, rendered, name, []string{workQuery})
		assertRenderedPromptDoesNotContain(t, rendered, name, []string{"gc agent claimed"})
	}
}

func TestMaterializeBuiltinLoopPromptsUseInjectedWorkQueryAndCurrentSlingCommand(t *testing.T) {
	dir := materializeBuiltinPromptsForTest(t)

	workQuery := "custom-work-query --agent=$GC_SESSION_NAME"
	tests := map[string]PromptContext{
		"loop.md": {
			AgentName:    "worker",
			TemplateName: "worker",
			WorkQuery:    workQuery,
		},
		"loop-mail.md": {
			AgentName:    "worker",
			TemplateName: "worker",
			WorkQuery:    workQuery,
		},
	}
	want := []string{
		workQuery,
		currentSlingCommand("$GC_AGENT", "<id>"),
	}
	blocked := []string{
		"gc agent claimed",
		"gc agent claim",
	}
	for name, ctx := range tests {
		rendered := renderBuiltinPromptForTest(t, dir, name, ctx)
		assertRenderedPromptContains(t, rendered, name, want)
		assertRenderedPromptDoesNotContain(t, rendered, name, blocked)
	}
}

func TestMaterializeBuiltinFormulas(t *testing.T) {
	dir := t.TempDir()
	if err := materializeBuiltinFormulas(dir); err != nil {
		t.Fatalf("materializeBuiltinFormulas: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, citylayout.FormulasRoot, "pancakes.formula.toml")); !os.IsNotExist(err) {
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
	stale := filepath.Join(formulasDir, "pancakes.formula.toml")
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
