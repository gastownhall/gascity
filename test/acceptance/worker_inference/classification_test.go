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

func TestClassifyLivePaneBlockedCodexUsageLimitSwitcher(t *testing.T) {
	blocked := classifyLivePaneBlocked(`
■ You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to
purchase more credits or try again at 11:26 PM.

  Approaching rate limits
  Switch to gpt-5.1-codex-mini for lower credit usage?
`)

	require.NotNil(t, blocked)
	require.Equal(t, "rate_limit", blocked.Kind)
}

func TestSessionStateCountsAsRunning(t *testing.T) {
	require.True(t, sessionStateCountsAsRunning("active"))
	require.True(t, sessionStateCountsAsRunning("awake"))
	require.False(t, sessionStateCountsAsRunning("asleep"))
	require.False(t, sessionStateCountsAsRunning("creating"))
}

func TestSelectInferenceSpawnedSessionAcceptsLiveProbeSession(t *testing.T) {
	session := sessionJSON{
		Template:    inferenceSlingTarget,
		SessionName: "probe",
		State:       "creating",
	}

	got, ok, err := selectInferenceSpawnedSession([]sessionJSON{session}, inferenceSlingTarget, func(name string) (bool, error) {
		require.Equal(t, "probe", name)
		return true, nil
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "probe", got.SessionName)
	require.Equal(t, "active", got.State)
}

func TestSelectInferenceSpawnedSessionFallsBackToNamedProbeSession(t *testing.T) {
	sessions := []sessionJSON{{
		Template:    "mayor",
		SessionName: "mayor",
		State:       "active",
	}}

	got, ok, err := selectInferenceSpawnedSession(sessions, inferenceSlingTarget, func(name string) (bool, error) {
		require.Equal(t, inferenceSlingTarget, name)
		return true, nil
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, inferenceSlingTarget, got.Template)
	require.Equal(t, inferenceSlingTarget, got.SessionName)
	require.Equal(t, "active", got.State)
}

func TestWaitForTmuxSessionStoppedRetriesUntilSessionExits(t *testing.T) {
	calls := 0
	err := waitForTmuxSessionStopped("probe", 50*time.Millisecond, time.Millisecond, func(name string) (bool, error) {
		require.Equal(t, "probe", name)
		calls++
		return calls < 3, nil
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, calls, 3)
}

func TestWaitForTmuxSessionStoppedFailsWhenSessionStaysLive(t *testing.T) {
	err := waitForTmuxSessionStopped("probe", 5*time.Millisecond, time.Millisecond, func(string) (bool, error) {
		return true, nil
	})
	require.ErrorContains(t, err, `tmux session "probe" still running after gc stop`)
}

func TestWaitForTranscriptSucceedsWithoutExpectedNeedles(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "city")
	searchBase := t.TempDir()
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(workDir)
	transcriptDir := filepath.Join(searchBase, slug)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))

	transcriptPath := filepath.Join(transcriptDir, "probe-session.jsonl")
	writeLines(t, transcriptPath,
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"bootstrap prompt"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-probe"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":"bootstrap reply"},"timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-probe"}`,
	)

	adapter := workerpkg.SessionLogAdapter{SearchPaths: []string{searchBase}}
	path, snapshot, evidence, err := waitForTranscript(adapter, workerpkg.ProfileClaudeTmuxCLI, workDir, "", "probe-session", "", "")
	require.NoError(t, err)
	require.Equal(t, transcriptPath, path)
	require.Equal(t, "probe-session", evidence["gc_session_id"])
	require.NotNil(t, snapshot)
	require.NotEmpty(t, snapshot.Entries)
}

func TestWaitForTranscriptSearchesGeminiCandidatesForEvidence(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "city")
	searchBase := filepath.Join(t.TempDir(), "gemini-tmp")
	projectDir := filepath.Join(searchBase, "at-test")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "chats"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644))

	prompt := `Create a file named worker-inference-continuation-ready-gemini.txt containing exactly "ready" and nothing else.`
	targetPath := filepath.Join(projectDir, "chats", "session-2026-04-14T19-22-target.json")
	writeGeminiChat(t, targetPath, "target-session", prompt, "ready")

	newerPath := filepath.Join(projectDir, "chats", "session-2026-04-14T19-23-mayor.json")
	writeGeminiChat(t, newerPath, "mayor-session", "mayor prompt", "checking bd ready output")

	now := time.Now()
	require.NoError(t, os.Chtimes(targetPath, now.Add(-time.Minute), now.Add(-time.Minute)))
	require.NoError(t, os.Chtimes(newerPath, now, now))

	adapter := workerpkg.SessionLogAdapter{SearchPaths: []string{searchBase}}
	path, snapshot, evidence, err := waitForTranscript(adapter, workerpkg.ProfileGeminiTmuxCLI, workDir, "s-a1-target", "", prompt, "ready")
	require.NoError(t, err)
	require.Equal(t, targetPath, path)
	require.Equal(t, targetPath, evidence["transcript_path"])
	require.Equal(t, "target-session", snapshot.ProviderSessionID)
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
	assertClaudeStateSeeded(t, rootLegacy, map[string]any{"custom": "value"})
}

func TestStageClaudeAuthFromAuthToken(t *testing.T) {
	gcHome := t.TempDir()
	env := helpers.NewEnv("", gcHome, t.TempDir())

	t.Setenv("ANTHROPIC_AUTH_TOKEN", "synthetic-token")

	source, err := stageClaudeAuth(gcHome, env)
	require.NoError(t, err)
	require.Equal(t, "env:ANTHROPIC_AUTH_TOKEN", source)
	require.Equal(t, "synthetic-token", env.Get("ANTHROPIC_AUTH_TOKEN"))
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
	rootLegacy, err := os.ReadFile(filepath.Join(gcHome, ".claude.json"))
	require.NoError(t, err)
	assertClaudeStateSeeded(t, rootLegacy, map[string]any{"trusted": true})
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

	require.Equal(t, true, cfg["hasCompletedOnboarding"])
	projects, ok := cfg["projects"].(map[string]any)
	require.True(t, ok)
	project, ok := projects[projectDir].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, project["hasCompletedProjectOnboarding"])
	require.Equal(t, true, project["hasTrustDialogAccepted"])
	require.Equal(t, float64(1), project["projectOnboardingSeenCount"])
}

func TestSeedClaudeProjectOnboardingCreatesConfigWhenMissing(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".claude", ".claude.json")
	projectDir := filepath.Join(t.TempDir(), "project")

	require.NoError(t, seedClaudeProjectOnboarding(configPath, projectDir))
	require.FileExists(t, configPath)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var cfg map[string]any
	require.NoError(t, json.Unmarshal(data, &cfg))

	require.Equal(t, true, cfg["hasCompletedOnboarding"])
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

func TestSeedGeminiFolderTrustMarksTrustedProject(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "trustedFolders.json")
	projectDir := filepath.Join(t.TempDir(), "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	require.NoError(t, seedGeminiFolderTrust(configPath, projectDir))

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var trusted map[string]string
	require.NoError(t, json.Unmarshal(data, &trusted))
	require.Equal(t, "TRUST_FOLDER", trusted[projectDir])
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
	require.Equal(t, filepath.Join(gcHome, ".codex"), env.Get("CODEX_HOME"))
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

func TestSeedLiveProviderStateGeminiMarksTrustedProject(t *testing.T) {
	gcHome := t.TempDir()
	prevEnv := liveEnv
	prevSetup := liveSetup
	liveEnv = helpers.NewEnv("", gcHome, t.TempDir())
	liveSetup = providerSetup{Profile: workerpkg.ProfileGeminiTmuxCLI}
	t.Cleanup(func() {
		liveEnv = prevEnv
		liveSetup = prevSetup
	})

	cityDir := filepath.Join(t.TempDir(), "city")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))
	require.NoError(t, seedLiveProviderState(cityDir))

	data, err := os.ReadFile(filepath.Join(gcHome, ".gemini", "trustedFolders.json"))
	require.NoError(t, err)
	var trusted map[string]string
	require.NoError(t, json.Unmarshal(data, &trusted))
	require.Equal(t, "TRUST_FOLDER", trusted[cityDir])
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

func TestStageGeminiAuthStripsHostHooks(t *testing.T) {
	gcHome := t.TempDir()
	env := helpers.NewEnv("", gcHome, t.TempDir())

	settingsPath := filepath.Join(t.TempDir(), "gemini-settings.json")
	credsPath := filepath.Join(t.TempDir(), "gemini-oauth.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{
  "hooks": {"BeforeTool": [{"matcher": "run_shell_command"}]},
  "security": {"auth": {"selectedType": "oauth-personal"}}
}`), 0o600))
	require.NoError(t, os.WriteFile(credsPath, []byte(`{"refresh_token":"abc"}`), 0o600))

	t.Setenv("GC_WORKER_INFERENCE_GEMINI_SETTINGS_FILE", settingsPath)
	t.Setenv("GC_WORKER_INFERENCE_GEMINI_OAUTH_CREDS_FILE", credsPath)

	_, err := stageGeminiAuth(gcHome, env)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(gcHome, ".gemini", "settings.json"))
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	require.NotContains(t, settings, "hooks")
	require.Contains(t, settings, "security")
	general, ok := settings["general"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, general["enableAutoUpdate"])
	require.Equal(t, false, general["enableAutoUpdateNotification"])
}

func TestCopySanitizedGeminiSettingsIfExistsStripsHooks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "settings.json")
	dst := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(src, []byte(`{
  "hooks": {"BeforeTool": [{"matcher": "run_shell_command"}]},
  "security": {"auth": {"selectedType": "oauth-personal"}}
}`), 0o600))

	require.NoError(t, copySanitizedGeminiSettingsIfExists(src, dst))

	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	require.NotContains(t, settings, "hooks")
	require.Contains(t, settings, "security")
	general, ok := settings["general"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, general["enableAutoUpdate"])
	require.Equal(t, false, general["enableAutoUpdateNotification"])
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
	require.True(t, isIgnorableTmuxProbeError(err), "unexpected tmux error: %v", err)
	_ = tmuxPath
}

func TestDetectLiveBlockedInteractionIgnoresMissingSocketServer(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found")
	}

	cityDir := filepath.Join(t.TempDir(), "at-test-socket")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))

	blocked, err := detectLiveBlockedInteraction(cityDir, "worker-live")
	require.NoError(t, err)
	require.Nil(t, blocked)
	_ = tmuxPath
}

func TestDetectLiveBlockedInteractionIgnoresMissingSessionOnLiveSocket(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found")
	}

	cityDir := filepath.Join(t.TempDir(), "at-test-socket")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))

	cmd := exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "new-session", "-d", "-s", "worker-live", "sleep", "30")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		exec.Command(tmuxPath, "-L", filepath.Base(cityDir), "kill-server").Run() //nolint:errcheck
	})

	blocked, err := detectLiveBlockedInteraction(cityDir, "missing-session")
	require.NoError(t, err)
	require.Nil(t, blocked)
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

	require.NoError(t, installInferenceProbeAgent(cityDir))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `[[agent]]
name = "probe"`)
	require.Contains(t, text, `[[named_session]]
template = "probe"`)
	require.Contains(t, text, "[orders]")
	require.Contains(t, text, "[session]")
	require.Contains(t, text, `startup_timeout = "`+liveSessionStartupTimeout+`"`)
	for _, name := range inferenceDisabledOrders {
		require.Contains(t, text, `"`+name+`"`)
	}
}

func TestInstallInferenceProbeAgentEnablesGeminiHooks(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := filepath.Join(cityDir, "city.toml")
	require.NoError(t, os.WriteFile(cityToml, []byte(`
[workspace]
name = "worker-inference-test"
provider = "gemini"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`), 0o644))

	require.NoError(t, installInferenceProbeAgent(cityDir, true))
	require.NoError(t, installInferenceProbeAgent(cityDir, true))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `[workspace]
name = "worker-inference-test"
provider = "gemini"
install_agent_hooks = ["gemini"]`)
	require.Equal(t, 1, strings.Count(text, `install_agent_hooks = ["gemini"]`))
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

func TestLiveHarnessConfigMutationsPreserveProbeOverrides(t *testing.T) {
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

	require.NoError(t, installInferenceProbeAgent(cityDir, true))
	require.NoError(t, installLiveProviderCommandOverride(cityDir, "codex", "/tmp/provider-bin/codex", []string{"codex", "node"}))
	require.NoError(t, setNamedSessionMode(cityDir, inferenceSlingTarget, "on_demand"))
	require.NoError(t, setAgentSuspended(cityDir, "mayor", true))

	data, err := os.ReadFile(cityToml)
	require.NoError(t, err)
	text := string(data)
	require.Contains(t, text, `[providers.codex]`)
	require.Contains(t, text, `command = "/tmp/provider-bin/codex"`)
	require.Contains(t, text, `process_names = ["codex", "node"]`)
	require.Contains(t, text, `[[agent]]
name = "probe"`)
	require.Contains(t, text, `[[named_session]]
template = "probe"
mode = "on_demand"`)
	require.Contains(t, text, `[orders]`)
	require.Contains(t, text, `[session]`)
	require.Contains(t, text, `suspended = true`)
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

func assertClaudeStateSeeded(t *testing.T, data []byte, preserved map[string]any) {
	t.Helper()

	var state map[string]any
	require.NoError(t, json.Unmarshal(data, &state))
	require.Equal(t, true, state["hasCompletedOnboarding"])
	require.Equal(t, "light", state["theme"])
	for key, want := range preserved {
		require.Equal(t, want, state[key], "preserved Claude state %s", key)
	}
}

func writeGeminiChat(t *testing.T, path, sessionID, userText, assistantText string) {
	t.Helper()

	data, err := json.MarshalIndent(map[string]any{
		"sessionId": sessionID,
		"messages": []map[string]any{
			{
				"id":        sessionID + "-user",
				"timestamp": "2026-04-14T19:22:01Z",
				"type":      "user",
				"content": []map[string]string{
					{"text": userText},
				},
			},
			{
				"id":        sessionID + "-assistant",
				"timestamp": "2026-04-14T19:22:02Z",
				"type":      "gemini",
				"content":   assistantText,
			},
		},
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}
