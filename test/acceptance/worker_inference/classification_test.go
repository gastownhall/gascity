//go:build acceptance_c

package workerinference_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	workerpkg "github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/internal/worker/workertest"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestValidateClaudeCredentialsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeClaudeCredentials(t, path, time.Now().Add(-time.Minute))

	err := validateClaudeCredentials(path, time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")
}

func TestValidateClaudeCredentialsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeClaudeCredentials(t, path, time.Now().Add(10*time.Minute))

	err := validateClaudeCredentials(path, time.Now())
	require.NoError(t, err)
}

func TestValidateClaudeCredentialsFreshUnixSeconds(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	payload := map[string]any{
		"claudeAiOauth": map[string]any{
			"expiresAt": time.Now().Add(10 * time.Minute).Unix(),
		},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	err = validateClaudeCredentials(path, time.Now())
	require.NoError(t, err)
}

func TestInstallProviderBinaryOverride(t *testing.T) {
	workRoot := t.TempDir()
	gcHome := t.TempDir()
	runtimeDir := t.TempDir()
	env := helpers.NewEnv("", gcHome, runtimeDir)

	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "fake-claude")
	require.NoError(t, os.WriteFile(targetPath, []byte("#!/usr/bin/env bash\necho binary:$1\n"), 0o755))

	shimPath, err := installProviderBinaryOverride(workRoot, env, "claude", targetPath)
	require.NoError(t, err)
	require.FileExists(t, shimPath)
	require.Contains(t, env.Get("PATH"), filepath.Dir(shimPath))

	cmd := exec.Command(shimPath, "hello")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "binary:hello\n", string(out))
}

func TestInstallProviderBinaryOverrideSetsHome(t *testing.T) {
	workRoot := t.TempDir()
	gcHome := t.TempDir()
	runtimeDir := t.TempDir()
	env := helpers.NewEnv("", gcHome, runtimeDir)

	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "fake-claude-home")
	require.NoError(t, os.WriteFile(targetPath, []byte("#!/usr/bin/env bash\necho home:$HOME\n"), 0o755))

	t.Setenv("GC_WORKER_INFERENCE_CLAUDE_HOME", "/home/test-user")
	shimPath, err := installProviderBinaryOverride(workRoot, env, "claude", targetPath)
	require.NoError(t, err)

	cmd := exec.Command(shimPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "home:/home/test-user\n", string(out))
}

func TestProviderOverrideProcessNames(t *testing.T) {
	t.Setenv("GC_WORKER_INFERENCE_CLAUDE_PROCESS_NAMES", " aimux , claude , node ")
	require.Equal(t, []string{"aimux", "claude", "node"}, providerOverrideProcessNames("claude"))
}

func TestProviderProcessNamesIncludesNodeForWrapperScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.js")
	require.NoError(t, os.WriteFile(path, []byte("#!/usr/bin/env node\nconsole.log('hi')\n"), 0o755))

	require.Equal(t, []string{"codex", "node"}, providerProcessNames("codex", path))
}

func TestProviderProcessNamesIncludesNodeForEnvSWrapperScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gemini.js")
	require.NoError(t, os.WriteFile(path, []byte("#!/usr/bin/env -S node --no-warnings=DEP0040\nconsole.log('hi')\n"), 0o755))

	require.Equal(t, []string{"gemini", "node"}, providerProcessNames("gemini", path))
}

func TestProviderProcessNamesPreservesExplicitOverridesBeforeInference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.js")
	require.NoError(t, os.WriteFile(path, []byte("#!/usr/bin/env node\nconsole.log('hi')\n"), 0o755))

	t.Setenv("GC_WORKER_INFERENCE_CODEX_PROCESS_NAMES", "aimux, codex")
	require.Equal(t, []string{"aimux", "codex", "node"}, providerProcessNames("codex", path))
}

func TestSelectSessionMatchPrefersExpectedRunningSession(t *testing.T) {
	sessions := []sessionJSON{
		{ID: "older", Alias: "probe", State: "asleep", SessionName: "probe-old"},
		{ID: "current", Alias: "probe", State: "active", SessionName: "probe-current"},
	}

	match, ok := selectSessionMatch(sessions, "probe", "probe-current", true)
	require.True(t, ok)
	require.Equal(t, "current", match.ID)
}

func TestSelectSessionMatchPrefersRunningAliasMatch(t *testing.T) {
	sessions := []sessionJSON{
		{ID: "older", Alias: "probe", State: "creating", SessionName: "probe-old"},
		{ID: "current", Alias: "probe", State: "awake", SessionName: "probe-current"},
	}

	match, ok := selectSessionMatch(sessions, "probe", "", true)
	require.True(t, ok)
	require.Equal(t, "current", match.ID)
}

func TestInstallProviderShellOverride(t *testing.T) {
	shellPath, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not found")
	}

	workRoot := t.TempDir()
	gcHome := t.TempDir()
	runtimeDir := t.TempDir()
	env := helpers.NewEnv("", gcHome, runtimeDir)
	rcPath := filepath.Join(t.TempDir(), ".zshrc")
	require.NoError(t, os.WriteFile(rcPath, []byte("alias claude='printf alias:%s'"), 0o644))

	t.Setenv("GC_WORKER_INFERENCE_SHELL", shellPath)
	t.Setenv("GC_WORKER_INFERENCE_SHELL_RC", rcPath)
	shimPath, err := installProviderShellOverride(workRoot, env, "claude", `claude`)
	require.NoError(t, err)
	require.FileExists(t, shimPath)
	require.Contains(t, env.Get("PATH"), filepath.Dir(shimPath))

	cmd := exec.Command(shimPath, "hello")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "alias:hello", string(out))
}

func TestInstallProviderShellOverrideUsesOriginalPathForBareProviderCommand(t *testing.T) {
	shellPath, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not found")
	}

	workRoot := t.TempDir()
	gcHome := t.TempDir()
	runtimeDir := t.TempDir()
	env := helpers.NewEnv("", gcHome, runtimeDir)

	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "claude")
	require.NoError(t, os.WriteFile(targetPath, []byte("#!/usr/bin/env bash\necho real:$1\n"), 0o755))
	env.With("PATH", targetDir+string(os.PathListSeparator)+env.Get("PATH"))

	rcPath := filepath.Join(t.TempDir(), ".zshrc")
	require.NoError(t, os.WriteFile(rcPath, []byte(""), 0o644))

	t.Setenv("GC_WORKER_INFERENCE_SHELL", shellPath)
	t.Setenv("GC_WORKER_INFERENCE_SHELL_RC", rcPath)
	shimPath, err := installProviderShellOverride(workRoot, env, "claude", `claude`)
	require.NoError(t, err)

	cmd := exec.Command(shimPath, "hello")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "real:hello\n", string(out))
}

func TestInstallProviderShellOverrideBlocksRecursiveShimLaunch(t *testing.T) {
	shellPath, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not found")
	}

	workRoot := t.TempDir()
	gcHome := t.TempDir()
	runtimeDir := t.TempDir()
	env := helpers.NewEnv("", gcHome, runtimeDir)

	rcPath := filepath.Join(t.TempDir(), ".zshrc")
	require.NoError(t, os.WriteFile(rcPath, []byte(""), 0o644))

	t.Setenv("GC_WORKER_INFERENCE_SHELL", shellPath)
	t.Setenv("GC_WORKER_INFERENCE_SHELL_RC", rcPath)
	shimPath, err := installProviderShellOverride(workRoot, env, "claude", `"${GC_WORKER_INFERENCE_CURRENT_SHIM}"`)
	require.NoError(t, err)

	cmd := exec.Command(shimPath)
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 97, exitErr.ExitCode())
	require.Contains(t, string(out), "worker-inference shim recursion detected")
}

func TestLiveFailureResultClassifiesAuthErrors(t *testing.T) {
	result := liveFailureResult(
		workertest.ProfileID("claude/tmux-cli"),
		workertest.RequirementInferenceContinuation,
		"live worker did not complete within timeout",
		map[string]string{"transcript_tail": "Please run /login · API Error: 401 authentication_error: OAuth token has expired."},
	)

	require.Equal(t, workertest.ResultEnvironmentErr, result.Status)
}

func TestLiveFailureResultClassifiesProviderIncidents(t *testing.T) {
	result := liveFailureResult(
		workertest.ProfileID("codex/tmux-cli"),
		workertest.RequirementInferenceFreshTask,
		"live worker did not complete within timeout",
		map[string]string{"transcript_tail": "HTTP 429 rate_limit exceeded, try again later"},
	)

	require.Equal(t, workertest.ResultProviderIssue, result.Status)
}

func TestLiveFailureResultClassifiesAuthErrorsFromPaneTail(t *testing.T) {
	result := liveFailureResult(
		workertest.ProfileID("claude/tmux-cli"),
		workertest.RequirementInferenceContinuation,
		"worker entered blocked interactive state",
		map[string]string{"pane_tail": "Please run /login · authentication_error: OAuth token has expired."},
	)

	require.Equal(t, workertest.ResultEnvironmentErr, result.Status)
}

func TestClassifyLivePaneBlockedApproval(t *testing.T) {
	blocked := classifyLivePaneBlocked(`
● Bash(ls -la)
This command requires approval
`)

	require.NotNil(t, blocked)
	require.Equal(t, "tool_approval", blocked.Kind)
}

func TestClassifyLivePaneBlockedIgnoresBypassPermissionsStatusLine(t *testing.T) {
	blocked := classifyLivePaneBlocked(`
╭─── Claude Code v2.1.92 ──────────────────────────────────────────────────────╮
❯ [at-test] probe • 2026-04-05T08:07:09

✻ Ruminating…

────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt      /buddy
`)

	require.Nil(t, blocked)
}

func TestClassifyLivePaneBlockedThemePicker(t *testing.T) {
	blocked := classifyLivePaneBlocked(`
Let's get started.
Choose the text style
`)

	require.NotNil(t, blocked)
	require.Equal(t, "first_run_picker", blocked.Kind)
}

func TestSessionStateCountsAsRunning(t *testing.T) {
	require.True(t, sessionStateCountsAsRunning("active"))
	require.True(t, sessionStateCountsAsRunning("awake"))
	require.False(t, sessionStateCountsAsRunning("asleep"))
	require.False(t, sessionStateCountsAsRunning("creating"))
}

func TestBeadStoreNotReadyDetailIncludesInitialStartError(t *testing.T) {
	detail := beadStoreNotReadyDetail("bead store did not become ready after restart", fmt.Errorf("exit status 1"))

	require.Equal(t, "bead store did not become ready after restart after initial gc start error: exit status 1", detail)
}

func TestBeadStoreNotReadyDetailIncludesTimeout(t *testing.T) {
	err := fmt.Errorf("timed out after 90s")
	detail := beadStoreNotReadyDetail("bead store did not become ready after gc start", err)

	require.Equal(t, "bead store did not become ready after gc start timed out: timed out after 90s", detail)
}

func TestWaitForManagedDoltStoppedWaitsForStateFile(t *testing.T) {
	cityDir := t.TempDir()
	statePath := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	writeManagedDoltState(t, statePath, liveManagedDoltState{Running: true, PID: 1234, Port: 0})

	go func() {
		time.Sleep(300 * time.Millisecond)
		writeManagedDoltState(t, statePath, liveManagedDoltState{Running: false, PID: 0, Port: 0})
	}()

	detail, err := waitForManagedDoltStopped(cityDir, 3*time.Second)
	require.NoError(t, err)
	require.Contains(t, detail, `"running":false`)
}

func TestWaitForManagedDoltStoppedWaitsForPortToClose(t *testing.T) {
	cityDir := t.TempDir()
	statePath := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	portPath := filepath.Join(cityDir, ".beads", "dolt-server.port")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(portPath), 0o755))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = ln.Close()
	})
	port := ln.Addr().(*net.TCPAddr).Port
	writeManagedDoltState(t, statePath, liveManagedDoltState{Running: false, PID: 0, Port: port})
	require.NoError(t, os.WriteFile(portPath, []byte(strconv.Itoa(port)), 0o644))

	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = ln.Close()
	}()

	detail, err := waitForManagedDoltStopped(cityDir, 3*time.Second)
	require.NoError(t, err)
	require.Contains(t, detail, "reachable=false")
}

func TestLiveCurrentDoltPortPrefersPortFile(t *testing.T) {
	cityDir := t.TempDir()
	portPath := filepath.Join(cityDir, ".beads", "dolt-server.port")
	statePath := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(portPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	require.NoError(t, os.WriteFile(portPath, []byte("3737\n"), 0o644))
	writeManagedDoltState(t, statePath, liveManagedDoltState{Running: true, PID: 111, Port: 4747})

	require.Equal(t, "3737", liveCurrentDoltPort(cityDir))
}

func TestLiveCurrentDoltPortFallsBackToStateFile(t *testing.T) {
	cityDir := t.TempDir()
	statePath := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	writeManagedDoltState(t, statePath, liveManagedDoltState{Running: true, PID: 111, Port: 4747})

	require.Equal(t, "4747", liveCurrentDoltPort(cityDir))
}

func TestStageClaudeAuthFromFiles(t *testing.T) {
	gcHome := t.TempDir()
	env := helpers.NewEnv("", gcHome, t.TempDir())

	credsPath := filepath.Join(t.TempDir(), "claude-credentials.json")
	settingsPath := filepath.Join(t.TempDir(), "claude-settings.json")
	legacyPath := filepath.Join(t.TempDir(), "claude-legacy.json")

	writeClaudeCredentials(t, credsPath, time.Now().Add(10*time.Minute))
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"theme":"light"}`), 0o600))
	require.NoError(t, os.WriteFile(legacyPath, []byte(`{"custom":"value"}`), 0o600))

	t.Setenv("GC_WORKER_INFERENCE_CLAUDE_CREDENTIALS_FILE", credsPath)
	t.Setenv("GC_WORKER_INFERENCE_CLAUDE_SETTINGS_FILE", settingsPath)
	t.Setenv("GC_WORKER_INFERENCE_CLAUDE_LEGACY_CONFIG_FILE", legacyPath)

	source, err := stageClaudeAuth(gcHome, env)
	require.NoError(t, err)
	require.Equal(t, "file-secret:claude", source)
	require.Equal(t, filepath.Join(gcHome, ".claude"), env.Get("CLAUDE_CONFIG_DIR"))
	require.FileExists(t, filepath.Join(gcHome, ".claude", ".credentials.json"))
	require.FileExists(t, filepath.Join(gcHome, ".claude", "settings.json"))
	require.FileExists(t, filepath.Join(gcHome, ".claude.json"))
	require.FileExists(t, filepath.Join(gcHome, ".claude", ".claude.json"))
	rootLegacy, err := os.ReadFile(filepath.Join(gcHome, ".claude.json"))
	require.NoError(t, err)
	nestedLegacy, err := os.ReadFile(filepath.Join(gcHome, ".claude", ".claude.json"))
	require.NoError(t, err)
	require.JSONEq(t, string(rootLegacy), string(nestedLegacy))
}

func TestStageClaudeAuthPrefersSourceConfigDir(t *testing.T) {
	gcHome := t.TempDir()
	env := helpers.NewEnv("", gcHome, t.TempDir())

	sourceDir := filepath.Join(t.TempDir(), "source-claude")
	require.NoError(t, os.MkdirAll(sourceDir, 0o755))
	writeClaudeCredentials(t, filepath.Join(sourceDir, ".credentials.json"), time.Now().Add(10*time.Minute))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "settings.json"), []byte(`{"theme":"dark"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, ".claude.json"), []byte(`{"trusted":true}`), 0o600))

	homeDir := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755))
	writeClaudeCredentials(t, filepath.Join(homeDir, ".claude", ".credentials.json"), time.Now().Add(-time.Minute))

	t.Setenv("HOME", homeDir)
	t.Setenv("CLAUDE_CONFIG_DIR", sourceDir)

	source, err := stageClaudeAuth(gcHome, env)
	require.NoError(t, err)
	require.Equal(t, "env:CLAUDE_CONFIG_DIR", source)
	require.Equal(t, filepath.Join(gcHome, ".claude"), env.Get("CLAUDE_CONFIG_DIR"))
	require.FileExists(t, filepath.Join(gcHome, ".claude", ".credentials.json"))
	require.FileExists(t, filepath.Join(gcHome, ".claude", "settings.json"))
	require.FileExists(t, filepath.Join(gcHome, ".claude", ".claude.json"))
	require.FileExists(t, filepath.Join(gcHome, ".claude.json"))
}

func TestSeedClaudeProjectOnboardingMarksTrustedProject(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".claude.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"projects":{}}`), 0o600))

	projectDir := filepath.Join(t.TempDir(), "project")
	require.NoError(t, seedClaudeProjectOnboarding(configPath, projectDir))

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var cfg map[string]any
	require.NoError(t, json.Unmarshal(data, &cfg))

	projects, ok := cfg["projects"].(map[string]any)
	require.True(t, ok)
	project, ok := projects[projectDir].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, project["hasCompletedProjectOnboarding"])
	require.Equal(t, true, project["hasTrustDialogAccepted"])
	require.Equal(t, float64(1), project["projectOnboardingSeenCount"])
}

func TestSeedCodexProjectTrustMarksTrustedProject(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("model = \"gpt-5.4\"\n"), 0o600))

	projectDir := filepath.Join(t.TempDir(), "project")
	require.NoError(t, seedCodexProjectTrust(configPath, projectDir))

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `model = "gpt-5.4"`)
	require.Contains(t, text, `[projects.`+strconv.Quote(projectDir)+`]`)
	require.Contains(t, text, `trust_level = "trusted"`)
}

func writeManagedDoltState(t *testing.T, path string, state liveManagedDoltState) {
	t.Helper()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func TestStageCodexAuthFromFile(t *testing.T) {
	gcHome := t.TempDir()
	env := helpers.NewEnv("", gcHome, t.TempDir())

	authPath := filepath.Join(t.TempDir(), "codex-auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte(`{"token":"abc"}`), 0o600))

	t.Setenv("GC_WORKER_INFERENCE_CODEX_AUTH_FILE", authPath)

	source, err := stageCodexAuth(gcHome, env)
	require.NoError(t, err)
	require.Equal(t, "file-secret:codex", source)
	require.FileExists(t, filepath.Join(gcHome, ".codex", "auth.json"))
}

func TestSeedLiveProviderStateCodexMarksTrustedProject(t *testing.T) {
	gcHome := t.TempDir()
	prevEnv := liveEnv
	prevSetup := liveSetup
	liveEnv = helpers.NewEnv("", gcHome, t.TempDir())
	liveSetup = providerSetup{Profile: workerpkg.ProfileCodexTmuxCLI}
	t.Cleanup(func() {
		liveEnv = prevEnv
		liveSetup = prevSetup
	})

	cityDir := filepath.Join(t.TempDir(), "city")
	require.NoError(t, seedLiveProviderState(cityDir))

	data, err := os.ReadFile(filepath.Join(gcHome, ".codex", "config.toml"))
	require.NoError(t, err)
	require.Contains(t, string(data), `[projects.`+strconv.Quote(cityDir)+`]`)
	require.Contains(t, string(data), `trust_level = "trusted"`)
}

func TestStageGeminiAuthFromFiles(t *testing.T) {
	gcHome := t.TempDir()
	env := helpers.NewEnv("", gcHome, t.TempDir())

	settingsPath := filepath.Join(t.TempDir(), "gemini-settings.json")
	credsPath := filepath.Join(t.TempDir(), "gemini-oauth.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"theme":"light"}`), 0o600))
	require.NoError(t, os.WriteFile(credsPath, []byte(`{"refresh_token":"abc"}`), 0o600))

	t.Setenv("GC_WORKER_INFERENCE_GEMINI_SETTINGS_FILE", settingsPath)
	t.Setenv("GC_WORKER_INFERENCE_GEMINI_OAUTH_CREDS_FILE", credsPath)

	source, err := stageGeminiAuth(gcHome, env)
	require.NoError(t, err)
	require.Equal(t, "file-secret:gemini", source)
	require.FileExists(t, filepath.Join(gcHome, ".gemini", "settings.json"))
	require.FileExists(t, filepath.Join(gcHome, ".gemini", "oauth_creds.json"))
}

func TestTmuxSessionLiveUsesCitySocket(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found")
	}

	cityDir := filepath.Join(t.TempDir(), "at-test-socket")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))

	sessionName := "worker-live"
	cmd := exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "new-session", "-d", "-s", sessionName, "sleep", "30")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "kill-server").Run() //nolint:errcheck
	})

	live, err := tmuxSessionLive(cityDir, sessionName)
	require.NoError(t, err)
	require.True(t, live)
}

func TestTmuxSessionExistsOnCitySocketUsesCitySocket(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found")
	}

	cityDir := filepath.Join(t.TempDir(), "at-test-socket")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))

	sessionName := "worker-live"
	cmd := exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "new-session", "-d", "-s", sessionName, "sleep", "30")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "kill-server").Run() //nolint:errcheck
	})

	live, err := tmuxSessionExistsOnCitySocket(cityDir, sessionName)
	require.NoError(t, err)
	require.True(t, live)
}

func TestTmuxHelpersUseConfiguredSocketName(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found")
	}

	socketName := "worker-inference-sock"
	cityDir := filepath.Join(t.TempDir(), "at-test-socket")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "worker-inference-name"

[session]
socket = "worker-inference-sock"
`), 0o644))

	sessionName := "worker-live"
	cmd := exec.Command(tmuxPath, "-L", socketName, "new-session", "-d", "-s", sessionName, "printf 'ready\\n'; sleep 30")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		exec.Command(tmuxPath, "-L", socketName, "kill-server").Run() //nolint:errcheck
	})

	exists, err := tmuxSessionExistsOnCitySocket(cityDir, sessionName)
	require.NoError(t, err)
	require.True(t, exists)

	live, err := tmuxSessionLive(cityDir, sessionName)
	require.NoError(t, err)
	require.True(t, live)

	pane, err := captureTmuxPane(cityDir, sessionName, 20)
	require.NoError(t, err)
	require.Contains(t, pane, "ready")
}

func TestCaptureTmuxPaneReturnsErrorForMissingSessionOnCitySocket(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found")
	}

	cityDir := filepath.Join(t.TempDir(), "at-test-socket")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))

	sessionName := "worker-live"
	cmd := exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "new-session", "-d", "-s", sessionName, "sleep", "30")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "kill-server").Run() //nolint:errcheck
	})

	_, err = captureTmuxPane(cityDir, "missing-session", 20)
	require.Error(t, err)
	require.Contains(t, err.Error(), "capture-pane")
}

func TestCaptureTmuxPaneReturnsErrorWhenSocketServerMissing(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found")
	}

	cityDir := filepath.Join(t.TempDir(), "at-test-socket")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))

	_, err = captureTmuxPane(cityDir, "worker-live", 20)
	require.Error(t, err)
	require.Contains(t, err.Error(), "capture-pane")
	require.Contains(t, strings.ToLower(err.Error()), "no server")
	_ = tmuxPath
}

func TestInstallInferenceProbeAgentDisablesBackgroundOrders(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(cityToml, []byte(`
[workspace]
name = "worker-inference-test"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[named_session]]
template = "mayor"
mode = "always"
`), 0o644))

	require.NoError(t, installInferenceProbeAgent(cityDir, true))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `[[agent]]
name = "probe"`)
	require.Contains(t, text, `[[named_session]]
template = "probe"`)
	require.Contains(t, text, "[orders]")
	for _, name := range inferenceDisabledOrders {
		require.Contains(t, text, `"`+name+`"`)
	}
}

func TestInstallLiveProviderCommandOverride(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(cityToml, []byte(`
[workspace]
name = "worker-inference-test"
provider = "claude"
`), 0o644))

	require.NoError(t, installLiveProviderCommandOverride(cityDir, "claude", "/tmp/provider-bin/claude", nil))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `[providers.claude]`)
	require.Contains(t, text, `command = "/tmp/provider-bin/claude"`)
	require.Contains(t, text, `path_check = "/tmp/provider-bin/claude"`)
}

func TestInstallLiveProviderCommandOverrideIncludesProcessNames(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(cityToml, []byte(`
[workspace]
name = "worker-inference-test"
provider = "claude"
`), 0o644))

	require.NoError(t, installLiveProviderCommandOverride(cityDir, "claude", "/tmp/provider-bin/claude", []string{"aimux", "claude"}))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `process_names = ["aimux", "claude"]`)
}

func TestSetNamedSessionMode(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(cityToml, []byte(`
[workspace]
name = "worker-inference-test"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[named_session]]
template = "mayor"
mode = "always"
`), 0o644))

	require.NoError(t, setNamedSessionMode(cityDir, "mayor", "on_demand"))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	require.Contains(t, string(data), `mode = "on_demand"`)
}

func TestSetNamedSessionModePreservesProviderOverrides(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(cityToml, []byte(`
[workspace]
name = "worker-inference-test"
provider = "codex"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[named_session]]
template = "mayor"
mode = "always"
`), 0o644))

	require.NoError(t, installLiveProviderCommandOverride(cityDir, "codex", "/tmp/provider-bin/codex", []string{"codex", "node"}))
	require.NoError(t, setNamedSessionMode(cityDir, "mayor", "on_demand"))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `mode = "on_demand"`)
	require.Contains(t, text, `[providers.codex]`)
	require.Contains(t, text, `command = "/tmp/provider-bin/codex"`)
	require.Contains(t, text, `process_names = ["codex", "node"]`)
}

func TestEnrichLiveFailureEvidencePrefersSessionKeyTranscript(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "city")
	searchBase := t.TempDir()
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(workDir)
	transcriptDir := filepath.Join(searchBase, slug)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))

	targetPath := filepath.Join(transcriptDir, "probe-session.jsonl")
	writeLines(t, targetPath,
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"probe prompt"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-probe"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":"probe reply"},"timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-probe"}`,
	)
	otherPath := filepath.Join(transcriptDir, "latest.jsonl")
	writeLines(t, otherPath,
		`{"uuid":"u2","type":"user","message":{"role":"user","content":"mayor prompt"},"timestamp":"2025-01-01T00:00:02Z","sessionId":"provider-mayor"}`,
		`{"uuid":"a2","parentUuid":"u2","type":"assistant","message":{"role":"assistant","content":"mayor reply"},"timestamp":"2025-01-01T00:00:03Z","sessionId":"provider-mayor"}`,
	)
	future := time.Now().Add(2 * time.Minute)
	require.NoError(t, os.Chtimes(targetPath, future, future))
	require.NoError(t, os.Chtimes(otherPath, future.Add(time.Minute), future.Add(time.Minute)))

	prev := liveSetup
	liveSetup = providerSetup{SearchPaths: []string{searchBase}}
	t.Cleanup(func() { liveSetup = prev })

	enriched := enrichLiveFailureEvidence(workertest.ProfileID("claude/tmux-cli"), map[string]string{
		"city_dir":    workDir,
		"session_key": "probe-session",
		"label":       fmt.Sprintf("workdir=%s", workDir),
	})

	require.Equal(t, targetPath, enriched["transcript_path"])
	require.Equal(t, "probe-session", enriched["provider_session_id"])
	require.Contains(t, enriched["normalized_tail"], "probe reply")
}

func writeClaudeCredentials(t *testing.T, path string, expiry time.Time) {
	t.Helper()

	data, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"expiresAt": expiry.UnixMilli(),
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}
