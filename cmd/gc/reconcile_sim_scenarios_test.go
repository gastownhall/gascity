package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
// mark the planner dirty and write nothing: no write loop.
func TestSimR17ReplayedEventsWriteNothing(t *testing.T) {
	s := scripted(t, nil, heldRow())
	s.inventory()
	s.pass()
	s.release(0)
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

// workFor is in-progress work routed to the worker pool and assigned to id.
func workFor(id string) beads.Bead {
	return beads.Bead{ID: "gw-" + id, Title: "work", Type: "task", Status: "in_progress", Assignee: id, Metadata: map[string]string{"gc.routed_to": "worker"}}
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
// time, and the runtime's death clears it in the dead-runtime heal's one CAS.
func TestSimDetachedAtFollowsTheAttachFact(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1"), workFor("gc-1"))
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
	if m := s.gc1(); m["state"] != "asleep" {
		t.Fatalf("row %v, want the dead runtime healed asleep", m)
	}
	s.noViolations(t)
}

// Kills the dead-runtime heal (v5.8 A6 item 4; SESS-531) narrowed to Wake
// or named rows: a pool row AL1 wants asleep (quarantined, its work waiting)
// whose runtime dies heals to asleep with runtime-missing and its
// continuation reset, which A21's pool-slot close can free. A heartbeat-held
// row waits for A17 while its hold runs, and heals once the timer heal has
// cleared the expired hold.
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
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", meta...), workFor("gc-1"))
	s.settle()
	s.sp.mu.Lock()
	s.sp.drop("s-gc-1")
	s.sp.mu.Unlock()
	s.advance(simPatrol)
	s.settle()
	if got := s.gc1()["state"]; held != (got == "active") {
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

// Kills an I15 oracle that lets any write end a live quarantine: A6's
// stability clear may clear quarantined_until only on a row its deciding pass
// read committed, its own runtime alive and 30s past its wake. A clear inside
// the 30s, on a dead runtime or on an uncommitted row is reported, and so is,
// on a stable row, a rewrite of the quarantine or a stop request.
func TestSimI15StabilityCarveOut(t *testing.T) {
	quarantined := func(state string, woke time.Duration) beads.Bead {
		return poolRow("gc-1", "worker", 1, state, "instance_token", "tok-1", "last_woke_at", rowAt(woke), "quarantined_until", rowAt(time.Hour))
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
		{"stable, the quarantine rewritten", quarantined("active", -time.Hour), false, session.MetadataPatch{"quarantined_until": rowAt(2 * time.Hour)}},
		{"stable, a stop request", quarantined("active", -time.Hour), false, session.MetadataPatch{"quarantined_until": "", session.DrainIntentReasonKey: "idle"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write := func(r *rowFacts) (intent, bool) {
				if r.row.Info.QuarantinedUntil == tc.patch["quarantined_until"] {
					return intent{}, false
				}
				return r.heal(intentRowHeal, decideStabilityClear, tc.patch)
			}
			s := scriptedWith(t, simOpts{arms: mutateHeal(write)}, []string{"gc-1"}, tc.row)
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
	// The real clear, on a stable row, is not.
	s := scripted(t, []string{"gc-1"}, quarantined("active", -time.Hour))
	s.settle()
	if s.gc1()["quarantined_until"] != "" {
		t.Fatalf("quarantined_until %q, want A6's stability clear", s.gc1()["quarantined_until"])
	}
	s.noViolations(t)
}

// Kills the dead-runtime heal (v5.8e A6 item 4) reaching past the sessions
// leg: a census-only rig-leg row with no runtime may be another city's
// on a shared rig store, so it keeps its state and its conversation keys,
// while the sessions-leg row beside it heals.
func TestSimDeadRuntimeHealSkipsCensusOnlyRigRows(t *testing.T) {
	meta := []string{"instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(0), "quarantined_until", rowAt(time.Hour)}
	rig := poolRow("rg-1", "worker", 11, "active", meta...)
	s := newSim(t, 1, simOpts{rows: func(*sim) ([]beads.Bead, []beads.Bead) {
		return []beads.Bead{poolRow("gc-1", "worker", 1, "active", meta...), workFor("gc-1")}, []beads.Bead{rig}
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
