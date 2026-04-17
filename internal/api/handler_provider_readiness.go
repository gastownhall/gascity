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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/searchpath"
	"gopkg.in/yaml.v3"
)

// Provider readiness statuses reported by the built-in onboarding probes.
const (
	ProbeStatusConfigured           = "configured"
	ProbeStatusNeedsAuth            = "needs_auth"
	ProbeStatusNotInstalled         = "not_installed"
	ProbeStatusInvalidConfiguration = "invalid_configuration"
	ProbeStatusProbeError           = "probe_error"

	ProbeKindProvider = "provider"
	ProbeKindTool     = "tool"
)

const (
	probeStatusConfigured           = ProbeStatusConfigured
	probeStatusNeedsAuth            = ProbeStatusNeedsAuth
	probeStatusNotInstalled         = ProbeStatusNotInstalled
	probeStatusInvalidConfiguration = ProbeStatusInvalidConfiguration
	probeStatusProbeError           = ProbeStatusProbeError

	probeKindProvider = ProbeKindProvider
	probeKindTool     = ProbeKindTool
)

var (
	providerProbePathEnv        = "/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin"
	providerProbeGOOS           = runtime.GOOS
	providerProbeCommandContext = exec.CommandContext
	providerProbeCache          = newCachedProviderProbeStore()

	defaultProviderReadinessItems = []string{"claude", "codex", "gemini"}
	defaultReadinessItems         = []string{"claude", "codex", "gemini", "github_cli"}
	supportedProviderReadiness    = readinessItemSet{
		"claude": {},
		"codex":  {},
		"gemini": {},
	}
	supportedReadiness = readinessItemSet{
		"claude":     {},
		"codex":      {},
		"gemini":     {},
		"github_cli": {},
	}
	readinessProbeSpecs = map[string]readinessProbeSpec{
		"claude": {
			displayName: "Claude Code",
			kind:        probeKindProvider,
			probe:       probeClaude,
		},
		"codex": {
			displayName: "Codex",
			kind:        probeKindProvider,
			probe: func(_ context.Context, homeDir string) providerProbeResult {
				return probeCodex(homeDir)
			},
		},
		"gemini": {
			displayName: "Gemini CLI",
			kind:        probeKindProvider,
			probe: func(_ context.Context, homeDir string) providerProbeResult {
				return probeGemini(homeDir)
			},
		},
		"github_cli": {
			displayName: "GitHub CLI",
			kind:        probeKindTool,
			probe:       probeGitHubCLI,
		},
	}
)

const providerProbeCacheTTL = 2 * time.Second

type providerReadinessResponse struct {
	Providers map[string]providerReadiness `json:"providers"`
}

type readinessResponse struct {
	Items map[string]ReadinessItem `json:"items"`
}

type providerReadiness struct {
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

// ReadinessItem is the normalized readiness result for one probed item.
type ReadinessItem struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
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

type githubAuthHost struct {
	OAuthToken string `yaml:"oauth_token"`
	Token      string `yaml:"token"`
}

type providerProbeResult struct {
	status string
}

type readinessItemSet map[string]struct{}

type readinessProbeSpec struct {
	displayName string
	kind        string
	probe       func(context.Context, string) providerProbeResult
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
	providers, err := parseRequestedReadinessItems(
		r.URL.Query().Get("providers"),
		"providers",
		defaultProviderReadinessItems,
		supportedProviderReadiness,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	fresh, err := parseFreshParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	resp, err := buildReadinessResponse(r.Context(), providers, fresh)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	providerResp := providerReadinessResponse{
		Providers: make(map[string]providerReadiness, len(providers)),
	}
	for _, provider := range providers {
		item := resp.Items[provider]
		providerResp.Providers[provider] = providerReadiness{
			DisplayName: item.DisplayName,
			Status:      item.Status,
		}
	}

	writeJSON(w, http.StatusOK, providerResp)
}

// SupportsProviderReadiness reports whether the named provider has a built-in
// readiness probe.
func SupportsProviderReadiness(name string) bool {
	_, ok := supportedProviderReadiness[strings.TrimSpace(name)]
	return ok
}

// ProbeProviders returns readiness results for the requested provider names.
// Provider names must be supported by the readiness registry.
func ProbeProviders(ctx context.Context, providers []string, fresh bool) (map[string]ReadinessItem, error) {
	items, err := validateRequestedReadinessItems(providers, supportedProviderReadiness, "provider")
	if err != nil {
		return nil, err
	}
	resp, err := buildReadinessResponse(ctx, items, fresh)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ReadinessItem, len(items))
	for _, provider := range items {
		out[provider] = resp.Items[provider]
	}
	return out, nil
}

func handleReadiness(w http.ResponseWriter, r *http.Request) {
	items, err := parseRequestedReadinessItems(
		r.URL.Query().Get("items"),
		"items",
		defaultReadinessItems,
		supportedReadiness,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	fresh, err := parseFreshParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	resp, err := buildReadinessResponse(r.Context(), items, fresh)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseRequestedReadinessItems(
	raw string,
	paramName string,
	defaults []string,
	allowed readinessItemSet,
) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string(nil), defaults...), nil
	}

	var items []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("unsupported %s value %q", paramName, name)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		items = append(items, name)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%s is required", paramName)
	}
	return items, nil
}

func validateRequestedReadinessItems(items []string, allowed readinessItemSet, label string) ([]string, error) {
	var out []string
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("unsupported %s %q", label, name)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is required", label)
	}
	return out, nil
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

func buildReadinessResponse(
	ctx context.Context,
	items []string,
	fresh bool,
) (readinessResponse, error) {
	homeDir, err := workspaceHomeDir()
	if err != nil {
		return readinessResponse{}, errors.New("workspace home unavailable")
	}

	resp := readinessResponse{
		Items: make(map[string]ReadinessItem, len(items)),
	}
	for _, itemName := range items {
		spec, ok := readinessProbeSpecs[itemName]
		if !ok {
			return readinessResponse{}, fmt.Errorf("unsupported readiness item %q", itemName)
		}
		result := probeReadinessItem(ctx, homeDir, itemName, fresh)
		resp.Items[itemName] = ReadinessItem{
			Name:        itemName,
			Kind:        spec.kind,
			DisplayName: spec.displayName,
			Status:      result.status,
		}
	}
	return resp, nil
}

func probeReadinessItem(ctx context.Context, homeDir, itemName string, fresh bool) providerProbeResult {
	cacheKey := homeDir + "\x00" + itemName
	if !fresh {
		if result, ok := providerProbeCache.load(cacheKey); ok {
			return result
		}
	}

	result := probeReadinessItemUncached(ctx, homeDir, itemName)
	providerProbeCache.store(cacheKey, result)
	return result
}

func probeReadinessItemUncached(ctx context.Context, homeDir, itemName string) providerProbeResult {
	spec, ok := readinessProbeSpecs[itemName]
	if !ok || spec.probe == nil {
		return providerProbeResult{status: probeStatusProbeError}
	}
	return spec.probe(ctx, homeDir)
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

func probeClaude(ctx context.Context, homeDir string) providerProbeResult {
	path, ok := findProbeBinary("claude", homeDir)
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
	if _, ok := findProbeBinary("codex", homeDir); !ok {
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
		if !codexTokensConfigured(auth.Tokens) {
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
	if _, ok := findProbeBinary("gemini", homeDir); !ok {
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
		if !geminiOAuthCredsConfigured(payload) {
			return providerProbeResult{status: probeStatusNeedsAuth}
		}
		return providerProbeResult{status: probeStatusConfigured}
	case "gemini-api-key", "vertex-ai", "compute-default-credentials":
		return providerProbeResult{status: probeStatusInvalidConfiguration}
	default:
		return providerProbeResult{status: probeStatusInvalidConfiguration}
	}
}

func probeGitHubCLI(ctx context.Context, homeDir string) providerProbeResult {
	ghPath, ok := findProbeBinary("gh", homeDir)
	if !ok {
		return providerProbeResult{status: probeStatusNotInstalled}
	}
	if githubCLITokenConfigured() {
		return providerProbeResult{status: probeStatusConfigured}
	}

	data, err := os.ReadFile(githubCLIHostsPath(homeDir))
	if err == nil {
		var hosts map[string]githubAuthHost
		if err := yaml.Unmarshal(data, &hosts); err != nil {
			return providerProbeResult{status: probeStatusProbeError}
		}

		for _, host := range hosts {
			if nonEmptyString(host.OAuthToken) || nonEmptyString(host.Token) {
				return providerProbeResult{status: probeStatusConfigured}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return providerProbeResult{status: probeStatusProbeError}
	}

	return probeGitHubCLIAuthStatus(ctx, homeDir, ghPath)
}

func githubCLIHostsPath(homeDir string) string {
	if configDir := strings.TrimSpace(os.Getenv("GH_CONFIG_DIR")); configDir != "" {
		return filepath.Join(configDir, "hosts.yml")
	}
	if xdgConfigHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdgConfigHome != "" {
		return filepath.Join(xdgConfigHome, "gh", "hosts.yml")
	}
	return filepath.Join(homeDir, ".config", "gh", "hosts.yml")
}

func githubCLITokenConfigured() bool {
	return nonEmptyString(os.Getenv("GH_TOKEN")) ||
		nonEmptyString(os.Getenv("GITHUB_TOKEN")) ||
		nonEmptyString(os.Getenv("GH_ENTERPRISE_TOKEN")) ||
		nonEmptyString(os.Getenv("GITHUB_ENTERPRISE_TOKEN"))
}

func probeGitHubCLIAuthStatus(ctx context.Context, homeDir, ghPath string) providerProbeResult {
	stdout, stderr, err := runProbeCommand(
		ctx,
		homeDir,
		2*time.Second,
		ghPath,
		"auth",
		"status",
	)
	if err == nil {
		return providerProbeResult{status: probeStatusConfigured}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if strings.TrimSpace(stdout) != "" || strings.TrimSpace(stderr) != "" {
			return providerProbeResult{status: probeStatusNeedsAuth}
		}
	}
	return providerProbeResult{status: probeStatusProbeError}
}

func findProbeBinary(name, homeDir string) (string, bool) {
	// Readiness probes use a deterministic, user-aware path rather than the
	// ambient process PATH so API calls do not depend on shell-specific edits.
	for _, dir := range providerProbeSearchDirs(homeDir, providerProbeGOOS, providerProbePathEnv) {
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

func providerProbeSearchDirs(homeDir, goos, basePath string) []string {
	return searchpath.Expand(homeDir, goos, basePath)
}

func providerProbeSearchPath(homeDir string) string {
	return searchpath.ExpandPath(homeDir, providerProbeGOOS, providerProbePathEnv)
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
	cmd.Env = probeCommandEnv(homeDir)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

func probeCommandEnv(homeDir string) []string {
	env := []string{
		"HOME=" + homeDir,
		"PATH=" + providerProbeSearchPath(homeDir),
		"TERM=dumb",
		"NO_COLOR=1",
		"LC_ALL=C.UTF-8",
	}
	// USER/LOGNAME are required on macOS for Keychain access — without them
	// Claude Code cannot read its stored OAuth credentials and reports
	// loggedIn: false even when the user is authenticated.
	for _, key := range []string{"USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	if xdgConfigHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdgConfigHome != "" {
		env = append(env, "XDG_CONFIG_HOME="+xdgConfigHome)
	} else {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(homeDir, ".config"))
	}
	if xdgStateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdgStateHome != "" {
		env = append(env, "XDG_STATE_HOME="+xdgStateHome)
	} else {
		env = append(env, "XDG_STATE_HOME="+filepath.Join(homeDir, ".local", "state"))
	}
	if configDir := strings.TrimSpace(os.Getenv("GH_CONFIG_DIR")); configDir != "" {
		env = append(env, "GH_CONFIG_DIR="+configDir)
	}
	for _, key := range []string{
		"GH_HOST",
		"GH_TOKEN",
		"GITHUB_TOKEN",
		"GH_ENTERPRISE_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN",
	} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func codexTokensConfigured(tokens json.RawMessage) bool {
	trimmed := bytes.TrimSpace(tokens)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false
	}

	var payload map[string]any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return false
	}
	return nonEmptyString(payload["access_token"]) ||
		nonEmptyString(payload["id_token"]) ||
		nonEmptyString(payload["refresh_token"])
}

func geminiOAuthCredsConfigured(payload map[string]any) bool {
	return nonEmptyString(payload["refresh_token"])
}

func nonEmptyString(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}
