package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Repro for ga-5wwp1u / ga-04knc7: renderPromptWithMeta silently returned
// raw, unrendered template text on parse/execute error, giving callers no
// way to distinguish a broken template from a legitimately empty or
// successfully-rendered one. gc prime exited 0 with literal `{{ template
// ... }}` syntax on stdout, and resolveTemplate shipped the same raw text
// as a spawned agent's live startup prompt.

func TestRenderPromptWithMetaParseErrorSetsErr(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/bad.template.md"] = []byte("Bad: {{ .Unclosed")
	var stderr strings.Builder
	res := renderPromptWithMeta(f, "/city", "", "prompts/bad.template.md", PromptContext{}, "", &stderr, nil, nil, nil)
	if res.Err == nil {
		t.Fatal("renderPromptWithMeta(parse error).Err = nil, want non-nil")
	}
	if res.Text != "Bad: {{ .Unclosed" {
		t.Errorf("res.Text = %q, want raw body preserved", res.Text)
	}
	if !strings.Contains(stderr.String(), "prompt template") {
		t.Errorf("stderr = %q, want warning about prompt template", stderr.String())
	}
}

func TestRenderPromptWithMetaExecuteErrorSetsErr(t *testing.T) {
	f := fsys.NewFake()
	body := `Hello {{ template "propulsion-dog" . }}`
	f.Files["/city/prompts/dog.template.md"] = []byte(body)
	var stderr strings.Builder
	res := renderPromptWithMeta(f, "/city", "", "prompts/dog.template.md", PromptContext{}, "", &stderr, nil, nil, nil)
	if res.Err == nil {
		t.Fatal("renderPromptWithMeta(undefined fragment).Err = nil, want non-nil")
	}
	if !strings.Contains(res.Err.Error(), "not defined") {
		t.Errorf("res.Err = %v, want to mention %q", res.Err, "not defined")
	}
	if res.Text != body {
		t.Errorf("res.Text = %q, want raw body preserved", res.Text)
	}
	if !strings.Contains(stderr.String(), "not defined") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "not defined")
	}
}

// TestResolveTemplatePropagatesPromptRenderError covers fix-spec item 3: the
// higher-severity path, since resolveTemplate's result becomes a spawned
// agent's live startup prompt, not just a diagnostic print.
func TestResolveTemplatePropagatesPromptRenderError(t *testing.T) {
	cityPath := t.TempDir()
	fs := fsys.NewFake()
	fs.Files[cityPath+"/prompts/dog.template.md"] = []byte(`Hello {{ template "propulsion-dog" . }}`)

	params := &agentBuildParams{
		fs:              fs,
		cityName:        "bright-lights",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "bright-lights", Provider: "opencode"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/opencode", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
	}
	agent := &config.Agent{
		Name:           "dog",
		PromptTemplate: "prompts/dog.template.md",
		Provider:       "opencode",
	}

	_, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err == nil {
		t.Fatal("resolveTemplate(undefined fragment) = nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "not defined") {
		t.Errorf("err = %v, want to mention %q", err, "not defined")
	}
}

// TestDoPrimeStrictUndefinedFragmentFails covers fix-spec item 2: gc prime
// --strict must fail loudly on a readable-but-unrenderable prompt_template
// instead of shipping raw template syntax to stdout with exit 0. The
// existing strict precondition loop only checks os.ReadFile succeeds, not
// that the template actually renders — this is the gap ga-04knc7 hit live.
func TestDoPrimeStrictUndefinedFragmentFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "dog.template.md"), []byte(`Hello {{ template "propulsion-dog" . }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "dog"
prompt_template = "prompts/dog.template.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_CITY_PATH", dir)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"dog"}, &stdout, &stderr, false, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, undefined fragment) = 0, want non-zero; stdout: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "{{ template") {
		t.Errorf("stdout = %q, must not contain raw template syntax", stdout.String())
	}
}

// TestDoPrimeNonStrictUndefinedFragmentFallsBackToDefault covers the
// non-strict done-when item: gc prime <agent> (no --strict) on the same
// unrenderable template must not write raw {{ ... }} template syntax to
// stdout either. It should succeed (falling back is the pre-existing,
// intended behavior for an unusable render) and emit the default/builtin
// prompt instead of the broken template's raw body.
func TestDoPrimeNonStrictUndefinedFragmentFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "dog.template.md"), []byte(`Hello {{ template "propulsion-dog" . }}`), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "dog"
prompt_template = "prompts/dog.template.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_CITY_PATH", dir)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"dog"}, &stdout, &stderr, false, false)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict=false, undefined fragment) = %d, want 0 (fallback is the intended non-strict behavior); stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "{{ template") {
		t.Errorf("stdout = %q, must not contain raw template syntax", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Gas City Agent") {
		t.Errorf("stdout = %q, want default/builtin fallback prompt", stdout.String())
	}
}
