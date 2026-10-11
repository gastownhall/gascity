package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The start arms' tests (CONTRACT v5 §4 A10, A18; plan C5b), over decideRow
// on one row heal (gc "heal", runtime s-heal, generation 3, token tok-3).

// Kills a start gate dropped or reordered, a start for a row that is not
// Wake or whose runtime is not gone or dead, and a start under unknown
// liveness (GUAR-053): each gate holds in legacy's order, a timed one asks
// for a pass at its end, and a passing row gets one bringUp at its endpoint,
// ranked by its last wake.
func TestDecideRowStartGates(t *testing.T) {
	woke := gatherNow.Add(-time.Hour).Format(time.RFC3339)
	soon := gatherNow.Add(time.Minute)
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		meta     []string
		episode  time.Time // the startup-health episode's quarantine end
		health   map[string]bool
		want     string
		wantKind string
		next     time.Time
	}{
		{name: "SESS-605 failed-create", liveness: livenessGone, desired: desireWake, meta: []string{"state", "failed-create"}, want: decideStartFailedCreate},
		{name: "SESS-605 before the quarantine", liveness: livenessGone, desired: desireWake, meta: []string{"state", "failed-create", "quarantined_until", soon.Format(time.RFC3339)}, want: decideStartFailedCreate, next: soon.Add(time.Second)}, // A6's pass at the quarantine's end
		{name: "SESS-606 quarantine", liveness: livenessGone, desired: desireWake, meta: []string{"quarantined_until", soon.Format(time.RFC3339)}, want: decideStartQuarantine, next: soon},
		{name: "SESS-606 before the episode", liveness: livenessGone, desired: desireWake, meta: []string{"quarantined_until", soon.Format(time.RFC3339)}, episode: soon.Add(time.Hour), want: decideStartQuarantine, next: soon},
		{name: "an expired quarantine heals in the start's CAS", liveness: livenessGone, desired: desireWake, meta: []string{"quarantined_until", woke}, want: decideStart, wantKind: intentStart},
		{name: "SESS-607 the startup-health episode (#46)", liveness: livenessGone, desired: desireWake, episode: soon, want: decideStartEpisode, next: soon},
		{name: "an expired episode starts", liveness: livenessGone, desired: desireWake, episode: gatherNow.Add(-time.Second), want: decideStart, wantKind: intentStart},
		{name: "SESS-610 a red provider", liveness: livenessGone, desired: desireWake, health: map[string]bool{"claude": false}, want: decideStartProviderRed},
		{name: "GUAR-034 unknown health fails open", liveness: livenessGone, desired: desireWake, health: map[string]bool{}, want: decideStart, wantKind: intentStart},
		{name: "a green provider starts", liveness: livenessGone, desired: desireWake, health: map[string]bool{"claude": true}, want: decideStart, wantKind: intentStart},
		{name: "a dead runtime starts", liveness: livenessDead, desired: desireWake, want: decideStart, wantKind: intentStart},
		{name: "an alive runtime does not", liveness: livenessAlive, desired: desireWake, want: decideNoAction},
		{name: "an occupied name does not", liveness: livenessOccupied, desired: desireWake, want: decideNoAction},
		{name: "GUAR-053 unknown liveness never starts", liveness: livenessUnknown, desired: desireWake, want: decideLivenessUnknown},
		{name: "Keep does not start", liveness: livenessGone, desired: desireKeep, want: decideNoAction},
		{name: "Sleep does not start", liveness: livenessGone, desired: desireSleep, want: decideNoAction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, tc.desired, append([]string{"state", "asleep", "last_woke_at", woke}, tc.meta...)...)
			e := c.a.Snapshot.Entries[c.k]
			e.Endpoint = "provider:claude"
			info := c.w.Census.Rows[c.k].Info
			if !tc.episode.IsZero() {
				c.w.Episodes = map[string]session.StartupHealthEpisode{startupHealthEpisodeKey(info, "s-heal"): {QuarantinedUntil: tc.episode}}
			}
			if tc.health != nil {
				c.w.ProviderHealth = &providerHealthSnapshot{present: true, entries: tc.health}
			}
			tp := TemplateParams{TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{Name: "claude"}}
			c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
			it, next := decideRow(c.w, c.a, c.k)
			if it.Reason != tc.want || it.Kind != tc.wantKind || !next.Equal(tc.next) {
				t.Fatalf("decideRow = (%q, %q, next %v), want (%q, %q, next %v)", it.Kind, it.Reason, next, tc.wantKind, tc.want, tc.next)
			}
			if it.Kind != intentStart {
				return
			}
			if it.Endpoint != "provider:claude" || it.Rank.Format(time.RFC3339) != woke || it.Basis != (rowBasis{Incarnation: 3, InstanceToken: "tok-3"}) || it.Rests != restRuntime {
				t.Fatalf("start %+v, want the row's endpoint, ranked by its last wake, at its basis, resting on the runtime", it)
			}
		})
	}
}

// Kills a second Launch over a runtime that is already up, and an adopt of
// a committed row or of a runtime that is not the row's own: a creating row
// whose own runtime reads alive and Current is adopted; a committed one,
// another token's runtime, or a creating row whose runtime is gone is not
// (a gone, wanted creating row launches on its own bead, S7).
func TestDecideRowAdoptsAliveUncommittedIntent(t *testing.T) {
	for _, tc := range []struct {
		name, state, token string
		desired            desire
		want, wantKind     string
	}{
		{"alive and Current", "creating", "tok-3", desireWake, decideAdopt, intentAdopt},
		{"a committed row", "active", "tok-3", desireWake, decideNoAction, ""},
		{"another token's runtime", "creating", "tok-9", desireWake, decideNoAction, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ownRuntimeCase(t, tc.token, "state", tc.state)
			c.a.Snapshot.Entries[c.k].Desired = tc.desired
			if it := c.decide(); it.Kind != tc.wantKind || it.Reason != tc.want {
				t.Fatalf("decideRow = (%q, %q), want (%q, %q)", it.Kind, it.Reason, tc.wantKind, tc.want)
			}
		})
	}
	c := newHealCase(t, livenessGone, desireWake, "state", "creating", "pending_create_claim", "true")
	if it := c.decide(); it.Kind != intentStart {
		t.Fatalf("a gone, wanted creating row: decideRow = (%q, %q), want a start on its own bead", it.Kind, it.Reason)
	}
}

// Kills A17 widened past the heartbeat hold or narrowed off it (SESS-602,
// v5.9i): a dead committed seat held only by an agent heartbeat, with
// assigned work, is respawned in the same pass carrying the dead-runtime
// heal; an operator's hold, no work, or a quarantine leaves it to A6's heal.
func TestDecideRowHeartbeatRespawn(t *testing.T) {
	soon := gatherNow.Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name    string
		meta    []string
		work    bool
		respawn bool
	}{
		{"heartbeat hold with work", []string{"held_until", soon}, true, true},
		{"an operator's hold", []string{"held_until", soon, "sleep_intent", "user-hold"}, true, false},
		{"no work", []string{"held_until", soon}, false, false},
		{"quarantined", []string{"held_until", soon, "quarantined_until", soon}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, desireSleep, append([]string{"state", "active", "session_key", "k-1", "started_config_hash", "h"}, tc.meta...)...)
			if tc.work {
				c.a.Snapshot.Entries[c.k].AssignedWork = &assignedWorkView{BeadID: "gw-1", Claimed: true}
			}
			it := c.decide()
			if got := it.Kind == intentStart && it.Reason == decideHeartbeatRespawn; got != tc.respawn {
				t.Fatalf("decideRow = (%q, %q), want a heartbeat respawn %v", it.Kind, it.Reason, tc.respawn)
			}
			if tc.respawn && it.Patch["state"] != string(session.StateAsleep) {
				t.Fatalf("respawn patch %v, want the dead-runtime heal folded in", it.Patch)
			}
			if !tc.respawn && it.Reason != decideDeadRuntimeHeal {
				t.Fatalf("decideRow = (%q, %q), want A6's dead-runtime heal", it.Kind, it.Reason)
			}
		})
	}
}

// Kills a dead seat's death accrual lost, miscounted, or restarting the
// pass legacy records it (SESS-537/538): a death inside the stability
// threshold accrues a wake failure, quarantining at the limit; one before the
// productivity threshold accrues churn and resets the conversation; each
// clears last_woke_at and restarts the next pass; a productive death clears
// churn and restarts in the same pass, as does a death in a drain the row
// asked for, which accrues nothing.
func TestDeadRuntimeAccrualMatchesLegacy(t *testing.T) {
	woke := func(ago time.Duration) string { return gatherNow.Add(-ago).Format(time.RFC3339) }
	for _, tc := range []struct {
		name     string
		meta     []string
		want     map[string]string // keys the dead-runtime patch must carry
		sameTick bool              // the start lands in this pass
	}{
		{"a rapid crash", []string{"last_woke_at", woke(10 * time.Second), "wake_attempts", "1"}, map[string]string{"wake_attempts": "2", "last_woke_at": ""}, false},
		{"a rapid crash at the limit", []string{"last_woke_at", woke(10 * time.Second), "wake_attempts", "4"}, map[string]string{"wake_attempts": "5", "sleep_reason": "quarantine"}, false},
		{"churn", []string{"last_woke_at", woke(2 * time.Minute), "churn_count", "1"}, map[string]string{"churn_count": "2", "last_woke_at": "", "continuation_reset_pending": "true"}, false},
		{"churn on a named always row, whose heal keeps the conversation", []string{"last_woke_at", woke(2 * time.Minute), "configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always"}, map[string]string{"churn_count": "1", "session_key": "", "continuation_reset_pending": "true"}, false},
		{"a productive death", []string{"last_woke_at", woke(10 * time.Minute), "churn_count", "2"}, map[string]string{"churn_count": "0", "state": "asleep"}, true},
		{"a drain it asked for", []string{"last_woke_at", woke(10 * time.Second), "drain_intent_reason", "idle", "drain_intent_at", woke(time.Minute), "drain_intent_incarnation", "3"}, map[string]string{"state": "asleep"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, desireWake, append([]string{"state", "active", "session_key", "k-1", "started_config_hash", "h"}, tc.meta...)...)
			it := c.decide()
			if started := it.Kind == intentStart; started != tc.sameTick {
				t.Fatalf("decideRow = (%q, %q), want a same-pass start %v", it.Kind, it.Reason, tc.sameTick)
			}
			for k, v := range tc.want {
				if got, ok := it.Patch[k]; !ok || got != v {
					t.Errorf("patch %s = %q (set %v), want %q; patch %v", k, got, ok, v, it.Patch)
				}
			}
		})
	}
}

// Kills a heal that pre-empts a wanted row's start by a pass: an expired
// hold on a wanted row whose runtime is gone heals in its start's PreWake,
// in the same pass, as legacy heals and wakes in one tick.
func TestTimerHealStartsAWantedRowInTheSamePass(t *testing.T) {
	c := newHealCase(t, livenessGone, desireWake, "state", "asleep", "held_until", gatherNow.Add(-time.Minute).Format(time.RFC3339), "sleep_reason", "user-hold")
	it := c.decide()
	if it.Kind != intentStart || it.Reason != decideStart || it.Patch["held_until"] != "" {
		t.Fatalf("decideRow = (%q, %q, %v), want a start carrying the timer heal", it.Kind, it.Reason, it.Patch)
	}
	if _, ok := it.Patch["held_until"]; !ok {
		t.Fatalf("patch %v, want the expired hold cleared in the start's CAS", it.Patch)
	}
}
