package cityinference

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func ptr(v uint64) *uint64 { return &v }

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func testSource() Source {
	return Source{SourceID: "src_city", Kind: "city", Tenant: "org_alpha", Epoch: 7}
}

func testMapper() Mapper { return Mapper{Source: testSource()} }

func mustDerive(t *testing.T, id NativeIdentity) string {
	t.Helper()
	got, err := DeriveExternalInferenceID(id)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return got
}

// The identity tuple is exactly (tenant, session_id, upstream_req_id). Nothing
// else may move the derived ID, and each component moving it alone is what
// proves the triplet is the whole identity.
func TestIdentityIsTheTripletAndNothingElse(t *testing.T) {
	base := NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "sess-1", UpstreamReqID: "req-1"}
	baseID := mustDerive(t, base)

	for _, tc := range []struct {
		name string
		id   NativeIdentity
	}{
		{"tenant", NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_beta", SessionID: "sess-1", UpstreamReqID: "req-1"}},
		{"session", NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "sess-2", UpstreamReqID: "req-1"}},
		{"req", NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "sess-1", UpstreamReqID: "req-2"}},
	} {
		if got := mustDerive(t, tc.id); got == baseID {
			t.Fatalf("changing %s left the identity unchanged: %s", tc.name, got)
		}
	}

	// A model, a provider and a timestamp are outside the tuple, so two
	// invocations that differ only in those share one identity.
	m := testMapper()
	a := CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "req-1", Provider: "anthropic", Model: "m-1", Started: mustTime(t, "2026-07-30T10:00:00Z")}
	b := a
	b.Provider, b.Model, b.Started = "openai", "m-2", mustTime(t, "2026-07-30T11:00:00Z")
	if m.Identity(a) != m.Identity(b) {
		t.Fatal("model/provider/timestamp leaked into the identity tuple")
	}
}

// Length-prefixed framing keeps the preimage injective when a component is
// empty (the legacy row) or carries a delimiter byte. A naive join does not,
// and a collision here would silently merge two tenants' invocations.
func TestDerivationIsInjectiveAcrossEmptyAndDelimiterComponents(t *testing.T) {
	cases := []NativeIdentity{
		{Kind: NativeIdentityKind, Tenant: "a", SessionID: "", UpstreamReqID: "bc"},
		{Kind: NativeIdentityKind, Tenant: "a", SessionID: "b", UpstreamReqID: "c"},
		{Kind: NativeIdentityKind, Tenant: "ab", SessionID: "", UpstreamReqID: "c"},
		{Kind: NativeIdentityKind, Tenant: "a:b", SessionID: "c", UpstreamReqID: "d"},
		{Kind: NativeIdentityKind, Tenant: "a", SessionID: "b:c", UpstreamReqID: "d"},
	}
	seen := map[string]NativeIdentity{}
	for _, id := range cases {
		got := mustDerive(t, id)
		if prior, dup := seen[got]; dup {
			t.Fatalf("collision: %+v and %+v both derive %s", prior, id, got)
		}
		seen[got] = id
	}
}

// The provider request ID must never become a public identifier, so the derived
// ID may not contain it in any recoverable form.
func TestDerivedIDDoesNotCarryTheProviderRequestID(t *testing.T) {
	id := NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "sess-1", UpstreamReqID: "req_abc123secretish"}
	got := mustDerive(t, id)
	if !strings.HasPrefix(got, "mfi1_") {
		t.Fatalf("unexpected identity shape %q", got)
	}
	for _, part := range []string{id.Tenant, id.SessionID, id.UpstreamReqID} {
		if strings.Contains(got, part) {
			t.Fatalf("derived id %q leaks component %q", got, part)
		}
	}
}

// The empty session ID is a legal legacy value, not a null. Refusing it would
// force a producer to invent one, which is the exact synthesis this domain
// forbids.
func TestLegacyEmptySessionMapsWithoutSynthesis(t *testing.T) {
	m := testMapper()
	req, err := m.MapInvocation(CityInvocation{
		UpstreamReqID: "req-legacy", Provider: "anthropic", Model: "m-1",
		Started: mustTime(t, "2026-07-30T10:00:00Z"),
	})
	if err != nil {
		t.Fatalf("legacy invocation refused: %v", err)
	}
	if req.NativeIdentity.SessionID != "" {
		t.Fatalf("legacy session id was synthesized as %q", req.NativeIdentity.SessionID)
	}
}

func TestRequestIDPrefixClassification(t *testing.T) {
	for _, tc := range []struct {
		reqID string
		scope IdentityScope
		err   error
	}{
		{"req-provider-1", ScopeInvocation, nil},
		{"gen_9f2a", ScopeObservation, nil},
		{"err_timeout", "", ErrAttemptRow},
		{"", "", ErrInvalidInvocation},
	} {
		scope, err := ClassifyReqID(tc.reqID)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Fatalf("%q: want %v, got %v", tc.reqID, tc.err, err)
			}
			continue
		}
		if err != nil || scope != tc.scope {
			t.Fatalf("%q: want %s, got %s (%v)", tc.reqID, tc.scope, scope, err)
		}
	}
}

// The tenant is enrollment data. A City-side field may not redirect a record
// into another tenant's identity space, so the mapper reads it from the source
// and there is no invocation field that could override it.
func TestTenantComesFromEnrollmentNotFromTheInvocation(t *testing.T) {
	m := testMapper()
	req, err := m.MapInvocation(CityInvocation{
		SessionNativeID: "sess-1", UpstreamReqID: "req-1", Provider: "anthropic",
		Model: "m-1", Started: mustTime(t, "2026-07-30T10:00:00Z"),
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if req.NativeIdentity.Tenant != testSource().Tenant {
		t.Fatalf("tenant %q is not the enrolled tenant", req.NativeIdentity.Tenant)
	}
}

// The offered external ID is a checksum, never a choice.
func TestPreflightRejectsAChosenExternalID(t *testing.T) {
	m := testMapper()
	req, err := m.MapInvocation(CityInvocation{
		SessionNativeID: "sess-1", UpstreamReqID: "req-1", Provider: "anthropic",
		Model: "m-1", Started: mustTime(t, "2026-07-30T10:00:00Z"),
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	req.ExternalInferenceID = "mfi1_notthederivation"
	if err := Preflight(req); !errors.Is(err, ErrInvalidInvocation) {
		t.Fatalf("want ErrInvalidInvocation, got %v", err)
	}
}
