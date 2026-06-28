package citywriteauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// mintFor signs a grant with priv and assembles the wire token. It mirrors what
// a trusted authority does out-of-band; tests own it so the package stays
// verify-only.
func mintFor(t *testing.T, priv ed25519.PrivateKey, g Grant) string {
	t.Helper()
	payload, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	sig := ed25519.Sign(priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

func newTestKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

// fixture returns a verifier pinned to `now`, plus a matching valid grant and
// the Expect the request side would compute for it.
func fixture(t *testing.T, now time.Time) (*Verifier, ed25519.PrivateKey, Grant, Expect) {
	t.Helper()
	pub, priv := newTestKeypair(t)
	v, err := New(Options{
		Aud:        "gc-city-write",
		Keys:       map[string]ed25519.PublicKey{"k1": pub},
		EpochFloor: 1,
		MaxTTL:     60 * time.Second,
		Skew:       5 * time.Second,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := []byte(`{"name":"worker"}`)
	digest := ReqDigest("POST", "/v0/city/acme/agents", body)
	g := Grant{
		Kid:   "k1",
		Aud:   "gc-city-write",
		City:  "acme",
		Epoch: 7,
		IAT:   now.Unix(),
		Exp:   now.Add(30 * time.Second).Unix(),
		JTI:   "jti-1",
		Req:   digest,
	}
	return v, priv, g, Expect{City: "acme", ReqDigest: digest}
}

func TestVerify_Valid(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	v, priv, g, expect := fixture(t, now)
	got, err := v.Verify(mintFor(t, priv, g), expect)
	if err != nil {
		t.Fatalf("Verify: unexpected error: %v", err)
	}
	if got.City != "acme" || got.JTI != "jti-1" {
		t.Fatalf("Verify returned %+v", got)
	}
}

func TestVerify_Rejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name    string
		mutate  func(g *Grant)            // mutate the (signed) grant claims
		expect  func(e *Expect)           // mutate the request-side expectations
		corrupt func(token string) string // corrupt the assembled token
		wantErr error
	}{
		{
			name:    "unknown kid",
			mutate:  func(g *Grant) { g.Kid = "kX" },
			wantErr: ErrUnknownKey,
		},
		{
			name:    "wrong audience",
			mutate:  func(g *Grant) { g.Aud = "some-other-aud" },
			wantErr: ErrAudience,
		},
		{
			name:    "expired beyond skew",
			mutate:  func(g *Grant) { g.IAT = now.Add(-30 * time.Second).Unix(); g.Exp = now.Add(-10 * time.Second).Unix() },
			wantErr: ErrExpired,
		},
		{
			name:    "not yet valid beyond skew",
			mutate:  func(g *Grant) { g.IAT = now.Add(60 * time.Second).Unix(); g.Exp = now.Add(90 * time.Second).Unix() },
			wantErr: ErrNotYetValid,
		},
		{
			name:    "ttl exceeds max",
			mutate:  func(g *Grant) { g.IAT = now.Unix(); g.Exp = now.Add(120 * time.Second).Unix() },
			wantErr: ErrTTLTooLong,
		},
		{
			name:    "epoch below floor",
			mutate:  func(g *Grant) { g.Epoch = 0 },
			wantErr: ErrEpoch,
		},
		{
			name:    "city mismatch vs request path",
			expect:  func(e *Expect) { e.City = "evil" },
			wantErr: ErrCityMismatch,
		},
		{
			name: "request binding mismatch",
			expect: func(e *Expect) {
				e.ReqDigest = ReqDigest("POST", "/v0/city/acme/agents", []byte(`{"name":"tampered"}`))
			},
			wantErr: ErrReqMismatch,
		},
		{
			name:    "tampered signature",
			corrupt: flipLastByte,
			wantErr: ErrBadSignature,
		},
		{
			name: "swapped payload keeps stale signature",
			corrupt: func(tok string) string {
				// Replace the payload segment with a different city; the old sig
				// no longer matches the new payload bytes.
				parts := strings.SplitN(tok, ".", 2)
				evil, _ := json.Marshal(Grant{Kid: "k1", Aud: "gc-city-write", City: "evil"})
				return base64.RawURLEncoding.EncodeToString(evil) + "." + parts[1]
			},
			wantErr: ErrBadSignature,
		},
		{
			name:    "malformed - not two segments",
			corrupt: func(string) string { return "garbage" },
			wantErr: ErrMalformed,
		},
		{
			name:    "malformed - bad base64",
			corrupt: func(string) string { return "!!!.@@@" },
			wantErr: ErrMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, priv, g, expect := fixture(t, now)
			if tt.mutate != nil {
				tt.mutate(&g)
			}
			if tt.expect != nil {
				tt.expect(&expect)
			}
			token := mintFor(t, priv, g)
			if tt.corrupt != nil {
				token = tt.corrupt(token)
			}
			_, err := v.Verify(token, expect)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify: got err %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerify_ReplayIsSingleUse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	v, priv, g, expect := fixture(t, now)
	token := mintFor(t, priv, g)

	if _, err := v.Verify(token, expect); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if _, err := v.Verify(token, expect); !errors.Is(err, ErrReplay) {
		t.Fatalf("second Verify: got %v, want ErrReplay", err)
	}
}

// A failed verification must NOT burn the jti — otherwise an attacker who
// observes a victim's token could invalidate it by replaying with a bad city.
func TestVerify_FailedCheckDoesNotConsumeJTI(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	v, priv, g, expect := fixture(t, now)
	token := mintFor(t, priv, g)

	bad := expect
	bad.City = "evil"
	if _, err := v.Verify(token, bad); !errors.Is(err, ErrCityMismatch) {
		t.Fatalf("expected ErrCityMismatch, got %v", err)
	}
	// The legitimate request must still succeed.
	if _, err := v.Verify(token, expect); err != nil {
		t.Fatalf("legit Verify after failed attempt: %v", err)
	}
}

// A grant is accepted until exp+Skew, so its jti must be retained at least that
// long. Otherwise a sweep that fires during the skew window (an attacker can
// induce one by filling the jti map to threshold) evicts the consumed jti and
// the same grant verifies a second time. Regression for the skew-window replay.
func TestVerify_ReplaySurvivesSweepInSkewWindow(t *testing.T) {
	realNow := time.Now()
	skew := time.Hour // wide window so the guard's wall-clock sweep is deterministic

	guard := NewMemoryReplayGuard()
	guard.sweepThreshold = 1 // any second Use triggers a sweep

	pub, priv := newTestKeypair(t)
	v, err := New(Options{
		Aud:        "gc-city-write",
		Keys:       map[string]ed25519.PublicKey{"k1": pub},
		EpochFloor: 1,
		MaxTTL:     time.Minute,
		Skew:       skew,
		Now:        func() time.Time { return realNow }, // sits inside (exp, exp+skew]
		Replay:     guard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Grant expired a minute ago in bare terms, but is still within the skew
	// window, so Verify accepts it.
	exp := realNow.Add(-1 * time.Minute)
	iat := exp.Add(-20 * time.Second)
	digest := ReqDigest("POST", "/v0/city/acme/agents", []byte(`{}`))
	g := Grant{
		Kid: "k1", Aud: "gc-city-write", City: "acme", Epoch: 7,
		IAT: iat.Unix(), Exp: exp.Unix(), JTI: "jti-replay", Req: digest,
	}
	expect := Expect{City: "acme", ReqDigest: digest}
	token := mintFor(t, priv, g)

	if _, err := v.Verify(token, expect); err != nil {
		t.Fatalf("first Verify (within skew): %v", err)
	}

	// Simulate concurrent write load: another grant's Use fires the sweep, which
	// must NOT evict the still-acceptable jti.
	_ = guard.Use("other-jti", realNow.Add(time.Hour))

	if _, err := v.Verify(token, expect); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay within skew window: got %v, want ErrReplay", err)
	}
}

func TestNew_RejectsBadOptions(t *testing.T) {
	pub, _ := newTestKeypair(t)
	cases := map[string]Options{
		"no aud":  {Keys: map[string]ed25519.PublicKey{"k1": pub}, MaxTTL: time.Minute},
		"no keys": {Aud: "gc-city-write", MaxTTL: time.Minute},
		"no ttl":  {Aud: "gc-city-write", Keys: map[string]ed25519.PublicKey{"k1": pub}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatalf("New(%s): expected error, got nil", name)
			}
		})
	}
}

func TestReqDigest(t *testing.T) {
	body := []byte(`{"a":1}`)
	base := ReqDigest("POST", "/v0/city/acme/agents", body)

	if base == "" {
		t.Fatal("ReqDigest returned empty")
	}
	if got := ReqDigest("POST", "/v0/city/acme/agents", body); got != base {
		t.Fatalf("ReqDigest not deterministic: %q vs %q", got, base)
	}
	// Sensitivity to each component.
	if ReqDigest("PUT", "/v0/city/acme/agents", body) == base {
		t.Fatal("ReqDigest insensitive to method")
	}
	if ReqDigest("POST", "/v0/city/acme/providers", body) == base {
		t.Fatal("ReqDigest insensitive to path")
	}
	if ReqDigest("POST", "/v0/city/acme/agents", []byte(`{"a":2}`)) == base {
		t.Fatal("ReqDigest insensitive to body")
	}
	if ReqDigest("POST", "/v0/city/acme/agents", nil) == base {
		t.Fatal("ReqDigest insensitive to empty vs non-empty body")
	}
}

func flipLastByte(tok string) string {
	parts := strings.SplitN(tok, ".", 2)
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(sig) == 0 {
		return tok
	}
	// Flip a bit in the decoded signature and re-encode canonically, so the
	// token still decodes and we exercise the signature check, not the decoder.
	sig[0] ^= 0x01
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(sig)
}
