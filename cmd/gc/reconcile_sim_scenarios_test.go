package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// CONTRACT v5 §11 scenarios on the simulator, scripted step by step. D1a has
// those whose arms and effects have merged; D1b adds the rest as their PRs
// land. Each names its expected outcome and the invariants it exercises,
// which the hooks check after every step as in the corpus.

// scripted is a run over rows on the city leg, each live row with a runtime
// carrying its identity, with no inventory published yet and no lag.
func scripted(t *testing.T, live []string, rows ...beads.Bead) *sim {
	return scriptedWith(t, simOpts{}, live, rows...)
}

// scriptedWith is scripted with o's faults; o.rows is replaced.
func scriptedWith(t *testing.T, o simOpts, live []string, rows ...beads.Bead) *sim {
	// The store keeps the beads it is seeded with: a clone leaves the
	// caller's rows the seed, so comparing one after a write is not vacuous.
	rows = slices.Clone(rows)
	for i := range rows {
		rows[i].Metadata = maps.Clone(rows[i].Metadata)
	}
	o.rows = func(s *sim) ([]beads.Bead, []beads.Bead) {
		for _, b := range rows {
			for _, id := range live {
				if b.ID == id {
					s.sp.put(b.Metadata["session_name"], b.ID, b.Metadata["generation"], b.Metadata["instance_token"])
				}
			}
		}
		return rows, nil
	}
	s := newSim(t, 1, o)
	s.lag = false
	s.advance(time.Second)
	return s
}

// liveness is the class the census gives the city row id now.
func (s *sim) liveness(id string) rowLiveness {
	_, obs := s.observed()
	return obs[rowKey{Leg: rowLeg, ID: id}].Liveness
}

// operator writes kv to the city row id as an outside writer, queuing its
// event, and returns that event.
func (s *sim) operator(id string, kv ...string) json.RawMessage {
	s.setMeta(s.legs[0], id, kv...)
	s.audit("operator")
	return s.legs[0].events[len(s.legs[0].events)-1]
}

func (s *sim) noViolations(t *testing.T) {
	t.Helper()
	if len(s.failures) > 0 {
		s.report(t)
	}
}

// heldRow is gc-1 asleep under a user hold that expired a minute ago.
func heldRow() beads.Bead {
	return poolRow("gc-1", "worker", 1, "asleep", "held_until", rowAt(-time.Minute), "sleep_reason", "user-hold")
}

// Scenario R5 (v5 R2; I15): a timer heal decided in one pass meets an
// operator's re-hold before its CAS. The fresh row decides at the store,
// however the operator's event travels: the effect reads the row from the
// backing, whose re-hold fails the premise (a lifecycle fact moved), so it
// writes nothing, the hold stands, and the next pass proposes nothing.
func TestSimR5StaleHealRedecidesAtTheStore(t *testing.T) {
	for _, c := range []struct {
		name  string
		event func(s *sim, rehold json.RawMessage)
		cause string
	}{
		{"the re-hold's event delivered", func(s *sim, ev json.RawMessage) { s.legs[0].cache.ApplyEvent("bead.updated", ev) }, causePremise},
		{"the re-hold's event held", func(*sim, json.RawMessage) {}, causePremise},
		// The cache, rescanned past the re-hold, then takes an older event,
		// reordered: the cache keeps the re-hold (mc-03lk4), so the effect's
		// read decides on it.
		{"an older event reordered after a rescan", func(s *sim, _ json.RawMessage) {
			older := s.legs[0].events[0]
			s.legs[0].cache.ReconcileNowForTest()
			s.legs[0].cache.ApplyEvent("bead.updated", older)
		}, causePremise},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := scripted(t, nil, heldRow())
			older := s.operator("gc-1", "held_until", s.rel(-30*time.Second))
			s.legs[0].cache.ApplyEvent("bead.updated", older)
			s.inventory()
			s.pass()
			if len(s.parked) != 1 || s.parked[0].it.Kind != intentRowHeal {
				t.Fatalf("parked %d effects, want gc-1's heal", len(s.parked))
			}
			hold := s.rel(time.Hour)
			c.event(s, s.operator("gc-1", "held_until", hold))
			s.release(0)
			s.audit("v2")
			s.pass()
			if got, _ := s.legs[0].backing.Get("gc-1"); got.Metadata["held_until"] != hold {
				t.Errorf("held_until = %q after the heal, want the operator's %q", got.Metadata["held_until"], hold)
			}
			if r := s.p.backoff.Snapshot()[rowBackoffKey(rowKey{Leg: rowLeg, ID: "gc-1"})]; r.Cause != c.cause {
				t.Errorf("the heal settled with cause %q, want %q", r.Cause, c.cause)
			}
			s.noViolations(t)
		})
	}
}

// Scenario R14 (v5 O1; I13, I23): a partial listing in the pass that misses
// a death concludes no absence; the next complete pass does.
func TestSimR14PartialListingConcludesNoAbsence(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"))
	s.inventory()
	if got := s.liveness("gc-1"); got != livenessAlive {
		t.Fatalf("gc-1 reads %s, want alive", got)
	}
	s.sp.drop("s-gc-1")
	s.sp.listing = 1
	s.advance(time.Second)
	s.inventory()
	if got := s.liveness("gc-1"); got == livenessGone {
		t.Fatal("a partial listing concluded gc-1 gone")
	}
	s.sp.listing = 0
	s.inventory()
	if got := s.liveness("gc-1"); got != livenessGone {
		t.Fatalf("after a complete listing gc-1 reads %s, want gone", got)
	}
	s.audit("v2")
	s.noViolations(t)
}

// Scenario R17 (INC-006; I10): an event replayed many times, and rescans,
// mark the planner dirty and write nothing: no write loop. The city first
// settles: the heal lands, then the freed slot's close (A21).
func TestSimR17ReplayedEventsWriteNothing(t *testing.T) {
	s := scripted(t, nil, heldRow())
	s.inventory()
	s.pass()
	s.release(0)
	for range 3 {
		s.pass()
		for len(s.parked) > 0 {
			s.release(0)
		}
	}
	s.audit("v2")
	ev := s.operator("gc-1", "held_until", s.rel(time.Hour))
	for range 5 {
		s.legs[0].cache.ApplyEvent("bead.updated", ev)
		s.legs[0].cache.ReconcileNowForTest()
		s.pass()
		if admitted := s.p.out.record.Load().Admitted; len(admitted) > 0 {
			t.Fatalf("a replay admitted %v", intentKeys(admitted))
		}
		s.audit("v2")
	}
	s.noViolations(t)
}

// Scenarios R34 and R35 (v5 O1, P2; I13): a tmux server that is gone makes
// a complete, empty pass only when confirmed dead. At a cold boot (R34) that
// opens the boot gate's inventory input and reads the rows gone; unconfirmed,
// the gate stays closed. Under live sessions (R35), unconfirmed, nothing
// reads gone, however long it lasts.
func TestSimR34R35ServerGone(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		for _, coldBoot := range []bool{true, false} {
			s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"))
			if !coldBoot {
				s.inventory()
			}
			for name := range s.sp.rts {
				s.sp.drop(name)
			}
			s.sp.serverDown, s.sp.confirmable = true, confirmed
			for range 3 {
				s.advance(simPatrol)
				s.inventory()
				s.pass()
			}
			gone := s.liveness("gc-1") == livenessGone
			if gone != confirmed || coldBoot && s.p.boot.InventoryComplete != confirmed {
				t.Errorf("cold boot %t, confirmed dead %t: gc-1 gone %t, boot inventory complete %t", coldBoot, confirmed, gone, s.p.boot.InventoryComplete)
			}
			for len(s.parked) > 0 { // a confirmed-gone row's close (A21) runs out
				s.release(0)
			}
			s.audit("v2")
			s.noViolations(t)
		}
	}
}

// Scenario R45 (v5 O1; I13): after a restart, a stop-pending row whose
// runtime died in the downtime and was never listed reads gone on the first
// complete pass, which also opens the boot gate's inventory input. A4's verb,
// which then finalizes it, is C6b2's.
func TestSimR45NeverListedStopPendingRowReadsGone(t *testing.T) {
	s := scripted(t, nil, poolRow("gc-9", "worker", 1, string(session.StateDraining), "state_reason", session.DrainAckStopPendingReason, "instance_token", "tok-1"))
	s.pass()
	if got := s.liveness("gc-9"); got != livenessUnknown || s.p.boot.InventoryComplete {
		t.Fatalf("before any inventory: gc-9 reads %s, inventory complete %t; want unknown and false", got, s.p.boot.InventoryComplete)
	}
	s.inventory()
	s.pass()
	if got := s.liveness("gc-9"); got != livenessGone || !s.p.boot.InventoryComplete {
		t.Errorf("after the first pass: gc-9 reads %s, inventory complete %t; want gone and true", got, s.p.boot.InventoryComplete)
	}
	s.audit("v2")
	s.noViolations(t)
}

// Scenario R47 (v5 D5; I18): `gc runtime drain-ack` races a PreWake. The CLI
// decided on the row it read; a PreWake moved the incarnation before its
// CAS, which refuses: the row carries no ack for the new incarnation, from
// the agent's pane or from an operator.
func TestSimR47DrainAckLosesToPreWake(t *testing.T) {
	for _, operator := range []bool{false, true} {
		s := scripted(t, []string{"gc-2"}, poolRow("gc-2", "worker", 1, "active", "instance_token", "tok-1"))
		commit, err := checkDrainAckRow(s.legs[0].backing, "gc-2", operator, "tok-1", s.clk.Now())
		if err != nil {
			t.Fatalf("operator %t: the ack refused before the race: %v", operator, err)
		}
		s.operator("gc-2", "generation", "2", "instance_token", "tok-2", "state", "awake")
		if err := commit(); err == nil {
			t.Errorf("operator %t: the ack landed across a PreWake", operator)
		}
		if got, _ := s.legs[0].backing.Get("gc-2"); got.Metadata[session.DrainAckIncarnationKey] != "" {
			t.Errorf("operator %t: the row carries ack %q", operator, got.Metadata[session.DrainAckIncarnationKey])
		}
		s.noViolations(t)
	}
}

// settle publishes an inventory, runs a pass and releases its effects.
func (s *sim) settle() {
	s.inventory()
	s.pass()
	for len(s.parked) > 0 {
		s.release(0)
	}
	s.audit("v2")
}

// gc1Work is in-progress work routed to the worker pool and assigned to
// gc-1.
func gc1Work() beads.Bead {
	return beads.Bead{ID: "gw-gc-1", Title: "work", Type: "task", Status: "in_progress", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}
}

// gc1 is the city row gc-1's metadata on the backing.
func (s *sim) gc1() map[string]string {
	b, err := s.legs[0].backing.Get("gc-1")
	if err != nil {
		s.t.Fatal(err)
	}
	return b.Metadata
}

// Kills the stability clears (v5.8 A6 item 6; SESS-539/540) lost, early or
// late, across time: a live quarantined row keeps its wake failures 29s past
// its wake, clears them and the quarantine at 31s, and its churn count only
// at 5 minutes.
func TestSimStabilityClearsAcrossTime(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "last_woke_at", rowAt(time.Second),
		"wake_attempts", "2", "churn_count", "3", "quarantined_until", rowAt(time.Hour)))
	for _, step := range []struct {
		at                           time.Duration // past the wake
		attempts, quarantined, churn bool          // still set
	}{
		{29 * time.Second, true, true, true},
		{31 * time.Second, false, false, true},
		{5*time.Minute - time.Second, false, false, true},
		{5 * time.Minute, false, false, false},
	} {
		s.advance(plannerT0.Add(time.Second + step.at).Sub(s.clk.Now()))
		s.settle()
		m := s.gc1()
		if got := [3]bool{m["wake_attempts"] != "0", m["quarantined_until"] != "", m["churn_count"] != "0"}; got != [3]bool{step.attempts, step.quarantined, step.churn} {
			t.Fatalf("%s past the wake: wake_attempts %q, quarantined_until %q, churn_count %q; want set %v", step.at, m["wake_attempts"], m["quarantined_until"], m["churn_count"], [3]bool{step.attempts, step.quarantined, step.churn})
		}
	}
	s.noViolations(t)
}

// Kills the detached-at marker (v5.8 A6 item 7; SESS-533/535/536) lost or
// stuck across time, on an interactive row under a sleep policy: a detach
// stamps the pass time, an attach clears it, the next detach stamps its own
// time, and the runtime's death clears it in the same-pass restart's PreWake
// CAS, which carries the dead-runtime heal.
func TestSimDetachedAtFollowsTheAttachFact(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1"), gc1Work())
	s.cfg.Agents[0].SleepAfterIdle = "5m"
	attach := func(on bool) {
		s.sp.mu.Lock()
		s.sp.rts["s-gc-1"].attached = on
		s.sp.mu.Unlock()
	}
	for _, step := range []struct {
		do   func()
		want func() string
	}{
		{func() {}, func() string { return s.rel(0) }},
		{func() { attach(true) }, func() string { return "" }},
		{func() { attach(false) }, func() string { return s.rel(0) }},
		{func() { s.sp.mu.Lock(); s.sp.drop("s-gc-1"); s.sp.mu.Unlock() }, func() string { return "" }},
	} {
		step.do()
		s.advance(simPatrol)
		s.settle()
		if got, want := s.gc1()["detached_at"], step.want(); got != want {
			t.Fatalf("at %s: detached_at %q, want %q", s.clk.Now().Sub(plannerT0), got, want)
		}
	}
	if m := s.gc1(); m["state"] != "awake" || m["generation"] != "2" {
		t.Fatalf("row %v, want the wanted row restarted in the pass its runtime died", m)
	}
	s.noViolations(t)
}

// Kills the dead-runtime heal (v5.8 A6 item 4; SESS-531) narrowed to Wake
// or named rows: a pool row AL1 wants asleep (quarantined, its work waiting)
// whose runtime dies heals to asleep with runtime-missing and its
// continuation reset, which A21's pool-slot close can free. A heartbeat-held
// one heals alike: A17 would respawn it through its hold, but its quarantine
// holds the respawn.
func TestSimDeadRuntimeHealFreesAnUnwantedRow(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held %t", held), func(t *testing.T) { deadRuntimeHealAcrossTime(t, held) })
	}
}

func deadRuntimeHealAcrossTime(t *testing.T, held bool) {
	meta := []string{"instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(0), "quarantined_until", rowAt(time.Hour)}
	if held {
		meta = append(meta, "held_until", rowAt(2*time.Minute))
	}
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", meta...), gc1Work())
	s.settle()
	s.sp.mu.Lock()
	s.sp.drop("s-gc-1")
	s.sp.mu.Unlock()
	s.advance(simPatrol)
	s.settle()
	if got := s.gc1()["state"]; got != "asleep" {
		t.Fatalf("held %t: state %q after the runtime died", held, got)
	}
	s.advance(2 * time.Minute)
	s.settle()
	s.settle()
	m := s.gc1()
	info := session.Info{MetadataState: m["state"], SleepReason: m["sleep_reason"]}
	if m["sleep_reason"] != string(session.SleepReasonRuntimeMissing) || m["session_key"] != "" || m["continuation_reset_pending"] != "true" || !isPoolSessionSlotFreeableInfo(info, s.clk.Now()) {
		t.Fatalf("held %t: row %v, want asleep, runtime-missing, its continuation reset and freeable", held, m)
	}
	s.noViolations(t)
}

// Kills an acked drain request's dead runtime restarted (CONTRACT v5.10k,
// §12.2 row 68; mc-7zlhz, J40 C): an operator's or a controller's drain
// that the agent acked on a floor member, the floor wanting it awake, whose
// runtime is gone, a dead pane, or a squatter's under a name the row does
// not own before the stop, is finalized as a drain's exit, the row closed
// with no work, and nothing is started under its name; so is a bare ack
// off the floor (v5.10k-1; the sim wants an off-floor row awake only for
// work, so its squatter is the case a heal would take, and the planner's
// table covers the rest). The death comes seconds after the ack, so a
// squatter is newer than it, as on tmux, and A11 does not void the ack for
// it (v5.10k-2). The dead-pane cases check the finalize end to end, but no
// heal would take them first: on a Wake row A12 never classifies the pane
// before A11 signals, so the planner's classified pane is the heal's case.
// A squatter under its bead-scoped name, which D3 reads as the row's own,
// is the squatter heal's still: the row parks asleep, never stuck
// stop-pending on a stop that only refuses. A floor member's bare ack is
// row 66's: its seat restarts in place.
func TestSimAckedRequestWithADeadRuntimeIsFinalized(t *testing.T) {
	for _, request := range []string{"operator", "controller", "bare", "bare on the floor"} {
		for _, death := range []string{"gone", "dead pane", "squatter", "squatter, bead-scoped name"} {
			if request == "bare on the floor" && death != "gone" && death != "dead pane" {
				continue
			}
			t.Run(request+", "+death, func(t *testing.T) {
				name := "s-slot-1" // a name the row does not own
				if death == "squatter, bead-scoped name" {
					name = "s-gc-1"
				}
				s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_name", name))
				if request != "bare" {
					s.cfg.Agents[0].MinActiveSessions = intPtr(1) // gc-1 the floor's member
				}
				now := s.clk.Now().Format(time.RFC3339)
				var req []string
				switch request {
				case "operator":
					req = []string{session.OperatorDrainKey, "1"}
				case "controller":
					req = []string{session.DrainIntentReasonKey, drainConfigDrift, session.DrainIntentAtKey, now, session.DrainIntentIncarnationKey, "1"}
				}
				s.operator("gc-1", append(req, session.DrainAckIncarnationKey, "1", session.DrainAckAtKey, now)...)
				s.advance(2 * time.Second) // the agent exits after its ack
				s.sp.mu.Lock()
				switch death {
				case "gone":
					s.sp.drop(name)
				case "dead pane":
					s.sp.rts[name].corpse = true
					s.sp.touch(name)
				default:
					s.sp.drop(name)
					s.sp.put(name, "gc-other", "1", "tok-squatter")
				}
				s.sp.mu.Unlock()
				for range 8 {
					s.settle()
					s.advance(simPatrol)
				}
				b, err := s.legs[0].backing.Get("gc-1")
				if err != nil {
					t.Fatal(err)
				}
				started := slices.ContainsFunc(s.sp.starts, func(st simStart) bool { return st.Name == name })
				switch {
				case request == "bare on the floor":
					if !started || b.Status != "open" {
						t.Errorf("row %s %v, started %t; want row 66's restart in place", b.Status, b.Metadata, started)
					}
				case started:
					t.Errorf("starts %+v; want nothing started under the acked row's name", s.sp.starts)
				case death == "squatter, bead-scoped name" && (b.Status != "open" || b.Metadata["state"] != "asleep"):
					t.Errorf("row %s %v; want the squatter heal's park", b.Status, b.Metadata)
				case death != "squatter, bead-scoped name" && b.Status != "closed":
					t.Errorf("row %s %v; want the drain finalized, the row closed", b.Status, b.Metadata)
				}
				s.noViolations(t)
			})
		}
	}
}

// Kills an I15 oracle that lets any write end a live quarantine: A6's
// stability clear may clear quarantined_until only on a row its deciding pass
// read committed, its own runtime alive and 30s past its wake. A clear inside
// the 30s, on a dead runtime or on an uncommitted row is reported, and so is,
// on a stable row, a shortening of the quarantine or a stop request; an
// extension is not (an accrual's, the #7706 review).
func TestSimI15StabilityCarveOut(t *testing.T) {
	quarantined := func(state string, woke time.Duration, meta ...string) beads.Bead {
		return poolRow("gc-1", "worker", 1, state, append([]string{"instance_token", "tok-1", "last_woke_at", rowAt(woke), "quarantined_until", rowAt(time.Hour)}, meta...)...)
	}
	clearQ := session.MetadataPatch{"quarantined_until": ""}
	for _, tc := range []struct {
		name   string
		row    beads.Bead
		zombie bool
		patch  session.MetadataPatch
	}{
		{"inside 30s", quarantined("active", -10*time.Second), false, clearQ},
		{"dead runtime", quarantined("active", -time.Hour), true, clearQ},
		{"uncommitted", quarantined("creating", -time.Hour), false, clearQ},
		{"stable, the quarantine shortened", quarantined("active", -time.Hour), false, session.MetadataPatch{"quarantined_until": rowAt(30 * time.Minute)}},
		{"stable, a stop request on a user-hold row", quarantined("active", -time.Hour, "sleep_intent", "user-hold"), false, session.MetadataPatch{"quarantined_until": "", session.DrainIntentReasonKey: "idle"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write := func(r *rowFacts) (intent, bool) {
				if r.row.Info.QuarantinedUntil == tc.patch["quarantined_until"] {
					return intent{}, false
				}
				// The quarantine's write is the Quarantine cell's, to the
				// scenario's deadline (none: cleared); the rest is the patch.
				until, _ := time.Parse(time.RFC3339, tc.patch["quarantined_until"])
				class := session.CellInfo(r.row.Info, session.ActorController, session.DispEvent{Kind: session.EvQuarantine, Until: until}, r.w.Now)
				patch := maps.Clone(tc.patch)
				delete(patch, "quarantined_until")
				return r.healClass(intentRowHeal, decideStabilityClear, patch, class)
			}
			arms := mutateHeal(write)
			if tc.row.Metadata["sleep_intent"] == "user-hold" {
				// A4's direct stop of the live user-hold row would preempt the
				// seeded write; the oracle under test is I15's, not A4's.
				arms = func(in []rowArm) []rowArm {
					return slices.DeleteFunc(mutateHeal(write)(in), func(a rowArm) bool { return a.name == "A4" })
				}
			}
			s := scriptedWith(t, simOpts{arms: arms}, []string{"gc-1"}, tc.row)
			if tc.zombie {
				s.sp.mu.Lock()
				s.sp.rts["s-gc-1"].zombie = true
				s.sp.touch("s-gc-1")
				s.sp.mu.Unlock()
			}
			s.settle()
			if !slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, "I15 ") }) {
				t.Fatalf("row %v, failures %v: want the write reported on I15", s.gc1(), s.failures)
			}
		})
	}
	// The real clear, on a stable row, is not, its quarantine's own reason
	// ended with it (mc-bwvzv).
	for _, reason := range []string{"", "quarantine", "rate_limit"} {
		s := scripted(t, []string{"gc-1"}, quarantined("active", -time.Hour, "sleep_reason", reason))
		s.settle()
		if m := s.gc1(); m["quarantined_until"] != "" || m["sleep_reason"] != "" {
			t.Fatalf("reason %q: row %v, want A6's stability clear, the reason ended", reason, m)
		}
		s.noViolations(t)
	}
	// An accrual's or a rate-limit quarantine's extension of a live
	// quarantine is no violation; a shortening is, as are an unsigned
	// extension (no counter advanced, no quarantine reason set, last_woke_at
	// kept: a quarantine reason the row already carries signs nothing) and
	// one past the longest quarantine the controller writes (the #7726
	// review).
	b := quarantined("active", -time.Hour, "sleep_reason", "idle", "wake_attempts", "4")
	short := quarantined("active", -time.Hour, "quarantined_until", rowAt(time.Minute), "sleep_reason", "idle", "wake_attempts", "4")
	shortQ := quarantined("active", -time.Hour, "quarantined_until", rowAt(time.Minute), "sleep_reason", "rate_limit")
	for _, c := range []struct {
		name string
		row  beads.Bead
		to   time.Duration
		meta []string
		want string
	}{
		{"an accrual's extension", short, defaultQuarantineDuration, []string{"wake_attempts", "5", "sleep_reason", "quarantine"}, ""},
		{"a churn accrual's extension", short, defaultQuarantineDuration, []string{"churn_count", "3"}, ""},
		{"a rate-limit quarantine's extension", short, defaultRateLimitQuarantineDuration, []string{"sleep_reason", "rate_limit"}, ""},
		{"an unsigned extension", short, defaultQuarantineDuration, nil, "quarantined_until"},
		{"an unsigned re-extension of a quarantined row", shortQ, defaultQuarantineDuration, nil, "quarantined_until"},
		{"a re-extension clearing last_woke_at", shortQ, defaultRateLimitQuarantineDuration, []string{"last_woke_at", ""}, ""},
		{"a signed extension past the longest quarantine", b, 2 * time.Hour, []string{"wake_attempts", "5", "sleep_reason", "quarantine"}, "quarantined_until"},
		{"a shortening", b, 30 * time.Minute, []string{"wake_attempts", "5", "sleep_reason", "quarantine"}, "quarantined_until"},
	} {
		a := maps.Clone(c.row.Metadata)
		a["quarantined_until"] = rowAt(c.to)
		for i := 0; i+1 < len(c.meta); i += 2 {
			a[c.meta[i]] = c.meta[i+1]
		}
		if k := dormantChange(c.row, a, gatherNow, false); k != c.want {
			t.Fatalf("%s: dormantChange = %q, want %q", c.name, k, c.want)
		}
	}
	b = quarantined("active", -time.Hour, "sleep_reason", "quarantine")
	// A quarantine's reason cleared while the quarantine stands is still
	// the quarantine's to keep.
	a := maps.Clone(b.Metadata)
	a["sleep_reason"] = ""
	if k := dormantChange(b, a, plannerT0, true); k != "sleep_reason" {
		t.Fatalf("dormantChange = %q, want sleep_reason: the quarantine stands", k)
	}
	// The clear that ends the quarantine may only clear its reason, never
	// rewrite it; and a context_churn sleep's reason stays the row's, the
	// clear ending only the quarantine (the #7696 review's nits).
	for _, c := range []struct {
		reason, to string
		want       string
	}{
		{"quarantine", "idle", "sleep_reason"},
		{"rate_limit", "idle", "sleep_reason"},
		{"context-churn", "", "sleep_reason"},
		{"quarantine", "", ""},
	} {
		b := quarantined("active", -time.Hour, "sleep_reason", c.reason)
		a := maps.Clone(b.Metadata)
		a["quarantined_until"], a["sleep_reason"] = "", c.to
		if k := dormantChange(b, a, plannerT0, true); k != c.want {
			t.Fatalf("%s -> %q on the clear: dormantChange = %q, want %q", c.reason, c.to, k, c.want)
		}
	}
}

// Kills the dead-runtime heal (v5.8e A6 item 4) reaching past the sessions
// leg: a census-only rig-leg row with no runtime may be another city's
// on a shared rig store, so it keeps its state and its conversation keys,
// while the sessions-leg row beside it heals.
func TestSimDeadRuntimeHealSkipsCensusOnlyRigRows(t *testing.T) {
	meta := []string{"instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(0), "quarantined_until", rowAt(time.Hour)}
	rig := poolRow("rg-1", "worker", 11, "active", meta...)
	s := newSim(t, 1, simOpts{rows: func(*sim) ([]beads.Bead, []beads.Bead) {
		return []beads.Bead{poolRow("gc-1", "worker", 1, "active", meta...), gc1Work()}, []beads.Bead{rig}
	}})
	s.lag = false
	for range 2 {
		s.advance(simPatrol)
		s.settle()
	}
	if m := s.gc1(); m["state"] != "asleep" {
		t.Fatalf("gc-1 %v, want the sessions-leg row healed", m)
	}
	got, err := s.legs[1].backing.Get("rg-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["state"] != "active" || got.Metadata["session_key"] != "k-1" {
		t.Fatalf("rg-1 %v, want the census-only rig row untouched", got.Metadata)
	}
	s.noViolations(t)
}

// The effect transaction inside the simulator (EFFECT-STRUCTURE §2.1, §2.5;
// v5 R2; I15): an operator's write behind the cache lands inside the heal,
// at a seam between its fresh read and its CAS. An unrelated write loses the
// heal's first CAS, and the heal reads the row again and lands; a re-hold
// loses it too, and the heal refuses on the premise. Either way the
// operator's write stands. A write behind the cache before the heal runs is
// read from the backing, so the heal lands on its first CAS.
func TestSimTxDecidesAgainInsideTheEffect(t *testing.T) {
	for _, c := range []struct {
		name, key string
		seam      txSeam // 0: before the release
		healed    bool
		attempts  int
	}{
		{"an unrelated write before the CAS", "test_note", seamBeforeCAS, true, 2},
		{"an unrelated write after the fresh read", "test_note", seamAfterRowRead, true, 2},
		{"a re-hold before the CAS", "held_until", seamBeforeCAS, false, 1},
		{"an unrelated write before the heal runs", "test_note", 0, true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := scripted(t, nil, heldRow())
			s.inventory()
			s.pass()
			if len(s.parked) != 1 || s.parked[0].it.Kind != intentRowHeal {
				t.Fatalf("parked %d effects, want gc-1's heal", len(s.parked))
			}
			val := s.rel(time.Hour)
			e := s.parked[0]
			write := func() {
				b, _ := s.legs[0].backing.Get("gc-1")
				s.outside("operator", s.legs[0], b, func(l *simLeg, b beads.Bead) { s.setMeta(l, b.ID, c.key, val) })
			}
			if e.seam, e.outside = c.seam, write; c.seam == 0 {
				write()
			}
			s.release(0)
			s.audit("v2")
			got, _ := s.legs[0].backing.Get("gc-1")
			if got.Metadata[c.key] != val || c.healed != (got.Metadata["held_until"] == "") {
				t.Errorf("row %v, want the operator's %s=%s and healed %t", got.Metadata, c.key, val, c.healed)
			}
			if n := len(slices.DeleteFunc(slices.Clone(e.seams), func(at txSeam) bool { return at != seamBeforeCAS })); c.healed && n != c.attempts {
				t.Errorf("the heal reached its CAS %d times, want %d", n, c.attempts)
			}
			s.noViolations(t)
		})
	}
}

// Kills a pool seat stranded by its young death (staging J23): AL1 never
// wakes a persistent pool's asleep seat that has no work assigned (A21's
// pool-slot close replaces it, C5c1), so a death that accrues churn
// restarts the seat in place in the pass that records it, for a singleton
// pool and for a pool at its max serving routed work, with no seat created
// beside it.
func TestSimYoungDeathRestartsAPoolSeatInPlace(t *testing.T) {
	ids := []string{"gc-s", "gc-1", "gc-2"}
	s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		s.cfg.Agents[0].MaxActiveSessions = intPtr(2)
		s.cfg.Agents = append(s.cfg.Agents, config.Agent{Name: "solo", StartCommand: "true", MaxActiveSessions: intPtr(1)})
		var rows []beads.Bead
		for i, id := range ids {
			template, slot := "worker", i
			if id == "gc-s" {
				template, slot = "solo", 1
			}
			row := poolRow(id, template, slot, "active", "instance_token", "tok-"+id, "session_key", "k-"+id, "last_woke_at", rowAt(-2*time.Minute))
			s.sp.put(row.Metadata["session_name"], id, "1", "tok-"+id)
			work := beads.Bead{ID: "gw-" + id, Title: "work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": template}}
			rows = append(rows, row, work)
		}
		return rows, nil
	}})
	s.lag = false
	s.advance(time.Second)
	s.sp.mu.Lock()
	for _, id := range ids {
		s.sp.drop("s-" + id)
	}
	s.sp.mu.Unlock()
	s.advance(simPatrol)
	s.settle()
	s.settle()
	for _, id := range ids {
		b, err := s.legs[0].backing.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if m := b.Metadata; m["state"] != "awake" || m["churn_count"] != "1" || !s.livePane(b) {
			t.Errorf("%s: row %v, want it restarted in place with its churn recorded", id, m)
		}
	}
	if rows, _ := s.legs[0].backing.List(beads.ListQuery{Label: session.LabelSession}); len(rows) != len(ids) {
		t.Errorf("%d session rows, want the %d seats and none beside them", len(rows), len(ids))
	}
	s.noViolations(t)
}

// Kills a deferred stranded death reclassified by a later pass (review J1,
// the budget shape): three seats rapid-crash together with one wake per
// pass (and room for their row writes); each records a wake failure, as
// legacy records it in the tick it sees it, and is restarted in turn.
func TestSimDeferredStrandedDeathsRecordOnce(t *testing.T) {
	ids := []string{"gc-1", "gc-2", "gc-3"}
	s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		one := 1
		s.cfg.Daemon.MaxWakesPerTick, s.cfg.Daemon.ProbeConcurrency = &one, intPtr(4)
		var rows []beads.Bead
		for i, id := range ids {
			row := poolRow(id, "worker", i+1, "active", "instance_token", "tok-"+id, "session_key", "k-"+id, "last_woke_at", plannerT0.Add(-10*time.Second).Format(time.RFC3339), "wake_attempts", "1")
			s.sp.put(row.Metadata["session_name"], id, "1", "tok-"+id)
			rows = append(rows, row, beads.Bead{ID: "gw-" + id, Title: "work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
		}
		return rows, nil
	}})
	s.lag = false
	s.sp.mu.Lock()
	for _, id := range ids {
		s.sp.drop("s-" + id)
	}
	s.sp.mu.Unlock()
	for range len(ids) { // inside 30s of the last restart, before its stability clear
		s.advance(simPatrol)
		s.settle()
	}
	for _, id := range ids {
		b, err := s.legs[0].backing.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if m := b.Metadata; m["wake_attempts"] != "2" || m["churn_count"] != "" || !s.livePane(b) {
			t.Errorf("%s: wake_attempts %q churn_count %q live %v, want the rapid crash recorded once and the seat restarted", id, m["wake_attempts"], m["churn_count"], s.livePane(b))
		}
	}
	s.noViolations(t)
}

// Kills a cutover drain storm (v5 M2, I21, Gate R R18; staging J13): a live
// seat whose stored hashes carry the previous fingerprint version is
// rebaselined to this binary's, all six keys, with no stop and no start.
func TestSimCutoverV5HashesRebaselineNotDrain(t *testing.T) {
	v5 := []string{"started_config_hash", "v5:core", "started_provision_hash", "v5:prov", "started_launch_hash", "v5:launch", "started_live_hash", "v5:live", "live_hash", "v5:live", "core_hash_breakdown", "{}"}
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", append([]string{"instance_token", "tok-1", "last_woke_at", rowAt(-time.Hour)}, v5...)...), gc1Work())
	for range 4 { // A7 converges the row's identity first, then A8 hashes it (v5 B4)
		s.settle()
	}
	m := s.gc1()
	for i := 0; i < len(v5); i += 2 {
		if k := v5[i]; k != "core_hash_breakdown" && !strings.HasPrefix(m[k], runtime.FingerprintVersion+":") || m[k] == v5[i+1] {
			t.Errorf("%s = %q, want rebaselined under %s", k, m[k], runtime.FingerprintVersion)
		}
	}
	if len(s.sp.kills) != 0 || len(s.sp.starts) != 0 || m["state"] != "active" {
		t.Errorf("kills %v starts %v state %q, want the seat left running", s.sp.kills, s.sp.starts, m["state"])
	}
	s.noViolations(t)
}

// Kills a soft reload under v2 that accepts nothing, accepts a row twice,
// or never replies (MAINT-025, v5 M2): a request for the env's generation
// accepts a live seat's drifted config, with no stop and no start, counts it
// once, and closes when no drifted row is left, so the reply carries it; a
// newer request closes the older at once.
func TestSimSoftReloadAcceptsDriftOnce(t *testing.T) {
	stale := runtime.FingerprintVersion + ":stale"
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "last_woke_at", rowAt(-time.Hour), "started_config_hash", stale), gc1Work())
	q := newSoftReloadRequest(1)
	s.p.openSoftReload(q)
	for range 5 { // A7 converges the row's identity first (v5 B4)
		s.settle()
	}
	select {
	case <-q.done:
	default:
		t.Fatal("the request is still open with no drifted row left")
	}
	var r reloadControlReply
	q.reply(&r)
	if m := s.gc1(); r.AcceptedDriftCount == nil || *r.AcceptedDriftCount != 1 || m["started_config_hash"] == stale || !strings.HasPrefix(m["started_config_hash"], runtime.FingerprintVersion+":") {
		t.Fatalf("reply %+v, row hash %q: want one accepted and the row's hash current", r, m["started_config_hash"])
	}
	if len(s.sp.kills) != 0 || len(s.sp.starts) != 0 {
		t.Fatalf("kills %v starts %v, want the seat left running", s.sp.kills, s.sp.starts)
	}
	older, newer := newSoftReloadRequest(1), newSoftReloadRequest(1)
	s.p.openSoftReload(older)
	s.p.openSoftReload(newer)
	select {
	case <-older.done:
	default:
		t.Fatal("a newer request left the older open")
	}
	s.noViolations(t)
}

// Kills A8 hashing a row before A7 rewrites its identity (v5 B4; R43): a
// live seat with the previous fingerprint's hashes and no origin is given
// its identity first, by A7 alone, then rebaselined by A8, with no stop and
// no start. (An alias A7 writes rebaselines in A7's own CAS, as legacy's
// does: the hashes are taken on the rewritten row either way.)
func TestSimR43IdentityBeforeRebaseline(t *testing.T) {
	v5 := []string{"started_config_hash", "v5:core", "started_provision_hash", "v5:prov", "started_launch_hash", "v5:launch", "started_live_hash", "v5:live", "live_hash", "v5:live", "core_hash_breakdown", "{}"}
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", append([]string{"instance_token", "tok-1", "last_woke_at", rowAt(-time.Hour)}, v5...)...), gc1Work())
	identityFirst := false
	for range 4 {
		s.settle()
		m := s.gc1()
		rebaselined := m["started_config_hash"] != "v5:core"
		if rebaselined && m["session_origin"] == "" {
			t.Fatalf("row %v: rebaselined before its identity was written", m)
		}
		identityFirst = identityFirst || !rebaselined && m["session_origin"] == "ephemeral"
	}
	m := s.gc1()
	if !identityFirst || !strings.HasPrefix(m["started_config_hash"], runtime.FingerprintVersion+":") {
		t.Fatalf("row %v: want the identity written alone, then the rebaseline", m)
	}
	if len(s.sp.kills) != 0 || len(s.sp.starts) != 0 || m["state"] != "active" {
		t.Errorf("kills %v starts %v state %q, want the seat left running", s.sp.kills, s.sp.starts, m["state"])
	}
	s.noViolations(t)
}

// Kills a rollback that stops a foreign occupant outside C8.2(a), and an I4
// that flags the stop C8.2(a) admits (the C5a2 review): a pending create
// whose bead-scoped name another row's runtime holds is rolled back, the
// occupant stopped by name, with no violation. (A Foreign runtime under any
// other name is rolled back without a stop: TestRollbackRuntime.)
func TestSimRollbackStopsABeadScopedNameOnly(t *testing.T) {
	run := func(t *testing.T, row beads.Bead) *sim {
		t.Helper()
		s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
			s.sp.put(row.Metadata["session_name"], "gc-other", "1", "tok-other")
			return []beads.Bead{row}, nil
		}})
		s.lag = false
		for range 3 {
			s.advance(simPatrol)
			s.settle()
		}
		s.noViolations(t)
		return s
	}
	started := plannerT0.Add(-time.Minute).Format(time.RFC3339)
	s := run(t, poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-gc-1", "pending_create_claim", "true", "pending_create_started_at", started))
	if b, _ := s.legs[0].backing.Get("gc-1"); b.Status != "closed" || len(s.sp.kills) != 1 {
		t.Fatalf("bead-scoped: row %s kills %+v, want rolled back with the occupant stopped", b.Status, s.sp.kills)
	}
}

// Kills a drift drain over an attached human (GUAR-006, SESS-553/554/561):
// a drifted seat that is attached stamps its deferral and is never drained
// or stopped; detached, it waits out the recent-deferral window, then its
// config-drift request is written, and nothing kills its runtime (the stop
// is C6b2's).
func TestSimConfigDriftWaitsForTheHumanThenDrains(t *testing.T) {
	stale := runtime.FingerprintVersion + ":stale"
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "last_woke_at", rowAt(-time.Hour), "started_config_hash", stale))
	s.cfg.Agents[0].MinActiveSessions = intPtr(1)
	attach := func(on bool) {
		s.sp.mu.Lock()
		s.sp.rts["s-gc-1"].attached = on
		s.sp.touch("s-gc-1")
		s.sp.mu.Unlock()
	}
	attach(true)
	for range 3 {
		s.advance(simPatrol)
		s.settle()
	}
	if m := s.gc1(); m["attached_config_drift_deferred_key"] == "" || m[session.DrainIntentReasonKey] != "" {
		t.Fatalf("attached: row %v, want the deferral stamped and no request", m)
	}
	attach(false)
	s.advance(time.Minute)
	s.settle()
	if m := s.gc1(); m[session.DrainIntentReasonKey] != "" {
		t.Fatalf("detached inside the window: request %q, want none", m[session.DrainIntentReasonKey])
	}
	s.advance(6 * time.Minute)
	s.settle()
	s.settle()
	if m := s.gc1(); m[session.DrainIntentReasonKey] != drainConfigDrift {
		t.Fatalf("past the window: row %v, want the config-drift request", m)
	}
	if len(s.sp.kills) != 0 {
		t.Fatalf("kills %v, want none", s.sp.kills)
	}
	s.noViolations(t)
}

// Scenario J27 B (C6a ruling 3, v5.9g; D1, D5): an agent acks its drain,
// its runtime dies, and Manager.ensureRunning restarts it at the same
// generation and token before the controller's next pass. The restarted
// runtime is never stopped for its predecessor's ack: the ack signal's
// fresh read finds a runtime created after the ack, and voids the ack half.
// With no restart the ack stops the acked runtime.
func TestSimJ27StaleAckAcrossSameGenerationRestart(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"), gc1Work())
		s.settle()
		commit, err := checkDrainAckRow(s.legs[0].backing, "gc-1", false, "tok-1", s.clk.Now())
		if err == nil {
			err = commit()
		}
		if err != nil {
			t.Fatalf("restarted %t: the agent's ack: %v", restarted, err)
		}
		s.audit("drain-ack")
		s.legs[0].cache.ApplyEvent("bead.updated", s.legs[0].events[len(s.legs[0].events)-1]) // the census sees the CLI's write
		s.advance(5 * time.Second)
		s.sp.mu.Lock()
		acked := s.sp.rts["s-gc-1"].object
		if restarted {
			s.sp.drop("s-gc-1")
			s.sp.put("s-gc-1", "gc-1", s.gc1()["generation"], "tok-1")
		}
		s.sp.mu.Unlock()
		for range 3 {
			s.settle()
			s.advance(time.Minute)
		}
		var stopped []int64
		for _, k := range s.sp.kills {
			if k.Name == "s-gc-1" && k.Hit != nil {
				stopped = append(stopped, k.Hit.object)
			}
		}
		m := s.gc1()
		switch {
		case restarted && (len(stopped) != 0 || m[session.DrainAckIncarnationKey] != "" || m["state_reason"] == session.DrainAckStopPendingReason):
			t.Errorf("restarted: stopped objects %v, row %v; want none stopped and the ack voided", stopped, m)
		case !restarted && !slices.Contains(stopped, acked):
			t.Errorf("not restarted: stopped objects %v, want the acked runtime $%d stopped", stopped, acked)
		}
		s.noViolations(t)
	}
}

// Kills a commit that drops a quarantine landed after its PreWake, and a
// row left up under it (§12.2 #57): S2 adopts the row's own live runtime and
// keeps the quarantine's keys; A6's stability clear then ends the quarantine
// of the row alive past its wake, inside I15's bound, which the run passes
// with no violation.
func TestSimLateQuarantineCommitsThenClears(t *testing.T) {
	until := rowAt(time.Hour)
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-1", "last_woke_at", rowAt(time.Second),
		"quarantined_until", until, "sleep_reason", "quarantine"))
	s.settle()
	if m := s.gc1(); m["state"] != "awake" || m["quarantined_until"] != until || m["sleep_reason"] != "quarantine" {
		t.Fatalf("row %v: want adopted awake with its quarantine kept", m)
	}
	for range quarantineDrainBound/simPatrol + 1 { // past I15's bound
		s.advance(simPatrol)
		s.settle()
	}
	s.noViolations(t)
	if m := s.gc1(); m["quarantined_until"] != "" || m["sleep_reason"] != "" || m["state"] != "awake" {
		t.Fatalf("row %v: want the quarantine ended with its sleep reason, the row awake (mc-bwvzv)", m)
	}
}

// Kills I15's §12.2 #57 exception excusing a commit under more than its
// late quarantine (E07 of the C5a3 delta review): the quarantine must hold
// alone; a held_until timer, a wait hold, a user hold or an operator
// suspend beside it, or a quarantine already ended, excuses nothing.
func TestQuarantineAloneIsTheOnlyHold(t *testing.T) {
	q := map[string]string{"state": "creating", "quarantined_until": rowAt(time.Hour)}
	with := func(k, v string) map[string]string { m := maps.Clone(q); m[k] = v; return m }
	for name, c := range map[string]struct {
		meta map[string]string
		want bool
	}{
		"alone":            {q, true},
		"held_until":       {with("held_until", rowAt(time.Hour)), false},
		"held_until ended": {with("held_until", rowAt(-time.Hour)), true},
		"wait hold":        {with("wait_hold", "w-1"), false},
		"user hold":        {with("sleep_intent", "user-hold"), false},
		"suspended":        {with("state", "suspended"), false},
		"ended":            {with("quarantined_until", rowAt(-time.Minute)), false},
	} {
		if got := quarantineAlone(c.meta, plannerT0); got != c.want {
			t.Errorf("%s: quarantineAlone = %v, want %v", name, got, c.want)
		}
	}
}

// simAck is the agent of row id (its pane's token tok) acking its own drain
// through the CLI's row ack (E3), with the census told of the write. It
// reports whether the ack landed.
func (s *sim) simAck(id, tok string) bool {
	commit, err := checkDrainAckRow(s.legs[0].backing, id, false, tok, s.clk.Now())
	if err == nil {
		err = commit()
	}
	if err != nil {
		return false
	}
	s.audit("drain-ack")
	s.legs[0].cache.ApplyEvent("bead.updated", s.legs[0].events[len(s.legs[0].events)-1])
	return true
}

// simOperatorDrain is `gc runtime drain` (drain true) or `undrain` on row
// gc-1 in a v2 city, with the census told of the write.
func (s *sim) simOperatorDrain(drain bool) {
	var err error
	if drain {
		err = writeOperatorDrainRow(s.legs[0].backing, "gc-1")
	} else {
		err = clearOperatorDrainRow(s.legs[0].backing, "gc-1")
	}
	if err != nil {
		s.t.Fatal(err)
	}
	s.audit("operator-drain")
	s.legs[0].cache.ApplyEvent("bead.updated", s.legs[0].events[len(s.legs[0].events)-1])
}

// floorRun settles s for patrols patrols, reporting whether row id was ever
// stop-pending.
func (s *sim) floorRun(id string, patrols int) (pending bool) {
	for range patrols {
		s.advance(simPatrol)
		s.settle()
		b, _ := s.legs[0].backing.Get(id)
		pending = pending || b.Metadata["state_reason"] == session.DrainAckStopPendingReason
	}
	return pending
}

// stoppedRow reports whether the sim ever stopped row id's runtime.
func (s *sim) stoppedRow(name string) bool {
	for _, k := range s.sp.kills {
		if k.Name == name {
			return true
		}
	}
	return false
}

// Scenario sc-j27j0d (DRAIN-010, PAR-FLOORACK): a floor member that acks its
// own idle drain, again and again as an idle agent does, is never marked
// stop-pending, never stopped and never recreated (its runtime object
// stands), its ack voided each time, while an elastic seat above the floor
// retires on its own ack; with the floor at 0 the ack stops the seat. An
// operator's `gc runtime drain` (legacy's GC_DRAIN) explains the ack, which
// then stops the floor seat; undrained, the ack is canceled again.
func TestSimFloorAckKeepsTheSeatWarm(t *testing.T) {
	seat := func(id string, slot int) beads.Bead {
		return poolRow(id, "worker", slot, "active", "instance_token", "tok-"+id, "last_woke_at", rowAt(-time.Hour))
	}
	for _, c := range []struct {
		name     string
		floor    int
		seats    []string
		operator string // "drain", "undrain" (drain, then undrain) before the acks
		warm     []string
		stopped  []string
	}{
		{"one seat at floor 1", 1, []string{"gc-1"}, "", []string{"gc-1"}, nil},
		{"one seat at floor 0", 0, []string{"gc-1"}, "", nil, []string{"gc-1"}},
		{"three seats at floor 2", 2, []string{"gc-1", "gc-2", "gc-3"}, "", []string{"gc-1", "gc-2"}, []string{"gc-3"}},
		{"an operator's drain on the floor seat", 1, []string{"gc-1"}, "drain", nil, []string{"gc-1"}},
		{"an operator's drain, undrained", 1, []string{"gc-1"}, "undrain", []string{"gc-1"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var rows []beads.Bead
			for i, id := range c.seats {
				rows = append(rows, seat(id, i+1))
			}
			s := scripted(t, c.seats, rows...)
			s.cfg.Agents[0].MinActiveSessions = intPtr(c.floor)
			s.settle()
			objects := map[string]int64{}
			s.sp.mu.Lock()
			for _, id := range c.seats {
				objects[id] = s.sp.rts["s-"+id].object
			}
			s.sp.mu.Unlock()
			switch c.operator {
			case "drain":
				s.simOperatorDrain(true)
				s.floorRun("gc-1", 3) // the request stands un-acked: nothing voids it
			case "undrain":
				s.simOperatorDrain(true)
				s.floorRun("gc-1", 3)
				s.simOperatorDrain(false)
			}
			pending := map[string]bool{}
			for range 3 {
				for _, id := range c.seats {
					s.simAck(id, "tok-"+id)
				}
				for _, id := range c.seats {
					pending[id] = s.floorRun(id, 3) || pending[id]
				}
			}
			for _, id := range c.warm {
				s.sp.mu.Lock()
				rt, live := s.sp.rts["s-"+id]
				s.sp.mu.Unlock()
				if b, _ := s.legs[0].backing.Get(id); s.stoppedRow("s-"+id) || pending[id] || !live || rt.object != objects[id] || b.Metadata[session.DrainAckIncarnationKey] != "" {
					t.Errorf("%s: stopped %v, ever stop-pending %v, live %v, row %v; want it warm and its ack canceled", id, s.stoppedRow("s-"+id), pending[id], live, b.Metadata)
				}
			}
			for _, id := range c.stopped {
				if !s.stoppedRow("s-"+id) && !pending[id] {
					t.Errorf("%s: kills %v; want its ack to stop it", id, s.sp.kills)
				}
			}
			s.noViolations(t)
		})
	}
}

// Scenario M12 (PAR-FLOORACK, CONTRACT v5.9p; mc-7is1r; §12.2 row 66): a
// floor member acks and its pane then dies before the controller's pass,
// the normal exit after a drain (remain-on-exit keeps the dead pane
// listed). A11 proposes nothing for the dead seat; A18's recycle kills the
// corpse and restarts the same row in place under a new runtime and token,
// with the ack on the row (so the death is a drain's), and its PreWake
// clears the request: never stop-pending and never closed. Legacy differs:
// it cancels only a live runtime's ack, so its dead floor seat is stopped,
// its row closed and the floor's create replaces it.
func TestSimFloorAckOnADeadPaneRunsAgainInPlace(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour)))
	s.cfg.Agents[0].MinActiveSessions = intPtr(1)
	s.settle()
	if !s.simAck("gc-1", "tok-gc-1") {
		t.Fatal("the agent's ack did not land")
	}
	s.sp.mu.Lock()
	dead := s.sp.rts["s-gc-1"]
	dead.corpse = true
	s.sp.touch("s-gc-1")
	s.sp.mu.Unlock()
	pending := s.floorRun("gc-1", 6)
	b, _ := s.legs[0].backing.Get("gc-1")
	s.sp.mu.Lock()
	rt := s.sp.rts["s-gc-1"]
	kills := slices.Clone(s.sp.kills)
	s.sp.mu.Unlock()
	switch {
	case pending || b.Status != "open" || b.Metadata[session.DrainAckIncarnationKey] != "":
		t.Errorf("ever stop-pending %v, row %s %v; want the ack canceled and the row open", pending, b.Status, b.Metadata)
	case rt == nil || rt.corpse || rt.id != "gc-1" || rt.object == dead.object || rt.token == "tok-gc-1" || rt.token != b.Metadata["instance_token"]:
		t.Errorf("runtime %+v (dead %+v), row token %q; want the same row running again under a new runtime and token", rt, dead, b.Metadata["instance_token"])
	case len(kills) != 1 || kills[0].Method != "KillCorpseObject":
		t.Errorf("kills %+v; want only the corpse killed", kills)
	}
	s.noViolations(t)
}

// floorAckThenDie seeds a floor seat that woke age ago, lands its agent's
// bare ack, and kills its pane showing pane, as an agent that acks and exits
// leaves it; then runs patrols patrols.
func floorAckThenDie(t *testing.T, age time.Duration, pane string, patrols int) (*sim, bool) {
	t.Helper()
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-age)))
	s.cfg.Agents[0].MinActiveSessions = intPtr(1)
	s.settle()
	if !s.simAck("gc-1", "tok-gc-1") {
		t.Fatalf("woke %v ago: the agent's ack did not land", age)
	}
	s.sp.mu.Lock()
	s.sp.rts["s-gc-1"].corpse = true
	s.sp.touch("s-gc-1")
	s.sp.mu.Unlock()
	s.sp.SetPeekOutput("s-gc-1", pane)
	return s, s.floorRun("gc-1", patrols)
}

// Scenario M12 by wake age (mc-7is1r review, §12.2 row 66): a floor seat's
// agent acks and exits however soon after its wake, the normal idle case
// included (under 5 minutes, which would read a rapid crash or churn): the
// seat restarts in place and nothing accrues (no wake attempt, churn or
// quarantine), its death a drain's. Kills: A11 canceling the dead seat's
// ack before A18's recycle accounts its death.
func TestSimFloorAckOnADeadPaneByWakeAge(t *testing.T) {
	for _, age := range []time.Duration{5 * time.Second, 2 * time.Minute, 20 * time.Minute, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			s, pending := floorAckThenDie(t, age, "", 30)
			b, _ := s.legs[0].backing.Get("gc-1")
			s.sp.mu.Lock()
			rt := s.sp.rts["s-gc-1"]
			s.sp.mu.Unlock()
			switch {
			case pending || b.Status != "open" || rt == nil || rt.corpse || rt.token == "tok-gc-1" || rt.token != b.Metadata["instance_token"]:
				t.Errorf("ever stop-pending %v, row %s %v, runtime %+v; want the same row running again under a new token", pending, b.Status, b.Metadata, rt)
			case b.Metadata["wake_attempts"] != "" && b.Metadata["wake_attempts"] != "0", b.Metadata["churn_count"] != "" && b.Metadata["churn_count"] != "0", b.Metadata["quarantined_until"] != "":
				t.Errorf("row %v; want nothing accrued for a drain's death", b.Metadata)
			}
			s.noViolations(t)
		})
	}
}

// Scenario M12 over a rate-limit screen (§12.2 row 66): the ack's pending
// drain suppresses only the death's crash and churn accounting, so a dead
// floor seat whose pane shows a rate-limit screen is quarantined, not
// relaunched into the limit. Kills: a bare ack suppressing the screen check.
func TestSimFloorAckOnARateLimitedDeadPaneIsQuarantined(t *testing.T) {
	s, _ := floorAckThenDie(t, 5*time.Second, "Usage limit reached", 2)
	b, _ := s.legs[0].backing.Get("gc-1")
	if b.Status != "open" || b.Metadata["quarantined_until"] == "" || b.Metadata["sleep_reason"] != string(session.SleepReasonRateLimit) {
		t.Errorf("row %s %v; want it quarantined on the rate-limit screen", b.Status, b.Metadata)
	}
	s.noViolations(t)
}

// Scenario (PAR-FLOORACK review, CONTRACT v5.9p): an operator's `gc runtime
// drain` the agent never acks (an idle agent never polls drain-check) blocks
// no controller drain or close: with the agent suspended in config, removed,
// or the city suspended, the floor seat is still stopped, and removed with
// its runtime gone, the row still closes as an orphan.
func TestSimUnackedOperatorDrainBlocksNoControllerDrain(t *testing.T) {
	for _, follow := range []string{"agent-suspended", "agent-removed", "city-suspended", "removed-then-gone"} {
		t.Run(follow, func(t *testing.T) {
			s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour)))
			s.cfg.Agents[0].MinActiveSessions = intPtr(1)
			s.settle()
			if err := newDrainOps(s.sp).setDrain("s-gc-1"); err != nil {
				t.Fatal(err)
			}
			s.simOperatorDrain(true)
			s.floorRun("gc-1", 2)
			switch follow {
			case "agent-suspended":
				s.cfg.Agents[0].Suspended = true
			case "agent-removed":
				s.cfg.Agents[0].Name = "other"
			case "city-suspended":
				t.Setenv("GC_SUSPENDED", "1")
			case "removed-then-gone":
				s.cfg.Agents[0].Name = "other"
				s.sp.mu.Lock()
				s.sp.drop("s-gc-1")
				s.sp.mu.Unlock()
			}
			s.floorRun("gc-1", 60) // 10 minutes: past INC-003's grace
			b, _ := s.legs[0].backing.Get("gc-1")
			switch {
			case follow == "removed-then-gone" && b.Status != "closed":
				t.Errorf("row %s %v, want it closed as an orphan", b.Status, b.Metadata)
			case follow != "removed-then-gone" && !s.stoppedRow("s-gc-1"):
				t.Errorf("row %s %v, kills %v; want the seat stopped", b.Status, b.Metadata, s.sp.kills)
			}
			s.noViolations(t)
		})
	}
}

// Scenario O11 (PAR-FLOORACK review): the pass decides a floor seat's ack
// canceled; the operator's drain lands on the row before the effect runs;
// the effect re-decides on the fresh row into the signal, the seat stops,
// and no "min-floor cancel" drain transition is recorded (the transition
// comes from the fresh decision).
func TestSimOperatorDrainBetweenPassAndEffect(t *testing.T) {
	var transitions []string
	saved := recordDrainTransition
	recordDrainTransition = func(_ context.Context, name, reason, transition string) {
		transitions = append(transitions, name+":"+reason+"/"+transition)
	}
	t.Cleanup(func() { recordDrainTransition = saved })
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour)))
	s.cfg.Agents[0].MinActiveSessions = intPtr(1)
	s.settle()
	if !s.simAck("gc-1", "tok-gc-1") {
		t.Fatal("the agent's ack did not land")
	}
	s.advance(simPatrol)
	s.inventory()
	s.pass()
	var decided []string
	for _, e := range s.parked {
		decided = append(decided, e.it.Reason)
	}
	if !slices.Contains(decided, decideFloorAckCancel) {
		t.Fatalf("the pass decided %v, want the floor cancel", decided)
	}
	s.simOperatorDrain(true) // lands between the decision and its effect
	for len(s.parked) > 0 {
		s.release(0)
	}
	s.audit("v2")
	pending := s.floorRun("gc-1", 6)
	if slices.ContainsFunc(transitions, func(tr string) bool { return strings.Contains(tr, ":min-floor/") }) || !pending {
		t.Errorf("transitions %v, ever stop-pending %v; want no min-floor cancel and the ack signaled", transitions, pending)
	}
	s.noViolations(t)
}

// Scenario (PAR-FLOORACK review, finding 3): a controller request stands on
// the floor seat when the operator drains; a lens then cancels the
// controller's request; the agent acks. The operator's mark is its own key,
// so it survives the cancel, explains the ack, and the seat stops.
func TestSimOperatorDrainSurvivesACanceledControllerRequest(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour),
		session.DrainIntentReasonKey, drainIdle, session.DrainIntentAtKey, rowAt(-time.Second), session.DrainIntentIncarnationKey, "1"))
	s.cfg.Agents[0].MinActiveSessions = intPtr(1)
	s.simOperatorDrain(true)
	s.settle()
	s.floorRun("gc-1", 2)
	if b, _ := s.legs[0].backing.Get("gc-1"); b.Metadata[session.DrainIntentReasonKey] != "" || b.Metadata[session.OperatorDrainKey] == "" {
		t.Fatalf("row %v, want the controller's request canceled and the operator's mark kept", b.Metadata)
	}
	if !s.simAck("gc-1", "tok-gc-1") {
		t.Fatal("the agent's ack did not land")
	}
	if pending := s.floorRun("gc-1", 6); !pending && !s.stoppedRow("s-gc-1") {
		t.Errorf("kills %v; want the explained ack to stop the seat", s.sp.kills)
	}
	s.noViolations(t)
}

// lostCommitRun drives seed's simulation to its first start and lands op on
// the start's row after its launch Call (S2's lost commit): the outcome of
// that start, the runtime under its name once it settled, and whether op
// fired at all (a start that ended before the seam never runs it).
func lostCommitRun(t *testing.T, seed uint64, op func(s *sim, l *simLeg, b beads.Bead)) (s *sim, outcome string, up, fired bool) {
	t.Helper()
	return lostCommitRunWith(t, seed, simOpts{}, op)
}

// lostCommitRunWith is lostCommitRun over a sim with o's faults.
func lostCommitRunWith(t *testing.T, seed uint64, o simOpts, op func(s *sim, l *simLeg, b beads.Bead)) (s *sim, outcome string, up, fired bool) {
	t.Helper()
	s = newSim(t, seed, o)
	s.inventory()
	for step := 0; step < 400 && len(s.failures) == 0; step++ {
		idx := slices.IndexFunc(s.parked, func(e *simEffect) bool { return e.it.Kind == intentStart && !e.settled })
		if idx < 0 {
			s.step()
			continue
		}
		e := s.parked[idx]
		name := ""
		if b, err := s.legOf(e.it.Key.Leg).backing.Get(e.it.Key.ID); err == nil {
			name = b.Metadata["session_name"]
		}
		fire := func() {
			l := s.legOf(e.it.Key.Leg)
			if b, err := l.backing.Get(e.it.Key.ID); err == nil {
				fired = true
				s.outside("lost commit of "+e.it.Key.ID, l, b, func(l *simLeg, b beads.Bead) { op(s, l, b) })
			}
		}
		e.seam = seamAfterCall
		e.outside = func() { e.seam, e.outside = seamAfterCall, fire } // the first Call's seam (the admit ticket's here: no dead runtime to recycle) comes first
		s.release(idx)
		s.audit("v2")
		s.sp.mu.Lock()
		_, up = s.sp.rts[name]
		s.sp.mu.Unlock()
		for i := len(s.schedule) - 1; i >= 0; i-- {
			if strings.Contains(s.schedule[i], "effect start "+e.it.Key.ID) {
				outcome = s.schedule[i]
				break
			}
		}
		return s, outcome, up, fired
	}
	return s, "", false, false
}

// Kills a lost commit left running (v5 S2; the #7650 review): an operator
// puts the row asleep, or closes it, after the start's launch and before its
// commit; the start commits nothing, stops the runtime it launched under its
// own token, and settles commit-lost, leaving no orphan.
func TestSimLostCommit(t *testing.T) {
	for name, op := range map[string]func(s *sim, l *simLeg, b beads.Bead){
		"asleep": func(s *sim, l *simLeg, b beads.Bead) { s.setMeta(l, b.ID, "state", "asleep", "state_reason", "") },
		"close":  func(_ *sim, l *simLeg, b beads.Bead) { _ = l.backing.Close(b.ID) },
	} {
		t.Run(name, func(t *testing.T) {
			for seed := uint64(1); seed <= 32; seed++ {
				s, outcome, up, _ := lostCommitRun(t, seed, op)
				if !strings.Contains(outcome, "commit-lost") {
					continue
				}
				if up {
					t.Fatalf("seed %d: %q left the start's runtime running", seed, outcome)
				}
				s.noViolations(t) // a close's stop: I4's "closed within the step" (#7648)
				return
			}
			t.Fatal("no seed reached the lost commit")
		})
	}
}

// Kills the S7 exit lost in the running controller (the #7650 review's
// reach): a pool pending create whose live runtime reads StaleSelf at its
// generation (its own, under an older token) is rolled back, its runtime
// stopped first (CONTRACT v5.9q), with no violation and no orphan.
func TestSimPendingCreateStaleSelfExit(t *testing.T) {
	started := plannerT0.Add(-time.Minute).Format(time.RFC3339)
	row := poolRow("gc-1", "worker", 1, "creating", "generation", "3", "instance_token", "tok-new", "pending_create_claim", "true", "pending_create_started_at", started)
	s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		s.sp.put(row.Metadata["session_name"], "gc-1", "3", "tok-old")
		return []beads.Bead{row}, nil
	}})
	s.lag = false
	for range 3 {
		s.advance(simPatrol)
		s.settle()
	}
	s.noViolations(t)
	b, _ := s.legs[0].backing.Get("gc-1")
	s.sp.mu.Lock()
	_, up := s.sp.rts[row.Metadata["session_name"]]
	s.sp.mu.Unlock()
	if b.Status != "closed" || up || !slices.ContainsFunc(s.schedule, func(l string) bool { return strings.Contains(l, "effect rollback gc-1") }) {
		t.Fatalf("row %s, runtime up %v, want the StaleSelf pending create rolled back with its runtime stopped", b.Status, up)
	}
}

// Kills a lost commit that stops another controller's runtime (the #7690
// review's A4; v5 S2, I3): a second controller re-PreWakes the row after the
// start's launch, stopping the start's runtime and launching its own under
// its token; the start's commit is lost, and the other controller's
// runtime, which carries no token of the start's, survives.
func TestSimLostCommitSparesASecondControllersRuntime(t *testing.T) {
	var op func(s *sim, l *simLeg, b beads.Bead)
	for _, o := range externalOps {
		if o.name == "second-controller-prewake" {
			op = o.fn
		}
	}
	for seed := uint64(1); seed <= 32; seed++ {
		s, outcome, up, fired := lostCommitRun(t, seed, op)
		if outcome == "" || !fired || strings.Contains(outcome, "outcome 1 ") {
			continue // never reached, the op never fired (the start ended first), or committed ahead of the write
		}
		if !up {
			t.Fatalf("seed %d: %q stopped the second controller's runtime", seed, outcome)
		}
		s.noViolations(t)
		return
	}
	t.Fatal("no seed reached the second controller's PreWake after the launch")
}

// Scenario v5.9u (the mc-yxjzl reversal; I15, I10): a soft suspend, the
// stop and rig-restart sweeps', is no hold, and a managed suspend is one.
// Over ten patrols a soft-suspended seat with work keeps its live runtime
// running, as legacy's state heal reads it awake, and one the sweep stopped
// (`gc rig restart`) is restarted. A managed suspend stops its seat's
// runtime and keeps its hold through the dormant finalize, and with no work
// through A21 as well. No start is ever refused `held`: A18 holds what the
// PreWake would refuse, so its admit-and-refuse loop cannot recur.
func TestSimSoftSuspendIsNoHold(t *testing.T) {
	soft := []string{"instance_token", "tok-1", "suspended_at", rowAt(-time.Minute)}
	managed := slices.Concat(soft, []string{"sleep_intent", "user-hold", "held_until", rowAt(1000 * time.Hour)})
	for _, c := range []struct {
		name       string
		meta       []string
		live, work bool
		up, same   bool // the seat runs at the end; on its first runtime
	}{
		{"soft, live, with work: kept running", soft, true, true, true, true},
		{"soft, stopped by the rig-restart sweep, with work: restarted", soft, false, true, true, false},
		{"managed, live, with work: stopped and held", managed, true, true, false, false},
		{"managed, stopped, with work: held", managed, false, true, false, false},
		{"managed, live, no work: stopped and held", managed, true, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows := []beads.Bead{poolRow("gc-1", "worker", 1, "suspended", c.meta...)}
			if c.work {
				rows = append(rows, gc1Work())
			}
			var live []string
			if c.live {
				live = []string{"gc-1"}
			}
			s := scripted(t, live, rows...)
			first := s.object("s-gc-1")
			for range 10 {
				s.settle()
				s.advance(simPatrol)
			}
			for _, l := range s.schedule {
				if strings.Contains(l, "effect start gc-1:") && strings.Contains(l, causeHeld) {
					t.Fatalf("a start was admitted and refused held: %s", l)
				}
			}
			b, _ := s.legs[0].backing.Get("gc-1")
			m, obj := b.Metadata, s.object("s-gc-1")
			if up := obj != 0 && (m["state"] == "awake" || m["state"] == "active"); up != c.up || c.same && obj != first {
				t.Fatalf("runtime object %d (first %d), state %q; want up %t on its first runtime %t", obj, first, m["state"], c.up, c.same)
			}
			if !c.up && (b.Status != "open" || m["sleep_intent"] != "user-hold" || m["held_until"] != managed[7] || m["suspended_at"] != soft[3]) {
				t.Fatalf("row %s %v: want the managed suspend kept", b.Status, m)
			}
			s.noViolations(t)
		})
	}
}

// object is the tmux object of the live runtime under name, else 0.
func (s *sim) object(name string) int64 {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	if rt := s.sp.rts[name]; rt != nil && !rt.corpse {
		return rt.object
	}
	return 0
}

// Scenario R46 (CONTRACT R46, I23, M1; mc-phuxq, J14 E): an operator drains
// a seat, its agent acks and exits, and its remain-on-exit corpse lost its
// GC_INSTANCE_TOKEN (or all its identity). The corpse counts as stopped:
// A11 signals the acked row though the census reads the identity Unknown,
// and the stop verb finalizes it drained and removes the corpse, as it does
// a corpse carrying the row's token. A token-less runtime still live is no
// corpse and holds.
func TestSimR46AckedRowOverATokenlessCorpseFinalizes(t *testing.T) {
	for _, c := range []struct {
		name      string
		id, token string
		corpse    bool
		finalized bool
	}{
		{"the row's token", "gc-1", "tok-gc-1", true, true},
		{"no token", "gc-1", "", true, true},
		{"no identity at all", "", "", true, true},
		{"no token, still live", "gc-1", "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := poolRow("gc-1", "worker", 1, "awake", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-10*time.Minute),
				session.OperatorDrainKey, "1", session.DrainAckIncarnationKey, "1")
			s := scripted(t, []string{"gc-1"}, row)
			s.advance(time.Second)
			s.operator("gc-1", session.DrainAckAtKey, s.rel(0)) // the ack lands after the runtime started
			name := row.Metadata["session_name"]
			rt := s.sp.rts[name]
			rt.id, rt.token, rt.corpse = c.id, c.token, c.corpse
			s.sp.touch(name)
			for range 6 {
				s.settle()
				s.advance(simPatrol)
			}
			s.quiesce()
			b, err := s.legs[0].backing.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			finalized := b.Metadata["state"] == string(session.StateDrained)
			if finalized != c.finalized || c.finalized && s.object(name) != 0 {
				t.Fatalf("row %s/%s, corpse object %d; want finalized %v and the corpse removed", b.Status, b.Metadata["state"], s.object(name), c.finalized)
			}
			s.noViolations(t)
		})
	}
}

// zombieAdmissions counts the zombie effects s's schedule admitted.
func (s *sim) zombieAdmissions() int {
	n := 0
	for _, l := range s.schedule {
		n += strings.Count(l, "zombie:gc-1")
	}
	return n
}

// Scenario mc-98agx / mc-nxpo8 (CONTRACT v5.10g): the inventory lists a
// quarantined seat's zombie; the runtime then vanishes before the next
// inventory. A12, decided on the stale dead read, runs once: its fresh read
// finds the name gone on every hop, the settlement notes the absence, and
// every later pass before the next inventory reads the row gone, so A12 is
// not proposed again (before, it ran every pass, taking and dropping the
// row's lease record). Kills: an absent read left unnoted, and a sim whose
// planner drops notes.
func TestSimZombieOverAGoneRuntimeRunsOnce(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour),
		"quarantined_until", rowAt(time.Hour), "sleep_reason", "quarantine"))
	s.sp.mu.Lock()
	s.sp.rts["s-gc-1"].zombie = true
	s.sp.touch("s-gc-1")
	s.sp.mu.Unlock()
	s.inventory() // lists the zombie: the census reads gc-1 dead
	s.sp.mu.Lock()
	s.sp.drop("s-gc-1")
	s.sp.mu.Unlock()
	s.advance(time.Second) // the effects read after the pass started: a tie is the pass's (Note)
	for range 6 {
		s.pass()
		for len(s.parked) > 0 {
			s.release(0)
		}
	}
	if n := s.zombieAdmissions(); n != 1 {
		t.Errorf("A12 admitted %d times before the next inventory, want once; schedule:\n%s", n, strings.Join(s.schedule, "\n"))
	}
	s.noViolations(t)
}

// Scenario v5.10g, the note's window: an effect's fresh read finds a wanted
// seat's runtime gone and notes it, and a runtime then comes back under the
// row's own identity (a start without the controller's PreWake, `gc session
// attach`'s shape) before the next inventory. The next pass reads the row
// gone and proposes the start, whose own fresh read finds the runtime alive
// and the row's: it adopts it, launching nothing and stopping nothing.
// Kills: a start that trusts the note over its own fresh read.
func TestSimRuntimeBackAfterAnAbsenceNoteIsAdopted(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1"), gc1Work())
	s.settle()
	m := s.gc1()
	s.sp.mu.Lock()
	s.sp.drop("s-gc-1")
	s.sp.mu.Unlock()
	s.advance(time.Second) // the read comes after the inventory pass started
	now := s.clk.Now()
	s.p.applyFacts(effectFacts{Noted: &notedRuntime{Name: "s-gc-1", At: now, Gone: true}}, now) // an effect's absent read
	s.sp.mu.Lock()
	s.sp.put("s-gc-1", "gc-1", m["generation"], m["instance_token"])
	back := s.sp.rts["s-gc-1"].object
	starts, kills := len(s.sp.starts), len(s.sp.kills)
	s.sp.mu.Unlock()
	if got := s.liveness("gc-1"); got != livenessGone {
		t.Fatalf("liveness %v after the note, want gone (the next inventory has not run)", got)
	}
	s.advance(time.Second)
	s.pass()
	for len(s.parked) > 0 {
		s.release(0)
	}
	s.sp.mu.Lock()
	rt := s.sp.rts["s-gc-1"]
	gotStarts, gotKills := len(s.sp.starts), len(s.sp.kills)
	s.sp.mu.Unlock()
	if rt == nil || rt.object != back || gotStarts != starts || gotKills != kills {
		t.Errorf("runtime %+v (back as object %d), starts %d -> %d, kills %d -> %d; want the runtime adopted, nothing launched or stopped; schedule:\n%s",
			rt, back, starts, gotStarts, kills, gotKills, strings.Join(s.schedule, "\n"))
	}
	if !slices.ContainsFunc(s.schedule, func(l string) bool { return strings.Contains(l, "admitted [start:gc-1") }) {
		t.Errorf("no start admitted on the gone read; schedule:\n%s", strings.Join(s.schedule, "\n"))
	}
	if b := s.gc1(); b["instance_token"] != m["instance_token"] {
		t.Errorf("row token %q, want %q kept", b["instance_token"], m["instance_token"])
	}
	s.noViolations(t)
}

// Scenario J26 D/E (mc-kgl4v; CONTRACT D7 rule 4, v5.10n, §12.2 row 26): an
// operator drains a pool seat, its agent acks and lingers, and an explicit
// wake lands while the stop's kill runs. The finalize never closes the row
// over the wake; the next passes start it again (the seat runs, the row
// reads awake) and its start consumes the wake.
func TestSimWakeDuringAStopKillRunsTheSeatAgain(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour)))
	s.settle()
	if err := newDrainOps(s.sp).setDrain("s-gc-1"); err != nil {
		t.Fatal(err)
	}
	s.simOperatorDrain(true)
	if !s.simAck("gc-1", "tok-gc-1") {
		t.Fatal("the agent's ack did not land")
	}
	woke := false
	for i := 0; i < 20 && !woke; i++ {
		s.advance(simPatrol)
		s.inventory()
		s.pass()
		for len(s.parked) > 0 {
			if e := s.parked[0]; e.it.Kind == intentStop && e.it.Key.ID == "gc-1" && !woke {
				woke, e.seam = true, seamAfterCall // the wake lands while the kill runs
				e.outside = func() {
					l := s.legs[0]
					b, _ := l.backing.Get("gc-1")
					s.outside("wake", l, b, func(l *simLeg, b beads.Bead) {
						s.setMeta(l, b.ID, "wake_request", "explicit", "wake_requested_at", s.clk.Now().UTC().Format(time.RFC3339))
					})
				}
			}
			s.release(0)
		}
		s.audit("v2")
	}
	if !woke {
		t.Fatal("the seat was never stopped")
	}
	row := func() beads.Bead {
		b, err := s.legs[0].backing.Get("gc-1")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	runs := func() bool {
		b := row()
		s.sp.mu.Lock()
		rt := s.sp.rts["s-gc-1"]
		s.sp.mu.Unlock()
		return b.Status != "closed" && rt != nil && b.Metadata["state"] == string(session.StateAwake) && b.Metadata["wake_request"] == ""
	}
	for i := 0; i < 12 && !runs(); i++ {
		if b := row(); b.Status == "closed" {
			t.Fatalf("the row closed over the wake: %v", b.Metadata)
		}
		s.advance(simPatrol)
		s.settle()
	}
	if !runs() {
		t.Fatalf("the woken seat never ran again: %v", row().Metadata)
	}
	s.noViolations(t)
}

// Scenario mc-4gyej (CONTRACT v5.10w, §12.2 row 66): a floor seat's agent
// acks its own idle drain and exits, leaving a corpse. With the row's token
// on the corpse the seat restarts in place, as row 66 says; with no token,
// or no identity at all, nothing can attribute the corpse to the row, so
// the ack is signaled and the stop finalizes the row (closed, drained,
// the corpse removed) for the floor to recreate the seat, instead of the
// floor check voiding the ack and A9 holding the row over the corpse for
// good. Kills: the floor check canceling the token-less ack, and the
// token case losing its restart in place.
func TestSimFloorAckOverATokenlessCorpseFinalizes(t *testing.T) {
	for _, c := range []struct {
		name      string
		id, token string
		inPlace   bool
	}{
		{"the row's token: restarted in place", "gc-1", "tok-gc-1", true},
		{"no token: finalized", "gc-1", "", false},
		{"no identity: finalized", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := poolRow("gc-1", "worker", 1, "awake", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-10*time.Minute), session.DrainAckIncarnationKey, "1")
			s := scripted(t, []string{"gc-1"}, row)
			s.cfg.Agents[0].MinActiveSessions = intPtr(1)
			s.advance(time.Second)
			s.operator("gc-1", session.DrainAckAtKey, s.rel(0)) // the ack lands after the runtime started
			name := row.Metadata["session_name"]
			s.sp.mu.Lock()
			rt := s.sp.rts[name]
			first := rt.object
			rt.id, rt.token, rt.corpse = c.id, c.token, true
			s.sp.touch(name)
			s.sp.mu.Unlock()
			for range 10 {
				s.settle()
				s.advance(simPatrol)
			}
			s.quiesce()
			b, err := s.legs[0].backing.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			s.sp.mu.Lock()
			now := s.sp.rts[name]
			s.sp.mu.Unlock()
			switch {
			case c.inPlace && (b.Status != "open" || now == nil || now.corpse || now.object == first || now.id != "gc-1"):
				t.Fatalf("row %s/%s, runtime %+v; want the seat restarted in place", b.Status, b.Metadata["state"], now)
			case !c.inPlace && (b.Status != "closed" || b.Metadata["state"] != string(session.StateDrained) || now != nil):
				t.Fatalf("row %s/%s, runtime %+v; want the row finalized (closed, drained) and the corpse removed", b.Status, b.Metadata["state"], now)
			}
			s.noViolations(t)
		})
	}
}

// Kills `gc rig restart` leaving a min_active_sessions pool at zero live
// seats under v2 (mc-ewptj): the real sweep (doRigRestart) runs as an
// outside writer over three live pool seats with no work, then the planner
// runs. A floor of 2 comes back to two live seats, revived from the
// city-stop seats the restart marked; with no floor none restarts; a seat
// with work restarts; no invariant breaks.
func TestSimRigRestartRevivesTheFloor(t *testing.T) {
	for _, c := range []struct {
		name  string
		floor int
		work  []string
		live  int
	}{
		{"no work, floor 2", 2, nil, 2},
		{"no work, no floor", 0, nil, 0},
		{"work on gc-1, no floor", 0, []string{"gc-1"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			var rows []beads.Bead
			var live []string
			for i := 1; i <= 3; i++ {
				id := fmt.Sprintf("gc-%d", i)
				rows = append(rows, poolRow(id, "worker", i, "awake", "instance_token", "tok-"+id, "last_woke_at", rowAt(-10*time.Minute)))
				live = append(live, id)
			}
			for _, id := range c.work {
				rows = append(rows, beads.Bead{ID: "gw-" + id, Title: "w", Type: "task", Status: "open", Assignee: id, Metadata: map[string]string{"gc.routed_to": "worker"}})
			}
			s := scripted(t, live, rows...)
			if c.floor > 0 {
				s.cfg.Agents[0].MinActiveSessions = intPtr(c.floor)
			}
			for range 3 {
				s.settle()
				s.advance(simPatrol)
			}
			first, err := s.legs[0].backing.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			code := -1
			s.outside("gc rig restart", s.legs[0], first, func(l *simLeg, _ beads.Bead) {
				code = doRigRestart(s.sp, events.Discard, l.backing, s.cfg, s.cfg.Agents, "", "test-city", "", &bytes.Buffer{}, &stderr)
				// The sweep stamps the wall clock; restamp its times on the
				// sim's, or quiescence waits out the gap as a timer.
				rows, _ := l.backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				for _, b := range rows {
					for _, key := range []string{"slept_at", "suspended_at"} {
						if b.Metadata[key] != "" {
							_ = l.backing.SetMetadata(b.ID, key, s.rel(0))
						}
					}
				}
			})
			if code != 0 {
				t.Fatalf("gc rig restart exited %d: %s", code, stderr.String())
			}
			for range 12 {
				s.settle()
				s.advance(simPatrol)
			}
			s.quiesce()
			all, err := s.legs[0].backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			var seats []string
			for _, b := range all {
				if b.Metadata["template"] != "worker" {
					continue
				}
				if b.Status != "closed" && s.object(b.Metadata["session_name"]) != 0 {
					got++
				}
				seats = append(seats, fmt.Sprintf("%s:%s/%s/%s", b.ID, b.Status, b.Metadata["state"], b.Metadata["sleep_reason"]))
			}
			if got != c.live {
				t.Errorf("live seats after the restart = %d, want %d: %v", got, c.live, seats)
			}
			s.report(t)
		})
	}
}

// Scenario J21 C / mc-sqdv6 (CONTRACT v5.7 D7 rule 2, §12.2 #26): an
// operator wakes an asleep seat and attaches, so its runtime comes up
// outside the controller, the wake request still on the row. A6's
// asleep-to-awake heal clears the wake in its own write (or, on an awake row,
// the satisfied-wake clear does). When the runtime later dies, the row's
// runtime-missing heal finds no wake left to restart it with. Kills a
// residue wake that restarts the seat once after the death.
func TestSimSatisfiedWakeDoesNotRestartAfterADeath(t *testing.T) {
	for _, state := range []string{"asleep", "awake"} {
		t.Run(state, func(t *testing.T) {
			s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, state, "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour),
				"wake_request", "explicit", "wake_requested_at", rowAt(-time.Minute)))
			s.settle()
			s.settle()
			if m := s.gc1(); m["wake_request"] != "" || m["wake_requested_at"] != "" {
				t.Fatalf("row %v: want the wake consumed while its runtime runs", m)
			}
			s.sp.mu.Lock()
			starts := len(s.sp.starts)
			s.sp.drop("s-gc-1")
			s.sp.mu.Unlock()
			for range 6 {
				s.advance(simPatrol)
				s.settle()
			}
			s.sp.mu.Lock()
			restarted := len(s.sp.starts) > starts
			s.sp.mu.Unlock()
			if restarted {
				t.Errorf("row %v: the seat restarted after its runtime died, want the wake consumed and no restart; schedule:\n%s", s.gc1(), strings.Join(s.schedule, "\n"))
			}
			s.noViolations(t)
		})
	}
}

// Scenario DRAIN-047 (the #7855 review): an operator wakes an awake seat
// whose idle or no-wake drain is requested (begun, not yet signaled); the
// wake makes A19 cancel that drain, and the seat stays up through it; the
// satisfied-wake clear consumes the wake only after the cancel. Kills the
// clear consuming that wake first, so the requested drain goes on and the
// seat is stopped. (Once the wake is consumed, the seat idle-sleeps
// normally on a later drain of its own, §12.2 #26.)
func TestSimWakeOverARequestedDrainCancelsIt(t *testing.T) {
	for _, reason := range []string{"idle", reasonNoWake} {
		t.Run(reason, func(t *testing.T) {
			s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "awake", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour),
				session.DrainIntentReasonKey, reason, session.DrainIntentAtKey, rowAt(-5*time.Second), session.DrainIntentIncarnationKey, "1",
				"wake_request", "explicit", "wake_requested_at", rowAt(-time.Second)))
			s.settle()
			s.settle()
			s.sp.mu.Lock()
			rt := s.sp.rts["s-gc-1"]
			kills := len(s.sp.kills)
			s.sp.mu.Unlock()
			b := s.gc1()
			canceled := slices.ContainsFunc(s.schedule, func(l string) bool { return strings.Contains(l, "admitted [drain-cancel:gc-1") })
			switch {
			case !canceled:
				t.Errorf("%s: no drain cancel admitted; schedule:\n%s", reason, strings.Join(s.schedule, "\n"))
			case rt == nil || rt.corpse || kills != 0 || b[session.DrainIntentReasonKey] != "":
				t.Errorf("%s: runtime %+v, kills %d, row %v; want the seat up and the requested drain canceled", reason, rt, kills, b)
			case b["wake_request"] != "":
				t.Errorf("%s: row %v; want the wake consumed once the cancel landed", reason, b)
			}
			s.noViolations(t)
		})
	}
}
