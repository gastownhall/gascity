package cliauth

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
	"strings"
	"time"
)

// Version is the service-protocol version this client speaks. It is sent as the
// X-GC-Service-Version request header on protocol requests.
const Version = "gascity.dev/service/v0"

// VersionHeader carries the protocol version on every protocol request.
const VersionHeader = "X-GC-Service-Version"

// Endpoints are the well-known paths of a Gas City service, relative to the
// base URL. ServiceV0Endpoints returns the fixed v0 paths; the struct exists so
// the same flows can serve a server that advertises relocated paths via
// discovery (spec §8) without changing this package.
type Endpoints struct {
	AuthPage    string // GET    — browser sign-in page
	DeviceCode  string // POST   — begin device-code login
	DeviceToken string // POST   — poll device-code token
	Me          string // GET    — identity
	Session     string // DELETE — revoke the presented session
}

// ServiceV0Endpoints returns the fixed well-known paths for gascity.dev/service/v0.
func ServiceV0Endpoints() Endpoints {
	return Endpoints{
		AuthPage:    "/gc/v0/auth/cli",
		DeviceCode:  "/gc/v0/auth/device/code",
		DeviceToken: "/gc/v0/auth/device/token",
		Me:          "/gc/v0/me",
		Session:     "/gc/v0/session",
	}
}

// User is the identity a service reports for an authenticated token. Message
// and Links are opaque, server-authored, and printed verbatim by the CLI.
type User struct {
	ID          string
	Handle      string
	DisplayName string
	Message     string
	Links       map[string]string
	Session     SessionInfo
}

// SessionInfo is display-only session metadata surfaced by `gc whoami`. All
// fields are optional and human-facing; the client never parses the token.
type SessionInfo struct {
	CreatedAt   string
	ExpiresAt   string
	LastUsed    string
	Fingerprint string
}

// ErrRevokeUnsupported reports that a service has not implemented session
// revocation yet (the local credential should still be removed).
var ErrRevokeUnsupported = errors.New("service does not support session revocation")

// Client speaks the service protocol against a single base URL.
type Client struct {
	// BaseURL is the resolved service endpoint (e.g. https://gascity.com).
	BaseURL string
	// HTTPClient issues protocol requests; defaults to a 30s-timeout client.
	HTTPClient *http.Client
	// Endpoints are the well-known paths; defaults to ServiceV0Endpoints.
	Endpoints Endpoints
	// OpenBrowser opens a URL in the user's browser; when nil, browser login
	// only prints the URL.
	OpenBrowser func(string) error
	// Out receives progress and prompts.
	Out io.Writer

	// after is time.After, overridable in tests to make device polling fast.
	after func(time.Duration) <-chan time.Time
}

// NewClient returns a Client with defaults applied.
func NewClient(baseURL string, out io.Writer) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTPClient: newHTTPClient(),
		Endpoints:  ServiceV0Endpoints(),
		Out:        out,
		after:      time.After,
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return newHTTPClient()
}

// newHTTPClient builds the protocol HTTP client with redirect hardening: the
// stored session bearer is the only long-lived credential, so it must never
// follow a redirect off the origin it was issued for (including an https→http
// same-host downgrade, which the stdlib does NOT strip).
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: refuseCrossOriginRedirect,
	}
}

func refuseCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if !sameOrigin(via[0].URL, req.URL) {
		return fmt.Errorf("refusing redirect to a different origin (%s → %s): credentials must not leave the login origin",
			originOf(via[0].URL), originOf(req.URL))
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// sameOrigin compares scheme+host+port (url.Host includes the port).
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func originOf(u *url.URL) string { return u.Scheme + "://" + u.Host }

func (c *Client) afterFunc() func(time.Duration) <-chan time.Time {
	if c.after != nil {
		return c.after
	}
	return time.After
}

// LoginOptions controls a login attempt.
type LoginOptions struct {
	// Label names the minted token on the account's token list (e.g. user@host).
	Label string
	// Device selects the headless device-code flow instead of browser callback.
	Device bool
	// NoBrowser prints the browser URL instead of opening it.
	NoBrowser bool
}

// Login obtains a bearer token via the browser-callback flow (default) or the
// device-code flow (opts.Device). It does not verify or store the token; the
// caller verifies with Whoami and persists via a Store.
func (c *Client) Login(ctx context.Context, opts LoginOptions) (string, error) {
	if opts.Device {
		return c.deviceLogin(ctx, opts.Label)
	}
	return c.browserLogin(ctx, opts.Label, !opts.NoBrowser)
}

// Whoami verifies token against the service and returns the identified user. A
// non-2xx response means the token is not valid.
func (c *Client) Whoami(ctx context.Context, token string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+c.Endpoints.Me, nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(VersionHeader, Version)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return User{}, fmt.Errorf("checking login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload meResponse
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A non-2xx means the token is not valid (spec §3.3). Classify by status
		// first so a bodyless or non-JSON error (a bare gateway/proxy/WAF 401 or
		// 5xx) still returns an *AuthError; decode the body only to enrich it.
		_ = decodeJSONResponse(resp, &payload)
		return User{}, newAuthError("checking login", resp.StatusCode, payload.Error.Code, payload.Error.Message)
	}
	if err := decodeJSONResponse(resp, &payload); err != nil {
		return User{}, fmt.Errorf("checking login: %w", err)
	}
	if strings.TrimSpace(payload.User.ID) == "" {
		return User{}, errors.New("service token did not authenticate a user")
	}
	return User{
		ID:          payload.User.ID,
		Handle:      payload.User.Handle,
		DisplayName: payload.User.DisplayName,
		Message:     payload.Message,
		Links:       payload.Links,
		Session: SessionInfo{
			CreatedAt:   payload.Session.CreatedAt,
			ExpiresAt:   payload.Session.ExpiresAt,
			LastUsed:    payload.Session.LastUsed,
			Fingerprint: payload.Session.Fingerprint,
		},
	}, nil
}

// Logout revokes the session server-side by deleting it. It is best-effort: a
// service that has not implemented revocation yet (404/405/501) returns
// ErrRevokeUnsupported so the caller can still remove the local credential. The
// bearer is the session being revoked.
func (c *Client) Logout(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+c.Endpoints.Session, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(VersionHeader, Version)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed, resp.StatusCode == http.StatusNotImplemented:
		return ErrRevokeUnsupported
	default:
		return newAuthError("revoking session", resp.StatusCode, "", "")
	}
}

func (c *Client) browserLogin(ctx context.Context, label string, openBrowser bool) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("starting local login callback: %w", err)
	}
	defer func() { _ = listener.Close() }()

	state, err := randomState()
	if err != nil {
		return "", err
	}
	resultCh := make(chan browserLoginResult, 1)
	server := &http.Server{
		Handler:           browserLoginHandler(state, resultCh),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	// Bound shutdown so a lingering keep-alive connection cannot hang the CLI.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()

	callbackURL := "http://" + listener.Addr().String() + "/callback"
	authURL := c.BaseURL + c.Endpoints.AuthPage + "?" + url.Values{
		"redirect_uri": {callbackURL},
		"state":        {state},
		"label":        {label},
	}.Encode()
	if openBrowser && c.OpenBrowser != nil {
		if err := c.OpenBrowser(authURL); err != nil {
			fmt.Fprintf(c.Out, "Open this URL to finish signing in:\n%s\n", authURL) //nolint:errcheck
		} else {
			fmt.Fprintf(c.Out, "Opened your browser to sign in.\n%s\n", authURL) //nolint:errcheck
		}
	} else {
		fmt.Fprintf(c.Out, "Open this URL to finish signing in:\n%s\n", authURL) //nolint:errcheck
	}

	select {
	case result := <-resultCh:
		// Mandatory service match: reject a callback whose service is absent or
		// unequal to the login target, so a stray/hostile callback can never
		// redirect the stored token to a different service.
		if result.Service != c.BaseURL {
			return "", fmt.Errorf("login callback service %q does not match %q; refusing to store the token", result.Service, c.BaseURL)
		}
		return result.Token, nil
	case <-ctx.Done():
		return "", errors.New("timed out waiting for browser login")
	}
}

// browserLoginHandler serves the loopback callback: /callback returns the page
// that forwards the URL-fragment credential, and /token receives it, rejecting
// a non-POST, a malformed body, a CSRF state mismatch, or a missing token
// before delivering the result with a non-blocking send.
func browserLoginHandler(state string, resultCh chan<- browserLoginResult) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, browserCallbackHTML())
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

func browserCallbackHTML() string {
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
    service: params.get("service") || "",
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

func (c *Client) deviceLogin(ctx context.Context, label string) (string, error) {
	body, err := json.Marshal(map[string]string{"label": label})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+c.Endpoints.DeviceCode, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(VersionHeader, Version)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting device login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var code deviceCodeResponse
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Classify by status first; a bodyless or non-JSON non-2xx must still
		// report the rejection instead of surfacing a decode error.
		_ = decodeJSONResponse(resp, &code)
		if code.Error.Message != "" {
			return "", fmt.Errorf("service rejected device login (%s): %s", code.Error.Code, code.Error.Message)
		}
		return "", fmt.Errorf("service rejected device login: HTTP %d", resp.StatusCode)
	}
	if err := decodeJSONResponse(resp, &code); err != nil {
		return "", fmt.Errorf("requesting device login: %w", err)
	}
	if code.DeviceCode == "" || code.UserCode == "" {
		return "", errors.New("service did not return a device code")
	}
	if code.Interval <= 0 {
		code.Interval = 5
	}
	deadline := time.Now().Add(time.Duration(code.ExpiresIn+30) * time.Second)
	fmt.Fprintf(c.Out, "Open %s and enter code %s\n", code.VerificationURI, code.UserCode) //nolint:errcheck
	if code.VerificationURIComplete != "" {
		fmt.Fprintf(c.Out, "Direct link: %s\n", code.VerificationURIComplete) //nolint:errcheck
	}

	for {
		if time.Now().After(deadline) {
			return "", errors.New("device login expired")
		}
		select {
		case <-c.afterFunc()(time.Duration(code.Interval) * time.Second):
		case <-ctx.Done():
			return "", errors.New("timed out waiting for device login")
		}
		token, pollInterval, slowDown, pending, err := c.pollDeviceToken(ctx, code.DeviceCode)
		if err != nil {
			return "", err
		}
		if token != "" {
			return token, nil
		}
		code.Interval = nextPollInterval(code.Interval, pollInterval, slowDown)
		if !pending {
			return "", errors.New("device login failed")
		}
	}
}

func (c *Client) pollDeviceToken(ctx context.Context, deviceCode string) (token string, interval int, slowDown, pending bool, err error) {
	body, err := json.Marshal(map[string]string{"device_code": deviceCode})
	if err != nil {
		return "", 0, false, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+c.Endpoints.DeviceToken, bytes.NewReader(body))
	if err != nil {
		return "", 0, false, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(VersionHeader, Version)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", 0, false, false, fmt.Errorf("polling device login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload deviceTokenResponse
	// Best-effort decode: the device-token endpoint carries its RFC-8628 error
	// code (authorization_pending, slow_down, …) in the JSON body even on non-2xx,
	// but a bodyless or non-JSON response must still fall through to the
	// status-derived failure below instead of surfacing a decode error.
	_ = decodeJSONResponse(resp, &payload)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && payload.AccessToken != "" {
		return payload.AccessToken, 0, false, false, nil
	}
	switch payload.Error {
	case "authorization_pending":
		// Report the server's suggested interval, if any; the caller keeps the
		// current interval and never shortens it (nextPollInterval).
		return "", payload.Interval, false, true, nil
	case "slow_down":
		// Surface any explicit interval verbatim and flag slow_down; the caller
		// increases the current interval rather than replacing it with a smaller
		// absolute value (Service Protocol v0 §3.2 requires slowing down).
		return "", payload.Interval, true, true, nil
	case "access_denied":
		return "", 0, false, false, errors.New("device login denied")
	case "expired_token":
		return "", 0, false, false, errors.New("device login expired")
	default:
		return "", 0, false, false, fmt.Errorf("device login failed: HTTP %d", resp.StatusCode)
	}
}

// slowDownStep is the fixed number of seconds added to the device-code poll
// interval on a slow_down that carries no explicit interval, per Service
// Protocol v0 §3.2 ("increase the interval … else by a fixed step") and
// RFC 8628 §3.5.
const slowDownStep = 5

// nextPollInterval computes the next device-code poll interval in seconds.
// It honors a larger server-provided interval in every pending state, but never
// shortens polling: a slow_down that does not increase the interval falls back
// to current + slowDownStep, and authorization_pending keeps the current
// interval (Service Protocol v0 §3.2).
func nextPollInterval(current, server int, slowDown bool) int {
	if server > current {
		return server
	}
	if slowDown {
		return current + slowDownStep
	}
	return current
}

func decodeJSONResponse(resp *http.Response, v any) error {
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func randomState() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating auth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}
