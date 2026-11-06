package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// principalProbe records the verified principal the downstream handler saw.
type principalProbe struct {
	ran   bool
	kid   string
	found bool
}

func (p *principalProbe) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ran = true
		p.kid, p.found = VerifiedPrincipal(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestVerifiedPrincipal_AbsentOnBareContext(t *testing.T) {
	if kid, ok := VerifiedPrincipal(context.Background()); ok || kid != "" {
		t.Fatalf("VerifiedPrincipal(bare ctx) = %q, %v; want \"\", false", kid, ok)
	}
	if kid, ok := VerifiedPrincipal(withVerifiedPrincipal(context.Background(), "")); ok || kid != "" {
		t.Fatalf("an empty kid must not count as a principal; got %q, %v", kid, ok)
	}
}

func TestWriteAuthMiddleware_AttachesVerifiedPrincipal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	body := []byte(`{"title":"x"}`)
	path := "/v0/city/acme/beads"
	tok := mintToken(t, priv, grantFor(now, "acme", http.MethodPost, path, body, "jp1"))

	var probe principalProbe
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, probe.handler())
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !probe.ran || rec.Code != http.StatusOK {
		t.Fatalf("valid grant should pass: ran=%v code=%d", probe.ran, rec.Code)
	}
	if !probe.found || probe.kid != "k1" {
		t.Fatalf("VerifiedPrincipal = %q, %v; want the grant kid \"k1\"", probe.kid, probe.found)
	}
}

func TestWriteAuthMiddleware_NoPrincipalWithoutVerifiedGrant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	_, otherPriv := mustKeypair(t)
	path := "/v0/city/acme/beads"
	body := []byte(`{"title":"x"}`)

	cases := []struct {
		name    string
		method  string
		path    string
		body    []byte
		grant   string
		wantRan bool
	}{
		{name: "read", method: http.MethodGet, path: path, wantRan: true},
		{name: "svc read", method: http.MethodGet, path: "/v0/city/acme/svc/foo", wantRan: true},
		{name: "supervisor-scope mutation", method: http.MethodPost, path: "/v0/city", body: []byte(`{}`), wantRan: true},
		{name: "missing grant", method: http.MethodPost, path: path, body: body},
		{
			name: "rejected grant", method: http.MethodPost, path: path, body: body,
			grant: mintToken(t, otherPriv, grantFor(now, "acme", http.MethodPost, path, body, "jp2")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var probe principalProbe
			h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, probe.handler())
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			req.Header.Set(csrfHeaderName, "1")
			if tc.grant != "" {
				req.Header.Set(writeAuthHeader, tc.grant)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if probe.ran != tc.wantRan {
				t.Fatalf("handler ran = %v, want %v", probe.ran, tc.wantRan)
			}
			if probe.found {
				t.Fatalf("handler saw principal %q without a verified grant", probe.kid)
			}
		})
	}
}

// The api request log line is written by withLogging, the outermost
// middleware, which cannot see context values set further in. The principal
// therefore travels back through the mutable slot withLogging installs.
func TestWithLogging_LogsVerifiedPrincipal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	_, otherPriv := mustKeypair(t)
	path := "/v0/city/acme/beads"
	body := []byte(`{"title":"x"}`)

	cases := []struct {
		name   string
		method string
		grant  string
		want   string
	}{
		{
			name: "verified mutation", method: http.MethodPost,
			grant: mintToken(t, priv, grantFor(now, "acme", http.MethodPost, path, body, "jl1")), want: "principal=k1",
		},
		{
			name: "rejected mutation", method: http.MethodPost,
			grant: mintToken(t, otherPriv, grantFor(now, "acme", http.MethodPost, path, body, "jl2")),
		},
		{name: "read", method: http.MethodGet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var probe principalProbe
			h := withLogging(writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, probe.handler()), requestAuditConfig{})
			req := httptest.NewRequest(tc.method, path, bytes.NewReader(body))
			req.Header.Set(csrfHeaderName, "1")
			if tc.grant != "" {
				req.Header.Set(writeAuthHeader, tc.grant)
			}
			var buf bytes.Buffer
			restore := captureLog(t, &buf)
			h.ServeHTTP(httptest.NewRecorder(), req)
			restore()

			line := buf.String()
			if !strings.Contains(line, "api: "+tc.method+" "+path) {
				t.Fatalf("missing api request log line: %q", line)
			}
			if tc.want != "" {
				if !strings.Contains(line, tc.want) {
					t.Fatalf("log line %q missing %q", line, tc.want)
				}
				return
			}
			if strings.Contains(line, "principal=") {
				t.Fatalf("log line %q names a principal without a verified grant", line)
			}
		})
	}
}
