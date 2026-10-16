package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// TestQuarantinedCommitBoundCountsOnlyV2sTime (CONTRACT v5.9x; the model and
// this test drafted by aebd4cf, folded in here for seed 690). Kills a #57
// bound that excuses v2's own delay, or that charges v2 for time it cannot
// act: a row committed under a late quarantine, its liveness known and no
// outside write, still up undrained past six patrols fails I15; the same
// delay while its liveness reads unknown (A9's hold) or its state is one A5
// holds (v5.10i) does not, nor one an
// outside write to its drain basis restarted, nor one an outside write
// restarted by refusing the row's own effect on its premise (any premise
// key the refusal names: a heartbeat's held_until, shifted seed 607;
// v5.9z-1). The row's own effect refused over v2's own write, over an outside
// write the refusal does not name, or for another cause, restarts nothing.
func TestQuarantinedCommitBoundCountsOnlyV2sTime(t *testing.T) {
	for _, c := range []struct {
		name   string
		during func(s *sim)
		fails  bool
	}{
		{"v2's own delay", func(*sim) {}, true},
		{"liveness unknown", func(s *sim) { s.sp.listing = 2 }, false},
		// v5.10i (seed 437 of the floored-pending corpus): a state A5 holds.
		{"state unknown", func(s *sim) { s.setMeta(s.legs[0], "gc-1", "state", "archived-v9") }, false},
		// Drain-ack stop-pending is no state A5 holds (the census's
		// UnknownState sets it aside): the clock runs.
		{"drain-ack stop-pending", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", "state", string(session.StateDraining), "state_reason", session.DrainAckStopPendingReason)
		}, true},
		{"an outside write moved the basis", func(s *sim) { s.operator("gc-1", "wake_request", "operator") }, false},
		{"an outside write refused its own effect on the premise", func(s *sim) {
			refuse(s, func() { s.operator("gc-1", "held_until", rowAt(2*time.Hour)) }, causePremise, "held_until")
		}, false},
		{"an outside write the refusal does not name", func(s *sim) {
			refuse(s, func() { s.operator("gc-1", "held_until", rowAt(2*time.Hour)) }, causePremise, "state")
		}, true},
		{"v2's own write refused its own effect on the premise", func(s *sim) {
			refuse(s, func() { s.setMeta(s.legs[0], "gc-1", "held_until", rowAt(2*time.Hour)); s.audit("v2") }, causePremise, "held_until")
		}, true},
		{"an outside write, the effect refused for another cause", func(s *sim) {
			refuse(s, func() { s.operator("gc-1", "held_until", rowAt(2*time.Hour)) }, causeRedecided, "held_until")
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "awake", "instance_token", "tok-1", "quarantined_until", rowAt(time.Hour), "sleep_reason", "quarantine"))
			s.inventory()
			id := s.legs[0].name + "/gc-1"
			now := s.clk.Now()
			s.quarantinedCommits = map[string]*quarantinedClock{id: {start: now, checked: now, moves: len(s.basisMoves)}}
			for i := range 8 {
				if i == 3 {
					c.during(s)
				}
				s.advance(simPatrol)
				s.inventory()
				s.checkQuarantinedCommits()
			}
			failed := len(s.failures) == 1 && strings.HasPrefix(s.failures[0], "I15 ")
			if failed != c.fails || len(s.failures) > 1 {
				t.Fatalf("failures %q, want an I15 bound miss %t", s.failures, c.fails)
			}
		})
	}
}

// refuse runs gc-1's heal through checkPremiseRefusal: released on the
// cache's row, then write lands on the store, then it settles refused with
// cause, the premise keys moved named as the refusal names them.
func refuse(s *sim, write func(), cause, moved string) {
	settle := s.checkPremiseRefusal(&simEffect{it: intent{Kind: intentRowHeal, Key: rowKey{Leg: rowLeg, ID: "gc-1"}}})
	write()
	settle(settlement{Outcome: settledRefused, Cause: cause, Err: fmt.Errorf("%w: %s", errPremiseMoved, moved)})
}
