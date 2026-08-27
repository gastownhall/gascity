package main

import (
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionLifecycleShadowProjectionIsImmutableAndKeyed(t *testing.T) {
	inputs := []sessionLifecycleShadowInput{
		{
			Info: session.Info{
				ID:           "session-b",
				Labels:       []string{"gc:session"},
				AliasHistory: []string{"old-b"},
			},
			RuntimeObserved: true,
		},
		{Info: session.Info{ID: "session-a"}, RuntimeObserved: true},
	}

	projection, err := newSessionLifecycleShadowProjection(7, inputs)
	if err != nil {
		t.Fatalf("newSessionLifecycleShadowProjection() error = %v", err)
	}
	inputs[0].Info.Labels[0] = "mutated-input"
	inputs[0].Info.AliasHistory[0] = "mutated-input"

	got, ok := projection.Read("session-b")
	if !ok {
		t.Fatal("exact keyed read did not find session-b")
	}
	if got.Info.Labels[0] != "gc:session" || got.Info.AliasHistory[0] != "old-b" {
		t.Fatalf("projection retained caller-owned slices: labels=%v aliases=%v", got.Info.Labels, got.Info.AliasHistory)
	}
	got.Info.Labels[0] = "mutated-read"
	got.Info.AliasHistory[0] = "mutated-read"
	again, ok := projection.Read("session-b")
	if !ok {
		t.Fatal("second exact keyed read did not find session-b")
	}
	if again.Info.Labels[0] != "gc:session" || again.Info.AliasHistory[0] != "old-b" {
		t.Fatalf("returned read mutated projection: labels=%v aliases=%v", again.Info.Labels, again.Info.AliasHistory)
	}
	if projection.Generation() != 7 {
		t.Fatalf("generation = %d, want 7", projection.Generation())
	}
	if got := projection.Keys(); !reflect.DeepEqual(got, []string{"session-a", "session-b"}) {
		t.Fatalf("Keys() = %v, want deterministic sorted census", got)
	}
	if _, ok := projection.Read("missing"); ok {
		t.Fatal("missing exact key reported present")
	}
}

func TestSessionLifecycleShadowProjectionRejectsInvalidIdentity(t *testing.T) {
	tests := []struct {
		name       string
		generation uint64
		inputs     []sessionLifecycleShadowInput
	}{
		{
			name:       "zero generation",
			generation: 0,
			inputs:     []sessionLifecycleShadowInput{{Info: session.Info{ID: "session-a"}}},
		},
		{
			name:       "empty id",
			generation: 1,
			inputs:     []sessionLifecycleShadowInput{{Info: session.Info{}}},
		},
		{
			name:       "noncanonical id",
			generation: 1,
			inputs:     []sessionLifecycleShadowInput{{Info: session.Info{ID: " session-a "}}},
		},
		{
			name:       "duplicate id",
			generation: 1,
			inputs: []sessionLifecycleShadowInput{
				{Info: session.Info{ID: "session-a"}},
				{Info: session.Info{ID: "session-a"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newSessionLifecycleShadowProjection(tt.generation, tt.inputs); err == nil {
				t.Fatal("newSessionLifecycleShadowProjection() error = nil")
			}
		})
	}
}

func TestPlanSessionLifecycleStatusMatchesLegacyDerivation(t *testing.T) {
	now := time.Date(2026, 7, 26, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		input      sessionLifecycleShadowInput
		want       sessionLifecycleStatusOutcome
		wantReason sessionLifecycleStatusReason
	}{
		{
			name: "converged",
			input: sessionLifecycleShadowInput{
				Info:            session.Info{ID: "session-converged", State: session.StateAsleep, MetadataState: string(session.StateAsleep)},
				RuntimeObserved: true,
				ObservedAt:      now,
			},
			want:       sessionLifecycleStatusNoop,
			wantReason: sessionLifecycleStatusReasonConverged,
		},
		{
			name: "heal",
			input: sessionLifecycleShadowInput{
				Info: session.Info{
					ID:                "session-heal",
					State:             session.StateAwake,
					MetadataState:     string(session.StateAwake),
					SessionKey:        "resume-key",
					StartedConfigHash: "config-hash",
					CreatedAt:         now.Add(-time.Hour),
				},
				RuntimeObserved: true,
				RuntimeAlive:    false,
				ObservedAt:      now,
			},
			want:       sessionLifecycleStatusHeal,
			wantReason: sessionLifecycleStatusReasonHeal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planSessionLifecycleStatus(tt.input)
			if plan.Outcome != tt.want || plan.Reason != tt.wantReason {
				t.Fatalf("plan = %+v, want outcome=%v reason=%v", plan, tt.want, tt.wantReason)
			}
			wantPatch := healStatePatchWithRollbackInfo(
				tt.input.Info,
				tt.input.RuntimeAlive,
				tt.input.RuntimeObserved,
				&clock.Fake{Time: tt.input.ObservedAt},
				tt.input.StartupTimeout,
				tt.input.RollbackAvailable,
			)
			if !maps.Equal(plan.Patch, wantPatch) {
				t.Fatalf("plan patch = %#v, legacy derivation = %#v", plan.Patch, wantPatch)
			}
			if tt.want == sessionLifecycleStatusHeal && len(plan.Patch) == 0 {
				t.Fatal("heal fixture produced an empty patch")
			}
		})
	}
}

func TestPlanSessionLifecycleStatusFailsClosedWithoutRuntimeFacts(t *testing.T) {
	now := time.Date(2026, 7, 26, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		input      sessionLifecycleShadowInput
		want       sessionLifecycleStatusOutcome
		wantReason sessionLifecycleStatusReason
	}{
		{
			name: "terminal wins before runtime",
			input: sessionLifecycleShadowInput{
				Info: session.Info{ID: "session-closed", Closed: true},
			},
			want:       sessionLifecycleStatusNoop,
			wantReason: sessionLifecycleStatusReasonTerminal,
		},
		{
			name: "runtime unobserved",
			input: sessionLifecycleShadowInput{
				Info:       session.Info{ID: "session-unobserved"},
				ObservedAt: now,
			},
			want:       sessionLifecycleStatusPark,
			wantReason: sessionLifecycleStatusReasonRuntimeUnknown,
		},
		{
			name: "observation time missing",
			input: sessionLifecycleShadowInput{
				Info:            session.Info{ID: "session-no-time"},
				RuntimeObserved: true,
			},
			want:       sessionLifecycleStatusPark,
			wantReason: sessionLifecycleStatusReasonObservationUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planSessionLifecycleStatus(tt.input)
			if plan.Outcome != tt.want || plan.Reason != tt.wantReason || plan.Patch != nil {
				t.Fatalf("plan = %+v, want outcome=%v reason=%v and no patch", plan, tt.want, tt.wantReason)
			}
		})
	}
}

func TestPlanSessionLifecycleStatusReturnsDetachedPatch(t *testing.T) {
	now := time.Date(2026, 7, 26, 3, 0, 0, 0, time.UTC)
	input := sessionLifecycleShadowInput{
		Info: session.Info{
			ID:            "session-heal",
			State:         session.StateAwake,
			MetadataState: string(session.StateAwake),
			SessionKey:    "resume-key",
		},
		RuntimeObserved: true,
		ObservedAt:      now,
	}

	first := planSessionLifecycleStatus(input)
	if first.Outcome != sessionLifecycleStatusHeal {
		t.Fatalf("first plan = %+v, want heal", first)
	}
	first.Patch["state"] = "corrupt"
	second := planSessionLifecycleStatus(input)
	if second.Patch["state"] == "corrupt" {
		t.Fatal("mutating a returned plan changed the next derivation")
	}
}
