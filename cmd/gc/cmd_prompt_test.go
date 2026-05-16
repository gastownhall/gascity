package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

func (r *fakeSynthRunner) run(_ context.Context, provider *config.ResolvedProvider, prompt, workDir string) (string, error) {
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

// TestRunPromptSynthRejectsRoleWithPathTraversal guards the validRoleName
// regex against attempts to escape the agents/<role>/ subdirectory via
// path-traversal sequences, hidden directories, or upper-case wildcards.
// Each rejected role must surface a clear "invalid --role" error and
// must NOT invoke the runner (provider resolution should not even be
// attempted on invalid input).
func TestRunPromptSynthRejectsRoleWithPathTraversal(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	for _, role := range []string{
		"../escape",
		"..",
		"foo/bar",
		"foo/../bar",
		".hidden",
		"-leading-dash",
		"7-leading-digit",
		"UPPERCASE",
		"foo bar",
		"",
	} {
		t.Run(role, func(t *testing.T) {
			runner := &fakeSynthRunner{body: "ignored"}
			var stdout, stderr bytes.Buffer
			err := runPromptSynth(context.Background(), promptSynthOpts{
				role:     role,
				provider: "claude",
				city:     cityDir,
			}, runner.run, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "invalid --role") {
				t.Errorf("role=%q: expected invalid-role error, got %v", role, err)
			}
			if runner.gotCalled {
				t.Errorf("role=%q: runner should not be called for invalid role", role)
			}
		})
	}
}

// TestRunPromptSynthAcceptsValidRoleNames ensures the validRoleName regex
// admits the names we actually use (mayor, polecat, codeprobe-worker, etc.)
// without false-positive rejections.
func TestRunPromptSynthAcceptsValidRoleNames(t *testing.T) {
	cityDir := writeMinimalCity(t, "claude")
	for _, role := range []string{
		"mayor",
		"polecat",
		"codeprobe-worker",
		"a",
		"a1",
		"agent-with-dashes-and-1-digit",
	} {
		t.Run(role, func(t *testing.T) {
			runner := &fakeSynthRunner{body: "# generated"}
			var stdout, stderr bytes.Buffer
			err := runPromptSynth(context.Background(), promptSynthOpts{
				role:     role,
				provider: "claude",
				city:     cityDir,
			}, runner.run, &stdout, &stderr)
			if err != nil {
				t.Errorf("role=%q: expected accept, got error %v", role, err)
			}
		})
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

// --- writer-agent / slingued mode ---

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

// fakeSlinger captures sling args for assertion and lets tests inject
// errors. Used to mock the gc-sling subprocess in slingued-mode tests.
type fakeSlinger struct {
	gotArgs [][]string
	err     error
}

func (s *fakeSlinger) call(_ context.Context, args []string) error {
	s.gotArgs = append(s.gotArgs, append([]string(nil), args...))
	return s.err
}

// writeCityWithAgent extends writeMinimalCity with a single configured
// agent the slingued-mode validation can find. providerKey is forwarded
// to writeMinimalCity (callers always pass "claude" today, but the
// parameter stays explicit so the helper can serve other providers
// without a signature break).
func writeCityWithAgent(t *testing.T, providerKey, agentName string) string { //nolint:unparam // providerKey kept explicit for future callers
	t.Helper()
	cityDir := writeMinimalCity(t, providerKey)
	tomlAdd := "\n[[agent]]\nname = \"" + agentName + "\"\n"
	f, err := os.OpenFile(filepath.Join(cityDir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open city.toml: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(tomlAdd); err != nil {
		t.Fatalf("append agent: %v", err)
	}
	return cityDir
}

func TestRunSlinguedSynthRejectsUnknownWriterAgent(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	slinger := &fakeSlinger{}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return beads.NewMemStore(), nil },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    10 * time.Millisecond,
	}

	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test-city", CityPath: cityDir}
	var stdout, stderr bytes.Buffer
	err = runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "ghost", city: cityDir, force: true},
		cfg, cityDir, "polecat", "rendered meta-prompt", mctx, deps, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected writer-agent-not-found error, got %v", err)
	}
	if !strings.Contains(err.Error(), "mayor") {
		t.Errorf("error should list known agents, got %v", err)
	}
	if len(slinger.gotArgs) > 0 {
		t.Errorf("sling must not be called for unknown writer-agent")
	}
}

func TestRunSlinguedSynthCreatesBeadStagesMetaAndCallsSling(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	store := beads.NewMemStore()
	slinger := &fakeSlinger{}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return store, nil },
		slingCaller: slinger.call,
		now:         func() time.Time { return time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC) },
		waitTick:    10 * time.Millisecond,
	}

	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	mctx := metaPromptCtx{
		Role:        "polecat",
		ProviderKey: "claude", ProviderDisplayName: "Claude Code",
		ContextType: "city", CityName: "test-city", CityPath: cityDir,
	}
	rendered := "RENDERED-META-PROMPT-MARKER-987"

	var stdout, stderr bytes.Buffer
	err = runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir},
		cfg, cityDir, "polecat", rendered, mctx, deps, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runSlinguedSynth: %v\nstderr=%s", err, stderr.String())
	}

	// Bead created with metadata.
	beadsList, _ := store.ListOpen()
	if len(beadsList) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(beadsList))
	}
	b := beadsList[0]
	if !strings.Contains(b.Title, "polecat") {
		t.Errorf("bead title should reference role, got %q", b.Title)
	}
	if b.Metadata["synth_role"] != "polecat" {
		t.Errorf("bead metadata.synth_role = %q, want polecat", b.Metadata["synth_role"])
	}
	if b.Metadata["synth_writer"] != "mayor" {
		t.Errorf("bead metadata.synth_writer = %q, want mayor", b.Metadata["synth_writer"])
	}
	wantDest := filepath.Join(cityDir, "agents", "polecat", "prompt.template.md")
	if b.Metadata["synth_dest"] != wantDest {
		t.Errorf("bead metadata.synth_dest = %q, want %q", b.Metadata["synth_dest"], wantDest)
	}

	// Meta-prompt staged at the path recorded in metadata.
	metaPath := b.Metadata["synth_meta_path"]
	if metaPath == "" {
		t.Fatalf("synth_meta_path missing from bead metadata")
	}
	staged, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read staged meta file: %v", err)
	}
	if !strings.Contains(string(staged), rendered) {
		t.Errorf("staged meta file should contain rendered meta-prompt; got %q", string(staged))
	}

	// Sling called with the right args.
	if len(slinger.gotArgs) != 1 {
		t.Fatalf("expected 1 sling call, got %d", len(slinger.gotArgs))
	}
	args := slinger.gotArgs[0]
	wantSubseq := []string{"mayor", b.ID, "--on", "mol-prompt-synth", "--var", "meta_prompt_path=" + metaPath, "--var", "dest_path=" + wantDest, "--var", "synth_role=polecat"}
	if !slicesEqual(args, wantSubseq) {
		t.Errorf("sling args = %v\nwant: %v", args, wantSubseq)
	}

	// Stdout summary mentions bead ID + destination.
	out := stdout.String()
	if !strings.Contains(out, b.ID) {
		t.Errorf("stdout should mention bead ID %q, got %q", b.ID, out)
	}
	if !strings.Contains(out, wantDest) {
		t.Errorf("stdout should mention dest path %q, got %q", wantDest, out)
	}
}

func TestRunSlinguedSynthRefusesToClobberWithoutForce(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	dest := filepath.Join(cityDir, "agents", "polecat", "prompt.template.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dest, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	store := beads.NewMemStore()
	slinger := &fakeSlinger{}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return store, nil },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    10 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test", CityPath: cityDir}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("expected refuse-to-clobber error, got %v", err)
	}
	if len(slinger.gotArgs) > 0 {
		t.Errorf("sling should not be called when destination conflict")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "ORIGINAL" {
		t.Errorf("destination should not be touched; got %q", got)
	}
}

func TestRunSlinguedSynthWaitReturnsWhenBeadCloses(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	store := beads.NewMemStore()
	slinger := &fakeSlinger{
		// Simulate the agent closing the bead "instantly" by closing
		// it ourselves right after sling is called.
	}
	closeAfterSling := func(s beads.Store) func(context.Context, []string) error {
		return func(_ context.Context, args []string) error {
			beadID := args[1]
			go func() {
				time.Sleep(20 * time.Millisecond)
				_ = s.Close(beadID)
			}()
			return nil
		}
	}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return store, nil },
		slingCaller: closeAfterSling(store),
		now:         time.Now,
		waitTick:    5 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test", CityPath: cityDir}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir, wait: true, waitTimeout: 5 * time.Second},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err != nil {
		t.Fatalf("--wait should succeed when bead closes: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "closed") {
		t.Errorf("stdout should announce bead close, got %q", stdout.String())
	}
	_ = slinger
}

func TestRunSlinguedSynthWaitTimesOut(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	store := beads.NewMemStore()
	slinger := &fakeSlinger{} // never closes the bead
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return store, nil },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    5 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test", CityPath: cityDir}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir, wait: true, waitTimeout: 50 * time.Millisecond},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- additional slingued-mode coverage (commit e68ff9ff hardening) ---

// errorOnCreateStore wraps MemStore and surfaces a configurable error
// from Create — used to verify CLI surfaces store failures cleanly
// without ever calling sling.
type errorOnCreateStore struct {
	beads.Store
	createErr error
}

func (s *errorOnCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.createErr != nil {
		return beads.Bead{}, s.createErr
	}
	return s.Store.Create(b)
}

// errorOnGetStore wraps MemStore and lets a test trigger a Get failure
// after N successful calls — used to test the polling loop in
// waitForSynthBeadClose.
type errorOnGetStore struct {
	beads.Store
	failAfter int
	getErr    error
	calls     int
}

func (s *errorOnGetStore) Get(id string) (beads.Bead, error) {
	s.calls++
	if s.getErr != nil && s.calls > s.failAfter {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

func TestRunSlinguedSynthRigContextPropagatesIntoBeadMetadata(t *testing.T) {
	rigPath := t.TempDir()
	cityDir := writeMinimalCity(t, "claude", config.Rig{Name: "myproj", Path: rigPath, DefaultBranch: "main"})
	// Add the writer-agent.
	if f, err := os.OpenFile(filepath.Join(cityDir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString("\n[[agent]]\nname = \"mayor\"\n")
		_ = f.Close()
	}

	store := beads.NewMemStore()
	slinger := &fakeSlinger{}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return store, nil },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    10 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{
		Role: "polecat", ProviderKey: "claude", ProviderDisplayName: "Claude Code",
		ContextType: "rig", CityName: "test-city", CityPath: cityDir,
		RigName: "myproj", RigPath: rigPath, RigDefaultBranch: "main",
	}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", rig: "myproj", writerAgent: "mayor", city: cityDir},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runSlinguedSynth: %v\nstderr=%s", err, stderr.String())
	}
	beadsList, _ := store.ListOpen()
	if len(beadsList) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(beadsList))
	}
	b := beadsList[0]
	if b.Metadata["synth_context"] != "rig" {
		t.Errorf("synth_context = %q, want rig", b.Metadata["synth_context"])
	}
	if !strings.Contains(b.Description, "rig \"myproj\"") {
		t.Errorf("bead description should mention rig name and path, got:\n%s", b.Description)
	}
	if !strings.Contains(b.Description, rigPath) {
		t.Errorf("bead description should mention rig path %q, got:\n%s", rigPath, b.Description)
	}
}

func TestRunSlinguedSynthForceAllowsClobber(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	dest := filepath.Join(cityDir, "agents", "polecat", "prompt.template.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dest, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	store := beads.NewMemStore()
	slinger := &fakeSlinger{}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return store, nil },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    10 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test", CityPath: cityDir}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", force: true, city: cityDir},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err != nil {
		t.Fatalf("--force should allow clobber: %v", err)
	}
	if len(slinger.gotArgs) != 1 {
		t.Errorf("expected sling to be called with --force; got %d calls", len(slinger.gotArgs))
	}
	// The destination file is left to the writer-agent to write — this
	// CLI doesn't touch it directly. We only verified preflight passed.
	got, _ := os.ReadFile(dest)
	if string(got) != "ORIGINAL" {
		t.Errorf("CLI must not touch the destination — that's the agent's job; got %q", got)
	}
}

func TestRunSlinguedSynthSurfacesSlingCallerError(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	store := beads.NewMemStore()
	slinger := &fakeSlinger{err: errors.New("sling refused: boom")}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return store, nil },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    10 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test", CityPath: cityDir}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected sling error to surface, got %v", err)
	}
	if !strings.Contains(err.Error(), "mayor") {
		t.Errorf("error should mention writer-agent for diagnosability, got %v", err)
	}
	// Bead was created before sling failed — that's fine, it sits open
	// for retry/inspection. We just verify that's the actual sequence.
	beadsList, _ := store.ListOpen()
	if len(beadsList) != 1 {
		t.Errorf("expected the synth bead to remain open for inspection after sling failure, got %d open beads", len(beadsList))
	}
}

func TestRunSlinguedSynthSurfacesStoreCreateError(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	wrapped := &errorOnCreateStore{Store: beads.NewMemStore(), createErr: errors.New("disk full")}
	slinger := &fakeSlinger{}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return wrapped, nil },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    10 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test", CityPath: cityDir}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("expected create-error to surface, got %v", err)
	}
	if len(slinger.gotArgs) > 0 {
		t.Errorf("sling must not be called when bead creation fails")
	}
}

func TestRunSlinguedSynthSurfacesStoreOpenError(t *testing.T) {
	cityDir := writeCityWithAgent(t, "claude", "mayor")
	slinger := &fakeSlinger{}
	deps := slinguedSynthDeps{
		storeOpener: func(string) (beads.Store, error) { return nil, errors.New("store init failed") },
		slingCaller: slinger.call,
		now:         time.Now,
		waitTick:    10 * time.Millisecond,
	}
	cfg, _ := loadCityConfig(cityDir, io.Discard)
	mctx := metaPromptCtx{Role: "polecat", ContextType: "city", CityName: "test", CityPath: cityDir}

	var stdout, stderr bytes.Buffer
	err := runSlinguedSynthWithDeps(context.Background(),
		promptSynthOpts{role: "polecat", writerAgent: "mayor", city: cityDir},
		cfg, cityDir, "polecat", "rendered", mctx, deps, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "store init failed") {
		t.Errorf("expected store-open error to surface, got %v", err)
	}
	if len(slinger.gotArgs) > 0 {
		t.Errorf("sling must not be called when store open fails")
	}
}

func TestWaitForSynthBeadCloseReturnsCtxErrWhenAlreadyCanceled(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "test", Type: "task"})
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	deps := slinguedSynthDeps{now: time.Now, waitTick: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	var stdout, stderr bytes.Buffer
	err = waitForSynthBeadClose(ctx, store, bead.ID, "/dev/null", time.Second, deps, &stdout, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestWaitForSynthBeadCloseSurfacesGetError(t *testing.T) {
	wrapped := &errorOnGetStore{Store: beads.NewMemStore(), failAfter: 0, getErr: errors.New("transient store glitch")}
	// Seed via the embedded MemStore directly (Create is promoted but we
	// avoid the indirection to keep the lint-suggested form).
	bead, err := wrapped.Create(beads.Bead{Title: "test", Type: "task"})
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	deps := slinguedSynthDeps{now: time.Now, waitTick: 10 * time.Millisecond}

	var stdout, stderr bytes.Buffer
	err = waitForSynthBeadClose(context.Background(), wrapped, bead.ID, "/dev/null", time.Second, deps, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "transient store glitch") {
		t.Errorf("expected store-get error to surface, got %v", err)
	}
}

func TestWaitForSynthBeadCloseNormalizesNonPositiveTimeout(t *testing.T) {
	// timeout <= 0 should be normalized to a sane default (10m). With
	// a tiny waitTick we'd loop forever otherwise; here we cancel via
	// ctx after one tick to verify the function does not immediately
	// time out on a 0 deadline.
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "test", Type: "task"})
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	deps := slinguedSynthDeps{now: time.Now, waitTick: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	var stdout, stderr bytes.Buffer
	err = waitForSynthBeadClose(ctx, store, bead.ID, "/dev/null", 0, deps, &stdout, &stderr)
	// Either ctx.DeadlineExceeded (normalized to 10m, never reached) or
	// context.Canceled is fine. What we must NOT see is the function's
	// own "timed out after 0s" message — that would mean timeout=0 was
	// taken literally.
	if err == nil {
		t.Fatalf("expected an error from canceled ctx, got nil")
	}
	if strings.Contains(err.Error(), "timed out after 0") {
		t.Errorf("non-positive timeout should be normalized, got %v", err)
	}
}

func TestAgentExistsInCityMatchesNameQualifiedAndBindingForms(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "polecat", Dir: "myrig"},
			{Name: "scout", BindingName: "extras"},
		},
	}
	cases := []struct {
		query string
		want  bool
	}{
		{"mayor", true},
		{"polecat", true},
		{"myrig/polecat", true}, // qualified
		{"scout", true},
		{"extras.scout", true}, // binding-qualified
		{"ghost", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := agentExistsInCity(tc.query, cfg); got != tc.want {
			t.Errorf("agentExistsInCity(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestAgentExistsInCityHandlesNilCfg(t *testing.T) {
	if agentExistsInCity("anything", nil) {
		t.Errorf("nil cfg must always return false")
	}
}

func TestKnownAgentNamesEmptyAndPopulated(t *testing.T) {
	if got := knownAgentNames(nil); got != "(none configured)" {
		t.Errorf("empty input: got %q, want '(none configured)'", got)
	}
	if got := knownAgentNames([]config.Agent{}); got != "(none configured)" {
		t.Errorf("empty slice: got %q, want '(none configured)'", got)
	}
	got := knownAgentNames([]config.Agent{
		{Name: "mayor"},
		{Name: "polecat", Dir: "rig1"},
	})
	if !strings.Contains(got, "mayor") || !strings.Contains(got, "rig1/polecat") {
		t.Errorf("populated: got %q, want both names listed", got)
	}
}
