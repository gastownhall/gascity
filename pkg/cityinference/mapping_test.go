package cityinference

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The mapping decision table. Every row is a shape the design names, and every
// outcome is the one it names for it.
func TestMappingDecisionTable(t *testing.T) {
	m := testMapper()
	base := CityInvocation{
		SessionNativeID: "sess-1", UpstreamReqID: "req-1", Provider: "anthropic",
		Model: "m-1", Status: "succeeded", Started: mustTime(t, "2026-07-30T10:00:00Z"),
	}
	with := func(mutate func(*CityInvocation)) CityInvocation {
		inv := base
		mutate(&inv)
		return inv
	}

	for _, tc := range []struct {
		name string
		inv  CityInvocation
		want error
	}{
		{"provider-assigned request id admits", base, nil},
		{"locally generated request id admits", with(func(i *CityInvocation) { i.UpstreamReqID = "gen_7" }), nil},
		{"legacy empty session admits", with(func(i *CityInvocation) { i.SessionNativeID = "" }), nil},
		{"attempt row is refused", with(func(i *CityInvocation) { i.UpstreamReqID = "err_upstream_timeout" }), ErrAttemptRow},
		{"missing request id is refused", with(func(i *CityInvocation) { i.UpstreamReqID = "" }), ErrInvalidInvocation},
		{"missing model is refused", with(func(i *CityInvocation) { i.Model = "" }), ErrInvalidInvocation},
		{"missing start is refused", with(func(i *CityInvocation) { i.Started = time.Time{} }), ErrInvalidInvocation},
		{"end before start is refused", with(func(i *CityInvocation) {
			end := mustTime(t, "2026-07-30T09:00:00Z")
			i.Ended = &end
		}), ErrInvalidInvocation},
		{"oversized request id is refused", with(func(i *CityInvocation) {
			i.UpstreamReqID = string(make([]byte, maxNativeIDLen+1))
		}), ErrInvalidInvocation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.MapInvocation(tc.inv)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("want admission, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// An unmappable City status becomes unknown rather than traveling as itself:
// the canonical contract has no raw passthrough.
func TestOutcomeVocabularyIsClosed(t *testing.T) {
	m := testMapper()
	for status, want := range map[string]Outcome{
		"succeeded": OutcomeOK, "failed": OutcomeError, "canceled": OutcomeCancelled,
		"reticulating splines": OutcomeUnknown, "": OutcomeUnknown,
	} {
		req, err := m.MapInvocation(CityInvocation{
			SessionNativeID: "sess-1", UpstreamReqID: "req-1", Provider: "anthropic",
			Model: "m-1", Status: status, Started: mustTime(t, "2026-07-30T10:00:00Z"),
		})
		if err != nil {
			t.Fatalf("%q: %v", status, err)
		}
		if req.Outcome != want {
			t.Fatalf("%q: want %s, got %s", status, want, req.Outcome)
		}
	}
}

// Only an exact identity tuple folds, and only when that tuple names an
// invocation. Every other basis is refused outright rather than answered.
func TestFoldOnlyOnExactTuple(t *testing.T) {
	m := testMapper()
	a := CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "req-1", Provider: "anthropic", Model: "m-1"}
	b := a

	folds, err := m.Fold(CorrelationClaim{Basis: FoldExactTuple, A: a, B: b})
	if err != nil || !folds {
		t.Fatalf("exact tuple must fold: %v %v", folds, err)
	}

	for _, basis := range ForbiddenFoldBases() {
		folds, err := m.Fold(CorrelationClaim{Basis: basis, A: a, B: b})
		if !errors.Is(err, ErrHeuristicFold) {
			t.Fatalf("%s: want ErrHeuristicFold, got %v", basis, err)
		}
		if folds {
			t.Fatalf("%s: refused claim still folded", basis)
		}
	}
}

// Similarity is not identity. Two calls that agree on model, provider, tenant
// and timestamp but differ in request id stay separate — as do two locally
// generated ids, which never converge on the same real call.
func TestSimilarRecordsStaySeparate(t *testing.T) {
	m := testMapper()
	ts := mustTime(t, "2026-07-30T10:00:00Z")
	for _, tc := range []struct {
		name string
		a, b CityInvocation
	}{
		{
			"near-identical timestamps, different request ids",
			CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "req-1", Model: "m-1", Started: ts},
			CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "req-2", Model: "m-1", Started: ts.Add(1)},
		},
		{
			"two locally generated observations of one call",
			CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "gen_a", Model: "m-1", Started: ts},
			CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "gen_b", Model: "m-1", Started: ts},
		},
		{
			"legacy row and EIA row sharing a provider request id",
			CityInvocation{SessionNativeID: "", UpstreamReqID: "req-1", Model: "m-1", Started: ts},
			CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "req-1", Model: "m-1", Started: ts},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folds, err := m.Fold(CorrelationClaim{Basis: FoldExactTuple, A: tc.a, B: tc.b})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if folds {
				t.Fatal("records folded on a non-identity similarity")
			}
			if m.Identity(tc.a) == m.Identity(tc.b) {
				t.Fatal("distinct calls collapsed onto one identity")
			}
		})
	}

	// Even an exact tuple does not fold when the id was generated locally: two
	// such observations never converge, so folding them would hide a double
	// count instead of removing one.
	gen := CityInvocation{SessionNativeID: "sess-1", UpstreamReqID: "gen_a", Model: "m-1", Started: ts}
	folds, err := m.Fold(CorrelationClaim{Basis: FoldExactTuple, A: gen, B: gen})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if folds {
		t.Fatal("a locally generated identity was treated as fold-eligible")
	}
}

// Ordinal and step reference are refused at this adapter's edge, before a byte
// leaves, and with the specific reason rather than a generic parse error.
func TestPreflightRefusesSynthesizedLinkage(t *testing.T) {
	id := NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "sess-1", UpstreamReqID: "req-1"}
	base := CreateInferenceRequest{
		NativeIdentity: id, ExternalInferenceID: mustDerive(t, id),
		Provider: "anthropic", Model: "m-1", Outcome: OutcomeOK,
		StartedAt: mustTime(t, "2026-07-30T10:00:00Z"),
	}
	if err := Preflight(base); err != nil {
		t.Fatalf("clean request refused: %v", err)
	}

	withOrdinal := base
	withOrdinal.Ordinal = ptr(3)
	if err := Preflight(withOrdinal); !errors.Is(err, ErrSyntheticLinkage) {
		t.Fatalf("ordinal: want ErrSyntheticLinkage, got %v", err)
	}

	withStep := base
	withStep.SourceStepRef = "step_9"
	if err := Preflight(withStep); !errors.Is(err, ErrSyntheticLinkage) {
		t.Fatalf("step: want ErrSyntheticLinkage, got %v", err)
	}
}

// The scanner works on outbound bytes, so it catches a field the contract does
// not admit no matter how it got there.
func TestScannerRefusesUnadmittedFieldsAndCredentials(t *testing.T) {
	id := NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "sess-1", UpstreamReqID: "req-1"}
	clean, err := json.Marshal(CreateInferenceRequest{
		NativeIdentity: id, ExternalInferenceID: mustDerive(t, id),
		Provider: "anthropic", Model: "m-1", Outcome: OutcomeOK,
		StartedAt: mustTime(t, "2026-07-30T10:00:00Z"),
		Usage:     &UsageObservation{InputTokens: ptr(1)},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := ScanForbidden(clean); err != nil {
		t.Fatalf("clean payload refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{"prompt text", `{"provider":"p","model":"m","prompt":"who are you"}`, ErrContentLeak},
		{"completion text", `{"provider":"p","model":"m","completion":"I am"}`, ErrContentLeak},
		{"transcript text", `{"provider":"p","model":"m","messages":[{"role":"user"}]}`, ErrContentLeak},
		{"raw provider payload", `{"provider":"p","model":"m","raw_response":{"id":"x"}}`, ErrContentLeak},
		{"nested content", `{"usage":{"input_tokens":1,"prompt":"hi"}}`, ErrContentLeak},
		{"bearer credential", `{"provider":"Bearer sk-abcdefgh12345678","model":"m"}`, ErrCredentialLeak},
		{"key identifier", `{"provider":"p","model":"m","native_identity":{"kind":"manifold.triplet.v1","tenant":"t","session_id":"s","upstream_req_id":"api_key=zz"}}`, ErrCredentialLeak},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ScanForbidden([]byte(tc.body)); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// The admitted key set is closed and matches the request struct exactly. If a
// field is ever added to the struct without being admitted here, this fails —
// which is the point: struct drift is how content first arrives.
func TestAdmittedKeySetMatchesTheRequestStruct(t *testing.T) {
	id := NativeIdentity{Kind: NativeIdentityKind, Tenant: "t", SessionID: "s", UpstreamReqID: "req-1"}
	raw, err := json.Marshal(CreateInferenceRequest{
		NativeIdentity: id, ExternalInferenceID: "x", Provider: "p", Model: "m",
		Outcome: OutcomeOK, StartedAt: mustTime(t, "2026-07-30T10:00:00Z"),
		EndedAt: func() *time.Time { ts := mustTime(t, "2026-07-30T10:01:00Z"); return &ts }(),
		Epoch:   1, SessionTeamID: "ses_1", RunTeamID: "run_1", TranscriptRecordID: "trc_1",
		ObservationID: "primary", Usage: &UsageObservation{
			InputTokens: ptr(1), OutputTokens: ptr(1), CachedInputTokens: ptr(1),
			CostMicros: ptr(1), SavedInputTokens: ptr(1), SavedCostMicros: ptr(1),
		},
		Ordinal: ptr(1), SourceStepRef: "step_1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key := range body {
		if !admittedKeys[""][key] {
			t.Fatalf("request struct emits unadmitted key %q", key)
		}
	}
	for level, sub := range map[string]json.RawMessage{"native_identity": body["native_identity"], "usage": body["usage"]} {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(sub, &obj); err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		for key := range obj {
			if !admittedKeys[level][key] {
				t.Fatalf("%s emits unadmitted key %q", level, key)
			}
		}
		if len(obj) != len(admittedKeys[level]) {
			t.Fatalf("%s: allowlist has %d keys, struct emits %d", level, len(admittedKeys[level]), len(obj))
		}
	}
}
