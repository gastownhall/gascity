package main

import (
	"testing"
	"time"
)

// TestAwakeDecisionStanding pins AwakeDecision.Standing (ga-z4joq0, #7680):
// true exactly for a wake configuration alone produces (named-always,
// min-active, pin, and reset-pending in place of one of those or of no
// reason) with no assigned work and no named, routed or work-query demand
// for the identity; false for every other reason.
func TestAwakeDecisionStanding(t *testing.T) {
	always := []AwakeNamedSession{{Identity: "rig/patrol", Template: "rig/patrol", Mode: "always"}}
	onDemand := []AwakeNamedSession{{Identity: "rig/patrol", Template: "rig/patrol", Mode: "on_demand"}}
	patrolAgent := []AwakeAgent{{QualifiedName: "rig/patrol"}}
	patrolBead := func(mut func(*AwakeSessionBead)) []AwakeSessionBead {
		b := AwakeSessionBead{ID: "mc-1", SessionName: "rig--patrol", Template: "rig/patrol", State: "asleep", NamedIdentity: "rig/patrol"}
		if mut != nil {
			mut(&b)
		}
		return []AwakeSessionBead{b}
	}
	poolBead := func(mut func(*AwakeSessionBead)) []AwakeSessionBead {
		b := AwakeSessionBead{ID: "mc-2", SessionName: "rig--worker-1", Template: "rig/worker", State: "asleep"}
		if mut != nil {
			mut(&b)
		}
		return []AwakeSessionBead{b}
	}
	worker := []AwakeAgent{{QualifiedName: "rig/worker"}}

	cases := []struct {
		name         string
		input        AwakeInput
		session      string
		wantReason   string
		wantStanding bool
	}{
		{
			name:         "named-always without demand",
			input:        AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil)},
			session:      "rig--patrol",
			wantReason:   "named-always",
			wantStanding: true,
		},
		{
			name:       "named-always with named demand",
			input:      AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil), NamedSessionDemand: map[string]bool{"rig/patrol": true}},
			session:    "rig--patrol",
			wantReason: "named-always",
		},
		{
			name:       "named-always with routed demand",
			input:      AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil), NamedSessionRoutedDemand: map[string]bool{"rig/patrol": true}},
			session:    "rig--patrol",
			wantReason: "named-always",
		},
		{
			name:       "named-always with work-query demand",
			input:      AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil), NamedSessionWorkQ: map[string]bool{"rig/patrol": true}},
			session:    "rig--patrol",
			wantReason: "named-always",
		},
		{
			name: "named-always with assigned work",
			input: AwakeInput{
				Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil),
				WorkBeads: []AwakeWorkBead{{ID: "w-1", Assignee: "rig--patrol", Status: "in_progress"}},
			},
			session:    "rig--patrol",
			wantReason: "assigned-work",
		},
		{
			name:       "named-always without a row: identity demand",
			input:      AwakeInput{Agents: patrolAgent, NamedSessions: always, NamedSessionRoutedDemand: map[string]bool{"rig/patrol": true}, SessionBeads: []AwakeSessionBead{{ID: "mc-9", SessionName: "rig/patrol", Template: "rig/patrol", State: "asleep"}}},
			session:    "rig/patrol",
			wantReason: "named-always",
		},
		{
			name: "min-active",
			input: AwakeInput{
				Agents:       []AwakeAgent{{QualifiedName: "rig/worker", MinActiveSessions: 1}},
				SessionBeads: poolBead(func(b *AwakeSessionBead) { b.SleepReason = "city-stop" }),
			},
			session:      "rig--worker-1",
			wantReason:   "min-active",
			wantStanding: true,
		},
		{
			name:         "pin",
			input:        AwakeInput{Agents: worker, SessionBeads: poolBead(func(b *AwakeSessionBead) { b.Pinned = true })},
			session:      "rig--worker-1",
			wantReason:   "pin",
			wantStanding: true,
		},
		{
			name:         "reset-pending that displaced no reason",
			input:        AwakeInput{Agents: worker, SessionBeads: poolBead(func(b *AwakeSessionBead) { b.ContinuationResetPending = true })},
			session:      "rig--worker-1",
			wantReason:   "reset-pending",
			wantStanding: true,
		},
		{
			name:         "reset-pending that displaced named-always",
			input:        AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(func(b *AwakeSessionBead) { b.ContinuationResetPending = true })},
			session:      "rig--patrol",
			wantReason:   "reset-pending",
			wantStanding: true,
		},
		{
			name: "reset-pending that displaced routed-demand",
			input: AwakeInput{
				Agents: patrolAgent, NamedSessions: onDemand, NamedSessionRoutedDemand: map[string]bool{"rig/patrol": true},
				SessionBeads: patrolBead(func(b *AwakeSessionBead) { b.ContinuationResetPending = true }),
			},
			session:    "rig--patrol",
			wantReason: "reset-pending",
		},
		{
			name: "reset-pending that displaced manual",
			input: AwakeInput{Agents: worker, SessionBeads: poolBead(func(b *AwakeSessionBead) {
				b.ManualSession, b.ContinuationResetPending = true, true
			})},
			session:    "rig--worker-1",
			wantReason: "reset-pending",
		},
		{
			name:         "pending-create row of a named-always identity without demand",
			input:        AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(func(b *AwakeSessionBead) { b.PendingCreate = true; b.State = "creating" })},
			session:      "rig--patrol",
			wantReason:   "named-always",
			wantStanding: true,
		},
		{
			name:       "pending-create pool session",
			input:      AwakeInput{Agents: worker, SessionBeads: poolBead(func(b *AwakeSessionBead) { b.PendingCreate = true; b.State = "creating" })},
			session:    "rig--worker-1",
			wantReason: "pending-create",
		},
		{
			name: "explicit-wake",
			input: AwakeInput{Agents: worker, SessionBeads: poolBead(func(b *AwakeSessionBead) {
				b.ExplicitWake, b.ExplicitWakeStartable = true, true
			})},
			session:    "rig--worker-1",
			wantReason: "explicit-wake",
		},
		{
			name:       "manual",
			input:      AwakeInput{Agents: worker, SessionBeads: poolBead(func(b *AwakeSessionBead) { b.ManualSession = true })},
			session:    "rig--worker-1",
			wantReason: "manual",
		},
		{
			name: "routed-demand",
			input: AwakeInput{
				Agents: patrolAgent, NamedSessions: onDemand, NamedSessionRoutedDemand: map[string]bool{"rig/patrol": true},
				SessionBeads: patrolBead(nil),
			},
			session:    "rig--patrol",
			wantReason: "routed-demand",
		},
		{
			name:       "attached override of named-always",
			input:      AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil), AttachedSessions: map[string]bool{"rig--patrol": true}},
			session:    "rig--patrol",
			wantReason: "attached",
		},
		{
			name:       "pending override of named-always",
			input:      AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil), PendingSessions: map[string]bool{"rig--patrol": true}},
			session:    "rig--patrol",
			wantReason: "pending",
		},
		{
			name:       "wait-ready override of named-always",
			input:      AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(nil), ReadyWaitSet: map[string]bool{"mc-1": true}},
			session:    "rig--patrol",
			wantReason: "wait-ready",
		},
		{
			name: "held named-always does not wake",
			input: AwakeInput{Agents: patrolAgent, NamedSessions: always, SessionBeads: patrolBead(func(b *AwakeSessionBead) {
				b.HeldUntil = now.Add(time.Hour)
			})},
			session:    "rig--patrol",
			wantReason: "held",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.input.Now = now
			result := computeAwakeSetChecked(t, tc.input)
			d, ok := result[tc.session]
			if !ok {
				t.Fatalf("no decision for %q in %+v", tc.session, result)
			}
			if d.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q (decision %+v)", d.Reason, tc.wantReason, d)
			}
			if d.Standing != tc.wantStanding {
				t.Fatalf("Standing = %v, want %v (decision %+v)", d.Standing, tc.wantStanding, d)
			}
		})
	}
}

// TestStandingWakeReasonsAreAClosedAllowlist pins standingWake's rule for
// reasons the awake-set fixtures above cannot produce: every reason off the
// allowlist, including one added later, is demand, and demand overrides a
// standing reason.
func TestStandingWakeReasonsAreAClosedAllowlist(t *testing.T) {
	for _, reason := range []string{"named-always", "min-active", "pin"} {
		if !standingWake(reason, "", false, false) {
			t.Errorf("standingWake(%q) = false, want true", reason)
		}
		if standingWake(reason, "", true, false) || standingWake(reason, "", false, true) {
			t.Errorf("standingWake(%q) with work or named demand = true, want false", reason)
		}
	}
	for _, reason := range []string{
		"pending-create", "explicit-wake", "manual", "assigned-work", "routed-demand", "named-demand",
		"work-query", "scaled:demand", "scaled:creating", "attached", "pending", "wait-ready",
		"on-demand:running", "some-future-reason", "",
	} {
		if standingWake(reason, "", false, false) {
			t.Errorf("standingWake(%q) = true, want false (demand)", reason)
		}
	}
	for displaced, want := range map[string]bool{
		"": true, "named-always": true, "min-active": true, "pin": true,
		"scaled:demand": false, "manual": false, "routed-demand": false, "some-future-reason": false,
	} {
		if got := standingWake("reset-pending", displaced, false, false); got != want {
			t.Errorf("standingWake(reset-pending, displaced %q) = %v, want %v", displaced, got, want)
		}
	}
}
