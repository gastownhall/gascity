package slingprivate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/slingassert"
)

const (
	testKid      = "bts-mint-1"
	testWorkload = "spiffe://gasworks/ns/bts/sa/city-sling-gateway"
	testCity     = "downtown"
)

var testNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// spyDispatcher is both the dispatch spy and the resolution spy for this
// boundary: the boundary itself resolves nothing, so target lookup happens
// strictly downstream of Dispatch. Zero calls therefore proves a rejection
// landed before both dispatch and target resolution.
type spyDispatcher struct {
	calls  []Command
	result Result
	err    error
}

func (s *spyDispatcher) Dispatch(_ context.Context, cmd Command) (Result, error) {
	s.calls = append(s.calls, cmd)
	return s.result, s.err
}

type harness struct {
	boundary *Boundary
	spy      *spyDispatcher
	priv     ed25519.PrivateKey
	handler  http.Handler
}

func newHarness(t *testing.T, tweak ...func(*slingassert.Options, *Options)) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	vopts := slingassert.Options{
		Keys: map[string]ed25519.PublicKey{testKid: pub},
		Commands: map[string]slingassert.Command{
			slingassert.CommandSlingCityWork: {
				PolicyID:    slingassert.PolicySlingCityWork,
				OperationID: slingassert.CommandSlingCityWork,
			},
		},
		MaxTTL: 2 * time.Minute,
		Skew:   30 * time.Second,
		Now:    func() time.Time { return testNow },
	}
	spy := &spyDispatcher{result: Result{Status: http.StatusAccepted, Body: []byte(`{"status":"routed"}`)}}
	bopts := Options{
		Dispatcher: spy,
		Workload:   func(*http.Request) (string, bool) { return testWorkload, true },
		Auditf:     func(string, ...any) {},
	}
	for _, f := range tweak {
		f(&vopts, &bopts)
	}
	v, err := slingassert.New(vopts)
	if err != nil {
		t.Fatalf("slingassert.New: %v", err)
	}
	bopts.Verifier = v
	b, err := New(bopts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{boundary: b, spy: spy, priv: priv, handler: b.Handler()}
}

func validBody() []byte { return []byte(`{"target":"ada","formula":"triage"}`) }

func validAssertion(body []byte) slingassert.Assertion {
	return slingassert.Assertion{
		Kid:         testKid,
		Aud:         slingassert.Audience,
		Nonce:       "nonce-0001",
		IAT:         testNow.Unix(),
		Exp:         testNow.Add(time.Minute).Unix(),
		Workload:    testWorkload,
		Method:      http.MethodPost,
		Command:     slingassert.CommandSlingCityWork,
		BodyHash:    slingassert.BodyHash(body),
		PolicyID:    slingassert.PolicySlingCityWork,
		OperationID: slingassert.CommandSlingCityWork,
		Org:         "org-acme",
		Workspace:   "ws-main",
		Principal:   "user-42",
		Source:      "web",
		City:        testCity,
		Project:     "rig-alpha",
		Issue:       "bd-101",
	}
}

func (h *harness) mint(t *testing.T, a slingassert.Assertion) string {
	t.Helper()
	payload, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sig := ed25519.Sign(h.priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func privatePath(command string) string {
	return PathPrefix + testCity + "/sling/" + command
}

func (h *harness) request(t *testing.T, a slingassert.Assertion, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost,
		privatePath(slingassert.CommandSlingCityWork), strings.NewReader(string(body)))
	r.Header.Set(AssertionHeader, h.mint(t, a))
	return r
}

func (h *harness) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func TestHappyPathDispatchesOnce(t *testing.T) {
	h := newHarness(t)
	body := validBody()
	w := h.do(h.request(t, validAssertion(body), body))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", w.Code, w.Body)
	}
	if len(h.spy.calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(h.spy.calls))
	}
	got := h.spy.calls[0]
	if got.Org != "org-acme" || got.Workspace != "ws-main" || got.Principal != "user-42" || got.Source != "web" {
		t.Fatalf("dispatch lost the verified tenant: %+v", got)
	}
	if got.City != testCity || got.Project != "rig-alpha" || got.Issue != "bd-101" {
		t.Fatalf("dispatch lost the verified target: %+v", got)
	}
	if got.OperationID != slingassert.CommandSlingCityWork || got.PolicyID != slingassert.PolicySlingCityWork {
		t.Fatalf("dispatch lost the authorization: %+v", got)
	}
}

// TestRejectionSetPerformsNoDispatch is the G-003 city rejection set. Each case
// must be refused before target resolution or dispatch, and every one must
// produce the byte-identical response so no case is a tenant oracle.
func TestRejectionSetPerformsNoDispatch(t *testing.T) {
	body := validBody()

	cases := []struct {
		name    string
		mutate  func(*slingassert.Assertion)
		prepare func(*harness, *http.Request)
	}{
		{
			name: "direct public credential: a Bearer token instead of an assertion",
			prepare: func(_ *harness, r *http.Request) {
				r.Header.Del(AssertionHeader)
				r.Header.Set("Authorization", "Bearer pub-token")
			},
		},
		{
			name: "broad gateway identity: a session cookie instead of an assertion",
			prepare: func(_ *harness, r *http.Request) {
				r.Header.Del(AssertionHeader)
				r.Header.Set("Cookie", "gc_session=abc")
			},
		},
		{
			name:    "dual credential: a valid assertion plus a Bearer token",
			prepare: func(_ *harness, r *http.Request) { r.Header.Set("Authorization", "Bearer pub-token") },
		},
		{
			name:    "dual credential: a valid assertion plus a city-write grant",
			prepare: func(_ *harness, r *http.Request) { r.Header.Set("X-GC-City-Write", "grant.sig") },
		},
		{
			name:    "public CSRF header re-pointed at the private port",
			prepare: func(_ *harness, r *http.Request) { r.Header.Set("X-GC-Request", "1") },
		},
		{
			name:    "no credential at all",
			prepare: func(_ *harness, r *http.Request) { r.Header.Del(AssertionHeader) },
		},
		{
			name: "two assertions: the boundary must not choose",
			prepare: func(h *harness, r *http.Request) {
				r.Header.Add(AssertionHeader, h.mint(t, validAssertion(body)))
			},
		},
		{
			name: "unknown workload: no verified mTLS identity",
			prepare: func(h *harness, _ *http.Request) {
				h.boundary.workload = func(*http.Request) (string, bool) { return "", false }
			},
		},
		{
			name: "unknown workload: a different verified peer",
			prepare: func(h *harness, _ *http.Request) {
				h.boundary.workload = func(*http.Request) (string, bool) {
					return "spiffe://gasworks/ns/other/sa/broker", true
				}
			},
		},
		{
			name:   "tenant disagreement: assertion minted for another city",
			mutate: func(a *slingassert.Assertion) { a.City = "uptown" },
		},
		{
			name: "expiry",
			mutate: func(a *slingassert.Assertion) {
				a.IAT, a.Exp = testNow.Add(-time.Hour).Unix(), testNow.Add(-59*time.Minute).Unix()
			},
		},
		{
			name:    "body mutation after minting",
			prepare: func(_ *harness, r *http.Request) { r.Body = http.NoBody },
		},
		{
			name:   "target mutation: assertion re-pointed at another issue",
			mutate: func(a *slingassert.Assertion) { a.Issue = "bd-999" },
			prepare: func(h *harness, r *http.Request) {
				// Body names the original issue, so the assertion and the body
				// disagree about the target.
				replaceBody(r, []byte(`{"target":"ada","bead":"bd-101"}`))
				rehash(t, h, r, []byte(`{"target":"ada","bead":"bd-101"}`), func(a *slingassert.Assertion) { a.Issue = "bd-999" })
			},
		},
		{
			name: "caller-supplied org in the body",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","org":"org-victim"}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name: "caller-supplied principal in the body",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","principal":"root"}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name: "caller-supplied source in the body",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","source":"trusted-internal"}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name: "caller-supplied workspace in the body",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","workspace":"ws-victim"}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name: "owner disagreement in the body",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","owner":"someone-else"}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name: "rig disagreement in the body",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","rig":"rig-victim"}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name: "formula variable shadowing the verified target",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","formula":"triage","vars":{"bead":"bd-999"}}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name: "formula variable shadowing the verified tenant",
			prepare: func(h *harness, r *http.Request) {
				b := []byte(`{"target":"ada","formula":"triage","vars":{"org":"org-victim"}}`)
				replaceBody(r, b)
				rehash(t, h, r, b, nil)
			},
		},
		{
			name:   "unregistered broker fallback (broker_bypass)",
			mutate: func(a *slingassert.Assertion) { a.Command = "brokerDispatch" },
			prepare: func(_ *harness, r *http.Request) {
				r.URL.Path = privatePath("brokerDispatch")
			},
		},
	}

	// The happy-path response is what every rejection must NOT look like; the
	// first rejection's response is the reference every other must match.
	var reference []byte
	var referenceCode int

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			a := validAssertion(body)
			if tc.mutate != nil {
				tc.mutate(&a)
			}
			r := h.request(t, a, body)
			if tc.prepare != nil {
				tc.prepare(h, r)
			}
			w := h.do(r)

			if len(h.spy.calls) != 0 {
				t.Fatalf("rejected request reached dispatch/resolution: %+v", h.spy.calls)
			}
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", w.Code, w.Body)
			}
			if i == 0 {
				reference, referenceCode = w.Body.Bytes(), w.Code
				return
			}
			if w.Code != referenceCode || w.Body.String() != string(reference) {
				t.Fatalf("rejection is distinguishable: got %d %s, want %d %s",
					w.Code, w.Body, referenceCode, reference)
			}
		})
	}
}

// replaceBody swaps the request body so a test can send bytes other than the
// harness default (paired with rehash when the assertion should cover them).
func replaceBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
}

// rehash re-mints the assertion over the replacement body so the case under
// test is the taint check, not a stale body hash.
func rehash(t *testing.T, h *harness, r *http.Request, body []byte, mutate func(*slingassert.Assertion)) {
	t.Helper()
	a := validAssertion(body)
	if mutate != nil {
		mutate(&a)
	}
	r.Header.Set(AssertionHeader, h.mint(t, a))
}

func TestDuplicateDeliveryReturnsPriorResult(t *testing.T) {
	h := newHarness(t)
	body := validBody()
	a := validAssertion(body)

	first := h.do(h.request(t, a, body))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.Code)
	}
	second := h.do(h.request(t, a, body))

	if len(h.spy.calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1 — a duplicate delivery must not dispatch again", len(h.spy.calls))
	}
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("duplicate returned %d %s, want the prior %d %s",
			second.Code, second.Body, first.Code, first.Body)
	}
}

func TestOrchestratorFailureReturnsNormalizedEvidence(t *testing.T) {
	h := newHarness(t)
	h.spy.err = errors.New("city orchestrator: rig /srv/rigs/alpha unreachable")
	body := validBody()
	a := validAssertion(body)

	w := h.do(h.request(t, a, body))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "srv/rigs/alpha") {
		t.Fatalf("failure response leaked City-internal detail: %s", w.Body)
	}

	// The assertion was single-use and is spent, so a duplicate delivery gets
	// the recorded failure rather than a second real dispatch.
	again := h.do(h.request(t, a, body))
	if len(h.spy.calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1 — a failed dispatch must not be retried by replay", len(h.spy.calls))
	}
	if again.Code != w.Code || again.Body.String() != w.Body.String() {
		t.Fatalf("duplicate after failure returned %d %s, want %d %s", again.Code, again.Body, w.Code, w.Body)
	}
}

// TestDuplicateWhileInFlightIsRefused covers the result-store miss: the first
// dispatch has not recorded a result yet, so the duplicate must be told to
// retry rather than dispatched.
func TestDuplicateWhileInFlightIsRefused(t *testing.T) {
	h := newHarness(t, func(_ *slingassert.Options, o *Options) { o.Results = emptyStore{} })
	body := validBody()
	a := validAssertion(body)

	if w := h.do(h.request(t, a, body)); w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", w.Code)
	}
	w := h.do(h.request(t, a, body))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if len(h.spy.calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(h.spy.calls))
	}
}

type emptyStore struct{}

func (emptyStore) Get(string) (Result, bool)     { return Result{}, false }
func (emptyStore) Put(string, Result, time.Time) {}

// TestRollbackClosesOnlySling is the AC3 rollback runtime-registry drill:
// withdrawing sling from the registry refuses it, and no alternate command
// becomes reachable in its place.
func TestRollbackClosesOnlySling(t *testing.T) {
	h := newHarness(t, func(v *slingassert.Options, _ *Options) {
		v.Commands = map[string]slingassert.Command{
			"someOtherCommand": {PolicyID: "p", OperationID: "o"},
		}
	})
	body := validBody()
	w := h.do(h.request(t, validAssertion(body), body))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if len(h.spy.calls) != 0 {
		t.Fatalf("a withdrawn command reached dispatch: %+v", h.spy.calls)
	}

	// The registry entry that remains is not a fallback: an assertion naming it
	// still fails, because this boundary serves the sling command only.
	r := httptest.NewRequest(http.MethodPost, privatePath("someOtherCommand"),
		strings.NewReader(string(body)))
	a := validAssertion(body)
	a.Command = "someOtherCommand"
	a.PolicyID, a.OperationID = "p", "o"
	r.Header.Set(AssertionHeader, h.mint(t, a))
	if got := h.do(r); got.Code == http.StatusAccepted {
		t.Fatal("a non-sling registry entry became a dispatch fallback")
	}
	if len(h.spy.calls) != 0 {
		t.Fatalf("a non-sling command reached the sling dispatcher: %+v", h.spy.calls)
	}
}

func TestNonPrivateGrammarIsNotServed(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/v0/city/" + testCity + "/sling",
		PathPrefix,
		PathPrefix + testCity + "/sling",
		PathPrefix + testCity + "/sling/" + slingassert.CommandSlingCityWork + "/extra",
		PathPrefix + "/sling/" + slingassert.CommandSlingCityWork,
	} {
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if w.Code == http.StatusAccepted {
			t.Fatalf("path %q was served", path)
		}
		if len(h.spy.calls) != 0 {
			t.Fatalf("path %q reached dispatch", path)
		}
	}
}

func TestGetIsNotServed(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		privatePath(slingassert.CommandSlingCityWork), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if len(h.spy.calls) != 0 {
		t.Fatal("a GET reached dispatch")
	}
}

func TestNewRefusesWeakConfiguration(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	v, err := slingassert.New(slingassert.Options{
		Keys: map[string]ed25519.PublicKey{testKid: pub},
		Commands: map[string]slingassert.Command{
			slingassert.CommandSlingCityWork: {PolicyID: slingassert.PolicySlingCityWork, OperationID: slingassert.CommandSlingCityWork},
		},
		MaxTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("slingassert.New: %v", err)
	}
	spy := &spyDispatcher{}
	wl := func(*http.Request) (string, bool) { return testWorkload, true }

	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no verifier", Options{Dispatcher: spy, Workload: wl}},
		{"no dispatcher", Options{Verifier: v, Workload: wl}},
		{"no workload extractor", Options{Verifier: v, Dispatcher: spy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatal("New should have refused the options")
			}
		})
	}
}

func TestSPIFFEWorkload(t *testing.T) {
	spiffe := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return u
	}
	withChain := func(uris ...*url.URL) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.TLS = &tls.ConnectionState{
			HandshakeComplete: true,
			VerifiedChains:    [][]*x509.Certificate{{{URIs: uris}}},
		}
		return r
	}

	got, ok := SPIFFEWorkload(withChain(spiffe("spiffe://GASWORKS/ns/bts/sa/city-sling-gateway")))
	if !ok || got != testWorkload {
		t.Fatalf("SPIFFEWorkload = %q, %v; want %q, true", got, ok, testWorkload)
	}

	// No TLS, an incomplete handshake, an unverified chain, a non-SPIFFE SAN,
	// and an ambiguous multi-SAN leaf all yield no identity.
	if _, ok := SPIFFEWorkload(httptest.NewRequest(http.MethodPost, "/", nil)); ok {
		t.Fatal("a plaintext request produced a workload identity")
	}
	unverified := httptest.NewRequest(http.MethodPost, "/", nil)
	unverified.TLS = &tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  []*x509.Certificate{{URIs: []*url.URL{spiffe(testWorkload)}}},
	}
	if _, ok := SPIFFEWorkload(unverified); ok {
		t.Fatal("an unverified peer certificate produced a workload identity")
	}
	if _, ok := SPIFFEWorkload(withChain(spiffe("https://example.test/svc"))); ok {
		t.Fatal("a non-SPIFFE SAN produced a workload identity")
	}
	if _, ok := SPIFFEWorkload(withChain(spiffe(testWorkload), spiffe("spiffe://gasworks/ns/other/sa/x"))); ok {
		t.Fatal("a multi-SAN leaf produced a workload identity")
	}
}
