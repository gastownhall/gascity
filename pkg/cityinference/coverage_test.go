package cityinference

import (
	"context"
	"errors"
	"testing"
)

// AC1 fixture: a City invocation with transcript-level attribution and no
// proven step. The record links the authorized canonical chain and says "step
// unavailable" out loud, with no synthetic identity and no inline content.
func TestCoverageFixtureTranscriptLevelAttribution(t *testing.T) {
	m := testMapper()
	inv := CityInvocation{
		SessionNativeID: "sess-1", UpstreamReqID: "req-1",
		Provider: "anthropic", Model: "m-1", Status: "succeeded",
		Started:            mustTime(t, "2026-07-30T10:00:00Z"),
		RunTeamID:          "run_1",
		SessionTeamID:      "ses_1",
		TranscriptRecordID: "trc_1",
		Usage:              &UsageObservation{InputTokens: ptr(120), OutputTokens: ptr(30), CostMicros: ptr(4500)},
	}
	want := map[string]Coverage{
		FieldGroupTokens:     CoverageMetered,
		FieldGroupCost:       CoverageEstimated,
		FieldGroupSavings:    CoverageUnavailable,
		FieldGroupStep:       CoverageUnavailable,
		FieldGroupTranscript: CoverageKnown,
		FieldGroupOutcome:    CoverageKnown,
	}
	got := m.ExpectedCoverage(inv)
	if len(got) != len(CoverageFieldGroups()) {
		t.Fatalf("coverage must carry every field group, got %d", len(got))
	}
	for group, class := range want {
		if got[group] != class {
			t.Fatalf("%s: want %s, got %s", group, class, got[group])
		}
	}

	req, err := m.MapInvocation(inv)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if req.SourceStepRef != "" || req.Ordinal != nil {
		t.Fatal("mapping produced a step or an ordinal")
	}
	if req.RunTeamID != "run_1" || req.SessionTeamID != "ses_1" || req.TranscriptRecordID != "trc_1" {
		t.Fatalf("canonical links were rewritten: %+v", req)
	}
}

// Step coverage is unavailable for every shape of input this adapter can hold.
// There is no City field, provider locator or timestamp that turns it into
// anything else, which is the structural half of "never synthesize a step".
func TestStepCoverageIsUnavailableForEveryInput(t *testing.T) {
	m := testMapper()
	for _, inv := range []CityInvocation{
		{SessionNativeID: "sess-1", UpstreamReqID: "req-1"},
		{SessionNativeID: "", UpstreamReqID: "req-1"},
		{SessionNativeID: "sess-1", UpstreamReqID: "gen_1", TranscriptRecordID: "trc_1"},
		{
			SessionNativeID: "sess-1", UpstreamReqID: "req-1", RunTeamID: "run_1", SessionTeamID: "ses_1",
			Usage: &UsageObservation{InputTokens: ptr(1), CostMicros: ptr(1), SavedInputTokens: ptr(1)},
		},
	} {
		if got := m.ExpectedCoverage(inv)[FieldGroupStep]; got != CoverageUnavailable {
			t.Fatalf("step coverage %s for %+v", got, inv)
		}
	}
}

// A legacy-shaped row's absent groups are legacy_unknown, never unavailable:
// collapsing the two would let an unexplained blank pass as a structural
// absence. Step stays unavailable, because that absence really is structural.
func TestLegacyRowClassifiesAbsenceAsLegacyUnknown(t *testing.T) {
	m := testMapper()
	got := m.ExpectedCoverage(CityInvocation{UpstreamReqID: "req-1"})
	for _, group := range []string{FieldGroupTokens, FieldGroupCost, FieldGroupSavings} {
		if got[group] != CoverageLegacyUnknown {
			t.Fatalf("%s: want legacy_unknown, got %s", group, got[group])
		}
	}
	if got[FieldGroupStep] != CoverageUnavailable {
		t.Fatalf("step: want unavailable, got %s", got[FieldGroupStep])
	}
}

// Provider-asserted is not verified and derived cost is not billed: token
// counts classify metered, cost and savings classify estimated, and savings sit
// in their own group so nothing sums them into cost.
func TestMeteredAndEstimatedStaySeparateGroups(t *testing.T) {
	m := testMapper()
	got := m.ExpectedCoverage(CityInvocation{
		SessionNativeID: "sess-1", UpstreamReqID: "req-1",
		Usage: &UsageObservation{InputTokens: ptr(10), CostMicros: ptr(99), SavedInputTokens: ptr(5), SavedCostMicros: ptr(7)},
	})
	if got[FieldGroupTokens] != CoverageMetered {
		t.Fatalf("tokens: want metered, got %s", got[FieldGroupTokens])
	}
	if got[FieldGroupCost] != CoverageEstimated || got[FieldGroupSavings] != CoverageEstimated {
		t.Fatalf("derived money must be estimated: %+v", got)
	}

	// Savings alone never make tokens metered — an upper bound is not a count.
	savingsOnly := m.ExpectedCoverage(CityInvocation{
		SessionNativeID: "sess-1", UpstreamReqID: "req-1",
		Usage: &UsageObservation{SavedInputTokens: ptr(5)},
	})
	if savingsOnly[FieldGroupTokens] != CoverageUnavailable {
		t.Fatalf("savings raised the token class to %s", savingsOnly[FieldGroupTokens])
	}
}

// A server response that claims a step, a transcript link we never offered, or
// a completeness better than unavailable is refused. The producer is lending
// its name to these claims, so it checks them rather than trusting them.
func TestProducerRefusesRaisedCoverage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutil func(*Inference)
	}{
		{"step claimed known", func(in *Inference) { in.Coverage[FieldGroupStep] = string(CoverageKnown) }},
		{"transcript claimed known", func(in *Inference) { in.Coverage[FieldGroupTranscript] = string(CoverageKnown) }},
		{"field group omitted", func(in *Inference) { delete(in.Coverage, FieldGroupOutcome) }},
		{"completeness claimed known", func(in *Inference) { in.Completeness = string(CoverageKnown) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Producer{Mapper: testMapper()}
			req := CreateInferenceRequest{
				NativeIdentity:      NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "s", UpstreamReqID: "req-1"},
				ExternalInferenceID: mustDerive(t, NativeIdentity{Kind: NativeIdentityKind, Tenant: "org_alpha", SessionID: "s", UpstreamReqID: "req-1"}),
			}
			got := Inference{
				ID: "inf_1", ExternalInferenceID: req.ExternalInferenceID,
				Completeness: string(CoverageUnavailable),
				Coverage: map[string]string{
					FieldGroupTokens: string(CoverageMetered), FieldGroupCost: string(CoverageEstimated),
					FieldGroupSavings: string(CoverageUnavailable), FieldGroupStep: string(CoverageUnavailable),
					FieldGroupTranscript: string(CoverageUnavailable), FieldGroupOutcome: string(CoverageKnown),
				},
				FoldEligible: true,
			}
			tc.mutil(&got)
			if err := p.verify(req, got); !errors.Is(err, ErrCoverageRaised) {
				t.Fatalf("want ErrCoverageRaised, got %v", err)
			}
		})
	}
}

// Completeness over a best-effort producer is always unavailable, and the
// accepted record says so.
func TestAcceptedRecordReportsCompletenessUnavailable(t *testing.T) {
	api, p := newHarness(t)
	if _, err := p.Push(context.Background(), []CityInvocation{happyInvocation(t)}); err != nil {
		t.Fatalf("push: %v", err)
	}
	page, err := api.ListInferences(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want 1 inference, got %d", len(page.Items))
	}
	if page.Items[0].Completeness != string(CoverageUnavailable) {
		t.Fatalf("completeness %q", page.Items[0].Completeness)
	}
	if page.Items[0].Coverage[FieldGroupStep] != string(CoverageUnavailable) {
		t.Fatalf("step coverage %q", page.Items[0].Coverage[FieldGroupStep])
	}
}
