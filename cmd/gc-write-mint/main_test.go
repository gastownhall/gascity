package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/clientgrant"
)

// testKey returns a deterministic ed25519 keypair so signatures are reproducible.
func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

// infoFor builds a self-consistent GrantInfo: the req_digest is the real
// ReqDigest over the method/path/query/body, exactly as the client would send it.
func infoFor(method, path, rawQuery string, body []byte) clientgrant.GrantInfo {
	h := sha256.Sum256(body)
	bodyHex := hex.EncodeToString(h[:])
	return clientgrant.GrantInfo{
		Version:        clientgrant.Version,
		Aud:            "gc-city-write",
		City:           "mc",
		Method:         method,
		Path:           path,
		CanonicalQuery: rawQuery,
		BodySHA256:     bodyHex,
		ReqDigest:      citywriteauth.ReqDigest(method, path, rawQuery, body),
	}
}

func baseParams(priv ed25519.PrivateKey) mintParams {
	fixed := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	return mintParams{
		Kid:   "k1",
		Epoch: 7,
		TTL:   time.Minute,
		Aud:   defaultAud,
		Key:   priv,
		Now:   func() time.Time { return fixed },
		Rand:  bytes.NewReader(bytes.Repeat([]byte{0xAB}, 64)),
	}
}

// verifierFor builds a verifier trusting pub, clocked inside the grant window.
func verifierFor(t *testing.T, pub ed25519.PublicKey) *citywriteauth.Verifier {
	t.Helper()
	v, err := citywriteauth.New(citywriteauth.Options{
		Aud:    "gc-city-write",
		Keys:   map[string]ed25519.PublicKey{"k1": pub},
		MaxTTL: 2 * time.Minute,
		Skew:   30 * time.Second,
		Now:    func() time.Time { return time.Date(2026, 7, 7, 12, 0, 30, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New verifier: %v", err)
	}
	return v
}

// The core proof: a minted token verifies against citywriteauth with the exact
// city + digest the server will independently recompute from the wire request.
func TestMint_ProducesVerifiableGrant(t *testing.T) {
	pub, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{"source":"pr-1"}`))

	token, err := mint(baseParams(priv), info)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	g, err := verifierFor(t, pub).Verify(token, citywriteauth.Expect{City: "mc", ReqDigest: info.ReqDigest})
	if err != nil {
		t.Fatalf("minted grant must verify: %v", err)
	}
	if g.Kid != "k1" || g.Epoch != 7 || g.City != "mc" {
		t.Fatalf("grant claims wrong: %+v", g)
	}
	if g.Exp-g.IAT != int64(time.Minute/time.Second) {
		t.Fatalf("ttl not honored: iat=%d exp=%d", g.IAT, g.Exp)
	}
}

// A query-bearing mutation round-trips: the grant binds the query, so the server
// verifying with the same digest accepts it and a query-less digest would not.
func TestMint_QueryBearingRoundTrip(t *testing.T) {
	pub, priv := testKey(t)
	info := infoFor("DELETE", "/v0/city/mc/workflow/wf-1", "scope=my%20rig&delete=true", []byte(`{}`))

	token, err := mint(baseParams(priv), info)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	v := verifierFor(t, pub)
	if _, err := v.Verify(token, citywriteauth.Expect{City: "mc", ReqDigest: info.ReqDigest}); err != nil {
		t.Fatalf("query-bearing grant must verify: %v", err)
	}
	// The query-less digest for the same method/path/body must NOT match — proving
	// the grant is query-bound end to end.
	queryless := citywriteauth.ReqDigest("DELETE", "/v0/city/mc/workflow/wf-1", "", []byte(`{}`))
	if queryless == info.ReqDigest {
		t.Fatal("test setup wrong: query and query-less digests collided")
	}
}

func TestMint_RefusesDigestMismatch(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{"source":"pr-1"}`))
	info.ReqDigest = strings.Repeat("0", 64) // client lied about the digest

	if _, err := mint(baseParams(priv), info); err == nil || !strings.Contains(err.Error(), "req_digest") {
		t.Fatalf("digest mismatch must be refused, got %v", err)
	}
}

func TestMint_RefusesBodyHashTamper(t *testing.T) {
	_, priv := testKey(t)
	// A consistent info, then swap the body hash so the recompute diverges from
	// the claimed digest — the minter must not sign a repurposed binding.
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{"source":"pr-1"}`))
	other := sha256.Sum256([]byte(`{"source":"evil"}`))
	info.BodySHA256 = hex.EncodeToString(other[:])

	if _, err := mint(baseParams(priv), info); err == nil {
		t.Fatal("body-hash tamper must be refused")
	}
}

func TestMint_RefusesAudienceMismatch(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	info.Aud = "some-other-aud"

	if _, err := mint(baseParams(priv), info); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("audience mismatch must be refused, got %v", err)
	}
}

func TestMint_RefusesCityMismatchWhenPinned(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	p := baseParams(priv)
	p.City = "other-city" // pinned; info says "mc"

	if _, err := mint(p, info); err == nil || !strings.Contains(err.Error(), "city") {
		t.Fatalf("pinned-city mismatch must be refused, got %v", err)
	}
}

func TestMint_AcceptsMatchingPinnedCity(t *testing.T) {
	pub, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	p := baseParams(priv)
	p.City = "mc"

	token, err := mint(p, info)
	if err != nil {
		t.Fatalf("matching pinned city must mint: %v", err)
	}
	if _, err := verifierFor(t, pub).Verify(token, citywriteauth.Expect{City: "mc", ReqDigest: info.ReqDigest}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestMint_RefusesBadTTL(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	// Sub-second TTLs are refused too: whole-second iat/exp truncation could
	// collapse them to a non-positive window the server rejects (ErrBadWindow).
	for _, ttl := range []time.Duration{0, -time.Second, time.Millisecond, 500 * time.Millisecond, 999 * time.Millisecond, 3 * time.Minute} {
		p := baseParams(priv)
		p.TTL = ttl
		if _, err := mint(p, info); err == nil || !strings.Contains(err.Error(), "ttl") {
			t.Fatalf("ttl=%s must be refused, got %v", ttl, err)
		}
	}
}

// A minimum (1s) TTL must never truncate to a non-positive window, regardless of
// the sub-second clock offset at mint time — otherwise a freshly minted grant
// would intermittently 403 with ErrBadWindow.
func TestMint_MinTTLSurvivesSubSecondClock(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	for _, ns := range []int{0, 1, 1_000_000, 500_000_000, 999_999_999} {
		p := baseParams(priv)
		p.TTL = minTTL
		fixed := time.Date(2026, 7, 7, 12, 0, 0, ns, time.UTC)
		p.Now = func() time.Time { return fixed }
		g := decodeGrant(t, mustMint(t, p, info))
		if g.Exp <= g.IAT {
			t.Fatalf("ns=%d: exp(%d) <= iat(%d) — truncation produced a non-positive window", ns, g.Exp, g.IAT)
		}
	}
}

func TestMint_RefusesVersionMismatch(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	info.Version = "gascity.dev/city-write-grant/v99"

	if _, err := mint(baseParams(priv), info); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("version mismatch must be refused, got %v", err)
	}
}

func TestMint_RequiresKid(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	p := baseParams(priv)
	p.Kid = ""
	if _, err := mint(p, info); err == nil || !strings.Contains(err.Error(), "kid") {
		t.Fatalf("empty kid must be refused, got %v", err)
	}
}

// jti must be fresh on every mint (single-use), so two mints of the identical
// request produce distinct grants — the server's replay guard depends on it.
func TestMint_FreshJTIEachCall(t *testing.T) {
	_, priv := testKey(t)
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))

	p1 := baseParams(priv)
	p1.Rand = bytes.NewReader(bytes.Repeat([]byte{0x01}, 64))
	p2 := baseParams(priv)
	p2.Rand = bytes.NewReader(bytes.Repeat([]byte{0x02}, 64))

	jti1 := decodeJTI(t, mustMint(t, p1, info))
	jti2 := decodeJTI(t, mustMint(t, p2, info))
	if jti1 == jti2 {
		t.Fatalf("jti must be fresh each mint, got %q twice", jti1)
	}
	if jti1 == "" {
		t.Fatal("jti must be non-empty")
	}
}

func TestLoadGrantInfo(t *testing.T) {
	info := infoFor("POST", "/v0/city/mc/sling", "", []byte(`{}`))
	raw, _ := json.Marshal(info)
	env := map[string]string{clientgrant.GrantInfoEnv: string(raw)}
	getenv := func(k string) string { return env[k] }

	got, err := loadGrantInfo(getenv)
	if err != nil {
		t.Fatalf("loadGrantInfo: %v", err)
	}
	if got.ReqDigest != info.ReqDigest || got.City != "mc" {
		t.Fatalf("round-trip lost fields: %+v", got)
	}

	if _, err := loadGrantInfo(func(string) string { return "" }); err == nil {
		t.Fatal("missing env must error")
	}
	if _, err := loadGrantInfo(func(string) string { return "not json" }); err == nil {
		t.Fatal("bad json must error")
	}
}

func TestParseEd25519PrivateKey(t *testing.T) {
	_, priv := testKey(t)

	// Raw seed (32 bytes).
	seed := priv.Seed()
	if got, err := parseEd25519PrivateKey(seed); err != nil || !got.Equal(priv) {
		t.Fatalf("raw seed: %v (equal=%v)", err, got.Equal(priv))
	}
	// Hex-encoded seed (a trailing newline as a file would have).
	hexSeed := []byte(hex.EncodeToString(seed) + "\n")
	if got, err := parseEd25519PrivateKey(hexSeed); err != nil || !got.Equal(priv) {
		t.Fatalf("hex seed: %v", err)
	}
	// Full 64-byte private key.
	if got, err := parseEd25519PrivateKey(priv); err != nil || !got.Equal(priv) {
		t.Fatalf("raw 64-byte key: %v", err)
	}
	// PEM PKCS#8 (the openssl genpkey form).
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	if got, err := parseEd25519PrivateKey(pemBytes); err != nil || !got.Equal(priv) {
		t.Fatalf("PEM PKCS#8: %v", err)
	}
	// Garbage.
	if _, err := parseEd25519PrivateKey([]byte("nope")); err == nil {
		t.Fatal("garbage key must error")
	}
}

func mustMint(t *testing.T, p mintParams, info clientgrant.GrantInfo) string {
	t.Helper()
	tok, err := mint(p, info)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

func decodeJTI(t *testing.T, token string) string {
	t.Helper()
	return decodeGrant(t, token).JTI
}

// decodeGrant recovers the signed Grant claims from a token's payload segment.
func decodeGrant(t *testing.T, token string) citywriteauth.Grant {
	t.Helper()
	payload, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var g citywriteauth.Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("unmarshal grant: %v", err)
	}
	return g
}
