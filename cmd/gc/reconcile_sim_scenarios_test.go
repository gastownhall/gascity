package main

import (
	"encoding/json"
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
	s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		for _, b := range rows {
			for _, id := range live {
				if b.ID == id {
					s.sp.put(b.Metadata["session_name"], b.ID, b.Metadata["generation"], b.Metadata["instance_token"])
				}
			}
		}
		return rows, nil
	}})
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
// however the operator's event travels: the effect refuses and writes
// nothing, the hold stands, and the next pass proposes nothing.
func TestSimR5StaleHealRedecidesAtTheStore(t *testing.T) {
	for _, c := range []struct {
		name  string
		event func(s *sim, rehold json.RawMessage)
		cause string
	}{
		{"the re-hold's event delivered", func(s *sim, ev json.RawMessage) { s.legs[0].cache.ApplyEvent("bead.updated", ev) }, causeRedecided},
		{"the re-hold's event held", func(*sim, json.RawMessage) {}, causeCAS},
		// The cache, rescanned past the re-hold, then takes an older event,
		// reordered: the cache keeps the re-hold (mc-03lk4), so the effect's
		// read decides on it.
		{"an older event reordered after a rescan", func(s *sim, _ json.RawMessage) {
			older := s.legs[0].events[0]
			s.legs[0].cache.ReconcileNowForTest()
			s.legs[0].cache.ApplyEvent("bead.updated", older)
		}, causeRedecided},
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
