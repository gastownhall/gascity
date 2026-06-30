package citywriteauth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	digest := ReqDigest("POST", "/v0/city/acme/agents", "", body)
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
			name:    "non-positive validity window",
			mutate:  func(g *Grant) { g.IAT = now.Unix(); g.Exp = now.Unix() },
			wantErr: ErrBadWindow,
		},
		{
			name:    "epoch below floor",
			mutate:  func(g *Grant) { g.Epoch = 0 },
			wantErr: ErrEpoch,
		},
		{
			name:    "empty city claim",
			mutate:  func(g *Grant) { g.City = "" },
			wantErr: ErrMissingClaim,
		},
		{
			name:    "empty request-binding claim",
			mutate:  func(g *Grant) { g.Req = "" },
			wantErr: ErrMissingClaim,
		},
		{
			name:    "empty jti claim",
			mutate:  func(g *Grant) { g.JTI = "" },
			wantErr: ErrMissingClaim,
		},
		{
			name:    "zero-value request expectation",
			expect:  func(e *Expect) { *e = Expect{} },
			wantErr: ErrMissingExpectation,
		},
		{
			name:    "city mismatch vs request path",
			expect:  func(e *Expect) { e.City = "evil" },
			wantErr: ErrCityMismatch,
		},
		{
			name: "request binding mismatch",
			expect: func(e *Expect) {
				e.ReqDigest = ReqDigest("POST", "/v0/city/acme/agents", "", []byte(`{"name":"tampered"}`))
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
	digest := ReqDigest("POST", "/v0/city/acme/agents", "", []byte(`{}`))
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

// replayErrGuard is a ReplayGuard whose Use always returns a fixed error. It
// models a custom durable/shared backend so the tests can drive how Verify
// classifies guard failures.
type replayErrGuard struct{ err error }

func (g replayErrGuard) Use(string, time.Time) error { return g.err }

// Verify must classify ReplayGuard failures by the advertised errors.Is
// contract: a duplicate jti (an error that wraps ErrReplay) stays ErrReplay,
// while any other backend failure surfaces as ErrReplayUnavailable and never as
// a false duplicate. Otherwise a shared guard that is merely unavailable would
// be reported to callers as a replayed token.
func TestVerify_ReplayGuardErrorClassification(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"name":"worker"}`)
	digest := ReqDigest("POST", "/v0/city/acme/agents", "", body)
	newGrant := func() Grant {
		return Grant{
			Kid: "k1", Aud: "gc-city-write", City: "acme", Epoch: 7,
			IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(), JTI: "jti-1", Req: digest,
		}
	}
	expect := Expect{City: "acme", ReqDigest: digest}
	newVerifier := func(t *testing.T, guard ReplayGuard) (*Verifier, ed25519.PrivateKey) {
		t.Helper()
		pub, priv := newTestKeypair(t)
		v, err := New(Options{
			Aud:        "gc-city-write",
			Keys:       map[string]ed25519.PublicKey{"k1": pub},
			EpochFloor: 1,
			MaxTTL:     60 * time.Second,
			Skew:       5 * time.Second,
			Now:        func() time.Time { return now },
			Replay:     guard,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return v, priv
	}

	t.Run("wrapped replay stays replay", func(t *testing.T) {
		v, priv := newVerifier(t, replayErrGuard{err: fmt.Errorf("shared-store: %w", ErrReplay)})
		_, err := v.Verify(mintFor(t, priv, newGrant()), expect)
		if !errors.Is(err, ErrReplay) {
			t.Fatalf("wrapped ErrReplay from guard: got %v, want ErrReplay", err)
		}
	})

	t.Run("non-replay backend error is unavailable not replay", func(t *testing.T) {
		backendErr := errors.New("datastore unavailable")
		v, priv := newVerifier(t, replayErrGuard{err: backendErr})
		_, err := v.Verify(mintFor(t, priv, newGrant()), expect)
		if !errors.Is(err, ErrReplayUnavailable) {
			t.Fatalf("guard backend failure: got %v, want ErrReplayUnavailable", err)
		}
		if errors.Is(err, ErrReplay) {
			t.Fatalf("guard unavailability must not satisfy errors.Is(_, ErrReplay): %v", err)
		}
		if !errors.Is(err, backendErr) {
			t.Fatalf("guard backend error must be wrapped for diagnosis: %v", err)
		}
	})
}

// The single-use property must hold under concurrency: many goroutines
// presenting the same valid token must yield exactly one success and the rest
// ErrReplay. Without a concurrent Verify in any test, go test -race never
// observes the contended check-then-insert path the guard's mutex protects, so
// a refactor that moved the presence check outside the lock would slip through.
func TestVerify_ConcurrentSingleUse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	v, priv, g, expect := fixture(t, now)
	token := mintFor(t, priv, g)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make(chan error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // release all goroutines together to maximize contention
			_, err := v.Verify(token, expect)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var success, replay int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrReplay):
			replay++
		default:
			t.Fatalf("unexpected Verify error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("got %d successful Verify calls, want exactly 1", success)
	}
	if replay != goroutines-1 {
		t.Fatalf("got %d ErrReplay results, want %d", replay, goroutines-1)
	}
}

func TestNew_RejectsBadOptions(t *testing.T) {
	pub, _ := newTestKeypair(t)
	cases := map[string]Options{
		"no aud":           {Keys: map[string]ed25519.PublicKey{"k1": pub}, MaxTTL: time.Minute},
		"no keys":          {Aud: "gc-city-write", MaxTTL: time.Minute},
		"no ttl":           {Aud: "gc-city-write", Keys: map[string]ed25519.PublicKey{"k1": pub}},
		"non-positive ttl": {Aud: "gc-city-write", Keys: map[string]ed25519.PublicKey{"k1": pub}, MaxTTL: -time.Second},
		"empty kid":        {Aud: "gc-city-write", Keys: map[string]ed25519.PublicKey{"": pub}, MaxTTL: time.Minute},
		"wrong key size":   {Aud: "gc-city-write", Keys: map[string]ed25519.PublicKey{"k1": pub[:10]}, MaxTTL: time.Minute},
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
	const path = "/v0/city/acme/agents"
	base := ReqDigest("POST", path, "", body)

	if base == "" {
		t.Fatal("ReqDigest returned empty")
	}
	if got := ReqDigest("POST", path, "", body); got != base {
		t.Fatalf("ReqDigest not deterministic: %q vs %q", got, base)
	}
	// Sensitivity to each component.
	if ReqDigest("PUT", path, "", body) == base {
		t.Fatal("ReqDigest insensitive to method")
	}
	if ReqDigest("POST", "/v0/city/acme/providers", "", body) == base {
		t.Fatal("ReqDigest insensitive to path")
	}
	if ReqDigest("POST", path, "", []byte(`{"a":2}`)) == base {
		t.Fatal("ReqDigest insensitive to body")
	}
	if ReqDigest("POST", path, "", nil) == base {
		t.Fatal("ReqDigest insensitive to empty vs non-empty body")
	}

	// The query-less preimage must stay byte-identical to the original
	// method"\n"path"\n"hex(sha256(body)) contract, so grants minted by the
	// cross-repo crucible (which computes the query-less digest) and the pinned
	// golden vector keep verifying. Pin the exact preimage independently.
	bodyHash := sha256.Sum256(body)
	want := sha256.Sum256([]byte("POST\n" + path + "\n" + hex.EncodeToString(bodyHash[:])))
	if base != hex.EncodeToString(want[:]) {
		t.Fatalf("query-less preimage drifted: got %s want %s", base, hex.EncodeToString(want[:]))
	}

	// A query is bound: the destructive ?delete=true variant must not share the
	// cancel-only (query-less) digest, and scope selectors must change it too.
	if ReqDigest("DELETE", path, "delete=true", body) == base {
		t.Fatal("ReqDigest must bind the query: ?delete=true matched the query-less digest")
	}
	if ReqDigest("DELETE", path, "scope_kind=rig", body) == ReqDigest("DELETE", path, "scope_kind=city", body) {
		t.Fatal("ReqDigest insensitive to query value")
	}
	// Canonicalization: parameter order is irrelevant (handlers read r.URL.Query).
	if ReqDigest("DELETE", path, "a=1&b=2", body) != ReqDigest("DELETE", path, "b=2&a=1", body) {
		t.Fatal("ReqDigest must be order-independent over query parameters")
	}
	// A semantically-empty query collapses back to the query-less digest.
	if ReqDigest("POST", path, "&", body) != base {
		t.Fatal("semantically-empty query must equal the query-less digest")
	}
}

// A caller that mutates its own Options.Keys slice after New must not be able to
// change the verifier's trust root: New must deep-copy each public key.
func TestNew_DeepCopiesKeys(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	pub, priv := newTestKeypair(t)
	callerKey := append(ed25519.PublicKey(nil), pub...) // caller-owned backing array

	v, err := New(Options{
		Aud:        "gc-city-write",
		Keys:       map[string]ed25519.PublicKey{"k1": callerKey},
		EpochFloor: 1,
		MaxTTL:     60 * time.Second,
		Skew:       5 * time.Second,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Scramble the caller's slice after construction. A verifier that aliased it
	// would now trust different key bytes and reject the legitimately signed grant.
	for i := range callerKey {
		callerKey[i] ^= 0xFF
	}

	body := []byte(`{"name":"worker"}`)
	digest := ReqDigest("POST", "/v0/city/acme/agents", "", body)
	g := Grant{
		Kid: "k1", Aud: "gc-city-write", City: "acme", Epoch: 7,
		IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(), JTI: "jti-1", Req: digest,
	}
	expect := Expect{City: "acme", ReqDigest: digest}
	if _, err := v.Verify(mintFor(t, priv, g), expect); err != nil {
		t.Fatalf("Verify after caller mutated its key slice: %v", err)
	}
}

// A grant whose iat is slightly in the future but within Skew must be accepted;
// the not-yet-valid guard only rejects drift beyond the skew tolerance.
func TestVerify_AcceptsFutureIATWithinSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	v, priv, g, expect := fixture(t, now)
	g.IAT = now.Add(3 * time.Second).Unix() // 3s ahead, skew is 5s
	g.Exp = now.Add(33 * time.Second).Unix()
	if _, err := v.Verify(mintFor(t, priv, g), expect); err != nil {
		t.Fatalf("Verify within-skew future iat: unexpected error: %v", err)
	}
}

// The core fail-closed regression: a validly signed grant with empty binding
// claims paired with a zero-value Expect must be rejected, not authorized.
// Both sides being empty would otherwise satisfy the city/req equality checks
// vacuously and consume an empty jti as if it were single-use.
func TestVerify_EmptyClaimsAndZeroExpectRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	v, priv, _, _ := fixture(t, now)
	g := Grant{
		Kid:   "k1",
		Aud:   "gc-city-write",
		Epoch: 7,
		IAT:   now.Unix(),
		Exp:   now.Add(30 * time.Second).Unix(),
		// City, Req, and JTI deliberately left empty.
	}
	if _, err := v.Verify(mintFor(t, priv, g), Expect{}); !errors.Is(err, ErrMissingClaim) {
		t.Fatalf("empty claims + zero expect: got %v, want ErrMissingClaim", err)
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
