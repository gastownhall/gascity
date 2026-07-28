package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

type primeHookFailWriter struct {
	err error
}

func (w primeHookFailWriter) Write([]byte) (int, error) {
	return 0, w.err
}

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
	if ctx.AssignedInProgressQuery != "echo demo-city demo worker" {
		t.Fatalf("AssignedInProgressQuery = %q, want expanded custom query", ctx.AssignedInProgressQuery)
	}
	if ctx.AssignedReadyQuery != "echo demo-city demo worker" {
		t.Fatalf("AssignedReadyQuery = %q, want expanded custom query", ctx.AssignedReadyQuery)
	}
	if ctx.RoutedPoolQuery != "echo demo-city demo worker" {
		t.Fatalf("RoutedPoolQuery = %q, want expanded custom query", ctx.RoutedPoolQuery)
	}
	if ctx.SlingQuery != "dispatch {} --route=demo/worker --city=demo-city" {
		t.Fatalf("SlingQuery = %q, want %q", ctx.SlingQuery, "dispatch {} --route=demo/worker --city=demo-city")
	}
}

func TestBuildPrimeContextUsesBD105ReadyCompatibility(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	ctx := buildPrimeContextForBeads(cityPath, "", &config.Agent{
		Name: "worker",
	}, nil, config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}, nil)

	if !strings.Contains(ctx.AssignedReadyQuery, `bd ready --include-ephemeral --assignee="$id"`) {
		t.Fatalf("AssignedReadyQuery = %q, want bd-1.0.5-compatible assigned ready query", ctx.AssignedReadyQuery)
	}
	if !strings.Contains(ctx.WorkQuery, "bd ready --include-ephemeral") {
		t.Fatalf("WorkQuery = %q, want bd-1.0.5-compatible ready probes", ctx.WorkQuery)
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

// TestPrimeInjectMailContentSurfacesUnreadMailForPromptlessWake covers the
// prime-inject-mail patch (dip-bj7pgj): an autonomous/promptless restart runs
// the SessionStart prime hook but NOT the UserPromptSubmit mail hook, so gc
// prime must fold unread mail into the SessionStart payload itself. With no
// unread mail the injection is empty (never noises up a prime); once mail is
// waiting for the self-recipient, prime surfaces the same <system-reminder>
// block the check path produces.
func TestPrimeInjectMailContentSurfacesUnreadMailForPromptlessWake(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")

	// No unread mail yet: a promptless wake must inject nothing.
	if got := primeInjectMailContent(); got != "" {
		t.Fatalf("primeInjectMailContent with an empty inbox = %q, want empty", got)
	}

	// Seed unread mail for the self-recipient (mayor) through the real city
	// provider so the read path is exercised end to end.
	mp, code := openCityMailProvider(io.Discard, "test seed")
	if mp == nil {
		t.Fatalf("openCityMailProvider returned nil (code=%d)", code)
	}
	if _, err := mp.Send("worker", "mayor", "PR ready", "please review the auth PR"); err != nil {
		t.Fatalf("seed Send: %v", err)
	}

	got := primeInjectMailContent()
	if !strings.Contains(got, "<system-reminder>") || !strings.Contains(got, "</system-reminder>") {
		t.Fatalf("prime mail injection missing system-reminder wrapper:\n%s", got)
	}
	if !strings.Contains(got, "unread message(s)") {
		t.Fatalf("prime mail injection missing unread count:\n%s", got)
	}
	if !strings.Contains(got, "please review the auth PR") {
		t.Fatalf("prime mail injection missing the seeded message body:\n%s", got)
	}
}

func TestDoPrimeScopesRigPackFragmentsByCurrentRig(t *testing.T) {
	clearGCEnv(t)

	cityDir := t.TempDir()
	write := func(rel, data string) {
		path := filepath.Join(cityDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	write("pack.toml", "[pack]\nname = \"prompt-city\"\nschema = 2\n")
	write("city.toml", `
[workspace]
name = "prompt-city"

[[rigs]]
name = "alpha"

[rigs.imports.alpha]
source = "./packs/alpha"

[[rigs]]
name = "bravo"

[rigs.imports.bravo]
source = "./packs/bravo"
`)
	write(".gc/site.toml", `
workspace_name = "prompt-city"

[[rig]]
name = "alpha"
path = "./rigs/alpha"

[[rig]]
name = "bravo"
path = "./rigs/bravo"
`)
	write("agents/alpha-worker/agent.toml", "dir = \"alpha\"\nprompt_template = \"agents/alpha-worker/prompt.template.md\"\n")
	write("agents/bravo-worker/agent.toml", "dir = \"bravo\"\nprompt_template = \"agents/bravo-worker/prompt.template.md\"\n")
	write("agents/alpha-worker/prompt.template.md", `{{ template "work-query" . }}`)
	write("agents/bravo-worker/prompt.template.md", `{{ template "work-query" . }}`)
	write("packs/alpha/pack.toml", "[pack]\nname = \"alpha\"\nschema = 2\n")
	write("packs/bravo/pack.toml", "[pack]\nname = \"bravo\"\nschema = 2\n")
	write("packs/alpha/template-fragments/work-query.template.md", `{{ define "work-query" }}alpha-work-query{{ end }}`)
	write("packs/bravo/template-fragments/work-query.template.md", `{{ define "work-query" }}bravo-work-query{{ end }}`)

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"alpha-worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrime() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "alpha-work-query" {
		t.Fatalf("stdout = %q, want alpha rig fragment; stderr=%q", got, stderr.String())
	}
	if strings.Contains(stdout.String(), "bravo-work-query") {
		t.Fatalf("stdout = %q, must not include bravo rig fragment", stdout.String())
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
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

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
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

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
		liveSession      bool
		wantPromptInHook bool
		wantBeacon       bool
	}{
		{
			name:             "startup hook delivered",
			delivered:        "1",
			managedHook:      "1",
			envHookSource:    "startup",
			envHookEvent:     "SessionStart",
			liveSession:      true,
			wantPromptInHook: false,
			wantBeacon:       true,
		},
		{
			name:             "resume hook delivered",
			delivered:        "1",
			managedHook:      "1",
			envHookSource:    "resume",
			envHookEvent:     "SessionStart",
			liveSession:      true,
			wantPromptInHook: false,
			wantBeacon:       true,
		},
		{name: "manual command with inherited marker", delivered: "1", wantPromptInHook: true, wantBeacon: true},
		{
			name:             "unmanaged session start gates prompt",
			delivered:        "1",
			envHookEvent:     "SessionStart",
			wantPromptInHook: false,
			wantBeacon:       false,
		},
		{
			name:             "startup hook not delivered",
			managedHook:      "1",
			envHookSource:    "startup",
			envHookEvent:     "SessionStart",
			liveSession:      true,
			wantPromptInHook: true,
			wantBeacon:       true,
		},
		{
			name:             "non startup event keeps prompt",
			delivered:        "1",
			envHookSource:    "startup",
			envHookEvent:     "UserPromptSubmit",
			wantPromptInHook: true,
			wantBeacon:       true,
		},
		{
			name:             "session start ignores source value",
			delivered:        "1",
			managedHook:      "1",
			envHookSource:    "manual",
			envHookEvent:     "SessionStart",
			liveSession:      true,
			wantPromptInHook: false,
			wantBeacon:       true,
		},
		{name: "unset source not delivered", wantPromptInHook: true, wantBeacon: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			withPrimeHookStdin(t)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_SESSION_NAME", "gastown--worker")
			if tc.liveSession {
				sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")
				t.Setenv("GC_SESSION_ID", sessionID)
			} else {
				t.Setenv("GC_SESSION_ID", "")
			}
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
			if got := strings.Contains(out, "[gastown] worker"); got != tc.wantBeacon {
				t.Fatalf("stdout = %q, beacon present = %v, want %v", out, got, tc.wantBeacon)
			}
		})
	}
}

// Tests that replace the startNudgePoller package seam must stay serial; do
// not add t.Parallel here.
func TestDoPrimeWithHook_StaleContinuationEpochRedeliversPrompt(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	prevPoller := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prevPoller })

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

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	seededSessionID, err := session.NewStore(beads.SessionStore{Store: store}).CreateSession(session.CreateSpec{
		Title:     "worker",
		AgentName: "worker",
		Metadata: map[string]string{
			"session_name":       "gastown--worker",
			"continuation_epoch": "3",
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	for _, tc := range []struct {
		name             string
		paneEpoch        string
		sessionID        string // defaults to the seeded session bead
		wantPromptInHook bool
	}{
		{name: "pane epoch behind bead redelivers", paneEpoch: "2", wantPromptInHook: true},
		{name: "pane epoch matches bead suppresses", paneEpoch: "3", wantPromptInHook: false},
		{name: "pane epoch ahead of bead suppresses", paneEpoch: "4", wantPromptInHook: false},
		{name: "missing pane epoch suppresses", paneEpoch: "", wantPromptInHook: false},
		{name: "unparsable pane epoch suppresses", paneEpoch: "junk", wantPromptInHook: false},
		{name: "missing session bead suppresses", paneEpoch: "2", sessionID: "sess-missing", wantPromptInHook: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPrimeHookStdin(t)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_SESSION_NAME", "gastown--worker")
			sessionID := tc.sessionID
			if sessionID == "" {
				sessionID = seededSessionID
			}
			t.Setenv("GC_SESSION_ID", sessionID)
			t.Setenv("GC_CONTINUATION_EPOCH", tc.paneEpoch)
			t.Setenv(managedSessionHookEnv, "1")
			t.Setenv("GC_HOOK_SOURCE", "startup")
			t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
			t.Setenv(startupPromptDeliveredEnv, "1")

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
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

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
	sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv(managedSessionHookEnv, "1")
	t.Setenv("GC_HOOK_SOURCE", "startup")
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
	t.Setenv(startupPromptDeliveredEnv, "1")
	withPrimeHookStdin(t)

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

// TestDoPrimeWithHook_SuppressedSessionStartInjectsUnreadMail drives the full
// SessionStart hook payload (doPrimeWithHookFormat) on the suppressed-startup-
// prompt path — the promptless-wake shape (dip-bj7pgj) where the rendered
// startup prompt is delivered out of band, so only hook-only context survives.
// With unread mail waiting for the self-recipient, the mail <system-reminder>
// block must land in additionalContext alongside the beacon (and after the
// suppressed prompt), for both the codex and gemini hook formats.
func TestDoPrimeWithHook_SuppressedSessionStartInjectsUnreadMail(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

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

	for _, hookFormat := range []string{hookOutputFormatCodex, hookOutputFormatGemini} {
		hookFormat := hookFormat
		t.Run(hookFormat, func(t *testing.T) {
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_SESSION_NAME", "gastown--worker")
			sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")
			t.Setenv("GC_SESSION_ID", sessionID)
			t.Setenv(managedSessionHookEnv, "1")
			t.Setenv("GC_HOOK_SOURCE", "startup")
			t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
			t.Setenv(startupPromptDeliveredEnv, "1")
			withPrimeHookStdin(t)

			// Seed unread mail for the self-recipient (worker) through the real
			// city provider so the SessionStart injection path is exercised end
			// to end.
			mp, code := openCityMailProvider(io.Discard, "test seed")
			if mp == nil {
				t.Fatalf("openCityMailProvider returned nil (code=%d)", code)
			}
			if _, err := mp.Send("boss", "worker", "restart handoff", "resume the migration"); err != nil {
				t.Fatalf("seed Send: %v", err)
			}

			var stdout, stderr bytes.Buffer
			if got := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookFormat, false); got != 0 {
				t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", got, stderr.String())
			}

			var out struct {
				HookSpecificOutput struct {
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("hook output is not JSON: %v; stdout=%q", err, stdout.String())
			}
			context := out.HookSpecificOutput.AdditionalContext
			if strings.Contains(context, promptContent) {
				t.Fatalf("additionalContext = %q, want no repeated startup prompt", context)
			}
			if !strings.Contains(context, "[gastown] worker") {
				t.Fatalf("additionalContext = %q, want hook beacon", context)
			}
			if !strings.Contains(context, "<system-reminder>") {
				t.Fatalf("additionalContext = %q, want mail system-reminder block", context)
			}
			if !strings.Contains(context, "unread message(s)") {
				t.Fatalf("additionalContext = %q, want unread-mail count", context)
			}
			if !strings.Contains(context, "resume the migration") {
				t.Fatalf("additionalContext = %q, want seeded mail body", context)
			}
			// Ordering: the mail block folds in after the beacon (which carries
			// the suppressed prompt slot), matching writePrimePromptWithFormat.
			if strings.Index(context, "[gastown] worker") > strings.Index(context, "<system-reminder>") {
				t.Fatalf("additionalContext = %q, want beacon before mail block", context)
			}
		})
	}
}

// mustCreateInProgressStore creates a bead in a beads.Store and transitions it
// to in_progress. It mirrors the MemStore helper in wisp_step_inject_test.go
// but works against the concrete city store opened on disk.
func mustCreateInProgressStore(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := store.Update(created.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}
	created.Status = status
	return created
}

// TestDoPrimeWithHook_DeliveredStartupPromptKeepsStepReminder is the
// managed-SessionStart regression: when the startup prompt is suppressed
// (GC_STARTUP_PROMPT_DELIVERED=1 + managed hook + SessionStart), the rendered
// startup prompt must be absent from the single hook payload, but the agent's
// active formula step and durable auto-handoff <system-reminders> must still be
// injected. Both are hook-only context, so they survive suppression.
func TestDoPrimeWithHook_DeliveredStartupPromptKeepsStepReminder(t *testing.T) {
	for _, hookFormat := range []string{"codex", hookOutputFormatGemini} {
		t.Run(hookFormat, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")

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

[mail]
provider = "exec:/not-used-by-auto-handoff"
`), 0o644); err != nil {
				t.Fatalf("WriteFile(city.toml): %v", err)
			}

			// Seed an in-progress molecule with an in-progress step child assigned
			// to the agent so wispStepInjectionContent resolves an active step.
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatalf("openCityStoreAt: %v", err)
			}
			mol := mustCreateInProgressStore(t, store, beads.Bead{
				Title:    "Formula: mol-worker",
				Type:     "molecule",
				Assignee: "worker",
			})
			step := mustCreateInProgressStore(t, store, beads.Bead{
				Title:       "Step 1: implement the widget",
				Description: "Write the widget code",
				Type:        "step",
				Assignee:    "worker",
				ParentID:    mol.ID,
			})

			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_SESSION_NAME", "gastown--worker")
			sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")
			auto, ok := createHandoffMail(store, store, events.Discard, sessionID, sessionID,
				[]string{"context cycle", "continue the durable task"}, "context cycle",
				[]string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel}, &bytes.Buffer{})
			if !ok {
				t.Fatal("createHandoffMail(auto) failed")
			}
			ordinary, err := beadmail.New(store).Send("human", sessionID, "ordinary", "leave this for UserPromptSubmit")
			if err != nil {
				t.Fatalf("Send ordinary mail: %v", err)
			}
			t.Setenv("GC_SESSION_ID", sessionID)
			t.Setenv(managedSessionHookEnv, "1")
			t.Setenv("GC_HOOK_SOURCE", "startup")
			t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
			t.Setenv(startupPromptDeliveredEnv, "1")
			withPrimeHookStdin(t)

			var stdout, stderr bytes.Buffer
			code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookFormat, false)
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
			// The suppressed startup prompt must be absent...
			if strings.Contains(context, promptContent) {
				t.Fatalf("additionalContext = %q, want no repeated startup prompt", context)
			}
			// ...but the active step reminder must survive suppression.
			for _, want := range []string{"<system-reminder>", step.Title, step.ID, "Write the widget code"} {
				if !strings.Contains(context, want) {
					t.Fatalf("additionalContext = %q, want step reminder substring %q", context, want)
				}
			}
			if !strings.Contains(context, "[gastown] worker") {
				t.Fatalf("additionalContext = %q, want hook beacon", context)
			}
			for _, want := range []string{auto.ID, auto.Subject, auto.Body} {
				if !strings.Contains(context, want) {
					t.Fatalf("additionalContext = %q, want auto-handoff substring %q", context, want)
				}
			}
			// This city configures an exec: ordinary-mail provider, so the
			// ordinary-mail read contributes nothing to the SessionStart payload
			// while beadmail-backed auto-handoff still does. (The beadmail-backed
			// ordinary case — where unread mail *is* injected — is pinned by
			// TestDoPrimeWithHook_SessionStartDedupsAutoHandoffAndKeepsOrdinaryMailOpen.)
			if strings.Contains(context, ordinary.ID) || strings.Contains(context, ordinary.Body) {
				t.Fatalf("additionalContext = %q, want no ordinary mail %q from the exec: provider at SessionStart", context, ordinary.ID)
			}
			if _, err := store.Get(auto.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("auto-handoff should be archived after SessionStart injection, got err=%v", err)
			}
			if _, err := store.Get(ordinary.ID); err != nil {
				t.Fatalf("ordinary mail should remain for UserPromptSubmit: %v", err)
			}

			// A provider-hook write failure must leave the next auto-handoff
			// durable for retry; delivery acknowledgement is the archive point.
			undelivered, ok := createHandoffMail(store, store, events.Discard, sessionID, sessionID,
				[]string{"context cycle retry", "retry the durable task"}, "context cycle",
				[]string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel}, &bytes.Buffer{})
			if !ok {
				t.Fatal("createHandoffMail(undelivered) failed")
			}
			if code := doPrimeWithHookFormat(nil, primeHookFailWriter{err: errors.New("hook output unavailable")}, &stderr, true, hookFormat, false); code != 0 {
				t.Fatalf("doPrimeWithHookFormat(failed writer) = %d, want 0; stderr=%q", code, stderr.String())
			}
			if _, err := store.Get(undelivered.ID); err != nil {
				t.Fatalf("auto-handoff must remain durable when SessionStart output fails: %v", err)
			}
		})
	}
}

// TestDoPrimeWithHook_SessionStartDedupsAutoHandoffAndKeepsOrdinaryMailOpen is
// the beadmail-backed counterpart to
// TestDoPrimeWithHook_DeliveredStartupPromptKeepsStepReminder: with no [mail]
// provider configured, beadmail backs ordinary mail too, so the SessionStart
// ordinary-unread read (dip-bj7pgj) sees the auto-handoff as well. It pins the
// three properties that shape depends on: the auto-handoff is rendered exactly
// once (the dedup branch actually filters), ordinary unread mail *is* surfaced,
// and the ordinary read is non-destructive — the message is still in the store
// after the hook run, so the later UserPromptSubmit delivery is not consumed.
func TestDoPrimeWithHook_SessionStartDedupsAutoHandoffAndKeepsOrdinaryMailOpen(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	promptDir := filepath.Join(cityDir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "worker.md"), []byte("launch-only startup prompt\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt): %v", err)
	}
	// No [mail] provider: beadmail backs ordinary mail, so the ordinary read
	// and the auto-handoff read hit the same store.
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "gastown"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_NAME", "gastown--worker")
	sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")
	auto, ok := createHandoffMail(store, store, events.Discard, sessionID, sessionID,
		[]string{"context cycle", "continue the durable task"}, "context cycle",
		[]string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel}, &bytes.Buffer{})
	if !ok {
		t.Fatal("createHandoffMail(auto) failed")
	}
	ordinary, err := beadmail.New(store).Send("human", sessionID, "ordinary", "review the auth PR")
	if err != nil {
		t.Fatalf("Send ordinary mail: %v", err)
	}
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv(managedSessionHookEnv, "1")
	t.Setenv("GC_HOOK_SOURCE", "startup")
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
	t.Setenv(startupPromptDeliveredEnv, "1")
	withPrimeHookStdin(t)

	var stdout, stderr bytes.Buffer
	if code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatCodex, false); code != 0 {
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

	// The auto-handoff is rendered by sessionStartAutoHandoffInjection and must
	// NOT be rendered a second time by the ordinary-mail block.
	if n := strings.Count(context, auto.ID); n != 1 {
		t.Fatalf("additionalContext contains auto-handoff %q %d time(s), want exactly 1:\n%s", auto.ID, n, context)
	}
	// Ordinary unread mail is surfaced at SessionStart so a promptless wake is
	// not blind to it.
	for _, want := range []string{ordinary.ID, ordinary.Body} {
		if !strings.Contains(context, want) {
			t.Fatalf("additionalContext = %q, want ordinary-mail substring %q", context, want)
		}
	}
	// ...and surfacing it is read-only: it stays in the store for the
	// UserPromptSubmit delivery that archives it.
	if _, err := store.Get(ordinary.ID); err != nil {
		t.Fatalf("ordinary mail must remain open after a SessionStart injection: %v", err)
	}
}

// TestDoPrimeWithHook_JSONModeDoesNotArchiveAutoHandoff pins the preview
// contract of `gc prime --hook --json`: it renders exactly what the hook would
// emit, including durable auto-handoff mail, but must not consume it. The
// --json path buffers into a strings.Builder whose writes never fail, so a
// consuming run would archive the handoff before the real stdout write — and
// even on success would eat the continuation the next SessionStart must deliver.
func TestDoPrimeWithHook_JSONModeDoesNotArchiveAutoHandoff(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	promptDir := filepath.Join(cityDir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(promptDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "worker.md"), []byte("launch-only startup prompt\n"), 0o644); err != nil {
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

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "worker")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_NAME", "gastown--worker")
	sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")
	auto, ok := createHandoffMail(store, store, events.Discard, sessionID, sessionID,
		[]string{"context cycle", "continue the durable task"}, "context cycle",
		[]string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel}, &bytes.Buffer{})
	if !ok {
		t.Fatal("createHandoffMail(auto) failed")
	}
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv(managedSessionHookEnv, "1")
	t.Setenv("GC_HOOK_SOURCE", "startup")
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
	t.Setenv(startupPromptDeliveredEnv, "1")
	withPrimeHookStdin(t)

	var stdout, stderr bytes.Buffer
	cmd := newPrimeCmd(&stdout, &stderr)
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", "--hook", "--hook-format", "codex"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc prime --json --hook = %v; stderr=%q", err, stderr.String())
	}

	var got primeJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v; stdout=%q", err, stdout.String())
	}
	// The preview must be faithful: the handoff the hook would emit is present.
	for _, want := range []string{auto.ID, auto.Subject, auto.Body} {
		if !strings.Contains(got.Content, want) {
			t.Fatalf("content = %q, want auto-handoff substring %q", got.Content, want)
		}
	}
	// ...but previewing must not consume it.
	if _, err := store.Get(auto.ID); err != nil {
		t.Fatalf("--json preview must leave auto-handoff durable, got err=%v", err)
	}

	// The real hook invocation still consumes it, so the preview did not
	// merely mark the mail read in a way that suppresses later delivery.
	var hookStdout bytes.Buffer
	if code := doPrimeWithHookFormat(nil, &hookStdout, &stderr, true, "codex", false); code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(hookStdout.String(), auto.ID) {
		t.Fatalf("hook output = %q, want auto-handoff %q", hookStdout.String(), auto.ID)
	}
	if _, err := store.Get(auto.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("auto-handoff should be archived after the real SessionStart injection, got err=%v", err)
	}
}

func TestDoPrimeWithHookFormat_GatesDefaultFallbackWithoutManagedSession(t *testing.T) {
	t.Setenv("GC_CITY", filepath.Join(t.TempDir(), "missing-city"))
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatCodex, false)
	if code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}

	if out := stdout.String(); out != "" {
		t.Fatalf("stdout = %q, want no hook output for unmanaged SessionStart hook", out)
	}
}

func TestDoPrimeExplicitInvocationStillFormatsDefaultFallback(t *testing.T) {
	t.Setenv("GC_CITY", filepath.Join(t.TempDir(), "missing-city"))
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_TEMPLATE", "")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithHookFormat(nil, &stdout, &stderr, false, "", false)
	if code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "# Gas City Agent") {
		t.Fatalf("stdout = %q, want default prime prompt", out)
	}
	for _, want := range []string{
		"You are an agent in a Gas City workspace. Claim available work and execute it.",
		"`gc hook --claim --json`",
		"`bd show <id>`",
		"`bd close <id>`",
		"Read the claimed bead and execute the work described in its title",
		"Check for more work. Repeat until the queue is empty.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestDoPrimeWithHook_DeliveredStartupPromptCodexJSONHookFormat(t *testing.T) {
	skipSlowCmdGCTest(t, "starts real Dolt lifecycle")
	clearGCEnv(t)
	clearInheritedBeadsEnv(t)
	clearInheritedCityRoutingEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	cleanupManagedDoltTestCity(t, cityDir)
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
	sessionID := createPrimeHookSession(t, cityDir, "gastown--worker", "worker")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv(managedSessionHookEnv, "1")
	t.Setenv("GC_HOOK_SOURCE", "startup")
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
	t.Setenv(startupPromptDeliveredEnv, "1")
	withPrimeHookStdin(t)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, "codex", false)
	if code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}

	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("hook output is not JSON: %v; stdout=%q", err, stdout.String())
	}
	if got.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hookEventName = %q, want SessionStart", got.HookSpecificOutput.HookEventName)
	}
	context := got.HookSpecificOutput.AdditionalContext
	if strings.Contains(context, promptContent) {
		t.Fatalf("additionalContext = %q, want no repeated startup prompt", context)
	}
	if !strings.Contains(context, "[gastown] worker") {
		t.Fatalf("additionalContext = %q, want hook beacon", context)
	}
}

func TestDoPrimeWithHook_CodexJSONFormatInfersAgentFromWorkDir(t *testing.T) {
	skipSlowCmdGCTest(t, "starts real Dolt lifecycle")
	for _, tt := range []struct {
		name        string
		identity    string
		agentDir    string
		agentName   string
		promptFile  string
		promptText  string
		beaconAgent string
	}{
		{
			name:        "city scoped",
			identity:    "mayor",
			agentName:   "mayor",
			promptFile:  "prompts/mayor.md",
			promptText:  "mayor startup prompt\n",
			beaconAgent: "mayor",
		},
		{
			name:        "rig scoped",
			identity:    "hello-world/witness",
			agentDir:    "hello-world",
			agentName:   "witness",
			promptFile:  "prompts/witness.md",
			promptText:  "witness startup prompt\n",
			beaconAgent: "hello-world/witness",
		},
		{
			name:        "workflow style",
			identity:    "gascity/workflows.codex-max",
			agentDir:    "gascity",
			agentName:   "workflows.codex-max",
			promptFile:  "prompts/codex-max.md",
			promptText:  "codex-max startup prompt\n",
			beaconAgent: "gascity/workflows.codex-max",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearGCEnv(t)
			clearInheritedBeadsEnv(t)
			clearInheritedCityRoutingEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_TEMPLATE", "")
			t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
			withPrimeHookStdin(t)

			cityDir := t.TempDir()
			cleanupManagedDoltTestCity(t, cityDir)
			agentWorkDirParts := append([]string{cityDir, ".gc", "agents"}, strings.Split(tt.identity, "/")...)
			agentWorkDir := filepath.Join(agentWorkDirParts...)
			if err := os.MkdirAll(agentWorkDir, 0o755); err != nil {
				t.Fatalf("MkdirAll(agentWorkDir): %v", err)
			}
			promptPath := filepath.Join(cityDir, tt.promptFile)
			if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
				t.Fatalf("MkdirAll(prompt dir): %v", err)
			}
			if err := os.WriteFile(promptPath, []byte(tt.promptText), 0o644); err != nil {
				t.Fatalf("WriteFile(prompt): %v", err)
			}
			cityTOML := fmt.Sprintf(`
[workspace]
name = "gastown"

[[agent]]
name = %q
dir = %q
prompt_template = %q
`, tt.agentName, tt.agentDir, tt.promptFile)
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
				t.Fatalf("WriteFile(city.toml): %v", err)
			}
			sessionName := "session-" + strings.ReplaceAll(tt.beaconAgent, "/", "-")
			sessionID := createPrimeHookSession(t, cityDir, sessionName, tt.beaconAgent)
			t.Setenv("GC_SESSION_ID", sessionID)
			t.Setenv("GC_SESSION_NAME", sessionName)
			t.Chdir(agentWorkDir)

			var stdout, stderr bytes.Buffer
			code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatCodex, false)
			if code != 0 {
				t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
			}

			var got struct {
				HookSpecificOutput struct {
					HookEventName     string `json:"hookEventName"`
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("hook output is not JSON: %v; stdout=%q", err, stdout.String())
			}
			if got.HookSpecificOutput.HookEventName != "SessionStart" {
				t.Fatalf("hookEventName = %q, want SessionStart", got.HookSpecificOutput.HookEventName)
			}
			context := got.HookSpecificOutput.AdditionalContext
			if !strings.Contains(context, strings.TrimSpace(tt.promptText)) {
				t.Fatalf("additionalContext = %q, want prompt %q", context, strings.TrimSpace(tt.promptText))
			}
			if !strings.Contains(context, "[gastown] "+tt.beaconAgent) {
				t.Fatalf("additionalContext = %q, want hook beacon for %s", context, tt.beaconAgent)
			}
		})
	}
}

func withPrimeHookStdin(t *testing.T) {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
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

func createPrimeHookSession(t *testing.T, cityDir, sessionName, template string) string {
	t.Helper()

	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%s): %v", cityDir, err)
	}
	created, err := store.Create(beads.Bead{
		Title:  sessionName,
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:" + template},
		Metadata: map[string]string{
			"agent_name":   template,
			"session_name": sessionName,
			"state":        "active",
			"template":     template,
		},
	})
	if err != nil {
		t.Fatalf("Create(session %s): %v", sessionName, err)
	}
	if strings.TrimSpace(created.ID) == "" {
		t.Fatalf("Create(session %s) returned empty ID", sessionName)
	}
	return created.ID
}
