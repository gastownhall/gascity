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

func TestRenderPromptPlainMarkdownDoesNotExecuteTemplates(t *testing.T) {
	f := fsys.NewFake()
	content := "Hello {{ .AgentName }}\n"
	f.Files["/city/prompts/plain.md"] = []byte(content)
	got := renderPrompt(f, "/city", "", "prompts/plain.md", PromptContext{AgentName: "mayor"}, "", io.Discard, nil, nil, nil)
	if got != content {
		t.Errorf("renderPrompt(plain markdown) = %q, want raw content %q", got, content)
	}
}

func TestRenderPromptBasicVars(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte("City: {{ .CityRoot }}\nAgent: {{ .AgentName }}\n")
	ctx := PromptContext{
		CityRoot:  "/home/user/bright-lights",
		AgentName: "hello-world/polecat-1",
	}
	got := renderPrompt(f, "/city", "bright-lights", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	want := "City: /home/user/bright-lights\nAgent: hello-world/polecat-1\n"
	if got != want {
		t.Errorf("renderPrompt(vars) = %q, want %q", got, want)
	}
}

func TestRenderPromptAbsolutePath(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/agents/ada/prompt.template.md"] = []byte("Agent: {{ .AgentName }}\n")
	got := renderPrompt(f, "/city", "", "/city/agents/ada/prompt.template.md", PromptContext{AgentName: "ada"}, "", io.Discard, nil, nil, nil)
	if got != "Agent: ada\n" {
		t.Errorf("renderPrompt(absolute path) = %q, want %q", got, "Agent: ada\n")
	}
}

func TestRenderPromptLegacyTemplateSuffixStillRenders(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Agent: {{ .AgentName }}\n")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{AgentName: "mayor"}, "", io.Discard, nil, nil, nil)
	if got != "Agent: mayor\n" {
		t.Errorf("renderPrompt(legacy suffix) = %q, want %q", got, "Agent: mayor\n")
	}
}

func TestRenderPromptCanonicalSharedTemplateOverridesLegacy(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte(`Hello {{ template "footer" . }}`)
	f.Files["/city/prompts/shared/footer.md.tmpl"] = []byte(`{{ define "footer" }}legacy{{ end }}`)
	f.Files["/city/prompts/shared/footer.template.md"] = []byte(`{{ define "footer" }}canonical{{ end }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Hello canonical" {
		t.Errorf("renderPrompt(canonical shared override) = %q, want %q", got, "Hello canonical")
	}
}

func TestRenderPromptAgentsAliasAppendFragmentsAffectRenderedPrompt(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[agents]
append_fragments = ["footer"]

[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	f := fsys.NewFake()
	f.Files["/city/agents/mayor/prompt.template.md"] = []byte("Hello")
	f.Files["/city/agents/mayor/template-fragments/footer.template.md"] = []byte(`{{ define "footer" }}Goodbye{{ end }}`)
	got := renderPrompt(f, "/city", "", "agents/mayor/prompt.template.md", PromptContext{}, "", io.Discard, nil, cfg.AgentDefaults.AppendFragments, nil)
	if got != "Hello\n\nGoodbye" {
		t.Errorf("renderPrompt(agents alias append_fragments) = %q, want %q", got, "Hello\n\nGoodbye")
	}
}

func TestRenderPromptAgentDefaultsAppendFragmentsAffectRenderedPrompt(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[agent_defaults]
append_fragments = ["footer"]

[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	f := fsys.NewFake()
	f.Files["/city/agents/mayor/prompt.template.md"] = []byte("Hello")
	f.Files["/city/agents/mayor/template-fragments/footer.template.md"] = []byte(`{{ define "footer" }}Goodbye{{ end }}`)
	got := renderPrompt(f, "/city", "", "agents/mayor/prompt.template.md", PromptContext{}, "", io.Discard, nil, cfg.AgentDefaults.AppendFragments, nil)
	if got != "Hello\n\nGoodbye" {
		t.Errorf("renderPrompt(agent_defaults append_fragments) = %q, want %q", got, "Hello\n\nGoodbye")
	}
}

func TestRenderPromptPatchedTemplateSuffixRenders(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/patches/gastown-mayor-prompt.template.md"] = []byte("Hello {{ .AgentName }}")
	got := renderPrompt(f, "/city", "", "patches/gastown-mayor-prompt.template.md", PromptContext{AgentName: "gastown.mayor"}, "", io.Discard, nil, nil, nil)
	if got != "Hello gastown.mayor" {
		t.Errorf("renderPrompt(patched template suffix) = %q, want %q", got, "Hello gastown.mayor")
	}
}

func TestRenderPromptPatchedPlainMarkdownStaysInert(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/patches/gastown-mayor-prompt.md"] = []byte("Hello {{ .AgentName }}")
	f.Files["/city/patches/template-fragments/footer.template.md"] = []byte(`{{ define "footer" }}Goodbye{{ end }}`)
	got := renderPrompt(f, "/city", "", "patches/gastown-mayor-prompt.md", PromptContext{AgentName: "gastown.mayor"}, "", io.Discard, nil, []string{"footer"}, nil)
	if got != "Hello {{ .AgentName }}" {
		t.Errorf("renderPrompt(patched plain markdown) = %q, want raw markdown", got)
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
Binding: {{ .BindingName }} {{ .BindingPrefix }}
`
	f.Files["/city/prompts/full.md.tmpl"] = []byte(tmpl)
	ctx := PromptContext{
		CityRoot:      "/home/user/city",
		AgentName:     "myrig/polecat-1",
		TemplateName:  "polecat",
		BindingName:   "gastown",
		BindingPrefix: "gastown.",
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
	if !strings.Contains(got, "Binding: gastown gastown.") {
		t.Errorf("missing binding namespace: %q", got)
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
		BindingName:   "dep",
		BindingPrefix: "dep.",
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
	if data["BindingName"] != "dep" {
		t.Errorf("BindingName = %q, want %q", data["BindingName"], "dep")
	}
	if data["BindingPrefix"] != "dep." {
		t.Errorf("BindingPrefix = %q, want %q", data["BindingPrefix"], "dep.")
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

func TestDefaultBranchFor_PreservesSlashesInBranchName(t *testing.T) {
	// Regression test for #719: defaultBranchFor must preserve slashes in
	// the default branch name. Previously strings.LastIndex(ref, "/") in
	// DefaultBranch() truncated "refs/remotes/origin/team/feature/x" to "x",
	// leaking the wrong branch name into PromptContext.DefaultBranch and
	// the direct cmd_sling / cmd_prime consumers.
	repoDir := newRepoWithOriginHead(t, "team/feature/x")
	got := defaultBranchFor(repoDir)
	if got != "team/feature/x" {
		t.Errorf("defaultBranchFor(repo with slashy default) = %q, want %q", got, "team/feature/x")
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
	f.Files["/city/prompts/shared/greeting.template.md"] = []byte(
		`{{ define "greeting" }}Hello, {{ .AgentName }}!{{ end }}`)
	// Main template uses it.
	f.Files["/city/prompts/test.template.md"] = []byte(
		`# Prompt\n{{ template "greeting" . }}`)
	ctx := PromptContext{AgentName: "mayor"}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
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
	// A .md file (not .template.md or legacy .md.tmpl) should be ignored.
	f.Files["/city/prompts/shared/readme.md"] = []byte(`{{ define "oops" }}should not load{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Plain text.")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Plain text." {
		t.Errorf("renderPrompt(non-template) = %q, want plain text", got)
	}
}

func TestRenderPromptSharedCanonicalOverridesLegacy(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}legacy{{ end }}`)
	f.Files["/city/prompts/shared/info.template.md"] = []byte(
		`{{ define "info" }}canonical{{ end }}`)
	f.Files["/city/prompts/test.template.md"] = []byte(`{{ template "info" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "canonical" {
		t.Errorf("canonical shared template = %q, want %q", got, "canonical")
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

func TestRenderPromptMaintenanceDogPromptHasRequiredSharedTemplates(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("filepath.Abs(repo root): %v", err)
	}
	maintenanceDir := filepath.Join(repoRoot, "examples", "gastown", "packs", "maintenance")
	promptPath := filepath.Join(maintenanceDir, "agents", "dog", "prompt.template.md")

	raw, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("os.ReadFile(maintenance dog prompt): %v", err)
	}

	var stderr strings.Builder
	got := renderPrompt(fsys.OSFS{}, "/tmp/city", "", promptPath, PromptContext{
		CityRoot:  "/tmp/city",
		AgentName: "dog",
		WorkQuery: "bd ready",
	}, "", &stderr, []string{maintenanceDir}, nil, nil)

	if strings.Contains(stderr.String(), "template not defined") {
		t.Fatalf("renderPrompt emitted missing-template warning: %s", stderr.String())
	}
	if got == string(raw) {
		t.Fatalf("renderPrompt fell back to raw prompt; expected rendered maintenance prompt")
	}
	if !strings.Contains(got, "Gas City Maintenance Context") {
		t.Fatalf("rendered prompt missing maintenance architecture context:\n%s", got)
	}
	if !strings.Contains(got, "Following Your Formula") {
		t.Fatalf("rendered prompt missing following-mol fragment:\n%s", got)
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
		{"dedup preserves first occurrence", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}, false},
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

func TestEffectivePromptFragments(t *testing.T) {
	got := effectivePromptFragments(
		[]string{"global"},
		[]string{"inject"},
		[]string{"append"},
		[]string{"inherited"},
		[]string{"default"},
	)
	want := []string{"global", "inject", "append", "inherited", "default"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if effectivePromptFragments(nil, nil, nil, nil, nil) != nil {
		t.Fatal("effectivePromptFragments(nil, nil, nil, nil, nil) = non-nil, want nil")
	}
}

func TestEffectivePromptFragmentsDedupsAcrossLayers(t *testing.T) {
	got := effectivePromptFragments(
		[]string{"shared"},
		[]string{"inject"},
		[]string{"inject", "agent"},
		[]string{"shared", "pack"},
		[]string{"pack", "city"},
	)
	want := []string{"shared", "inject", "agent", "pack", "city"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
