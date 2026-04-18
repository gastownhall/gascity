package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBuildPrimeContextFallsBackToConfiguredRigRoot(t *testing.T) {
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_RIG_ROOT", "")
	t.Setenv("GC_DIR", "/tmp/demo-work")
	t.Setenv("GC_BRANCH", "")

	ctx := buildPrimeContext("/city", &config.Agent{Name: "polecat", Dir: "demo"}, []config.Rig{
		{Name: "demo", Path: "/repos/demo", Prefix: "dm"},
	})

	if ctx.RigName != "demo" {
		t.Fatalf("RigName = %q, want demo", ctx.RigName)
	}
	if ctx.RigRoot != "/repos/demo" {
		t.Fatalf("RigRoot = %q, want /repos/demo", ctx.RigRoot)
	}
}

func TestDoPrime_RendersConventionDiscoveredRootCityAgent(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "agents", "ada"), 0o755); err != nil {
		t.Fatalf("MkdirAll(agents/ada): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "backstage"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte(`
[pack]
name = "backstage"
schema = 2
`), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "agents", "ada", "prompt.template.md"), []byte("Agent: {{ .AgentName }}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt.template.md): %v", err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"ada"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "Agent: ada\n" {
		t.Fatalf("stdout = %q, want %q", got, "Agent: ada\n")
	}
}

func TestBuildPrimeContextPrefersGCAliasOverGCAgent(t *testing.T) {
	// When GC_AGENT is a session bead ID, buildPrimeContext should prefer
	// GC_ALIAS for AgentName so the prompt doesn't contain a bead ID.
	t.Setenv("GC_AGENT", "bl-9jl")
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_BRANCH", "")

	ctx := buildPrimeContext("/city", &config.Agent{Name: "mayor"}, nil)

	if ctx.AgentName != "mayor" {
		t.Errorf("AgentName = %q, want %q (should prefer GC_ALIAS over GC_AGENT)", ctx.AgentName, "mayor")
	}
}

func TestBuildPrimeContextUsesAliasEvenWhenDifferentFromConfigName(t *testing.T) {
	// When GC_ALIAS is set but differs from the config agent name, AgentName
	// should still reflect GC_ALIAS — the alias is the public identity the
	// prompt should use.
	t.Setenv("GC_AGENT", "bl-9jl")
	t.Setenv("GC_ALIAS", "custom-alias")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_BRANCH", "")

	ctx := buildPrimeContext("/city", &config.Agent{Name: "mayor"}, nil)

	if ctx.AgentName != "custom-alias" {
		t.Errorf("AgentName = %q, want %q (should use GC_ALIAS even when it differs from config name)", ctx.AgentName, "custom-alias")
	}
}

func TestBuildPrimeContextFallsBackToGCAgentWhenNoAlias(t *testing.T) {
	// When GC_ALIAS is not set, buildPrimeContext should still use GC_AGENT.
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_BRANCH", "")

	ctx := buildPrimeContext("/city", &config.Agent{Name: "mayor"}, nil)

	if ctx.AgentName != "mayor" {
		t.Errorf("AgentName = %q, want %q", ctx.AgentName, "mayor")
	}
}

func TestDoPrime_UsesGCTemplateForNamepoolSessionContext(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigrepo")
	workDir := filepath.Join(cityDir, ".gc", "worktrees", "rigrepo", "polecats", "furiosa")
	promptDir := filepath.Join(cityDir, "prompts")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workDir): %v", err)
	}
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "polecat.template.md"), []byte("Agent={{ .AgentName }}\nTemplate={{ .TemplateName }}\nRig={{ .RigName }}\nRoot={{ .RigRoot }}\nWorkDir={{ .WorkDir }}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "gastown"

[[rigs]]
name = "rigrepo"
path = "rigrepo"
prefix = "rr"

[[agent]]
name = "polecat"
dir = "rigrepo"
prompt_template = "prompts/polecat.template.md"

[agent.pool]
min = 0
max = 5
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "rigrepo/furiosa")
	t.Setenv("GC_AGENT", "rigrepo/furiosa")
	t.Setenv("GC_TEMPLATE", "rigrepo/polecat")
	t.Setenv("GC_SESSION_NAME", "rigrepo--furiosa")
	t.Setenv("GC_DIR", workDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Setenv("GC_BRANCH", "")

	var stdout, stderr bytes.Buffer
	code := doPrime(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime() = %d, want 0; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Agent=rigrepo/furiosa",
		"Template=polecat",
		"Rig=rigrepo",
		"Root=" + rigDir,
		"WorkDir=" + workDir,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "# Gas City Agent") {
		t.Fatalf("stdout = %q, want resolved polecat prompt, not generic fallback", out)
	}
}

func TestDoPrimeWithHook_UsesGCTemplateForNamepoolSessionContext(t *testing.T) {
	cityDir := t.TempDir()
	promptDir := filepath.Join(cityDir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "polecat.template.md"), []byte("prompt for {{ .AgentName }}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "gastown"

[[agent]]
name = "polecat"
dir = "rigrepo"
prompt_template = "prompts/polecat.template.md"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "rigrepo/furiosa")
	t.Setenv("GC_AGENT", "rigrepo/furiosa")
	t.Setenv("GC_TEMPLATE", "rigrepo/polecat")
	t.Setenv("GC_SESSION_NAME", "rigrepo--furiosa")
	t.Setenv("GC_SESSION_ID", "sess-777")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode(nil, &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode() = %d, want 0; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "prompt for rigrepo/furiosa") {
		t.Fatalf("stdout = %q, want resolved hook prompt for session alias", out)
	}
	if !strings.Contains(out, "[gastown] rigrepo/furiosa") {
		t.Fatalf("stdout = %q, want hook beacon for public alias", out)
	}
	if strings.Contains(out, "# Gas City Agent") {
		t.Fatalf("stdout = %q, want resolved hook prompt, not generic fallback", out)
	}
}
