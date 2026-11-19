package main

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The awake set classifies each wake once (#7680): Standing is true exactly
// for wakes that configuration alone keeps up — named mode "always", a
// min_active_sessions floor, a pin, or a continuation reset that displaced no
// reason or another standing one — and only when the session has no assigned
// work and no named, routed or work-query demand for its identity. Every
// other wake is demand, so the per-tick wake budget starts it first.

// assertStanding asserts the wake decision's Standing classification.
func assertStanding(t *testing.T, result map[string]AwakeDecision, sessionName string, want bool) {
	t.Helper()
	d, ok := result[sessionName]
	if !ok {
		t.Errorf("session %q not in result", sessionName)
		return
	}
	if !d.ShouldWake {
		t.Errorf("session %q should be awake but isn't (reason: %s)", sessionName, d.Reason)
		return
	}
	if d.Standing != want {
		t.Errorf("session %q (reason %q) Standing = %v, want %v", sessionName, d.Reason, d.Standing, want)
	}
}

// TestStandingWakeClassification pins the closed allowlist: a wake reason is
// standing only for the four configuration reasons, only without work or
// demand, and never by default.
func TestStandingWakeClassification(t *testing.T) {
	cases := []struct {
		name            string
		reason          string
		displacedReason string
		hasAssignedWork bool
		namedDemand     bool
		want            bool
	}{
		{name: "named-always", reason: "named-always", want: true},
		{name: "min-active", reason: "min-active", want: true},
		{name: "pin", reason: "pin", want: true},
		{name: "reset-pending displaced nothing", reason: "reset-pending", want: true},
		{name: "reset-pending displaced standing named-always", reason: "reset-pending", displacedReason: "named-always", want: true},
		{name: "reset-pending displaced standing min-active", reason: "reset-pending", displacedReason: "min-active", want: true},
		{name: "reset-pending displaced standing pin", reason: "reset-pending", displacedReason: "pin", want: true},
		{name: "reset-pending displaced demand manual", reason: "reset-pending", displacedReason: "manual", want: false},
		{name: "reset-pending displaced demand pending-create", reason: "reset-pending", displacedReason: "pending-create", want: false},
		{name: "reset-pending displaced demand assigned-work", reason: "reset-pending", displacedReason: "assigned-work", want: false},
		{name: "named-always with assigned work", reason: "named-always", hasAssignedWork: true, want: false},
		{name: "named-always with named demand", reason: "named-always", namedDemand: true, want: false},
		{name: "pending-create", reason: "pending-create", want: false},
		{name: "explicit-wake", reason: "explicit-wake", want: false},
		{name: "manual", reason: "manual", want: false},
		{name: "assigned-work", reason: "assigned-work", want: false},
		{name: "routed-demand", reason: "routed-demand", want: false},
		{name: "named-demand", reason: "named-demand", want: false},
		{name: "work-query", reason: "work-query", want: false},
		{name: "scaled demand", reason: "scaled:demand", want: false},
		{name: "scaled creating", reason: "scaled:creating", want: false},
		{name: "on-demand running", reason: "on-demand:running", want: false},
		{name: "attached", reason: "attached", want: false},
		{name: "pending", reason: "pending", want: false},
		{name: "wait-ready", reason: "wait-ready", want: false},
		// The allowlist is closed: a reason added later is demand by
		// default and can never be starved by accident.
		{name: "unknown reason", reason: "some-new-reason", want: false},
	}
	for _, tc := range cases {
		if got := standingWake(tc.reason, tc.displacedReason, tc.hasAssignedWork, tc.namedDemand); got != tc.want {
			t.Errorf("%s: standingWake(%q, %q, work=%v, demand=%v) = %v, want %v",
				tc.name, tc.reason, tc.displacedReason, tc.hasAssignedWork, tc.namedDemand, got, tc.want)
		}
	}
}

func TestStandingNamedAlwaysNoDemand(t *testing.T) {
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep", NamedIdentity: "deacon"}},
		Now:           now,
	})
	assertStanding(t, result, "deacon", true)
}

// An always identity with named, routed or work-query demand is demand, not
// standing: its Reason stays "named-always", so the demand flag is what
// classifies it.
func TestStandingNamedAlwaysWithDemand(t *testing.T) {
	for name, demand := range map[string]func(*AwakeInput){
		"named demand":  func(in *AwakeInput) { in.NamedSessionDemand = map[string]bool{"refinery": true} },
		"routed demand": func(in *AwakeInput) { in.NamedSessionRoutedDemand = map[string]bool{"refinery": true} },
		"work query":    func(in *AwakeInput) { in.NamedSessionWorkQ = map[string]bool{"refinery": true} },
	} {
		t.Run(name, func(t *testing.T) {
			input := AwakeInput{
				Agents:        []AwakeAgent{{QualifiedName: "refinery"}},
				NamedSessions: []AwakeNamedSession{{Identity: "refinery", Template: "refinery", Mode: "always"}},
				SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "refinery", Template: "refinery", State: "asleep", NamedIdentity: "refinery"}},
				Now:           now,
			}
			demand(&input)
			result := computeAwakeSetChecked(t, input)
			assertStanding(t, result, "refinery", false)
		})
	}
}

// A min_active_sessions floor over a city-stopped pool bead is standing.
func TestStandingMinActive(t *testing.T) {
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "polecat", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep", SleepReason: string(sessionpkg.SleepReasonCityStop)}},
		Now:          now,
	})
	assertStanding(t, result, "polecat-1", true)
	assertReason(t, result, "polecat-1", "min-active")
}

// A durable pin is standing.
func TestStandingPin(t *testing.T) {
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep", Pinned: true}},
		Now:          now,
	})
	assertStanding(t, result, "polecat-1", true)
	assertReason(t, result, "polecat-1", "pin")
}

// A continuation reset that displaced no reason is standing.
func TestStandingResetPendingDisplacingNothing(t *testing.T) {
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep", ContinuationResetPending: true}},
		Now:          now,
	})
	assertStanding(t, result, "polecat-1", true)
	assertReason(t, result, "polecat-1", "reset-pending")
}

// A continuation reset that displaced a demand reason (manual) keeps that
// reason's demand class.
func TestStandingResetPendingDisplacingDemand(t *testing.T) {
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
		SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep", ManualSession: true, ContinuationResetPending: true}},
		Now:          now,
	})
	assertStanding(t, result, "polecat-1", false)
	assertReason(t, result, "polecat-1", "reset-pending")
}

// A continuation reset that displaced a standing reason (named-always) stays
// standing.
func TestStandingResetPendingDisplacingNamedAlways(t *testing.T) {
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "deacon"}},
		NamedSessions: []AwakeNamedSession{{Identity: "deacon", Template: "deacon", Mode: "always"}},
		SessionBeads:  []AwakeSessionBead{{ID: "mc-1", SessionName: "deacon", Template: "deacon", State: "asleep", NamedIdentity: "deacon", ContinuationResetPending: true}},
		Now:           now,
	})
	assertStanding(t, result, "deacon", true)
	assertReason(t, result, "deacon", "reset-pending")
}

// Every demand reason classifies as demand end to end.
func TestStandingDemandReasons(t *testing.T) {
	t.Run("pending create", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
			SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep", PendingCreate: true}},
			Now:          now,
		})
		assertStanding(t, result, "polecat-1", false)
	})
	t.Run("explicit wake", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
			SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep", ExplicitWake: true, ExplicitWakeStartable: true}},
			Now:          now,
		})
		assertStanding(t, result, "polecat-1", false)
	})
	t.Run("manual", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
			SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep", ManualSession: true}},
			Now:          now,
		})
		assertStanding(t, result, "polecat-1", false)
	})
	t.Run("attached", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:           []AwakeAgent{{QualifiedName: "polecat"}},
			SessionBeads:     []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep"}},
			AttachedSessions: map[string]bool{"polecat-1": true},
			Now:              now,
		})
		assertStanding(t, result, "polecat-1", false)
	})
	t.Run("pending interaction", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:          []AwakeAgent{{QualifiedName: "polecat"}},
			SessionBeads:    []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep"}},
			PendingSessions: map[string]bool{"polecat-1": true},
			Now:             now,
		})
		assertStanding(t, result, "polecat-1", false)
	})
	t.Run("wait ready", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
			SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep"}},
			ReadyWaitSet: map[string]bool{"mc-1": true},
			Now:          now,
		})
		assertStanding(t, result, "polecat-1", false)
	})
	t.Run("on demand named session demand", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:             []AwakeAgent{{QualifiedName: "refinery"}},
			NamedSessions:      []AwakeNamedSession{{Identity: "refinery", Template: "refinery", Mode: "on_demand"}},
			SessionBeads:       []AwakeSessionBead{{ID: "mc-1", SessionName: "refinery", Template: "refinery", State: "asleep", NamedIdentity: "refinery"}},
			NamedSessionDemand: map[string]bool{"refinery": true},
			Now:                now,
		})
		assertStanding(t, result, "refinery", false)
		assertReason(t, result, "refinery", "named-demand")
	})
	t.Run("on demand named session routed demand", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:                   []AwakeAgent{{QualifiedName: "refinery"}},
			NamedSessions:            []AwakeNamedSession{{Identity: "refinery", Template: "refinery", Mode: "on_demand"}},
			SessionBeads:             []AwakeSessionBead{{ID: "mc-1", SessionName: "refinery", Template: "refinery", State: "asleep", NamedIdentity: "refinery"}},
			NamedSessionRoutedDemand: map[string]bool{"refinery": true},
			Now:                      now,
		})
		assertStanding(t, result, "refinery", false)
		assertReason(t, result, "refinery", "routed-demand")
	})
	t.Run("assigned work", func(t *testing.T) {
		result := computeAwakeSetChecked(t, AwakeInput{
			Agents:       []AwakeAgent{{QualifiedName: "polecat"}},
			SessionBeads: []AwakeSessionBead{{ID: "mc-1", SessionName: "polecat-1", Template: "polecat", State: "asleep"}},
			WorkBeads:    []AwakeWorkBead{{ID: "work-1", Assignee: "polecat-1", Status: "open", Ready: true}},
			Now:          now,
		})
		assertStanding(t, result, "polecat-1", false)
		assertReason(t, result, "polecat-1", "assigned-work")
	})
}
