package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// These tests cover the min_active_sessions-aware wake path added for #2739:
// a city-scoped pool agent whose only instance is asleep with
// sleep_reason=city-stop must be revived so the always-warm guarantee
// (min_active_sessions) survives a gc stop && gc start, even with no work
// routed to it.

// TestMinActive_AsleepCityStopWakes is the canonical #2739 case: a pool agent
// configured min_active_sessions=1 whose only session is asleep(city-stop)
// with no assigned work must be woken to satisfy the min.
func TestMinActive_AsleepCityStopWakes(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "city-stop",
		}},
		Now: now,
	})
	assertAwake(t, result, "rig--pl")
	assertReason(t, result, "rig--pl", "min-active")
}

// TestMinActive_ActiveSatisfiesMin verifies a live (active) session counts
// toward the min, so a coexisting asleep city-stop bead is NOT additionally
// woken when min is already met.
func TestMinActive_ActiveSatisfiesMin(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-live", SessionName: "rig--pl-1", Template: "rig/pl", State: "active"},
			{ID: "s-cold", SessionName: "rig--pl-2", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl-2")
}

// TestMinActive_FillsDeficitAcrossMultiple verifies that when min=2 and one
// session is live, exactly one additional city-stop bead is revived (not more).
func TestMinActive_FillsDeficitAcrossMultiple(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 2}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-live", SessionName: "rig--pl-0", Template: "rig/pl", State: "active"},
			{ID: "s-a", SessionName: "rig--pl-a", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
			{ID: "s-b", SessionName: "rig--pl-b", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	woken := 0
	for _, name := range []string{"rig--pl-a", "rig--pl-b"} {
		if d, ok := result[name]; ok && d.ShouldWake {
			woken++
		}
	}
	if woken != 1 {
		t.Errorf("min-active deficit fill woke %d city-stop beads, want exactly 1 (live=1, min=2)", woken)
	}
}

// TestMinActive_OnlyCityStopReason verifies the wake is scoped to
// sleep_reason=city-stop: an idle-timeout asleep bead is NOT revived by the
// min-active pass (idle_timeout / wake_mode semantics are unchanged).
func TestMinActive_OnlyCityStopReason(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "idle-timeout",
		}},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl")
}

// TestMinActive_NotResleptByIdleSuppression verifies the min-active wake is
// exempt from idle-sleep suppression. A city-stop bead carries an old
// IdleSince (the detach time), and the agent has a sleep_after_idle; without
// the exemption the idle-sleep block would immediately flip the wake back to
// asleep, defeating the fix.
func TestMinActive_NotResleptByIdleSuppression(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1, SleepAfterIdle: time.Hour}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "city-stop",
			IdleSince: now.Add(-24 * time.Hour),
		}},
		Now: now,
	})
	assertAwake(t, result, "rig--pl")
	assertReason(t, result, "rig--pl", "min-active")
}

// TestMinActive_SuspendedAgentNoWake verifies a suspended agent's asleep
// city-stop bead is not revived.
func TestMinActive_SuspendedAgentNoWake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1, Suspended: true}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "city-stop",
		}},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl")
}

// TestMinActive_ZeroMinNoWake verifies that without min_active_sessions the
// asleep city-stop bead stays asleep (no behavior change for min=0 agents).
func TestMinActive_ZeroMinNoWake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 0}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "city-stop",
		}},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl")
}

// TestMinActive_NamedBeadNotCountedAsPool verifies the min-active pass only
// considers pool beads: a configured-named asleep city-stop bead is not
// revived by the pool min path (named sessions have their own keep-awake
// rules via the named-always / named-demand passes).
func TestMinActive_NamedBeadNotCountedAsPool(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "city-stop",
			NamedIdentity: "rig/pl", ConfiguredNamedSession: true,
		}},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl")
}

// TestBuildAwakeInputPropagatesMinActiveSessions verifies the reconciler
// bridge threads an agent's effective min_active_sessions into AwakeAgent so
// ComputeAwakeSet can honor the always-warm guarantee (#2739 wiring seam).
func TestBuildAwakeInputPropagatesMinActiveSessions(t *testing.T) {
	minSess := 1
	input := buildAwakeInputFromReconciler(
		&config.City{Agents: []config.Agent{{Name: "pl", MinActiveSessions: &minSess}}},
		"", // cityPath: empty exercises zero suspension state
		nil, nil, nil, nil, nil, nil, nil, nil,
		time.Now().UTC(),
	)
	var found bool
	for _, a := range input.Agents {
		if a.QualifiedName == "pl" {
			found = true
			if a.MinActiveSessions != 1 {
				t.Errorf("AwakeAgent.MinActiveSessions = %d, want 1", a.MinActiveSessions)
			}
		}
	}
	if !found {
		t.Fatalf("agent %q not present in AwakeInput.Agents", "pl")
	}
}

// TestMinActive_LegacyBoundTemplateRevivedThroughBridge verifies an adopted
// session bead persisted under a removed binding ("rig/gc.pl") still counts
// for — and is revived by — the current unbound agent's min_active_sessions
// guarantee. The bridge must canonicalize the stored template; with the raw
// value the min-active pass would neither count nor wake the adopted bead and
// the pool would stay cold after gc stop && gc start.
func TestMinActive_LegacyBoundTemplateRevivedThroughBridge(t *testing.T) {
	minSess := 1
	cfg := &config.City{
		Agents: []config.Agent{{Name: "pl", Dir: "rig", MinActiveSessions: &minSess}},
	}
	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]beads.Bead{{
			ID:     "s-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":        "stopped",
				"sleep_reason": "city-stop",
				"session_name": "rig--pl-legacy",
				"template":     "rig/gc.pl",
			},
		}},
		nil, nil, nil, nil, nil, nil, nil,
		time.Now().UTC(),
	)
	result := ComputeAwakeSet(input)
	assertAwake(t, result, "rig--pl-legacy")
	assertReason(t, result, "rig--pl-legacy", "min-active")
}

// TestMinActive_HeldBeadNotWoken verifies hold suppression still wins: a
// city-stop bead under an active hold is not revived by the min-active pass.
func TestMinActive_HeldBeadNotWoken(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "city-stop",
			HeldUntil: now.Add(time.Hour),
		}},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl")
}

// TestMinActive_HeldCandidateDoesNotConsumeCoverage verifies a held city-stop
// candidate does not consume the only min-active slot when an eligible sibling
// can satisfy the guarantee instead.
func TestMinActive_HeldCandidateDoesNotConsumeCoverage(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-a", SessionName: "rig--pl-held", Template: "rig/pl", State: "asleep", SleepReason: "city-stop", HeldUntil: now.Add(time.Hour)},
			{ID: "s-b", SessionName: "rig--pl-ready", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl-held")
	assertAwake(t, result, "rig--pl-ready")
	assertReason(t, result, "rig--pl-ready", "min-active")
}

// TestMinActive_QuarantinedCandidateDoesNotConsumeCoverage mirrors the held
// replacement case for crash-loop quarantine, which is also a hard blocker.
func TestMinActive_QuarantinedCandidateDoesNotConsumeCoverage(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-a", SessionName: "rig--pl-quarantined", Template: "rig/pl", State: "asleep", SleepReason: "city-stop", QuarantinedUntil: now.Add(time.Hour)},
			{ID: "s-b", SessionName: "rig--pl-ready", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl-quarantined")
	assertAwake(t, result, "rig--pl-ready")
	assertReason(t, result, "rig--pl-ready", "min-active")
}

// TestMinActive_WaitHoldCandidateDoesNotConsumeCoverage mirrors the held
// replacement case for a user-issued wait hold, which suppresses ordinary
// demand-driven wakes until the durable wait is ready.
func TestMinActive_WaitHoldCandidateDoesNotConsumeCoverage(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-a", SessionName: "rig--pl-waiting", Template: "rig/pl", State: "asleep", SleepReason: "city-stop", WaitHold: true},
			{ID: "s-b", SessionName: "rig--pl-ready", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl-waiting")
	assertAwake(t, result, "rig--pl-ready")
	assertReason(t, result, "rig--pl-ready", "min-active")
}

// TestMinActive_DependencyOnlyDoesNotCount verifies a dependency-only session
// does not satisfy the min_active_sessions guarantee even while live: it is
// excluded from the min-active pool (it wakes only via dependency gating), so
// a coexisting asleep city-stop pool bead must still be revived to cover the
// min. Without excluding dependency-only beads from the coverage count, the
// live dependency-only bead would mask the deficit and leave the pool cold.
func TestMinActive_DependencyOnlyDoesNotCount(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-dep", SessionName: "rig--pl-dep", Template: "rig/pl", State: "active", DependencyOnly: true},
			{ID: "s-cold", SessionName: "rig--pl-pool", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	assertAwake(t, result, "rig--pl-pool")
	assertReason(t, result, "rig--pl-pool", "min-active")
}

// TestMinActive_DependencyOnlyNotRevived verifies the min-active pass never
// revives a dependency-only bead: when the only city-stop pool candidate is
// dependency-only, the deficit cannot be filled by it (dependency-only
// sessions wake exclusively via dependency gating).
func TestMinActive_DependencyOnlyNotRevived(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{{
			ID: "s-1", SessionName: "rig--pl", Template: "rig/pl",
			State: "asleep", SleepReason: "city-stop", DependencyOnly: true,
		}},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl")
}

// TestMinActive_SuspendedDoesNotCount verifies that a non-live, non-asleep
// state (here: suspended) does NOT satisfy the min_active_sessions guarantee.
// Only active/creating beads count as covered; a suspended sibling must not
// mask the deficit, so the asleep city-stop pool bead is still revived.
func TestMinActive_SuspendedDoesNotCount(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-susp", SessionName: "rig--pl-susp", Template: "rig/pl", State: "suspended"},
			{ID: "s-cold", SessionName: "rig--pl-pool", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	assertAwake(t, result, "rig--pl-pool")
	assertReason(t, result, "rig--pl-pool", "min-active")
}

// TestMinActive_CreatingCounts verifies a creating bead counts as live toward
// the min (a session mid-spawn is on its way to active), so a coexisting
// asleep city-stop bead is not additionally woken.
func TestMinActive_CreatingCounts(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "rig/pl", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "s-new", SessionName: "rig--pl-1", Template: "rig/pl", State: "creating"},
			{ID: "s-cold", SessionName: "rig--pl-2", Template: "rig/pl", State: "asleep", SleepReason: "city-stop"},
		},
		Now: now,
	})
	assertAsleep(t, result, "rig--pl-2")
}
