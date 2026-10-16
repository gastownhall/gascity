package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/resilience"
)

// Rotation reorders only an open episode. A closed breaker leaves the wake
// fairness order alone, so a fleet that is not failing sees no change.
func TestSortCandidatesByFleetProbeRotation(t *testing.T) {
	r := newFleetProbeRun(t, "")
	order := func(cs []startCandidate) string {
		s := ""
		for _, c := range cs {
			s += c.name() + " "
		}
		return s
	}
	cands := append([]startCandidate(nil), r.cands...)
	want := order(cands)

	sortCandidatesByFleetProbeRotation(cands, nil)
	if got := order(cands); got != want {
		t.Errorf("nil guard reordered: %q, want %q", got, want)
	}
	sortCandidatesByFleetProbeRotation(cands, r.g)
	if got := order(cands); got != want {
		t.Errorf("closed guard reordered: %q, want %q", got, want)
	}

	r.g.breaker().Trip()
	// w0 and w1 failed inside the episode: they go last, the rest keep their
	// relative order, and w0 (failed more often) goes after w1.
	r.g.mu.Lock()
	r.g.attempts[r.cands[0].info.ID] = 2
	r.g.attempts[r.cands[1].info.ID] = 1
	r.g.mu.Unlock()
	sortCandidatesByFleetProbeRotation(cands, r.g)
	if got, want := order(cands), "w2 w3 w4 w5 w6 w7 w8 w1 w0 "; got != want {
		t.Errorf("rotated order = %q, want %q", got, want)
	}
}

// The failure count of an episode is reset when the breaker closes, and an
// operator reset forgets it too, so a later episode starts even.
func TestFleetGuard_EpisodeFailureCountsEndWithTheEpisode(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	failFleetStart(t, g, "s1", "a")
	clk.Advance(time.Second)
	failFleetStart(t, g, "s2", "b")
	clk.Advance(time.Second)
	failFleetStart(t, g, "s3", "c")
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("state = %v, want open", got)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if got := g.episodeFailures(id); got != 1 {
			t.Errorf("episodeFailures(%s) after the trip = %d, want 1 (the failure that tripped it)", id, got)
		}
	}
	clk.Advance(time.Hour)
	ticket, ok := g.Admit("s1", "a")
	if !ok || !ticket.Probe() {
		t.Fatalf("Admit(s1) = %v probe=%v, want the probe", ok, ticket.Probe())
	}
	ticket.Resolve(fleetStartFailed)
	if got := g.episodeFailures("s1"); got != 2 {
		t.Errorf("episodeFailures(s1) after a failed probe = %d, want 2", got)
	}
	if !g.Reset() {
		t.Fatal("Reset reported the breaker was not open")
	}
	if got := g.episodeFailures("s1"); got != 0 {
		t.Errorf("episodeFailures(s1) after an operator reset = %d, want 0", got)
	}
}

// A breaker that closes on its own ends the episode: the next one ranks every
// session even again, not by what failed before the recovery.
func TestFleetGuard_EpisodeFailureCountsClearWhenTheBreakerCloses(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	probe := admitFleetProbe(t, g, clk, "p1")
	probe.Resolve(fleetStartOK)
	clk.Advance(10 * time.Second)
	g.ObserveLiveness("p1", true)
	clk.Advance(stabilityThreshold)
	g.ObserveLiveness("p1", true)
	if got := fleetState(g); got != resilience.StateClosed {
		t.Fatalf("premise: state = %v, want closed", got)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if got := g.episodeFailures(id); got != 0 {
			t.Errorf("episodeFailures(%s) after the breaker closed = %d, want 0", id, got)
		}
	}
}

// Failures that aged out of the trip window without opening the breaker belong
// to no episode: a later trip starts every session's count from its own
// failure, not from the stale one.
func TestFleetGuard_FailuresBeforeTheWindowDoNotCarryIntoTheNextEpisode(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	failFleetStart(t, g, "s1", "a")
	clk.Advance(fleetFailureWindow + time.Second)
	failFleetStart(t, g, "s2", "a")
	clk.Advance(time.Second)
	failFleetStart(t, g, "s3", "b")
	clk.Advance(time.Second)
	failFleetStart(t, g, "s4", "c")
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("premise: state = %v, want open", got)
	}
	if got := g.episodeFailures("s1"); got != 0 {
		t.Errorf("episodeFailures(s1) = %d, want 0: its failure aged out before the episode began", got)
	}
	if got := g.episodeFailures("s2"); got != 1 {
		t.Errorf("episodeFailures(s2) = %d, want 1", got)
	}
}

// A session the reconciler is done with carries no failure count.
func TestFleetGuard_ForgetSessionDropsItsEpisodeFailureCount(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	if got := g.episodeFailures("s1"); got != 1 {
		t.Fatalf("premise: episodeFailures(s1) = %d, want 1", got)
	}
	g.ForgetSession("s1")
	if got := g.episodeFailures("s1"); got != 0 {
		t.Errorf("episodeFailures(s1) after ForgetSession = %d, want 0", got)
	}
	if got := g.episodeFailures("s2"); got != 1 {
		t.Errorf("episodeFailures(s2) = %d, want 1: ForgetSession must touch only its own session", got)
	}
}

// A probe whose session starts but dies inside the stability window failed that
// probe: it counts against the session, so the next probe goes elsewhere.
func TestFleetGuard_ProbeDeathInsideStabilityCountsAgainstTheSession(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	probe := admitFleetProbe(t, g, clk, "p1")
	probe.Resolve(fleetStartOK)
	clk.Advance(5 * time.Second)
	g.ObserveLiveness("p1", false)
	if got := g.episodeFailures("p1"); got != 1 {
		t.Errorf("episodeFailures(p1) after its probe session died = %d, want 1", got)
	}
}
