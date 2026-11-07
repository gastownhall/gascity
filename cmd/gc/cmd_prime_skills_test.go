package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePrimeSkillsCity lays out a city whose pack ships one shared skill and
// whose workspace provider has a vendor skill sink, so a rendered prompt can
// legitimately carry the assigned-skills appendix. The agent lives in
// agents/worker/agent.toml because a schema-2 pack rejects [[agent]] tables
// in city.toml. An empty templateBody models a template that legitimately
// renders to nothing.
func writePrimeSkillsCity(t *testing.T, cityTOML, agentTOML, templateBody string) string {
	t.Helper()

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
	write("pack.toml", "[pack]\nname = \"skills-city\"\nversion = \"0.1.0\"\nschema = 2\n")
	write("city.toml", cityTOML)
	write("agents/worker/agent.toml", agentTOML)
	write("prompts/worker.md", templateBody)
	write("skills/plan/SKILL.md", "---\nname: plan\ndescription: Plan the work\n---\nbody\n")
	return cityDir
}

const primeSkillsCityTOML = `
[workspace]
name = "skills-city"
provider = "opencode"

[providers.opencode]
base = "builtin:opencode"
`

const primeSkillsAgentTOML = "prompt_template = \"prompts/worker.md\"\n"

// TestDoPrimeWithHook_AppendsAssignedSkillsWhenRuntimeDelivers pins that the
// hook copy of the role carries the same assigned-skills appendix the launch
// path appends in resolveTemplate, under the same skills-materialization
// gate: a provider with a vendor sink, an agent that has not opted out, and a
// session runtime that materializes the skills into the session workdir. The
// opencode-style hook (delivered marker set, no managed SessionStart markers)
// supplies the role to every generation, so once the resume path stops
// replaying the role this copy is the only one that reaches a resumed
// session.
func TestDoPrimeWithHook_AppendsAssignedSkillsWhenRuntimeDelivers(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	const appendixHeading = "## Skills available to this session"

	for _, tc := range []struct {
		name         string
		cityTOML     string
		agentTOML    string
		templateBody string
		hookMode     bool
		managedHook  string
		hookEvent    string
		gcDir        func(cityDir string) string
		wantPrompt   bool
		// wantDefault expects the builtin run-once fallback prompt instead of
		// the rendered template (promptless or empty-template agents).
		wantDefault  bool
		wantAppendix bool
	}{
		{
			name:         "per-turn hook copy carries the appendix",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    primeSkillsAgentTOML,
			hookMode:     true,
			wantPrompt:   true,
			wantAppendix: true,
		},
		{
			name:         "managed SessionStart suppression drops prompt and appendix together",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    primeSkillsAgentTOML,
			hookMode:     true,
			managedHook:  "1",
			hookEvent:    "SessionStart",
			wantPrompt:   false,
			wantAppendix: false,
		},
		{
			name:         "explicit gc prime renders the template body only",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    primeSkillsAgentTOML,
			hookMode:     false,
			wantPrompt:   true,
			wantAppendix: false,
		},
		{
			name:         "agent opt-out is honored",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    primeSkillsAgentTOML + "inject_assigned_skills = false\n",
			hookMode:     true,
			wantPrompt:   true,
			wantAppendix: false,
		},
		{
			name: "runtime that cannot deliver skills gets no appendix",
			cityTOML: `
[workspace]
name = "skills-city"
provider = "opencode"

[providers.opencode]
base = "builtin:opencode"

[session]
provider = "k8s"
`,
			agentTOML:    primeSkillsAgentTOML,
			hookMode:     true,
			wantPrompt:   true,
			wantAppendix: false,
		},
		{
			name:         "workdir outside the scope root on a stage-2 runtime still delivers",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    primeSkillsAgentTOML,
			hookMode:     true,
			gcDir:        func(cityDir string) string { return filepath.Join(cityDir, ".gc", "agents", "worker") },
			wantPrompt:   true,
			wantAppendix: true,
		},
		{
			// A promptless agent falls through to the default run-once prompt.
			// The launch path still appends the appendix to that prompt, and
			// the hook copy is what a resumed session sees, so the fallback
			// has to carry it too.
			name:         "promptless agent fallback carries the appendix",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    "",
			hookMode:     true,
			wantDefault:  true,
			wantAppendix: true,
		},
		{
			name:         "empty template fallback carries the appendix",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    primeSkillsAgentTOML,
			templateBody: "",
			hookMode:     true,
			wantDefault:  true,
			wantAppendix: true,
		},
		{
			name:         "promptless agent fallback under managed SessionStart suppression",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    "",
			hookMode:     true,
			managedHook:  "1",
			hookEvent:    "SessionStart",
			wantDefault:  false,
			wantAppendix: false,
		},
		{
			name:         "promptless agent explicit gc prime renders the default only",
			cityTOML:     primeSkillsCityTOML,
			agentTOML:    "",
			hookMode:     false,
			wantDefault:  true,
			wantAppendix: false,
		},
		{
			name: "provider without a vendor sink gets no appendix",
			cityTOML: `
[workspace]
name = "skills-city"
provider = "copilot"

[providers.copilot]
base = "builtin:copilot"
`,
			agentTOML:    primeSkillsAgentTOML,
			hookMode:     true,
			wantPrompt:   true,
			wantAppendix: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			templateBody := "role prompt body\n"
			if tc.wantDefault {
				templateBody = tc.templateBody
			}
			cityDir := writePrimeSkillsCity(t, tc.cityTOML, tc.agentTOML, templateBody)
			withPrimeHookStdin(t)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_SESSION_NAME", "skills-city--worker")
			sessionID := createPrimeHookSession(t, cityDir, "skills-city--worker", "worker")
			t.Setenv("GC_SESSION_ID", sessionID)
			gcDir := cityDir
			if tc.gcDir != nil {
				gcDir = tc.gcDir(cityDir)
			}
			t.Setenv("GC_DIR", gcDir)
			t.Setenv(managedSessionHookEnv, tc.managedHook)
			t.Setenv("GC_HOOK_EVENT_NAME", tc.hookEvent)
			t.Setenv(startupPromptDeliveredEnv, "1")

			var stdout, stderr bytes.Buffer
			var code int
			if tc.hookMode {
				code = doPrimeWithMode(nil, &stdout, &stderr, true, false)
			} else {
				code = doPrime([]string{"worker"}, &stdout, &stderr)
			}
			if code != 0 {
				t.Fatalf("prime exit = %d, want 0; stderr=%q", code, stderr.String())
			}
			out := stdout.String()
			if got := strings.Contains(out, "role prompt body"); got != tc.wantPrompt {
				t.Fatalf("stdout = %q, prompt present = %v, want %v; stderr=%q", out, got, tc.wantPrompt, stderr.String())
			}
			if got := strings.Contains(out, defaultPrimePrompt); got != tc.wantDefault {
				t.Fatalf("stdout = %q, default prompt present = %v, want %v; stderr=%q", out, got, tc.wantDefault, stderr.String())
			}
			bodyMarker := "role prompt body"
			if tc.wantDefault {
				bodyMarker = defaultPrimePrompt
			}
			if got := strings.Contains(out, appendixHeading); got != tc.wantAppendix {
				t.Fatalf("stdout = %q, appendix present = %v, want %v", out, got, tc.wantAppendix)
			}
			if !tc.wantAppendix {
				return
			}
			if !strings.Contains(out, "`plan` — Plan the work") {
				t.Fatalf("stdout = %q, want the shared skill listed", out)
			}
			if strings.Index(out, appendixHeading) < strings.Index(out, bodyMarker) {
				t.Fatalf("stdout = %q, want the appendix after the rendered role", out)
			}
			if strings.Count(out, appendixHeading) != 1 {
				t.Fatalf("stdout = %q, want the appendix exactly once", out)
			}
		})
	}
}
