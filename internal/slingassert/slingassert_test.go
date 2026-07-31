package slingassert

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

const testKid = "bts-mint-1"

var testNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

func newTestVerifier(t *testing.T, pub ed25519.PublicKey, opts ...func(*Options)) *Verifier {
	t.Helper()
	o := Options{
		Keys: map[string]ed25519.PublicKey{testKid: pub},
		Commands: map[string]Command{
			CommandSlingCityWork: {PolicyID: PolicySlingCityWork, OperationID: CommandSlingCityWork},
		},
		MaxTTL: 2 * time.Minute,
		Skew:   30 * time.Second,
		Now:    func() time.Time { return testNow },
	}
	for _, f := range opts {
		f(&o)
	}
	v, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// validAssertion is the one shape that must verify. Every rejection case in
// this file is this assertion with exactly one thing changed, so a test that
// starts passing for the wrong reason shows up as the happy case breaking too.
func validAssertion() Assertion {
	return Assertion{
		Kid:         testKid,
		Aud:         Audience,
		Nonce:       "nonce-0001",
		IAT:         testNow.Unix(),
		Exp:         testNow.Add(time.Minute).Unix(),
		Workload:    "spiffe://gasworks/ns/bts/sa/city-sling-gateway",
		Method:      "POST",
		Command:     CommandSlingCityWork,
		BodyHash:    BodyHash([]byte(`{"target":"ada"}`)),
		PolicyID:    PolicySlingCityWork,
		OperationID: CommandSlingCityWork,
		Org:         "org-acme",
		Workspace:   "ws-main",
		Principal:   "user-42",
		Source:      "web",
		City:        "downtown",
		Project:     "rig-alpha",
		Issue:       "bd-101",
	}
}

func validExpect(a Assertion) Expect {
	return Expect{
		Workload: a.Workload,
		Method:   a.Method,
		Command:  a.Command,
		BodyHash: a.BodyHash,
		City:     a.City,
	}
}

func mint(t *testing.T, priv ed25519.PrivateKey, a Assertion) string {
	t.Helper()
	payload, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sig := ed25519.Sign(priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func TestVerifyHappyPath(t *testing.T) {
	pub, priv := testKeys(t)
	v := newTestVerifier(t, pub)
	a := validAssertion()

	got, err := v.Verify(mint(t, priv, a), validExpect(a))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Org != "org-acme" || got.Workspace != "ws-main" || got.City != "downtown" || got.Issue != "bd-101" {
		t.Fatalf("verified assertion lost its tenant: %+v", got)
	}
}

// TestVerifyRejectionMatrix is the AC1 / G-003 exact rejection set. Each case
// mutates exactly one field of the happy assertion or its expectation.
func TestVerifyRejectionMatrix(t *testing.T) {
	pub, priv := testKeys(t)

	cases := []struct {
		name    string
		mutate  func(*Assertion)
		expect  func(*Expect)
		wantErr error
	}{
		{
			name:    "unknown workload: assertion minted for another mTLS identity",
			expect:  func(e *Expect) { e.Workload = "spiffe://gasworks/ns/other/sa/broker" },
			wantErr: ErrWorkloadMismatch,
		},
		{
			name:    "unknown minting key",
			mutate:  func(a *Assertion) { a.Kid = "rotated-out" },
			wantErr: ErrUnknownKey,
		},
		{
			name:    "wrong audience: a city-write grant replayed at the sling boundary",
			mutate:  func(a *Assertion) { a.Aud = citywriteauth.AudienceCityWrite },
			wantErr: ErrAudience,
		},
		{
			name: "expired",
			mutate: func(a *Assertion) {
				a.IAT, a.Exp = testNow.Add(-10*time.Minute).Unix(), testNow.Add(-9*time.Minute).Unix()
			},
			wantErr: ErrExpired,
		},
		{
			name: "not yet valid",
			mutate: func(a *Assertion) {
				a.IAT, a.Exp = testNow.Add(9*time.Minute).Unix(), testNow.Add(10*time.Minute).Unix()
			},
			wantErr: ErrNotYetValid,
		},
		{
			name:    "ttl beyond the verifier max",
			mutate:  func(a *Assertion) { a.Exp = testNow.Add(time.Hour).Unix() },
			wantErr: ErrTTLTooLong,
		},
		{
			name:    "inverted validity window",
			mutate:  func(a *Assertion) { a.Exp = a.IAT },
			wantErr: ErrBadWindow,
		},
		{
			name:    "method mutation",
			mutate:  func(a *Assertion) { a.Method = "DELETE" },
			wantErr: ErrMethodMismatch,
		},
		{
			name:    "body mutation after minting",
			expect:  func(e *Expect) { e.BodyHash = BodyHash([]byte(`{"target":"attacker"}`)) },
			wantErr: ErrBodyMismatch,
		},
		{
			name:    "city disagreement: another tenant's city on the path",
			expect:  func(e *Expect) { e.City = "uptown" },
			wantErr: ErrCityMismatch,
		},
		{
			name:    "unregistered command: the broker_bypass fallback",
			mutate:  func(a *Assertion) { a.Command = "brokerDispatch" },
			expect:  func(e *Expect) { e.Command = "brokerDispatch" },
			wantErr: ErrUnknownCommand,
		},
		{
			name:    "registered command carrying another policy",
			mutate:  func(a *Assertion) { a.PolicyID = "beads:city.admin" },
			wantErr: ErrPolicyMismatch,
		},
		{
			name:    "registered command carrying another operation id",
			mutate:  func(a *Assertion) { a.OperationID = "deleteCity" },
			wantErr: ErrPolicyMismatch,
		},
		{name: "missing org", mutate: func(a *Assertion) { a.Org = "" }, wantErr: ErrMissingClaim},
		{name: "missing workspace", mutate: func(a *Assertion) { a.Workspace = "" }, wantErr: ErrMissingClaim},
		{name: "missing principal", mutate: func(a *Assertion) { a.Principal = "" }, wantErr: ErrMissingClaim},
		{name: "missing source", mutate: func(a *Assertion) { a.Source = "" }, wantErr: ErrMissingClaim},
		{name: "missing project", mutate: func(a *Assertion) { a.Project = "" }, wantErr: ErrMissingClaim},
		{name: "missing issue", mutate: func(a *Assertion) { a.Issue = "" }, wantErr: ErrMissingClaim},
		{name: "missing nonce", mutate: func(a *Assertion) { a.Nonce = "" }, wantErr: ErrMissingClaim},
		{name: "missing times", mutate: func(a *Assertion) { a.IAT, a.Exp = 0, 0 }, wantErr: ErrMissingClaim},
		{
			name:    "empty expectation cannot wildcard a binding",
			expect:  func(e *Expect) { e.Workload = "" },
			wantErr: ErrMissingExpectation,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newTestVerifier(t, pub)
			a := validAssertion()
			if tc.mutate != nil {
				tc.mutate(&a)
			}
			e := validExpect(validAssertion())
			if tc.expect != nil {
				tc.expect(&e)
			}
			got, err := v.Verify(mint(t, priv, a), e)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, tc.wantErr)
			}
			if got != nil {
				t.Fatalf("rejected assertion must not be returned, got %+v", got)
			}
		})
	}
}

func TestVerifyRejectsForgedSignature(t *testing.T) {
	pub, _ := testKeys(t)
	_, attacker := testKeys(t)
	v := newTestVerifier(t, pub)
	a := validAssertion()

	if _, err := v.Verify(mint(t, attacker, a), validExpect(a)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsTamperedClaims(t *testing.T) {
	pub, priv := testKeys(t)
	v := newTestVerifier(t, pub)
	a := validAssertion()
	token := mint(t, priv, a)

	// Re-encode the payload with another org while keeping the original
	// signature: the classic claim-swap.
	tampered := a
	tampered.Org = "org-victim"
	payload, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sig := token[len(token)-86:] // signature segment is fixed-width base64url
	forged := base64.RawURLEncoding.EncodeToString(payload) + "." + sig

	if _, err := v.Verify(forged, validExpect(a)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify error = %v, want ErrBadSignature", err)
	}
}

func TestVerifyMalformedTokens(t *testing.T) {
	pub, _ := testKeys(t)
	v := newTestVerifier(t, pub)
	a := validAssertion()

	for _, token := range []string{"", ".", "onlyonesegment", "not-base64!.also-not", "e30.", ".c2ln"} {
		if _, err := v.Verify(token, validExpect(a)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Verify(%q) error = %v, want ErrMalformed", token, err)
		}
	}
}

// TestVerifyReplayReturnsAuthenticatedAssertion covers AC3's duplicate internal
// delivery: the second use is refused as a replay, but the assertion comes back
// authenticated so the caller can answer from its result store.
func TestVerifyReplayReturnsAuthenticatedAssertion(t *testing.T) {
	pub, priv := testKeys(t)
	v := newTestVerifier(t, pub)
	a := validAssertion()
	token := mint(t, priv, a)
	e := validExpect(a)

	if _, err := v.Verify(token, e); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	got, err := v.Verify(token, e)
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("second Verify error = %v, want ErrReplay", err)
	}
	if got == nil || got.Nonce != a.Nonce {
		t.Fatalf("replay must return the authenticated assertion, got %+v", got)
	}
}

// TestVerifyFailedCheckDoesNotBurnNonce proves the ordering claim: a rejection
// leaves the nonce unconsumed so the legitimate retry still works.
func TestVerifyFailedCheckDoesNotBurnNonce(t *testing.T) {
	pub, priv := testKeys(t)
	v := newTestVerifier(t, pub)
	a := validAssertion()
	token := mint(t, priv, a)

	bad := validExpect(a)
	bad.BodyHash = BodyHash([]byte("tampered"))
	if _, err := v.Verify(token, bad); !errors.Is(err, ErrBodyMismatch) {
		t.Fatalf("Verify error = %v, want ErrBodyMismatch", err)
	}
	if _, err := v.Verify(token, validExpect(a)); err != nil {
		t.Fatalf("legitimate retry after a rejection: %v", err)
	}
}

type brokenGuard struct{}

func (brokenGuard) Use(string, time.Time) error { return errors.New("store unreachable") }

func TestVerifyFailsClosedOnReplayGuardOutage(t *testing.T) {
	pub, priv := testKeys(t)
	v := newTestVerifier(t, pub, func(o *Options) { o.Replay = brokenGuard{} })
	a := validAssertion()

	got, err := v.Verify(mint(t, priv, a), validExpect(a))
	if !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("Verify error = %v, want ErrReplayUnavailable", err)
	}
	if errors.Is(err, ErrReplay) {
		t.Fatal("guard outage must not be reported as a replay")
	}
	if got != nil {
		t.Fatalf("guard outage must not return an assertion, got %+v", got)
	}
}

// TestRollbackWithdrawsOnlySling is the AC3 runtime-registry drill: an empty
// sling registration cannot be constructed, and a registry holding some other
// command refuses sling without opening any alternate path.
func TestRollbackWithdrawsOnlySling(t *testing.T) {
	pub, priv := testKeys(t)
	v := newTestVerifier(t, pub, func(o *Options) {
		o.Commands = map[string]Command{"someOtherCommand": {PolicyID: "p", OperationID: "o"}}
	})
	if v.Registered(CommandSlingCityWork) {
		t.Fatal("sling must not be registered after rollback")
	}
	a := validAssertion()
	if _, err := v.Verify(mint(t, priv, a), validExpect(a)); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Verify error = %v, want ErrUnknownCommand", err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	pub, _ := testKeys(t)
	keys := map[string]ed25519.PublicKey{testKid: pub}
	cmds := map[string]Command{CommandSlingCityWork: {PolicyID: PolicySlingCityWork, OperationID: CommandSlingCityWork}}

	cases := []struct {
		name string
		opts Options
	}{
		{"no keys", Options{Commands: cmds, MaxTTL: time.Minute}},
		{"no commands", Options{Keys: keys, MaxTTL: time.Minute}},
		{"no ttl", Options{Keys: keys, Commands: cmds}},
		{"empty kid", Options{Keys: map[string]ed25519.PublicKey{"": pub}, Commands: cmds, MaxTTL: time.Minute}},
		{"short key", Options{Keys: map[string]ed25519.PublicKey{testKid: {1, 2, 3}}, Commands: cmds, MaxTTL: time.Minute}},
		{"command without policy", Options{Keys: keys, Commands: map[string]Command{"c": {OperationID: "o"}}, MaxTTL: time.Minute}},
		{"command without operation", Options{Keys: keys, Commands: map[string]Command{"c": {PolicyID: "p"}}, MaxTTL: time.Minute}},
		{"empty command name", Options{Keys: keys, Commands: map[string]Command{"": {PolicyID: "p", OperationID: "o"}}, MaxTTL: time.Minute}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatal("New should have refused the options")
			}
		})
	}
}

// TestNewClonesKeys guards the trust root against post-construction mutation of
// the caller's key slice.
func TestNewClonesKeys(t *testing.T) {
	pub, priv := testKeys(t)
	keys := map[string]ed25519.PublicKey{testKid: pub}
	v, err := New(Options{
		Keys:     keys,
		Commands: map[string]Command{CommandSlingCityWork: {PolicyID: PolicySlingCityWork, OperationID: CommandSlingCityWork}},
		MaxTTL:   2 * time.Minute,
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range keys[testKid] {
		keys[testKid][i] = 0
	}
	a := validAssertion()
	if _, err := v.Verify(mint(t, priv, a), validExpect(a)); err != nil {
		t.Fatalf("verifier trust root was aliased to the caller's key: %v", err)
	}
}
