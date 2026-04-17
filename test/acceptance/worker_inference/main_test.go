//go:build acceptance_c

package workerinference_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workerpkg "github.com/gastownhall/gascity/internal/worker"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var (
	liveEnv   *helpers.Env
	liveSetup providerSetup
)

type providerSetup struct {
	Profile      workerpkg.Profile
	Provider     string
	BinaryPath   string
	ProcessNames []string
	AuthSource   string
	SearchPaths  []string
	SetupError   string
}

func TestMain(m *testing.M) {
	tmpRoot, err := acceptanceTempRoot()
	if err != nil {
		panic("worker-inference: preparing temp root: " + err.Error())
	}
	if err := os.Setenv("TMPDIR", tmpRoot); err != nil {
		panic("worker-inference: setting TMPDIR: " + err.Error())
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, "gcwi-*")
	if err != nil {
		panic("worker-inference: creating temp dir: " + err.Error())
	}
	if os.Getenv("GC_ACCEPTANCE_KEEP") != "1" {
		defer os.RemoveAll(tmpDir)
	}

	gcBinary := helpers.BuildGC(tmpDir)
	gcHome := filepath.Join(tmpDir, "gc-home")
	runtimeDir := filepath.Join(tmpDir, "runtime")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic("worker-inference: " + err.Error())
		}
	}
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		panic("worker-inference: " + err.Error())
	}

	doltCfgDir := filepath.Join(gcHome, ".dolt")
	if err := os.MkdirAll(doltCfgDir, 0o755); err != nil {
		panic("worker-inference: " + err.Error())
	}
	doltCfg := `{"user.name":"gc-test","user.email":"gc-test@test.local"}`
	if err := os.WriteFile(filepath.Join(doltCfgDir, "config_global.json"), []byte(doltCfg), 0o644); err != nil {
		panic("worker-inference: " + err.Error())
	}

	liveEnv = helpers.NewEnv(gcBinary, gcHome, runtimeDir).
		Without("GC_SESSION").
		Without("GC_BEADS").
		Without("GC_DOLT").
		With("DOLT_ROOT_PATH", gcHome)
	liveSetup = prepareProviderSetup(gcHome, liveEnv)

	code := m.Run()
	if liveEnv != nil {
		helpers.RunGC(liveEnv, "", "supervisor", "stop") //nolint:errcheck
	}
	os.Exit(code)
}

func prepareProviderSetup(gcHome string, env *helpers.Env) providerSetup {
	setup := providerSetup{
		Profile: resolveProfile(os.Getenv("PROFILE")),
	}
	setup.Provider = profileProvider(setup.Profile)
	setup.SearchPaths = profileSearchPaths(gcHome, setup.Profile)
	if setup.Provider == "" {
		setup.SetupError = fmt.Sprintf("unsupported worker-inference profile %q", setup.Profile)
		return setup
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		setup.SetupError = "tmux not found in PATH"
		return setup
	}
	if _, err := exec.LookPath("bd"); err != nil {
		setup.SetupError = "bd not found in PATH"
		return setup
	}
	binaryPath, err := exec.LookPath(setup.Provider)
	if err != nil {
		setup.SetupError = fmt.Sprintf("%s CLI not found in PATH", setup.Provider)
		return setup
	}
	setup.BinaryPath = binaryPath
	authSource, err := stageProviderAuth(gcHome, env, setup.Profile)
	if err != nil {
		setup.SetupError = err.Error()
		return setup
	}
	setup.AuthSource = authSource
	return setup
}

func resolveProfile(raw string) workerpkg.Profile {
	switch strings.TrimSpace(raw) {
	case "", string(workerpkg.ProfileClaudeTmuxCLI):
		return workerpkg.ProfileClaudeTmuxCLI
	case string(workerpkg.ProfileCodexTmuxCLI):
		return workerpkg.ProfileCodexTmuxCLI
	case string(workerpkg.ProfileGeminiTmuxCLI):
		return workerpkg.ProfileGeminiTmuxCLI
	default:
		return workerpkg.Profile(strings.TrimSpace(raw))
	}
}

func profileProvider(profile workerpkg.Profile) string {
	switch profile {
	case workerpkg.ProfileClaudeTmuxCLI:
		return "claude"
	case workerpkg.ProfileCodexTmuxCLI:
		return "codex"
	case workerpkg.ProfileGeminiTmuxCLI:
		return "gemini"
	default:
		return ""
	}
}

func profileSearchPaths(gcHome string, profile workerpkg.Profile) []string {
	switch profile {
	case workerpkg.ProfileCodexTmuxCLI:
		return []string{filepath.Join(gcHome, ".codex", "sessions")}
	case workerpkg.ProfileGeminiTmuxCLI:
		return []string{filepath.Join(gcHome, ".gemini", "tmp")}
	default:
		return []string{filepath.Join(gcHome, ".claude", "projects")}
	}
}

func stageProviderAuth(gcHome string, env *helpers.Env, profile workerpkg.Profile) (string, error) {
	switch profile {
	case workerpkg.ProfileClaudeTmuxCLI:
		return stageClaudeAuth(gcHome, env)
	case workerpkg.ProfileCodexTmuxCLI:
		return stageCodexAuth(gcHome, env)
	case workerpkg.ProfileGeminiTmuxCLI:
		return stageGeminiAuth(gcHome, env)
	default:
		return "", fmt.Errorf("unsupported worker-inference profile %q", profile)
	}
}

func stageClaudeAuth(gcHome string, env *helpers.Env) (string, error) {
	claudeDir := filepath.Join(gcHome, ".claude")
	stagedCreds, credsFromFile, err := stagedValue(
		"GC_WORKER_INFERENCE_CLAUDE_CREDENTIALS_JSON",
		"GC_WORKER_INFERENCE_CLAUDE_CREDENTIALS_FILE",
	)
	if err != nil {
		return "", fmt.Errorf("claude auth unavailable: %w", err)
	}
	stagedSettings, settingsFromFile, err := stagedValue(
		"GC_WORKER_INFERENCE_CLAUDE_SETTINGS_JSON",
		"GC_WORKER_INFERENCE_CLAUDE_SETTINGS_FILE",
	)
	if err != nil {
		return "", fmt.Errorf("claude auth unavailable: %w", err)
	}
	stagedLegacy, legacyFromFile, err := stagedValue(
		"GC_WORKER_INFERENCE_CLAUDE_LEGACY_CONFIG_JSON",
		"GC_WORKER_INFERENCE_CLAUDE_LEGACY_CONFIG_FILE",
	)
	if err != nil {
		return "", fmt.Errorf("claude auth unavailable: %w", err)
	}
	if stagedCreds != "" || stagedSettings != "" || stagedLegacy != "" {
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return "", err
		}
		if stagedCreds != "" {
			if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(stagedCreds), 0o600); err != nil {
				return "", err
			}
		}
		if stagedSettings != "" {
			if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(stagedSettings), 0o600); err != nil {
				return "", err
			}
		}
		if stagedLegacy != "" {
			if err := os.WriteFile(filepath.Join(gcHome, ".claude.json"), []byte(stagedLegacy), 0o600); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"), []byte(stagedLegacy), 0o600); err != nil {
				return "", err
			}
		}
		if err := helpers.EnsureClaudeStateFile(gcHome, claudeDir); err != nil {
			return "", fmt.Errorf("claude auth unavailable: %w", err)
		}
		if err := validateClaudeCredentials(filepath.Join(claudeDir, ".credentials.json"), time.Now()); err != nil {
			return "", fmt.Errorf("claude auth unavailable: %w", err)
		}
		env.With("CLAUDE_CONFIG_DIR", claudeDir)
		return stagedSecretSource("claude", credsFromFile || settingsFromFile || legacyFromFile), nil
	}
	if authToken := strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")); authToken != "" {
		env.With("ANTHROPIC_AUTH_TOKEN", authToken)
		return "env:ANTHROPIC_AUTH_TOKEN", nil
	}
	if apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); apiKey != "" {
		env.With("ANTHROPIC_API_KEY", apiKey)
		return "env:ANTHROPIC_API_KEY", nil
	}
	if sourceDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); sourceDir != "" {
		if err := stageClaudeOAuthSource(sourceDir, "", gcHome); err == nil {
			if err := helpers.EnsureClaudeStateFile(gcHome, filepath.Join(gcHome, ".claude")); err != nil {
				return "", fmt.Errorf("claude auth unavailable: %w", err)
			}
			env.With("CLAUDE_CONFIG_DIR", filepath.Join(gcHome, ".claude"))
			return "env:CLAUDE_CONFIG_DIR", nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claude auth unavailable: %w", err)
	}
	srcClaudeDir := filepath.Join(home, ".claude")
	if _, err := os.Stat(srcClaudeDir); err == nil {
		if err := stageClaudeOAuth(home, gcHome); err != nil {
			return "", fmt.Errorf("claude auth unavailable: %w", err)
		}
		if err := helpers.EnsureClaudeStateFile(gcHome, filepath.Join(gcHome, ".claude")); err != nil {
			return "", fmt.Errorf("claude auth unavailable: %w", err)
		}
		env.With("CLAUDE_CONFIG_DIR", filepath.Join(gcHome, ".claude"))
		return "host-home:claude", nil
	}
	if err := stageClaudeOAuth(home, gcHome); err == nil {
		if err := helpers.EnsureClaudeStateFile(gcHome, filepath.Join(gcHome, ".claude")); err != nil {
			return "", fmt.Errorf("claude auth unavailable: %w", err)
		}
		env.With("CLAUDE_CONFIG_DIR", filepath.Join(gcHome, ".claude"))
		return "host-home:claude", nil
	}
	return "", fmt.Errorf("claude auth unavailable: set ANTHROPIC_AUTH_TOKEN/ANTHROPIC_API_KEY or stage ~/.claude credentials")
}

func stageCodexAuth(gcHome string, env *helpers.Env) (string, error) {
	codexDir := filepath.Join(gcHome, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		return "", err
	}
	env.With("CODEX_HOME", codexDir)
	stagedAuth, authFromFile, err := stagedValue(
		"GC_WORKER_INFERENCE_CODEX_AUTH_JSON",
		"GC_WORKER_INFERENCE_CODEX_AUTH_FILE",
	)
	if err != nil {
		return "", fmt.Errorf("codex auth unavailable: %w", err)
	}
	if stagedAuth != "" {
		if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(stagedAuth), 0o600); err != nil {
			return "", err
		}
		return stagedSecretSource("codex", authFromFile), nil
	}
	if apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); apiKey != "" {
		env.With("OPENAI_API_KEY", apiKey)
		return "env:OPENAI_API_KEY", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex auth unavailable: %w", err)
	}
	if err := copyFileIfExists(filepath.Join(home, ".codex", "auth.json"), filepath.Join(codexDir, "auth.json"), 0o600); err != nil {
		return "", fmt.Errorf("codex auth unavailable: %w", err)
	}
	if fileExists(filepath.Join(codexDir, "auth.json")) {
		return "host-home:codex", nil
	}
	return "", fmt.Errorf("codex auth unavailable: set OPENAI_API_KEY or stage ~/.codex/auth.json")
}

func stageGeminiAuth(gcHome string, env *helpers.Env) (string, error) {
	geminiDir := filepath.Join(gcHome, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o755); err != nil {
		return "", err
	}
	settings, settingsFromFile, err := stagedValue(
		"GC_WORKER_INFERENCE_GEMINI_SETTINGS_JSON",
		"GC_WORKER_INFERENCE_GEMINI_SETTINGS_FILE",
	)
	if err != nil {
		return "", fmt.Errorf("gemini auth unavailable: %w", err)
	}
	creds, credsFromFile, err := stagedValue(
		"GC_WORKER_INFERENCE_GEMINI_OAUTH_CREDS_JSON",
		"GC_WORKER_INFERENCE_GEMINI_OAUTH_CREDS_FILE",
	)
	if err != nil {
		return "", fmt.Errorf("gemini auth unavailable: %w", err)
	}
	if settings != "" || creds != "" {
		adcSource, err := stageGoogleApplicationCredentials(gcHome, env)
		if err != nil {
			return "", fmt.Errorf("gemini auth unavailable: %w", err)
		}
		if settings != "" {
			sanitized, err := sanitizeGeminiSettings(settings)
			if err != nil {
				return "", fmt.Errorf("gemini auth unavailable: %w", err)
			}
			if err := os.WriteFile(filepath.Join(geminiDir, "settings.json"), []byte(sanitized), 0o600); err != nil {
				return "", err
			}
		}
		if creds != "" {
			if err := os.WriteFile(filepath.Join(geminiDir, "oauth_creds.json"), []byte(creds), 0o600); err != nil {
				return "", err
			}
		}
		if apiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); apiKey != "" {
			env.With("GEMINI_API_KEY", apiKey)
		}
		if apiKey := strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")); apiKey != "" {
			env.With("GOOGLE_API_KEY", apiKey)
		}
		return combineAuthSource(stagedSecretSource("gemini", settingsFromFile || credsFromFile), adcSource), nil
	}
	if apiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); apiKey != "" {
		adcSource, err := stageGoogleApplicationCredentials(gcHome, env)
		if err != nil {
			return "", fmt.Errorf("gemini auth unavailable: %w", err)
		}
		env.With("GEMINI_API_KEY", apiKey)
		return combineAuthSource("env:GEMINI_API_KEY", adcSource), nil
	}
	if apiKey := strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")); apiKey != "" {
		adcSource, err := stageGoogleApplicationCredentials(gcHome, env)
		if err != nil {
			return "", fmt.Errorf("gemini auth unavailable: %w", err)
		}
		env.With("GOOGLE_API_KEY", apiKey)
		return combineAuthSource("env:GOOGLE_API_KEY", adcSource), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("gemini auth unavailable: %w", err)
	}
	if err := copySanitizedGeminiSettingsIfExists(filepath.Join(home, ".gemini", "settings.json"), filepath.Join(geminiDir, "settings.json")); err != nil {
		return "", fmt.Errorf("gemini auth unavailable: %w", err)
	}
	if err := copyFileIfExists(filepath.Join(home, ".gemini", "oauth_creds.json"), filepath.Join(geminiDir, "oauth_creds.json"), 0o600); err != nil {
		return "", fmt.Errorf("gemini auth unavailable: %w", err)
	}
	if fileExists(filepath.Join(geminiDir, "settings.json")) && fileExists(filepath.Join(geminiDir, "oauth_creds.json")) {
		adcSource, err := stageGoogleApplicationCredentials(gcHome, env)
		if err != nil {
			return "", fmt.Errorf("gemini auth unavailable: %w", err)
		}
		return combineAuthSource("host-home:gemini", adcSource), nil
	}
	adcSource, err := stageGoogleApplicationCredentials(gcHome, env)
	if err != nil {
		return "", fmt.Errorf("gemini auth unavailable: %w", err)
	}
	if adcSource != "" {
		return adcSource, nil
	}
	return "", fmt.Errorf("gemini auth unavailable: set GEMINI_API_KEY/GOOGLE_API_KEY or stage ~/.gemini oauth files")
}

func copySanitizedGeminiSettingsIfExists(src, dst string) error {
	data, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sanitized, err := sanitizeGeminiSettings(string(data))
	if err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(sanitized), 0o600)
}

func sanitizeGeminiSettings(settings string) (string, error) {
	settings = strings.TrimSpace(settings)
	if settings == "" {
		return "", nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(settings), &cfg); err != nil {
		return "", fmt.Errorf("parsing Gemini settings: %w", err)
	}
	delete(cfg, "hooks")
	general, _ := cfg["general"].(map[string]any)
	if general == nil {
		general = make(map[string]any)
	}
	general["enableAutoUpdate"] = false
	general["enableAutoUpdateNotification"] = false
	cfg["general"] = general
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	return string(append(encoded, '\n')), nil
}

func stageGoogleApplicationCredentials(gcHome string, env *helpers.Env) (string, error) {
	adcJSON := strings.TrimSpace(os.Getenv("GC_WORKER_INFERENCE_GOOGLE_APPLICATION_CREDENTIALS_JSON"))
	if adcJSON == "" {
		adcPath := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
		if adcPath == "" {
			return "", nil
		}
		adcJSONBytes, err := os.ReadFile(adcPath)
		if err != nil {
			return "", fmt.Errorf("reading GOOGLE_APPLICATION_CREDENTIALS %q: %w", adcPath, err)
		}
		adcJSON = string(adcJSONBytes)
	}
	dst := filepath.Join(gcHome, "google-application-credentials.json")
	if err := os.WriteFile(dst, []byte(adcJSON), 0o600); err != nil {
		return "", err
	}
	env.With("GOOGLE_APPLICATION_CREDENTIALS", dst)
	return "env:GOOGLE_APPLICATION_CREDENTIALS", nil
}

func stagedValue(contentEnv, fileEnv string) (string, bool, error) {
	if staged := strings.TrimSpace(os.Getenv(contentEnv)); staged != "" {
		return staged, false, nil
	}
	path := strings.TrimSpace(os.Getenv(fileEnv))
	if path == "" {
		return "", false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", true, fmt.Errorf("read %s %q: %w", fileEnv, path, err)
	}
	return string(data), true, nil
}

func stagedSecretSource(provider string, fromFile bool) string {
	provider = strings.TrimSpace(provider)
	if fromFile {
		return "file-secret:" + provider
	}
	return "inline-secret:" + provider
}

func stageClaudeOAuthSource(sourceDir, rootConfigPath, gcHome string) error {
	sourceDir = strings.TrimSpace(sourceDir)
	if sourceDir == "" {
		return fmt.Errorf("claude source config dir is empty")
	}
	dstClaudeDir := filepath.Join(gcHome, ".claude")
	if err := os.MkdirAll(dstClaudeDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{".credentials.json", "settings.json"} {
		if err := copyFileIfExists(filepath.Join(sourceDir, name), filepath.Join(dstClaudeDir, name), 0o600); err != nil {
			return err
		}
	}
	rootConfigPath = strings.TrimSpace(rootConfigPath)
	if rootConfigPath == "" {
		rootConfigPath = filepath.Join(filepath.Dir(sourceDir), ".claude.json")
	}
	if err := mergeClaudeLocalConfig(
		rootConfigPath,
		filepath.Join(sourceDir, ".claude.json"),
		filepath.Join(dstClaudeDir, ".claude.json"),
	); err != nil {
		return err
	}
	if err := copyFileIfExists(rootConfigPath, filepath.Join(gcHome, ".claude.json"), 0o600); err != nil {
		return err
	}
	return validateClaudeCredentials(filepath.Join(dstClaudeDir, ".credentials.json"), time.Now())
}

func stageClaudeOAuth(realHome, gcHome string) error {
	srcClaudeDir := filepath.Join(realHome, ".claude")
	dstClaudeDir := filepath.Join(gcHome, ".claude")
	if err := os.MkdirAll(dstClaudeDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{".credentials.json", "settings.json"} {
		if err := copyFileIfExists(filepath.Join(srcClaudeDir, name), filepath.Join(dstClaudeDir, name), 0o600); err != nil {
			return err
		}
	}
	if err := mergeClaudeLocalConfig(
		filepath.Join(realHome, ".claude.json"),
		filepath.Join(srcClaudeDir, ".claude.json"),
		filepath.Join(dstClaudeDir, ".claude.json"),
	); err != nil {
		return err
	}
	if err := copyFileIfExists(filepath.Join(realHome, ".claude.json"), filepath.Join(gcHome, ".claude.json"), 0o600); err != nil {
		return err
	}
	return validateClaudeCredentials(filepath.Join(dstClaudeDir, ".credentials.json"), time.Now())
}

func copyFileIfExists(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}

func mergeClaudeLocalConfig(rootSrc, nestedSrc, dst string) error {
	rootData, err := readJSONMapIfExists(rootSrc)
	if err != nil {
		return err
	}
	nestedData, err := readJSONMapIfExists(nestedSrc)
	if err != nil {
		return err
	}
	if len(rootData) == 0 && len(nestedData) == 0 {
		return nil
	}
	merged := make(map[string]any, len(rootData)+len(nestedData))
	for key, value := range rootData {
		merged[key] = value
	}
	for key, value := range nestedData {
		merged[key] = value
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, append(data, '\n'), 0o600)
}

func validateClaudeCredentials(path string, now time.Time) error {
	data, err := readJSONMapIfExists(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("Claude OAuth credentials file is missing")
	}
	oauthRaw, ok := data["claudeAiOauth"]
	if !ok {
		return nil
	}
	oauth, ok := oauthRaw.(map[string]any)
	if !ok {
		return nil
	}
	expiry, ok, err := parseUnixMillis(oauth["expiresAt"])
	if err != nil {
		return fmt.Errorf("parse %s expiresAt: %w", path, err)
	}
	if !ok {
		return nil
	}
	if !expiry.After(now.Add(2 * time.Minute)) {
		return fmt.Errorf("OAuth token expired at %s", expiry.UTC().Format(time.RFC3339))
	}
	return nil
}

func parseUnixMillis(value any) (time.Time, bool, error) {
	switch typed := value.(type) {
	case nil:
		return time.Time{}, false, nil
	case float64:
		return time.UnixMilli(int64(typed)), true, nil
	case int64:
		return time.UnixMilli(typed), true, nil
	case int:
		return time.UnixMilli(int64(typed)), true, nil
	case json.Number:
		millis, err := typed.Int64()
		if err != nil {
			return time.Time{}, false, err
		}
		return time.UnixMilli(millis), true, nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return time.Time{}, false, nil
		}
		millis, err := json.Number(strings.TrimSpace(typed)).Int64()
		if err != nil {
			return time.Time{}, false, err
		}
		return time.UnixMilli(millis), true, nil
	default:
		return time.Time{}, false, fmt.Errorf("unsupported type %T", value)
	}
}

func readJSONMapIfExists(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func combineAuthSource(primary, secondary string) string {
	primary = strings.TrimSpace(primary)
	secondary = strings.TrimSpace(secondary)
	if primary == "" {
		return secondary
	}
	if secondary == "" {
		return primary
	}
	return primary + "+" + secondary
}

func acceptanceTempRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_TMPDIR"))
	if root == "" {
		root = filepath.Join("/tmp", "gcac")
		if err := os.MkdirAll(root, 0o755); err != nil {
			root = filepath.Join(os.TempDir(), "gcac")
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}
