package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// writeServerCA writes an httptest TLS server's certificate to a PEM file and
// returns its path, so a client can verify the self-signed server.
func writeServerCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRemoteCheckRedirect(t *testing.T) {
	mkReq := func(rawurl string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, rawurl, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	orig := mkReq("https://box.internal:9443/v0/city/mc/status")

	t.Run("no via is allowed", func(t *testing.T) {
		if err := remoteCheckRedirect(mkReq("https://box.internal:9443/x"), nil); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("cross-host refused", func(t *testing.T) {
		err := remoteCheckRedirect(mkReq("https://evil.example.com/x"), []*http.Request{orig})
		if err == nil {
			t.Fatal("cross-host redirect must be refused")
		}
	})
	t.Run("https->http downgrade refused", func(t *testing.T) {
		err := remoteCheckRedirect(mkReq("http://box.internal:9443/x"), []*http.Request{orig})
		if err == nil {
			t.Fatal("downgrade redirect must be refused")
		}
	})
	t.Run("same-host allowed and strips creds", func(t *testing.T) {
		next := mkReq("https://box.internal:9443/other")
		next.Header.Set("Authorization", "Bearer secret")
		next.Header.Set("X-GC-Request", "true")
		next.Header.Set("X-GC-City-Write", "grant")
		next.Header.Set("Accept", "text/event-stream")
		if err := remoteCheckRedirect(next, []*http.Request{orig}); err != nil {
			t.Fatalf("same-host redirect should be allowed: %v", err)
		}
		if next.Header.Get("Authorization") != "" {
			t.Error("Authorization must be stripped on a followed redirect")
		}
		if next.Header.Get("X-GC-Request") != "" || next.Header.Get("X-GC-City-Write") != "" {
			t.Error("X-GC-* headers must be stripped on a followed redirect")
		}
		if next.Header.Get("Accept") != "text/event-stream" {
			t.Error("non-sensitive headers must be preserved")
		}
	})
	t.Run("too many redirects refused", func(t *testing.T) {
		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = orig
		}
		if err := remoteCheckRedirect(mkReq("https://box.internal:9443/x"), via); err == nil {
			t.Fatal("must stop after too many redirects")
		}
	})
	t.Run("grant-bearing request refuses ALL redirects", func(t *testing.T) {
		grantOrig := mkReq("https://box.internal:9443/v0/city/mc/sling")
		grantOrig.Header.Set("X-GC-City-Write", "payload.sig")
		// Even a same-host, same-scheme redirect must be refused: the grant is
		// single-use and bound to the exact original request, so following any
		// hop either breaks the binding or wastes the one-shot grant.
		if err := remoteCheckRedirect(mkReq("https://box.internal:9443/other"), []*http.Request{grantOrig}); err == nil {
			t.Fatal("a grant-bearing request must refuse even a same-host redirect")
		}
	})
}

func TestRemoteGrantEditor(t *testing.T) {
	t.Run("mutating request gets a grant bound to the request", func(t *testing.T) {
		body := []byte(`{"source":"pr-1"}`)
		var got GrantBinding
		c := &Client{grantSource: func(b GrantBinding) (string, error) { got = b; return "payload.sig", nil }}
		req, _ := http.NewRequest(http.MethodPost, "https://box/v0/city/mc/sling?x=1", bytes.NewReader(body))
		if err := remoteGrantEditor(c)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("X-GC-City-Write") != "payload.sig" {
			t.Errorf("grant header = %q", req.Header.Get("X-GC-City-Write"))
		}
		sum := sha256.Sum256(body)
		if got.Method != "POST" || got.Path != "/v0/city/mc/sling" || got.CanonicalQuery != "x=1" {
			t.Errorf("binding parts wrong: %+v", got)
		}
		if got.BodySHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("body hash = %q", got.BodySHA256)
		}
		if got.ReqDigest != citywriteauth.ReqDigest("POST", "/v0/city/mc/sling", "x=1", body) {
			t.Errorf("req digest = %q", got.ReqDigest)
		}
		// The actual send body must be intact (the editor reads a copy via GetBody).
		sent, _ := io.ReadAll(req.Body)
		if string(sent) != string(body) {
			t.Errorf("send body disturbed: %q", sent)
		}
	})
	t.Run("reads get no grant", func(t *testing.T) {
		called := false
		c := &Client{grantSource: func(GrantBinding) (string, error) { called = true; return "x.y", nil }}
		for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
			req, _ := http.NewRequest(m, "https://box/v0/city/mc/beads", nil)
			if err := remoteGrantEditor(c)(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if req.Header.Get("X-GC-City-Write") != "" {
				t.Errorf("%s must carry no grant", m)
			}
		}
		if called {
			t.Error("grant source must not be invoked for reads")
		}
	})
	t.Run("nil grant source attaches nothing", func(t *testing.T) {
		c := &Client{}
		req, _ := http.NewRequest(http.MethodPost, "https://box/x", bytes.NewReader([]byte(`{}`)))
		if err := remoteGrantEditor(c)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("X-GC-City-Write") != "" {
			t.Error("no grant without a grant source")
		}
	})
	t.Run("grant source error propagates", func(t *testing.T) {
		c := &Client{grantSource: func(GrantBinding) (string, error) { return "", errors.New("mint failed") }}
		req, _ := http.NewRequest(http.MethodPost, "https://box/x", bytes.NewReader([]byte(`{}`)))
		if err := remoteGrantEditor(c)(context.Background(), req); err == nil {
			t.Fatal("mint error must propagate")
		}
	})
	t.Run("empty token is an error", func(t *testing.T) {
		c := &Client{grantSource: func(GrantBinding) (string, error) { return "  ", nil }}
		req, _ := http.NewRequest(http.MethodPost, "https://box/x", bytes.NewReader([]byte(`{}`)))
		if err := remoteGrantEditor(c)(context.Background(), req); err == nil {
			t.Fatal("empty grant token must error")
		}
	})
	t.Run("body-less mutation binds the empty-body hash", func(t *testing.T) {
		var got GrantBinding
		c := &Client{grantSource: func(b GrantBinding) (string, error) { got = b; return "x.y", nil }}
		req, _ := http.NewRequest(http.MethodDelete, "https://box/v0/city/mc/workflow/w1", nil)
		if err := remoteGrantEditor(c)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		empty := sha256.Sum256(nil)
		if got.BodySHA256 != hex.EncodeToString(empty[:]) {
			t.Errorf("empty-body hash = %q", got.BodySHA256)
		}
		if got.ReqDigest != citywriteauth.ReqDigest("DELETE", "/v0/city/mc/workflow/w1", "", nil) {
			t.Errorf("req digest = %q", got.ReqDigest)
		}
	})
}

func TestRemoteTLSConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := remoteTLSConfig(RemoteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinVersion != tls.VersionTLS12 || cfg.RootCAs != nil || cfg.InsecureSkipVerify {
			t.Errorf("unexpected default cfg: %+v", cfg)
		}
	})
	t.Run("server name + insecure propagate", func(t *testing.T) {
		cfg, err := remoteTLSConfig(RemoteOptions{TLSServerName: "box", InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ServerName != "box" || !cfg.InsecureSkipVerify {
			t.Errorf("cfg = %+v", cfg)
		}
	})
	t.Run("missing ca file errors", func(t *testing.T) {
		if _, err := remoteTLSConfig(RemoteOptions{CAFile: "/no/such/ca.pem"}); err == nil {
			t.Fatal("missing ca_file must error")
		}
	})
	t.Run("garbage ca file errors", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.pem")
		if err := os.WriteFile(p, []byte("not a pem"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := remoteTLSConfig(RemoteOptions{CAFile: p}); err == nil {
			t.Fatal("garbage ca_file must error")
		}
	})
}

func TestRemoteAuthEditor(t *testing.T) {
	t.Run("attaches bearer", func(t *testing.T) {
		c := &Client{tokenSource: func() (string, error) { return "tok123", nil }}
		req, _ := http.NewRequest(http.MethodGet, "https://h/x", nil)
		if err := remoteAuthEditor(c)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("Authorization = %q", got)
		}
	})
	t.Run("nil source attaches nothing", func(t *testing.T) {
		c := &Client{}
		req, _ := http.NewRequest(http.MethodGet, "https://h/x", nil)
		if err := remoteAuthEditor(c)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("no bearer must be attached without a token source")
		}
	})
	t.Run("source error propagates", func(t *testing.T) {
		c := &Client{tokenSource: func() (string, error) { return "", errors.New("mint failed") }}
		req, _ := http.NewRequest(http.MethodGet, "https://h/x", nil)
		if err := remoteAuthEditor(c)(context.Background(), req); err == nil {
			t.Fatal("token source error must propagate")
		}
	})
}

func TestNewRemoteCityScopedClient_Basics(t *testing.T) {
	c, err := NewRemoteCityScopedClient("https://box:9443", "mc", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsRemote() {
		t.Error("client must be marked remote")
	}
	if c.cityName != "mc" || c.streamClient == nil {
		t.Errorf("client not wired: cityName=%q streamClient=%v", c.cityName, c.streamClient)
	}
	// A bad CA file is a hard error at construction (never a fallback stub).
	if _, err := NewRemoteCityScopedClient("https://box:9443", "mc", RemoteOptions{CAFile: "/no/such"}); err == nil {
		t.Fatal("bad ca_file must fail construction")
	}
}

// End-to-end over TLS: the REST shape verifies the server against a supplied CA,
// fails without it, succeeds with InsecureSkipVerify, and delivers both the
// X-GC-Request and Authorization headers.
func TestNewRemoteCityScopedClient_TLSAndHeaders(t *testing.T) {
	var gotAuth, gotReq string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReq = r.Header.Get("X-GC-Request")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	caPath := writeServerCA(t, srv)

	t.Run("verified with ca, headers delivered", func(t *testing.T) {
		gotAuth, gotReq = "", ""
		c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{
			CAFile: caPath,
			Token:  func() (string, error) { return "tok123", nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetStatus(); err != nil {
			// A decode/shape error is fine; a transport/TLS error is not.
			if IsConnError(err) {
				t.Fatalf("TLS/transport error with valid CA: %v", err)
			}
		}
		if gotReq != "true" {
			t.Errorf("X-GC-Request = %q, want true", gotReq)
		}
		if gotAuth != "Bearer tok123" {
			t.Errorf("Authorization = %q, want Bearer tok123", gotAuth)
		}
	})

	t.Run("fails without ca", func(t *testing.T) {
		c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetStatus(); err == nil || !IsConnError(err) {
			t.Fatalf("expected a TLS/transport error without CA, got %v", err)
		}
	})

	t.Run("insecure skip verify succeeds", func(t *testing.T) {
		gotReq = ""
		c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetStatus(); err != nil && IsConnError(err) {
			t.Fatalf("insecure_skip_verify should connect: %v", err)
		}
		if gotReq != "true" {
			t.Errorf("request did not reach server (X-GC-Request=%q)", gotReq)
		}
	})
}

func TestRequestIDForError(t *testing.T) {
	h := http.Header{}
	if got := RequestIDForError(h); got != "" {
		t.Errorf("absent id -> %q, want empty", got)
	}
	h.Set("X-GC-Request-Id", "abc123")
	if got := RequestIDForError(h); got != "request_id=abc123" {
		t.Errorf("RequestIDForError = %q", got)
	}
}

// The core no-fallback property (gate G1): every error a LOCAL client would
// fall back on is non-fallbackable for a REMOTE client, so a remote read/write
// error is surfaced instead of silently rerouted to a local store. The guard is
// on the client, not the error type, so one representative error proves it for
// all types (the check returns before any errors.As inspection).
func TestRemoteClientNeverFallsBack(t *testing.T) {
	remote := &Client{isRemote: true}
	conn := &connError{err: errors.New("connection refused")}

	if ShouldFallback(remote, conn) {
		t.Error("remote client must not fall back (ShouldFallback)")
	}
	if ShouldFallbackForRead(remote, conn) {
		t.Error("remote client must not fall back (ShouldFallbackForRead)")
	}
	if got := FallbackReason(remote, conn); got != "remote" {
		t.Errorf("FallbackReason = %q, want remote", got)
	}
	// Sanity: a nil (local) client still falls back on the very same error.
	if !ShouldFallback(nil, conn) {
		t.Error("nil/local client should still fall back on a conn error")
	}
	if !ShouldFallbackForRead(nil, conn) {
		t.Error("nil/local client should still fall back for reads on a conn error")
	}
}

// A cross-host redirect must be refused end-to-end (the second host never
// receives the request, so a bearer cannot leak to it).
func TestRemoteClient_RefusesCrossHostRedirect(t *testing.T) {
	var reachedSecond bool
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedSecond = true
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/v0/city/mc/status", http.StatusFound)
	}))
	defer first.Close()

	c, err := NewRemoteCityScopedClient(first.URL, "mc", RemoteOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetStatus()
	if err == nil || !IsConnError(err) {
		t.Fatalf("cross-host redirect must fail as a transport error, got %v", err)
	}
	if reachedSecond {
		t.Fatal("request must NOT reach the cross-host redirect target")
	}
}

// rtFunc adapts a function to an http.RoundTripper for reauth tests.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestReauthRoundTripper_RetriesOn401 proves a 401 triggers one re-mint + retry
// with the fresh bearer, recovering a token rejected before its expiry.
func TestReauthRoundTripper_RetriesOn401(t *testing.T) {
	var calls int
	var sawAuth []string
	base := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		sawAuth = append(sawAuth, req.Header.Get("Authorization"))
		code := http.StatusOK
		if calls == 1 {
			code = http.StatusUnauthorized
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("x")), Header: http.Header{}}, nil
	})
	rt := &reauthRoundTripper{base: base, refresh: func() (string, error) { return "fresh", nil }}
	req, _ := http.NewRequest("GET", "https://example/y", nil)
	req.Header.Set("Authorization", "Bearer stale")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (retry once)", calls)
	}
	if sawAuth[0] != "Bearer stale" || sawAuth[1] != "Bearer fresh" {
		t.Fatalf("auth seq = %v, want [Bearer stale, Bearer fresh]", sawAuth)
	}
}

// TestReauthRoundTripper_SkipsGrantedRequest proves a request carrying a
// single-use X-GC-City-Write grant is NOT retried (the grant cannot be re-minted
// at the transport layer).
func TestReauthRoundTripper_SkipsGrantedRequest(t *testing.T) {
	var calls int
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	rt := &reauthRoundTripper{base: base, refresh: func() (string, error) { return "fresh", nil }}
	req, _ := http.NewRequest("POST", "https://example/y", nil)
	req.Header.Set("X-GC-City-Write", "grant")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (granted request must not retry)", calls)
	}
}

// TestNewRemoteEventsClientAttachesAuth proves the events client (used by
// `gc events --context`) carries the X-GC-Request CSRF header and an
// Authorization bearer from opts.Token — so the events feed authenticates to a
// remote edge like the REST client does.
func TestNewRemoteEventsClientAttachesAuth(t *testing.T) {
	var gotAuth, gotCSRF string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCSRF = r.Header.Get("X-GC-Request")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	gen, err := NewRemoteEventsClient(srv.URL, RemoteOptions{
		Token: func() (string, error) { return "tok", nil },
	})
	if err != nil {
		t.Fatalf("NewRemoteEventsClient: %v", err)
	}
	if _, err := gen.StreamEvents(context.Background(), "mc", &genclient.StreamEventsParams{}); err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if gotCSRF != "true" {
		t.Fatalf("X-GC-Request = %q, want true", gotCSRF)
	}
}
