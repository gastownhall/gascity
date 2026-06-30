package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return pub, priv
}

func newTestWriteVerifier(t *testing.T, pub ed25519.PublicKey, now time.Time) *citywriteauth.Verifier {
	t.Helper()
	v, err := citywriteauth.New(citywriteauth.Options{
		Aud:    writeAuthAudience,
		Keys:   map[string]ed25519.PublicKey{"k1": pub},
		MaxTTL: 2 * time.Minute,
		Skew:   30 * time.Second,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New verifier: %v", err)
	}
	return v
}

func mintToken(t *testing.T, priv ed25519.PrivateKey, g citywriteauth.Grant) string {
	t.Helper()
	payload, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sig := ed25519.Sign(priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func grantFor(now time.Time, city, method, path string, body []byte, jti string) citywriteauth.Grant {
	return grantForQuery(now, city, method, path, "", body, jti)
}

// grantForQuery is grantFor with an explicit raw query bound into the request
// digest, for tests that exercise query-scoped authorization (for example the
// destructive ?delete=true workflow variant or scope_* selectors).
func grantForQuery(now time.Time, city, method, path, rawQuery string, body []byte, jti string) citywriteauth.Grant {
	return citywriteauth.Grant{
		Kid: "k1", Aud: writeAuthAudience, City: city, Epoch: 0,
		IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(),
		JTI: jti, Req: citywriteauth.ReqDigest(method, path, rawQuery, body),
	}
}

// echoNext records that the downstream handler ran and echoes the body it
// received, so tests can assert the middleware reset r.Body after buffering it.
func echoNext(seen *bool, gotBody *[]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = true
		b, _ := io.ReadAll(r.Body)
		*gotBody = b
		w.WriteHeader(http.StatusOK)
	})
}

func TestWriteAuthMiddleware_AllowsNonMutation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodGet, "/v0/city/acme/agents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("GET should pass: seen=%v code=%d", seen, rec.Code)
	}
}

func TestWriteAuthMiddleware_RejectsMissingGrant(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city/acme/agents", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen {
		t.Fatal("handler must not run without a grant")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

func TestWriteAuthMiddleware_AcceptsValidGrantAndResetsBody(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	body := []byte(`{"name":"worker"}`)
	path := "/v0/city/acme/agents"
	tok := mintToken(t, priv, grantFor(now, "acme", "POST", path, body, "j1"))
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("valid grant should pass: seen=%v code=%d", seen, rec.Code)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("handler body = %q, want %q (body reset failed)", got, body)
	}
}

func TestWriteAuthMiddleware_GatesOtherMutationMethods(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/providers/inline/x"

	// PUT with a valid grant passes.
	body := []byte(`{"k":"v"}`)
	tok := mintToken(t, priv, grantFor(now, "acme", "PUT", path, body, "jput"))
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("PUT with valid grant: seen=%v code=%d", seen, rec.Code)
	}

	// DELETE without a grant is rejected.
	seen = false
	h = writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodDelete, path, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusUnauthorized {
		t.Fatalf("DELETE without grant: seen=%v code=%d", seen, rec.Code)
	}
}

func TestWriteAuthMiddleware_RejectsWrongCity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	body := []byte(`{}`)
	tok := mintToken(t, priv, grantFor(now, "other", "POST", "/v0/city/acme/agents", body, "j1"))
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city/acme/agents", bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("wrong city: seen=%v code=%d", seen, rec.Code)
	}
}

func TestWriteAuthMiddleware_RejectsBodyTamper(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/agents"
	tok := mintToken(t, priv, grantFor(now, "acme", "POST", path, []byte(`{"name":"orig"}`), "j1"))
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"name":"tampered"}`)))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("body tamper: seen=%v code=%d", seen, rec.Code)
	}
}

// The query string is part of the request binding: a grant minted for one query
// variant must not authorize another. This guards the escalation where a
// cancel-only DELETE grant would otherwise authorize the ?delete=true permanent
// purge, and where a narrow scope selector could be widened by dropping it.
func TestWriteAuthMiddleware_RejectsQueryTamper(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	const path = "/v0/city/acme/workflow/wf-1"

	// Each call gets a fresh verifier so the single-use replay guard never
	// crosses cases; every request here carries an empty body.
	run := func(t *testing.T, tok, target string) (seen bool, code int) {
		t.Helper()
		var got []byte
		h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
		req := httptest.NewRequest(http.MethodDelete, target, nil)
		if tok != "" {
			req.Header.Set(writeAuthHeader, tok)
			req.Header.Set(csrfHeaderName, "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return seen, rec.Code
	}

	// A grant minted for the cancel-only DELETE (no query) must NOT authorize the
	// destructive ?delete=true purge: method+path+body are identical, only the
	// now-bound query differs.
	t.Run("cancel grant cannot purge", func(t *testing.T) {
		tok := mintToken(t, priv, grantForQuery(now, "acme", "DELETE", path, "", nil, "jq1"))
		if seen, code := run(t, tok, path+"?delete=true"); seen || code != http.StatusForbidden {
			t.Fatalf("?delete=true with cancel grant: seen=%v code=%d want 403", seen, code)
		}
	})

	// The grant minted for the purge variant authorizes exactly that request.
	t.Run("purge grant authorizes purge", func(t *testing.T) {
		tok := mintToken(t, priv, grantForQuery(now, "acme", "DELETE", path, "delete=true", nil, "jq2"))
		if seen, code := run(t, tok, path+"?delete=true"); !seen || code != http.StatusOK {
			t.Fatalf("?delete=true with matching grant: seen=%v code=%d want 200", seen, code)
		}
	})

	// A grant minted for a narrow scope selector must NOT be broadened to every
	// store by dropping the scope query.
	t.Run("scope selector cannot be dropped", func(t *testing.T) {
		tok := mintToken(t, priv, grantForQuery(now, "acme", "DELETE", path, "scope_kind=rig&scope_ref=alpha", nil, "jq3"))
		if seen, code := run(t, tok, path); seen || code != http.StatusForbidden {
			t.Fatalf("scope drop with scoped grant: seen=%v code=%d want 403", seen, code)
		}
	})

	// Canonicalization: the grant binds the semantic query, so reordering the
	// parameters on the wire still verifies.
	t.Run("query order independent", func(t *testing.T) {
		tok := mintToken(t, priv, grantForQuery(now, "acme", "DELETE", path, "scope_kind=rig&scope_ref=alpha", nil, "jq4"))
		if seen, code := run(t, tok, path+"?scope_ref=alpha&scope_kind=rig"); !seen || code != http.StatusOK {
			t.Fatalf("reordered query with matching grant: seen=%v code=%d want 200", seen, code)
		}
	})
}

func TestWriteAuthMiddleware_GatesBareCityPatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme" // suspend/resume PATCH — no sub-resource

	// No grant -> 401.
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(`{"suspended":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusUnauthorized {
		t.Fatalf("bare-city PATCH without grant: seen=%v code=%d", seen, rec.Code)
	}

	// Valid grant -> pass.
	body := []byte(`{"suspended":true}`)
	tok := mintToken(t, priv, grantFor(now, "acme", "PATCH", path, body, "jpatch"))
	seen = false
	h = writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("bare-city PATCH with valid grant: seen=%v code=%d", seen, rec.Code)
	}
}

// POST /v0/city (city registry creation) is intentionally outside the
// write-auth gate: a grant binds a path-resident city name, and a not-yet-
// created city carries none, so creation stays governed by the existing
// supervisor-registry guards (CSRF/read-only). This is the documented carve-out
// in cityScopedObjectMutation and docs/reference/config.md, not an oversight.
func TestWriteAuthMiddleware_PassesThroughCityRegistryCreate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen {
		t.Fatal("POST /v0/city (registry creation) must pass through to the supervisor-registry guards")
	}
}

// The gate is a general per-city write gate, not config-only: every mutating
// request to an already-registered city is gated, including high-traffic
// runtime writes such as bead mutations, mail sends, and session message
// submits. Pinning this guards the documented contract against a future
// narrowing of the matcher silently dropping the gate on non-config routes.
func TestWriteAuthMiddleware_GatesNonConfigPerCityWrites(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	for _, path := range []string{
		"/v0/city/acme/bead/bd-1/update",   // bead mutation
		"/v0/city/acme/mail",               // mail send
		"/v0/city/acme/session/s-1/submit", // session message submit
	} {
		t.Run(path, func(t *testing.T) {
			var seen bool
			var got []byte
			h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if seen || rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s without grant: seen=%v code=%d want 401", path, seen, rec.Code)
			}
		})
	}
}

// A control character decoded into the gated path (for example %0A) is rejected
// before the digest is computed, closing the newline-delimiter ambiguity in the
// preimage. No grant could bind such a path and it fails routing anyway, so the
// guard is pure fail-closed defense in depth.
func TestWriteAuthMiddleware_RejectsControlCharPath(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city/acme/agents", strings.NewReader(`{}`))
	req.URL.Path = "/v0/city/acme/agents\ndelete=true" // decoded %0A in the path
	req.Header.Set(writeAuthHeader, "bogus")           // path check must fire before token checks
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusBadRequest {
		t.Fatalf("control-char path: seen=%v code=%d want 400", seen, rec.Code)
	}
}

func TestWriteAuthMiddleware_ExemptsSvc(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city/acme/svc/foo", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen {
		t.Fatal("/svc/ pass-through must be exempt from write-auth")
	}
}

func TestWriteAuthMiddleware_RejectsReplay(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	body := []byte(`{}`)
	path := "/v0/city/acme/agents"
	tok := mintToken(t, priv, grantFor(now, "acme", "POST", path, body, "j1"))
	v := newTestWriteVerifier(t, pub, now)
	do := func() int {
		var seen bool
		var got []byte
		h := writeAuthMiddleware(v, false, echoNext(&seen, &got))
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set(writeAuthHeader, tok)
		req.Header.Set(csrfHeaderName, "1")
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

func TestWriteAuthMiddleware_RejectsOversizeBody(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme/agents"
	big := bytes.Repeat([]byte("a"), maxWriteBodyBytes+1)
	tok := mintToken(t, priv, grantFor(now, "acme", "POST", path, big, "j1"))
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(big))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: seen=%v code=%d want 413", seen, rec.Code)
	}
}

// A valid grant on a request missing the X-GC-Request CSRF header is rejected
// before the grant is verified, so the single-use jti is NOT consumed. This is
// the regression guard for the ordering bug where the downstream CSRF rejection
// ran only after write-auth had already spent the grant, making the caller's
// legitimate retry look like a replay.
func TestWriteAuthMiddleware_DoesNotConsumeGrantWhenCSRFMissing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	body := []byte(`{"name":"worker"}`)
	path := "/v0/city/acme/agents"
	tok := mintToken(t, priv, grantFor(now, "acme", "POST", path, body, "j1"))
	v := newTestWriteVerifier(t, pub, now)

	// First attempt: valid grant, but the client forgot X-GC-Request.
	var seen bool
	var got []byte
	h := writeAuthMiddleware(v, false, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: seen=%v code=%d want 403 and handler not reached", seen, rec.Code)
	}

	// Retry the SAME grant with the CSRF header: it must still verify, proving the
	// first attempt did not consume the jti.
	seen = false
	h = writeAuthMiddleware(v, false, echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("retry with CSRF: seen=%v code=%d want 200 (grant must not have been consumed)", seen, rec.Code)
	}
}

// In read-only mode every mutation is refused, so a valid grant must be rejected
// before it is verified and the single-use jti must survive. Otherwise a grant
// is silently burned against a server that never performs the write.
func TestWriteAuthMiddleware_DoesNotConsumeGrantWhenReadOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	body := []byte(`{"name":"worker"}`)
	path := "/v0/city/acme/agents"
	tok := mintToken(t, priv, grantFor(now, "acme", "POST", path, body, "j1"))
	v := newTestWriteVerifier(t, pub, now)

	// Read-only middleware: a fully valid, CSRF-bearing request is still refused.
	var seen bool
	var got []byte
	h := writeAuthMiddleware(v, true, echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("read-only: seen=%v code=%d want 403 and handler not reached", seen, rec.Code)
	}

	// The same grant verifies once mutations are allowed, proving the read-only
	// rejection did not consume the jti.
	seen = false
	h = writeAuthMiddleware(v, false, echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	req.Header.Set(csrfHeaderName, "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("after read-only lifted: seen=%v code=%d want 200 (grant must not have been consumed)", seen, rec.Code)
	}
}

// End-to-end through the full SupervisorMux middleware chain: a city-scoped
// mutation with no grant must be rejected before dispatch. Guards against a
// middleware-ordering regression that would let writes slip past the gate.
func TestSupervisorMux_WriteAuthGuardsMutation(t *testing.T) {
	pub, _ := mustKeypair(t)
	v := newTestWriteVerifier(t, pub, time.Now())
	sm := NewSupervisorMux(nil, nil, false, "test", "", time.Now()).
		WithAnyHostAllowed().
		WithWriteAuth(v)

	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v0/city/acme/agents", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-GC-Request", "1") // would satisfy CSRF if it were reached
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mutation without grant: status=%d want 401", resp.StatusCode)
	}
}

// Opt-in/off-by-default contract: with no verifying key configured the gate is
// not installed, so a first-party mutation carrying only the CSRF header — what
// the bundled gc API client and dashboard SPA send — is never turned away for a
// missing grant. This is the other half of the write-auth contract: enabling
// the gate is what fronts first-party clients with the authority
// (TestSupervisorMux_WriteAuthGuardsMutation covers the gate-on rejection);
// leaving it unconfigured preserves the prior CSRF/read-only behavior.
func TestSupervisorMux_NoWriteAuthAllowsFirstPartyMutation(t *testing.T) {
	sm := NewSupervisorMux(nil, nil, false, "test", "", time.Now()).
		WithAnyHostAllowed()
	if sm.writeAuth != nil {
		t.Fatal("write-auth must be disabled when no key is configured")
	}

	srv := httptest.NewServer(sm.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v0/city/acme/agents", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-GC-Request", "true") // the only header first-party clients attach
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The gate is off, so whatever the (backend-less) downstream does, the
	// response must not be the write-auth missing-grant rejection.
	if resp.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), writeAuthHeader) {
			t.Fatalf("first-party mutation gated by write-auth when no key configured: %s", body)
		}
	}
}

func TestCityScopedObjectMutation(t *testing.T) {
	cases := []struct {
		path string
		city string
		ok   bool
	}{
		{"/v0/city/acme/agents", "acme", true},
		{"/v0/city/acme/providers/inline/x", "acme", true},
		{"/v0/city/acme/unregister", "acme", true},
		{"/v0/city/acme/bead/bd-1/update", "acme", true},   // runtime write: bead mutation
		{"/v0/city/acme/mail", "acme", true},               // runtime write: mail send
		{"/v0/city/acme/session/s-1/submit", "acme", true}, // runtime write: session submit
		{"/v0/city/acme/svc/foo", "", false},
		{"/v0/city/acme/svc", "", false},
		{"/v0/city/acme", "acme", true}, // bare-city PATCH (suspend/resume) is gated
		{"/v0/city/acme/", "", false},
		{"/v0/city//agents", "", false},
		{"/v0/city", "", false},
		{"/v0/cities", "", false},
		{"/health", "", false},
	}
	for _, c := range cases {
		city, ok := cityScopedObjectMutation(c.path)
		if ok != c.ok || city != c.city {
			t.Errorf("%q: got (%q,%v) want (%q,%v)", c.path, city, ok, c.city, c.ok)
		}
	}
}

func TestParseVerifyKeys(t *testing.T) {
	pub, _ := mustKeypair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)

	keys, err := parseVerifyKeys("k1:" + b64)
	if err != nil {
		t.Fatalf("parse single: %v", err)
	}
	if !bytes.Equal(keys["k1"], pub) {
		t.Fatal("k1 pubkey mismatch")
	}

	pub2, _ := mustKeypair(t)
	keys, err = parseVerifyKeys("k1:" + b64 + ", k2:" + base64.StdEncoding.EncodeToString(pub2))
	if err != nil || len(keys) != 2 {
		t.Fatalf("parse multi: err=%v n=%d", err, len(keys))
	}

	for _, bad := range []string{
		"",
		"k1",
		"k1:not-base64!!!",
		"k1:" + base64.StdEncoding.EncodeToString([]byte("too-short")),
		":" + b64,
	} {
		if _, err := parseVerifyKeys(bad); err == nil {
			t.Errorf("parseVerifyKeys(%q) should error", bad)
		}
	}
}

func TestResolveWriteAuthVerifier(t *testing.T) {
	pub, _ := mustKeypair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)

	t.Run("not enabled returns nil", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
		v, err := ResolveWriteAuthVerifier("", false)
		if err != nil || v != nil {
			t.Fatalf("want (nil,nil) got (%v,%v)", v, err)
		}
	})
	t.Run("env key enables", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "k1:"+b64)
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
		v, err := ResolveWriteAuthVerifier("", false)
		if err != nil || v == nil {
			t.Fatalf("env key should enable: (%v,%v)", v, err)
		}
	})
	t.Run("config fallback when env empty", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
		v, err := ResolveWriteAuthVerifier("k1:"+b64, false)
		if err != nil || v == nil {
			t.Fatalf("config key should enable: (%v,%v)", v, err)
		}
	})
	t.Run("env required but missing errors (fail-closed boot)", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "1")
		if _, err := ResolveWriteAuthVerifier("", false); err == nil {
			t.Fatal("env-required + missing key must error")
		}
	})
	t.Run("config required but missing errors (fail-closed boot)", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
		if _, err := ResolveWriteAuthVerifier("", true); err == nil {
			t.Fatal("config-required + missing key must error")
		}
	})
}

func TestInstallWriteAuth(t *testing.T) {
	pub, _ := mustKeypair(t)
	b64 := base64.StdEncoding.EncodeToString(pub)

	t.Run("installs the gate when a key is configured", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
		sm := NewSupervisorMux(nil, nil, false, "t", "", time.Now())
		if err := InstallWriteAuth(sm, "k1:"+b64, false); err != nil {
			t.Fatalf("install: %v", err)
		}
		if sm.writeAuth == nil {
			t.Fatal("verifier was not installed")
		}
	})
	t.Run("no-op when unconfigured", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
		sm := NewSupervisorMux(nil, nil, false, "t", "", time.Now())
		if err := InstallWriteAuth(sm, "", false); err != nil {
			t.Fatalf("install: %v", err)
		}
		if sm.writeAuth != nil {
			t.Fatal("gate should not be installed when unconfigured")
		}
	})
	t.Run("errors when required but missing", func(t *testing.T) {
		t.Setenv("GC_CITY_WRITE_PUBKEY", "")
		t.Setenv("GC_CITY_WRITE_REQUIRED", "")
		sm := NewSupervisorMux(nil, nil, false, "t", "", time.Now())
		if err := InstallWriteAuth(sm, "", true); err == nil {
			t.Fatal("expected fail-closed error")
		}
	})
}
