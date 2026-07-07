package webhookverify

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	testIssuer   = "https://issuer.gascity.example"
	testAudience = "supervisor-webhook"
	testKID      = "key-1"
)

var testNow = time.Unix(1_700_000_000, 0)

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}

func jwksJSON(t *testing.T, pub *rsa.PublicKey, kid string) []byte {
	t.Helper()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: kid, Algorithm: "RS256", Use: "sig"}}}
	b, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return b
}

func signRS256(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func goodClaims() jwt.RegisteredClaims {
	return jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   "user-42",
		Audience:  jwt.ClaimStrings{testAudience},
		ID:        "jti-abc",
		ExpiresAt: jwt.NewNumericDate(testNow.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(testNow.Add(-time.Minute)),
	}
}

// jwksFixture spins a TLS JWKS server and returns a constructed verifier plus a
// counter of how many times the JWKS was fetched.
func jwksFixture(t *testing.T, priv *rsa.PrivateKey) (Verifier, *int64) {
	t.Helper()
	var fetches int64
	body := jwksJSON(t, &priv.PublicKey, testKID)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&fetches, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	policy := JWTVerifierPolicy{Issuer: testIssuer, Audience: testAudience, JWKSURL: srv.URL}
	v, err := New("jwt-jwks", config.WebhookVerify{}, Options{
		JWTPolicy:  &policy,
		HTTPClient: srv.Client(),
		Now:        func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v, &fetches
}

func bearer(token string) http.Header { return hdr("Authorization", "Bearer "+token) }

func TestJWTJWKS_ValidToken(t *testing.T) {
	priv := newRSAKey(t)
	v, fetches := jwksFixture(t, priv)
	token := signRS256(t, priv, testKID, goodClaims())

	res, err := v.Verify(context.Background(), VerifyRequest{Header: bearer(token)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK, reason %q", res.Reason)
	}
	if res.Identity != "user-42" {
		t.Errorf("Identity = %q, want user-42", res.Identity)
	}
	if res.DedupID != "jti-abc" {
		t.Errorf("DedupID = %q, want jti-abc", res.DedupID)
	}

	// Second verify should hit the cache, not re-fetch.
	if _, err := v.Verify(context.Background(), VerifyRequest{Header: bearer(token)}); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if got := atomic.LoadInt64(fetches); got != 1 {
		t.Errorf("JWKS fetched %d times, want 1 (cached)", got)
	}
}

func TestJWTJWKS_BadIssuerRejected(t *testing.T) {
	priv := newRSAKey(t)
	v, _ := jwksFixture(t, priv)
	claims := goodClaims()
	claims.Issuer = "https://attacker.example"
	res, err := v.Verify(context.Background(), VerifyRequest{Header: bearer(signRS256(t, priv, testKID, claims))})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("token with wrong issuer must be rejected")
	}
}

func TestJWTJWKS_BadAudienceRejected(t *testing.T) {
	priv := newRSAKey(t)
	v, _ := jwksFixture(t, priv)
	claims := goodClaims()
	claims.Audience = jwt.ClaimStrings{"some-other-service"}
	res, _ := v.Verify(context.Background(), VerifyRequest{Header: bearer(signRS256(t, priv, testKID, claims))})
	if res.OK {
		t.Fatal("token with wrong audience must be rejected")
	}
}

func TestJWTJWKS_ExpiredRejected(t *testing.T) {
	priv := newRSAKey(t)
	v, _ := jwksFixture(t, priv)
	claims := goodClaims()
	claims.ExpiresAt = jwt.NewNumericDate(testNow.Add(-time.Hour))
	res, _ := v.Verify(context.Background(), VerifyRequest{Header: bearer(signRS256(t, priv, testKID, claims))})
	if res.OK {
		t.Fatal("expired token must be rejected")
	}
}

func TestJWTJWKS_WrongSigningKeyRejected(t *testing.T) {
	priv := newRSAKey(t)
	other := newRSAKey(t)
	v, _ := jwksFixture(t, priv)
	// Signed by a key not in the JWKS, but claiming the published kid.
	res, _ := v.Verify(context.Background(), VerifyRequest{Header: bearer(signRS256(t, other, testKID, goodClaims()))})
	if res.OK {
		t.Fatal("token signed by an unpublished key must be rejected")
	}
}

func TestJWTJWKS_AlgConfusionRejected(t *testing.T) {
	priv := newRSAKey(t)
	v, _ := jwksFixture(t, priv)
	// Attacker forges an HS256 token; the verifier pins asymmetric algs only.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, goodClaims())
	tok.Header["kid"] = testKID
	forged, err := tok.SignedString([]byte("public-key-as-hmac-secret"))
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	res, err := v.Verify(context.Background(), VerifyRequest{Header: bearer(forged)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("HS256 (alg-confusion) token must be rejected")
	}
}

func TestJWTJWKS_UnknownKIDRejected(t *testing.T) {
	priv := newRSAKey(t)
	v, _ := jwksFixture(t, priv)
	res, err := v.Verify(context.Background(), VerifyRequest{Header: bearer(signRS256(t, priv, "no-such-kid", goodClaims()))})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("token referencing an unknown kid must be rejected")
	}
}

func TestJWTJWKS_MissingTokenHeader(t *testing.T) {
	priv := newRSAKey(t)
	v, _ := jwksFixture(t, priv)
	res, err := v.Verify(context.Background(), VerifyRequest{Header: hdr()})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("missing token header must not verify")
	}
}

func TestJWTJWKS_UnavailableJWKSIsOperationalError(t *testing.T) {
	priv := newRSAKey(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	policy := JWTVerifierPolicy{Issuer: testIssuer, Audience: testAudience, JWKSURL: srv.URL}
	v, err := New("jwt-jwks", config.WebhookVerify{}, Options{JWTPolicy: &policy, HTTPClient: srv.Client(), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = v.Verify(context.Background(), VerifyRequest{Header: bearer(signRS256(t, priv, testKID, goodClaims()))})
	if err == nil {
		t.Fatal("an unreachable JWKS must surface as an operational error, not OK=false")
	}
}

func TestJWTJWKS_RequiresOperatorPolicy(t *testing.T) {
	// R1: no policy means a pack cannot supply iss/aud/jwks — construction fails.
	if _, err := New("jwt-jwks", config.WebhookVerify{Issuer: "pack-supplied", JWKSURL: "https://pack/jwks", Audience: "pack"}, Options{}); err == nil {
		t.Fatal("jwt-jwks without an operator JWTVerifierPolicy must fail construction")
	}
}
