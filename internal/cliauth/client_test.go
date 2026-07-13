package cliauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func immediateAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func TestWhoamiReturnsUserAndSendsProtocolHeaders(t *testing.T) {
	var gotAuth, gotVersion, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get(VersionHeader)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user":{"id":"acct_1","handle":"julian","display_name":"Julian K."},"message":"$5 credit","links":{"account":"https://x/account"}}`)
	}))
	defer server.Close()

	user, err := NewClient(server.URL, io.Discard).Whoami(context.Background(), "tok-xyz")
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if user.ID != "acct_1" || user.Handle != "julian" || user.DisplayName != "Julian K." {
		t.Fatalf("user = %+v", user)
	}
	if user.Message != "$5 credit" || user.Links["account"] != "https://x/account" {
		t.Fatalf("opaque fields not surfaced: %+v", user)
	}
	if gotAuth != "Bearer tok-xyz" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotVersion != Version {
		t.Fatalf("version header = %q; want %q", gotVersion, Version)
	}
	if gotPath != "/gc/v0/me" {
		t.Fatalf("me path = %q; want /gc/v0/me", gotPath)
	}
}

func TestWhoamiRejectsInvalidToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"Session expired."}}`)
	}))
	defer server.Close()

	_, err := NewClient(server.URL, io.Discard).Whoami(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), "Session expired") {
		t.Fatalf("err = %v; want a rejection surfacing the server message", err)
	}
}

// TestBrowserLoginRoundTrip drives the loopback callback the way the browser +
// server-rendered page would: it parses the auth URL, then POSTs the credential
// to the CLI's /token endpoint.
func TestBrowserLoginRoundTrip(t *testing.T) {
	const base = "https://service.example"
	c := NewClient(base, io.Discard)
	c.OpenBrowser = func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		tokenURL := strings.Replace(q.Get("redirect_uri"), "/callback", "/token", 1)
		body, _ := json.Marshal(browserLoginResult{Token: "tok-abc", Service: base, State: q.Get("state")})
		resp, err := http.Post(tokenURL, "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token, err := c.Login(ctx, LoginOptions{Label: "test@host"})
	if err != nil {
		t.Fatalf("browser login: %v", err)
	}
	if token != "tok-abc" {
		t.Fatalf("token = %q; want tok-abc", token)
	}
}

func TestBrowserLoginRejectsServiceMismatch(t *testing.T) {
	const base = "https://service.example"
	c := NewClient(base, io.Discard)
	c.OpenBrowser = func(authURL string) error {
		u, _ := url.Parse(authURL)
		q := u.Query()
		tokenURL := strings.Replace(q.Get("redirect_uri"), "/callback", "/token", 1)
		// A stray callback tries to redirect the token to another service.
		body, _ := json.Marshal(browserLoginResult{Token: "tok-abc", Service: "https://evil.example", State: q.Get("state")})
		resp, err := http.Post(tokenURL, "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Login(ctx, LoginOptions{}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v; want a service-mismatch rejection", err)
	}
}

func TestBrowserLoginRejectsAbsentService(t *testing.T) {
	const base = "https://service.example"
	c := NewClient(base, io.Discard)
	c.OpenBrowser = func(authURL string) error {
		u, _ := url.Parse(authURL)
		q := u.Query()
		tokenURL := strings.Replace(q.Get("redirect_uri"), "/callback", "/token", 1)
		// service omitted entirely — must be rejected (mandatory service match).
		body, _ := json.Marshal(map[string]string{"token": "tok", "state": q.Get("state")})
		resp, err := http.Post(tokenURL, "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Login(ctx, LoginOptions{}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v; want rejection when the callback omits service", err)
	}
}

func TestWhoamiRefusesCrossOriginRedirect(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true // reaching here means the bearer followed the redirect
		_, _ = io.WriteString(w, `{"user":{"id":"x"}}`)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/gc/v0/me", http.StatusFound)
	}))
	defer redirector.Close()

	_, err := NewClient(redirector.URL, io.Discard).Whoami(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("err = %v; want a refused cross-origin redirect", err)
	}
	if reached {
		t.Fatalf("bearer followed a redirect to a different origin")
	}
}

func TestWhoamiClassifiesStatuses(t *testing.T) {
	cases := []struct {
		status      int
		body        string
		wantUnauthd bool
	}{
		{http.StatusUnauthorized, `{"error":{"code":"invalid_token","message":"expired"}}`, true},
		{http.StatusForbidden, `{"error":{"code":"forbidden","message":"no scope"}}`, false},
		{http.StatusInternalServerError, `{"error":{"message":"boom"}}`, false},
		// Bodyless and non-JSON non-2xx (a bare gateway/proxy/WAF 401 or 5xx) must
		// still classify by status instead of surfacing a decode EOF/parse error.
		{http.StatusUnauthorized, ``, true},
		{http.StatusUnauthorized, `<html><body>401 Unauthorized</body></html>`, true},
		{http.StatusBadGateway, `502 Bad Gateway`, false},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		_, err := NewClient(server.URL, io.Discard).Whoami(context.Background(), "tok")
		server.Close()
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("status %d: err = %v; want an *AuthError", tc.status, err)
		}
		if ae.Unauthenticated() != tc.wantUnauthd {
			t.Fatalf("status %d: Unauthenticated()=%v want %v", tc.status, ae.Unauthenticated(), tc.wantUnauthd)
		}
	}
}

func TestWhoamiSurfacesSessionMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user":{"id":"a","handle":"jk"},"session":{"created_at":"2026-07-01T00:00:00Z","expires_at":"2026-07-31T00:00:00Z","last_used":"2026-07-10T00:00:00Z","fingerprint":"gcs_ab"}}`)
	}))
	defer server.Close()
	u, err := NewClient(server.URL, io.Discard).Whoami(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if u.Session.ExpiresAt != "2026-07-31T00:00:00Z" || u.Session.LastUsed == "" || u.Session.Fingerprint != "gcs_ab" {
		t.Fatalf("session metadata not surfaced: %+v", u.Session)
	}
}

func TestLogoutRevokesAndTolerates(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	if err := NewClient(ok.URL, io.Discard).Logout(context.Background(), "tok"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/gc/v0/session" || gotAuth != "Bearer tok" {
		t.Fatalf("wrong revoke request: %s %s %q", gotMethod, gotPath, gotAuth)
	}

	// A server without revocation yet returns ErrRevokeUnsupported (caller still
	// removes the local token).
	nore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer nore.Close()
	if err := NewClient(nore.URL, io.Discard).Logout(context.Background(), "tok"); !errors.Is(err, ErrRevokeUnsupported) {
		t.Fatalf("err = %v; want ErrRevokeUnsupported", err)
	}

	// Empty token is a no-op.
	if err := NewClient("https://x", io.Discard).Logout(context.Background(), ""); err != nil {
		t.Fatalf("empty token: %v", err)
	}
}

func TestDeviceLoginPollsToToken(t *testing.T) {
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gc/v0/auth/device/code":
			_, _ = io.WriteString(w, `{"device_code":"dev-1","user_code":"BDWK-JQPX","verification_uri":"https://x/device","expires_in":900,"interval":1}`)
		case "/gc/v0/auth/device/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"tok-device","token_type":"bearer"}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, io.Discard)
	c.after = immediateAfter
	token, err := c.Login(context.Background(), LoginOptions{Device: true, Label: "test@host"})
	if err != nil {
		t.Fatalf("device login: %v", err)
	}
	if token != "tok-device" {
		t.Fatalf("token = %q; want tok-device", token)
	}
	if polls < 2 {
		t.Fatalf("polls = %d; want the client to honor authorization_pending", polls)
	}
}

func TestDeviceLoginSurfacesDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gc/v0/auth/device/code":
			_, _ = io.WriteString(w, `{"device_code":"dev-1","user_code":"AAAA-BBBB","verification_uri":"https://x/device","expires_in":900,"interval":1}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"access_denied"}`)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, io.Discard)
	c.after = immediateAfter
	if _, err := c.Login(context.Background(), LoginOptions{Device: true}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v; want a denial", err)
	}
}

// TestDeviceLoginBodylessError verifies a bodyless non-2xx from the device-code
// endpoint (a bare gateway/proxy 5xx) reports the rejection by status instead of
// leaking a decode EOF.
func TestDeviceLoginBodylessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gc/v0/auth/device/code" {
			w.WriteHeader(http.StatusServiceUnavailable) // bodyless 5xx
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	c := NewClient(server.URL, io.Discard)
	c.after = immediateAfter
	_, err := c.Login(context.Background(), LoginOptions{Device: true})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err = %v; want a status-derived device rejection", err)
	}
	if strings.Contains(err.Error(), "EOF") {
		t.Fatalf("err = %v; bodyless non-2xx leaked a decode EOF", err)
	}
}

// TestDeviceLoginPollBodylessError verifies a bodyless non-2xx while polling the
// device-token endpoint reports the failure by status instead of leaking a
// decode EOF.
func TestDeviceLoginPollBodylessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gc/v0/auth/device/code":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_code":"dev-1","user_code":"AAAA-BBBB","verification_uri":"https://x/device","expires_in":900,"interval":1}`)
		default:
			w.WriteHeader(http.StatusBadGateway) // bodyless non-2xx while polling
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, io.Discard)
	c.after = immediateAfter
	_, err := c.Login(context.Background(), LoginOptions{Device: true})
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("err = %v; want a status-derived poll failure", err)
	}
	if strings.Contains(err.Error(), "EOF") {
		t.Fatalf("err = %v; bodyless poll response leaked a decode EOF", err)
	}
}

func TestNextPollInterval(t *testing.T) {
	tests := []struct {
		name            string
		current, server int
		slowDown        bool
		want            int
	}{
		{"pending keeps the current interval when the server suggests none", 15, 0, false, 15},
		{"pending never shortens on a smaller server interval", 15, 3, false, 15},
		{"pending honors a larger server interval", 5, 10, false, 10},
		{"slow_down without an interval adds the fixed step", 15, 0, true, 15 + slowDownStep},
		{"slow_down never shortens below the current interval", 15, 12, true, 15 + slowDownStep},
		{"slow_down honors a larger server interval", 15, 30, true, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextPollInterval(tt.current, tt.server, tt.slowDown); got != tt.want {
				t.Fatalf("nextPollInterval(%d, %d, %v) = %d; want %d", tt.current, tt.server, tt.slowDown, got, tt.want)
			}
		})
	}
}

// TestDeviceLoginSlowDownNeverShortensInterval pins the bug where a slow_down
// response with no explicit interval replaced an initial interval above 10s
// with the absolute value 10, making the client poll faster instead of slower
// (Service Protocol v0 §3.2).
func TestDeviceLoginSlowDownNeverShortensInterval(t *testing.T) {
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gc/v0/auth/device/code":
			// Initial interval deliberately above the old absolute 10s clamp.
			_, _ = io.WriteString(w, `{"device_code":"dev-1","user_code":"BDWK-JQPX","verification_uri":"https://x/device","expires_in":900,"interval":15}`)
		case "/gc/v0/auth/device/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"slow_down"}`) // no interval field
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"tok-device","token_type":"bearer"}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewClient(server.URL, io.Discard)
	var waits []time.Duration
	c.after = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		return immediateAfter(d)
	}
	token, err := c.Login(context.Background(), LoginOptions{Device: true, Label: "test@host"})
	if err != nil {
		t.Fatalf("device login: %v", err)
	}
	if token != "tok-device" {
		t.Fatalf("token = %q; want tok-device", token)
	}
	if len(waits) < 2 {
		t.Fatalf("waits = %v; want at least the initial sleep and the post-slow_down sleep", waits)
	}
	if waits[0] != 15*time.Second {
		t.Fatalf("first wait = %v; want the initial 15s interval", waits[0])
	}
	if waits[1] <= waits[0] {
		t.Fatalf("second wait = %v; slow_down must not shorten the %v interval", waits[1], waits[0])
	}
	if waits[1] != (15+slowDownStep)*time.Second {
		t.Fatalf("second wait = %v; want the 15s interval increased by the fixed slow-down step", waits[1])
	}
}
