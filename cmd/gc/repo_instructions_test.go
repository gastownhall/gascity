package main

import (
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestReadRepoInstructionsFromRigRoot(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/rig/CLAUDE.md"] = []byte("# Quality Gates\nRun go test ./...\n")
	ctx := PromptContext{
		RigRoot:          "/rig",
		WorkDir:          "/rig/polecats/p1",
		InstructionsFile: "CLAUDE.md",
	}
	got := readRepoInstructions(f, ctx)
	if got != "# Quality Gates\nRun go test ./..." {
		t.Errorf("readRepoInstructions(rig root) = %q, want trimmed CLAUDE.md content", got)
	}
}

func TestReadRepoInstructionsFromWorkDir(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/work/AGENTS.md"] = []byte("# Agent Instructions\nRun npm test\n")
	ctx := PromptContext{
		RigRoot:          "",
		WorkDir:          "/work",
		InstructionsFile: "AGENTS.md",
	}
	got := readRepoInstructions(f, ctx)
	if got != "# Agent Instructions\nRun npm test" {
		t.Errorf("readRepoInstructions(work dir) = %q, want trimmed AGENTS.md content", got)
	}
}

func TestReadRepoInstructionsRigRootTakesPriority(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/rig/CLAUDE.md"] = []byte("rig version")
	f.Files["/work/CLAUDE.md"] = []byte("work version")
	ctx := PromptContext{
		RigRoot:          "/rig",
		WorkDir:          "/work",
		InstructionsFile: "CLAUDE.md",
	}
	got := readRepoInstructions(f, ctx)
	if got != "rig version" {
		t.Errorf("readRepoInstructions(priority) = %q, want %q", got, "rig version")
	}
}

func TestReadRepoInstructionsEmptyInstructionsFile(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/rig/CLAUDE.md"] = []byte("content")
	ctx := PromptContext{
		RigRoot:          "/rig",
		InstructionsFile: "",
	}
	got := readRepoInstructions(f, ctx)
	if got != "" {
		t.Errorf("readRepoInstructions(empty instructions file) = %q, want empty", got)
	}
}

func TestReadRepoInstructionsMissingFile(t *testing.T) {
	f := fsys.NewFake()
	ctx := PromptContext{
		RigRoot:          "/rig",
		WorkDir:          "/work",
		InstructionsFile: "CLAUDE.md",
	}
	got := readRepoInstructions(f, ctx)
	if got != "" {
		t.Errorf("readRepoInstructions(missing file) = %q, want empty", got)
	}
}

func TestReadRepoInstructionsSkipsWorkDirWhenSameAsRigRoot(t *testing.T) {
	f := fsys.NewFake()
	// File only exists at the shared path. If RigRoot == WorkDir, the
	// function should try RigRoot and NOT double-try.
	f.Files["/repo/CLAUDE.md"] = []byte("content")
	ctx := PromptContext{
		RigRoot:          "/repo",
		WorkDir:          "/repo",
		InstructionsFile: "CLAUDE.md",
	}
	got := readRepoInstructions(f, ctx)
	if got != "content" {
		t.Errorf("readRepoInstructions(same dir) = %q, want %q", got, "content")
	}
}

func TestRenderPromptRepoInstructionsFunction(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/rig/CLAUDE.md"] = []byte("Run go test ./...")
	f.Files["/city/prompts/test.template.md"] = []byte("Gates: {{ repoInstructions }}")
	ctx := PromptContext{
		RigRoot:          "/rig",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	if got != "Gates: Run go test ./..." {
		t.Errorf("renderPrompt(repoInstructions) = %q, want %q", got, "Gates: Run go test ./...")
	}
}

func TestRenderPromptRepoInstructionsMissing(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte("Gates: {{ repoInstructions }}")
	ctx := PromptContext{
		RigRoot:          "/rig",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	if got != "Gates: " {
		t.Errorf("renderPrompt(missing repo instructions) = %q, want %q", got, "Gates: ")
	}
}

func TestRenderPromptRepoInstructionsNoInstructionsFile(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte("Gates: {{ repoInstructions }}")
	ctx := PromptContext{
		RigRoot: "/rig",
		// InstructionsFile intentionally empty
	}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	if got != "Gates: " {
		t.Errorf("renderPrompt(no instructions file) = %q, want %q", got, "Gates: ")
	}
}

func TestRenderPromptRepoInstructionsProviderAware(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/rig/AGENTS.md"] = []byte("Codex quality gates here")
	f.Files["/city/prompts/test.template.md"] = []byte("{{ repoInstructions }}")
	ctx := PromptContext{
		RigRoot:          "/rig",
		InstructionsFile: "AGENTS.md", // codex/gemini provider
	}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	if got != "Codex quality gates here" {
		t.Errorf("renderPrompt(AGENTS.md) = %q, want %q", got, "Codex quality gates here")
	}
}

func TestBuildTemplateDataIncludesInstructionsFile(t *testing.T) {
	ctx := PromptContext{
		InstructionsFile: "CLAUDE.md",
	}
	data := buildTemplateData(ctx)
	if data["InstructionsFile"] != "CLAUDE.md" {
		t.Errorf("InstructionsFile = %q, want %q", data["InstructionsFile"], "CLAUDE.md")
	}
}

func TestRenderPromptRepoInstructionsInConditional(t *testing.T) {
	// Demonstrates the primary use case: fallback when pack guidance is empty.
	f := fsys.NewFake()
	f.Files["/rig/CLAUDE.md"] = []byte("Run make test && make lint")
	f.Files["/city/prompts/test.template.md"] = []byte(
		`{{ $ri := repoInstructions }}{{ if $ri }}## Quality Gates (from repo)

{{ $ri }}{{ else }}No quality gate guidance available.{{ end }}`)
	ctx := PromptContext{
		RigRoot:          "/rig",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "Run make test && make lint") {
		t.Errorf("conditional with instructions present: %q", got)
	}
	if !strings.Contains(got, "Quality Gates (from repo)") {
		t.Errorf("missing header in conditional output: %q", got)
	}
}

func TestRenderPromptRepoInstructionsConditionalFallsThrough(t *testing.T) {
	f := fsys.NewFake()
	// No CLAUDE.md at rig root — repoInstructions returns empty.
	f.Files["/city/prompts/test.template.md"] = []byte(
		`{{ $ri := repoInstructions }}{{ if $ri }}{{ $ri }}{{ else }}No guidance{{ end }}`)
	ctx := PromptContext{
		RigRoot:          "/rig",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	if got != "No guidance" {
		t.Errorf("conditional without instructions: %q, want %q", got, "No guidance")
	}
}
