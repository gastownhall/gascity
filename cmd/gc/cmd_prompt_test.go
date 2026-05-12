package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// fakeSynthRunner returns a canned body and records its inputs so tests can
// assert on what was passed to the provider.
type fakeSynthRunner struct {
	body         string
	err          error
	gotProvider  *config.ResolvedProvider
	gotPrompt    string
	gotWorkDir   string
	gotCalled    bool
}

func (r *fakeSynthRunner) run(ctx context.Context, provider *config.ResolvedProvider, prompt, workDir string) (string, error) {
	r.gotCalled = true
	r.gotProvider = provider
	r.gotPrompt = prompt
	r.gotWorkDir = workDir
	if r.err != nil {
		return "", r.err
	}
	return r.body, nil
}

// writeMinimalCity creates a city.toml that ResolveProvider can chew on
// (workspace.provider = "claude" — uses the builtin spec).
func writeMinimalCity(t *testing.T, providerKey string) string {
	t.Helper()
	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"test-city\"\n"
	if providerKey != "" {
		toml += "provider = \"" + providerKey + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return cityDir
}

func TestRenderMetaPromptSubstitutesBracketDelimsAndPreservesGoTemplateSyntax(t *testing.T) {
	source := `Role: [[ .Role ]]
Provider: [[ .ProviderDisplayName ]] ([[ .ProviderKey ]])
Project: [[ .ProjectName ]] at [[ .ProjectPath ]]
City: [[ .CityRoot ]]

The agent template should reference {{ .CityRoot }} and use
{{ templateFirst . "x" "default" }} verbatim.`

	got, err := renderMetaPrompt(source, metaPromptCtx{
		Role:                "mayor",
		ProviderKey:         "claude",
		ProviderDisplayName: "Claude Code",
		ProjectPath:         "/tmp/foo",
		ProjectName:         "foo",
		CityRoot:            "/tmp/city",
	})
	if err != nil {
		t.Fatalf("renderMetaPrompt: %v", err)
	}
	wantSubs := []string{
		"Role: mayor",
		"Provider: Claude Code (claude)",
		"Project: foo at /tmp/foo",
		"City: /tmp/city",
		"reference {{ .CityRoot }} and use",
		`{{ templateFirst . "x" "default" }} verbatim.`,
	}
	for _, want := range wantSubs {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestEmbeddedMetaAgentAuthorPromptParsesAndRenders(t *testing.T) {
	got, err := renderMetaPrompt(string(metaAgentAuthorPrompt), metaPromptCtx{
		Role:                "mayor",
		ProviderKey:         "claude",
		ProviderDisplayName: "Claude Code",
		ProjectPath:         "/Users/test/foo",
		ProjectName:         "foo",
		CityRoot:            "/Users/test/foo/.gc",
	})
	if err != nil {
		t.Fatalf("embedded meta-prompt failed to render: %v", err)
	}
	// Smoke-checks: substitutions happened, Go-template literals survived.
	if !strings.Contains(got, "mayor") {
		t.Errorf("expected role 'mayor' in rendered meta-prompt")
	}
	if !strings.Contains(got, "Claude Code") {
		t.Errorf("expected display name 'Claude Code' in rendered meta-prompt")
	}
	if !strings.Contains(got, "{{ templateFirst") {
		t.Errorf("expected literal Go-template syntax {{ templateFirst to survive rendering")
	}
	if !strings.Contains(got, "{{ .CityRoot }}") {
		t.Errorf("expected literal {{ .CityRoot }} to survive rendering")
	}
}

func TestRunPromptSynthRequiresProviderEither_FromFlagOrWorkspace(t *testing.T) {
	cityDir := writeMinimalCity(t, "") // no workspace.provider
	runner := &fakeSynthRunner{body: "ignored"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role: "mayor",
		city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected error when no provider configured; stdout=%q", stdout.String())
	}
	if !strings.Contains(err.Error(), "no provider") {
		t.Errorf("error = %v, want it to mention missing provider", err)
	}
	if runner.gotCalled {
		t.Errorf("runner should not have been called when provider resolution fails")
	}
}

func TestRunPromptSynthHonorsExplicitProviderFlag(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "# Codex Mayor\n\nbody."}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role:     "mayor",
		provider: "codex",
		project:  "/tmp/proj",
		city:     cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	if !runner.gotCalled {
		t.Fatalf("runner not called")
	}
	if runner.gotProvider == nil || runner.gotProvider.Name != "codex" {
		t.Errorf("provider passed to runner = %+v, want name=codex", runner.gotProvider)
	}
	if !strings.Contains(stdout.String(), "Codex Mayor") {
		t.Errorf("stdout should contain runner's body, got %q", stdout.String())
	}
}

func TestRunPromptSynthAutoDetectsProjectFromCwd(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	projDir := t.TempDir()
	if err := os.Chdir(projDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err = runPromptSynth(context.Background(), promptSynthOpts{
		role: "mayor",
		city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	// The meta-prompt should include the resolved project name (basename
	// of the cwd) and the resolved project path.
	wantBase := filepath.Base(projDir)
	if !strings.Contains(runner.gotPrompt, wantBase) {
		t.Errorf("rendered meta-prompt should mention project basename %q\n--- got prompt ---\n%s",
			wantBase, runner.gotPrompt)
	}
	// Resolve symlinks (macOS /tmp -> /private/tmp) before comparing.
	gotWD, _ := filepath.EvalSymlinks(runner.gotWorkDir)
	wantWD, _ := filepath.EvalSymlinks(projDir)
	if gotWD != wantWD {
		t.Errorf("workDir passed to runner = %q, want %q", runner.gotWorkDir, projDir)
	}
}

func TestRunPromptSynthExplicitProjectNameOverridesBasename(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role:        "mayor",
		project:     "/some/path",
		projectName: "MyCustomName",
		city:        cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v", err)
	}
	if !strings.Contains(runner.gotPrompt, "MyCustomName") {
		t.Errorf("rendered meta-prompt should use explicit --project-name 'MyCustomName'\n--- got ---\n%s", runner.gotPrompt)
	}
}

func TestRunPromptSynthRejectsEmptyProviderOutput(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "   \n  "} // whitespace only
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", city: cityDir}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Errorf("expected empty-output error, got %v", err)
	}
}

func TestRunPromptSynthSurfacesRunnerError(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{err: errors.New("boom")}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", city: cityDir}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected runner error to surface, got %v", err)
	}
}

func TestRunPromptSynthWriteCreatesFileWithHeader(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "# Mayor Context\n\nThe mayor coordinates."}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role:    "mayor",
		project: "/tmp/proj",
		write:   true,
		city:    cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	dst := filepath.Join(cityDir, "agents", "mayor", "prompt.template.md")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	got := string(data)
	wantSubs := []string{
		"Generated by `gc prompt synth`",
		"role:     mayor",
		"provider: claude (Claude Code)",
		"# Mayor Context",
		"LLM-generated content. Review carefully",
	}
	for _, want := range wantSubs {
		if !strings.Contains(got, want) {
			t.Errorf("written file missing %q\n--- got ---\n%s", want, got)
		}
	}
	if !strings.Contains(stderr.String(), "wrote ") {
		t.Errorf("stderr should mention what was written, got %q", stderr.String())
	}
}

func TestRunPromptSynthWriteRefusesToClobberWithoutForce(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	dst := filepath.Join(cityDir, "agents", "mayor", "prompt.template.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	runner := &fakeSynthRunner{body: "REPLACEMENT"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role: "mayor", write: true, city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("expected refuse-to-clobber error, got %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "ORIGINAL" {
		t.Errorf("file should be unchanged, got %q", got)
	}
}

func TestRunPromptSynthWriteForceOverwrites(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	dst := filepath.Join(cityDir, "agents", "mayor", "prompt.template.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	runner := &fakeSynthRunner{body: "# New Mayor"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role: "mayor", write: true, force: true, city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	got, _ := os.ReadFile(dst)
	if !strings.Contains(string(got), "# New Mayor") {
		t.Errorf("file not overwritten, got %q", got)
	}
	if strings.Contains(string(got), "ORIGINAL") {
		t.Errorf("original content should be gone, got %q", got)
	}
}

func TestRunPromptSynthMetaPromptOverrideUsesExternalFile(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	tmp := t.TempDir()
	overridePath := filepath.Join(tmp, "custom-meta.md")
	if err := os.WriteFile(overridePath, []byte("CUSTOM META role=[[ .Role ]]"), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role:               "mayor",
		city:               cityDir,
		metaPromptOverride: overridePath,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v", err)
	}
	if runner.gotPrompt != "CUSTOM META role=mayor" {
		t.Errorf("override not used; runner got %q", runner.gotPrompt)
	}
}

func TestRunPromptSynthRejectsSlinguedModeUntilImplemented(t *testing.T) {
	// --writer-agent=<name> is the slingued-mode flag (planned for the
	// next PR). Until that lands, it must fail loudly rather than
	// silently fall back to direct mode — silent fallback would mask
	// user intent and surprise scripts that explicitly target an agent.
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "should not be called"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role:        "polecat",
		writerAgent: "mayor",
		city:        cityDir,
	}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("expected slingued-mode-not-implemented error, got %v", err)
	}
	if runner.gotCalled {
		t.Errorf("runner must not be called in slingued mode")
	}
}

func TestRunPromptSynthEmptyWriterAgentTakesDirectPath(t *testing.T) {
	// --writer-agent="" (the default and only currently-supported value)
	// must still trigger the direct-mode happy path.
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role:        "mayor",
		writerAgent: "", // explicit empty
		city:        cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("direct mode (writer-agent='') should succeed: %v", err)
	}
	if !runner.gotCalled {
		t.Errorf("direct-mode runner should be called")
	}
}

func TestRunPromptSynthRejectsProviderWithoutPrintArgs(t *testing.T) {
	cityDir := t.TempDir()
	// Custom provider without print_args — one-shot mode unsupported.
	tomlBody := `[workspace]
name = "test-city"
provider = "noprint"

[providers.noprint]
command = "echo"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(tomlBody), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	runner := &fakeSynthRunner{body: "should not be called"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role: "mayor", city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Errorf("expected one-shot/print_args error, got %v", err)
	}
	if runner.gotCalled {
		t.Errorf("runner should not be called when provider lacks print_args")
	}
}
