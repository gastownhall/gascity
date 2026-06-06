//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_EnvVars_CityScoped verifies that agents receive GC_AGENT, GC_CITY,
// and GC_DIR env vars. Agents without rigs should NOT have GC_RIG.
func TestE2E_EnvVars_CityScoped(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "envtest", StartCommand: e2eReportScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "envtest", e2eDefaultTimeout())

	// GC_AGENT must match agent name.
	if !report.has("GC_AGENT", "envtest") {
		t.Errorf("GC_AGENT: got %v, want [envtest]", report.getAll("GC_AGENT"))
	}

	// GC_CITY must be the city directory.
	if !report.hasPath(t, "GC_CITY", cityDir) {
		t.Errorf("GC_CITY: got %v, want [%s]", report.getAll("GC_CITY"), cityDir)
	}

	// GC_DIR must be set (defaults to city dir when no dir configured).
	if !report.hasKey("GC_DIR") {
		t.Error("GC_DIR not found in report")
	}

	// No GC_RIG for city-scoped agents.
	if report.hasKey("GC_RIG") {
		t.Errorf("unexpected GC_RIG in city-scoped agent: %v", report.getAll("GC_RIG"))
	}
}

// TestE2E_EnvVars_Custom verifies that agent-level env vars propagate.
func TestE2E_EnvVars_Custom(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "customenv",
				StartCommand: e2eReportScript(),
				Env: map[string]string{
					"CUSTOM_FOO": "bar",
					"CUSTOM_BAZ": "qux",
				},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "customenv", e2eDefaultTimeout())

	if !report.has("CUSTOM_FOO", "bar") {
		t.Errorf("CUSTOM_FOO: got %v, want [bar]", report.getAll("CUSTOM_FOO"))
	}
	if !report.has("CUSTOM_BAZ", "qux") {
		t.Errorf("CUSTOM_BAZ: got %v, want [qux]", report.getAll("CUSTOM_BAZ"))
	}
}

// TestE2E_Dir_Default verifies that when no dir is configured, the agent's
// CWD is the city directory.
func TestE2E_Dir_Default(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "nodir", StartCommand: e2eReportScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "nodir", e2eDefaultTimeout())

	cwd := report.get("CWD")
	if !sameE2EPath(t, cwd, cityDir) {
		t.Errorf("CWD = %q, want %q (city directory)", cwd, cityDir)
	}
}

// TestE2E_Dir_Relative verifies that a relative dir is resolved relative
// to the city directory.
func TestE2E_Dir_Relative(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "reldir",
				StartCommand: e2eReportScript(),
				Dir:          "work/agent",
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	// QualifiedName = dir/name = "work/agent/reldir"
	report := waitForReport(t, cityDir, "work/agent/reldir", e2eDefaultTimeout())

	want := filepath.Join(cityDir, "work", "agent")
	cwd := report.get("CWD")
	if !sameE2EPath(t, cwd, want) {
		t.Errorf("CWD = %q, want %q", cwd, want)
	}
}

// TestE2E_Dir_GC_DIR verifies that GC_DIR env var is set to the resolved
// working directory (not the city dir) when dir is configured.
func TestE2E_Dir_GC_DIR(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "direnv",
				StartCommand: e2eReportScript(),
				Dir:          "subdir",
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	// QualifiedName = dir/name = "subdir/direnv"
	report := waitForReport(t, cityDir, "subdir/direnv", e2eDefaultTimeout())

	want := filepath.Join(cityDir, "subdir")
	gcDir := report.get("GC_DIR")
	if !sameE2EPath(t, gcDir, want) {
		t.Errorf("GC_DIR = %q, want %q", gcDir, want)
	}
	// GC_CITY should still be the city root.
	if !report.hasPath(t, "GC_CITY", cityDir) {
		t.Errorf("GC_CITY = %v, want [%s]", report.getAll("GC_CITY"), cityDir)
	}
}

// TestE2E_Overlay verifies that overlay_dir files are present in the
// agent's working directory before the agent starts.
func TestE2E_Overlay(t *testing.T) {
	// Set up city first to create overlay dir inside it.
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "overlay", StartCommand: e2eReportScript()},
		},
	}
	cityDir := setupE2ECityNoStart(t, city)

	// Create overlay directory with marker files.
	overlayRel := createOverlayDir(t, cityDir)

	// Update agent config with overlay_dir.
	city.Agents[0].OverlayDir = overlayRel
	rewriteE2ETomlPreservingNamedSessions(t, cityDir, city)

	// Start the city.
	out, err := gc("", "start", cityDir)
	if err != nil {
		t.Fatalf("gc start failed: %v\noutput: %s", err, out)
	}

	report := waitForReport(t, cityDir, "overlay", e2eDefaultTimeout())

	if !report.has("FILE_PRESENT", ".overlay-marker") {
		t.Error("overlay marker file not found in workdir")
	}
	if !report.has("FILE_PRESENT", "overlay-subdir/nested.txt") {
		t.Error("nested overlay file not found in workdir")
	}
}

func TestE2E_NoStartClearsReportsFromInit(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{Name: "nostart-report", StartCommand: e2eReportScript()},
		},
	}
	cityDir := setupE2ECityNoStart(t, city)
	reportDir := filepath.Join(cityDir, ".gc-reports")
	entries, err := os.ReadDir(reportDir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("reading report dir: %v", err)
	}
	if len(entries) > 0 {
		t.Fatalf("setupE2ECityNoStart left stale report files: %v", entries)
	}
}

// TestE2E_Hooks_Gemini verifies that install_agent_hooks=["gemini"] creates
// .gemini/settings.json in the workdir.
func TestE2E_Hooks_Gemini(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:              "gemhook",
				StartCommand:      e2eReportScript(),
				InstallAgentHooks: []string{"gemini"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "gemhook", e2eDefaultTimeout())

	if !report.has("HOOK_PRESENT", ".gemini/settings.json") {
		t.Error("gemini hook file not found in workdir")
	}
}

// TestE2E_Hooks_Claude verifies that install_agent_hooks=["claude"] creates
// .gc/settings.json in the city directory.
func TestE2E_Hooks_Claude(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:              "claudehook",
				StartCommand:      e2eReportScript(),
				InstallAgentHooks: []string{"claude"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "claudehook", e2eDefaultTimeout())

	// Claude hook is installed in cityDir/.gc/settings.json.
	if !report.has("HOOK_PRESENT", ".gc/settings.json") {
		t.Error("claude hook file (.gc/settings.json) not found")
	}
}

// TestE2E_Hooks_WorkspaceDefault verifies that workspace-level
// install_agent_hooks applies to all agents.
func TestE2E_Hooks_WorkspaceDefault(t *testing.T) {
	city := e2eCity{
		Workspace: e2eWorkspace{
			InstallAgentHooks: []string{"gemini"},
		},
		Agents: []e2eAgent{
			{Name: "wshook", StartCommand: e2eReportScript()},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "wshook", e2eDefaultTimeout())

	if !report.has("HOOK_PRESENT", ".gemini/settings.json") {
		t.Error("workspace-level gemini hook not applied to agent")
	}
}

// TestE2E_Hooks_AgentOverride verifies that agent-level install_agent_hooks
// replaces (not merges with) workspace-level hooks.
func TestE2E_Hooks_AgentOverride(t *testing.T) {
	city := e2eCity{
		Workspace: e2eWorkspace{
			InstallAgentHooks: []string{"gemini"},
		},
		Agents: []e2eAgent{
			{
				Name:              "hookoverride",
				StartCommand:      e2eReportScript(),
				InstallAgentHooks: []string{"claude"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "hookoverride", e2eDefaultTimeout())

	// Agent specified claude, so gemini should NOT be installed.
	if report.has("HOOK_PRESENT", ".gemini/settings.json") {
		t.Error("workspace gemini hook should be replaced by agent claude hook")
	}
	// Claude hook should be present.
	if !report.has("HOOK_PRESENT", ".gc/settings.json") {
		t.Error("agent-level claude hook not found")
	}
}

// TestE2E_PreStart verifies that pre_start commands execute before the agent
// starts (tmux only — subprocess skips pre_start).
func TestE2E_PreStart(t *testing.T) {
	if usingSubprocess() {
		t.Skip("pre_start requires tmux provider")
	}

	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "prestart",
				StartCommand: e2eReportScript(),
				PreStart:     []string{"touch prestart-marker"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)
	report := waitForReport(t, cityDir, "prestart", e2eDefaultTimeout())

	if !report.has("FILE_PRESENT", "prestart-marker") {
		t.Error("pre_start marker file not found — pre_start command did not execute")
	}
}

// TestE2E_Suspended verifies that suspended=true prevents agent from starting.
func TestE2E_Suspended(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "suspended",
				StartCommand: e2eReportScript(),
				Suspended:    true,
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Give it time — a suspended agent should NOT produce a report.
	safeName := strings.ReplaceAll("suspended", "/", "__")
	reportPath := filepath.Join(cityDir, ".gc-reports", safeName+".report")

	// Wait briefly and verify no report.
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(reportPath); err == nil {
			t.Fatal("suspended agent should not have started — report file exists")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
