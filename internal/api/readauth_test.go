package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func newTestReadVerifier(t *testing.T, pub ed25519.PublicKey, now time.Time) *citywriteauth.Verifier {
	t.Helper()
	v, err := citywriteauth.New(citywriteauth.Options{
		Aud:    readAuthAudience,
		Keys:   map[string]ed25519.PublicKey{"k1": pub},
		MaxTTL: 2 * time.Minute,
		Skew:   30 * time.Second,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New read verifier: %v", err)
	}
	return v
}

// readGrant mints a read grant bound to a GET/HEAD request. The body is always
// empty for reads, so the digest is computed over nil (== the empty-body hash).
func readGrant(now time.Time, city, method, path, rawQuery, jti string) citywriteauth.Grant {
	return citywriteauth.Grant{
		Kid: "k1", Aud: readAuthAudience, City: city, Epoch: 0,
		IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(),
		JTI: jti, Req: citywriteauth.ReqDigest(method, path, rawQuery, nil),
	}
}

// Read-auth is the jurisdiction of GET/HEAD only. A mutation passes straight
// through to the next handler — write-auth (if any) gates it, not this.
func TestReadAuthMiddleware_IgnoresMutations(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city/acme/agents", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("POST must pass through read-auth untouched: seen=%v code=%d", seen, rec.Code)
	}
}

func TestReadAuthMiddleware_RejectsMissingGrant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodGet, "/v0/city/acme/agents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen {
		t.Fatal("handler must not run without a read grant")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// A valid GET grant passes and pins the empty-body digest: the minter binds
// ReqDigest("GET", path, "", nil) and the middleware must compute the same.
func TestReadAuthMiddleware_AcceptsValidGrant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/beads"
	tok := mintToken(t, priv, readGrant(now, "acme", "GET", path, "", "jr1"))
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(readAuthHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("valid read grant should pass: seen=%v code=%d", seen, rec.Code)
	}
}

// HEAD is gated like GET, and the method is part of the request binding: a GET
// grant must not authorize a HEAD of the same path.
func TestReadAuthMiddleware_GatesHEAD(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/beads"

	// HEAD without a grant -> 401.
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodHead, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusUnauthorized {
		t.Fatalf("HEAD without grant: seen=%v code=%d want 401", seen, rec.Code)
	}

	// A GET-bound grant must NOT authorize a HEAD (method is in the preimage).
	seen = false
	getTok := mintToken(t, priv, readGrant(now, "acme", "GET", path, "", "jhead-get"))
	h = readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodHead, path, nil)
	req.Header.Set(readAuthHeader, getTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("GET grant on HEAD: seen=%v code=%d want 403", seen, rec.Code)
	}

	// A HEAD-bound grant authorizes the HEAD.
	seen = false
	headTok := mintToken(t, priv, readGrant(now, "acme", "HEAD", path, "", "jhead-ok"))
	h = readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodHead, path, nil)
	req.Header.Set(readAuthHeader, headTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("HEAD with matching grant: seen=%v code=%d want 200", seen, rec.Code)
	}
}

// Audience isolation: a write grant (aud gc-city-write) must not authorize a
// read. The read verifier's audience is gc-city-read.
func TestReadAuthMiddleware_RejectsWriteAudienceGrant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/beads"
	// A grant that would be valid for a write, presented on a read.
	writeTok := mintToken(t, priv, citywriteauth.Grant{
		Kid: "k1", Aud: writeAuthAudience, City: "acme", Epoch: 0,
		IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(),
		JTI: "jw1", Req: citywriteauth.ReqDigest("GET", path, "", nil),
	})
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(readAuthHeader, writeTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("write-aud grant on read: seen=%v code=%d want 403", seen, rec.Code)
	}
}

// The converse of the aud-isolation guard: a read grant must not authorize a
// write through the write-auth gate.
func TestWriteAuthMiddleware_RejectsReadAudienceGrant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/agents"
	body := []byte(`{}`)
	readTok := mintToken(t, priv, citywriteauth.Grant{
		Kid: "k1", Aud: readAuthAudience, City: "acme", Epoch: 0,
		IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(),
		JTI: "jr-on-w", Req: citywriteauth.ReqDigest("POST", path, "", body),
	})
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, readTok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("read-aud grant on write: seen=%v code=%d want 403", seen, rec.Code)
	}
}

// v1 scope boundary: read-auth gates only the typed /v0/city/{name} reads.
// Supervisor-scope reads, the aggregate event feed, the dashboard host /api/*
// plane (a parallel per-city read surface), static, and the /svc/ pass-through
// all fall through ungated in v1 — pinned here so the boundary is explicit and a
// future narrowing/widening of the grammar is caught. Gating /api/* and
// /v0/events is tracked follow-up under the supervisor-scope grant.
func TestReadAuthMiddleware_PassesThroughNonCityPaths(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	for _, path := range []string{
		"/v0/cities",
		"/health",
		"/openapi.json",
		"/v0/events",                       // supervisor-scope aggregate feed (deferred)
		"/v0/events/stream",                // supervisor-scope aggregate SSE (deferred)
		"/api/city/acme/supervisor-status", // dashboard host plane per-city read (deferred)
		"/api/city/acme/runs/r-1/detail",   // dashboard host plane per-city read (deferred)
		"/v0/city/acme/svc/foo",
		"/v0/city/acme/", // empty sub-resource
		"/v0/city",
	} {
		t.Run(path, func(t *testing.T) {
			var seen bool
			var got []byte
			h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if !seen {
				t.Fatalf("%s must pass through read-auth (not city-scoped): code=%d", path, rec.Code)
			}
		})
	}
}

// The query string is part of the read binding: a grant for one query variant
// must not authorize another, and reordered params still verify.
func TestReadAuthMiddleware_QueryBound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	const path = "/v0/city/acme/beads"

	run := func(t *testing.T, tok, target string) (seen bool, code int) {
		t.Helper()
		var got []byte
		h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if tok != "" {
			req.Header.Set(readAuthHeader, tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return seen, rec.Code
	}

	t.Run("scoped grant cannot be widened by dropping the query", func(t *testing.T) {
		tok := mintToken(t, priv, readGrant(now, "acme", "GET", path, "status=open", "jq1"))
		if seen, code := run(t, tok, path); seen || code != http.StatusForbidden {
			t.Fatalf("query drop: seen=%v code=%d want 403", seen, code)
		}
	})
	t.Run("matching query authorizes", func(t *testing.T) {
		tok := mintToken(t, priv, readGrant(now, "acme", "GET", path, "status=open", "jq2"))
		if seen, code := run(t, tok, path+"?status=open"); !seen || code != http.StatusOK {
			t.Fatalf("matching query: seen=%v code=%d want 200", seen, code)
		}
	})
	t.Run("query order independent", func(t *testing.T) {
		tok := mintToken(t, priv, readGrant(now, "acme", "GET", path, "a=1&b=2", "jq3"))
		if seen, code := run(t, tok, path+"?b=2&a=1"); !seen || code != http.StatusOK {
			t.Fatalf("reordered query: seen=%v code=%d want 200", seen, code)
		}
	})
}

// SSE stream endpoints are city-scoped GETs and are gated at connect (admission).
func TestReadAuthMiddleware_GatesSSEAdmission(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/events/stream"

	// No grant -> 401 at admission.
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusUnauthorized {
		t.Fatalf("SSE without grant: seen=%v code=%d want 401", seen, rec.Code)
	}

	// Valid grant -> admitted.
	seen = false
	tok := mintToken(t, priv, readGrant(now, "acme", "GET", path, "", "jsse"))
	h = readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(readAuthHeader, tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("SSE with grant: seen=%v code=%d want 200", seen, rec.Code)
	}
}

func TestReadAuthMiddleware_RejectsControlCharPath(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodGet, "/v0/city/acme/beads", nil)
	req.URL.Path = "/v0/city/acme/beads\nx" // decoded %0A in path
	req.Header.Set(readAuthHeader, "bogus") // path check fires before token checks
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusBadRequest {
		t.Fatalf("control-char path: seen=%v code=%d want 400", seen, rec.Code)
	}
}

func TestReadAuthMiddleware_RejectsReplay(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/beads"
	tok := mintToken(t, priv, readGrant(now, "acme", "GET", path, "", "jrep"))
	v := newTestReadVerifier(t, pub, now)
	do := func() int {
		var seen bool
		var got []byte
		h := readAuthMiddleware(v, echoNext(&seen, &got))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(readAuthHeader, tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("first: code=%d want 200", code)
	}
	if code := do(); code != http.StatusForbidden {
		t.Fatalf("replay: code=%d want 403", code)
	}
}

func TestResolveReadAuthVerifier(t *testing.T) {
	pub, _ := mustKeypair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)

	t.Run("not enabled returns nil", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "")
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		v, err := ResolveReadAuthVerifier("", false)
		if err != nil || v != nil {
			t.Fatalf("want (nil,nil) got (%v,%v)", v, err)
		}
	})
	t.Run("env key enables", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "k1:"+b64)
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		v, err := ResolveReadAuthVerifier("", false)
		if err != nil || v == nil {
			t.Fatalf("env key should enable: (%v,%v)", v, err)
		}
	})
	t.Run("config fallback when env empty", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "")
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		v, err := ResolveReadAuthVerifier("k1:"+b64, false)
		if err != nil || v == nil {
			t.Fatalf("config key should enable: (%v,%v)", v, err)
		}
	})
	t.Run("env required but missing errors (fail-closed boot)", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "")
		t.Setenv("GC_CITY_READ_REQUIRED", "1")
		if _, err := ResolveReadAuthVerifier("", false); err == nil {
			t.Fatal("env-required + missing key must error")
		}
	})
	t.Run("config required but missing errors (fail-closed boot)", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "")
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		if _, err := ResolveReadAuthVerifier("", true); err == nil {
			t.Fatal("config-required + missing key must error")
		}
	})
}

func TestInstallReadAuth(t *testing.T) {
	pub, _ := mustKeypair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)

	t.Run("installs the gate when a key is configured", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "")
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		sm := NewSupervisorMux(nil, nil, false, "t", "", time.Now())
		if err := InstallReadAuth(sm, "k1:"+b64, false); err != nil {
			t.Fatalf("install: %v", err)
		}
		if sm.readAuth == nil {
			t.Fatal("read verifier was not installed")
		}
	})
	t.Run("no-op when unconfigured", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "")
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		sm := NewSupervisorMux(nil, nil, false, "t", "", time.Now())
		if err := InstallReadAuth(sm, "", false); err != nil {
			t.Fatalf("install: %v", err)
		}
		if sm.readAuth != nil {
			t.Fatal("gate should not be installed when unconfigured")
		}
	})
	t.Run("errors when required but missing", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "")
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		sm := NewSupervisorMux(nil, nil, false, "t", "", time.Now())
		if err := InstallReadAuth(sm, "", true); err == nil {
			t.Fatal("expected fail-closed error")
		}
	})
}

// End-to-end through the full SupervisorMux middleware chain: a city-scoped read
// with no grant is rejected before dispatch when read-auth is installed.
func TestSupervisorMux_ReadAuthGuardsRead(t *testing.T) {
	pub, _ := mustKeypair(t)
	v := newTestReadVerifier(t, pub, time.Now())
	sm := NewSupervisorMux(nil, nil, false, "test", "", time.Now()).
		WithAnyHostAllowed().
		WithReadAuth(v)

	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v0/city/acme/beads")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("read without grant: status=%d want 401", resp.StatusCode)
	}
}

// Opt-in/off-by-default: with no key configured the read gate is not installed,
// so a first-party city read is never turned away for a missing grant.
func TestSupervisorMux_NoReadAuthAllowsOpenReads(t *testing.T) {
	sm := NewSupervisorMux(nil, nil, false, "test", "", time.Now()).
		WithAnyHostAllowed()
	if sm.readAuth != nil {
		t.Fatal("read-auth must be disabled when no key is configured")
	}

	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v0/city/acme/beads")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Gate is off: whatever the backend-less downstream returns, it must not be
	// the read-auth missing-grant rejection.
	if resp.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		if bytes.Contains(body, []byte(readAuthHeader)) {
			t.Fatalf("first-party read gated by read-auth when no key configured: %s", body)
		}
	}
}

// A read grant bound to a different city must not authorize a read of this one:
// the City claim is part of the verified expectation (mirror of the write gate's
// RejectsWrongCity).
func TestReadAuthMiddleware_RejectsWrongCity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/beads"
	// Digest binds the real path; only the City claim is wrong.
	tok := mintToken(t, priv, readGrant(now, "other", "GET", path, "", "jwc"))
	var seen bool
	var got []byte
	h := readAuthMiddleware(newTestReadVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(readAuthHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("wrong city: seen=%v code=%d want 403", seen, rec.Code)
	}
}

// The prior middleware tests build verifiers directly. This drives the gate
// through the PRODUCTION ResolveReadAuthVerifier path (env key, real clock,
// audience, and epoch floor) so the wiring — not just the test harness — is
// covered.
func TestReadAuthMiddleware_WithResolvedVerifier(t *testing.T) {
	pub, priv := mustKeypair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)
	path := "/v0/city/acme/beads"

	// mintReal signs a grant against the real clock (the resolved verifier uses
	// time.Now, so a fixed test clock would fall outside its skew window).
	mintReal := func(aud string, epoch int64, jti string) string {
		now := time.Now()
		return mintToken(t, priv, citywriteauth.Grant{
			Kid: "k1", Aud: aud, City: "acme", Epoch: epoch,
			IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(),
			JTI: jti, Req: citywriteauth.ReqDigest("GET", path, "", nil),
		})
	}
	drive := func(t *testing.T, v *citywriteauth.Verifier, tok string) (seen bool, code int) {
		t.Helper()
		var got []byte
		h := readAuthMiddleware(v, echoNext(&seen, &got))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(readAuthHeader, tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return seen, rec.Code
	}

	t.Run("resolved read verifier accepts a gc-city-read grant", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "k1:"+b64)
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		t.Setenv("GC_CITY_READ_EPOCH_FLOOR", "")
		v, err := ResolveReadAuthVerifier("", false)
		if err != nil || v == nil {
			t.Fatalf("resolve: (%v,%v)", v, err)
		}
		if seen, code := drive(t, v, mintReal(readAuthAudience, 0, "jrv-ok")); !seen || code != http.StatusOK {
			t.Fatalf("resolved-verifier read grant: seen=%v code=%d want 200", seen, code)
		}
	})

	t.Run("resolved read verifier rejects a write-audience grant", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "k1:"+b64)
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		t.Setenv("GC_CITY_READ_EPOCH_FLOOR", "")
		v, err := ResolveReadAuthVerifier("", false)
		if err != nil || v == nil {
			t.Fatalf("resolve: (%v,%v)", v, err)
		}
		if seen, code := drive(t, v, mintReal(writeAuthAudience, 0, "jrv-wrongaud")); seen || code != http.StatusForbidden {
			t.Fatalf("write-aud on resolved read verifier: seen=%v code=%d want 403", seen, code)
		}
	})

	t.Run("epoch floor revokes grants below the floor", func(t *testing.T) {
		t.Setenv("GC_CITY_READ_PUBKEY", "k1:"+b64)
		t.Setenv("GC_CITY_READ_REQUIRED", "")
		t.Setenv("GC_CITY_READ_EPOCH_FLOOR", "5")
		v, err := ResolveReadAuthVerifier("", false)
		if err != nil || v == nil {
			t.Fatalf("resolve: (%v,%v)", v, err)
		}
		if seen, code := drive(t, v, mintReal(readAuthAudience, 4, "jrv-e4")); seen || code != http.StatusForbidden {
			t.Fatalf("epoch 4 below floor 5: seen=%v code=%d want 403", seen, code)
		}
		if seen, code := drive(t, v, mintReal(readAuthAudience, 5, "jrv-e5")); !seen || code != http.StatusOK {
			t.Fatalf("epoch 5 at floor 5: seen=%v code=%d want 200", seen, code)
		}
	})
}

// End-to-end acceptance + single-use through the full SupervisorMux chain: a
// valid read grant clears the gate (the backend-less downstream then 404s, which
// is fine), and re-presenting the single-use token is rejected as a replay.
func TestSupervisorMux_ReadAuthAcceptsValidGrant(t *testing.T) {
	now := time.Now()
	pub, priv := mustKeypair(t)
	sm := NewSupervisorMux(nil, nil, false, "test", "", now).
		WithAnyHostAllowed().
		WithReadAuth(newTestReadVerifier(t, pub, now))
	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()

	const target = "/v0/city/acme/beads"
	tok := mintToken(t, priv, readGrant(now, "acme", "GET", target, "status=open", "je2e"))

	do := func() int {
		req, err := http.NewRequest(http.MethodGet, srv.URL+target+"?status=open", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set(readAuthHeader, tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	// First presentation clears the gate (not a read-auth rejection).
	if code := do(); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Fatalf("valid read grant should clear the gate, got %d", code)
	}
	// Single-use: the same token replayed is rejected.
	if code := do(); code != http.StatusForbidden {
		t.Fatalf("replayed read grant: code=%d want 403", code)
	}
}

// Reads must keep working in read-only mode — the default posture of the
// non-localhost binds that will enable read-auth. A valid read grant clears both
// gates even when readOnly is true (which only refuses mutations).
func TestSupervisorMux_ReadAuthPassesInReadOnlyMode(t *testing.T) {
	now := time.Now()
	pub, priv := mustKeypair(t)
	sm := NewSupervisorMux(nil, nil, true /* readOnly */, "test", "", now).
		WithAnyHostAllowed().
		WithReadAuth(newTestReadVerifier(t, pub, now))
	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()

	const target = "/v0/city/acme/beads"
	tok := mintToken(t, priv, readGrant(now, "acme", "GET", target, "", "jro"))
	req, err := http.NewRequest(http.MethodGet, srv.URL+target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(readAuthHeader, tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("read in read-only mode must clear the gate, got %d", resp.StatusCode)
	}
}
