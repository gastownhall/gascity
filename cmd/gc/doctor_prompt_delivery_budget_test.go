package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// startupPromptOverhead is the number of bytes launch adds ahead of a
// non-empty rendered prompt for agentName in the "demo" fixture city: the
// beacon plus the blank line separating it from the prompt. The beacon
// carries no single quotes, so it adds the same count to the raw and
// argv-encoded lengths.
func startupPromptOverhead(agentName string) int {
	return len(runtime.FormatBeaconAt("demo", agentName, false, time.Time{})) + len("\n\n")
}

// clearPromptDeliveryBudgetEnv clears the ambient GC_* variables that
// buildPrimeContextFor reads directly (GC_ALIAS, GC_AGENT, GC_DIR, GC_RIG,
// GC_RIG_ROOT), auto-restored by t.Setenv on test cleanup. Without this, a
// test run from inside a real gc-managed session (which sets these) would
// leak ambient rig/agent identity into what should be a hermetic fixture.
// Every reader of these vars (cmd_prime.go's buildPrimeContextFor) treats
// an empty value the same as unset (`os.Getenv(k) != ""`), so t.Setenv(k,
// "") is behaviorally equivalent to unsetting.
func clearPromptDeliveryBudgetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GC_ALIAS", "GC_AGENT", "GC_DIR", "GC_RIG", "GC_RIG_ROOT"} {
		t.Setenv(k, "")
	}
}

// fakePromptDeliveryLookPath always fails. Every fixture in this file
// resolves its provider via the StartCommand escape hatch, which never
// calls lookPath, so a real PATH lookup is never exercised here.
func fakePromptDeliveryLookPath(string) (string, error) {
	return "", fmt.Errorf("fakePromptDeliveryLookPath: binary lookup is not available in tests")
}

// writePromptFile writes a prompt fixture at cityPath/relPath (creating
// parent directories as needed) and returns relPath, ready to assign to
// config.Agent.PromptTemplate (which is resolved relative to the city dir).
func writePromptFile(t *testing.T, cityPath, relPath, content string) string {
	t.Helper()
	full := filepath.Join(cityPath, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("writePromptFile: mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("writePromptFile: write: %v", err)
	}
	return relPath
}

// promptFixtureAgent builds a config.Agent that resolves its provider via
// the StartCommand escape hatch (so cfg.Providers/lookPath never come into
// play), with an explicit promptMode ("arg" unless a test needs "none") and
// session (the runtime-name input to promptDeliverySupportFor).
func promptFixtureAgent(name, promptTemplate, session, promptMode string) config.Agent {
	return config.Agent{
		Name:           name,
		StartCommand:   "true",
		PromptMode:     promptMode,
		Session:        session,
		PromptTemplate: promptTemplate,
	}
}

func runPromptDeliveryBudgetCheck(t *testing.T, cfg *config.City, cityPath string) *doctor.CheckResult {
	t.Helper()
	check := newPromptDeliveryBudgetDoctorCheck(cityPath, cfg, fakePromptDeliveryLookPath)
	return check.Run(&doctor.CheckContext{CityPath: cityPath})
}

func joinedDetails(res *doctor.CheckResult) string {
	return strings.Join(res.Details, "\n")
}

func TestPromptDeliveryBudgetCheck_NilConfig(t *testing.T) {
	check := newPromptDeliveryBudgetDoctorCheck("", nil, fakePromptDeliveryLookPath)
	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("nil config: status = %v, want StatusOK; message=%q", res.Status, res.Message)
	}
}

func TestPromptDeliveryBudgetCheck_NoAgents(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("no agents: status = %v, want StatusOK; message=%q details=%v", res.Status, res.Message, res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_SafePrompt_RawThreshold(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	// raw=99999 (<100000) and quoted=100001 (<128000) once the beacon is prepended: safe.
	body := strings.Repeat("a", maxPromptSuffixRawBytes-1-startupPromptOverhead("safe-raw"))
	tmpl := writePromptFile(t, cityPath, "prompts/safe-raw.md", body)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{promptFixtureAgent("safe-raw", tmpl, "subprocess", "arg")},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("safe raw-threshold prompt: status = %v, want StatusOK; message=%q details=%v", res.Status, res.Message, res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_SafePrompt_QuotedThreshold(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	// quoted=overhead+4*n+2 stays under 128000 once the beacon is prepended: safe.
	body := strings.Repeat("'", (maxPromptSuffixQuotedBytes-1-2-startupPromptOverhead("safe-quoted"))/4)
	tmpl := writePromptFile(t, cityPath, "prompts/safe-quoted.md", body)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{promptFixtureAgent("safe-quoted", tmpl, "subprocess", "arg")},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("safe quoted-threshold prompt: status = %v, want StatusOK; message=%q details=%v", res.Status, res.Message, res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_OversizedRaw_UnsupportedRuntime(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("a", 100000) // raw=100000 (>=100000): trips the raw guard
	tmpl := writePromptFile(t, cityPath, "prompts/oversized-raw.md", body)
	agent := promptFixtureAgent("oversized-raw-unsupported", tmpl, "subprocess", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusError {
		t.Fatalf("oversized raw on unsupported runtime: status = %v, want StatusError; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, agent.Name) {
		t.Errorf("details missing agent name %q: %v", agent.Name, res.Details)
	}
	if !strings.Contains(details, "subprocess") {
		t.Errorf("details missing runtime name %q: %v", "subprocess", res.Details)
	}
	if !strings.Contains(details, "hard-fail") {
		t.Errorf("details missing hard-fail classification: %v", res.Details)
	}
	if !strings.Contains(details, "configured_mode=arg") {
		t.Errorf("details missing configured_mode=arg: %v", res.Details)
	}
	if !strings.Contains(details, "effective_mode=hard-fail") {
		t.Errorf("details missing effective_mode=hard-fail: %v", res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_OversizedQuoted_UnsupportedRuntime(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("'", 32000) // raw=32000 (safe), quoted=4*32000+2=128002 (>=128000): trips the quoted guard
	tmpl := writePromptFile(t, cityPath, "prompts/oversized-quoted.md", body)
	agent := promptFixtureAgent("oversized-quoted-unsupported", tmpl, "subprocess", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusError {
		t.Fatalf("oversized quoted on unsupported runtime: status = %v, want StatusError; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, agent.Name) {
		t.Errorf("details missing agent name %q: %v", agent.Name, res.Details)
	}
	if !strings.Contains(details, "hard-fail") {
		t.Errorf("details missing hard-fail classification: %v", res.Details)
	}
	if !strings.Contains(details, "configured_mode=arg") {
		t.Errorf("details missing configured_mode=arg: %v", res.Details)
	}
	if !strings.Contains(details, "effective_mode=hard-fail") {
		t.Errorf("details missing effective_mode=hard-fail: %v", res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_OversizedRaw_NudgeFallbackRuntime(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("a", 100000)
	tmpl := writePromptFile(t, cityPath, "prompts/oversized-raw-tmux.md", body)
	agent := promptFixtureAgent("oversized-raw-tmux", tmpl, "tmux", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusWarning {
		t.Fatalf("oversized raw on nudge-fallback runtime: status = %v, want StatusWarning; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, agent.Name) {
		t.Errorf("details missing agent name %q: %v", agent.Name, res.Details)
	}
	if !strings.Contains(details, "tmux") {
		t.Errorf("details missing runtime name %q: %v", "tmux", res.Details)
	}
	if !strings.Contains(details, "nudge") {
		t.Errorf("details missing nudge-fallback classification: %v", res.Details)
	}
	if !strings.Contains(details, "configured_mode=arg") {
		t.Errorf("details missing configured_mode=arg: %v", res.Details)
	}
	if !strings.Contains(details, "effective_mode=nudge-fallback") {
		t.Errorf("details missing effective_mode=nudge-fallback: %v", res.Details)
	}
	// body is 100000 'a's behind the beacon: raw=100000+overhead (trips the
	// raw guard), quoted=raw+2 (no embedded quotes to escape).
	rawBytes := 100000 + startupPromptOverhead(agent.Name)
	if want := fmt.Sprintf("raw_bytes=%d", rawBytes); !strings.Contains(details, want) {
		t.Errorf("details missing %s: %v", want, res.Details)
	}
	if !strings.Contains(details, "raw_limit=100000") {
		t.Errorf("details missing raw_limit=100000: %v", res.Details)
	}
	if want := fmt.Sprintf("argv_bytes=%d", rawBytes+2); !strings.Contains(details, want) {
		t.Errorf("details missing %s: %v", want, res.Details)
	}
	if !strings.Contains(details, "argv_limit=128000") {
		t.Errorf("details missing argv_limit=128000: %v", res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_OversizedQuoted_NudgeFallbackRuntime(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("'", 32000)
	tmpl := writePromptFile(t, cityPath, "prompts/oversized-quoted-default.md", body)
	// Session left empty; cfg.Session.Provider is also empty (zero value), so
	// effectiveSessionProvider resolves to "" — one of the built-in
	// nudge-fallback runtime names alongside "tmux"/"herdr"/"k8s"/"hybrid".
	agent := promptFixtureAgent("oversized-quoted-default", tmpl, "", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusWarning {
		t.Fatalf("oversized quoted on default (empty) runtime: status = %v, want StatusWarning; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, agent.Name) {
		t.Errorf("details missing agent name %q: %v", agent.Name, res.Details)
	}
	if !strings.Contains(details, "nudge") {
		t.Errorf("details missing nudge-fallback classification: %v", res.Details)
	}
	if !strings.Contains(details, "configured_mode=arg") {
		t.Errorf("details missing configured_mode=arg: %v", res.Details)
	}
	if !strings.Contains(details, "effective_mode=nudge-fallback") {
		t.Errorf("details missing effective_mode=nudge-fallback: %v", res.Details)
	}
	// body is 32000 "'"s behind the beacon: raw=32000+overhead (safe),
	// quoted=overhead+4*32000+2 (each embedded ' becomes '\'', trips the
	// quoted guard past its limit).
	overhead := startupPromptOverhead(agent.Name)
	if want := fmt.Sprintf("raw_bytes=%d", 32000+overhead); !strings.Contains(details, want) {
		t.Errorf("details missing %s: %v", want, res.Details)
	}
	if !strings.Contains(details, "raw_limit=100000") {
		t.Errorf("details missing raw_limit=100000: %v", res.Details)
	}
	if want := fmt.Sprintf("argv_bytes=%d", overhead+128002); !strings.Contains(details, want) {
		t.Errorf("details missing %s: %v", want, res.Details)
	}
	if !strings.Contains(details, "argv_limit=128000") {
		t.Errorf("details missing argv_limit=128000: %v", res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_ACPSession_BypassesSizeGuard(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("a", 100000) // oversized by raw count, but isACP short-circuits before the guard
	tmpl := writePromptFile(t, cityPath, "prompts/acp.md", body)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{promptFixtureAgent("acp-agent", tmpl, "acp", "arg")},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("ACP session with oversized body: status = %v, want StatusOK (ACP bypasses the size guard entirely); message=%q details=%v", res.Status, res.Message, res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_PromptModeNone_BypassesSizeGuard(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("a", 100000) // oversized by raw count, but PromptMode "none" short-circuits before the guard
	tmpl := writePromptFile(t, cityPath, "prompts/promptmode-none.md", body)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{promptFixtureAgent("none-mode-agent", tmpl, "subprocess", "none")},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("prompt_mode=none with oversized body: status = %v, want StatusOK (\"none\" bypasses the size guard entirely); message=%q details=%v", res.Status, res.Message, res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_UnresolvableProvider_DoesNotBlockDeliveryCheck(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	tmpl := writePromptFile(t, cityPath, "prompts/badprovider.md", "short prompt body")
	agent := config.Agent{
		Name:           "badprovider-agent",
		Provider:       "does-not-exist",
		PromptMode:     "arg",
		PromptTemplate: tmpl,
		// No StartCommand: forces provider-name resolution, which fails
		// because "does-not-exist" is absent from cfg.Providers (nil/empty).
		// ResolveProvider's error is intentionally ignored here (mirroring
		// cmd_prime.go), so this agent's prompt is still judged purely on
		// delivery-budget merits: a short, well-formed prompt should clear
		// with StatusOK rather than being hard-failed for an unrelated,
		// out-of-scope provider misconfiguration (that's provider-catalog's
		// job, not this check's).
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("unresolvable provider with a safe prompt: status = %v, want StatusOK; message=%q details=%v", res.Status, res.Message, res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_RenderError(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	// .template.md forces actual Go-template execution (unlike a plain .md
	// passthrough). buildTemplateData flattens PromptContext into a
	// map[string]string, and the "missingkey=zero" option silently zeroes an
	// absent top-level key rather than erroring — so a bare {{.NoSuchField}}
	// would NOT fail. Chaining a further field access onto that zeroed
	// string result does fail (a string has no fields), so tmpl.Execute
	// errors and renderPromptWithMeta falls back to the raw unrendered body
	// (tiny, nowhere near either size threshold).
	tmpl := writePromptFile(t, cityPath, "prompts/broken.template.md", "{{.NoSuchField.Nested}}")
	agent := promptFixtureAgent("render-error-agent", tmpl, "subprocess", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusWarning {
		t.Fatalf("template render error: status = %v, want StatusWarning; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, agent.Name) {
		t.Errorf("details missing agent name %q: %v", agent.Name, res.Details)
	}
	if !strings.Contains(details, "render") {
		t.Errorf("details missing render-warning wording: %v", res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_MultipleAgents_WorstCaseWins(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	okTmpl := writePromptFile(t, cityPath, "prompts/multi-ok.md", "short and safe")
	hardFailTmpl := writePromptFile(t, cityPath, "prompts/multi-hardfail.md", strings.Repeat("a", 100000))

	okAgent := promptFixtureAgent("multi-ok-agent", okTmpl, "subprocess", "arg")
	hardFailAgent := promptFixtureAgent("multi-hardfail-agent", hardFailTmpl, "subprocess", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{okAgent, hardFailAgent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusError {
		t.Fatalf("mixed OK+hard-fail agents: status = %v, want StatusError (worst case wins); message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, hardFailAgent.Name) {
		t.Errorf("details missing the hard-fail agent's name %q: %v", hardFailAgent.Name, res.Details)
	}
	if strings.Contains(details, okAgent.Name) {
		t.Errorf("details unexpectedly mention the OK agent %q, which contributed nothing: %v", okAgent.Name, res.Details)
	}
}

func TestPromptDeliveryBudgetCheck_SuspendedAgentSkipped(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	tmpl := writePromptFile(t, cityPath, "prompts/suspended-hardfail.md", strings.Repeat("a", 100000))
	agent := promptFixtureAgent("suspended-agent", tmpl, "subprocess", "arg")
	agent.Suspended = true
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("suspended agent with an otherwise hard-fail prompt: status = %v, want StatusOK (suspended agents are skipped entirely); message=%q details=%v", res.Status, res.Message, res.Details)
	}
}

// The wiring is the fix: promptDeliveryBudgetDoctorCheck only runs at all if
// buildDoctorChecks registers it, mirroring
// TestBuildDoctorChecksRegistersRigWorktreesCheck's pattern for a different
// check.
func TestPromptDeliveryBudgetCheck_RegisteredInDoctorChecks(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	names := doctorCheckNames(checks)
	if doctorCheckIndex(names, "prompt-delivery-budget") < 0 {
		t.Errorf("prompt-delivery-budget not registered; names=%v", names)
	}
}

// TestPromptDeliveryBudgetCheck_CanFixIsFalse guards the CanFix() contract
// directly: this check only surfaces oversized/broken prompt templates for a
// human to edit, so it must never claim it can auto-fix them.
func TestPromptDeliveryBudgetCheck_CanFixIsFalse(t *testing.T) {
	check := newPromptDeliveryBudgetDoctorCheck("", nil, fakePromptDeliveryLookPath)
	if check.CanFix() {
		t.Fatalf("CanFix() = true, want false: prompt-template fixes require human judgment about content, not an automated rewrite")
	}
}

// TestPromptDeliveryBudgetCheck_NoPromptLeakageInDetails guards against the
// check's Details/Message ever embedding the offending prompt's raw content.
// The classification messages are built from fmt.Sprintf with the agent name
// and runtime, never the prompt text itself, but a future edit that adds
// %v-of-the-prompt for debugging would leak arbitrarily large (and
// potentially sensitive) template output into doctor's summary surface.
func TestPromptDeliveryBudgetCheck_NoPromptLeakageInDetails(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("a", 100000) // raw=100000: trips the hard-fail guard (see OversizedRaw_UnsupportedRuntime)
	tmpl := writePromptFile(t, cityPath, "prompts/leakage.md", body)
	agent := promptFixtureAgent("leakage-agent", tmpl, "subprocess", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusError {
		t.Fatalf("setup check: status = %v, want StatusError (fixture must actually hard-fail for this test to be meaningful); message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if strings.Contains(details, body) {
		t.Errorf("check details leak the raw 100000-byte prompt filler verbatim -- doctor output must summarize, never embed full prompt content (details len=%d)", len(details))
	}
	if strings.Contains(res.Message, body) {
		t.Errorf("check message leaks the raw 100000-byte prompt filler verbatim: message=%q", res.Message)
	}
}

// TestPromptDeliveryBudgetCheck_MultiRig_PackDirsScopedPerAgent exercises
// config.City.PackDirsForRig's rigName != "" branch end to end through the
// check's own Run() -- every other fixture in this file leaves Agent.Dir
// unset, so ctx.RigName is always "" and the check always takes the
// AllPackDirs() (union of every rig) branch instead. Two rigs each import a
// pack dir defining the SAME fragment name ("rig-marker") with very
// different rendered sizes: if packDirs ever leaked rig beta's pack dir into
// rig alpha's resolution (or vice versa), alpha's tiny fragment and beta's
// oversized one would collide and either both or neither agent would
// hard-fail, instead of exactly beta.
func TestPromptDeliveryBudgetCheck_MultiRig_PackDirsScopedPerAgent(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()

	alphaPackDir := t.TempDir()
	betaPackDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(alphaPackDir, "template-fragments"), 0o755); err != nil {
		t.Fatalf("mkdir alpha template-fragments: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(betaPackDir, "template-fragments"), 0o755); err != nil {
		t.Fatalf("mkdir beta template-fragments: %v", err)
	}
	smallFragment := `{{define "rig-marker"}}small{{end}}`
	largeFragment := `{{define "rig-marker"}}` + strings.Repeat("a", 100000) + `{{end}}`
	if err := os.WriteFile(filepath.Join(alphaPackDir, "template-fragments", "rig-marker.template.md"), []byte(smallFragment), 0o644); err != nil {
		t.Fatalf("write alpha fragment: %v", err)
	}
	if err := os.WriteFile(filepath.Join(betaPackDir, "template-fragments", "rig-marker.template.md"), []byte(largeFragment), 0o644); err != nil {
		t.Fatalf("write beta fragment: %v", err)
	}

	// .template.md forces real template execution (see RenderError above),
	// so {{template "rig-marker" .}} actually resolves against whichever
	// pack dir PackDirsForRig hands this agent's own rig.
	tmpl := writePromptFile(t, cityPath, "prompts/multirig.template.md", `{{template "rig-marker" .}}`)

	alphaAgent := promptFixtureAgent("rig-alpha-agent", tmpl, "subprocess", "arg")
	alphaAgent.Dir = "alpha" // legacy dir-as-rig convention (workdir.ConfiguredRigName): Dir == a configured Rig.Name
	betaAgent := promptFixtureAgent("rig-beta-agent", tmpl, "subprocess", "arg")
	betaAgent.Dir = "beta"

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "alpha"}, {Name: "beta"}},
		RigPackDirs: map[string][]string{
			"alpha": {alphaPackDir},
			"beta":  {betaPackDir},
		},
		Agents: []config.Agent{alphaAgent, betaAgent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusError {
		t.Fatalf("rig-beta agent should hard-fail on its own rig's oversized fragment: status = %v, want StatusError; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, betaAgent.Name) {
		t.Errorf("details missing beta agent's name %q (its own rig's oversized fragment should hard-fail it): %v", betaAgent.Name, res.Details)
	}
	if strings.Contains(details, alphaAgent.Name) {
		t.Errorf("details unexpectedly mention alpha agent %q -- alpha's own rig fragment is tiny and safe; its presence here means rig beta's pack dir leaked into rig alpha's resolution: %v", alphaAgent.Name, res.Details)
	}
}

// TestPromptDeliveryBudgetCheck_IgnoresAmbientSessionEnv guards against the
// check rendering every agent with the identity of the session gc doctor
// runs in: with GC_RIG/GC_AGENT pointing at rig alpha, each agent must still
// resolve its own configured rig, so only rig beta's agent hard-fails.
func TestPromptDeliveryBudgetCheck_IgnoresAmbientSessionEnv(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	t.Setenv("GC_RIG", "alpha")
	t.Setenv("GC_AGENT", "someone-else")
	cityPath := t.TempDir()

	alphaPackDir := t.TempDir()
	betaPackDir := t.TempDir()
	for _, dir := range []string{alphaPackDir, betaPackDir} {
		if err := os.MkdirAll(filepath.Join(dir, "template-fragments"), 0o755); err != nil {
			t.Fatalf("mkdir template-fragments: %v", err)
		}
	}
	smallFragment := `{{define "rig-marker"}}small{{end}}`
	largeFragment := `{{define "rig-marker"}}` + strings.Repeat("a", 100000) + `{{end}}`
	if err := os.WriteFile(filepath.Join(alphaPackDir, "template-fragments", "rig-marker.template.md"), []byte(smallFragment), 0o644); err != nil {
		t.Fatalf("write alpha fragment: %v", err)
	}
	if err := os.WriteFile(filepath.Join(betaPackDir, "template-fragments", "rig-marker.template.md"), []byte(largeFragment), 0o644); err != nil {
		t.Fatalf("write beta fragment: %v", err)
	}
	tmpl := writePromptFile(t, cityPath, "prompts/multirig.template.md", `{{template "rig-marker" .}}`)

	alphaAgent := promptFixtureAgent("rig-alpha-agent", tmpl, "subprocess", "arg")
	alphaAgent.Dir = "alpha"
	betaAgent := promptFixtureAgent("rig-beta-agent", tmpl, "subprocess", "arg")
	betaAgent.Dir = "beta"

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "alpha"}, {Name: "beta"}},
		RigPackDirs: map[string][]string{
			"alpha": {alphaPackDir},
			"beta":  {betaPackDir},
		},
		Agents: []config.Agent{alphaAgent, betaAgent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusError {
		t.Fatalf("rig-beta agent should hard-fail on its own rig's fragment despite GC_RIG=alpha: status = %v, want StatusError; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, betaAgent.Name) {
		t.Errorf("details missing beta agent's name %q: %v", betaAgent.Name, res.Details)
	}
	if strings.Contains(details, alphaAgent.Name) {
		t.Errorf("details unexpectedly mention alpha agent %q: %v", alphaAgent.Name, res.Details)
	}
}

// TestPromptDeliveryBudgetCheck_CountsStartupBeacon guards that the check
// measures the startup prompt launch sends, beacon included: a rendered body
// just under the raw limit is pushed over it by the beacon launch prepends.
func TestPromptDeliveryBudgetCheck_CountsStartupBeacon(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	body := strings.Repeat("a", maxPromptSuffixRawBytes-10)
	tmpl := writePromptFile(t, cityPath, "prompts/beacon.md", body)
	agent := promptFixtureAgent("beacon-agent", tmpl, "subprocess", "arg")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusError {
		t.Fatalf("body 10 bytes under the raw limit plus beacon: status = %v, want StatusError; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	details := joinedDetails(res)
	if !strings.Contains(details, agent.Name) {
		t.Errorf("details missing agent name %q: %v", agent.Name, res.Details)
	}
	if want := fmt.Sprintf("prompt %d raw bytes", maxPromptSuffixRawBytes-10+startupPromptOverhead(agent.Name)); !strings.Contains(details, want) {
		t.Errorf("details missing %q (rendered body plus beacon): %v", want, res.Details)
	}
}

// TestPromptDeliveryBudgetCheck_SuppressedStartupPromptSkipped guards that an
// agent whose startup prompt launch suppresses (the deterministic control
// dispatcher) is not judged on a prompt it is never sent.
func TestPromptDeliveryBudgetCheck_SuppressedStartupPromptSkipped(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	tmpl := writePromptFile(t, cityPath, "prompts/control-dispatcher.md", strings.Repeat("a", 100000))
	agent := promptFixtureAgent(config.ControlDispatcherAgentName, tmpl, "subprocess", "arg")
	agent.StartCommand = "gc convoy control --serve"
	if !config.IsDeterministicControlDispatcher(&agent) {
		t.Fatalf("fixture agent is not a deterministic control dispatcher: %+v", agent)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{agent},
	}

	res := runPromptDeliveryBudgetCheck(t, cfg, cityPath)
	if res.Status != doctor.StatusOK {
		t.Fatalf("suppressed startup prompt with an oversized template: status = %v, want StatusOK; message=%q details=%v", res.Status, res.Message, res.Details)
	}
}
