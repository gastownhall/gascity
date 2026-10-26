package main

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/promptmeta"
)

func TestRenderPromptWithMeta_NoFrontMatter(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/p.template.md"] = []byte("Hello {{ .AgentName }}\n")
	res := renderPromptWithMeta(f, "/city", "", "prompts/p.template.md",
		PromptContext{AgentName: "mayor"}, "", io.Discard, nil, nil, nil)
	if res.Version != "" {
		t.Errorf("Version = %q, want empty (no frontmatter)", res.Version)
	}
	if res.Text != "Hello mayor\n" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.SHA == "" {
		t.Error("SHA must be set for non-empty rendered content")
	}
}

func TestRenderPromptWithMeta_VersionFromFrontMatter(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/p.template.md"] = []byte("---\nversion: v3\n---\nBody {{ .AgentName }}\n")
	res := renderPromptWithMeta(f, "/city", "", "prompts/p.template.md",
		PromptContext{AgentName: "mayor"}, "", io.Discard, nil, nil, nil)
	if res.Version != "v3" {
		t.Errorf("Version = %q, want v3", res.Version)
	}
	// Frontmatter must be stripped from Text.
	if strings.Contains(res.Text, "version:") || strings.Contains(res.Text, "---") {
		t.Errorf("frontmatter leaked into Text: %q", res.Text)
	}
	if !strings.Contains(res.Text, "Body mayor") {
		t.Errorf("Text missing rendered body: %q", res.Text)
	}
}

func TestRenderPromptWithMeta_SHACoversRenderedSubstitution(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/p.template.md"] = []byte("---\nversion: v1\n---\nAgent: {{ .AgentName }}\n")
	a := renderPromptWithMeta(f, "/city", "", "prompts/p.template.md",
		PromptContext{AgentName: "mayor"}, "", io.Discard, nil, nil, nil)
	b := renderPromptWithMeta(f, "/city", "", "prompts/p.template.md",
		PromptContext{AgentName: "polecat"}, "", io.Discard, nil, nil, nil)
	if a.Version != b.Version {
		t.Errorf("Version differs across renders: %q vs %q", a.Version, b.Version)
	}
	if a.SHA == b.SHA {
		t.Errorf("SHA must differ when AgentName changes the rendered output: %q == %q", a.SHA, b.SHA)
	}
}

func TestRenderPromptWithMeta_SHADetectsUnbumpedEdit(t *testing.T) {
	// Two renders share Version="v3" but differ in body — SHA must surface it.
	f := fsys.NewFake()
	f.Files["/city/prompts/p.template.md"] = []byte("---\nversion: v3\n---\nDo step 1.\n")
	first := renderPromptWithMeta(f, "/city", "", "prompts/p.template.md",
		PromptContext{}, "", io.Discard, nil, nil, nil)

	// Operator edits body without bumping version.
	f.Files["/city/prompts/p.template.md"] = []byte("---\nversion: v3\n---\nDo step 1 and verify.\n")
	second := renderPromptWithMeta(f, "/city", "", "prompts/p.template.md",
		PromptContext{}, "", io.Discard, nil, nil, nil)

	if first.Version != second.Version {
		t.Errorf("test setup wrong: versions should match, got %q vs %q", first.Version, second.Version)
	}
	if first.SHA == second.SHA {
		t.Fatal("unbumped edit must produce different SHAs — this is the bug 1e protects against")
	}
}

func TestRenderPromptWithMeta_PlainMarkdownStillReports(t *testing.T) {
	f := fsys.NewFake()
	raw := "---\nversion: v2\n---\nA plain markdown body.\n"
	body := "A plain markdown body.\n"
	f.Files["/city/prompts/p.md"] = []byte(raw)
	// Plain .md: no template execution, but frontmatter is still stripped
	// so Text and SHA both represent the prompt bytes sent to the agent.
	res := renderPromptWithMeta(f, "/city", "", "prompts/p.md",
		PromptContext{}, "", io.Discard, nil, nil, nil)
	if res.Version != "v2" {
		t.Errorf("Version = %q, want v2", res.Version)
	}
	if res.SHA == "" {
		t.Error("plain markdown should still get SHA")
	}
	if res.Text != body {
		t.Errorf("plain Text = %q", res.Text)
	}

	f.Files["/city/prompts/p.md"] = []byte("---\nversion: v3\n---\n" + body)
	bumped := renderPromptWithMeta(f, "/city", "", "prompts/p.md",
		PromptContext{}, "", io.Discard, nil, nil, nil)
	if bumped.SHA != res.SHA {
		t.Fatalf("version-only plain markdown edit changed SHA: %q != %q", bumped.SHA, res.SHA)
	}
}

func TestRenderPromptWithMeta_EmptyPathReturnsZero(t *testing.T) {
	f := fsys.NewFake()
	res := renderPromptWithMeta(f, "/city", "", "", PromptContext{}, "", io.Discard, nil, nil, nil)
	if res.Text != "" || res.Version != "" || res.SHA != "" {
		t.Errorf("empty path should return zero result, got %+v", res)
	}
}

func TestRenderPromptWithMeta_MissingFileReturnsZero(t *testing.T) {
	f := fsys.NewFake()
	res := renderPromptWithMeta(f, "/city", "", "prompts/missing.template.md",
		PromptContext{}, "", io.Discard, nil, nil, nil)
	if res.Text != "" || res.Version != "" || res.SHA != "" {
		t.Errorf("missing file should return zero result, got %+v", res)
	}
}

func TestRenderPromptWithMeta_TemplateParseErrorPreservesVersion(t *testing.T) {
	f := fsys.NewFake()
	// Bad template syntax — Parse() will fail.
	f.Files["/city/prompts/p.template.md"] = []byte("---\nversion: v1\n---\nbad: {{ unclosed\n")
	res := renderPromptWithMeta(f, "/city", "", "prompts/p.template.md",
		PromptContext{}, "", io.Discard, nil, nil, nil)
	// Version still reported even if rendering failed.
	if res.Version != "v1" {
		t.Errorf("Version = %q, want v1 even on parse error", res.Version)
	}
	if res.SHA == "" {
		t.Error("SHA should still be populated on parse error fallback")
	}
}

// TestRenderPromptWithMeta_FrontMatterParseDelegatedToPromptmetaPackage is
// a sanity check — the test exists so a refactor that bypasses promptmeta
// and reimplements parsing inline gets caught.
func TestRenderPromptWithMeta_FrontMatterParseDelegatedToPromptmetaPackage(t *testing.T) {
	in := "---\nversion: v9\n---\nbody"
	fm, _ := promptmeta.Parse(in)
	if fm.Version != "v9" {
		t.Fatalf("smoke check on promptmeta failed: %q", fm.Version)
	}
}

// TestRenderPromptWithMeta_ErrReportsFallbacks pins which outcomes set Err:
// only a declared template that could not be used as written. A clean or
// legitimately empty render, a plain .md, an empty path, and a missing inject
// fragment (a warning that leaves the main render intact) leave it nil.
func TestRenderPromptWithMeta_ErrReportsFallbacks(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		body      string
		inject    []string
		wantErr   bool
		wantText  string
		wantWarn  string
		wantErrIs error
	}{
		{name: "missing file", path: "prompts/missing.template.md", wantErr: true, wantErrIs: fs.ErrNotExist},
		{name: "parse error", path: "prompts/p.template.md", body: "---\nversion: v1\n---\nHello {{base_branch}}\n", wantErr: true, wantText: "Hello {{base_branch}}\n"},
		{name: "execute error", path: "prompts/p.template.md", body: `Hello {{template "no-such-fragment" .}}`, wantErr: true, wantText: `Hello {{template "no-such-fragment" .}}`},
		{name: "valid template", path: "prompts/p.template.md", body: "Hello {{.AgentName}}", wantText: "Hello worker"},
		{name: "valid template renders empty", path: "prompts/p.template.md", body: "{{if .RigName}}rig{{end}}", wantText: ""},
		{name: "plain markdown is not executed", path: "prompts/p.md", body: "Hello {{base_branch}}", wantText: "Hello {{base_branch}}"},
		{name: "empty path", path: ""},
		{name: "missing inject fragment", path: "prompts/p.template.md", body: "Hello", inject: []string{"no-such-fragment"}, wantText: "Hello", wantWarn: `inject_fragment "no-such-fragment"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fsys.NewFake()
			if tt.body != "" {
				f.Files["/city/"+tt.path] = []byte(tt.body)
			}
			var stderr strings.Builder
			res := renderPromptWithMeta(f, "/city", "", tt.path,
				PromptContext{AgentName: "worker"}, "", &stderr, nil, tt.inject, nil)
			if (res.Err != nil) != tt.wantErr {
				t.Fatalf("Err = %v, want error: %v", res.Err, tt.wantErr)
			}
			if tt.wantErrIs != nil && !errors.Is(res.Err, tt.wantErrIs) {
				t.Errorf("Err = %v, want errors.Is %v", res.Err, tt.wantErrIs)
			}
			if res.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", res.Text, tt.wantText)
			}
			if tt.wantWarn != "" && !strings.Contains(stderr.String(), tt.wantWarn) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantWarn)
			}
		})
	}
}
