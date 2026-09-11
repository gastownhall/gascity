package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// realClockVerifier trusts pub under the real wall clock, matching a grant the
// client mints with time.Now.
func realClockVerifier(t *testing.T, pub ed25519.PublicKey) *citywriteauth.Verifier {
	t.Helper()
	v, err := citywriteauth.New(citywriteauth.Options{
		Aud:    citywriteauth.AudienceCityWrite,
		Keys:   map[string]ed25519.PublicKey{"k1": pub},
		MaxTTL: 2 * time.Minute,
		Skew:   30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// signingGrantSource is a test GrantSource: it signs a citywriteauth.Grant bound
// to the request the editor computed, standing in for a real gc-write-mint. jti
// is monotonic so each mint is single-use.
func signingGrantSource(priv ed25519.PrivateKey, city string) GrantSource {
	var n int64
	return func(b GrantBinding) (string, error) {
		n++
		now := time.Now()
		g := citywriteauth.Grant{
			Kid:  "k1",
			Aud:  citywriteauth.AudienceCityWrite,
			City: city,
			IAT:  now.Unix(),
			Exp:  now.Add(time.Minute).Unix(),
			JTI:  fmt.Sprintf("jti-%d", n),
			Req:  b.ReqDigest,
		}
		payload, err := json.Marshal(g)
		if err != nil {
			return "", err
		}
		sig := ed25519.Sign(priv, payload)
		return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
	}
}

// End-to-end capstone: a client-minted grant flows through the real transport
// grant editor to the real write-auth middleware + verifier, which accepts the
// mutation; a grant-less client is refused, non-fallbackably.
func TestWriteAuthE2E_SlingThroughGrant(t *testing.T) {
	pub, priv := mustKeypair(t)
	v := realClockVerifier(t, pub)

	var reachedBody string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reachedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"routed","target":"mayor","bead":"BL-1"}`))
	})
	srv := httptest.NewServer(writeAuthMiddleware(v, false, handler))
	defer srv.Close()

	// With a grant source, the mutation verifies and reaches the handler.
	c, err := NewRemoteCityScopedClient(srv.URL, "acme", RemoteOptions{Grant: signingGrantSource(priv, "acme")})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Sling(SlingRequest{Target: "mayor", Bead: "BL-1"})
	if err != nil {
		t.Fatalf("granted sling must succeed: %v", err)
	}
	if res.Status != "routed" || res.Target != "mayor" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(reachedBody, `"bead":"BL-1"`) {
		t.Errorf("handler saw body %q", reachedBody)
	}

	// A grant-less client against the same hardened city is refused (401) and the
	// error is non-fallbackable (gate G1).
	noGrant, err := NewRemoteCityScopedClient(srv.URL, "acme", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = noGrant.Sling(SlingRequest{Target: "mayor", Bead: "BL-1"})
	if err == nil {
		t.Fatal("a grant-less mutation against a hardened city must fail")
	}
	if ShouldFallback(noGrant, err) {
		t.Error("a remote write-auth rejection must be non-fallbackable (gate G1)")
	}
}

// The grant is bound to the exact query and (decoded) path: a grant minted for
// one query does not authorize a different one, verified through the real
// middleware which independently recomputes the digest from the wire request.
func TestWriteAuthE2E_GrantBindsQueryAndEncodedPath(t *testing.T) {
	pub, priv := mustKeypair(t)
	v := realClockVerifier(t, pub)
	var reached bool
	h := writeAuthMiddleware(v, false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{"x":1}`)
	// A query-bearing DELETE with a percent-encoded path segment (w%201). Built
	// with http.NewRequest so GetBody is set, exactly as the genclient transport
	// produces — the grant editor buffers the body via GetBody.
	req, err := http.NewRequest(http.MethodDelete, "http://x/v0/city/acme/workflow/w%201?scope_kind=rig&confirm=1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(csrfHeaderName, "1")

	c := &Client{grantSource: signingGrantSource(priv, "acme")}
	if err := remoteGrantEditor(c)(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("grant-bound query request must verify: reached=%v code=%d body=%s", reached, rec.Code, rec.Body.String())
	}

	// Replay the grant minted for scope_kind=rig against a scope_kind=city
	// request: the middleware recomputes the digest from the actual query and
	// rejects it — the query is bound end to end.
	reached = false
	req2, err := http.NewRequest(http.MethodDelete, "http://x/v0/city/acme/workflow/w%201?scope_kind=city&confirm=1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set(csrfHeaderName, "1")
	req2.Header.Set(writeAuthHeader, req.Header.Get(writeAuthHeader))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if reached || rec2.Code == http.StatusOK {
		t.Fatalf("a grant minted for scope_kind=rig must not authorize scope_kind=city (code=%d)", rec2.Code)
	}
}
