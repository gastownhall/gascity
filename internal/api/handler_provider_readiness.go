package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	probeStatusConfigured           = "configured"
	probeStatusNeedsAuth            = "needs_auth"
	probeStatusNotInstalled         = "not_installed"
	probeStatusInvalidConfiguration = "invalid_configuration"
	probeStatusProbeError           = "probe_error"
)

var (
	providerProbePathEnv        = "/usr/local/bin:/usr/bin:/bin"
	providerProbeCommandContext = exec.CommandContext
	providerProbeCache          = newCachedProviderProbeStore()
)

const providerProbeCacheTTL = 2 * time.Second

type providerReadinessResponse struct {
	Providers map[string]providerReadiness `json:"providers"`
}

type providerReadiness struct {
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

type claudeAuthStatus struct {
	LoggedIn    bool   `json:"loggedIn"`
	AuthMethod  string `json:"authMethod"`
	APIProvider string `json:"apiProvider"`
}

type codexAuthFile struct {
	AuthMode string          `json:"auth_mode"`
	Tokens   json.RawMessage `json:"tokens"`
}

type geminiSettings struct {
	Security struct {
		Auth struct {
			SelectedType string `json:"selectedType"`
		} `json:"auth"`
	} `json:"security"`
}

type providerProbeResult struct {
	status string
}

type cachedProviderProbe struct {
	result  providerProbeResult
	expires time.Time
}

type cachedProviderProbeStore struct {
	mu      sync.Mutex
	entries map[string]cachedProviderProbe
}

func handleProviderReadiness(w http.ResponseWriter, r *http.Request) {
	providers, err := parseRequestedProviders(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	fresh, err := parseFreshParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	homeDir, err := workspaceHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "workspace home unavailable")
		return
	}

	resp := providerReadinessResponse{
		Providers: make(map[string]providerReadiness, len(providers)),
	}
	for _, provider := range providers {
		result := probeProvider(r.Context(), homeDir, provider, fresh)
		resp.Providers[provider] = providerReadiness{
			DisplayName: providerDisplayName(provider),
			Status:      result.status,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func parseRequestedProviders(r *http.Request) ([]string, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("providers"))
	if raw == "" {
		return []string{"claude", "codex", "gemini"}, nil
	}

	var providers []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		switch name {
		case "claude", "codex", "gemini":
		default:
			return nil, fmt.Errorf("unsupported provider %q", name)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		providers = append(providers, name)
	}
	if len(providers) == 0 {
		return nil, errors.New("providers is required")
	}
	return providers, nil
}

func parseFreshParam(r *http.Request) (bool, error) {
	fresh := strings.TrimSpace(r.URL.Query().Get("fresh"))
	if fresh == "" {
		return false, nil
	}
	switch fresh {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("fresh must be 0 or 1")
	}
}

func workspaceHomeDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home, nil
	}
	return os.UserHomeDir()
}

func probeProvider(ctx context.Context, homeDir, provider string, fresh bool) providerProbeResult {
	cacheKey := homeDir + "\x00" + provider
	if !fresh {
		if result, ok := providerProbeCache.load(cacheKey); ok {
			return result
		}
	}

	result := probeProviderUncached(ctx, homeDir, provider)
	providerProbeCache.store(cacheKey, result)
	return result
}

func probeProviderUncached(ctx context.Context, homeDir, provider string) providerProbeResult {
	switch provider {
	case "claude":
		return probeClaude(ctx, homeDir)
	case "codex":
		return probeCodex(homeDir)
	case "gemini":
		return probeGemini(homeDir)
	default:
		return providerProbeResult{status: probeStatusProbeError}
	}
}

func newCachedProviderProbeStore() *cachedProviderProbeStore {
	return &cachedProviderProbeStore{
		entries: make(map[string]cachedProviderProbe),
	}
}

func (s *cachedProviderProbeStore) load(key string) (providerProbeResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[key]
	if !ok {
		return providerProbeResult{}, false
	}
	if time.Now().After(entry.expires) {
		delete(s.entries, key)
		return providerProbeResult{}, false
	}
	return entry.result, true
}

func (s *cachedProviderProbeStore) store(key string, result providerProbeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[key] = cachedProviderProbe{
		result:  result,
		expires: time.Now().Add(providerProbeCacheTTL),
	}
}

func providerDisplayName(provider string) string {
	switch provider {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "gemini":
		return "Gemini CLI"
	default:
		return provider
	}
}

func probeClaude(ctx context.Context, homeDir string) providerProbeResult {
	path, ok := findProbeBinary("claude")
	if !ok {
		return providerProbeResult{status: probeStatusNotInstalled}
	}

	stdout, _, err := runProbeCommand(ctx, homeDir, 5*time.Second, path, "auth", "status", "--json")
	if err != nil && strings.TrimSpace(stdout) == "" {
		return providerProbeResult{status: probeStatusProbeError}
	}

	var status claudeAuthStatus
	if decodeErr := json.Unmarshal([]byte(stdout), &status); decodeErr != nil {
		return providerProbeResult{status: probeStatusProbeError}
	}
	if !status.LoggedIn {
		return providerProbeResult{status: probeStatusNeedsAuth}
	}
	// Onboarding only supports the first-party claude.ai OAuth flow. API-key
	// or alternate providers are intentionally treated as unsupported.
	if status.AuthMethod == "claude.ai" && status.APIProvider == "firstParty" {
		return providerProbeResult{status: probeStatusConfigured}
	}
	return providerProbeResult{status: probeStatusInvalidConfiguration}
}

func probeCodex(homeDir string) providerProbeResult {
	if _, ok := findProbeBinary("codex"); !ok {
		return providerProbeResult{status: probeStatusNotInstalled}
	}

	data, err := os.ReadFile(filepath.Join(homeDir, ".codex", "auth.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return providerProbeResult{status: probeStatusNeedsAuth}
		}
		return providerProbeResult{status: probeStatusProbeError}
	}

	var auth codexAuthFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return providerProbeResult{status: probeStatusProbeError}
	}

	switch strings.ToLower(strings.TrimSpace(auth.AuthMode)) {
	case "chatgpt":
		if len(bytes.TrimSpace(auth.Tokens)) == 0 || string(bytes.TrimSpace(auth.Tokens)) == "null" {
			return providerProbeResult{status: probeStatusNeedsAuth}
		}
		return providerProbeResult{status: probeStatusConfigured}
	case "", "none":
		return providerProbeResult{status: probeStatusNeedsAuth}
	case "api_key", "api-key", "apikey":
		return providerProbeResult{status: probeStatusInvalidConfiguration}
	default:
		return providerProbeResult{status: probeStatusInvalidConfiguration}
	}
}

func probeGemini(homeDir string) providerProbeResult {
	if _, ok := findProbeBinary("gemini"); !ok {
		return providerProbeResult{status: probeStatusNotInstalled}
	}

	settingsPath := filepath.Join(homeDir, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return providerProbeResult{status: probeStatusNeedsAuth}
		}
		return providerProbeResult{status: probeStatusProbeError}
	}

	var settings geminiSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return providerProbeResult{status: probeStatusProbeError}
	}

	selectedType := strings.TrimSpace(settings.Security.Auth.SelectedType)
	switch selectedType {
	case "":
		return providerProbeResult{status: probeStatusNeedsAuth}
	case "oauth-personal":
		credData, err := os.ReadFile(filepath.Join(homeDir, ".gemini", "oauth_creds.json"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return providerProbeResult{status: probeStatusNeedsAuth}
			}
			return providerProbeResult{status: probeStatusProbeError}
		}
		var payload map[string]any
		if err := json.Unmarshal(credData, &payload); err != nil {
			return providerProbeResult{status: probeStatusProbeError}
		}
		return providerProbeResult{status: probeStatusConfigured}
	case "gemini-api-key", "vertex-ai", "compute-default-credentials":
		return providerProbeResult{status: probeStatusInvalidConfiguration}
	default:
		return providerProbeResult{status: probeStatusInvalidConfiguration}
	}
}

func findProbeBinary(name string) (string, bool) {
	// Readiness probes only trust system install locations baked into the
	// runtime image so browser polling cannot pick up arbitrary user PATH edits.
	for _, dir := range strings.Split(providerProbePathEnv, ":") {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, true
	}
	return "", false
}

func runProbeCommand(
	ctx context.Context,
	homeDir string,
	timeout time.Duration,
	path string,
	args ...string,
) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := providerProbeCommandContext(ctx, path, args...)
	cmd.Dir = homeDir
	cmd.Env = []string{
		"HOME=" + homeDir,
		"PATH=" + providerProbePathEnv,
		"TERM=dumb",
		"NO_COLOR=1",
		"LC_ALL=C.UTF-8",
		"XDG_CONFIG_HOME=" + filepath.Join(homeDir, ".config"),
		"XDG_STATE_HOME=" + filepath.Join(homeDir, ".local", "state"),
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}
