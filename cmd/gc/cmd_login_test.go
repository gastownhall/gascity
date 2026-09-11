package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/cliauth"
)

func meHandler(t *testing.T, wantBearer, body string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gc/v0/me" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wantBearer {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"bad token"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

func TestDoLoginStoresVerifiedTokenAndRelaysServerMessage(t *testing.T) {
	t.Setenv(cliauth.StorePathEnv, filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv(serviceTokenEnv, "")
	server := httptest.NewServer(meHandler(t, "paste-tok",
		`{"user":{"id":"acct_9","handle":"jk","display_name":"JK"},"message":"Welcome — $5 of trial credit."}`))
	defer server.Close()

	var out, errb bytes.Buffer
	opts := loginOptions{ServiceURL: server.URL, Token: "paste-tok", Timeout: 5 * time.Second}
	if code := doLogin(context.Background(), opts, &out, &errb); code != 0 {
		t.Fatalf("doLogin exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "as @jk") {
		t.Fatalf("stdout missing handle: %q", out.String())
	}
	// The server-authored trial message must be relayed verbatim (spec §5).
	if !strings.Contains(out.String(), "Welcome — $5 of trial credit.") {
		t.Fatalf("server message not relayed: %q", out.String())
	}
	base, _ := normalizeServiceBaseURL(server.URL)
	if tok, _ := cliauth.NewStore(cliauth.DefaultStorePath()).Token(base); tok != "paste-tok" {
		t.Fatalf("stored token = %q; want paste-tok", tok)
	}
}

func TestDoLoginRejectsBadTokenWithoutStoring(t *testing.T) {
	t.Setenv(cliauth.StorePathEnv, filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv(serviceTokenEnv, "")
	server := httptest.NewServer(meHandler(t, "good", `{"user":{"id":"x","handle":"h"}}`))
	defer server.Close()

	var out, errb bytes.Buffer
	opts := loginOptions{ServiceURL: server.URL, Token: "wrong", Timeout: 5 * time.Second}
	if code := doLogin(context.Background(), opts, &out, &errb); code == 0 {
		t.Fatalf("doLogin should fail on a rejected token")
	}
	base, _ := normalizeServiceBaseURL(server.URL)
	if tok, _ := cliauth.NewStore(cliauth.DefaultStorePath()).Token(base); tok != "" {
		t.Fatalf("a rejected token must not be stored; got %q", tok)
	}
}

func TestDoWhoamiUsesStoredToken(t *testing.T) {
	t.Setenv(cliauth.StorePathEnv, filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv(serviceTokenEnv, "")
	server := httptest.NewServer(meHandler(t, "stored-tok", `{"user":{"id":"acct_1","handle":"jk"}}`))
	defer server.Close()

	base, _ := normalizeServiceBaseURL(server.URL)
	if err := cliauth.NewStore(cliauth.DefaultStorePath()).SetToken(base, "stored-tok"); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	opts := loginOptions{ServiceURL: server.URL, Timeout: 5 * time.Second}
	if code := doWhoami(context.Background(), opts, &out, &errb); code != 0 {
		t.Fatalf("doWhoami exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "@jk") {
		t.Fatalf("stdout=%q", out.String())
	}
}

func TestDoWhoamiNotLoggedIn(t *testing.T) {
	t.Setenv(cliauth.StorePathEnv, filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv(serviceTokenEnv, "")
	var out, errb bytes.Buffer
	opts := loginOptions{ServiceURL: "https://gascity.com", Timeout: 5 * time.Second}
	if code := doWhoami(context.Background(), opts, &out, &errb); code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(errb.String(), "not logged in") {
		t.Fatalf("stderr=%q", errb.String())
	}
}

func TestDoLogoutRevokesAndRemovesLocal(t *testing.T) {
	t.Setenv(cliauth.StorePathEnv, filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv(serviceTokenEnv, "")
	var revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/gc/v0/session" {
			revoked = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	base, _ := normalizeServiceBaseURL(server.URL)
	if err := cliauth.NewStore(cliauth.DefaultStorePath()).SetToken(base, "tok"); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := doLogout(context.Background(), server.URL, false, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	if !revoked {
		t.Fatalf("server-side revoke was not called")
	}
	if tok, _ := cliauth.NewStore(cliauth.DefaultStorePath()).Token(base); tok != "" {
		t.Fatalf("local token not removed: %q", tok)
	}
	if !strings.Contains(out.String(), "Revoked session") {
		t.Fatalf("stdout=%q", out.String())
	}
}

func TestDoLogoutRemovesLocalWhenServerHasNoRevoke(t *testing.T) {
	t.Setenv(cliauth.StorePathEnv, filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv(serviceTokenEnv, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer server.Close()
	base, _ := normalizeServiceBaseURL(server.URL)
	if err := cliauth.NewStore(cliauth.DefaultStorePath()).SetToken(base, "tok"); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	// A server without revocation is not a hard failure; the local token is still removed.
	if code := doLogout(context.Background(), server.URL, false, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	if tok, _ := cliauth.NewStore(cliauth.DefaultStorePath()).Token(base); tok != "" {
		t.Fatalf("local token not removed: %q", tok)
	}
}

func TestResolveServiceBaseURLLadder(t *testing.T) {
	store := cliauth.NewStore(filepath.Join(t.TempDir(), "credentials.json"))

	t.Run("explicit flag wins", func(t *testing.T) {
		t.Setenv(serviceURLEnv, "https://env.example")
		got, err := resolveServiceBaseURL("https://flag.example", store)
		if err != nil || got != "https://flag.example" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("env when no flag", func(t *testing.T) {
		t.Setenv(serviceURLEnv, "env.example")
		got, err := resolveServiceBaseURL("", store)
		if err != nil || got != "https://env.example" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("compiled default when nothing set", func(t *testing.T) {
		t.Setenv(serviceURLEnv, "")
		got, err := resolveServiceBaseURL("", store)
		if err != nil || got != defaultServiceURL {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("stored default beats compiled default", func(t *testing.T) {
		t.Setenv(serviceURLEnv, "")
		s := cliauth.NewStore(filepath.Join(t.TempDir(), "credentials.json"))
		if err := s.SetToken("https://stored.example", "tok"); err != nil {
			t.Fatal(err)
		}
		got, err := resolveServiceBaseURL("", s)
		if err != nil || got != "https://stored.example" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
}

func TestNormalizeServiceBaseURL(t *testing.T) {
	cases := map[string]string{
		"gascity.com":                 "https://gascity.com",
		"https://x.example/":          "https://x.example",
		"http://127.0.0.1:8080/base/": "http://127.0.0.1:8080/base", // loopback http allowed
		"http://localhost:9000":       "http://localhost:9000",      // loopback http allowed
		"https://x.example?a=b#c":     "https://x.example",
		"":                            defaultServiceURL,
	}
	for in, want := range cases {
		got, err := normalizeServiceBaseURL(in)
		if err != nil {
			t.Fatalf("normalize(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("normalize(%q) = %q; want %q", in, got, want)
		}
	}
	if _, err := normalizeServiceBaseURL("https://"); err == nil {
		t.Fatalf("normalize should reject a URL with no host")
	}
	// Plain http against a non-loopback host must be rejected (cleartext bearer).
	if _, err := normalizeServiceBaseURL("http://gascity.com"); err == nil {
		t.Fatalf("normalize should reject non-loopback http")
	}
}

// TestLoginLabelFlagDefaultIsHostIndependent pins the regression where
// --label's Cobra default was computed at command-construction time
// (defaultTokenLabel(), i.e. this builder's USER@hostname) and leaked into the
// generated CLI reference docs, drifting on every machine.
func TestLoginLabelFlagDefaultIsHostIndependent(t *testing.T) {
	cmd := newLoginCmd(io.Discard, io.Discard)
	f := cmd.Flags().Lookup("label")
	if f == nil {
		t.Fatal("login command is missing the --label flag")
	}
	if f.DefValue != "" {
		t.Fatalf("--label default = %q; want empty so generated CLI docs stay host-independent", f.DefValue)
	}
}

func TestLoginLabelOrDefault(t *testing.T) {
	if got := loginLabelOrDefault("custom-label"); got != "custom-label" {
		t.Fatalf("explicit label = %q; want custom-label", got)
	}
	fallback := defaultTokenLabel()
	if got := loginLabelOrDefault("   "); got != fallback {
		t.Fatalf("blank label = %q; want defaultTokenLabel() fallback %q", got, fallback)
	}
	if got := loginLabelOrDefault(""); got != fallback || got == "" {
		t.Fatalf("empty label = %q; want non-empty defaultTokenLabel() fallback %q", got, fallback)
	}
}
