package main

import (
	"strings"
	"testing"
	"time"
)

// TestQuarantinedCommitBoundCountsOnlyV2sTime (CONTRACT v5.9x; the model and
// this test drafted by aebd4cf, folded in here for seed 690). Kills a #57
// bound that excuses v2's own delay, or that charges v2 for time it cannot
// act: a row committed under a late quarantine, its liveness known and no
// outside write, still up undrained past six patrols fails I15; the same
// delay while its liveness reads unknown (A9's hold) does not, nor one an
// outside write to its drain basis restarted.
func TestQuarantinedCommitBoundCountsOnlyV2sTime(t *testing.T) {
	for _, c := range []struct {
		name   string
		during func(s *sim)
		fails  bool
	}{
		{"v2's own delay", func(*sim) {}, true},
		{"liveness unknown", func(s *sim) { s.sp.listing = 2 }, false},
		{"an outside write moved the basis", func(s *sim) { s.operator("gc-1", "wake_request", "operator") }, false},
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
