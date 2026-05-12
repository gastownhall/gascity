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

// fakeSynthRunner returns a canned body and records its inputs so tests
// can assert on what was passed to the provider.
type fakeSynthRunner struct {
	body        string
	err         error
	gotProvider *config.ResolvedProvider
	gotPrompt   string
	gotWorkDir  string
	gotCalled   bool
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

// writeMinimalCity creates a city.toml that ResolveProvider can chew on.
// When rigs is non-empty, an [[rigs]] entry is emitted for each.
func writeMinimalCity(t *testing.T, providerKey string, rigs ...config.Rig) string {
	t.Helper()
	cityDir := t.TempDir()
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"test-city\"\n")
	if providerKey != "" {
		b.WriteString("provider = \"" + providerKey + "\"\n")
	}
	for _, r := range rigs {
		b.WriteString("\n[[rigs]]\n")
		b.WriteString("name = \"" + r.Name + "\"\n")
		if r.Path != "" {
			b.WriteString("path = \"" + r.Path + "\"\n")
		}
		if r.DefaultBranch != "" {
			b.WriteString("default_branch = \"" + r.DefaultBranch + "\"\n")
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return cityDir
}

// --- meta-prompt rendering ---

func TestRenderMetaPromptSubstitutesBracketDelimsAndPreservesGoTemplateSyntax(t *testing.T) {
	source := `Role: [[ .Role ]]
Provider: [[ .ProviderDisplayName ]] ([[ .ProviderKey ]])
Context: [[ .ContextType ]]
City: [[ .CityName ]] @ [[ .CityPath ]]

The agent template should reference {{ .CityRoot }} and use
{{ templateFirst . "x" "default" }} verbatim.`

	got, err := renderMetaPrompt(source, metaPromptCtx{
		Role:                "mayor",
		ProviderKey:         "claude",
		ProviderDisplayName: "Claude Code",
		ContextType:         "city",
		CityName:            "test-city",
		CityPath:            "/tmp/city",
	})
	if err != nil {
		t.Fatalf("renderMetaPrompt: %v", err)
	}
	wantSubs := []string{
		"Role: mayor",
		"Provider: Claude Code (claude)",
		"Context: city",
		"City: test-city @ /tmp/city",
		"reference {{ .CityRoot }} and use",
		`{{ templateFirst . "x" "default" }} verbatim.`,
	}
	for _, want := range wantSubs {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestEmbeddedMetaAgentAuthorPromptRendersCityContext(t *testing.T) {
	got, err := renderMetaPrompt(string(metaAgentAuthorPrompt), metaPromptCtx{
		Role:                "mayor",
		ProviderKey:         "claude",
		ProviderDisplayName: "Claude Code",
		ContextType:         "city",
		CityName:            "test-city",
		CityPath:            "/tmp/city",
	})
	if err != nil {
		t.Fatalf("embedded meta-prompt failed to render (city context): %v", err)
	}
	// City-branch content should appear; rig-branch should not.
	if !strings.Contains(got, "HQ-only") {
		t.Errorf("city-context branch should mention 'HQ-only'\n--- got ---\n%s", got)
	}
	if strings.Contains(got, "Rig path:") {
		t.Errorf("city-context render should not include rig-branch content")
	}
	if !strings.Contains(got, "{{ .CityRoot }}") {
		t.Errorf("literal {{ .CityRoot }} should survive [[ ]] templating")
	}
	if !strings.Contains(got, "{{ templateFirst") {
		t.Errorf("literal {{ templateFirst should survive [[ ]] templating")
	}
}

func TestEmbeddedMetaAgentAuthorPromptRendersRigContext(t *testing.T) {
	got, err := renderMetaPrompt(string(metaAgentAuthorPrompt), metaPromptCtx{
		Role:                "polecat",
		ProviderKey:         "codex",
		ProviderDisplayName: "Codex CLI",
		ContextType:         "rig",
		CityName:            "city-x",
		CityPath:            "/tmp/city-x",
		RigName:             "myrepo",
		RigPath:             "/Users/test/myrepo",
		RigDefaultBranch:    "main",
	})
	if err != nil {
		t.Fatalf("embedded meta-prompt failed to render (rig context): %v", err)
	}
	if !strings.Contains(got, "myrepo") {
		t.Errorf("rig-context render should mention rig name 'myrepo'")
	}
	if !strings.Contains(got, "/Users/test/myrepo") {
		t.Errorf("rig-context render should mention rig path")
	}
	if !strings.Contains(got, "Default branch:  main") {
		t.Errorf("rig-context render should mention default branch")
	}
	if strings.Contains(got, "HQ-only") {
		t.Errorf("rig-context render should NOT include city-branch HQ-only content")
	}
}

// --- provider resolution ---

func TestRunPromptSynthRequiresProviderEither_FromFlagOrWorkspace(t *testing.T) {
	cityDir := writeMinimalCity(t, "")
	runner := &fakeSynthRunner{body: "ignored"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", city: cityDir}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Errorf("expected missing-provider error, got %v", err)
	}
	if runner.gotCalled {
		t.Errorf("runner should not be called when provider resolution fails")
	}
}

func TestRunPromptSynthHonorsExplicitProviderFlag(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "# Codex Mayor\n\nbody."}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role:     "mayor",
		provider: "codex",
		city:     cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	if runner.gotProvider == nil || runner.gotProvider.Name != "codex" {
		t.Errorf("provider passed to runner = %+v, want name=codex", runner.gotProvider)
	}
	if !strings.Contains(stdout.String(), "Codex Mayor") {
		t.Errorf("stdout should contain runner's body, got %q", stdout.String())
	}
}

func TestRunPromptSynthRejectsProviderWithoutPrintArgs(t *testing.T) {
	cityDir := t.TempDir()
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
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", city: cityDir}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Errorf("expected one-shot/print_args error, got %v", err)
	}
	if runner.gotCalled {
		t.Errorf("runner should not be called when provider lacks print_args")
	}
}

// --- context type (rig vs city) ---

func TestRunPromptSynthDefaultsToCityContext(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", city: cityDir}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v", err)
	}
	// City-branch content should appear in the rendered meta-prompt.
	if !strings.Contains(runner.gotPrompt, "Context type: city") {
		t.Errorf("default context should be 'city', meta-prompt missing marker\n--- got ---\n%s", runner.gotPrompt)
	}
	if strings.Contains(runner.gotPrompt, "Rig path:") {
		t.Errorf("city-context meta-prompt should not include rig-branch content")
	}
	// Working directory should be the city path (city context).
	cityResolved, _ := filepath.EvalSymlinks(cityDir)
	gotWD, _ := filepath.EvalSymlinks(runner.gotWorkDir)
	if gotWD != cityResolved {
		t.Errorf("workDir = %q, want city path %q", gotWD, cityResolved)
	}
}

func TestRunPromptSynthRigContextLooksUpRigInCityToml(t *testing.T) {
	rigPath := t.TempDir()
	cityDir := writeMinimalCity(t, "claude", config.Rig{
		Name:          "myproj",
		Path:          rigPath,
		DefaultBranch: "develop",
	})
	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role: "polecat",
		rig:  "myproj",
		city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	wantSubs := []string{
		"Context type: rig",
		"myproj",
		rigPath,
		"Default branch:  develop",
	}
	for _, want := range wantSubs {
		if !strings.Contains(runner.gotPrompt, want) {
			t.Errorf("rig-context meta-prompt missing %q\n--- got ---\n%s", want, runner.gotPrompt)
		}
	}
	// Working directory should be the rig path (rig context).
	rigResolved, _ := filepath.EvalSymlinks(rigPath)
	gotWD, _ := filepath.EvalSymlinks(runner.gotWorkDir)
	if gotWD != rigResolved {
		t.Errorf("workDir = %q, want rig path %q", gotWD, rigResolved)
	}
}

func TestRunPromptSynthRigContextRejectsUnknownRig(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude", config.Rig{Name: "real-rig", Path: "/tmp/real"})
	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role: "polecat",
		rig:  "nonexistent",
		city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected rig-not-found error, got %v", err)
	}
	if !strings.Contains(err.Error(), "real-rig") {
		t.Errorf("error should list known rigs, got %v", err)
	}
	if runner.gotCalled {
		t.Errorf("runner should not be called when rig lookup fails")
	}
}

// --- baseline loading ---

func TestLoadBaselinePromptUserCustomizationWins(t *testing.T) {
	cityDir := t.TempDir()
	userDir := filepath.Join(cityDir, "agents", "polecat")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "prompt.template.md"), []byte("USER VERSION"), 0o644); err != nil {
		t.Fatalf("write user prompt: %v", err)
	}
	// Pack default would also exist — should still lose to user customization.
	packDir := filepath.Join(cityDir, ".gc", "system", "packs", "core", "agents", "polecat")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir pack: %v", err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "prompt.template.md"), []byte("PACK VERSION"), 0o644); err != nil {
		t.Fatalf("write pack prompt: %v", err)
	}

	body, source, own := loadBaselinePrompt(cityDir, "polecat")
	if body != "USER VERSION" {
		t.Errorf("user customization should win, got %q", body)
	}
	if !strings.Contains(source, "city customization") {
		t.Errorf("source should describe user customization, got %q", source)
	}
	if !own {
		t.Errorf("user customization is role-specific, expected own=true")
	}
}

func TestLoadBaselinePromptFallsBackToPackDefault(t *testing.T) {
	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, ".gc", "system", "packs", "gastown", "agents", "witness")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "prompt.template.md"), []byte("PACK VERSION"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	body, source, own := loadBaselinePrompt(cityDir, "witness")
	if body != "PACK VERSION" {
		t.Errorf("pack default should be used, got %q", body)
	}
	if !strings.Contains(source, "pack default") {
		t.Errorf("source should describe pack default, got %q", source)
	}
	if !own {
		t.Errorf("pack default is role-specific, expected own=true")
	}
}

func TestLoadBaselinePromptUsesEmbeddedMayorForKnownRole(t *testing.T) {
	// "mayor" exists as embed; should be returned as own baseline.
	cityDir := t.TempDir() // empty city, no overrides
	body, source, own := loadBaselinePrompt(cityDir, "mayor")
	if body == "" {
		t.Fatalf("embedded mayor.md should be available as baseline")
	}
	if !strings.Contains(source, "embedded prompts/mayor.md") {
		t.Errorf("source should describe embedded mayor.md, got %q", source)
	}
	if !own {
		t.Errorf("mayor baseline for role=mayor is own, expected own=true")
	}
}

func TestLoadBaselinePromptFallsBackToMayorAsStructuralReference(t *testing.T) {
	// Unknown role with no overrides — should fall back to mayor.md as
	// structural reference, marked NOT own.
	cityDir := t.TempDir()
	body, source, own := loadBaselinePrompt(cityDir, "totally-novel-role")
	if body == "" {
		t.Fatalf("expected mayor.md fallback to be present")
	}
	if !strings.Contains(source, "structural reference") {
		t.Errorf("source should mark this as a structural reference, got %q", source)
	}
	if own {
		t.Errorf("mayor.md as fallback for non-mayor role is NOT own, expected own=false")
	}
}

func TestRunPromptSynthFeedsBaselineToMetaPrompt(t *testing.T) {
	// Drop a custom user file to make sure it ends up inside the
	// rendered meta-prompt body (so the LLM sees it as the refinement
	// baseline).
	cityDir := writeMinimalCity(t, "claude")
	userDir := filepath.Join(cityDir, "agents", "mayor")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	marker := "MARKER-USER-BASELINE-12345"
	if err := os.WriteFile(filepath.Join(userDir, "prompt.template.md"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", city: cityDir}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v", err)
	}
	if !strings.Contains(runner.gotPrompt, marker) {
		t.Errorf("meta-prompt should embed the baseline content (marker %q missing)\n--- got ---\n%s",
			marker, runner.gotPrompt)
	}
	if !strings.Contains(runner.gotPrompt, "current prompt template for mayor") {
		t.Errorf("meta-prompt should frame the baseline as 'current template' (own=true case)")
	}
}

func TestRunPromptSynthFramesFallbackBaselineAsStructuralReference(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "totally-novel-role", city: cityDir}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v", err)
	}
	if !strings.Contains(runner.gotPrompt, "structural reference") {
		t.Errorf("meta-prompt should mark fallback as structural reference for novel role")
	}
}

// --- output handling ---

func TestRunPromptSynthRejectsEmptyProviderOutput(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "   \n  "}
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

func TestRunPromptSynthWriteCreatesFileWithHeaderCityContext(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "# Mayor Context\n\nThe mayor coordinates."}
	var stdout, stderr bytes.Buffer
	// Use a novel role so the user-customization path doesn't kick in
	// and loadBaselinePrompt's file writes don't interfere.
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "newrole", write: true, city: cityDir}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	dst := filepath.Join(cityDir, "agents", "newrole", "prompt.template.md")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	got := string(data)
	wantSubs := []string{
		"Generated by `gc prompt synth`",
		"role:     newrole",
		"provider: claude (Claude Code)",
		`context:  city "test-city"`,
		"baseline: embedded prompts/mayor.md (structural reference",
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

func TestRunPromptSynthWriteCreatesFileWithHeaderRigContext(t *testing.T) {
	rigPath := t.TempDir()
	cityDir := writeMinimalCity(t, "claude", config.Rig{Name: "myproj", Path: rigPath, DefaultBranch: "main"})
	runner := &fakeSynthRunner{body: "# Polecat\n\nbody."}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{
		role: "polecat", rig: "myproj", write: true, city: cityDir,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	dst := filepath.Join(cityDir, "agents", "polecat", "prompt.template.md")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `context:  rig "myproj" at `+rigPath) {
		t.Errorf("header should reflect rig context, got\n%s", got)
	}
}

func TestRunPromptSynthWriteRefusesToClobberWithoutForce(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	dst := filepath.Join(cityDir, "agents", "mayor", "prompt.template.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runner := &fakeSynthRunner{body: "REPLACEMENT"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", write: true, city: cityDir}, runner.run, &stdout, &stderr)
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
		t.Fatalf("seed: %v", err)
	}
	runner := &fakeSynthRunner{body: "# New Mayor"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", write: true, force: true, city: cityDir}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v\nstderr=%s", err, stderr.String())
	}
	got, _ := os.ReadFile(dst)
	if !strings.Contains(string(got), "# New Mayor") {
		t.Errorf("file not overwritten, got %q", got)
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
		role: "mayor", city: cityDir, metaPromptOverride: overridePath,
	}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPromptSynth: %v", err)
	}
	if runner.gotPrompt != "CUSTOM META role=mayor" {
		t.Errorf("override not used; runner got %q", runner.gotPrompt)
	}
}

// --- writer-agent (slingued mode placeholder) ---

func TestRunPromptSynthRejectsSlinguedModeUntilImplemented(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "should not be called"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir}, runner.run, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("expected slingued-mode-not-implemented error, got %v", err)
	}
	if runner.gotCalled {
		t.Errorf("runner must not be called in slingued mode")
	}
}

func TestRunPromptSynthEmptyWriterAgentTakesDirectPath(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	runner := &fakeSynthRunner{body: "ok"}
	var stdout, stderr bytes.Buffer
	err := runPromptSynth(context.Background(), promptSynthOpts{role: "mayor", writerAgent: "", city: cityDir}, runner.run, &stdout, &stderr)
	if err != nil {
		t.Fatalf("direct mode (writer-agent='') should succeed: %v", err)
	}
	if !runner.gotCalled {
		t.Errorf("direct-mode runner should be called")
	}
}
