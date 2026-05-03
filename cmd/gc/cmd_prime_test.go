package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestBuildPrimeContextFallsBackToConfiguredRigRoot(t *testing.T) {
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_RIG_ROOT", "")
	t.Setenv("GC_DIR", "/tmp/demo-work")
	t.Setenv("GC_BRANCH", "")

	ctx := buildPrimeContext("/city", "test-city", &config.Agent{Name: "polecat", Dir: "demo"}, []config.Rig{
		{Name: "demo", Path: "/repos/demo", Prefix: "dm"},
	}, nil)

	if ctx.RigName != "demo" {
		t.Fatalf("RigName = %q, want demo", ctx.RigName)
	}
	if ctx.RigRoot != "/repos/demo" {
		t.Fatalf("RigRoot = %q, want /repos/demo", ctx.RigRoot)
	}
}

func TestBuildPrimeContextExpandsTemplateCommands(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}

	ctx := buildPrimeContext(cityPath, "", &config.Agent{
		Name:       "worker",
		Dir:        "demo",
		WorkQuery:  "echo {{.CityName}} {{.Rig}} {{.AgentBase}}",
		SlingQuery: "dispatch {} --route={{.Rig}}/{{.AgentBase}} --city={{.CityName}}",
	}, rigs, nil)

	if ctx.WorkQuery != "echo demo-city demo worker" {
		t.Fatalf("WorkQuery = %q, want %q", ctx.WorkQuery, "echo demo-city demo worker")
	}
	if ctx.SlingQuery != "dispatch {} --route=demo/worker --city=demo-city" {
		t.Fatalf("SlingQuery = %q, want %q", ctx.SlingQuery, "dispatch {} --route=demo/worker --city=demo-city")
	}
}

func TestBuildPrimeContextLogsTemplateExpansionWarning(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	var stderr bytes.Buffer

	ctx := buildPrimeContext(cityPath, "", &config.Agent{
		Name:      "worker",
		WorkQuery: "echo {{.Rig",
	}, nil, &stderr)

	if ctx.WorkQuery != "echo {{.Rig" {
		t.Fatalf("WorkQuery = %q, want raw command fallback", ctx.WorkQuery)
	}
	if !strings.Contains(stderr.String(), "work_query") {
		t.Fatalf("stderr missing field name: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "echo {{.Rig") {
		t.Fatalf("stderr should redact raw template, got %q", stderr.String())
	}
}

func TestBuildPrimeContextRendersBindingQualifiedRoute(t *testing.T) {
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_BRANCH", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_ALIAS", "")

	cityPath := t.TempDir()
	promptDir := filepath.Join(cityPath, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "polecat.template.md"), []byte("route={{ .RigName }}/{{ .BindingPrefix }}refinery\nbinding={{ .BindingName }}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt): %v", err)
	}

	ctx := buildPrimeContext(cityPath, "test-city", &config.Agent{
		Name:        "polecat",
		Dir:         "demo",
		BindingName: "gastown",
	}, []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}, nil)

	if ctx.BindingName != "gastown" {
		t.Fatalf("BindingName = %q, want gastown", ctx.BindingName)
	}
	if ctx.BindingPrefix != "gastown." {
		t.Fatalf("BindingPrefix = %q, want gastown.", ctx.BindingPrefix)
	}
	var stderr bytes.Buffer
	got := renderPrompt(fsys.OSFS{}, cityPath, "test-city", "prompts/polecat.template.md", ctx, "", &stderr, nil, nil, nil)
	want := "route=demo/gastown.refinery\nbinding=gastown\n"
	if got != want {
		t.Fatalf("rendered prompt = %q, want %q; stderr=%q", got, want, stderr.String())
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

	ctx := buildPrimeContext("/city", "test-city", &config.Agent{Name: "mayor"}, nil, nil)

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

	ctx := buildPrimeContext("/city", "test-city", &config.Agent{Name: "mayor"}, nil, nil)

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

	ctx := buildPrimeContext("/city", "test-city", &config.Agent{Name: "mayor"}, nil, nil)

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
	code := doPrimeWithMode(nil, &stdout, &stderr, true, false)
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

func TestDoPrimeWithHook_StartupPromptDeliveryEnvControlsPromptSuppression(t *testing.T) {
	cityDir := t.TempDir()
	promptDir := filepath.Join(cityDir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	const promptContent = "launch-only startup prompt\n"
	if err := os.WriteFile(filepath.Join(promptDir, "worker.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "gastown"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	for _, tc := range []struct {
		name             string
		delivered        string
		managedHook      string
		envHookSource    string
		envHookEvent     string
		wantPromptInHook bool
	}{
		{
			name:             "startup hook delivered",
			delivered:        "1",
			managedHook:      "1",
			envHookSource:    "startup",
			envHookEvent:     "SessionStart",
			wantPromptInHook: false,
		},
		{
			name:             "resume hook delivered",
			delivered:        "1",
			managedHook:      "1",
			envHookSource:    "resume",
			envHookEvent:     "SessionStart",
			wantPromptInHook: false,
		},
		{name: "manual command with inherited marker", delivered: "1", wantPromptInHook: true},
		{
			name:             "unmanaged session start keeps prompt",
			delivered:        "1",
			envHookEvent:     "SessionStart",
			wantPromptInHook: true,
		},
		{
			name:             "startup hook not delivered",
			managedHook:      "1",
			envHookSource:    "startup",
			envHookEvent:     "SessionStart",
			wantPromptInHook: true,
		},
		{
			name:             "non startup event keeps prompt",
			delivered:        "1",
			envHookSource:    "startup",
			envHookEvent:     "UserPromptSubmit",
			wantPromptInHook: true,
		},
		{
			name:             "session start ignores source value",
			delivered:        "1",
			managedHook:      "1",
			envHookSource:    "manual",
			envHookEvent:     "SessionStart",
			wantPromptInHook: false,
		},
		{name: "unset source not delivered", wantPromptInHook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPrimeHookStdin(t, nil)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_SESSION_NAME", "gastown--worker")
			t.Setenv("GC_SESSION_ID", "sess-777")
			t.Setenv(managedSessionHookEnv, tc.managedHook)
			t.Setenv("GC_HOOK_SOURCE", tc.envHookSource)
			t.Setenv("GC_HOOK_EVENT_NAME", tc.envHookEvent)
			t.Setenv(startupPromptDeliveredEnv, tc.delivered)

			var stdout, stderr bytes.Buffer
			code := doPrimeWithMode(nil, &stdout, &stderr, true, false)
			if code != 0 {
				t.Fatalf("doPrimeWithMode() = %d, want 0; stderr=%q", code, stderr.String())
			}
			out := stdout.String()
			if got := strings.Contains(out, promptContent); got != tc.wantPromptInHook {
				t.Fatalf("stdout = %q, prompt present = %v, want %v", out, got, tc.wantPromptInHook)
			}
			if !strings.Contains(out, "[gastown] worker") {
				t.Fatalf("stdout = %q, want hook beacon", out)
			}
		})
	}
}

func TestDoPrimeWithHook_DeliveredStartupPromptJSONHookFormat(t *testing.T) {
	cityDir := t.TempDir()
	promptDir := filepath.Join(cityDir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	const promptContent = "launch-only startup prompt\n"
	if err := os.WriteFile(filepath.Join(promptDir, "worker.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "gastown"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_NAME", "gastown--worker")
	t.Setenv("GC_SESSION_ID", "sess-777")
	t.Setenv(managedSessionHookEnv, "1")
	t.Setenv("GC_HOOK_SOURCE", "startup")
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
	t.Setenv(startupPromptDeliveredEnv, "1")
	withPrimeHookStdin(t, nil)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatGemini, false)
	if code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}

	var got struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("hook output is not JSON: %v; stdout=%q", err, stdout.String())
	}
	context := got.HookSpecificOutput.AdditionalContext
	if strings.Contains(context, promptContent) {
		t.Fatalf("additionalContext = %q, want no repeated startup prompt", context)
	}
	if !strings.Contains(context, "[gastown] worker") {
		t.Fatalf("additionalContext = %q, want hook beacon", context)
	}
}

func TestDoPrimeWithHookFormat_FormatsDefaultFallback(t *testing.T) {
	t.Setenv("GC_CITY", filepath.Join(t.TempDir(), "missing-city"))
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatCodex, false)
	if code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}

	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not hook JSON: %v\n%s", err, stdout.String())
	}
	if got, want := payload.HookSpecificOutput.HookEventName, "SessionStart"; got != want {
		t.Fatalf("hookEventName = %q, want %q", got, want)
	}
	if !strings.Contains(payload.HookSpecificOutput.AdditionalContext, "# Gas City Agent") {
		t.Fatalf("additionalContext = %q, want default prime prompt", payload.HookSpecificOutput.AdditionalContext)
	}
}

func withPrimeHookStdin(t *testing.T, payload map[string]string) {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		if err := json.NewEncoder(writer).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	oldPrimeStdin := primeStdin
	primeStdin = func() *os.File { return reader }
	t.Cleanup(func() {
		primeStdin = oldPrimeStdin
		_ = reader.Close()
	})
}
