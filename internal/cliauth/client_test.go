package cliauth

import (
	"bytes"
	"context"
	"encoding/json"
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
	if _, err := c.Login(ctx, LoginOptions{}); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("err = %v; want a service-mismatch rejection", err)
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
