package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

func TestPlanSessionLifecycleStartSelection(t *testing.T) {
	now := time.Date(2026, 7, 26, 7, 0, 0, 0, time.UTC)
	ready := sessionLifecycleStartShadowInput{
		Info: session.Info{
			ID:            "session-ready",
			State:         session.StateAsleep,
			MetadataState: string(session.StateAsleep),
		},
		WakeDecisionObserved: true,
		ShouldWake:           true,
		RuntimeObserved:      true,
		ObservedAt:           now,
		StartupTimeout:       time.Minute,
	}

	tests := []struct {
		name       string
		mutate     func(*sessionLifecycleStartShadowInput)
		want       sessionLifecycleStartSelectionOutcome
		wantReason sessionLifecycleStartSelectionReason
	}{
		{
			name:       "ready",
			want:       sessionLifecycleStartSelectionPrepare,
			wantReason: sessionLifecycleStartSelectionReasonReady,
		},
		{
			name: "invalid id",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.Info.ID = " session-invalid "
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonInvalidInput,
		},
		{
			name: "terminal",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.Info.Closed = true
			},
			want:       sessionLifecycleStartSelectionNoop,
			wantReason: sessionLifecycleStartSelectionReasonTerminal,
		},
		{
			name: "wake decision unknown",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.WakeDecisionObserved = false
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonWakeUnknown,
		},
		{
			name: "runtime unknown",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.RuntimeObserved = false
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonRuntimeUnknown,
		},
		{
			name: "observation time unknown",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.ObservedAt = time.Time{}
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonObservationUnknown,
		},
		{
			name: "config suppressed",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.ShouldWake = false
				input.ConfigSuppressed = true
			},
			want:       sessionLifecycleStartSelectionNoop,
			wantReason: sessionLifecycleStartSelectionReasonConfigSuppressed,
		},
		{
			name: "not needed",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.ShouldWake = false
			},
			want:       sessionLifecycleStartSelectionNoop,
			wantReason: sessionLifecycleStartSelectionReasonNotNeeded,
		},
		{
			name: "already running",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.RuntimeAlive = true
			},
			want:       sessionLifecycleStartSelectionNoop,
			wantReason: sessionLifecycleStartSelectionReasonAlreadyRunning,
		},
		{
			name: "failed create",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.Info.State = session.StateFailedCreate
				input.Info.MetadataState = string(session.StateFailedCreate)
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonFailedCreate,
		},
		{
			name: "quarantined",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.Info.QuarantinedUntil = now.Add(time.Minute).Format(time.RFC3339)
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonQuarantined,
		},
		{
			name: "start in flight",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.Info.State = session.StateCreating
				input.Info.MetadataState = string(session.StateCreating)
				input.Info.PendingCreateClaim = true
				input.Info.PendingCreateClaimMetadata = "true"
				input.Info.LastWokeAt = now.Format(time.RFC3339)
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonStartInFlight,
		},
		{
			name: "circuit open",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.CircuitOpen = true
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonCircuitOpen,
		},
		{
			name: "provider unavailable",
			mutate: func(input *sessionLifecycleStartShadowInput) {
				input.ProviderUnavailable = true
			},
			want:       sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonProviderUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := ready
			if tt.mutate != nil {
				tt.mutate(&input)
			}
			got := planSessionLifecycleStartSelection(input)
			if got.SessionID != input.Info.ID || got.Outcome != tt.want || got.Reason != tt.wantReason {
				t.Fatalf("plan = %+v, want session=%q outcome=%v reason=%v", got, input.Info.ID, tt.want, tt.wantReason)
			}
		})
	}
}

func TestCompareSessionLifecycleStartSelection(t *testing.T) {
	tests := []struct {
		name           string
		plan           sessionLifecycleStartSelectionPlan
		legacySelected bool
		want           sessionLifecycleStartSelectionComparisonOutcome
		wantReason     sessionLifecycleStartSelectionComparisonReason
	}{
		{
			name: "matching prepare",
			plan: sessionLifecycleStartSelectionPlan{
				SessionID: "session-prepare",
				Outcome:   sessionLifecycleStartSelectionPrepare,
				Reason:    sessionLifecycleStartSelectionReasonReady,
			},
			legacySelected: true,
			want:           sessionLifecycleStartSelectionComparisonMatched,
			wantReason:     sessionLifecycleStartSelectionComparisonReasonEquivalent,
		},
		{
			name: "matching known park",
			plan: sessionLifecycleStartSelectionPlan{
				SessionID: "session-park",
				Outcome:   sessionLifecycleStartSelectionPark,
				Reason:    sessionLifecycleStartSelectionReasonCircuitOpen,
			},
			want:       sessionLifecycleStartSelectionComparisonMatched,
			wantReason: sessionLifecycleStartSelectionComparisonReasonEquivalent,
		},
		{
			name: "selection mismatch",
			plan: sessionLifecycleStartSelectionPlan{
				SessionID: "session-mismatch",
				Outcome:   sessionLifecycleStartSelectionPrepare,
				Reason:    sessionLifecycleStartSelectionReasonReady,
			},
			want:       sessionLifecycleStartSelectionComparisonMismatched,
			wantReason: sessionLifecycleStartSelectionComparisonReasonSelectionMismatch,
		},
		{
			name: "unknown observation",
			plan: sessionLifecycleStartSelectionPlan{
				SessionID: "session-unknown",
				Outcome:   sessionLifecycleStartSelectionPark,
				Reason:    sessionLifecycleStartSelectionReasonRuntimeUnknown,
			},
			want:       sessionLifecycleStartSelectionComparisonIncomparable,
			wantReason: sessionLifecycleStartSelectionComparisonReasonShadowUnknown,
		},
		{
			name: "invalid plan",
			plan: sessionLifecycleStartSelectionPlan{
				SessionID: "session-invalid",
			},
			want:       sessionLifecycleStartSelectionComparisonIncomparable,
			wantReason: sessionLifecycleStartSelectionComparisonReasonCandidateInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareSessionLifecycleStartSelection(tt.plan, tt.legacySelected)
			if got.Outcome != tt.want || got.Reason != tt.wantReason {
				t.Fatalf("comparison = %+v, want outcome=%v reason=%v", got, tt.want, tt.wantReason)
			}
			if got.LegacySelected != tt.legacySelected {
				t.Fatalf("legacy selected = %v, want %v", got.LegacySelected, tt.legacySelected)
			}
		})
	}
}
