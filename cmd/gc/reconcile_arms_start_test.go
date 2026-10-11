package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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
// heal; an operator's hold (a hold past a heartbeat's horizon included), no
// work, or a quarantine leaves it to A6's heal, and a drain to its drain.
func TestDecideRowHeartbeatRespawn(t *testing.T) {
	soon := gatherNow.Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name    string
		meta    []string
		work    bool
		respawn bool
	}{
		{"heartbeat hold with work", []string{"held_until", soon}, true, true},
		{"a heartbeat's longest hold", []string{"held_until", gatherNow.Add(maximumHeartbeatDuration).Format(time.RFC3339)}, true, true},
		{"an operator's hold", []string{"held_until", soon, "sleep_intent", "user-hold"}, true, false},
		{"an operator's hold whose intent a blind write cleared", []string{"held_until", gatherNow.AddDate(100, 0, 0).Format(time.RFC3339)}, true, false},
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
	drain := newHealCase(t, livenessGone, desireDrain, "state", "active", "held_until", soon)
	drain.a.Snapshot.Entries[drain.k].AssignedWork = &assignedWorkView{BeadID: "gw-1", Claimed: true}
	if it := drain.decide(); it.Reason == decideHeartbeatRespawn {
		t.Fatalf("decideRow = (%q, %q), want no respawn of a row AL1 drains", it.Kind, it.Reason)
	}
}

// Kills a dead seat's death accrual lost, miscounted, or restarting the
// pass legacy records it (SESS-537/538): a death inside the stability
// threshold accrues a wake failure, quarantining at the limit; one before the
// productivity threshold accrues churn and resets the conversation; each
// clears last_woke_at and restarts the next pass; a productive death clears
// churn and restarts in the same pass, as does a death in a drain the row
// asked for, or a subprocess worker's exit, which accrue nothing.
func TestDeadRuntimeAccrualMatchesLegacy(t *testing.T) {
	woke := func(ago time.Duration) string { return gatherNow.Add(-ago).Format(time.RFC3339) }
	for _, tc := range []struct {
		name     string
		meta     []string
		want     map[string]string // keys the dead-runtime patch must carry
		sameTick bool              // the start lands in this pass
		kept     string            // a key the patch must not write
	}{
		{"a rapid crash", []string{"last_woke_at", woke(10 * time.Second), "wake_attempts", "1"}, map[string]string{"wake_attempts": "2", "last_woke_at": ""}, false, ""},
		{"a rapid crash at the limit", []string{"last_woke_at", woke(10 * time.Second), "wake_attempts", "4"}, map[string]string{"wake_attempts": "5", "sleep_reason": "quarantine"}, false, ""},
		{"a rapid crash of a named always codex row, whose heal keeps the conversation", []string{"last_woke_at", woke(10 * time.Second), "provider_kind", "codex", "configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always"}, map[string]string{"wake_attempts": "1", "session_key": "", "started_config_hash": "", "continuation_reset_pending": "true"}, false, ""},
		{"churn", []string{"last_woke_at", woke(2 * time.Minute), "churn_count", "1"}, map[string]string{"churn_count": "2", "last_woke_at": "", "continuation_reset_pending": "true"}, false, ""},
		{"a rapid crash of a named always claude row keeps the conversation for its probe", []string{"last_woke_at", woke(10 * time.Second), "provider_kind", "claude", "configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always"}, map[string]string{"wake_attempts": "1"}, false, "session_key"},
		{"a death in churn's window of a start not yet committed", []string{"last_woke_at", woke(2 * time.Minute), "churn_count", "1", "pending_create_started_at", woke(3 * time.Minute)}, map[string]string{"state": "asleep"}, true, "churn_count"},
		{"churn on a named always row, whose heal keeps the conversation", []string{"last_woke_at", woke(2 * time.Minute), "configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always"}, map[string]string{"churn_count": "1", "session_key": "", "continuation_reset_pending": "true"}, false, ""},
		{"a productive death", []string{"last_woke_at", woke(10 * time.Minute), "churn_count", "2"}, map[string]string{"churn_count": "0", "state": "asleep"}, true, ""},
		{"a drain it asked for", []string{"last_woke_at", woke(10 * time.Second), "drain_intent_reason", "idle", "drain_intent_at", woke(time.Minute), "drain_intent_incarnation", "3"}, map[string]string{"state": "asleep"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, desireWake, append([]string{"state", "active", "session_key", "k-1", "started_config_hash", "h"}, tc.meta...)...)
			it := c.decide()
			if started := it.Kind == intentStart; started != tc.sameTick {
				t.Fatalf("decideRow = (%q, %q), want a same-pass start %v", it.Kind, it.Reason, tc.sameTick)
			}
			if _, wrote := it.Patch[tc.kept]; tc.kept != "" && wrote {
				t.Fatalf("patch %v writes %s, which the PreWake's transcript probe owns", it.Patch, tc.kept)
			}
			for k, v := range tc.want {
				if got, ok := it.Patch[k]; !ok || got != v {
					t.Errorf("patch %s = %q (set %v), want %q; patch %v", k, got, ok, v, it.Patch)
				}
			}
		})
	}
	// A subprocess worker exits after its unit of work: no crash, no churn.
	c := newHealCase(t, livenessGone, desireWake, "state", "active", "session_key", "k-1", "last_woke_at", woke(10*time.Second))
	c.w.Env = &reconcileEnv{Cfg: &config.City{Session: config.SessionConfig{Provider: "subprocess"}}}
	if it := c.decide(); it.Kind != intentStart || it.Patch["wake_attempts"] != "" {
		t.Fatalf("subprocess exit: decideRow = (%q, %q, %v), want a same-pass start with no accrual", it.Kind, it.Reason, it.Patch)
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
	// The heal the start carries for when it cannot run lands as its own
	// row write, though the fresh decision composes the start again.
	p, it := c.pass(t, runtime.NewFake(), nil)
	if it.Heal == nil || it.Heal.Kind != intentRowHeal {
		t.Fatalf("start carries heal %+v, want its A6 row heal", it.Heal)
	}
	if s := runTx(context.Background(), p, *it.Heal, effectSpecs[it.Heal.Kind], nil); s.Outcome != settledLanded || c.meta(t)["held_until"] != "" {
		t.Fatalf("heal settled %+v with held_until %q, want it landed and cleared", s, c.meta(t)["held_until"])
	}
}

// Kills a persistent pool seat's young death left to a next pass that never
// comes (staging J23): AL1 never wakes such a seat asleep, so its accruing
// death restarts it in the same pass, carrying the accrual, and its fallback
// records the death alone rather than strand it asleep; a seat with its work assigned
// and a one-shot pool's, which AL1 wakes asleep, and a named row record the
// death first, as legacy's.
func TestDeadRuntimeAccrualRestartsAStrandedSeatInPlace(t *testing.T) {
	woke := gatherNow.Add(-2 * time.Minute).Format(time.RFC3339)
	for _, tc := range []struct {
		name      string
		lifecycle string
		named     bool
		inPlace   bool
	}{
		{"a persistent pool seat", "", false, true},
		{"a persistent pool seat with its work assigned", "", false, false},
		{"a one-shot pool seat", config.AgentLifecycleOneShot, false, false},
		{"a named row", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := []string{"state", "active", "session_key", "k-1", "last_woke_at", woke, "pool_managed", "true", "pool_slot", "1"}
			if tc.named {
				meta = append(meta, "configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always")
			}
			c := newHealCase(t, livenessGone, desireWake, meta...)
			c.w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1), Lifecycle: tc.lifecycle}}}}
			c.a.Snapshot.Entries[c.k].Template = "worker"
			if strings.HasSuffix(tc.name, "assigned") {
				c.a.Snapshot.Entries[c.k].AssignedWork = &assignedWorkView{BeadID: "gw-1", Claimed: true}
			}
			it := c.decide()
			if got := it.Kind == intentStart; got != tc.inPlace {
				t.Fatalf("decideRow = (%q, %q), want a same-pass restart %v", it.Kind, it.Reason, tc.inPlace)
			}
			if tc.inPlace && (it.Patch["churn_count"] != "1" || it.Heal == nil || it.Heal.Patch["churn_count"] != "1" || it.Heal.Patch["state"] != "") {
				t.Fatalf("restart patch %v heal %+v, want the churn accrued and a fallback recording it alone", it.Patch, it.Heal)
			}
		})
	}
}

// strandedCase is a persistent singleton pool seat with no work assigned,
// committed, its runtime gone, woken ago before the pass.
func strandedCase(t *testing.T, ago time.Duration, kv ...string) *healCase {
	t.Helper()
	meta := append([]string{"state", "active", "session_key", "k-1", "last_woke_at", gatherNow.Add(-ago).Format(time.RFC3339), "pool_managed", "true", "pool_slot", "1"}, kv...)
	c := newHealCase(t, livenessGone, desireWake, meta...)
	c.w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1)}}}}
	c.a.Snapshot.Entries[c.k].Template = "worker"
	return c
}

// Kills a stranded seat's death recorded only by its start (review J1):
// while the start cannot run, the death's class drifts with the pass clock
// (a rapid crash turns churn at 30s, a productive death at 5m). Its start
// carries the accrual alone, the row left committed, and once that lands
// later passes record nothing more.
func TestStrandedDeathIsRecordedOnce(t *testing.T) {
	c := strandedCase(t, 10*time.Second, "wake_attempts", "2", "churn_count", "2")
	it := c.decide()
	if it.Kind != intentStart || it.Heal == nil {
		t.Fatalf("decideRow = (%q, %q) heal %+v, want a start carrying the death's record", it.Kind, it.Reason, it.Heal)
	}
	rec := it.Heal.Patch
	if _, healed := rec["state"]; healed || rec["wake_attempts"] != "3" || rec["last_woke_at"] != "" {
		t.Fatalf("fallback %v, want the wake failure alone, last_woke_at cleared, the row left committed", rec)
	}
	if err := c.store.SetMetadataBatch(c.k.ID, rec); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{40 * time.Second, 6 * time.Minute} {
		c2 := strandedCase(t, 10*time.Second)
		if err := c2.store.SetMetadataBatch(c2.k.ID, c.meta(t)); err != nil {
			t.Fatal(err)
		}
		c2.w.Census = readCensus(t, gatherNow, censusLegs(rowLeg, c2.store))
		c2.w.Now = gatherNow.Add(at)
		it := c2.decide()
		if it.Kind != intentStart || it.Heal != nil || it.Patch["churn_count"] != "" || it.Patch["wake_attempts"] != "" {
			t.Fatalf("+%v: decideRow = (%q, %q) patch %v heal %+v, want a plain restart recording nothing more", at, it.Kind, it.Reason, it.Patch, it.Heal)
		}
	}
}

// Kills a stranded seat started past its own quarantine (review J2, D18):
// the accrual that quarantines holds the start, and the heal lands with it.
func TestStrandedQuarantineHoldsTheStart(t *testing.T) {
	it := strandedCase(t, 10*time.Second, "wake_attempts", "4").decide()
	if it.Kind == intentStart || it.Patch["quarantined_until"] == "" || it.Patch["state"] != "asleep" {
		t.Fatalf("decideRow = (%q, %q) patch %v, want the heal with its quarantine and no start", it.Kind, it.Reason, it.Patch)
	}
}

// Pins C5c1's leftover (review J3): a red provider holds a stranded seat's
// restart, so A6 heals it asleep, where it stays until A21 closes it.
func TestStrandedSeatUnderARedProviderHealsAsleep(t *testing.T) {
	c := strandedCase(t, 10*time.Minute)
	c.w.ProviderHealth = &providerHealthSnapshot{present: true, entries: map[string]bool{"claude": false}}
	tp := TemplateParams{TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{Name: "claude"}}
	c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(c.w.Census.Rows[c.k].Info): {TP: tp}}}
	if it := c.decide(); it.Kind != intentRowHealFresh || it.Patch["state"] != string(session.StateAsleep) {
		t.Fatalf("decideRow = (%q, %q) patch %v, want A6's heal asleep", it.Kind, it.Reason, it.Patch)
	}
}
