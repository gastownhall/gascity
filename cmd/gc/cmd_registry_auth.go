package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/spf13/cobra"
)

const registryCLIConfigEnv = "GC_REGISTRY_CONFIG_PATH"

type registryCLIConfig struct {
	DefaultRegistryURL string                            `json:"defaultRegistryUrl,omitempty"`
	Registries         map[string]registryCLIConfigEntry `json:"registries"`
}

type registryCLIConfigEntry struct {
	Token     string `json:"token"`
	UpdatedAt string `json:"updatedAt"`
}

type registryCurrentUser struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
}

type registryLoginOptions struct {
	RegistryURL string
	Token       string
	Label       string
	Device      bool
	NoBrowser   bool
	Timeout     time.Duration
}

func newRegistryLoginCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := registryLoginOptions{
		Label:   "GC CLI login",
		Timeout: 15 * time.Minute,
	}
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Gas City Registry",
		Long: `Log in to Gas City Registry and store a local API token.

By default this opens a browser for GitHub or Google Workspace sign-in. Use
--device for headless shells, or --token to store an existing registry token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if doRegistryLogin(cmd.Context(), opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.RegistryURL, "registry-url", "", "registry app base URL; defaults to GC_REGISTRY_URL, the stored login default, then "+defaultRegistryPublishURL)
	cmd.Flags().StringVar(&opts.Token, "token", "", "registry API token; defaults to GC_REGISTRY_TOKEN")
	cmd.Flags().StringVar(&opts.Label, "label", opts.Label, "label for the registry API token")
	cmd.Flags().BoolVar(&opts.Device, "device", false, "use device-code login instead of browser callback login")
	cmd.Flags().BoolVar(&opts.NoBrowser, "no-browser", false, "print the browser login URL instead of opening it")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "maximum time to wait for interactive login")
	return cmd
}

func newRegistryWhoamiCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := registryLoginOptions{
		Timeout: 30 * time.Second,
	}
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated registry account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if doRegistryWhoami(cmd.Context(), opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.RegistryURL, "registry-url", "", "registry app base URL; defaults to GC_REGISTRY_URL, the stored login default, then "+defaultRegistryPublishURL)
	cmd.Flags().StringVar(&opts.Token, "token", "", "registry API token; defaults to GC_REGISTRY_TOKEN or stored login")
	return cmd
}

func doRegistryLogin(ctx context.Context, opts registryLoginOptions, stdout, stderr io.Writer) int {
	baseURL, err := resolveRegistryPublishBaseURL(opts.RegistryURL)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry login: %v\n", err) //nolint:errcheck
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Secrets resolve at execution time, never as flag defaults, so help
	// output cannot render credential values from the environment.
	token := strings.TrimSpace(registryFirstNonEmpty(opts.Token, os.Getenv("GC_REGISTRY_TOKEN")))
	if token == "" {
		if opts.Device {
			token, err = registryDeviceLogin(ctx, registryPublishHTTPClient, baseURL, opts.Label, stdout)
		} else {
			token, err = registryBrowserLogin(ctx, baseURL, opts.Label, stdout, !opts.NoBrowser)
		}
		if err != nil {
			fmt.Fprintf(stderr, "gc registry login: %v\n", err) //nolint:errcheck
			return 1
		}
	}

	user, err := registryFetchCurrentUser(ctx, registryPublishHTTPClient, baseURL, token)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry login: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := writeRegistryConfiguredToken(baseURL, token); err != nil {
		fmt.Fprintf(stderr, "gc registry login: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Logged in to %s as @%s\n", baseURL, user.Handle) //nolint:errcheck
	return 0
}

func doRegistryWhoami(ctx context.Context, opts registryLoginOptions, stdout, stderr io.Writer) int {
	baseURL, err := resolveRegistryPublishBaseURL(opts.RegistryURL)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry whoami: %v\n", err) //nolint:errcheck
		return 1
	}
	// Secrets resolve at execution time, never as flag defaults, so help
	// output cannot render credential values from the environment.
	token := strings.TrimSpace(registryFirstNonEmpty(opts.Token, os.Getenv("GC_REGISTRY_TOKEN")))
	if token == "" {
		token, err = readRegistryConfiguredToken(baseURL)
		if err != nil {
			fmt.Fprintf(stderr, "gc registry whoami: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if token == "" {
		fmt.Fprintln(stderr, "gc registry whoami: not logged in; run `gc registry login`") //nolint:errcheck
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	user, err := registryFetchCurrentUser(ctx, registryPublishHTTPClient, baseURL, token)
	if err != nil {
		fmt.Fprintf(stderr, "gc registry whoami: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "@%s (%s)\n", user.Handle, user.ID) //nolint:errcheck
	return 0
}

// registryCLIConfigPath resolves the hosted-registry auth config file path.
// GC_REGISTRY_CONFIG_PATH wins; otherwise the file lives under the canonical
// Gas City state root so isolated runs and tests stay sandboxed.
func registryCLIConfigPath() string {
	if override := strings.TrimSpace(os.Getenv(registryCLIConfigEnv)); override != "" {
		return override
	}
	return filepath.Join(gchome.Default(), "registry.json")
}

func loadRegistryCLIConfig(path string) (registryCLIConfig, error) {
	cfg := registryCLIConfig{Registries: map[string]registryCLIConfigEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading registry config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing registry config: %w", err)
	}
	if cfg.Registries == nil {
		cfg.Registries = map[string]registryCLIConfigEntry{}
	}
	return cfg, nil
}

func saveRegistryCLIConfig(path string, cfg registryCLIConfig) error {
	if cfg.Registries == nil {
		cfg.Registries = map[string]registryCLIConfigEntry{}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating registry config directory: %w", err)
	}
	// A fresh 0600 temp file renamed over the target keeps the write atomic
	// and sheds any looser permissions from a pre-existing config file.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing registry config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing registry config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing registry config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing registry config: %w", err)
	}
	return nil
}

func readRegistryConfiguredToken(baseURL string) (string, error) {
	cfg, err := loadRegistryCLIConfig(registryCLIConfigPath())
	if err != nil {
		return "", err
	}
	entry, ok := cfg.Registries[baseURL]
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(entry.Token), nil
}

func writeRegistryConfiguredToken(baseURL, token string) error {
	path := registryCLIConfigPath()
	cfg, err := loadRegistryCLIConfig(path)
	if err != nil {
		return err
	}
	if cfg.Registries == nil {
		cfg.Registries = map[string]registryCLIConfigEntry{}
	}
	cfg.DefaultRegistryURL = baseURL
	cfg.Registries[baseURL] = registryCLIConfigEntry{
		Token:     strings.TrimSpace(token),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return saveRegistryCLIConfig(path, cfg)
}

func registryFetchCurrentUser(ctx context.Context, client *http.Client, baseURL, token string) (registryCurrentUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/me", nil)
	if err != nil {
		return registryCurrentUser{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	resp, err := client.Do(req)
	if err != nil {
		return registryCurrentUser{}, fmt.Errorf("checking registry login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		User  registryCurrentUser `json:"user"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := registryDecodeJSONResponse(resp, &payload); err != nil {
		return registryCurrentUser{}, fmt.Errorf("checking registry login: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if payload.Error.Message != "" {
			return registryCurrentUser{}, fmt.Errorf("registry rejected token (%s): %s", payload.Error.Code, payload.Error.Message)
		}
		return registryCurrentUser{}, fmt.Errorf("registry rejected token: HTTP %d", resp.StatusCode)
	}
	if strings.TrimSpace(payload.User.ID) == "" {
		return registryCurrentUser{}, errors.New("registry token did not authenticate a user")
	}
	return payload.User, nil
}

func registryBrowserLogin(ctx context.Context, baseURL, label string, stdout io.Writer, openBrowser bool) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("starting local login callback: %w", err)
	}
	defer func() { _ = listener.Close() }()

	state, err := randomRegistryState()
	if err != nil {
		return "", err
	}
	resultCh := make(chan browserLoginResult, 1)
	server := &http.Server{
		Handler:           registryBrowserLoginHandler(state, resultCh),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()
	// Bound shutdown so a lingering keep-alive connection cannot hang the CLI.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()

	callbackURL := "http://" + listener.Addr().String() + "/callback"
	authURL := baseURL + "/cli/auth?" + url.Values{
		"redirect_uri": {callbackURL},
		"state":        {state},
		"label":        {label},
	}.Encode()
	if openBrowser {
		if err := openRegistryURL(authURL); err != nil {
			fmt.Fprintf(stdout, "Open this URL to finish registry login:\n%s\n", authURL) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "Opened browser for registry login.\n%s\n", authURL) //nolint:errcheck
		}
	} else {
		fmt.Fprintf(stdout, "Open this URL to finish registry login:\n%s\n", authURL) //nolint:errcheck
	}

	return resolveRegistryBrowserLoginResult(ctx, baseURL, resultCh)
}

// registryBrowserLoginHandler builds the local callback server used by browser
// login. The /callback route serves the page that forwards the URL-fragment
// credentials; the /token route rejects non-POST requests, malformed bodies, a
// CSRF state mismatch, and a missing token before delivering the captured
// result to resultCh with a non-blocking send.
func registryBrowserLoginHandler(state string, resultCh chan<- browserLoginResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, registryBrowserCallbackHTML())
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload browserLoginResult
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&payload); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if payload.State != state {
			http.Error(w, "bad state", http.StatusForbidden)
			return
		}
		if strings.TrimSpace(payload.Token) == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}
		select {
		case resultCh <- payload:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	return mux
}

// resolveRegistryBrowserLoginResult blocks until the callback delivers a result
// or ctx is done. It rejects a result whose registry does not match the login
// target so a stray callback cannot redirect the stored token to another
// registry.
func resolveRegistryBrowserLoginResult(ctx context.Context, baseURL string, resultCh <-chan browserLoginResult) (string, error) {
	select {
	case result := <-resultCh:
		if result.Registry != "" && result.Registry != baseURL {
			return "", fmt.Errorf("registry callback returned %q, want %q", result.Registry, baseURL)
		}
		return result.Token, nil
	case <-ctx.Done():
		return "", errors.New("timed out waiting for browser login")
	}
}

type browserLoginResult struct {
	Token    string `json:"token"`
	Registry string `json:"registry"`
	State    string `json:"state"`
}

func registryBrowserCallbackHTML() string {
	return `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Gas City CLI Login</title></head>
<body>
<main style="font-family: system-ui, sans-serif; max-width: 48rem; margin: 3rem auto; line-height: 1.5;">
<h1>Completing Gas City CLI login</h1>
<p id="status">Sending credentials to the local CLI callback.</p>
</main>
<script>
const status = document.getElementById("status");
const params = new URLSearchParams(window.location.hash.slice(1));
fetch("/token", {
  method: "POST",
  headers: {"Content-Type": "application/json"},
  body: JSON.stringify({
    token: params.get("token") || "",
    registry: params.get("registry") || "",
    state: params.get("state") || ""
  })
}).then((response) => {
  status.textContent = response.ok ? "Login complete. You can return to your terminal." : "Login failed. Return to your terminal and try again.";
}).catch(() => {
  status.textContent = "Login failed. Return to your terminal and try again.";
});
</script>
</body>
</html>`
}

func registryDeviceLogin(ctx context.Context, client *http.Client, baseURL, label string, stdout io.Writer) (string, error) {
	body, err := json.Marshal(map[string]string{"label": label})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/cli/device/code", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting device login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var code struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := registryDecodeJSONResponse(resp, &code); err != nil {
		return "", fmt.Errorf("requesting device login: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if code.Error.Message != "" {
			return "", fmt.Errorf("registry rejected device login (%s): %s", code.Error.Code, code.Error.Message)
		}
		return "", fmt.Errorf("registry rejected device login: HTTP %d", resp.StatusCode)
	}
	if code.DeviceCode == "" || code.UserCode == "" {
		return "", errors.New("registry did not return a device code")
	}
	if code.Interval <= 0 {
		code.Interval = 5
	}
	deadline := time.Now().Add(time.Duration(code.ExpiresIn+30) * time.Second)
	fmt.Fprintf(stdout, "Open %s and enter code %s\n", code.VerificationURI, code.UserCode) //nolint:errcheck
	if code.VerificationURIComplete != "" {
		fmt.Fprintf(stdout, "Direct link: %s\n", code.VerificationURIComplete) //nolint:errcheck
	}

	for {
		if time.Now().After(deadline) {
			return "", errors.New("device login expired")
		}
		select {
		case <-time.After(time.Duration(code.Interval) * time.Second):
		case <-ctx.Done():
			return "", errors.New("timed out waiting for device login")
		}
		token, pollInterval, pending, err := registryPollDeviceToken(ctx, client, baseURL, code.DeviceCode)
		if err != nil {
			return "", err
		}
		if token != "" {
			return token, nil
		}
		if pollInterval > 0 {
			code.Interval = pollInterval
		}
		if !pending {
			return "", errors.New("device login failed")
		}
	}
}

func registryPollDeviceToken(ctx context.Context, client *http.Client, baseURL, deviceCode string) (token string, interval int, pending bool, err error) {
	body, err := json.Marshal(map[string]string{"device_code": deviceCode})
	if err != nil {
		return "", 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/cli/device/token", bytes.NewReader(body))
	if err != nil {
		return "", 0, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, false, fmt.Errorf("polling device login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
		Interval    int    `json:"interval"`
	}
	if err := registryDecodeJSONResponse(resp, &payload); err != nil {
		return "", 0, false, fmt.Errorf("polling device login: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && payload.AccessToken != "" {
		return payload.AccessToken, 0, false, nil
	}
	switch payload.Error {
	case "authorization_pending":
		return "", payload.Interval, true, nil
	case "slow_down":
		if payload.Interval <= 0 {
			payload.Interval = 10
		}
		return "", payload.Interval, true, nil
	case "access_denied":
		return "", 0, false, errors.New("device login denied")
	case "expired_token":
		return "", 0, false, errors.New("device login expired")
	default:
		return "", 0, false, fmt.Errorf("device login failed: HTTP %d", resp.StatusCode)
	}
}

func registryGitHubActionsOIDCAvailable() bool {
	return strings.TrimSpace(os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")) != "" &&
		strings.TrimSpace(os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")) != ""
}

func registryRequestGitHubActionsOIDCToken(ctx context.Context, client *http.Client, audience string) (string, error) {
	requestURL := strings.TrimSpace(os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"))
	requestToken := strings.TrimSpace(os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"))
	if requestURL == "" || requestToken == "" {
		return "", errors.New("GitHub Actions OIDC environment is not available")
	}
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("parsing GitHub Actions OIDC URL: %w", err)
	}
	query := u.Query()
	query.Set("audience", audience)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+requestToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting GitHub Actions OIDC token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		Value string `json:"value"`
	}
	if err := registryDecodeJSONResponse(resp, &payload); err != nil {
		return "", fmt.Errorf("requesting GitHub Actions OIDC token: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub Actions OIDC request failed: HTTP %d", resp.StatusCode)
	}
	if strings.TrimSpace(payload.Value) == "" {
		return "", errors.New("GitHub Actions OIDC response did not include a token")
	}
	return payload.Value, nil
}

func registryMintGitHubActionsPublishToken(ctx context.Context, client *http.Client, baseURL string, request registryPublishRequest, oidcToken string) (string, error) {
	body, err := json.Marshal(struct {
		registryPublishRequest
		GitHubOIDCToken string `json:"githubOidcToken"`
	}{
		registryPublishRequest: request,
		GitHubOIDCToken:        oidcToken,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/publish-tokens/github-actions/mint", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("minting GitHub Actions publish token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		AccessToken string `json:"access_token"`
		Error       struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := registryDecodeJSONResponse(resp, &payload); err != nil {
		return "", fmt.Errorf("minting GitHub Actions publish token: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if payload.Error.Message != "" {
			return "", fmt.Errorf("registry rejected GitHub Actions publish token (%s): %s", payload.Error.Code, payload.Error.Message)
		}
		return "", fmt.Errorf("registry rejected GitHub Actions publish token: HTTP %d", resp.StatusCode)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("registry did not return a GitHub Actions publish token")
	}
	return payload.AccessToken, nil
}

func openRegistryURL(rawURL string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", rawURL).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}

func randomRegistryState() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating auth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}
