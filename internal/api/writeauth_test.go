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
	return citywriteauth.Grant{
		Kid: "k1", Aud: writeAuthAudience, City: city, Epoch: 0,
		IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(),
		JTI: jti, Req: citywriteauth.ReqDigest(method, path, body),
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
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
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
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
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
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
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
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("PUT with valid grant: seen=%v code=%d", seen, rec.Code)
	}

	// DELETE without a grant is rejected.
	seen = false
	h = writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
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
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city/acme/agents", bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
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
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"name":"tampered"}`)))
	req.Header.Set(writeAuthHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusForbidden {
		t.Fatalf("body tamper: seen=%v code=%d", seen, rec.Code)
	}
}

func TestWriteAuthMiddleware_GatesBareCityPatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	path := "/v0/city/acme" // suspend/resume PATCH — no sub-resource

	// No grant -> 401.
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
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
	h = writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
	req = httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(body))
	req.Header.Set(writeAuthHeader, tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen || rec.Code != http.StatusOK {
		t.Fatalf("bare-city PATCH with valid grant: seen=%v code=%d", seen, rec.Code)
	}
}

func TestWriteAuthMiddleware_PassesThroughNonCityMutation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, "/v0/city", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !seen {
		t.Fatal("non-city-scoped mutation should pass through to existing guards")
	}
}

func TestWriteAuthMiddleware_ExemptsSvc(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, _ := mustKeypair(t)
	var seen bool
	var got []byte
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
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
		h := writeAuthMiddleware(v, echoNext(&seen, &got))
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set(writeAuthHeader, tok)
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
	h := writeAuthMiddleware(newTestWriteVerifier(t, pub, now), echoNext(&seen, &got))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(big))
	req.Header.Set(writeAuthHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: seen=%v code=%d want 413", seen, rec.Code)
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

func TestCityScopedObjectMutation(t *testing.T) {
	cases := []struct {
		path string
		city string
		ok   bool
	}{
		{"/v0/city/acme/agents", "acme", true},
		{"/v0/city/acme/providers/inline/x", "acme", true},
		{"/v0/city/acme/unregister", "acme", true},
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
