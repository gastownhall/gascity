package main

import (
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestRenderPromptEmptyPath(t *testing.T) {
	f := fsys.NewFake()
	got := renderPrompt(f, "/city", "", "", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "" {
		t.Errorf("renderPrompt(empty path) = %q, want empty", got)
	}
}

func TestRenderPromptMissingFile(t *testing.T) {
	f := fsys.NewFake()
	got := renderPrompt(f, "/city", "", "prompts/missing.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "" {
		t.Errorf("renderPrompt(missing) = %q, want empty", got)
	}
}

func TestRenderPromptNoExpressions(t *testing.T) {
	f := fsys.NewFake()
	content := "# Simple Prompt\n\nNo template expressions here.\n"
	f.Files["/city/prompts/plain.md"] = []byte(content)
	got := renderPrompt(f, "/city", "", "prompts/plain.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != content {
		t.Errorf("renderPrompt(plain) = %q, want %q", got, content)
	}
}

func TestRenderPromptBasicVars(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("City: {{ .CityRoot }}\nAgent: {{ .AgentName }}\n")
	ctx := PromptContext{
		CityRoot:  "/home/user/bright-lights",
		AgentName: "hello-world/polecat-1",
	}
	got := renderPrompt(f, "/city", "bright-lights", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	want := "City: /home/user/bright-lights\nAgent: hello-world/polecat-1\n"
	if got != want {
		t.Errorf("renderPrompt(vars) = %q, want %q", got, want)
	}
}

func TestRenderPromptTemplateName(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Template: {{ .TemplateName }}")
	ctx := PromptContext{TemplateName: "polecat"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Template: polecat" {
		t.Errorf("renderPrompt(template name) = %q, want %q", got, "Template: polecat")
	}
}

func TestRenderPromptBasenameFunction(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Instance: {{ basename .AgentName }}")
	ctx := PromptContext{AgentName: "hello-world/polecat-3"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Instance: polecat-3" {
		t.Errorf("renderPrompt(basename) = %q, want %q", got, "Instance: polecat-3")
	}
}

func TestRenderPromptBasenameSingleton(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Instance: {{ basename .AgentName }}")
	ctx := PromptContext{AgentName: "mayor"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Instance: mayor" {
		t.Errorf("renderPrompt(basename singleton) = %q, want %q", got, "Instance: mayor")
	}
}

func TestRenderPromptCmdFunction(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Run `{{ cmd }}` to start")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	// cmd returns filepath.Base(os.Args[0]) — in tests this is the test binary name.
	// Just verify it doesn't contain "{{ cmd }}" (i.e., the function was called).
	if strings.Contains(got, "{{ cmd }}") {
		t.Errorf("renderPrompt(cmd) still contains template expression: %q", got)
	}
	if !strings.Contains(got, "Run `") {
		t.Errorf("renderPrompt(cmd) missing prefix: %q", got)
	}
}

func TestRenderPromptSessionFunction(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`Session: {{ session "deacon" }}`)
	got := renderPrompt(f, "/city", "gastown", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Session: deacon" {
		t.Errorf("renderPrompt(session) = %q, want %q", got, "Session: deacon")
	}
}

func TestRenderPromptSessionFunctionCustomTemplate(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`Session: {{ session "deacon" }}`)
	got := renderPrompt(f, "/city", "gastown", "prompts/test.md.tmpl", PromptContext{}, "{{.City}}-{{.Agent}}", io.Discard, nil, nil, nil)
	if got != "Session: gastown-deacon" {
		t.Errorf("renderPrompt(session custom) = %q, want %q", got, "Session: gastown-deacon")
	}
}

func TestRenderPromptMissingKeyEmptyString(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Branch: {{ .Branch }}")
	// Branch not set → should be empty string (missingkey=zero).
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Branch: " {
		t.Errorf("renderPrompt(missing key) = %q, want %q", got, "Branch: ")
	}
}

func TestRenderPromptEnvMerge(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Custom: {{ .MyCustomVar }}")
	ctx := PromptContext{
		Env: map[string]string{"MyCustomVar": "hello"},
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Custom: hello" {
		t.Errorf("renderPrompt(env) = %q, want %q", got, "Custom: hello")
	}
}

func TestRenderPromptDefaultBranch(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Branch: {{ .DefaultBranch }}")
	ctx := PromptContext{DefaultBranch: "main"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Branch: main" {
		t.Errorf("renderPrompt(DefaultBranch) = %q, want %q", got, "Branch: main")
	}
}

func TestRenderPromptEnvOverridePriority(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Root: {{ .CityRoot }}")
	ctx := PromptContext{
		CityRoot: "/real/path",
		Env:      map[string]string{"CityRoot": "/env/path"},
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	// SDK vars take priority over Env.
	if got != "Root: /real/path" {
		t.Errorf("renderPrompt(override) = %q, want %q", got, "Root: /real/path")
	}
}

func TestRenderPromptParseErrorFallback(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/bad.md.tmpl"] = []byte("Bad: {{ .Unclosed")
	var stderr strings.Builder
	got := renderPrompt(f, "/city", "", "prompts/bad.md.tmpl", PromptContext{}, "", &stderr, nil, nil, nil)
	// Should return raw text on parse error.
	if got != "Bad: {{ .Unclosed" {
		t.Errorf("renderPrompt(parse error) = %q, want raw text", got)
	}
	if !strings.Contains(stderr.String(), "prompt template") {
		t.Errorf("stderr = %q, want warning about prompt template", stderr.String())
	}
}

func TestRenderPromptReadError(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/city/prompts/broken.md"] = errExit
	got := renderPrompt(f, "/city", "", "prompts/broken.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "" {
		t.Errorf("renderPrompt(read error) = %q, want empty", got)
	}
}

func TestRenderPromptMultiVariable(t *testing.T) {
	f := fsys.NewFake()
	tmpl := `# {{ .AgentName }} in {{ .RigName }}
Working in {{ .WorkDir }}
City: {{ .CityRoot }}
Template: {{ .TemplateName }}
Basename: {{ basename .AgentName }}
Prefix: {{ .IssuePrefix }}
Branch: {{ .Branch }}
Run {{ cmd }} to start
Session: {{ session "deacon" }}
Custom: {{ .DefaultBranch }}
`
	f.Files["/city/prompts/full.md.tmpl"] = []byte(tmpl)
	ctx := PromptContext{
		CityRoot:      "/home/user/city",
		AgentName:     "myrig/polecat-1",
		TemplateName:  "polecat",
		RigName:       "myrig",
		WorkDir:       "/home/user/city/myrig/polecats/polecat-1",
		IssuePrefix:   "mr-",
		Branch:        "feature/foo",
		DefaultBranch: "main",
	}
	got := renderPrompt(f, "/city", "gastown", "prompts/full.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "# myrig/polecat-1 in myrig") {
		t.Errorf("missing agent/rig: %q", got)
	}
	if !strings.Contains(got, "Working in /home/user/city/myrig/polecats/polecat-1") {
		t.Errorf("missing workdir: %q", got)
	}
	if !strings.Contains(got, "City: /home/user/city") {
		t.Errorf("missing city: %q", got)
	}
	if !strings.Contains(got, "Template: polecat") {
		t.Errorf("missing template name: %q", got)
	}
	if !strings.Contains(got, "Basename: polecat-1") {
		t.Errorf("missing basename: %q", got)
	}
	if !strings.Contains(got, "Prefix: mr-") {
		t.Errorf("missing prefix: %q", got)
	}
	if !strings.Contains(got, "Branch: feature/foo") {
		t.Errorf("missing branch: %q", got)
	}
	if !strings.Contains(got, "Session: deacon") {
		t.Errorf("missing session: %q", got)
	}
	if !strings.Contains(got, "Custom: main") {
		t.Errorf("missing env var: %q", got)
	}
}

func TestRenderPromptWorkQuery(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Work: {{ .WorkQuery }}")
	ctx := PromptContext{WorkQuery: "bd ready --assignee=mayor"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Work: bd ready --assignee=mayor" {
		t.Errorf("renderPrompt(WorkQuery) = %q, want %q", got, "Work: bd ready --assignee=mayor")
	}
}

func TestBuildTemplateData(t *testing.T) {
	ctx := PromptContext{
		CityRoot:      "/city",
		AgentName:     "a/b",
		TemplateName:  "b",
		RigName:       "a",
		WorkDir:       "/city/a",
		IssuePrefix:   "te-",
		Branch:        "main",
		DefaultBranch: "main",
		Env:           map[string]string{"Custom": "val", "CityRoot": "override"},
	}
	data := buildTemplateData(ctx)
	// SDK vars override Env.
	if data["CityRoot"] != "/city" {
		t.Errorf("CityRoot = %q, want %q", data["CityRoot"], "/city")
	}
	if data["Custom"] != "val" {
		t.Errorf("Custom = %q, want %q", data["Custom"], "val")
	}
	if data["TemplateName"] != "b" {
		t.Errorf("TemplateName = %q, want %q", data["TemplateName"], "b")
	}
	if data["DefaultBranch"] != "main" {
		t.Errorf("DefaultBranch = %q, want %q", data["DefaultBranch"], "main")
	}
}

func TestDefaultBranchFor_EmptyDir(t *testing.T) {
	// Empty dir should return "main" (safe fallback).
	got := defaultBranchFor("")
	if got != "main" {
		t.Errorf("defaultBranchFor(\"\") = %q, want %q", got, "main")
	}
}

func TestDefaultBranchFor_NonGitDir(t *testing.T) {
	// Non-git directory should return "main" (safe fallback).
	got := defaultBranchFor(t.TempDir())
	if got != "main" {
		t.Errorf("defaultBranchFor(tmpdir) = %q, want %q", got, "main")
	}
}

func TestBuildTemplateDataDefaultBranchOverridesEnv(t *testing.T) {
	ctx := PromptContext{
		DefaultBranch: "develop",
		Env:           map[string]string{"DefaultBranch": "env-main"},
	}
	data := buildTemplateData(ctx)
	// SDK field (DefaultBranch) should override Env value.
	if data["DefaultBranch"] != "develop" {
		t.Errorf("DefaultBranch = %q, want %q (SDK override)", data["DefaultBranch"], "develop")
	}
}

func TestBuildTemplateDataEmptyEnv(t *testing.T) {
	ctx := PromptContext{AgentName: "test"}
	data := buildTemplateData(ctx)
	if data["AgentName"] != "test" {
		t.Errorf("AgentName = %q, want %q", data["AgentName"], "test")
	}
}

func TestRenderPromptSharedTemplates(t *testing.T) {
	f := fsys.NewFake()
	// Shared template defines a named block.
	f.Files["/city/prompts/shared/greeting.md.tmpl"] = []byte(
		`{{ define "greeting" }}Hello, {{ .AgentName }}!{{ end }}`)
	// Main template uses it.
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`# Prompt\n{{ template "greeting" . }}`)
	ctx := PromptContext{AgentName: "mayor"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "Hello, mayor!") {
		t.Errorf("shared template not rendered: %q", got)
	}
}

func TestRenderPromptSharedMissingDir(t *testing.T) {
	f := fsys.NewFake()
	// No shared/ directory — should render normally without error.
	f.Files["/city/prompts/test.md.tmpl"] = []byte("No shared templates here.")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "No shared templates here." {
		t.Errorf("renderPrompt(no shared) = %q, want plain text", got)
	}
}

func TestRenderPromptSharedParseError(t *testing.T) {
	f := fsys.NewFake()
	// Bad shared template — should warn but still render main.
	f.Files["/city/prompts/shared/bad.md.tmpl"] = []byte(`{{ define "broken" }}{{ .Unclosed`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Main template works.")
	var stderr strings.Builder
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", &stderr, nil, nil, nil)
	if got != "Main template works." {
		t.Errorf("renderPrompt(bad shared) = %q, want main text", got)
	}
	if !strings.Contains(stderr.String(), "shared template") {
		t.Errorf("stderr = %q, want shared template warning", stderr.String())
	}
}

func TestRenderPromptSharedVariableAccess(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}Template: {{ .TemplateName }}, Work: {{ .WorkQuery }}{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`{{ template "info" . }}`)
	ctx := PromptContext{
		TemplateName: "polecat",
		WorkQuery:    "bd ready --label=pool:rig/polecat",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "Template: polecat") {
		t.Errorf("missing TemplateName in shared: %q", got)
	}
	if !strings.Contains(got, "Work: bd ready --label=pool:rig/polecat") {
		t.Errorf("missing WorkQuery in shared: %q", got)
	}
}

func TestRenderPromptSharedMultipleFiles(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/alpha.md.tmpl"] = []byte(
		`{{ define "alpha" }}A{{ end }}`)
	f.Files["/city/prompts/shared/beta.md.tmpl"] = []byte(
		`{{ define "beta" }}B{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`{{ template "alpha" . }}-{{ template "beta" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "A-B" {
		t.Errorf("renderPrompt(multi shared) = %q, want %q", got, "A-B")
	}
}

func TestRenderPromptSharedIgnoresNonTemplate(t *testing.T) {
	f := fsys.NewFake()
	// A .md file (not .md.tmpl) should be ignored.
	f.Files["/city/prompts/shared/readme.md"] = []byte(`{{ define "oops" }}should not load{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Plain text.")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Plain text." {
		t.Errorf("renderPrompt(non-template) = %q, want plain text", got)
	}
}

func TestRenderPromptCrossPackShared(t *testing.T) {
	f := fsys.NewFake()
	// Pack dir with prompts/shared/ containing a named template.
	f.Dirs["/extra/prompts/shared"] = true
	f.Files["/extra/prompts/shared/greet.md.tmpl"] = []byte(
		`{{ define "greet" }}Hi from cross-pack!{{ end }}`)
	// Main template references it.
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`{{ template "greet" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		[]string{"/extra"}, nil, nil)
	if got != "Hi from cross-pack!" {
		t.Errorf("cross-pack shared = %q, want %q", got, "Hi from cross-pack!")
	}
}

func TestRenderPromptCrossPackPriority(t *testing.T) {
	f := fsys.NewFake()
	// Pack dir with prompts/shared/ defining "info".
	f.Dirs["/extra/prompts/shared"] = true
	f.Files["/extra/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}cross-pack{{ end }}`)
	// Sibling shared dir also defines "info" — should win.
	f.Files["/city/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}sibling{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`{{ template "info" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		[]string{"/extra"}, nil, nil)
	if got != "sibling" {
		t.Errorf("priority = %q, want %q (sibling wins)", got, "sibling")
	}
}

func TestRenderPromptInjectFragments(t *testing.T) {
	f := fsys.NewFake()
	// Shared dir has named fragments.
	f.Files["/city/prompts/shared/frag.md.tmpl"] = []byte(
		`{{ define "footer" }}--- footer ---{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Main body.")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		nil, []string{"footer"}, nil)
	want := "Main body.\n\n--- footer ---"
	if got != want {
		t.Errorf("inject = %q, want %q", got, want)
	}
}

func TestRenderPromptInjectMissing(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Main body.")
	var stderr strings.Builder
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", &stderr,
		nil, []string{"nonexistent"}, nil)
	// Should not crash, just warn.
	if got != "Main body." {
		t.Errorf("inject missing = %q, want %q", got, "Main body.")
	}
	if !strings.Contains(stderr.String(), "nonexistent") {
		t.Errorf("stderr = %q, want warning about nonexistent", stderr.String())
	}
}

func TestRenderPromptGlobalAndPerAgent(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/frag.md.tmpl"] = []byte(
		`{{ define "global-frag" }}GLOBAL{{ end }}{{ define "agent-frag" }}AGENT{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Body.")
	// Global fragments come before per-agent.
	fragments := mergeFragmentLists([]string{"global-frag"}, []string{"agent-frag"})
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		nil, fragments, nil)
	want := "Body.\n\nGLOBAL\n\nAGENT"
	if got != want {
		t.Errorf("global+agent = %q, want %q", got, want)
	}
}

func TestExtractQualityGates(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "empty content",
			content: "",
			want:    "",
		},
		{
			name:    "no quality gates section",
			content: "# README\n\nSome text.\n",
			want:    "",
		},
		{
			name: "h2 quality gates section",
			content: `# Project

## Code quality gates

- go test ./...
- go vet ./...

## Next section

Other stuff.
`,
			want: "- go test ./...\n- go vet ./...",
		},
		{
			name: "h3 quality gates section",
			content: `## Development

### Quality Gates

Run tests before merging.

### Other
`,
			want: "Run tests before merging.",
		},
		{
			name: "quality gates at end of file",
			content: `# Project

## Quality gate checks

` + "```bash\nmake test\nmake lint\n```",
			want: "```bash\nmake test\nmake lint\n```",
		},
		{
			name: "case insensitive match",
			content: "## QUALITY GATES\n\nRun all tests.\n\n## Other\n",
			want:    "Run all tests.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractQualityGates(tt.content)
			if got != tt.want {
				t.Errorf("extractQualityGates() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQualityGatesFuncWithFakeFS(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/work/CLAUDE.md"] = []byte(`# Project

## Code quality gates

- go test ./...
- go vet ./...

## Other
`)
	fn := qualityGatesFunc(f)
	got := fn("/work", "CLAUDE.md")
	want := "- go test ./...\n- go vet ./..."
	if got != want {
		t.Errorf("quality_gates() = %q, want %q", got, want)
	}
}

func TestQualityGatesFuncMissingFile(t *testing.T) {
	f := fsys.NewFake()
	fn := qualityGatesFunc(f)
	got := fn("/work", "CLAUDE.md")
	if got != "" {
		t.Errorf("quality_gates(missing) = %q, want empty", got)
	}
}

func TestQualityGatesFuncEmptyArgs(t *testing.T) {
	fn := qualityGatesFunc(nil)
	if got := fn("", "CLAUDE.md"); got != "" {
		t.Errorf("quality_gates(empty workdir) = %q, want empty", got)
	}
	if got := fn("/work", ""); got != "" {
		t.Errorf("quality_gates(empty file) = %q, want empty", got)
	}
}

func TestQualityGatesTemplateFunction(t *testing.T) {
	f := fsys.NewFake()
	// Instructions file in the work directory.
	f.Files["/work/CLAUDE.md"] = []byte(`# Dev Guide

## Code quality gates

- make test
- make lint

## Architecture
`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`Gates: {{ quality_gates .WorkDir .InstructionsFile }}`)
	ctx := PromptContext{
		WorkDir:          "/work",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "make test") {
		t.Errorf("quality_gates in template = %q, want to contain 'make test'", got)
	}
}

func TestQualityGatesTemplateFallback(t *testing.T) {
	f := fsys.NewFake()
	// No instructions file — quality_gates returns empty, template should handle gracefully.
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`{{ $g := quality_gates .WorkDir .InstructionsFile }}{{ if $g }}{{ $g }}{{ else }}default gates{{ end }}`)
	ctx := PromptContext{
		WorkDir:          "/work",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "default gates" {
		t.Errorf("fallback = %q, want %q", got, "default gates")
	}
}

func TestQualityGatesProviderAware(t *testing.T) {
	f := fsys.NewFake()
	// AGENTS.md has different quality gates than CLAUDE.md.
	f.Files["/work/AGENTS.md"] = []byte(`# Project

## Quality gates

- npm test
- npm run lint

## Other
`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`{{ quality_gates .WorkDir .InstructionsFile }}`)
	ctx := PromptContext{
		WorkDir:          "/work",
		InstructionsFile: "AGENTS.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "npm test") {
		t.Errorf("AGENTS.md gates = %q, want to contain 'npm test'", got)
	}
}

func TestBuildTemplateDataInstructionsFile(t *testing.T) {
	ctx := PromptContext{
		InstructionsFile: "CLAUDE.md",
	}
	data := buildTemplateData(ctx)
	if data["InstructionsFile"] != "CLAUDE.md" {
		t.Errorf("InstructionsFile = %q, want %q", data["InstructionsFile"], "CLAUDE.md")
	}
}

func TestQualityGatesSharedTemplateWithRepoFallback(t *testing.T) {
	f := fsys.NewFake()
	// Shared quality-gates template that mirrors the real one.
	f.Files["/city/prompts/shared/quality-gates.md.tmpl"] = []byte(
		`{{ define "quality-gates" -}}
{{ $gates := quality_gates .WorkDir .InstructionsFile -}}
{{ if $gates -}}
{{ $gates }}
{{ else -}}
go test ./...
golangci-lint run ./...
{{ end -}}
{{ end -}}`)
	// Main template uses the shared quality-gates template.
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`Quality gates:
{{ template "quality-gates" . }}`)
	// Repo CLAUDE.md with quality gates section.
	f.Files["/work/CLAUDE.md"] = []byte(`# Project

## Code quality gates

- npm test
- npm run lint

## Other
`)
	ctx := PromptContext{
		WorkDir:          "/work",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	// Should use the repo's quality gates, not the hardcoded defaults.
	if !strings.Contains(got, "npm test") {
		t.Errorf("expected repo quality gates, got: %q", got)
	}
	if strings.Contains(got, "golangci-lint") {
		t.Errorf("should NOT contain hardcoded defaults when repo gates exist, got: %q", got)
	}
}

func TestQualityGatesSharedTemplateDefaultFallback(t *testing.T) {
	f := fsys.NewFake()
	// Shared quality-gates template.
	f.Files["/city/prompts/shared/quality-gates.md.tmpl"] = []byte(
		`{{ define "quality-gates" -}}
{{ $gates := quality_gates .WorkDir .InstructionsFile -}}
{{ if $gates -}}
{{ $gates }}
{{ else -}}
go test ./...
golangci-lint run ./...
{{ end -}}
{{ end -}}`)
	// Main template uses the shared quality-gates template.
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`Quality gates:
{{ template "quality-gates" . }}`)
	// No instructions file exists — should fall back to hardcoded defaults.
	ctx := PromptContext{
		WorkDir:          "/work",
		InstructionsFile: "CLAUDE.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "golangci-lint") {
		t.Errorf("expected hardcoded defaults when no repo gates, got: %q", got)
	}
}

func TestQualityGatesSharedTemplateNoQualitySection(t *testing.T) {
	f := fsys.NewFake()
	// Shared quality-gates template.
	f.Files["/city/prompts/shared/quality-gates.md.tmpl"] = []byte(
		`{{ define "quality-gates" -}}
{{ $gates := quality_gates .WorkDir .InstructionsFile -}}
{{ if $gates -}}
{{ $gates }}
{{ else -}}
go test ./...
golangci-lint run ./...
{{ end -}}
{{ end -}}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`{{ template "quality-gates" . }}`)
	// AGENTS.md exists but has no quality gates section.
	f.Files["/work/AGENTS.md"] = []byte(`# Project

## Getting Started

Just run it.
`)
	ctx := PromptContext{
		WorkDir:          "/work",
		InstructionsFile: "AGENTS.md",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	// No quality gates section in AGENTS.md → should fall back to defaults.
	if !strings.Contains(got, "golangci-lint") {
		t.Errorf("expected defaults when no quality section in AGENTS.md, got: %q", got)
	}
}

func TestMergeFragmentLists(t *testing.T) {
	tests := []struct {
		name    string
		global  []string
		agent   []string
		want    []string
		wantNil bool
	}{
		{"both nil", nil, nil, nil, true},
		{"global only", []string{"a"}, nil, []string{"a"}, false},
		{"agent only", nil, []string{"b"}, []string{"b"}, false},
		{"both", []string{"a", "b"}, []string{"c"}, []string{"a", "b", "c"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeFragmentLists(tt.global, tt.agent)
			if tt.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
