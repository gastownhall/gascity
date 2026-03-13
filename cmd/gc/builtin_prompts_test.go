package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
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

	got := renderPrompt(fsys.OSFS{}, dir, "gastown", filepath.Join(".gc", "prompts", name), ctx, "", io.Discard, nil, nil, nil)
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

func currentHookCommand(target string) string {
	return "gc " + newHookCmd(io.Discard, io.Discard).Name() + " " + target
}

func currentSlingCommand(target, bead string) string {
	return "gc " + newSlingCmd(io.Discard, io.Discard).Name() + " " + target + " " + bead
}

func TestMaterializeBuiltinPrompts(t *testing.T) {
	dir := t.TempDir()
	if err := materializeBuiltinPrompts(dir); err != nil {
		t.Fatalf("materializeBuiltinPrompts: %v", err)
	}

	// All 8 embedded prompts should exist.
	want := []string{
		"foreman.md", "loop-mail.md", "loop.md", "mayor.md",
		"one-shot.md", "pool-worker.md", "scoped-worker.md", "worker.md",
	}
	promptsDir := filepath.Join(dir, ".gc", "prompts")
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
	promptsDir := filepath.Join(dir, ".gc", "prompts")
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

func TestMaterializeBuiltinFixedWorkerPromptsUseCurrentHookCommand(t *testing.T) {
	dir := materializeBuiltinPromptsForTest(t)

	tests := map[string]PromptContext{
		"worker.md":        {AgentName: "mayor", TemplateName: "mayor"},
		"one-shot.md":      {AgentName: "mayor", TemplateName: "mayor"},
		"scoped-worker.md": {AgentName: "hello-world/worker", TemplateName: "worker", RigName: "hello-world", WorkDir: "/city/hello-world"},
	}
	for name, ctx := range tests {
		rendered := renderBuiltinPromptForTest(t, dir, name, ctx)
		assertRenderedPromptContains(t, rendered, name, []string{currentHookCommand("$GC_AGENT")})
	}
}

func TestMaterializeBuiltinLoopPromptsUseCurrentHookAndSlingCommands(t *testing.T) {
	dir := materializeBuiltinPromptsForTest(t)

	tests := map[string]PromptContext{
		"loop.md":      {AgentName: "worker", TemplateName: "worker"},
		"loop-mail.md": {AgentName: "worker", TemplateName: "worker"},
	}
	want := []string{
		currentHookCommand("$GC_AGENT"),
		currentSlingCommand("$GC_AGENT", "<id>"),
	}
	for name, ctx := range tests {
		rendered := renderBuiltinPromptForTest(t, dir, name, ctx)
		assertRenderedPromptContains(t, rendered, name, want)
	}
}

func TestRenderBuiltinPoolWorkerPromptUsesPoolTemplateTarget(t *testing.T) {
	dir := materializeBuiltinPromptsForTest(t)

	tests := []struct {
		name string
		ctx  PromptContext
	}{
		{
			name: "rig-scoped",
			ctx: PromptContext{
				AgentName:    "hello-world/polecat-2",
				TemplateName: "polecat",
				RigName:      "hello-world",
			},
		},
		{
			name: "city-scoped",
			ctx: PromptContext{
				AgentName:    "polecat-2",
				TemplateName: "polecat",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := renderBuiltinPromptForTest(t, dir, "pool-worker.md", tt.ctx)
			wantTarget := currentHookCommand((&config.Agent{Dir: tt.ctx.RigName, Name: tt.ctx.TemplateName}).QualifiedName())
			assertRenderedPromptContains(t, rendered, "pool-worker.md", []string{wantTarget})
			if strings.Contains(rendered, currentHookCommand("$GC_AGENT")) {
				t.Errorf("pool worker prompt should not use instance name for pool hook: %q", rendered)
			}
			if strings.Contains(rendered, "gc "+newSlingCmd(io.Discard, io.Discard).Name()) {
				t.Errorf("pool worker prompt should direct execution of pooled work, not mention gc sling: %q", rendered)
			}
		})
	}
}

func TestMaterializeBuiltinFormulas(t *testing.T) {
	dir := t.TempDir()
	if err := materializeBuiltinFormulas(dir); err != nil {
		t.Fatalf("materializeBuiltinFormulas: %v", err)
	}

	// All 5 embedded formulas should exist.
	want := []string{
		"cooking.formula.toml",
		"mol-do-work.formula.toml",
		"mol-polecat-base.formula.toml",
		"mol-polecat-commit.formula.toml",
		"pancakes.formula.toml",
	}
	formulasDir := filepath.Join(dir, ".gc", "formulas")
	for _, name := range want {
		path := filepath.Join(formulasDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("missing formula %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("formula %s is empty", name)
		}
	}
}

func TestMaterializeBuiltinFormulasOverwrites(t *testing.T) {
	dir := t.TempDir()
	formulasDir := filepath.Join(dir, ".gc", "formulas")
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
	if string(data) == "stale" {
		t.Error("stale content was not overwritten")
	}
}
